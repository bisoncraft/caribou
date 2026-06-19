//go:build harness

package ltc

// electrum_simnet_test.go contains harness integration tests for the
// ltcElectrumWallet (walletTypeElectrum) backed by a live electrumx server.
//
// Prerequisites (run by the user, not this file):
//   - LTC simnet harness: cd util/testing/ltc && ./harness.sh
//     (tmux session: ltc-harness, harness-ctl in window 2)
//   - Electrumx server:   cd util/testing/ltc && ./electrumx.sh
//     (delegates to btc/electrumx.sh Litecoin 20566 55002; serves
//     ssl://127.0.0.1:55002)
//
// Run the tests:
//
//	go test -tags harness ./wallet/asset/ltc/ -run TestLTCElectrum
//
// MWEB NOTE: LTC MWEB-stripped decode through electrum is the highest-risk
// integration point. Raw transactions from the electrumx server are decoded
// via the bisonwire MWEB-aware decoder; MWEB extension-block outputs are
// silently stripped. Operations on standard transparent (P2WPKH / P2PKH)
// outputs — which these tests exercise exclusively — are unaffected. Full
// MWEB receive/send detection through an electrum backend is a known follow-up
// item and is NOT tested here.

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/config"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	dexbtc "github.com/bisoncraft/bosswallet/wallet/asset/btc"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
)

// ltcGammaSeed is the HD seed for the LTC gamma wallet, matching
// util/testing/ltc/harness.sh GAMMA_WALLET_SEED (WIF → raw entropy).
// We use the same byte sequence as the BTC simnet_test.go gamma seed to keep
// things simple; the gamma wallet is freshly funded by newLTCAlphaBackend each
// time.
const ltcGammaSeed = "1285a47d6a59f9c548b2a72c2c34a2de97967bede3844090102bbba76707fe9d"

var (
	ltcLogger   util.Logger
	ltcCtx      context.Context
	ltcPassword = []byte("abc")
)

// TestMain is the harness test entry point for the LTC package. Because there
// is no pre-existing TestMain in this package for the harness tag, we define it
// here. Non-harness TestMains live in other files with !harness constraints.
func TestMain(m *testing.M) {
	ltcLogger = util.StdOutLogger("LTCTEST", util.LevelTrace)
	var shutdown func()
	ltcCtx, shutdown = context.WithCancel(context.Background())
	doIt := func() int {
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

// mineLTCAlpha sends the mine-alpha script to the ltc-harness tmux session.
// The tmux session name is "ltc-harness" (SYMBOL=ltc → SESSION="${SYMBOL}-harness").
// Window 0 is the alpha litecoind process; window 2 is harness-ctl where the
// mine-alpha helper lives. We send to window 2.
func mineLTCAlpha() error {
	return exec.Command("tmux", "send-keys", "-t", "ltc-harness:2", "./mine-alpha 1", "C-m").Run()
}

// newLTCAlphaBackend opens an asset.Wallet backed by the harness alpha
// litecoind node. It reads the standard harness config file at
// ~/dextest/ltc/alpha/alpha.conf. Wallet type is walletTypeRPC.
func newLTCAlphaBackend(t *testing.T) (asset.Wallet, *util.ConnectionMaster) {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	cfgPath := filepath.Join(u.HomeDir, "dextest", "ltc", "alpha", "alpha.conf")
	settings, err := config.Parse(cfgPath)
	if err != nil {
		t.Fatalf("config.Parse(%s): %v", cfgPath, err)
	}

	walletCfg := &asset.WalletConfig{
		Type:     walletTypeRPC,
		Settings: settings,
		Emit:     asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), dexbtc.BipID, ltcLogger),
		PeersChange: func(n uint32, err error) {
			ltcLogger.Debugf("alpha peers=%d err=%v", n, err)
		},
	}
	w, err := NewWallet(walletCfg, ltcLogger, util.Regtest)
	if err != nil {
		t.Fatalf("NewWallet(alpha): %v", err)
	}
	cm := util.NewConnectionMaster(w)
	if err = cm.Connect(ltcCtx); err != nil {
		t.Fatalf("Connect(alpha): %v", err)
	}
	return w, cm
}

// newLTCElectrumWallet constructs a seeded ltcElectrumWallet for the LTC
// simnet harness, connects it, and returns the asset.Wallet and
// ConnectionMaster. The wallet is placed in a temp directory and uses the same
// gamma seed constant defined above.
//
// Differences from newLTCAlphaBackend:
//   - Uses walletTypeElectrum / createLTCElectrumWallet instead of walletTypeRPC.
//   - Returns asset.Wallet (not any LTC-specific concrete type) because
//     ltcElectrumWallet implements only the lean asset.Wallet interface.
//   - Does not read any config file; the electrumx server address is built-in
//     (ssl://127.0.0.1:55002, the regtest entry in ltcElectrumServers).
func newLTCElectrumWallet(t *testing.T) (asset.Wallet, *util.ConnectionMaster) {
	t.Helper()

	seed, err := hex.DecodeString(ltcGammaSeed)
	if err != nil {
		t.Fatalf("newLTCElectrumWallet: decode seed: %v", err)
	}

	dataDir := t.TempDir()
	walletDir := filepath.Join(dataDir, RegressionNetParams.Name)

	if err = createLTCElectrumWallet(ltcPassword, seed, dexbtc.DefaultWalletBirthday, walletDir, ltcLogger, 0, 0, RegressionNetParams); err != nil {
		t.Fatalf("newLTCElectrumWallet: createLTCElectrumWallet: %v", err)
	}

	walletCfg := &asset.WalletConfig{
		Type:     walletTypeElectrum,
		DataDir:  dataDir,
		Settings: map[string]string{},
		Emit:     asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), BipID, ltcLogger),
		PeersChange: func(n uint32, err error) {
			ltcLogger.Debugf("electrum LTC peers=%d err=%v", n, err)
		},
	}

	w, err := NewWallet(walletCfg, ltcLogger, util.Regtest)
	if err != nil {
		t.Fatalf("newLTCElectrumWallet: NewWallet: %v", err)
	}

	cm := util.NewConnectionMaster(w)
	if err = cm.Connect(ltcCtx); err != nil {
		t.Fatalf("newLTCElectrumWallet: Connect: %v", err)
	}
	return w, cm
}

// ltcElectrumSyncTimeout is how long we wait for the LTC electrum wallet to
// synchronise with the harness electrumx server.
const ltcElectrumSyncTimeout = 90 * time.Second

// waitLTCElectrumSynced polls SyncStatus until synced or timeout.
func waitLTCElectrumSynced(t *testing.T, w asset.Wallet) {
	t.Helper()
	const poll = 500 * time.Millisecond
	deadline := time.Now().Add(ltcElectrumSyncTimeout)
	for time.Now().Before(deadline) {
		ss, err := w.SyncStatus()
		if err != nil {
			t.Logf("waitLTCElectrumSynced: SyncStatus err: %v", err)
		} else if ss != nil && ss.Synced {
			return
		}
		time.Sleep(poll)
	}
	t.Fatalf("LTC electrum wallet did not sync within %v", ltcElectrumSyncTimeout)
}

// waitLTCBalanceAtLeast polls Balance until Available >= wantMin or timeout.
func waitLTCBalanceAtLeast(t *testing.T, w asset.Wallet, wantMin uint64) *asset.Balance {
	t.Helper()
	const poll = 500 * time.Millisecond
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		bal, err := w.Balance()
		if err == nil && bal != nil && bal.Available >= wantMin {
			return bal
		}
		time.Sleep(poll)
	}
	t.Fatalf("LTC balance did not reach %d litoshi within 60s", wantMin)
	return nil
}

// waitLTCPendingContains polls PendingTransactions until txID appears.
func waitLTCPendingContains(t *testing.T, w asset.Wallet, txID string) {
	t.Helper()
	const poll = 500 * time.Millisecond
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, pt := range w.PendingTransactions(ltcCtx) {
			if pt.ID == txID {
				return
			}
		}
		time.Sleep(poll)
	}
	t.Fatalf("tx %s not in LTC PendingTransactions within 30s", txID)
}

// waitLTCPendingNotContains polls PendingTransactions until txID is absent.
func waitLTCPendingNotContains(t *testing.T, w asset.Wallet, txID string) {
	t.Helper()
	const poll = 500 * time.Millisecond
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, pt := range w.PendingTransactions(ltcCtx) {
			if pt.ID == txID {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(poll)
	}
	t.Fatalf("tx %s still in LTC PendingTransactions after 60s", txID)
}

// waitLTCTxConfirmed polls WalletTransaction until confirmed or timeout.
func waitLTCTxConfirmed(t *testing.T, w asset.Wallet, txID string) *txdb.WalletTransaction {
	t.Helper()
	const poll = 500 * time.Millisecond
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		wt, err := w.WalletTransaction(ltcCtx, txID)
		if err == nil && wt != nil && wt.Confirmed {
			return wt
		}
		time.Sleep(poll)
	}
	t.Fatalf("LTC tx %s not confirmed within 60s", txID)
	return nil
}

// waitLTCTxInHistory polls TxHistory until txID appears.
func waitLTCTxInHistory(t *testing.T, w asset.Wallet, txID string) *txdb.WalletTransaction {
	t.Helper()
	const poll = 500 * time.Millisecond
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
		time.Sleep(poll)
	}
	t.Fatalf("LTC tx %s not found in TxHistory within 60s", txID)
	return nil
}

// toSatoshiLTC converts LTC to litoshi (same 1e8 factor as BTC satoshi).
func toSatoshiLTC(ltc float64) uint64 {
	const convFactor = 1e8
	return uint64(ltc * convFactor)
}

// TestLTCElectrumConnectSyncStatus verifies that the LTC electrum wallet
// connects to the harness electrumx server and reaches the Synced state.
func TestLTCElectrumConnectSyncStatus(t *testing.T) {
	w, cm := newLTCElectrumWallet(t)
	defer cm.Disconnect()

	waitLTCElectrumSynced(t, w)

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
	t.Logf("LTC electrum synced: height=%d", ss.Blocks)
}

// TestLTCElectrumDepositAddressOwnsValidate verifies DepositAddress,
// OwnsDepositAddress, and ValidateAddress round-trip correctly.
func TestLTCElectrumDepositAddressOwnsValidate(t *testing.T) {
	w, cm := newLTCElectrumWallet(t)
	defer cm.Disconnect()

	waitLTCElectrumSynced(t, w)

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}
	if addr == "" {
		t.Fatal("DepositAddress returned empty string")
	}
	t.Logf("LTC electrum deposit address: %s", addr)

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
	if w.ValidateAddress("not-a-valid-ltc-address") {
		t.Fatal("ValidateAddress(garbage) returned true")
	}
}

// TestLTCElectrumBalanceAfterFunding funds the LTC electrum wallet from the
// alpha litecoind wallet and verifies the balance increases.
func TestLTCElectrumBalanceAfterFunding(t *testing.T) {
	alpha, alphaCM := newLTCAlphaBackend(t)
	defer alphaCM.Disconnect()

	if auth, ok := alpha.(asset.Authenticator); ok {
		if err := auth.Unlock(ltcPassword); err != nil {
			t.Fatalf("unlock alpha: %v", err)
		}
	}

	w, cm := newLTCElectrumWallet(t)
	defer cm.Disconnect()

	waitLTCElectrumSynced(t, w)

	balBefore, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance before: %v", err)
	}
	t.Logf("LTC balance before funding: available=%d", balBefore.Available)

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const fundAmt = 10 // LTC
	if _, err = alpha.Send(addr, toSatoshiLTC(fundAmt), 10); err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}

	if err = mineLTCAlpha(); err != nil {
		t.Fatalf("mineLTCAlpha: %v", err)
	}

	wantMin := balBefore.Available + toSatoshiLTC(fundAmt)
	bal := waitLTCBalanceAtLeast(t, w, wantMin)
	t.Logf("LTC balance after funding: available=%d (want>=%d)", bal.Available, wantMin)
}

// TestLTCElectrumSendSelfPendingToConfirmed sends coins within the LTC electrum
// wallet, asserts the tx is in PendingTransactions, mines, then asserts
// it moves to confirmed in WalletTransaction and TxHistory.
func TestLTCElectrumSendSelfPendingToConfirmed(t *testing.T) {
	alpha, alphaCM := newLTCAlphaBackend(t)
	defer alphaCM.Disconnect()

	if auth, ok := alpha.(asset.Authenticator); ok {
		if err := auth.Unlock(ltcPassword); err != nil {
			t.Fatalf("unlock alpha: %v", err)
		}
	}

	w, cm := newLTCElectrumWallet(t)
	defer cm.Disconnect()

	waitLTCElectrumSynced(t, w)

	// Unlock the electrum wallet for signing if it implements Authenticator.
	if auth, ok := w.(asset.Authenticator); ok {
		if err := auth.Unlock(ltcPassword); err != nil {
			t.Fatalf("unlock LTC electrum wallet: %v", err)
		}
	}

	// Fund with 20 LTC.
	fundAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress for funding: %v", err)
	}
	if _, err = alpha.Send(fundAddr, toSatoshiLTC(20), 10); err != nil {
		t.Fatalf("alpha.Send for funding: %v", err)
	}
	if err = mineLTCAlpha(); err != nil {
		t.Fatalf("mineLTCAlpha after funding: %v", err)
	}
	waitLTCBalanceAtLeast(t, w, toSatoshiLTC(20))

	// Self-send 5 LTC.
	destAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress for self-send dest: %v", err)
	}
	coin, err := w.Send(destAddr, toSatoshiLTC(5), 10)
	if err != nil {
		t.Fatalf("Send self: %v", err)
	}
	txID := coin.TxID()
	t.Logf("LTC self-send txid: %s", txID)

	waitLTCPendingContains(t, w, txID)
	t.Logf("LTC tx %s found in PendingTransactions", txID)

	if err = mineLTCAlpha(); err != nil {
		t.Fatalf("mineLTCAlpha after self-send: %v", err)
	}
	waitLTCPendingNotContains(t, w, txID)

	wt := waitLTCTxConfirmed(t, w, txID)
	t.Logf("LTC self-send confirmed at block %d", wt.BlockNumber)

	htx := waitLTCTxInHistory(t, w, txID)
	if !htx.Confirmed {
		t.Fatalf("TxHistory entry for %s has Confirmed=false", txID)
	}
}

// TestLTCElectrumReceiveDetection sends coins from alpha to the electrum
// wallet, mines, and asserts the receive appears in TxHistory and the balance
// reflects it.
func TestLTCElectrumReceiveDetection(t *testing.T) {
	alpha, alphaCM := newLTCAlphaBackend(t)
	defer alphaCM.Disconnect()

	if auth, ok := alpha.(asset.Authenticator); ok {
		if err := auth.Unlock(ltcPassword); err != nil {
			t.Fatalf("unlock alpha: %v", err)
		}
	}

	w, cm := newLTCElectrumWallet(t)
	defer cm.Disconnect()

	waitLTCElectrumSynced(t, w)

	balBefore, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance before receive: %v", err)
	}

	recvAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const recvAmt = 7 // LTC
	coin, err := alpha.Send(recvAddr, toSatoshiLTC(recvAmt), 10)
	if err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}
	recvTxID := coin.TxID()
	t.Logf("LTC receive txid: %s", recvTxID)

	if err = mineLTCAlpha(); err != nil {
		t.Fatalf("mineLTCAlpha: %v", err)
	}

	wantMin := balBefore.Available + toSatoshiLTC(recvAmt)
	bal := waitLTCBalanceAtLeast(t, w, wantMin)
	t.Logf("LTC balance after receive: available=%d (want>=%d)", bal.Available, wantMin)

	htx := waitLTCTxInHistory(t, w, recvTxID)
	t.Logf("LTC receive in TxHistory: confirmed=%v type=%v", htx.Confirmed, htx.Type)
}

// TestLTCElectrumStandardSendFee checks that StandardSendFee returns a
// positive, monotonically increasing value.
func TestLTCElectrumStandardSendFee(t *testing.T) {
	w, cm := newLTCElectrumWallet(t)
	defer cm.Disconnect()

	const feeRate = 10
	fee := w.StandardSendFee(feeRate)
	if fee == 0 {
		t.Fatal("StandardSendFee(10) returned 0")
	}
	fee2 := w.StandardSendFee(feeRate * 2)
	if fee2 <= fee {
		t.Fatalf("StandardSendFee not monotonic: fee(%d)=%d fee(%d)=%d",
			feeRate, fee, feeRate*2, fee2)
	}
	t.Logf("LTC StandardSendFee(10)=%d, StandardSendFee(20)=%d", fee, fee2)
}

// TestLTCElectrumInfo verifies that Info returns non-nil WalletInfo consistent
// with Litecoin.
func TestLTCElectrumInfo(t *testing.T) {
	w, cm := newLTCElectrumWallet(t)
	defer cm.Disconnect()

	info := w.Info()
	if info == nil {
		t.Fatal("Info() returned nil")
	}
	if info.Name != "Litecoin" {
		t.Fatalf("Info.Name = %q, want %q", info.Name, "Litecoin")
	}
}
