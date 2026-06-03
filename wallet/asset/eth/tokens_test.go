// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/ethereum/go-ethereum/common"
)

func i64ptr(v int64) *int64 { return &v }

func TestTokenFactor(t *testing.T) {
	tests := []struct {
		name      string
		evmFactor *int64
		want      *big.Int
	}{
		{"default is 9", nil, big.NewInt(1_000_000_000)},
		{"zero", i64ptr(0), big.NewInt(1)},
		{"six", i64ptr(6), big.NewInt(1_000_000)},
		{"eighteen", i64ptr(18), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tok := &Token{EVMFactor: tc.evmFactor}
			if got := tok.factor(); got.Cmp(tc.want) != 0 {
				t.Errorf("factor = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTokenAtomicEVMConversions(t *testing.T) {
	t.Run("default factor round trip", func(t *testing.T) {
		tok := &Token{} // default EVMFactor 9 -> factor 1e9
		evm := tok.AtomicToEVM(5)
		if evm.Cmp(big.NewInt(5_000_000_000)) != 0 {
			t.Fatalf("AtomicToEVM(5) = %s, want 5e9", evm)
		}
		if got := tok.EVMToAtomic(evm); got != 5 {
			t.Fatalf("EVMToAtomic(5e9) = %d, want 5", got)
		}
	})

	t.Run("EVMToAtomic truncates remainder", func(t *testing.T) {
		tok := &Token{} // factor 1e9
		// 5e9 + 123 wei still maps down to 5 atomic units.
		v := new(big.Int).Add(big.NewInt(5_000_000_000), big.NewInt(123))
		if got := tok.EVMToAtomic(v); got != 5 {
			t.Fatalf("EVMToAtomic = %d, want 5", got)
		}
	})

	t.Run("zero factor is identity", func(t *testing.T) {
		tok := &Token{EVMFactor: i64ptr(0)} // factor 1
		if got := tok.AtomicToEVM(42); got.Cmp(big.NewInt(42)) != 0 {
			t.Fatalf("AtomicToEVM(42) = %s, want 42", got)
		}
		if got := tok.EVMToAtomic(big.NewInt(42)); got != 42 {
			t.Fatalf("EVMToAtomic(42) = %d, want 42", got)
		}
	})

	t.Run("EVMToAtomic overflowing uint64 returns 0", func(t *testing.T) {
		tok := &Token{EVMFactor: i64ptr(0)} // factor 1, so vDEX == v
		// 2^65 does not fit in a uint64.
		v := new(big.Int).Lsh(big.NewInt(1), 65)
		if got := tok.EVMToAtomic(v); got != 0 {
			t.Fatalf("EVMToAtomic(2^65) = %d, want 0", got)
		}
	})
}

func TestRegisterEVMTokens(t *testing.T) {
	// Use an assetID that isn't in the real registry so we don't clobber it,
	// and clean up after ourselves since EVMTokens is package-global.
	const fakeID uint32 = 0xFFFFFEED
	if _, exists := EVMTokens[fakeID]; exists {
		t.Fatalf("test assetID %d unexpectedly already registered", fakeID)
	}
	t.Cleanup(func() { delete(EVMTokens, fakeID) })

	tok := &Token{Token: &util.Token{Name: "FAKE"}}
	RegisterEVMTokens(map[uint32]*Token{fakeID: tok})

	if EVMTokens[fakeID] != tok {
		t.Fatal("RegisterEVMTokens did not copy the token into EVMTokens")
	}
}

func TestMaybeGetContractAddrFromFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file is zero address", func(t *testing.T) {
		got := maybeGetContractAddrFromFile(filepath.Join(dir, "nope.txt"))
		if (got != common.Address{}) {
			t.Errorf("got %s, want zero address", got)
		}
	})

	t.Run("empty file is zero address", func(t *testing.T) {
		p := filepath.Join(dir, "empty.txt")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := maybeGetContractAddrFromFile(p); (got != common.Address{}) {
			t.Errorf("got %s, want zero address", got)
		}
	})

	t.Run("valid address with trailing newline", func(t *testing.T) {
		want := common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")
		p := filepath.Join(dir, "addr.txt")
		// The reader strips the final byte, expecting a trailing newline.
		if err := os.WriteFile(p, []byte(want.Hex()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := maybeGetContractAddrFromFile(p); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
}

// TestTokensRegistryConsistency asserts structural invariants of the static
// Tokens map that are easy to break when adding a new token or network.
func TestTokensRegistryConsistency(t *testing.T) {
	for assetID, tok := range Tokens {
		if tok.Token == nil {
			t.Errorf("assetID %d: nil embedded util.Token", assetID)
			continue
		}
		if tok.Name == "" {
			t.Errorf("assetID %d: empty token name", assetID)
		}
		if len(tok.NetTokens) == 0 {
			t.Errorf("assetID %d (%s): no NetTokens", assetID, tok.Name)
		}
		for net, nt := range tok.NetTokens {
			if len(nt.Gas) == 0 {
				t.Errorf("%s/%s: no gas tables", tok.Name, net)
			}
			// Simnet token/contract addresses are filled in at runtime from the
			// harness; mainnet and testnet must be present in the static map.
			if net != util.Simnet && (nt.Address == common.Address{}) {
				t.Errorf("%s/%s: zero token address", tok.Name, net)
			}
			// All v1 gas tables share the single tokenV1Gases schedule.
			if g, ok := nt.Gas[1]; ok && *g != tokenV1Gases {
				t.Errorf("%s/%s: v1 gas schedule diverges from tokenV1Gases", tok.Name, net)
			}
		}
	}
}
