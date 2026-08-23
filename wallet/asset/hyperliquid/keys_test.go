// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// TestPrivKeyFromSeedMatchesInternalDerivation guards the invariant the
// hyperevm wallet depends on to recover funds sent to a Hyperliquid address
// over HyperEVM: the exported PrivKeyFromSeed must derive the exact same key
// (and therefore address) as this package's own internal privKeyFromSeed. If
// someone changes one derivation path without the other, HyperEVM and
// HyperCore silently diverge into two different addresses and funds sent to
// one become unreachable from the other.
func TestPrivKeyFromSeedMatchesInternalDerivation(t *testing.T) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	wantPriv, wantAddr, zero, err := privKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("privKeyFromSeed: %v", err)
	}
	defer zero()
	wantRaw := gethcrypto.FromECDSA(wantPriv)

	gotRaw, zeroGot, err := PrivKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("PrivKeyFromSeed: %v", err)
	}
	defer zeroGot()

	if !bytes.Equal(gotRaw, wantRaw) {
		t.Fatalf("PrivKeyFromSeed diverged from the internal derivation: got %x, want %x", gotRaw, wantRaw)
	}

	gotPriv, err := gethcrypto.ToECDSA(gotRaw)
	if err != nil {
		t.Fatalf("ToECDSA: %v", err)
	}
	gotAddr := gethcrypto.PubkeyToAddress(gotPriv.PublicKey).Hex()
	if !strings.EqualFold(gotAddr, wantAddr) {
		t.Fatalf("derived address %s does not match HyperCore's own %s", gotAddr, wantAddr)
	}
}

// TestPrivKeyFromSeedZeroesOwnCopy ensures the wrapper's zero func clears the
// bytes it itself allocated (gethcrypto.FromECDSA returns a fresh slice, not
// the one the internal zero func already clears), not just the internal
// derivation's intermediate buffer.
func TestPrivKeyFromSeedZeroesOwnCopy(t *testing.T) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	raw, zero, err := PrivKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("PrivKeyFromSeed: %v", err)
	}
	var nonZero bool
	for _, b := range raw {
		if b != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		t.Fatal("expected a freshly derived key to be non-zero before zero() is called")
	}
	zero()
	for i, b := range raw {
		if b != 0 {
			t.Fatalf("zero() left byte %d non-zero: key was not cleared", i)
		}
	}
}
