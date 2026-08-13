package lexi

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/dgraph-io/badger/v4"
)

func newTestDB(t *testing.T) (*DB, func()) {
	tmpDir := t.TempDir()
	db, err := New(&Config{
		Path: filepath.Join(tmpDir, "test.db"),
		Log:  util.StdOutLogger("T", util.LevelInfo),
	})
	if err != nil {
		t.Fatalf("error constructing db: %v", err)
	}
	return db, func() {}
}

func TestPrefixes(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	pfix, err := db.prefixForName("1")
	if err != nil {
		t.Fatalf("error getting prefix 1: %v", err)
	}
	if pfix != firstAvailablePrefix {
		t.Fatalf("expected prefix %s, got %s", firstAvailablePrefix, pfix)
	}

	pfix, err = db.prefixForName("2")
	if err != nil {
		t.Fatalf("error getting prefix 2: %v", err)
	}
	if secondPfix := incrementPrefix(firstAvailablePrefix); pfix != secondPfix {
		t.Fatalf("expected prefix %s, got %s", secondPfix, pfix)
	}

	// Make sure requests for the same table name return the already-registered
	// prefix.
	pfix, err = db.prefixForName("1")
	if err != nil {
		t.Fatalf("error getting prefix 1 again: %v", err)
	}
	if pfix != firstAvailablePrefix {
		t.Fatalf("expected prefix %s, got %s", firstAvailablePrefix, pfix)
	}
}

type tValue struct {
	k, v, idx []byte
}

func (v *tValue) MarshalBinary() ([]byte, error) {
	return v.v, nil
}

func (v *tValue) UnmarshalBinary(data []byte) error {
	v.v = make([]byte, len(data))
	copy(v.v, data)
	return nil
}

func valueIndex(k, v KV) ([]byte, error) {
	return v.(*tValue).idx, nil
}

func valueKey(k, v KV) ([]byte, error) {
	return v.(*tValue).k, nil
}

func TestIndex(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	tbl, err := db.table("T")
	if err != nil {
		t.Fatalf("Error creating table: %v", err)
	}

	idx, err := tbl.AddIndex("I", valueIndex)
	if err != nil {
		t.Fatalf("Error adding index: %v", err)
	}

	keyIdx, err := tbl.AddIndex("K", valueKey)
	if err != nil {
		t.Fatalf("Error adding index: %v", err)
	}

	// Put 100 values in.
	const nVs = 100
	vs := make([]*tValue, nVs)
	for i := 0; i < nVs; i++ {
		// Random value, but with a flag at the end.
		k := append(encode.RandomBytes(5), byte(i))
		// The index is keyed on i, with a prefix of 0, until 40, after which
		// the prefix is 1.
		indexKey := []byte{byte(i)}
		prefix := []byte{0}
		if i >= 40 {
			prefix = []byte{1}
		}
		indexKey = append(prefix, indexKey...)
		v := &tValue{k: indexKey, v: encode.RandomBytes(10), idx: []byte{byte(i)}}
		vs[i] = v
		if err := tbl.Set(k, v); err != nil {
			t.Fatalf("Error setting table entry: %v", err)
		}
	}

	// Iterate forwards.
	var i int
	idx.Iterate(nil, func(it kvstore.StoreIter) error {
		v := vs[i]
		it.V(func(vB []byte) error {
			if !bytes.Equal(vB, v.v) {
				t.Fatalf("Wrong bytes for forward iteration index %d", i)
			}
			return nil
		})
		i++
		return nil
	})
	if i != nVs {
		t.Fatalf("Expected to iterate %d items but only did %d", nVs, i)
	}

	// Iterate backwards
	i = nVs
	idx.Iterate(nil, func(it kvstore.StoreIter) error {
		i--
		v := vs[i]
		return it.V(func(vB []byte) error {
			if !bytes.Equal(vB, v.v) {
				t.Fatalf("Wrong bytes for reverse iteration index %d", i)
			}
			return nil
		})
	}, kvstore.WithReverse())
	if i != 0 {
		t.Fatalf("Expected to iterate back to zero but only got to %d", i)
	}

	// Iterate forwards with prefix.
	keyIdx.Iterate([]byte{0}, func(it kvstore.StoreIter) error {
		v := vs[i]
		it.V(func(vB []byte) error {
			if !bytes.Equal(vB, v.v) {
				t.Fatalf("Wrong bytes for forward iteration index %d", i)
			}
			return nil
		})
		i++
		return nil
	})
	if i != 40 {
		t.Fatalf("Expected to iterate 40 items but only did %d", i)
	}

	// Iterate backwards with prefix.
	keyIdx.Iterate([]byte{0}, func(it kvstore.StoreIter) error {
		i--
		v := vs[i]
		return it.V(func(vB []byte) error {
			if !bytes.Equal(vB, v.v) {
				t.Fatalf("Wrong bytes for reverse iteration index %d", i)
			}
			return nil
		})
	}, kvstore.WithReverse())
	if i != 0 {
		t.Fatalf("Expected to iterate back to zero but only got to %d", i)
	}

	// Iterate forward and delete the first half.
	i = 0
	if err := idx.Iterate(nil, func(it kvstore.StoreIter) error {
		if i < 50 {
			i++
			return it.Delete()
		}
		return ErrEndIteration
	}, kvstore.WithUpdate()); err != nil {
		t.Fatalf("Error iterating forward to delete entries: %v", err)
	}
	if i != 50 {
		t.Fatalf("Expected to iterate forward to 50, but only got to %d", i)
	}

	idx.Iterate(nil, func(it kvstore.StoreIter) error {
		return it.V(func(vB []byte) error {
			if !bytes.Equal(vB, vs[50].v) {
				t.Fatal("Wrong first iteration item after deletion")
			}
			return ErrEndIteration
		})
	})

	// Seek a specific item.
	i = 75
	idx.Iterate(nil, func(it kvstore.StoreIter) error {
		if i == 75 {
			i--
			return it.V(func(vB []byte) error {
				if !bytes.Equal(vB, vs[75].v) {
					t.Fatal("first item wasn't 25")
				}
				return nil
			})
		} else if i == 74 {
			return ErrEndIteration
		}
		t.Fatal("reached an unexpected value")
		return nil
	}, kvstore.WithSeek(vs[75].idx), kvstore.WithReverse())
	if i != 74 {
		t.Fatal("never reached 74")
	}

	// Make sure we can iterate the table directly
	i = 0
	if err := tbl.Iterate(nil, func(it kvstore.StoreIter) error {
		i++
		return nil
	}); err != nil {
		t.Fatalf("Error iterating table: %v", err)
	}
	if i != 50 {
		t.Fatal("table didn't have 50")
	}
}

func TestDatum(t *testing.T) {
	testEncodeDecode := func(tag string, d *datum) {
		t.Helper()
		b, err := d.bytes()
		if err != nil {
			t.Fatalf("%s: error encoding simple datum: %v", tag, err)
		}
		reD, err := decodeDatum(b)
		if err != nil {
			t.Fatalf("%s: error decoding simple datum: %v", tag, err)
		}
		if !bytes.Equal(reD.v, d.v) {
			t.Fatalf("%s: decoding datum value incorrect. %x != %x", tag, reD.v, d.v)
		}
		if d.version != 0 {
			t.Fatalf("%s: wrong datum version. expected %d, got %d", tag, d.version, reD.version)
		}
		if reD.keyID != d.keyID {
			t.Fatalf("%s: wrong datum KeyID. expected %s, got %s", tag, d.keyID, reD.keyID)
		}
		if len(d.indexes) != len(reD.indexes) {
			t.Fatalf("%s: wrong number of indexes. wanted %d, got %d", tag, len(d.indexes), reD.indexes)
		}
		for i, idx := range d.indexes {
			if !bytes.Equal(idx, reD.indexes[i]) {
				t.Fatalf("%s: Wrong index # %d", tag, i)
			}
		}
	}

	d := &datum{version: 1, v: []byte{0x01}}
	if _, err := d.bytes(); err == nil || !strings.Contains(err.Error(), "unknown datum version") {
		t.Fatalf("Wrong error for unknown datum version: %v", err)
	}
	d.version = 0
	d.keyID = KeyID{1, 2, 3, 4, 5, 6, 7, 8}

	testEncodeDecode("simple", d)

	// An empty value with no indexes is the minimum legal datum, 11 bytes:
	// a version byte, a KeyID, and two zero varints.
	testEncodeDecode("empty", &datum{})
	testEncodeDecode("empty value with index", &datum{indexes: [][]byte{encode.RandomBytes(5)}})

	d = &datum{keyID: KeyID{0xff}, v: encode.RandomBytes(300)}
	d.indexes = append(d.indexes, encode.RandomBytes(5))
	d.indexes = append(d.indexes, encode.RandomBytes(300))
	testEncodeDecode("complex", d)

	// A truncated blob is an error, not a short read.
	full, err := d.bytes()
	if err != nil {
		t.Fatalf("error encoding datum for truncation check: %v", err)
	}
	if _, err := decodeDatum(full[:len(full)-1]); err == nil {
		t.Fatalf("no error decoding truncated datum")
	}
}

// TestDecodeDatumOversizedLengths asserts that a datum whose length prefixes
// exceed the bytes actually present is rejected as an error.
//
// The lengths are read as varints straight off disk and then used to size
// allocations, so an unbounded one is an out-of-range makeslice panic rather
// than a decode error. Every caller of decodeDatum - Table.get, Iter.datum,
// removeTableEntry - handles a decode error and none survives a panic, so the
// difference is a returned error versus a downed process on a corrupt record.
func TestDecodeDatumOversizedLengths(t *testing.T) {
	// maxVarInt is 0xff followed by eight length bytes, i.e. the largest value
	// wire.ReadVarInt will hand back.
	maxVarInt := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	// The framed payload follows the version byte and the KeyID.
	frame := func(payload ...byte) []byte {
		return append(append([]byte{0}, make([]byte, KeyIDSize)...), payload...)
	}

	for _, tt := range []struct {
		name string
		blob []byte
	}{{
		// index count = 2^64-1, nothing following it.
		name: "index count",
		blob: append(frame(), maxVarInt...),
	}, {
		// one index, whose length is 2^64-1.
		name: "index length",
		blob: append(frame(0x01), maxVarInt...),
	}, {
		// no indexes, value length 2^64-1.
		name: "value length",
		blob: append(frame(0x00), maxVarInt...),
	}} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeDatum(tt.blob); err == nil {
				t.Fatalf("no error decoding datum with oversized %s", tt.name)
			}
		})
	}
}

// TestEmptyValue ensures a zero-length value survives the full table
// round-trip: Set, Get, and iteration.
func TestEmptyValue(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	tbl, err := db.table("empties")
	if err != nil {
		t.Fatalf("Table error: %v", err)
	}
	k := []byte("k")
	if err := tbl.Set(k, []byte{}, kvstore.WithReplace()); err != nil {
		t.Fatalf("Set error: %v", err)
	}
	b, err := tbl.Get(k)
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("expected empty value, got %d bytes", len(b))
	}
	var n int
	err = tbl.Iterate(nil, func(it kvstore.StoreIter) error {
		n++
		return it.V(func(v []byte) error {
			if len(v) != 0 {
				return fmt.Errorf("expected empty value in iteration, got %d bytes", len(v))
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("Iterate error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 iteration, got %d", n)
	}
}

func TestUniqueIndex(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	tbl, err := db.table("UniqueTest")
	if err != nil {
		t.Fatalf("Error creating table: %v", err)
	}

	uniqueIdx, err := tbl.AddUniqueIndex("Unique", func(k, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err != nil {
		t.Fatalf("Error adding unique index: %v", err)
	}

	key1 := []byte("key1")
	key2 := []byte("key2")

	// Both values have the same index key to test uniqueness constraint
	sameIndexKey := []byte{0x01}

	val1 := &tValue{
		k:   key1,
		v:   []byte("value1"),
		idx: sameIndexKey,
	}
	val2 := &tValue{
		k:   key2,
		v:   []byte("value2"),
		idx: sameIndexKey,
	}

	// First insert should succeed
	if err := tbl.Set(key1, val1); err != nil {
		t.Fatalf("Error setting first value: %v", err)
	}

	// Second insert with same index key should fail without kvstore.WithReplace()
	err = tbl.Set(key2, val2)
	if err == nil {
		t.Fatal("Expected error when inserting duplicate index value without kvstore.WithReplace(), but got none")
	}
	if !strings.Contains(err.Error(), "index uniqueness violation") {
		t.Fatalf("Expected unique index error, got: %v", err)
	}

	// Verify the first value is still there
	var retrievedVal []byte
	numEntries := 0
	err = tbl.Iterate(nil, func(it kvstore.StoreIter) error {
		numEntries++
		err := it.V(func(v []byte) error {
			retrievedVal = v
			return nil
		})
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Error iterating table: %v", err)
	}
	if !bytes.Equal(retrievedVal, val1.v) {
		t.Fatalf("Expected value1 to still be in table, got %s", retrievedVal)
	}
	if numEntries != 1 {
		t.Fatalf("Expected 1 entry in table, got %d", numEntries)
	}

	// Second insert with same index key should succeed with kvstore.WithReplace()
	if err := tbl.Set(key2, val2, kvstore.WithReplace()); err != nil {
		t.Fatalf("Error setting second value with kvstore.WithReplace(): %v", err)
	}

	// Verify the replacement worked by checking the table content
	numEntries = 0
	var lastVal []byte
	err = tbl.Iterate(nil, func(it kvstore.StoreIter) error {
		numEntries++
		return it.V(func(v []byte) error {
			lastVal = v
			return nil
		})
	})
	if err != nil {
		t.Fatalf("Error iterating table after replace: %v", err)
	}
	if numEntries != 1 {
		t.Fatalf("Expected 1 value in table after replace, got %d", numEntries)
	}
	if !bytes.Equal(lastVal, val2.v) {
		t.Fatalf("Expected replaced value to be value2, got %s", lastVal)
	}

	// Verify we can iterate through the unique index and get the correct result
	numEntries = 0
	uniqueIdx.Iterate(nil, func(it kvstore.StoreIter) error {
		numEntries++
		return it.V(func(v []byte) error {
			if !bytes.Equal(v, val2.v) {
				t.Fatalf("Expected value2 when iterating unique index, got %s", v)
			}
			return nil
		})
	})
	if numEntries != 1 {
		t.Fatalf("Expected 1 value in unique index, got %d", numEntries)
	}
}

// TestNotIndexed tests that when an index mapping function returns
// ErrNotIndexed, the datum is not added to the index.
func TestNotIndexed(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	tbl, err := db.table("NotIndexedTest")
	if err != nil {
		t.Fatalf("Error creating table: %v", err)
	}

	// Create an index that only indexes even values where the first byte is
	// even.
	idx, err := tbl.AddIndex("EvenOnly", func(k, v KV) ([]byte, error) {
		val := v.(*tValue)
		if len(val.v) > 0 && val.v[0]%2 == 0 {
			return []byte{val.v[0]}, nil
		}
		return nil, ErrNotIndexed
	})
	if err != nil {
		t.Fatalf("Error adding index: %v", err)
	}

	// Insert 10 values, with alternating even/odd first bytes
	for i := 0; i < 10; i++ {
		k := []byte{byte(i)}
		v := &tValue{
			k:   k,
			v:   append([]byte{byte(i)}, encode.RandomBytes(5)...),
			idx: []byte{byte(i)},
		}
		if err := tbl.Set(k, v); err != nil {
			t.Fatalf("Error setting value %d: %v", i, err)
		}
	}

	count := 0
	expectedValues := []byte{0, 2, 4, 6, 8}
	foundValues := make([]byte, 0, 5)
	err = idx.Iterate(nil, func(it kvstore.StoreIter) error {
		count++
		return it.V(func(vB []byte) error {
			if len(vB) > 0 {
				foundValues = append(foundValues, vB[0])
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("Error iterating index: %v", err)
	}

	// Ensure that the number of values in the index is correct,
	// and that the expected values were found.
	if count != 5 {
		t.Fatalf("Expected 5 indexed values, got %d", count)
	}
	slices.Sort(foundValues)
	if !reflect.DeepEqual(foundValues, expectedValues) {
		t.Fatalf("Expected values %v, got %v", expectedValues, foundValues)
	}

	// Ensure the table has all the values, even those that were not indexed.
	tableCount := 0
	err = tbl.Iterate(nil, func(it kvstore.StoreIter) error {
		tableCount++
		return nil
	})
	if err != nil {
		t.Fatalf("Error iterating table: %v", err)
	}
	if tableCount != 10 {
		t.Fatalf("Expected 10 table entries, got %d", tableCount)
	}

	// Delete an even and an odd value
	evenKey := []byte{2}
	oddKey := []byte{3}
	if err := tbl.Delete(evenKey); err != nil {
		t.Fatalf("Error deleting even key: %v", err)
	}
	if err := tbl.Delete(oddKey); err != nil {
		t.Fatalf("Error deleting odd key: %v", err)
	}

	// Recheck the index after deletion
	count = 0
	expectedValues = []byte{0, 4, 6, 8} // 2 was removed
	foundValues = make([]byte, 0, 4)
	err = idx.Iterate(nil, func(it kvstore.StoreIter) error {
		count++
		return it.V(func(vB []byte) error {
			if len(vB) > 0 {
				foundValues = append(foundValues, vB[0])
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("Error iterating index after deletion: %v", err)
	}

	// Ensure that the number of values in the index is correct after deletion,
	// and that the expected values were found.
	if count != 4 {
		t.Fatalf("Expected 4 indexed values after deletion, got %d", count)
	}
	slices.Sort(foundValues)
	if !reflect.DeepEqual(foundValues, expectedValues) {
		t.Fatalf("Expected values %v after deletion, got %v", expectedValues, foundValues)
	}

	// Recheck the table after deletion
	tableCount = 0
	err = tbl.Iterate(nil, func(it kvstore.StoreIter) error {
		tableCount++
		return nil
	})
	if err != nil {
		t.Fatalf("Error iterating table after deletion: %v", err)
	}
	if tableCount != 8 {
		t.Fatalf("Expected 8 table entries after deletion, got %d", tableCount)
	}
}

// TestTransactionOptions tests the new WithTxn and WithGetTxn options for
// Set and Get operations.
func TestTransactionOptions(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	tbl, err := db.table("TxnTest")
	if err != nil {
		t.Fatalf("Error creating table: %v", err)
	}

	key := []byte("key")
	val := &tValue{
		k:   key,
		v:   []byte("val"),
		idx: []byte{0x99},
	}

	// This transaction should rollback and not persist the value
	err = db.Update(func(txn *badger.Txn) error {
		if err := tbl.Set(key, val, WithTxn(txn)); err != nil {
			return err
		}

		// Verify we can read it within the transaction
		if _, err := tbl.Get(key, WithGetTxn(txn)); err != nil {
			return fmt.Errorf("unexpected error reading within txn: %w", err)
		}

		// Force rollback by returning an error
		return fmt.Errorf("intentional rollback")
	})
	if err == nil || !strings.Contains(err.Error(), "intentional rollback") {
		t.Fatalf("Expected intentional rollback error, got: %v", err)
	}

	// Verify the value was not persisted
	_, err = tbl.Get(key)
	if err == nil {
		t.Fatalf("Expected error when getting rolled back value, but got none")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "key not found") {
		t.Fatalf("Expected key not found error, got: %v", err)
	}
}

// TestSetBatchIgnoresCallerTxn verifies that a caller-supplied WithTxn cannot
// override SetBatch's internal batch transaction: SetBatch overwrites the
// resolved options' Txn with its own after applying the caller's options.
// Otherwise the writes would land in the caller's transaction and break batch
// atomicity.
func TestSetBatchIgnoresCallerTxn(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	tbl, err := db.table("SetBatchTxnTest")
	if err != nil {
		t.Fatalf("Error creating table: %v", err)
	}

	// A stray transaction that is never committed. If a caller-supplied
	// WithTxn were allowed to win, SetBatch's writes would land here and be
	// lost when the txn is discarded.
	strayTxn := db.NewTransaction(true)
	defer strayTxn.Discard()

	pairs := []kvstore.BatchEntry{
		{K: []byte("k1"), V: &tValue{v: []byte("v1")}},
		{K: []byte("k2"), V: &tValue{v: []byte("v2")}},
	}

	if err := tbl.SetBatch(pairs, WithTxn(strayTxn)); err != nil {
		t.Fatalf("SetBatch error: %v", err)
	}

	// Every entry must be readable from committed state, proving it landed in
	// SetBatch's own committed transaction rather than the stray one.
	for _, p := range pairs {
		if _, err := tbl.Get(p.K); err != nil {
			t.Fatalf("entry %q missing after SetBatch (caller WithTxn leaked): %v", p.K, err)
		}
	}
}

// TestIndexDanglingRow forces Iter.datum's dangling-entry path for index
// iteration by removing the row an index entry ultimately resolves to. The
// error has to name the condition: the datum is resolved through the KeyID→key
// map in extra lookups, so a bare ErrKeyNotFound out of an index scan says
// nothing about what is missing.
func TestIndexDanglingRow(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	tbl, err := db.table("dangling")
	if err != nil {
		t.Fatalf("error creating table: %v", err)
	}
	idx, err := tbl.AddIndex("byte0", func(_, v KV) ([]byte, error) {
		return v.([]byte)[:1], nil
	})
	if err != nil {
		t.Fatalf("error adding index: %v", err)
	}
	if err := tbl.Set([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("error setting: %v", err)
	}

	// Remove the row out from under the index entry and the KeyID→key map.
	if err := db.Update(func(txn *badger.Txn) error {
		return txn.Delete(PrefixedKey(tbl.prefix, []byte("k")))
	}); err != nil {
		t.Fatalf("error deleting row: %v", err)
	}

	err = idx.Iterate(nil, func(it kvstore.StoreIter) error {
		return it.V(func(_ []byte) error { return nil })
	})
	if err == nil {
		t.Fatal("expected dangling-entry error")
	}
	if !strings.Contains(err.Error(), "dangling entry") {
		t.Fatalf("error does not name the condition: %v", err)
	}
}

// TestIndexDanglingIDMapEntry forces Iter.K's dangling-entry path by removing
// the KeyID→key mapping an index entry depends on. Same reasoning as
// TestIndexDanglingRow: K resolves the key in a second lookup, so a bare
// ErrKeyNotFound out of an index scan says nothing about what is missing.
func TestIndexDanglingIDMapEntry(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	tbl, err := db.table("danglingID")
	if err != nil {
		t.Fatalf("error creating table: %v", err)
	}
	idx, err := tbl.AddIndex("byte0", func(_, v KV) ([]byte, error) {
		return v.([]byte)[:1], nil
	})
	if err != nil {
		t.Fatalf("error adding index: %v", err)
	}
	if err := tbl.Set([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("error setting: %v", err)
	}

	// Drop the KeyID→key mapping the index entry resolves through.
	keyID, err := tbl.KeyID([]byte("k"))
	if err != nil {
		t.Fatalf("KeyID: %v", err)
	}
	if err := db.Update(func(txn *badger.Txn) error {
		return txn.Delete(idMapKey(tbl.prefix, keyID))
	}); err != nil {
		t.Fatalf("error deleting id map entry: %v", err)
	}

	err = idx.Iterate(nil, func(it kvstore.StoreIter) error {
		_, err := it.K()
		return err
	})
	if err == nil {
		t.Fatal("expected dangling-entry error")
	}
	if !strings.Contains(err.Error(), "dangling entry") {
		t.Fatalf("error does not name the condition: %v", err)
	}
}

// TestUniqueIndexPrefixShadowing pins the exact-value match in the unique
// conflict check. The conflict scan matches the index value as a byte prefix,
// so with variable-length values an entry for "ab" is visited when checking
// "a" (and, through the KeyID bytes, potentially the reverse); only an entry
// of exactly the checked value's length is a real conflict. Without the
// length guard the shorter value reported a false conflict - and under
// WithReplace evicted the unrelated row.
func TestUniqueIndexPrefixShadowing(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	tbl, err := db.Table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if _, err := tbl.AddUniqueIndex("u", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	}); err != nil {
		t.Fatalf("AddUniqueIndex: %v", err)
	}

	set := func(k, idx string, opts ...kvstore.SetOption) error {
		return tbl.Set([]byte(k), &tValue{v: []byte("v-" + k), idx: []byte(idx)}, opts...)
	}
	if err := set("k1", "ab"); err != nil {
		t.Fatalf("initial Set: %v", err)
	}
	// A value that is a strict prefix of an existing value is not a conflict.
	if err := set("k2", "a"); err != nil {
		t.Fatalf("prefix of an existing value reported as conflict: %v", err)
	}
	// Nor is a value that extends an existing one.
	if err := set("k3", "abc"); err != nil {
		t.Fatalf("extension of an existing value reported as conflict: %v", err)
	}
	// An exact duplicate is.
	if err := set("k4", "ab"); err == nil {
		t.Fatal("exact duplicate not reported as conflict")
	}
	// Under WithReplace, only the row actually holding the value is evicted.
	if err := set("k4", "ab", kvstore.WithReplace()); err != nil {
		t.Fatalf("replace of exact duplicate failed: %v", err)
	}
	if _, err := tbl.Get([]byte("k1")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("conflicting row not evicted, Get(k1) = %v", err)
	}
	for _, k := range []string{"k2", "k3", "k4"} {
		if _, err := tbl.Get([]byte(k)); err != nil {
			t.Fatalf("row %q lost to a false conflict: %v", k, err)
		}
	}
}
