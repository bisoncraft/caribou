//go:build harness

package btc

// Proves the unification end-to-end: the same lean NativeWallet, given the
// neutrino backend factory instead of the electrum one, connects to the SPV
// harness and syncs the pre-funded gamma seed — i.e. SPV and electrum differ
// only in which chain.Backend they build. Does NOT yet rewire walletTypeSPV;
// that comes once this validates.
//
// Prereqs: the dextest BTC SPV harness (bitcoind alpha + the neutrino-serving
// node) must be running.

import (
	"path/filepath"
	"testing"

	"encoding/hex"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/btcsuite/btcd/chaincfg"
)

func TestNeutrinoLeanWalletSync(t *testing.T) {
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

	// The lean wallet with the SPV (neutrino) config — exactly what
	// openSPVWallet / walletTypeSPV builds, including the recovery-window
	// default that drives the cfilter scan. No manual settings.
	cc := btcSPVChainConfig(&chaincfg.RegressionNetParams)

	walletCfg := &asset.WalletConfig{
		Type:        walletTypeSPV,
		DataDir:     dataDir,
		Settings:    map[string]string{},
		Emit:        asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), 0, tLogger),
		NetworkTxDB: masterDB.NetworkTxDB(BipID),
		PeersChange: func(n uint32, err error) {
			tLogger.Debugf("neutrino lean wallet peers=%d err=%v", n, err)
		},
	}

	w := NewNativeWallet(cc, walletCfg, tLogger)
	cm := util.NewConnectionMaster(w)
	if err = cm.Connect(tCtx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer cm.Disconnect()

	waitElectrumSynced(t, w)
	bal := waitElectrumBalanceAtLeast(t, w, 1)
	t.Logf("neutrino lean wallet synced; available balance = %d", bal.Available)
}
