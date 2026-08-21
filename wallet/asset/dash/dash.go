// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dash

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/config"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
	inWallet "github.com/bisoncraft/caribou/wallet/utxowallet/wallet"

	"github.com/btcsuite/btcd/chaincfg"
)

const (
	// BipID is the BIP-0044 asset ID.
	BipID              = 5
	walletTypeElectrum = "electrumRPC"
	walletTypeSPV      = "SPV"
	dbTimeout          = 20 * time.Second
)

var (
	// spvWalletDefinition lives in wallets_desktop.go: it is referenced only by
	// that file's availableWallets, so defining it here would leave it unused
	// under the mobile tag.

	// WalletInfo defines some general information about a Dash wallet.
	// AvailableWallets is ordered electrum-first so it is the default, and is
	// build-tagged to drop SPV entirely on mobile. See wallets_desktop.go and
	// wallets_mobile.go.
	WalletInfo = &asset.WalletInfo{
		Name:             "Dash",
		UnitInfo:         UnitInfo,
		AvailableWallets: availableWallets,
		BlockchainClass:  asset.BlockchainClassUTXO,
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

var _ asset.Driver = (*Driver)(nil)
var _ asset.Creator = (*Driver)(nil)

// Open creates the Dash wallet. Start the wallet with its Connect method.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	return newWallet(cfg, logger, network)
}

// DecodeCoinID creates a human-readable representation of a coin ID for Dash.
func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	// Dash and Bitcoin have the same tx hash and output format.
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
	return btc.MinLotSize(maxFeeRate, false)
}

// Exists checks the existence of the wallet. Part of the Creator interface, so
// only used for the seeded SPV and electrum wallet types (which share one
// on-disk database).
func (d *Driver) Exists(walletType, dataDir string, settings map[string]string, net util.Network) (bool, error) {
	switch walletType {
	case walletTypeSPV, walletTypeElectrum:
	default:
		return false, fmt.Errorf("no Dash wallet of type %q available", walletType)
	}
	chainParams, err := parseChainParams(net)
	if err != nil {
		return false, err
	}
	walletDir := filepath.Join(dataDir, chainParams.Name)
	loader := inWallet.NewLoader(dashTableNetParams(chainParams), walletDir, true, dbTimeout, 250)
	return loader.WalletExists()
}

// Create creates a new seeded Dash wallet. Both walletTypeSPV and
// walletTypeElectrum are supported; they use the same underlying btcwallet
// database and create path, differing only in the chain backend at open time.
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

	recoveryCfg := new(btc.RecoveryCfg)
	if err = config.Unmapify(params.Settings, recoveryCfg); err != nil {
		return err
	}

	bday := btc.DefaultWalletBirthday
	if params.Birthday != 0 {
		bday = time.Unix(int64(params.Birthday), 0)
	}

	walletDir := filepath.Join(params.DataDir, chainParams.Name)
	return createDashElectrumWallet(params.Pass, params.Seed, bday, walletDir,
		params.Logger, recoveryCfg.NumExternalAddresses, recoveryCfg.NumInternalAddresses, chainParams)
}

// parseChainParams returns the clone *chaincfg.Params for the network.
func parseChainParams(net util.Network) (*chaincfg.Params, error) {
	switch net {
	case util.Mainnet:
		return MainNetParams, nil
	case util.Testnet:
		return TestNetParams, nil
	case util.Regtest:
		return RegressionNetParams, nil
	}
	return nil, fmt.Errorf("unknown network ID %v", net)
}

// newWallet constructs a new client wallet for Dash based on the
// WalletDefinition.Type.
func newWallet(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	params, err := parseChainParams(network)
	if err != nil {
		return nil, err
	}

	switch cfg.Type {
	case walletTypeElectrum:
		return openDashElectrumWallet(cfg, params, logger), nil
	case walletTypeSPV:
		// The SPV wallet is the same lean wallet as electrum, driven by the
		// neutrino backend instead of an electrum client.
		return openDashSPVWallet(cfg, params, logger), nil
	default:
		return nil, fmt.Errorf("unknown wallet type %q", cfg.Type)
	}
}
