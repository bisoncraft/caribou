// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package txdb

import (
	"github.com/bisoncraft/bosswallet/util"
)

// The transaction database is built on a small storage abstraction so that the
// backing engine can be swapped at build time: Badger (util/lexi) on desktop and
// bbolt (wallet/glossi) on mobile. The interfaces below capture the common
// surface of those two engines.
//
// The sentinel errors deliberately use the same string values as both engines
// (util.ErrorKind compares by value), so ErrEndIteration and ErrNotIndexed
// returned by txdb-level callbacks/index functions are recognized by whichever
// engine is in use without translation. Only ErrKeyNotFound differs between the
// engines and is translated to this one inside the Store.Get adapters.
const (
	// ErrKeyNotFound is returned by Table.Get when a key is absent.
	ErrKeyNotFound = util.ErrorKind("key not found")
	// ErrEndIteration may be returned from an Iterate callback to stop iteration
	// without an error.
	ErrEndIteration = util.ErrorKind("end iteration")
	// ErrNotIndexed may be returned from an IndexFunc to skip indexing a datum.
	ErrNotIndexed = util.ErrorKind("not indexed")
)

// IndexFunc derives an index entry from a datum's key and value. It may return
// ErrNotIndexed to skip the datum.
type IndexFunc func(k, v []byte) ([]byte, error)

// Store is an indexed key-value store. It is implemented by adapters over
// util/lexi (Badger) and wallet/glossi (bbolt).
type Store interface {
	// Table returns (creating if necessary) a namespaced, indexable table.
	Table(name string) (StoreTable, error)
	// Close closes the underlying database.
	Close() error
}

// StoreTable is a namespaced section of a Store that supports secondary indexes.
type StoreTable interface {
	AddIndex(name string, f IndexFunc) (StoreIndex, error)
	AddUniqueIndex(name string, f IndexFunc) (StoreIndex, error)
	// Set inserts a value for the key. If replace is false and the key exists,
	// an error is returned.
	Set(k, v []byte, replace bool) error
	// Get returns the value for the key, or ErrKeyNotFound.
	Get(k []byte) ([]byte, error)
	Delete(k []byte) error
	// Iterate visits primary keys, optionally constrained to a prefix, in
	// forward or reverse order, optionally starting at seek.
	Iterate(prefix []byte, f func(StoreIter) error, reverse bool, seek []byte) error
}

// StoreIndex is a secondary index on a StoreTable.
type StoreIndex interface {
	// Iterate visits index entries in order. prefix and seek are in index space.
	Iterate(prefix []byte, f func(StoreIter) error, reverse bool, seek []byte) error
}

// StoreIter is an entry yielded during iteration. The exposed bytes are only
// valid for the duration of the callback.
type StoreIter interface {
	K() ([]byte, error)
	V(func([]byte) error) error
}
