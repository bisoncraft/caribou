// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
)

// cappedLedgerServer serves userNonFundingLedgerUpdates from the given events,
// returning at most cap oldest rows with Time >= startTime — modeling the real
// API's per-response row cap.
func cappedLedgerServer(t *testing.T, events []ledgerUpdate, cap int) (*rpcClient, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			StartTime int64 `json:"startTime"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var page []ledgerUpdate
		for _, u := range events {
			if int64(u.Time) >= req.StartTime {
				page = append(page, u)
			}
		}
		sort.SliceStable(page, func(i, j int) bool { return page[i].Time < page[j].Time })
		if len(page) > cap {
			page = page[:cap]
		}
		_ = json.NewEncoder(rw).Encode(page)
	}))
	rpc, err := newRPCClient(srv.URL, "", util.StdOutLogger("HLPAGE", util.LevelError))
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}
	return rpc, srv.Close
}

// eventsAtTimes builds distinct-hash HYPE transfer events at the given times.
func eventsAtTimes(times ...int) []ledgerUpdate {
	out := make([]ledgerUpdate, len(times))
	for i, ms := range times {
		out[i] = ledgerUpdate{
			Time:  uint64(1700000000000 + ms),
			Hash:  fmt.Sprintf("0x%064x", i),
			Delta: ledgerDelta{Type: "spotTransfer", Token: "HYPE", Amount: "1"},
		}
	}
	return out
}

func assertNoDuplicates(t *testing.T, got []ledgerUpdate) {
	t.Helper()
	seen := map[string]bool{}
	for _, u := range got {
		if seen[u.Hash] {
			t.Fatalf("duplicate event %s returned", u.Hash)
		}
		seen[u.Hash] = true
	}
}

// TestLedgerUpdatesPagination covers the common cases: a history longer than one
// capped page with strictly increasing timestamps, and same-millisecond buckets
// (each smaller than the cap) that straddle page boundaries. Both must return
// every event exactly once.
func TestLedgerUpdatesPagination(t *testing.T) {
	cases := []struct {
		name  string
		times []int
		cap   int
	}{
		{"strictly increasing", []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, 4},
		// Buckets of size 2 straddle the cap-4 page boundaries; the inclusive
		// advance + hash de-dup must reassemble them.
		{"same-ms straddle under cap", []int{1, 1, 2, 2, 3, 3, 4}, 4},
		{"larger same-ms straddle", []int{1, 1, 1, 2, 2, 2, 3, 3, 3, 4}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := eventsAtTimes(tc.times...)
			rpc, closeFn := cappedLedgerServer(t, events, tc.cap)
			defer closeFn()

			got, err := rpc.ledgerUpdates(context.Background(), "0xabc", 0)
			if err != nil {
				t.Fatalf("ledgerUpdates: %v", err)
			}
			if len(got) != len(events) {
				t.Fatalf("got %d updates, want %d — pagination dropped or duplicated events", len(got), len(events))
			}
			assertNoDuplicates(t, got)
		})
	}
}

// TestLedgerUpdatesPaginationPinnedBucket documents the one unavoidable limit: a
// single millisecond holding a full page of events cannot be paged past via
// startTime. The scan must still terminate (no infinite loop), return no
// duplicates, and deliver everything up to the pinned bucket; events after it
// are recovered on a later scan once the watermark advances.
func TestLedgerUpdatesPaginationPinnedBucket(t *testing.T) {
	// cap 2, with three events sharing time bucket "2" (>= cap), then a later one.
	events := eventsAtTimes(1, 2, 2, 2, 3)
	rpc, closeFn := cappedLedgerServer(t, events, 2)
	defer closeFn()

	got, err := rpc.ledgerUpdates(context.Background(), "0xabc", 0)
	if err != nil {
		t.Fatalf("ledgerUpdates: %v", err)
	}
	assertNoDuplicates(t, got)
	// The event before the pinned bucket must be present.
	if len(got) == 0 || got[0].Time != uint64(1700000000001) {
		t.Fatalf("expected the pre-bucket event first, got %d events", len(got))
	}
	// It must not return more than exist, and must terminate (reaching here).
	if len(got) > len(events) {
		t.Fatalf("returned %d events, more than the %d available", len(got), len(events))
	}
}
