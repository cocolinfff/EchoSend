// tcp.go – TCP File Server for EchoSend
//
// The TCP file server allows remote peers to pull files that this node is
// currently seeding.  Design goals:
//
//   1. Streaming I/O: files are sent directly from disk to the network socket
//      with a fixed-size copy buffer.  The entire file is never held in RAM,
//      keeping memory usage O(bufferSize) regardless of file size.
//
//   2. Concurrency cap: a buffered-channel semaphore enforces the
//      MaxConcurrentSyncs limit so low-end devices are not overwhelmed by
//      simultaneous upload connections.
//
//   3. Per-connection deadline: every connection gets a read/write deadline so
//      a stalled peer cannot hold a semaphore slot indefinitely.
//
// Wire protocol (line-delimited JSON framing):
//
//   Client → Server:  {"file_hash":"<sha256-hex>"}\n
//   Server → Client:  {"ok":true,"file_name":"x","file_size":1234}\n
//                     <raw file bytes>
//   or on error:
//   Server → Client:  {"ok":false,"error":"reason"}\n
//
// The server closes the connection after each transfer.
package network

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"p2p-sync/internal/config"
	"p2p-sync/internal/models"
	"p2p-sync/internal/storage"
)

// ─────────────────────────────────────────────────────────────────────────────
// Constants
// ─────────────────────────────────────────────────────────────────────────────

const (
	// tcpCopyBufSize is the size of the intermediate copy buffer used when
	// streaming a file from disk to the network socket.  32 KiB is a good
	// balance between syscall overhead and stack pressure.
	tcpCopyBufSize = 32 * 1024

	// tcpConnDeadline is the maximum time the server will wait for a client
	// to send its request header, or for a file transfer to complete.
	// Chosen conservatively to accommodate slow LAN links.
	tcpConnDeadline = 5 * time.Minute

	// tcpHandshakeDeadline is the tighter deadline applied only to the initial
	// JSON request line.  If the client hasn't sent a valid request within
	// this window we hang up.
	tcpHandshakeDeadline = 15 * time.Second
)

// ─────────────────────────────────────────────────────────────────────────────
// Wire types
// ─────────────────────────────────────────────────────────────────────────────

// tcpRequest is the JSON structure sent by the downloading peer.
type tcpRequest struct {
	FileHash string `json:"file_hash"`
}

// tcpResponse is the JSON header sent back before the raw file bytes.
// On error Ok is false and Error contains a human-readable message.
type tcpResponse struct {
	OK       bool   `json:"ok"`
	FileName string `json:"file_name,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// TCPServer
// ─────────────────────────────────────────────────────────────────────────────

// TCPServer listens for inbound file-pull connections and serves seeded files
// to remote peers using a streaming, zero-copy-to-RAM approach.
type TCPServer struct {
	cfg   *config.Config
	store *storage.Store

	// sem is a counting semaphore implemented as a buffered channel.
	// Acquiring a slot means taking one item; releasing means putting one back.
	// Its capacity equals cfg.MaxConcurrentSyncs.
	sem chan struct{}

	// listener is kept so Run can close it on context cancellation.
	listenerMu sync.Mutex
	listener   net.Listener
}

// NewTCPServer constructs a TCPServer.  Call Run to start accepting connections.
func NewTCPServer(cfg *config.Config, store *storage.Store) *TCPServer {
	cap := cfg.MaxConcurrentSyncs
	if cap <= 0 {
		cap = 3
	}
	sem := make(chan struct{}, cap)
	// Pre-fill the semaphore: each available slot is represented by one item.
	for i := 0; i < cap; i++ {
		sem <- struct{}{}
	}

	return &TCPServer{
		cfg:   cfg,
		store: store,
		sem:   sem,
	}
}

// Run starts the TCP listener and accepts connections until ctx is cancelled.
// It blocks until the listener is closed.
func (s *TCPServer) Run(ctx context.Context) error {
	addr := fmt.Sprintf("0.0.0.0:%d", s.cfg.DaemonPortTCP)
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return fmt.Errorf("tcp: listen %s: %w", addr, err)
	}

	s.listenerMu.Lock()
	s.listener = ln
	s.listenerMu.Unlock()

	log.Printf("[tcp] file server listening on %s (max %d concurrent uploads)", addr, cap(s.sem))

	// Close the listener when the context is cancelled so Accept unblocks.
	go func() {
		<-ctx.Done()
		s.listenerMu.Lock()
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.listenerMu.Unlock()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				log.Printf("[tcp] server stopped")
				return nil
			default:
				log.Printf("[tcp] accept error: %v", err)
				// Brief pause before retrying to avoid a hot error loop on
				// transient OS errors (e.g. "too many open files").
				time.Sleep(50 * time.Millisecond)
				continue
			}
		}

		go s.handleConn(ctx, conn)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Connection handler
// ─────────────────────────────────────────────────────────────────────────────

// handleConn processes a single inbound file-pull connection.
func (s *TCPServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close() // always close, even on panic

	remote := conn.RemoteAddr().String()
	log.Printf("[tcp] incoming connection from %s", remote)

	// ── 1. Read the JSON request header ──────────────────────────────────────

	// Apply a tight deadline for the initial handshake only.
	if err := conn.SetDeadline(time.Now().Add(tcpHandshakeDeadline)); err != nil {
		log.Printf("[tcp] %s: SetDeadline: %v", remote, err)
		return
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		if err != io.EOF {
			log.Printf("[tcp] %s: read request: %v", remote, err)
		}
		return
	}

	var req tcpRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		s.writeErrorResponse(conn, fmt.Sprintf("bad request: %v", err))
		return
	}
	req.FileHash = strings.ToLower(strings.TrimSpace(req.FileHash))
	if req.FileHash == "" {
		s.writeErrorResponse(conn, "file_hash is required")
		return
	}

	// ── 2. Look up the file record ────────────────────────────────────────────

	rec, found, err := s.store.GetFile(req.FileHash)
	if err != nil {
		log.Printf("[tcp] %s: db lookup %s: %v", remote, req.FileHash, err)
		s.writeErrorResponse(conn, "internal error")
		return
	}
	if !found {
		s.writeErrorResponse(conn, "file not found")
		return
	}
	if rec.Status != models.FileStatusSeeding && rec.Status != models.FileStatusComplete {
		s.writeErrorResponse(conn, fmt.Sprintf("file not available (status: %s)", rec.Status))
		return
	}
	if rec.LocalPath == "" {
		s.writeErrorResponse(conn, "file has no local path")
		return
	}

	// ── 3. Acquire a semaphore slot ───────────────────────────────────────────
	// If all upload slots are taken, we wait or abandon depending on the
	// context state, so we don't starve the caller forever.

	select {
	case <-s.sem:
		// Slot acquired.
	case <-ctx.Done():
		s.writeErrorResponse(conn, "server shutting down")
		return
	case <-time.After(30 * time.Second):
		// All upload slots busy – tell the peer to retry later.
		s.writeErrorResponse(conn, "server busy, try again later")
		return
	}
	defer func() { s.sem <- struct{}{} }() // release slot on return

	// ── 4. Open the local file ────────────────────────────────────────────────

	f, err := os.Open(rec.LocalPath)
	if err != nil {
		log.Printf("[tcp] %s: open %s: %v", remote, rec.LocalPath, err)
		s.writeErrorResponse(conn, "could not open file")
		return
	}
	defer f.Close()

	// Re-stat only for sanity checks; transfer size must come from rec.FileSize
	// (publish-time snapshot) to keep hash stable across retries.
	fi, err := f.Stat()
	if err != nil {
		log.Printf("[tcp] %s: stat %s: %v", remote, rec.LocalPath, err)
		s.writeErrorResponse(conn, "could not stat file")
		return
	}

	if rec.FileSize < 0 {
		log.Printf("[tcp] %s: invalid snapshot size for %s: %d", remote, rec.FileName, rec.FileSize)
		s.writeErrorResponse(conn, "invalid file metadata")
		return
	}
	if fi.Size() < rec.FileSize {
		log.Printf("[tcp] %s: file shrank under snapshot for %s: current=%d snapshot=%d",
			remote, rec.FileName, fi.Size(), rec.FileSize)
		s.writeErrorResponse(conn, "file changed, retry later")
		return
	}

	// ── 5. Send the JSON response header ─────────────────────────────────────

	resp := tcpResponse{
		OK:       true,
		FileName: filepath.Base(rec.LocalPath),
		FileSize: rec.FileSize,
	}
	respData, _ := json.Marshal(resp)
	respData = append(respData, '\n')

	// Switch to the full transfer deadline now.
	if err := conn.SetDeadline(time.Now().Add(tcpConnDeadline)); err != nil {
		log.Printf("[tcp] %s: SetDeadline (transfer): %v", remote, err)
		return
	}

	if _, err := conn.Write(respData); err != nil {
		log.Printf("[tcp] %s: write response header: %v", remote, err)
		return
	}

	// ── 6. Stream the file body ───────────────────────────────────────────────
	// Use a fixed-size copy buffer (via a pool) so memory stays O(bufSize)
	// regardless of how large the file is.

	written, err := streamFile(io.LimitReader(f, rec.FileSize), conn)
	if err != nil {
		log.Printf("[tcp] %s: stream %s (wrote %d B): %v",
			remote, rec.FileName, written, err)
		return
	}
	if written != rec.FileSize {
		log.Printf("[tcp] %s: short stream for %s: wrote=%d expected=%d",
			remote, rec.FileName, written, rec.FileSize)
		return
	}

	log.Printf("[tcp] %s: sent %s (%d B) → %s",
		remote, rec.FileName, written, conn.RemoteAddr())
}

// ─────────────────────────────────────────────────────────────────────────────
// Client-side: DownloadFile
// ─────────────────────────────────────────────────────────────────────────────

// DownloadResult is returned by DownloadFile on success.
type DownloadResult struct {
	FileName  string
	FileSize  int64
	LocalPath string
}

// DownloadFile connects to peerAddr (host:port), requests the file identified
// by fileHash, streams the response directly to destDir/<FileName>, and
// verifies that the written bytes match expectedHash.
//
// The download is entirely streaming: at no point is the file body buffered in
// RAM.  Memory usage is bounded by tcpCopyBufSize (32 KiB).
//
// On success the local file path is returned.
// On any failure (connection, hash mismatch, etc.) the partial file is deleted
// and a non-nil error is returned.
func DownloadFile(
	ctx context.Context,
	peerAddr string,
	fileHash string,
	destDir string,
) (*DownloadResult, error) {
	fileHash = strings.ToLower(strings.TrimSpace(fileHash))
	if fileHash == "" {
		return nil, fmt.Errorf("tcp download: empty file hash")
	}

	// ── 1. Connect ────────────────────────────────────────────────────────────

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp4", peerAddr)
	if err != nil {
		return nil, fmt.Errorf("tcp download: dial %s: %w", peerAddr, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(tcpConnDeadline)); err != nil {
		return nil, fmt.Errorf("tcp download: SetDeadline: %w", err)
	}

	// ── 2. Send request ───────────────────────────────────────────────────────

	req := tcpRequest{FileHash: fileHash}
	reqData, _ := json.Marshal(req)
	reqData = append(reqData, '\n')

	if _, err := conn.Write(reqData); err != nil {
		return nil, fmt.Errorf("tcp download: write request: %w", err)
	}

	// ── 3. Read response header ───────────────────────────────────────────────

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("tcp download: read response header: %w", err)
	}

	var resp tcpResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("tcp download: parse response header: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("tcp download: server error: %s", resp.Error)
	}

	// Sanitise the file name to prevent path traversal attacks.
	safeName := filepath.Base(resp.FileName)
	if safeName == "" || safeName == "." || safeName == "/" {
		safeName = fileHash[:16]
	}

	// ── 4. Prepare the destination file ──────────────────────────────────────

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("tcp download: mkdir %s: %w", destDir, err)
	}

	// Write to a .tmp file first; rename only after hash verification.
	tmpPath := filepath.Join(destDir, safeName+".tmp")
	finalPath := filepath.Join(destDir, safeName)

	// Remove stale temp files from a previous failed attempt.
	_ = os.Remove(tmpPath)

	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("tcp download: create %s: %w", tmpPath, err)
	}

	// ── 5. Stream body → disk while computing SHA-256 ─────────────────────────
	// io.TeeReader feeds the network stream simultaneously into the hash and
	// the file writer.  No intermediate buffer accumulates the whole body.

	written, computedHash, err := streamAndHash(reader, tmpFile, resp.FileSize)
	tmpFile.Close() // close before rename even on error

	if err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("tcp download: stream %s: %w", safeName, err)
	}

	// ── 6. Verify integrity ───────────────────────────────────────────────────

	if computedHash != fileHash {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf(
			"tcp download: hash mismatch for %s: got %s, want %s",
			safeName, computedHash, fileHash,
		)
	}

	// ── 7. Atomic rename into final location ─────────────────────────────────

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("tcp download: rename %s → %s: %w", tmpPath, finalPath, err)
	}

	log.Printf("[tcp] downloaded %s (%d B), hash OK", safeName, written)

	return &DownloadResult{
		FileName:  safeName,
		FileSize:  written,
		LocalPath: finalPath,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Streaming helpers
// ─────────────────────────────────────────────────────────────────────────────

// copyBufPool recycles 32 KiB byte slices used for streaming file data.
// Having one pool for both upload and download paths means the buffers are
// shared across all concurrent transfers, keeping peak RSS low.
var copyBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, tcpCopyBufSize)
		return &b
	},
}

// streamFile copies all data from src to dst using a pooled buffer.
// Returns the number of bytes written.
func streamFile(src io.Reader, dst io.Writer) (int64, error) {
	bufPtr := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufPtr)

	n, err := io.CopyBuffer(dst, src, *bufPtr)
	return n, err
}

// streamAndHash streams up to maxBytes from src into dst while simultaneously
// computing a SHA-256 hash of the received data.
//
// It uses io.TeeReader so the data flows:
//
//	src (network) → TeeReader → hash.Write  (hash updated on every read)
//	                         → dst (disk)   (written on every read)
//
// This means at any instant only one copyBufSize chunk is held in memory.
// Returns (bytesWritten, sha256HexString, error).
func streamAndHash(src io.Reader, dst io.Writer, expectedSize int64) (int64, string, error) {
	import_crypto_sha256_hash := newSHA256()

	// TeeReader: every Read from tee also writes to the hash.
	tee := io.TeeReader(src, import_crypto_sha256_hash)

	bufPtr := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufPtr)

	var written int64
	var copyErr error

	if expectedSize > 0 {
		// Copy exactly expectedSize bytes; avoids reading trailing garbage if
		// the peer sends more data than advertised.
		written, copyErr = io.CopyBuffer(dst, io.LimitReader(tee, expectedSize), *bufPtr)
	} else {
		written, copyErr = io.CopyBuffer(dst, tee, *bufPtr)
	}

	if copyErr != nil {
		return written, "", copyErr
	}

	hashStr := fmt.Sprintf("%x", import_crypto_sha256_hash.Sum(nil))
	return written, hashStr, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SHA-256 factory (thin wrapper to keep imports in one place)
// ─────────────────────────────────────────────────────────────────────────────

// sha256Writer is an io.Writer whose Sum method returns the current digest.
// It is satisfied by crypto/sha256.New() return value (hash.Hash).
type sha256Writer interface {
	io.Writer
	Sum(b []byte) []byte
}

// sha256FactoryFn is overridable in tests.
var sha256FactoryFn func() sha256Writer

func newSHA256() sha256Writer {
	if sha256FactoryFn != nil {
		return sha256FactoryFn()
	}
	return _newSHA256()
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// writeErrorResponse writes a JSON error response line to conn and is used
// for early-exit error paths before the file transfer begins.
func (s *TCPServer) writeErrorResponse(conn net.Conn, msg string) {
	resp := tcpResponse{OK: false, Error: msg}
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	_, _ = conn.Write(data)
}
