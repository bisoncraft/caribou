// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/config"
	"github.com/bisoncraft/bosswallet/util/dexnet"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const (
	version = 0

	// BipID is the BIP-0044 asset ID.
	BipID = 0

	// The default fee is passed to the user as part of the asset.WalletInfo
	// structure.
	defaultFee = 100
	// defaultFeeRateLimit is the default value for the feeratelimit.
	defaultFeeRateLimit = 1400
	// defaultRedeemConfTarget is the default redeem transaction confirmation
	// target in blocks used by estimatesmartfee to get the optimal fee for a
	// redeem transaction.
	defaultRedeemConfTarget = 2

	walletTypeSPV      = "SPV"
	walletTypeElectrum = "electrumRPC"

	multiSplitKey       = "multisplit"
	multiSplitBufferKey = "multisplitbuffer"

	// confTxFinality is the amount of confirms a redeem or refund
	// transaction needs before the trade is considered confirmed. The tx is
	// monitored until this number of confirms is reached.
	confTxFinality = 1
)

var (
	// ContractSearchLimit is how far back in time AuditContract in SPV mode
	// will search for a contract if no txData is provided. This should be a
	// positive duration.
	ContractSearchLimit = 48 * time.Hour

	conventionalConversionFactor = float64(UnitInfo.Conventional.ConversionFactor)

	// 02 Jun 21 21:12 CDT
	defaultWalletBirthdayUnix = 1622668320
	DefaultWalletBirthday     = time.Unix(int64(defaultWalletBirthdayUnix), 0)

	MultiFundingOpts = []*asset.OrderOption{
		{
			ConfigOption: asset.ConfigOption{
				Key:         multiSplitKey,
				DisplayName: "Allow multi split",
				Description: "Allow split funding transactions that pre-size outputs to " +
					"prevent excessive overlock.",
				IsBoolean:    true,
				DefaultValue: "true",
			},
		},
		{
			ConfigOption: asset.ConfigOption{
				Key:         multiSplitBufferKey,
				DisplayName: "Multi split buffer",
				Description: "Add an integer percent buffer to split output amounts to " +
					"facilitate output reuse. This is only required for quote assets.",
				DefaultValue: "5",
				DependsOn:    multiSplitKey,
			},
			QuoteAssetOnly: true,
			XYRange: &asset.XYRange{
				Start: asset.XYRangePoint{
					Label: "0%",
					X:     0,
					Y:     0,
				},
				End: asset.XYRangePoint{
					Label: "100%",
					X:     100,
					Y:     100,
				},
				XUnit:  "%",
				YUnit:  "%",
				RoundX: true,
				RoundY: true,
			},
		},
	}

	spvWalletDefinition = &asset.WalletDefinition{
		Type:             walletTypeSPV,
		Tab:              "Native",
		Description:      "Use the built-in SPV wallet",
		ConfigOpts:       CommonConfigOpts("BTC", true),
		Seeded:           true,
		MultiFundingOpts: MultiFundingOpts,
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
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
			Priority:      true,
		},
	}
)

func apiFallbackOpt(defaultV bool) *asset.ConfigOption {
	return &asset.ConfigOption{
		Key:         "apifeefallback",
		DisplayName: "External fee rate estimates",
		Description: "Allow fee rate estimation from a block explorer API. " +
			"This is useful as a fallback for light wallets that have recently " +
			"been started.",
		IsBoolean:    true,
		DefaultValue: strconv.FormatBool(defaultV),
	}
}

// CommonConfigOpts are the common options that the Wallets recognize.
func CommonConfigOpts(symbol string /* upper-case */, withApiFallback bool) []*asset.ConfigOption {
	opts := []*asset.ConfigOption{
		{
			Key:         "fallbackfee",
			DisplayName: "Fallback fee rate",
			Description: fmt.Sprintf("The fee rate to use for sending or withdrawing funds and fee payment when"+
				" estimatesmartfee is not available. Units: %s/kB", symbol),
			DefaultValue: strconv.FormatFloat(defaultFee*1000/1e8, 'f', -1, 64),
		},
		{
			Key:         "feeratelimit",
			DisplayName: "Highest acceptable fee rate",
			Description: fmt.Sprintf("This is the highest network fee rate you are willing to "+
				"pay on swap transactions. If feeratelimit is lower than a market's "+
				"maxfeerate, you will not be able to trade on that market with this "+
				"wallet.  Units: %s/kB", symbol),
			DefaultValue: strconv.FormatFloat(defaultFeeRateLimit*1000/1e8, 'f', -1, 64),
		},
		{
			Key:         "redeemconftarget",
			DisplayName: "Redeem confirmation target",
			Description: "The target number of blocks for the redeem transaction " +
				"to be mined. Used to set the transaction's fee rate. " +
				"(default: 2 blocks)",
			DefaultValue: strconv.FormatUint(defaultRedeemConfTarget, 10),
		},
		{
			Key:         "txsplit",
			DisplayName: "Pre-size funding inputs",
			Description: "When placing an order, create a \"split\" transaction to " +
				"fund the order without locking more of the wallet balance than " +
				"necessary. Otherwise, excess funds may be reserved to fund the order " +
				"until the first swap contract is broadcast during match settlement, " +
				"or the order is canceled. This an extra transaction for which network " +
				"mining fees are paid.",
			IsBoolean:    true,
			DefaultValue: "false",
		},
	}

	if withApiFallback {
		opts = append(opts, apiFallbackOpt(true))
	}
	return opts
}

// TxInSigner is a transaction input signer. In addition to the standard Bitcoin
// arguments, TxInSigner receives all values and pubkey scripts for previous
// outpoints spent in this transaction.
type TxInSigner func(tx *wire.MsgTx, idx int, subScript []byte, hashType txscript.SigHashType,
	key *btcec.PrivateKey, vals []int64, prevScripts [][]byte) ([]byte, error)

// PaymentScripter can be implemented to make non-standard payment scripts.
type PaymentScripter interface {
	PaymentScript() ([]byte, error)
}

// WalletConfig are wallet-level configuration settings.
type WalletConfig struct {
	UseSplitTx       bool    `ini:"txsplit"`
	FallbackFeeRate  float64 `ini:"fallbackfee"`
	FeeRateLimit     float64 `ini:"feeratelimit"`
	RedeemConfTarget uint64  `ini:"redeemconftarget"`
	ActivelyUsed     bool    `ini:"special_activelyUsed"` // injected by core
	ApiFeeFallback   bool    `ini:"apifeefallback"`
}

func readBaseWalletConfig(walletCfg *WalletConfig) (*baseWalletConfig, error) {
	cfg := &baseWalletConfig{}
	// if values not specified, use defaults. As they are validated as BTC/KB,
	// we need to convert first.
	if walletCfg.FallbackFeeRate == 0 {
		walletCfg.FallbackFeeRate = float64(defaultFee) * 1000 / 1e8
	}
	if walletCfg.FeeRateLimit == 0 {
		walletCfg.FeeRateLimit = float64(defaultFeeRateLimit) * 1000 / 1e8
	}
	if walletCfg.RedeemConfTarget == 0 {
		walletCfg.RedeemConfTarget = defaultRedeemConfTarget
	}
	// If set in the user config, the fallback fee will be in conventional units
	// per kB, e.g. BTC/kB. Translate that to sats/byte.
	cfg.fallbackFeeRate = toSatoshi(walletCfg.FallbackFeeRate / 1000)
	if cfg.fallbackFeeRate == 0 {
		return nil, fmt.Errorf("fallback fee rate limit is smaller than the minimum 1000 sats/byte: %v",
			walletCfg.FallbackFeeRate)
	}
	// If set in the user config, the fee rate limit will be in units of BTC/KB.
	// Convert to sats/byte & error if value is smaller than smallest unit.
	cfg.feeRateLimit = toSatoshi(walletCfg.FeeRateLimit / 1000)
	if cfg.feeRateLimit == 0 {
		return nil, fmt.Errorf("fee rate limit is smaller than the minimum 1000 sats/byte: %v",
			walletCfg.FeeRateLimit)
	}

	cfg.redeemConfTarget = walletCfg.RedeemConfTarget
	cfg.useSplitTx = walletCfg.UseSplitTx
	cfg.apiFeeFallback = walletCfg.ApiFeeFallback

	return cfg, nil
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

// createConfig combines the configuration settings used for wallet creation.
type createConfig struct {
	WalletConfig `ini:",extends"`
	RecoveryCfg  `ini:",extends"`
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

	cfg := new(createConfig)
	err = config.Unmapify(params.Settings, cfg)
	if err != nil {
		return err
	}

	_, err = readBaseWalletConfig(&cfg.WalletConfig)
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

// Open opens or connects to the BTC exchange wallet. Start the wallet with its
// Run method.
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

// baseWalletConfig is the validated, unit-converted, user-configurable wallet
// settings.
type baseWalletConfig struct {
	fallbackFeeRate  uint64 // atoms/byte
	feeRateLimit     uint64 // atoms/byte
	redeemConfTarget uint64
	useSplitTx       bool
	apiFeeFallback   bool
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

// NewWallet is the exported constructor by which the DEX will import the
// exchange wallet.
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

// externalFeeRate gets the fee rate from the external API and returns it
// in sats/vByte.
func externalFeeRate(ctx context.Context, net util.Network) (uint64, error) {
	// https://mempool.space/docs/api
	var uri string
	if net == util.Testnet {
		uri = "https://mempool.space/testnet/api/v1/fees/recommended"
	} else {
		uri = "https://mempool.space/api/v1/fees/recommended"
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var resp struct {
		Fastest  uint64 `json:"fastestFee"`
		HalfHour uint64 `json:"halfHourFee"`
		Hour     uint64 `json:"hourFee"`
		Economy  uint64 `json:"economyFee"`
		Minimum  uint64 `json:"minimumFee"`
	}
	if err := dexnet.Get(ctx, uri, &resp, dexnet.WithSizeLimit(1<<14)); err != nil {
		return 0, err
	}
	if resp.Fastest == 0 {
		return 0, errors.New("no fee rate found")
	}
	return resp.Fastest, nil
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

// BitcoreRateFetcher generates a rate fetching function for the bitcore.io API.
func BitcoreRateFetcher(ticker string) func(ctx context.Context, net util.Network) (uint64, error) {
	const uriTemplate = "https://api.bitcore.io/api/%s/%s/fee/1"
	mainnetURI, testnetURI := fmt.Sprintf(uriTemplate, ticker, "mainnet"), fmt.Sprintf(uriTemplate, ticker, "testnet")

	return func(ctx context.Context, net util.Network) (uint64, error) {
		var uri string
		if net == util.Testnet {
			uri = testnetURI
		} else {
			uri = mainnetURI
		}
		ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		var resp struct {
			RatePerKB float64 `json:"feerate"`
		}
		if err := dexnet.Get(ctx, uri, &resp); err != nil {
			return 0, err
		}
		if resp.RatePerKB <= 0 {
			return 0, fmt.Errorf("zero or negative fee rate")
		}
		return uint64(math.Round(resp.RatePerKB * 1e5)), nil // 1/kB => 1/B
	}
}
