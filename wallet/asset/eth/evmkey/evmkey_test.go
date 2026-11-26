package evmkey

import (
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/hdkeychain/v3"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestPrivKeyFromSeedAccountVectors pins the derivation behind every EVM
// wallet (account 0) and its neighbor account for a fixed asset seed.
// Changing either value changes the address of existing wallets, and
// account 0 is also the RHEA lending identity, so a failure here is a
// fund-recovery regression rather than a test to update.
func TestPrivKeyFromSeedAccountVectors(t *testing.T) {
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	tests := []struct {
		account uint32
		key     string
		addr    string
	}{
		{0, "e3b99b954c842dbd1e61148af4c295e15fda6d41d1633fd10fab2c6090f42669", "0xF9297b542BDb5DA50C364f9AE4Cbe1F3933bA40F"},
		{1, "a68ef00dea0b7c8e485d44634411584ddf4a7b42db5930c4e8a0cb7f7b4300d2", "0x8771B6667865B8Ce9586686211A271d496C90552"},
	}
	for _, tt := range tests {
		pk, zero, err := PrivKeyFromSeedAccount(seed, tt.account)
		if err != nil {
			t.Fatalf("account %d: %v", tt.account, err)
		}
		if got := hex.EncodeToString(pk); got != tt.key {
			t.Fatalf("account %d key = %s, want %s", tt.account, got, tt.key)
		}
		priv, err := crypto.ToECDSA(pk)
		if err != nil {
			t.Fatalf("account %d: ToECDSA: %v", tt.account, err)
		}
		if got := crypto.PubkeyToAddress(priv.PublicKey).Hex(); got != tt.addr {
			t.Fatalf("account %d address = %s, want %s", tt.account, got, tt.addr)
		}
		zero()
		for i, b := range pk {
			if b != 0 {
				t.Fatalf("account %d: key byte %d not zeroed", tt.account, i)
			}
		}
	}
	if _, _, err := PrivKeyFromSeedAccount(seed, hdkeychain.HardenedKeyStart); err == nil {
		t.Fatalf("hardened-range account accepted")
	}
	if _, _, err := PrivKeyFromSeedAccount(seed[:16], 0); err == nil {
		t.Fatalf("short seed accepted")
	}
}
