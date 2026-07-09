// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package glossi is a bbolt-backed key-value store with secondary indexing. Its
// API mirrors util/lexi (which is backed by Badger) so that the two are
// interchangeable. Glossi is intended for environments where a pure-Go,
// cgo-free, low-overhead embedded database is preferred, e.g. mobile.
package glossi

import (
	"context"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"go.etcd.io/bbolt"
)

// ErrKeyNotFound is returned when a requested key is not in the database. It
// mirrors lexi.ErrKeyNotFound so consumers don't have to care which engine is
// in use.
const ErrKeyNotFound = util.ErrorKind("key not found")

// DB is a bbolt database with the ability to add indexed data via Tables. The
// underlying *bbolt.DB is intentionally unexported so the Connect-managed
// lifecycle cannot be bypassed by external callers; package-internal use goes
// through the Update/View wrappers below.
type DB struct {
	bdb *bbolt.DB
	log util.Logger
	wg  sync.WaitGroup
}

// New opens (or creates) a glossi database at the provided path. The DB is
// usable immediately for ad-hoc work and for tests that drive Close directly.
// Long-running consumers should call Connect to bind the DB's lifetime to a
// context.
func New(path string, log util.Logger) (*DB, error) {
	bdb, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	return &DB{
		bdb: bdb,
		log: log,
	}, nil
}

// Connect ties the DB's shutdown to ctx: when ctx is canceled, the DB is
// closed. The returned WaitGroup completes once shutdown is finished. This
// mirrors lexi.DB.Connect so the two engines are interchangeable behind a
// common interface. bbolt has no periodic maintenance work, so the spawned
// goroutine only waits on ctx.
//
// Do not call Close after Connect; cancel ctx and wait the returned WaitGroup
// instead.
func (db *DB) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	db.wg.Add(1)
	go func() {
		defer db.wg.Done()
		<-ctx.Done()
		if err := db.bdb.Close(); err != nil {
			db.log.Errorf("Error closing glossi DB: %v", err)
		}
	}()
	return &db.wg, nil
}

// Close closes the underlying bbolt database. Only safe to call when Connect
// has not been called; for the managed lifecycle, cancel the context passed to
// Connect and wait the returned WaitGroup.
func (db *DB) Close() error {
	return db.bdb.Close()
}

// Update runs f inside a bbolt read-write transaction.
func (db *DB) Update(f func(*bbolt.Tx) error) error { return db.bdb.Update(f) }

// View runs f inside a bbolt read-only transaction.
func (db *DB) View(f func(*bbolt.Tx) error) error { return db.bdb.View(f) }
