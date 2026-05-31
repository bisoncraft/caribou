// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package glossi is a bbolt-backed key-value store with secondary indexing. Its
// API mirrors util/lexi (which is backed by Badger) so that the two are
// interchangeable. Glossi is intended for environments where a pure-Go,
// cgo-free, low-overhead embedded database is preferred, e.g. mobile.
package glossi

import (
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"go.etcd.io/bbolt"
)

// ErrKeyNotFound is returned when a requested key is not in the database. It
// mirrors lexi.ErrKeyNotFound so consumers don't have to care which engine is
// in use.
const ErrKeyNotFound = util.ErrorKind("key not found")

// DB is a bbolt database with the ability to add indexed data via Tables.
type DB struct {
	*bbolt.DB
	log util.Logger
}

// New opens (or creates) a glossi database at the provided path.
func New(path string, log util.Logger) (*DB, error) {
	bdb, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	return &DB{
		DB:  bdb,
		log: log,
	}, nil
}

// Close closes the underlying bbolt database.
func (db *DB) Close() error {
	return db.DB.Close()
}
