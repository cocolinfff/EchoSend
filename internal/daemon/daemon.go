// Package daemon implements the EchoSend P2P daemon orchestrator.
//
// The Daemon struct is the top-level coordinator that wires every subsystem
// together, manages their lifetimes and handles OS signals for graceful
// shutdown.
//
// Startup order:
//  1. Open BoltDB storage
//  2. Build in-memory peer registry (warm from DB)
//  3. Create UDP gossip engine
//  4. Create TCP file server
//  5. Create file-sync orchestrator (registers itself as a UDP handler)
//  6. Create IPC HTTP server
//  7. Register all UDP packet handlers (PRESENCE, MSG, FILE_META)
//  8. Launch all subsystems as concurrent goroutines under a shared context
//  9. Block until SIGINT/SIGTERM, then drain gracefully
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"p2p-sync/internal/config"
	"p2p-sync/internal/filesync"
	"p2p-sync/internal/ipc"
	"p2p-sync/internal/models"
	"p2p-sync/internal/network"
	"p2p-sync/internal/storage"
)

// ─────────────────────────────────────────────────────────────────────────────
// Daemon
// ─────────────────────────────────────────────────────────────────────────────

// Daemon is the top-level runtime that holds references to all active
// subsystems and coordinates their start/stop lifecycle.
type Daemon struct {
	cfg   *config.Config
	store *storage.Store

	peers  *network.PeerRegistry
	udp    *network.UDPEngine
	tcp    *network.TCPServer
	syncer *filesync.Orchestrator
	ipcSrv *ipc.Server
}

// New constructs a Daemon from the given config.  Storage is opened
// immediately; network sockets are not bound until Run is called.
func New(cfg *config.Config) (*Daemon, error) {
	// ── 1. Storage ────────────────────────────────────────────────────────────
	store, err := storage.Open(cfg.StorageDir)
	if err != nil {
		return nil, fmt.Errorf("daemon: open storage: %w", err)
	}

	// ── 2. Peer registry ──────────────────────────────────────────────────────
	peers := network.NewPeerRegistry(store)
	if err := peers.LoadFromStore(); err != nil {
		// Non-fatal: the registry starts empty and fills up from heartbeats.
		log.Printf("[daemon] warning: could not pre-warm peer registry: %v", err)
	}

	// ── 3. UDP gossip engine ──────────────────────────────────────────────────
	udpEngine := network.NewUDPEngine(cfg, store, peers)

	// ── 4. TCP file server ────────────────────────────────────────────────────
	tcpServer := network.NewTCPServer(cfg, store)

	// ── 5. File-sync orchestrator ─────────────────────────────────────────────
	syncer := filesync.New(cfg, store, udpEngine)

	// ── 6. IPC HTTP server ────────────────────────────────────────────────────
	ipcServer := ipc.NewServer(cfg, store, udpEngine, syncer, peers)

	d := &Daemon{
		cfg:    cfg,
		store:  store,
		peers:  peers,
		udp:    udpEngine,
		tcp:    tcpServer,
		syncer: syncer,
		ipcSrv: ipcServer,
	}

	// ── 7. Register UDP packet handlers ──────────────────────────────────────
	d.registerHandlers()

	return d, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Run
// ─────────────────────────────────────────────────────────────────────────────

// Run starts all subsystems and blocks until a SIGINT or SIGTERM is received
// (or until the provided context is cancelled by the caller).
// It performs a graceful shutdown and returns nil on clean exit.
func (d *Daemon) Run(ctx context.Context) error {
	log.Printf("[daemon] starting EchoSend daemon  node=%s  id=%s",
		d.cfg.NodeName, d.cfg.NodeID)

	// Reset any downloads left in DOWNLOADING state from a previous crashed run.
	if err := d.syncer.ResumePendingDownloads(); err != nil {
		log.Printf("[daemon] warning: resume pending downloads: %v", err)
	}

	// ── Derive a cancellable context for all subsystems ───────────────────────
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// ── Listen for OS signals ─────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// ── Launch subsystems ─────────────────────────────────────────────────────
	// Each subsystem runs in its own goroutine.  Errors are forwarded on errCh.
	errCh := make(chan error, 4)

	go func() {
		if err := d.udp.Run(runCtx); err != nil {
			errCh <- fmt.Errorf("udp engine: %w", err)
		}
	}()

	go func() {
		if err := d.tcp.Run(runCtx); err != nil {
			errCh <- fmt.Errorf("tcp server: %w", err)
		}
	}()

	go func() {
		if err := d.ipcSrv.Run(runCtx); err != nil {
			errCh <- fmt.Errorf("ipc server: %w", err)
		}
	}()

	// Background housekeeping (DB pruning, stats logging).
	go d.housekeepingLoop(runCtx)

	log.Printf("[daemon] all subsystems started")
	log.Printf("[daemon] UDP gossip  → :%d", d.cfg.DaemonPortUDP)
	log.Printf("[daemon] TCP files   → :%d", d.cfg.DaemonPortTCP)
	log.Printf("[daemon] IPC HTTP    → 127.0.0.1:%d", d.cfg.DaemonPortIPC)
	log.Printf("[daemon] storage dir → %s", d.cfg.StorageDir)

	// ── Block until shutdown ──────────────────────────────────────────────────
	select {
	case sig := <-sigCh:
		log.Printf("[daemon] received signal %s – shutting down", sig)
	case err := <-errCh:
		log.Printf("[daemon] subsystem error – shutting down: %v", err)
	case <-ctx.Done():
		log.Printf("[daemon] context cancelled – shutting down")
	}

	// Cancel the run context to trigger all subsystem shutdown paths.
	cancel()

	// Give subsystems a moment to drain gracefully.
	log.Printf("[daemon] waiting for subsystems to stop…")
	time.Sleep(500 * time.Millisecond)

	// ── Flush and close storage ───────────────────────────────────────────────
	if err := d.store.Close(); err != nil {
		log.Printf("[daemon] warning: close storage: %v", err)
	}

	log.Printf("[daemon] shutdown complete")
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// UDP packet handler registration
// ─────────────────────────────────────────────────────────────────────────────

// registerHandlers wires all domain-level packet handlers into the UDP engine.
// Each handler is invoked once per novel (non-duplicate) gossip packet.
func (d *Daemon) registerHandlers() {
	d.udp.AddHandler(func(pkt models.Packet, from *net.UDPAddr) {
		switch pkt.Type {
		case models.PacketPresence:
			d.handlePresence(pkt, from)
		case models.PacketMsg:
			d.handleMessage(pkt, from)
		case models.PacketFileMeta:
			d.syncer.HandleFileMeta(pkt, from)
		case models.PacketSyncReq:
			// Reserved for future use (e.g. explicit pull requests).
			log.Printf("[daemon] SYNC_REQ from %s (not yet implemented)", pkt.SenderID)
		default:
			log.Printf("[daemon] unknown packet type %q from %s", pkt.Type, pkt.SenderID)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// PRESENCE handler
// ─────────────────────────────────────────────────────────────────────────────

// handlePresence processes an incoming PRESENCE heartbeat packet.
// It deserialises the NodeInfo payload and upserts the peer into the registry.
func (d *Daemon) handlePresence(pkt models.Packet, from *net.UDPAddr) {
	var node models.NodeInfo
	if err := json.Unmarshal(pkt.Payload, &node); err != nil {
		log.Printf("[daemon] malformed PRESENCE payload from %s: %v", pkt.SenderID, err)
		return
	}

	// Fill in the IP from the packet envelope or the UDP source address
	// (the embedded IP may be 0.0.0.0 on multi-homed nodes).
	if node.IP == "" || node.IP == "0.0.0.0" {
		if from != nil {
			node.IP = from.IP.String()
		}
	}
	if node.IP == "" && pkt.SenderIP != "" {
		node.IP = pkt.SenderIP
	}

	// Ensure mandatory fields are present before persisting.
	if node.NodeID == "" {
		node.NodeID = pkt.SenderID
	}
	if node.NodeID == "" {
		return // cannot index without an ID
	}

	isNew := d.peers.Upsert(node)
	if isNew {
		log.Printf("[daemon] new peer discovered: %s (%s) at %s:%d",
			node.NodeName, node.NodeID[:8], node.IP, node.UDPPort)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// MSG handler
// ─────────────────────────────────────────────────────────────────────────────

// handleMessage processes an incoming MSG gossip packet.
// It deserialises the Message payload, deduplicates it, and persists it.
func (d *Daemon) handleMessage(pkt models.Packet, from *net.UDPAddr) {
	var msg models.Message
	if err := json.Unmarshal(pkt.Payload, &msg); err != nil {
		log.Printf("[daemon] malformed MSG payload from %s: %v", pkt.SenderID, err)
		return
	}

	// Guarantee the message ID matches the packet ID so the deduplication
	// key is consistent between the gossip layer and the storage layer.
	if msg.MessageID == "" {
		msg.MessageID = pkt.PacketID
	}
	if msg.SenderID == "" {
		msg.SenderID = pkt.SenderID
	}
	if msg.SenderIP == "" {
		msg.SenderIP = pkt.SenderIP
		if msg.SenderIP == "" && from != nil {
			msg.SenderIP = from.IP.String()
		}
	}
	if msg.Timestamp == 0 {
		msg.Timestamp = pkt.Timestamp
	}

	err := d.store.InsertMessage(msg)
	switch err {
	case nil:
		log.Printf("[daemon] 💬 [%s] %s: %s",
			formatTime(msg.Timestamp), msg.SenderName, msg.Content)
	case storage.ErrDuplicateMessage:
		// Already stored by an earlier gossip hop – benign, ignore.
	default:
		log.Printf("[daemon] store message from %s: %v", pkt.SenderID, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Background housekeeping
// ─────────────────────────────────────────────────────────────────────────────

// housekeepingLoop performs periodic maintenance tasks that don't belong in
// any single subsystem:
//   - Pruning stale node records from BoltDB (peers that haven't sent a
//     PRESENCE for > 10 minutes)
//   - Logging a brief stats summary every minute
func (d *Daemon) housekeepingLoop(ctx context.Context) {
	pruneTicker := time.NewTicker(5 * time.Minute)
	statsTicker := time.NewTicker(1 * time.Minute)
	defer pruneTicker.Stop()
	defer statsTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-pruneTicker.C:
			n, err := d.store.PruneStaleNodes(10 * time.Minute)
			if err != nil {
				log.Printf("[daemon] housekeeping: prune nodes: %v", err)
			} else if n > 0 {
				log.Printf("[daemon] housekeeping: pruned %d stale node record(s)", n)
			}

		case <-statsTicker.C:
			nodes, msgs, files, err := d.store.Stats()
			if err != nil {
				log.Printf("[daemon] housekeeping: stats: %v", err)
				continue
			}
			log.Printf("[daemon] stats — peers(db)=%d peers(live)=%d messages=%d files=%d",
				nodes, d.peers.Count(), msgs, files)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// formatTime converts a Unix nanosecond timestamp to a compact local time
// string for log output.
func formatTime(unixNano int64) string {
	return time.Unix(0, unixNano).Local().Format("15:04:05")
}
