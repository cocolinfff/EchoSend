package storage

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"p2p-sync/internal/models"
)

func TestInsertFileRecord_PreservesInitialTimestamp(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ts1 := time.Now().Add(-time.Hour).UnixNano()
	ts2 := time.Now().UnixNano()

	if err := store.InsertFileRecord(models.FileRecord{
		FileHash:  "ABCDEF1234",
		FileName:  "a.txt",
		FileSize:  12,
		SenderID:  "n1",
		SenderIP:  "10.0.0.2",
		SenderTCP: 9100,
		Status:    models.FileStatusKnown,
		Timestamp: ts1,
	}); err != nil {
		t.Fatalf("insert initial record: %v", err)
	}

	if err := store.InsertFileRecord(models.FileRecord{
		FileHash:  "abcdef1234",
		FileName:  "a.txt",
		FileSize:  12,
		SenderID:  "n1",
		SenderIP:  "10.0.0.2",
		SenderTCP: 9100,
		Status:    models.FileStatusFailed,
		Timestamp: ts2,
	}); err != nil {
		t.Fatalf("overwrite file record: %v", err)
	}

	rec, found, err := store.GetFile("ABCDEF1234")
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if !found {
		t.Fatalf("record not found")
	}

	if rec.Timestamp != ts1 {
		t.Fatalf("timestamp mutated: got %d want %d", rec.Timestamp, ts1)
	}
	if rec.Status != models.FileStatusFailed {
		t.Fatalf("status not updated: got %s want %s", rec.Status, models.FileStatusFailed)
	}
	if rec.FileHash != "abcdef1234" {
		t.Fatalf("hash not normalized: got %s", rec.FileHash)
	}
}

func TestFileHashLegacyCaseInsensitiveLookupAndUpdate(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	legacy := models.FileRecord{
		FileHash:  "ABCDEF9876",
		FileName:  "legacy.bin",
		FileSize:  42,
		SenderID:  "n-legacy",
		SenderIP:  "10.0.0.5",
		SenderTCP: 9001,
		Status:    models.FileStatusKnown,
		Timestamp: time.Now().Add(-2 * time.Hour).UnixNano(),
	}

	// Simulate pre-fix DB content where file hash keys were stored in uppercase.
	if err := store.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		data, mErr := json.Marshal(legacy)
		if mErr != nil {
			return mErr
		}
		return b.Put([]byte(legacy.FileHash), data)
	}); err != nil {
		t.Fatalf("seed legacy record: %v", err)
	}

	rec, found, err := store.GetFile("abcdef9876")
	if err != nil {
		t.Fatalf("get legacy file: %v", err)
	}
	if !found {
		t.Fatalf("legacy record not found")
	}
	if rec.FileHash != "abcdef9876" {
		t.Fatalf("normalized hash mismatch: got %s", rec.FileHash)
	}

	if err := store.UpdateFileStatus("abcdef9876", models.FileStatusFailed, ""); err != nil {
		t.Fatalf("update legacy status: %v", err)
	}

	if err := store.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		if b.Get([]byte("ABCDEF9876")) != nil {
			t.Fatalf("legacy uppercase key still present after update")
		}
		if b.Get([]byte("abcdef9876")) == nil {
			t.Fatalf("normalized lowercase key missing after update")
		}
		return nil
	}); err != nil {
		t.Fatalf("verify migrated key: %v", err)
	}
}
