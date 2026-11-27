// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// openTxEnv is a txEnv whose wallet is open and synced at tip.
func openTxEnv(t *testing.T, tip uint32) *txEnv {
	t.Helper()
	e := newTxEnv(t)
	if err := e.w.OpenWithPW(context.Background(), testPW); err != nil {
		t.Fatal(err)
	}
	e.setStatus(zeclib.SyncSynced, tip)
	return e
}

func (e *txEnv) setStatus(state zeclib.SyncState, tip uint32) {
	e.o.latest().set(func(b *fakeBackend) { b.status = zeclib.SyncStatus{State: state, ChainTip: tip} })
}

func (e *txEnv) project(ev projectEvent) {
	e.w.project(context.Background(), ev)
}

// receive is a record of a payment received at the deposit address.
func receive(id string, value uint64, mined uint32) zeclib.TxRecord {
	r := zeclib.TxRecord{TxID: id, TotalReceived: value, BalanceDelta: int64(value), RawPresent: true,
		Outputs: []zeclib.TxOutput{{Pool: zeclib.OutputIronwood, Value: value, Address: testAddress, ToWallet: true}}}
	if mined != 0 {
		r.MinedHeight, r.BlockTime = u32(mined), u64(1_700_000_000+uint64(mined))
	}
	return r
}

// send is a record of a payment of value to someone else with change.
func send(id string, value, fee uint64, mined uint32) zeclib.TxRecord {
	r := zeclib.TxRecord{TxID: id, TotalSpent: value + fee + 1_000, TotalReceived: 1_000,
		BalanceDelta: -int64(value + fee), Fee: u64(fee), RawPresent: true, CreatedByWallet: true,
		Outputs: []zeclib.TxOutput{
			{Pool: zeclib.OutputIronwood, Value: value, Address: "ufake-other"},
			{Pool: zeclib.OutputIronwood, Value: 1_000, ToWallet: true, IsChange: true},
		}}
	if mined != 0 {
		r.MinedHeight, r.BlockTime = u32(mined), u64(1_700_000_000+uint64(mined))
	}
	return r
}

func TestFullListing(t *testing.T) {
	e := openTxEnv(t, 1_000)
	for i := 1; i <= 450; i++ {
		e.c.put(receive(txid(i), uint64(i), 100))
	}
	e.project(projectStart)
	for _, i := range []int{1, 200, 201, 450} {
		if wt := e.stored(txid(i)); wt == nil || wt.Amount != uint64(i) || wt.Type != txdb.Receive || !wt.Confirmed {
			t.Errorf("record %d: %+v", i, wt)
		}
	}
	notes := e.notes.drain()
	var news, synced int
	for _, n := range notes {
		switch n := n.(type) {
		case *asset.TransactionNote:
			if n.New {
				news++
			}
		default:
			if strings.Contains(fmt.Sprintf("%+v", n), "transactionHistorySynced") {
				synced++
			}
		}
	}
	if news != 450 || synced != 1 {
		t.Errorf("%d new-transaction notes, %d history-synced notes", news, synced)
	}
	if e.w.proj.watermark != 450 {
		t.Errorf("watermark %d", e.w.proj.watermark)
	}
	// Nothing changed: a second pass writes nothing.
	e.project(projectChange)
	if n := len(e.txNotes()); n != 0 {
		t.Errorf("%d notes without a change", n)
	}

	// The watermark survives a reopening, and the first connection after it
	// lists in full again, which writes nothing new.
	e.w.Close()
	w2 := e.newTxInstance()
	e.w = w2
	if err := w2.OpenWithPW(context.Background(), testPW); err != nil {
		t.Fatal(err)
	}
	e.setStatus(zeclib.SyncSynced, 1_000)
	e.project(projectStart)
	if w2.proj.watermark != 450 {
		t.Errorf("watermark after reopening %d", w2.proj.watermark)
	}
	if n := len(e.txNotes()); n != 0 {
		t.Errorf("%d notes from the listing after reopening", n)
	}
}

// Rows after the watermark are read on a change, whatever their height. A
// row linked below the watermark, as a restore can find one, waits for a
// full listing, which a catch-up brings.
func TestRowsAfterTheWatermark(t *testing.T) {
	e := openTxEnv(t, 1_000)
	e.c.put(receive(txid(1), 1, 900))
	e.project(projectStart)
	e.c.put(receive(txid(2), 2, 0))
	e.project(projectChange)
	if wt := e.stored(txid(2)); wt == nil || wt.BlockNumber != 0 || wt.Confirmed {
		t.Fatalf("an unmined receive: %+v", wt)
	}
	late := receive(txid(3), 3, 500)
	late.Seq = 1 // below the watermark
	e.c.mtx.Lock()
	e.c.records[late.TxID] = &late
	e.c.mtx.Unlock()
	e.project(projectChange)
	if e.stored(txid(3)) != nil {
		t.Fatal("a row below the watermark was found without a full listing")
	}
	status := e.w.onSyncStatus()
	status(zeclib.SyncStatus{State: zeclib.SyncSyncing, FullyScanned: 800})
	status(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 1_000})
	e.project(projectStatus)
	if e.stored(txid(3)) == nil {
		t.Fatal("the full listing after a catch-up did not find the late row")
	}
}

// The catch-up latch: set at the end of a stretch of syncing that moved the
// chain by more than catchUpBlocks or recovered, not by a block or two.
func TestCatchUpLatch(t *testing.T) {
	e := openTxEnv(t, 1_000)
	status := e.w.onSyncStatus()
	for _, tc := range []struct {
		name  string
		steps []zeclib.SyncStatus
		want  bool
	}{
		{"one block", []zeclib.SyncStatus{
			{State: zeclib.SyncSyncing, FullyScanned: 1_000}, {State: zeclib.SyncSynced, ChainTip: 1_001}}, false},
		{"a catch-up", []zeclib.SyncStatus{
			{State: zeclib.SyncSyncing, FullyScanned: 1_001}, {State: zeclib.SyncSyncing, FullyScanned: 1_050},
			{State: zeclib.SyncSynced, ChainTip: 1_200}}, true},
		{"a recovery", []zeclib.SyncStatus{
			{State: zeclib.SyncSyncing, FullyScanned: 1_200, Recovering: true}, {State: zeclib.SyncSynced, ChainTip: 1_201}}, true},
		{"a disconnection during a catch-up", []zeclib.SyncStatus{
			{State: zeclib.SyncSyncing, FullyScanned: 1_201}, {State: zeclib.SyncDisconnected},
			{State: zeclib.SyncSyncing, FullyScanned: 1_250}, {State: zeclib.SyncSynced, ChainTip: 1_400}}, true},
	} {
		for _, s := range tc.steps {
			status(s)
		}
		if got := e.w.proj.relist.Swap(false); got != tc.want {
			t.Errorf("%s: latched %t", tc.name, got)
		}
	}
}

// Confirmations count against the listing's tip, the record is final at
// finalConfirmations, and a row that leaves the open rows is read once more.
func TestOpenRows(t *testing.T) {
	e := openTxEnv(t, 100)
	e.c.put(receive(txid(1), 5, 95))
	e.project(projectStart)
	wt := e.stored(txid(1))
	if wt.Confirms.Current != 6 || wt.Confirms.Target != finalConfirmations || wt.Confirmed {
		t.Fatalf("at 6 confirmations: %+v", wt.Confirms)
	}
	e.setStatus(zeclib.SyncSynced, 104)
	e.project(projectStatus)
	if wt := e.stored(txid(1)); wt.Confirms.Current != finalConfirmations || !wt.Confirmed {
		t.Fatalf("at 10 confirmations: %+v, confirmed %t", wt.Confirms, wt.Confirmed)
	}
	// Past the depth it leaves the open rows; it is read once more and
	// nothing changes.
	e.txNotes()
	e.setStatus(zeclib.SyncSynced, 105)
	lookups := e.c.lookups
	e.project(projectStatus)
	if e.c.lookups != lookups+1 {
		t.Errorf("%d lookups of the row that left", e.c.lookups-lookups)
	}
	if _, open := e.w.proj.open[txid(1)]; open {
		t.Error("still open")
	}
	if n := len(e.txNotes()); n != 0 {
		t.Errorf("%d notes for a row that did not change", n)
	}
	// The same tip again reads nothing.
	lookups = e.c.lookups
	e.project(projectStatus)
	if e.c.lookups != lookups {
		t.Error("a status at the same tip made an open pass")
	}
}

// A row mined above the listing's tip, which a status reply newer than the
// listing can give, has no confirmations.
func TestConfirmationsAboveTheTip(t *testing.T) {
	r := receive(txid(1), 5, 120)
	if c := confirmations(&r, 110); c != 0 {
		t.Errorf("%d confirmations above the tip", c)
	}
	r.MinedHeight = nil
	if c := confirmations(&r, 110); c != 0 {
		t.Errorf("%d confirmations unmined", c)
	}
}

// A row the account funded is held back until the wallet has its full
// transaction, and written once it does.
func TestHeldBackUntilRaw(t *testing.T) {
	e := openTxEnv(t, 200)
	r := send(txid(1), 50_000, 10_000, 195)
	r.RawPresent = false
	r.Outputs = []zeclib.TxOutput{{Pool: zeclib.OutputIronwood, Value: 1_000, ToWallet: true, IsChange: true}}
	e.c.put(r)
	e.project(projectStart)
	if e.stored(txid(1)) != nil {
		t.Fatal("a funded row without its full transaction was written")
	}
	if _, open := e.w.proj.open[txid(1)]; !open {
		t.Fatal("the held row is not open")
	}
	e.c.put(send(txid(1), 50_000, 10_000, 195))
	e.project(projectChange)
	if wt := e.stored(txid(1)); wt == nil || wt.Type != txdb.Send || wt.Amount != 50_000 {
		t.Fatalf("after the full transaction: %+v", wt)
	}
}

func TestClassification(t *testing.T) {
	e := openTxEnv(t, 300)
	self := zeclib.TxRecord{TxID: txid(2), TotalSpent: 61_000, TotalReceived: 51_000, BalanceDelta: -10_000,
		Fee: u64(10_000), RawPresent: true, MinedHeight: u32(290), BlockTime: u64(1),
		Outputs: []zeclib.TxOutput{
			// A payment to one of the account's addresses is marked as change
			// as well; its address tells it from change.
			{Pool: zeclib.OutputIronwood, Value: 50_000, Address: testAddress, ToWallet: true, IsChange: true},
			{Pool: zeclib.OutputIronwood, Value: 1_000, ToWallet: true, IsChange: true},
		}}
	shielding := zeclib.TxRecord{TxID: txid(3), TotalSpent: 100_000, TotalReceived: 85_000, BalanceDelta: -15_000,
		Fee: u64(15_000), RawPresent: true, IsShielding: true, MinedHeight: u32(290), BlockTime: u64(1),
		Outputs: []zeclib.TxOutput{{Pool: zeclib.OutputIronwood, Value: 85_000, ToWallet: true, IsChange: true}}}
	unknownFee := send(txid(4), 20_000, 0, 290)
	unknownFee.Fee = nil
	odd := send(txid(5), 30_000, 10_000, 290)
	odd.BalanceDelta = -1
	e.c.put(send(txid(1), 40_000, 10_000, 290))
	e.c.put(self)
	e.c.put(shielding)
	e.c.put(unknownFee)
	e.c.put(odd)
	e.c.put(receive(txid(6), 7_000, 290))
	e.project(projectStart)

	check := func(id string, typ txdb.TransactionType, amount, fees uint64, recipient string) *txdb.WalletTransaction {
		t.Helper()
		wt := e.stored(id)
		if wt == nil {
			t.Fatalf("%s not stored", id)
		}
		var got string
		if wt.Recipient != nil {
			got = *wt.Recipient
		}
		if wt.Type != typ || wt.Amount != amount || wt.Fees != fees || got != recipient ||
			wt.NetworkID != BipID || wt.AssetID != BipID {
			t.Errorf("%s: type %d amount %d fees %d recipient %q", id, wt.Type, wt.Amount, wt.Fees, got)
		}
		return wt
	}
	check(txid(1), txdb.Send, 40_000, 10_000, "ufake-other")
	check(txid(2), txdb.SelfSend, 50_000, 10_000, testAddress)
	check(txid(3), txdb.SelfSend, 85_000, 15_000, "")
	if wt := check(txid(4), txdb.Send, 20_000, 0, "ufake-other"); wt.AdditionalData["feeUnknown"] != "true" {
		t.Errorf("an unknown fee is not marked: %v", wt.AdditionalData)
	}
	check(txid(5), txdb.Send, 30_000, 10_000, "ufake-other")
	if e.log.count("not by the amount") != 1 {
		t.Error("a balance change that does not match the amount and fee was not logged")
	}
	check(txid(6), txdb.Receive, 7_000, 0, testAddress)
}

// An unmined row past its expiry is rejected, with no fee, only once zeclib
// is synced: before, a block not scanned yet can hold it.
func TestExpiry(t *testing.T) {
	e := openTxEnv(t, 300)
	r := send(txid(1), 40_000, 10_000, 0)
	r.ExpiredUnmined = true
	e.c.put(r)
	e.setStatus(zeclib.SyncSyncing, 300)
	e.project(projectStart)
	if wt := e.stored(txid(1)); wt.Rejected || wt.Confirmed || wt.Fees != 10_000 {
		t.Fatalf("expired while syncing: %+v", wt)
	}
	e.setStatus(zeclib.SyncSynced, 301)
	e.project(projectStatus)
	if wt := e.stored(txid(1)); !wt.Rejected || !wt.Confirmed || wt.Fees != 0 {
		t.Fatalf("expired while synced: %+v", wt)
	}
	if pending := e.w.PendingTransactions(context.Background()); len(pending) != 0 {
		t.Errorf("a rejected row is pending: %d", len(pending))
	}
}

// A reorganization unmines a row stored as final: it is rewritten, with a
// warning.
func TestRewindRewritesAFinalRecord(t *testing.T) {
	e := openTxEnv(t, 300)
	e.c.put(receive(txid(1), 5, 280))
	e.project(projectStart)
	if !e.stored(txid(1)).Confirmed {
		t.Fatal("not final")
	}
	e.c.put(receive(txid(1), 5, 0))
	e.txNotes()
	e.project(projectChange)
	wt := e.stored(txid(1))
	if wt.Confirmed || wt.BlockNumber != 0 {
		t.Fatalf("after the rewind: %+v", wt)
	}
	if notes := e.txNotes(); len(notes) != 1 || notes[0].New {
		t.Errorf("notes: %+v", notes)
	}
	if e.log.count("changed after a chain reorganization") != 1 {
		t.Error("no warning")
	}
	if pending := e.w.PendingTransactions(context.Background()); len(pending) != 1 {
		t.Errorf("%d pending", len(pending))
	}
}

func TestMemoText(t *testing.T) {
	for _, tc := range []struct {
		memo []byte
		want string
		ok   bool
	}{
		{nil, "", false},
		{[]byte{0xF6}, "", false},
		{append([]byte{0xF6}, make([]byte, 511)...), "", false},
		{append([]byte("thanks"), make([]byte, 506)...), "thanks", true},
		{[]byte("caf\xc3\xa9"), "café", true},
		{[]byte{0xF5, 1, 2}, "f50102", true},
		{[]byte{0x61, 0xff}, "61ff", true},
	} {
		got, ok := memoText(tc.memo)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%x: %q %t, want %q %t", tc.memo, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRecordMemo(t *testing.T) {
	e := openTxEnv(t, 300)
	r := receive(txid(1), 5, 290)
	r.Outputs[0].Memo = append([]byte("for lunch"), 0, 0)
	e.c.put(r)
	e.project(projectStart)
	if m := e.stored(txid(1)).AdditionalData["memo"]; m != "for lunch" {
		t.Errorf("memo %q", m)
	}
}

func TestHistoryReads(t *testing.T) {
	e := openTxEnv(t, 300)
	e.c.put(receive(txid(1), 5, 290))
	e.c.put(receive(txid(2), 6, 0))
	e.project(projectStart)
	if res, err := e.w.TxHistory(nil); err != nil || len(res.Txs) != 0 {
		t.Errorf("a nil request: %+v, %v", res, err)
	}
	res, err := e.w.TxHistory(&txdb.TxHistoryRequest{N: 10})
	if err != nil || len(res.Txs) != 2 {
		t.Errorf("TxHistory: %+v, %v", res, err)
	}
	if wt, err := e.w.WalletTransaction(context.Background(), txid(1)); err != nil || wt.ID != txid(1) {
		t.Errorf("by display ID: %+v, %v", wt, err)
	}
	wire := e.w.coinFor([]string{txid(1)}, 0).ID().String()
	if wt, err := e.w.WalletTransaction(context.Background(), wire); err != nil || wt.ID != txid(1) {
		t.Errorf("by wire-order ID: %+v, %v", wt, err)
	}
	if _, err := e.w.WalletTransaction(context.Background(), txid(9)); !errors.Is(err, asset.CoinNotFoundError) {
		t.Errorf("an unknown ID: %v", err)
	}
	if pending := e.w.PendingTransactions(context.Background()); len(pending) != 1 || pending[0].ID != txid(2) {
		t.Errorf("pending: %+v", pending)
	}
}

// syncedNotes counts the history-synced notes among notes.
func syncedNotes(notes []asset.WalletNotification) int {
	n := 0
	for _, note := range notes {
		if strings.Contains(fmt.Sprintf("%+v", note), "transactionHistorySynced") {
			n++
		}
	}
	return n
}

// The worker projects: a full listing on the first connection after the
// database is opened, a change through the rows after the watermark, and no
// second full listing when a pause and resume connect the same instance.
func TestWorkerProjects(t *testing.T) {
	e := newTxEnv(t)
	e.c.put(receive(txid(1), 5, 90))
	disconnect := e.connect(e.w)
	be := e.o.latest()
	be.emitStatus(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 100})
	var notes []asset.WalletNotification
	eventually(t, "the full listing", func() bool {
		notes = append(notes, e.notes.drain()...)
		return syncedNotes(notes) == 1 && e.stored(txid(1)) != nil
	})
	e.c.put(receive(txid(2), 6, 0))
	be.emitChange()
	eventually(t, "the new row", func() bool { return e.stored(txid(2)) != nil })
	disconnect()

	// Resume.
	e.connect(e.w)
	e.c.put(receive(txid(3), 7, 0))
	be.emitChange()
	eventually(t, "the row after the resume", func() bool { return e.stored(txid(3)) != nil })
	if n := syncedNotes(e.notes.drain()); n != 0 {
		t.Errorf("%d full listings after a resume", n)
	}
}
