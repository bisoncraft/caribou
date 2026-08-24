// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
)

// These run against whichever engine the build selects, so `go test` covers
// Badger (util/lexi) and `go test -tags mobile` covers bbolt (util/glossi).
// The behavior asserted here is the kvstore.Store contract itself, which both engines
// have to honor identically or a database built on kvstore behaves differently
// depending on the platform it shipped to.

func newTestStore(t *testing.T) kvstore.Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "db"), util.StdOutLogger("kvstore_TEST", util.LevelError))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func mustSet(t *testing.T, tbl kvstore.StoreTable, k, v string) {
	t.Helper()
	if err := tbl.Set([]byte(k), []byte(v), kvstore.WithReplace()); err != nil {
		t.Fatalf("Set(%q): %v", k, err)
	}
}

// collectKeys iterates and returns the keys visited, in visitation order.
func collectKeys(t *testing.T, it func(func(kvstore.StoreIter) error) error) []string {
	t.Helper()
	var got []string
	if err := it(func(i kvstore.StoreIter) error {
		k, err := i.K()
		if err != nil {
			return err
		}
		got = append(got, string(k))
		return nil
	}); err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	return got
}

func eq(a []string, b ...string) bool {
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

// TestTableIterateContract pins the documented behavior of kvstore.StoreTable.Iterate:
// primary keys, in key order, constrained by prefix, optionally started at seek.
//
// A caller that asks for a prefix and silently receives nothing has no way to
// tell that from an empty table, so a violation here is a wrong answer rather
// than an error. Keys are inserted out of lexical order so that insertion order
// cannot pass for key order.
func TestTableIterateContract(t *testing.T) {
	tbl, err := newTestStore(t).Table("t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	for _, k := range []string{"bb1", "aa2", "aa1", "cc1"} {
		mustSet(t, tbl, k, "v")
	}

	forward := func(prefix, seek string, reverse bool) []string {
		var p, s []byte
		if prefix != "" {
			p = []byte(prefix)
		}
		if seek != "" {
			s = []byte(seek)
		}
		return collectKeys(t, func(f func(kvstore.StoreIter) error) error {
			var opts []kvstore.IterationOption
			if reverse {
				opts = append(opts, kvstore.WithReverse())
			}
			if len(s) > 0 {
				opts = append(opts, kvstore.WithSeek(s))
			}
			return tbl.Iterate(p, f, opts...)
		})
	}

	if got := forward("", "", false); !eq(got, "aa1", "aa2", "bb1", "cc1") {
		t.Errorf("full forward scan = %v, want key order [aa1 aa2 bb1 cc1]", got)
	}
	if got := forward("", "", true); !eq(got, "cc1", "bb1", "aa2", "aa1") {
		t.Errorf("full reverse scan = %v, want [cc1 bb1 aa2 aa1]", got)
	}
	if got := forward("aa", "", false); !eq(got, "aa1", "aa2") {
		t.Errorf("prefix scan = %v, want [aa1 aa2]", got)
	}
	if got := forward("", "bb1", false); !eq(got, "bb1", "cc1") {
		t.Errorf("seek scan = %v, want [bb1 cc1]", got)
	}
	if got := forward("zz", "", false); len(got) != 0 {
		t.Errorf("non-matching prefix scan = %v, want none", got)
	}

	// Combinations of prefix, seek and direction. These are where the two
	// engines are most likely to drift apart, since each reaches its starting
	// position differently: lexi seeks past a trailing KeyID, glossi steps back
	// from a computed prefix upper bound.
	if got := forward("", "bb1", true); !eq(got, "bb1", "aa2", "aa1") {
		t.Errorf("reverse from seek = %v, want [bb1 aa2 aa1]", got)
	}
	if got := forward("aa", "", true); !eq(got, "aa2", "aa1") {
		t.Errorf("reverse within prefix = %v, want [aa2 aa1]", got)
	}
	if got := forward("aa", "aa2", true); !eq(got, "aa2", "aa1") {
		t.Errorf("reverse within prefix from seek = %v, want [aa2 aa1]", got)
	}
	if got := forward("aa", "aa2", false); !eq(got, "aa2") {
		t.Errorf("forward within prefix from seek = %v, want [aa2]", got)
	}
	if got := forward("", "zz", false); len(got) != 0 {
		t.Errorf("forward seek past the last key = %v, want none", got)
	}
}

// TestTablesDoNotShareKeys asserts that tables are independent key spaces:
// deleting a key from one table must not disturb the same key in another.
//
// This is the shape wallet/asset/sol relies on. Its native wallet and each of
// its token wallets get separately-named meta tables but store the same key
// strings ("beforeCursor", "backfillUntil"), and clearing one is a Delete. If
// the engine keys anything shared on the bare user key, one wallet finishing a
// backfill silently destroys another wallet's scan cursor, and the loss shows
// up later as wrong transaction history rather than as an error.
func TestTablesDoNotShareKeys(t *testing.T) {
	s := newTestStore(t)
	native, err := s.Table("net501_meta")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	token, err := s.Table("net501_meta_token1001")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}

	const k = "beforeCursor"
	mustSet(t, native, k, "native-sig")
	mustSet(t, token, k, "token-sig")

	if err := token.Delete([]byte(k)); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	b, err := native.Get([]byte(k))
	if err != nil {
		t.Fatalf("native Get after unrelated table's Delete: %v", err)
	}
	if string(b) != "native-sig" {
		t.Fatalf("native value = %q, want %q", b, "native-sig")
	}
	if _, err := token.Get([]byte(k)); !errors.Is(err, kvstore.ErrKeyNotFound) {
		t.Fatalf("token Get after Delete = %v, want kvstore.ErrKeyNotFound", err)
	}

	// The native row must still be reachable by iteration, with its key intact.
	// A row whose key mapping was torn down survives physically but iterates
	// with a failing K().
	if got := collectKeys(t, func(f func(kvstore.StoreIter) error) error {
		return native.Iterate(nil, f)
	}); !eq(got, k) {
		t.Fatalf("native iteration = %v, want [%s]", got, k)
	}
}

// TestTableKeysAreIndependentPerTable is the write-side counterpart: the same
// key in two tables must address two distinct rows.
func TestTableKeysAreIndependentPerTable(t *testing.T) {
	s := newTestStore(t)
	a, err := s.Table("a")
	if err != nil {
		t.Fatalf("Table a: %v", err)
	}
	b, err := s.Table("b")
	if err != nil {
		t.Fatalf("Table b: %v", err)
	}

	mustSet(t, a, "k", "in-a")
	mustSet(t, b, "k", "in-b")
	// Rewriting one must not disturb the other.
	mustSet(t, a, "k", "in-a-again")

	for _, c := range []struct {
		tbl  kvstore.StoreTable
		name string
		want string
	}{{a, "a", "in-a-again"}, {b, "b", "in-b"}} {
		got, err := c.tbl.Get([]byte("k"))
		if err != nil {
			t.Fatalf("table %s Get: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("table %s value = %q, want %q", c.name, got, c.want)
		}
	}
}

// contractRecord is a typed value with its own binary encoding, standing in
// for the typed records (txdb's txRecord, wallet/db's noteRecord) whose index
// functions depend on receiving the caller's original KV.
type contractRecord struct {
	stamp byte
	body  string
}

func (r *contractRecord) MarshalBinary() ([]byte, error) {
	return append([]byte{r.stamp}, r.body...), nil
}

// TestIndexTypedKVContract pins the central typed-insertion contract: an
// IndexFunc receives the caller's original (unparsed) key and value, so it
// can type-assert the value rather than decode stored bytes, and the stored
// value is the MarshalBinary encoding. Every wallet/txdb and wallet/db write
// depends on this holding identically on both engines.
func TestIndexTypedKVContract(t *testing.T) {
	tbl, err := newTestStore(t).Table("t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	idx, err := tbl.AddIndex("byStamp", func(k, v kvstore.KV) ([]byte, error) {
		if _, ok := k.([]byte); !ok {
			return nil, errors.New("key did not arrive as the original []byte")
		}
		rec, ok := v.(*contractRecord)
		if !ok {
			return nil, errors.New("value did not arrive as the original *contractRecord")
		}
		if rec.stamp == 0xff {
			return nil, kvstore.ErrNotIndexed
		}
		return []byte{rec.stamp}, nil
	})
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}

	for _, r := range []*contractRecord{
		{stamp: 2, body: "two"},
		{stamp: 1, body: "one"},
		{stamp: 0xff, body: "unindexed"},
	} {
		if err := tbl.Set([]byte(r.body), r, kvstore.WithReplace()); err != nil {
			t.Fatalf("Set(%q): %v", r.body, err)
		}
	}

	// Index iteration must visit the indexed records in stamp order, exposing
	// the primary key, the index entry, and the MarshalBinary-encoded value.
	var keys, entries, values []string
	err = idx.Iterate(nil, func(it kvstore.StoreIter) error {
		k, err := it.K()
		if err != nil {
			return err
		}
		keys = append(keys, string(k))
		if err := it.Entry(func(e []byte) error {
			entries = append(entries, string(e))
			return nil
		}); err != nil {
			return err
		}
		return it.V(func(v []byte) error {
			values = append(values, string(v))
			return nil
		})
	})
	if err != nil {
		t.Fatalf("index Iterate: %v", err)
	}
	if !eq(keys, "one", "two") {
		t.Fatalf("index keys = %v, want [one two] (ErrNotIndexed row excluded, stamp order)", keys)
	}
	if !eq(entries, "\x01", "\x02") {
		t.Fatalf("index entries = %q, want the stamp bytes", entries)
	}
	if !eq(values, "\x01one", "\x02two") {
		t.Fatalf("index values = %q, want the MarshalBinary encodings", values)
	}
}

// TestUniqueIndexContract pins unique-index semantics: a second key mapping
// to the same index entry is rejected without WithReplace, and with it the
// conflicting datum (a different primary key) is replaced.
func TestUniqueIndexContract(t *testing.T) {
	tbl, err := newTestStore(t).Table("t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	idx, err := tbl.AddUniqueIndex("byStamp", func(_, v kvstore.KV) ([]byte, error) {
		return []byte{v.(*contractRecord).stamp}, nil
	})
	if err != nil {
		t.Fatalf("AddUniqueIndex: %v", err)
	}

	if err := tbl.Set([]byte("a"), &contractRecord{stamp: 7, body: "a"}); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if err := tbl.Set([]byte("b"), &contractRecord{stamp: 7, body: "b"}); err == nil {
		t.Fatal("conflicting Set without WithReplace succeeded")
	}
	if err := tbl.Set([]byte("b"), &contractRecord{stamp: 7, body: "b"}, kvstore.WithReplace()); err != nil {
		t.Fatalf("conflicting Set with WithReplace: %v", err)
	}
	// The conflicting datum "a" must be gone, leaving only "b".
	if _, err := tbl.Get([]byte("a")); !errors.Is(err, kvstore.ErrKeyNotFound) {
		t.Fatalf("replaced datum Get = %v, want ErrKeyNotFound", err)
	}
	var n int
	if err := idx.Iterate(nil, func(kvstore.StoreIter) error { n++; return nil }); err != nil {
		t.Fatalf("index Iterate: %v", err)
	}
	if n != 1 {
		t.Fatalf("index count after replace = %d, want 1", n)
	}
}

// TestUniqueIndexExactValueContract pins that a unique index treats only an
// exact value match as a conflict, on both engines. Each engine's conflict
// check scans the index value as a byte prefix, so variable-length values
// that prefix one another are where an engine could falsely conflict — or
// worse, evict an unrelated row under replace.
func TestUniqueIndexExactValueContract(t *testing.T) {
	tbl, err := newTestStore(t).Table("t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if _, err := tbl.AddUniqueIndex("u", func(_, v kvstore.KV) ([]byte, error) {
		return v.([]byte), nil
	}); err != nil {
		t.Fatalf("AddUniqueIndex: %v", err)
	}

	set := func(k, v string, opts ...kvstore.SetOption) error {
		return tbl.Set([]byte(k), []byte(v), opts...)
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
	// Under replace, only the row actually holding the duplicated value is
	// evicted; the prefix-sharing rows survive.
	if err := set("k4", "ab", kvstore.WithReplace()); err != nil {
		t.Fatalf("replace of exact duplicate failed: %v", err)
	}
	if _, err := tbl.Get([]byte("k1")); !errors.Is(err, kvstore.ErrKeyNotFound) {
		t.Fatalf("conflicting row not evicted, Get(k1) = %v", err)
	}
	for _, k := range []string{"k2", "k3", "k4"} {
		if _, err := tbl.Get([]byte(k)); err != nil {
			t.Fatalf("row %q lost to a false conflict: %v", k, err)
		}
	}
}

// TestSetBatchContract pins SetBatch atomicity: without WithReplace a
// mid-batch key conflict rolls back the whole batch, and with it the batch
// applies.
func TestSetBatchContract(t *testing.T) {
	tbl, err := newTestStore(t).Table("t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if err := tbl.Set([]byte("existing"), []byte("v0")); err != nil {
		t.Fatalf("seed Set: %v", err)
	}

	batch := []kvstore.BatchEntry{
		{K: []byte("fresh"), V: []byte("v1")},
		{K: []byte("existing"), V: []byte("v2")},
	}
	if err := tbl.SetBatch(batch); err == nil {
		t.Fatal("conflicting SetBatch without WithReplace succeeded")
	}
	if _, err := tbl.Get([]byte("fresh")); !errors.Is(err, kvstore.ErrKeyNotFound) {
		t.Fatalf("pre-conflict batch entry survived rollback: %v", err)
	}

	if err := tbl.SetBatch(batch, kvstore.WithReplace()); err != nil {
		t.Fatalf("SetBatch with WithReplace: %v", err)
	}
	for k, want := range map[string]string{"fresh": "v1", "existing": "v2"} {
		got, err := tbl.Get([]byte(k))
		if err != nil {
			t.Fatalf("Get(%q): %v", k, err)
		}
		if string(got) != want {
			t.Fatalf("Get(%q) = %q, want %q", k, got, want)
		}
	}
}

// TestIterDeleteContract pins StoreIter.Delete: it requires WithUpdate on
// both engines, and with it the datum and its index entries are removed
// mid-iteration.
func TestIterDeleteContract(t *testing.T) {
	tbl, err := newTestStore(t).Table("t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	idx, err := tbl.AddIndex("byStamp", func(_, v kvstore.KV) ([]byte, error) {
		return []byte{v.(*contractRecord).stamp}, nil
	})
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}
	for _, r := range []*contractRecord{{stamp: 1, body: "a"}, {stamp: 2, body: "b"}} {
		if err := tbl.Set([]byte(r.body), r); err != nil {
			t.Fatalf("Set(%q): %v", r.body, err)
		}
	}

	// Delete without WithUpdate must fail on both engines.
	err = tbl.Iterate(nil, func(it kvstore.StoreIter) error { return it.Delete() })
	if err == nil || !strings.Contains(err.Error(), "requires WithUpdate") {
		t.Fatalf("Delete without WithUpdate = %v, want a requires-WithUpdate error", err)
	}

	// With WithUpdate, delete "a" mid-iteration.
	err = tbl.Iterate(nil, func(it kvstore.StoreIter) error {
		k, err := it.K()
		if err != nil {
			return err
		}
		if string(k) == "a" {
			return it.Delete()
		}
		return nil
	}, kvstore.WithUpdate())
	if err != nil {
		t.Fatalf("Iterate with WithUpdate: %v", err)
	}
	if _, err := tbl.Get([]byte("a")); !errors.Is(err, kvstore.ErrKeyNotFound) {
		t.Fatalf("deleted datum Get = %v, want ErrKeyNotFound", err)
	}
	var stamps []string
	if err := idx.Iterate(nil, func(it kvstore.StoreIter) error {
		return it.Entry(func(e []byte) error {
			stamps = append(stamps, string(e))
			return nil
		})
	}); err != nil {
		t.Fatalf("index Iterate: %v", err)
	}
	if !eq(stamps, "\x02") {
		t.Fatalf("index entries after delete = %q, want only stamp 2", stamps)
	}
}

// TestDefaultOptionsContract pins UseDefaultSetOptions and
// UseDefaultIterationOptions: defaults apply to every call, per-call options
// override them where they overlap (WithForward clearing a WithReverse
// default), and SetBatch honors the table's Set defaults.
func TestDefaultOptionsContract(t *testing.T) {
	tbl, err := newTestStore(t).Table("t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	idx, err := tbl.AddIndex("byVal", func(_, v kvstore.KV) ([]byte, error) {
		return v.([]byte), nil
	})
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}

	// A WithReplace default must let re-Sets and re-batching succeed without
	// per-call options.
	tbl.UseDefaultSetOptions(kvstore.WithReplace())
	for _, k := range []string{"a", "b", "c"} {
		if err := tbl.Set([]byte(k), []byte(k)); err != nil {
			t.Fatalf("Set(%q): %v", k, err)
		}
		if err := tbl.Set([]byte(k), []byte(k)); err != nil {
			t.Fatalf("default-replace re-Set(%q): %v", k, err)
		}
	}
	if err := tbl.SetBatch([]kvstore.BatchEntry{{K: []byte("a"), V: []byte("a")}}); err != nil {
		t.Fatalf("default-replace SetBatch: %v", err)
	}

	keysOf := func(it func(func(kvstore.StoreIter) error, ...kvstore.IterationOption) error, opts ...kvstore.IterationOption) []string {
		t.Helper()
		return collectKeys(t, func(f func(kvstore.StoreIter) error) error {
			return it(f, opts...)
		})
	}
	tblIter := func(f func(kvstore.StoreIter) error, opts ...kvstore.IterationOption) error {
		return tbl.Iterate(nil, f, opts...)
	}
	idxIter := func(f func(kvstore.StoreIter) error, opts ...kvstore.IterationOption) error {
		return idx.Iterate(nil, f, opts...)
	}

	// A WithReverse default flips iteration on both the table and the index;
	// a per-call WithForward overrides it back.
	tbl.UseDefaultIterationOptions(kvstore.WithReverse())
	idx.UseDefaultIterationOptions(kvstore.WithReverse())
	if got := keysOf(tblIter); !eq(got, "c", "b", "a") {
		t.Errorf("table default-reverse = %v, want [c b a]", got)
	}
	if got := keysOf(tblIter, kvstore.WithForward()); !eq(got, "a", "b", "c") {
		t.Errorf("table WithForward override = %v, want [a b c]", got)
	}
	if got := keysOf(idxIter); !eq(got, "c", "b", "a") {
		t.Errorf("index default-reverse = %v, want [c b a]", got)
	}
	if got := keysOf(idxIter, kvstore.WithForward()); !eq(got, "a", "b", "c") {
		t.Errorf("index WithForward override = %v, want [a b c]", got)
	}
}

// TestSeekerContract pins the cursor contract of kvstore.StoreIndex.Seeker:
// the seek it returns for a primary key is that row's own entry in the index,
// in index space, so an Iterate handed the seek visits the row first in either
// direction — including when other rows share the same index value, the
// disambiguation a pagination cursor (wallet/txdb RefID) depends on — and the
// seek composes with a prefix-scoped iteration. A key with no row, or a row
// the index function skipped (ErrNotIndexed), reports ErrKeyNotFound.
func TestSeekerContract(t *testing.T) {
	tbl, err := newTestStore(t).Table("t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	// The entry is the value's first two bytes: group || ord. A value whose
	// first byte is 0xff is not indexed.
	idx, err := tbl.AddIndex("byGroupOrd", func(_, v kvstore.KV) ([]byte, error) {
		b := v.([]byte)
		if b[0] == 0xff {
			return nil, kvstore.ErrNotIndexed
		}
		return b[:2], nil
	})
	if err != nil {
		t.Fatalf("AddIndex: %v", err)
	}

	for _, row := range []struct {
		k string
		v []byte
	}{
		{"a", []byte{1, 1}},
		{"b", []byte{1, 2}}, // "b" and "c" share an index value.
		{"c", []byte{1, 2}},
		{"d", []byte{1, 3}},
		{"e", []byte{2, 1}},
		{"x", []byte{0xff, 0}}, // not indexed
	} {
		mustSet(t, tbl, row.k, string(row.v))
	}

	seekFor := func(k string) []byte {
		t.Helper()
		seek, err := idx.Seeker([]byte(k))
		if err != nil {
			t.Fatalf("Seeker(%q): %v", k, err)
		}
		return seek
	}
	iterKeys := func(prefix []byte, opts ...kvstore.IterationOption) []string {
		return collectKeys(t, func(f func(kvstore.StoreIter) error) error {
			return idx.Iterate(prefix, f, opts...)
		})
	}

	// Iteration begun at a row's seek visits that row first in both
	// directions, landing exactly even where rows share an index value.
	if got := iterKeys(nil, kvstore.WithSeek(seekFor("c"))); !eq(got, "c", "d", "e") {
		t.Fatalf("forward from c = %v, want [c d e]", got)
	}
	if got := iterKeys(nil, kvstore.WithReverse(), kvstore.WithSeek(seekFor("c"))); !eq(got, "c", "b", "a") {
		t.Fatalf("reverse from c = %v, want [c b a]", got)
	}
	if got := iterKeys(nil, kvstore.WithSeek(seekFor("b"))); !eq(got, "b", "c", "d", "e") {
		t.Fatalf("forward from b = %v, want [b c d e]", got)
	}
	if got := iterKeys(nil, kvstore.WithReverse(), kvstore.WithSeek(seekFor("b"))); !eq(got, "b", "a") {
		t.Fatalf("reverse from b = %v, want [b a]", got)
	}

	// The seek composes with a prefix scope — the wallet/txdb shape, where the
	// prefix is a network/asset scope and the seek a cursor row within it.
	if got := iterKeys([]byte{1}, kvstore.WithSeek(seekFor("c"))); !eq(got, "c", "d") {
		t.Fatalf("forward from c within group 1 = %v, want [c d]", got)
	}
	if got := iterKeys([]byte{1}, kvstore.WithReverse(), kvstore.WithSeek(seekFor("c"))); !eq(got, "c", "b", "a") {
		t.Fatalf("reverse from c within group 1 = %v, want [c b a]", got)
	}

	if _, err := idx.Seeker([]byte("nope")); !errors.Is(err, kvstore.ErrKeyNotFound) {
		t.Fatalf("Seeker(missing key) err = %v, want ErrKeyNotFound", err)
	}
	if _, err := idx.Seeker([]byte("x")); !errors.Is(err, kvstore.ErrKeyNotFound) {
		t.Fatalf("Seeker(unindexed row) err = %v, want ErrKeyNotFound", err)
	}
}

// TestCrossIndexContract pins kvstore.StoreIter.CrossIndex: while iterating
// one index (or the table itself), the entry any other index generated for the
// current datum is readable without decoding the datum's value — the mechanism
// wallet/txdb's type filtering uses to skip records of ignored types before
// paying for a full decode. A datum with no entry in the named index reports
// ErrKeyNotFound; naming an index the table does not have is a distinct error.
func TestCrossIndexContract(t *testing.T) {
	tbl, err := newTestStore(t).Table("t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	// Two independent indexes over the value bytes: byFirst on v[0], bySecond
	// on v[1]. bySecond skips values whose second byte is 0xff.
	byFirst, err := tbl.AddIndex("byFirst", func(_, v kvstore.KV) ([]byte, error) {
		return v.([]byte)[:1], nil
	})
	if err != nil {
		t.Fatalf("AddIndex(byFirst): %v", err)
	}
	if _, err := tbl.AddIndex("bySecond", func(_, v kvstore.KV) ([]byte, error) {
		b := v.([]byte)
		if b[1] == 0xff {
			return nil, kvstore.ErrNotIndexed
		}
		return b[1:2], nil
	}); err != nil {
		t.Fatalf("AddIndex(bySecond): %v", err)
	}

	mustSet(t, tbl, "a", string([]byte{1, 9}))
	mustSet(t, tbl, "b", string([]byte{2, 8}))
	mustSet(t, tbl, "c", string([]byte{3, 0xff})) // absent from bySecond

	crossOf := func(iterate func(f func(kvstore.StoreIter) error) error) (map[string]string, map[string]error) {
		entries := make(map[string]string)
		errs := make(map[string]error)
		if err := iterate(func(it kvstore.StoreIter) error {
			k, err := it.K()
			if err != nil {
				return err
			}
			e, err := it.CrossIndex("bySecond")
			if err != nil {
				errs[string(k)] = err
				return nil
			}
			entries[string(k)] = string(e)
			return nil
		}); err != nil {
			t.Fatalf("Iterate: %v", err)
		}
		return entries, errs
	}
	check := func(scan string, entries map[string]string, errs map[string]error) {
		t.Helper()
		if entries["a"] != "\x09" || entries["b"] != "\x08" || len(entries) != 2 {
			t.Fatalf("%s cross entries = %q, want a:0x09 b:0x08 only", scan, entries)
		}
		if err := errs["c"]; !errors.Is(err, kvstore.ErrKeyNotFound) {
			t.Fatalf("%s cross of unindexed datum err = %v, want ErrKeyNotFound", scan, err)
		}
	}

	entries, errs := crossOf(func(f func(kvstore.StoreIter) error) error { return byFirst.Iterate(nil, f) })
	check("index scan", entries, errs)
	entries, errs = crossOf(func(f func(kvstore.StoreIter) error) error { return tbl.Iterate(nil, f) })
	check("table scan", entries, errs)

	// An index the table does not have is an error distinct from not-found.
	err = byFirst.Iterate(nil, func(it kvstore.StoreIter) error {
		if _, err := it.CrossIndex("nope"); err == nil {
			return errors.New("CrossIndex(unknown name) returned a nil error")
		} else if errors.Is(err, kvstore.ErrKeyNotFound) {
			return errors.New("CrossIndex(unknown name) reported not-found; want a distinct error")
		}
		return kvstore.ErrEndIteration
	})
	if err != nil {
		t.Fatalf("unknown-name check: %v", err)
	}
}
