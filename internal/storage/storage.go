// Package storage implements the BoltDB-backed persistence layer for EchoSend.
//
// Three top-level buckets are used:
//
//	"nodes"    – known LAN peers, keyed by NodeID
//	"messages" – received/sent text messages, keyed by MessageID
//	"files"    – file metadata + download state, keyed by FileHash
//
// All write operations perform strict deduplication before touching disk,
// keeping I/O frequency low on constrained devices.
package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"

	"p2p-sync/internal/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// Bucket names
// ─────────────────────────────────────────────────────────────────────────────

var (
	bucketNodes    = []byte("nodes")
	bucketMessages = []byte("messages")
	bucketFiles    = []byte("files")
)

// ─────────────────────────────────────────────────────────────────────────────
// Store
// ─────────────────────────────────────────────────────────────────────────────

// Store wraps a BoltDB instance and exposes domain-level read/write helpers.
// It is safe for concurrent use from multiple goroutines.
type Store struct {
	db *bolt.DB
}

// Open opens (or creates) the BoltDB database file inside dir and initialises
// all required buckets.  The returned Store must be closed with Close() when
// the daemon shuts down.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: mkdir %s: %w", dir, err)
	}

	dbPath := filepath.Join(dir, "echosend.db")

	opts := &bolt.Options{
		Timeout:      2 * time.Second,
		FreelistType: bolt.FreelistMapType, // lower memory footprint
	}

	db, err := bolt.Open(dbPath, 0o600, opts)
	if err != nil {
		return nil, fmt.Errorf("storage: open db %s: %w", dbPath, err)
	}

	// Ensure all buckets exist in a single write transaction.
	err = db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{bucketNodes, bucketMessages, bucketFiles} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return fmt.Errorf("create bucket %s: %w", bucket, err)
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: init buckets: %w", err)
	}

	return &Store{db: db}, nil
}

// Close flushes pending writes and releases the database file lock.
func (s *Store) Close() error {
	return s.db.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Nodes
// ─────────────────────────────────────────────────────────────────────────────

// UpsertNode writes node into the nodes bucket.
// If a record with the same NodeID already exists it is overwritten, which
// keeps the LastSeen timestamp and IP address up-to-date.
func (s *Store) UpsertNode(node models.NodeInfo) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		data, err := json.Marshal(node)
		if err != nil {
			return err
		}
		return b.Put([]byte(node.NodeID), data)
	})
}

// GetNode returns the NodeInfo for nodeID, or (NodeInfo{}, false, nil) if not
// found.  A non-nil error indicates a database failure.
func (s *Store) GetNode(nodeID string) (models.NodeInfo, bool, error) {
	var node models.NodeInfo
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketNodes).Get([]byte(nodeID))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &node)
	})
	return node, found, err
}

// ListNodes returns all known peers ordered by LastSeen descending.
func (s *Store) ListNodes() ([]models.NodeInfo, error) {
	var nodes []models.NodeInfo
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketNodes).ForEach(func(_, v []byte) error {
			var n models.NodeInfo
			if err := json.Unmarshal(v, &n); err != nil {
				return err
			}
			nodes = append(nodes, n)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].LastSeen > nodes[j].LastSeen
	})
	return nodes, nil
}

// DeleteNode removes a peer record (e.g. after a long absence).
func (s *Store) DeleteNode(nodeID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketNodes).Delete([]byte(nodeID))
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Messages
// ─────────────────────────────────────────────────────────────────────────────

// ErrDuplicateMessage is returned when the MessageID already exists in the
// database.  Callers should treat this as a no-op and discard the packet.
var ErrDuplicateMessage = errors.New("storage: duplicate message")

// InsertMessage persists msg if its MessageID has not been seen before.
// Returns ErrDuplicateMessage without writing if already present, which lets
// the gossip layer cheaply discard re-broadcast duplicates.
func (s *Store) InsertMessage(msg models.Message) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMessages)
		key := []byte(msg.MessageID)
		if b.Get(key) != nil {
			return ErrDuplicateMessage
		}
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}

// GetMessage retrieves a single message by its ID.
func (s *Store) GetMessage(messageID string) (models.Message, bool, error) {
	var msg models.Message
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketMessages).Get([]byte(messageID))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &msg)
	})
	return msg, found, err
}

// ListMessages returns all messages ordered by Timestamp descending.
// Pass limit <= 0 to retrieve all records.
func (s *Store) ListMessages(limit int) ([]models.Message, error) {
	var msgs []models.Message
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMessages).ForEach(func(_, v []byte) error {
			var m models.Message
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			msgs = append(msgs, m)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].Timestamp > msgs[j].Timestamp
	})

	if limit > 0 && len(msgs) > limit {
		msgs = msgs[:limit]
	}
	return msgs, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Files
// ─────────────────────────────────────────────────────────────────────────────

// ErrDuplicateFile is returned when a FileRecord with the same FileHash is
// already present and has status COMPLETE or SEEDING (i.e. we already own it).
var ErrDuplicateFile = errors.New("storage: file already complete or seeding")

// InsertFileRecord persists a new file record keyed by FileHash.
// Returns ErrDuplicateFile if a complete/seeding record already exists so the
// caller can skip re-downloading a file we already have.
// If a KNOWN/FAILED/DOWNLOADING record exists it is overwritten, allowing
// retry logic to work correctly.
func (s *Store) InsertFileRecord(rec models.FileRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		key := []byte(rec.FileHash)

		existing := b.Get(key)
		if existing != nil {
			var prev models.FileRecord
			if err := json.Unmarshal(existing, &prev); err == nil {
				if prev.Status == models.FileStatusComplete ||
					prev.Status == models.FileStatusSeeding {
					return ErrDuplicateFile
				}
			}
		}

		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}

// UpdateFileStatus atomically updates the Status and (optionally) the LocalPath
// of the file record identified by fileHash.  It is a no-op if the record does
// not exist.
func (s *Store) UpdateFileStatus(fileHash string, status models.FileStatus, localPath string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		key := []byte(fileHash)

		v := b.Get(key)
		if v == nil {
			return nil // record not found – benign race condition, ignore
		}

		var rec models.FileRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return err
		}

		rec.Status = status
		if localPath != "" {
			rec.LocalPath = localPath
		}

		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}

// GetFile retrieves a FileRecord by its SHA-256 hash.
func (s *Store) GetFile(fileHash string) (models.FileRecord, bool, error) {
	var rec models.FileRecord
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketFiles).Get([]byte(fileHash))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	return rec, found, err
}

// ListFiles returns all file records ordered by Timestamp descending.
// Pass limit <= 0 to retrieve all records.
func (s *Store) ListFiles(limit int) ([]models.FileRecord, error) {
	var files []models.FileRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketFiles).ForEach(func(_, v []byte) error {
			var f models.FileRecord
			if err := json.Unmarshal(v, &f); err != nil {
				return err
			}
			files = append(files, f)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Timestamp > files[j].Timestamp
	})

	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	return files, nil
}

// ListFilesByStatus returns file records matching the given status,
// ordered by Timestamp descending.
func (s *Store) ListFilesByStatus(status models.FileStatus) ([]models.FileRecord, error) {
	var files []models.FileRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketFiles).ForEach(func(_, v []byte) error {
			var f models.FileRecord
			if err := json.Unmarshal(v, &f); err != nil {
				return err
			}
			if f.Status == status {
				files = append(files, f)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Timestamp > files[j].Timestamp
	})
	return files, nil
}

// DeleteFile removes a file record (e.g. after a failed download is cleaned up).
func (s *Store) DeleteFile(fileHash string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketFiles).Delete([]byte(fileHash))
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Housekeeping
// ─────────────────────────────────────────────────────────────────────────────

// PruneStaleNodes removes peer records whose LastSeen timestamp is older than
// maxAge.  Call this periodically (e.g. every minute) to keep the nodes bucket
// from growing without bound.
func (s *Store) PruneStaleNodes(maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge).UnixNano()
	var stale []string

	// Collect stale keys in a read transaction first.
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketNodes).ForEach(func(k, v []byte) error {
			var n models.NodeInfo
			if err := json.Unmarshal(v, &n); err != nil {
				stale = append(stale, string(k)) // corrupt record – also prune
				return nil
			}
			if n.LastSeen < cutoff {
				stale = append(stale, string(k))
			}
			return nil
		})
	})
	if err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}

	// Delete in a single write transaction.
	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		for _, id := range stale {
			if err := b.Delete([]byte(id)); err != nil {
				return err
			}
		}
		return nil
	})
	return len(stale), err
}

// Stats returns a quick summary of record counts across all buckets.
func (s *Store) Stats() (nodes, messages, files int, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		nodes = tx.Bucket(bucketNodes).Stats().KeyN
		messages = tx.Bucket(bucketMessages).Stats().KeyN
		files = tx.Bucket(bucketFiles).Stats().KeyN
		return nil
	})
	return
}
