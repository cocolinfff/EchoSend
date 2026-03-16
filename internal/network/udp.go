// udp.go – UDP Gossip Engine for EchoSend
//
// Responsibilities:
//
//  1. Listen on the configured UDP port for incoming gossip packets
//     (PRESENCE, MSG, FILE_META, SYNC_REQ).
//  2. Deduplicate packets by PacketID using a time-expiring seen-set so
//     that circular re-broadcasts terminate naturally.
//  3. Re-broadcast (flood) every novel packet to all known peers except
//     the original sender, decrementing the TTL counter each hop.
//  4. Emit periodic PRESENCE heartbeats to:
//     - the subnet broadcast address(es) and 255.255.255.255
//     - all explicitly configured static peers (single IPs and CIDR expansions)
//     with a token-bucket rate-limiter to protect large /16 probes.
//  5. Expose a Send helper so other subsystems (IPC handler, file-sync)
//     can inject packets into the gossip mesh.
//
// Performance constraints honoured:
//   - UDP receive buffers are recycled via sync.Pool → near-zero allocation
//     on the hot receive path, keeping GC pauses minimal.
//   - Static-peer unicast probing uses a worker-pool + ticker rate limiter
//     (ProbeRatePerSec) to avoid bursting the kernel UDP send buffer.
package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"p2p-sync/internal/config"
	"p2p-sync/internal/models"
	"p2p-sync/internal/storage"
	"p2p-sync/internal/utils"
)

// ─────────────────────────────────────────────────────────────────────────────
// Constants
// ─────────────────────────────────────────────────────────────────────────────

const (
	// udpMaxDatagramSize is the maximum UDP payload we will read into a buffer.
	// Standard Ethernet MTU is 1500; we stay well below the theoretical 65 507
	// byte limit and pick a size comfortable for gossip envelopes.
	udpMaxDatagramSize = 8192

	// defaultGossipTTL is the initial hop-count placed into every locally
	// originated packet.  Each forwarding hop decrements it by one; when it
	// reaches 0 the packet is dropped rather than re-broadcast.
	defaultGossipTTL = 8

	// seenMapCleanupInterval controls how often the expired-entry GC sweep
	// runs on the seen-packet map.
	seenMapCleanupInterval = 30 * time.Second

	// peerPruneInterval controls how often stale peers are evicted from the
	// in-memory registry.
	peerPruneInterval = 60 * time.Second

	// peerStaleAge is the maximum time a peer can be silent before being
	// considered gone and removed from the registry.
	peerStaleAge = 3 * time.Minute

	// probeWorkers is the number of concurrent goroutines used when sending
	// unicast PRESENCE probes to static-peer CIDR expansions.
	probeWorkers = 16
)

// ─────────────────────────────────────────────────────────────────────────────
// seenEntry
// ─────────────────────────────────────────────────────────────────────────────

type seenEntry struct {
	expiresAt time.Time
}

// ─────────────────────────────────────────────────────────────────────────────
// PacketHandler
// ─────────────────────────────────────────────────────────────────────────────

// PacketHandler is a callback invoked by the UDP engine for every novel,
// semantically valid packet received from the network.  Implementations must
// be goroutine-safe and should return quickly; heavy work must be dispatched
// to a separate goroutine.
type PacketHandler func(pkt models.Packet, fromAddr *net.UDPAddr)

// ─────────────────────────────────────────────────────────────────────────────
// UDPEngine
// ─────────────────────────────────────────────────────────────────────────────

// UDPEngine is the core gossip subsystem.  Create one with NewUDPEngine and
// start it with Run.  It is safe to call Send from any goroutine once Run
// has been called.
type UDPEngine struct {
	cfg      *config.Config
	store    *storage.Store
	peers    *PeerRegistry
	selfID   string   // this node's NodeID
	selfName string   // this node's NodeName
	localIPs []string // non-loopback IPv4 addresses of this host

	conn *net.UDPConn // the single shared UDP socket

	// bufPool recycles fixed-size byte slices used for reading UDP datagrams.
	// This is the primary GC-pressure reduction mechanism: on a busy LAN with
	// hundreds of packets/second, reusing these buffers avoids thousands of
	// short-lived allocations per second.
	bufPool sync.Pool

	// seenMu protects seenPkts.
	seenMu   sync.Mutex
	seenPkts map[string]seenEntry // PacketID → expiry time
	seenTTL  time.Duration        // how long to remember a seen packet

	// handlers are invoked (in order of registration) for each novel packet.
	handlerMu sync.RWMutex
	handlers  []PacketHandler
}

// NewUDPEngine constructs a UDPEngine.  It does not open any sockets yet;
// call Run to start listening and emitting.
func NewUDPEngine(
	cfg *config.Config,
	store *storage.Store,
	peers *PeerRegistry,
) *UDPEngine {
	localIPs, _ := utils.LocalIPv4Addresses()

	return &UDPEngine{
		cfg:      cfg,
		store:    store,
		peers:    peers,
		selfID:   cfg.NodeID,
		selfName: cfg.NodeName,
		localIPs: localIPs,
		seenPkts: make(map[string]seenEntry),
		seenTTL:  time.Duration(cfg.SeenPacketTTLSec) * time.Second,
		bufPool: sync.Pool{
			New: func() interface{} {
				b := make([]byte, udpMaxDatagramSize)
				return &b
			},
		},
	}
}

// AddHandler registers a PacketHandler that will be called for every novel
// packet the engine receives.  May be called before or after Run.
func (e *UDPEngine) AddHandler(h PacketHandler) {
	e.handlerMu.Lock()
	e.handlers = append(e.handlers, h)
	e.handlerMu.Unlock()
}

// ─────────────────────────────────────────────────────────────────────────────
// Run
// ─────────────────────────────────────────────────────────────────────────────

// Run opens the UDP socket, starts the receive loop, the heartbeat emitter,
// and the background maintenance goroutines.  It blocks until ctx is
// cancelled, then performs a clean shutdown.
func (e *UDPEngine) Run(ctx context.Context) error {
	addr := &net.UDPAddr{
		IP:   net.IPv4zero,
		Port: e.cfg.DaemonPortUDP,
	}

	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("udp: listen :%d: %w", e.cfg.DaemonPortUDP, err)
	}
	e.conn = conn
	log.Printf("[udp] listening on :%d", e.cfg.DaemonPortUDP)

	// Allow UDP broadcast packets to be sent.
	if err := setBroadcast(conn); err != nil {
		log.Printf("[udp] warning: could not enable SO_BROADCAST: %v", err)
	}

	// Start all background goroutines.
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		e.receiveLoop(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		e.heartbeatLoop(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		e.maintenanceLoop(ctx)
	}()

	// Block until context is done, then close the socket to unblock receiveLoop.
	<-ctx.Done()
	_ = conn.Close()
	wg.Wait()
	log.Printf("[udp] engine stopped")
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Receive loop
// ─────────────────────────────────────────────────────────────────────────────

// receiveLoop reads datagrams from the UDP socket until the context is
// cancelled.  Each datagram is processed in its own lightweight goroutine so
// that a slow handler does not stall ingestion.
func (e *UDPEngine) receiveLoop(ctx context.Context) {
	for {
		// Borrow a buffer from the pool.
		bufPtr := e.bufPool.Get().(*[]byte)
		buf := *bufPtr

		n, addr, err := e.conn.ReadFromUDP(buf)
		if err != nil {
			// Return the buffer before possibly exiting.
			e.bufPool.Put(bufPtr)

			select {
			case <-ctx.Done():
				return // clean shutdown: socket was closed by Run
			default:
				log.Printf("[udp] read error: %v", err)
				continue
			}
		}

		// Copy the payload out of the pooled buffer before spawning the
		// goroutine, then return the buffer immediately so the pool stays hot.
		data := make([]byte, n)
		copy(data, buf[:n])
		e.bufPool.Put(bufPtr)

		go e.handleDatagram(ctx, data, addr)
	}
}

// handleDatagram deserialises, deduplicates, processes and re-broadcasts a
// single UDP datagram.
func (e *UDPEngine) handleDatagram(_ context.Context, data []byte, from *net.UDPAddr) {
	var pkt models.Packet
	if err := json.Unmarshal(data, &pkt); err != nil {
		// Malformed packet – silently discard (could be non-EchoSend traffic).
		return
	}

	// Drop packets originating from ourselves to avoid self-feedback.
	if pkt.SenderID == e.selfID {
		return
	}

	// Deduplication: have we already processed this PacketID?
	if e.markSeen(pkt.PacketID) {
		return // duplicate – discard
	}

	// Dispatch to all registered handlers.
	e.handlerMu.RLock()
	handlers := e.handlers
	e.handlerMu.RUnlock()

	for _, h := range handlers {
		h(pkt, from)
	}

	// Gossip re-broadcast: decrement TTL and forward to all known peers
	// except the original sender.
	if pkt.TTL > 1 {
		pkt.TTL--
		e.floodToPeers(data, pkt, pkt.SenderID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Send (public API)
// ─────────────────────────────────────────────────────────────────────────────

// Send serialises pkt and broadcasts it to all known peers plus the LAN
// broadcast address(es).  The packet is also marked as seen locally so that
// any reflection from the network is suppressed.
//
// pkt.TTL is set to defaultGossipTTL if it is zero.
// pkt.SenderID and pkt.Timestamp are set if empty/zero.
func (e *UDPEngine) Send(pkt models.Packet) error {
	if pkt.SenderID == "" {
		pkt.SenderID = e.selfID
	}
	if pkt.Timestamp == 0 {
		pkt.Timestamp = time.Now().UnixNano()
	}
	if pkt.TTL == 0 {
		pkt.TTL = defaultGossipTTL
	}
	if pkt.SenderIP == "" {
		if len(e.localIPs) > 0 {
			pkt.SenderIP = e.localIPs[0]
		}
	}

	data, err := json.Marshal(pkt)
	if err != nil {
		return fmt.Errorf("udp: marshal packet: %w", err)
	}

	// Mark as seen before sending so reflected copies are suppressed.
	e.markSeen(pkt.PacketID)

	// Broadcast to LAN broadcast addresses.
	bcastAddrs, _ := utils.LocalBroadcastAddresses()
	for _, bcast := range bcastAddrs {
		target := fmt.Sprintf("%s:%d", bcast, e.cfg.DaemonPortUDP)
		_ = e.sendRaw(data, target)
	}

	// Also unicast directly to all currently-known peers for reliability
	// (broadcast may be blocked on some managed switches).
	for _, addr := range e.peers.UDPAddresses(e.selfID) {
		_ = e.sendRaw(data, addr)
	}

	return nil
}

// sendRaw writes data to the given "host:port" UDP address.
// It borrows a connection from the pool (or simply uses WriteTo on e.conn).
func (e *UDPEngine) sendRaw(data []byte, addr string) error {
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return err
	}
	if e.conn == nil {
		return fmt.Errorf("udp: engine not started")
	}
	_, err = e.conn.WriteToUDP(data, udpAddr)
	return err
}

// floodToPeers re-broadcasts the already-serialised packet data to all known
// peers except excludeID.  If re-serialisation is needed (TTL was decremented)
// the packet is re-marshalled.
func (e *UDPEngine) floodToPeers(originalData []byte, pkt models.Packet, excludeID string) {
	// Re-serialise only if TTL was decremented (i.e. the data changed).
	var data []byte
	if pkt.TTL != defaultGossipTTL {
		var err error
		data, err = json.Marshal(pkt)
		if err != nil {
			return
		}
	} else {
		data = originalData
	}

	for _, addr := range e.peers.UDPAddresses(excludeID) {
		_ = e.sendRaw(data, addr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Seen-packet deduplication
// ─────────────────────────────────────────────────────────────────────────────

// markSeen records that PacketID has been processed and returns true if it
// was already in the seen-set (i.e. this is a duplicate).
// The TTL for the entry is e.seenTTL.
func (e *UDPEngine) markSeen(packetID string) (duplicate bool) {
	now := time.Now()

	e.seenMu.Lock()
	defer e.seenMu.Unlock()

	if entry, ok := e.seenPkts[packetID]; ok {
		if now.Before(entry.expiresAt) {
			return true // still within TTL window → duplicate
		}
		// Entry has expired: treat as new but refresh it.
	}

	e.seenPkts[packetID] = seenEntry{expiresAt: now.Add(e.seenTTL)}
	return false
}

// sweepSeen removes expired entries from the seen-packet map.
// Called by the maintenance loop; must not be called concurrently with itself.
func (e *UDPEngine) sweepSeen() {
	now := time.Now()

	e.seenMu.Lock()
	defer e.seenMu.Unlock()

	for id, entry := range e.seenPkts {
		if now.After(entry.expiresAt) {
			delete(e.seenPkts, id)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Heartbeat emitter
// ─────────────────────────────────────────────────────────────────────────────

// heartbeatLoop emits PRESENCE packets at the configured interval until ctx
// is cancelled.
func (e *UDPEngine) heartbeatLoop(ctx context.Context) {
	interval := time.Duration(e.cfg.HeartbeatIntervalSec) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Emit one immediately on startup so peers learn about us fast.
	e.emitPresence(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.emitPresence(ctx)
		}
	}
}

// emitPresence constructs a PRESENCE packet, broadcasts it to LAN broadcast
// addresses, and probes all static peers (with rate limiting).
func (e *UDPEngine) emitPresence(ctx context.Context) {
	node := models.NodeInfo{
		NodeID:   e.selfID,
		NodeName: e.selfName,
		UDPPort:  e.cfg.DaemonPortUDP,
		TCPPort:  e.cfg.DaemonPortTCP,
		LastSeen: time.Now().UnixNano(),
	}
	if len(e.localIPs) > 0 {
		node.IP = e.localIPs[0]
	}

	payload, err := json.Marshal(node)
	if err != nil {
		log.Printf("[udp] heartbeat: marshal presence: %v", err)
		return
	}

	pkt := models.Packet{
		PacketID:  newPacketID(),
		SenderID:  e.selfID,
		SenderIP:  node.IP,
		Type:      models.PacketPresence,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
		TTL:       defaultGossipTTL,
	}

	pktData, err := json.Marshal(pkt)
	if err != nil {
		log.Printf("[udp] heartbeat: marshal packet: %v", err)
		return
	}

	// Mark as seen before sending.
	e.markSeen(pkt.PacketID)

	// 1. LAN broadcast (subnet broadcast + 255.255.255.255).
	bcastAddrs, err := utils.LocalBroadcastAddresses()
	if err != nil {
		log.Printf("[udp] heartbeat: get broadcast addrs: %v", err)
	}
	for _, bcast := range bcastAddrs {
		target := fmt.Sprintf("%s:%d", bcast, e.cfg.DaemonPortUDP)
		if err := e.sendRaw(pktData, target); err != nil {
			log.Printf("[udp] heartbeat: broadcast to %s: %v", target, err)
		}
	}

	// 2. Unicast to all already-known peers (improves reliability when
	//    broadcast is blocked by managed switches).
	for _, addr := range e.peers.UDPAddresses(e.selfID) {
		_ = e.sendRaw(pktData, addr)
	}

	// 3. Static-peer probing (may involve large CIDR expansions).
	if len(e.cfg.StaticPeers) > 0 {
		go e.probeStaticPeers(ctx, pktData)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Static-peer probing with rate limiting
// ─────────────────────────────────────────────────────────────────────────────

// probeStaticPeers expands all StaticPeers entries (single IPs or CIDR blocks)
// into individual target addresses and sends a unicast PRESENCE packet to each
// one.  A token-bucket approach (ticker + buffered worker pool) ensures we
// never exceed cfg.ProbeRatePerSec packets per second, guarding against
// saturating the UDP send buffer when a /16 or similar large block is
// configured.
func (e *UDPEngine) probeStaticPeers(ctx context.Context, pktData []byte) {
	// Expand all static peer entries into a flat list of IP strings.
	var targets []string
	for _, entry := range e.cfg.StaticPeers {
		ips, err := utils.ParseTarget(entry)
		if err != nil {
			log.Printf("[udp] static peer parse %q: %v", entry, err)
			continue
		}
		targets = append(targets, ips...)
	}
	if len(targets) == 0 {
		return
	}

	// Calculate the inter-packet delay for the configured rate limit.
	// probeRatePerSec = 1000 → delay = 1 ms per packet.
	ratePerSec := e.cfg.ProbeRatePerSec
	if ratePerSec <= 0 {
		ratePerSec = 1000
	}
	delay := time.Second / time.Duration(ratePerSec)

	// Use a ticker as a token source and a work channel to fan out across
	// probeWorkers goroutines.
	workCh := make(chan string, probeWorkers*2)
	ticker := time.NewTicker(delay)
	defer ticker.Stop()

	// Worker pool.
	var wg sync.WaitGroup
	for i := 0; i < probeWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range workCh {
				target := fmt.Sprintf("%s:%d", ip, e.cfg.DaemonPortUDP)
				if err := e.sendRaw(pktData, target); err != nil {
					// Not logged individually – could be millions of unreachable IPs.
					_ = err
				}
			}
		}()
	}

	// Feed work at the rate-limited pace.
	for _, ip := range targets {
		select {
		case <-ctx.Done():
			close(workCh)
			wg.Wait()
			return
		case <-ticker.C:
			workCh <- ip
		}
	}

	close(workCh)
	wg.Wait()
}

// ─────────────────────────────────────────────────────────────────────────────
// Maintenance loop
// ─────────────────────────────────────────────────────────────────────────────

// maintenanceLoop runs periodic housekeeping tasks:
//   - sweeping expired entries from the seen-packet map
//   - pruning stale peers from the in-memory registry
func (e *UDPEngine) maintenanceLoop(ctx context.Context) {
	seenTicker := time.NewTicker(seenMapCleanupInterval)
	peerTicker := time.NewTicker(peerPruneInterval)
	defer seenTicker.Stop()
	defer peerTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-seenTicker.C:
			before := e.seenMapLen()
			e.sweepSeen()
			after := e.seenMapLen()
			if before != after {
				log.Printf("[udp] seen-map GC: %d → %d entries", before, after)
			}
		case <-peerTicker.C:
			pruned := e.peers.PruneExpired(peerStaleAge)
			if len(pruned) > 0 {
				log.Printf("[udp] pruned %d stale peer(s): %v", len(pruned), pruned)
			}
		}
	}
}

// seenMapLen returns the current number of entries in the seen-packet map.
// Used only for diagnostic logging.
func (e *UDPEngine) seenMapLen() int {
	e.seenMu.Lock()
	n := len(e.seenPkts)
	e.seenMu.Unlock()
	return n
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// newPacketID generates a 128-bit random hex string suitable for use as a
// globally unique PacketID.  It is intentionally a standalone function (not a
// method) so it can be called from other packages via a thin wrapper.
func newPacketID() string {
	// Use a pool-backed buffer to avoid an allocation on the crypto/rand path.
	b := make([]byte, 16)
	// math/rand would be faster but crypto/rand eliminates any collision risk
	// across thousands of nodes with clock skew.
	if _, err := cryptoRandRead(b); err != nil {
		// Fallback: use timestamp + pid combination (practically unique enough).
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), pseudoCounter.Add(1))
	}
	return fmt.Sprintf("%x", b)
}

// NewPacketID is the exported wrapper for use by the IPC handler and other
// subsystems that need to stamp a fresh PacketID onto an outgoing packet.
func NewPacketID() string { return newPacketID() }
