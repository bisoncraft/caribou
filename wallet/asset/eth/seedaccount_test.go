//go:build !harness && !rpclive

package eth

import (
	"encoding/hex"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset/eth/evmkey"
)

// TestPrivKeyFromSeedIsEVMKeyAccount0 ties the wallet's derivation to
// evmkey account 0 and to the pinned vector there: the RHEA lending
// identity derives through evmkey and must equal the wallet's key.
func TestPrivKeyFromSeedIsEVMKeyAccount0(t *testing.T) {
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	walletKey, zero, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("privKeyFromSeed: %v", err)
	}
	got := hex.EncodeToString(walletKey)
	zero()
	if want := "e3b99b954c842dbd1e61148af4c295e15fda6d41d1633fd10fab2c6090f42669"; got != want {
		t.Fatalf("privKeyFromSeed = %s, want %s", got, want)
	}
	acct0, zero, err := evmkey.PrivKeyFromSeedAccount(seed, 0)
	if err != nil {
		t.Fatalf("PrivKeyFromSeedAccount: %v", err)
	}
	defer zero()
	if hex.EncodeToString(acct0) != got {
		t.Fatalf("evmkey account 0 differs from the wallet key")
	}
}
