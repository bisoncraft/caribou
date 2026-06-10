// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package bnb

import (
	"math/big"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestDriverInfo(t *testing.T) {
	drv := &Driver{}
	info := drv.Info()
	if info.Name != "BNB Smart Chain" {
		t.Fatalf("Info.Name = %q, want %q", info.Name, "BNB Smart Chain")
	}
	if info.BlockchainClass != asset.BlockchainClassEVM {
		t.Fatalf("Info.BlockchainClass = %q, want %q", info.BlockchainClass, asset.BlockchainClassEVM)
	}
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
	if _, err := drv.DecodeCoinID(encode.RandomBytes(20)); err == nil {
		t.Fatal("expected error for unsupported coin ID length")
	}
}

func TestChainConfig(t *testing.T) {
	cases := []struct {
		net    util.Network
		wantID int64
	}{
		{util.Mainnet, MainnetChainID},
		{util.Testnet, TestnetChainID},
		{util.Simnet, SimnetChainID},
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
		if cfg.LondonBlock == nil || cfg.LondonBlock.Sign() != 0 {
			t.Fatalf("ChainConfig(%s).LondonBlock = %v, want 0", tc.net, cfg.LondonBlock)
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
		{util.Mainnet, common.HexToAddress("0x50e7cd491fa11d54b73913b58459cad5fd66a40a")},
		{util.Testnet, common.HexToAddress("0x020ea48f8f18d5c56c41d105921e3aafd98f99b3")},
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

func TestSimnetDataDir(t *testing.T) {
	dir, err := simnetDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dir, filepath.Join("dextest", "bnb")) {
		t.Fatalf("simnetDataDir = %q, want path ending in dextest/bnb", dir)
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
	if reg.Tokens != nil {
		t.Fatal("native BNB asset should not register tokens in the initial implementation")
	}
}

func TestLatestSignerRecoversDynamicFeeSender(t *testing.T) {
	privKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey error: %v", err)
	}
	wantAddr := crypto.PubkeyToAddress(privKey.PublicKey)

	for _, net := range []util.Network{util.Mainnet, util.Testnet, util.Simnet} {
		cfg, err := ChainConfig(net)
		if err != nil {
			t.Fatalf("ChainConfig(%s) error: %v", net, err)
		}

		chainID := big.NewInt(ChainIDs[net])
		tx := types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     1,
			GasTipCap: big.NewInt(1),
			GasFeeCap: big.NewInt(2),
			Gas:       21_000,
			To:        &common.Address{},
			Value:     big.NewInt(3),
		})
		signedTx, err := types.SignTx(tx, types.NewLondonSigner(chainID), privKey)
		if err != nil {
			t.Fatalf("SignTx(%s) error: %v", net, err)
		}

		gotAddr, err := types.LatestSigner(cfg).Sender(signedTx)
		if err != nil {
			t.Fatalf("Sender(%s) error: %v", net, err)
		}
		if gotAddr != wantAddr {
			t.Fatalf("Sender(%s) = %s, want %s", net, gotAddr.Hex(), wantAddr.Hex())
		}
	}
}
