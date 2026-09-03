package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/intents"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

func unitInfo(convFactor uint64) *util.UnitInfo {
	return &util.UnitInfo{Conventional: util.Denomination{ConversionFactor: convFactor}}
}

func TestIntentsAmountScaling(t *testing.T) {
	tests := []struct {
		name       string
		atoms      uint64
		convFactor uint64
		decimals   int
		want       string
	}{
		{"btc sats 1:1", 12345678, 1e8, 8, "12345678"},
		{"near drops to yocto", 5_000_000, 1e8, 24, "50000000000000000000000"},
		{"eth gwei to wei", 1_500_000_000, 1e9, 18, "1500000000000000000"},
		{"xrp drops 1:1", 25_000_000, 1e6, 6, "25000000"},
	}
	for _, tt := range tests {
		got, err := toIntentsAmount(tt.atoms, unitInfo(tt.convFactor), tt.decimals)
		if err != nil {
			t.Fatalf("%s: toIntentsAmount: %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: toIntentsAmount = %s, want %s", tt.name, got, tt.want)
		}
		back, err := fromIntentsAmount(got, unitInfo(tt.convFactor), tt.decimals)
		if err != nil {
			t.Fatalf("%s: fromIntentsAmount: %v", tt.name, err)
		}
		if back != tt.atoms {
			t.Errorf("%s: round trip = %d, want %d", tt.name, back, tt.atoms)
		}
	}

	// Truncation on the way back down.
	atoms, err := fromIntentsAmount("50000000000000000009999", unitInfo(1e8), 24)
	if err != nil {
		t.Fatalf("fromIntentsAmount: %v", err)
	}
	if atoms != 5_000_000 {
		t.Errorf("truncation: got %d, want 5000000", atoms)
	}

	// Overflow detection.
	if _, err := fromIntentsAmount("99999999999999999999999999999999", unitInfo(1e8), 8); err == nil {
		t.Errorf("expected overflow error")
	}

	// Non-power-of-ten conversion factor.
	if _, err := toIntentsAmount(1, unitInfo(3), 8); err == nil {
		t.Errorf("expected power-of-ten error")
	}
}

func TestIntentsSwapStates(t *testing.T) {
	for state, terminal := range map[string]bool{
		IntentsStateQuoted:              false,
		IntentsStateDepositSent:         false,
		IntentsStateExpired:             true,
		intents.StatusProcessing:        false,
		intents.StatusKnownDepositTx:    false,
		intents.StatusIncompleteDeposit: false,
		intents.StatusSuccess:           true,
		intents.StatusRefunded:          true,
		intents.StatusFailed:            true,
	} {
		if got := (&IntentsSwap{State: state}).Terminal(); got != terminal {
			t.Errorf("Terminal(%s) = %v, want %v", state, got, terminal)
		}
	}
}

func TestSwapFromStatus(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "intents", "testdata", "status-pending.json"))
	if err != nil {
		t.Fatalf("error reading fixture: %v", err)
	}
	status := new(intents.StatusResponse)
	if err := json.Unmarshal(b, status); err != nil {
		t.Fatalf("error decoding fixture: %v", err)
	}

	// Stub resolver: the fixture's origin is nep141:btc.omft.near (8
	// decimals) and its destination nep141:sol.omft.near (9 decimals). The
	// destination maps to a registered test asset so the atoms conversion
	// (unit-info lookup included) actually runs.
	resolve := func(assetID string) (uint32, *intents.Token, bool) {
		switch assetID {
		case "nep141:btc.omft.near":
			return tUTXOAssetB.ID, &intents.Token{AssetID: assetID, Decimals: 8}, true
		case "nep141:sol.omft.near":
			return tUTXOAssetA.ID, &intents.Token{AssetID: assetID, Decimals: 9}, true
		}
		return 0, nil, false
	}
	swap, err := swapFromStatus(resolve, tUTXOAssetB.ID, "bc1qdepositaddr", "sometxid", status)
	if err != nil {
		t.Fatalf("swapFromStatus: %v", err)
	}
	if !swap.Reconstructed {
		t.Fatalf("record not flagged Reconstructed")
	}
	// PENDING_DEPOSIT maps to the local DEPOSIT_SENT (a deposit tx exists,
	// or reconstruction wouldn't have probed this address).
	if swap.State != IntentsStateDepositSent {
		t.Fatalf("state = %s, want %s", swap.State, IntentsStateDepositSent)
	}
	if swap.OriginAsset != "nep141:btc.omft.near" {
		t.Fatalf("origin asset = %s", swap.OriginAsset)
	}
	if swap.DestAssetID != tUTXOAssetA.ID {
		t.Fatalf("dest asset ID = %d, want %d", swap.DestAssetID, tUTXOAssetA.ID)
	}
	if swap.DepositTxID != "sometxid" {
		t.Fatalf("deposit txid not carried")
	}
	if len(swap.QuoteEnvelope) == 0 {
		t.Fatalf("no quote envelope retained")
	}
	// Both atoms fields must be populated (both test assets use a 1e8
	// conversion factor): 10000000 @ 8 decimals in, 83464781817 @ 9
	// decimals out, truncated to wallet precision.
	if swap.AtomsIn != 10_000_000 {
		t.Fatalf("AtomsIn = %d, want 10000000", swap.AtomsIn)
	}
	if swap.AtomsOut != 8_346_478_181 {
		t.Fatalf("AtomsOut = %d, want 8346478181", swap.AtomsOut)
	}
	// The re-serialized quote's own USD pricing is retained (the fixture's
	// swap details carry only nulls).
	if swap.USDIn != 6493.900000000001 || swap.USDOut != 6480.205660271880 {
		t.Fatalf("quote USD pricing not captured: %f / %f", swap.USDIn, swap.USDOut)
	}
}

// TestSwapFromStatusRealizedFill covers the fill-preference: when the swap
// details report a realized amountOut, the reconstructed record carries it
// (string and atoms) instead of the quoted output. A later poll never
// revisits it — realizedAtomsOut skips details matching the record — so it
// must be right at construction.
func TestSwapFromStatusRealizedFill(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "intents", "testdata", "status-pending.json"))
	if err != nil {
		t.Fatalf("error reading fixture: %v", err)
	}
	status := new(intents.StatusResponse)
	if err := json.Unmarshal(b, status); err != nil {
		t.Fatalf("error decoding fixture: %v", err)
	}
	status.Status = intents.StatusSuccess
	status.SwapDetails.AmountOut = "83000000000"
	resolve := func(assetID string) (uint32, *intents.Token, bool) {
		if assetID == "nep141:sol.omft.near" {
			return tUTXOAssetA.ID, &intents.Token{AssetID: assetID, Decimals: 9}, true
		}
		return 0, nil, false
	}
	swap, err := swapFromStatus(resolve, tUTXOAssetB.ID, "bc1qdepositaddr", "sometxid", status)
	if err != nil {
		t.Fatalf("swapFromStatus: %v", err)
	}
	if swap.AmountOut != "83000000000" {
		t.Fatalf("AmountOut = %s, want the realized fill 83000000000", swap.AmountOut)
	}
	if swap.AtomsOut != 8_300_000_000 {
		t.Fatalf("AtomsOut = %d, want 8300000000", swap.AtomsOut)
	}
	// The origin asset didn't resolve; best-effort leaves zero.
	if swap.AtomsIn != 0 {
		t.Fatalf("AtomsIn = %d, want 0 for an unresolved origin token", swap.AtomsIn)
	}
}

// TestSwapFromStatusUnknownChain covers the DestAssetID sentinel: an
// unresolved destination must be intentsAssetUnknown, never 0 (BTC).
func TestSwapFromStatusUnknownChain(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "intents", "testdata", "status-pending.json"))
	if err != nil {
		t.Fatalf("error reading fixture: %v", err)
	}
	status := new(intents.StatusResponse)
	if err := json.Unmarshal(b, status); err != nil {
		t.Fatalf("error decoding fixture: %v", err)
	}
	resolve := func(string) (uint32, *intents.Token, bool) { return 0, nil, false } // nothing resolves
	swap, err := swapFromStatus(resolve, 0, "bc1qdepositaddr", "sometxid", status)
	if err != nil {
		t.Fatalf("swapFromStatus: %v", err)
	}
	if swap.DestAssetID != intentsAssetUnknown {
		t.Fatalf("unresolved dest = %d, want sentinel %d (must not be 0/BTC)", swap.DestAssetID, intentsAssetUnknown)
	}
	if got := intentsAssetLabel(swap.DestAssetID, swap.DestAsset); got != swap.DestAsset {
		t.Fatalf("unknown asset label = %q, want the Intents asset ID %q", got, swap.DestAsset)
	}
}

// TestSwapFromStatusDeadlineFallback covers the unparseable-deadline
// path: a zero Deadline on a reconstructed record would read as long past
// and applyTimeout would immediately and terminally fail the swap.
func TestSwapFromStatusDeadlineFallback(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "intents", "testdata", "status-pending.json"))
	if err != nil {
		t.Fatalf("error reading fixture: %v", err)
	}
	resolve := func(string) (uint32, *intents.Token, bool) { return 0, nil, false }
	for _, deadline := range []string{"", "not-a-timestamp", "2026-07-18 12:00:00"} {
		status := new(intents.StatusResponse)
		if err := json.Unmarshal(b, status); err != nil {
			t.Fatalf("error decoding fixture: %v", err)
		}
		status.QuoteResponse.Quote.Deadline = deadline
		swap, err := swapFromStatus(resolve, 0, "bc1qdepositaddr", "sometxid", status)
		if err != nil {
			t.Fatalf("swapFromStatus(deadline=%q): %v", deadline, err)
		}
		if swap.Deadline.IsZero() {
			t.Fatalf("deadline %q produced a zero Deadline", deadline)
		}
		if !swap.Deadline.After(time.Now()) {
			t.Fatalf("deadline %q fallback %v is not in the future", deadline, swap.Deadline)
		}
	}
}

// newTestIntentsEngine builds a swap engine over the rig's Core and stub
// db. The client is left nil: the state-machine paths under test
// (mergeStatus, applyTimeout, storeReconstructed) make no network calls.
func newTestIntentsEngine(rig *testRig) *intentsEngine {
	return &intentsEngine{
		c:       rig.core,
		log:     tLogger,
		db:      rig.db,
		ctx:     rig.core.ctx,
		trigger: make(chan struct{}, 1),
	}
}

// subscribeNotes taps the rig Core's notification fan-out.
func subscribeNotes(rig *testRig) chan Notification {
	ch := make(chan Notification, 16)
	rig.core.noteMtx.Lock()
	rig.core.noteChans[1] = ch
	rig.core.noteMtx.Unlock()
	return ch
}

func testSwapRecord(depositAddress, state string) *IntentsSwap {
	return &IntentsSwap{
		DepositAddress: depositAddress,
		State:          state,
		OriginAssetID:  0,
		DestAssetID:    501,
		OriginAsset:    "nep141:btc.omft.near",
		DestAsset:      "nep141:sol.omft.near",
		AtomsIn:        100_000,
		AmountIn:       "100000",
		AmountOut:      "83000000000",
		AtomsOut:       83_000_000_000,
		Deadline:       time.Now().Add(time.Hour),
		CreatedAt:      time.Now(),
	}
}

func TestIntentsMergeStatus(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	eng := newTestIntentsEngine(rig)

	if err := eng.db.StoreIntentsSwap(testSwapRecord("addr", IntentsStateDepositSent)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A genuine state change is applied, persisted, and noted. The service's
	// execution-time USD pricing supersedes any quote-time values.
	status := &intents.StatusResponse{
		Status: intents.StatusProcessing,
		SwapDetails: intents.SwapDetails{
			NearTxHashes: []string{"neartx1"},
			AmountInUsd:  "115.25",
		},
	}
	updated, note := eng.mergeStatus("addr", status, 0, false, 0, 0)
	if updated == nil || note == nil {
		t.Fatalf("state change not applied: swap %v note %v", updated, note)
	}
	if updated.State != intents.StatusProcessing || len(updated.NearTxHashes) != 1 {
		t.Fatalf("bad merge: %+v", updated)
	}
	if updated.USDIn != 115.25 {
		t.Fatalf("details USD pricing not applied: %f", updated.USDIn)
	}
	if stored, _ := eng.db.IntentsSwap("addr"); stored.State != intents.StatusProcessing {
		t.Fatalf("merge not persisted: %s", stored.State)
	}

	// The same status again is not a change.
	if updated, note = eng.mergeStatus("addr", status, 0, false, 0, 0); updated != nil || note != nil {
		t.Fatalf("no-op status produced an update")
	}

	// A realized fill replaces the quoted output.
	status = &intents.StatusResponse{
		Status: intents.StatusSuccess,
		SwapDetails: intents.SwapDetails{
			NearTxHashes:        []string{"neartx1"},
			AmountOut:           "82500000000",
			AmountOutUsd:        "114.80",
			DestinationChainTxs: []intents.TxHashRef{{Hash: "soltx"}},
		},
	}
	updated, note = eng.mergeStatus("addr", status, 82_500_000_000, true, 0, 0)
	if updated == nil || note == nil {
		t.Fatalf("success status not applied")
	}
	if updated.AmountOut != "82500000000" || updated.AtomsOut != 82_500_000_000 {
		t.Fatalf("realized fill not recorded: %s / %d", updated.AmountOut, updated.AtomsOut)
	}
	if updated.USDOut != 114.80 {
		t.Fatalf("realized USD pricing not recorded: %f", updated.USDOut)
	}
	if len(updated.DestinationTxs) != 1 || updated.DestinationTxs[0] != "soltx" {
		t.Fatalf("destination txs not recorded: %v", updated.DestinationTxs)
	}

	// A terminal record is never regressed, whatever the service says.
	status = &intents.StatusResponse{Status: intents.StatusProcessing}
	if updated, note = eng.mergeStatus("addr", status, 0, false, 0, 0); updated != nil || note != nil {
		t.Fatalf("terminal record was regressed")
	}
	if stored, _ := eng.db.IntentsSwap("addr"); stored.State != intents.StatusSuccess {
		t.Fatalf("terminal record changed in db: %s", stored.State)
	}

	// A refund records the origin-chain refund transactions.
	if err := eng.db.StoreIntentsSwap(testSwapRecord("addr2", IntentsStateDepositSent)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	status = &intents.StatusResponse{
		Status: intents.StatusRefunded,
		SwapDetails: intents.SwapDetails{
			RefundedAmount: "100000",
			RefundReason:   "no solver",
			OriginChainTxs: []intents.TxHashRef{{Hash: "refundtx"}},
		},
	}
	updated, _ = eng.mergeStatus("addr2", status, 0, false, 95_000, 61.05)
	if updated == nil || updated.State != intents.StatusRefunded {
		t.Fatalf("refund not applied: %+v", updated)
	}
	if len(updated.RefundTxs) != 1 || updated.RefundTxs[0] != "refundtx" ||
		updated.RefundedAmount != "100000" || updated.RefundReason != "no solver" {
		t.Fatalf("refund details not recorded: %+v", updated)
	}
	if updated.USDRefunded != 61.05 {
		t.Fatalf("refund USD value not recorded: %f", updated.USDRefunded)
	}
	if updated.RefundedAtoms != 95_000 {
		t.Fatalf("refund atoms not recorded: %d", updated.RefundedAtoms)
	}

	// The service's own refund pricing wins over the derived fallback.
	if err := eng.db.StoreIntentsSwap(testSwapRecord("addr3", IntentsStateDepositSent)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	status.SwapDetails.RefundedAmountUsd = "60.90"
	updated, _ = eng.mergeStatus("addr3", status, 0, false, 95_000, 61.05)
	if updated == nil || updated.USDRefunded != 60.90 {
		t.Fatalf("service refund pricing not preferred: %+v", updated)
	}
}

func TestIntentsApplyTimeout(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	eng := newTestIntentsEngine(rig)
	notes := subscribeNotes(rig)

	expired := time.Now().Add(-(intentsExpiryGrace + time.Minute))

	drainNotes := func() (n int) {
		for {
			select {
			case <-notes:
				n++
			default:
				return n
			}
		}
	}

	// A missing record is a no-op.
	eng.applyTimeout("nonexistent")

	// A zero deadline can never prove a timeout: the record must survive
	// untouched (the reconstruction-fallback guarantee).
	zero := testSwapRecord("zero", IntentsStateDepositSent)
	zero.Deadline = time.Time{}
	_ = eng.db.StoreIntentsSwap(zero)
	eng.applyTimeout("zero")
	if stored, _ := eng.db.IntentsSwap("zero"); stored.State != IntentsStateDepositSent {
		t.Fatalf("zero-deadline record timed out to %s", stored.State)
	}

	// A live deadline is not a timeout.
	_ = eng.db.StoreIntentsSwap(testSwapRecord("live", IntentsStateQuoted))
	eng.applyTimeout("live")
	if stored, _ := eng.db.IntentsSwap("live"); stored.State != IntentsStateQuoted {
		t.Fatalf("live record timed out to %s", stored.State)
	}
	if n := drainNotes(); n != 0 {
		t.Fatalf("%d notes for non-timeouts", n)
	}

	// An unfunded quote past its deadline expires.
	quoted := testSwapRecord("quoted", IntentsStateQuoted)
	quoted.Deadline = expired
	_ = eng.db.StoreIntentsSwap(quoted)
	eng.applyTimeout("quoted")
	if stored, _ := eng.db.IntentsSwap("quoted"); stored.State != IntentsStateExpired {
		t.Fatalf("quoted record = %s, want EXPIRED", stored.State)
	}
	if n := drainNotes(); n != 1 {
		t.Fatalf("%d notes for quote expiry, want 1", n)
	}

	// A sent deposit the service never observed fails, with the reason
	// recorded.
	sent := testSwapRecord("sent", IntentsStateDepositSent)
	sent.Deadline = expired
	_ = eng.db.StoreIntentsSwap(sent)
	eng.applyTimeout("sent")
	stored, _ := eng.db.IntentsSwap("sent")
	if stored.State != intents.StatusFailed {
		t.Fatalf("sent record = %s, want FAILED", stored.State)
	}
	if stored.RefundReason == "" {
		t.Fatalf("no refund reason recorded")
	}
	if n := drainNotes(); n != 1 {
		t.Fatalf("%d notes for deposit timeout, want 1", n)
	}

	// A terminal record past its deadline is left alone.
	done := testSwapRecord("done", intents.StatusSuccess)
	done.Deadline = expired
	_ = eng.db.StoreIntentsSwap(done)
	eng.applyTimeout("done")
	if stored, _ := eng.db.IntentsSwap("done"); stored.State != intents.StatusSuccess {
		t.Fatalf("terminal record timed out to %s", stored.State)
	}
}

func TestIntentsCaptureDepositFee(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	eng := newTestIntentsEngine(rig)
	notes := subscribeNotes(rig)

	// A stub catalog endpoint prices BTC. The price is mutable so the
	// USD-backfill and no-repricing rules can be exercised; setPrice drops
	// the engine's cached catalog to force a refetch, which the TTL would
	// otherwise prevent.
	var priceMtx sync.Mutex
	price := 0.0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		priceMtx.Lock()
		defer priceMtx.Unlock()
		fmt.Fprintf(w, `[{"assetId":"nep141:btc.omft.near","decimals":8,"blockchain":"btc","symbol":"BTC","price":%f,"contractAddress":""}]`, price)
	}))
	defer srv.Close()
	client, err := intents.NewClient(&intents.Config{BaseURL: srv.URL}, tLogger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	eng.client = client
	setPrice := func(p float64) {
		priceMtx.Lock()
		price = p
		priceMtx.Unlock()
		eng.catMtx.Lock()
		eng.catalog = nil
		eng.catMtx.Unlock()
	}
	// feeUSDAt mirrors usdValue's runtime float operations, so equality
	// checks are exact.
	feeUSDAt := func(fee uint64, price float64) float64 {
		return float64(fee) / float64(uint64(1e8)) * price
	}

	xcw, tWallet := newTWallet(0)
	rig.core.wallets[0] = xcw

	drainNotes := func() (n int) {
		for {
			select {
			case <-notes:
				n++
			default:
				return n
			}
		}
	}

	swap := testSwapRecord("addr", IntentsStateDepositSent)
	swap.DepositTxID = "deposittx"
	if err := eng.db.StoreIntentsSwap(swap); err != nil {
		t.Fatalf("seed: %v", err)
	}
	reload := func() *IntentsSwap {
		s, err := eng.db.IntentsSwap("addr")
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		return s
	}

	// No wallet record for the tx yet: nothing captured, nothing noted.
	eng.captureDepositFee(tCtx, reload())
	if fee := reload().DepositTxFee; fee != 0 {
		t.Fatalf("fee %d captured with no wallet tx record", fee)
	}
	if n := drainNotes(); n != 0 {
		t.Fatalf("%d notes for a no-op capture", n)
	}

	// The wallet reports a fee (an estimate, for an EVM-style wallet) while
	// no USD price is published: the fee is captured without a USD value.
	tWallet.walletTx = &txdb.WalletTransaction{ID: "deposittx", Fees: 300}
	eng.captureDepositFee(tCtx, reload())
	if s := reload(); s.DepositTxFee != 300 || s.USDDepositFee != 0 {
		t.Fatalf("priceless capture: fee %d, USD %f", s.DepositTxFee, s.USDDepositFee)
	}
	if n := drainNotes(); n != 1 {
		t.Fatalf("%d notes for a fee capture, wanted 1", n)
	}

	// A price appearing later backfills the missing USD value once.
	setPrice(60_000)
	eng.captureDepositFee(tCtx, reload())
	if s := reload(); s.USDDepositFee != feeUSDAt(300, 60_000) {
		t.Fatalf("USD not backfilled: %f", s.USDDepositFee)
	}
	if n := drainNotes(); n != 1 {
		t.Fatalf("%d notes for a USD backfill, wanted 1", n)
	}

	// An unchanged fee with a captured USD value is a no-op — the record
	// keeps the rate near the fee event, never repricing on price moves.
	setPrice(65_000)
	eng.captureDepositFee(tCtx, reload())
	if s := reload(); s.USDDepositFee != feeUSDAt(300, 60_000) {
		t.Fatalf("captured USD value repriced: %f", s.USDDepositFee)
	}
	if n := drainNotes(); n != 0 {
		t.Fatalf("%d notes for an unchanged fee", n)
	}

	// The wallet refines the fee with the receipt: re-captured and repriced
	// at the current rate.
	tWallet.walletTx.Fees = 250
	eng.captureDepositFee(tCtx, reload())
	if s := reload(); s.DepositTxFee != 250 || s.USDDepositFee != feeUSDAt(250, 65_000) {
		t.Fatalf("refined fee not captured: fee %d, USD %f", s.DepositTxFee, s.USDDepositFee)
	}
	if n := drainNotes(); n != 1 {
		t.Fatalf("%d notes for a fee refinement, wanted 1", n)
	}

	// A lookup error leaves the recorded fee alone.
	tWallet.walletTxErr = errors.New("test error")
	eng.captureDepositFee(tCtx, reload())
	if fee := reload().DepositTxFee; fee != 250 {
		t.Fatalf("fee changed on lookup error: %d", fee)
	}
	tWallet.walletTxErr = nil

	// A record with no deposit txid never queries the wallet.
	noTx := testSwapRecord("addr2", IntentsStateQuoted)
	if err := eng.db.StoreIntentsSwap(noTx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	eng.captureDepositFee(tCtx, noTx)
	if s, _ := eng.db.IntentsSwap("addr2"); s.DepositTxFee != 0 {
		t.Fatalf("fee %d captured with no deposit txid", s.DepositTxFee)
	}
}

func TestIntentsStoreReconstructed(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	eng := newTestIntentsEngine(rig)

	rebuilt := testSwapRecord("addr", IntentsStateDepositSent)
	rebuilt.Reconstructed = true

	stored, err := eng.storeReconstructed(rebuilt)
	if err != nil || !stored {
		t.Fatalf("first store: stored=%v err=%v", stored, err)
	}

	// A live record is authoritative: a second reconstruction for the same
	// deposit address must not clobber it.
	live, _ := eng.db.IntentsSwap("addr")
	live.State = intents.StatusProcessing
	live.Reconstructed = false
	_ = eng.db.StoreIntentsSwap(live)

	stored, err = eng.storeReconstructed(rebuilt)
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	if stored {
		t.Fatalf("reconstruction clobbered a live record")
	}
	if got, _ := eng.db.IntentsSwap("addr"); got.State != intents.StatusProcessing || got.Reconstructed {
		t.Fatalf("live record modified: %+v", got)
	}
}

func TestIntentsDeadlines(t *testing.T) {
	if d := intentsDeadline(0); d != 2*time.Hour {
		t.Errorf("btc deadline %v", d)
	}
	if d := intentsDeadline(501); d != 45*time.Minute {
		t.Errorf("sol default deadline %v", d)
	}
}

// TestIntentsTokenResolution covers resolveIntentsAsset and
// verifyQuoteAsset for chain natives and registered tokens, using a
// catalog fetched from a stub tokens endpoint.
func TestIntentsTokenResolution(t *testing.T) {
	const polID = 966
	usdcPolygonID, _ := util.BipSymbolID("usdc.polygon")
	// Mixed case on one side, to prove contract matching is case-folded.
	const contract = "0xDb7d6ab1f17c6b31909aE466702703dAEf9269cf"
	const nativeIntentsID = "nep245:v2_1.omni.hot.tg:137_111"
	usdcIntentsID := "nep141:pol-" + strings.ToLower(contract) + ".omft.near"

	asset.Register(polID, &tDriver{decodedCoinID: "pol-coin", winfo: tWalletInfo}, true)
	asset.RegisterToken(usdcPolygonID, &util.Token{
		NetworkID: polID,
		Name:      "USDC",
		UnitInfo:  util.UnitInfo{Conventional: util.Denomination{Unit: "USDC", ConversionFactor: 1e6}},
	}, &asset.WalletDefinition{}, map[util.Network]string{util.Simnet: contract}, true)
	asset.SetNetwork(util.Simnet)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[
			{"assetId":%q,"decimals":18,"blockchain":"pol","symbol":"POL","contractAddress":""},
			{"assetId":%q,"decimals":6,"blockchain":"pol","symbol":"USDC","contractAddress":%q}
		]`, nativeIntentsID, usdcIntentsID, contract)
	}))
	defer srv.Close()
	client, err := intents.NewClient(&intents.Config{BaseURL: srv.URL}, tLogger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cat, err := client.Catalog(tCtx)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	if tok := resolveIntentsAsset(cat, polID); tok == nil || tok.AssetID != nativeIntentsID {
		t.Fatalf("native resolution = %+v", tok)
	}
	if tok := resolveIntentsAsset(cat, usdcPolygonID); tok == nil || tok.AssetID != usdcIntentsID {
		t.Fatalf("token resolution = %+v", tok)
	}
	if tok := resolveIntentsAsset(cat, 12345); tok != nil {
		t.Fatalf("unknown asset resolved to %+v", tok)
	}

	if err := verifyQuoteAsset(cat, nativeIntentsID, polID); err != nil {
		t.Errorf("native echo rejected: %v", err)
	}
	if err := verifyQuoteAsset(cat, usdcIntentsID, usdcPolygonID); err != nil {
		t.Errorf("token echo rejected: %v", err)
	}
	// A native echo for a token request (or vice versa) must fail: that is
	// exactly the redirect verifyQuoteAssets exists to catch.
	if err := verifyQuoteAsset(cat, nativeIntentsID, usdcPolygonID); err == nil {
		t.Error("native echo accepted for token request")
	}
	if err := verifyQuoteAsset(cat, usdcIntentsID, polID); err == nil {
		t.Error("token echo accepted for native request")
	}
	if err := verifyQuoteAsset(cat, "nep141:unknown.omft.near", polID); err == nil {
		t.Error("unknown echo accepted")
	}

	if chainID := intentsChainID(usdcPolygonID); chainID != polID {
		t.Errorf("intentsChainID(token) = %d", chainID)
	}
}
