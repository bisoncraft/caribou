// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
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

// TestKeyIDStringAndMarshal covers KeyID.String, KeyID.MarshalBinary, and
// newKeyIDFromBytes.
func TestKeyIDStringAndMarshal(t *testing.T) {
	raw := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	keyID := newKeyIDFromBytes(raw)
	if got := keyID.String(); got != "0001020304050607" {
		t.Fatalf("KeyID.String = %q", got)
	}
	b, err := keyID.MarshalBinary()
	if err != nil {
		t.Fatalf("KeyID.MarshalBinary: %v", err)
	}
	if !bytes.Equal(b, raw) {
		t.Fatalf("KeyID.MarshalBinary = %x, want %x", b, raw)
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
	tbl, err := db.table("conn")
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

// TestKeyID exercises (*Table).KeyID for an existing key and confirms
// repeated calls return the same id. KeyID is read-only, so an unknown key
// must fail.
func TestKeyID(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.table("ids")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	k := []byte("the-key")
	if err := tbl.Set(k, &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	id1, err := tbl.KeyID(k)
	if err != nil {
		t.Fatalf("KeyID: %v", err)
	}
	id2, err := tbl.KeyID(k)
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
	id3, err := tbl.KeyID(k2)
	if err != nil {
		t.Fatalf("KeyID k2: %v", err)
	}
	if id3 == id1 {
		t.Fatalf("distinct keys produced the same id %v", id1)
	}
	// KeyID is read-only, so an unknown key must fail, with badger's error
	// translated to the package sentinel.
	if _, err := tbl.KeyID([]byte("not-there")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("unknown-key KeyID error = %v, want ErrKeyNotFound", err)
	}
}

// TestParseKV exercises every type accepted by parseKV plus the unknown-type
// error.
func TestParseKV(t *testing.T) {
	// []byte
	if b, err := kvstore.ParseKV([]byte{1, 2, 3}); err != nil || !bytes.Equal(b, []byte{1, 2, 3}) {
		t.Fatalf("[]byte: got %x err=%v", b, err)
	}
	// uint32 -> 4-byte big-endian
	if b, err := kvstore.ParseKV(uint32(0x01020304)); err != nil || !bytes.Equal(b, []byte{1, 2, 3, 4}) {
		t.Fatalf("uint32: got %x err=%v", b, err)
	}
	// time.Time -> 8-byte big-endian unix-milli
	tm := time.UnixMilli(0x123456)
	b, err := kvstore.ParseKV(tm)
	if err != nil {
		t.Fatalf("time: %v", err)
	}
	if len(b) != 8 || binary.BigEndian.Uint64(b) != 0x123456 {
		t.Fatalf("time: got %x", b)
	}
	// BinaryMarshaler (KeyID has a value-receiver MarshalBinary).
	keyID := KeyID{0xaa}
	b, err = kvstore.ParseKV(keyID)
	if err != nil {
		t.Fatalf("BinaryMarshaler: %v", err)
	}
	if !bytes.Equal(b, keyID[:]) {
		t.Fatalf("BinaryMarshaler: got %x, want %x", b, keyID[:])
	}
	// nil
	if b, err := kvstore.ParseKV(nil); err != nil || b != nil {
		t.Fatalf("nil: got %x err=%v", b, err)
	}
	// Unknown type
	if _, err := kvstore.ParseKV(struct{ X int }{}); err == nil {
		t.Fatal("expected error for unknown type")
	}
}

// TestIterEntry covers Iter.Entry on both index and table iteration.
func TestIterEntry(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.table("T")
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
	if err := idx.Iterate(nil, func(it kvstore.StoreIter) error {
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

	// Table iteration's Entry yields the primary key, which is what the row
	// is stored under.
	var tableEntry []byte
	if err := tbl.Iterate(nil, func(it kvstore.StoreIter) error {
		return it.Entry(func(b []byte) error {
			tableEntry = append([]byte(nil), b...)
			return nil
		})
	}); err != nil {
		t.Fatalf("table Iterate: %v", err)
	}
	if !bytes.Equal(tableEntry, []byte("k1")) {
		t.Fatalf("table Entry = %q, want %q", tableEntry, "k1")
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

// TestSetEmptyKey and TestSetWithoutReplace round out the table.Set error paths.
func TestSetEmptyKey(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.table("X")
	if err := tbl.Set([]byte{}, &tValue{v: []byte("v")}); err == nil {
		t.Fatal("expected error for zero-length key")
	}
}

func TestSetWithoutReplace(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.table("X")
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
	// A datum is a version byte, a KeyID, and then the framed payload.
	frame := func(payload ...byte) []byte {
		return append(append([]byte{0}, make([]byte, KeyIDSize)...), payload...)
	}
	// Too short to contain a version + KeyID + the minimum framing.
	if _, err := decodeDatum(nil); err == nil {
		t.Fatal("expected error for empty blob")
	}
	if _, err := decodeDatum(make([]byte, KeyIDSize+2)); err == nil {
		t.Fatal("expected length error for truncated blob")
	}
	// Unknown version (still long enough to pass the length check).
	badVersion := frame(0, 0)
	badVersion[0] = 0xff
	if _, err := decodeDatum(badVersion); err == nil {
		t.Fatal("expected unknown-version error")
	}
	// Claims one 100-byte index but provides almost no payload.
	if _, err := decodeDatum(frame(1, 100, 0)); err == nil {
		t.Fatal("expected truncated-index-read error")
	}
}

// TestDeleteMissingKey verifies Delete returns ErrKeyNotFound when the key
// doesn't exist.
func TestDeleteMissingKey(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.table("X")
	if err := tbl.Delete([]byte("nope")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

// TestGetMissingKey verifies Get returns ErrKeyNotFound when the key doesn't
// exist.
func TestGetMissingKey(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.table("X")
	if _, err := tbl.Get([]byte("nope")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

// TestSetIndexFunctionError verifies a non-ErrNotIndexed error from an index
// function propagates from Set (the default branch of Set's index-error
// switch).
func TestSetIndexFunctionError(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.table("X")
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

// TestIterateCallbackErrorForward verifies that a non-ErrEndIteration error
// returned from a forward-iteration callback is propagated.
func TestIterateCallbackErrorForward(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.table("X")
	idx, _ := tbl.AddIndex("byVal", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v"), idx: []byte{1}}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	sentinel := errors.New("cb error")
	if err := idx.Iterate(nil, func(it kvstore.StoreIter) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

// TestIterateCallbackErrorReverse mirrors TestIterateCallbackErrorForward for
// reverse iteration.
func TestIterateCallbackErrorReverse(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.table("X")
	idx, _ := tbl.AddIndex("byVal", func(_, v KV) ([]byte, error) {
		return v.(*tValue).idx, nil
	})
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v"), idx: []byte{1}}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	sentinel := errors.New("cb error")
	err := idx.Iterate(nil, func(it kvstore.StoreIter) error { return sentinel }, kvstore.WithReverse())
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

// TestSetNotIndexedSkipsEntry covers the ErrNotIndexed branch inside Index.add.
// When an index returns ErrNotIndexed for a datum, the datum is still stored
// but no index entry is created.
func TestSetNotIndexedSkipsEntry(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.table("X")
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
	idx.Iterate(nil, func(_ kvstore.StoreIter) error { n++; return nil })
	if n != 2 {
		t.Fatalf("indexed count = %d, want 2 (evens only)", n)
	}
}

// TestUniqueIndexConflictAsError verifies errors.As works against the
// internal uniqueIndexConflictError type that Set wraps inside its switch.
func TestUniqueIndexConflictAsError(t *testing.T) {
	var src error = uniqueIndexConflictError{indexName: "X", conflictKeyID: KeyID{0xab}}
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
	tbl, err := db.table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Overwrite the datum row with a value too short to decode.
	err = db.DB.Update(func(txn *badger.Txn) error {
		return txn.Set(PrefixedKey(tbl.prefix, []byte("k")), []byte{0xff})
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	err = tbl.Iterate(nil, func(it kvstore.StoreIter) error {
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
	tbl, err := db.table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	err = db.DB.Update(func(txn *badger.Txn) error {
		return txn.Set(PrefixedKey(tbl.prefix, []byte("k")), []byte{0xff})
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	err = tbl.Set([]byte("k"), &tValue{v: []byte("v2")}, kvstore.WithReplace())
	if err == nil {
		t.Fatal("expected decode error from replace of corrupt entry")
	}
}

// TestDeleteCorruptDatum forces the decode-error path in Delete's row lookup
// when it encounters a malformed datum row.
func TestDeleteCorruptDatum(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	err = db.DB.Update(func(txn *badger.Txn) error {
		return txn.Set(PrefixedKey(tbl.prefix, []byte("k")), []byte{0xff})
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
	// 0xff is a uint64 varint marker that requires 8 trailing bytes. The
	// framed payload follows the version byte and the KeyID.
	frame := func(payload ...byte) []byte {
		return append(append([]byte{0}, make([]byte, KeyIDSize)...), payload...)
	}

	// Index-count varint truncated.
	if _, err := decodeDatum(frame(0xff, 0, 0)); err == nil {
		t.Fatal("expected nIndexes read error")
	}
	// Index-length varint truncated (nIndexes=1, then 0xff with too few bytes).
	if _, err := decodeDatum(frame(1, 0xff, 0)); err == nil {
		t.Fatal("expected indexLen read error")
	}
	// Value-length varint truncated (nIndexes=0, then 0xff with too few bytes).
	if _, err := decodeDatum(frame(0, 0xff, 0)); err == nil {
		t.Fatal("expected valueLen read error")
	}
}

// TestGetWithTxn covers the WithGetTxn branch of Get inside a read-only
// transaction, distinct from TestTransactionOptions' read-write transaction.
func TestGetWithTxn(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, _ := db.table("X")
	if err := tbl.Set([]byte("k"), &tValue{v: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	err := db.View(func(txn *badger.Txn) error {
		raw, err := tbl.Get([]byte("k"), WithGetTxn(txn))
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, []byte("v")) {
			t.Fatalf("Get = %q, want v", raw)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestTableIterateDeleteDuringIteration covers Iter.Delete under WithUpdate on
// a table.
//
// Table iteration walks the rows themselves, and Iter.Delete removes the very
// row the cursor sits on, so the cursor is mutating the range it is walking.
// Badger iterates a consistent snapshot, which is what makes this safe; the
// test pins that, since a cursor that skipped or repeated entries here would
// silently leave rows behind on any bulk delete.
func TestTableIterateDeleteDuringIteration(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	for _, k := range []string{"a", "b", "c", "d"} {
		if err := tbl.Set([]byte(k), &tValue{v: []byte("v")}, kvstore.WithReplace()); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	collect := func(opts ...kvstore.IterationOption) []string {
		t.Helper()
		var got []string
		if err := tbl.Iterate(nil, func(it kvstore.StoreIter) error {
			k, err := it.K()
			if err != nil {
				return err
			}
			got = append(got, string(k))
			if s := string(k); s == "b" || s == "c" {
				if len(opts) > 0 {
					return it.Delete()
				}
			}
			return nil
		}, opts...); err != nil {
			t.Fatalf("Iterate: %v", err)
		}
		return got
	}

	// Every key is visited even though two are deleted mid-walk.
	if got := collect(kvstore.WithUpdate()); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("visited %v, want [a b c d]", got)
	}
	if got := collect(); !slices.Equal(got, []string{"a", "d"}) {
		t.Fatalf("remaining %v, want [a d]", got)
	}
	if _, err := tbl.Get([]byte("b")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get of deleted key = %v, want ErrKeyNotFound", err)
	}
}

// TestWrongTypeTxnOption covers resolveTxn's rejection of a transaction
// handle that is not a *badger.Txn.
func TestWrongTypeTxnOption(t *testing.T) {
	db, _ := newTestDB(t)
	tbl, err := db.table("X")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	badSet := func(o *kvstore.SetOpts) { o.Txn = 42 }
	if err := tbl.Set([]byte("k"), []byte("v"), badSet); err == nil {
		t.Fatal("Set with wrong-typed txn handle succeeded")
	}
	badGet := func(o *kvstore.GetOpts) { o.Txn = 42 }
	if _, err := tbl.Get([]byte("k"), badGet); err == nil {
		t.Fatal("Get with wrong-typed txn handle succeeded")
	}
}

// seekerTestIndex builds a table with an index on the value's first byte,
// skipping values whose first byte is 0xff.
func seekerTestIndex(t *testing.T, db *DB) (*Table, kvstore.StoreIndex) {
	t.Helper()
	tbl, err := db.table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	idx, err := tbl.AddIndex("byVal", func(_, v kvstore.KV) ([]byte, error) {
		b := v.([]byte)
		if b[0] == 0xff {
			return nil, ErrNotIndexed
		}
		return b[:1], nil
	})
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}
	return tbl, idx
}

// TestSeeker covers Index.Seeker: the returned seek is the row's index entry
// followed by its KeyID — the layout the reverse-iteration lastKeyID padding
// relies on to make the seek inclusive in both directions — it tracks the row
// through replacement, and a missing or unindexed row reports ErrKeyNotFound.
func TestSeeker(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()
	tbl, idx := seekerTestIndex(t, db)

	set := func(k string, val byte) {
		t.Helper()
		if err := tbl.Set([]byte(k), []byte{val}, kvstore.WithReplace()); err != nil {
			t.Fatalf("Set(%q): %v", k, err)
		}
	}
	set("a", 1)
	set("b", 2)
	set("c", 2) // "b" and "c" share the index value
	set("x", 0xff)

	seek, err := idx.Seeker([]byte("c"))
	if err != nil {
		t.Fatalf("Seeker: %v", err)
	}
	if len(seek) != 1+KeyIDSize || seek[0] != 2 {
		t.Fatalf("seek = %x, want entry byte 0x02 + %d-byte KeyID", seek, KeyIDSize)
	}
	keyID, err := tbl.KeyID([]byte("c"))
	if err != nil {
		t.Fatalf("KeyID: %v", err)
	}
	if !bytes.Equal(seek[1:], keyID[:]) {
		t.Fatalf("seek KeyID suffix = %x, want %x", seek[1:], keyID[:])
	}

	collect := func(opts ...kvstore.IterationOption) (keys []string) {
		t.Helper()
		if err := idx.Iterate(nil, func(it kvstore.StoreIter) error {
			k, err := it.K()
			if err != nil {
				return err
			}
			keys = append(keys, string(k))
			return nil
		}, opts...); err != nil {
			t.Fatalf("Iterate: %v", err)
		}
		return
	}
	if got := collect(kvstore.WithSeek(seek)); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("forward from c = %v, want [c]", got)
	}
	if got := collect(kvstore.WithReverse(), kvstore.WithSeek(seek)); !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Fatalf("reverse from c = %v, want [c b a]", got)
	}

	// Replacing the row moves its index entry (it keeps its KeyID); Seeker
	// returns the new position.
	set("c", 1) // order is now a(1), c(1), b(2)
	seek, err = idx.Seeker([]byte("c"))
	if err != nil {
		t.Fatalf("Seeker after replace: %v", err)
	}
	if got := collect(kvstore.WithSeek(seek)); !slices.Equal(got, []string{"c", "b"}) {
		t.Fatalf("forward from replaced c = %v, want [c b]", got)
	}

	if _, err := idx.Seeker([]byte("nope")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Seeker(missing key) err = %v, want ErrKeyNotFound", err)
	}
	if _, err := idx.Seeker([]byte("x")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Seeker(unindexed row) err = %v, want ErrKeyNotFound", err)
	}
}

// TestCrossIndex covers Iter.CrossIndex from both an index scan and a table
// scan: another index's entry for the current datum is readable (KeyID suffix
// excluded), a datum absent from the named index reports ErrKeyNotFound, and
// an unknown index name is a distinct error.
func TestCrossIndex(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()
	tbl, err := db.table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	byFirst, err := tbl.AddIndex("byFirst", func(_, v kvstore.KV) ([]byte, error) {
		return v.([]byte)[:1], nil
	})
	if err != nil {
		t.Fatalf("AddIndex(byFirst): %v", err)
	}
	if _, err := tbl.AddIndex("bySecond", func(_, v kvstore.KV) ([]byte, error) {
		b := v.([]byte)
		if b[1] == 0xff {
			return nil, ErrNotIndexed
		}
		return b[1:2], nil
	}); err != nil {
		t.Fatalf("AddIndex(bySecond): %v", err)
	}

	if err := tbl.Set([]byte("a"), []byte{1, 9}); err != nil {
		t.Fatalf("Set(a): %v", err)
	}
	if err := tbl.Set([]byte("b"), []byte{2, 0xff}); err != nil { // absent from bySecond
		t.Fatalf("Set(b): %v", err)
	}

	checkIter := func(scan string, it kvstore.StoreIter) error {
		k, err := it.K()
		if err != nil {
			return err
		}
		switch string(k) {
		case "a":
			if e, err := it.CrossIndex("bySecond"); err != nil || !bytes.Equal(e, []byte{9}) {
				t.Fatalf("%s: CrossIndex(bySecond) = %x, %v; want 0x09", scan, e, err)
			}
			// The iterated index's own entry is readable too.
			if e, err := it.CrossIndex("byFirst"); err != nil || !bytes.Equal(e, []byte{1}) {
				t.Fatalf("%s: CrossIndex(byFirst) = %x, %v; want 0x01", scan, e, err)
			}
			if _, err := it.CrossIndex("nope"); err == nil || errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("%s: CrossIndex(unknown name) err = %v, want a non-not-found error", scan, err)
			}
		case "b":
			if _, err := it.CrossIndex("bySecond"); !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("%s: CrossIndex of unindexed datum err = %v, want ErrKeyNotFound", scan, err)
			}
		}
		return nil
	}
	if err := byFirst.Iterate(nil, func(it kvstore.StoreIter) error {
		return checkIter("index scan", it)
	}); err != nil {
		t.Fatalf("index Iterate: %v", err)
	}
	if err := tbl.Iterate(nil, func(it kvstore.StoreIter) error {
		return checkIter("table scan", it)
	}); err != nil {
		t.Fatalf("table Iterate: %v", err)
	}
}

// TestSeekerCrossIndexMalformedEntry pins the malformed-entry guard shared by
// Seeker and CrossIndex: a stored index entry too short to carry a key prefix
// and KeyID is refused with an error rather than sliced out of range.
func TestSeekerCrossIndexMalformedEntry(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()
	tbl, idx := seekerTestIndex(t, db)

	kB := []byte("k")
	if err := tbl.Set(kB, []byte{1}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	keyID, err := tbl.KeyID(kB)
	if err != nil {
		t.Fatalf("KeyID: %v", err)
	}
	// Rewrite the row's datum with an index-entry blob shorter than
	// PrefixSize+KeyIDSize.
	corrupt := &datum{keyID: keyID, indexes: [][]byte{{0x01}}, v: []byte{1}}
	raw, err := corrupt.bytes()
	if err != nil {
		t.Fatalf("datum bytes: %v", err)
	}
	err = db.DB.Update(func(txn *badger.Txn) error {
		return txn.Set(PrefixedKey(tbl.prefix, kB), raw)
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}

	if _, err := idx.Seeker(kB); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("Seeker on corrupt datum err = %v, want malformed-entry error", err)
	}
	err = tbl.Iterate(nil, func(it kvstore.StoreIter) error {
		_, err := it.CrossIndex("byVal")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("CrossIndex on corrupt datum err = %v, want malformed-entry error", err)
	}
}
