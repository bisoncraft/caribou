// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dbtest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/intents"
)

// testIntentsSwaps exercises the NEAR Intents swap-record storage:
// round-trip, newest-first listing off the stamp index, the active set off
// the active index, and index maintenance when a record is re-stored in a
// terminal state.
func testIntentsSwaps(t *testing.T, d db.DB) {
	// Empty store: listings are empty, a lookup misses distinctly.
	if _, err := d.IntentsSwap("nope"); !errors.Is(err, db.ErrIntentsSwapNotFound) {
		t.Fatalf("expected ErrIntentsSwapNotFound for missing swap, got %v", err)
	}
	if swaps, err := d.IntentsSwaps(); err != nil || len(swaps) != 0 {
		t.Fatalf("IntentsSwaps on empty store: %d records, err = %v", len(swaps), err)
	}
	if swaps, err := d.ActiveIntentsSwaps(); err != nil || len(swaps) != 0 {
		t.Fatalf("ActiveIntentsSwaps on empty store: %d records, err = %v", len(swaps), err)
	}

	// A record with no deposit address is rejected.
	if err := d.StoreIntentsSwap(&db.IntentsSwap{State: db.IntentsStateQuoted}); err == nil {
		t.Fatal("expected error storing a swap with no deposit address")
	}

	// Three swaps, stored out of creation order, the middle one terminal.
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	swap := func(i int) *db.IntentsSwap {
		state := db.IntentsStateQuoted
		if i == 1 {
			state = intents.StatusSuccess
		}
		return &db.IntentsSwap{
			DepositAddress: fmt.Sprintf("addr%d", i),
			State:          state,
			AtomsIn:        uint64(i + 1),
			CreatedAt:      base.Add(time.Duration(i) * time.Minute),
		}
	}
	for _, i := range []int{1, 0, 2} {
		if err := d.StoreIntentsSwap(swap(i)); err != nil {
			t.Fatalf("StoreIntentsSwap(%d): %v", i, err)
		}
	}

	// Round trip, with UpdatedAt stamped by the store.
	got, err := d.IntentsSwap("addr1")
	if err != nil {
		t.Fatalf("IntentsSwap: %v", err)
	}
	want := swap(1)
	if got.DepositAddress != want.DepositAddress || got.State != want.State ||
		got.AtomsIn != want.AtomsIn || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, want)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt not stamped by StoreIntentsSwap")
	}

	// Full listing is newest first, regardless of insertion order.
	swaps, err := d.IntentsSwaps()
	if err != nil {
		t.Fatalf("IntentsSwaps: %v", err)
	}
	if len(swaps) != 3 {
		t.Fatalf("IntentsSwaps: %d records, want 3", len(swaps))
	}
	for listIdx, i := range []int{2, 1, 0} {
		if swaps[listIdx].DepositAddress != fmt.Sprintf("addr%d", i) {
			t.Fatalf("IntentsSwaps[%d] = %s, want addr%d", listIdx, swaps[listIdx].DepositAddress, i)
		}
	}

	// The active set excludes the terminal record and is oldest first.
	active, err := d.ActiveIntentsSwaps()
	if err != nil {
		t.Fatalf("ActiveIntentsSwaps: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("ActiveIntentsSwaps: %d records, want 2", len(active))
	}
	for listIdx, i := range []int{0, 2} {
		if active[listIdx].DepositAddress != fmt.Sprintf("addr%d", i) {
			t.Fatalf("ActiveIntentsSwaps[%d] = %s, want addr%d", listIdx, active[listIdx].DepositAddress, i)
		}
	}

	// Re-storing a record in a terminal state removes it from the active
	// set — this is how a swap leaves the poller's working set — and
	// updates the stored record without duplicating the listing entry.
	final := swap(0)
	final.State = intents.StatusRefunded
	if err := d.StoreIntentsSwap(final); err != nil {
		t.Fatalf("re-store: %v", err)
	}
	if active, err = d.ActiveIntentsSwaps(); err != nil || len(active) != 1 {
		t.Fatalf("ActiveIntentsSwaps after terminal re-store: %d records, err = %v", len(active), err)
	}
	if active[0].DepositAddress != "addr2" {
		t.Fatalf("remaining active record = %s, want addr2", active[0].DepositAddress)
	}
	if swaps, err = d.IntentsSwaps(); err != nil || len(swaps) != 3 {
		t.Fatalf("IntentsSwaps after re-store: %d records, err = %v", len(swaps), err)
	}
	if swaps[2].State != intents.StatusRefunded {
		t.Fatalf("re-stored record state = %s, want %s", swaps[2].State, intents.StatusRefunded)
	}
}
