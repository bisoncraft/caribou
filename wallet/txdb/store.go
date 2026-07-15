// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package txdb

import "github.com/bisoncraft/caribou/util/kvstore"

// The storage abstraction this package is built on lives in util/kvstore, which
// is shared with other databases. The names are aliased here because wallets
// use them to manage their own auxiliary tables through NetworkTxDB.

type (
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
)

const (
	// ErrKeyNotFound is returned by StoreTable.Get when a key is absent.
	ErrKeyNotFound = kvstore.ErrKeyNotFound
	// ErrEndIteration may be returned from an Iterate callback to stop
	// iteration without an error.
	ErrEndIteration = kvstore.ErrEndIteration
	// ErrNotIndexed may be returned from an IndexFunc to skip indexing a datum.
	ErrNotIndexed = kvstore.ErrNotIndexed
)
