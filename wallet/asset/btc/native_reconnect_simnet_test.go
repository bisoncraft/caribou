//go:build harness

package btc

// Validates that a NativeWallet can be disconnected and reconnected in the same
// process: disconnect must unload the btcwallet (closing its db) so a fresh
// Connect can reopen it — otherwise the bdb file lock blocks the second open.
// This reproduces btcSPVWallet.Stop's UnloadWallet behavior on the shared wallet.
//
// Prereqs: the dextest BTC electrum harness (electrumx on 127.0.0.1:54002).

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/btcsuite/btcd/chaincfg"
)

func TestNativeWalletReconnect(t *testing.T) {
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

	mkCfg := func() *asset.WalletConfig {
		return &asset.WalletConfig{
			Type:        walletTypeElectrum,
			DataDir:     dataDir,
			Settings:    map[string]string{},
			Emit:        asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), 0, tLogger),
			NetworkTxDB: masterDB.NetworkTxDB(BipID),
			PeersChange: func(n uint32, err error) {},
		}
	}

	connectOnce := func(label string) {
		w := NewNativeWallet(btcElectrumChainConfig(&chaincfg.RegressionNetParams), mkCfg(), tLogger)
		cm := util.NewConnectionMaster(w)
		if err := cm.Connect(tCtx); err != nil {
			t.Fatalf("%s Connect: %v", label, err)
		}
		waitElectrumSynced(t, w)
		if _, err := w.Balance(); err != nil {
			t.Fatalf("%s Balance: %v", label, err)
		}
		// Disconnect waits on the Connect WaitGroup, which includes the unload.
		cm.Disconnect()
	}

	connectOnce("first")
	// The second open reuses the same wallet dir; it succeeds only because the
	// first disconnect unloaded the wallet db.
	connectOnce("second (reconnect)")
}
