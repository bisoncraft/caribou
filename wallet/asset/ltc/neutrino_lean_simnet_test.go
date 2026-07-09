//go:build harness

package ltc

// Proves the SPV unification for LTC: walletTypeSPV, routed through NewWallet,
// builds the same lean wallet electrum does (btc.NativeWallet) driven by the
// neutrino backend with LTC's seeds and chain-service wrapper, and discovers
// funds paid to it over neutrino — the LTC counterpart to btc's
// TestNeutrinoLeanWalletSync.
//
// It funds the wallet's own deposit address at runtime (like the LTC electrum
// tests) rather than asserting pre-existing on-chain funds: the LTC harness
// seeds its gamma wallet from a key unrelated to ltcGammaSeed, so nothing
// on-chain pays the test wallet's derived addresses (BTC's harness, by
// contrast, funds the same seed its test uses).
//
// Prereqs: the dextest LTC SPV harness (litecoind + the neutrino-serving node on
// 127.0.0.1:20585) must be running.

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
)

func TestLTCNeutrinoLeanWalletSync(t *testing.T) {
	// Funding source: the harness alpha litecoind wallet.
	alpha, alphaCM := newLTCAlphaBackend(t)
	defer alphaCM.Disconnect()
	if err := alpha.Unlock(ltcPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}

	seed, err := hex.DecodeString(ltcGammaSeed)
	if err != nil {
		t.Fatalf("decode ltcGammaSeed: %v", err)
	}

	dataDir := t.TempDir()
	walletDir := filepath.Join(dataDir, RegressionNetParams.Name)
	if err = createLTCElectrumWallet(ltcPassword, seed, btc.DefaultWalletBirthday, walletDir, ltcLogger, 0, 0, RegressionNetParams); err != nil {
		t.Fatalf("createLTCElectrumWallet: %v", err)
	}

	// walletTypeSPV routes through NewWallet -> openLTCSPVWallet (the real path).
	walletCfg := &asset.WalletConfig{
		Type:     walletTypeSPV,
		DataDir:  dataDir,
		Settings: map[string]string{},
		Emit:     asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), BipID, ltcLogger),
		PeersChange: func(n uint32, err error) {
			ltcLogger.Debugf("LTC neutrino lean wallet peers=%d err=%v", n, err)
		},
	}

	w, err := NewWallet(walletCfg, ltcLogger, util.Regtest)
	if err != nil {
		t.Fatalf("NewWallet: %v", err)
	}

	cm := util.NewConnectionMaster(w)
	if err = cm.Connect(ltcCtx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer cm.Disconnect()

	waitLTCElectrumSynced(t, w)

	balBefore, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance before: %v", err)
	}

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

	// neutrino must discover the deposit to its own address via the live filter
	// scan and credit it.
	wantMin := balBefore.Available + toSatoshiLTC(fundAmt)
	bal := waitLTCBalanceAtLeast(t, w, wantMin)
	t.Logf("LTC neutrino lean wallet discovered deposit; available=%d (want>=%d)", bal.Available, wantMin)
}
