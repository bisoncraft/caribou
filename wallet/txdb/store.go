// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package txdb

import "github.com/bisoncraft/caribou/util/kvstore"

// The storage abstraction this package is built on lives in util/kvstore, which
// is shared with other databases. The names are aliased here because wallets
// use them to manage their own auxiliary tables through NetworkTxDB.

type (
	// KV is a key or value argument accepted by the store: raw bytes, one of
	// a few common scalar types, or an encoding.BinaryMarshaler.
	KV = kvstore.KV
	// Store is an indexed key-value store.
	Store = kvstore.Store
	// StoreTable is a namespaced section of a Store that supports secondary
	// indexes.
	StoreTable = kvstore.StoreTable
	// StoreIndex is a secondary index on a StoreTable.
	StoreIndex = kvstore.StoreIndex
	// StoreIter is an entry yielded during iteration.
	StoreIter = kvstore.StoreIter
	// IndexFunc derives an index entry from a datum's key and value.
	IndexFunc = kvstore.IndexFunc
	// BatchEntry is a single key/value pair for StoreTable.SetBatch.
	BatchEntry = kvstore.BatchEntry
	// SetOption is a knob controlling StoreTable.Set and SetBatch.
	SetOption = kvstore.SetOption
	// GetOption is a knob controlling StoreTable.Get.
	GetOption = kvstore.GetOption
	// IterationOption is a knob controlling Iterate on a table or index.
	IterationOption = kvstore.IterationOption
)

// The option constructors are re-exported as functions rather than assigned
// package variables so an importer cannot reassign them.

// WithReplace allows replacing a pre-existing value when calling Set.
func WithReplace() SetOption { return kvstore.WithReplace() }

// WithReverse sets the direction of iteration to reverse-lexicographical.
func WithReverse() IterationOption { return kvstore.WithReverse() }

// WithSeek starts iteration at the specified key.
func WithSeek(seek []byte) IterationOption { return kvstore.WithSeek(seek) }

// WithUpdate must be used if an Iterate callback intends to modify the
// database, e.g. via StoreIter.Delete.
func WithUpdate() IterationOption { return kvstore.WithUpdate() }

// WithForward sets the direction of iteration to lexicographical, clearing a
// WithReverse default established with UseDefaultIterationOptions.
func WithForward() IterationOption { return kvstore.WithForward() }

const (
	// ErrKeyNotFound is returned by StoreTable.Get when a key is absent.
	ErrKeyNotFound = kvstore.ErrKeyNotFound
	// ErrEndIteration may be returned from an Iterate callback to stop
	// iteration without an error.
	ErrEndIteration = kvstore.ErrEndIteration
	// ErrNotIndexed may be returned from an IndexFunc to skip indexing a datum.
	ErrNotIndexed = kvstore.ErrNotIndexed
)
