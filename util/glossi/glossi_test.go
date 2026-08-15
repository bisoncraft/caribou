// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package glossi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
	"go.etcd.io/bbolt"
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

func TestSetBatch(t *testing.T) {
	db := newTestDB(t)
	tbl, err := db.Table("things")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}

	// Empty batch is a no-op.
	if err := tbl.SetBatch(nil); err != nil {
		t.Fatalf("SetBatch(nil): %v", err)
	}

	entries := []kvstore.BatchEntry{
		{K: []byte("k1"), V: []byte("v1")},
		{K: []byte("k2"), V: []byte("v2")},
		{K: []byte("k3"), V: []byte("v3")},
	}
	if err := tbl.SetBatch(entries); err != nil {
		t.Fatalf("SetBatch: %v", err)
	}
	for _, e := range entries {
		got, err := tbl.Get(e.K)
		if err != nil || !bytes.Equal(got, e.V.([]byte)) {
			t.Fatalf("Get(%q) = %q, %v", e.K, got, err)
		}
	}

	// A zero-length key anywhere in the batch is rejected before any write.
	if err := tbl.SetBatch([]kvstore.BatchEntry{{K: []byte("k4"), V: []byte("v4")}, {K: nil, V: []byte("x")}}); err == nil {
		t.Fatal("expected error for zero-length key in batch")
	}
	if _, err := tbl.Get([]byte("k4")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("k4 should not have been written, got %v", err)
	}

	// Without WithReplace, a conflict mid-batch rolls back the whole batch:
	// the earlier entry in the same batch must not persist.
	err = tbl.SetBatch([]kvstore.BatchEntry{
		{K: []byte("fresh"), V: []byte("a")},
		{K: []byte("k1"), V: []byte("dup")}, // k1 already exists -> conflict
	})
	if err == nil {
		t.Fatal("expected conflict error without WithReplace")
	}
	if _, err := tbl.Get([]byte("fresh")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("batch should have rolled back; 'fresh' persisted: %v", err)
	}

	// With WithReplace, existing keys are updated.
	if err := tbl.SetBatch([]kvstore.BatchEntry{{K: []byte("k1"), V: []byte("v1-new")}}, WithReplace()); err != nil {
		t.Fatalf("SetBatch replace: %v", err)
	}
	if got, _ := tbl.Get([]byte("k1")); !bytes.Equal(got, []byte("v1-new")) {
		t.Fatalf("k1 after replace = %q", got)
	}
}

// blockIndexFunc indexes a value whose first 8 bytes are a big-endian uint64.
func blockIndexFunc(_, vI KV) ([]byte, error) {
	v := vI.([]byte)
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
	idx, _ := tbl.AddIndex("selective", func(_, vI KV) ([]byte, error) {
		v := vI.([]byte)
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

// TestDatum exercises the datum encode/decode round-trip and the error paths in
// decodeDatum (corrupted blob, unknown version, missing value push).
func TestDatum(t *testing.T) {
	value := []byte("the answer is 42")
	idxA := []byte{0x01, 0x02, 0x03}
	idxB := bytes.Repeat([]byte{0xab}, 300) // forces a multi-byte length prefix
	blob := encodeDatum(value, [][]byte{idxA, idxB})
	gotV, gotIdx, err := decodeDatum(blob)
	if err != nil {
		t.Fatalf("round-trip decode: %v", err)
	}
	if !bytes.Equal(gotV, value) {
		t.Fatalf("value round-trip: got %q, want %q", gotV, value)
	}
	if len(gotIdx) != 2 || !bytes.Equal(gotIdx[0], idxA) || !bytes.Equal(gotIdx[1], idxB) {
		t.Fatalf("index round-trip mismatch: %v", gotIdx)
	}

	// No-index round trip.
	blob = encodeDatum(value, nil)
	gotV, gotIdx, err = decodeDatum(blob)
	if err != nil {
		t.Fatalf("no-index decode: %v", err)
	}
	if !bytes.Equal(gotV, value) || len(gotIdx) != 0 {
		t.Fatalf("no-index round-trip mismatch")
	}

	// Empty input is rejected by the underlying DecodeBlob.
	if _, _, err := decodeDatum(nil); err == nil {
		t.Fatal("expected error decoding empty blob")
	}

	// A single byte = version, no pushes; the value push is missing.
	if _, _, err := decodeDatum([]byte{datumVersion}); err == nil {
		t.Fatal("expected error decoding blob with no value push")
	}

	// Version byte that doesn't match datumVersion.
	if _, _, err := decodeDatum([]byte{datumVersion + 1}); err == nil {
		t.Fatal("expected error decoding blob with unknown version")
	}
}

// TestNewBadPath verifies that New surfaces the underlying bbolt.Open error
// when the path can't be opened.
func TestNewBadPath(t *testing.T) {
	// A path under a regular file (not a directory) cannot be opened.
	dir := t.TempDir()
	notDir := filepath.Join(dir, "not_a_dir")
	if err := os.WriteFile(notDir, []byte("x"), 0600); err != nil {
		t.Fatalf("seeding regular file: %v", err)
	}
	db, err := New(filepath.Join(notDir, "db.glossi"), util.NewLogger("T", util.LevelInfo, os.Stdout))
	if err == nil {
		db.Close()
		t.Fatal("expected error opening db under a regular file")
	}
}

// TestEmptyTableAndIndexNames verifies the validation errors for empty table
// and index names.
func TestEmptyTableAndIndexNames(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Table(""); err == nil {
		t.Fatal("expected error from db.Table(\"\")")
	}
	tbl, err := db.Table("ok")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if _, err := tbl.AddIndex("", blockIndexFunc); err == nil {
		t.Fatal("expected error from AddIndex(\"\")")
	}
	if _, err := tbl.AddUniqueIndex("", blockIndexFunc); err == nil {
		t.Fatal("expected error from AddUniqueIndex(\"\")")
	}
}

// TestConnect verifies that the DB is closed when the context is canceled and
// that the returned WaitGroup completes.
func TestConnect(t *testing.T) {
	dir := t.TempDir()
	db, err := New(filepath.Join(dir, "conn.db"), util.NewLogger("T", util.LevelInfo, os.Stdout))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	wg, err := db.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Do real work to be sure the DB is alive.
	tbl, err := db.Table("t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if err := tbl.Set([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Cancel and wait for shutdown.
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Connect goroutine did not exit after context cancel")
	}

	// After shutdown the DB must reject new work.
	if err := tbl.Set([]byte("k2"), []byte("v2")); err == nil {
		t.Fatal("expected error using DB after Connect-driven shutdown")
	}
}

// TestIterEntry verifies Iter.Entry yields the index entry (sans index id and
// primary key) during index iteration and the primary key during table
// iteration.
func TestIterEntry(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	idx, _ := tbl.AddIndex("block", blockIndexFunc)

	tbl.Set([]byte("alpha"), u64(7))
	tbl.Set([]byte("beta"), u64(11))

	var indexEntries [][]byte
	if err := idx.Iterate(nil, func(it *Iter) error {
		return it.Entry(func(e []byte) error {
			indexEntries = append(indexEntries, append([]byte(nil), e...))
			return nil
		})
	}); err != nil {
		t.Fatalf("index iterate: %v", err)
	}
	want := [][]byte{u64(7), u64(11)}
	if len(indexEntries) != 2 ||
		!bytes.Equal(indexEntries[0], want[0]) ||
		!bytes.Equal(indexEntries[1], want[1]) {
		t.Fatalf("index Entry = %v, want %v", indexEntries, want)
	}

	var tableEntries [][]byte
	if err := tbl.Iterate(nil, func(it *Iter) error {
		return it.Entry(func(e []byte) error {
			tableEntries = append(tableEntries, append([]byte(nil), e...))
			return nil
		})
	}); err != nil {
		t.Fatalf("table iterate: %v", err)
	}
	wantKeys := [][]byte{[]byte("alpha"), []byte("beta")}
	if len(tableEntries) != 2 ||
		!bytes.Equal(tableEntries[0], wantKeys[0]) ||
		!bytes.Equal(tableEntries[1], wantKeys[1]) {
		t.Fatalf("table Entry = %v, want %v", tableEntries, wantKeys)
	}
}

// TestIterDeleteRequiresUpdate ensures Iter.Delete refuses to run in a
// read-only iteration.
func TestIterDeleteRequiresUpdate(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	tbl.Set([]byte("k"), []byte("v"))

	err := tbl.Iterate(nil, func(it *Iter) error {
		return it.Delete()
	})
	if err == nil {
		t.Fatal("expected error from Iter.Delete without WithUpdate")
	}
	// The key must still be there.
	if _, err := tbl.Get([]byte("k")); err != nil {
		t.Fatalf("key was unexpectedly deleted: %v", err)
	}
}

// TestForwardSeekBeforeBound exercises the seek-below-bound clamp in
// forwardWalk: an index-space seek key that sorts before the iterate prefix
// must still produce all matching entries rather than silently returning none.
func TestForwardSeekBeforeBound(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	// Index by the first byte of the value.
	idx, _ := tbl.AddIndex("byte0", func(_, vI KV) ([]byte, error) {
		v := vI.([]byte)
		return v[:1], nil
	})
	// Two index "groups": prefix A and prefix B.
	tbl.Set([]byte("a1"), []byte{'A', 1})
	tbl.Set([]byte("a2"), []byte{'A', 2})
	tbl.Set([]byte("b1"), []byte{'B', 1})
	tbl.Set([]byte("b2"), []byte{'B', 2})

	// Iterate the "B" prefix but seek to "A" (which sorts before B).
	// The implementation must clamp seek up to bound and yield both B entries.
	var keys []string
	err := idx.Iterate([]byte("B"), func(it *Iter) error {
		k, _ := it.K()
		keys = append(keys, string(k))
		return nil
	}, WithSeek([]byte("A")))
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(keys) != 2 || keys[0] != "b1" || keys[1] != "b2" {
		t.Fatalf("got %v, want [b1 b2]", keys)
	}
}

// TestReverseSeekBeforeBound mirrors TestForwardSeekBeforeBound for reverse
// iteration.
func TestReverseSeekBeforeBound(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	idx, _ := tbl.AddIndex("byte0", func(_, vI KV) ([]byte, error) {
		v := vI.([]byte)
		return v[:1], nil
	})
	tbl.Set([]byte("a1"), []byte{'A', 1})
	tbl.Set([]byte("b1"), []byte{'B', 1})
	tbl.Set([]byte("b2"), []byte{'B', 2})

	var keys []string
	err := idx.Iterate([]byte("B"), func(it *Iter) error {
		k, _ := it.K()
		keys = append(keys, string(k))
		return nil
	}, WithReverse(), WithSeek([]byte("A")))
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(keys) != 2 || keys[0] != "b2" || keys[1] != "b1" {
		t.Fatalf("got %v, want [b2 b1]", keys)
	}
}

// TestReverseAllOnesPrefix forces prefixUpperBound to return nil by iterating
// the table in reverse with a prefix of all 0xff bytes.
func TestReverseAllOnesPrefix(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	tbl.Set([]byte{0xff, 0x01}, []byte("a"))
	tbl.Set([]byte{0xff, 0x02}, []byte("b"))
	tbl.Set([]byte{0xfe, 0x03}, []byte("c")) // outside the prefix

	var keys [][]byte
	err := tbl.Iterate([]byte{0xff}, func(it *Iter) error {
		k, _ := it.K()
		keys = append(keys, append([]byte(nil), k...))
		return nil
	}, WithReverse())
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if len(keys) != 2 ||
		!bytes.Equal(keys[0], []byte{0xff, 0x02}) ||
		!bytes.Equal(keys[1], []byte{0xff, 0x01}) {
		t.Fatalf("got %x, want [ff02, ff01]", keys)
	}
}

// TestReverseTableNoPrefix exercises the prefixUpperBound(nil) → c.Last branch
// in reverseWalk via a table iteration without a prefix.
func TestReverseTableNoPrefix(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	tbl.Set([]byte("a"), []byte("1"))
	tbl.Set([]byte("b"), []byte("2"))
	tbl.Set([]byte("c"), []byte("3"))

	var keys []string
	err := tbl.Iterate(nil, func(it *Iter) error {
		k, _ := it.K()
		keys = append(keys, string(k))
		return nil
	}, WithReverse())
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if len(keys) != 3 || keys[0] != "c" || keys[1] != "b" || keys[2] != "a" {
		t.Fatalf("got %v, want [c b a]", keys)
	}
}

// TestTableIterateAll exercises the prefix-less forward branch of forwardWalk.
func TestTableIterateAll(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	for i := range 5 {
		if err := tbl.Set([]byte{'k', byte(i)}, []byte{byte(i)}); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	var n int
	if err := tbl.Iterate(nil, func(it *Iter) error { n++; return nil }); err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if n != 5 {
		t.Fatalf("expected 5 entries, got %d", n)
	}
}

// TestIndexIteratePrefix exercises iteration constrained to an in-index
// prefix, in both directions.
func TestIndexIteratePrefix(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	// Two-byte index: group, ordinal.
	idx, _ := tbl.AddIndex("grp", func(_, vI KV) ([]byte, error) {
		v := vI.([]byte)
		return v[:2], nil
	})
	tbl.Set([]byte("a"), []byte{0x10, 0x01})
	tbl.Set([]byte("b"), []byte{0x10, 0x02})
	tbl.Set([]byte("c"), []byte{0x20, 0x01})
	tbl.Set([]byte("d"), []byte{0x20, 0x02})

	var keys []string
	if err := idx.Iterate([]byte{0x10}, func(it *Iter) error {
		k, _ := it.K()
		keys = append(keys, string(k))
		return nil
	}); err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
		t.Fatalf("forward got %v, want [a b]", keys)
	}

	keys = nil
	if err := idx.Iterate([]byte{0x20}, func(it *Iter) error {
		k, _ := it.K()
		keys = append(keys, string(k))
		return nil
	}, WithReverse()); err != nil {
		t.Fatalf("Iterate reverse: %v", err)
	}
	if len(keys) != 2 || keys[0] != "d" || keys[1] != "c" {
		t.Fatalf("reverse got %v, want [d c]", keys)
	}
}

// TestSeekPastEnd verifies that seeking beyond all entries yields no rows.
func TestSeekPastEnd(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	idx, _ := tbl.AddIndex("block", blockIndexFunc)
	for i, b := range []uint64{10, 20, 30} {
		tbl.Set([]byte{byte('a' + i)}, u64(b))
	}
	var n int
	err := idx.Iterate(nil, func(it *Iter) error { n++; return nil }, WithSeek(u64(999)))
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 entries, got %d", n)
	}
}

// TestIndexFnError verifies that a non-ErrNotIndexed error from the index
// function aborts the Set call.
func TestIndexFnError(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	sentinel := errors.New("index function exploded")
	_, err := tbl.AddIndex("explode", func(_, _ KV) ([]byte, error) {
		return nil, sentinel
	})
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}
	err = tbl.Set([]byte("k"), []byte("v"))
	if err == nil {
		t.Fatal("expected error from index function to propagate")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel error, got %v", err)
	}
	// The data must not have been written, since the txn rolled back.
	if _, err := tbl.Get([]byte("k")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected key to be absent after failed Set, got err=%v", err)
	}
}

// TestMultipleIndexesReplaceAndDelete verifies that Set-with-replace and
// Delete purge entries from every index a datum populated.
func TestMultipleIndexesReplaceAndDelete(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	byBlock, _ := tbl.AddIndex("block", blockIndexFunc)
	bySuffix, _ := tbl.AddIndex("suffix", func(_, vI KV) ([]byte, error) {
		v := vI.([]byte)
		return v[len(v)-1:], nil
	})

	// Initial Set populates both indexes.
	tbl.Set([]byte("k"), append(u64(100), 0xaa))
	count := func(idx *Index) int {
		var n int
		idx.Iterate(nil, func(_ *Iter) error { n++; return nil })
		return n
	}
	if got := count(byBlock); got != 1 {
		t.Fatalf("byBlock initial count = %d, want 1", got)
	}
	if got := count(bySuffix); got != 1 {
		t.Fatalf("bySuffix initial count = %d, want 1", got)
	}

	// Replace moves both index values; the old entries must be gone.
	tbl.Set([]byte("k"), append(u64(200), 0xbb), WithReplace())

	got := []uint64{}
	byBlock.Iterate(nil, func(it *Iter) error {
		return it.V(func(v []byte) error {
			got = append(got, binary.BigEndian.Uint64(v[:8]))
			return nil
		})
	})
	if !equalU64(got, []uint64{200}) {
		t.Fatalf("byBlock after replace = %v, want [200]", got)
	}

	suffixes := []byte{}
	bySuffix.Iterate(nil, func(it *Iter) error {
		return it.V(func(v []byte) error {
			suffixes = append(suffixes, v[len(v)-1])
			return nil
		})
	})
	if len(suffixes) != 1 || suffixes[0] != 0xbb {
		t.Fatalf("bySuffix after replace = %x, want [bb]", suffixes)
	}

	// Delete must clear both indexes.
	if err := tbl.Delete([]byte("k")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := count(byBlock); got != 0 {
		t.Fatalf("byBlock after delete = %d, want 0", got)
	}
	if got := count(bySuffix); got != 0 {
		t.Fatalf("bySuffix after delete = %d, want 0", got)
	}
}

// TestTooManyIndexes ensures the 1-byte id sequence overflows cleanly. Using
// the unexported sequence bucket lets us prime the counter at 0xff so we don't
// have to allocate 255 real indexes.
func TestTooManyIndexes(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	// Add one real index so the sequence sub-bucket exists and is initialized
	// by addIndex's normal path.
	if _, err := tbl.AddIndex("first", blockIndexFunc); err != nil {
		t.Fatalf("AddIndex first: %v", err)
	}
	// Bump the sequence to 0xff directly via bbolt.
	err := db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(tbl.name).Bucket(seqBucketKey).Put(seqKey, []byte{0xff})
	})
	if err != nil {
		t.Fatalf("seq bump: %v", err)
	}
	if _, err := tbl.AddIndex("overflow", blockIndexFunc); err == nil {
		t.Fatal("expected overflow error adding index past 0xff")
	}
}

// TestUniqueIndexAcrossReopen ensures the unique constraint still trips after
// closing and reopening the DB. The persisted state has to be enough to detect
// the conflict on its own.
func TestUniqueIndexAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unique.db")
	log := util.NewLogger("T", util.LevelInfo, os.Stdout)

	db, err := New(path, log)
	if err != nil {
		t.Fatal(err)
	}
	tbl, _ := db.Table("txs")
	if _, err := tbl.AddUniqueIndex("nonce", blockIndexFunc); err != nil {
		t.Fatalf("AddUniqueIndex: %v", err)
	}
	if err := tbl.Set([]byte("k1"), u64(7)); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := New(path, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db2.Close() })
	tbl2, _ := db2.Table("txs")
	if _, err := tbl2.AddUniqueIndex("nonce", blockIndexFunc); err != nil {
		t.Fatalf("re-AddUniqueIndex: %v", err)
	}
	err = tbl2.Set([]byte("k2"), u64(7))
	if !errors.Is(err, ErrUniqueConflict) {
		t.Fatalf("expected ErrUniqueConflict after reopen, got %v", err)
	}
}

// TestGetValueIsolated verifies the bytes returned by Get are independent of
// the underlying bbolt transaction: mutating them must not affect later reads.
func TestGetValueIsolated(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	tbl.Set([]byte("k"), []byte("original"))

	got, err := tbl.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Mutate the returned slice.
	for i := range got {
		got[i] = 'x'
	}
	again, err := tbl.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	if !bytes.Equal(again, []byte("original")) {
		t.Fatalf("stored value was mutated: got %q", again)
	}
}

// TestIndexFuncErrorWrapped verifies that fmt.Errorf wrapping in Set names the
// failing index — useful when multiple indexes are in play.
func TestIndexFuncErrorWrapped(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	tbl.AddIndex("ok", blockIndexFunc)
	boom := errors.New("kaboom")
	tbl.AddIndex("breakable", func(_, _ KV) ([]byte, error) { return nil, boom })

	err := tbl.Set([]byte("k"), u64(1))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
	// Sanity: the error message should mention the failing index name.
	if want := "breakable"; !contains(err.Error(), want) {
		t.Fatalf("error %q does not name failing index %q", err, want)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && bytes.Contains([]byte(s), []byte(sub))
}

// assertNoErr is a tiny helper used by integration-style tests below.
func assertNoErr(t *testing.T, where string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", where, err)
	}
}

// TestIterDeleteOnBareIter covers the defensive guard in Iter.Delete that
// refuses to run on an Iter without a backing bucket. Production code paths
// always populate both buckets, so the only way to reach the guard is to
// construct an Iter directly inside the package.
func TestIterDeleteOnBareIter(t *testing.T) {
	if err := (&Iter{}).Delete(); err == nil {
		t.Fatal("expected error from Iter.Delete on a bare Iter")
	}
}

// TestTableIterateUpdate covers Table.Iterate's read-write branch and verifies
// that Iter.Delete can purge entries during a table iteration.
func TestTableIterateUpdate(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	idx, _ := tbl.AddIndex("byte0", func(_, vI KV) ([]byte, error) {
		v := vI.([]byte)
		return v[:1], nil
	})
	tbl.Set([]byte("a"), []byte{1})
	tbl.Set([]byte("b"), []byte{2})
	tbl.Set([]byte("c"), []byte{3})

	// Iterate the table with WithUpdate and delete "b".
	err := tbl.Iterate(nil, func(it *Iter) error {
		k, _ := it.K()
		if bytes.Equal(k, []byte("b")) {
			return it.Delete()
		}
		return nil
	}, WithUpdate())
	if err != nil {
		t.Fatalf("Iterate w/ update: %v", err)
	}

	var keys []string
	tbl.Iterate(nil, func(it *Iter) error {
		k, _ := it.K()
		keys = append(keys, string(k))
		return nil
	})
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "c" {
		t.Fatalf("after delete, table = %v, want [a c]", keys)
	}
	// The index entry for "b" must also be gone.
	var idxKeys []string
	idx.Iterate(nil, func(it *Iter) error {
		k, _ := it.K()
		idxKeys = append(idxKeys, string(k))
		return nil
	})
	if len(idxKeys) != 2 || idxKeys[0] != "a" || idxKeys[1] != "c" {
		t.Fatalf("after delete, index = %v, want [a c]", idxKeys)
	}
}

// TestEndIterationReverse verifies that ErrEndIteration from a reverse-
// iteration callback terminates cleanly (no error returned).
func TestEndIterationReverse(t *testing.T) {
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
	}, WithReverse())
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if seen != 2 {
		t.Fatalf("expected to stop at 2, saw %d", seen)
	}
}

// TestIterateCallbackErrorReverse verifies that a non-ErrEndIteration error
// returned from a reverse-iteration callback is propagated.
func TestIterateCallbackErrorReverse(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	idx, _ := tbl.AddIndex("block", blockIndexFunc)
	for i, b := range []uint64{10, 20, 30} {
		tbl.Set([]byte{byte('a' + i)}, u64(b))
	}
	sentinel := errors.New("cb error")
	err := idx.Iterate(nil, func(it *Iter) error {
		return sentinel
	}, WithReverse())
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected callback error, got %v", err)
	}
}

// TestIterateCallbackErrorForward covers the corresponding forward-iteration
// error-propagation path.
func TestIterateCallbackErrorForward(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("txs")
	idx, _ := tbl.AddIndex("block", blockIndexFunc)
	tbl.Set([]byte("k"), u64(1))
	sentinel := errors.New("cb error")
	err := idx.Iterate(nil, func(it *Iter) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected callback error, got %v", err)
	}
}

// TestGetCorruptDatum verifies that Get surfaces the datum-decoding error when
// the stored bytes don't parse. Corruption is injected directly via bbolt.
func TestGetCorruptDatum(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	// Inject a value whose first byte is a non-zero version, which will fail
	// the version check in decodeDatum.
	assertNoErr(t, "inject", db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(tbl.name).Bucket(dataBucketKey).Put([]byte("k"), []byte{0xff})
	}))
	if _, err := tbl.Get([]byte("k")); err == nil {
		t.Fatal("expected decode error from Get on corrupt datum")
	}
}

// TestSetReplaceCorruptDatum forces the decodeDatum error path inside Set's
// replace branch.
func TestSetReplaceCorruptDatum(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	// Inject a corrupted existing entry.
	assertNoErr(t, "inject", db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(tbl.name).Bucket(dataBucketKey).Put([]byte("k"), []byte{0xff})
	}))
	if err := tbl.Set([]byte("k"), []byte("v2"), WithReplace()); err == nil {
		t.Fatal("expected decode error during replace of corrupt entry")
	}
}

// TestDeleteCorruptDatum forces the decodeDatum error path inside Delete.
func TestDeleteCorruptDatum(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	assertNoErr(t, "inject", db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(tbl.name).Bucket(dataBucketKey).Put([]byte("k"), []byte{0xff})
	}))
	if err := tbl.Delete([]byte("k")); err == nil {
		t.Fatal("expected decode error from Delete on corrupt datum")
	}
}

// TestTableIterateCorruptDatum forces the decodeDatum error path inside
// Table.Iterate's per-entry decoding.
func TestTableIterateCorruptDatum(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	assertNoErr(t, "inject", db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(tbl.name).Bucket(dataBucketKey).Put([]byte("k"), []byte{0xff})
	}))
	err := tbl.Iterate(nil, func(it *Iter) error { return nil })
	if err == nil {
		t.Fatal("expected decode error from Iterate over corrupt datum")
	}
}

// TestIndexDanglingEntry forces idx.Iterate's "dangling index entry" path by
// injecting an index entry that points to a non-existent primary key.
func TestIndexDanglingEntry(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	idx, _ := tbl.AddIndex("byte0", func(_, vI KV) ([]byte, error) {
		v := vI.([]byte)
		return v[:1], nil
	})
	// Inject an index entry whose trailing primary key doesn't exist in the
	// data bucket. The shape is [idx.id][indexEntry][primaryKey] -> primaryKey.
	bogusKey := []byte("nope")
	entryKey := append([]byte{idx.id, 0x42}, bogusKey...)
	assertNoErr(t, "inject", db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(tbl.name).Bucket(indexBucketKey).Put(entryKey, bogusKey)
	}))
	err := idx.Iterate(nil, func(it *Iter) error { return nil })
	if err == nil {
		t.Fatal("expected dangling-index-entry error")
	}
}

// TestIndexIterateCorruptDatum forces idx.Iterate's decodeDatum error path
// (the entry resolves to a data row whose bytes don't parse).
func TestIndexIterateCorruptDatum(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	idx, _ := tbl.AddIndex("byte0", func(_, vI KV) ([]byte, error) {
		v := vI.([]byte)
		return v[:1], nil
	})
	// Plant an index entry that points at a primary key, then overwrite the
	// data row at that key with garbage so decodeDatum fails.
	pk := []byte("k1")
	entryKey := append([]byte{idx.id, 0x07}, pk...)
	assertNoErr(t, "inject", db.Update(func(tx *bbolt.Tx) error {
		tb := tx.Bucket(tbl.name)
		if err := tb.Bucket(indexBucketKey).Put(entryKey, pk); err != nil {
			return err
		}
		return tb.Bucket(dataBucketKey).Put(pk, []byte{0xff})
	}))
	err := idx.Iterate(nil, func(it *Iter) error { return nil })
	if err == nil {
		t.Fatal("expected decode error from index iterate over corrupt datum")
	}
}

// TestBoundedReverseHitsBreak ensures the bound-check break path in
// reverseWalk fires: with two indexes in the same bucket, reverse iteration
// over the second index must stop at the first index's keys.
func TestBoundedReverseHitsBreak(t *testing.T) {
	db := newTestDB(t)
	tbl, _ := db.Table("kv")
	idxA, _ := tbl.AddIndex("A", blockIndexFunc)
	idxB, _ := tbl.AddIndex("B", blockIndexFunc)

	for i := range 3 {
		assertNoErr(t, fmt.Sprintf("Set %d", i), tbl.Set([]byte{byte(i)}, u64(uint64(i))))
	}

	// Sanity: forward over A first.
	var aFwd []uint64
	idxA.Iterate(nil, func(it *Iter) error {
		return it.V(func(v []byte) error {
			aFwd = append(aFwd, binary.BigEndian.Uint64(v[:8]))
			return nil
		})
	})
	if !equalU64(aFwd, []uint64{0, 1, 2}) {
		t.Fatalf("A forward = %v", aFwd)
	}

	// Reverse over B exercises the break on the A→B prefix boundary at the end
	// of iteration: c.Prev() walks past B's first key into A's entries and the
	// bound check must end iteration.
	var bRev []uint64
	idxB.Iterate(nil, func(it *Iter) error {
		return it.V(func(v []byte) error {
			bRev = append(bRev, binary.BigEndian.Uint64(v[:8]))
			return nil
		})
	}, WithReverse())
	if !equalU64(bRev, []uint64{2, 1, 0}) {
		t.Fatalf("B reverse = %v", bRev)
	}
}
