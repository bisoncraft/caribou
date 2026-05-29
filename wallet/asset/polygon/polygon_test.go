// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package polygon

import (
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/ethereum/go-ethereum/common"
)

func TestDriverInfo(t *testing.T) {
	drv := &Driver{}
	info := drv.Info()
	if info.Name != "Polygon" {
		t.Fatalf("Info.Name = %q, want %q", info.Name, "Polygon")
	}
	if info.BlockchainClass != asset.BlockchainClassEVM {
		t.Fatalf("Info.BlockchainClass = %q, want %q", info.BlockchainClass, asset.BlockchainClassEVM)
	}
	// Info does a value copy of WalletInfo, so scalar fields are isolated
	// from the package-level value. Slice fields like AvailableWallets are
	// shared by reference; that limitation is intentional, not asserted
	// here.
	info.Name = "Mutated"
	if drv.Info().Name == "Mutated" {
		t.Fatal("Driver.Info should isolate scalar fields from package-level WalletInfo")
	}
}

func TestDriverExists(t *testing.T) {
	drv := &Driver{}
	tmp := t.TempDir()
	if _, err := drv.Exists("not-rpc", tmp, nil, util.Simnet); err == nil {
		t.Fatal("expected error for unknown wallet type")
	}
	exists, err := drv.Exists(walletTypeRPC, tmp, nil, util.Simnet)
	if err != nil {
		t.Fatalf("Exists(rpc, tempdir) error: %v", err)
	}
	if exists {
		t.Fatal("a fresh tempdir should not report an existing wallet")
	}
}

func TestDriverDecodeCoinID(t *testing.T) {
	drv := &Driver{}
	raw := encode.RandomBytes(common.HashLength)
	got, err := drv.DecodeCoinID(raw)
	if err != nil {
		t.Fatalf("DecodeCoinID error: %v", err)
	}
	want := common.BytesToHash(raw).String()
	if got != want {
		t.Fatalf("DecodeCoinID = %q, want %q", got, want)
	}
	// An unsupported length should error, not silently produce a result.
	if _, err := drv.DecodeCoinID(encode.RandomBytes(20)); err == nil {
		t.Fatal("expected error for unsupported coin ID length")
	}
}

func TestPolygonFinalizeConfs(t *testing.T) {
	if got := polygonFinalizeConfs(util.Simnet); got != 2 {
		t.Fatalf("simnet confs = %d, want 2", got)
	}
	for _, net := range []util.Network{util.Mainnet, util.Testnet} {
		if got := polygonFinalizeConfs(net); got != 64 {
			t.Fatalf("%s confs = %d, want 64", net, got)
		}
	}
}

func TestChainConfig(t *testing.T) {
	cases := []struct {
		net    util.Network
		wantID int64
	}{
		{util.Mainnet, 137},
		{util.Testnet, 80002},
		// Simnet builds a developer genesis via ethcore.DeveloperGenesisBlock,
		// which uses params.AllDevChainProtocolChanges (ChainID 1337).
		{util.Simnet, 1337},
	}
	for _, tc := range cases {
		cfg, err := ChainConfig(tc.net)
		if err != nil {
			t.Fatalf("ChainConfig(%s) error: %v", tc.net, err)
		}
		if cfg == nil {
			t.Fatalf("ChainConfig(%s) returned nil config", tc.net)
		}
		if cfg.ChainID == nil || cfg.ChainID.Int64() != tc.wantID {
			t.Fatalf("ChainConfig(%s).ChainID = %v, want %d", tc.net, cfg.ChainID, tc.wantID)
		}
	}
	if _, err := ChainConfig(util.Network(99)); err == nil {
		t.Fatal("expected error for unknown network")
	}
}

func TestNetworkCompatibilityData(t *testing.T) {
	cases := []struct {
		net      util.Network
		wantAddr common.Address
	}{
		{util.Mainnet, common.HexToAddress("0x5973918275C01F50555d44e92c9d9b353CaDAD54")},
		{util.Testnet, common.HexToAddress("0x248528f5A2C3731fb598E8cc1dc5dB5f997E74BC")},
	}
	for _, tc := range cases {
		c, err := NetworkCompatibilityData(tc.net)
		if err != nil {
			t.Fatalf("NetworkCompatibilityData(%s) error: %v", tc.net, err)
		}
		if c.Addr != tc.wantAddr {
			t.Fatalf("NetworkCompatibilityData(%s).Addr = %s, want %s", tc.net, c.Addr.Hex(), tc.wantAddr.Hex())
		}
	}
	if _, err := NetworkCompatibilityData(util.Network(99)); err == nil {
		t.Fatal("expected error for unknown network")
	}
}

// TestTokenRegistration verifies the package init() registered USDC, USDT,
// WETH, and WBTC under the polygon parent asset with the expected wallet
// definition. Per-network address filtering is enforced by asset.SetNetwork
// at runtime, which is destructive of global registry state and so is not
// exercised here.
func TestTokenRegistration(t *testing.T) {
	cases := []struct {
		id   uint32
		name string
	}{
		{usdcTokenID, "USDC"},
		{usdtTokenID, "Tether"},
		{wethTokenID, "Wrapped Ether"},
		{wbtcTokenID, "Wrapped Bitcoin"},
	}
	for _, tc := range cases {
		tok := asset.TokenInfo(tc.id)
		if tok == nil {
			t.Fatalf("TokenInfo(%d) returned nil; %s not registered", tc.id, tc.name)
		}
		if tok.Token == nil {
			t.Fatalf("%s: embedded util.Token is nil", tc.name)
		}
		if tok.NetworkID != BipID {
			t.Fatalf("%s NetworkID = %d, want %d (polygon BipID)", tc.name, tok.NetworkID, BipID)
		}
		if tok.Name != tc.name {
			t.Fatalf("token id %d Name = %q, want %q", tc.id, tok.Name, tc.name)
		}
		if tok.Definition == nil {
			t.Fatalf("%s has nil Definition", tc.name)
		}
		if tok.Definition.Type != walletTypeToken {
			t.Fatalf("%s Definition.Type = %q, want %q", tc.name, tok.Definition.Type, walletTypeToken)
		}
		if tok.Definition.Tab != "Polygon token" {
			t.Fatalf("%s Definition.Tab = %q, want %q", tc.name, tok.Definition.Tab, "Polygon token")
		}
	}
}

func TestChainIDs(t *testing.T) {
	cases := []struct {
		net    util.Network
		wantID int64
	}{
		{util.Mainnet, MainnetChainID},
		{util.Testnet, TestnetChainID},
		{util.Simnet, SimnetChainID},
	}
	for _, tc := range cases {
		got, ok := ChainIDs[tc.net]
		if !ok {
			t.Fatalf("ChainIDs missing entry for %s", tc.net)
		}
		if got != tc.wantID {
			t.Fatalf("ChainIDs[%s] = %d, want %d", tc.net, got, tc.wantID)
		}
	}
	if _, ok := ChainIDs[util.Network(99)]; ok {
		t.Fatal("ChainIDs should not have an entry for an unknown network")
	}

	// Driver.Open reads ChainConfig(net).ChainID and Driver.Create reads
	// ChainIDs[net]. These two paths must agree on the chain ID for each
	// network or a wallet created on one path will talk to a different
	// chain than a wallet opened on the other.
	for _, net := range []util.Network{util.Mainnet, util.Testnet, util.Simnet} {
		cfg, err := ChainConfig(net)
		if err != nil {
			t.Fatalf("ChainConfig(%s) error: %v", net, err)
		}
		if cfg.ChainID == nil {
			t.Fatalf("ChainConfig(%s).ChainID is nil", net)
		}
		if cfg.ChainID.Int64() != ChainIDs[net] {
			t.Fatalf("chain ID disagreement for %s: ChainConfig=%d, ChainIDs=%d",
				net, cfg.ChainID.Int64(), ChainIDs[net])
		}
	}
}

// TestMultiBalanceAddresses verifies the multi-balance contract address is
// configured for mainnet and testnet. Simnet does not deploy a multi-balance
// contract; Driver.Open passes the zero value from the map miss.
func TestMultiBalanceAddresses(t *testing.T) {
	for _, net := range []util.Network{util.Mainnet, util.Testnet} {
		addr, ok := MultiBalanceAddresses[net]
		if !ok {
			t.Fatalf("MultiBalanceAddresses missing %s", net)
		}
		if (addr == common.Address{}) {
			t.Fatalf("MultiBalanceAddresses[%s] is the zero address", net)
		}
	}
}

// TestPolygonAssetRegistered ensures the polygon Driver itself is registered
// at BipID and surfaces the expected base-chain WalletInfo.
func TestPolygonAssetRegistered(t *testing.T) {
	ra := asset.Asset(BipID)
	if ra == nil {
		t.Fatalf("asset.Asset(%d) returned nil; polygon driver not registered", BipID)
	}
	if ra.Info == nil {
		t.Fatal("registered asset has nil Info")
	}
	if ra.Info.Name != "Polygon" {
		t.Fatalf("registered Info.Name = %q, want %q", ra.Info.Name, "Polygon")
	}
	if ra.Info.BlockchainClass != asset.BlockchainClassEVM {
		t.Fatalf("registered Info.BlockchainClass = %q, want EVM", ra.Info.BlockchainClass)
	}
}
