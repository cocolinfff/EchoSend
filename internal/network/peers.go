// Package network implements the UDP gossip layer, TCP file server, and peer
// registry for the EchoSend P2P daemon.
package network

import (
	"fmt"
	"sync"
	"time"

	"p2p-sync/internal/models"
	"p2p-sync/internal/storage"
)

// ─────────────────────────────────────────────────────────────────────────────
// PeerRegistry
// ─────────────────────────────────────────────────────────────────────────────

// PeerRegistry maintains an in-memory, thread-safe map of all currently known
// LAN peers.  It acts as a fast lookup cache on top of the BoltDB store:
//
//   - Every PRESENCE packet received upserts the peer both here and in the DB.
//   - The UDP gossip layer queries this registry to obtain the fan-out target
//     list when re-broadcasting packets.
//   - The heartbeat emitter queries this registry to send directed unicast
//     keep-alives to already-known peers.
//
// The registry is intentionally separate from the storage layer so that reads
// (which happen on every received UDP packet) never touch the disk.
type PeerRegistry struct {
	mu    sync.RWMutex
	peers map[string]*peerEntry // keyed by NodeID

	store *storage.Store // backing persistent store (may be nil in tests)
}

// peerEntry is the in-memory representation of a single known peer.
type peerEntry struct {
	Info     models.NodeInfo
	LastSeen time.Time
}

// NewPeerRegistry creates an empty PeerRegistry backed by store.
// store may be nil for unit tests; persistence is simply skipped in that case.
func NewPeerRegistry(store *storage.Store) *PeerRegistry {
	return &PeerRegistry{
		peers: make(map[string]*peerEntry),
		store: store,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Write operations
// ─────────────────────────────────────────────────────────────────────────────

// Upsert inserts or updates the peer described by node.
// It refreshes LastSeen to now and persists the record to BoltDB.
// Returns true if this is a brand-new peer (useful for logging).
func (r *PeerRegistry) Upsert(node models.NodeInfo) (isNew bool) {
	node.LastSeen = time.Now().UnixNano()

	r.mu.Lock()
	_, exists := r.peers[node.NodeID]
	r.peers[node.NodeID] = &peerEntry{
		Info:     node,
		LastSeen: time.Now(),
	}
	r.mu.Unlock()

	// Persist asynchronously so the UDP receive hot-path is not blocked on I/O.
	if r.store != nil {
		go func() {
			if err := r.store.UpsertNode(node); err != nil {
				// Non-fatal: the in-memory entry is already updated.
				_ = fmt.Errorf("peer registry: persist %s: %w", node.NodeID, err)
			}
		}()
	}

	return !exists
}

// Remove deletes the peer identified by nodeID from both the in-memory cache
// and the persistent store.
func (r *PeerRegistry) Remove(nodeID string) {
	r.mu.Lock()
	delete(r.peers, nodeID)
	r.mu.Unlock()

	if r.store != nil {
		go func() { _ = r.store.DeleteNode(nodeID) }()
	}
}

// PruneExpired removes peers whose last-seen timestamp is older than maxAge.
// It returns the NodeIDs of the removed entries.
func (r *PeerRegistry) PruneExpired(maxAge time.Duration) []string {
	cutoff := time.Now().Add(-maxAge)

	r.mu.Lock()
	var pruned []string
	for id, e := range r.peers {
		if e.LastSeen.Before(cutoff) {
			pruned = append(pruned, id)
			delete(r.peers, id)
		}
	}
	r.mu.Unlock()

	if r.store != nil && len(pruned) > 0 {
		go func() {
			for _, id := range pruned {
				_ = r.store.DeleteNode(id)
			}
		}()
	}

	return pruned
}

// LoadFromStore populates the in-memory cache from the BoltDB nodes bucket.
// Call once at daemon startup so the registry is warm before the first
// heartbeat cycle fires.
func (r *PeerRegistry) LoadFromStore() error {
	if r.store == nil {
		return nil
	}
	nodes, err := r.store.ListNodes()
	if err != nil {
		return fmt.Errorf("peer registry: load from store: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range nodes {
		r.peers[n.NodeID] = &peerEntry{
			Info:     n,
			LastSeen: time.Unix(0, n.LastSeen),
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Read operations
// ─────────────────────────────────────────────────────────────────────────────

// Get returns the NodeInfo for nodeID.  The second return value is false when
// the peer is not known.
func (r *PeerRegistry) Get(nodeID string) (models.NodeInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.peers[nodeID]
	if !ok {
		return models.NodeInfo{}, false
	}
	return e.Info, true
}

// Count returns the number of currently tracked peers.
func (r *PeerRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.peers)
}

// All returns a snapshot of all currently known peers as a slice.
// The slice is a copy – callers may mutate it freely.
func (r *PeerRegistry) All() []models.NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]models.NodeInfo, 0, len(r.peers))
	for _, e := range r.peers {
		result = append(result, e.Info)
	}
	return result
}

// AllExcept returns all known peers except the one matching excludeID.
// Used by the gossip re-broadcast logic to avoid sending a packet back to its
// original sender.
func (r *PeerRegistry) AllExcept(excludeID string) []models.NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]models.NodeInfo, 0, len(r.peers))
	for id, e := range r.peers {
		if id != excludeID {
			result = append(result, e.Info)
		}
	}
	return result
}

// UDPAddresses returns "ip:port" strings for all known peers, excluding the
// peer identified by excludeID.  Convenience wrapper used by the UDP gossip
// fan-out sender.
func (r *PeerRegistry) UDPAddresses(excludeID string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]string, 0, len(r.peers))
	for id, e := range r.peers {
		if id != excludeID {
			result = append(result, fmt.Sprintf("%s:%d", e.Info.IP, e.Info.UDPPort))
		}
	}
	return result
}

// IsKnown reports whether nodeID is present in the registry.
func (r *PeerRegistry) IsKnown(nodeID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.peers[nodeID]
	return ok
}
