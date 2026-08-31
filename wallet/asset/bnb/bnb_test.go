// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package bnb

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	dexeth "github.com/bisoncraft/caribou/wallet/asset/eth"
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

func TestDriverOpen(t *testing.T) {
	drv := &Driver{}
	wallet, err := drv.Open(&asset.WalletConfig{
		Type:     walletTypeRPC,
		Settings: map[string]string{},
		DataDir:  t.TempDir(),
	}, util.Disabled, util.Testnet)
	if err != nil {
		t.Fatalf("Open(testnet) error: %v", err)
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
	origHomeDir := userHomeDir
	t.Cleanup(func() {
		userHomeDir = origHomeDir
	})

	userHomeDir = func() (string, error) {
		return "/tmp/test-home", nil
	}

	dir, err := simnetDataDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/tmp/test-home", "dextest", "bnb")
	if dir != want {
		t.Fatalf("simnetDataDir = %q, want %q", dir, want)
	}
}

func TestSimnetDataDirError(t *testing.T) {
	origHomeDir := userHomeDir
	t.Cleanup(func() {
		userHomeDir = origHomeDir
	})

	userHomeDir = func() (string, error) {
		return "", errors.New("boom")
	}

	if _, err := simnetDataDir(); err == nil {
		t.Fatal("expected error from userHomeDir")
	}
}

func TestNetworkCompatibilityDataSimnet(t *testing.T) {
	origHomeDir := userHomeDir
	origTokenAddr := simnetUSDC.Address
	t.Cleanup(func() {
		userHomeDir = origHomeDir
		simnetUSDC.Address = origTokenAddr
	})

	tmp := t.TempDir()
	userHomeDir = func() (string, error) {
		return tmp, nil
	}

	simnetDir := filepath.Join(tmp, "dextest", "bnb")
	if err := os.MkdirAll(simnetDir, 0o755); err != nil {
		t.Fatalf("MkdirAll error: %v", err)
	}
	writeFile := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(simnetDir, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error: %v", name, err)
		}
	}
	writeFile("test_tx_hash.txt", "0x2222222222222222222222222222222222222222222222222222222222222222\n")
	writeFile("test_block1_hash.txt", "0x3333333333333333333333333333333333333333333333333333333333333333\n")

	t.Run("in-memory token addr", func(t *testing.T) {
		simnetUSDC.Address = common.HexToAddress("0x1111111111111111111111111111111111111111")
		compat, err := NetworkCompatibilityData(util.Simnet)
		if err != nil {
			t.Fatalf("NetworkCompatibilityData(simnet) error: %v", err)
		}
		if compat.Addr != common.HexToAddress("0x18d65fb8d60c1199bb1ad381be47aa692b482605") {
			t.Fatalf("simnet Addr = %s", compat.Addr)
		}
		if compat.TokenAddr != simnetUSDC.Address {
			t.Fatalf("simnet TokenAddr = %s, want %s", compat.TokenAddr, simnetUSDC.Address)
		}
		if compat.TxHash != common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222") {
			t.Fatalf("simnet TxHash = %s", compat.TxHash)
		}
		if compat.BlockHash != common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333") {
			t.Fatalf("simnet BlockHash = %s", compat.BlockHash)
		}
	})

	t.Run("token addr from file", func(t *testing.T) {
		simnetUSDC.Address = common.Address{}
		fileAddr := "0x4444444444444444444444444444444444444444"
		writeFile("test_usdc_contract_address.txt", fileAddr+"\n")
		compat, err := NetworkCompatibilityData(util.Simnet)
		if err != nil {
			t.Fatalf("NetworkCompatibilityData(simnet) error: %v", err)
		}
		if compat.TokenAddr != common.HexToAddress(fileAddr) {
			t.Fatalf("simnet TokenAddr = %s, want %s", compat.TokenAddr.Hex(), fileAddr)
		}
	})
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
	if reg.Tokens == nil {
		t.Fatal("expected native BNB asset to register USDC/USDT tokens")
	}
	if _, ok := reg.Tokens[usdcTokenID]; !ok {
		t.Fatal("expected usdc.bnb to be registered")
	}
	if _, ok := reg.Tokens[usdtTokenID]; !ok {
		t.Fatal("expected usdt.bnb to be registered")
	}
}

// TestTokenRegistration verifies the package init() registered USDC and USDT
// under the BNB parent asset with the expected wallet definition. Per-network
// address filtering is enforced by asset.SetNetwork at runtime, which is
// destructive of global registry state and so is not exercised here.
func TestTokenRegistration(t *testing.T) {
	cases := []struct {
		id   uint32
		name string
	}{
		{usdcTokenID, "USDC"},
		{usdtTokenID, "Tether"},
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
			t.Fatalf("%s NetworkID = %d, want %d (bnb BipID)", tc.name, tok.NetworkID, BipID)
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
		if tok.Definition.Tab != "BNB token" {
			t.Fatalf("%s Definition.Tab = %q, want %q", tc.name, tok.Definition.Tab, "BNB token")
		}
	}
}

// TestTokenEVMFactor guards against Binance-Peg USDC/USDT being registered
// with Ethereum's 6-decimal EVMFactor: both use 18 on-chain decimals, unlike
// their Ethereum counterparts.
func TestTokenEVMFactor(t *testing.T) {
	for _, tok := range []*dexeth.Token{TokenUSDC, TokenUSDT} {
		if tok.EVMFactor == nil || *tok.EVMFactor != 12 {
			t.Fatalf("%s EVMFactor = %v, want 12 (6 conventional + 12 = 18 on-chain decimals)", tok.Name, tok.EVMFactor)
		}
		oneToken := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // 1 token at 18 decimals
		if atomic := tok.EVMToAtomic(oneToken); atomic != 1_000_000 {
			t.Fatalf("%s EVMToAtomic(1e18) = %d, want 1_000_000 (1 %s)", tok.Name, atomic, tok.Name)
		}
	}
}

func TestDriverCreate(t *testing.T) {
	drv := &Driver{}
	tmp := t.TempDir()
	err := drv.Create(&asset.CreateWalletParams{
		Type:     walletTypeRPC,
		Seed:     encode.RandomBytes(32),
		Pass:     encode.RandomBytes(32),
		Settings: map[string]string{},
		DataDir:  tmp,
		Net:      util.Testnet,
	})
	if err != nil {
		t.Fatalf("Create error: %v", err)
	}
	exists, err := drv.Exists(walletTypeRPC, tmp, nil, util.Testnet)
	if err != nil {
		t.Fatalf("Exists error: %v", err)
	}
	if !exists {
		t.Fatal("wallet should exist after Create")
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

// TestTokensRegistryConsistency asserts structural invariants of the static
// Tokens map. OpenTokenWallet refuses to open a token wallet whose NetToken
// has no GasEstimate, so an omission here is a wallet-open regression.
func TestTokensRegistryConsistency(t *testing.T) {
	t.Parallel()
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
			if g := nt.GasEstimate; g == nil || g.Approve == 0 || g.Transfer == 0 {
				t.Errorf("%s/%s: missing gas estimate", tok.Name, net)
			}
			// Simnet token addresses are filled in at runtime from the harness;
			// mainnet and testnet must be present in the static map.
			if net != util.Simnet && (nt.Address == common.Address{}) {
				t.Errorf("%s/%s: zero token address", tok.Name, net)
			}
		}
	}
}
