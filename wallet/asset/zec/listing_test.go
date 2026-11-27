// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// A row whose record cannot be read or written is not dropped: a listing
// with a failed row is not complete, sends no history-synced note, and the
// next event lists again.
func TestFailedRowInAListing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		gets, stores int
	}{{"read", 1, 0}, {"write", 0, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			e := openTxEnv(t, 1_000)
			advance := testClock(t)
			e.c.put(receive(txid(1), 5, 100)) // settled: final, raw, no fee owed
			e.faulty.fail(tc.gets, tc.stores)
			e.project(projectStart)
			if e.stored(txid(1)) != nil {
				t.Fatal("stored despite the failure")
			}
			if n := e.countSynced(); n != 0 {
				t.Errorf("%d history-synced notes from a failed listing", n)
			}
			if !e.w.proj.fullNeeded {
				t.Error("no full listing due after a failed one")
			}
			advance(listingRetryDelay)
			e.project(projectStatus)
			if e.stored(txid(1)) == nil {
				t.Fatal("not stored by the next listing")
			}
			if n := e.countSynced(); n != 1 || e.w.proj.fullNeeded {
				t.Errorf("after the next listing: %d notes, due %t", n, e.w.proj.fullNeeded)
			}
		})
	}
}

// The last read of a row that leaves the open rows can fail: the row stays
// open and is read again.
func TestFailedRowLeavingTheOpenRows(t *testing.T) {
	for _, tc := range []struct {
		name         string
		gets, stores int
	}{{"read", 1, 0}, {"write", 0, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			e := openTxEnv(t, 100)
			r := send(txid(1), 40_000, 0, 80)
			r.Fee = nil // open while its fee is unknown
			e.c.put(r)
			e.project(projectStart)
			if wt := e.stored(txid(1)); wt == nil || wt.AdditionalData["feeUnknown"] != "true" {
				t.Fatalf("before: %+v", wt)
			}
			e.c.put(send(txid(1), 40_000, 10_000, 80)) // the fee is learned; it leaves
			e.faulty.fail(tc.gets, tc.stores)
			e.setStatus(zeclib.SyncSynced, 101)
			e.project(projectStatus)
			if _, open := e.w.proj.open[txid(1)]; !open {
				t.Fatal("a row whose last write failed left the open rows")
			}
			e.setStatus(zeclib.SyncSynced, 102)
			e.project(projectStatus)
			if wt := e.stored(txid(1)); wt.Fees != 10_000 {
				t.Fatalf("the fee was never written: %+v", wt)
			}
			if _, open := e.w.proj.open[txid(1)]; open {
				t.Error("still open after the write")
			}
		})
	}
}

// A catch-up whose full listing fails keeps the listing due until one
// succeeds, also across a pause and a resume.
func TestFailedCatchUpListingIsKept(t *testing.T) {
	e := openTxEnv(t, 1_000)
	advance := testClock(t)
	e.project(projectStart)
	e.countSynced()
	e.c.put(receive(txid(1), 5, 100))
	e.w.proj.relist.Store(true)
	e.c.mtx.Lock()
	e.c.listErr = errors.New("injected listing failure")
	e.c.mtx.Unlock()
	e.project(projectStatus)
	if e.stored(txid(1)) != nil || !e.w.proj.fullNeeded {
		t.Fatalf("after the failed listing: stored %t, due %t", e.stored(txid(1)) != nil, e.w.proj.fullNeeded)
	}
	// A cancelled listing leaves it due as well.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.w.project(ctx, projectStatus)
	if !e.w.proj.fullNeeded {
		t.Fatal("a cancelled listing cleared the listing due")
	}
	advance(listingRetryDelay)
	e.project(projectStatus)
	if e.stored(txid(1)) == nil || e.w.proj.fullNeeded || e.countSynced() != 1 {
		t.Fatal("the listing was not made again")
	}
}

// A rewind and the rescan that mines a final row again at another height,
// both done before the worker runs: the rewind lists the history again, and
// the record moves to the new height.
func TestRewindRelists(t *testing.T) {
	e := openTxEnv(t, 160)
	e.c.put(receive(txid(1), 5, 140))
	e.project(projectStart)
	if wt := e.stored(txid(1)); !wt.Confirmed || wt.BlockNumber != 140 {
		t.Fatalf("before: %+v", wt)
	}
	e.c.put(receive(txid(1), 5, 145))
	// Without the rewind, nothing reads the row again: it is not open.
	e.project(projectStatus)
	e.project(projectChange)
	if wt := e.stored(txid(1)); wt.BlockNumber != 140 {
		t.Fatalf("read without a reason: %+v", wt)
	}
	e.w.onRewind(135)
	e.project(projectStatus)
	if wt := e.stored(txid(1)); wt.BlockNumber != 145 || wt.Timestamp != 1_700_000_145 {
		t.Fatalf("after the rewind: %+v", wt)
	}
}

// Becoming synced at the same tip runs the open pass, which can now write an
// expired row as rejected.
func TestSyncedFlipRunsTheOpenPass(t *testing.T) {
	e := openTxEnv(t, 300)
	r := send(txid(1), 40_000, 10_000, 0)
	r.ExpiredUnmined = true
	e.c.put(r)
	e.setStatus(zeclib.SyncSyncing, 300)
	e.project(projectStart)
	if e.stored(txid(1)).Rejected {
		t.Fatal("rejected while syncing")
	}
	e.setStatus(zeclib.SyncSynced, 300)
	e.project(projectStatus)
	if !e.stored(txid(1)).Rejected {
		t.Fatal("not rejected after becoming synced at the same tip")
	}
}

// A close during a full listing gets in between two pages, and the listing
// stops, still due.
func TestCloseStopsAListing(t *testing.T) {
	e := openTxEnv(t, 1_000)
	for i := 1; i <= 450; i++ {
		e.c.put(receive(txid(i), uint64(i), 100))
	}
	var pages []uint64
	closed := make(chan struct{})
	e.c.listHook = func(after uint64) {
		pages = append(pages, after)
		if after == historyPage {
			go func() {
				defer close(closed)
				e.w.Close()
			}()
			// The close waits for this page.
			time.Sleep(50 * time.Millisecond)
		}
	}
	e.project(projectStart)
	waitClosed(t, "the close", closed)
	if len(pages) != 2 {
		t.Errorf("pages read from %v", pages)
	}
	if !e.w.proj.fullNeeded {
		t.Error("the stopped listing is not due")
	}
}

// A close already waiting for the guard when the last page of a full
// listing returns finishes first: the listing then finds the instance
// closed, sends no history-synced note and stays due.
func TestCloseWaitingAtTheEndOfAListing(t *testing.T) {
	e := openTxEnv(t, 1_000)
	e.c.put(receive(txid(1), 5, 100))
	closed := make(chan struct{})
	var once sync.Once
	e.c.listHook = func(uint64) {
		once.Do(func() {
			go func() {
				defer close(closed)
				e.w.Close()
			}()
			// The page holds the guard; the close waits for it once a
			// read lock cannot be taken.
			eventually(t, "the close to wait", func() bool {
				if e.w.beMtx.TryRLock() {
					e.w.beMtx.RUnlock()
					return false
				}
				return true
			})
		})
	}
	e.project(projectStart)
	waitClosed(t, "the close", closed)
	if n := e.countSynced(); n != 0 {
		t.Errorf("%d history-synced notes from a closed instance", n)
	}
	if !e.w.proj.fullNeeded {
		t.Error("the listing is not due after the close")
	}
}

// A listing whose database is closed and opened again between two pages
// ends without reading a page from the new database.
func TestListingAcrossAReopen(t *testing.T) {
	e := openTxEnv(t, 1_000)
	for i := 1; i <= 250; i++ {
		e.c.put(receive(txid(i), uint64(i), 100))
	}
	var pages []uint64
	reopened := make(chan struct{})
	e.c.listHook = func(after uint64) {
		pages = append(pages, after)
		if after != 0 {
			return
		}
		go func() {
			defer close(reopened)
			e.w.Close()
			if err := e.w.OpenWithPW(context.Background(), testPW); err != nil {
				t.Errorf("OpenWithPW: %v", err)
			}
		}()
		eventually(t, "the close to wait", func() bool {
			if e.w.beMtx.TryRLock() {
				e.w.beMtx.RUnlock()
				return false
			}
			return true
		})
	}
	old, _, release, err := e.w.backend()
	if err != nil {
		t.Fatal(err)
	}
	release()
	err = e.w.fullListing(context.Background())
	waitClosed(t, "the reopen", reopened)
	if !errors.Is(err, errReopened) && !errors.Is(err, errClosed) {
		t.Fatalf("the listing across a reopen ended with %v", err)
	}
	if len(pages) != 1 {
		t.Errorf("pages read from %v", pages)
	}
	// Once the new database is open, a page of the old listing is refused
	// before anything is read.
	pages = nil
	e.c.listHook = func(after uint64) { pages = append(pages, after) }
	if _, _, _, _, err := e.w.listPage(old, historyPage); !errors.Is(err, errReopened) {
		t.Errorf("a page pinned to the old database: %v", err)
	}
	if len(pages) != 0 {
		t.Errorf("the new database was read: %v", pages)
	}
}

// A failed ownership lookup fails the row: nothing is written for it, no
// Send note is sent, and the next pass writes it as the SelfSend it is.
func TestFailedOwnershipLookup(t *testing.T) {
	e := openTxEnv(t, 300)
	parsed["ufake-mine2"] = &zeclib.ParsedAddress{Kind: zeclib.AddressUnified, Payable: true,
		Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: "ufake-mine2-o"}}}
	t.Cleanup(func() { delete(parsed, "ufake-mine2") })
	be := e.o.latest()
	be.set(func(b *fakeBackend) {
		b.owns["ufake-mine2-o"] = "acct-0"
		b.ownsErr = errors.New("injected lookup failure")
	})
	e.c.put(zeclib.TxRecord{TxID: txid(1), TotalSpent: 31_000, TotalReceived: 1_000, BalanceDelta: -10_000,
		Fee: u64(10_000), RawPresent: true, MinedHeight: u32(290), BlockTime: u64(1),
		Outputs: []zeclib.TxOutput{
			{Pool: zeclib.OutputIronwood, Value: 20_000, Address: "ufake-mine2"},
			{Pool: zeclib.OutputIronwood, Value: 1_000, ToWallet: true, IsChange: true},
		}})
	e.project(projectStart)
	if e.stored(txid(1)) != nil {
		t.Fatal("written although the lookup failed")
	}
	if e.log.count("injected lookup failure") != 1 {
		t.Errorf("the failure was logged %d times", e.log.count("injected lookup failure"))
	}
	// The log line names the address by its first ten characters.
	if e.log.count("ufake-mine2") != 0 || e.log.count("Checking whether ufake-mine… is the wallet's") != 1 {
		t.Errorf("the address is not abbreviated: %q", e.log.lines)
	}
	e.setStatus(zeclib.SyncSynced, 301)
	e.project(projectStatus)
	wt := e.stored(txid(1))
	if wt == nil || wt.Type != txdb.SelfSend || wt.Amount != 20_000 {
		t.Fatalf("after the retry: %+v", wt)
	}
	for _, n := range e.txNotes() {
		if n.Transaction.Type == txdb.Send {
			t.Errorf("a Send note: %+v", n.Transaction)
		}
	}
}

// After a full listing fails, no other starts for a minute; events do their
// incremental work meanwhile.
func TestFailedListingWaitsBeforeTheNext(t *testing.T) {
	e := openTxEnv(t, 1_000)
	advance := testClock(t)
	e.c.put(receive(txid(1), 5, 100))
	e.faulty.fail(0, 1_000)
	const failed = "some records could not be written"
	e.project(projectStart)
	if n := e.log.count(failed); n != 1 {
		t.Fatalf("%d listings", n)
	}
	advance(30 * time.Second)
	e.c.put(receive(txid(2), 6, 0))
	e.faulty.fail(0, 0)
	e.project(projectChange)
	if n := e.log.count(failed); n != 1 {
		t.Errorf("a listing inside the minute: %d", n)
	}
	if e.stored(txid(2)) == nil {
		t.Error("the incremental work did not run inside the minute")
	}
	if !e.w.proj.fullNeeded {
		t.Error("the listing is no longer due")
	}
	advance(31 * time.Second)
	e.project(projectStatus)
	if e.w.proj.fullNeeded || e.stored(txid(1)) == nil || e.countSynced() != 1 {
		t.Errorf("after the minute: due %t, stored %t", e.w.proj.fullNeeded, e.stored(txid(1)) != nil)
	}
}

// A rewind during the wait is latched: incremental work goes on, and the
// full listing it asks for runs on the first event after the wait.
func TestRewindDuringTheWait(t *testing.T) {
	e := openTxEnv(t, 160)
	advance := testClock(t)
	e.c.put(receive(txid(1), 5, 140))
	e.project(projectStart)
	e.countSynced()
	e.c.put(receive(txid(2), 6, 150))
	e.w.proj.relist.Store(true)
	e.failListing(t)

	e.c.put(receive(txid(1), 5, 145)) // remined at another height
	e.w.onRewind(135)
	e.c.put(receive(txid(3), 7, 0))
	e.project(projectChange)
	if e.stored(txid(3)) == nil {
		t.Error("no incremental work during the wait")
	}
	if wt := e.stored(txid(1)); wt.BlockNumber != 140 {
		t.Errorf("listed in full during the wait: %+v", wt)
	}
	advance(listingRetryDelay)
	e.project(projectStatus)
	if wt := e.stored(txid(1)); wt.BlockNumber != 145 || e.countSynced() != 1 || e.w.proj.fullNeeded {
		t.Errorf("after the wait: %+v, due %t", wt, e.w.proj.fullNeeded)
	}
}

// A close and reopen during the wait does not end it: the reopened backend's
// first full listing runs on the first event after the wait, not before, and
// is not lost.
func TestReopenDuringTheWait(t *testing.T) {
	e := openTxEnv(t, 160)
	advance := testClock(t)
	e.project(projectStart)
	e.countSynced()
	e.c.put(receive(txid(1), 5, 140))
	e.w.proj.relist.Store(true)
	e.failListing(t)

	if err := e.w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.w.OpenWithPW(context.Background(), testPW); err != nil {
		t.Fatal(err)
	}
	e.setStatus(zeclib.SyncSynced, 160)
	e.project(projectStatus)
	if e.countSynced() != 0 {
		t.Error("the reopened backend was listed in full during the wait")
	}
	advance(listingRetryDelay)
	e.project(projectStatus)
	if e.countSynced() != 1 || e.w.proj.fullNeeded || e.stored(txid(1)) == nil {
		t.Errorf("after the wait: due %t, stored %t", e.w.proj.fullNeeded, e.stored(txid(1)) != nil)
	}
}
