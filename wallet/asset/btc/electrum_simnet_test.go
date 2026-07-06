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
// live in electrum.go (same package). TestMain, tBackend, mineAlpha, gammaSeed,
// walletPassword, toSatoshi, tCtx, tLogger, and randBytes are declared in
// simnet_test.go — do NOT redeclare them here.

import (
	"encoding/hex"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/btcsuite/btcd/chaincfg"
)

// electrumSyncTimeout is how long we wait for an electrum wallet operation
// (sync, balance, confirmation) to complete against the harness electrumx
// server. It is generous on purpose: public/over-loaded electrum servers can be
// slow, and the wallet (and these tests) must tolerate that rather than assume
// snappy responses.
const electrumSyncTimeout = 3 * time.Minute

// electrumPollInterval is the interval between SyncStatus polls.
const electrumPollInterval = 500 * time.Millisecond

// newElectrumWallet constructs a seeded electrumWallet for the BTC simnet
// harness, connects it via a ConnectionMaster, and waits up to
// electrumSyncTimeout for it to sync. It returns the asset.Wallet and the
// ConnectionMaster. The wallet is created in a fresh temp directory and uses
// the same gamma seed as the SPV gamma wallet so that pre-funded UTXOs are
// visible.
func newElectrumWallet(t *testing.T) (asset.Wallet, *util.ConnectionMaster) {
	t.Helper()
	w, cm, _ := newElectrumWalletWithNotes(t)
	return w, cm
}

// newElectrumWalletWithNotes is newElectrumWallet but also returns the wallet's
// notification channel so a test can assert on emitted notes (TipChange,
// BalanceChange, ...). Callers that care about the notes must drain the channel
// promptly: WalletEmitter.emit drops notes when the channel is full.
func newElectrumWalletWithNotes(t *testing.T) (asset.Wallet, *util.ConnectionMaster, chan asset.WalletNotification) {
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

	notes := make(chan asset.WalletNotification, 256)

	// Provide a master tx database scoped to this asset, the way Core does via
	// the wallet config; the electrum wallet uses this handle for tx history.
	masterDB, err := txdb.New(filepath.Join(dataDir, "txdb"), tLogger.SubLogger("TXDB"))
	if err != nil {
		t.Fatalf("newElectrumWallet: txdb.New: %v", err)
	}
	if _, err = masterDB.Connect(tCtx); err != nil {
		t.Fatalf("newElectrumWallet: txdb.Connect: %v", err)
	}
	t.Cleanup(func() { masterDB.Close() })

	walletCfg := &asset.WalletConfig{
		Type:        walletTypeElectrum,
		DataDir:     dataDir,
		Settings:    map[string]string{},
		Emit:        asset.NewWalletEmitter(notes, 0, tLogger),
		NetworkTxDB: masterDB.NetworkTxDB(BipID),
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

	return w, cm, notes
}

// drainNotes empties any buffered notifications without blocking.
func drainNotes(c <-chan asset.WalletNotification) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
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
	deadline := time.Now().Add(electrumSyncTimeout)
	for time.Now().Before(deadline) {
		wt, err := w.WalletTransaction(tCtx, txID)
		if err == nil && wt != nil && wt.Confirmed {
			return wt
		}
		time.Sleep(electrumPollInterval)
	}
	t.Fatalf("tx %s not confirmed within %v", txID, electrumSyncTimeout)
	return nil
}

// waitElectrumBalanceAtLeast polls Balance until Available >= wantMin or
// timeout.
func waitElectrumBalanceAtLeast(t *testing.T, w asset.Wallet, wantMin uint64) *asset.Balance {
	t.Helper()
	deadline := time.Now().Add(electrumSyncTimeout)
	for time.Now().Before(deadline) {
		bal, err := w.Balance()
		if err == nil && bal != nil && bal.Available >= wantMin {
			return bal
		}
		time.Sleep(electrumPollInterval)
	}
	t.Fatalf("balance did not reach %d satoshis within %v", wantMin, electrumSyncTimeout)
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

// TestElectrumReceiveEmitsNotification verifies that a confirmed receive
// propagates to core through the wallet's notification channel. blockLoop
// previously emitted no TipChange/BalanceChange, so core never re-polled the
// balance and the UI never updated — even on a manual refresh — despite the
// wallet itself seeing the funds. The fix emits a TipChange on each new block
// (which drives core's balance re-poll) and a BalanceChange on unmined-tx
// changes.
func TestElectrumReceiveEmitsNotification(t *testing.T) {
	alpha, alphaCM := tBackend(t, "alpha", false, nil)
	defer alphaCM.Disconnect()

	if err := alpha.Unlock(walletPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}

	w, cm, notes := newElectrumWalletWithNotes(t)
	defer cm.Disconnect()

	waitElectrumSynced(t, w)

	// Discard notes emitted during the sync phase so we only observe those
	// caused by the receive below.
	drainNotes(notes)

	recvAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const recvAmt = 7 // BTC
	if _, err = alpha.Send(recvAddr, toSatoshi(recvAmt), 10); err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}
	if err = mineAlpha(); err != nil {
		t.Fatalf("mineAlpha after receive: %v", err)
	}

	// The confirmed receive must surface as a tip or balance notification so
	// core re-polls and the UI updates.
	deadline := time.After(electrumSyncTimeout)
	for {
		select {
		case n := <-notes:
			switch n.(type) {
			case *asset.TipChangeNote:
				t.Logf("got TipChangeNote after confirmed receive")
				return
			case *asset.BalanceChangeNote:
				t.Logf("got BalanceChangeNote after confirmed receive")
				return
			}
		case <-deadline:
			t.Fatalf("no TipChange/BalanceChange emitted within %v after confirmed receive", electrumSyncTimeout)
		}
	}
}

// TestElectrumUnconfirmedReceiveSurfaces verifies that an incoming receive
// surfaces as an unconfirmed transaction notification while it is still in the
// mempool, i.e. before it is mined. Outgoing sends already surfaced via
// storePendingTx; incoming receives previously had no emit path until a block
// confirmed them. blockLoop now routes mempool arrivals (btcwallet's NtfnServer
// UnminedTransactions) through the tx-recording path.
func TestElectrumUnconfirmedReceiveSurfaces(t *testing.T) {
	alpha, alphaCM := tBackend(t, "alpha", false, nil)
	defer alphaCM.Disconnect()

	if err := alpha.Unlock(walletPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}

	w, cm, notes := newElectrumWalletWithNotes(t)
	defer cm.Disconnect()

	waitElectrumSynced(t, w)
	drainNotes(notes)

	recvAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const recvAmt = 5 // BTC
	coin, err := alpha.Send(recvAddr, toSatoshi(recvAmt), 10)
	if err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}
	recvTxID := coin.TxID()
	t.Logf("unconfirmed receive txid: %s", recvTxID)

	// Deliberately do NOT mine: the receive must surface while still in the
	// mempool.
	deadline := time.After(electrumSyncTimeout)
	for {
		select {
		case n := <-notes:
			tn, ok := n.(*asset.TransactionNote)
			if !ok || tn.Transaction == nil || tn.Transaction.ID != recvTxID {
				continue
			}
			if tn.Transaction.Confirmed {
				t.Fatalf("receive %s reported confirmed before mining", recvTxID)
			}
			t.Logf("unconfirmed receive surfaced via TransactionNote (confirmed=%v)", tn.Transaction.Confirmed)
			return
		case <-deadline:
			t.Fatalf("unconfirmed receive %s did not surface within %v", recvTxID, electrumSyncTimeout)
		}
	}
}

// TestElectrumZeroConfBalanceAvailable verifies that an unconfirmed (0-conf)
// receive is spendable immediately: it raises Available and pushes a
// BalanceChange to the UI WITHOUT mining, surfaces as an unconfirmed
// transaction, and later flips to confirmed. minconf 0 in Balance is what makes
// the mempool funds count. The specific receive txid is tracked so the shared
// gamma wallet's unrelated activity can't produce a false positive.
func TestElectrumZeroConfBalanceAvailable(t *testing.T) {
	alpha, alphaCM := tBackend(t, "alpha", false, nil)
	defer alphaCM.Disconnect()
	if err := alpha.Unlock(walletPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}
	w, cm, notes := newElectrumWalletWithNotes(t)
	defer cm.Disconnect()
	waitElectrumSynced(t, w)

	balBefore, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	recvAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}
	const recvAmt = 6
	coin, err := alpha.Send(recvAddr, toSatoshi(recvAmt), 10)
	if err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}
	recvTxID := coin.TxID()
	wantMin := balBefore.Available + toSatoshi(recvAmt)

	// Mempool, no mining: the receive must surface as an unconfirmed tx AND a
	// BalanceChange carrying the raised (0-conf-inclusive) Available must fire.
	var gotUnconfirmed, gotBalanceNote bool
	deadline := time.After(electrumSyncTimeout)
	for !(gotUnconfirmed && gotBalanceNote) {
		select {
		case n := <-notes:
			switch v := n.(type) {
			case *asset.TransactionNote:
				if v.Transaction.ID == recvTxID && !v.Transaction.Confirmed {
					gotUnconfirmed = true
				}
			case *asset.BalanceChangeNote:
				if v.Balance.Available >= wantMin {
					gotBalanceNote = true
				}
			}
		case <-deadline:
			t.Fatalf("mempool receive not reflected without mining: unconfirmedTx=%v balanceNote>=want=%v",
				gotUnconfirmed, gotBalanceNote)
		}
	}

	// Balance() itself must agree the 0-conf funds are available pre-mine.
	if bal, err := w.Balance(); err != nil {
		t.Fatalf("Balance: %v", err)
	} else if bal.Available < wantMin {
		t.Fatalf("0-conf not in Available pre-mine: Available=%d want>=%d", bal.Available, wantMin)
	}
	t.Logf("0-conf receive spendable before mining")

	// Mining flips the receive to confirmed in the tx list.
	if err := mineAlpha(); err != nil {
		t.Fatalf("mineAlpha: %v", err)
	}
	deadline = time.After(electrumSyncTimeout)
	for {
		select {
		case n := <-notes:
			if tn, ok := n.(*asset.TransactionNote); ok && tn.Transaction.ID == recvTxID && tn.Transaction.Confirmed {
				t.Logf("receive confirmed")
				return
			}
		case <-deadline:
			t.Fatalf("receive %s did not confirm", recvTxID)
		}
	}
}

// TestElectrumMultipleReceivesToSameAddress verifies that several back-to-back
// receives to the same (already-subscribed) deposit address are each detected.
// This exercises the concurrent raw-tx fetch in the scripthash processor: with
// the previous serial fetch, later receives queued behind each earlier one's
// network round-trip.
func TestElectrumMultipleReceivesToSameAddress(t *testing.T) {
	alpha, alphaCM := tBackend(t, "alpha", false, nil)
	defer alphaCM.Disconnect()
	if err := alpha.Unlock(walletPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}
	w, cm, notes := newElectrumWalletWithNotes(t)
	defer cm.Disconnect()
	waitElectrumSynced(t, w)

	recvAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const n = 3
	want := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		coin, err := alpha.Send(recvAddr, toSatoshi(2), 10)
		if err != nil {
			t.Fatalf("send #%d: %v", i, err)
		}
		want[coin.TxID()] = true
	}

	seen := make(map[string]bool, n)
	deadline := time.After(electrumSyncTimeout)
	for len(seen) < n {
		select {
		case note := <-notes:
			if tn, ok := note.(*asset.TransactionNote); ok && want[tn.Transaction.ID] {
				seen[tn.Transaction.ID] = true
			}
		case <-deadline:
			t.Fatalf("only %d/%d receives detected within %v", len(seen), n, electrumSyncTimeout)
		}
	}
}

// connectElectrumWalletAt creates a seeded electrum wallet at dataDir from the
// given seed and connects it. The recoveryWindow is set explicitly via the
// "recoverywindow" setting — including 0, which disables the address-probe
// recovery scan that electrum wallets otherwise run by default
// (ElectrumRecoveryWindow). Taking an explicit dir + seed lets a test stand up
// two wallets from the SAME seed in different directories (the recovery test).
func connectElectrumWalletAt(t *testing.T, dataDir string, seed []byte, recoveryWindow uint32) (asset.Wallet, *util.ConnectionMaster) {
	t.Helper()
	walletDir := filepath.Join(dataDir, chaincfg.RegressionNetParams.Name)
	// A recent birthday (the test's funds are seconds old) keeps SyncedTo from
	// walking the whole chain from 2021 — the O(chain) catchUpHashes cost is the
	// separate Phase-2 concern. It must predate the funding so ingestion sees it.
	bday := time.Now().Add(-time.Hour)
	if err := createElectrumWallet(walletPassword, seed, bday, walletDir, tLogger, 0, 0, &chaincfg.RegressionNetParams); err != nil {
		t.Fatalf("connectElectrumWalletAt: createElectrumWallet: %v", err)
	}
	settings := map[string]string{
		"recoverywindow": strconv.Itoa(int(recoveryWindow)),
	}

	masterDB, err := txdb.New(filepath.Join(dataDir, "txdb"), tLogger.SubLogger("TXDB"))
	if err != nil {
		t.Fatalf("connectElectrumWalletAt: txdb.New: %v", err)
	}
	if _, err = masterDB.Connect(tCtx); err != nil {
		t.Fatalf("connectElectrumWalletAt: txdb.Connect: %v", err)
	}
	t.Cleanup(func() { masterDB.Close() })

	walletCfg := &asset.WalletConfig{
		Type:        walletTypeElectrum,
		DataDir:     dataDir,
		Settings:    settings,
		Emit:        asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), 0, tLogger),
		NetworkTxDB: masterDB.NetworkTxDB(BipID),
		PeersChange: func(n uint32, err error) {
			tLogger.Debugf("electrum recovery wallet peers=%d err=%v", n, err)
		},
	}
	w, err := NewWallet(walletCfg, tLogger, util.Simnet)
	if err != nil {
		t.Fatalf("connectElectrumWalletAt: NewWallet: %v", err)
	}
	cm := util.NewConnectionMaster(w)
	if err = cm.Connect(tCtx); err != nil {
		t.Fatalf("connectElectrumWalletAt: Connect: %v", err)
	}
	return w, cm
}

// TestElectrumRecovery verifies electrum-native gap-limit recovery. Wallet A
// funds a run of external addresses; a brand-new wallet B from the SAME seed,
// created with a recovery window, must rediscover all of those funds on connect
// (via the address-probe scan in syncWithChain), not just the first few. A
// fresh random seed keeps the test isolated from the shared gamma seed's
// accumulated on-chain history.
func TestElectrumRecovery(t *testing.T) {
	alpha, alphaCM := tBackend(t, "alpha", false, nil)
	defer alphaCM.Disconnect()
	if err := alpha.Unlock(walletPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}

	seed := randBytes(32)

	// Wallet A: fund a sequence of distinct external addresses. No recovery
	// window — it derives and funds its own addresses, nothing to discover.
	wA, cmA := connectElectrumWalletAt(t, t.TempDir(), seed, 0)
	waitElectrumSynced(t, wA)

	const nAddrs = 8
	const perAddr = 1 // BTC each
	seen := make(map[string]bool)
	for i := 0; i < nAddrs; i++ {
		addr, err := wA.DepositAddress()
		if err != nil {
			t.Fatalf("DepositAddress %d: %v", i, err)
		}
		if seen[addr] {
			t.Fatalf("DepositAddress did not advance at i=%d (addr %s reused)", i, addr)
		}
		seen[addr] = true
		if _, err := alpha.Send(addr, toSatoshi(perAddr), 10); err != nil {
			t.Fatalf("alpha.Send %d: %v", i, err)
		}
		if err := mineAlpha(); err != nil {
			t.Fatalf("mineAlpha %d: %v", i, err)
		}
		// Wait for A to register the receive so DepositAddress advances.
		waitElectrumBalanceAtLeast(t, wA, toSatoshi(float64(i+1)*perAddr))
	}
	wantTotal := toSatoshi(nAddrs * perAddr)
	cmA.Disconnect()

	// Wallet B: fresh wallet, same seed, WITH a recovery window. The gap-limit
	// scan must rediscover everything wallet A funded.
	wB, cmB := connectElectrumWalletAt(t, t.TempDir(), seed, 20)
	defer cmB.Disconnect()
	waitElectrumSynced(t, wB)

	bal := waitElectrumBalanceAtLeast(t, wB, wantTotal)
	t.Logf("recovered balance: available=%d (want>=%d across %d addrs)", bal.Available, wantTotal, nAddrs)
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
