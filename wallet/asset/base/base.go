// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package base

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/eth"
)

func registerToken(tokenID uint32, desc string, allowedNets ...util.Network) {
	token, found := Tokens[tokenID]
	if !found {
		panic("token " + strconv.Itoa(int(tokenID)) + " not known")
	}
	netAddrs := make(map[util.Network]string)
	for net, netToken := range token.NetTokens {
		if !slices.Contains(allowedNets, net) {
			continue
		}
		netAddrs[net] = netToken.Address.String()
	}
	asset.RegisterToken(tokenID, token.Token, &asset.WalletDefinition{
		Type:        walletTypeToken,
		Tab:         "Base token",
		Description: desc,
	}, netAddrs)
}

func init() {
	MaybeReadSimnetAddrs()
	asset.Register(BipID, &Driver{})
	registerToken(usdcTokenID, "The USDC Base ERC20 token.", util.Mainnet, util.Testnet, util.Simnet)
	registerToken(usdtTokenID, "The USDT Base ERC20 token.", util.Mainnet, util.Testnet, util.Simnet)
}

const (
	// BipID is our custom BIP-0044 asset ID for Base weth.
	BipID              = 8453
	defaultGasFeeLimit = 1000
	walletTypeRPC      = "rpc"
	walletTypeToken    = "token"
)

var (
	// WalletInfo defines some general information about a Base Wallet(EVM
	// Compatible).

	walletOpts = []*asset.ConfigOption{
		{
			Key:         "gasfeelimit",
			DisplayName: "Gas Fee Limit",
			Description: "This is the highest network fee rate you are willing to " +
				"pay on swap transactions. If gasfeelimit is lower than a market's " +
				"maxfeerate, you will not be able to trade on that market with this " +
				"wallet.  Units: gwei / gas",
			DefaultValue: strconv.FormatUint(defaultGasFeeLimit, 10),
		},
	}
	rpcWalletDefinition = &asset.WalletDefinition{
		Type:        walletTypeRPC,
		Tab:         "External",
		Description: "Infrastructure providers (e.g. Infura) or local nodes",
		ConfigOpts:  append(eth.RPCOpts, walletOpts...),
		Seeded:      true,
		NoAuth:      true,
	}

	WalletInfo = asset.WalletInfo{
		Name:             "Base",
		UnitInfo:         UnitInfo,
		AvailableWallets: []*asset.WalletDefinition{rpcWalletDefinition},
		BlockchainClass:  asset.BlockchainClassEVM,
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
			Priority:      true,
		},
	}
)

type Driver struct{}

// Open opens the Base exchange wallet. Start the wallet with its Run method.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, net util.Network) (asset.Wallet, error) {
	chainCfg, err := ChainConfig(net)
	if err != nil {
		return nil, fmt.Errorf("failed to locate Base genesis configuration for network %s", net)
	}
	compat, err := NetworkCompatibilityData(net)
	if err != nil {
		return nil, fmt.Errorf("failed to locate Base compatibility data: %s", net)
	}
	var defaultProviders []string
	switch net {
	case util.Simnet:
		defaultProviders = []string{"http://127.0.0.1:39556"}
	case util.Testnet:
		defaultProviders = []string{
			// Verified working (2026-09-01)
			"https://base-sepolia-rpc.publicnode.com",
			"https://sepolia.base.org",
			"https://base-sepolia.drpc.org",
			"https://84532.rpc.thirdweb.com", // thirdweb
			"https://base-sepolia.gateway.tenderly.co",
		}
	case util.Mainnet:
		defaultProviders = []string{
			// Verified working (2026-09-01)
			"https://base-rpc.publicnode.com", // not for production use
			"https://mainnet.base.org",
			"https://base.drpc.org",
			// 1rpc.io caps eth_getLogs at a 50-block range (below
			// maxScanBlocks), so it's kept for general RPC coverage
			// rather than the transfer scan.
			"https://1rpc.io/base",
			"https://base.api.onfinality.io/public",
		}
	}

	return eth.NewEVMWallet(&eth.EVMWalletConfig{
		BaseChainID:      BipID,
		ChainCfg:         chainCfg,
		AssetCfg:         cfg,
		CompatData:       &compat,
		Tokens:           Tokens,
		FinalizeConfs:    3,
		Logger:           logger,
		WalletInfo:       WalletInfo,
		Net:              net,
		DefaultProviders: defaultProviders,
		MaxTxFeeGwei:     eth.GweiFactor, // 1 ETH
		IsOpStack:        true,
	})
}

func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	return (&eth.Driver{}).DecodeCoinID(coinID)
}

func (d *Driver) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

func (d *Driver) Exists(walletType, dataDir string, settings map[string]string, net util.Network) (bool, error) {
	if walletType != walletTypeRPC {
		return false, fmt.Errorf("unknown wallet type %q", walletType)
	}
	return (&eth.Driver{}).Exists(walletType, dataDir, settings, net)
}

func (d *Driver) Create(cfg *asset.CreateWalletParams) error {
	compat, err := NetworkCompatibilityData(cfg.Net)
	if err != nil {
		return fmt.Errorf("error finding compatibility data: %v", err)
	}
	return eth.CreateEVMWallet(ChainIDs[cfg.Net], cfg, &compat, false)
}
