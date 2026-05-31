// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package txdb

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
)

// newTestTxDB builds a TxDB backed by the build-selected storage engine (Badger
// via util/lexi on desktop, bbolt via wallet/glossi on mobile). Running the
// suite under both -tags settings verifies the two engines behave identically.
func newTestTxDB(t *testing.T) *TxDB {
	t.Helper()
	log := util.NewLogger("T", util.LevelInfo, os.Stdout)
	db, err := New(filepath.Join(t.TempDir(), "tx.db"), log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func tx(id string, blockNum, timestamp uint64) *WalletTransaction {
	return &WalletTransaction{
		Type:        Send,
		ID:          id,
		Amount:      1,
		BlockNumber: blockNum,
		Timestamp:   timestamp,
		Confirmed:   blockNum > 0,
	}
}

const netA, netB uint32 = 0, 60

func ids(txs []*WalletTransaction) []string {
	out := make([]string, len(txs))
	for i, t := range txs {
		out[i] = t.ID
	}
	return out
}

func eq(a, b []string) bool {
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

func TestStoreGetRemove(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	wt := tx("abc", 10, 100)
	if err := n.StoreTx(wt); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}
	got, err := n.GetTx("abc")
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if got == nil || got.ID != "abc" || got.Amount != 1 || got.BlockNumber != 10 {
		t.Fatalf("unexpected tx: %+v", got)
	}

	// Missing tx -> nil, nil.
	missing, err := n.GetTx("nope")
	if err != nil || missing != nil {
		t.Fatalf("expected nil,nil for missing tx, got %v, %v", missing, err)
	}

	// Update in place.
	wt.Amount = 5
	if err := n.StoreTx(wt); err != nil {
		t.Fatalf("StoreTx update: %v", err)
	}
	got, _ = n.GetTx("abc")
	if got.Amount != 5 {
		t.Fatalf("update not persisted, amount = %d", got.Amount)
	}

	if err := n.RemoveTx("abc"); err != nil {
		t.Fatalf("RemoveTx: %v", err)
	}
	got, _ = n.GetTx("abc")
	if got != nil {
		t.Fatal("expected tx gone after RemoveTx")
	}
	// Removing a missing tx is not an error.
	if err := n.RemoveTx("abc"); err != nil {
		t.Fatalf("RemoveTx missing: %v", err)
	}
}

func TestRawTxRoundTrip(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	raw := []byte{0xde, 0xad, 0xbe, 0xef}
	wt := tx("withraw", 1, 100)
	wt.RawTx = raw
	if err := n.StoreTx(wt); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}
	got, _ := n.GetTx("withraw")
	if !bytes.Equal(got.RawTx, raw) {
		t.Fatalf("RawTx round-trip failed: %x", got.RawTx)
	}

	// RawTx must not appear in the agnostic JSON.
	js, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(js, raw) {
		t.Fatalf("RawTx leaked into JSON: %s", js)
	}
	if bytes.Contains(bytes.ToLower(js), []byte("rawtx")) {
		t.Fatalf("RawTx field name leaked into JSON: %s", js)
	}
}

func TestGetTxsOrderAndPagination(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	// Stored out of order; effective time == Timestamp.
	for _, wt := range []*WalletTransaction{
		tx("t100", 1, 100),
		tx("t300", 3, 300),
		tx("t200", 2, 200),
	} {
		if err := n.StoreTx(wt); err != nil {
			t.Fatal(err)
		}
	}

	// Default (RefID nil): most recent first.
	resp, err := n.GetTxs(&TxHistoryRequest{})
	if err != nil {
		t.Fatalf("GetTxs: %v", err)
	}
	if !eq(ids(resp.Txs), []string{"t300", "t200", "t100"}) {
		t.Fatalf("default order = %v", ids(resp.Txs))
	}
	if resp.MoreAvailable {
		t.Fatal("MoreAvailable should be false when all returned")
	}

	// Pagination: N=2 -> two most recent + MoreAvailable.
	resp, _ = n.GetTxs(&TxHistoryRequest{N: 2})
	if !eq(ids(resp.Txs), []string{"t300", "t200"}) || !resp.MoreAvailable {
		t.Fatalf("paged = %v, more = %v", ids(resp.Txs), resp.MoreAvailable)
	}

	// RefID forward (past=false): from t200 ascending, inclusive.
	ref := "t200"
	resp, _ = n.GetTxs(&TxHistoryRequest{RefID: &ref, Past: false})
	if !eq(ids(resp.Txs), []string{"t200", "t300"}) {
		t.Fatalf("ref forward = %v", ids(resp.Txs))
	}

	// RefID past (past=true): from t200 descending, inclusive.
	resp, _ = n.GetTxs(&TxHistoryRequest{RefID: &ref, Past: true})
	if !eq(ids(resp.Txs), []string{"t200", "t100"}) {
		t.Fatalf("ref past = %v", ids(resp.Txs))
	}

	// Unknown RefID.
	bad := "missing"
	if _, err := n.GetTxs(&TxHistoryRequest{RefID: &bad}); !errors.Is(err, ErrUnknownTx) {
		t.Fatalf("expected ErrUnknownTx, got %v", err)
	}
}

func TestIgnoreTypes(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)
	send := tx("send", 1, 100)
	recv := tx("recv", 2, 200)
	recv.Type = Receive
	n.StoreTx(send)
	n.StoreTx(recv)

	resp, _ := n.GetTxs(&TxHistoryRequest{IgnoreTypes: []TransactionType{Receive}})
	if !eq(ids(resp.Txs), []string{"send"}) {
		t.Fatalf("ignore types = %v", ids(resp.Txs))
	}
}

func TestPendingTxs(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	n.StoreTx(tx("mined", 5, 100))
	n.StoreTx(tx("pend1", 0, 0)) // BlockNumber 0 -> pending
	n.StoreTx(tx("pend2", 0, 0))

	pending, err := n.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	got := map[string]bool{}
	for _, p := range pending {
		got[p.ID] = true
	}
	if len(pending) != 2 || !got["pend1"] || !got["pend2"] {
		t.Fatalf("pending = %v", ids(pending))
	}
}

func TestNetworkIsolation(t *testing.T) {
	db := newTestTxDB(t)
	a := db.NetworkTxDB(netA)
	b := db.NetworkTxDB(netB)

	a.StoreTx(tx("shared", 1, 100))
	b.StoreTx(tx("shared", 2, 200)) // same ID, different network

	ga, _ := a.GetTx("shared")
	gb, _ := b.GetTx("shared")
	if ga.BlockNumber != 1 || gb.BlockNumber != 2 {
		t.Fatalf("network isolation broken: a=%d b=%d", ga.BlockNumber, gb.BlockNumber)
	}

	// Each network's history only sees its own.
	ra, _ := a.GetTxs(&TxHistoryRequest{})
	rb, _ := b.GetTxs(&TxHistoryRequest{})
	if len(ra.Txs) != 1 || len(rb.Txs) != 1 {
		t.Fatalf("history bleed: a=%d b=%d", len(ra.Txs), len(rb.Txs))
	}
}

func TestAuxTableNamespacing(t *testing.T) {
	db := newTestTxDB(t)
	a := db.NetworkTxDB(netA)
	b := db.NetworkTxDB(netB)

	ta, err := a.Table("aux")
	if err != nil {
		t.Fatalf("a.Table: %v", err)
	}
	tb, err := b.Table("aux")
	if err != nil {
		t.Fatalf("b.Table: %v", err)
	}
	if err := ta.Set([]byte("k"), []byte("va"), true); err != nil {
		t.Fatal(err)
	}
	if err := tb.Set([]byte("k"), []byte("vb"), true); err != nil {
		t.Fatal(err)
	}
	va, _ := ta.Get([]byte("k"))
	vb, _ := tb.Get([]byte("k"))
	if !bytes.Equal(va, []byte("va")) || !bytes.Equal(vb, []byte("vb")) {
		t.Fatalf("aux table not isolated: %q %q", va, vb)
	}
}

func TestAddTxIndexUsesRawTx(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	// A wallet-defined index ordering by the first byte of RawTx.
	idx, err := n.AddTxIndex("byraw", func(wt *WalletTransaction) ([]byte, error) {
		if len(wt.RawTx) == 0 {
			return nil, ErrNotIndexed
		}
		return wt.RawTx[:1], nil
	})
	if err != nil {
		t.Fatalf("AddTxIndex: %v", err)
	}

	// Store out of RawTx order.
	store := func(id string, first byte) {
		wt := tx(id, 1, 100)
		wt.RawTx = []byte{first, 0xff}
		if err := n.StoreTx(wt); err != nil {
			t.Fatal(err)
		}
	}
	store("three", 3)
	store("one", 1)
	store("two", 2)
	// One without RawTx -> not indexed.
	n.StoreTx(tx("none", 1, 100))

	var order []string
	err = idx.Iterate(nil, func(it StoreIter) error {
		wt, err := decodeIter(it)
		if err != nil {
			return err
		}
		order = append(order, wt.ID)
		return nil
	}, false, nil)
	if err != nil {
		t.Fatalf("custom index iterate: %v", err)
	}
	if !eq(order, []string{"one", "two", "three"}) {
		t.Fatalf("custom index order = %v", order)
	}
}
