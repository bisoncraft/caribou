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

	"github.com/bisoncraft/caribou/util"
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

func tx(net uint32, id string, blockNum, timestamp uint64) *WalletTransaction {
	return &WalletTransaction{
		Type:        Send,
		ID:          id,
		Amount:      1,
		NetworkID:   net,
		AssetID:     net,
		BlockNumber: blockNum,
		Timestamp:   timestamp,
		Confirmed:   blockNum > 0,
	}
}

// tokenTx is like tx, but stamped with the token's own AssetID, as a token
// wallet sharing its parent network's NetworkTxDB must for
// GetAssetTxs/GetAssetPendingTxs to scope it correctly.
func tokenTx(net uint32, id string, tokenID uint32, blockNum, timestamp uint64) *WalletTransaction {
	wt := tx(net, id, blockNum, timestamp)
	wt.AssetID = tokenID
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

	wt := tx(netA, "abc", 10, 100)
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
	wt := tx(netA, "withraw", 1, 100)
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
		tx(netA, "t100", 1, 100),
		tx(netA, "t300", 3, 300),
		tx(netA, "t200", 2, 200),
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
		tx(netA, "a", 1, 100),
		tx(netA, "b", 1, 200),
		tx(netA, "c", 1, 200),
		tx(netA, "d", 1, 300),
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
	send := tx(netA, "send", 1, 100)
	recv := tx(netA, "recv", 2, 200)
	recv.Type = Receive
	n.StoreTx(send)
	n.StoreTx(recv)

	resp, _ := n.GetTxs(&TxHistoryRequest{IgnoreTypes: []TransactionType{Receive}})
	if !eq(ids(resp.Txs), []string{"send"}) {
		t.Fatalf("ignore types = %v", ids(resp.Txs))
	}
}

// TestGetTxsStopsAtPageFull asserts that once a page fills, the scan does not
// keep decoding subsequent ignored-type rows: a long run of ignored types
// after the page limit must not affect MoreAvailable or panic on a malformed
// trailing record, since it should never be reached.
func TestGetTxsStopsAtPageFull(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	// The default scan is reverse (most-recent-first), so send needs the
	// newest timestamp to be visited first, with the ignored-type records
	// behind it — otherwise the page-full early exit this test is named for
	// is never reached.
	send := tx(netA, "send", 1, 1000)
	n.StoreTx(send)

	for i := range 50 {
		recv := tx(netA, "recv"+string(rune('a'+i)), uint64(i+2), uint64(100+i))
		recv.Type = Receive
		n.StoreTx(recv)
	}

	resp, err := n.GetTxs(&TxHistoryRequest{N: 1, IgnoreTypes: []TransactionType{Receive}})
	if err != nil {
		t.Fatalf("GetTxs: %v", err)
	}
	if !eq(ids(resp.Txs), []string{"send"}) {
		t.Fatalf("got = %v", ids(resp.Txs))
	}
	if resp.MoreAvailable {
		t.Fatalf("MoreAvailable = true, want false: every other record is Receive, which the request ignores")
	}
}

func TestPendingTxs(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	n.StoreTx(tx(netA, "mined", 5, 100))
	n.StoreTx(tx(netA, "pend1", 0, 0)) // BlockNumber 0 -> pending
	n.StoreTx(tx(netA, "pend2", 0, 0))

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

	a.StoreTx(tx(netA, "a-pend", 0, 0)) // BlockNumber 0 -> pending
	b.StoreTx(tx(netB, "b-pend", 0, 0))
	b.StoreTx(tx(netB, "b-mined", 7, 100))

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

	minedUnconfirmed := tx(netA, "mined-unconf", 5, 100)
	minedUnconfirmed.Confirmed = false // mined but not yet finalized
	n.StoreTx(minedUnconfirmed)
	n.StoreTx(tx(netA, "mempool", 0, 0)) // unconfirmed, still in mempool
	n.StoreTx(tx(netA, "final", 9, 200)) // mined and Confirmed -> not pending

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

	n.StoreTx(tx(netA, "native", 1, 100))
	n.StoreTx(tokenTx(netA, "usdc-tx", usdcID, 1, 200))
	n.StoreTx(tokenTx(netA, "usdt-tx", usdtID, 1, 300))

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

	n.StoreTx(tx(netA, "native-pend", 0, 0))
	n.StoreTx(tokenTx(netA, "usdc-pend", usdcID, 0, 0))
	n.StoreTx(tx(netA, "native-mined", 5, 100))

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
		tx(netA, "b1", 1, 100),
		tx(netA, "b2", 2, 200),
		tx(netA, "b3", 0, 0), // unconfirmed
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
	if err := n.StoreTxs([]*WalletTransaction{tx(netA, "b1", 3, 300)}); err != nil {
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

	if err := n.StoreTx(tx(netA, "a", 5, 100)); err != nil {
		t.Fatalf("StoreTx a: %v", err)
	}
	// Storing b with the same index value (BlockNumber 5) evicts a.
	if err := n.StoreTx(tx(netA, "b", 5, 200)); err != nil {
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

	a.StoreTx(tx(netA, "shared", 1, 100))
	b.StoreTx(tx(netB, "shared", 2, 200)) // same ID, different network

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
	if err := ta.Set([]byte("k"), []byte("va"), WithReplace()); err != nil {
		t.Fatal(err)
	}
	if err := tb.Set([]byte("k"), []byte("vb"), WithReplace()); err != nil {
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
		wt := tx(netA, id, 1, 100)
		wt.RawTx = []byte{first, 0xff}
		if err := n.StoreTx(wt); err != nil {
			t.Fatal(err)
		}
	}
	store("three", 3)
	store("one", 1)
	store("two", 2)
	// One without RawTx -> not indexed.
	n.StoreTx(tx(netA, "none", 1, 100))

	var order []string
	err = idx.Iterate(nil, func(wt *WalletTransaction) error {
		order = append(order, wt.ID)
		return nil
	})
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
	txA := &WalletTransaction{Type: Send, ID: "usdcA", Amount: 1, NetworkID: netA, AssetID: usdcA, BlockNumber: 1, Timestamp: 10, Confirmed: true}
	txB := &WalletTransaction{Type: Receive, ID: "usdcB", Amount: 2, NetworkID: netB, AssetID: usdcB, BlockNumber: 1, Timestamp: 30, Confirmed: true}
	if err := db.NetworkTxDB(netA).StoreTx(txA); err != nil {
		t.Fatalf("store usdcA: %v", err)
	}
	if err := db.NetworkTxDB(netB).StoreTx(txB); err != nil {
		t.Fatalf("store usdcB: %v", err)
	}
	if err := db.NetworkTxDB(netA).StoreTx(tx(netA, "btc", 1, 20)); err != nil {
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
		if wt := resp.Txs[i]; wt.AssetID != wantID {
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
	if wt := resp.Txs[0]; wt.AssetID != netA {
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

	// The cursor seeks to the RefID's index entry and the RefID transaction is
	// the first row of the page, matching GetTxs (see TestGetTxs).
	ref := "usdcB"
	refNet := netB
	resp, err = db.GetTxsByTicker("USDC", &TxHistoryRequest{N: 10, Past: true, RefID: &ref, RefNetwork: &refNet})
	if err != nil {
		t.Fatalf("GetTxsByTicker refID: %v", err)
	}
	if got := ids(resp.Txs); !eq(got, []string{"usdcB", "usdcA"}) {
		t.Fatalf("refID page = %v, want [usdcB usdcA] (inclusive)", got)
	}

	// A RefID cannot be resolved to a primary key without its network.
	ref = "usdcB"
	if _, err = db.GetTxsByTicker("USDC", &TxHistoryRequest{N: 10, Past: true, RefID: &ref}); err != ErrMissingRefNetwork {
		t.Fatalf("refID without refNetwork err = %v, want ErrMissingRefNetwork", err)
	}

	// A RefNetwork that does not carry RefID (wrong network / stale cursor)
	// fails the primary-key lookup and reports ErrUnknownTx.
	ref = "usdcB"
	refNet = netA
	if _, err = db.GetTxsByTicker("USDC", &TxHistoryRequest{N: 10, Past: true, RefID: &ref, RefNetwork: &refNet}); err != ErrUnknownTx {
		t.Fatalf("wrong refNetwork err = %v, want ErrUnknownTx", err)
	}
}

// TestPageSizeIsNotTrusted covers the three history readers against a
// TxHistoryRequest.N that came off the wire. N is decoded from the HTTP request
// body and only the ticker is validated above, so it reaches the readers
// unchecked: a negative N once panicked the process in makeslice, and a huge one
// preallocated its full width, which OOMs rather than erroring. Both must be
// served as ordinary requests, and N <= 0 must keep returning everything, which
// Core.TxHistory documents as legal.
func TestPageSizeIsNotTrusted(t *testing.T) {
	db := newTestTxDB(t)
	db.SetTickerResolver(func(assetID uint32) string {
		if assetID == netA {
			return "BTC"
		}
		return ""
	})
	n := db.NetworkTxDB(netA)

	want := []string{"t300", "t200", "t100"}
	for _, wt := range []*WalletTransaction{
		tx(netA, "t100", 1, 100),
		tx(netA, "t200", 2, 200),
		tx(netA, "t300", 3, 300),
	} {
		if err := n.StoreTx(wt); err != nil {
			t.Fatal(err)
		}
	}

	readers := map[string]func(*TxHistoryRequest) (*TxHistoryResponse, error){
		"GetTxs":         n.GetTxs,
		"GetAssetTxs":    func(req *TxHistoryRequest) (*TxHistoryResponse, error) { return n.GetAssetTxs(nil, req) },
		"GetTxsByTicker": func(req *TxHistoryRequest) (*TxHistoryResponse, error) { return db.GetTxsByTicker("BTC", req) },
	}

	for name, read := range readers {
		// Negative N is unbounded, same as the zero value.
		resp, err := read(&TxHistoryRequest{N: -1})
		if err != nil {
			t.Fatalf("%s N=-1: %v", name, err)
		}
		if got := ids(resp.Txs); !eq(got, want) {
			t.Fatalf("%s N=-1 = %v, want %v", name, got, want)
		}
		if resp.MoreAvailable {
			t.Fatalf("%s N=-1 reported more available", name)
		}

		// A page larger than the table is served without reserving its width.
		resp, err = read(&TxHistoryRequest{N: 500_000_000})
		if err != nil {
			t.Fatalf("%s N=500000000: %v", name, err)
		}
		if got := ids(resp.Txs); !eq(got, want) {
			t.Fatalf("%s N=500000000 = %v, want %v", name, got, want)
		}
		if c := cap(resp.Txs); c > maxPrealloc {
			t.Fatalf("%s N=500000000 preallocated cap %d, want <= %d", name, c, maxPrealloc)
		}
	}
}

// TestStoreTxValidation pins the storage contract enforced by StoreTx and
// StoreTxs: NetworkID must match the handle's network, and a zero AssetID is
// only allowable on the network whose own asset ID is zero (BTC).
func TestStoreTxValidation(t *testing.T) {
	db := newTestTxDB(t)
	a := db.NetworkTxDB(netA) // netA == 0
	b := db.NetworkTxDB(netB)

	// Zero AssetID on network 0 is legitimate (it is the network's own ID).
	if err := a.StoreTx(tx(netA, "btc-native", 1, 100)); err != nil {
		t.Fatalf("zero AssetID on network 0 rejected: %v", err)
	}

	// A NetworkID that does not match the handle's network is rejected.
	wrongNet := tx(netA, "wrong-net", 1, 100)
	if err := b.StoreTx(wrongNet); err == nil {
		t.Fatal("mismatched NetworkID accepted")
	}

	// A zero AssetID on a non-zero network is rejected.
	zeroAsset := tx(netB, "zero-asset", 1, 100)
	zeroAsset.AssetID = 0
	if err := b.StoreTx(zeroAsset); err == nil {
		t.Fatal("zero AssetID on non-zero network accepted")
	}

	// StoreTxs applies the same checks, rejecting the whole batch.
	if err := b.StoreTxs([]*WalletTransaction{tx(netB, "ok", 1, 100), zeroAsset}); err == nil {
		t.Fatal("StoreTxs accepted a batch with a zero AssetID")
	}
	if got, _ := b.GetTx("ok"); got != nil {
		t.Fatal("rejected batch left a stored transaction behind")
	}
	if err := b.StoreTxs([]*WalletTransaction{tx(netB, "ok", 1, 100), tokenTx(netB, "tok", 100, 1, 200)}); err != nil {
		t.Fatalf("valid batch rejected: %v", err)
	}
}

// TestIgnoreTypesMoreAvailable pins MoreAvailable semantics under type
// filtering: a page reports more available only when at least one more kept
// transaction exists, not when the remainder is entirely filtered. Since
// RefID paging is inclusive, an all-filtered tail reported as more-available
// would have a paging client refetch the same page forever.
func TestIgnoreTypesMoreAvailable(t *testing.T) {
	db := newTestTxDB(t)
	n := db.NetworkTxDB(netA)

	keep := tx(netA, "keep", 3, 300)
	ignoredOld := tx(netA, "ignored-old", 1, 100)
	ignoredOld.Type = Receive
	if err := n.StoreTx(keep); err != nil {
		t.Fatal(err)
	}
	if err := n.StoreTx(ignoredOld); err != nil {
		t.Fatal(err)
	}

	req := func() *TxHistoryRequest {
		return &TxHistoryRequest{N: 1, Past: true, IgnoreTypes: []TransactionType{Receive}}
	}

	// Everything past the page is filtered: not more available.
	resp, err := n.GetTxs(req())
	if err != nil {
		t.Fatalf("GetTxs: %v", err)
	}
	if !eq(ids(resp.Txs), []string{"keep"}) {
		t.Fatalf("page = %v, want [keep]", ids(resp.Txs))
	}
	if resp.MoreAvailable {
		t.Fatal("MoreAvailable = true with only filtered entries remaining")
	}

	// A kept transaction past the page still reports more available.
	if err := n.StoreTx(tx(netA, "kept-old", 2, 200)); err != nil {
		t.Fatal(err)
	}
	resp, err = n.GetTxs(req())
	if err != nil {
		t.Fatalf("GetTxs: %v", err)
	}
	if !eq(ids(resp.Txs), []string{"keep"}) || !resp.MoreAvailable {
		t.Fatalf("page = %v, MoreAvailable = %v; want [keep], true", ids(resp.Txs), resp.MoreAvailable)
	}
}
