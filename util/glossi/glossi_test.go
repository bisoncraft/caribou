// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package glossi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "test.db"), util.NewLogger("T", util.LevelInfo, os.Stdout))
	if err != nil {
		t.Fatalf("error constructing db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func TestSetGetDelete(t *testing.T) {
	db := newTestDB(t)
	tbl, err := db.Table("things")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}

	k, v := []byte("key1"), []byte("value1")
	if err := tbl.Set(k, v); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := tbl.Get(k)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, v) {
		t.Fatalf("got %q, want %q", got, v)
	}

	// Replacing without WithReplace must fail.
	if err := tbl.Set(k, []byte("value2")); err == nil {
		t.Fatal("expected error replacing without WithReplace")
	}
	// Replacing with WithReplace must succeed.
	if err := tbl.Set(k, []byte("value2"), WithReplace()); err != nil {
		t.Fatalf("Set replace: %v", err)
	}
	got, _ = tbl.Get(k)
	if !bytes.Equal(got, []byte("value2")) {
		t.Fatalf("after replace got %q", got)
	}

	if err := tbl.Delete(k); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := tbl.Get(k); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
	if err := tbl.Delete(k); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound deleting missing, got %v", err)
	}
}

func TestZeroLengthKey(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("things")
	if err := tbl.Set(nil, []byte("v")); err == nil {
		t.Fatal("expected error for zero-length key")
	}
}

// blockIndexFunc indexes a value whose first 8 bytes are a big-endian uint64.
func blockIndexFunc(_, v []byte) ([]byte, error) {
	return v[:8], nil
}

func TestIndexIterationOrder(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	idx, err := tbl.AddIndex("block", blockIndexFunc)
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}

	// Insert out of order. Value = block(8) so the index orders by block.
	blocks := []uint64{30, 10, 20, 50, 40}
	for i, b := range blocks {
		k := []byte{byte('a' + i)}
		if err := tbl.Set(k, u64(b)); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	collect := func(opts ...IterationOption) []uint64 {
		var out []uint64
		err := idx.Iterate(nil, func(it *Iter) error {
			return it.V(func(v []byte) error {
				out = append(out, binary.BigEndian.Uint64(v[:8]))
				return nil
			})
		}, opts...)
		if err != nil {
			t.Fatalf("Iterate: %v", err)
		}
		return out
	}

	fwd := collect()
	want := []uint64{10, 20, 30, 40, 50}
	if !equalU64(fwd, want) {
		t.Fatalf("forward order = %v, want %v", fwd, want)
	}

	rev := collect(WithReverse())
	wantRev := []uint64{50, 40, 30, 20, 10}
	if !equalU64(rev, wantRev) {
		t.Fatalf("reverse order = %v, want %v", rev, wantRev)
	}
}

func TestIndexSeek(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	idx, _ := tbl.AddIndex("block", blockIndexFunc)
	for i, b := range []uint64{10, 20, 30, 40, 50} {
		if err := tbl.Set([]byte{byte('a' + i)}, u64(b)); err != nil {
			t.Fatal(err)
		}
	}

	// Forward seek to block 30 should yield 30,40,50.
	var fwd []uint64
	idx.Iterate(nil, func(it *Iter) error {
		return it.V(func(v []byte) error {
			fwd = append(fwd, binary.BigEndian.Uint64(v[:8]))
			return nil
		})
	}, WithSeek(u64(30)))
	if !equalU64(fwd, []uint64{30, 40, 50}) {
		t.Fatalf("forward seek = %v", fwd)
	}

	// Reverse seek to block 30 should yield 30,20,10.
	var rev []uint64
	idx.Iterate(nil, func(it *Iter) error {
		return it.V(func(v []byte) error {
			rev = append(rev, binary.BigEndian.Uint64(v[:8]))
			return nil
		})
	}, WithReverse(), WithSeek(u64(30)))
	if !equalU64(rev, []uint64{30, 20, 10}) {
		t.Fatalf("reverse seek = %v", rev)
	}
}

func TestUniqueIndex(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	// Unique index on the first 8 bytes of the value.
	if _, err := tbl.AddUniqueIndex("nonce", blockIndexFunc); err != nil {
		t.Fatalf("AddUniqueIndex: %v", err)
	}

	if err := tbl.Set([]byte("k1"), u64(7)); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	// Different key, same index value -> conflict.
	err := tbl.Set([]byte("k2"), u64(7))
	if !errors.Is(err, ErrUniqueConflict) {
		t.Fatalf("expected ErrUniqueConflict, got %v", err)
	}
	// Same key, replace -> ok.
	if err := tbl.Set([]byte("k1"), append(u64(7), 'x'), WithReplace()); err != nil {
		t.Fatalf("replace same key: %v", err)
	}
	// A different index value for a new key -> ok.
	if err := tbl.Set([]byte("k2"), u64(8)); err != nil {
		t.Fatalf("distinct index value: %v", err)
	}
}

func TestReplacePurgesIndexEntries(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	idx, _ := tbl.AddIndex("block", blockIndexFunc)

	tbl.Set([]byte("k1"), u64(100))
	// Move it to a new block; old index entry must be purged.
	tbl.Set([]byte("k1"), u64(200), WithReplace())

	var blocks []uint64
	idx.Iterate(nil, func(it *Iter) error {
		return it.V(func(v []byte) error {
			blocks = append(blocks, binary.BigEndian.Uint64(v[:8]))
			return nil
		})
	})
	if !equalU64(blocks, []uint64{200}) {
		t.Fatalf("expected single entry [200], got %v", blocks)
	}
}

func TestNotIndexed(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	// Index only values whose first byte is 1.
	idx, _ := tbl.AddIndex("selective", func(_, v []byte) ([]byte, error) {
		if v[0] != 1 {
			return nil, ErrNotIndexed
		}
		return v[1:9], nil
	})
	tbl.Set([]byte("indexed"), append([]byte{1}, u64(5)...))
	tbl.Set([]byte("skipped"), append([]byte{0}, u64(6)...))

	var n int
	idx.Iterate(nil, func(it *Iter) error { n++; return nil })
	if n != 1 {
		t.Fatalf("expected 1 indexed entry, got %d", n)
	}
}

func TestEndIteration(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	idx, _ := tbl.AddIndex("block", blockIndexFunc)
	for i, b := range []uint64{10, 20, 30, 40, 50} {
		tbl.Set([]byte{byte('a' + i)}, u64(b))
	}
	var seen int
	err := idx.Iterate(nil, func(it *Iter) error {
		seen++
		if seen == 2 {
			return ErrEndIteration
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if seen != 2 {
		t.Fatalf("expected to stop at 2, saw %d", seen)
	}
}

func TestDeleteDuringIteration(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	idx, _ := tbl.AddIndex("block", blockIndexFunc)
	for i, b := range []uint64{10, 20, 30} {
		tbl.Set([]byte{byte('a' + i)}, u64(b))
	}
	// Delete block 20 during iteration.
	err := idx.Iterate(nil, func(it *Iter) error {
		var del bool
		it.V(func(v []byte) error {
			del = binary.BigEndian.Uint64(v[:8]) == 20
			return nil
		})
		if del {
			return it.Delete()
		}
		return nil
	}, WithUpdate())
	if err != nil {
		t.Fatalf("Iterate w/ delete: %v", err)
	}
	var blocks []uint64
	idx.Iterate(nil, func(it *Iter) error {
		return it.V(func(v []byte) error {
			blocks = append(blocks, binary.BigEndian.Uint64(v[:8]))
			return nil
		})
	})
	if !equalU64(blocks, []uint64{10, 30}) {
		t.Fatalf("after delete = %v, want [10 30]", blocks)
	}
}

func TestTablePrefixIteration(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	tbl.Set([]byte("apple"), []byte("1"))
	tbl.Set([]byte("apricot"), []byte("2"))
	tbl.Set([]byte("banana"), []byte("3"))

	var keys []string
	tbl.Iterate([]byte("ap"), func(it *Iter) error {
		k, _ := it.K()
		keys = append(keys, string(k))
		return nil
	})
	if len(keys) != 2 || keys[0] != "apple" || keys[1] != "apricot" {
		t.Fatalf("prefix iteration = %v", keys)
	}
}

func TestTableIsolation(t *testing.T) {
	db := newTestDB(t)
	a, _ := db.Table("a")
	b, _ := db.Table("b")
	a.Set([]byte("k"), []byte("va"))
	b.Set([]byte("k"), []byte("vb"))
	got, _ := a.Get([]byte("k"))
	if !bytes.Equal(got, []byte("va")) {
		t.Fatalf("table a got %q", got)
	}
	got, _ = b.Get([]byte("k"))
	if !bytes.Equal(got, []byte("vb")) {
		t.Fatalf("table b got %q", got)
	}
}

func TestIndexIDPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.db")
	log := util.NewLogger("T", util.LevelInfo, os.Stdout)

	db, err := New(path, log)
	if err != nil {
		t.Fatal(err)
	}
	tbl, _ := db.Table("txs")
	tbl.AddIndex("first", blockIndexFunc)
	idxB, _ := tbl.AddIndex("second", blockIndexFunc)
	secondID := idxB.id
	tbl.Set([]byte("k"), u64(1))
	db.Close()

	// Reopen and re-register indexes in the same order; ids must be stable.
	db2, err := New(path, log)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	tbl2, _ := db2.Table("txs")
	tbl2.AddIndex("first", blockIndexFunc)
	idx2, _ := tbl2.AddIndex("second", blockIndexFunc)
	if idx2.id != secondID {
		t.Fatalf("index id changed across reopen: %d != %d", idx2.id, secondID)
	}
	// The persisted entry must still be discoverable via the index.
	var found bool
	idx2.Iterate(nil, func(it *Iter) error { found = true; return nil })
	if !found {
		t.Fatal("persisted index entry not found after reopen")
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
