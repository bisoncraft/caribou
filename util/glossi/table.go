// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package glossi

import (
	"errors"
	"fmt"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
	"go.etcd.io/bbolt"
)

// KV is any one of a number of common types whose binary encoding is
// straight-forward. It is an alias of kvstore.KV (as is lexi.KV); see
// kvstore.ParseKV for the supported types.
type KV = kvstore.KV

// ErrUniqueConflict is returned by Set when an insert would violate a unique
// index constraint and WithReplace was not specified.
const ErrUniqueConflict = util.ErrorKind("unique index conflict")

var (
	// Sub-buckets within a Table's bucket. Splitting the name→id map and the
	// id sequence into separate buckets makes it structurally impossible for
	// an index name to collide with the sequence key.
	dataBucketKey  = []byte("d") // primary key -> datum
	indexBucketKey = []byte("i") // indexID||indexEntry||key -> key
	metaBucketKey  = []byte("m") // index name -> indexID
	seqBucketKey   = []byte("s") // single entry: the next index id

	// seqKey is the only key in the seq bucket; the bucket-as-namespace makes
	// this key arbitrary.
	seqKey = []byte("seq")
)

// Table is a namespaced section of the DB. A Table can have indexes, such that
// data inserted into the Table generates index entries for use in lookup and
// ordered iteration. It mirrors lexi.Table and implements kvstore.StoreTable.
type Table struct {
	db      *DB
	name    []byte
	indexes []*Index

	defaultSetOptions       kvstore.SetOpts
	defaultIterationOptions kvstore.IterationOpts
}

// Table constructs (or opens) a Table in the DB.
func (db *DB) Table(name string) (kvstore.StoreTable, error) {
	t, err := db.table(name)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// table is Table returning the concrete type, for package-internal use.
func (db *DB) table(name string) (*Table, error) {
	if name == "" {
		return nil, errors.New("glossi: empty table name")
	}
	t := &Table{db: db, name: []byte(name)}
	err := db.Update(func(tx *bbolt.Tx) error {
		tb, err := tx.CreateBucketIfNotExists(t.name)
		if err != nil {
			return err
		}
		for _, sub := range [][]byte{dataBucketKey, indexBucketKey, metaBucketKey, seqBucketKey} {
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

// WithTxn specifies a bbolt read-write transaction to run Set inside. SetBatch
// always runs in its own transaction and ignores this option.
func WithTxn(tx *bbolt.Tx) kvstore.SetOption {
	return func(o *kvstore.SetOpts) { o.Txn = tx }
}

// WithGetTxn specifies a bbolt transaction to run Get inside.
func WithGetTxn(tx *bbolt.Tx) kvstore.GetOption {
	return func(o *kvstore.GetOpts) { o.Txn = tx }
}

// resolveTxn asserts an option-supplied transaction handle. A non-nil handle
// of any other engine's transaction type is a caller error, not a nil.
func resolveTxn(i any) (*bbolt.Tx, error) {
	if i == nil {
		return nil, nil
	}
	tx, ok := i.(*bbolt.Tx)
	if !ok {
		return nil, fmt.Errorf("glossi: transaction option holds %T, not *bbolt.Tx", i)
	}
	return tx, nil
}

// UseDefaultSetOptions sets default options for Set and SetBatch.
func (t *Table) UseDefaultSetOptions(opts ...kvstore.SetOption) {
	for _, opt := range opts {
		opt(&t.defaultSetOptions)
	}
}

// UseDefaultIterationOptions sets default options for Iterate.
func (t *Table) UseDefaultIterationOptions(opts ...kvstore.IterationOption) {
	for _, opt := range opts {
		opt(&t.defaultIterationOptions)
	}
}

// Set inserts a value for the key and creates index entries. Zero-length keys
// are not allowed.
func (t *Table) Set(k, v KV, opts ...kvstore.SetOption) error {
	kB, err := kvstore.ParseKV(k)
	if err != nil {
		return fmt.Errorf("glossi: error marshaling key: %w", err)
	}
	if len(kB) == 0 {
		return errors.New("glossi: no zero-length keys allowed")
	}
	vB, err := kvstore.ParseKV(v)
	if err != nil {
		return fmt.Errorf("glossi: error marshaling value: %w", err)
	}
	o := t.defaultSetOptions
	for _, opt := range opts {
		opt(&o)
	}
	tx, err := resolveTxn(o.Txn)
	if err != nil {
		return err
	}
	if tx != nil {
		return t.setInTx(tx, k, v, kB, vB, o.Replace)
	}
	return t.db.Update(func(tx *bbolt.Tx) error {
		return t.setInTx(tx, k, v, kB, vB, o.Replace)
	})
}

// SetBatch inserts every entry in a single transaction, collapsing what would
// otherwise be one bbolt.Update (and one fsync) per Set into one. This is the
// path bulk inserts should use on mobile - an initial history sync or a
// migration that stores many transactions at once - where per-call fsync is
// noticeably slow. Without WithReplace, any existing key rolls back the whole
// batch. The batch always runs in its own transaction; a caller-supplied
// WithTxn is ignored, matching lexi.
func (t *Table) SetBatch(entries []kvstore.BatchEntry, opts ...kvstore.SetOption) error {
	if len(entries) == 0 {
		return nil
	}
	type kvBytes struct{ kB, vB []byte }
	parsed := make([]kvBytes, len(entries))
	for i, e := range entries {
		kB, err := kvstore.ParseKV(e.K)
		if err != nil {
			return fmt.Errorf("glossi: error marshaling key: %w", err)
		}
		if len(kB) == 0 {
			return errors.New("glossi: no zero-length keys allowed")
		}
		vB, err := kvstore.ParseKV(e.V)
		if err != nil {
			return fmt.Errorf("glossi: error marshaling value: %w", err)
		}
		parsed[i] = kvBytes{kB: kB, vB: vB}
	}
	o := t.defaultSetOptions
	for _, opt := range opts {
		opt(&o)
	}
	return t.db.Update(func(tx *bbolt.Tx) error {
		for i, e := range entries {
			if err := t.setInTx(tx, e.K, e.V, parsed[i].kB, parsed[i].vB, o.Replace); err != nil {
				return err
			}
		}
		return nil
	})
}

// setInTx performs one Set within an existing transaction. Set and SetBatch
// both funnel through it so the index bookkeeping lives in a single place. k
// and v are the caller's original KVs, passed through to index functions
// (mirroring lexi, whose index functions also see the unparsed values); kB and
// vB are their parsed encodings, which is what gets stored.
func (t *Table) setInTx(tx *bbolt.Tx, k, v KV, kB, vB []byte, replace bool) error {
	tb := tx.Bucket(t.name)
	data := tb.Bucket(dataBucketKey)
	index := tb.Bucket(indexBucketKey)

	// If an entry exists, either reject or purge its old index entries.
	if old := data.Get(kB); old != nil {
		if !replace {
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
		ik, err := idx.entryKey(k, v, kB)
		if err != nil {
			if errors.Is(err, ErrNotIndexed) {
				continue
			}
			return fmt.Errorf("index %q error: %w", idx.name, err)
		}
		if idx.unique {
			// On a unique-index conflict, reject unless replacing. When
			// replacing, delete the conflicting entry (a different primary key
			// sharing this index value), matching lexi: this is how callers
			// model replacement keyed by an index value rather than the primary
			// key (e.g. an Ethereum nonce-replacement transaction).
			if conflict := idx.uniqueConflict(index, ik, kB); conflict != nil {
				if !replace {
					return fmt.Errorf("%w on %q", ErrUniqueConflict, idx.name)
				}
				if err := t.deleteInTx(tb, conflict); err != nil && !errors.Is(err, ErrKeyNotFound) {
					return fmt.Errorf("error deleting unique-conflict entry: %w", err)
				}
			}
		}
		if err := index.Put(ik, kB); err != nil {
			return fmt.Errorf("error writing index entry: %w", err)
		}
		idxKeys = append(idxKeys, ik)
	}
	return data.Put(kB, encodeDatum(vB, idxKeys))
}

// Get retrieves a value from the Table. It returns ErrKeyNotFound if the key is
// not present.
func (t *Table) Get(k KV, options ...kvstore.GetOption) (v []byte, err error) {
	kB, err := kvstore.ParseKV(k)
	if err != nil {
		return nil, fmt.Errorf("glossi: error marshaling key: %w", err)
	}
	var opts kvstore.GetOpts
	for _, opt := range options {
		opt(&opts)
	}
	tx, err := resolveTxn(opts.Txn)
	if err != nil {
		return nil, err
	}
	getTxFunc := func(tx *bbolt.Tx) error {
		raw := tx.Bucket(t.name).Bucket(dataBucketKey).Get(kB)
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
	}
	if tx != nil {
		err = getTxFunc(tx)
	} else {
		err = t.db.View(getTxFunc)
	}
	return
}

// Delete removes the value for the key along with all of its index entries.
// It returns ErrKeyNotFound if the key is not present.
func (t *Table) Delete(k KV) error {
	kB, err := kvstore.ParseKV(k)
	if err != nil {
		return fmt.Errorf("glossi: error marshaling key: %w", err)
	}
	return t.db.Update(func(tx *bbolt.Tx) error {
		return t.deleteInTx(tx.Bucket(t.name), kB)
	})
}

// deleteInTx removes an entry and its index entries within an existing
// transaction. Delete and the unique-conflict path in setInTx funnel through it.
func (t *Table) deleteInTx(tb *bbolt.Bucket, k []byte) error {
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
}

// Iterate iterates the table's primary keys. If prefix is non-empty, only keys
// with that prefix are visited. The key and value exposed via the Iter are only
// valid for the duration of the callback.
func (t *Table) Iterate(prefixI KV, f func(kvstore.StoreIter) error, opts ...kvstore.IterationOption) error {
	prefix, err := kvstore.ParseKV(prefixI)
	if err != nil {
		return fmt.Errorf("glossi: error marshaling prefix: %w", err)
	}
	o := t.defaultIterationOptions
	for _, opt := range opts {
		opt(&o)
	}
	run := t.db.View
	if o.Update {
		run = t.db.Update
	}
	return run(func(tx *bbolt.Tx) error {
		tb := tx.Bucket(t.name)
		data := tb.Bucket(dataBucketKey)
		index := tb.Bucket(indexBucketKey)
		c := data.Cursor()
		return walk(c, prefix, o.Seek, o.Reverse, func(key, val []byte) error {
			value, idxKeys, err := decodeDatum(val)
			if err != nil {
				return err
			}
			return f(&Iter{k: key, v: value, table: t, idxKeys: idxKeys, update: o.Update, index: index, dataBkt: data})
		})
	})
}
