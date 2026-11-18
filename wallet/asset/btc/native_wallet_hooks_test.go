// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package btc

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/bisoncraft/caribou/wallet/asset"
)

func TestInnerWallet(t *testing.T) {
	w := NewNativeWallet(btcElectrumChainConfig(&chaincfg.RegressionNetParams), &asset.WalletConfig{}, tLogger)
	if w.InnerWallet() != nil {
		t.Fatal("the inner wallet is set before Connect")
	}
	w = newOfflineNativeWallet(t)
	if w.InnerWallet() == nil || w.InnerWallet() != w.btcw {
		t.Fatal("InnerWallet is not the open wallet")
	}
}

// The chain's hook changes the balance Balance returns, so that a balance the
// wallet pushes by itself has it too.
func TestBalanceAdjustHook(t *testing.T) {
	w := newOfflineNativeWallet(t)
	bal, err := w.Balance()
	if err != nil {
		t.Fatal(err)
	}
	if bal.Available != 0 || bal.Locked != 0 || len(bal.Other) != 0 {
		t.Fatalf("an empty wallet's balance is %+v", bal)
	}

	calls := 0
	w.cfg.AdjustBalance = func(b *asset.Balance) {
		calls++
		b.Locked += 9
		b.Other[asset.BalanceCategoryStaked] = asset.CustomBalance{Amount: 9}
	}
	bal, err = w.Balance()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || bal.Locked != 9 || bal.Other[asset.BalanceCategoryStaked].Amount != 9 {
		t.Fatalf("%d calls, balance %+v", calls, bal)
	}
}
