package store

import (
	"errors"
	"fmt"
	"io"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Backup writes a consistent copy of the whole database to w while it stays in use.
// The copy holds password hashes, token hashes and the JWT secret: admin-only.
func (s *BoltStore) Backup(w io.Writer) (int64, error) {
	var n int64
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		n, err = tx.WriteTo(w)
		return err
	})
	return n, err
}

// ValidateBackup checks that path is a Velocor database: it opens, read-only, and holds the
// node's system configuration.
func ValidateBackup(path string) error {
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 3 * time.Second})
	if err != nil {
		return fmt.Errorf("%w: not a Velocor database: %v", ErrInvalidArgument, err)
	}
	defer func() { _ = db.Close() }()
	s := &BoltStore{db: db}
	// Buckets added by later versions are created when the database is opened; only the
	// system bucket must exist.
	hasSystem := false
	_ = db.View(func(tx *bolt.Tx) error { hasSystem = tx.Bucket(bucketSystem) != nil; return nil })
	if !hasSystem {
		return fmt.Errorf("%w: not a Velocor database", ErrInvalidArgument)
	}
	cfg, err := s.GetSystemConfig()
	if errors.Is(err, ErrNotFound) || (err == nil && cfg.NodeID == "") {
		return fmt.Errorf("%w: the database has no node configuration", ErrInvalidArgument)
	}
	return err
}
