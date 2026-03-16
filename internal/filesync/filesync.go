// Package filesync implements the file synchronisation orchestrator for the
// EchoSend daemon.
//
// Responsibilities:
//
//  1. React to incoming FILE_META gossip packets and decide whether to trigger
//     an automatic download based on the configured AutoDownloadMaxMB ceiling.
//
//  2. Enforce the MaxConcurrentSyncs limit via a buffered-channel semaphore so
//     that low-end devices are never overwhelmed by simultaneous downloads.
//
//  3. Execute each download using the streaming TCP client (network.DownloadFile)
//     which keeps RAM usage O(copyBufSize) regardless of file size.
//
//  4. After a successful download, update the database record to COMPLETE and
//     promote this node to a seeder so the file propagates further.
//
//  5. For files the local user explicitly sends (via CLI), compute the SHA-256
//     hash in a single streaming pass (no full-file buffering) and broadcast
//     a FILE_META gossip packet so peers can pull the file.
//
// Thread safety: all exported methods are safe for concurrent use.
package filesync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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
	"p2p-sync/internal/network"
	"p2p-sync/internal/storage"
)

// ─────────────────────────────────────────────────────────────────────────────
// Constants
// ─────────────────────────────────────────────────────────────────────────────

const (
	// hashBufSize is the read-buffer size used when computing the SHA-256 of a
	// local file before broadcasting it.  64 KiB balances syscall overhead
	// against stack pressure.
	hashBufSize = 64 * 1024

	// downloadRetryDelay is the minimum pause before retrying a failed download.
	downloadRetryDelay = 5 * time.Second

	// maxDownloadRetries is how many times a single file download is retried
	// before the record is marked FAILED and abandoned.
	maxDownloadRetries = 3

	// autoToManualFailThreshold is how many exhausted auto-download sessions
	// (each session already includes maxDownloadRetries attempts) are allowed
	// before we stop auto-downloading that source/file and require manual pull.
	autoToManualFailThreshold = 3
)

var (
	// ErrManualPullNotFound means the requested file hash does not exist in local metadata.
	ErrManualPullNotFound = errors.New("file hash not found in local history")
	// ErrManualPullNoSource means we have metadata but no reachable source endpoint.
	ErrManualPullNoSource = errors.New("no source peer available for this file")
	// ErrManualPullInFlight means this hash is already downloading.
	ErrManualPullInFlight = errors.New("file download already in progress")
)

// ─────────────────────────────────────────────────────────────────────────────
// Sender interface
// ─────────────────────────────────────────────────────────────────────────────

// GossipSender is implemented by network.UDPEngine.  The interface is defined
// here (rather than importing the concrete type) to break the import cycle
// between the filesync and network packages and to simplify unit testing.
type GossipSender interface {
	Send(pkt models.Packet) error
}

// ─────────────────────────────────────────────────────────────────────────────
// Orchestrator
// ─────────────────────────────────────────────────────────────────────────────

// Orchestrator manages the complete lifecycle of file synchronisation:
// receiving metadata, scheduling downloads, verifying integrity and seeding.
type Orchestrator struct {
	cfg    *config.Config
	store  *storage.Store
	gossip GossipSender

	// sem is a counting semaphore (buffered channel) that caps the number of
	// concurrent file downloads.  Acquiring = receive from channel;
	// releasing = send to channel.
	sem chan struct{}

	// inFlight tracks the set of file hashes currently being downloaded so we
	// never start two goroutines for the same file.
	inFlightMu sync.Mutex
	inFlight   map[string]struct{}

	// auto-failover tracking: after repeated failed auto-download sessions for
	// the same (sender, file name), we switch that key to manual-only mode.
	autoModeMu      sync.Mutex
	autoFailCount   map[string]int
	manualOnlyAuto  map[string]struct{}
}

// New creates an Orchestrator.  gossip may be nil during unit tests (no
// broadcasts will be sent).
func New(cfg *config.Config, store *storage.Store, gossip GossipSender) *Orchestrator {
	cap := cfg.MaxConcurrentSyncs
	if cap <= 0 {
		cap = 3
	}

	// Pre-fill the semaphore channel so each slot is immediately available.
	sem := make(chan struct{}, cap)
	for i := 0; i < cap; i++ {
		sem <- struct{}{}
	}

	return &Orchestrator{
		cfg:      cfg,
		store:    store,
		gossip:   gossip,
		sem:      sem,
		inFlight: make(map[string]struct{}),

		autoFailCount:  make(map[string]int),
		manualOnlyAuto: make(map[string]struct{}),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// HandleFileMeta – react to an incoming FILE_META gossip packet
// ─────────────────────────────────────────────────────────────────────────────

// HandleFileMeta is the PacketHandler callback registered with the UDP engine
// for FILE_META packets.  It is called from a goroutine inside the UDP engine
// and must return quickly; any heavy work is dispatched asynchronously.
func (o *Orchestrator) HandleFileMeta(pkt models.Packet, from *net.UDPAddr) {
	var meta models.FileMeta
	if err := json.Unmarshal(pkt.Payload, &meta); err != nil {
		log.Printf("[sync] malformed FILE_META from %s: %v", pkt.SenderID, err)
		return
	}

	log.Printf("[sync] received FILE_META: %s (%s) from %s",
		meta.FileName, humanSize(meta.FileSize), pkt.SenderID)

	meta.FileHash = strings.ToLower(strings.TrimSpace(meta.FileHash))
	if meta.FileHash == "" {
		log.Printf("[sync] malformed FILE_META from %s: empty file hash", pkt.SenderID)
		return
	}

	recTs := pkt.Timestamp
	if recTs == 0 {
		recTs = time.Now().UnixNano()
	}

	// Persist the record (KNOWN state) so --history shows it immediately.
	rec := models.FileRecord{
		FileHash:  meta.FileHash,
		FileName:  meta.FileName,
		FileSize:  meta.FileSize,
		SenderID:  pkt.SenderID,
		SenderIP:  pkt.SenderIP,
		SenderTCP: meta.TCPPort,
		Status:    models.FileStatusKnown,
		Timestamp: recTs,
	}

	// If this hash has already failed before, keep it in manual mode and do not
	// auto-download it again on every history replay.
	if prev, found, err := o.store.GetFile(rec.FileHash); err != nil {
		log.Printf("[sync] db read existing file record: %v", err)
	} else if found && prev.Status == models.FileStatusFailed {
		log.Printf("[sync] skip auto-download for previously failed hash %s (%s); use --pull for manual retry",
			rec.FileHash, rec.FileName)
		return
	}

	if o.isManualOnlyForAuto(rec.SenderID, rec.FileName) {
		// Persist metadata for visibility/history, but do not auto-download.
		rec.Status = models.FileStatusFailed
		if err := o.store.InsertFileRecord(rec); err != nil && err != storage.ErrDuplicateFile {
			log.Printf("[sync] db insert file meta (manual-only): %v", err)
		}
		log.Printf("[sync] auto-download disabled for %s from %s after repeated failures; use --pull %s",
			rec.FileName, rec.SenderID, rec.FileHash)
		return
	}

	if err := o.store.InsertFileRecord(rec); err != nil {
		if err == storage.ErrDuplicateFile {
			log.Printf("[sync] already have %s – skipping", meta.FileName)
			return
		}
		log.Printf("[sync] db insert file meta: %v", err)
		// Continue: even if persistence failed, we can still try to download.
	}

	// Auto-download decision:
	//   • AutoDownloadMaxMB == 0  → disabled
	//   • FileSize > threshold    → skip (user must pull manually)
	if o.cfg.AutoDownloadMaxMB == 0 {
		log.Printf("[sync] auto-download disabled, skipping %s", meta.FileName)
		return
	}
	if meta.FileSize > o.cfg.AutoDownloadMaxBytes() {
		log.Printf("[sync] %s (%s) exceeds auto-download limit (%d MB), skipping",
			meta.FileName, humanSize(meta.FileSize), o.cfg.AutoDownloadMaxMB)
		return
	}

	// Determine the source address for the TCP pull.
	sourceIP := pkt.SenderIP
	if sourceIP == "" && from != nil {
		sourceIP = from.IP.String()
	}
	peerAddr := fmt.Sprintf("%s:%d", sourceIP, meta.TCPPort)

	_ = o.startTrackedDownload(rec, peerAddr, true)
}

// PullFileByHash schedules a manual pull/retry for a file already present in
// local history metadata. The operation is asynchronous and returns once the
// download goroutine has been queued.
func (o *Orchestrator) PullFileByHash(fileHash string) error {
	fileHash = strings.ToLower(strings.TrimSpace(fileHash))
	if fileHash == "" {
		return ErrManualPullNotFound
	}

	rec, found, err := o.store.GetFile(fileHash)
	if err != nil {
		return fmt.Errorf("manual pull: load file record: %w", err)
	}
	if !found {
		return ErrManualPullNotFound
	}

	sourceIP := strings.TrimSpace(rec.SenderIP)
	sourceTCP := rec.SenderTCP

	if (sourceIP == "" || sourceTCP <= 0) && rec.SenderID != "" {
		node, ok, nodeErr := o.store.GetNode(rec.SenderID)
		if nodeErr != nil {
			return fmt.Errorf("manual pull: load sender node: %w", nodeErr)
		}
		if ok {
			if sourceIP == "" {
				sourceIP = strings.TrimSpace(node.IP)
			}
			if sourceTCP <= 0 {
				sourceTCP = node.TCPPort
			}
		}
	}

	if sourceIP == "" || sourceTCP <= 0 {
		return ErrManualPullNoSource
	}

	if rec.FileHash == "" {
		rec.FileHash = fileHash
	}
	peerAddr := fmt.Sprintf("%s:%d", sourceIP, sourceTCP)

	if err := o.startTrackedDownload(rec, peerAddr, false); err != nil {
		return err
	}

	log.Printf("[sync] manual pull scheduled: %s from %s", rec.FileHash, peerAddr)
	return nil
}

// startTrackedDownload starts an async download for rec if it is not already in flight.
func (o *Orchestrator) startTrackedDownload(rec models.FileRecord, peerAddr string, autoTriggered bool) error {
	o.inFlightMu.Lock()
	if _, already := o.inFlight[rec.FileHash]; already {
		o.inFlightMu.Unlock()
		return ErrManualPullInFlight
	}
	o.inFlight[rec.FileHash] = struct{}{}
	o.inFlightMu.Unlock()

	go o.downloadWithRetry(context.Background(), rec, peerAddr, autoTriggered)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Download pipeline
// ─────────────────────────────────────────────────────────────────────────────

// downloadWithRetry attempts to download rec from peerAddr up to
// maxDownloadRetries times, with an exponential-ish back-off between attempts.
// It acquires the semaphore before each attempt so the cap is always respected.
func (o *Orchestrator) downloadWithRetry(ctx context.Context, rec models.FileRecord, peerAddr string, autoTriggered bool) {
	defer func() {
		// Always release the in-flight tracking entry, regardless of outcome.
		o.inFlightMu.Lock()
		delete(o.inFlight, rec.FileHash)
		o.inFlightMu.Unlock()
	}()

	var lastErr error
	for attempt := 1; attempt <= maxDownloadRetries; attempt++ {
		// Back-off between retries.
		if attempt > 1 {
			backoff := time.Duration(attempt-1) * downloadRetryDelay
			log.Printf("[sync] retry %d/%d for %s in %s (last error: %v)",
				attempt, maxDownloadRetries, rec.FileName, backoff, lastErr)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}

		// Acquire a semaphore slot.
		select {
		case <-o.sem:
			// Slot acquired.
		case <-ctx.Done():
			return
		}

		err := o.executeDownload(ctx, rec, peerAddr)
		o.sem <- struct{}{} // release slot unconditionally

		if err == nil {
			return // success
		}
		lastErr = err
		log.Printf("[sync] download attempt %d/%d failed for %s: %v",
			attempt, maxDownloadRetries, rec.FileName, err)
	}

	// All retries exhausted – mark as FAILED.
	log.Printf("[sync] giving up on %s after %d attempts", rec.FileName, maxDownloadRetries)
	if err := o.store.UpdateFileStatus(rec.FileHash, models.FileStatusFailed, ""); err != nil {
		log.Printf("[sync] db update status FAILED: %v", err)
	}
	if autoTriggered {
		o.noteAutoFailure(rec)
	}
}

// executeDownload performs one download attempt for rec from peerAddr.
// It marks the record DOWNLOADING before starting, calls network.DownloadFile
// for the actual streaming transfer, and on success promotes to COMPLETE/SEEDING.
func (o *Orchestrator) executeDownload(ctx context.Context, rec models.FileRecord, peerAddr string) error {
	// Mark as DOWNLOADING in the database so the status is visible via --history.
	if err := o.store.UpdateFileStatus(rec.FileHash, models.FileStatusDownloading, ""); err != nil {
		log.Printf("[sync] db update DOWNLOADING: %v", err)
		// Non-fatal: continue with the download.
	}

	log.Printf("[sync] downloading %s from %s", rec.FileName, peerAddr)

	result, err := network.DownloadFile(ctx, peerAddr, rec.FileHash, o.cfg.StorageDir)
	if err != nil {
		// DownloadFile already deleted the partial file on error, so just
		// update the status and propagate.
		if dbErr := o.store.UpdateFileStatus(rec.FileHash, models.FileStatusFailed, ""); dbErr != nil {
			log.Printf("[sync] db update FAILED: %v", dbErr)
		}
		return err
	}

	// ── Promote to COMPLETE and become a seeder ───────────────────────────────

	if err := o.store.UpdateFileStatus(rec.FileHash, models.FileStatusSeeding, result.LocalPath); err != nil {
		log.Printf("[sync] db update SEEDING: %v", err)
	}

	log.Printf("[sync] ✓ %s saved to %s (%s)",
		result.FileName, result.LocalPath, humanSize(result.FileSize))

	// Any successful download (automatic or manual) clears manual-only mode
	// for this source/file so future updates can auto-sync again.
	o.clearAutoFailure(rec)

	// Re-broadcast the FILE_META so this node propagates the file further.
	o.rebroadcastFileMeta(rec, result.LocalPath)

	return nil
}

func autoKey(senderID, fileName string) string {
	sid := strings.TrimSpace(senderID)
	if sid == "" {
		sid = "unknown"
	}
	name := strings.ToLower(strings.TrimSpace(fileName))
	return sid + "|" + name
}

func (o *Orchestrator) isManualOnlyForAuto(senderID, fileName string) bool {
	key := autoKey(senderID, fileName)
	o.autoModeMu.Lock()
	defer o.autoModeMu.Unlock()
	_, blocked := o.manualOnlyAuto[key]
	return blocked
}

func (o *Orchestrator) noteAutoFailure(rec models.FileRecord) {
	key := autoKey(rec.SenderID, rec.FileName)
	o.autoModeMu.Lock()
	defer o.autoModeMu.Unlock()

	o.autoFailCount[key]++
	if o.autoFailCount[key] >= autoToManualFailThreshold {
		o.manualOnlyAuto[key] = struct{}{}
		log.Printf("[sync] auto->manual fallback enabled for %s from %s after %d failed sessions",
			rec.FileName, rec.SenderID, o.autoFailCount[key])
	}
}

func (o *Orchestrator) clearAutoFailure(rec models.FileRecord) {
	key := autoKey(rec.SenderID, rec.FileName)
	o.autoModeMu.Lock()
	defer o.autoModeMu.Unlock()
	delete(o.autoFailCount, key)
	delete(o.manualOnlyAuto, key)
}

// rebroadcastFileMeta announces a newly completed download to the gossip mesh
// so other nodes learn that this node is now a seeder.
func (o *Orchestrator) rebroadcastFileMeta(rec models.FileRecord, localPath string) {
	if o.gossip == nil {
		return
	}

	meta := models.FileMeta{
		FileName: filepath.Base(localPath),
		FileSize: rec.FileSize,
		FileHash: rec.FileHash,
		TCPPort:  o.cfg.DaemonPortTCP,
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		log.Printf("[sync] rebroadcast marshal: %v", err)
		return
	}

	pkt := models.Packet{
		PacketID:  network.NewPacketID(),
		SenderID:  o.cfg.NodeID,
		SenderIP:  "", // filled in by UDPEngine.Send
		Type:      models.PacketFileMeta,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
	}

	if err := o.gossip.Send(pkt); err != nil {
		log.Printf("[sync] rebroadcast FILE_META: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// PublishFile – called by the CLI --send -f path handler (via IPC)
// ─────────────────────────────────────────────────────────────────────────────

// PublishFile computes the SHA-256 hash of the file at localPath using a
// single streaming pass (no full-file buffering), records it in the database
// as SEEDING, and broadcasts a FILE_META gossip packet so remote peers can
// pull it.
//
// This function blocks until the hash computation and broadcast are complete,
// which for a large file may take several seconds.  The caller (IPC handler)
// should run it in a goroutine if responsiveness matters.
func (o *Orchestrator) PublishFile(localPath string) error {
	// Resolve to an absolute path so the database record is stable even if the
	// daemon's working directory changes.
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("publish: abs path: %w", err)
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("publish: stat %s: %w", absPath, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("publish: %s is a directory; only files are supported", absPath)
	}

	// ── Streaming SHA-256 computation ─────────────────────────────────────────
	// Open the file and feed it through sha256.New() in fixed-size chunks.
	// At no point is the file content accumulated in a []byte.

	f, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("publish: open %s: %w", absPath, err)
	}
	defer f.Close()

	h := sha256.New()
	buf := make([]byte, hashBufSize)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return fmt.Errorf("publish: hash %s: %w", absPath, err)
	}

	fileHash := fmt.Sprintf("%x", h.Sum(nil))
	fileName := filepath.Base(absPath)

	log.Printf("[sync] publishing %s (%s) hash=%s", fileName, humanSize(fi.Size()), fileHash)

	// ── Persist as SEEDING ────────────────────────────────────────────────────

	rec := models.FileRecord{
		FileHash:  fileHash,
		FileName:  fileName,
		FileSize:  fi.Size(),
		SenderID:  o.cfg.NodeID,
		SenderIP:  "", // local node
		SenderTCP: o.cfg.DaemonPortTCP,
		LocalPath: absPath,
		Status:    models.FileStatusSeeding,
		Timestamp: time.Now().UnixNano(),
	}

	if err := o.store.InsertFileRecord(rec); err != nil && err != storage.ErrDuplicateFile {
		return fmt.Errorf("publish: db insert: %w", err)
	}

	// ── Broadcast FILE_META ───────────────────────────────────────────────────

	if o.gossip == nil {
		log.Printf("[sync] gossip sender not set – skipping broadcast")
		return nil
	}

	meta := models.FileMeta{
		FileName: fileName,
		FileSize: fi.Size(),
		FileHash: fileHash,
		TCPPort:  o.cfg.DaemonPortTCP,
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("publish: marshal meta: %w", err)
	}

	pkt := models.Packet{
		PacketID:  network.NewPacketID(),
		SenderID:  o.cfg.NodeID,
		Type:      models.PacketFileMeta,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
	}

	if err := o.gossip.Send(pkt); err != nil {
		return fmt.Errorf("publish: broadcast: %w", err)
	}

	log.Printf("[sync] ✓ FILE_META broadcast for %s", fileName)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ResumePendingDownloads – called at daemon startup
// ─────────────────────────────────────────────────────────────────────────────

// ResumePendingDownloads scans the database for any FileRecord that was left
// in DOWNLOADING state (i.e. the daemon crashed mid-transfer) and resets them
// to KNOWN so they are eligible for re-download on the next FILE_META receipt.
// Records in FAILED state are left as-is; the user can inspect them via
// --history.
func (o *Orchestrator) ResumePendingDownloads() error {
	downloading, err := o.store.ListFilesByStatus(models.FileStatusDownloading)
	if err != nil {
		return fmt.Errorf("resume: list downloading: %w", err)
	}

	for _, rec := range downloading {
		log.Printf("[sync] resetting stale DOWNLOADING record for %s", rec.FileName)
		if err := o.store.UpdateFileStatus(rec.FileHash, models.FileStatusKnown, ""); err != nil {
			log.Printf("[sync] resume: reset %s: %v", rec.FileName, err)
		}
		// Keep .tmp so the next pull can resume from the persisted offset.
	}

	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// humanSize formats a byte count into a human-readable string.
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
