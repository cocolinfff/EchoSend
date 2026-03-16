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
