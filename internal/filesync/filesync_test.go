package filesync

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"p2p-sync/internal/config"
	"p2p-sync/internal/models"
	"p2p-sync/internal/storage"
)

func TestHandleFileMeta_SkipsPreviouslyFailedHash(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	store, err := storage.Open(filepath.Join(tempDir, "db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	failed := models.FileRecord{
		FileHash:  "deadbeef1234",
		FileName:  "old.log",
		FileSize:  1024,
		SenderID:  "peer-old",
		SenderIP:  "192.168.1.20",
		SenderTCP: 7778,
		Status:    models.FileStatusFailed,
		Timestamp: time.Now().Add(-time.Hour).UnixNano(),
	}
	if err := store.InsertFileRecord(failed); err != nil {
		t.Fatalf("insert failed record: %v", err)
	}

	o := New(&config.Config{
		NodeID:             "node-test",
		AutoDownloadMaxMB:  100,
		MaxConcurrentSyncs: 1,
		StorageDir:         tempDir,
	}, store, nil)

	meta := models.FileMeta{
		FileName: failed.FileName,
		FileSize: failed.FileSize,
		FileHash: failed.FileHash,
		TCPPort:  7778,
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}

	o.HandleFileMeta(models.Packet{
		PacketID:  "pkt-1",
		SenderID:  "peer-new",
		SenderIP:  "192.168.1.30",
		Type:      models.PacketFileMeta,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
	}, &net.UDPAddr{IP: net.ParseIP("192.168.1.30"), Port: 7777})

	got, found, err := store.GetFile(failed.FileHash)
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if !found {
		t.Fatalf("file record missing after FILE_META")
	}
	if got.Status != models.FileStatusFailed {
		t.Fatalf("status changed unexpectedly: got %s want %s", got.Status, models.FileStatusFailed)
	}

	o.inFlightMu.Lock()
	inFlight := len(o.inFlight)
	o.inFlightMu.Unlock()
	if inFlight != 0 {
		t.Fatalf("download unexpectedly scheduled; inFlight=%d", inFlight)
	}
}

func TestPullFileByHash_CaseInsensitiveLookup(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	store, err := storage.Open(filepath.Join(tempDir, "db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := store.InsertFileRecord(models.FileRecord{
		FileHash:  "abcdef001122",
		FileName:  "demo.txt",
		FileSize:  128,
		SenderID:  "peer-1",
		SenderIP:  "",
		SenderTCP: 0,
		Status:    models.FileStatusKnown,
		Timestamp: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("insert file record: %v", err)
	}

	o := New(&config.Config{
		NodeID:             "node-test",
		AutoDownloadMaxMB:  0,
		MaxConcurrentSyncs: 1,
		StorageDir:         tempDir,
	}, store, nil)

	// Uppercase hash should still hit the same record and continue down the
	// manual pull pipeline (which then fails here due to missing source info).
	err = o.PullFileByHash("ABCDEF001122")
	if err != ErrManualPullNoSource {
		t.Fatalf("unexpected error: got %v want %v", err, ErrManualPullNoSource)
	}
}

func TestHandleFileMeta_PreservesFirstTimestampAcrossUpdates(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	store, err := storage.Open(filepath.Join(tempDir, "db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	o := New(&config.Config{
		NodeID:             "node-test",
		AutoDownloadMaxMB:  0, // disable auto download so test is deterministic
		MaxConcurrentSyncs: 1,
		StorageDir:         tempDir,
	}, store, nil)

	meta := models.FileMeta{
		FileName: "ts.txt",
		FileSize: 256,
		FileHash: "ABC123FED",
		TCPPort:  7778,
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}

	ts1 := time.Now().Add(-2 * time.Hour).UnixNano()
	ts2 := time.Now().UnixNano()

	o.HandleFileMeta(models.Packet{
		PacketID:  "pkt-ts-1",
		SenderID:  "peer-ts",
		SenderIP:  "192.168.1.40",
		Type:      models.PacketFileMeta,
		Payload:   payload,
		Timestamp: ts1,
	}, &net.UDPAddr{IP: net.ParseIP("192.168.1.40"), Port: 7777})

	// Simulate replay/update with a newer packet timestamp; stored timestamp
	// must remain the original first-seen timestamp.
	o.HandleFileMeta(models.Packet{
		PacketID:  "pkt-ts-2",
		SenderID:  "peer-ts",
		SenderIP:  "192.168.1.40",
		Type:      models.PacketFileMeta,
		Payload:   payload,
		Timestamp: ts2,
	}, &net.UDPAddr{IP: net.ParseIP("192.168.1.40"), Port: 7777})

	rec, found, err := store.GetFile("abc123fed")
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if !found {
		t.Fatalf("file record missing")
	}

	if rec.Timestamp != ts1 {
		t.Fatalf("timestamp changed unexpectedly: got %d want %d", rec.Timestamp, ts1)
	}
}
