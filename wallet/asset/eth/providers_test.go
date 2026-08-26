// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"math/big"
	"slices"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/ethereum/go-ethereum/params"
)

// TestLimitDefaultProviders pins the provider cap per build. The expectation is
// build-tagged alongside the function itself; see providers_desktop.go and
// providers_mobile.go.
func TestLimitDefaultProviders(t *testing.T) {
	full := []string{"one", "two", "three", "four", "five"}

	// The input must survive: it is package-level state in the chain packages,
	// shared by every wallet they construct, so a cap that aliased or truncated
	// it in place would corrupt the next wallet's list.
	input := slices.Clone(full)
	got := limitDefaultProviders(input)
	if !slices.Equal(input, full) {
		t.Fatalf("input mutated to %v, want %v", input, full)
	}
	if len(got) != wantProviderLimit {
		t.Fatalf("limited %d providers to %d, want %d", len(full), len(got), wantProviderLimit)
	}
	if !slices.Equal(got, full[:wantProviderLimit]) {
		t.Fatalf("kept %v, want the first %d of %v", got, wantProviderLimit, full)
	}

	// When a trim happened the result must be a copy, so that writing through it
	// cannot reach the shared list. A build that does not trim returns the very
	// same slice, which is correct and nothing to assert against.
	if len(got) < len(full) && len(got) > 0 {
		got[0] = "mutated"
		if input[0] != full[0] {
			t.Fatalf("trimmed result aliases the input: input[0] = %q", input[0])
		}
	}

	// A list already at or under the cap comes back whole.
	short := []string{"only"}
	if kept := limitDefaultProviders(short); !slices.Equal(kept, short) {
		t.Fatalf("limited a single provider to %v, want %v", kept, short)
	}

	// Empty and nil must not panic. Nothing configured is a real case: the
	// simnet lists are a single local endpoint and a chain could define none.
	if kept := limitDefaultProviders(nil); len(kept) != 0 {
		t.Fatalf("limited nil to %v, want empty", kept)
	}
	if kept := limitDefaultProviders([]string{}); len(kept) != 0 {
		t.Fatalf("limited an empty list to %v, want empty", kept)
	}
}

// TestNewEVMWalletLimitsDefaultProviders pins the cap's wiring, not its
// arithmetic. TestLimitDefaultProviders above proves the function trims; this
// proves NewEVMWallet is still the thing that calls it. Without this, dropping
// the call at the ETHWallet literal leaves every test in the package green
// while a phone silently goes back to dialing the full list, which is the whole
// cost this build tag exists to avoid.
//
// The assertion compares against limitDefaultProviders rather than a literal so
// it holds under both tags. That means a desktop build cannot fail it: there the
// function is the identity, so a missing call is by definition unobservable. The
// mobile build is where this bites, and it is the build that matters.
func TestNewEVMWalletLimitsDefaultProviders(t *testing.T) {
	full := []string{"one", "two", "three", "four", "five"}

	// The chain packages hand the same package-level slice to every wallet they
	// construct, so pass a clone and check the original is untouched.
	input := slices.Clone(full)

	w, err := NewEVMWallet(&EVMWalletConfig{
		BaseChainID: 60,
		ChainCfg:    &params.ChainConfig{ChainID: big.NewInt(1)},
		AssetCfg: &asset.WalletConfig{
			Type:     walletTypeRPC,
			Settings: map[string]string{},
			DataDir:  t.TempDir(),
		},
		// A local logger, not the shared tLogger: this file is untagged and so
		// must build under every tag set, while tLogger lives in a file excluded
		// by the harness and rpclive builds.
		Logger:           util.StdOutLogger("PROVIDERSTEST", util.LevelOff),
		Net:              util.Simnet,
		DefaultProviders: input,
	})
	if err != nil {
		t.Fatalf("NewEVMWallet: %v", err)
	}

	want := limitDefaultProviders(full)
	if !slices.Equal(w.defaultProviders, want) {
		t.Fatalf("wallet carries %v, want %v — is limitDefaultProviders still called in NewEVMWallet?",
			w.defaultProviders, want)
	}
	if !slices.Equal(input, full) {
		t.Fatalf("construction mutated the caller's list to %v, want %v", input, full)
	}
}
