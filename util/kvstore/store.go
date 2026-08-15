// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package kvstore is a small storage abstraction that lets the backing engine
// be swapped at build time: Badger (util/lexi) on desktop and bbolt
// (util/glossi) on mobile. The interfaces here capture the common surface of
// those two engines, so a database built on them works under either. Each
// engine package provides a NewStore constructor returning a Store; build-time
// selection lives in util/kvstore/engine.
//
// The abstraction deliberately omits transactions. lexi can compose an atomic
// write across tables and glossi cannot, so the honest intersection is the
// per-table atomicity that SetBatch provides on both. A database needing more
// than that cannot be expressed against both engines.
package kvstore

import (
	"context"
	"sync"

	"github.com/bisoncraft/caribou/util"
)

// The sentinel errors are shared with the engines: glossi aliases them
// directly, and lexi's ErrEndIteration and ErrNotIndexed deliberately use the
// same string values (util.ErrorKind compares by value), so errors returned by
// caller-supplied callbacks and index functions are recognized by whichever
// engine is in use without translation. Only lexi's ErrKeyNotFound
// (badger.ErrKeyNotFound) differs and is translated to this one inside its
// store adapter.
const (
	// ErrKeyNotFound is returned by Table.Get when a key is absent.
	ErrKeyNotFound = util.ErrorKind("key not found")
	// ErrEndIteration may be returned from an Iterate callback to stop iteration
	// without an error.
	ErrEndIteration = util.ErrorKind("end iteration")
	// ErrNotIndexed may be returned from an IndexFunc to skip indexing a datum.
	ErrNotIndexed = util.ErrorKind("not indexed")
)

// IndexFunc derives an index entry from a datum's key and value. The engines
// pass the caller's original (unparsed) KVs, so a function indexing a typed
// value can type-assert it back rather than re-decoding the stored bytes. It
// may return ErrNotIndexed to skip the datum.
type IndexFunc func(k, v KV) ([]byte, error)

// BatchEntry is a single key/value pair for batch insertion, used by both
// StoreTable.SetBatch and the engines' Table.SetBatch. The fields accept any
// KV; they are parsed with ParseKV at insertion time.
type BatchEntry struct {
	K, V KV
}

// Store is an indexed key-value store. It is implemented by adapters over
// util/lexi (Badger) and util/glossi (bbolt).
type Store interface {
	// Connect ties the store's shutdown to ctx: when ctx is canceled, the
	// underlying database is closed. The returned WaitGroup completes once
	// shutdown is finished. Long-running consumers should call Connect; tests
	// and one-shot tools may instead use Close directly.
	Connect(ctx context.Context) (*sync.WaitGroup, error)
	// Table returns (creating if necessary) a namespaced, indexable table.
	Table(name string) (StoreTable, error)
	// Close closes the underlying database. Only safe to call when Connect has
	// not been called; for the managed lifecycle, cancel the context passed to
	// Connect and wait the returned WaitGroup.
	Close() error
}

// StoreTable is a namespaced section of a Store that supports secondary indexes.
type StoreTable interface {
	AddIndex(name string, f IndexFunc) (StoreIndex, error)
	AddUniqueIndex(name string, f IndexFunc) (StoreIndex, error)
	// Set inserts a value for the key, parsing both with ParseKV. Values whose
	// types implement encoding.BinaryMarshaler can be passed directly, which
	// lets index functions recover the typed value instead of re-decoding the
	// stored bytes. If replace is false and the key exists, an error is
	// returned.
	Set(k, v KV, replace bool) error
	// SetBatch inserts every entry in a single underlying transaction. On
	// engines that fsync per write (bbolt on mobile) this collapses N fsyncs
	// into one, which matters for bulk inserts such as an initial history sync
	// or a migration. replace has the same meaning as in Set; if it is false
	// and any key already exists, the whole batch is rolled back.
	SetBatch(entries []BatchEntry, replace bool) error
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
