// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package kvstore is a small storage abstraction that lets the backing engine
// be swapped at build time: Badger (util/lexi) on desktop and bbolt
// (util/glossi) on mobile. The interfaces here capture the common surface of
// those two engines, which implement them natively (lexi.DB and glossi.DB are
// both a Store); build-time selection lives in util/kvstore/engine.
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

// The sentinel errors are shared with the engines: both alias them directly,
// so errors returned by caller-supplied callbacks and index functions are
// recognized by whichever engine is in use, and errors tested with errors.Is
// against these sentinels match regardless of engine. lexi translates badger's
// ErrKeyNotFound to this package's internally.
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
// may return ErrNotIndexed to skip the datum. The function must derive the
// entry from the KVs alone and must not modify them: the stored bytes are
// parsed before the engine transaction opens, while index functions run
// inside it (possibly more than once if the engine retries), so a mutation
// would desync the index entry from the stored record.
type IndexFunc func(k, v KV) ([]byte, error)

// BatchEntry is a single key/value pair for batch insertion, used by both
// StoreTable.SetBatch and the engines' Table.SetBatch. The fields accept any
// KV; they are parsed with ParseKV at insertion time.
type BatchEntry struct {
	K, V KV
}

// Store is an indexed key-value store. It is implemented natively by
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
	// UseDefaultSetOptions establishes defaults applied to every Set and
	// SetBatch before the call's own options, so per-call options win where
	// they overlap. Intended for table setup; not synchronized with
	// concurrent use of the table.
	UseDefaultSetOptions(opts ...SetOption)
	// UseDefaultIterationOptions establishes defaults applied to every
	// Iterate before the call's own options (WithForward overrides a
	// WithReverse default). Intended for table setup; not synchronized with
	// concurrent use of the table.
	UseDefaultIterationOptions(opts ...IterationOption)
	// Set inserts a value for the key, parsing both with ParseKV. Values whose
	// types implement encoding.BinaryMarshaler can be passed directly, which
	// lets index functions recover the typed value instead of re-decoding the
	// stored bytes. The caller must not mutate k or v until Set returns: the
	// stored bytes are parsed up front while index entries are derived from
	// the live values inside the write transaction, so a concurrent mutation
	// would desync the indexes from the stored record. Without WithReplace,
	// an existing key is an error.
	Set(k, v KV, opts ...SetOption) error
	// SetBatch inserts every entry in a single underlying transaction. On
	// engines that fsync per write (bbolt on mobile) this collapses N fsyncs
	// into one, which matters for bulk inserts such as an initial history sync
	// or a migration. The options apply to every entry; without WithReplace,
	// any existing key rolls back the whole batch.
	SetBatch(entries []BatchEntry, opts ...SetOption) error
	// Get returns the value for the key, or ErrKeyNotFound.
	Get(k KV, opts ...GetOption) ([]byte, error)
	// Delete removes the value for the key along with its index entries.
	Delete(k KV) error
	// Iterate visits primary keys in key order, optionally constrained to a
	// prefix, subject to the IterationOptions.
	Iterate(prefix KV, f func(StoreIter) error, opts ...IterationOption) error
}

// StoreIndex is a secondary index on a StoreTable.
type StoreIndex interface {
	// UseDefaultIterationOptions establishes defaults applied to every
	// Iterate before the call's own options; see the StoreTable method of
	// the same name.
	UseDefaultIterationOptions(opts ...IterationOption)
	// Iterate visits index entries in order. prefix and WithSeek are in index
	// space.
	Iterate(prefix KV, f func(StoreIter) error, opts ...IterationOption) error
}

// StoreIter is an entry yielded during iteration. The exposed bytes are only
// valid for the duration of the callback.
type StoreIter interface {
	// K is the datum's primary key.
	K() ([]byte, error)
	// V provides access to the datum's value bytes.
	V(func([]byte) error) error
	// Entry provides access to the index entry when iterating an index; when
	// iterating a table it yields the primary key.
	Entry(func([]byte) error) error
	// Delete removes the datum along with all of its index entries. The
	// iteration must have been started with WithUpdate.
	Delete() error
}
