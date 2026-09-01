// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package bnb

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
	netVersions := make(map[util.Network][]uint32, len(allowedNets))
	for net, netToken := range token.NetTokens {
		if !slices.Contains(allowedNets, net) {
			continue
		}
		netAddrs[net] = netToken.Address.String()
		netVersions[net] = make([]uint32, 0, 1)
		for ver := range netToken.Gas {
			netVersions[net] = append(netVersions[net], ver)
		}
	}
	asset.RegisterToken(tokenID, token.Token, &asset.WalletDefinition{
		Type:        walletTypeToken,
		Tab:         "BNB token",
		Description: desc,
	}, netAddrs, netVersions)
}

func init() {
	MaybeReadSimnetAddrs()
	asset.Register(BipID, &Driver{})
	eth.RegisterEVMTokens(Tokens)
	registerToken(usdcTokenID, "The USDC BEP-20 token.", util.Mainnet, util.Simnet)
	registerToken(usdtTokenID, "The USDT BEP-20 token.", util.Mainnet, util.Simnet)
}

const (
	// BipID is the BIP-0044 asset ID for BNB Smart Chain.
	BipID              = 714
	defaultGasFeeLimit = 200
	walletTypeRPC      = "rpc"
	walletTypeToken    = "token"
)

var (
	walletOpts = []*asset.ConfigOption{
		eth.GasFeeConfigOption(defaultGasFeeLimit),
	}
	rpcWalletDefinition = &asset.WalletDefinition{
		Type:        walletTypeRPC,
		Tab:         "RPC",
		Description: "Infrastructure providers (e.g. BNB Chain RPC providers) or local nodes",
		ConfigOpts:  append(eth.RPCOpts, walletOpts...),
		Seeded:      true,
		NoAuth:      true,
	}

	WalletInfo = asset.WalletInfo{
		Name:              "BNB Smart Chain",
		SupportedVersions: []uint32{1},
		UnitInfo:          UnitInfo,
		AvailableWallets:  []*asset.WalletDefinition{rpcWalletDefinition},
		BlockchainClass:   asset.BlockchainClassEVM,
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
		},
	}
)

type Driver struct{}

// Open opens the BNB Smart Chain exchange wallet. Start the wallet with its Run method.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, net util.Network) (asset.Wallet, error) {
	chainCfg, err := ChainConfig(net)
	if err != nil {
		return nil, fmt.Errorf("failed to locate BNB Smart Chain configuration for network %s: %w", net, err)
	}
	compat, err := NetworkCompatibilityData(net)
	if err != nil {
		return nil, fmt.Errorf("failed to locate BNB Smart Chain compatibility data for network %s: %w", net, err)
	}
	var defaultProviders []string
	switch net {
	case util.Simnet:
		defaultProviders = []string{"ws://127.0.0.1:48557"}
	case util.Testnet:
		defaultProviders = []string{
			"https://bsc-testnet-dataseed.bnbchain.org",
			"https://bsc-testnet.bnbchain.org",
			"https://bsc-prebsc-dataseed.bnbchain.org",
			// Verified working (2026-09-01), including eth_getLogs over a
			// full maxScanBlocks (128-block) range (address-scoped, same as
			// the mainnet PublicNode entry below).
			"https://bsc-testnet.publicnode.com",
		}
	case util.Mainnet:
		defaultProviders = []string{
			// Verified working (2026-08-22). Neither of these serves
			// eth_getLogs at all (confirmed live: -32005 "limit exceeded" on
			// every query shape, down to a single block/address/no topics),
			// so they're kept for every other RPC method but can't carry the
			// incoming-transfer scan's ERC20 log-filter calls on their own.
			"https://bsc-dataseed.bnbchain.org",
			"https://bsc-dataseed-public.bnbchain.org",
			// Verified working (2026-09-01). Caps eth_getLogs at a 50-block
			// range (below maxScanBlocks), so like the two dataseed entries
			// above it's kept for general RPC coverage rather than the
			// transfer scan.
			"https://1rpc.io/bnb",
			// Verified working (2026-08-22), including eth_getLogs over a
			// full maxScanBlocks (128-block) range. multiRPCClient fans a
			// call out across every configured provider until one succeeds,
			// so this is what actually carries ERC20 detection on BNB.
			"https://bsc-rpc.publicnode.com",
		}
	}

	return eth.NewEVMWallet(&eth.EVMWalletConfig{
		BaseChainID:      BipID,
		ChainCfg:         chainCfg,
		AssetCfg:         cfg,
		CompatData:       &compat,
		VersionedGases:   VersionedGases,
		Tokens:           Tokens,
		FinalizeConfs:    3,
		Logger:           logger,
		MultiBalAddress:  MultiBalanceAddresses[net],
		WalletInfo:       WalletInfo,
		Net:              net,
		DefaultProviders: defaultProviders,
		MaxTxFeeGwei:     eth.GweiFactor, // 1 BNB
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
