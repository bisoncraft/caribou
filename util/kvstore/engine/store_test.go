// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package engine

import (
	"errors"
	"path/filepath"
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
	if err := tbl.Set([]byte(k), []byte(v), true); err != nil {
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
			return tbl.Iterate(p, f, reverse, s)
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
	// position differently: lexi seeks past a trailing DBID, glossi steps back
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
		return native.Iterate(nil, f, false, nil)
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
