//go:build harness

package dash

// neutrino_simnet_test.go contains harness integration tests for the
// neutrino-backed (BIP-157/158) Dash SPV wallet (walletTypeSPV). It shares the
// TestMain, mining helper, alpha RPC backend, and wait helpers defined in
// electrum_simnet_test.go.
//
// Unlike electrum, the SPV wallet peers directly with the regtest dashd over
// P2P and fetches compact filters, so the harness node must run with
// -peerblockfilters=1 -blockfilterindex=basic (baked into util/testing/dash/
// harness.sh). The wallet validates block headers by their X11 hash throughout
// the neutrino stack, and advertises Dash's minimum protocol version (70221)
// and a witness-free service mask in the version handshake.
//
// Prerequisites (run by the user):
//   - Dash regtest harness: cd util/testing/dash && ./harness.sh
//
// Run:
//
//	go test -tags harness ./wallet/asset/dash/ -run TestDashSPV

import (
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// newDashSPVWallet creates a seeded neutrino SPV Dash wallet in a temp dir,
// connects it (peering with the harness alpha node), and returns it.
func newDashSPVWallet(t *testing.T) (asset.Wallet, *util.ConnectionMaster) {
	t.Helper()
	seed, err := hex.DecodeString(dashGammaSeed)
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	// A recent birthday keeps the compact-filter scan tight (regtest blocks are
	// all "now"); it must predate any funding the tests do.
	return connectDashSPVWalletAt(t, t.TempDir(), seed, 0, time.Now().Add(-time.Hour))
}

// connectDashSPVWalletAt creates and connects a neutrino SPV Dash wallet. A
// recoveryWindow of 0 leaves the SPV default (neutrinoRecoveryWindow) in place —
// that window is what drives the compact-filter scan that discovers funds — so
// callers only pass a value to override it.
func connectDashSPVWalletAt(t *testing.T, dataDir string, seed []byte, recoveryWindow uint32, bday time.Time) (asset.Wallet, *util.ConnectionMaster) {
	t.Helper()
	walletDir := filepath.Join(dataDir, RegressionNetParams.Name)
	// SPV and electrum share the same on-disk wallet and create path.
	if err := createDashElectrumWallet(dashPassword, seed, bday, walletDir, dashLogger, 0, 0, RegressionNetParams); err != nil {
		t.Fatalf("createDashElectrumWallet (SPV): %v", err)
	}

	tdb, err := txdb.New(filepath.Join(dataDir, "tx.db"), dashLogger)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { tdb.Close() })

	settings := map[string]string{}
	if recoveryWindow > 0 {
		settings["recoverywindow"] = strconv.Itoa(int(recoveryWindow))
	}
	walletCfg := &asset.WalletConfig{
		Type:        walletTypeSPV,
		DataDir:     dataDir,
		Settings:    settings,
		NetworkTxDB: tdb.NetworkTxDB(BipID),
		Emit:        asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), BipID, dashLogger),
		PeersChange: func(n uint32, err error) {
			dashLogger.Debugf("spv DASH peers=%d err=%v", n, err)
		},
	}
	w, err := newWallet(walletCfg, dashLogger, util.Regtest)
	if err != nil {
		t.Fatalf("newWallet (SPV): %v", err)
	}
	cm := util.NewConnectionMaster(w)
	if err = cm.Connect(dashCtx); err != nil {
		t.Fatalf("Connect (SPV): %v", err)
	}
	return w, cm
}

// TestDashSPVConnectSyncStatus verifies the SPV wallet peers with the harness
// node, syncs headers (validated by X11) and filter headers, and reaches Synced.
func TestDashSPVConnectSyncStatus(t *testing.T) {
	w, cm := newDashSPVWallet(t)
	defer cm.Disconnect()

	waitDashSynced(t, w)

	ss, err := w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus: %v", err)
	}
	if !ss.Synced {
		t.Fatalf("expected Synced=true, got %+v", ss)
	}
	t.Logf("Dash SPV synced: height=%d target=%d", ss.Blocks, ss.TargetHeight)
}

// TestDashSPVDepositAddress verifies the SPV wallet derives a legacy Dash
// regtest P2PKH ('y...') deposit address.
func TestDashSPVDepositAddress(t *testing.T) {
	w, cm := newDashSPVWallet(t)
	defer cm.Disconnect()

	waitDashSynced(t, w)

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}
	if !strings.HasPrefix(addr, "y") {
		t.Fatalf("SPV deposit address %q does not start with 'y'", addr)
	}
	t.Logf("Dash SPV deposit address: %s", addr)
}

// TestDashSPVBalanceAfterFunding funds the SPV wallet from alpha and verifies
// the compact-filter scan detects it and the balance increases.
func TestDashSPVBalanceAfterFunding(t *testing.T) {
	alpha, alphaCM := newDashAlphaBackend(t)
	defer alphaCM.Disconnect()
	unlock(t, alpha)

	w, cm := newDashSPVWallet(t)
	defer cm.Disconnect()
	waitDashSynced(t, w)

	balBefore, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance before: %v", err)
	}

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}
	const fundAmt = 10 // DASH
	if _, err = alpha.Send(addr, toSatoshiDash(fundAmt), 10); err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}
	if err = mineDashAlpha(); err != nil {
		t.Fatalf("mineDashAlpha: %v", err)
	}

	wantMin := balBefore.Available + toSatoshiDash(fundAmt)
	bal := waitDashBalanceAtLeast(t, w, wantMin)
	t.Logf("Dash SPV balance after funding: available=%d (want>=%d)", bal.Available, wantMin)
}

// TestDashSPVSendSelfPendingToConfirmed funds the SPV wallet, self-sends, and
// asserts the tx goes pending -> confirmed.
func TestDashSPVSendSelfPendingToConfirmed(t *testing.T) {
	alpha, alphaCM := newDashAlphaBackend(t)
	defer alphaCM.Disconnect()
	unlock(t, alpha)

	w, cm := newDashSPVWallet(t)
	defer cm.Disconnect()
	waitDashSynced(t, w)
	unlock(t, w)

	fundAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress for funding: %v", err)
	}
	if _, err = alpha.Send(fundAddr, toSatoshiDash(20), 10); err != nil {
		t.Fatalf("alpha.Send for funding: %v", err)
	}
	if err = mineDashAlpha(); err != nil {
		t.Fatalf("mineDashAlpha after funding: %v", err)
	}
	waitDashBalanceAtLeast(t, w, toSatoshiDash(20))

	destAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress for self-send dest: %v", err)
	}
	coin, err := w.Send(destAddr, toSatoshiDash(5), 10)
	if err != nil {
		t.Fatalf("Send self: %v", err)
	}
	txID := coin.TxID()
	t.Logf("Dash SPV self-send txid: %s", txID)

	waitDashPendingContains(t, w, txID)
	if err = mineDashAlpha(); err != nil {
		t.Fatalf("mineDashAlpha after self-send: %v", err)
	}
	waitDashPendingNotContains(t, w, txID)

	wt := waitDashTxConfirmed(t, w, txID)
	t.Logf("Dash SPV self-send confirmed at block %d", wt.BlockNumber)
}

// TestDashSPVRecovery funds a run of external addresses from wallet A, then a
// fresh wallet B from the SAME seed with a recovery window must rediscover the
// funds via the compact-filter scan (the neutrino fund-discovery path under
// Dash's coin type m/44'/5').
func TestDashSPVRecovery(t *testing.T) {
	alpha, alphaCM := newDashAlphaBackend(t)
	defer alphaCM.Disconnect()
	unlock(t, alpha)

	seed := randBytes(t, 32)
	bday := time.Now().Add(-time.Hour)

	wA, cmA := connectDashSPVWalletAt(t, t.TempDir(), seed, 0, bday)
	waitDashSynced(t, wA)

	const nAddrs = 6
	const perAddr = 1 // DASH each
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
		if _, err := alpha.Send(addr, toSatoshiDash(perAddr), 10); err != nil {
			t.Fatalf("alpha.Send %d: %v", i, err)
		}
		if err := mineDashAlpha(); err != nil {
			t.Fatalf("mineDashAlpha %d: %v", i, err)
		}
		waitDashBalanceAtLeast(t, wA, toSatoshiDash(float64(i+1)*perAddr))
	}
	wantTotal := toSatoshiDash(nAddrs * perAddr)
	cmA.Disconnect()

	wB, cmB := connectDashSPVWalletAt(t, t.TempDir(), seed, 50, bday)
	defer cmB.Disconnect()
	waitDashSynced(t, wB)

	bal := waitDashBalanceAtLeast(t, wB, wantTotal)
	t.Logf("Dash SPV recovered balance: available=%d (want>=%d across %d addrs)", bal.Available, wantTotal, nAddrs)
}
