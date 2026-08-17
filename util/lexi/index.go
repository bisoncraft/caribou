// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/dgraph-io/badger/v4"
)

const (
	// ErrEndIteration can be returned from the function passed to Iterate
	// to end iteration. No error will be returned from Iterate.
	ErrEndIteration = kvstore.ErrEndIteration
	// ErrNotIndexed can be returned from the function passed to AddIndex
	// to indicate that a datum should not be added to the index.
	ErrNotIndexed = kvstore.ErrNotIndexed
)

// Index is just a lexicographically-ordered list of byte slices. An Index is
// associated with a Table, and a datum inserted into a table can put entries
// into the Index. The Index can be iterated to view sorted data in the table.
// Index implements kvstore.StoreIndex.
type Index struct {
	*DB
	name                    string
	table                   *Table
	prefix                  keyPrefix
	f                       kvstore.IndexFunc
	unique                  bool
	defaultIterationOptions kvstore.IterationOpts
}

func (t *Table) addIndex(name string, f kvstore.IndexFunc, unique bool) (*Index, error) {
	p, err := t.prefixForName(t.name + indexNameSeparator + name)
	if err != nil {
		return nil, err
	}
	idx := &Index{
		DB:     t.DB,
		name:   name,
		table:  t,
		prefix: p,
		f:      f,
		unique: unique,
	}
	t.indexes = append(t.indexes, idx)
	return idx, nil
}

// AddIndex adds an index to a Table. After an index is added, every item
// added to the Table with Set will automatically generate a corresponding
// entry in this index.
func (t *Table) AddIndex(name string, f kvstore.IndexFunc) (kvstore.StoreIndex, error) {
	idx, err := t.addIndex(name, f, false)
	if err != nil {
		return nil, err
	}
	return idx, nil
}

// AddUniqueIndex adds a unique index to a Table. After an index is added, every
// item added to the Table with Set will automatically generate a corresponding
// entry in this index. The unique index guarantees that no two entries in the
// index have the same value. If you attempt to add an entry that would create a
// duplicate in the unique index, an error will be returned. However, if Set() is
// called with the WithReplace() option, the existing entry will be overwritten
// instead.
func (t *Table) AddUniqueIndex(name string, f kvstore.IndexFunc) (kvstore.StoreIndex, error) {
	idx, err := t.addIndex(name, f, true)
	if err != nil {
		return nil, err
	}
	return idx, nil
}

// uniqueIndexConflictError is returned when a unique index conflict is encountered.
// The caller should delete the table entry identified by conflictDBID before adding
// the new entry.
type uniqueIndexConflictError struct {
	indexName    string
	conflictDBID DBID
}

func (e uniqueIndexConflictError) Error() string {
	return fmt.Sprintf("unique index conflict on %s", e.indexName)
}

func (idx *Index) checkForIndexConflict(txn *badger.Txn, idxB []byte, updatingDBID DBID) error {
	conflictPrefix := PrefixedKey(idx.prefix, idxB)

	var conflictDBID DBID
	var foundConflict bool

	err := iteratePrefix(txn, conflictPrefix, nil, func(iter *badger.Iterator) error {
		item := iter.Item()
		key := item.Key()

		if len(key) < PrefixSize+DBIDSize {
			return fmt.Errorf("index entry too small. length = %d", len(key))
		}

		existingDBID := newDBIDFromBytes(key[len(key)-DBIDSize:])

		if existingDBID != updatingDBID {
			conflictDBID = existingDBID
			foundConflict = true
			return ErrEndIteration
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("error checking for index uniqueness: %w", err)
	}

	if foundConflict {
		return uniqueIndexConflictError{
			indexName:    idx.name,
			conflictDBID: conflictDBID,
		}
	}

	return nil
}

// add adds an entry to the index. If this element is not required to be
// indexed, a nil byte slice is returned and no error is returned.
func (idx *Index) add(txn *badger.Txn, k, v KV, dbID DBID) ([]byte, error) {
	idxB, err := idx.f(k, v)
	if err != nil {
		if errors.Is(err, ErrNotIndexed) {
			return nil, nil
		}
		return nil, fmt.Errorf("error getting index value: %w", err)
	}
	b := PrefixedKey(idx.prefix, append(idxB, dbID[:]...))

	if idx.unique {
		if err := idx.checkForIndexConflict(txn, idxB, dbID); err != nil {
			return nil, err
		}
	}

	if err := txn.Set(b, nil); err != nil {
		return nil, fmt.Errorf("error writing index entry: %w", err)
	}
	return b, nil
}

// UseDefaultIterationOptions sets default options for Iterate.
func (idx *Index) UseDefaultIterationOptions(optss ...kvstore.IterationOption) {
	for i := range optss {
		optss[i](&idx.defaultIterationOptions)
	}
}

// Iter is an entry in the Index or Table. The caller can use Iter to access and
// delete data associated with the entry and it's datum. Iter implements
// kvstore.StoreIter.
type Iter struct {
	table   *Table
	isIndex bool
	item    *badger.Item
	txn     *badger.Txn
	dbID    DBID
	d       *datum
	// update records whether the iteration runs in a read-write transaction
	// (WithUpdate), which Delete requires.
	update bool
	// k is the caller's key, set during table iteration where the key-map entry
	// already carries it. Nil during index iteration, where K resolves it.
	k []byte
}

// V gives access to the datum bytes. The byte slice passed to f is only valid
// for the duration of the function call. The caller should make a copy if they
// intend to use the bytes outside of the scope of f.
func (i *Iter) V(f func(vB []byte) error) error {
	d, err := i.datum()
	if err != nil {
		return err
	}
	return f(d.v)
}

// K is the key for the datum.
func (i *Iter) K() ([]byte, error) {
	if i.k != nil {
		return CloneBytes(i.k), nil
	}
	item, err := i.txn.Get(PrefixedKey(IdToKeyPrefix, i.dbID[:]))
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil, fmt.Errorf("dangling entry for table %q: %w", i.table.name, ErrKeyNotFound)
		}
		return nil, err
	}
	kB, err := item.ValueCopy(nil)
	if err != nil {
		return nil, err
	}
	return UnqualifyKey(kB)
}

// Entry is the actual index entry when iterating an Index. When iterating a
// Table it yields the primary key.
func (i *Iter) Entry(f func(idxB []byte) error) error {
	if !i.isIndex {
		return f(i.k)
	}
	k := i.item.Key()
	if len(k) < PrefixSize+DBIDSize {
		return fmt.Errorf("index entry too small. length = %d", len(k))
	}
	return f(k[PrefixSize : len(k)-DBIDSize])
}

func (i *Iter) datum() (_ *datum, err error) {
	if i.d != nil {
		return i.d, nil
	}
	i.d, err = i.table.get(i.txn, i.dbID)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			return nil, fmt.Errorf("dangling entry for table %q: %w", i.table.name, err)
		}
		return nil, err
	}
	return i.d, nil
}

// Delete deletes the indexed datum and any associated index entries. The
// iteration must have been started with WithUpdate.
func (i *Iter) Delete() error {
	if !i.update {
		return errors.New("lexi: Delete requires WithUpdate")
	}
	d, err := i.datum()
	if err != nil {
		return err
	}
	return i.table.deleteDatum(i.txn, i.dbID, d)
}

// Iterate iterates the index, providing access to the index entry, datum, and
// datum key via the Iter.
func (idx *Index) Iterate(prefixI KV, f func(kvstore.StoreIter) error, iterOpts ...kvstore.IterationOption) error {
	return idx.iterate(idx.prefix[:], idx.table, idx.defaultIterationOptions, true, prefixI, f, iterOpts...)
}

// iterate iterates a table or index. base is the byte range the iteration is
// confined to: an index's own prefix, or a table's key-map prefix. The caller's
// prefix and seek are appended to it, so both are interpreted in the space that
// range is ordered by - index-entry order for an index, user-key order for a
// table.
func (db *DB) iterate(base []byte, table *Table, io kvstore.IterationOpts, isIndex bool, prefixI KV, f func(kvstore.StoreIter) error, iterOpts ...kvstore.IterationOption) error {
	prefix, err := kvstore.ParseKV(prefixI)
	if err != nil {
		return err
	}
	for i := range iterOpts {
		iterOpts[i](&io)
	}
	iterFunc := iteratePrefix
	if io.Reverse {
		iterFunc = reverseIteratePrefix
	}
	viewUpdate := db.View
	if io.Update {
		viewUpdate = db.Update
	}
	joined := func(b []byte) []byte {
		k := make([]byte, len(base)+len(b))
		copy(k, base)
		copy(k[len(base):], b)
		return k
	}
	var seek []byte
	if len(io.Seek) > 0 {
		seek = joined(io.Seek)
	}
	return viewUpdate(func(txn *badger.Txn) error {
		return iterFunc(txn, joined(prefix), seek, func(iter *badger.Iterator) error {
			item := iter.Item()
			k := item.Key()

			it := &Iter{
				isIndex: isIndex,
				table:   table,
				item:    item,
				txn:     txn,
				update:  io.Update,
			}
			if isIndex {
				if len(k) < PrefixSize+DBIDSize {
					return fmt.Errorf("invalid index entry length %d", len(k))
				}
				it.dbID = newDBIDFromBytes(k[len(k)-DBIDSize:])
				return f(it)
			}
			// Table iteration: the entry is a key-map row, so the key past the
			// base is the caller's key and the value is its DBID.
			if len(k) <= len(base) {
				return fmt.Errorf("invalid table key-map entry length %d", len(k))
			}
			it.k = k[len(base):]
			return item.Value(func(v []byte) error {
				if len(v) != DBIDSize {
					return fmt.Errorf("invalid DBID length %d", len(v))
				}
				it.dbID = newDBIDFromBytes(v)
				return f(it)
			})
		})
	})
}

type badgerIterationOption func(opts *badger.IteratorOptions)

func withPrefetchSize(n int) badgerIterationOption {
	return func(opts *badger.IteratorOptions) {
		opts.PrefetchSize = n
	}
}

func iteratePrefix(txn *badger.Txn, prefix, seek []byte, f func(iter *badger.Iterator) error, iterOpts ...badgerIterationOption) error {
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	for i := range iterOpts {
		iterOpts[i](&opts)
	}
	iter := txn.NewIterator(opts)
	defer iter.Close()

	if len(seek) == 0 {
		iter.Rewind()
	} else {
		iter.Seek(seek)
	}

	for ; iter.Valid(); iter.Next() {
		if err := f(iter); err != nil {
			if errors.Is(err, ErrEndIteration) {
				return nil
			}
			return err
		}
	}
	return nil
}

// https://github.com/dgraph-io/badger/issues/436#issuecomment-1073008604
func seekLast(it *badger.Iterator, prefix []byte) {
	tweaked := make([]byte, len(prefix))
	copy(tweaked, prefix)
	n := len(prefix)
	for n > 0 {
		if tweaked[n-1] == 0xff {
			n -= 1
		} else {
			tweaked[n-1] += 1
			break
		}
	}
	tweaked = tweaked[0:n]
	it.Seek(tweaked)
	if it.Valid() && bytes.Equal(tweaked, it.Item().Key()) {
		it.Next()
	}
}

func reverseIteratePrefix(txn *badger.Txn, prefix, seek []byte, f func(iter *badger.Iterator) error, iterOpts ...badgerIterationOption) error {
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	opts.Reverse = true
	for i := range iterOpts {
		iterOpts[i](&opts)
	}
	iter := txn.NewIterator(opts)
	defer iter.Close()

	if len(seek) == 0 {
		seekLast(iter, prefix)
	} else {
		iter.Seek(append(seek, lastDBID[:]...))
	}

	for ; iter.ValidForPrefix(prefix); iter.Next() {
		if err := f(iter); err != nil {
			if errors.Is(err, ErrEndIteration) {
				return nil
			}
			return err
		}
	}
	return nil
}
