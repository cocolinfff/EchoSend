package network

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"p2p-sync/internal/config"
	"p2p-sync/internal/models"
	"p2p-sync/internal/storage"
)

func TestTCPServer_HandleConn_UsesSnapshotSizeAndStableContent(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	store, err := storage.Open(filepath.Join(tempDir, "db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Simulate a file that keeps growing (e.g. daemon log): hash/snapshot are
	// computed from prefix only, while disk file contains extra appended bytes.
	snapshot := []byte("alpha-beta-gamma")
	extra := []byte("-new-tail-data-that-must-not-be-sent")
	onDisk := append(append([]byte{}, snapshot...), extra...)

	localPath := filepath.Join(tempDir, "echosend.log")
	if err := os.WriteFile(localPath, onDisk, 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	h := sha256.Sum256(snapshot)
	rec := models.FileRecord{
		FileHash:   hex.EncodeToString(h[:]),
		FileName:   "echosend.log",
		FileSize:   int64(len(snapshot)),
		LocalPath:  localPath,
		Status:     models.FileStatusSeeding,
		Timestamp:  time.Now().UnixNano(),
	}
	if err := store.InsertFileRecord(rec); err != nil {
		t.Fatalf("insert file record: %v", err)
	}

	srv := NewTCPServer(&config.Config{MaxConcurrentSyncs: 1}, store)

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleConn(context.Background(), serverConn)
	}()

	reqData, _ := json.Marshal(tcpRequest{FileHash: rec.FileHash})
	reqData = append(reqData, '\n')
	if _, err := clientConn.Write(reqData); err != nil {
		t.Fatalf("write request: %v", err)
	}

	reader := bufio.NewReader(clientConn)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read response header: %v", err)
	}

	var resp tcpResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("server returned error: %s", resp.Error)
	}
	if resp.FileSize != rec.FileSize {
		t.Fatalf("unexpected response file_size: got %d want %d", resp.FileSize, rec.FileSize)
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read file body: %v", err)
	}
	if int64(len(body)) != rec.FileSize {
		t.Fatalf("unexpected body length: got %d want %d", len(body), rec.FileSize)
	}
	if !bytes.Equal(body, snapshot) {
		t.Fatalf("body content mismatch: got %q want %q", body, snapshot)
	}

	gotHash := sha256.Sum256(body)
	if hex.EncodeToString(gotHash[:]) != rec.FileHash {
		t.Fatalf("hash mismatch after transfer: got %s want %s", hex.EncodeToString(gotHash[:]), rec.FileHash)
	}

	<-done
}
