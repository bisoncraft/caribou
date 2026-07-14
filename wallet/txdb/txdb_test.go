// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package txdb

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
)

// newTestTxDB builds a TxDB backed by the build-selected storage engine (Badger
// via util/lexi on desktop, bbolt via util/glossi on mobile). Running the
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

// tokenTx is like tx, but stamped with TokenID, as a token wallet sharing its
// parent network's NetworkTxDB must for GetAssetTxs/GetAssetPendingTxs to
// scope it correctly.
func tokenTx(id string, tokenID uint32, blockNum, timestamp uint64) *WalletTransaction {
	wt := tx(id, blockNum, timestamp)
	wt.TokenID = &tokenID
	return wt
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

func TestGetTxsSameEffTimePagination(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	// b and c share an effective time (200). The networkTime index entry
	// carries the tx id so a RefID seek lands on the exact tx rather than the
	// first sibling sharing that time.
	for _, wt := range []*WalletTransaction{
		tx("a", 1, 100),
		tx("b", 1, 200),
		tx("c", 1, 200),
		tx("d", 1, 300),
	} {
		if err := n.StoreTx(wt); err != nil {
			t.Fatal(err)
		}
	}

	// Default order: most recent first, ties broken by id descending.
	resp, _ := n.GetTxs(&TxHistoryRequest{})
	if !eq(ids(resp.Txs), []string{"d", "c", "b", "a"}) {
		t.Fatalf("default = %v", ids(resp.Txs))
	}

	cases := []struct {
		ref  string
		past bool
		want []string
	}{
		{"c", false, []string{"c", "d"}},      // forward from the later sibling
		{"b", false, []string{"b", "c", "d"}}, // forward from the earlier sibling
		{"b", true, []string{"b", "a"}},       // past from the earlier sibling
		{"c", true, []string{"c", "b", "a"}},  // past from the later sibling
	}
	for _, c := range cases {
		ref := c.ref
		resp, _ := n.GetTxs(&TxHistoryRequest{RefID: &ref, Past: c.past})
		if !eq(ids(resp.Txs), c.want) {
			t.Fatalf("ref=%s past=%v = %v, want %v", c.ref, c.past, ids(resp.Txs), c.want)
		}
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

func TestPendingTxsNetworkScoped(t *testing.T) {
	db := newTestTxDB(t)
	a := db.NetworkTxDB(netA)
	b := db.NetworkTxDB(netB)

	a.StoreTx(tx("a-pend", 0, 0)) // BlockNumber 0 -> pending
	b.StoreTx(tx("b-pend", 0, 0))
	b.StoreTx(tx("b-mined", 7, 100))

	pa, err := a.GetPendingTxs()
	if err != nil {
		t.Fatalf("netA GetPendingTxs: %v", err)
	}
	if !eq(ids(pa), []string{"a-pend"}) {
		t.Fatalf("netA pending = %v, want [a-pend]", ids(pa))
	}

	pb, err := b.GetPendingTxs()
	if err != nil {
		t.Fatalf("netB GetPendingTxs: %v", err)
	}
	if !eq(ids(pb), []string{"b-pend"}) {
		t.Fatalf("netB pending = %v, want [b-pend]", ids(pb))
	}
}

// TestPendingTxsMinedUnconfirmed verifies the pending set means "unconfirmed",
// not "mempool-only": a tx that is mined (BlockNumber > 0) but not yet
// finalized (Confirmed == false) must still be returned as pending, matching
// the per-wallet tx DBs wallets migrate from.
func TestPendingTxsMinedUnconfirmed(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	minedUnconfirmed := tx("mined-unconf", 5, 100)
	minedUnconfirmed.Confirmed = false // mined but not yet finalized
	n.StoreTx(minedUnconfirmed)
	n.StoreTx(tx("mempool", 0, 0)) // unconfirmed, still in mempool
	n.StoreTx(tx("final", 9, 200)) // mined and Confirmed -> not pending

	pending, err := n.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	got := map[string]bool{}
	for _, p := range pending {
		got[p.ID] = true
	}
	if len(pending) != 2 || !got["mined-unconf"] || !got["mempool"] {
		t.Fatalf("pending = %v, want [mined-unconf mempool]", ids(pending))
	}
}

// TestGetAssetTxsScoped guards against a token wallet sharing its parent
// network's NetworkTxDB seeing the parent's (or a sibling token's)
// transactions and vice versa: GetAssetTxs(nil, ...) must return only the
// network's own native-asset records, and GetAssetTxs(&tokenID, ...) must
// return only that token's.
func TestGetAssetTxsScoped(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)
	usdcID, usdtID := uint32(1001), uint32(1002)

	n.StoreTx(tx("native", 1, 100))
	n.StoreTx(tokenTx("usdc-tx", usdcID, 1, 200))
	n.StoreTx(tokenTx("usdt-tx", usdtID, 1, 300))

	nativeResp, err := n.GetAssetTxs(nil, &TxHistoryRequest{})
	if err != nil {
		t.Fatalf("GetAssetTxs(nil): %v", err)
	}
	if !eq(ids(nativeResp.Txs), []string{"native"}) {
		t.Fatalf("native asset txs = %v, want [native]", ids(nativeResp.Txs))
	}

	usdcResp, err := n.GetAssetTxs(&usdcID, &TxHistoryRequest{})
	if err != nil {
		t.Fatalf("GetAssetTxs(usdc): %v", err)
	}
	if !eq(ids(usdcResp.Txs), []string{"usdc-tx"}) {
		t.Fatalf("usdc txs = %v, want [usdc-tx]", ids(usdcResp.Txs))
	}

	usdtResp, err := n.GetAssetTxs(&usdtID, &TxHistoryRequest{})
	if err != nil {
		t.Fatalf("GetAssetTxs(usdt): %v", err)
	}
	if !eq(ids(usdtResp.Txs), []string{"usdt-tx"}) {
		t.Fatalf("usdt txs = %v, want [usdt-tx]", ids(usdtResp.Txs))
	}
}

// TestGetAssetPendingTxsScoped is the pending-set analog of
// TestGetAssetTxsScoped: without it, a native wallet's checkPendingTxs-style
// user-facing queries would see (and could misinterpret/overwrite) a
// sibling token wallet's in-flight transactions, and vice versa.
func TestGetAssetPendingTxsScoped(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)
	usdcID := uint32(1001)

	n.StoreTx(tx("native-pend", 0, 0))
	n.StoreTx(tokenTx("usdc-pend", usdcID, 0, 0))
	n.StoreTx(tx("native-mined", 5, 100))

	nativePending, err := n.GetAssetPendingTxs(nil)
	if err != nil {
		t.Fatalf("GetAssetPendingTxs(nil): %v", err)
	}
	if !eq(ids(nativePending), []string{"native-pend"}) {
		t.Fatalf("native pending = %v, want [native-pend]", ids(nativePending))
	}

	usdcPending, err := n.GetAssetPendingTxs(&usdcID)
	if err != nil {
		t.Fatalf("GetAssetPendingTxs(usdc): %v", err)
	}
	if !eq(ids(usdcPending), []string{"usdc-pend"}) {
		t.Fatalf("usdc pending = %v, want [usdc-pend]", ids(usdcPending))
	}

	// GetPendingTxs (unfiltered) must still return everything mixed
	// together, since e.g. XRP's sequence accounting relies on seeing every
	// asset's pending transactions in the network.
	allPending, err := n.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	got := map[string]bool{}
	for _, p := range allPending {
		got[p.ID] = true
	}
	if len(allPending) != 2 || !got["native-pend"] || !got["usdc-pend"] {
		t.Fatalf("GetPendingTxs = %v, want [native-pend usdc-pend]", ids(allPending))
	}
}

// TestStoreTxsBatch verifies the bulk insert path stores every tx, makes each
// retrievable, and populates the history and pending indexes.
func TestStoreTxsBatch(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	// Empty batch is a no-op.
	if err := n.StoreTxs(nil); err != nil {
		t.Fatalf("StoreTxs(nil): %v", err)
	}

	batch := []*WalletTransaction{
		tx("b1", 1, 100),
		tx("b2", 2, 200),
		tx("b3", 0, 0), // unconfirmed
	}
	if err := n.StoreTxs(batch); err != nil {
		t.Fatalf("StoreTxs: %v", err)
	}

	for _, want := range batch {
		got, err := n.GetTx(want.ID)
		if err != nil || got == nil {
			t.Fatalf("GetTx(%s) = %v, %v", want.ID, got, err)
		}
	}

	resp, err := n.GetTxs(&TxHistoryRequest{})
	if err != nil {
		t.Fatalf("GetTxs: %v", err)
	}
	if len(resp.Txs) != 3 {
		t.Fatalf("history len = %d, want 3", len(resp.Txs))
	}

	pending, err := n.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	if !eq(ids(pending), []string{"b3"}) {
		t.Fatalf("pending = %v, want [b3]", ids(pending))
	}

	// A second batch with overlapping IDs updates in place (replace).
	if err := n.StoreTxs([]*WalletTransaction{tx("b1", 3, 300)}); err != nil {
		t.Fatalf("StoreTxs replace: %v", err)
	}
	got, _ := n.GetTx("b1")
	if got == nil || got.BlockNumber != 3 {
		t.Fatalf("b1 not updated by batch: %+v", got)
	}
}

// TestAddUniqueTxIndexReplace verifies that storing a transaction whose unique
// index entry collides with a different existing transaction deletes that
// existing transaction (the semantic eth relies on for nonce replacement).
func TestAddUniqueTxIndexReplace(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	// Unique index keyed by BlockNumber (fixed 8 bytes).
	if _, err := n.AddUniqueTxIndex("block", func(wt *WalletTransaction) ([]byte, error) {
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, wt.BlockNumber)
		return b, nil
	}); err != nil {
		t.Fatalf("AddUniqueTxIndex: %v", err)
	}

	if err := n.StoreTx(tx("a", 5, 100)); err != nil {
		t.Fatalf("StoreTx a: %v", err)
	}
	// Storing b with the same index value (BlockNumber 5) evicts a.
	if err := n.StoreTx(tx("b", 5, 200)); err != nil {
		t.Fatalf("StoreTx b: %v", err)
	}

	gotA, err := n.GetTx("a")
	if err != nil {
		t.Fatalf("GetTx a: %v", err)
	}
	if gotA != nil {
		t.Fatalf("expected a to be evicted by unique-index conflict, got %+v", gotA)
	}
	gotB, err := n.GetTx("b")
	if err != nil || gotB == nil {
		t.Fatalf("GetTx b = %v, %v; want present", gotB, err)
	}

	// a must also be gone from history (all of its index entries purged).
	resp, err := n.GetTxs(&TxHistoryRequest{})
	if err != nil {
		t.Fatalf("GetTxs: %v", err)
	}
	if !eq(ids(resp.Txs), []string{"b"}) {
		t.Fatalf("history = %v, want [b]", ids(resp.Txs))
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
	err = idx.Iterate(nil, func(wt *WalletTransaction) error {
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

// TestTickerIndex verifies the ticker index merges a ticker group's
// transactions across networks in time order, isolates one ticker from another,
// and paginates.
func TestTickerIndex(t *testing.T) {
	db := newTestTxDB(t)
	db.SetTickerResolver(func(assetID uint32) string {
		switch assetID {
		case netA: // btc native
			return "BTC"
		case netB: // eth native
			return "ETH"
		case 100, 200: // usdc token on two networks
			return "USDC"
		}
		return ""
	})

	usdcA, usdcB := uint32(100), uint32(200)
	// USDC on netA at t=10, USDC on netB at t=30, and a native BTC tx at t=20.
	txA := &WalletTransaction{Type: Send, ID: "usdcA", Amount: 1, BlockNumber: 1, Timestamp: 10, Confirmed: true, TokenID: &usdcA}
	txB := &WalletTransaction{Type: Receive, ID: "usdcB", Amount: 2, BlockNumber: 1, Timestamp: 30, Confirmed: true, TokenID: &usdcB}
	if err := db.NetworkTxDB(netA).StoreTx(txA); err != nil {
		t.Fatalf("store usdcA: %v", err)
	}
	if err := db.NetworkTxDB(netB).StoreTx(txB); err != nil {
		t.Fatalf("store usdcB: %v", err)
	}
	if err := db.NetworkTxDB(netA).StoreTx(tx("btc", 1, 20)); err != nil {
		t.Fatalf("store btc: %v", err)
	}

	// Most-recent-first across both networks; the BTC tx is excluded.
	resp, err := db.GetTxsByTicker("USDC", &TxHistoryRequest{N: 10, Past: true})
	if err != nil {
		t.Fatalf("GetTxsByTicker USDC: %v", err)
	}
	if got := ids(resp.Txs); !eq(got, []string{"usdcB", "usdcA"}) {
		t.Fatalf("USDC order = %v, want [usdcB usdcA]", got)
	}
	// Token transactions are attributed to their token asset ID.
	for i, wantID := range []uint32{usdcB, usdcA} {
		if wt := resp.Txs[i]; wt.AssetID == nil || *wt.AssetID != wantID {
			t.Fatalf("tx %s AssetID = %v, want %d", wt.ID, wt.AssetID, wantID)
		}
	}

	// A different ticker returns only its own transactions.
	resp, err = db.GetTxsByTicker("BTC", &TxHistoryRequest{N: 10, Past: true})
	if err != nil {
		t.Fatalf("GetTxsByTicker BTC: %v", err)
	}
	if got := ids(resp.Txs); !eq(got, []string{"btc"}) {
		t.Fatalf("BTC = %v, want [btc]", got)
	}
	// A native transaction is attributed to its network's native asset.
	if wt := resp.Txs[0]; wt.AssetID == nil || *wt.AssetID != netA {
		t.Fatalf("native tx AssetID = %v, want %d", wt.AssetID, netA)
	}

	// N caps the page and reports more available.
	resp, err = db.GetTxsByTicker("USDC", &TxHistoryRequest{N: 1, Past: true})
	if err != nil {
		t.Fatalf("GetTxsByTicker N=1: %v", err)
	}
	if got := ids(resp.Txs); !eq(got, []string{"usdcB"}) || !resp.MoreAvailable {
		t.Fatalf("paged = %v more=%v, want [usdcB] more=true", got, resp.MoreAvailable)
	}

	// RefID skips forward past the reference in index order.
	ref := "usdcB"
	resp, err = db.GetTxsByTicker("USDC", &TxHistoryRequest{N: 10, Past: true, RefID: &ref})
	if err != nil {
		t.Fatalf("GetTxsByTicker refID: %v", err)
	}
	if got := ids(resp.Txs); !eq(got, []string{"usdcA"}) {
		t.Fatalf("refID page = %v, want [usdcA]", got)
	}

	// A RefID not in this ticker's index (stale cursor, or another ticker's
	// tx) is an error, not an empty final page.
	ref = "btc"
	if _, err = db.GetTxsByTicker("USDC", &TxHistoryRequest{N: 10, Past: true, RefID: &ref}); err != ErrUnknownTx {
		t.Fatalf("foreign refID err = %v, want ErrUnknownTx", err)
	}
}
