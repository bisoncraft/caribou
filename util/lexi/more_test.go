// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/dgraph-io/badger/v4"
)

// TestJSON covers the JSON BinaryMarshaler adapter and UnJSON helper in
// json.go, both of which are otherwise unexercised.
func TestJSON(t *testing.T) {
	type payload struct {
		Name  string
		Block uint64
	}
	p := &payload{Name: "alpha", Block: 42}
	bm := JSON(p)
	data, err := bm.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	var got payload
	bm2 := JSON(&got)
	if err := bm2.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if got.Name != p.Name || got.Block != p.Block {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, *p)
	}

	// UnJSON unwraps the embedded value.
	if x := UnJSON(JSON(p)); x != p {
		t.Fatalf("UnJSON returned %v, want %v", x, p)
	}
	// UnJSON on a non-lexiJSON returns struct{}{} as documented.
	if x := UnJSON([]byte{1, 2, 3}); x != (struct{}{}) {
		t.Fatalf("UnJSON of non-wrapped returned %v", x)
	}
}

// TestDBIDStringAndMarshal covers DBID.String, DBID.MarshalBinary, and
// newDBIDFromBytes.
func TestDBIDStringAndMarshal(t *testing.T) {
	raw := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	dbID := newDBIDFromBytes(raw)
	if got := dbID.String(); got != "0001020304050607" {
		t.Fatalf("DBID.String = %q", got)
	}
	b, err := dbID.MarshalBinary()
	if err != nil {
		t.Fatalf("DBID.MarshalBinary: %v", err)
	}
	if !bytes.Equal(b, raw) {
		t.Fatalf("DBID.MarshalBinary = %x, want %x", b, raw)
	}
}

// TestKeyPrefixString covers keyPrefix.String.
func TestKeyPrefixString(t *testing.T) {
	p := keyPrefix{0xab, 0xcd}
	if got := p.String(); got != "abcd" {
		t.Fatalf("keyPrefix.String = %q", got)
	}
}

// TestCloneBytes covers all three branches: nil, empty, populated (and
// verifies that the clone does not alias the source).
func TestCloneBytes(t *testing.T) {
	if got := CloneBytes(nil); got != nil {
		t.Fatalf("CloneBytes(nil) = %v", got)
	}
	if got := CloneBytes([]byte{}); got != nil {
		t.Fatalf("CloneBytes([]byte{}) = %v", got)
	}
	src := []byte{1, 2, 3}
	got := CloneBytes(src)
	if !bytes.Equal(got, src) {
		t.Fatalf("clone differs: %x vs %x", got, src)
	}
	got[0] = 9
	if src[0] == 9 {
		t.Fatal("clone aliases source storage")
	}
}

// TestPrefixForName covers the package-level PrefixForName lookup for an
// existing name (the first registered Table) and the unknown-name error path.
func TestPrefixForName(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()
	if _, err := db.Table("T"); err != nil {
		t.Fatalf("Table: %v", err)
	}
	p, err := PrefixForName(db.DB, "T")
	if err != nil {
		t.Fatalf("PrefixForName: %v", err)
	}
	if p != [PrefixSize]byte(firstAvailablePrefix) {
		t.Fatalf("PrefixForName = %v, want %v", p, firstAvailablePrefix)
	}
	if _, err := PrefixForName(db.DB, "no-such-table"); err == nil {
		t.Fatal("expected error for unknown name")
	}
}

// TestBadgerLoggerWrapper exercises the delegate methods of badgerLoggerWrapper
// (Debug, Debugf, Info, Infof, Warning, Warningf). They route to the embedded
// util.Logger, so capturing the writer is enough to confirm they're wired up.
func TestBadgerLoggerWrapper(t *testing.T) {
	var buf bytes.Buffer
	log := util.NewLogger("T", util.LevelTrace, &buf)
	w := &badgerLoggerWrapper{log}
	w.Debug("a", "b")
	w.Debugf("d=%d", 1)
	w.Info("c")
	w.Infof("i=%d", 2)
	w.Warning("warn")
	w.Warningf("w=%d", 3)
	if buf.Len() == 0 {
		t.Fatal("badgerLoggerWrapper produced no output")
	}
}

// TestConnect verifies that the GC-ticker goroutine started by Connect exits
// when its context is canceled and that the returned WaitGroup completes.
func TestConnect(t *testing.T) {
	db, _ := newTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	wg, err := db.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Do real work to confirm the DB is alive.
	tbl, err := db.Table("conn")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Connect goroutine did not exit after context cancel")
	}
}

// TestKeyID exercises (*DB).KeyID for an existing key and confirms repeated
// calls return the same id. KeyID is read-only, so an unknown key must fail.
func TestKeyID(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.Table("ids")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	k := []byte("the-key")
	if err := tbl.Set(k, &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	id1, err := db.KeyID(k)
	if err != nil {
		t.Fatalf("KeyID: %v", err)
	}
	id2, err := db.KeyID(k)
	if err != nil {
		t.Fatalf("KeyID again: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("KeyID returned different ids for same key: %v vs %v", id1, id2)
	}
	// A second key gets a distinct id.
	k2 := []byte("the-other-key")
	if err := tbl.Set(k2, &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set k2: %v", err)
	}
	id3, err := db.KeyID(k2)
	if err != nil {
		t.Fatalf("KeyID k2: %v", err)
	}
	if id3 == id1 {
		t.Fatalf("distinct keys produced the same id %v", id1)
	}
	// KeyID is read-only, so an unknown key must surface the badger error.
	if _, err := db.KeyID([]byte("not-there")); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

// TestParseKV exercises every type accepted by parseKV plus the unknown-type
// error.
func TestParseKV(t *testing.T) {
	// []byte
	if b, err := parseKV([]byte{1, 2, 3}); err != nil || !bytes.Equal(b, []byte{1, 2, 3}) {
		t.Fatalf("[]byte: got %x err=%v", b, err)
	}
	// uint32 -> 4-byte big-endian
	if b, err := parseKV(uint32(0x01020304)); err != nil || !bytes.Equal(b, []byte{1, 2, 3, 4}) {
		t.Fatalf("uint32: got %x err=%v", b, err)
	}
	// time.Time -> 8-byte big-endian unix-milli
	tm := time.UnixMilli(0x123456)
	b, err := parseKV(tm)
	if err != nil {
		t.Fatalf("time: %v", err)
	}
	if len(b) != 8 || binary.BigEndian.Uint64(b) != 0x123456 {
		t.Fatalf("time: got %x", b)
	}
	// BinaryMarshaler (DBID has a value-receiver MarshalBinary).
	dbID := DBID{0xaa}
	b, err = parseKV(dbID)
	if err != nil {
		t.Fatalf("BinaryMarshaler: %v", err)
	}
	if !bytes.Equal(b, dbID[:]) {
		t.Fatalf("BinaryMarshaler: got %x, want %x", b, dbID[:])
	}
	// nil
	if b, err := parseKV(nil); err != nil || b != nil {
		t.Fatalf("nil: got %x err=%v", b, err)
	}
	// Unknown type
	if _, err := parseKV(struct{ X int }{}); err == nil {
		t.Fatal("expected error for unknown type")
	}
}

// TestIterEntry covers Iter.Entry on both index and table iteration.
func TestIterEntry(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.Table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	idx, err := tbl.AddIndex("byVal", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}
	if err := tbl.Set([]byte("k1"), &tValue{v: []byte("v1"), idx: []byte{0x42}}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var indexEntry []byte
	if err := idx.Iterate(nil, func(it *Iter) error {
		return it.Entry(func(idxB []byte) error {
			indexEntry = append([]byte(nil), idxB...)
			return nil
		})
	}); err != nil {
		t.Fatalf("index Iterate: %v", err)
	}
	if !bytes.Equal(indexEntry, []byte{0x42}) {
		t.Fatalf("index Entry = %x, want 42", indexEntry)
	}

	// Table iteration's Entry returns the DBID bytes (DBIDSize long).
	var tableEntry []byte
	if err := tbl.Iterate(nil, func(it *Iter) error {
		return it.Entry(func(b []byte) error {
			tableEntry = append([]byte(nil), b...)
			return nil
		})
	}); err != nil {
		t.Fatalf("table Iterate: %v", err)
	}
	if len(tableEntry) != DBIDSize {
		t.Fatalf("table Entry length = %d, want %d", len(tableEntry), DBIDSize)
	}
}

// TestUniqueIndexConflictErrorMessage covers Error() on the package-internal
// uniqueIndexConflictError type.
func TestUniqueIndexConflictErrorMessage(t *testing.T) {
	e := uniqueIndexConflictError{indexName: "TheIndex"}
	if got := e.Error(); !strings.Contains(got, "TheIndex") {
		t.Fatalf("Error() = %q, does not name the index", got)
	}
}

// TestWithForward verifies WithForward clears a previously-set WithReverse.
func TestWithForward(t *testing.T) {
	var opts iteratorOpts
	WithReverse()(&opts)
	if !opts.reverse {
		t.Fatal("WithReverse did not set reverse")
	}
	WithForward()(&opts)
	if opts.reverse {
		t.Fatal("WithForward did not clear reverse")
	}
}

// TestUseDefaultSetOptions verifies table-level default Set options take effect.
func TestUseDefaultSetOptions(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.Table("X")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	tbl.UseDefaultSetOptions(WithReplace())
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v1")}); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	// Without UseDefaultSetOptions, the second Set would fail. With the
	// WithReplace default, it must succeed.
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v2")}); err != nil {
		t.Fatalf("default-replace Set: %v", err)
	}
	var got tValue
	if err := tbl.Get([]byte("k"), &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got.v, []byte("v2")) {
		t.Fatalf("after default-replace got %q", got.v)
	}
}

// TestUseDefaultIterationOptions exercises Index.UseDefaultIterationOptions
// and Table.UseDefaultIterationOptions.
func TestUseDefaultIterationOptions(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.Table("X")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	idx, err := tbl.AddIndex("byVal", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}
	for i := range 5 {
		b := byte(i)
		if err := tbl.Set([]byte{b}, &tValue{v: []byte{b}, idx: []byte{b}}); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	idx.UseDefaultIterationOptions(WithReverse())
	var first []byte
	if err := idx.Iterate(nil, func(it *Iter) error {
		return it.V(func(v []byte) error {
			first = append([]byte(nil), v...)
			return ErrEndIteration
		})
	}); err != nil {
		t.Fatalf("idx Iterate: %v", err)
	}
	if !bytes.Equal(first, []byte{4}) {
		t.Fatalf("default-reverse first value = %x, want 04", first)
	}

	tbl.UseDefaultIterationOptions(WithReverse())
	var seen int
	if err := tbl.Iterate(nil, func(it *Iter) error {
		seen++
		return nil
	}); err != nil {
		t.Fatalf("tbl Iterate: %v", err)
	}
	if seen != 5 {
		t.Fatalf("default-reverse table iteration saw %d, want 5", seen)
	}
}

// TestReIndexErrNotIndexed covers the ErrNotIndexed branch of ReIndex (which
// must purge any existing entries for this index).
func TestReIndexErrNotIndexed(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.Table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	idx, err := tbl.AddIndex("byVal", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}
	for i := range 5 {
		k := []byte{byte(i)}
		if err := tbl.Set(k, &tValue{v: []byte{byte(i)}, idx: []byte{byte(i)}}); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}
	// Sanity: 5 entries in the index.
	var n int
	idx.Iterate(nil, func(_ *Iter) error { n++; return nil })
	if n != 5 {
		t.Fatalf("pre-reindex count = %d, want 5", n)
	}
	// Re-index returning ErrNotIndexed for every row should empty the index.
	err = db.Upgrade(func() error {
		return db.ReIndex("T", "byVal", func(k, v []byte) ([]byte, error) {
			return nil, ErrNotIndexed
		})
	})
	if err != nil {
		t.Fatalf("ReIndex: %v", err)
	}
	n = 0
	idx.Iterate(nil, func(_ *Iter) error { n++; return nil })
	if n != 0 {
		t.Fatalf("post-reindex count = %d, want 0", n)
	}
}

// TestReIndexAddsForNewIndex covers the "no existing entry → append" branch of
// ReIndex (the common case when an index is added after data exists).
func TestReIndexAddsForNewIndex(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.Table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	// Insert data with no index.
	for i := range 3 {
		k := []byte{byte(i)}
		if err := tbl.Set(k, &tValue{v: []byte{byte(i)}, idx: []byte{byte(i)}}); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	// Add a fresh index after the data is in place; ReIndex must populate it.
	idx, err := tbl.AddIndex("byVal", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}
	err = db.Upgrade(func() error {
		return db.ReIndex("T", "byVal", func(k, v []byte) ([]byte, error) {
			return v, nil
		})
	})
	if err != nil {
		t.Fatalf("ReIndex: %v", err)
	}
	var n int
	idx.Iterate(nil, func(_ *Iter) error { n++; return nil })
	if n != 3 {
		t.Fatalf("reindex populated %d, want 3", n)
	}
}

// TestDeleteIndexUnknownTable: deleting an index of a non-existent table is a
// silent no-op.
func TestDeleteIndexUnknownTable(t *testing.T) {
	db, _ := newTestDB(t)
	if err := db.DeleteIndex("no-such-table", "no-such-index"); err != nil {
		t.Fatalf("DeleteIndex returned error: %v", err)
	}
}

// TestDeleteIndexUnknownIndex: known table, unknown index name is a silent
// no-op.
func TestDeleteIndexUnknownIndex(t *testing.T) {
	db, _ := newTestDB(t)
	if _, err := db.Table("T"); err != nil {
		t.Fatalf("Table: %v", err)
	}
	if err := db.DeleteIndex("T", "no-such-index"); err != nil {
		t.Fatalf("DeleteIndex returned error: %v", err)
	}
}

// TestReIndexUnknownTable / TestReIndexUnknownIndex: silent no-op.
func TestReIndexUnknownTable(t *testing.T) {
	db, _ := newTestDB(t)
	err := db.ReIndex("no-such-table", "no-idx", func(k, v []byte) ([]byte, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected nil for unknown table, got %v", err)
	}
}

func TestReIndexUnknownIndex(t *testing.T) {
	db, _ := newTestDB(t)
	if _, err := db.Table("T"); err != nil {
		t.Fatalf("Table: %v", err)
	}
	err := db.ReIndex("T", "no-idx", func(k, v []byte) ([]byte, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected nil for unknown index, got %v", err)
	}
}

// TestSetEmptyKey and TestSetWithoutReplace round out the table.Set error paths.
func TestSetEmptyKey(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("X")
	if err := tbl.Set([]byte{}, &tValue{v: []byte("v")}); err == nil {
		t.Fatal("expected error for zero-length key")
	}
}

func TestSetWithoutReplace(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("X")
	k := []byte("k")
	if err := tbl.Set(k, &tValue{v: []byte("v1")}); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if err := tbl.Set(k, &tValue{v: []byte("v2")}); err == nil {
		t.Fatal("expected error replacing without WithReplace")
	}
}

// TestDecodeDatumErrors covers the error paths in decodeDatum.
func TestDecodeDatumErrors(t *testing.T) {
	// Too short to contain a version + the minimum framing.
	if _, err := decodeDatum(nil); err == nil {
		t.Fatal("expected error for empty blob")
	}
	if _, err := decodeDatum([]byte{0, 1, 2}); err == nil {
		t.Fatal("expected length error for 3-byte blob")
	}
	// Unknown version (still long enough to pass the length check).
	if _, err := decodeDatum([]byte{0xff, 0, 0, 0}); err == nil {
		t.Fatal("expected unknown-version error")
	}
	// Claims one 100-byte index but provides almost no payload.
	if _, err := decodeDatum([]byte{0, 1, 100, 0}); err == nil {
		t.Fatal("expected truncated-index-read error")
	}
}

// TestDeleteMissingKey verifies Delete returns ErrKeyNotFound when the key
// doesn't exist.
func TestDeleteMissingKey(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("X")
	if err := tbl.Delete([]byte("nope")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

// TestGetMissingKey verifies Get returns ErrKeyNotFound when the key doesn't
// exist.
func TestGetMissingKey(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("X")
	var got tValue
	if err := tbl.Get([]byte("nope"), &got); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

// TestSetIndexFunctionError verifies a non-ErrNotIndexed error from an index
// function propagates from Set (the default branch of Set's index-error
// switch).
func TestSetIndexFunctionError(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("X")
	sentinel := errors.New("custom-index-error")
	if _, err := tbl.AddIndex("boom", func(_, _ KV) ([]byte, error) {
		return nil, sentinel
	}); err != nil {
		t.Fatalf("AddIndex: %v", err)
	}
	err := tbl.Set([]byte("k"), &tValue{v: []byte("v")})
	if err == nil {
		t.Fatal("expected error from Set when index function fails")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

// TestReIndexReplacesExistingEntry covers ReIndex's "delete old + replace"
// branch (the case where the datum already has an entry in this index and
// must be re-derived).
func TestReIndexReplacesExistingEntry(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("T")
	idx, _ := tbl.AddIndex("byVal", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v"), idx: []byte{1}}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// ReIndex with a different derivation: each entry's index must be replaced.
	err := db.Upgrade(func() error {
		return db.ReIndex("T", "byVal", func(k, v []byte) ([]byte, error) {
			return []byte{2}, nil
		})
	})
	if err != nil {
		t.Fatalf("ReIndex: %v", err)
	}
	var entry []byte
	idx.Iterate(nil, func(it *Iter) error {
		return it.Entry(func(b []byte) error {
			entry = append([]byte(nil), b...)
			return nil
		})
	})
	if !bytes.Equal(entry, []byte{2}) {
		t.Fatalf("post-reindex entry = %x, want 02", entry)
	}
}

// TestReIndexIndexerError covers the path where the user's reindex callback
// returns a non-ErrNotIndexed error.
func TestReIndexIndexerError(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("T")
	if _, err := tbl.AddIndex("byVal", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	}); err != nil {
		t.Fatalf("AddIndex: %v", err)
	}
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v"), idx: []byte{1}}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	sentinel := errors.New("indexer error")
	err := db.Upgrade(func() error {
		return db.ReIndex("T", "byVal", func(k, v []byte) ([]byte, error) {
			return nil, sentinel
		})
	})
	if err == nil {
		t.Fatal("expected ReIndex to surface indexer error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

// TestPrefixForNameBadSize covers the "unexpected prefix size" path in the
// package-level PrefixForName by injecting a mapping of the wrong length.
func TestPrefixForNameBadSize(t *testing.T) {
	db, _ := newTestDB(t)
	err := db.DB.Update(func(txn *badger.Txn) error {
		// Write a 1-byte value where PrefixForName expects PrefixSize bytes.
		return txn.Set(PrefixedKey(nameToPrefixPrefix, []byte("bad")), []byte{0x01})
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := PrefixForName(db.DB, "bad"); err == nil {
		t.Fatal("expected error for malformed prefix value")
	}
}

// TestIterateCallbackErrorForward verifies that a non-ErrEndIteration error
// returned from a forward-iteration callback is propagated.
func TestIterateCallbackErrorForward(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("X")
	idx, _ := tbl.AddIndex("byVal", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v"), idx: []byte{1}}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	sentinel := errors.New("cb error")
	if err := idx.Iterate(nil, func(it *Iter) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

// TestIterateCallbackErrorReverse mirrors TestIterateCallbackErrorForward for
// reverse iteration.
func TestIterateCallbackErrorReverse(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("X")
	idx, _ := tbl.AddIndex("byVal", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v"), idx: []byte{1}}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	sentinel := errors.New("cb error")
	err := idx.Iterate(nil, func(it *Iter) error { return sentinel }, WithReverse())
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

// TestSetNotIndexedSkipsEntry covers the ErrNotIndexed branch inside Index.add.
// When an index returns ErrNotIndexed for a datum, the datum is still stored
// but no index entry is created.
func TestSetNotIndexedSkipsEntry(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("X")
	idx, _ := tbl.AddIndex("evens", func(_, v KV) ([]byte, error) {
		val := v.(*tValue).v
		if len(val) > 0 && val[0]%2 == 0 {
			return val[:1], nil
		}
		return nil, ErrNotIndexed
	})
	for i := range 4 {
		if err := tbl.Set([]byte{byte(i)}, &tValue{v: []byte{byte(i)}}); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	var n int
	idx.Iterate(nil, func(_ *Iter) error { n++; return nil })
	if n != 2 {
		t.Fatalf("indexed count = %d, want 2 (evens only)", n)
	}
}

// TestDeleteIndexPreservesOtherIndexes covers the branch of DeleteIndex that
// rewrites each datum to retain references to the surviving indexes.
func TestDeleteIndexPreservesOtherIndexes(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("T")
	keep, err := tbl.AddIndex("keep", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err != nil {
		t.Fatalf("AddIndex keep: %v", err)
	}
	if _, err := tbl.AddIndex("drop", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	}); err != nil {
		t.Fatalf("AddIndex drop: %v", err)
	}
	for i := range 3 {
		if err := tbl.Set([]byte{byte(i)}, &tValue{v: []byte{byte(i)}, idx: []byte{byte(i)}}); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	if err := db.DeleteIndex("T", "drop"); err != nil {
		t.Fatalf("DeleteIndex: %v", err)
	}
	// The surviving "keep" index must still resolve all rows.
	var n int
	keep.Iterate(nil, func(_ *Iter) error { n++; return nil })
	if n != 3 {
		t.Fatalf("kept index count = %d, want 3", n)
	}
	// And the underlying data must still be readable.
	for i := range 3 {
		var got tValue
		if err := tbl.Get([]byte{byte(i)}, &got); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		if !bytes.Equal(got.v, []byte{byte(i)}) {
			t.Fatalf("Get %d = %x, want %02x", i, got.v, i)
		}
	}
}

// TestUniqueIndexConflictAsError verifies errors.As works against the
// internal uniqueIndexConflictError type that Set wraps inside its switch.
func TestUniqueIndexConflictAsError(t *testing.T) {
	var src error = uniqueIndexConflictError{indexName: "X", conflictDBID: DBID{0xab}}
	var target uniqueIndexConflictError
	if !errors.As(src, &target) {
		t.Fatal("errors.As did not match uniqueIndexConflictError")
	}
	if target.indexName != "X" {
		t.Fatalf("target.indexName = %q", target.indexName)
	}
}

// TestIterDatumDecodeError plants a corrupted datum value at the table's
// internal prefix and verifies that Iter.V (via the lazy Iter.datum lookup)
// surfaces the decode error rather than returning silently.
func TestIterDatumDecodeError(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.Table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	dbID, err := db.KeyID([]byte("k"))
	if err != nil {
		t.Fatalf("KeyID: %v", err)
	}
	// Overwrite the datum row with a value too short to decode.
	err = db.DB.Update(func(txn *badger.Txn) error {
		return txn.Set(PrefixedKey(tbl.prefix, dbID[:]), []byte{0xff})
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	err = tbl.Iterate(nil, func(it *Iter) error {
		return it.V(func(v []byte) error { return nil })
	})
	if err == nil {
		t.Fatal("expected decode error from Iter.V on corrupt datum")
	}
}

// TestSetReplaceCorruptOldDatum forces the decode-error path in Set when an
// existing datum's bytes don't parse during a WithReplace operation.
func TestSetReplaceCorruptOldDatum(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.Table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	dbID, err := db.KeyID([]byte("k"))
	if err != nil {
		t.Fatalf("KeyID: %v", err)
	}
	err = db.DB.Update(func(txn *badger.Txn) error {
		return txn.Set(PrefixedKey(tbl.prefix, dbID[:]), []byte{0xff})
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	err = tbl.Set([]byte("k"), &tValue{v: []byte("v2")}, WithReplace())
	if err == nil {
		t.Fatal("expected decode error from replace of corrupt entry")
	}
}

// TestDeleteCorruptDatum forces the decode-error path inside removeTableEntry
// when Delete encounters a malformed datum row.
func TestDeleteCorruptDatum(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.Table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	dbID, err := db.KeyID([]byte("k"))
	if err != nil {
		t.Fatalf("KeyID: %v", err)
	}
	err = db.DB.Update(func(txn *badger.Txn) error {
		return txn.Set(PrefixedKey(tbl.prefix, dbID[:]), []byte{0xff})
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := tbl.Delete([]byte("k")); err == nil {
		t.Fatal("expected decode error from Delete on corrupt datum")
	}
}

// TestDecodeDatumReadErrors covers the remaining wire.ReadVarInt error
// branches in decodeDatum — index-count, index-length, and value-length read
// errors triggered by a truncated multi-byte varint marker.
func TestDecodeDatumReadErrors(t *testing.T) {
	// 0xff is a uint64 varint marker that requires 8 trailing bytes.

	// Index-count varint truncated.
	if _, err := decodeDatum([]byte{0, 0xff, 0, 0}); err == nil {
		t.Fatal("expected nIndexes read error")
	}
	// Index-length varint truncated (nIndexes=1, then 0xff with too few bytes).
	if _, err := decodeDatum([]byte{0, 1, 0xff, 0}); err == nil {
		t.Fatal("expected indexLen read error")
	}
	// Value-length varint truncated (nIndexes=0, then 0xff with too few bytes).
	if _, err := decodeDatum([]byte{0, 0, 0xff, 0}); err == nil {
		t.Fatal("expected valueLen read error")
	}
}

// TestGetRawWithTxn covers the WithGetTxn branch of GetRaw, distinct from the
// existing TestTransactionOptions which exclusively uses Get.
func TestGetRawWithTxn(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.Table("X")
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	err := db.View(func(txn *badger.Txn) error {
		raw, err := tbl.GetRaw([]byte("k"), WithGetTxn(txn))
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, []byte("v")) {
			t.Fatalf("GetRaw = %q, want v", raw)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
}
