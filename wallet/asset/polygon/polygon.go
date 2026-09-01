// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package polygon

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/eth"
)

func init() {
	MaybeReadSimnetAddrs()
}

func registerToken(tokenID uint32, desc string, allowedNets ...util.Network) {
	token, found := Tokens[tokenID]
	if !found {
		panic("token " + strconv.Itoa(int(tokenID)) + " not known")
	}
	netAddrs := make(map[util.Network]string)
	netVersions := make(map[util.Network][]uint32, 3)
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
		Tab:         "Polygon token",
		Description: desc,
	}, netAddrs, netVersions)
}

func init() {
	asset.Register(BipID, &Driver{})
	eth.RegisterEVMTokens(Tokens)
	registerToken(usdcTokenID, "The USDC Ethereum ERC20 token.", util.Mainnet, util.Testnet, util.Simnet)
	registerToken(usdtTokenID, "The USDT Ethereum ERC20 token.", util.Mainnet, util.Testnet, util.Simnet)
	registerToken(wbtcTokenID, "Wrapped BTC.", util.Mainnet)
	registerToken(wethTokenID, "Wrapped ETH.", util.Mainnet, util.Testnet)
}

const (
	// BipID is the BIP-0044 asset ID for Polygon.
	BipID              = 966
	defaultGasFeeLimit = 2500
	walletTypeRPC      = "rpc"
	walletTypeToken    = "token"
)

var (
	// WalletInfo defines some general information about a Polygon Wallet(EVM
	// Compatible).

	walletOpts = []*asset.ConfigOption{
		eth.GasFeeConfigOption(defaultGasFeeLimit),
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
		Name:              "Polygon",
		SupportedVersions: []uint32{0, 1},
		UnitInfo:          UnitInfo,
		AvailableWallets:  []*asset.WalletDefinition{rpcWalletDefinition},
		BlockchainClass:   asset.BlockchainClassEVM,
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
			Priority:      true,
		},
	}
)

func polygonFinalizeConfs(net util.Network) uint64 {
	if net == util.Simnet {
		return 2
	}
	return 64
}

type Driver struct{}

// Open opens the Polygon exchange wallet. Start the wallet with its Run method.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, net util.Network) (asset.Wallet, error) {
	chainCfg, err := ChainConfig(net)
	if err != nil {
		return nil, fmt.Errorf("failed to locate Polygon genesis configuration for network %s", net)
	}
	compat, err := NetworkCompatibilityData(net)
	if err != nil {
		return nil, fmt.Errorf("failed to locate Polygon compatibility data: %s", net)
	}
	var defaultProviders []string
	switch net {
	case util.Simnet:
		defaultProviders = []string{"http://127.0.0.1:48296"}
	case util.Testnet:
		defaultProviders = []string{
			// Verified working (2026-09-01)
			"https://polygon-amoy.drpc.org",                   // dRPC - verified working
			"https://gateway.tenderly.co/public/polygon-amoy", // Tenderly
			"https://polygon-amoy-bor-rpc.publicnode.com",     // PublicNode
			"wss://polygon-amoy-bor-rpc.publicnode.com",       // PublicNode WSS
		}
	case util.Mainnet:
		defaultProviders = []string{
			// Verified working (2026-09-01)
			"https://1rpc.io/matic",                      // 1RPC
			"https://rpc-mainnet.matic.quiknode.pro",     // QuikNode
			"https://gateway.tenderly.co/public/polygon", // Tenderly
			"https://polygon-bor-rpc.publicnode.com",     // PublicNode
		}
	}

	// BipID, chainCfg, cfg, &t, VersionedGases, Tokens, logger, net
	evmWallet, err := eth.NewEVMWallet(&eth.EVMWalletConfig{
		BaseChainID:      BipID,
		ChainCfg:         chainCfg,
		AssetCfg:         cfg,
		CompatData:       &compat,
		VersionedGases:   VersionedGases,
		Tokens:           Tokens,
		FinalizeConfs:    polygonFinalizeConfs(net),
		Logger:           logger,
		MultiBalAddress:  MultiBalanceAddresses[net],
		WalletInfo:       WalletInfo,
		Net:              net,
		DefaultProviders: defaultProviders,
		MaxTxFeeGwei:     1000 * eth.GweiFactor, // 1000 POL
	})
	if err != nil {
		return nil, err
	}

	return evmWallet, nil
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
