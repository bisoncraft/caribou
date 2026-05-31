// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package glossi

import (
	"errors"
	"fmt"

	"github.com/bisoncraft/bosswallet/util"
	"go.etcd.io/bbolt"
)

// ErrUniqueConflict is returned by Set when an insert would violate a unique
// index constraint and WithReplace was not specified.
const ErrUniqueConflict = util.ErrorKind("unique index conflict")

var (
	// Sub-buckets within a Table's bucket.
	dataBucketKey  = []byte("d") // primary key -> datum
	indexBucketKey = []byte("i") // indexID||indexEntry||key -> key
	metaBucketKey  = []byte("m") // index name -> indexID, plus the id sequence

	// indexSeqKey is the reserved meta key holding the next index id. Index
	// names are non-empty, so a single 0x00 byte cannot collide with one.
	indexSeqKey = []byte{0x00}
)

// Table is a namespaced section of the DB. A Table can have indexes, such that
// data inserted into the Table generates index entries for use in lookup and
// ordered iteration. It mirrors lexi.Table.
type Table struct {
	db      *DB
	name    []byte
	indexes []*Index
}

// Table constructs (or opens) a Table in the DB.
func (db *DB) Table(name string) (*Table, error) {
	if name == "" {
		return nil, errors.New("glossi: empty table name")
	}
	t := &Table{db: db, name: []byte(name)}
	err := db.Update(func(tx *bbolt.Tx) error {
		tb, err := tx.CreateBucketIfNotExists(t.name)
		if err != nil {
			return err
		}
		for _, sub := range [][]byte{dataBucketKey, indexBucketKey, metaBucketKey} {
			if _, err := tb.CreateBucketIfNotExists(sub); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

type setOpts struct {
	replace bool
}

// SetOption is a knob controlling how items are inserted with Set.
type SetOption func(*setOpts)

// WithReplace allows replacing a pre-existing value when calling Set.
func WithReplace() SetOption {
	return func(o *setOpts) { o.replace = true }
}

// Set inserts a value for the key and creates index entries. Zero-length keys
// are not allowed.
func (t *Table) Set(k, v []byte, opts ...SetOption) error {
	if len(k) == 0 {
		return errors.New("glossi: no zero-length keys allowed")
	}
	var o setOpts
	for _, opt := range opts {
		opt(&o)
	}
	return t.db.Update(func(tx *bbolt.Tx) error {
		tb := tx.Bucket(t.name)
		data := tb.Bucket(dataBucketKey)
		index := tb.Bucket(indexBucketKey)

		// If an entry exists, either reject or purge its old index entries.
		if old := data.Get(k); old != nil {
			if !o.replace {
				return errors.New("glossi: attempted to replace an entry without WithReplace")
			}
			_, oldIdxKeys, err := decodeDatum(old)
			if err != nil {
				return err
			}
			for _, ik := range oldIdxKeys {
				if err := index.Delete(ik); err != nil {
					return fmt.Errorf("error deleting replaced index entry: %w", err)
				}
			}
		}

		idxKeys := make([][]byte, 0, len(t.indexes))
		for _, idx := range t.indexes {
			ik, err := idx.entryKey(k, v)
			if err != nil {
				if errors.Is(err, ErrNotIndexed) {
					continue
				}
				return fmt.Errorf("index %q error: %w", idx.name, err)
			}
			if idx.unique {
				if err := idx.checkUnique(index, ik, k); err != nil {
					return err
				}
			}
			if err := index.Put(ik, k); err != nil {
				return fmt.Errorf("error writing index entry: %w", err)
			}
			idxKeys = append(idxKeys, ik)
		}
		return data.Put(k, encodeDatum(v, idxKeys))
	})
}

// Get retrieves a value from the Table. It returns ErrKeyNotFound if the key is
// not present.
func (t *Table) Get(k []byte) (v []byte, err error) {
	err = t.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(t.name).Bucket(dataBucketKey).Get(k)
		if raw == nil {
			return ErrKeyNotFound
		}
		val, _, err := decodeDatum(raw)
		if err != nil {
			return err
		}
		// Copy out of the transaction's memory.
		v = append([]byte(nil), val...)
		return nil
	})
	return
}

// Delete removes the value for the key along with all of its index entries.
// It returns ErrKeyNotFound if the key is not present.
func (t *Table) Delete(k []byte) error {
	return t.db.Update(func(tx *bbolt.Tx) error {
		tb := tx.Bucket(t.name)
		data := tb.Bucket(dataBucketKey)
		index := tb.Bucket(indexBucketKey)
		raw := data.Get(k)
		if raw == nil {
			return ErrKeyNotFound
		}
		_, idxKeys, err := decodeDatum(raw)
		if err != nil {
			return err
		}
		for _, ik := range idxKeys {
			if err := index.Delete(ik); err != nil {
				return fmt.Errorf("error deleting index entry: %w", err)
			}
		}
		return data.Delete(k)
	})
}

// Iterate iterates the table's primary keys. If prefix is non-empty, only keys
// with that prefix are visited. The key and value exposed via the Iter are only
// valid for the duration of the callback.
func (t *Table) Iterate(prefix []byte, f func(*Iter) error, opts ...IterationOption) error {
	var o iterOpts
	for _, opt := range opts {
		opt(&o)
	}
	run := t.db.View
	if o.update {
		run = t.db.Update
	}
	return run(func(tx *bbolt.Tx) error {
		tb := tx.Bucket(t.name)
		data := tb.Bucket(dataBucketKey)
		index := tb.Bucket(indexBucketKey)
		c := data.Cursor()
		return walk(c, prefix, o.seek, o.reverse, func(key, val []byte) error {
			value, _, err := decodeDatum(val)
			if err != nil {
				return err
			}
			return f(&Iter{k: key, v: value, index: index, dataBkt: data})
		})
	})
}
