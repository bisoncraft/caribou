// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package ltc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/config"
	"github.com/bisoncraft/bosswallet/util/dexnet"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/btc"
	inWallet "github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet"
	"github.com/btcsuite/btcd/chaincfg"
)

const (
	version = 1
	// BipID is the BIP-0044 asset ID.
	BipID              = 2
	walletTypeSPV      = "SPV"
	walletTypeElectrum = "electrumRPC"
	dbTimeout          = 20 * time.Second
)

var (
	NetPorts = btc.NetPorts{
		Mainnet: "9332",
		Testnet: "19332",
		Simnet:  "19443",
	}
	spvWalletDefinition = &asset.WalletDefinition{
		Type:             walletTypeSPV,
		Tab:              "Native",
		Description:      "Use the built-in SPV wallet",
		ConfigOpts:       btc.CommonConfigOpts("LTC", true),
		Seeded:           true,
		MultiFundingOpts: btc.MultiFundingOpts,
	}
	// WalletInfo defines some general information about a Litecoin wallet.
	WalletInfo = &asset.WalletInfo{
		Name:              "Litecoin",
		SupportedVersions: []uint32{version},
		UnitInfo:          UnitInfo,
		AvailableWallets: []*asset.WalletDefinition{
			spvWalletDefinition,
			ltcElectrumWalletDefinition,
		},
		BlockchainClass: asset.BlockchainClassUTXO,
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
			Priority:      true,
		},
	}
)

func init() {
	asset.Register(BipID, &Driver{})
}

// Driver implements asset.Driver.
type Driver struct{}

// Check that Driver implements asset.Driver.
var _ asset.Driver = (*Driver)(nil)

// Open creates the LTC exchange wallet. Start the wallet with its Run method.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	return NewWallet(cfg, logger, network)
}

// DecodeCoinID creates a human-readable representation of a coin ID for
// Litecoin.
func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	// Litecoin and Bitcoin have the same tx hash and output format.
	return (&btc.Driver{}).DecodeCoinID(coinID)
}

// Info returns basic information about the wallet and asset.
func (d *Driver) Info() *asset.WalletInfo {
	return WalletInfo
}

// MinLotSize calculates the minimum lot size for a given fee rate that avoids
// dust outputs on the swap and refund txs, assuming the maxFeeRate doesn't
// change.
func (d *Driver) MinLotSize(maxFeeRate uint64) uint64 {
	return btc.MinLotSize(maxFeeRate, true)
}

// Exists checks the existence of the wallet. Part of the Creator interface, so
// only used for wallets with WalletDefinition.Seeded = true.
func (d *Driver) Exists(walletType, dataDir string, settings map[string]string, net util.Network) (bool, error) {
	switch walletType {
	case walletTypeSPV, walletTypeElectrum:
	default:
		return false, fmt.Errorf("no Litecoin wallet of type %q available", walletType)
	}

	chainParams, err := parseChainParams(net)
	if err != nil {
		return false, err
	}
	walletDir := filepath.Join(dataDir, chainParams.Name)
	loader := inWallet.NewLoader(ltcTableNetParams(chainParams), walletDir, true, dbTimeout, 250)
	return loader.WalletExists()
}

// Create creates a new seeded wallet. Both walletTypeSPV and
// walletTypeElectrum are supported; they use the same underlying btcwallet
// database format.
func (d *Driver) Create(params *asset.CreateWalletParams) error {
	switch params.Type {
	case walletTypeSPV, walletTypeElectrum:
	default:
		return fmt.Errorf("unsupported seeded wallet type %q", params.Type)
	}
	if len(params.Seed) == 0 {
		return errors.New("wallet seed cannot be empty")
	}
	if len(params.DataDir) == 0 {
		return errors.New("must specify wallet data directory")
	}
	chainParams, err := parseChainParams(params.Net)
	if err != nil {
		return fmt.Errorf("error parsing chain: %w", err)
	}

	walletCfg := new(btc.WalletConfig)
	err = config.Unmapify(params.Settings, walletCfg)
	if err != nil {
		return err
	}

	recoveryCfg := new(btc.RecoveryCfg)
	err = config.Unmapify(params.Settings, recoveryCfg)
	if err != nil {
		return err
	}

	bday := btc.DefaultWalletBirthday
	if params.Birthday != 0 {
		bday = time.Unix(int64(params.Birthday), 0)
	}

	walletDir := filepath.Join(params.DataDir, chainParams.Name)
	// SPV and electrum share the same lean wallet; the same create path serves
	// both wallet types.
	return createLTCElectrumWallet(params.Pass, params.Seed, bday, walletDir,
		params.Logger, recoveryCfg.NumExternalAddresses, recoveryCfg.NumInternalAddresses, chainParams)
}

// NewWallet is the exported constructor by which the DEX will import the
// exchange wallet.
func NewWallet(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	var cloneParams *chaincfg.Params
	switch network {
	case util.Mainnet:
		cloneParams = MainNetParams
	case util.Testnet:
		cloneParams = TestNet4Params
	case util.Regtest:
		cloneParams = RegressionNetParams
	default:
		return nil, fmt.Errorf("unknown network ID %v", network)
	}

	switch cfg.Type {
	case walletTypeSPV:
		// The SPV wallet is the same lean wallet as electrum, driven by the
		// neutrino backend instead of an electrum client.
		return openLTCSPVWallet(cfg, cloneParams, logger), nil
	case walletTypeElectrum:
		return openLTCElectrumWallet(cfg, cloneParams, logger), nil
	default:
		return nil, fmt.Errorf("unknown wallet type %q", cfg.Type)
	}
}

func parseChainParams(net util.Network) (*chaincfg.Params, error) {
	switch net {
	case util.Mainnet:
		return MainNetParams, nil
	case util.Testnet:
		return TestNet4Params, nil
	case util.Regtest:
		return RegressionNetParams, nil
	}
	return nil, fmt.Errorf("unknown network ID %v", net)
}

func externalFeeRate(ctx context.Context, net util.Network) (uint64, error) {
	switch net {
	case util.Mainnet, util.Simnet:
		return fetchBlockCypherFees(ctx)
	}
	return bitcoreFeeRate(ctx, net)
}

var bitcoreFeeRate = btc.BitcoreRateFetcher("LTC")

func fetchBlockCypherFees(ctx context.Context) (uint64, error) {
	// Posted rate limits for blockcypher are 3/sec, 100/hr, 1000/day. 1000/day
	// is once per 86.4 seconds, but I've seen unexpected metering before.
	// Once per 5 minutes is a good rate, and that's the default.
	// Can't find a source for testnet. Just using mainnet rate for everything.
	const uri = "https://api.blockcypher.com/v1/ltc/main"
	var res struct {
		Low    float64 `json:"low_fee_per_kb"`
		Medium float64 `json:"medium_fee_per_kb"`
		High   float64 `json:"high_fee_per_kb"`
	}
	if err := dexnet.Get(ctx, uri, &res); err != nil {
		return 0, err
	}
	if res.Low == 0 {
		return 0, errors.New("no fee rate in result")
	}
	return uint64(math.Round(res.Low / 1000)), nil
}
