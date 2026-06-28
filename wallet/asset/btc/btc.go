// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/config"
	"github.com/bisoncraft/bosswallet/util/dexnet"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/broadcast"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/rpcclient/v8"
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

	minNetworkVersion  = 270000
	minProtocolVersion = 70015
	// version which descriptor wallets have been introduced.
	minDescriptorVersion = 220000

	walletTypeLegacy   = ""
	walletTypeRPC      = "bitcoindRPC"
	walletTypeSPV      = "SPV"
	walletTypeElectrum = "electrumRPC"

	multiSplitKey       = "multisplit"
	multiSplitBufferKey = "multisplitbuffer"

	// confTxFinality is the amount of confirms a redeem or refund
	// transaction needs before the trade is considered confirmed. The tx is
	// monitored until this number of confirms is reached.
	confTxFinality = 1
)

const (
	minTimeBeforeAcceleration uint64 = 3600 // 1 hour
)

var (
	// ContractSearchLimit is how far back in time AuditContract in SPV mode
	// will search for a contract if no txData is provided. This should be a
	// positive duration.
	ContractSearchLimit = 48 * time.Hour

	// blockTicker is the delay between calls to check for new blocks.
	blockTicker                  = time.Second
	peerCountTicker              = 5 * time.Second
	walletBlockAllowance         = time.Second * 10
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

	rpcWalletDefinition = &asset.WalletDefinition{
		Type:              walletTypeRPC,
		Tab:               "External",
		Description:       "Connect to bitcoind",
		DefaultConfigPath: SystemConfigPath("bitcoin"),
		ConfigOpts:        append(RPCConfigOpts("Bitcoin", "8332"), CommonConfigOpts("BTC", false)...),
		MultiFundingOpts:  MultiFundingOpts,
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
			rpcWalletDefinition,
			electrumWalletDefinition,
		},
		LegacyWalletIndex: 1,
		BlockchainClass:   asset.BlockchainClassUTXO,
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
			"This is useful as a fallback for SPV wallets and RPC wallets " +
			"that have recently been started.",
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

// RPCConfigOpts are the settings that are used to connect to an external RPC
// wallet.
func RPCConfigOpts(name, rpcPort string) []*asset.ConfigOption {
	return []*asset.ConfigOption{
		{
			Key:         "walletname",
			DisplayName: "Wallet Name",
			Description: "The wallet name",
		},
		{
			Key:         "rpcuser",
			DisplayName: "JSON-RPC Username",
			Description: fmt.Sprintf("%s's 'rpcuser' setting", name),
		},
		{
			Key:         "rpcpassword",
			DisplayName: "JSON-RPC Password",
			Description: fmt.Sprintf("%s's 'rpcpassword' setting", name),
			NoEcho:      true,
		},
		{
			Key:          "rpcbind",
			DisplayName:  "JSON-RPC Address",
			Description:  "<addr> or <addr>:<port> (default 'localhost')",
			DefaultValue: "127.0.0.1",
		},
		{
			Key:          "rpcport",
			DisplayName:  "JSON-RPC Port",
			Description:  "Port for RPC connections (if not set in rpcbind)",
			DefaultValue: rpcPort,
		},
	}
}

// TxInSigner is a transaction input signer. In addition to the standard Bitcoin
// arguments, TxInSigner receives all values and pubkey scripts for previous
// outpoints spent in this transaction.
type TxInSigner func(tx *wire.MsgTx, idx int, subScript []byte, hashType txscript.SigHashType,
	key *btcec.PrivateKey, vals []int64, prevScripts [][]byte) ([]byte, error)

// BTCCloneCFG holds clone specific parameters.
type BTCCloneCFG struct {
	WalletCFG         *asset.WalletConfig
	MinNetworkVersion uint64
	WalletInfo        *asset.WalletInfo
	Symbol            string
	Logger            util.Logger
	Network           util.Network
	ChainParams       *chaincfg.Params
	// Ports is the default wallet RPC tcp ports used when undefined in
	// WalletConfig.
	Ports               NetPorts
	DefaultFallbackFee  uint64 // sats/byte
	DefaultFeeRateLimit uint64 // sats/byte
	// LegacyBalance is for clones that don't yet support the 'getbalances' RPC
	// call.
	LegacyBalance bool
	// BalanceFunc is a custom function for getting the wallet's balance.
	// BalanceFunc precludes any other methods of balance retrieval.
	BalanceFunc func(ctx context.Context, locked uint64) (*asset.Balance, error)
	// If segwit is false, legacy addresses and contracts will be used. This
	// setting must match the configuration of the server's asset backend.
	Segwit bool
	// LegacyRawFeeLimit can be true if the RPC only supports the boolean
	// allowHighFees argument to the sendrawtransaction RPC.
	LegacyRawFeeLimit bool
	// InitTxSize is the size of a swap initiation transaction with a single
	// input i.e. chained swaps.
	InitTxSize uint32
	// InitTxSizeBase is the size of a swap initiation transaction with no
	// inputs. This is used to accurately determine the size of the first swap
	// in a chain when considered with the actual inputs.
	InitTxSizeBase uint32
	// PrivKeyFunc is an optional function to get a private key for an address
	// from the wallet. If not given the usual dumpprivkey RPC will be used.
	PrivKeyFunc func(addr string) (*btcec.PrivateKey, error)
	// AddressDecoder is an optional argument that can decode an address string
	// into btcutil.Address. If AddressDecoder is not supplied,
	// btcutil.DecodeAddress will be used.
	AddressDecoder AddressDecoder // string => btcutil.Address
	// AddressStringer is an optional argument that can encode a btcutil.Address
	// into an address string. If AddressStringer is not supplied, the
	// (btcutil.Address).String method will be used.
	AddressStringer AddressStringer // btcutil.Address => string, may be an override or just the String method
	// BlockDeserializer can be used in place of (*wire.MsgBlock).Deserialize.
	BlockDeserializer func([]byte) (*wire.MsgBlock, error)
	// ArglessChangeAddrRPC can be true if the getrawchangeaddress takes no
	// address-type argument.
	ArglessChangeAddrRPC bool
	// NonSegwitSigner can be true if the transaction signature hash data is not
	// the standard for non-segwit Bitcoin. If nil, txscript.
	NonSegwitSigner TxInSigner
	// ConnectFunc, if provided, is called by the RPC client at the end of the
	// (*rpcClient).connect method. Errors returned by ConnectFunc will preclude
	// the starting of goroutines associated with block and peer monitoring.
	ConnectFunc func() error
	// FeeEstimator provides a way to get fees given an RawRequest-enabled
	// client and a confirmation target.
	FeeEstimator func(context.Context, RawRequester, uint64) (uint64, error)
	// ExternalFeeEstimator should be supplied if the clone provides the
	// apifeefallback ConfigOpt. TODO: confTarget uint64
	ExternalFeeEstimator func(context.Context, util.Network) (uint64, error)
	// ExternalFeeShelfLife can be set to adjust the time to staleness of
	// external fee rates. Default is 5 minutes.
	ExternalFeeShelfLife time.Duration
	// OmitAddressType causes the address type (bech32, legacy) to be omitted
	// from calls to getnewaddress.
	OmitAddressType bool
	// LegacySignTxRPC causes the RPC client to use the signrawtransaction
	// endpoint instead of the signrawtransactionwithwallet endpoint.
	LegacySignTxRPC bool
	// BooleanGetBlockRPC causes the RPC client to use a boolean second argument
	// for the getblock endpoint, instead of Bitcoin's numeric.
	BooleanGetBlockRPC bool
	// LegacyValidateAddressRPC uses the validateaddress endpoint instead of
	// getaddressinfo in order to discover ownership of an address.
	LegacyValidateAddressRPC bool
	// SingularWallet signals that the node software supports only one wallet,
	// so the RPC endpoint does not have a /wallet/{walletname} path.
	SingularWallet bool
	// UnlockSpends manually unlocks outputs as they are spent. Most assets will
	// unlock wallet outputs automatically as they are spent.
	UnlockSpends bool
	// TxDeserializer is an optional function used to deserialize a transaction.
	TxDeserializer func([]byte) (*wire.MsgTx, error)
	// TxSerializer is an optional function used to serialize a transaction.
	TxSerializer func(*wire.MsgTx) ([]byte, error)
	// TxHasher is a function that generates a tx hash from a MsgTx.
	TxHasher func(*wire.MsgTx) *chainhash.Hash
	// TxSizeCalculator is an optional function that will be used to calculate
	// the size of a transaction.
	TxSizeCalculator func(*wire.MsgTx) uint64
	// NumericGetRawRPC uses a numeric boolean indicator for the
	// getrawtransaction RPC.
	NumericGetRawRPC bool
	// TxVersion is an optional function that returns a version to use for
	// new transactions.
	TxVersion func() int32
	// ManualMedianTime causes the median time to be calculated manually.
	ManualMedianTime bool
	// ConstantDustLimit is used if an asset enforces a dust limit (minimum
	// output value) that doesn't depend on the serialized size of the output.
	// If ConstantDustLimit is zero, IsDust is used.
	ConstantDustLimit uint64
	// OmitRPCOptionsArg is for clones that don't take an options argument.
	OmitRPCOptionsArg bool
	// AssetID is the asset ID of the clone.
	AssetID uint32
	// MinDescriptorVersion is the minimum node version required for descriptor
	// wallet support. If not set, defaults to Bitcoin's minDescriptorVersion (220000).
	// This allows Bitcoin forks and related codebases with different versioning schemes
	// (e.g., DigiByte v8.26 = 82600) to specify their own minimum descriptor version.
	MinDescriptorVersion uint64
}

// PaymentScripter can be implemented to make non-standard payment scripts.
type PaymentScripter interface {
	PaymentScript() ([]byte, error)
}

// RPCWalletConfig is a combination of RPCConfig and WalletConfig. Used for a
// wallet based on a bitcoind-like RPC API.
type RPCWalletConfig struct {
	RPCConfig    `ini:",extends"`
	WalletConfig `ini:",extends"`
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

// readRPCWalletConfig parses the settings map into a *RPCWalletConfig.
func readRPCWalletConfig(settings map[string]string, symbol string, net util.Network, ports NetPorts) (cfg *RPCWalletConfig, err error) {
	cfg = new(RPCWalletConfig)
	err = config.Unmapify(settings, cfg)
	if err != nil {
		return nil, fmt.Errorf("error parsing rpc wallet config: %w", err)
	}
	err = CheckRPCConfig(&cfg.RPCConfig, symbol, net, ports)
	return
}

// parseRPCWalletConfig parses a *RPCWalletConfig from the settings map and
// creates the unconnected *rpcclient.Client.
func parseRPCWalletConfig(settings map[string]string, symbol string, net util.Network,
	ports NetPorts, singularWallet bool) (*RPCWalletConfig, *rpcclient.Client, error) {
	cfg, err := readRPCWalletConfig(settings, symbol, net, ports)
	if err != nil {
		return nil, nil, err
	}

	// For BTC, external fee rates are the default because of the instability
	// of estimatesmartfee.
	if symbol == "btc" {
		cfg.ApiFeeFallback = true
	}

	cl, err := newRPCConnection(cfg, singularWallet)
	if err != nil {
		return nil, nil, err
	}

	return cfg, cl, nil
}

// newRPCConnection creates a new RPC client.
func newRPCConnection(cfg *RPCWalletConfig, singularWallet bool) (*rpcclient.Client, error) {
	endpoint := cfg.RPCBind
	if !singularWallet {
		endpoint += "/wallet/" + cfg.WalletName
	}

	return rpcclient.New(&rpcclient.ConnConfig{
		HTTPPostMode: true,
		DisableTLS:   true,
		Host:         endpoint,
		User:         cfg.RPCUser,
		Pass:         cfg.RPCPass,
	}, nil)
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

type CustomWallet interface {
	Wallet
	TxFeeEstimator
	TipRedemptionWallet
}

type CustomWalletConstructor func(settings map[string]string, params *chaincfg.Params) (CustomWallet, error)

// customWalletConstructors are functions for setting up CustomWallet
// implementations used by ExchangeWalletCustom.
var customWalletConstructors = map[string]CustomWalletConstructor{}

// RegisterCustomWallet registers a function that should be used in creating a
// CustomWallet implementation for ExchangeWalletCustom. External consumers can
// use this function to provide CustomWallet implementation, and must do so
// before attempting to create an ExchangeWalletCustom instance of this type.
// It'll panic if callers try to register a wallet twice.
func RegisterCustomWallet(constructor CustomWalletConstructor, def *asset.WalletDefinition) {
	for _, availableWallets := range WalletInfo.AvailableWallets {
		if def.Type == availableWallets.Type {
			panic(fmt.Sprintf("wallet type (%q) is already registered", def.Type))
		}
	}
	customWalletConstructors[def.Type] = constructor
	WalletInfo.AvailableWallets = append(WalletInfo.AvailableWallets, def)
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

// feeRateCache wraps a ExternalFeeEstimator function and caches results.
type feeRateCache struct {
	f         func(context.Context, util.Network) (uint64, error)
	shelfLife time.Duration

	mtx        sync.Mutex
	fetchStamp time.Time
	lastRate   uint64
	errorStamp time.Time
	lastError  error
}

func (c *feeRateCache) rate(ctx context.Context, net util.Network) (uint64, error) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	const defaultShelfLife = time.Minute * 5
	shelfLife := defaultShelfLife
	if c.shelfLife > 0 {
		shelfLife = c.shelfLife
	}
	if time.Since(c.fetchStamp) < shelfLife {
		return c.lastRate, nil
	}
	const errorDelay = time.Minute
	if time.Since(c.errorStamp) < errorDelay {
		return 0, c.lastError
	}
	feeRate, err := c.f(ctx, net)
	if err != nil {
		c.errorStamp = time.Now()
		c.lastError = err
		return 0, err
	}
	c.fetchStamp = time.Now()
	c.lastRate = feeRate
	return feeRate, nil
}

// baseWallet is a wallet backend for Bitcoin. The backend is how the DEX
// client app communicates with the BTC blockchain and wallet. baseWallet
// satisfies the util.Wallet interface.
type baseWallet struct {
	// 64-bit atomic variables first. See
	// https://golang.org/pkg/sync/atomic/#pkg-note-BUG
	tipAtConnect int64

	cfgV              atomic.Pointer[baseWalletConfig]
	node              Wallet
	walletInfo        *asset.WalletInfo
	cloneParams       *BTCCloneCFG
	chainParams       *chaincfg.Params
	log               util.Logger
	symbol            string
	emit              *asset.WalletEmitter
	lastPeerCount     uint32
	peersChange       func(uint32, error)
	minNetworkVersion uint64
	dustLimit         uint64
	initTxSize        uint64
	initTxSizeBase    uint64
	useLegacyBalance  bool
	balanceFunc       func(ctx context.Context, locked uint64) (*asset.Balance, error)
	segwit            bool
	signNonSegwit     TxInSigner
	localFeeRate      func(context.Context, RawRequester, uint64) (uint64, error)
	feeCache          *feeRateCache
	decodeAddr        AddressDecoder
	walletDir         string
	// noListTxHistory is true for assets that cannot call the
	// ListTransactionSinceBlock method. This is true for Firo
	// electrum as of electrum 4.1.5.3.
	noListTxHistory bool

	deserializeTx func([]byte) (*wire.MsgTx, error)
	serializeTx   func(*wire.MsgTx) ([]byte, error)
	calcTxSize    func(*wire.MsgTx) uint64
	hashTx        func(*wire.MsgTx) *chainhash.Hash

	stringAddr AddressStringer

	txVersion func() int32

	Network util.Network
	ctx     context.Context // the asset subsystem starts with Connect(ctx)

	// TODO: remove currentTip and the mutex, and make it local to the
	// watchBlocks->reportNewTip call stack. The tests are reliant on current
	// internals, so this will take a little work.
	tipMtx     sync.RWMutex
	currentTip *BlockVector

	cm *CoinManager

	pendingTxsMtx sync.RWMutex
	pendingTxs    map[chainhash.Hash]ExtendedWalletTx

	// receiveTxLastQuery stores the last block height at which the wallet
	// was queried for receive transactions. This is also stored in the
	// txHistoryDB.
	receiveTxLastQuery atomic.Uint64

	txHistoryDB atomic.Value // *BadgerTxDB
	// netTxDB is the master transaction database handle supplied by Core,
	// scoped to this wallet's network. It backs txHistoryDB.
	netTxDB txdb.NetworkTxDB

	ar *AddressRecycler
}

func (w *baseWallet) cfg() *baseWalletConfig {
	return w.cfgV.Load()
}

func (w *baseWallet) fallbackFeeRate() uint64 {
	return w.cfg().fallbackFeeRate
}

func (w *baseWallet) feeRateLimit() uint64 {
	return w.cfg().feeRateLimit
}

func (w *baseWallet) redeemConfTarget() uint64 {
	return w.cfg().redeemConfTarget
}

func (w *baseWallet) useSplitTx() bool {
	return w.cfg().useSplitTx
}

func (w *baseWallet) UseSplitTx() bool {
	return w.useSplitTx()
}

func (w *baseWallet) apiFeeFallback() bool {
	return w.cfg().apiFeeFallback
}

type intermediaryWallet struct {
	*baseWallet
	txFeeEstimator TxFeeEstimator
	tipRedeemer    TipRedemptionWallet

	syncingTxHistory atomic.Bool
}

// ExchangeWalletSPV embeds a ExchangeWallet, but also provides the Rescan
// method to implement asset.Rescanner.
type ExchangeWalletSPV struct {
	*intermediaryWallet
	*authAddOn

	spvNode *spvWallet
}

// ExchangeWalletFullNode implements Wallet and adds the FeeRate method.
type ExchangeWalletFullNode struct {
	*intermediaryWallet
	*authAddOn
}

type ExchangeWalletNoAuth struct {
	*intermediaryWallet
}

// ExchangeWalletAccelerator implements the Accelerator interface on an
// ExchangeWalletFullNode.
type ExchangeWalletAccelerator struct {
	*ExchangeWalletFullNode
}

// ExchangeWalletCustom is an external wallet that implements the Wallet,
// TxFeeEstimator and TipRedemptionWallet interface.
type ExchangeWalletCustom struct {
	*intermediaryWallet
	*authAddOn
}

// Check that wallets satisfy their supported interfaces.
var _ asset.Wallet = (*intermediaryWallet)(nil)
var _ asset.Accelerator = (*ExchangeWalletAccelerator)(nil)
var _ asset.Accelerator = (*ExchangeWalletSPV)(nil)
var _ asset.Withdrawer = (*baseWallet)(nil)
var _ asset.FeeRater = (*baseWallet)(nil)
var _ asset.Rescanner = (*ExchangeWalletSPV)(nil)
var _ asset.LogFiler = (*ExchangeWalletSPV)(nil)
var _ asset.Recoverer = (*ExchangeWalletSPV)(nil)
var _ asset.PeerManager = (*ExchangeWalletSPV)(nil)
var _ asset.TxFeeEstimator = (*intermediaryWallet)(nil)
var _ asset.Authenticator = (*ExchangeWalletSPV)(nil)
var _ asset.Authenticator = (*ExchangeWalletFullNode)(nil)
var _ asset.Authenticator = (*ExchangeWalletAccelerator)(nil)
var _ asset.Authenticator = (*ExchangeWalletCustom)(nil)
var _ asset.NewAddresser = (*baseWallet)(nil)

// RecoveryCfg is the information that is transferred from the old wallet
// to the new one when the wallet is recovered.
type RecoveryCfg struct {
	NumExternalAddresses uint32 `ini:"numexternaladdr"`
	NumInternalAddresses uint32 `ini:"numinternaladdr"`
}

// GetRecoveryCfg returns information that will help the wallet get
// back to its previous state after it is recreated. Part of the
// Recoverer interface.
func (btc *ExchangeWalletSPV) GetRecoveryCfg() (map[string]string, error) {
	internal, external, err := btc.spvNode.numDerivedAddresses()
	if err != nil {
		return nil, err
	}

	reCfg := &RecoveryCfg{
		NumInternalAddresses: internal,
		NumExternalAddresses: external,
	}
	cfg, err := config.Mapify(reCfg)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// Destroy will delete all the wallet files so the wallet can be recreated.
// Part of the Recoverer interface.
func (btc *ExchangeWalletSPV) Move(backupDir string) error {
	err := btc.spvNode.moveWalletData(backupDir)
	if err != nil {
		return fmt.Errorf("unable to move wallet data: %w", err)
	}

	return nil
}

// Rescan satisfies the asset.Rescanner interface, and issues a rescan wallet
// command if the backend is an SPV wallet.
func (btc *ExchangeWalletSPV) Rescan(_ context.Context, _ /* bday already stored internally */ uint64) error {
	atomic.StoreInt64(&btc.tipAtConnect, 0) // for progress
	// Caller should start calling SyncStatus on a ticker.
	if err := btc.spvNode.wallet.RescanAsync(); err != nil {
		return err
	}
	btc.receiveTxLastQuery.Store(0)
	// Rescan is occuring asynchronously, so there's probably no point in
	// running checkPendingTxs.
	return nil
}

// Peers returns a list of peers that the wallet is connected to.
func (btc *ExchangeWalletSPV) Peers() ([]*asset.WalletPeer, error) {
	return btc.spvNode.peers()
}

// AddPeer connects the wallet to a new peer. The peer's address will be
// persisted and connected to each time the wallet is started up.
func (btc *ExchangeWalletSPV) AddPeer(addr string) error {
	return btc.spvNode.addPeer(addr)
}

// RemovePeer will remove a peer that was added by AddPeer. This peer may
// still be connected to by the wallet if it discovers it on it's own.
func (btc *ExchangeWalletSPV) RemovePeer(addr string) error {
	return btc.spvNode.removePeer(addr)
}

var _ asset.FeeRater = (*ExchangeWalletFullNode)(nil)
var _ asset.FeeRater = (*ExchangeWalletNoAuth)(nil)

// FeeRate satisfies asset.FeeRater.
func (btc *baseWallet) FeeRate() uint64 {
	rate, err := btc.feeRate(1)
	if err != nil {
		btc.log.Tracef("Failed to get fee rate: %v", err)
		return 0
	}
	return rate
}

// LogFilePath returns the path to the neutrino log file.
func (btc *ExchangeWalletSPV) LogFilePath() string {
	return btc.spvNode.logFilePath()
}

// WithdrawTx generates a transaction that withdraws all funds to the specified
// address.
func (btc *ExchangeWalletSPV) WithdrawTx(ctx context.Context, walletPW []byte, addr btcutil.Address) (_ *wire.MsgTx, err error) {
	btc.ctx = ctx
	spvw, ok := btc.node.(*spvWallet)
	if !ok {
		return nil, fmt.Errorf("invalid node type for SPV wallet")
	}
	spvw.cl, err = spvw.wallet.Start()
	if err != nil {
		return nil, fmt.Errorf("error starting wallet")
	}

	defer spvw.wallet.Stop()

	if err := spvw.Unlock(walletPW); err != nil {
		return nil, fmt.Errorf("error unlocking wallet: %w", err)
	}

	feeRate := btc.FeeRate()
	if feeRate == 0 {
		return nil, errors.New("no fee rate")
	}
	utxos, _, _, err := btc.cm.SpendableUTXOs(0)
	if err != nil {
		return nil, err
	}
	var inputsSize uint64
	coins := make(asset.Coins, 0, len(utxos))
	for _, utxo := range utxos {
		op := NewOutput(utxo.TxHash, utxo.Vout, utxo.Amount)
		coins = append(coins, op)
		inputsSize += uint64(utxo.Input.VBytes())
	}

	var baseSize uint64 = MinimumTxOverhead
	if btc.segwit {
		baseSize += P2WPKHOutputSize * 2
	} else {
		baseSize += P2PKHOutputSize * 2
	}

	fundedTx, totalIn, _, err := btc.fundedTx(coins)
	if err != nil {
		return nil, fmt.Errorf("error adding inputs to transaction: %w", err)
	}

	fees := feeRate * (inputsSize + baseSize)
	toSend := totalIn - fees

	signedTx, _, _, err := btc.signTxAndAddChange(ctx, fundedTx, addr, toSend, 0, feeRate)
	if err != nil {
		return nil, err
	}

	return signedTx, nil
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

	cloneCFG := &BTCCloneCFG{
		WalletCFG:           cfg,
		MinNetworkVersion:   minNetworkVersion,
		WalletInfo:          WalletInfo,
		Symbol:              "btc",
		Logger:              logger,
		Network:             net,
		ChainParams:         params,
		Ports:               RPCPorts,
		DefaultFallbackFee:  defaultFee,
		DefaultFeeRateLimit: defaultFeeRateLimit,
		Segwit:              true,
		// FeeEstimator must default to rpcFeeRate if not set, but set a
		// specific external estimator:
		ExternalFeeEstimator: externalFeeRate,
		AssetID:              BipID,
	}

	switch cfg.Type {
	case walletTypeSPV:
		// The SPV wallet is the same lean wallet as electrum, driven by the
		// neutrino backend instead of an electrum client.
		return openSPVWallet(cfg, params, logger), nil
	case walletTypeRPC, walletTypeLegacy:
		rpcWallet, err := BTCCloneWallet(cloneCFG)
		if err != nil {
			return nil, err
		}
		return &ExchangeWalletAccelerator{rpcWallet}, nil
	case walletTypeElectrum:
		return openElectrumWallet(cfg, params, logger), nil
	default:
		makeCustomWallet, ok := customWalletConstructors[cfg.Type]
		if !ok {
			return nil, fmt.Errorf("unknown wallet type %q", cfg.Type)
		}
		return OpenCustomWallet(cloneCFG, makeCustomWallet)
	}
}

// BTCCloneWallet creates a wallet backend for a set of network parameters and
// default network ports. A BTC clone can use this method, possibly in
// conjunction with ReadCloneParams, to create a ExchangeWallet for other assets
// with minimal coding.
func BTCCloneWallet(cfg *BTCCloneCFG) (*ExchangeWalletFullNode, error) {
	iw, err := btcCloneWallet(cfg)
	if err != nil {
		return nil, err
	}
	return &ExchangeWalletFullNode{iw, &authAddOn{iw.node}}, nil
}

// BTCCloneWalletNoAuth is like BTCCloneWallet but the wallet created does not
// implement asset.Authenticator.
func BTCCloneWalletNoAuth(cfg *BTCCloneCFG) (*ExchangeWalletNoAuth, error) {
	iw, err := btcCloneWallet(cfg)
	if err != nil {
		return nil, err
	}
	return &ExchangeWalletNoAuth{iw}, nil
}

// btcCloneWallet creates a wallet backend for a set of network parameters and
// default network ports.
func btcCloneWallet(cfg *BTCCloneCFG) (*intermediaryWallet, error) {
	clientCfg, client, err := parseRPCWalletConfig(cfg.WalletCFG.Settings, cfg.Symbol, cfg.Network, cfg.Ports, cfg.SingularWallet)
	if err != nil {
		return nil, err
	}

	iw, err := newRPCWallet(client, cfg, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("error creating %s exchange wallet: %v", cfg.Symbol,
			err)
	}

	return iw, nil
}

// newRPCWallet creates the ExchangeWallet and starts the block monitor.
func newRPCWallet(requester RawRequester, cfg *BTCCloneCFG, parsedCfg *RPCWalletConfig) (*intermediaryWallet, error) {
	btc, err := newUnconnectedWallet(cfg, &parsedCfg.WalletConfig)
	if err != nil {
		return nil, err
	}

	blockDeserializer := cfg.BlockDeserializer
	if blockDeserializer == nil {
		blockDeserializer = deserializeBlock
	}

	// Use asset-specific minDescriptorVersion if set, otherwise use Bitcoin's default
	descriptorVersion := cfg.MinDescriptorVersion
	if descriptorVersion == 0 {
		descriptorVersion = minDescriptorVersion
	}

	core := &rpcCore{
		rpcConfig:            &parsedCfg.RPCConfig,
		cloneParams:          cfg,
		segwit:               cfg.Segwit,
		decodeAddr:           btc.decodeAddr,
		stringAddr:           btc.stringAddr,
		deserializeBlock:     blockDeserializer,
		legacyRawSends:       cfg.LegacyRawFeeLimit,
		minNetworkVersion:    cfg.MinNetworkVersion,
		minDescriptorVersion: descriptorVersion,

		log:             cfg.Logger.SubLogger("RPC"),
		chainParams:     cfg.ChainParams,
		omitAddressType: cfg.OmitAddressType,
		legacySignTx:    cfg.LegacySignTxRPC,
		booleanGetBlock: cfg.BooleanGetBlockRPC,
		unlockSpends:    cfg.UnlockSpends,

		deserializeTx:      btc.deserializeTx,
		serializeTx:        btc.serializeTx,
		hashTx:             btc.hashTx,
		numericGetRawTxRPC: cfg.NumericGetRawRPC,
		manualMedianTime:   cfg.ManualMedianTime,

		legacyValidateAddressRPC: cfg.LegacyValidateAddressRPC,
		omitRPCOptionsArg:        cfg.OmitRPCOptionsArg,
		privKeyFunc:              cfg.PrivKeyFunc,
	}
	core.requesterV.Store(requester)
	node := newRPCClient(core)
	btc.setNode(node)
	w := &intermediaryWallet{
		baseWallet:     btc,
		txFeeEstimator: node,
		tipRedeemer:    node,
	}

	return w, nil
}

func decodeAddress(addr string, params *chaincfg.Params) (btcutil.Address, error) {
	a, err := btcutil.DecodeAddress(addr, params)
	if err != nil {
		return nil, err
	}
	if !a.IsForNet(params) {
		return nil, errors.New("wrong network")
	}
	return a, nil
}

func newUnconnectedWallet(cfg *BTCCloneCFG, walletCfg *WalletConfig) (*baseWallet, error) {
	// Make sure we can use the specified wallet directory.
	walletDir := filepath.Join(cfg.WalletCFG.DataDir, cfg.ChainParams.Name)
	if err := os.MkdirAll(walletDir, 0744); err != nil {
		return nil, fmt.Errorf("error creating wallet directory: %w", err)
	}

	baseCfg, err := readBaseWalletConfig(walletCfg)
	if err != nil {
		return nil, err
	}

	addrDecoder := decodeAddress
	if cfg.AddressDecoder != nil {
		addrDecoder = cfg.AddressDecoder
	}

	nonSegwitSigner := rawTxInSig
	if cfg.NonSegwitSigner != nil {
		nonSegwitSigner = cfg.NonSegwitSigner
	}

	initTxSize := uint64(cfg.InitTxSize)
	if initTxSize == 0 {
		if cfg.Segwit {
			initTxSize = InitTxSizeSegwit
		} else {
			initTxSize = InitTxSize
		}
	}

	initTxSizeBase := uint64(cfg.InitTxSizeBase)
	if initTxSizeBase == 0 {
		if cfg.Segwit {
			initTxSizeBase = InitTxSizeBaseSegwit
		} else {
			initTxSizeBase = InitTxSizeBase
		}
	}

	txDeserializer := cfg.TxDeserializer
	if txDeserializer == nil {
		txDeserializer = msgTxFromBytes
	}

	txSerializer := cfg.TxSerializer
	if txSerializer == nil {
		txSerializer = serializeMsgTx
	}

	txSizeCalculator := cfg.TxSizeCalculator
	if txSizeCalculator == nil {
		txSizeCalculator = MsgTxVBytes
	}

	txHasher := cfg.TxHasher
	if txHasher == nil {
		txHasher = hashTx
	}

	addrStringer := cfg.AddressStringer
	if addrStringer == nil {
		addrStringer = stringifyAddress
	}

	txVersion := cfg.TxVersion
	if txVersion == nil {
		txVersion = func() int32 { return wire.TxVersion }
	}

	addressRecyler, err := NewAddressRecycler(filepath.Join(walletDir, "recycled-addrs.txt"), cfg.Logger)
	if err != nil {
		return nil, err
	}

	var feeCache *feeRateCache
	if cfg.ExternalFeeEstimator != nil {
		feeCache = &feeRateCache{
			f:         cfg.ExternalFeeEstimator,
			shelfLife: cfg.ExternalFeeShelfLife,
		}
	}

	w := &baseWallet{
		symbol:            cfg.Symbol,
		chainParams:       cfg.ChainParams,
		cloneParams:       cfg,
		log:               cfg.Logger,
		emit:              cfg.WalletCFG.Emit,
		peersChange:       cfg.WalletCFG.PeersChange,
		minNetworkVersion: cfg.MinNetworkVersion,
		dustLimit:         cfg.ConstantDustLimit,
		useLegacyBalance:  cfg.LegacyBalance,
		balanceFunc:       cfg.BalanceFunc,
		segwit:            cfg.Segwit,
		initTxSize:        initTxSize,
		initTxSizeBase:    initTxSizeBase,
		signNonSegwit:     nonSegwitSigner,
		localFeeRate:      cfg.FeeEstimator,
		feeCache:          feeCache,
		decodeAddr:        addrDecoder,
		stringAddr:        addrStringer,
		walletInfo:        cfg.WalletInfo,
		deserializeTx:     txDeserializer,
		serializeTx:       txSerializer,
		hashTx:            txHasher,
		calcTxSize:        txSizeCalculator,
		txVersion:         txVersion,
		Network:           cfg.Network,
		pendingTxs:        make(map[chainhash.Hash]ExtendedWalletTx),
		walletDir:         walletDir,
		netTxDB:           cfg.WalletCFG.NetworkTxDB,
		ar:                addressRecyler,
	}
	w.cfgV.Store(baseCfg)

	// Default to the BTC RPC estimator (see LTC). Consumers can use
	// noLocalFeeRate or a similar dummy function to power feeRate() requests
	// with only an external fee rate source available. Otherwise, all method
	// calls must provide a rate or accept the configured fallback.
	if w.localFeeRate == nil {
		w.localFeeRate = rpcFeeRate
	}

	return w, nil
}

// noLocalFeeRate is a dummy function for BTCCloneCFG.FeeEstimator for a wallet
// instance that cannot support a local fee rate estimate but has an external
// fee rate source.
func noLocalFeeRate(ctx context.Context, rr RawRequester, u uint64) (uint64, error) {
	return 0, errors.New("no local fee rate estimate possible")
}

// OpenCustomWallet opens a custom wallet.
func OpenCustomWallet(cfg *BTCCloneCFG, walletConstructor CustomWalletConstructor) (*ExchangeWalletCustom, error) {
	walletCfg := new(WalletConfig)
	err := config.Unmapify(cfg.WalletCFG.Settings, walletCfg)
	if err != nil {
		return nil, err
	}

	// Custom wallets without a FeeEstimator will default to any enabled
	// external fee estimator.
	if cfg.FeeEstimator == nil {
		cfg.FeeEstimator = noLocalFeeRate
	}

	btc, err := newUnconnectedWallet(cfg, walletCfg)
	if err != nil {
		return nil, err
	}

	customWallet, err := walletConstructor(cfg.WalletCFG.Settings, cfg.ChainParams)
	if err != nil {
		return nil, err
	}
	btc.setNode(customWallet)

	w := &ExchangeWalletCustom{
		intermediaryWallet: &intermediaryWallet{
			baseWallet:     btc,
			txFeeEstimator: customWallet,
			tipRedeemer:    customWallet,
		},
		authAddOn: &authAddOn{customWallet},
	}
	return w, nil
}

// OpenSPVWallet opens the previously created native SPV wallet.
func OpenSPVWallet(cfg *BTCCloneCFG, walletConstructor BTCWalletConstructor) (*ExchangeWalletSPV, error) {
	walletCfg := new(WalletConfig)
	err := config.Unmapify(cfg.WalletCFG.Settings, walletCfg)
	if err != nil {
		return nil, err
	}

	// SPV wallets without a FeeEstimator will default to any enabled external
	// fee estimator.
	if cfg.FeeEstimator == nil {
		cfg.FeeEstimator = noLocalFeeRate
	}

	btc, err := newUnconnectedWallet(cfg, walletCfg)
	if err != nil {
		return nil, err
	}

	spvw := &spvWallet{
		chainParams: cfg.ChainParams,
		cfg:         walletCfg,
		acctNum:     defaultAcctNum,
		acctName:    defaultAcctName,
		dir:         filepath.Join(cfg.WalletCFG.DataDir, cfg.ChainParams.Name),
		log:         cfg.Logger.SubLogger("SPV"),
		tipChan:     make(chan *BlockVector, 8),
		decodeAddr:  btc.decodeAddr,
	}

	spvw.BlockFiltersScanner = NewBlockFiltersScanner(spvw, spvw.log)
	spvw.wallet = walletConstructor(spvw.dir, spvw.cfg, spvw.chainParams, spvw.log)
	btc.setNode(spvw)

	w := &ExchangeWalletSPV{
		intermediaryWallet: &intermediaryWallet{
			baseWallet:     btc,
			txFeeEstimator: spvw,
			tipRedeemer:    spvw,
		},
		authAddOn: &authAddOn{spvw},
		spvNode:   spvw,
	}
	return w, nil
}

func (btc *baseWallet) setNode(node Wallet) {
	btc.node = node
	btc.cm = NewCoinManager(
		btc.log,
		btc.chainParams,
		func(val, lots, maxFeeRate uint64, reportChange bool) EnoughFunc {
			return orderEnough(val, lots, maxFeeRate, btc.initTxSizeBase, btc.initTxSize, btc.segwit, reportChange)
		},
		func() ([]*ListUnspentResult, error) { // list
			return node.ListUnspent()
		},
		func(unlock bool, ops []*Output) error { // lock
			return node.LockUnspent(unlock, ops)
		},
		func() ([]*RPCOutpoint, error) { // listLocked
			return node.ListLockUnspent()
		},
		func(txHash *chainhash.Hash, vout uint32) (*wire.TxOut, error) {
			txRaw, _, err := btc.rawWalletTx(txHash)
			if err != nil {
				return nil, err
			}
			msgTx, err := btc.deserializeTx(txRaw)
			if err != nil {
				btc.log.Warnf("Invalid transaction %v (%x): %v", txHash, txRaw, err)
				return nil, nil
			}
			if vout >= uint32(len(msgTx.TxOut)) {
				btc.log.Warnf("Invalid vout %d for %v", vout, txHash)
				return nil, nil
			}
			return msgTx.TxOut[vout], nil
		},
		func(addr btcutil.Address) (string, error) {
			return btc.stringAddr(addr, btc.chainParams)
		},
	)
}

// Info returns basic information about the wallet and asset.
func (btc *baseWallet) Info() *asset.WalletInfo {
	return btc.walletInfo
}

func (btc *baseWallet) startTxHistoryDB(ctx context.Context) (*sync.WaitGroup, error) {
	// The transaction database is the master tx DB handle supplied by Core,
	// scoped to this wallet's network. Core owns its lifecycle, so there is no
	// per-wallet database to open or close here and nothing to wait on.
	var wg sync.WaitGroup
	if btc.netTxDB == nil {
		return &wg, nil
	}

	db, err := NewBadgerTxDB(btc.netTxDB, btc.log)
	if err != nil {
		return nil, err
	}
	btc.txHistoryDB.Store(db)

	pendingTxs, err := db.GetPendingTxs()
	if err != nil {
		return nil, fmt.Errorf("failed to load unconfirmed txs: %v", err)
	}

	btc.pendingTxsMtx.Lock()
	for _, tx := range pendingTxs {
		txHash, err := chainhash.NewHashFromStr(tx.ID)
		if err != nil {
			btc.log.Errorf("Invalid txid %v from tx history db: %v", tx.ID, err)
			continue
		}
		btc.pendingTxs[*txHash] = *tx
	}
	btc.pendingTxsMtx.Unlock()

	lastQuery, err := db.GetLastReceiveTxQuery()
	if errors.Is(err, ErrNeverQueried) {
		lastQuery = 0
	} else if err != nil {
		return nil, fmt.Errorf("failed to load last query time: %v", err)
	}

	btc.receiveTxLastQuery.Store(lastQuery)

	return &wg, nil
}

// connect is shared between Wallet implementations that may have different
// monitoring goroutines or other configuration set after connect. For example
// an asset.Wallet implementation that embeds baseWallet may override Connect to
// perform monitoring differently, but still use this connect method to start up
// the btc.Wallet (the btc.node field).
func (btc *baseWallet) connect(ctx context.Context) (*sync.WaitGroup, error) {
	btc.ctx = ctx
	var wg sync.WaitGroup
	if err := btc.node.Connect(ctx, &wg); err != nil {
		return nil, err
	}

	// Initialize the best block.
	bestBlockHdr, err := btc.node.GetBestBlockHeader()
	if err != nil {
		return nil, fmt.Errorf("error initializing best block for %s: %w", btc.symbol, err)
	}
	bestBlockHash, err := chainhash.NewHashFromStr(bestBlockHdr.Hash)
	if err != nil {
		return nil, fmt.Errorf("invalid best block hash from %s node: %v", btc.symbol, err)
	}
	// Check for method unknown error for feeRate method.
	_, err = btc.feeRate(1)
	if isMethodNotFoundErr(err) {
		return nil, fmt.Errorf("fee estimation method not found. Are you configured for the correct RPC?")
	}

	bestBlock := &BlockVector{bestBlockHdr.Height, *bestBlockHash}
	btc.log.Infof("Connected wallet with current best block %v (%d)", bestBlock.Hash, bestBlock.Height)
	btc.tipMtx.Lock()
	btc.currentTip = bestBlock
	btc.tipMtx.Unlock()
	atomic.StoreInt64(&btc.tipAtConnect, btc.currentTip.Height)

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		btc.ar.WriteRecycledAddrsToFile()
	}()

	return &wg, nil
}

// Connect connects the wallet to the btc.Wallet backend and starts monitoring
// blocks and peers. Satisfies the util.Connector interface.
func (btc *intermediaryWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	wg, err := btc.connect(ctx)
	if err != nil {
		return nil, err
	}

	dbWG, err := btc.startTxHistoryDB(ctx)
	if err != nil {
		return nil, err
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		dbWG.Wait()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		btc.watchBlocks(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		btc.monitorPeers(ctx)
	}()

	wg.Add(1)
	func() {
		defer wg.Done()
		btc.tipMtx.RLock()
		tip := btc.currentTip
		btc.tipMtx.RUnlock()
		go btc.syncTxHistory(uint64(tip.Height))
	}()

	return wg, nil
}

// Reconfigure attempts to reconfigure the wallet.
func (btc *baseWallet) Reconfigure(ctx context.Context, cfg *asset.WalletConfig, currentAddress string) (restart bool, err error) {
	// See what the node says.
	restart, err = btc.node.Reconfigure(cfg, currentAddress)
	if err != nil {
		return false, err
	}

	parsedCfg := new(RPCWalletConfig)
	if err = config.Unmapify(cfg.Settings, parsedCfg); err != nil {
		return false, err
	}
	walletCfg := &parsedCfg.WalletConfig

	// Make sure the configuration parameters are valid. If restart is required,
	// this validates the configuration parameters, preventing an ugly surprise
	// when the caller attempts to open the wallet. If no restart is required,
	// we'll swap out the configuration parameters right away.
	newCfg, err := readBaseWalletConfig(walletCfg)
	if err != nil {
		return false, err
	}
	btc.cfgV.Store(newCfg) // probably won't matter if restart/reinit required

	return restart, nil
}

// IsDust checks if the tx output's value is dust. If the dustLimit is set, it
// is compared against that, otherwise the formula in IsDust is used.
func (btc *baseWallet) IsDust(txOut *wire.TxOut, minRelayTxFee uint64) bool {
	if btc.dustLimit > 0 {
		return txOut.Value < int64(btc.dustLimit)
	}
	return IsDust(txOut, minRelayTxFee)
}

// GetBlockchainInfoResult models the data returned from the getblockchaininfo
// command.
type GetBlockchainInfoResult struct {
	Chain         string `json:"chain"`
	Blocks        int64  `json:"blocks"`
	Headers       int64  `json:"headers"`
	BestBlockHash string `json:"bestblockhash"`
	// InitialBlockDownload will be true if the node is still in the initial
	// block download mode.
	InitialBlockDownload *bool `json:"initialblockdownload"`
	// InitialBlockDownloadComplete will be true if this node has completed its
	// initial block download and is expected to be synced to the network.
	// Zcash uses this terminology instead of initialblockdownload.
	InitialBlockDownloadComplete *bool `json:"initial_block_download_complete"`
}

func (r *GetBlockchainInfoResult) Syncing() bool {
	if r.InitialBlockDownloadComplete != nil && *r.InitialBlockDownloadComplete {
		return false
	}
	if r.InitialBlockDownload != nil && *r.InitialBlockDownload {
		return true
	}
	return r.Headers-r.Blocks > 1
}

// SyncStatus is information about the blockchain sync status.
func (btc *baseWallet) SyncStatus() (*asset.SyncStatus, error) {
	ss, err := btc.node.SyncStatus()
	if err != nil {
		return nil, err
	}
	if ss == nil {
		return nil, errors.New("node returned nil sync status")
	}
	ss.StartingBlocks = uint64(atomic.LoadInt64(&btc.tipAtConnect))
	if ss.Synced {
		numPeers, err := btc.node.PeerCount()
		if err != nil {
			return nil, err
		}
		ss.Synced = numPeers > 0
	}
	return ss, nil
}

// OwnsDepositAddress indicates if the provided address can be used
// to deposit funds into the wallet.
func (btc *baseWallet) OwnsDepositAddress(address string) (bool, error) {
	addr, err := btc.decodeAddr(address, btc.chainParams) // maybe move into the ownsAddress impls
	if err != nil {
		return false, err
	}
	return btc.node.OwnsAddress(addr)
}

func (btc *baseWallet) balance() (*asset.Balance, error) {
	if btc.balanceFunc != nil {
		locked, err := btc.lockedSats()
		if err != nil {
			return nil, fmt.Errorf("(legacy) lockedSats error: %w", err)
		}
		return btc.balanceFunc(btc.ctx, locked)
	}
	if btc.useLegacyBalance {
		return btc.legacyBalance()
	}
	balances, err := btc.node.Balances()
	if err != nil {
		return nil, err
	}
	locked, err := btc.lockedSats()
	if err != nil {
		return nil, err
	}

	return &asset.Balance{
		Available: toSatoshi(balances.Mine.Trusted) - locked,
		Immature:  toSatoshi(balances.Mine.Immature + balances.Mine.Untrusted),
		Locked:    locked,
		Other:     make(map[asset.BalanceCategory]asset.CustomBalance),
	}, nil
}

// Balance should return the total available funds in the wallet.
func (btc *baseWallet) Balance() (*asset.Balance, error) {
	return btc.balance()
}

// legacyBalance is used for clones that are < node version 0.18 and so don't
// have 'getbalances'.
func (btc *baseWallet) legacyBalance() (*asset.Balance, error) {
	cl, ok := btc.node.(*rpcClient)
	if !ok {
		return nil, fmt.Errorf("legacyBalance unimplemented for spv clients")
	}

	locked, err := btc.lockedSats()
	if err != nil {
		return nil, fmt.Errorf("(legacy) lockedSats error: %w", err)
	}

	walletInfo, err := cl.GetWalletInfo()
	if err != nil {
		return nil, fmt.Errorf("(legacy) GetWalletInfo error: %w", err)
	}

	return &asset.Balance{
		Available: toSatoshi(walletInfo.Balance+walletInfo.UnconfirmedBalance) - locked,
		Immature:  toSatoshi(walletInfo.ImmatureBalance),
		Locked:    locked,
		Other:     make(map[asset.BalanceCategory]asset.CustomBalance),
	}, nil
}

// feeRate returns the current optimal fee rate in sat / byte using the
// estimatesmartfee RPC or an external API if configured and enabled.
func (btc *baseWallet) feeRate(confTarget uint64) (feeRate uint64, err error) {
	allowExternalFeeRate := btc.apiFeeFallback()
	// Because of the problems Bitcoin's unstable estimatesmartfee has caused,
	// we won't use it.
	if btc.symbol != "btc" || !allowExternalFeeRate {
		feeRate, err := btc.localFeeRate(btc.ctx, btc.node, confTarget) // e.g. rpcFeeRate
		if err == nil {
			return feeRate, nil
		} else if !allowExternalFeeRate {
			return 0, fmt.Errorf("error getting local rate and external rates are disabled: %w", err)
		}
	}

	if btc.feeCache == nil {
		return 0, fmt.Errorf("external fee rate fetcher not configured")
	}

	// External estimate fallback. Error if it exceeds our limit, and the caller
	// may use btc.fallbackFeeRate(), as in feeRateWithFallback.
	feeRate, err = btc.feeCache.rate(btc.ctx, btc.Network) // e.g. externalFeeRate
	if err != nil {
		btc.log.Meter("feeRate.rate.fail", time.Hour).Errorf("Failed to get fee rate from external API: %v", err)
		return 0, nil
	}
	if feeRate <= 0 || feeRate > btc.feeRateLimit() { // but fetcher shouldn't return <= 0 without error
		return 0, fmt.Errorf("external fee rate %v exceeds configured limit", feeRate)
	}
	btc.log.Tracef("Retrieved fee rate from external API: %v", feeRate)
	return feeRate, nil
}

func rpcFeeRate(ctx context.Context, rr RawRequester, confTarget uint64) (uint64, error) {
	feeResult, err := estimateSmartFee(ctx, rr, confTarget, &btcjson.EstimateModeEconomical)
	if err != nil {
		return 0, err
	}

	if len(feeResult.Errors) > 0 {
		return 0, errors.New(strings.Join(feeResult.Errors, "; "))
	}
	if feeResult.FeeRate == nil {
		return 0, fmt.Errorf("no fee rate available")
	}
	satPerKB, err := btcutil.NewAmount(*feeResult.FeeRate) // satPerKB is 0 when err != nil
	if err != nil {
		return 0, err
	}
	if satPerKB <= 0 {
		return 0, errors.New("zero or negative fee rate")
	}
	return uint64(util.IntDivUp(int64(satPerKB), 1000)), nil
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

type amount uint64

func (a amount) String() string {
	return strconv.FormatFloat(btcutil.Amount(a).ToBTC(), 'f', -1, 64) // dec, but no trailing zeros
}

// feeRateWithFallback filters the suggested fee rate by ensuring it is within
// limits. If not, the configured fallbackFeeRate is returned and a warning
// logged.
func (btc *baseWallet) feeRateWithFallback(feeSuggestion uint64) uint64 {
	if feeSuggestion > 0 && feeSuggestion < btc.feeRateLimit() {
		btc.log.Tracef("feeRateWithFallback using caller's suggestion for fee rate, %d.",
			feeSuggestion)
		return feeSuggestion
	}
	btc.log.Warnf("Unable to get optimal fee rate, using fallback of %d", btc.fallbackFeeRate)
	return btc.fallbackFeeRate()
}

// sizeUnit returns the short form of the unit used to measure size, either
// vB if segwit, else B.
func (btc *baseWallet) sizeUnit() string {
	if btc.segwit {
		return "vB"
	}
	return "B"
}

// ReturnCoins unlocks coins. This would be used in the case of a canceled or
// partially filled order. Part of the asset.Wallet interface.
func (btc *baseWallet) ReturnCoins(unspents asset.Coins) error {
	return btc.cm.ReturnCoins(unspents)
}

// rawWalletTx gets the raw bytes of a transaction and the number of
// confirmations. This is a wrapper for checkWalletTx (if node is a
// walletTxChecker), with a fallback to getWalletTransaction.
func (btc *baseWallet) rawWalletTx(hash *chainhash.Hash) ([]byte, uint32, error) {
	if fast, ok := btc.node.(walletTxChecker); ok {
		txRaw, confs, err := fast.checkWalletTx(hash.String())
		if err == nil {
			return txRaw, confs, nil
		}
		btc.log.Warnf("checkWalletTx: %v", err)
		// fallback to getWalletTransaction
	}

	tx, err := btc.node.GetWalletTransaction(hash)
	if err != nil {
		return nil, 0, err
	}
	return tx.Bytes, uint32(tx.Confirmations), nil
}

// authAddOn implements the asset.Authenticator.
type authAddOn struct {
	w Wallet
}

// Unlock unlocks the underlying wallet. The pw supplied should be the same as
// the password for the underlying bitcoind wallet which will also be unlocked.
// It implements asset.authenticator.
func (a *authAddOn) Unlock(pw []byte) error {
	return a.w.WalletUnlock(pw)
}

// Lock locks the underlying bitcoind wallet. It implements asset.authenticator.
func (a *authAddOn) Lock() error {
	return a.w.WalletLock()
}

// Locked will be true if the wallet is currently locked. It implements
// asset.authenticator.
func (a *authAddOn) Locked() bool {
	return a.w.Locked()
}

func (btc *baseWallet) addInputsToTx(tx *wire.MsgTx, coins asset.Coins) (uint64, []OutPoint, error) {
	var totalIn uint64
	// Add the funding utxos.
	pts := make([]OutPoint, 0, len(coins))
	for _, coin := range coins {
		op, err := ConvertCoin(coin)
		if err != nil {
			return 0, nil, fmt.Errorf("error converting coin: %w", err)
		}
		if op.Val == 0 {
			return 0, nil, fmt.Errorf("zero-valued output detected for %s:%d", op.txHash(), op.vout())
		}
		totalIn += op.Val
		txIn := wire.NewTxIn(op.WireOutPoint(), []byte{}, nil)
		tx.AddTxIn(txIn)
		pts = append(pts, op.Pt)
	}
	return totalIn, pts, nil
}

// fundedTx creates and returns a new MsgTx with the provided coins as inputs.
func (btc *baseWallet) fundedTx(coins asset.Coins) (*wire.MsgTx, uint64, []OutPoint, error) {
	baseTx := wire.NewMsgTx(btc.txVersion())
	totalIn, pts, err := btc.addInputsToTx(baseTx, coins)
	if err != nil {
		return nil, 0, nil, err
	}
	return baseTx, totalIn, pts, nil
}

// lookupWalletTxOutput looks up the value of a transaction output that is
// spandable by this wallet, and creates an output.
func (btc *baseWallet) lookupWalletTxOutput(txHash *chainhash.Hash, vout uint32) (*Output, error) {
	getTxResult, err := btc.node.GetWalletTransaction(txHash)
	if err != nil {
		return nil, err
	}

	tx, err := btc.deserializeTx(getTxResult.Bytes)
	if err != nil {
		return nil, err
	}
	if len(tx.TxOut) <= int(vout) {
		return nil, fmt.Errorf("txId %s only has %d outputs. tried to access index %d",
			txHash, len(tx.TxOut), vout)
	}

	value := tx.TxOut[vout].Value
	return NewOutput(txHash, vout, uint64(value)), nil
}

// getTransactions retrieves the transactions that created coins. The
// returned slice will be in the same order as the argument.
func (btc *baseWallet) getTransactions(coins []util.Bytes) ([]*GetTransactionResult, error) {
	txs := make([]*GetTransactionResult, 0, len(coins))

	for _, coinID := range coins {
		txHash, _, err := decodeCoinID(coinID)
		if err != nil {
			return nil, err
		}
		getTxRes, err := btc.node.GetWalletTransaction(txHash)
		if err != nil {
			return nil, err
		}
		txs = append(txs, getTxRes)
	}

	return txs, nil
}

func (btc *baseWallet) getTxFee(tx *wire.MsgTx) (uint64, error) {
	var in, out uint64

	for _, txOut := range tx.TxOut {
		out += uint64(txOut.Value)
	}

	for _, txIn := range tx.TxIn {
		prevTx, err := btc.node.GetWalletTransaction(&txIn.PreviousOutPoint.Hash)
		if err != nil {
			return 0, err
		}
		prevMsgTx, err := btc.deserializeTx(prevTx.Bytes)
		if err != nil {
			return 0, err
		}
		if len(prevMsgTx.TxOut) <= int(txIn.PreviousOutPoint.Index) {
			return 0, fmt.Errorf("tx %x references index %d output of %x, but it only has %d outputs",
				btc.hashTx(tx), txIn.PreviousOutPoint.Index, prevMsgTx.TxHash(), len(prevMsgTx.TxOut))
		}
		in += uint64(prevMsgTx.TxOut[int(txIn.PreviousOutPoint.Index)].Value)
	}

	if in < out {
		return 0, fmt.Errorf("tx %x has value of inputs %d < value of outputs %d",
			btc.hashTx(tx), in, out)
	}

	return in - out, nil
}

// sizeAndFeesOfUnconfirmedTxs returns the total size in vBytes and the total
// fees spent by the unconfirmed transactions in txs.
func (btc *baseWallet) sizeAndFeesOfUnconfirmedTxs(txs []*GetTransactionResult) (size uint64, fees uint64, err error) {
	for _, tx := range txs {
		if tx.Confirmations > 0 {
			continue
		}

		msgTx, err := btc.deserializeTx(tx.Bytes)
		if err != nil {
			return 0, 0, err
		}

		fee, err := btc.getTxFee(msgTx)
		if err != nil {
			return 0, 0, err
		}

		fees += fee
		size += btc.calcTxSize(msgTx)
	}

	return size, fees, nil
}

// additionalFeesRequired calculates the additional satoshis that need to be
// sent to miners in order to increase the average fee rate of unconfirmed
// transactions to newFeeRate. An error is returned if no additional fees
// are required.
func (btc *baseWallet) additionalFeesRequired(txs []*GetTransactionResult, newFeeRate uint64) (uint64, error) {
	size, fees, err := btc.sizeAndFeesOfUnconfirmedTxs(txs)
	if err != nil {
		return 0, err
	}

	if fees >= size*newFeeRate {
		return 0, fmt.Errorf("extra fees are not needed. %d would be needed "+
			"for a fee rate of %d, but %d was already paid",
			size*newFeeRate, newFeeRate, fees)
	}

	return size*newFeeRate - fees, nil
}

// changeCanBeAccelerated returns nil if the change can be accelerated,
// otherwise it returns an error containing the reason why it cannot.
func (btc *baseWallet) changeCanBeAccelerated(change *Output, remainingSwaps bool) error {
	lockedUtxos, err := btc.node.ListLockUnspent()
	if err != nil {
		return err
	}

	changeTxHash := change.Pt.TxHash.String()
	for _, utxo := range lockedUtxos {
		if utxo.TxID == changeTxHash && utxo.Vout == change.Pt.Vout {
			if !remainingSwaps {
				return errors.New("change locked by another order")
			}
			// change is locked by this order
			return nil
		}
	}

	utxos, err := btc.node.ListUnspent()
	if err != nil {
		return err
	}
	for _, utxo := range utxos {
		if utxo.TxID == changeTxHash && utxo.Vout == change.Pt.Vout {
			return nil
		}
	}

	return errors.New("change already spent")
}

// signedAccelerationTx returns a signed transaction that sends funds to a
// change address controlled by this wallet. This new transaction will have
// a fee high enough to make the average fee of the unmined previousTxs to
// the newFeeRate. orderChange latest change in the order, and must be spent
// by this new transaction in order to accelerate the order.
// requiredForRemainingSwaps is the amount of funds that are still required
// to complete the order, so the change of the acceleration transaction must
// contain at least that amount.
func (btc *baseWallet) signedAccelerationTx(previousTxs []*GetTransactionResult, orderChange *Output, requiredForRemainingSwaps, newFeeRate uint64) (*wire.MsgTx, *Output, uint64, error) {
	makeError := func(err error) (*wire.MsgTx, *Output, uint64, error) {
		return nil, nil, 0, err
	}

	err := btc.changeCanBeAccelerated(orderChange, requiredForRemainingSwaps > 0)
	if err != nil {
		return makeError(err)
	}

	additionalFeesRequired, err := btc.additionalFeesRequired(previousTxs, newFeeRate)
	if err != nil {
		return makeError(err)
	}

	// Figure out how much funds we need to increase the fee to the requested
	// amount.
	txSize := uint64(MinimumTxOverhead)
	// Add the size of using the order change as an input
	if btc.segwit {
		txSize += RedeemP2WPKHInputTotalSize
	} else {
		txSize += RedeemP2PKHInputSize
	}
	// We need an output if funds are still required for additional swaps in
	// the order.
	if requiredForRemainingSwaps > 0 {
		if btc.segwit {
			txSize += P2WPKHOutputSize
		} else {
			txSize += P2PKHOutputSize
		}
	}
	fundsRequired := additionalFeesRequired + requiredForRemainingSwaps + txSize*newFeeRate

	var additionalInputs asset.Coins
	if fundsRequired > orderChange.Val {
		// If change not enough, need to use other UTXOs.
		enough := func(_, inputSize, inputsVal uint64) (bool, uint64) {
			txSize := MinimumTxOverhead + inputSize

			// add the order change as an input
			if btc.segwit {
				txSize += RedeemP2WPKHInputTotalSize
			} else {
				txSize += RedeemP2PKHInputSize
			}

			if requiredForRemainingSwaps > 0 {
				if btc.segwit {
					txSize += P2WPKHOutputSize
				} else {
					txSize += P2PKHOutputSize
				}
			}

			totalFees := additionalFeesRequired + txSize*newFeeRate
			totalReq := requiredForRemainingSwaps + totalFees
			totalVal := inputsVal + orderChange.Val
			return totalReq <= totalVal, totalVal - totalReq
		}
		minConfs := uint32(1)
		additionalInputs, _, _, _, _, _, err = btc.cm.Fund(0, minConfs, false, enough)
		if err != nil {
			return makeError(fmt.Errorf("failed to fund acceleration tx: %w", err))
		}
	}

	baseTx, totalIn, _, err := btc.fundedTx(append(additionalInputs, orderChange))
	if err != nil {
		return makeError(err)
	}

	addr, err := btc.node.ExternalAddress(btc.ctx)
	if err != nil {
		return makeError(fmt.Errorf("error creating change address: %w", err))
	}

	tx, output, txFee, err := btc.signTxAndAddChange(btc.ctx, baseTx, addr, totalIn, additionalFeesRequired, newFeeRate)
	if err != nil {
		return makeError(err)
	}

	return tx, output, txFee + additionalFeesRequired, nil
}

// FeesForRemainingSwaps returns the fees for a certain number of swaps at a given
// feeRate. This is only accurate if each swap has a single input. Accurate
// estimates should use PreSwap or FundOrder.
func (btc *intermediaryWallet) FeesForRemainingSwaps(n, feeRate uint64) uint64 {
	return btc.initTxSize * n * feeRate
}

// AccelerateOrder uses the Child-Pays-For-Parent technique to accelerate a
// chain of swap transactions and previous accelerations. It broadcasts a new
// transaction with a fee high enough so that the average fee of all the
// unconfirmed transactions in the chain and the new transaction will have
// an average fee rate of newFeeRate. The changeCoin argument is the latest
// change in the order. It must be the input in the acceleration transaction
// in order for the order to be accelerated. requiredForRemainingSwaps is the
// amount of funds required to complete the rest of the swaps in the order.
// The change output of the acceleration transaction will have at least
// this amount.
//
// The returned change coin may be nil, and should be checked before use.
func (btc *ExchangeWalletAccelerator) AccelerateOrder(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, newFeeRate uint64) (asset.Coin, string, error) {
	return accelerateOrder(btc.baseWallet, swapCoins, accelerationCoins, changeCoin, requiredForRemainingSwaps, newFeeRate)
}

// AccelerateOrder uses the Child-Pays-For-Parent technique to accelerate a
// chain of swap transactions and previous accelerations. It broadcasts a new
// transaction with a fee high enough so that the average fee of all the
// unconfirmed transactions in the chain and the new transaction will have
// an average fee rate of newFeeRate. The changeCoin argument is the latest
// change in the order. It must be the input in the acceleration transaction
// in order for the order to be accelerated. requiredForRemainingSwaps is the
// amount of funds required to complete the rest of the swaps in the order.
// The change output of the acceleration transaction will have at least
// this amount.
//
// The returned change coin may be nil, and should be checked before use.
func (btc *ExchangeWalletSPV) AccelerateOrder(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, newFeeRate uint64) (asset.Coin, string, error) {
	return accelerateOrder(btc.baseWallet, swapCoins, accelerationCoins, changeCoin, requiredForRemainingSwaps, newFeeRate)
}

func accelerateOrder(btc *baseWallet, swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, newFeeRate uint64) (asset.Coin, string, error) {
	changeTxHash, changeVout, err := decodeCoinID(changeCoin)
	if err != nil {
		return nil, "", err
	}
	changeOutput, err := btc.lookupWalletTxOutput(changeTxHash, changeVout)
	if err != nil {
		return nil, "", err
	}
	previousTxs, err := btc.getTransactions(append(swapCoins, accelerationCoins...))
	if err != nil {
		return nil, "", err
	}
	signedTx, newChange, fees, err :=
		btc.signedAccelerationTx(previousTxs, changeOutput, requiredForRemainingSwaps, newFeeRate)
	if err != nil {
		return nil, "", err
	}

	_, err = btc.broadcastTx(btc.ctx, signedTx)
	if err != nil {
		return nil, "", err
	}

	txHash := btc.hashTx(signedTx)
	btc.addTxToHistory(&txdb.WalletTransaction{
		Type: txdb.Acceleration,
		ID:   txHash.String(),
		Fees: fees,
	}, txHash)

	// Delete the old change from the cache
	btc.cm.ReturnOutPoint(NewOutPoint(changeTxHash, changeVout))

	if newChange == nil {
		return nil, txHash.String(), nil
	}

	// Add the new change to the cache if needed. We check if
	// required for remaining swaps > 0 because this ensures if the
	// previous change was locked, this one will also be locked. If
	// requiredForRemainingSwaps = 0, but the change was locked,
	// changeCanBeAccelerated would have returned an error since this means
	// that the change was locked by another order.
	if requiredForRemainingSwaps > 0 {
		err = btc.node.LockUnspent(false, []*Output{newChange})
		if err != nil {
			// The transaction is already broadcasted, so don't fail now.
			btc.log.Errorf("failed to lock change output: %v", err)
		}

		// Log it as a fundingCoin, since it is expected that this will be
		// chained into further matches.
		btc.cm.LockUTXOs([]*UTxO{{
			TxHash:  newChange.txHash(),
			Vout:    newChange.vout(),
			Address: newChange.String(),
			Amount:  newChange.Val,
		}})
	}

	// return nil error since tx is already broadcast, and core needs to update
	// the change coin
	return newChange, txHash.String(), nil
}

// AccelerationEstimate takes the same parameters as AccelerateOrder, but
// instead of broadcasting the acceleration transaction, it just returns
// the amount of funds that will need to be spent in order to increase the
// average fee rate to the desired amount.
func (btc *ExchangeWalletAccelerator) AccelerationEstimate(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, newFeeRate uint64) (uint64, error) {
	return accelerationEstimate(btc.baseWallet, swapCoins, accelerationCoins, changeCoin, requiredForRemainingSwaps, newFeeRate)
}

// AccelerationEstimate takes the same parameters as AccelerateOrder, but
// instead of broadcasting the acceleration transaction, it just returns
// the amount of funds that will need to be spent in order to increase the
// average fee rate to the desired amount.
func (btc *ExchangeWalletSPV) AccelerationEstimate(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, newFeeRate uint64) (uint64, error) {
	return accelerationEstimate(btc.baseWallet, swapCoins, accelerationCoins, changeCoin, requiredForRemainingSwaps, newFeeRate)
}

func accelerationEstimate(btc *baseWallet, swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, newFeeRate uint64) (uint64, error) {
	previousTxs, err := btc.getTransactions(append(swapCoins, accelerationCoins...))
	if err != nil {
		return 0, fmt.Errorf("failed to get transactions: %w", err)
	}

	changeTxHash, changeVout, err := decodeCoinID(changeCoin)
	if err != nil {
		return 0, err
	}
	changeOutput, err := btc.lookupWalletTxOutput(changeTxHash, changeVout)
	if err != nil {
		return 0, err
	}

	_, _, fee, err := btc.signedAccelerationTx(previousTxs, changeOutput, requiredForRemainingSwaps, newFeeRate)
	if err != nil {
		return 0, err
	}

	return fee, nil
}

// tooEarlyToAccelerate returns an asset.EarlyAcceleration if
// minTimeBeforeAcceleration has not passed since either the earliest
// unconfirmed swap transaction, or the latest acceleration transaction.
func tooEarlyToAccelerate(swapTxs []*GetTransactionResult, accelerationTxs []*GetTransactionResult) (*asset.EarlyAcceleration, error) {
	accelerationTxLookup := make(map[string]bool, len(accelerationTxs))
	for _, accelerationCoin := range accelerationTxs {
		accelerationTxLookup[accelerationCoin.TxID] = true
	}

	var latestAcceleration, earliestUnconfirmed uint64 = 0, math.MaxUint64
	for _, tx := range swapTxs {
		if tx.Confirmations > 0 {
			continue
		}
		if tx.Time < earliestUnconfirmed {
			earliestUnconfirmed = tx.Time
		}
	}
	for _, tx := range accelerationTxs {
		if tx.Confirmations > 0 {
			continue
		}
		if tx.Time > latestAcceleration {
			latestAcceleration = tx.Time
		}
	}

	var actionTime uint64
	var wasAccelerated bool
	if latestAcceleration == 0 && earliestUnconfirmed == math.MaxUint64 {
		return nil, fmt.Errorf("no need to accelerate because all tx are confirmed")
	} else if earliestUnconfirmed > latestAcceleration && earliestUnconfirmed < math.MaxUint64 {
		actionTime = earliestUnconfirmed
	} else {
		actionTime = latestAcceleration
		wasAccelerated = true
	}

	currentTime := uint64(time.Now().Unix())
	if actionTime+minTimeBeforeAcceleration > currentTime {
		return &asset.EarlyAcceleration{
			TimePast:       currentTime - actionTime,
			WasAccelerated: wasAccelerated,
		}, nil
	}

	return nil, nil
}

// PreAccelerate returns the current average fee rate of the unmined swap
// initiation and acceleration transactions, and also returns a suggested
// range that the fee rate should be increased to in order to expedite mining.
// The feeSuggestion argument is the current prevailing network rate. It is
// used to help determine the suggestedRange, which is a range meant to give
// the user a good amount of flexibility in determining the post acceleration
// effective fee rate, but still not allowing them to pick something
// outrageously high.
func (btc *ExchangeWalletAccelerator) PreAccelerate(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, feeSuggestion uint64) (uint64, *asset.XYRange, *asset.EarlyAcceleration, error) {
	return preAccelerate(btc.baseWallet, swapCoins, accelerationCoins, changeCoin, requiredForRemainingSwaps, feeSuggestion)
}

// PreAccelerate returns the current average fee rate of the unmined swap
// initiation and acceleration transactions, and also returns a suggested
// range that the fee rate should be increased to in order to expedite mining.
// The feeSuggestion argument is the current prevailing network rate. It is
// used to help determine the suggestedRange, which is a range meant to give
// the user a good amount of flexibility in determining the post acceleration
// effective fee rate, but still not allowing them to pick something
// outrageously high.
func (btc *ExchangeWalletSPV) PreAccelerate(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, feeSuggestion uint64) (uint64, *asset.XYRange, *asset.EarlyAcceleration, error) {
	return preAccelerate(btc.baseWallet, swapCoins, accelerationCoins, changeCoin, requiredForRemainingSwaps, feeSuggestion)
}

// maxAccelerationRate returns the max rate to which an order can be
// accelerated, if the max rate is less than rateNeeded. If the max rate is
// greater than rateNeeded, rateNeeded is returned.
func (btc *baseWallet) maxAccelerationRate(changeVal, feesAlreadyPaid, orderTxVBytes, requiredForRemainingSwaps, rateNeeded uint64) (uint64, error) {
	var txSize, witnessSize, additionalUtxosVal uint64

	// First, add all the elements that will definitely be part of the
	// acceleration transaction, without any additional inputs.
	txSize += MinimumTxOverhead
	if btc.segwit {
		txSize += RedeemP2WPKHInputSize
		witnessSize += RedeemP2WPKHInputWitnessWeight
	} else {
		txSize += RedeemP2PKHInputSize
	}
	if requiredForRemainingSwaps > 0 {
		if btc.segwit {
			txSize += P2WPKHOutputSize
		} else {
			txSize += P2PKHOutputSize
		}
	}

	calcFeeRate := func() uint64 {
		accelerationTxVBytes := txSize + (witnessSize+3)/4
		totalValue := changeVal + feesAlreadyPaid + additionalUtxosVal
		if totalValue < requiredForRemainingSwaps {
			return 0
		}
		totalValue -= requiredForRemainingSwaps
		totalSize := accelerationTxVBytes + orderTxVBytes
		return totalValue / totalSize
	}

	if calcFeeRate() >= rateNeeded {
		return rateNeeded, nil
	}

	// If necessary, use as many additional utxos as needed
	utxos, _, _, err := btc.cm.SpendableUTXOs(1)
	if err != nil {
		return 0, err
	}

	for _, utxo := range utxos {
		if utxo.Input.NonStandardScript {
			continue
		}
		txSize += TxInOverhead +
			uint64(wire.VarIntSerializeSize(uint64(utxo.Input.SigScriptSize))) +
			uint64(utxo.Input.SigScriptSize)
		witnessSize += uint64(utxo.Input.WitnessSize)
		additionalUtxosVal += utxo.Amount
		if calcFeeRate() >= rateNeeded {
			return rateNeeded, nil
		}
	}

	return calcFeeRate(), nil
}

func preAccelerate(btc *baseWallet, swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, feeSuggestion uint64) (uint64, *asset.XYRange, *asset.EarlyAcceleration, error) {
	makeError := func(err error) (uint64, *asset.XYRange, *asset.EarlyAcceleration, error) {
		return 0, &asset.XYRange{}, nil, err
	}

	changeTxHash, changeVout, err := decodeCoinID(changeCoin)
	if err != nil {
		return makeError(err)
	}
	changeOutput, err := btc.lookupWalletTxOutput(changeTxHash, changeVout)
	if err != nil {
		return makeError(err)
	}

	err = btc.changeCanBeAccelerated(changeOutput, requiredForRemainingSwaps > 0)
	if err != nil {
		return makeError(err)
	}

	txs, err := btc.getTransactions(append(swapCoins, accelerationCoins...))
	if err != nil {
		return makeError(fmt.Errorf("failed to get transactions: %w", err))
	}

	existingTxSize, feesAlreadyPaid, err := btc.sizeAndFeesOfUnconfirmedTxs(txs)
	if err != nil {
		return makeError(err)
	}
	// Is it safe to assume that transactions will all have some fee?
	if feesAlreadyPaid == 0 {
		return makeError(fmt.Errorf("all transactions are already confirmed, no need to accelerate"))
	}

	earlyAcceleration, err := tooEarlyToAccelerate(txs[:len(swapCoins)], txs[len(swapCoins):])
	if err != nil {
		return makeError(err)
	}

	// The suggested range will be the min and max of the slider that is
	// displayed on the UI. The minimum of the range is 1 higher than the
	// current effective range of the swap transactions. The max of the range
	// will be the maximum of 5x the current effective rate, or 5x the current
	// prevailing network rate. This is a completely arbitrary choice, but in
	// this way the user will definitely be able to accelerate at least 5x the
	// original rate, and even if the prevailing network rate is much higher
	// than the current effective rate, they will still have a comformtable
	// buffer above the prevailing network rate.
	const scalingFactor = 5
	currentEffectiveRate := feesAlreadyPaid / existingTxSize
	maxSuggestion := currentEffectiveRate * scalingFactor
	if feeSuggestion > currentEffectiveRate {
		maxSuggestion = feeSuggestion * scalingFactor
	}

	// We must make sure that the wallet can fund an acceleration at least
	// the max suggestion, and if not, lower the max suggestion to the max
	// rate that the wallet can fund.
	maxRate, err := btc.maxAccelerationRate(changeOutput.Val, feesAlreadyPaid, existingTxSize, requiredForRemainingSwaps, maxSuggestion)
	if err != nil {
		return makeError(err)
	}
	if maxRate <= currentEffectiveRate {
		return makeError(fmt.Errorf("cannot accelerate, max rate %v <= current rate %v", maxRate, currentEffectiveRate))
	}
	if maxRate < maxSuggestion {
		maxSuggestion = maxRate
	}

	suggestedRange := asset.XYRange{
		Start: asset.XYRangePoint{
			Label: "Min",
			X:     float64(currentEffectiveRate+1) / float64(currentEffectiveRate),
			Y:     float64(currentEffectiveRate + 1),
		},
		End: asset.XYRangePoint{
			Label: "Max",
			X:     float64(maxSuggestion) / float64(currentEffectiveRate),
			Y:     float64(maxSuggestion),
		},
		XUnit: "X",
		YUnit: btc.walletInfo.UnitInfo.AtomicUnit + "/" + btc.sizeUnit(),
	}

	return currentEffectiveRate, &suggestedRange, earlyAcceleration, nil
}

func (btc *baseWallet) txDB() *BadgerTxDB {
	dbi := btc.txHistoryDB.Load()
	if dbi == nil {
		return nil
	}
	db, ok := dbi.(*BadgerTxDB)
	if !ok {
		return nil
	}
	return db
}

func (btc *baseWallet) addTxToHistory(wt *txdb.WalletTransaction, txHash *chainhash.Hash, skipNotes ...bool) {
	txHistoryDB := btc.txDB()
	if txHistoryDB == nil {
		return
	}

	ewt := &ExtendedWalletTx{
		WalletTransaction: wt,
	}

	if wt.BlockNumber == 0 {
		btc.pendingTxsMtx.Lock()
		btc.pendingTxs[*txHash] = *ewt
		btc.pendingTxsMtx.Unlock()
	}

	if err := txHistoryDB.StoreTx(ewt); err != nil {
		btc.log.Errorf("failed to store tx in tx history db: %v", err)
	}

	skipNote := len(skipNotes) > 0 && skipNotes[0]
	if !skipNote {
		btc.emit.TransactionNote(wt, true)
	}
}

// DepositAddress returns an address for depositing funds into the
// exchange wallet.
func (btc *baseWallet) DepositAddress() (string, error) {
	addr, err := btc.node.ExternalAddress(btc.ctx)
	if err != nil {
		return "", err
	}
	return btc.stringAddr(addr, btc.chainParams)
}

// A recyclable address is a redemption or refund address that may be recycled
// if unused. If already recycled addresses are available, one will be returned.
func (btc *baseWallet) recyclableAddress() (string, error) {
	var returns []string
	defer btc.ar.ReturnAddresses(returns)
	for {
		addr := btc.ar.Address()
		if addr == "" {
			break
		}
		if owns, err := btc.OwnsDepositAddress(addr); owns {
			return addr, nil
		} else if err != nil {
			btc.log.Errorf("Error checking ownership of recycled address %q: %v", addr, err)
			returns = append(returns, addr)
		}
	}
	return btc.DepositAddress()
}

// ReturnRefundContracts should be called with the Receipt.Contract() data for
// any swaps that will not be refunded.
func (btc *baseWallet) ReturnRefundContracts(contracts [][]byte) {
	addrs := make([]string, 0, len(contracts))
	for _, c := range contracts {
		sender, _, _, _, err := ExtractSwapDetails(c, btc.segwit, btc.chainParams)
		if err != nil {
			btc.log.Errorf("Error extracting refund address from contract '%x': %v", c, err)
			continue
		}
		addr, err := btc.stringAddr(sender, btc.chainParams)
		if err != nil {
			btc.log.Errorf("Error stringifying address %q: %v", addr, err)
			continue
		}
		addrs = append(addrs, addr)
	}
	if len(addrs) > 0 {
		btc.ar.ReturnAddresses(addrs)
	}
}

// ReturnRedemptionAddress accepts a Wallet.RedemptionAddress() if the address
// will not be used.
func (btc *baseWallet) ReturnRedemptionAddress(addr string) {
	btc.ar.ReturnAddresses([]string{addr})
}

// NewAddress returns a new address from the wallet. This satisfies the
// NewAddresser interface.
func (btc *baseWallet) NewAddress() (string, error) {
	return btc.DepositAddress()
}

// AddressUsed checks if a wallet address has been used.
func (btc *baseWallet) AddressUsed(addrStr string) (bool, error) {
	return btc.node.AddressUsed(addrStr)
}

// Withdraw withdraws funds to the specified address. Fees are subtracted from
// the value. feeRate is in units of sats/byte.
// Withdraw satisfies asset.Withdrawer.
func (btc *baseWallet) Withdraw(address string, value, feeRate uint64) (asset.Coin, error) {
	txHash, vout, sent, err := btc.send(address, value, btc.feeRateWithFallback(feeRate), true)
	if err != nil {
		return nil, err
	}
	return NewOutput(txHash, vout, sent), nil
}

// Send sends the exact value to the specified address. This is different from
// Withdraw, which subtracts the tx fees from the amount sent. feeRate is in
// units of sats/byte.
func (btc *baseWallet) Send(address string, value, feeRate uint64) (asset.Coin, error) {
	txHash, vout, sent, err := btc.send(address, value, btc.feeRateWithFallback(feeRate), false)
	if err != nil {
		return nil, err
	}
	return NewOutput(txHash, vout, sent), nil
}

// SendTransaction broadcasts a valid fully-signed transaction.
func (btc *baseWallet) SendTransaction(rawTx []byte) ([]byte, error) {
	msgTx, err := btc.deserializeTx(rawTx)
	if err != nil {
		return nil, err
	}

	txHash, err := btc.node.SendRawTransaction(btc.ctx, msgTx)
	if err != nil {
		return nil, err
	}

	return ToCoinID(txHash, 0), nil
}

// send the value to the address, with the given fee rate. If subtract is true,
// the fees will be subtracted from the value. If false, the fees are in
// addition to the value. feeRate is in units of sats/byte.
func (btc *baseWallet) send(address string, val uint64, feeRate uint64, subtract bool) (*chainhash.Hash, uint32, uint64, error) {
	addr, err := btc.decodeAddr(address, btc.chainParams)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("invalid address: %s", address)
	}
	var pay2script []byte
	if scripter, is := addr.(PaymentScripter); is {
		pay2script, err = scripter.PaymentScript()
	} else {
		pay2script, err = txscript.PayToAddrScript(addr)
	}
	if err != nil {
		return nil, 0, 0, fmt.Errorf("PayToAddrScript error: %w", err)
	}

	baseSize := MinimumTxOverhead
	if btc.segwit {
		baseSize += P2WPKHOutputSize * 2
	} else {
		baseSize += P2PKHOutputSize * 2
	}

	enough := SendEnough(val, feeRate, subtract, uint64(baseSize), btc.segwit, true)
	minConfs := uint32(0)
	coins, _, _, _, inputsSize, _, err := btc.cm.Fund(0, minConfs, false, enough)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("error funding transaction: %w", err)
	}

	fundedTx, totalIn, _, err := btc.fundedTx(coins)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("error adding inputs to transaction: %w", err)
	}

	fees := feeRate * (inputsSize + uint64(baseSize))
	var toSend uint64
	if subtract {
		toSend = val - fees
	} else {
		toSend = val
	}
	fundedTx.AddTxOut(wire.NewTxOut(int64(toSend), pay2script))

	changeAddr, err := btc.node.ChangeAddress(btc.ctx)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("error creating change address: %w", err)
	}

	msgTx, err := btc.sendWithReturn(fundedTx, changeAddr, totalIn, toSend, feeRate)
	if err != nil {
		return nil, 0, 0, err
	}

	txHash := btc.hashTx(msgTx)

	var totalOut uint64
	for _, txOut := range msgTx.TxOut {
		totalOut += uint64(txOut.Value)
	}

	selfSend, err := btc.OwnsDepositAddress(address)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("error checking address ownership: %w", err)
	}
	txType := txdb.Send
	if selfSend {
		txType = txdb.SelfSend
	}

	btc.addTxToHistory(&txdb.WalletTransaction{
		Type:      txType,
		ID:        txHash.String(),
		Amount:    toSend,
		Fees:      totalIn - totalOut,
		Recipient: &address,
		Timestamp: uint64(time.Now().Unix()),
		Confirms:  &txdb.Confirms{Target: confTxFinality},
	}, txHash)

	return txHash, 0, toSend, nil
}

func (btc *baseWallet) checkPeers() {
	numPeers, err := btc.node.PeerCount()
	if err != nil {
		prevPeer := atomic.SwapUint32(&btc.lastPeerCount, 0)
		if prevPeer != 0 {
			btc.log.Errorf("Failed to get peer count: %v", err)
			btc.peersChange(0, err)
		}
		return
	}
	prevPeer := atomic.SwapUint32(&btc.lastPeerCount, numPeers)
	if prevPeer != numPeers {
		btc.peersChange(numPeers, nil)
	}
}

func (btc *baseWallet) monitorPeers(ctx context.Context) {
	ticker := time.NewTicker(peerCountTicker)
	defer ticker.Stop()
	for {
		btc.checkPeers()

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// watchBlocks pings for new blocks and runs the tipChange callback function
// when the block changes.
func (btc *intermediaryWallet) watchBlocks(ctx context.Context) {
	ticker := time.NewTicker(blockTicker)
	defer ticker.Stop()

	var walletBlock <-chan *BlockVector
	if notifier, isNotifier := btc.node.(tipNotifier); isNotifier {
		walletBlock = notifier.tipFeed()
	}

	// A polledBlock is a block found during polling, but whose broadcast has
	// been queued in anticipation of a wallet notification.
	type polledBlock struct {
		*BlockVector
		queue *time.Timer
	}

	// queuedBlock is the currently queued, polling-discovered block that will
	// be broadcast after a timeout if the wallet doesn't send the matching
	// notification.
	var queuedBlock *polledBlock

	for {
		select {

		// Poll for the block. If the wallet offers tip reports, delay reporting
		// the tip to give the wallet a moment to request and scan block data.
		case <-ticker.C:
			newTipHdr, err := btc.node.GetBestBlockHeader()
			if err != nil {
				btc.log.Errorf("failed to get best block header from %s node: %v", btc.symbol, err)
				continue
			}
			newTipHash, err := chainhash.NewHashFromStr(newTipHdr.Hash)
			if err != nil {
				btc.log.Errorf("invalid best block hash from %s node: %v", btc.symbol, err)
				continue
			}

			if queuedBlock != nil && *newTipHash == queuedBlock.BlockVector.Hash {
				continue
			}

			btc.tipMtx.RLock()
			sameTip := btc.currentTip.Hash == *newTipHash
			btc.tipMtx.RUnlock()
			if sameTip {
				continue
			}

			newTip := &BlockVector{newTipHdr.Height, *newTipHash}

			// If the wallet is not offering tip reports, send this one right
			// away.
			if walletBlock == nil {
				btc.reportNewTip(newTip)
			} else {
				// Queue it for reporting, but don't send it right away. Give the
				// wallet a chance to provide their block update. SPV wallet may
				// need more time after storing the block header to fetch and
				// scan filters and issue the FilteredBlockConnected report.
				if queuedBlock != nil {
					queuedBlock.queue.Stop()
				}
				blockAllowance := walletBlockAllowance
				syncStatus, err := btc.node.SyncStatus()
				if err != nil {
					btc.log.Errorf("Error retrieving sync status before queuing polled block: %v", err)
				} else if syncStatus != nil && !syncStatus.Synced {
					blockAllowance *= 10
				}
				queuedBlock = &polledBlock{
					BlockVector: newTip,
					queue: time.AfterFunc(blockAllowance, func() {
						if ss, _ := btc.SyncStatus(); ss != nil && ss.Synced {
							btc.log.Warnf("Reporting a block found in polling that the wallet apparently "+
								"never reported: %d %s. If you see this message repeatedly, it may indicate "+
								"an issue with the wallet.", newTip.Height, newTip.Hash)
						}
						btc.reportNewTip(newTip)
					}),
				}
			}

		// Tip reports from the wallet are always sent, and we'll clear any
		// queued polled block that would appear to be superceded by this one.
		case walletTip := <-walletBlock:
			if queuedBlock != nil && walletTip.Height >= queuedBlock.Height {
				if !queuedBlock.queue.Stop() && walletTip.Hash == queuedBlock.Hash {
					continue
				}
				queuedBlock = nil
			}
			btc.reportNewTip(walletTip)

		case <-ctx.Done():
			return
		}

		// Ensure context cancellation takes priority before the next iteration.
		if ctx.Err() != nil {
			return
		}
	}
}

// reportNewTip sets the currentTip and invokes the tipChange callback.
func (btc *intermediaryWallet) reportNewTip(newTip *BlockVector) {
	btc.tipMtx.Lock()
	defer btc.tipMtx.Unlock()

	prevTip := btc.currentTip
	btc.currentTip = newTip
	btc.log.Tracef("tip change: %d (%s) => %d (%s)", prevTip.Height, prevTip.Hash, newTip.Height, newTip.Hash)
	btc.emit.TipChange(uint64(newTip.Height))

	go btc.syncTxHistory(uint64(newTip.Height))
}

// sendWithReturn sends the unsigned transaction with an added output (unless
// dust) for the change.
func (btc *baseWallet) sendWithReturn(baseTx *wire.MsgTx, addr btcutil.Address,
	totalIn, totalOut, feeRate uint64) (*wire.MsgTx, error) {

	signedTx, _, _, err := btc.signTxAndAddChange(btc.ctx, baseTx, addr, totalIn, totalOut, feeRate)
	if err != nil {
		return nil, err
	}

	_, err = btc.broadcastTx(btc.ctx, signedTx)
	return signedTx, err
}

// signTxAndAddChange signs the passed tx and adds a change output if the change
// wouldn't be dust. Returns but does NOT broadcast the signed tx.
func (btc *baseWallet) signTxAndAddChange(ctx context.Context, baseTx *wire.MsgTx, addr btcutil.Address,
	totalIn, totalOut, feeRate uint64) (*wire.MsgTx, *Output, uint64, error) {

	makeErr := func(s string, a ...any) (*wire.MsgTx, *Output, uint64, error) {
		return nil, nil, 0, fmt.Errorf(s, a...)
	}

	// Sign the transaction to get an initial size estimate and calculate whether
	// a change output would be dust.
	sigCycles := 1
	msgTx, err := btc.node.SignTx(ctx, baseTx)
	if err != nil {
		return makeErr("signing error: %v, raw tx: %x", err, btc.wireBytes(baseTx))
	}
	vSize := btc.calcTxSize(msgTx)
	minFee := feeRate * vSize
	remaining := totalIn - totalOut
	if minFee > remaining {
		return makeErr("not enough funds to cover minimum fee rate. %.8f < %.8f, raw tx: %x",
			toBTC(totalIn), toBTC(minFee+totalOut), btc.wireBytes(baseTx))
	}

	// Create a change output.
	changeScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return makeErr("error creating change script: %v", err)
	}
	changeFees := P2PKHOutputSize * feeRate
	if btc.segwit {
		changeFees = P2WPKHOutputSize * feeRate
	}
	changeIdx := len(baseTx.TxOut)
	changeOutput := wire.NewTxOut(int64(remaining-minFee-changeFees), changeScript)
	if changeFees+minFee > remaining { // Prevent underflow
		changeOutput.Value = 0
	}
	// If the change is not dust, recompute the signed txn size and iterate on
	// the fees vs. change amount.
	changeAdded := !btc.IsDust(changeOutput, feeRate)
	if changeAdded {
		// Add the change output.
		vSize0 := btc.calcTxSize(baseTx)
		baseTx.AddTxOut(changeOutput)
		changeSize := btc.calcTxSize(baseTx) - vSize0       // may be P2WPKHOutputSize
		addrStr, _ := btc.stringAddr(addr, btc.chainParams) // just for logging
		btc.log.Tracef("Change output size = %d, addr = %s", changeSize, addrStr)

		vSize += changeSize
		fee := feeRate * vSize
		changeOutput.Value = int64(remaining - fee)
		// Find the best fee rate by closing in on it in a loop.
		tried := map[uint64]bool{}
		for {
			// Sign the transaction with the change output and compute new size.
			sigCycles++
			msgTx, err = btc.node.SignTx(ctx, baseTx)
			if err != nil {
				return makeErr("signing error: %v, raw tx: %x", err, btc.wireBytes(baseTx))
			}
			vSize = btc.calcTxSize(msgTx) // recompute the size with new tx signature
			reqFee := feeRate * vSize
			if reqFee > remaining {
				// I can't imagine a scenario where this condition would be true, but
				// I'd hate to be wrong.
				btc.log.Errorf("reached the impossible place. in = %.8f, out = %.8f, reqFee = %.8f, lastFee = %.8f, raw tx = %x, vSize = %d, feeRate = %d",
					toBTC(totalIn), toBTC(totalOut), toBTC(reqFee), toBTC(fee), btc.wireBytes(msgTx), vSize, feeRate)
				return makeErr("change error")
			}
			if fee == reqFee || (fee > reqFee && tried[reqFee]) {
				// If a lower fee appears available, but it's already been attempted and
				// had a longer serialized size, the current fee is likely as good as
				// it gets.
				break
			}

			// We must have some room for improvement.
			tried[fee] = true
			fee = reqFee
			changeOutput.Value = int64(remaining - fee)
			if btc.IsDust(changeOutput, feeRate) {
				// Another condition that should be impossible, but check anyway in case
				// the maximum fee was underestimated causing the first check to be
				// missed.
				btc.log.Errorf("reached the impossible place. in = %.8f, out = %.8f, reqFee = %.8f, lastFee = %.8f, raw tx = %x",
					toBTC(totalIn), toBTC(totalOut), toBTC(reqFee), toBTC(fee), btc.wireBytes(msgTx))
				return makeErr("dust error")
			}
			continue
		}

		totalOut += uint64(changeOutput.Value)
	} else {
		btc.log.Debugf("Foregoing change worth up to %v in tx %v because it is dust",
			changeOutput.Value, btc.hashTx(msgTx))
	}

	txHash := btc.hashTx(msgTx)

	fee := totalIn - totalOut
	actualFeeRate := fee / vSize
	btc.log.Debugf("%d signature cycles to converge on fees for tx %s: "+
		"min rate = %d, actual fee rate = %d (%v for %v bytes), change = %v",
		sigCycles, txHash, feeRate, actualFeeRate, fee, vSize, changeAdded)

	var change *Output
	if changeAdded {
		change = NewOutput(txHash, uint32(changeIdx), uint64(changeOutput.Value))
	}

	return msgTx, change, fee, nil
}

func (btc *baseWallet) broadcastTx(ctx context.Context, signedTx *wire.MsgTx) (*chainhash.Hash, error) {
	txHash, err := btc.node.SendRawTransaction(ctx, signedTx)
	if err != nil {
		if broadcast.IsAlreadyBroadcastErr(err) {
			h := btc.hashTx(signedTx)
			btc.log.Warnf("Transaction %s appears to already be broadcast: %v", h, err)
			return h, nil
		}
		return nil, fmt.Errorf("sendrawtx error: %v, raw tx: %x", err, btc.wireBytes(signedTx))
	}
	checkHash := btc.hashTx(signedTx)
	if *txHash != *checkHash {
		return nil, fmt.Errorf("transaction sent, but received unexpected transaction ID back from RPC server. "+
			"expected %s, got %s. raw tx: %x", checkHash, *txHash, btc.wireBytes(signedTx))
	}
	return txHash, nil
}

// ValidateAddress checks that the provided address is valid.
func (btc *baseWallet) ValidateAddress(address string) bool {
	_, err := btc.decodeAddr(address, btc.chainParams)
	return err == nil
}

// dummyP2PKHScript only has to be a valid 25-byte pay-to-pubkey-hash pkScript
// for EstimateSendTxFee when an empty or invalid address is provided.
var dummyP2PKHScript = []byte{0x76, 0xa9, 0x14, 0xe4, 0x28, 0x61, 0xa,
	0xfc, 0xd0, 0x4e, 0x21, 0x94, 0xf7, 0xe2, 0xcc, 0xf8,
	0x58, 0x7a, 0xc9, 0xe7, 0x2c, 0x79, 0x7b, 0x88, 0xac,
}

// EstimateSendTxFee returns a tx fee rate estimate for sending or withdrawing
// the provided amount using the provided feeRate.
func (btc *intermediaryWallet) EstimateSendTxFee(address string, sendAmount, feeRate uint64, subtract, _ bool) (fee uint64, isValidAddress bool, err error) {
	if sendAmount == 0 {
		return 0, false, fmt.Errorf("cannot check fee: send amount = 0")
	}

	var pkScript []byte
	if addr, err := btc.decodeAddr(address, btc.chainParams); err == nil {
		pkScript, err = txscript.PayToAddrScript(addr)
		if err != nil {
			return 0, false, fmt.Errorf("error generating pubkey script: %w", err)
		}
		isValidAddress = true
	} else {
		// use a dummy 25-byte p2pkh script
		pkScript = dummyP2PKHScript
	}

	wireOP := wire.NewTxOut(int64(sendAmount), pkScript)
	if IsDust(wireOP, feeRate) {
		return 0, false, errors.New("output value is dust")
	}

	tx := wire.NewMsgTx(btc.txVersion())
	tx.AddTxOut(wireOP)
	fee, err = btc.txFeeEstimator.EstimateSendTxFee(tx, btc.feeRateWithFallback(feeRate), subtract)
	if err != nil {
		return 0, false, err
	}
	return fee, isValidAddress, nil
}

// StandardSendFee returns the fees for a simple send tx with one input and two
// outputs.
func (btc *baseWallet) StandardSendFee(feeRate uint64) uint64 {
	var sz uint64 = MinimumTxOverhead
	if btc.segwit {
		inputSize := RedeemP2WPKHInputSize + uint64((RedeemP2PKSigScriptSize+2+3)/4)
		sz += inputSize + P2WPKHOutputSize*2
	} else {
		sz += RedeemP2PKHInputSize + P2PKHOutputSize*2
	}
	return feeRate * sz
}

// LockTimeExpired returns true if the specified locktime has expired, making it
// possible to spend the time-locked output. For Bitcoin, the median of the last
// 11 blocks must be past the locktime, not the current time.
func (btc *baseWallet) LockTimeExpired(_ context.Context, lockTime time.Time) (bool, error) {
	medianTime, err := btc.node.MedianTime() // TODO: pass ctx
	if err != nil {
		return false, fmt.Errorf("error getting median time: %w", err)
	}
	return medianTime.After(lockTime), nil
}

func rpcTxFee(tx *ListTransactionsResult) uint64 {
	if tx.Fee != nil {
		// Fee always seems to be negative in btcwallet, but just
		// in case.
		if *tx.Fee < 0 {
			return toSatoshi(-*tx.Fee)
		}
		return toSatoshi(*tx.Fee)
	}
	return 0
}

// idUnknownTx identifies the type and details of a transaction either made
// or received by the wallet.
func (btc *baseWallet) idUnknownTx(tx *ListTransactionsResult) (*txdb.WalletTransaction, error) {
	txHash, err := chainhash.NewHashFromStr(tx.TxID)
	if err != nil {
		return nil, fmt.Errorf("error decoding tx hash %s: %v", tx.TxID, err)
	}
	txRaw, _, err := btc.rawWalletTx(txHash)
	if err != nil {
		return nil, err
	}
	msgTx, err := btc.deserializeTx(txRaw)
	if err != nil {
		return nil, fmt.Errorf("error deserializing tx: %v", err)
	}

	fee := rpcTxFee(tx)

	var totalOut uint64
	for _, txOut := range msgTx.TxOut {
		totalOut += uint64(txOut.Value)
	}

	// Any other P2SH may be a swap or a send. We cannot determine unless we
	// look up the transaction that spends this UTXO.
	txPaysToScriptHash := func(msgTx *wire.MsgTx) (v uint64) {
		for _, txOut := range msgTx.TxOut {
			scriptClass := txscript.GetScriptClass(txOut.PkScript)
			if scriptClass == txscript.WitnessV0ScriptHashTy || scriptClass == txscript.ScriptHashTy {
				v += uint64(txOut.Value)
			}
		}
		return
	}
	if v := txPaysToScriptHash(msgTx); tx.Send && v > 0 {
		return &txdb.WalletTransaction{
			ID:     tx.TxID,
			Type:   txdb.SwapOrSend,
			Amount: v,
			Fees:   fee,
		}, nil
	}

	// Helper function will help us identify inputs that spend P2SH contracts.
	containsContractAtPushIndex := func(msgTx *wire.MsgTx, idx int, isContract func(segwit bool, contract []byte) bool) bool {
	txinloop:
		for _, txIn := range msgTx.TxIn {
			if len(txIn.Witness) > 0 {
				// segwit
				if len(txIn.Witness) < idx+1 {
					continue
				}
				contract := txIn.Witness[idx]
				if isContract(true, contract) {
					return true
				}
			} else {
				// not segwit
				const scriptVer = 0
				tokenizer := txscript.MakeScriptTokenizer(scriptVer, txIn.SignatureScript)
				for i := 0; i <= idx; i++ { // contract is 5th item item in redemption and 4th in refund
					if !tokenizer.Next() {
						continue txinloop
					}
				}
				if isContract(false, tokenizer.Data()) {
					return true
				}
			}
		}
		return false
	}

	// Swap redemptions and refunds
	contractIsSwap := func(segwit bool, contract []byte) bool {
		_, _, _, _, err := ExtractSwapDetails(contract, segwit, btc.chainParams)
		return err == nil
	}
	redeemsSwap := func(msgTx *wire.MsgTx) bool {
		return containsContractAtPushIndex(msgTx, 4, contractIsSwap)
	}
	if redeemsSwap(msgTx) {
		return &txdb.WalletTransaction{
			ID:     tx.TxID,
			Type:   txdb.Redeem,
			Amount: totalOut + fee,
			Fees:   fee,
		}, nil
	}
	refundsSwap := func(msgTx *wire.MsgTx) bool {
		return containsContractAtPushIndex(msgTx, 3, contractIsSwap)
	}
	if refundsSwap(msgTx) {
		return &txdb.WalletTransaction{
			ID:     tx.TxID,
			Type:   txdb.Refund,
			Amount: totalOut + fee,
			Fees:   fee,
		}, nil
	}

	allOutputsPayUs := func(msgTx *wire.MsgTx) bool {
		for _, txOut := range msgTx.TxOut {
			scriptClass, addrs, _, err := txscript.ExtractPkScriptAddrs(txOut.PkScript, btc.chainParams)
			if err != nil {
				btc.log.Errorf("ExtractPkScriptAddrs error: %w", err)
				return false
			}
			switch scriptClass {
			case txscript.PubKeyHashTy, txscript.WitnessV0PubKeyHashTy:
			default:
				return false
			}
			if len(addrs) != 1 { // sanity check
				return false
			}

			addr := addrs[0]
			owns, err := btc.node.OwnsAddress(addr)
			if err != nil {
				btc.log.Errorf("ownsAddress error: %w", err)
				return false
			}
			if !owns {
				return false
			}
		}

		return true
	}

	if tx.Send && allOutputsPayUs(msgTx) {
		if len(msgTx.TxOut) == 1 {
			return &txdb.WalletTransaction{
				ID:     tx.TxID,
				Type:   txdb.Acceleration,
				Amount: 0,
				Fees:   fee,
			}, nil

		}
		return &txdb.WalletTransaction{
			ID:     tx.TxID,
			Type:   txdb.Split,
			Amount: 0,
			Fees:   fee,
		}, nil
	}

	txOutDirection := func(msgTx *wire.MsgTx) (in, out uint64) {
		for _, txOut := range msgTx.TxOut {
			_, addrs, _, err := txscript.ExtractPkScriptAddrs(txOut.PkScript, btc.chainParams)
			if err != nil {
				btc.log.Errorf("ExtractPkScriptAddrs error: %w", err)
				continue
			}

			if len(addrs) == 0 { // sanity check
				continue
			}

			addr := addrs[0]
			owns, err := btc.node.OwnsAddress(addr)
			if err != nil {
				btc.log.Errorf("ownsAddress error: %w", err)
				continue
			}
			if owns {
				in += uint64(txOut.Value)
			} else {
				out += uint64(txOut.Value)
			}
		}
		return
	}
	in, out := txOutDirection(msgTx)

	getRecipient := func(msgTx *wire.MsgTx, receive bool) *string {
		for _, txOut := range msgTx.TxOut {
			_, addrs, _, err := txscript.ExtractPkScriptAddrs(txOut.PkScript, btc.chainParams)
			if err != nil {
				btc.log.Errorf("ExtractPkScriptAddrs error: %w", err)
				continue
			}

			if len(addrs) == 0 { // sanity check
				continue
			}

			addr := addrs[0]
			owns, err := btc.node.OwnsAddress(addr)
			if err != nil {
				btc.log.Errorf("ownsAddress error: %w", err)
				continue
			}

			if receive == owns {
				str := addr.String()
				return &str
			}
		}
		return nil
	}

	if tx.Send {
		return &txdb.WalletTransaction{
			ID:        tx.TxID,
			Type:      txdb.Send,
			Amount:    out,
			Fees:      fee,
			Recipient: getRecipient(msgTx, false),
		}, nil
	}

	return &txdb.WalletTransaction{
		ID:        tx.TxID,
		Type:      txdb.Receive,
		Amount:    in,
		Fees:      fee,
		Recipient: getRecipient(msgTx, true),
	}, nil
}

// addUnknownTransactionsToHistory checks for any transactions the wallet has
// made or received that are not part of the transaction history. It scans
// from the last point to which it had previously scanned to the current tip.
func (btc *baseWallet) addUnknownTransactionsToHistory(tip uint64) {
	txHistoryDB := btc.txDB()
	if txHistoryDB == nil {
		return
	}

	if btc.noListTxHistory {
		return
	}

	const blockQueryBuffer = 3
	var blockToQuery uint64
	lastQuery := btc.receiveTxLastQuery.Load()
	if lastQuery == 0 {
		// TODO: use wallet birthday instead of block 0.
		// blockToQuery = 0
	} else if lastQuery < tip-blockQueryBuffer {
		blockToQuery = lastQuery - blockQueryBuffer
	} else {
		blockToQuery = tip - blockQueryBuffer
	}

	txs, err := btc.node.ListTransactionsSinceBlock(int32(blockToQuery))
	if err != nil {
		btc.log.Errorf("Error listing transactions since block %d: %v", blockToQuery, err)
		return
	}

	for _, tx := range txs {
		if btc.ctx.Err() != nil {
			return
		}
		txHash, err := chainhash.NewHashFromStr(tx.TxID)
		if err != nil {
			btc.log.Errorf("Error decoding tx hash %s: %v", tx.TxID, err)
			continue
		}
		_, err = txHistoryDB.GetTx(txHash.String())
		if err == nil {
			continue
		}
		if !errors.Is(err, asset.CoinNotFoundError) {
			btc.log.Errorf("Error getting tx %s: %v", txHash.String(), err)
			continue
		}
		wt, err := btc.idUnknownTx(tx)
		if err != nil {
			btc.log.Errorf("error identifying transaction: %v", err)
			continue
		}
		if tx.BlockHeight > 0 && tx.BlockHeight < uint32(tip-blockQueryBuffer) {
			wt.BlockNumber = uint64(tx.BlockHeight)
			wt.Timestamp = tx.BlockTime
		} else {
			wt.Timestamp = uint64(time.Now().Unix())
		}

		// Don't send notifications for the initial sync to avoid spamming the
		// front end. A notification is sent at the end of the initial sync.
		btc.addTxToHistory(wt, txHash, blockToQuery == 0)
	}

	btc.receiveTxLastQuery.Store(tip)
	err = txHistoryDB.SetLastReceiveTxQuery(tip)
	if err != nil {
		btc.log.Errorf("Error setting last query to %d: %v", tip, err)
	}

	if blockToQuery == 0 {
		btc.emit.TransactionHistorySyncedNote()
	}
}

// syncTxHistory checks to see if there are any transactions which the wallet
// has made or received that are not part of the transaction history, then
// identifies and adds them. It also checks all the pending transactions to see
// if they have been mined into a block, and if so, updates the transaction
// history to reflect the block height.
func (btc *intermediaryWallet) syncTxHistory(tip uint64) {
	if !btc.syncingTxHistory.CompareAndSwap(false, true) {
		return
	}
	defer btc.syncingTxHistory.Store(false)

	txHistoryDB := btc.txDB()
	if txHistoryDB == nil {
		return
	}

	ss, err := btc.SyncStatus()
	if err != nil {
		btc.log.Errorf("Error getting sync status: %v", err)
		return
	}
	if ss == nil || !ss.Synced {
		return
	}

	btc.addUnknownTransactionsToHistory(tip)

	pendingTxsCopy := make(map[chainhash.Hash]ExtendedWalletTx, len(btc.pendingTxs))
	btc.pendingTxsMtx.RLock()
	maps.Copy(pendingTxsCopy, btc.pendingTxs)
	btc.pendingTxsMtx.RUnlock()

	handlePendingTx := func(txHash chainhash.Hash, tx *ExtendedWalletTx) {
		gtr, err := btc.node.GetWalletTransaction(&txHash)
		if errors.Is(err, asset.CoinNotFoundError) {
			err = txHistoryDB.RemoveTx(txHash.String())
			if err == nil || errors.Is(err, asset.CoinNotFoundError) {
				btc.pendingTxsMtx.Lock()
				delete(btc.pendingTxs, txHash)
				btc.pendingTxsMtx.Unlock()
			} else {
				// Leave it in the pendingPendingTxs and attempt to remove it
				// again next time.
				btc.log.Errorf("Error removing tx %s from the history store: %v", txHash.String(), err)
			}
			return
		}
		if err != nil {
			btc.log.Errorf("Error getting transaction %s: %v", txHash.String(), err)
			return
		}

		var updated bool
		if gtr.BlockHash != "" {
			blockHash, err := chainhash.NewHashFromStr(gtr.BlockHash)
			if err != nil {
				btc.log.Errorf("Error decoding block hash %s: %v", gtr.BlockHash, err)
				return
			}
			blockHeight, err := btc.tipRedeemer.GetBlockHeight(blockHash)
			if err != nil {
				btc.log.Errorf("Error getting block height for %s: %v", blockHash, err)
				return
			}
			if tx.BlockNumber != uint64(blockHeight) || tx.Timestamp != gtr.BlockTime {
				tx.BlockNumber = uint64(blockHeight)
				tx.Timestamp = gtr.BlockTime
			}
		} else if gtr.BlockHash == "" && tx.BlockNumber != 0 {
			tx.BlockNumber = 0
			tx.Timestamp = 0
		}

		var confs uint64
		if tx.BlockNumber > 0 && tip >= tx.BlockNumber {
			confs = tip - tx.BlockNumber + 1
		}
		if confs >= confTxFinality {
			tx.Confirmed = true
			tx.Confirms = nil
			updated = true
		} else {
			tx.Confirmed = false
			updated = tx.Confirms == nil || tx.Confirms.Current != uint32(confs)
			tx.Confirms = &txdb.Confirms{
				Current: uint32(confs),
				Target:  confTxFinality,
			}
		}

		if updated {
			err = txHistoryDB.StoreTx(tx)
			if err != nil {
				btc.log.Errorf("Error updating tx %s: %v", txHash, err)
				return
			}

			btc.pendingTxsMtx.Lock()
			if tx.Confirmed {
				delete(btc.pendingTxs, txHash)
			} else {
				btc.pendingTxs[txHash] = *tx
			}
			btc.pendingTxsMtx.Unlock()

			btc.emit.TransactionNote(tx.WalletTransaction, false)
		}
	}

	for hash, tx := range pendingTxsCopy {
		if btc.ctx.Err() != nil {
			return
		}
		handlePendingTx(hash, &tx)
	}
}

// WalletTransaction returns a transaction that either the wallet has made or
// one in which the wallet has received funds. The txID can be either a byte
// reversed tx hash or a hex encoded coin ID.
func (btc *intermediaryWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	coinID, err := hex.DecodeString(txID)
	if err == nil {
		txHash, _, err := decodeCoinID(coinID)
		if err == nil {
			txID = txHash.String()
		}
	}

	txHistoryDB := btc.txDB()
	if txHistoryDB == nil {
		return nil, fmt.Errorf("tx database not initialized")
	}

	tx, err := txHistoryDB.GetTx(txID)
	if err != nil && !errors.Is(err, asset.CoinNotFoundError) {
		return nil, err
	}
	if tx != nil && tx.Confirmed {
		return tx, nil
	}

	txHash, err := chainhash.NewHashFromStr(txID)
	if err != nil {
		return nil, fmt.Errorf("error decoding txid %s: %w", txID, err)
	}
	gtr, err := btc.node.GetWalletTransaction(txHash)
	if err != nil {
		return nil, fmt.Errorf("error getting transaction %s: %w", txID, err)
	}

	var blockHeight uint32
	if gtr.BlockHash != "" {
		blockHash, err := chainhash.NewHashFromStr(gtr.BlockHash)
		if err != nil {
			return nil, fmt.Errorf("error decoding block hash %s: %w", gtr.BlockHash, err)
		}
		height, err := btc.tipRedeemer.GetBlockHeight(blockHash)
		if err != nil {
			return nil, fmt.Errorf("error getting block height for %s: %w", blockHash, err)
		}
		blockHeight = uint32(height)
	}

	updated := tx == nil
	if tx == nil {
		tx, err = btc.idUnknownTx(&ListTransactionsResult{
			BlockHeight: blockHeight,
			BlockTime:   gtr.BlockTime,
			TxID:        txID,
		})
		if err != nil {
			return nil, fmt.Errorf("error identifying transaction: %v", err)
		}
	}

	if tx.BlockNumber != uint64(blockHeight) || tx.Timestamp != gtr.BlockTime {
		tx.BlockNumber = uint64(blockHeight)
		tx.Timestamp = gtr.BlockTime
		tx.Confirmed = blockHeight > 0
		if tx.Confirmed {
			tx.Confirms = nil
		} else {
			tx.Confirms = &txdb.Confirms{
				Current: 0,
				Target:  1,
			}
		}
		updated = true
	}

	if updated {
		btc.addTxToHistory(tx, txHash, false)
	}

	return tx, nil
}

// PendingTransactions loads wallet transactions that are not yet confirmed.
func (btc *baseWallet) PendingTransactions(ctx context.Context) []*txdb.WalletTransaction {
	btc.pendingTxsMtx.RLock()
	defer btc.pendingTxsMtx.RUnlock()
	txs := make([]*txdb.WalletTransaction, 0, len(btc.pendingTxs))
	for _, tx := range btc.pendingTxs {
		if tx.Confirmed {
			continue
		}
		txCopy := *tx.WalletTransaction
		txs = append(txs, &txCopy)
	}
	return txs
}

// TxHistory returns all the transactions the wallet has made. If refID is nil,
// then transactions starting from the most recent are returned (past is ignored).
// If past is true, the transactions prior to the refID are returned, otherwise
// the transactions after the refID are returned. n is the number of
// transactions to return. If n is <= 0, all the transactions will be returned.
func (btc *intermediaryWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	txHistoryDB := btc.txDB()
	if txHistoryDB == nil {
		return nil, fmt.Errorf("tx database not initialized")
	}
	return txHistoryDB.GetTxs(req)
}

// lockedSats is the total value of locked outputs, as locked with LockUnspent.
func (btc *baseWallet) lockedSats() (uint64, error) {
	lockedOutpoints, err := btc.node.ListLockUnspent()
	if err != nil {
		return 0, err
	}
	var sum uint64
	for _, rpcOP := range lockedOutpoints {
		txHash, err := chainhash.NewHashFromStr(rpcOP.TxID)
		if err != nil {
			return 0, err
		}
		pt := NewOutPoint(txHash, rpcOP.Vout)
		utxo := btc.cm.LockedOutput(pt)
		if utxo != nil {
			sum += utxo.Amount
			continue
		}
		tx, err := btc.node.GetWalletTransaction(txHash)
		if err != nil {
			return 0, err
		}
		txOut, err := TxOutFromTxBytes(tx.Bytes, rpcOP.Vout, btc.deserializeTx, btc.hashTx)
		if err != nil {
			return 0, err
		}
		sum += uint64(txOut.Value)
	}
	return sum, nil
}

// wireBytes dumps the serialized transaction bytes.
func (btc *baseWallet) wireBytes(tx *wire.MsgTx) []byte {
	b, err := btc.serializeTx(tx)
	// wireBytes is just used for logging, and a serialization error is
	// extremely unlikely, so just log the error and return the nil bytes.
	if err != nil {
		btc.log.Errorf("error serializing %s transaction: %v", btc.symbol, err)
		return nil
	}
	return b
}

// GetBestBlockHeight is exported for use by clone wallets. Not part of the
// asset.Wallet interface.
func (btc *baseWallet) GetBestBlockHeight() (int32, error) {
	return btc.node.GetBestBlockHeight()
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

// hashContract hashes the contract for use in a p2sh or p2wsh pubkey script.
// The hash function used depends on whether the wallet is configured for
// segwit. Non-segwit uses Hash160, segwit uses SHA256.
func hashContract(segwit bool, contract []byte) []byte {
	if segwit {
		h := sha256.Sum256(contract) // BIP141
		return h[:]
	}
	return btcutil.Hash160(contract) // BIP16
}

// CallRPC is a method for making RPC calls directly on an underlying RPC
// client. CallRPC is not part of the wallet interface. Its intended use is for
// clone wallets to implement custom functionality.
func (btc *baseWallet) CallRPC(method string, args []any, thing any) error {
	rpcCl, is := btc.node.(*rpcClient)
	if !is {
		return errors.New("wallet is not RPC")
	}
	return rpcCl.call(method, args, thing)
}

func scriptHashAddress(segwit bool, contract []byte, chainParams *chaincfg.Params) (btcutil.Address, error) {
	if segwit {
		return btcutil.NewAddressWitnessScriptHash(hashContract(segwit, contract), chainParams)
	}
	return btcutil.NewAddressScriptHash(contract, chainParams)
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

// toBTC returns a float representation in conventional units for the sats.
func toBTC[V uint64 | int64](v V) float64 {
	return btcutil.Amount(v).ToBTC()
}

// rawTxInSig signs the transaction in input using the standard bitcoin
// signature hash and ECDSA algorithm.
func rawTxInSig(tx *wire.MsgTx, idx int, pkScript []byte, hashType txscript.SigHashType,
	key *btcec.PrivateKey, _ []int64, _ [][]byte) ([]byte, error) {

	return txscript.RawTxInSignature(tx, idx, pkScript, txscript.SigHashAll, key)
}

// prettyBTC prints a value as a float with up to 8 digits of precision, but
// with trailing zeros and decimal points removed.
func prettyBTC(v uint64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(float64(v)/1e8, 'f', 8, 64), "0"), ".")
}

func hashTx(tx *wire.MsgTx) *chainhash.Hash {
	h := tx.TxHash()
	return &h
}

func stringifyAddress(addr btcutil.Address, _ *chaincfg.Params) (string, error) {
	return addr.String(), nil
}

func deserializeBlock(b []byte) (*wire.MsgBlock, error) {
	msgBlock := &wire.MsgBlock{}
	return msgBlock, msgBlock.Deserialize(bytes.NewReader(b))
}

// serializeMsgTx serializes the wire.MsgTx.
func serializeMsgTx(msgTx *wire.MsgTx) ([]byte, error) {
	buf := bytes.NewBuffer(make([]byte, 0, msgTx.SerializeSize()))
	err := msgTx.Serialize(buf)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// deserializeMsgTx creates a wire.MsgTx by deserializing data from the Reader.
func deserializeMsgTx(r io.Reader) (*wire.MsgTx, error) {
	msgTx := new(wire.MsgTx)
	err := msgTx.Deserialize(r)
	if err != nil {
		return nil, err
	}
	return msgTx, nil
}

// msgTxFromBytes creates a wire.MsgTx by deserializing the transaction.
func msgTxFromBytes(txB []byte) (*wire.MsgTx, error) {
	return deserializeMsgTx(bytes.NewReader(txB))
}

type AddressRecycler struct {
	recyclePath string
	log         util.Logger

	mtx   sync.Mutex
	addrs map[string]struct{}
}

func NewAddressRecycler(recyclePath string, log util.Logger) (*AddressRecycler, error) {
	// Try to load any cached unused redemption addresses.
	b, err := os.ReadFile(recyclePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("error looking for recycled address file: %w", err)
	}
	addrs := strings.Split(string(b), "\n")
	recycledAddrs := make(map[string]struct{}, len(addrs))
	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		recycledAddrs[addr] = struct{}{}
	}
	return &AddressRecycler{
		recyclePath: recyclePath,
		log:         log,
		addrs:       recycledAddrs,
	}, nil
}

// WriteRecycledAddrsToFile writes the recycled address cache to file.
func (a *AddressRecycler) WriteRecycledAddrsToFile() {
	a.mtx.Lock()
	addrs := make([]string, 0, len(a.addrs))
	for addr := range a.addrs {
		addrs = append(addrs, addr)
	}
	a.mtx.Unlock()
	contents := []byte(strings.Join(addrs, "\n"))
	if err := os.WriteFile(a.recyclePath, contents, 0600); err != nil {
		a.log.Errorf("Error writing recycled address file: %v", err)
	}
}

func (a *AddressRecycler) Address() string {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	for addr := range a.addrs {
		delete(a.addrs, addr)
		return addr
	}
	return ""
}

func (a *AddressRecycler) ReturnAddresses(addrs []string) {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	for _, addr := range addrs {
		if _, exists := a.addrs[addr]; exists {
			a.log.Errorf("Returned address %q was already indexed", addr)
			continue
		}
		a.addrs[addr] = struct{}{}
	}
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
