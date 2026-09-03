package eth_test

import (
	"strconv"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	dexbase "github.com/bisoncraft/caribou/wallet/asset/base"
	"github.com/bisoncraft/caribou/wallet/asset/eth"
	dexpolygon "github.com/bisoncraft/caribou/wallet/asset/polygon"
	"github.com/ethereum/go-ethereum/common"
)

func regToken(tokens map[uint32]*eth.Token, tokenID uint32, desc string) {
	token, found := tokens[tokenID]
	if !found {
		panic("token " + strconv.Itoa(int(tokenID)) + " not known")
	}
	netAddrs := make(map[util.Network]string)
	for net, netToken := range token.NetTokens {
		netAddrs[net] = netToken.Address.String()
	}
	asset.RegisterToken(tokenID, token.Token, &asset.WalletDefinition{
		Type:        eth.WalletTypeToken,
		Tab:         "Polygon token",
		Description: desc,
	}, netAddrs, true)
}

func registerTokens() {
	asset.Register(966, &eth.Driver{}, true)
	regToken(dexpolygon.Tokens, 966001, "The USDC Ethereum ERC20 token.")
	regToken(dexpolygon.Tokens, 966004, "The USDT Ethereum ERC20 token.")
	asset.Register(8453, &eth.Driver{}, true)
	regToken(dexbase.Tokens, 61000, "The USDC Base ERC20 token.")
}

func TestAcrossAssetToID(t *testing.T) {
	// asset.SetNetwork mutates the global tokens map without holding
	// driversMtx, so this test is not parallelized.
	registerTokens()
	asset.SetNetwork(util.Mainnet)

	expectedMappings := map[uint32]*eth.AcrossAssetMapping{
		60: {
			ChainID: 1,
			Symbol:  "WETH",
			Address: common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		},
		60001: {
			ChainID: 1,
			Symbol:  "USDC",
			Address: common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
		},
		60002: {
			ChainID: 1,
			Symbol:  "USDT",
			Address: common.HexToAddress("0xdac17f958d2ee523a2206206994597c13d831ec7"),
		},
		966001: {
			ChainID: 137,
			Symbol:  "USDC",
			Address: common.HexToAddress("0x3c499c542cef5e3811e1192ce70d8cc03d5c3359"),
		},
		966004: {
			ChainID: 137,
			Symbol:  "USDT",
			Address: common.HexToAddress("0xc2132D05D31c914a87C6611C10748AEb04B58e8F"),
		},
		8453: {
			ChainID: 8453,
			Symbol:  "WETH",
			Address: common.HexToAddress("0x4200000000000000000000000000000000000006"),
		},
		61000: {
			ChainID: 8453,
			Symbol:  "USDC",
			Address: common.HexToAddress("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"),
		},
	}

	for assetID, expectedAcross := range expectedMappings {
		got := eth.AssetIDToAcrossAsset(util.Mainnet, assetID)
		if got == nil {
			t.Errorf("Expected AssetIDToAcrossAsset to return non-nil for asset %d", assetID)
			continue
		}
		if got.ChainID != expectedAcross.ChainID {
			t.Errorf("Asset %d: Expected chainID %d, got %d", assetID, expectedAcross.ChainID, got.ChainID)
		}
		if got.Symbol != expectedAcross.Symbol {
			t.Errorf("Asset %d: Expected symbol %s, got %s", assetID, expectedAcross.Symbol, got.Symbol)
		}
		if got.Address != expectedAcross.Address {
			t.Errorf("Asset %d: Expected address %s, got %s", assetID, expectedAcross.Address.Hex(), got.Address.Hex())
		}
	}

	for expectedAssetID, across := range expectedMappings {
		assetID, ok := eth.AcrossAssetToAssetID(util.Mainnet, across.Symbol, across.ChainID)
		if !ok {
			t.Errorf("Expected AcrossAssetToAssetID to succeed for %+v", across)
			continue
		}
		if assetID != expectedAssetID {
			t.Errorf("Expected asset ID %d, got %d", expectedAssetID, assetID)
		}
	}

	for assetID := range expectedMappings {
		across := eth.AssetIDToAcrossAsset(util.Mainnet, assetID)
		if across == nil {
			t.Errorf("Forward conversion failed for asset ID %d", assetID)
			continue
		}
		convertedAssetID, ok := eth.AcrossAssetToAssetID(util.Mainnet, across.Symbol, across.ChainID)
		if !ok {
			t.Errorf("Reverse conversion failed for across asset %+v", across)
			continue
		}
		if convertedAssetID != assetID {
			t.Errorf("Round-trip failed: expected asset ID %d, got %d", assetID, convertedAssetID)
		}
	}
}
