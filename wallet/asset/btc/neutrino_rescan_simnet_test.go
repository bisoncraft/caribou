//go:build harness

package btc

// Verifies asset.Rescanner on the neutrino-backed wallet: a rescan drops the
// wallet's transaction history and rediscovers its funds from the seed birthday,
// and — critically — the wallet keeps detecting new deposits afterward, proving
// the notification path (blockLoop's NtfnServer subscription) survives the
// stop/restart the rescan performs.
//
// Prereqs: the dextest BTC SPV harness must be running.

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/btcsuite/btcd/chaincfg"
)

func TestNeutrinoRescan(t *testing.T) {
	seed, err := hex.DecodeString(gammaSeed)
	if err != nil {
		t.Fatalf("decode gammaSeed: %v", err)
	}

	dataDir := t.TempDir()
	walletDir := filepath.Join(dataDir, chaincfg.RegressionNetParams.Name)
	if err = createElectrumWallet(walletPassword, seed, DefaultWalletBirthday, walletDir, tLogger, 0, 0, &chaincfg.RegressionNetParams); err != nil {
		t.Fatalf("createElectrumWallet: %v", err)
	}

	masterDB, err := txdb.New(filepath.Join(dataDir, "txdb"), tLogger.SubLogger("TXDB"))
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	if _, err = masterDB.Connect(tCtx); err != nil {
		t.Fatalf("txdb.Connect: %v", err)
	}
	t.Cleanup(func() { masterDB.Close() })

	walletCfg := &asset.WalletConfig{
		Type:        walletTypeSPV,
		DataDir:     dataDir,
		Settings:    map[string]string{},
		Emit:        asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), 0, tLogger),
		NetworkTxDB: masterDB.NetworkTxDB(BipID),
		PeersChange: func(n uint32, err error) {},
	}

	w := NewNativeWallet(btcSPVChainConfig(&chaincfg.RegressionNetParams), walletCfg, tLogger)
	cm := util.NewConnectionMaster(w)
	if err = cm.Connect(tCtx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer cm.Disconnect()

	// Initial discovery of the pre-funded gamma seed.
	waitElectrumSynced(t, w)
	bal1 := waitElectrumBalanceAtLeast(t, w, 1)
	t.Logf("before rescan: available=%d", bal1.Available)

	// Rescan: drop history + re-recover from the birthday.
	if err = w.Rescan(tCtx, 0); err != nil {
		t.Fatalf("Rescan: %v", err)
	}

	// The same funds must be rediscovered.
	waitElectrumSynced(t, w)
	bal2 := waitElectrumBalanceAtLeast(t, w, bal1.Available)
	t.Logf("after rescan: available=%d", bal2.Available)

	// And the wallet must still detect new activity — the notification path
	// survived the rescan's stop/restart.
	alpha, alphaCM := tBackend(t, "alpha", false, nil)
	defer alphaCM.Disconnect()
	if err = alpha.Unlock(walletPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const fundAmt = 5 // BTC
	if _, err = alpha.Send(addr, toSatoshi(fundAmt), 10); err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}
	if err = mineAlpha(); err != nil {
		t.Fatalf("mineAlpha: %v", err)
	}

	bal3 := waitElectrumBalanceAtLeast(t, w, bal2.Available+toSatoshi(fundAmt))
	t.Logf("after post-rescan deposit: available=%d", bal3.Available)
}

// TestElectrumRescan verifies the electrum backend refuses a rescan cleanly. The
// shared wallet implements asset.Rescanner for the neutrino backend, but electrum
// can't restart its network in place and rediscovers funds by re-querying address
// history every sync, so Rescan returns an error instead of tearing the network
// down. The wallet must stay functional afterward.
func TestElectrumRescan(t *testing.T) {
	w, cm := newElectrumWallet(t)
	defer cm.Disconnect()
	waitElectrumSynced(t, w)

	rescanner, ok := w.(asset.Rescanner)
	if !ok {
		t.Fatal("electrum wallet does not implement asset.Rescanner")
	}
	if err := rescanner.Rescan(tCtx, 0); err == nil {
		t.Fatal("expected Rescan to return an error for the electrum backend")
	} else {
		t.Logf("electrum Rescan correctly refused: %v", err)
	}

	// The wallet must remain functional after the refused rescan.
	if _, err := w.SyncStatus(); err != nil {
		t.Fatalf("SyncStatus after refused rescan: %v", err)
	}
}
