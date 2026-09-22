// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/ethereum/go-ethereum/common"
)

// unknownNet is a Network value that isn't Mainnet/Testnet/Simnet, used to
// exercise the default error branches.
const unknownNet = util.Network(99)

func TestNetworkCompatibilityData(t *testing.T) {
	t.Parallel()
	t.Run("mainnet", func(t *testing.T) {
		t.Parallel()
		got, err := NetworkCompatibilityData(util.Mainnet)
		if err != nil {
			t.Fatal(err)
		}
		if got != mainnetCompatibilityData {
			t.Errorf("got %+v, want %+v", got, mainnetCompatibilityData)
		}
	})

	t.Run("testnet", func(t *testing.T) {
		t.Parallel()
		got, err := NetworkCompatibilityData(util.Testnet)
		if err != nil {
			t.Fatal(err)
		}
		if got != testnetCompatibilityData {
			t.Errorf("got %+v, want %+v", got, testnetCompatibilityData)
		}
	})

	t.Run("unknown network errors", func(t *testing.T) {
		t.Parallel()
		if _, err := NetworkCompatibilityData(unknownNet); err == nil {
			t.Error("expected error for unknown network")
		}
	})
}

func TestSimnetDataDir(t *testing.T) {
	t.Parallel()
	dir, err := simnetDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dir, filepath.Join("dextest", "eth")) {
		t.Errorf("simnetDataDir = %q, want a path ending in dextest/eth", dir)
	}
}

func TestETHConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		net         util.Network
		wantChainID uint64
		wantErr     bool
	}{
		{"mainnet", util.Mainnet, 1, false},
		{"testnet sepolia", util.Testnet, 11155111, false},
		{"simnet developer", util.Simnet, 1337, false},
		{"unknown", unknownNet, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := ETHConfig(tc.net)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.NetworkId != tc.wantChainID {
				t.Errorf("NetworkId = %d, want %d", c.NetworkId, tc.wantChainID)
			}
			// NetworkId is derived from the genesis chain ID; they must agree.
			if got := c.Genesis.Config.ChainID.Uint64(); got != tc.wantChainID {
				t.Errorf("genesis chainID = %d, want %d", got, tc.wantChainID)
			}
		})
	}
}

func TestReadSimnetCompatibilityData(t *testing.T) {
	t.Parallel()

	writeHarnessFiles := func(t *testing.T, dir string, withTokenAddrFile bool) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "test_tx_hash.txt"),
			[]byte("67b27503aa20dd35bc2ae18fabfec477cf4d089b32173a590dc8d7319cbd0184\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "test_block1_hash.txt"),
			[]byte("95118897eba948a62df72765e20186081f2554d90a146403ba93da690b322e06\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if withTokenAddrFile {
			if err := os.WriteFile(filepath.Join(dir, "test_usdc_contract_address.txt"),
				[]byte("1c7D4B196Cb0C7B01d743Fbc6116a902379C7238\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("reads token address from file when tokenAddr is the zero address", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeHarnessFiles(t, dir, true)
		walletAddr := common.HexToAddress("18d65fb8d60c1199bb1ad381be47aa692b482605")
		got, err := ReadSimnetCompatibilityData(dir, walletAddr, common.Address{})
		if err != nil {
			t.Fatal(err)
		}
		want := CompatibilityData{
			Addr:      walletAddr,
			TokenAddr: common.HexToAddress("1c7D4B196Cb0C7B01d743Fbc6116a902379C7238"),
			TxHash:    common.HexToHash("67b27503aa20dd35bc2ae18fabfec477cf4d089b32173a590dc8d7319cbd0184"),
			BlockHash: common.HexToHash("95118897eba948a62df72765e20186081f2554d90a146403ba93da690b322e06"),
		}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("uses the passed-in tokenAddr without reading the file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeHarnessFiles(t, dir, false)
		tokenAddr := common.HexToAddress("a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48")
		got, err := ReadSimnetCompatibilityData(dir, common.Address{}, tokenAddr)
		if err != nil {
			t.Fatal(err)
		}
		if got.TokenAddr != tokenAddr {
			t.Errorf("TokenAddr = %s, want %s", got.TokenAddr, tokenAddr)
		}
	})

	t.Run("missing tx hash file errors", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if _, err := ReadSimnetCompatibilityData(dir, common.Address{}, common.Address{}); err == nil {
			t.Error("expected error for missing test_tx_hash.txt")
		}
	})

	t.Run("missing block hash file errors", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "test_tx_hash.txt"), []byte("aa"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSimnetCompatibilityData(dir, common.Address{}, common.Address{}); err == nil {
			t.Error("expected error for missing test_block1_hash.txt")
		}
	})

	t.Run("missing token address file errors", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeHarnessFiles(t, dir, false)
		if _, err := ReadSimnetCompatibilityData(dir, common.Address{}, common.Address{}); err == nil {
			t.Error("expected error for missing test_usdc_contract_address.txt")
		}
	})
}

func TestChainConfig(t *testing.T) {
	t.Parallel()
	t.Run("mainnet", func(t *testing.T) {
		t.Parallel()
		cfg, err := ChainConfig(util.Mainnet)
		if err != nil {
			t.Fatal(err)
		}
		if cfg == nil || cfg.ChainID == nil || cfg.ChainID.Uint64() != 1 {
			t.Fatalf("unexpected mainnet chain config: %+v", cfg)
		}
	})

	t.Run("unknown errors", func(t *testing.T) {
		t.Parallel()
		if _, err := ChainConfig(unknownNet); err == nil {
			t.Error("expected error for unknown network")
		}
	})
}
