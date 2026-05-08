package wallet

import (
	"os"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/assets"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
)

// TestScopeForPurposeBTC verifies that for the BTC testnet params used by
// most tests in this package the helper returns {Purpose, Coin: 0}. This
// fixes the existing observed value as a regression boundary against future
// drift in BTCParams["testnet"]'s HDCoinType.
func TestScopeForPurposeBTC(t *testing.T) {
	w, cleanup := testWallet(t)
	defer cleanup()

	for _, purpose := range []uint32{44, 49, 84, 86} {
		got := w.ScopeForPurpose(purpose)
		if got.Purpose != purpose {
			t.Errorf("ScopeForPurpose(%d).Purpose = %d, want %d",
				purpose, got.Purpose, purpose)
		}
		// BTCParams["testnet"] is currently HDCoinType=0; see
		// DERIVATION.md for the rationale.
		if got.Coin != 0 {
			t.Errorf("ScopeForPurpose(%d).Coin = %d, want 0",
				purpose, got.Coin)
		}
	}
}

// TestScopeForPurposeLTC exercises the coin-aware ScopeForPurpose helper
// against a real non-zero HDCoinType. assets.LTCParams["mainnet"] sets
// HDCoinType=2 (SLIP-44 Litecoin), so a wallet rooted at this entry must:
//
//  1. resolve w.ScopeForPurpose(p) to {Purpose: p, Coin: 2} for every BIP-44
//     purpose code,
//
//  2. create scoped key managers at coin index 2 such that NewAddress under
//     the resolved scope produces a pubkey whose DerivationInfo() return
//     value embeds the same KeyScope.
//
// This is the production path for every Litecoin address Boss Wallet will
// ever derive. Closes audit finding C2.
func TestScopeForPurposeLTC(t *testing.T) {
	w, cleanup := testWalletForLTC(t)
	defer cleanup()

	// Stage 1: ScopeForPurpose returns {Purpose, 2} for every BIP-44 purpose.
	for _, purpose := range []uint32{44, 49, 84, 86} {
		got := w.ScopeForPurpose(purpose)
		if got.Purpose != purpose || got.Coin != 2 {
			t.Errorf("ScopeForPurpose(%d) = %+v, want {Purpose: %d, Coin: 2}",
				purpose, got, purpose)
		}
	}

	// Stage 2: an address derived under ScopeForPurpose(84) must report
	// the same scope (and therefore the same coin index) when we look up
	// its ManagedPubKeyAddress and ask for its derivation info.
	scope := w.ScopeForPurpose(84)
	addr, err := w.NewAddress(0, scope)
	if err != nil {
		t.Fatalf("NewAddress: %v", err)
	}

	var (
		gotScope waddrmgr.KeyScope
		gotPath  waddrmgr.DerivationPath
	)
	err = walletdb.View(w.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		mgrAddr, err := w.Manager.Address(ns, addr)
		if err != nil {
			return err
		}
		pkAddr, ok := mgrAddr.(waddrmgr.ManagedPubKeyAddress)
		if !ok {
			t.Fatalf("expected ManagedPubKeyAddress, got %T", mgrAddr)
		}
		var derived bool
		gotScope, gotPath, derived = pkAddr.DerivationInfo()
		if !derived {
			t.Fatal("DerivationInfo reports the address is not derived")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Manager.Address lookup: %v", err)
	}

	if gotScope.Purpose != 84 {
		t.Errorf("derived scope Purpose = %d, want 84", gotScope.Purpose)
	}
	if gotScope.Coin != 2 {
		t.Errorf("derived scope Coin = %d, want 2 (SLIP-44 Litecoin)",
			gotScope.Coin)
	}
	// On a brand-new wallet the first external address sits at
	// m/84'/2'/0'/0/0. DerivationPath.Account stores the *hardened*
	// child index (0x80000000 | account), while InternalAccount stores
	// the wallet-database-internal account number (0).
	const wantAccountHardened = uint32(0) | hdkeychain.HardenedKeyStart
	if gotPath.InternalAccount != 0 ||
		gotPath.Account != wantAccountHardened ||
		gotPath.Branch != waddrmgr.ExternalBranch ||
		gotPath.Index != 0 {

		t.Errorf("derivation path = {InternalAccount=%d, Account=0x%x, "+
			"Branch=%d, Index=%d}, want {0, 0x%x, %d, 0}",
			gotPath.InternalAccount, gotPath.Account,
			gotPath.Branch, gotPath.Index,
			wantAccountHardened, waddrmgr.ExternalBranch)
	}
}

// testWalletForLTC builds and unlocks a wallet rooted at the LTC mainnet
// chain params. Kept separate from testWallet so the BTC-coin-zero
// assumptions of the existing tests are not perturbed.
func testWalletForLTC(t *testing.T) (*Wallet, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("", "test_wallet_ltc")
	if err != nil {
		t.Fatalf("Failed to create db dir: %v", err)
	}
	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("could not cleanup test: %v", err)
		}
	}

	seed, err := hdkeychain.GenerateSeed(hdkeychain.MinSeedBytes)
	if err != nil {
		t.Fatalf("unable to create seed: %v", err)
	}

	pubPass := []byte("hello")
	privPass := []byte("world")

	loader := NewLoader(
		assets.LTCParams["mainnet"], dir, true, defaultDBTimeout, 250,
		WithWalletSyncRetryInterval(10*time.Millisecond),
	)
	w, err := loader.CreateNewWallet(pubPass, privPass, seed, time.Now())
	if err != nil {
		cleanup()
		t.Fatalf("unable to create wallet: %v", err)
	}
	w.chainClient = &mockChainClient{}
	if err := w.Unlock(privPass, time.After(10*time.Minute)); err != nil {
		cleanup()
		t.Fatalf("unable to unlock wallet: %v", err)
	}
	return w, cleanup
}
