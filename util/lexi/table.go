// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"errors"
	"fmt"

	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/dgraph-io/badger/v4"
)

// Table is a prefixed section of the k-v DB. A Table can have indexes, such
// that data inserted into the Table will generates index entries for use in
// lookup and iteration. Table implements kvstore.StoreTable.
type Table struct {
	*DB
	name                    string
	prefix                  keyPrefix
	indexes                 []*Index
	defaultSetOptions       kvstore.SetOpts
	defaultIterationOptions kvstore.IterationOpts
}

// Table constructs a new table in the DB.
func (db *DB) Table(name string) (kvstore.StoreTable, error) {
	t, err := db.table(name)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// table is Table returning the concrete type, for package-internal use.
func (db *DB) table(name string) (*Table, error) {
	p, err := db.prefixForName(name)
	if err != nil {
		return nil, err
	}
	return &Table{
		DB:     db,
		name:   name,
		prefix: p,
	}, nil
}

// WithGetTxn specifies a Badger transaction to run Get inside.
func WithGetTxn(txn *badger.Txn) kvstore.GetOption {
	return func(o *kvstore.GetOpts) { o.Txn = txn }
}

// WithTxn specifies a Badger transaction to run Set inside. SetBatch always
// runs in its own transaction and ignores this option.
func WithTxn(txn *badger.Txn) kvstore.SetOption {
	return func(o *kvstore.SetOpts) { o.Txn = txn }
}

// resolveTxn asserts an option-supplied transaction handle. A non-nil handle
// of any other engine's transaction type is a caller error, not a nil.
func resolveTxn(i any) (*badger.Txn, error) {
	if i == nil {
		return nil, nil
	}
	txn, ok := i.(*badger.Txn)
	if !ok {
		return nil, fmt.Errorf("lexi: transaction option holds %T, not *badger.Txn", i)
	}
	return txn, nil
}

// Get retrieves a value from the Table as raw bytes.
func (t *Table) Get(k KV, options ...kvstore.GetOption) (b []byte, err error) {
	kB, err := kvstore.ParseKV(k)
	if err != nil {
		return nil, fmt.Errorf("error marshaling key: %w", err)
	}

	var opts kvstore.GetOpts
	for i := range options {
		options[i](&opts)
	}
	txn, err := resolveTxn(opts.Txn)
	if err != nil {
		return nil, err
	}

	getTxnFunc := func(txn *badger.Txn) error {
		dbID, err := t.keyID(txn, t.prefix, kB, true)
		if err != nil {
			return convertError(err)
		}
		d, err := t.get(txn, dbID)
		if err != nil {
			return err
		}
		b = d.v
		return nil
	}

	if txn != nil {
		err = getTxnFunc(txn)
	} else {
		err = t.View(getTxnFunc)
	}
	return
}

func (t *Table) get(txn *badger.Txn, dbID DBID) (d *datum, err error) {
	item, err := txn.Get(PrefixedKey(t.prefix, dbID[:]))
	if err != nil {
		return nil, convertError(err)
	}
	err = item.Value(func(dB []byte) error {
		d, err = decodeDatum(dB)
		if err != nil {
			return fmt.Errorf("error decoding datum: %w", err)
		}
		return nil
	})
	return
}

// UseDefaultSetOptions sets default options for Set and SetBatch.
func (t *Table) UseDefaultSetOptions(setOpts ...kvstore.SetOption) {
	for i := range setOpts {
		setOpts[i](&t.defaultSetOptions)
	}
}

// Set inserts a new value for the key, and creates index entries.
func (t *Table) Set(k, v KV, setOpts ...kvstore.SetOption) error {
	opts := t.defaultSetOptions
	for i := range setOpts {
		setOpts[i](&opts)
	}
	return t.set(k, v, opts)
}

// set performs one Set with resolved options. Set and SetBatch both funnel
// through it.
func (t *Table) set(k, v KV, opts kvstore.SetOpts) error {
	kB, err := kvstore.ParseKV(k)
	if err != nil {
		return fmt.Errorf("error marshaling key: %w", err)
	}
	// zero length keys are not allowed because it screws up the reverse
	// iteration scheme.
	if len(kB) == 0 {
		return errors.New("no zero-length keys allowed")
	}
	vB, err := kvstore.ParseKV(v)
	if err != nil {
		return fmt.Errorf("error marshaling value: %w", err)
	}
	txn, err := resolveTxn(opts.Txn)
	if err != nil {
		return err
	}
	updateFunc := func(txn *badger.Txn) error {
		// Built per attempt: Update retries this closure on badger.ErrConflict,
		// and a datum hoisted out of it would accumulate one copy of every
		// index entry per attempt, persisting a datum whose index list grows
		// with the retry count.
		d := &datum{v: vB, indexes: make([][]byte, 0, len(t.indexes))}

		dbID, err := t.keyID(txn, t.prefix, kB, false)
		if err != nil {
			return convertError(err)
		}
		// See if an entry already exists
		oldDatum, err := t.get(txn, dbID)
		if !errors.Is(err, ErrKeyNotFound) {
			if err != nil {
				return fmt.Errorf("error looking for existing entry: %w", err)
			}
			// We found an old entry
			if !opts.Replace {
				return errors.New("attempted to replace an entry without specifying WithReplace")
			}
			// Delete any old indexes
			for _, k := range oldDatum.indexes {
				if err := txn.Delete(k); err != nil {
					return fmt.Errorf("error deleting replaced datum's index entry; %w", err)
				}
			}
		}

		// Add to indexes
		for _, idx := range t.indexes {
			var indexEntry []byte
			if indexEntry, err = idx.add(txn, k, v, dbID); err != nil {
				// Handle unique index conflicts
				var indexConflictError uniqueIndexConflictError
				switch {
				case errors.As(err, &indexConflictError):
					// If the index is unique and we're not replacing, return an error
					if !opts.Replace {
						return fmt.Errorf("index uniqueness violation on %q", indexConflictError.indexName)
					}

					// If we're replacing, delete the old entry
					if err := t.removeTableEntry(txn, indexConflictError.conflictDBID); err != nil {
						return fmt.Errorf("error deleting conflicting entry from index %q: %w", indexConflictError.indexName, err)
					}

					// Try again
					indexEntry, err = idx.add(txn, k, v, dbID)
					if err != nil {
						return fmt.Errorf("error adding entry to index after deleting conflicting entry: %w", err)
					}
				default:
					return fmt.Errorf("error adding entry to index: %w", err)
				}
			}

			if indexEntry != nil {
				d.indexes = append(d.indexes, indexEntry)
			}
		}

		dB, err := d.bytes()
		if err != nil {
			return fmt.Errorf("error encoding datum: %w", err)
		}

		return txn.Set(PrefixedKey(t.prefix, dbID[:]), dB)
	}

	if txn != nil {
		return updateFunc(txn)
	}
	return t.Update(updateFunc)
}

// SetBatch inserts every pair within a single Badger transaction. Badger
// already absorbs bulk writes through its LSM tree, so unlike glossi/bbolt this
// is primarily for write atomicity and API parity. If any Set fails (including
// a unique-index conflict without WithReplace), the whole batch is rolled back.
// The setOpts apply to every pair; a caller-supplied WithTxn is overridden by
// the batch's own transaction.
func (t *Table) SetBatch(pairs []kvstore.BatchEntry, setOpts ...kvstore.SetOption) error {
	if len(pairs) == 0 {
		return nil
	}
	opts := t.defaultSetOptions
	for i := range setOpts {
		setOpts[i](&opts)
	}
	return t.Update(func(txn *badger.Txn) error {
		opts.Txn = txn
		for _, p := range pairs {
			if err := t.set(p.K, p.V, opts); err != nil {
				return err
			}
		}
		return nil
	})
}

// Delete deletes the data associated with the key, including any index entries
// and the id<->key mappings.
func (t *Table) Delete(k KV) error {
	kB, err := kvstore.ParseKV(k)
	if err != nil {
		return fmt.Errorf("error marshaling key: %w", err)
	}
	return t.Update(func(txn *badger.Txn) error {
		dbID, err := t.keyID(txn, t.prefix, kB, true)
		if err != nil {
			return convertError(err)
		}
		return t.removeTableEntry(txn, dbID)
	})
}

func (t *Table) removeTableEntry(txn *badger.Txn, dbID DBID) error {
	item, err := txn.Get(PrefixedKey(t.prefix, dbID[:]))
	if err != nil {
		return convertError(err)
	}
	return item.Value(func(dB []byte) error {
		d, err := decodeDatum(dB)
		if err != nil {
			return fmt.Errorf("error decoding datum: %w", err)
		}
		return t.deleteDatum(txn, dbID, d)
	})
}

func (t *Table) deleteDatum(txn *badger.Txn, dbID DBID, d *datum) error {
	for _, k := range d.indexes {
		if err := txn.Delete(k); err != nil {
			return fmt.Errorf("error deleting index entry; %w", err)
		}
	}
	if err := txn.Delete(PrefixedKey(t.prefix, dbID[:])); err != nil {
		return fmt.Errorf("error deleting table entry: %w", err)
	}
	return t.deleteDBID(txn, dbID)
}

// UseDefaultIterationOptions sets default options for Iterate.
func (t *Table) UseDefaultIterationOptions(optss ...kvstore.IterationOption) {
	for i := range optss {
		optss[i](&t.defaultIterationOptions)
	}
}

// Iterate iterates the table's primary keys in key order. If prefix is
// non-empty, only keys beginning with it are visited.
//
// Iteration walks the table's key→DBID map rather than the table's own rows.
// The rows are stored under the DBID, so iterating them directly would order by
// DBID (i.e. insertion order) and match prefix and seek against DBID bytes
// instead of the caller's keys. The key map is ordered by user key, so walking
// it is what makes prefix, seek and reverse mean what the caller expects. The
// DBID comes out of the entry's value, costing no more than the lookup Iter.K
// would otherwise do.
func (t *Table) Iterate(prefixI KV, f func(kvstore.StoreIter) error, iterOpts ...kvstore.IterationOption) error {
	return t.iterate(tableKeyMapPrefix(t.prefix), t, t.defaultIterationOptions, false, prefixI, f, iterOpts...)
}
