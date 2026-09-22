// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"math/big"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/bisoncraft/caribou/util"
)

func TestLoadGenesisFile(t *testing.T) {
	t.Parallel()

	t.Run("valid genesis file", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "genesis.json")
		const genesisJSON = `{"config":{"chainId":1337},"gasLimit":"0x1c9c380","difficulty":"0x1","alloc":{}}`
		if err := os.WriteFile(path, []byte(genesisJSON), 0644); err != nil {
			t.Fatal(err)
		}
		genesis, err := LoadGenesisFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if genesis.Config == nil || genesis.Config.ChainID == nil || genesis.Config.ChainID.Int64() != 1337 {
			t.Fatalf("genesis.Config.ChainID = %v, want 1337", genesis.Config)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadGenesisFile(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
			t.Error("expected error for a missing genesis file")
		}
	})

	t.Run("malformed json errors", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "bad-genesis.json")
		if err := os.WriteFile(path, []byte("not json"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadGenesisFile(path); err == nil {
			t.Error("expected error for malformed genesis JSON")
		}
	})
}

func TestConversions(t *testing.T) {
	t.Parallel()
	newBigIntFromStr := func(str string) *big.Int {
		t.Helper()
		bi, ok := new(big.Int).SetString(str, 10)
		if !ok {
			t.Fatalf("bad int str %v", str)
		}
		return bi
	}
	tests := []struct {
		name string
		gwei uint64
		wei  *big.Int
	}{
		{
			"ok",
			12,
			newBigIntFromStr("12000000000"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if gotWei := GweiToWei(tt.gwei); !reflect.DeepEqual(gotWei, tt.wei) {
				t.Errorf("GweiToWei() = %v, want %v", gotWei, tt.wei)
			}
			if gotGwei := WeiToGwei(tt.wei); !reflect.DeepEqual(gotGwei, tt.gwei) {
				t.Errorf("WeiToGwei() = %v, want %v", gotGwei, tt.wei)
			}
		})

	}
}

func TestBufferedGasLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		estimate, want uint64
	}{
		// Below the crossover (100,000), the additive floor dominates.
		{0, liveGasLimitBufferMin},
		{21_000, 41_000},
		{50_000, 70_000},
		{100_000, 120_000},
		// Above the crossover, the percentage dominates.
		{200_000, 240_000},
		{1_000_000, 1_200_000},
	}
	for _, test := range tests {
		if got := bufferedGasLimit(test.estimate); got != test.want {
			t.Errorf("bufferedGasLimit(%d) = %d, want %d", test.estimate, got, test.want)
		}
	}
}

func TestPAXGTokenDefinition(t *testing.T) {
	t.Parallel()
	tokenID, ok := util.BipSymbolID("paxg.eth")
	if !ok {
		t.Fatal("paxg.eth not registered")
	}

	token, ok := Tokens[tokenID]
	if !ok {
		t.Fatal("paxg.eth not in Tokens map")
	}

	// Decimal invariant: log10(ConversionFactor) + EVMFactor(default 9) = 18.
	cf := token.UnitInfo.Conventional.ConversionFactor
	if cf != 1e9 {
		t.Fatalf("unexpected ConversionFactor: got %d, want 1e9", cf)
	}
	if token.EVMFactor != nil {
		t.Fatalf("EVMFactor should be nil (default 9), got %d", *token.EVMFactor)
	}

	// Gas estimates on mainnet, testnet, and simnet.
	for _, net := range []util.Network{util.Mainnet, util.Testnet, util.Simnet} {
		netToken, ok := token.NetTokens[net]
		if !ok {
			t.Fatalf("missing NetToken for network %v", net)
		}
		if g := netToken.GasEstimate; g == nil || g.Approve == 0 || g.Transfer == 0 {
			t.Fatalf("network %v: missing gas estimate", net)
		}
	}

	// Mainnet and testnet addresses must be non-zero.
	for _, net := range []util.Network{util.Mainnet, util.Testnet} {
		if addr := token.NetTokens[net].Address; addr == (common.Address{}) {
			t.Fatalf("network %v: token address is zero", net)
		}
	}

	// Simnet address starts zero in source; MaybeReadSimnetAddrs (called from
	// init) populates it only when harness files exist. Skip on harness machines.
	usr, err := user.Current()
	if err == nil {
		harnessDir := filepath.Join(usr.HomeDir, "dextest", "eth")
		if _, err := os.Stat(harnessDir); os.IsNotExist(err) {
			if token.NetTokens[util.Simnet].Address != (common.Address{}) {
				t.Fatal("simnet address should be zero before harness runs")
			}
		}
	}
}

func TestXAUTTokenDefinition(t *testing.T) {
	t.Parallel()
	tokenID, ok := util.BipSymbolID("xaut.eth")
	if !ok {
		t.Fatal("xaut.eth not registered")
	}

	token, ok := Tokens[tokenID]
	if !ok {
		t.Fatal("xaut.eth not in Tokens map")
	}

	// Decimal invariant: log10(ConversionFactor) + EVMFactor(0) = 6.
	cf := token.UnitInfo.Conventional.ConversionFactor
	if cf != 1e6 {
		t.Fatalf("unexpected ConversionFactor: got %d, want 1e6", cf)
	}
	if token.EVMFactor == nil || *token.EVMFactor != 0 {
		t.Fatalf("EVMFactor should be 0, got %v", token.EVMFactor)
	}

	// Gas estimates on mainnet and simnet.
	for _, net := range []util.Network{util.Mainnet, util.Simnet} {
		netToken, ok := token.NetTokens[net]
		if !ok {
			t.Fatalf("missing NetToken for network %v", net)
		}
		if g := netToken.GasEstimate; g == nil || g.Approve == 0 || g.Transfer == 0 {
			t.Fatalf("network %v: missing gas estimate", net)
		}
	}

	// No official testnet deployment.
	if _, ok := token.NetTokens[util.Testnet]; ok {
		t.Fatal("unexpected Testnet entry for xaut.eth")
	}

	// Mainnet address must be non-zero.
	if addr := token.NetTokens[util.Mainnet].Address; addr == (common.Address{}) {
		t.Fatal("mainnet: token address is zero")
	}

	// Simnet address starts zero in source; MaybeReadSimnetAddrs (called from
	// init) populates it only when harness files exist. Skip on harness machines.
	usr, err := user.Current()
	if err == nil {
		harnessDir := filepath.Join(usr.HomeDir, "dextest", "eth")
		if _, err := os.Stat(harnessDir); os.IsNotExist(err) {
			if token.NetTokens[util.Simnet].Address != (common.Address{}) {
				t.Fatal("simnet address should be zero before harness runs")
			}
		}
	}
}

func TestLinkTokenDefinition(t *testing.T) {
	t.Parallel()
	tokenID, ok := util.BipSymbolID("link.eth")
	if !ok {
		t.Fatal("link.eth not registered")
	}

	token, ok := Tokens[tokenID]
	if !ok {
		t.Fatal("link.eth not in Tokens map")
	}

	// Decimal invariant: log10(ConversionFactor) + EVMFactor(default 9) = 18.
	cf := token.UnitInfo.Conventional.ConversionFactor
	if cf != 1e9 {
		t.Fatalf("unexpected ConversionFactor: got %d, want 1e9", cf)
	}
	if token.EVMFactor != nil {
		t.Fatalf("EVMFactor should be nil (default 9), got %d", *token.EVMFactor)
	}

	// Gas estimates on mainnet, testnet, and simnet.
	for _, net := range []util.Network{util.Mainnet, util.Testnet, util.Simnet} {
		netToken, ok := token.NetTokens[net]
		if !ok {
			t.Fatalf("missing NetToken for network %v", net)
		}
		if g := netToken.GasEstimate; g == nil || g.Approve == 0 || g.Transfer == 0 {
			t.Fatalf("network %v: missing gas estimate", net)
		}
	}

	// Mainnet and testnet addresses must be non-zero.
	for _, net := range []util.Network{util.Mainnet, util.Testnet} {
		addr := token.NetTokens[net].Address
		if addr == (common.Address{}) {
			t.Fatalf("network %v: token address is zero", net)
		}
	}

	// Simnet address starts zero in source; MaybeReadSimnetAddrs (called from
	// init) populates it only when harness files exist. Skip on harness machines.
	usr, err := user.Current()
	if err == nil {
		harnessDir := filepath.Join(usr.HomeDir, "dextest", "eth")
		if _, err := os.Stat(harnessDir); os.IsNotExist(err) {
			if token.NetTokens[util.Simnet].Address != (common.Address{}) {
				t.Fatal("simnet address should be zero before harness runs")
			}
		}
	}
}
