// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// newTestCoinManager returns a coinManager whose RPC-facing hooks all fail
// unless overridden.
func newTestCoinManager() *coinManager {
	return newCoinManager(
		tLogger,
		MainNetParams,
		func(val, lots, maxFeeRate uint64, reportChange bool) enoughFunc {
			return reserveEnough(val)
		},
		func() ([]*listUnspentResult, error) { return nil, notImplErr("listUnspent") },
		func(unlock bool, ops []*btc.Output) error { return notImplErr("lockUnspent") },
		func() ([]*rpcOutpoint, error) { return nil, notImplErr("listLocked") },
		func(*chainhash.Hash, uint32) (*wire.TxOut, error) { return nil, notImplErr("getTxOut") },
		func(a btcutil.Address) (string, error) { return a.String(), nil },
	)
}

func newTestListUnspentResult(txid string, vout uint32, amt float64, confs uint32) *listUnspentResult {
	return &listUnspentResult{
		TxID:          txid,
		Vout:          vout,
		Address:       tAddr,
		ScriptPubKey:  tP2PKH,
		Amount:        amt,
		Confirmations: confs,
		Spendable:     true,
	}
}

func TestConvertUnspent(t *testing.T) {
	good1 := newTestListUnspentResult(tTxID, 0, 1, 5)
	good2 := newTestListUnspentResult(tTxID, 1, 2, 10)

	f := false
	unsafe := newTestListUnspentResult(tTxID, 2, 1, 5)
	unsafe.SafePtr = &f

	unspendable := newTestListUnspentResult(tTxID, 3, 1, 5)
	unspendable.Spendable = false

	lowConf := newTestListUnspentResult(tTxID, 4, 1, 0)

	unsupported := newTestListUnspentResult(tTxID, 5, 1, 5)
	unsupported.ScriptPubKey = []byte{0x6a, 0x00} // OP_RETURN: unsupported script

	utxos, utxoMap, sum, err := convertUnspent(1, []*listUnspentResult{good1, good2, unsafe, unspendable, lowConf, unsupported}, MainNetParams)
	if err != nil {
		t.Fatalf("convertUnspent error: %v", err)
	}
	if len(utxos) != 2 {
		t.Fatalf("expected 2 spendable utxos, got %d", len(utxos))
	}
	if sum != toSatoshi(1)+toSatoshi(2) {
		t.Fatalf("wrong sum: %d", sum)
	}
	if len(utxoMap) != 2 {
		t.Fatalf("expected 2 entries in utxoMap, got %d", len(utxoMap))
	}
	// Sorted ascending by amount.
	if utxos[0].Amount != toSatoshi(1) || utxos[1].Amount != toSatoshi(2) {
		t.Fatalf("utxos not sorted ascending: %+v, %+v", utxos[0], utxos[1])
	}

	// Bad txid.
	bad := newTestListUnspentResult("not-a-txid", 0, 1, 5)
	if _, _, _, err := convertUnspent(1, []*listUnspentResult{bad}, MainNetParams); err == nil {
		t.Fatalf("expected error for bad txid")
	}
}

func newCompositeUTXO(txid string, vout uint32, amt uint64, confs uint32) *compositeUTXO {
	h, _ := chainhash.NewHashFromStr(txid)
	return &compositeUTXO{
		UTxO: &btc.UTxO{
			TxHash:  h,
			Vout:    vout,
			Address: tAddr,
			Amount:  amt,
		},
		Confs: confs,
		Input: &btc.SpendInfo{},
	}
}

func TestTryFund(t *testing.T) {
	utxos := []*compositeUTXO{
		newCompositeUTXO(tTxID, 0, 1e8, 1),
		newCompositeUTXO(tTxID, 1, 2e8, 1),
		newCompositeUTXO(tTxID, 2, 5e8, 1),
	}
	enough := reserveEnough(3e8)

	sum, _, _, coins, fundingCoins, _, spents, err := tryFund(utxos, enough)
	if err != nil {
		t.Fatalf("tryFund error: %v", err)
	}
	if sum < 3e8 {
		t.Fatalf("insufficient sum: %d", sum)
	}
	if len(coins) == 0 || len(fundingCoins) == 0 || len(spents) == 0 {
		t.Fatalf("expected non-empty results")
	}

	// Not enough funds available at all.
	if _, _, _, _, _, _, _, err := tryFund(utxos, reserveEnough(100e8)); err == nil {
		t.Fatalf("expected error for insufficient funds")
	}

	// Falls back to 0-conf when no confirmed UTXOs satisfy the request.
	zeroConfUTXOs := []*compositeUTXO{newCompositeUTXO(tTxID, 0, 5e8, 0)}
	sum, _, _, _, _, _, _, err = tryFund(zeroConfUTXOs, reserveEnough(1e8))
	if err != nil {
		t.Fatalf("tryFund (0-conf fallback) error: %v", err)
	}
	if sum < 1e8 {
		t.Fatalf("insufficient sum from 0-conf fallback: %d", sum)
	}
}

func TestCoinManagerSpendableUTXOs(t *testing.T) {
	cm := newTestCoinManager()
	cm.listUnspent = func() ([]*listUnspentResult, error) {
		return []*listUnspentResult{newTestListUnspentResult(tTxID, 0, 1, 5)}, nil
	}

	utxos, utxoMap, sum, err := cm.SpendableUTXOs(1)
	if err != nil {
		t.Fatalf("SpendableUTXOs error: %v", err)
	}
	if len(utxos) != 1 || len(utxoMap) != 1 || sum != toSatoshi(1) {
		t.Fatalf("unexpected result: utxos=%d map=%d sum=%d", len(utxos), len(utxoMap), sum)
	}

	// listUnspent error propagates.
	cm.listUnspent = func() ([]*listUnspentResult, error) { return nil, errTest }
	if _, _, _, err := cm.SpendableUTXOs(1); err == nil {
		t.Fatalf("expected error from listUnspent failure")
	}

	// A UTXO already tracked as locked is filtered out and relocked.
	res := newTestListUnspentResult(tTxID, 0, 1, 5)
	pt := btc.NewOutPoint(tTxHash, 0)
	cm.lockedOutputs[pt] = &btc.UTxO{TxHash: tTxHash, Vout: 0, Amount: toSatoshi(1)}
	var relocked bool
	cm.listUnspent = func() ([]*listUnspentResult, error) { return []*listUnspentResult{res}, nil }
	cm.lockUnspent = func(unlock bool, ops []*btc.Output) error {
		relocked = true
		return nil
	}
	utxos, _, _, err = cm.SpendableUTXOs(1)
	if err != nil {
		t.Fatalf("SpendableUTXOs (relock) error: %v", err)
	}
	if len(utxos) != 0 {
		t.Fatalf("expected known locked coin to be filtered out, got %d", len(utxos))
	}
	if !relocked {
		t.Fatalf("expected wallet to be asked to relock the known coin")
	}
}

func TestCoinManagerFundAndFundWithUTXOs(t *testing.T) {
	cm := newTestCoinManager()
	cm.listUnspent = func() ([]*listUnspentResult, error) {
		return []*listUnspentResult{
			newTestListUnspentResult(tTxID, 0, 1, 5),
			newTestListUnspentResult(tTxID, 1, 2, 5),
		}, nil
	}
	var locked bool
	cm.lockUnspent = func(unlock bool, ops []*btc.Output) error {
		locked = true
		return nil
	}

	coins, fundingCoins, spents, _, _, sum, err := cm.Fund(0, 1, true, reserveEnough(toSatoshi(1)))
	if err != nil {
		t.Fatalf("Fund error: %v", err)
	}
	if sum < toSatoshi(1) || len(coins) == 0 || len(fundingCoins) == 0 || len(spents) == 0 {
		t.Fatalf("unexpected Fund result")
	}
	if !locked {
		t.Fatalf("expected lockUnspent to be called")
	}

	// Insufficient funds.
	if _, _, _, _, _, _, err := cm.Fund(0, 1, false, reserveEnough(100e8)); err == nil {
		t.Fatalf("expected insufficient-funds error")
	}

	// spendableUTXOs error propagates.
	cm.listUnspent = func() ([]*listUnspentResult, error) { return nil, errTest }
	if _, _, _, _, _, _, err := cm.Fund(0, 1, false, reserveEnough(toSatoshi(1))); err == nil {
		t.Fatalf("expected error from spendableUTXOs failure")
	}
}

func TestCoinManagerFundWithUTXOsKeepReserve(t *testing.T) {
	cm := newTestCoinManager()
	utxos := []*compositeUTXO{
		newCompositeUTXO(tTxID, 0, 1e8, 1),
		newCompositeUTXO(tTxID, 1, 5e8, 1),
	}
	cm.lockUnspent = func(unlock bool, ops []*btc.Output) error { return nil }

	coins, fundingCoins, spents, _, _, sum, err := cm.FundWithUTXOs(utxos, toSatoshi(1), false, reserveEnough(toSatoshi(2)))
	if err != nil {
		t.Fatalf("FundWithUTXOs error: %v", err)
	}
	if sum < toSatoshi(2) || len(coins) == 0 || len(fundingCoins) == 0 || len(spents) == 0 {
		t.Fatalf("unexpected FundWithUTXOs result")
	}

	// lockUnspent failure surfaces as an error.
	cm.lockUnspent = func(unlock bool, ops []*btc.Output) error { return errTest }
	if _, _, _, _, _, _, err := cm.FundWithUTXOs(utxos, 0, true, reserveEnough(toSatoshi(2))); err == nil {
		t.Fatalf("expected error from lockUnspent failure")
	}

	// Setting aside enough UTXOs to respect the keep reserve leaves too little
	// to fund the order, but there's just enough to fund the order if the
	// keep reserve is violated. The keep reserve takes priority: this must be
	// reported as insufficient balance rather than silently spending into the
	// reserve.
	tightUTXOs := []*compositeUTXO{
		newCompositeUTXO(tTxID, 0, toSatoshi(1), 1),
		newCompositeUTXO(tTxID, 1, toSatoshi(1), 1),
		newCompositeUTXO(tTxID, 2, toSatoshi(1), 1),
	}
	cm.lockUnspent = func(unlock bool, ops []*btc.Output) error { return nil }
	if _, _, _, _, _, _, err := cm.FundWithUTXOs(tightUTXOs, toSatoshi(2), false, reserveEnough(toSatoshi(1.5))); err != asset.ErrInsufficientBalance {
		t.Fatalf("expected asset.ErrInsufficientBalance, got %v", err)
	}
}

func TestCoinManagerReturnCoins(t *testing.T) {
	cm := newTestCoinManager()

	// Return all: nil argument.
	var unlockedAll bool
	cm.lockUnspent = func(unlock bool, ops []*btc.Output) error {
		if !unlock || ops != nil {
			t.Errorf("expected unlock-all call, got unlock=%v ops=%v", unlock, ops)
		}
		unlockedAll = true
		return nil
	}
	cm.lockedOutputs[btc.NewOutPoint(tTxHash, 0)] = &btc.UTxO{}
	if err := cm.ReturnCoins(nil); err != nil {
		t.Fatalf("ReturnCoins(nil) error: %v", err)
	}
	if !unlockedAll || len(cm.lockedOutputs) != 0 {
		t.Fatalf("expected all coins unlocked and map cleared")
	}

	// Empty (non-nil) slice is an error.
	if err := cm.ReturnCoins(asset.Coins{}); err == nil {
		t.Fatalf("expected error for empty coins slice")
	}

	// Specific coins.
	coin := btc.NewOutput(tTxHash, 0, toSatoshi(1))
	cm.lockedOutputs[coin.Pt] = &btc.UTxO{}
	var unlockedSpecific []*btc.Output
	cm.lockUnspent = func(unlock bool, ops []*btc.Output) error {
		unlockedSpecific = ops
		return nil
	}
	if err := cm.ReturnCoins(asset.Coins{coin}); err != nil {
		t.Fatalf("ReturnCoins error: %v", err)
	}
	if len(unlockedSpecific) != 1 || cm.lockedOutputs[coin.Pt] != nil {
		t.Fatalf("expected specific coin to be unlocked and removed")
	}

	// lockUnspent failure propagates.
	cm.lockUnspent = func(unlock bool, ops []*btc.Output) error { return errTest }
	if err := cm.ReturnCoins(asset.Coins{coin}); err == nil {
		t.Fatalf("expected error from lockUnspent failure")
	}
}

func TestCoinManagerReturnOutPoint(t *testing.T) {
	cm := newTestCoinManager()
	pt := btc.NewOutPoint(tTxHash, 0)
	cm.lockedOutputs[pt] = &btc.UTxO{}

	cm.lockUnspent = func(unlock bool, ops []*btc.Output) error { return nil }
	if err := cm.ReturnOutPoint(pt); err != nil {
		t.Fatalf("ReturnOutPoint error: %v", err)
	}
	if cm.lockedOutputs[pt] != nil {
		t.Fatalf("expected outpoint to be removed from locked map")
	}

	cm.lockUnspent = func(unlock bool, ops []*btc.Output) error { return errTest }
	if err := cm.ReturnOutPoint(pt); err == nil {
		t.Fatalf("expected error from lockUnspent failure")
	}
}

func TestCoinManagerLockHelpers(t *testing.T) {
	cm := newTestCoinManager()
	pt := btc.NewOutPoint(tTxHash, 0)
	utxo := &btc.UTxO{TxHash: tTxHash, Vout: 0}

	cm.LockUTXOs([]*btc.UTxO{utxo})
	if cm.LockedOutput(pt) != utxo {
		t.Fatalf("expected utxo to be locked")
	}

	cm.UnlockOutPoints([]btc.OutPoint{pt})
	if cm.LockedOutput(pt) != nil {
		t.Fatalf("expected utxo to be unlocked")
	}

	cm.LockOutputsMap(map[btc.OutPoint]*btc.UTxO{pt: utxo})
	if cm.LockedOutput(pt) != utxo {
		t.Fatalf("expected utxo to be locked via map")
	}
}

func TestCoinManagerFundingCoins(t *testing.T) {
	cm := newTestCoinManager()

	pt0 := btc.NewOutPoint(tTxHash, 0)
	cachedUTXO := &btc.UTxO{TxHash: tTxHash, Vout: 0, Amount: toSatoshi(1)}
	cm.lockedOutputs[pt0] = cachedUTXO

	id0 := encodeCoinID(tTxHash, 0)

	// Cached case: found without any RPC calls.
	coins, err := cm.FundingCoins([]util.Bytes{id0})
	if err != nil {
		t.Fatalf("FundingCoins (cached) error: %v", err)
	}
	if len(coins) != 1 {
		t.Fatalf("expected 1 coin, got %d", len(coins))
	}

	// Bad coin ID decode error.
	if _, err := cm.FundingCoins([]util.Bytes{{0x01}}); err == nil {
		t.Fatalf("expected decode error")
	}

	// Not cached: found via listLocked + getTxOut.
	cm2 := newTestCoinManager()
	id1 := encodeCoinID(tTxHash, 1)
	cm2.listLocked = func() ([]*rpcOutpoint, error) {
		return []*rpcOutpoint{{TxID: tTxHash.String(), Vout: 1}}, nil
	}
	cm2.getTxOut = func(h *chainhash.Hash, vout uint32) (*wire.TxOut, error) {
		return wire.NewTxOut(int64(toSatoshi(2)), tP2PKH), nil
	}
	coins, err = cm2.FundingCoins([]util.Bytes{id1})
	if err != nil {
		t.Fatalf("FundingCoins (listLocked) error: %v", err)
	}
	if len(coins) != 1 {
		t.Fatalf("expected 1 coin from listLocked path, got %d", len(coins))
	}
	if cm2.lockedOutputs[btc.NewOutPoint(tTxHash, 1)] == nil {
		t.Fatalf("expected coin to be cached after lookup")
	}

	// Not cached and not locked: fallback to spendableUTXOs, then lock it.
	cm3 := newTestCoinManager()
	id2 := encodeCoinID(tTxHash, 2)
	cm3.listLocked = func() ([]*rpcOutpoint, error) { return nil, nil }
	cm3.listUnspent = func() ([]*listUnspentResult, error) {
		return []*listUnspentResult{newTestListUnspentResult(tTxHash.String(), 2, 1, 5)}, nil
	}
	var lockedCoins []*btc.Output
	cm3.lockUnspent = func(unlock bool, ops []*btc.Output) error {
		lockedCoins = ops
		return nil
	}
	coins, err = cm3.FundingCoins([]util.Bytes{id2})
	if err != nil {
		t.Fatalf("FundingCoins (fallback) error: %v", err)
	}
	if len(coins) != 1 || len(lockedCoins) != 1 {
		t.Fatalf("expected fallback coin to be found and locked")
	}

	// Coin not found anywhere is an error.
	cm4 := newTestCoinManager()
	cm4.listLocked = func() ([]*rpcOutpoint, error) { return nil, nil }
	cm4.listUnspent = func() ([]*listUnspentResult, error) { return nil, nil }
	if _, err := cm4.FundingCoins([]util.Bytes{id2}); err == nil {
		t.Fatalf("expected not-found error")
	}

	// listLocked error propagates.
	cm5 := newTestCoinManager()
	cm5.listLocked = func() ([]*rpcOutpoint, error) { return nil, errTest }
	if _, err := cm5.FundingCoins([]util.Bytes{id2}); err == nil {
		t.Fatalf("expected error from listLocked failure")
	}
}
