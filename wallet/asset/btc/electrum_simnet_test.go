//go:build harness

package btc

// electrum_simnet_test.go contains harness integration tests for the
// electrumWallet (walletTypeElectrum) backed by a live electrumx server.
//
// Prerequisites (run by the user, not this file):
//   - BTC simnet harness: cd util/testing/btc && ./harness.sh
//   - Electrumx server:   cd util/testing/btc && ./electrumx.sh
//     (serves ssl://127.0.0.1:54002)
//
// Run the tests:
//
//	go test -tags harness ./wallet/asset/btc/ -run TestElectrum
//
// NOTE: walletTypeElectrum, createElectrumWallet, and openElectrumWallet all
// live in electrum.go (same package). TestMain, tBackend, mineAlpha, mineBeta,
// gammaSeed, walletPassword, toSatoshi, tCtx, tLogger, waitNetwork, newTestRig,
// and randBytes are declared in simnet_test.go — do NOT redeclare them here.

import (
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/btcsuite/btcd/chaincfg"
)

// electrumSyncTimeout is how long we wait for the electrum wallet to
// synchronise with the harness electrumx server.
const electrumSyncTimeout = 60 * time.Second

// electrumPollInterval is the interval between SyncStatus polls.
const electrumPollInterval = 500 * time.Millisecond

// newElectrumWallet constructs a seeded electrumWallet for the BTC simnet
// harness, connects it via a ConnectionMaster, and waits up to
// electrumSyncTimeout for it to sync. It returns the asset.Wallet and the
// ConnectionMaster. The wallet is created in a fresh temp directory and uses
// the same gamma seed as the SPV gamma wallet so that pre-funded UTXOs are
// visible.
//
// Differences from tBackend:
//   - Uses walletTypeElectrum / createElectrumWallet instead of walletTypeSPV /
//     createSPVWallet.
//   - Returns asset.Wallet (not *ExchangeWalletFullNode) because electrumWallet
//     is not an ExchangeWalletFullNode and only implements the lean interface.
//   - Does NOT perform the SPV-specific type-assert or the SPV sync wait inside
//     tBackend; instead it polls SyncStatus until Synced, mirroring the
//     harness's wait logic.
func newElectrumWallet(t *testing.T) (asset.Wallet, *util.ConnectionMaster) {
	t.Helper()

	seed, err := hex.DecodeString(gammaSeed)
	if err != nil {
		t.Fatalf("newElectrumWallet: decode gammaSeed: %v", err)
	}

	dataDir := t.TempDir()
	walletDir := filepath.Join(dataDir, chaincfg.RegressionNetParams.Name)

	if err = createElectrumWallet(walletPassword, seed, DefaultWalletBirthday, walletDir, tLogger, 0, 0, &chaincfg.RegressionNetParams); err != nil {
		t.Fatalf("newElectrumWallet: createElectrumWallet: %v", err)
	}

	walletCfg := &asset.WalletConfig{
		Type:     walletTypeElectrum,
		DataDir:  dataDir,
		Settings: map[string]string{},
		Emit: asset.NewWalletEmitter(
			make(chan asset.WalletNotification, 128), 0, tLogger,
		),
		PeersChange: func(n uint32, err error) {
			tLogger.Debugf("electrum wallet peers=%d err=%v", n, err)
		},
	}

	w, err := NewWallet(walletCfg, tLogger, util.Simnet)
	if err != nil {
		t.Fatalf("newElectrumWallet: NewWallet: %v", err)
	}

	cm := util.NewConnectionMaster(w)
	if err = cm.Connect(tCtx); err != nil {
		t.Fatalf("newElectrumWallet: Connect: %v", err)
	}

	return w, cm
}

// waitElectrumSynced polls SyncStatus until synced or timeout.
func waitElectrumSynced(t *testing.T, w asset.Wallet) {
	t.Helper()
	deadline := time.Now().Add(electrumSyncTimeout)
	for time.Now().Before(deadline) {
		ss, err := w.SyncStatus()
		if err != nil {
			t.Logf("waitElectrumSynced: SyncStatus err: %v", err)
		} else if ss != nil && ss.Synced {
			return
		}
		time.Sleep(electrumPollInterval)
	}
	t.Fatalf("electrum wallet did not sync within %v", electrumSyncTimeout)
}

// waitElectrumTxConfirmed polls TxHistory until the given txID appears
// as confirmed (Confirmed == true) or until timeout. Returns the final
// WalletTransaction for further assertions.
func waitElectrumTxConfirmed(t *testing.T, w asset.Wallet, txID string) *txdb.WalletTransaction {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		wt, err := w.WalletTransaction(tCtx, txID)
		if err == nil && wt != nil && wt.Confirmed {
			return wt
		}
		time.Sleep(electrumPollInterval)
	}
	t.Fatalf("tx %s not confirmed within 60s", txID)
	return nil
}

// waitElectrumBalanceAtLeast polls Balance until Available >= wantMin or
// timeout.
func waitElectrumBalanceAtLeast(t *testing.T, w asset.Wallet, wantMin uint64) *asset.Balance {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		bal, err := w.Balance()
		if err == nil && bal != nil && bal.Available >= wantMin {
			return bal
		}
		time.Sleep(electrumPollInterval)
	}
	t.Fatalf("balance did not reach %d satoshis within 60s", wantMin)
	return nil
}

// waitElectrumPendingContains polls PendingTransactions until the given txID
// appears in the pending set, or until timeout.
func waitElectrumPendingContains(t *testing.T, w asset.Wallet, txID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, pt := range w.PendingTransactions(tCtx) {
			if pt.ID == txID {
				return
			}
		}
		time.Sleep(electrumPollInterval)
	}
	t.Fatalf("tx %s not in PendingTransactions within 30s", txID)
}

// waitElectrumPendingNotContains polls PendingTransactions until the given
// txID is absent (moved to confirmed), or until timeout.
func waitElectrumPendingNotContains(t *testing.T, w asset.Wallet, txID string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, pt := range w.PendingTransactions(tCtx) {
			if pt.ID == txID {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(electrumPollInterval)
	}
	t.Fatalf("tx %s still in PendingTransactions after 60s", txID)
}

// waitElectrumTxInHistory polls TxHistory until the given txID appears, or
// timeout.
func waitElectrumTxInHistory(t *testing.T, w asset.Wallet, txID string) *txdb.WalletTransaction {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := w.TxHistory(&txdb.TxHistoryRequest{N: 100})
		if err == nil && resp != nil {
			for _, wt := range resp.Txs {
				if wt.ID == txID {
					return wt
				}
			}
		}
		time.Sleep(electrumPollInterval)
	}
	t.Fatalf("tx %s not found in TxHistory within 60s", txID)
	return nil
}

// TestElectrumConnectSyncStatus verifies that the electrum wallet connects to
// the harness electrumx server and reaches the Synced state.
func TestElectrumConnectSyncStatus(t *testing.T) {
	w, cm := newElectrumWallet(t)
	defer cm.Disconnect()

	waitElectrumSynced(t, w)

	ss, err := w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus: %v", err)
	}
	if !ss.Synced {
		t.Fatalf("expected Synced=true, got %+v", ss)
	}
	if ss.TargetHeight == 0 {
		t.Fatal("TargetHeight should be >0 after sync")
	}
	t.Logf("electrum BTC synced: height=%d", ss.Blocks)
}

// TestElectrumDepositAddressOwnsValidate verifies DepositAddress,
// OwnsDepositAddress, and ValidateAddress round-trip correctly.
func TestElectrumDepositAddressOwnsValidate(t *testing.T) {
	w, cm := newElectrumWallet(t)
	defer cm.Disconnect()

	waitElectrumSynced(t, w)

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}
	if addr == "" {
		t.Fatal("DepositAddress returned empty string")
	}
	t.Logf("electrum BTC deposit address: %s", addr)

	owns, err := w.OwnsDepositAddress(addr)
	if err != nil {
		t.Fatalf("OwnsDepositAddress(%s): %v", addr, err)
	}
	if !owns {
		t.Fatalf("OwnsDepositAddress(%s) = false, expected true", addr)
	}

	if !w.ValidateAddress(addr) {
		t.Fatalf("ValidateAddress(%s) = false for own address", addr)
	}

	// A clearly bogus address should not be valid.
	if w.ValidateAddress("not-an-address") {
		t.Fatal("ValidateAddress(not-an-address) returned true")
	}
}

// TestElectrumBalanceAfterFunding funds the electrum wallet from the alpha
// full-node wallet and verifies the balance increases.
func TestElectrumBalanceAfterFunding(t *testing.T) {
	// alpha is the harness full-node funder; we use tBackend to get it.
	alpha, alphaCM := tBackend(t, "alpha", false, nil)
	defer alphaCM.Disconnect()

	if err := alpha.Unlock(walletPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}

	w, cm := newElectrumWallet(t)
	defer cm.Disconnect()

	waitElectrumSynced(t, w)

	balBefore, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance before: %v", err)
	}
	t.Logf("balance before funding: available=%d", balBefore.Available)

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const fundAmt = 10 // BTC (in simnet units)
	if _, err = alpha.Send(addr, toSatoshi(fundAmt), 10); err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}

	// Mine a block so the transaction gets confirmed.
	if err = mineAlpha(); err != nil {
		t.Fatalf("mineAlpha: %v", err)
	}

	wantMin := balBefore.Available + toSatoshi(fundAmt)
	bal := waitElectrumBalanceAtLeast(t, w, wantMin)
	t.Logf("balance after funding: available=%d (want>=%d)", bal.Available, wantMin)
}

// TestElectrumSendSelfPendingToConfirmed sends coins from the electrum wallet
// back to one of its own addresses, asserts the transaction appears in
// PendingTransactions immediately after broadcast, then mines a block and
// verifies the transaction moves to confirmed in both WalletTransaction and
// TxHistory.
func TestElectrumSendSelfPendingToConfirmed(t *testing.T) {
	// Fund the electrum wallet first via alpha so there's something to spend.
	alpha, alphaCM := tBackend(t, "alpha", false, nil)
	defer alphaCM.Disconnect()

	if err := alpha.Unlock(walletPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}

	w, cm := newElectrumWallet(t)
	defer cm.Disconnect()

	waitElectrumSynced(t, w)

	// Unlock the electrum wallet so it can sign.
	if auth, ok := w.(asset.Authenticator); ok {
		if err := auth.Unlock(walletPassword); err != nil {
			t.Fatalf("unlock electrum wallet: %v", err)
		}
	}

	// Fund with 20 BTC so there's change after the self-send.
	fundAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress for funding: %v", err)
	}
	if _, err = alpha.Send(fundAddr, toSatoshi(20), 10); err != nil {
		t.Fatalf("alpha.Send to fund electrum wallet: %v", err)
	}
	if err = mineAlpha(); err != nil {
		t.Fatalf("mineAlpha after funding: %v", err)
	}
	waitElectrumBalanceAtLeast(t, w, toSatoshi(20))

	// Now send 5 BTC to another own address.
	destAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress for self-send dest: %v", err)
	}
	const feeRate = 10
	coin, err := w.Send(destAddr, toSatoshi(5), feeRate)
	if err != nil {
		t.Fatalf("Send self: %v", err)
	}
	txID := coin.TxID()
	t.Logf("self-send txid: %s", txID)

	// The tx should appear in PendingTransactions right away.
	waitElectrumPendingContains(t, w, txID)
	t.Logf("tx %s found in PendingTransactions", txID)

	// Mine a block; the tx should move out of pending and into confirmed.
	if err = mineAlpha(); err != nil {
		t.Fatalf("mineAlpha after self-send: %v", err)
	}
	waitElectrumPendingNotContains(t, w, txID)

	wt := waitElectrumTxConfirmed(t, w, txID)
	t.Logf("self-send confirmed at block %d", wt.BlockNumber)

	// Also assert it appears in TxHistory.
	htx := waitElectrumTxInHistory(t, w, txID)
	if !htx.Confirmed {
		t.Fatalf("TxHistory entry for %s has Confirmed=false", txID)
	}
}

// TestElectrumReceiveDetection has alpha send coins to the electrum wallet,
// mines the block, and verifies the receive appears in TxHistory with the
// balance updated.
func TestElectrumReceiveDetection(t *testing.T) {
	alpha, alphaCM := tBackend(t, "alpha", false, nil)
	defer alphaCM.Disconnect()

	if err := alpha.Unlock(walletPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}

	w, cm := newElectrumWallet(t)
	defer cm.Disconnect()

	waitElectrumSynced(t, w)

	balBefore, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance before receive: %v", err)
	}

	recvAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const recvAmt = 7 // BTC
	coin, err := alpha.Send(recvAddr, toSatoshi(recvAmt), 10)
	if err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}
	recvTxID := coin.TxID()
	t.Logf("receive txid from alpha: %s", recvTxID)

	if err = mineAlpha(); err != nil {
		t.Fatalf("mineAlpha after receive: %v", err)
	}

	// Balance should increase.
	wantMin := balBefore.Available + toSatoshi(recvAmt)
	bal := waitElectrumBalanceAtLeast(t, w, wantMin)
	t.Logf("balance after receive: available=%d (want>=%d)", bal.Available, wantMin)

	// The receive should appear in TxHistory.
	htx := waitElectrumTxInHistory(t, w, recvTxID)
	t.Logf("receive tx in TxHistory: confirmed=%v type=%v", htx.Confirmed, htx.Type)
}

// TestElectrumStandardSendFee checks that StandardSendFee returns a positive
// non-zero value for a typical fee rate.
func TestElectrumStandardSendFee(t *testing.T) {
	w, cm := newElectrumWallet(t)
	defer cm.Disconnect()

	const feeRate = 10
	fee := w.StandardSendFee(feeRate)
	if fee == 0 {
		t.Fatal("StandardSendFee(10) returned 0")
	}
	t.Logf("StandardSendFee(10) = %d satoshis", fee)

	// Fee should be monotonically increasing with fee rate.
	fee2 := w.StandardSendFee(feeRate * 2)
	if fee2 <= fee {
		t.Fatalf("StandardSendFee not monotonic: fee(%d)=%d fee(%d)=%d",
			feeRate, fee, feeRate*2, fee2)
	}
}

// TestElectrumInfo verifies that Info returns non-nil WalletInfo consistent
// with Bitcoin.
func TestElectrumInfo(t *testing.T) {
	w, cm := newElectrumWallet(t)
	defer cm.Disconnect()

	info := w.Info()
	if info == nil {
		t.Fatal("Info() returned nil")
	}
	if info.Name != "Bitcoin" {
		t.Fatalf("Info.Name = %q, want %q", info.Name, "Bitcoin")
	}

	// electrumWallet implements asset.Authenticator by delegating to the
	// underlying wallet.Wallet. Exercise the Unlock/Locked/Lock round-trip.
	auth, ok := w.(asset.Authenticator)
	if !ok {
		t.Fatal("electrumWallet does not implement asset.Authenticator")
	}
	if err := auth.Unlock(walletPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if auth.Locked() {
		t.Fatal("wallet reports locked after Unlock")
	}
	if err := auth.Lock(); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if !auth.Locked() {
		t.Fatal("wallet reports unlocked after Lock")
	}
}
