// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperevm

import (
	"fmt"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/eth"
	"github.com/bisoncraft/caribou/wallet/asset/hyperliquid"
)

func init() {
	asset.Register(BipID, &Driver{})
}

const (
	// BipID is the BIP-0044 asset ID for HyperEVM.
	BipID              = 10002
	defaultGasFeeLimit = 200
	walletTypeRPC      = "rpc"
)

var (
	walletOpts = []*asset.ConfigOption{
		eth.GasFeeConfigOption(defaultGasFeeLimit),
	}
	rpcWalletDefinition = &asset.WalletDefinition{
		Type:        walletTypeRPC,
		Tab:         "RPC",
		Description: "Infrastructure providers (e.g. HyperEVM RPC providers) or local nodes",
		ConfigOpts:  append(eth.RPCOpts, walletOpts...),
		Seeded:      true,
		NoAuth:      true,
	}

	// WalletInfo declares SeedAssetID: hyperliquid.BipID, so this wallet
	// derives the exact same address as the Hyperliquid (HyperCore) wallet —
	// the two chains share one secp256k1 account, and funds sent to that
	// address over either chain must stay reachable by this wallet.
	WalletInfo = asset.WalletInfo{
		Name:             "HyperEVM",
		UnitInfo:         UnitInfo,
		AvailableWallets: []*asset.WalletDefinition{rpcWalletDefinition},
		BlockchainClass:  asset.BlockchainClassEVM,
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
		},
		SeedAssetID: hyperliquid.BipID,
	}
)

type Driver struct{}

// Open opens the HyperEVM exchange wallet. Start the wallet with its Run
// method.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, net util.Network) (asset.Wallet, error) {
	chainCfg, err := ChainConfig(net)
	if err != nil {
		return nil, fmt.Errorf("failed to locate HyperEVM configuration for network %s: %w", net, err)
	}
	compat, err := NetworkCompatibilityData(net)
	if err != nil {
		return nil, fmt.Errorf("failed to locate HyperEVM compatibility data for network %s: %w", net, err)
	}
	var defaultProviders []string
	switch net {
	case util.Mainnet:
		defaultProviders = []string{
			// Verified working (2026-09-01)
			"https://rpc.hyperliquid.xyz/evm", // Hyperliquid Labs official
			"https://hyperliquid.drpc.org",    // dRPC
		}
	case util.Testnet:
		defaultProviders = []string{
			// Verified working (2026-09-03)
			"https://rpc.hyperliquid-testnet.xyz/evm",  // Hyperliquid Labs official
			"https://rpcs.chain.link/hyperevm/testnet", // Chainlink
		}
	}

	return eth.NewEVMWallet(&eth.EVMWalletConfig{
		BaseChainID:      BipID,
		ChainCfg:         chainCfg,
		AssetCfg:         cfg,
		CompatData:       &compat,
		FinalizeConfs:    3,
		Logger:           logger,
		WalletInfo:       WalletInfo,
		Net:              net,
		DefaultProviders: defaultProviders,
		MaxTxFeeGwei:     eth.GweiFactor, // 1 HYPE
		PrivKeyFromSeed:  hyperliquid.PrivKeyFromSeed,
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
	return eth.CreateEVMWalletKeyed(ChainIDs[cfg.Net], cfg, &compat, false, hyperliquid.PrivKeyFromSeed)
}
