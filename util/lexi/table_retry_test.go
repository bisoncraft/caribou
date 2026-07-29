// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"sync/atomic"
	"testing"

	"github.com/dgraph-io/badger/v4"
)

// TestSetDatumIsPerAttempt asserts that a Set forced to retry stores exactly
// one index entry per index.
//
// Update re-runs its closure on badger.ErrConflict, so any state the closure
// mutates has to be built inside it. A datum hoisted above the closure
// collects the index entries of every attempt, and since Index.add is
// deterministic those are duplicates of one another: the stored datum's index
// list grows with the retry count, and every later replace or delete walks the
// padding.
//
// The retry is provoked deterministically rather than by contention. The index
// function runs inside the transaction, so the first time it is called it
// commits a competing write to the same key from a separate transaction. That
// invalidates the read the in-flight transaction already did, so its commit
// returns ErrConflict and Update runs the closure again. A contention-based
// test would not do: the datum left on disk is written by whichever Set
// committed last, which is not necessarily one that retried.
func TestSetDatumIsPerAttempt(t *testing.T) {
	db, shutdown := newTestDB(t)
	defer shutdown()

	tbl, err := db.Table("retry")
	if err != nil {
		t.Fatalf("error creating table: %v", err)
	}

	k := []byte("contended")

	// interfered gates the one competing write; the nested Set below re-enters
	// the index function, and the CAS is what keeps that from recursing.
	var interfered atomic.Bool
	var indexCalls atomic.Int64
	if _, err := tbl.AddIndex("idx", func(_, _ KV) ([]byte, error) {
		indexCalls.Add(1)
		if interfered.CompareAndSwap(false, true) {
			if err := tbl.Set(k, []byte("interference"), WithReplace()); err != nil {
				return nil, err
			}
		}
		return []byte("constant-index-entry"), nil
	}); err != nil {
		t.Fatalf("error adding index: %v", err)
	}

	// Seed the key so the Set under test reads an existing datum, which is the
	// read the competing write invalidates.
	interfered.Store(true) // no interference while seeding
	if err := tbl.Set(k, []byte("seed"), WithReplace()); err != nil {
		t.Fatalf("error seeding entry: %v", err)
	}
	interfered.Store(false)
	indexCalls.Store(0)

	if err := tbl.Set(k, []byte("final"), WithReplace()); err != nil {
		t.Fatalf("error setting entry: %v", err)
	}

	// One call for the aborted attempt, one for the nested competing write, and
	// at least one for the retry that succeeded. Fewer means no retry happened
	// and the invariant below went unexercised.
	if calls := indexCalls.Load(); calls < 3 {
		t.Fatalf("index function called %d times; expected a retry", calls)
	}

	var d *datum
	if err := db.View(func(txn *badger.Txn) error {
		dbID, err := tbl.keyID(txn, tbl.prefix, k, true)
		if err != nil {
			return err
		}
		d, err = tbl.get(txn, dbID)
		return err
	}); err != nil {
		t.Fatalf("error reading back datum: %v", err)
	}
	if string(d.v) != "final" {
		t.Fatalf("stored value %q, want %q", d.v, "final")
	}
	if len(d.indexes) != 1 {
		t.Fatalf("stored datum has %d index entries, want 1", len(d.indexes))
	}
}
