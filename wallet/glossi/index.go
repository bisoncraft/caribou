// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package glossi

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/bisoncraft/bosswallet/util"
	"go.etcd.io/bbolt"
)

const (
	// ErrEndIteration can be returned from an Iterate callback to end iteration
	// without returning an error.
	ErrEndIteration = util.ErrorKind("end iteration")
	// ErrNotIndexed can be returned from an index function to indicate that a
	// datum should not be added to the index.
	ErrNotIndexed = util.ErrorKind("not indexed")
)

// Index is a lexicographically-ordered secondary index on a Table. Each datum
// inserted into the Table may generate an entry in the Index, enabling ordered
// iteration and lookup. It mirrors lexi.Index.
//
// Index entries are stored in the table's index bucket as:
//
//	[indexID:1][indexEntry][primaryKey] -> primaryKey
//
// indexID is a 1-byte id assigned (and persisted) per index name. Unique
// indexes must emit fixed-length index entries so that uniqueness can be
// checked by prefix.
type Index struct {
	table  *Table
	name   string
	id     byte
	f      func(k, v []byte) ([]byte, error)
	unique bool
}

// AddIndex adds a non-unique index to the Table. After an index is added, every
// item added with Set generates a corresponding entry.
func (t *Table) AddIndex(name string, f func(k, v []byte) ([]byte, error)) (*Index, error) {
	return t.addIndex(name, f, false)
}

// AddUniqueIndex adds a unique index to the Table. Set will return
// ErrUniqueConflict if an insert would create a duplicate index value, unless
// WithReplace is specified.
func (t *Table) AddUniqueIndex(name string, f func(k, v []byte) ([]byte, error)) (*Index, error) {
	return t.addIndex(name, f, true)
}

func (t *Table) addIndex(name string, f func(k, v []byte) ([]byte, error), unique bool) (*Index, error) {
	if name == "" {
		return nil, errors.New("glossi: empty index name")
	}
	idx := &Index{table: t, name: name, f: f, unique: unique}
	err := t.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(t.name).Bucket(metaBucketKey)
		if existing := meta.Get([]byte(name)); existing != nil {
			idx.id = existing[0]
			return nil
		}
		// Assign the next id from the sequence.
		var seq byte
		if s := meta.Get(indexSeqKey); s != nil {
			seq = s[0]
		}
		if seq == 0xff {
			return errors.New("glossi: too many indexes on table")
		}
		idx.id = seq
		if err := meta.Put([]byte(name), []byte{idx.id}); err != nil {
			return err
		}
		return meta.Put(indexSeqKey, []byte{seq + 1})
	})
	if err != nil {
		return nil, err
	}
	t.indexes = append(t.indexes, idx)
	return idx, nil
}

// entryKey returns the full index-bucket key (indexID||indexEntry||primaryKey)
// for the datum (k, v). It returns ErrNotIndexed (from f) if the datum should
// not be indexed.
func (idx *Index) entryKey(k, v []byte) ([]byte, error) {
	idxB, err := idx.f(k, v)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 1+len(idxB)+len(k))
	key[0] = idx.id
	copy(key[1:], idxB)
	copy(key[1+len(idxB):], k)
	return key, nil
}

// checkUnique returns ErrUniqueConflict if an entry with the same index value
// (indexID||indexEntry prefix) but a different primary key already exists.
func (idx *Index) checkUnique(index *bbolt.Bucket, fullKey, k []byte) error {
	prefix := fullKey[:len(fullKey)-len(k)] // indexID||indexEntry
	c := index.Cursor()
	for ek, ev := c.Seek(prefix); ek != nil && bytes.HasPrefix(ek, prefix); ek, ev = c.Next() {
		if !bytes.Equal(ev, k) {
			return fmt.Errorf("%w on %q", ErrUniqueConflict, idx.name)
		}
	}
	return nil
}

type iterOpts struct {
	reverse bool
	update  bool
	seek    []byte
}

// IterationOption is a knob controlling how Iterate runs.
type IterationOption func(*iterOpts)

// WithReverse sets the direction of iteration to reverse-lexicographical.
func WithReverse() IterationOption {
	return func(o *iterOpts) { o.reverse = true }
}

// WithUpdate must be used if the callback intends to modify the database during
// iteration, e.g. via Iter.Delete.
func WithUpdate() IterationOption {
	return func(o *iterOpts) { o.update = true }
}

// WithSeek starts iteration at the specified index-space prefix.
func WithSeek(prefix []byte) IterationOption {
	return func(o *iterOpts) { o.seek = prefix }
}

// Iterate iterates the index in entry order. prefix, if non-empty, constrains
// iteration to index entries beginning with it (in index space, i.e. without
// the internal index id). The key and value exposed via the Iter are only valid
// for the duration of the callback.
func (idx *Index) Iterate(prefix []byte, f func(*Iter) error, opts ...IterationOption) error {
	var o iterOpts
	for _, opt := range opts {
		opt(&o)
	}
	bound := append([]byte{idx.id}, prefix...)
	var seek []byte
	if len(o.seek) > 0 {
		seek = append([]byte{idx.id}, o.seek...)
	}
	run := idx.table.db.View
	if o.update {
		run = idx.table.db.Update
	}
	return run(func(tx *bbolt.Tx) error {
		tb := tx.Bucket(idx.table.name)
		index := tb.Bucket(indexBucketKey)
		data := tb.Bucket(dataBucketKey)
		c := index.Cursor()
		return walk(c, bound, seek, o.reverse, func(entryKey, k []byte) error {
			raw := data.Get(k)
			if raw == nil {
				return fmt.Errorf("dangling index entry for index %q", idx.name)
			}
			value, _, err := decodeDatum(raw)
			if err != nil {
				return err
			}
			return f(&Iter{
				k:       k,
				v:       value,
				entry:   entryKey,
				kLen:    len(k),
				index:   index,
				dataBkt: data,
			})
		})
	})
}

// Iter is an entry yielded during iteration of a Table or Index.
type Iter struct {
	k     []byte
	v     []byte
	entry []byte // index entry key (index iteration only); nil for table iteration
	kLen  int

	// For Delete during iteration (requires WithUpdate).
	index   *bbolt.Bucket
	dataBkt *bbolt.Bucket
}

// K returns the primary key for the entry. Valid only for the duration of the
// iteration callback.
func (i *Iter) K() ([]byte, error) { return i.k, nil }

// V provides access to the value bytes. The bytes are only valid for the
// duration of f.
func (i *Iter) V(f func(v []byte) error) error { return f(i.v) }

// Entry provides access to the index entry (without the internal index id or the
// trailing primary key) when iterating an Index. When iterating a Table it
// yields the primary key. The bytes are only valid for the duration of f.
func (i *Iter) Entry(f func(idxB []byte) error) error {
	if i.entry == nil {
		return f(i.k)
	}
	return f(i.entry[1 : len(i.entry)-i.kLen])
}

// Delete removes the datum at the current entry along with all of its index
// entries. The iteration must have been started with WithUpdate.
func (i *Iter) Delete() error {
	if i.index == nil || i.dataBkt == nil {
		return errors.New("glossi: Delete requires WithUpdate")
	}
	raw := i.dataBkt.Get(i.k)
	if raw == nil {
		return ErrKeyNotFound
	}
	_, idxKeys, err := decodeDatum(raw)
	if err != nil {
		return err
	}
	for _, ik := range idxKeys {
		if err := i.index.Delete(ik); err != nil {
			return err
		}
	}
	return i.dataBkt.Delete(i.k)
}

// walk iterates a bbolt cursor. bound constrains iteration to keys with that
// prefix. seek, if non-empty, positions the starting point. For reverse
// iteration the walk proceeds from the greatest matching key downward.
func walk(c *bbolt.Cursor, bound, seek []byte, reverse bool, f func(key, val []byte) error) error {
	if reverse {
		return reverseWalk(c, bound, seek, f)
	}
	return forwardWalk(c, bound, seek, f)
}

func forwardWalk(c *bbolt.Cursor, bound, seek []byte, f func(key, val []byte) error) error {
	var k, v []byte
	switch {
	case len(seek) > 0:
		k, v = c.Seek(seek)
	case len(bound) > 0:
		k, v = c.Seek(bound)
	default:
		k, v = c.First()
	}
	for ; k != nil; k, v = c.Next() {
		if len(bound) > 0 && !bytes.HasPrefix(k, bound) {
			break
		}
		if err := f(k, v); err != nil {
			if errors.Is(err, ErrEndIteration) {
				return nil
			}
			return err
		}
	}
	return nil
}

func reverseWalk(c *bbolt.Cursor, bound, seek []byte, f func(key, val []byte) error) error {
	start := bound
	if len(seek) > 0 {
		start = seek
	}
	var k, v []byte
	if ub := prefixUpperBound(start); ub == nil {
		k, v = c.Last()
	} else if k, _ = c.Seek(ub); k == nil {
		// No key >= ub: the greatest key in the DB is our starting point.
		k, v = c.Last()
	} else {
		// Seek landed on the first key >= ub; step back to the last key < ub.
		k, v = c.Prev()
	}
	for ; k != nil; k, v = c.Prev() {
		if len(bound) > 0 && !bytes.HasPrefix(k, bound) {
			break
		}
		if err := f(k, v); err != nil {
			if errors.Is(err, ErrEndIteration) {
				return nil
			}
			return err
		}
	}
	return nil
}

// prefixUpperBound returns the smallest byte slice that is strictly greater than
// every byte slice having p as a prefix. It returns nil if p consists entirely
// of 0xff bytes (no upper bound exists).
func prefixUpperBound(p []byte) []byte {
	ub := append([]byte(nil), p...)
	for i := len(ub) - 1; i >= 0; i-- {
		if ub[i] != 0xff {
			ub[i]++
			return ub[:i+1]
		}
	}
	return nil
}
