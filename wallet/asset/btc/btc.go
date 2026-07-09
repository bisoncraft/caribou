// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/config"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

const (
	version = 0

	// BipID is the BIP-0044 asset ID.
	BipID = 0

	walletTypeSPV      = "SPV"
	walletTypeElectrum = "electrumRPC"

	// confTxFinality is the number of confirmations after which a wallet
	// transaction is treated as final and no longer monitored.
	confTxFinality = 1
)

var (
	conventionalConversionFactor = float64(UnitInfo.Conventional.ConversionFactor)

	// DefaultWalletBirthday re-exports the utxowallet fallback birthday
	// (pinned near launch) for the asset packages; ltc and bch reference it
	// through this package.
	DefaultWalletBirthday = wallet.DefaultWalletBirthday

	spvWalletDefinition = &asset.WalletDefinition{
		Type:        walletTypeSPV,
		Tab:         "Native",
		Description: "Use the built-in SPV wallet",
		Seeded:      true,
	}

	// WalletInfo defines some general information about a Bitcoin wallet.
	WalletInfo = &asset.WalletInfo{
		Name:              "Bitcoin",
		SupportedVersions: []uint32{version},
		UnitInfo:          UnitInfo,
		AvailableWallets: []*asset.WalletDefinition{
			spvWalletDefinition,
			electrumWalletDefinition,
		},
		BlockchainClass: asset.BlockchainClassUTXO,
		// Historical extremes are in the several-hundred sat/vB range, so
		// 5,000 never binds on a real market but catches units mixups.
		MaxFeeRate: 5_000,
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
			Priority:      true,
		},
	}
)

// PaymentScripter can be implemented to make non-standard payment scripts.
type PaymentScripter interface {
	PaymentScript() ([]byte, error)
}

// Driver implements asset.Driver.
type Driver struct{}

// Check that Driver implements Driver and Creator.
var _ asset.Driver = (*Driver)(nil)
var _ asset.Creator = (*Driver)(nil)

// Exists checks the existence of the wallet. Part of the Creator interface, so
// only used for wallets with WalletDefinition.Seeded = true.
func (d *Driver) Exists(walletType, dataDir string, settings map[string]string, net util.Network) (bool, error) {
	switch walletType {
	case walletTypeSPV, walletTypeElectrum:
	default:
		return false, fmt.Errorf("no Bitcoin wallet of type %q available", walletType)
	}

	chainParams, err := parseChainParams(net)
	if err != nil {
		return false, err
	}

	dir := filepath.Join(dataDir, chainParams.Name)
	return walletExists(dir, chainParams)
}

// walletExists checks the existence of the wallet.
func walletExists(dir string, chainParams *chaincfg.Params) (bool, error) {
	// timeout and recoverWindow arguments borrowed from btcwallet directly.
	loader := wallet.NewLoader(btcTableNetParams(chainParams), dir, true, dbTimeout, 250)
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

	cfg := new(RecoveryCfg)
	err = config.Unmapify(params.Settings, cfg)
	if err != nil {
		return err
	}

	bday := DefaultWalletBirthday
	if params.Birthday != 0 {
		bday = time.Unix(int64(params.Birthday), 0)
	}

	dir := filepath.Join(params.DataDir, chainParams.Name)
	// SPV and electrum share the same lean wallet (an in-tree btcwallet); the
	// same create path serves both wallet types.
	return createElectrumWallet(params.Pass, params.Seed, bday, dir,
		params.Logger, cfg.NumExternalAddresses, cfg.NumInternalAddresses, chainParams)
}

// Open opens or connects to the BTC wallet. Start the wallet with its Connect
// method.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	return NewWallet(cfg, logger, network)
}

// DecodeCoinID creates a human-readable representation of a coin ID for
// Bitcoin.
func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	txid, vout, err := decodeCoinID(coinID)
	if err != nil {
		return "<invalid>", err
	}
	return fmt.Sprintf("%v:%d", txid, vout), nil
}

// Info returns basic information about the wallet and asset.
func (d *Driver) Info() *asset.WalletInfo {
	return WalletInfo
}

// MinLotSize calculates the minimum lot size for a given fee rate that avoids
// dust outputs on the swap and refund txs, assuming the maxFeeRate doesn't
// change.
func (d *Driver) MinLotSize(maxFeeRate uint64) uint64 {
	return MinLotSize(maxFeeRate, true)
}

func init() {
	asset.Register(BipID, &Driver{})
}

// RecoveryCfg is the information that is transferred from the old wallet
// to the new one when the wallet is recovered.
type RecoveryCfg struct {
	NumExternalAddresses uint32 `ini:"numexternaladdr"`
	NumInternalAddresses uint32 `ini:"numinternaladdr"`
}

func parseChainParams(net util.Network) (*chaincfg.Params, error) {
	switch net {
	case util.Mainnet:
		return &chaincfg.MainNetParams, nil
	case util.Testnet:
		return &chaincfg.TestNet3Params, nil
	case util.Regtest:
		return &chaincfg.RegressionNetParams, nil
	}
	return nil, fmt.Errorf("unknown network ID %v", net)
}

// NewWallet is the exported constructor for the Bitcoin wallet.
func NewWallet(cfg *asset.WalletConfig, logger util.Logger, net util.Network) (asset.Wallet, error) {
	params, err := parseChainParams(net)
	if err != nil {
		return nil, err
	}

	switch cfg.Type {
	case walletTypeSPV:
		// The SPV wallet is the same lean wallet as electrum, driven by the
		// neutrino backend instead of an electrum client.
		return openSPVWallet(cfg, params, logger), nil
	case walletTypeElectrum:
		return openElectrumWallet(cfg, params, logger), nil
	default:
		return nil, fmt.Errorf("unknown wallet type %q", cfg.Type)
	}
}

// Convert the BTC value to satoshi.
func toSatoshi(v float64) uint64 {
	return uint64(math.Round(v * conventionalConversionFactor))
}

// BlockHeader is a partial btcjson.GetBlockHeaderVerboseResult with mediantime
// included.
type BlockHeader struct {
	Hash              string `json:"hash"`
	Confirmations     int64  `json:"confirmations"`
	Height            int64  `json:"height"`
	Time              int64  `json:"time"`
	PreviousBlockHash string `json:"previousblockhash"`
	MedianTime        int64  `json:"mediantime"`
}

// ToCoinID converts the tx hash and vout to a coin ID, as a []byte.
func ToCoinID(txHash *chainhash.Hash, vout uint32) []byte {
	coinID := make([]byte, chainhash.HashSize+4)
	copy(coinID[:chainhash.HashSize], txHash[:])
	binary.BigEndian.PutUint32(coinID[chainhash.HashSize:], vout)
	return coinID
}

// decodeCoinID decodes the coin ID into a tx hash and a vout.
func decodeCoinID(coinID util.Bytes) (*chainhash.Hash, uint32, error) {
	if len(coinID) != 36 {
		return nil, 0, fmt.Errorf("coin ID wrong length. expected 36, got %d", len(coinID))
	}
	var txHash chainhash.Hash
	copy(txHash[:], coinID[:32])
	return &txHash, binary.BigEndian.Uint32(coinID[32:]), nil
}
