// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperevm

import (
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/hyperliquid"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestDriverInfo(t *testing.T) {
	drv := &Driver{}
	info := drv.Info()
	if info.Name != "HyperEVM" {
		t.Fatalf("Info.Name = %q, want %q", info.Name, "HyperEVM")
	}
	if info.BlockchainClass != asset.BlockchainClassEVM {
		t.Fatalf("Info.BlockchainClass = %q, want %q", info.BlockchainClass, asset.BlockchainClassEVM)
	}
	if info.UnitInfo.Conventional.ConversionFactor != 1e9 {
		t.Fatalf("ConversionFactor = %v, want 1e9", info.UnitInfo.Conventional.ConversionFactor)
	}
	if info.SeedAssetID != hyperliquid.BipID {
		t.Fatalf("SeedAssetID = %d, want %d (hyperliquid.BipID) so this wallet shares HyperCore's address",
			info.SeedAssetID, hyperliquid.BipID)
	}
	info.Name = "Mutated"
	if drv.Info().Name == "Mutated" {
		t.Fatal("Driver.Info should isolate scalar fields from package-level WalletInfo")
	}
}

func TestDriverExists(t *testing.T) {
	drv := &Driver{}
	tmp := t.TempDir()
	if _, err := drv.Exists("not-rpc", tmp, nil, util.Mainnet); err == nil {
		t.Fatal("expected error for unknown wallet type")
	}
	exists, err := drv.Exists(walletTypeRPC, tmp, nil, util.Mainnet)
	if err != nil {
		t.Fatalf("Exists(rpc, tempdir) error: %v", err)
	}
	if exists {
		t.Fatal("a fresh tempdir should not report an existing wallet")
	}
}

func TestDriverOpen(t *testing.T) {
	drv := &Driver{}
	wallet, err := drv.Open(&asset.WalletConfig{
		Type:     walletTypeRPC,
		Settings: map[string]string{},
		DataDir:  t.TempDir(),
	}, util.Disabled, util.Mainnet)
	if err != nil {
		t.Fatalf("Open(mainnet) error: %v", err)
	}
	if wallet == nil {
		t.Fatal("Open returned nil wallet")
	}
	info := wallet.Info()
	if info == nil {
		t.Fatal("wallet.Info returned nil")
	}
	if info.Name != WalletInfo.Name {
		t.Fatalf("wallet.Info().Name = %q, want %q", info.Name, WalletInfo.Name)
	}
	if _, err := drv.Open(&asset.WalletConfig{
		Type:     walletTypeRPC,
		Settings: map[string]string{},
		DataDir:  t.TempDir(),
	}, util.Disabled, util.Testnet); err == nil {
		t.Fatal("expected error opening on testnet: no verified chain config exists yet")
	}
}

func TestChainConfig(t *testing.T) {
	cfg, err := ChainConfig(util.Mainnet)
	if err != nil {
		t.Fatalf("ChainConfig(mainnet) error: %v", err)
	}
	if cfg.ChainID == nil || cfg.ChainID.Int64() != MainnetChainID {
		t.Fatalf("ChainConfig(mainnet).ChainID = %v, want %d", cfg.ChainID, MainnetChainID)
	}
	if cfg.LondonBlock == nil || cfg.LondonBlock.Sign() != 0 {
		t.Fatalf("ChainConfig(mainnet).LondonBlock = %v, want 0", cfg.LondonBlock)
	}
	for _, net := range []util.Network{util.Testnet, util.Simnet, util.Network(99)} {
		if _, err := ChainConfig(net); err == nil {
			t.Fatalf("expected error for unsupported network %s", net)
		}
	}
}

func TestNetworkCompatibilityData(t *testing.T) {
	c, err := NetworkCompatibilityData(util.Mainnet)
	if err != nil {
		t.Fatalf("NetworkCompatibilityData(mainnet) error: %v", err)
	}
	wantAddr := common.HexToAddress("0x7e9401990267577a30ea452e2c2d4d35bcaa62b9")
	if c.Addr != wantAddr {
		t.Fatalf("NetworkCompatibilityData(mainnet).Addr = %s, want %s", c.Addr.Hex(), wantAddr.Hex())
	}
	if c.TokenAddr != (common.Address{}) {
		t.Fatalf("NetworkCompatibilityData(mainnet).TokenAddr = %s, want empty (no token compat checks)", c.TokenAddr.Hex())
	}
	for _, net := range []util.Network{util.Testnet, util.Simnet, util.Network(99)} {
		if _, err := NetworkCompatibilityData(net); err == nil {
			t.Fatalf("expected error for unsupported network %s", net)
		}
	}
}

func TestChainIDs(t *testing.T) {
	got, ok := ChainIDs[util.Mainnet]
	if !ok {
		t.Fatal("ChainIDs missing entry for mainnet")
	}
	if got != MainnetChainID {
		t.Fatalf("ChainIDs[mainnet] = %d, want %d", got, MainnetChainID)
	}
	if _, ok := ChainIDs[util.Testnet]; ok {
		t.Fatal("ChainIDs should not have a testnet entry yet — its chain ID is unverified")
	}
	cfg, err := ChainConfig(util.Mainnet)
	if err != nil {
		t.Fatalf("ChainConfig(mainnet) error: %v", err)
	}
	if cfg.ChainID.Int64() != ChainIDs[util.Mainnet] {
		t.Fatalf("chain ID disagreement: ChainConfig=%d, ChainIDs=%d", cfg.ChainID.Int64(), ChainIDs[util.Mainnet])
	}
}

// TestSharesHyperCoreAddress is the end-to-end regression test for the whole
// point of this package: a HyperEVM wallet created from the same seed as a
// Hyperliquid (HyperCore) wallet must control the exact same address, or
// funds sent to that address over either chain become unreachable from the
// other. This is the test that can actually fail if someone later "cleans
// up" the SeedAssetID/PrivKeyFromSeed plumbing without realizing it's load
// bearing.
func TestSharesHyperCoreAddress(t *testing.T) {
	seed := encode.RandomBytes(32)
	pass := encode.RandomBytes(32)

	rawKey, zero, err := hyperliquid.PrivKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("hyperliquid.PrivKeyFromSeed: %v", err)
	}
	defer zero()
	priv, err := crypto.ToECDSA(rawKey)
	if err != nil {
		t.Fatalf("ToECDSA: %v", err)
	}
	wantAddr := crypto.PubkeyToAddress(priv.PublicKey)

	tmpDir := t.TempDir()
	if err := (&Driver{}).Create(&asset.CreateWalletParams{
		Type:     walletTypeRPC,
		Seed:     seed,
		Pass:     pass,
		Settings: map[string]string{},
		DataDir:  tmpDir,
		Net:      util.Mainnet,
		Logger:   util.Disabled,
	}); err != nil {
		t.Fatalf("hyperevm Driver.Create: %v", err)
	}

	ks := keystore.NewKeyStore(filepath.Join(tmpDir, util.Mainnet.String(), "keystore"),
		keystore.LightScryptN, keystore.LightScryptP)
	accts := ks.Accounts()
	if len(accts) != 1 {
		t.Fatalf("expected 1 account in hyperevm keystore, got %d", len(accts))
	}
	if accts[0].Address != wantAddr {
		t.Fatalf("hyperevm wallet address %s does not match hyperliquid's %s for the same seed",
			accts[0].Address.Hex(), wantAddr.Hex())
	}
}

func TestWalletInfoRegistration(t *testing.T) {
	reg := asset.Asset(BipID)
	if reg == nil {
		t.Fatalf("asset.Asset(%d) returned nil", BipID)
	}
	if reg.Info == nil {
		t.Fatal("registered asset info is nil")
	}
	if reg.Info.Name != WalletInfo.Name {
		t.Fatalf("registered asset name = %q, want %q", reg.Info.Name, WalletInfo.Name)
	}
	if reg.Info.SeedAssetID != hyperliquid.BipID {
		t.Fatalf("registered asset SeedAssetID = %d, want %d", reg.Info.SeedAssetID, hyperliquid.BipID)
	}
}
