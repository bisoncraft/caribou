// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
)

// unknownNet is a Network value that isn't Mainnet/Testnet/Simnet, used to
// exercise the default error branches.
const unknownNet = util.Network(99)

func TestNetworkCompatibilityData(t *testing.T) {
	t.Run("mainnet", func(t *testing.T) {
		got, err := NetworkCompatibilityData(util.Mainnet)
		if err != nil {
			t.Fatal(err)
		}
		if got != mainnetCompatibilityData {
			t.Errorf("got %+v, want %+v", got, mainnetCompatibilityData)
		}
	})

	t.Run("testnet", func(t *testing.T) {
		got, err := NetworkCompatibilityData(util.Testnet)
		if err != nil {
			t.Fatal(err)
		}
		if got != testnetCompatibilityData {
			t.Errorf("got %+v, want %+v", got, testnetCompatibilityData)
		}
	})

	t.Run("unknown network errors", func(t *testing.T) {
		if _, err := NetworkCompatibilityData(unknownNet); err == nil {
			t.Error("expected error for unknown network")
		}
	})
}

func TestSimnetDataDir(t *testing.T) {
	dir, err := simnetDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dir, filepath.Join("dextest", "eth")) {
		t.Errorf("simnetDataDir = %q, want a path ending in dextest/eth", dir)
	}
}

func TestETHConfig(t *testing.T) {
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

func TestChainConfig(t *testing.T) {
	t.Run("mainnet", func(t *testing.T) {
		cfg, err := ChainConfig(util.Mainnet)
		if err != nil {
			t.Fatal(err)
		}
		if cfg == nil || cfg.ChainID == nil || cfg.ChainID.Uint64() != 1 {
			t.Fatalf("unexpected mainnet chain config: %+v", cfg)
		}
	})

	t.Run("unknown errors", func(t *testing.T) {
		if _, err := ChainConfig(unknownNet); err == nil {
			t.Error("expected error for unknown network")
		}
	})
}
