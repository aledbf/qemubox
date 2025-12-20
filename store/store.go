// Package boltstore provides typed storage backed by bbolt.
package boltstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	bolt "go.etcd.io/bbolt"
)

// Store provides type-safe key-value storage
type Store[T any] interface {
	Get(ctx context.Context, key string) (*T, error)
	Set(ctx context.Context, key string, value *T) error
	Delete(ctx context.Context, key string) error
	Scan(ctx context.Context, prefix string, fn func(key string, value *T) error) error
	Close() error
}

// ErrNotFound is returned when a key is missing from the store.
var ErrNotFound = errdefs.ErrNotFound

// BoltStore provides a bolt-backed implementation of Store[T]
// Multiple BoltStore instances can share the same underlying bolt.DB connection
type BoltStore[T any] struct {
	db         *bolt.DB
	bucketName []byte
	shared     bool // indicates if db is shared with other stores
}

var (
	// Global registry of shared database connections
	sharedDBs = make(map[string]*sharedDB)
	dbMu      sync.Mutex
)

type sharedDB struct {
	db       *bolt.DB
	refCount int
}

// NewBoltStore creates a new bolt store that shares a database connection with other stores
// using the same dbPath. This avoids lock contention when multiple stores access the same database.
func NewBoltStore[T any](dbPath string, bucketName string) (Store[T], error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0750); err != nil {
		return nil, fmt.Errorf("failed to create db dir: %w", err)
	}

	// Check if we already have a connection for this path
	sdb, exists := sharedDBs[dbPath]
	if !exists {
		// Open new connection
		db, err := bolt.Open(dbPath, 0600, &bolt.Options{
			Timeout:        30 * time.Second,
			NoFreelistSync: true,
			FreelistType:   bolt.FreelistMapType,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to open bolt db: %w", err)
		}

		sdb = &sharedDB{
			db:       db,
			refCount: 0,
		}
		sharedDBs[dbPath] = sdb
	}

	// Increment reference count
	sdb.refCount++

	// Create bucket if it doesn't exist
	err := sdb.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucketName))
		return err
	})
	if err != nil {
		sdb.refCount--
		if sdb.refCount == 0 {
			_ = sdb.db.Close()
			delete(sharedDBs, dbPath)
		}
		return nil, fmt.Errorf("failed to create bucket: %w", err)
	}

	return &BoltStore[T]{
		db:         sdb.db,
		bucketName: []byte(bucketName),
		shared:     true,
	}, nil
}

// Get retrieves a value by key
func (s *BoltStore[T]) Get(ctx context.Context, key string) (*T, error) {
	var value T
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucketName)
		if b == nil {
			return fmt.Errorf("bucket %s not found", string(s.bucketName))
		}
		data := b.Get([]byte(key))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &value)
	})
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// Set stores a value by key
func (s *BoltStore[T]) Set(ctx context.Context, key string, value *T) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucketName)
		if b == nil {
			return fmt.Errorf("bucket %s not found", string(s.bucketName))
		}
		data, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("failed to marshal value: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

// Delete removes a value by key
func (s *BoltStore[T]) Delete(ctx context.Context, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucketName)
		if b == nil {
			return fmt.Errorf("bucket %s not found", string(s.bucketName))
		}
		return b.Delete([]byte(key))
	})
}

// Scan iterates over all keys with the given prefix
func (s *BoltStore[T]) Scan(ctx context.Context, prefix string, fn func(key string, value *T) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucketName)
		if b == nil {
			return fmt.Errorf("bucket %s not found", string(s.bucketName))
		}
		c := b.Cursor()

		prefixBytes := []byte(prefix)
		for k, v := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, v = c.Next() {
			var value T
			if err := json.Unmarshal(v, &value); err != nil {
				return fmt.Errorf("failed to unmarshal value for key %s: %w", string(k), err)
			}
			if err := fn(string(k), &value); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close decrements the reference count for shared databases
// The actual database is only closed when the last reference is removed
func (s *BoltStore[T]) Close() error {
	if !s.shared {
		// Not a shared connection, close directly
		if s.db != nil {
			return s.db.Close()
		}
		return nil
	}

	// For shared connections, decrement reference count
	dbMu.Lock()
	defer dbMu.Unlock()

	// Find the shared DB entry
	for path, sdb := range sharedDBs {
		if sdb.db == s.db {
			sdb.refCount--
			if sdb.refCount == 0 {
				// Last reference, close the database
				err := sdb.db.Close()
				delete(sharedDBs, path)
				return err
			}
			return nil
		}
	}

	return nil
}
