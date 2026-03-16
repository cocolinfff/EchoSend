// Package ipc implements the localhost-only HTTP server (daemon side) and the
// corresponding HTTP client (CLI side) that together form the IPC channel
// between the EchoSend daemon process and the CLI sub-commands.
//
// Why HTTP over a raw socket?
//   - Works identically on Windows (no Unix-domain socket support pre-Go 1.23),
//     Linux and macOS without build tags.
//   - The standard library's net/http gives us framing, request routing, and
//     JSON serialisation for free.
//   - Binding exclusively to 127.0.0.1 means the extra surface area is
//     invisible to the rest of the network.
//
// API surface:
//
//	POST /api/send/message   – broadcast a text message to the LAN mesh
//	POST /api/send/file      – hash a local file and broadcast its FILE_META
//	POST /api/peers/add      – add a static peer (IP or CIDR) to the config
//	GET  /api/history        – return messages + file records (newest first)
//	GET  /api/status         – return node ID, name and known-peer list
//	GET  /api/ping           – liveness probe used by CLI to detect a running daemon
package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"p2p-sync/internal/config"
	"p2p-sync/internal/models"
	"p2p-sync/internal/network"
	"p2p-sync/internal/storage"
)

// ─────────────────────────────────────────────────────────────────────────────
// Dependency interfaces
// ─────────────────────────────────────────────────────────────────────────────

// FilePublisher is implemented by filesync.Orchestrator.
// Defined here as an interface to avoid an import cycle.
type FilePublisher interface {
	PublishFile(localPath string) error
}

// GossipSender is implemented by network.UDPEngine.
type GossipSender interface {
	Send(pkt models.Packet) error
}

// PeerAdder is implemented by config.Config.
type PeerAdder interface {
	AddStaticPeer(target string) error
}

// PeerLister is implemented by network.PeerRegistry.
type PeerLister interface {
	All() []models.NodeInfo
	Count() int
}

// ─────────────────────────────────────────────────────────────────────────────
// Server
// ─────────────────────────────────────────────────────────────────────────────

// Server is the daemon-side IPC HTTP server.  It binds exclusively to
// 127.0.0.1 so that only processes on the same host can reach it.
type Server struct {
	cfg       *config.Config
	store     *storage.Store
	gossip    GossipSender
	publisher FilePublisher
	peers     PeerLister
	peerAdder PeerAdder

	httpServer *http.Server
}

// NewServer constructs a Server.  All dependency arguments are required.
func NewServer(
	cfg *config.Config,
	store *storage.Store,
	gossip GossipSender,
	publisher FilePublisher,
	peers PeerLister,
) *Server {
	s := &Server{
		cfg:       cfg,
		store:     store,
		gossip:    gossip,
		publisher: publisher,
		peers:     peers,
		peerAdder: cfg, // config.Config implements AddStaticPeer
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", s.handlePing)
	mux.HandleFunc("/api/send/message", s.handleSendMessage)
	mux.HandleFunc("/api/send/file", s.handleSendFile)
	mux.HandleFunc("/api/peers/add", s.handleAddPeer)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/status", s.handleStatus)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("127.0.0.1:%d", cfg.DaemonPortIPC),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return s
}

// Run starts the HTTP server and blocks until ctx is cancelled.
// After context cancellation a graceful 5-second shutdown is attempted.
func (s *Server) Run(ctx context.Context) error {
	// Bind explicitly to 127.0.0.1 to ensure we never accidentally accept
	// connections from the network even if the port is also open on 0.0.0.0.
	ln, err := net.Listen("tcp4", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("ipc: listen %s: %w", s.httpServer.Addr, err)
	}

	log.Printf("[ipc] HTTP server listening on %s (localhost only)", s.httpServer.Addr)

	// Shutdown goroutine: triggers graceful stop when the parent context ends.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(shutCtx); err != nil {
			log.Printf("[ipc] shutdown: %v", err)
		}
	}()

	if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("ipc: serve: %w", err)
	}
	log.Printf("[ipc] server stopped")
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Handlers
// ─────────────────────────────────────────────────────────────────────────────

// handlePing is a trivial liveness probe.  The CLI uses it to detect whether
// a daemon is already running before deciding to start one.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, models.IPCReply{
		OK:      true,
		Message: "pong",
		Data: map[string]interface{}{
			"node_id":   s.cfg.NodeID,
			"node_name": s.cfg.NodeName,
		},
	})
}

// handleSendMessage broadcasts a text message to the gossip mesh.
//
//	POST /api/send/message
//	Body: {"content": "hello world"}
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorReply("method not allowed"))
		return
	}

	var req models.IPCSendMessageReq
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorReply(err.Error()))
		return
	}
	if req.Content == "" {
		writeJSON(w, http.StatusBadRequest, errorReply("content must not be empty"))
		return
	}

	// Build the Message record.
	msg := models.Message{
		MessageID:  network.NewPacketID(),
		SenderID:   s.cfg.NodeID,
		SenderName: s.cfg.NodeName,
		Content:    req.Content,
		Timestamp:  time.Now().UnixNano(),
	}

	// Persist locally first.
	if err := s.store.InsertMessage(msg); err != nil && err != storage.ErrDuplicateMessage {
		log.Printf("[ipc] send message: db insert: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorReply("database error"))
		return
	}

	// Serialise as the packet payload.
	payload, err := json.Marshal(msg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorReply("serialise error"))
		return
	}

	pkt := models.Packet{
		PacketID:  msg.MessageID,
		SenderID:  s.cfg.NodeID,
		Type:      models.PacketMsg,
		Payload:   payload,
		Timestamp: msg.Timestamp,
	}

	if err := s.gossip.Send(pkt); err != nil {
		log.Printf("[ipc] send message: gossip: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorReply("gossip send failed: "+err.Error()))
		return
	}

	log.Printf("[ipc] message sent: %q", req.Content)
	writeJSON(w, http.StatusOK, models.IPCReply{
		OK:      true,
		Message: "message sent",
		Data:    msg,
	})
}

// handleSendFile hashes a local file and broadcasts its FILE_META.
//
//	POST /api/send/file
//	Body: {"path": "/absolute/or/relative/path/to/file"}
func (s *Server) handleSendFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorReply("method not allowed"))
		return
	}

	var req models.IPCSendFileReq
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorReply(err.Error()))
		return
	}
	if req.Path == "" {
		writeJSON(w, http.StatusBadRequest, errorReply("path must not be empty"))
		return
	}

	// PublishFile is potentially slow (SHA-256 of a large file) so run it in a
	// goroutine and return an "accepted" response immediately.  The caller can
	// watch --history to see when the broadcast appears.
	go func() {
		if err := s.publisher.PublishFile(req.Path); err != nil {
			log.Printf("[ipc] publish file %s: %v", req.Path, err)
		}
	}()

	writeJSON(w, http.StatusAccepted, models.IPCReply{
		OK:      true,
		Message: fmt.Sprintf("hashing and broadcasting %s (asynchronous)", req.Path),
	})
}

// handleAddPeer adds a new static peer entry (IP or CIDR) to the config.
//
//	POST /api/peers/add
//	Body: {"target": "192.168.2.100"}  or  {"target": "10.0.0.0/24"}
func (s *Server) handleAddPeer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorReply("method not allowed"))
		return
	}

	var req models.IPCAddPeerReq
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorReply(err.Error()))
		return
	}
	if req.Target == "" {
		writeJSON(w, http.StatusBadRequest, errorReply("target must not be empty"))
		return
	}

	if err := s.peerAdder.AddStaticPeer(req.Target); err != nil {
		log.Printf("[ipc] add peer %s: %v", req.Target, err)
		writeJSON(w, http.StatusInternalServerError, errorReply(err.Error()))
		return
	}

	log.Printf("[ipc] static peer added: %s", req.Target)
	writeJSON(w, http.StatusOK, models.IPCReply{
		OK:      true,
		Message: fmt.Sprintf("peer %q added to static_peers and persisted to config", req.Target),
	})
}

// handleHistory returns a combined, newest-first history of messages and files.
//
//	GET /api/history?limit=50
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorReply("method not allowed"))
		return
	}

	limit := 50
	if lv := r.URL.Query().Get("limit"); lv != "" {
		if _, err := fmt.Sscan(lv, &limit); err != nil || limit < 0 {
			limit = 50
		}
	}

	msgs, err := s.store.ListMessages(limit)
	if err != nil {
		log.Printf("[ipc] history: list messages: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorReply("db error: messages"))
		return
	}

	files, err := s.store.ListFiles(limit)
	if err != nil {
		log.Printf("[ipc] history: list files: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorReply("db error: files"))
		return
	}

	// Ensure nil slices marshal as [] rather than null.
	if msgs == nil {
		msgs = []models.Message{}
	}
	if files == nil {
		files = []models.FileRecord{}
	}

	writeJSON(w, http.StatusOK, models.IPCReply{
		OK: true,
		Data: models.IPCHistoryResp{
			Messages: msgs,
			Files:    files,
		},
	})
}

// handleStatus returns the daemon's self-description and the current peer list.
//
//	GET /api/status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorReply("method not allowed"))
		return
	}

	peerList := s.peers.All()
	if peerList == nil {
		peerList = []models.NodeInfo{}
	}

	writeJSON(w, http.StatusOK, models.IPCReply{
		OK: true,
		Data: models.IPCStatusResp{
			NodeID:    s.cfg.NodeID,
			NodeName:  s.cfg.NodeName,
			PeerCount: s.peers.Count(),
			Peers:     peerList,
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// JSON helpers
// ─────────────────────────────────────────────────────────────────────────────

// writeJSON serialises v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("[ipc] writeJSON encode: %v", err)
	}
}

// decodeJSON reads the request body and unmarshals it into v.
func decodeJSON(r *http.Request, v interface{}) error {
	if r.Body == nil {
		return fmt.Errorf("request body is empty")
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// errorReply is a shorthand constructor for a failed IPCReply.
func errorReply(msg string) models.IPCReply {
	return models.IPCReply{OK: false, Message: msg}
}
