// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dcr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/config"
	"github.com/bisoncraft/caribou/util/dexnet"
	pi "github.com/bisoncraft/caribou/util/politeia"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/broadcast"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
	"github.com/bisoncraft/caribou/wallet/txdb"

	walletjson "decred.org/dcrwallet/v5/rpc/jsonrpc/types"
	"decred.org/dcrwallet/v5/wallet"
	_ "decred.org/dcrwallet/v5/wallet/drivers/bdb"
	"github.com/davecgh/go-spew/spew"
	"github.com/decred/dcrd/blockchain/stake/v5"
	blockchain "github.com/decred/dcrd/blockchain/standalone/v2"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrjson/v4"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/hdkeychain/v3"
	chainjson "github.com/decred/dcrd/rpc/jsonrpc/types/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/sign"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/txscript/v4/stdscript"
	"github.com/decred/dcrd/wire"
	tv1 "github.com/decred/politeia/politeiawww/api/ticketvote/v1"
	vspdjson "github.com/decred/vspd/types/v2"
)

const (
	// The implementation version. This considers the dex/networks package too.
	version = 0

	// BipID is the BIP-0044 asset ID.
	BipID = 42

	// defaultFee is the default value for the fallbackfee.
	defaultFee = 20
	// defaultFeeRateLimit is the default value for the feeratelimit.
	defaultFeeRateLimit = 100
	// defaultRedeemConfTarget is the default redeem transaction confirmation
	// target in blocks used by estimatesmartfee to get the optimal fee for a
	// redeem transaction.
	defaultRedeemConfTarget = 1

	walletTypeDcrwRPC = "dcrwalletRPC"
	walletTypeLegacy  = "" // dcrwallet RPC prior to wallet types
	walletTypeSPV     = "SPV"

	// freshFeeAge is the expiry age for cached fee rates of external origin,
	// past which fetchFeeFromOracle should be used to refresh the rate.
	freshFeeAge = time.Minute

	// confTxFinality is the amount of confirms a redeem or refund
	// transaction needs before the trade is considered confirmed. The tx is
	// monitored until this number of confirms is reached. Two to make sure
	// the block containing the tx is stakeholder-approved
	confTxFinality = 2

	vspFileName = "vsp.json"

	ticketSize               = MsgTxOverhead + P2PKHInputSize + 2*P2SHOutputSize /* stakesubmission and sstxchanges */ + 32 /* see e.g. RewardCommitmentScript */
	minVSPTicketPurchaseSize = MsgTxOverhead + P2PKHInputSize + P2PKHOutputSize + ticketSize

	// sstxCommitmentString is the string to insert when a verbose
	// transaction output's pkscript type is a ticket commitment.
	sstxCommitmentString = "sstxcommitment"
)

var (
	// ContractSearchLimit is how far back in time AuditContract in SPV mode
	// will search for a contract if no txData is provided. This should be a
	// positive duration.
	ContractSearchLimit = 48 * time.Hour

	// blockTicker is the delay between calls to check for new blocks.
	blockTicker                  = time.Second
	peerCountTicker              = 5 * time.Second
	conventionalConversionFactor = float64(UnitInfo.Conventional.ConversionFactor)
	walletBlockAllowance         = time.Second * 10

	WalletOpts = []*asset.ConfigOption{
		{
			Key:         "fallbackfee",
			DisplayName: "Fallback fee rate",
			Description: "The fee rate to use for fee payment and withdrawals when " +
				"estimatesmartfee is not available. Units: DCR/kB",
			DefaultValue: strconv.FormatFloat(defaultFee*1000/1e8, 'f', -1, 64),
		},
		{
			Key:         "feeratelimit",
			DisplayName: "Highest acceptable fee rate",
			Description: "This is the highest network fee rate you are willing to " +
				"pay on swap transactions. If feeratelimit is lower than a market's " +
				"maxfeerate, you will not be able to trade on that market with this " +
				"wallet.  Units: DCR/kB",
			DefaultValue: strconv.FormatFloat(defaultFeeRateLimit*1000/1e8, 'f', -1, 64),
		},
		{
			Key:         "redeemconftarget",
			DisplayName: "Redeem confirmation target",
			Description: "The target number of blocks for the redeem transaction " +
				"to get a confirmation. Used to set the transaction's fee rate." +
				" (default: 1 block)",
			DefaultValue: strconv.FormatUint(defaultRedeemConfTarget, 10),
		},
		{
			Key:          "gaplimit",
			DisplayName:  "Address Gap Limit",
			Description:  "The gap limit for used address discovery",
			DefaultValue: strconv.FormatUint(uint64(wallet.DefaultGapLimit), 10),
		},
		{
			Key:         "txsplit",
			DisplayName: "Pre-size funding inputs",
			Description: "When placing an order, create a \"split\" transaction to " +
				"fund the order without locking more of the wallet balance than " +
				"necessary. Otherwise, excess funds may be reserved to fund the order " +
				"until the first swap contract is broadcast during match settlement, or " +
				"the order is canceled. This an extra transaction for which network " +
				"mining fees are paid.",
			IsBoolean:    true,
			DefaultValue: "true", // cheap fees, and adjustable at order-time
		},
		{
			Key:         "apifeefallback",
			DisplayName: "External fee rate estimates",
			Description: "Allow fee rate estimation from a block explorer API. " +
				"This is useful as a fallback for SPV wallets and RPC wallets " +
				"that have recently been started.",
			IsBoolean:    true,
			DefaultValue: "true",
		},
		{
			Key:          "politeiaenabled",
			DisplayName:  "Enable Politeia proposals",
			Description:  "Sync Decred governance proposals from Politeia and enable the proposals UI.",
			IsBoolean:    true,
			DefaultValue: "true",
		},
	}

	rpcOpts = []*asset.ConfigOption{
		{
			Key:         "account",
			DisplayName: "Account Name",
			Description: "Primary dcrwallet account name for trading. If automatic mixing of trading funds is " +
				"desired, this should be the wallet's mixed account and the other accounts should be set too. " +
				"See wallet documentation for mixing wallet setup instructions.",
		},
		{
			Key:         "unmixedaccount",
			DisplayName: "Change Account Name",
			Description: "dcrwallet change account name. This and the 'Temporary Trading Account' should only be " +
				"set if mixing is enabled on the wallet. If set, deposit addresses will be from this account and will " +
				"be mixed before being available to trade.",
		},
		{
			Key:         "tradingaccount",
			DisplayName: "Temporary Trading Account",
			Description: "dcrwallet account to temporarily store split tx outputs or change from chained swaps in " +
				"multi-lot orders. This should only be set if 'Change Account Name' is set.",
		},
		{
			Key:         "username",
			DisplayName: "RPC Username",
			Description: "dcrwallet's 'username' setting for JSON-RPC",
		},
		{
			Key:         "password",
			DisplayName: "RPC Password",
			Description: "dcrwallet's 'password' setting for JSON-RPC",
			NoEcho:      true,
		},
		{
			Key:          "rpclisten",
			DisplayName:  "RPC Address",
			Description:  "dcrwallet's address (host or host:port) (default port: 9110)",
			DefaultValue: "127.0.0.1:9110",
		},
		{
			Key:          "rpccert",
			DisplayName:  "TLS Certificate",
			Description:  "Path to the dcrwallet TLS certificate file",
			DefaultValue: defaultRPCCert,
		},
	}

	multiFundingOpts = []*asset.OrderOption{
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
		ConfigOpts:       WalletOpts,
		Seeded:           true,
		MultiFundingOpts: multiFundingOpts,
	}

	// WalletInfo defines some general information about a Decred wallet.
	WalletInfo = &asset.WalletInfo{
		Name:              "Decred",
		SupportedVersions: []uint32{version},
		UnitInfo:          UnitInfo,
		AvailableWallets: []*asset.WalletDefinition{
			spvWalletDefinition,
			{
				Type:              walletTypeDcrwRPC,
				Tab:               "External",
				Description:       "Connect to dcrwallet",
				DefaultConfigPath: defaultConfigPath,
				ConfigOpts:        append(rpcOpts, WalletOpts...),
				MultiFundingOpts:  multiFundingOpts,
			},
		},
		BlockchainClass: asset.BlockchainClassUTXO,
		// DCR fee rates sit near the 10 atoms/B relay floor; 1,000 never
		// binds on a real market but catches units mixups.
		MaxFeeRate: 1_000,
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
			Priority:      true,
		},
	}
	multiSplitKey       = "multisplit"
	multiSplitBufferKey = "multisplitbuffer"
)

// outPoint is the hash and output index of a transaction output.
type outPoint struct {
	txHash chainhash.Hash
	vout   uint32
}

// newOutPoint is the constructor for a new outPoint.
func newOutPoint(txHash *chainhash.Hash, vout uint32) outPoint {
	return outPoint{
		txHash: *txHash,
		vout:   vout,
	}
}

// String is a human-readable string representation of the outPoint.
func (pt outPoint) String() string {
	return pt.txHash.String() + ":" + strconv.Itoa(int(pt.vout))
}

// output is information about a transaction output. output satisfies the
// asset.Coin interface.
type output struct {
	pt    outPoint
	tree  int8
	value uint64
}

// newOutput is the constructor for an output.
func newOutput(txHash *chainhash.Hash, vout uint32, value uint64, tree int8) *output {
	return &output{
		pt: outPoint{
			txHash: *txHash,
			vout:   vout,
		},
		value: value,
		tree:  tree,
	}
}

// Value returns the value of the output. Part of the asset.Coin interface.
func (op *output) Value() uint64 {
	return op.value
}

// ID is the output's coin ID. Part of the asset.Coin interface. For DCR, the
// coin ID is 36 bytes = 32 bytes tx hash + 4 bytes big-endian vout.
func (op *output) ID() util.Bytes {
	return ToCoinID(op.txHash(), op.vout())
}

func (op *output) TxID() string {
	return op.txHash().String()
}

// String is a string representation of the coin.
func (op *output) String() string {
	return op.pt.String()
}

// txHash returns the pointer of the outPoint's txHash.
func (op *output) txHash() *chainhash.Hash {
	return &op.pt.txHash
}

// vout returns the outPoint's vout.
func (op *output) vout() uint32 {
	return op.pt.vout
}

// wireOutPoint creates and returns a new *wire.OutPoint for the output.
func (op *output) wireOutPoint() *wire.OutPoint {
	return wire.NewOutPoint(op.txHash(), op.vout(), op.tree)
}

// fundingCoin is similar to output, but also stores the address. The
// ExchangeWallet fundingCoins dict is used as a local cache of coins being
// spent.
type fundingCoin struct {
	op   *output
	addr string
}

// Driver implements asset.Driver.
type Driver struct{}

// Check that Driver implements asset.Driver.
var _ asset.Driver = (*Driver)(nil)
var _ asset.Creator = (*Driver)(nil)

// Open creates the DCR exchange wallet. Start the wallet with its Run method.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	return NewWallet(cfg, logger, network)
}

// DecodeCoinID creates a human-readable representation of a coin ID for Decred.
func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	txid, vout, err := decodeCoinID(coinID)
	if err != nil {
		return "<invalid>", err
	}
	return fmt.Sprintf("%v:%d", txid, vout), err
}

// Info returns basic information about the wallet and asset.
func (d *Driver) Info() *asset.WalletInfo {
	return WalletInfo
}

// Exists checks the existence of the wallet. Part of the Creator interface.
func (d *Driver) Exists(walletType, dataDir string, _ map[string]string, net util.Network) (bool, error) {
	if walletType != walletTypeSPV {
		return false, fmt.Errorf("no Decred wallet of type %q available", walletType)
	}

	chainParams, err := parseChainParams(net)
	if err != nil {
		return false, err
	}

	return walletExists(filepath.Join(dataDir, chainParams.Name, "spv"))
}

// Create creates a new SPV wallet.
func (d *Driver) Create(params *asset.CreateWalletParams) error {
	if params.Type != walletTypeSPV {
		return fmt.Errorf("SPV is the only seeded wallet type. required = %q, requested = %q", walletTypeSPV, params.Type)
	}
	if len(params.Seed) == 0 {
		return errors.New("wallet seed cannot be empty")
	}
	if len(params.DataDir) == 0 {
		return errors.New("must specify wallet data directory")
	}
	chainParams, err := parseChainParams(params.Net)
	if err != nil {
		return fmt.Errorf("error parsing chain params: %w", err)
	}

	recoveryCfg := new(RecoveryCfg)
	err = config.Unmapify(params.Settings, recoveryCfg)
	if err != nil {
		return err
	}

	return createSPVWallet(params.Pass, params.Seed, params.DataDir, recoveryCfg.NumExternalAddresses,
		recoveryCfg.NumInternalAddresses, recoveryCfg.GapLimit, chainParams)
}

// MinLotSize calculates the minimum lot size for a given fee rate that avoids
// dust outputs on the swap and refund txs, assuming the maxFeeRate doesn't
// change.
func (d *Driver) MinLotSize(maxFeeRate uint64) uint64 {
	return MinLotSize(maxFeeRate)
}

func init() {
	asset.Register(BipID, &Driver{})
}

// RecoveryCfg is the information that is transferred from the old wallet
// to the new one when the wallet is recovered.
type RecoveryCfg struct {
	NumExternalAddresses uint32 `ini:"numexternaladdr"`
	NumInternalAddresses uint32 `ini:"numinternaladdr"`
	GapLimit             uint32 `ini:"gaplimit"`
}

type feeStamped struct {
	rate  uint64
	stamp time.Time
}

// exchangeWalletConfig is the validated, unit-converted, user-configurable
// wallet settings.
type exchangeWalletConfig struct {
	useSplitTx       bool
	fallbackFeeRate  uint64
	feeRateLimit     uint64
	redeemConfTarget uint64
	apiFeeFallback   bool
	politeiaEnabled  bool
}

// vsp holds info needed for purchasing tickets from a vsp. PubKey is from the
// vsp and is used for verifying communications.
type vsp struct {
	URL           string  `json:"url"`
	FeePercentage float64 `json:"feepercent"`
	PubKey        string  `json:"pubkey"`
}

// rescanProgress is the progress of an asynchronous rescan.
type rescanProgress struct {
	scannedThrough int64
}

// ExchangeWallet is a wallet backend for Decred. The backend is how the DEX
// client app communicates with the Decred blockchain and wallet. ExchangeWallet
// satisfies the util.Wallet interface.
type ExchangeWallet struct {
	cfgV atomic.Pointer[exchangeWalletConfig]

	ctx            context.Context // the asset subsystem starts with Connect(ctx)
	wg             sync.WaitGroup
	wallet         Wallet
	chainParams    *chaincfg.Params
	log            util.Logger
	network        util.Network
	emit           *asset.WalletEmitter
	lastPeerCount  uint32
	peersChange    func(uint32, error)
	vspFilepath    string
	walletType     string
	walletDir      string
	startingBlocks atomic.Uint64

	oracleFeesMtx sync.Mutex
	oracleFees    map[uint64]feeStamped // conf target => fee rate
	oracleFailing bool

	handleTipMtx sync.Mutex
	currentTip   atomic.Pointer[block]

	// Coins returned by Fund are cached for quick reference.
	fundingMtx   sync.RWMutex
	fundingCoins map[outPoint]*fundingCoin

	findRedemptionMtx   sync.RWMutex
	findRedemptionQueue map[outPoint]*findRedemptionReq

	vspV atomic.Value // *vsp

	connected atomic.Bool

	subsidyCache *blockchain.SubsidyCache

	ticketBuyer struct {
		running            atomic.Bool
		remaining          atomic.Int32
		unconfirmedTickets map[chainhash.Hash]struct{}
	}

	// Embedding wallets can set cycleMixer, which will be triggered after
	// new block are seen.
	cycleMixer func()
	mixing     atomic.Bool

	pendingTxsMtx sync.RWMutex
	pendingTxs    map[chainhash.Hash]*btc.ExtendedWalletTx

	receiveTxLastQuery atomic.Uint64

	txHistoryDB atomic.Value // *btc.BadgerTxDB
	// netTxDB is the master transaction database handle supplied by Core,
	// scoped to this wallet's network. It backs txHistoryDB.
	netTxDB          txdb.NetworkTxDB
	syncingTxHistory atomic.Bool

	previouslySynced atomic.Bool

	rescan struct {
		sync.RWMutex
		progress *rescanProgress // nil = no rescan in progress
	}

	politeiaMtx     sync.RWMutex
	politeia        *pi.Politeia
	politeiaSyncing atomic.Bool
	politeiaCancel  context.CancelFunc
	politeiaDone    chan struct{} // closed when politeia goroutine exits
}

func (dcr *ExchangeWallet) config() *exchangeWalletConfig {
	return dcr.cfgV.Load()
}

// Check that ExchangeWallet satisfies the Wallet interface.
var _ asset.Wallet = (*ExchangeWallet)(nil)
var _ asset.Withdrawer = (*ExchangeWallet)(nil)
var _ asset.LiveReconfigurer = (*ExchangeWallet)(nil)
var _ asset.TxFeeEstimator = (*ExchangeWallet)(nil)
var _ asset.Authenticator = (*ExchangeWallet)(nil)
var _ asset.TicketBuyer = (*ExchangeWallet)(nil)
var _ asset.NewAddresser = (*ExchangeWallet)(nil)
var _ asset.GeocodeRedeemer = (*ExchangeWallet)(nil)
var _ asset.TxAbandoner = (*ExchangeWallet)(nil)
var _ asset.Multisigner = (*ExchangeWallet)(nil)
var _ asset.PoliteiaVoter = (*ExchangeWallet)(nil)

type block struct {
	height int64
	hash   *chainhash.Hash
}

// findRedemptionReq represents a request to find a contract's redemption,
// which is added to the findRedemptionQueue with the contract outpoint as
// key.
type findRedemptionReq struct {
	ctx                     context.Context
	contractP2SHScript      []byte
	contractOutputScriptVer uint16
	resultChan              chan *findRedemptionResult
}

func (frr *findRedemptionReq) canceled() bool {
	return frr.ctx.Err() != nil
}

// findRedemptionResult models the result of a find redemption attempt.
type findRedemptionResult struct {
	RedemptionCoinID util.Bytes
	Secret           util.Bytes
	Err              error
}

// NewWallet is the exported constructor by which the DEX will import the
// exchange wallet.
func NewWallet(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	// loadConfig will set fields if defaults are used and set the chainParams
	// variable.
	walletCfg := new(walletConfig)
	chainParams, err := loadConfig(cfg.Settings, network, walletCfg)
	if err != nil {
		return nil, err
	}

	dcr, err := unconnectedWallet(cfg, walletCfg, chainParams, logger, network)
	if err != nil {
		return nil, err
	}

	var w asset.Wallet = dcr

	switch cfg.Type {
	case walletTypeDcrwRPC, walletTypeLegacy:
		dcr.wallet, err = newRPCWallet(cfg.Settings, logger, network)
		if err != nil {
			return nil, err
		}
	case walletTypeSPV:
		dcr.wallet, err = openSPVWallet(cfg.DataDir, walletCfg.GapLimit, chainParams, logger)
		if err != nil {
			return nil, err
		}
		w, err = initNativeWallet(dcr)
		if err != nil {
			return nil, err
		}
	default:
		makeCustomWallet, ok := customWalletConstructors[cfg.Type]
		if !ok {
			return nil, fmt.Errorf("unknown wallet type %q", cfg.Type)
		}

		// Create custom wallet and return early if we encounter any error.
		dcr.wallet, err = makeCustomWallet(cfg.Settings, chainParams, logger)
		if err != nil {
			return nil, fmt.Errorf("custom wallet setup error: %v", err)
		}
	}

	return w, nil
}

func getExchangeWalletCfg(dcrCfg *walletConfig, logger util.Logger) (*exchangeWalletConfig, error) {
	// If set in the user config, the fallback fee will be in units of DCR/kB.
	// Convert to atoms/B.
	fallbackFeesPerByte := toAtoms(dcrCfg.FallbackFeeRate / 1000)
	if fallbackFeesPerByte == 0 {
		fallbackFeesPerByte = defaultFee
	}
	logger.Tracef("Fallback fees set at %d atoms/byte", fallbackFeesPerByte)

	// If set in the user config, the fee rate limit will be in units of DCR/KB.
	// Convert to atoms/byte & error if value is smaller than smallest unit.
	feesLimitPerByte := uint64(defaultFeeRateLimit)
	if dcrCfg.FeeRateLimit > 0 {
		feesLimitPerByte = toAtoms(dcrCfg.FeeRateLimit / 1000)
		if feesLimitPerByte == 0 {
			return nil, fmt.Errorf("fee rate limit is smaller than smallest unit: %v",
				dcrCfg.FeeRateLimit)
		}
	}
	logger.Tracef("Fees rate limit set at %d atoms/byte", feesLimitPerByte)

	redeemConfTarget := dcrCfg.RedeemConfTarget
	if redeemConfTarget == 0 {
		redeemConfTarget = defaultRedeemConfTarget
	}
	logger.Tracef("Redeem conf target set to %d blocks", redeemConfTarget)

	return &exchangeWalletConfig{
		fallbackFeeRate:  fallbackFeesPerByte,
		feeRateLimit:     feesLimitPerByte,
		redeemConfTarget: redeemConfTarget,
		useSplitTx:       dcrCfg.UseSplitTx,
		apiFeeFallback:   dcrCfg.ApiFeeFallback,
		politeiaEnabled:  dcrCfg.PoliteiaEnabled,
	}, nil
}

// unconnectedWallet returns an ExchangeWallet without a base wallet. The wallet
// should be set before use.
func unconnectedWallet(cfg *asset.WalletConfig, dcrCfg *walletConfig, chainParams *chaincfg.Params, logger util.Logger, network util.Network) (*ExchangeWallet, error) {
	walletCfg, err := getExchangeWalletCfg(dcrCfg, logger)
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(cfg.DataDir, chainParams.Name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("unable to create wallet dir: %v", err)
	}

	vspFilepath := filepath.Join(dir, vspFileName)

	w := &ExchangeWallet{
		log:                 logger,
		chainParams:         chainParams,
		network:             network,
		emit:                cfg.Emit,
		peersChange:         cfg.PeersChange,
		fundingCoins:        make(map[outPoint]*fundingCoin),
		findRedemptionQueue: make(map[outPoint]*findRedemptionReq),
		oracleFees:          make(map[uint64]feeStamped),
		vspFilepath:         vspFilepath,
		walletType:          cfg.Type,
		subsidyCache:        blockchain.NewSubsidyCache(chainParams),
		pendingTxs:          make(map[chainhash.Hash]*btc.ExtendedWalletTx),
		walletDir:           dir,
		netTxDB:             cfg.NetworkTxDB,
	}

	if b, err := os.ReadFile(vspFilepath); err == nil {
		var v vsp
		err = json.Unmarshal(b, &v)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshal vsp file: %v", err)
		}
		w.vspV.Store(&v)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("unable to read vsp file: %v", err)
	}

	w.cfgV.Store(walletCfg)

	return w, nil
}

// openSPVWallet opens the previously created native SPV wallet.
func openSPVWallet(dataDir string, gapLimit uint32, chainParams *chaincfg.Params, log util.Logger) (*spvWallet, error) {
	dir := filepath.Join(dataDir, chainParams.Name, "spv")
	if exists, err := walletExists(dir); err != nil {
		return nil, err
	} else if !exists {
		return nil, fmt.Errorf("wallet at %q doesn't exists", dir)
	}

	return &spvWallet{
		dir:         dir,
		chainParams: chainParams,
		log:         log.SubLogger("SPV"),
		blockCache: blockCache{
			blocks: make(map[chainhash.Hash]*cachedBlock),
		},
		tipChan:  make(chan *block, 16),
		gapLimit: gapLimit,
	}, nil
}

// Info returns basic information about the wallet and asset.
func (dcr *ExchangeWallet) Info() *asset.WalletInfo {
	return WalletInfo
}

// var logup uint32

// func rpclog(log util.Logger) {
// 	if atomic.CompareAndSwapUint32(&logup, 0, 1) {
// 		rpcclient.UseLogger(log)
// 	}
// }

func (dcr *ExchangeWallet) startTxHistoryDB(ctx context.Context) error {
	// The transaction database is the master tx DB handle supplied by Core,
	// scoped to this wallet's network. Core owns its lifecycle, so there is no
	// per-wallet database to open or close here.
	if dcr.netTxDB == nil {
		return nil
	}

	db, err := btc.NewBadgerTxDB(dcr.netTxDB, dcr.log)
	if err != nil {
		return err
	}
	dcr.txHistoryDB.Store(db)

	pendingTxs, err := db.GetPendingTxs()
	if err != nil {
		return fmt.Errorf("failed to load unconfirmed txs: %v", err)
	}

	dcr.pendingTxsMtx.Lock()
	for _, tx := range pendingTxs {
		txHash, err := chainhash.NewHashFromStr(tx.ID)
		if err != nil {
			dcr.log.Errorf("Invalid txid %v from tx history db: %v", tx.ID, err)
			continue
		}
		dcr.pendingTxs[*txHash] = tx
	}
	dcr.pendingTxsMtx.Unlock()

	lastQuery, err := db.GetLastReceiveTxQuery()
	if errors.Is(err, btc.ErrNeverQueried) {
		lastQuery = 0
	} else if err != nil {
		return fmt.Errorf("failed to load last query time: %v", err)
	}

	dcr.receiveTxLastQuery.Store(lastQuery)

	return nil
}

// Connect connects the wallet to the RPC server. Satisfies the util.Connector
// interface.
func (dcr *ExchangeWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	// rpclog(dcr.log)
	dcr.ctx = ctx

	err := dcr.wallet.Connect(ctx)
	if err != nil {
		return nil, err
	}

	// The wallet is connected now, so if any of the following checks
	// fails and we return with a non-nil error, we must disconnect the
	// wallet.
	// This is especially important as the wallet may be using an rpc
	// connection which was established above and if we do not disconnect,
	// subsequent reconnect attempts will be met with "websocket client
	// has already connected".
	var success bool
	defer func() {
		if !success {
			dcr.wallet.Disconnect()
		}
	}()

	// Validate accounts early on to prevent errors later.
	for _, acct := range dcr.allAccounts() {
		if acct == "" {
			continue
		}
		_, err = dcr.wallet.AccountUnlocked(ctx, acct)
		if err != nil {
			return nil, fmt.Errorf("unexpected AccountUnlocked error for %q account: %w", acct, err)
		}
	}

	// Initialize the best block.
	tip, err := dcr.getBestBlock(ctx)
	if err != nil {
		return nil, fmt.Errorf("error initializing best block for DCR: %w", err)
	}
	dcr.currentTip.Store(tip)
	dcr.startingBlocks.Store(uint64(tip.height))

	if err := dcr.startTxHistoryDB(ctx); err != nil {
		return nil, err
	}

	success = true // All good, don't disconnect the wallet when this method returns.
	dcr.connected.Store(true)

	dcr.wg.Add(1)
	go func() {
		defer dcr.wg.Done()
		dcr.monitorBlocks(ctx)
		dcr.shutdown()
	}()

	dcr.wg.Add(1)
	go func() {
		defer dcr.wg.Done()
		dcr.monitorPeers(ctx)
	}()

	dcr.wg.Add(1)
	go func() {
		defer dcr.wg.Done()
		dcr.syncTxHistory(ctx, uint64(tip.height))
	}()

	if dcr.config().politeiaEnabled {
		dcr.startPoliteia(ctx)
	}

	return &dcr.wg, nil
}

// Reconfigure attempts to reconfigure the wallet.
func (dcr *ExchangeWallet) Reconfigure(ctx context.Context, cfg *asset.WalletConfig, currentAddress string) (restart bool, err error) {
	dcrCfg := new(walletConfig)
	_, err = loadConfig(cfg.Settings, dcr.network, dcrCfg)
	if err != nil {
		return false, err
	}

	restart, err = dcr.wallet.Reconfigure(ctx, cfg, dcr.network, currentAddress)
	if err != nil || restart {
		return restart, err
	}

	oldCfg := dcr.config()
	exchangeWalletCfg, err := getExchangeWalletCfg(dcrCfg, dcr.log)
	if err != nil {
		return false, err
	}
	dcr.cfgV.Store(exchangeWalletCfg)

	// Handle Politeia toggle. startPoliteia stops any existing instance first,
	// so this is safe even for rapid reconfigurations.
	if exchangeWalletCfg.politeiaEnabled {
		dcr.startPoliteia(ctx)
	} else if oldCfg.politeiaEnabled {
		dcr.stopPoliteia()
	}

	return false, nil
}

// depositAccount returns the account that may be used to receive funds into
// the wallet, either by a direct deposit action or via redemption or refund.
func (dcr *ExchangeWallet) depositAccount() string {
	accts := dcr.wallet.Accounts()
	if accts.UnmixedAccount != "" {
		return accts.UnmixedAccount
	}
	return accts.PrimaryAccount
}

func (dcr *ExchangeWallet) allAccounts() []string {
	accts := dcr.wallet.Accounts()
	if accts.UnmixedAccount == "" {
		return []string{accts.PrimaryAccount}
	}
	return []string{accts.PrimaryAccount, accts.TradingAccount, accts.UnmixedAccount}
}

// OwnsDepositAddress indicates if the provided address can be used to deposit
// funds into the wallet.
func (dcr *ExchangeWallet) OwnsDepositAddress(address string) (bool, error) {
	addr, err := stdaddr.DecodeAddress(address, dcr.chainParams)
	if err != nil {
		return false, err
	}
	return dcr.wallet.AccountOwnsAddress(dcr.ctx, addr, dcr.depositAccount())
}

func (dcr *ExchangeWallet) balance() (*asset.Balance, error) {
	accts := dcr.wallet.Accounts()

	locked, err := dcr.lockedAtoms(accts.PrimaryAccount)
	if err != nil {
		return nil, err
	}
	ab, err := dcr.wallet.AccountBalance(dcr.ctx, 0, accts.PrimaryAccount)
	if err != nil {
		return nil, err
	}
	bal := &asset.Balance{
		Available: toAtoms(ab.Spendable) - locked,
		Immature: toAtoms(ab.ImmatureCoinbaseRewards) +
			toAtoms(ab.ImmatureStakeGeneration),
		Locked: locked + toAtoms(ab.LockedByTickets),
		Other:  make(map[asset.BalanceCategory]asset.CustomBalance),
	}

	bal.Other[asset.BalanceCategoryStaked] = asset.CustomBalance{
		Amount: toAtoms(ab.LockedByTickets),
	}

	if accts.UnmixedAccount == "" {
		// Mixing is disabled, but if the mixed or trading accounts exist
		// and have funds (e.g. from a previous mixing session), include
		// them as locked so the user can see they exist.
		for _, acctName := range []string{mixedAccountName, tradingAccountName} {
			acctBal, err := dcr.wallet.AccountBalance(dcr.ctx, 0, acctName)
			if err != nil {
				continue // account may not exist
			}
			total := toAtoms(acctBal.Total)
			if total > 0 {
				bal.Locked += total
			}
			staked := toAtoms(acctBal.LockedByTickets)
			if staked > 0 {
				bal.Other[asset.BalanceCategoryStaked] = asset.CustomBalance{
					Amount: bal.Other[asset.BalanceCategoryStaked].Amount + staked,
				}
			}
		}
		return bal, nil
	}

	// Mixing is enabled, consider ...
	// 1) trading account spendable (-locked) as available,
	// 2) all unmixed funds as immature, and
	// 3) all locked utxos in the trading account as locked (for swapping).
	tradingAcctBal, err := dcr.wallet.AccountBalance(dcr.ctx, 0, accts.TradingAccount)
	if err != nil {
		return nil, err
	}
	tradingAcctLocked, err := dcr.lockedAtoms(accts.TradingAccount)
	if err != nil {
		return nil, err
	}
	unmixedAcctBal, err := dcr.wallet.AccountBalance(dcr.ctx, 0, accts.UnmixedAccount)
	if err != nil {
		return nil, err
	}

	bal.Available += toAtoms(tradingAcctBal.Spendable) - tradingAcctLocked
	bal.Immature += toAtoms(unmixedAcctBal.Total)
	bal.Locked += tradingAcctLocked

	bal.Other[asset.BalanceCategoryUnmixed] = asset.CustomBalance{
		Amount: toAtoms(unmixedAcctBal.Total),
	}

	return bal, nil
}

// Balance should return the total available funds in the wallet.
func (dcr *ExchangeWallet) Balance() (*asset.Balance, error) {
	return dcr.balance()
}

// FeeRate satisfies asset.Wallet.
func (dcr *ExchangeWallet) FeeRate() uint64 {
	const confTarget = 2 // 1 historically gives crazy rates
	rate, err := dcr.feeRate(confTarget)
	if err != nil {
		dcr.log.Debugf("feeRate error: %v", err)
		return 0
	}
	return rate
}

// feeRate returns the current optimal fee rate in atoms / byte.
func (dcr *ExchangeWallet) feeRate(confTarget uint64) (uint64, error) {
	if !dcr.connected.Load() {
		return 0, errors.New("not connected")
	}
	if feeEstimator, is := dcr.wallet.(FeeRateEstimator); is && !dcr.wallet.SpvMode() {
		dcrPerKB, err := feeEstimator.EstimateSmartFeeRate(dcr.ctx, int64(confTarget), chainjson.EstimateSmartFeeConservative)
		if err == nil && dcrPerKB > 0 {
			return dcrPerKBToAtomsPerByte(dcrPerKB)
		}
		if err != nil {
			dcr.log.Warnf("Failed to get local fee rate estimate: %v", err)
		} else { // dcrPerKB == 0
			dcr.log.Warnf("Local fee rate estimate is zero.")
		}
	}

	cfg := dcr.config()

	// Either SPV wallet or EstimateSmartFeeRate failed.
	if !cfg.apiFeeFallback {
		return 0, fmt.Errorf("fee rate estimation unavailable and external API is disabled")
	}

	now := time.Now()

	dcr.oracleFeesMtx.Lock()
	defer dcr.oracleFeesMtx.Unlock()
	oracleFee := dcr.oracleFees[confTarget]
	if now.Sub(oracleFee.stamp) < freshFeeAge {
		return oracleFee.rate, nil
	}
	if dcr.oracleFailing {
		return 0, errors.New("fee rate oracle is in a temporary failing state")
	}

	dcr.log.Tracef("Retrieving fee rate from external fee oracle for %d target blocks", confTarget)
	dcrPerKB, err := fetchFeeFromOracle(dcr.ctx, dcr.network, confTarget)
	if err != nil {
		// Just log it and return zero. If we return an error, it's just logged
		// anyway, and we want to meter these logs.
		dcr.log.Meter("feeRate.fetch.fail", time.Hour).Errorf("external fee rate API failure: %v", err)
		// Flag the oracle as failing so subsequent requests don't also try and
		// fail after the request timeout. Remove the flag after a bit.
		dcr.oracleFailing = true
		time.AfterFunc(freshFeeAge, func() {
			dcr.oracleFeesMtx.Lock()
			dcr.oracleFailing = false
			dcr.oracleFeesMtx.Unlock()
		})
		return 0, nil
	}
	if dcrPerKB <= 0 {
		return 0, fmt.Errorf("invalid fee rate %f from fee oracle", dcrPerKB)
	}
	// Convert to atoms/B and error if it is greater than fee rate limit.
	atomsPerByte, err := dcrPerKBToAtomsPerByte(dcrPerKB)
	if err != nil {
		return 0, err
	}
	if atomsPerByte > cfg.feeRateLimit {
		return 0, fmt.Errorf("fee rate from external API greater than fee rate limit: %v > %v",
			atomsPerByte, cfg.feeRateLimit)
	}
	dcr.oracleFees[confTarget] = feeStamped{atomsPerByte, now}
	return atomsPerByte, nil
}

// dcrPerKBToAtomsPerByte converts a estimated feeRate from dcr/KB to atoms/B.
func dcrPerKBToAtomsPerByte(dcrPerkB float64) (uint64, error) {
	// The caller should check for non-positive numbers, but don't allow
	// underflow when converting to an unsigned integer.
	if dcrPerkB < 0 {
		return 0, fmt.Errorf("negative fee rate")
	}
	// dcrPerkB * 1e8 / 1e3 => atomsPerB
	atomsPerKB, err := dcrutil.NewAmount(dcrPerkB)
	if err != nil {
		return 0, err
	}
	return uint64(util.IntDivUp(int64(atomsPerKB), 1000)), nil
}

// fetchFeeFromOracle gets the fee rate from the external API.
func fetchFeeFromOracle(ctx context.Context, net util.Network, nb uint64) (float64, error) {
	var uri string
	if net == util.Testnet {
		uri = fmt.Sprintf("https://testnet.decred.org/insight/api/utils/estimatefee?nbBlocks=%d", nb)
	} else { // mainnet and simnet
		uri = fmt.Sprintf("https://dcrdata.decred.org/insight/api/utils/estimatefee?nbBlocks=%d", nb)
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var resp map[uint64]float64
	if err := dexnet.Get(ctx, uri, &resp); err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, errors.New("null response")
	}
	dcrPerKB, ok := resp[nb]
	if !ok {
		return 0, errors.New("no fee rate for requested number of blocks")
	}
	return dcrPerKB, nil
}

// targetFeeRateWithFallback attempts to get a fresh fee rate for the target
// number of confirmations, but falls back to the suggestion or fallbackFeeRate
// via feeRateWithFallback.
func (dcr *ExchangeWallet) targetFeeRateWithFallback(confTarget, feeSuggestion uint64) uint64 {
	feeRate, err := dcr.feeRate(confTarget)
	if err != nil {
		dcr.log.Debugf("Failed to get fee rate: %v", err)
	} else if feeRate != 0 {
		dcr.log.Tracef("Obtained estimate for %d-conf fee rate, %d", confTarget, feeRate)
		return feeRate
	}

	return dcr.feeRateWithFallback(feeSuggestion)
}

// feeRateWithFallback filters the suggested fee rate by ensuring it is within
// limits. If not, the configured fallbackFeeRate is returned and a warning
// logged.
func (dcr *ExchangeWallet) feeRateWithFallback(feeSuggestion uint64) uint64 {
	cfg := dcr.config()

	if feeSuggestion > 0 && feeSuggestion < cfg.feeRateLimit {
		dcr.log.Tracef("Using caller's suggestion for fee rate, %d", feeSuggestion)
		return feeSuggestion
	}
	dcr.log.Warnf("No usable fee rate suggestion. Using fallback of %d", cfg.fallbackFeeRate)
	return cfg.fallbackFeeRate
}

type amount uint64

func (a amount) String() string {
	return strconv.FormatFloat(dcrutil.Amount(a).ToCoin(), 'f', -1, 64) // dec, but no trailing zeros
}

// fundInternalWithUTXOs finds coins from a set of UTXOs for a specified value. This method
// is the same as "fund", except the UTXOs must be passed in, and fundingMtx
// must be held by the caller.
func (dcr *ExchangeWallet) fundInternalWithUTXOs(utxos []*compositeUTXO, keep uint64, // leave utxos for this reserve amt
	enough func(sum uint64, size uint32, unspent *compositeUTXO) (bool, uint64), lock bool) (
	coins asset.Coins, redeemScripts []util.Bytes, spents []*fundingCoin, sum, size uint64, err error) {
	avail := sumUTXOs(utxos)
	if keep > avail { // skip utxo selection if we can't possibly make reserves
		return nil, nil, nil, 0, 0, asset.ErrInsufficientBalance
	}

	var sz uint32

	// First take some UTXOs out of the mix for any keep amount. Select these
	// with the objective of being as close to the amount as possible, unlike
	// tryFund that minimizes the number of UTXOs chosen. By doing this first,
	// we may be making the order spend a larger number of UTXOs, but we
	// mitigate subsequent order funding failure due to reserves because we know
	// this order will leave behind sufficient UTXOs without relying on change.
	if keep > 0 {
		kept := leastOverFund(reserveEnough(keep), utxos)
		dcr.log.Debugf("Setting aside %v DCR in %d UTXOs to respect the %v DCR reserved amount",
			toDCR(sumUTXOs(kept)), len(kept), toDCR(keep))
		utxosPruned := utxoSetDiff(utxos, kept)
		sum, _, sz, coins, spents, redeemScripts, err = tryFund(utxosPruned, enough)
		if err != nil { // try with the full set
			dcr.log.Debugf("Unable to fund order with UTXOs set aside (%v), trying again with full UTXO set.", err)
		} // else spents is populated
	}
	if len(spents) == 0 { // either keep is zero or it failed with utxosPruned
		// Without utxos set aside for keep, we have to consider any spendable
		// change (extra) that the enough func grants us.
		var extra uint64
		sum, extra, sz, coins, spents, redeemScripts, err = tryFund(utxos, enough)
		if err != nil {
			return nil, nil, nil, 0, 0, err
		}
		if avail-sum+extra < keep {
			return nil, nil, nil, 0, 0, asset.ErrInsufficientBalance
		}
		// else we got lucky with the legacy funding approach and there was
		// either available unspent or the enough func granted spendable change.
		if keep > 0 && extra > 0 {
			dcr.log.Debugf("Funding succeeded with %f DCR in spendable change.", toDCR(extra))
		}
	}

	if lock {
		err = dcr.lockFundingCoins(spents)
		if err != nil {
			return nil, nil, nil, 0, 0, err
		}
	}
	return coins, redeemScripts, spents, sum, uint64(sz), nil
}

// fund finds coins for the specified value. A function is provided that can
// check whether adding the provided output would be enough to satisfy the
// needed value. Preference is given to selecting coins with 1 or more confs,
// falling back to 0-conf coins where there are not enough 1+ confs coins. If
// change should not be considered "kept" (e.g. no preceding split txn, or
// mixing sends change to umixed account where it is unusable for reserves),
// caller should return 0 extra from enough func.
func (dcr *ExchangeWallet) fund(keep uint64, // leave utxos for this reserve amt
	enough func(sum uint64, size uint32, unspent *compositeUTXO) (bool, uint64)) (
	coins asset.Coins, redeemScripts []util.Bytes, sum, size uint64, err error) {

	// Keep a consistent view of spendable and locked coins in the wallet and
	// the fundingCoins map to make this safe for concurrent use.
	dcr.fundingMtx.Lock()         // before listing unspents in wallet
	defer dcr.fundingMtx.Unlock() // hold until lockFundingCoins (wallet and map)

	utxos, err := dcr.spendableUTXOs()
	if err != nil {
		return nil, nil, 0, 0, err
	}

	coins, redeemScripts, _, sum, size, err = dcr.fundInternalWithUTXOs(utxos, keep, enough, true)
	return coins, redeemScripts, sum, size, err
}

// spendableUTXOs generates a slice of spendable *compositeUTXO.
func (dcr *ExchangeWallet) spendableUTXOs() ([]*compositeUTXO, error) {
	accts := dcr.wallet.Accounts()
	unspents, err := dcr.wallet.Unspents(dcr.ctx, accts.PrimaryAccount)
	if err != nil {
		return nil, err
	}
	if accts.TradingAccount != "" {
		// Trading account may contain spendable utxos such as unspent split tx
		// outputs that are unlocked/returned. TODO: Care should probably be
		// taken to ensure only unspent split tx outputs are selected and other
		// unmixed outputs in the trading account are ignored.
		tradingAcctSpendables, err := dcr.wallet.Unspents(dcr.ctx, accts.TradingAccount)
		if err != nil {
			return nil, err
		}
		unspents = append(unspents, tradingAcctSpendables...)
	}
	if len(unspents) == 0 {
		return nil, fmt.Errorf("insufficient funds. 0 DCR available to spend in account %q", accts.PrimaryAccount)
	}

	// Parse utxos to include script size for spending input. Returned utxos
	// will be sorted in ascending order by amount (smallest first).
	utxos, err := dcr.parseUTXOs(unspents)
	if err != nil {
		return nil, fmt.Errorf("error parsing unspent outputs: %w", err)
	}
	if len(utxos) == 0 {
		return nil, fmt.Errorf("no funds available")
	}
	return utxos, nil
}

// tryFund attempts to use the provided UTXO set to satisfy the enough function
// with the fewest number of inputs. The selected utxos are not locked. If the
// requirement can be satisfied without 0-conf utxos, that set will be selected
// regardless of whether the 0-conf inclusive case would be cheaper. The
// provided UTXOs must be sorted in ascending order by value.
func tryFund(utxos []*compositeUTXO,
	enough func(sum uint64, size uint32, unspent *compositeUTXO) (bool, uint64)) (
	sum, extra uint64, size uint32, coins asset.Coins, spents []*fundingCoin, redeemScripts []util.Bytes, err error) {

	addUTXO := func(unspent *compositeUTXO) error {
		txHash, err := chainhash.NewHashFromStr(unspent.rpc.TxID)
		if err != nil {
			return fmt.Errorf("error decoding txid: %w", err)
		}
		v := toAtoms(unspent.rpc.Amount)
		redeemScript, err := hex.DecodeString(unspent.rpc.RedeemScript)
		if err != nil {
			return fmt.Errorf("error decoding redeem script for %s, script = %s: %w",
				unspent.rpc.TxID, unspent.rpc.RedeemScript, err)
		}
		op := newOutput(txHash, unspent.rpc.Vout, v, unspent.rpc.Tree)
		coins = append(coins, op)
		spents = append(spents, &fundingCoin{
			op:   op,
			addr: unspent.rpc.Address,
		})
		redeemScripts = append(redeemScripts, redeemScript)
		size += unspent.input.Size()
		sum += v
		return nil
	}

	isEnoughWith := func(utxo *compositeUTXO) bool {
		ok, _ := enough(sum, size, utxo)
		return ok
	}

	tryUTXOs := func(minconf int64) (ok bool, err error) {
		sum, size = 0, 0 // size is only sum of inputs size, not including tx overhead or outputs
		coins, spents, redeemScripts = nil, nil, nil

		okUTXOs := make([]*compositeUTXO, 0, len(utxos)) // over-allocate
		for _, cu := range utxos {
			if cu.confs >= minconf && cu.rpc.Spendable {
				okUTXOs = append(okUTXOs, cu)
			}
		}

		for {
			// If there are none left, we don't have enough.
			if len(okUTXOs) == 0 {
				return false, nil
			}

			// Check if the largest output is too small.
			lastUTXO := okUTXOs[len(okUTXOs)-1]
			if !isEnoughWith(lastUTXO) {
				if err = addUTXO(lastUTXO); err != nil {
					return false, err
				}
				okUTXOs = okUTXOs[0 : len(okUTXOs)-1]
				continue
			}

			// We only need one then. Find it.
			idx := sort.Search(len(okUTXOs), func(i int) bool {
				return isEnoughWith(okUTXOs[i])
			})
			// No need to check idx == n. We already verified that the last
			// utxo passes above.
			final := okUTXOs[idx]
			_, extra = enough(sum, size, final) // sort.Search might not have called isEnough for this utxo last
			if err = addUTXO(final); err != nil {
				return false, err
			}
			return true, nil
		}
	}

	// First try with confs>0.
	ok, err := tryUTXOs(1)
	if err != nil {
		return 0, 0, 0, nil, nil, nil, err
	}

	// Fallback to allowing 0-conf outputs.
	if !ok {
		ok, err = tryUTXOs(0)
		if err != nil {
			return 0, 0, 0, nil, nil, nil, err
		}
		if !ok {
			return 0, 0, 0, nil, nil, nil, fmt.Errorf("not enough to cover requested funds. "+
				"%s DCR available in %d UTXOs (%w)", amount(sum), len(coins), asset.ErrInsufficientBalance)
		}
	}

	return
}

// lockFundingCoins locks the funding coins via RPC and stores them in the map.
// This function is not safe for concurrent use. The caller should lock
// dcr.fundingMtx.
func (dcr *ExchangeWallet) lockFundingCoins(fCoins []*fundingCoin) error {
	wireOPs := make([]*wire.OutPoint, 0, len(fCoins))
	for _, c := range fCoins {
		wireOPs = append(wireOPs, wire.NewOutPoint(c.op.txHash(), c.op.vout(), c.op.tree))
	}
	err := dcr.wallet.LockUnspent(dcr.ctx, false, wireOPs)
	if err != nil {
		return err
	}
	for _, c := range fCoins {
		dcr.fundingCoins[c.op.pt] = c
	}
	return nil
}

// returnCoins unlocks coins and removes them from the fundingCoins map.
// Requires fundingMtx to be write-locked.
func (dcr *ExchangeWallet) returnCoins(unspents asset.Coins) ([]*fundingCoin, error) {
	if len(unspents) == 0 {
		return nil, fmt.Errorf("cannot return zero coins")
	}

	ops := make([]*wire.OutPoint, 0, len(unspents))
	fundingCoins := make([]*fundingCoin, 0, len(unspents))

	dcr.log.Debugf("returning coins %s", unspents)
	for _, unspent := range unspents {
		op, err := dcr.convertCoin(unspent)
		if err != nil {
			return nil, fmt.Errorf("error converting coin: %w", err)
		}
		ops = append(ops, op.wireOutPoint()) // op.tree may be wire.TxTreeUnknown, but that's fine since wallet.LockUnspent doesn't rely on it
		if fCoin, ok := dcr.fundingCoins[op.pt]; ok {
			fundingCoins = append(fundingCoins, fCoin)
		} else {
			dcr.log.Warnf("returning coin %s that is not cached as a funding coin", op)
			fundingCoins = append(fundingCoins, &fundingCoin{op: op})
		}
	}

	if err := dcr.wallet.LockUnspent(dcr.ctx, true, ops); err != nil {
		return nil, err
	}

	for _, fCoin := range fundingCoins {
		delete(dcr.fundingCoins, fCoin.op.pt)
	}

	return fundingCoins, nil
}

// lookupTxOutput attempts to find and return details for the specified output,
// first checking for an unspent output and if not found, checking wallet txs.
// Returns asset.CoinNotFoundError if the output is not found.
//
// NOTE: This method is only guaranteed to return results for outputs belonging
// to transactions that are tracked by the wallet, although full node wallets
// are able to look up non-wallet outputs that are unspent.
//
// If the value of the spent flag is -1, it could not be determined with the SPV
// wallet if it is spent, and the caller should perform a block filters scan to
// locate a (mined) spending transaction if needed.
func (dcr *ExchangeWallet) lookupTxOutput(ctx context.Context, txHash *chainhash.Hash, vout uint32) (txOut *wire.TxOut, confs uint32, spent int8, err error) {
	// Check for an unspent output.
	output, err := dcr.wallet.UnspentOutput(ctx, txHash, vout, wire.TxTreeUnknown)
	if err == nil {
		return output.TxOut, output.Confirmations, 0, nil
	} else if !errors.Is(err, asset.CoinNotFoundError) {
		return nil, 0, 0, err
	}

	// Check wallet transactions.
	tx, err := dcr.wallet.GetTransaction(ctx, txHash)
	if err != nil {
		return nil, 0, 0, err // asset.CoinNotFoundError if not found
	}
	if int(vout) >= len(tx.MsgTx.TxOut) {
		return nil, 0, 0, fmt.Errorf("tx %s has no output at %d", txHash, vout)
	}

	txOut = tx.MsgTx.TxOut[vout]
	confs = uint32(tx.Confirmations)

	// We have the requested output. Check if it is spent.
	if confs == 0 {
		// Only counts as spent if spent in a mined transaction,
		// unconfirmed tx outputs can't be spent in a mined tx.

		// There is a dcrwallet bug by which the user can shut down at the wrong
		// time and a tx will never be marked as confirmed. We'll force a
		// cfilter scan for unconfirmed txs until the bug is resolved.
		// https://github.com/decred/dcrdex/pull/2444
		if dcr.wallet.SpvMode() {
			return txOut, confs, -1, nil
		}
		return txOut, confs, 0, nil
	}

	if !dcr.wallet.SpvMode() {
		// A mined output that is not found by wallet.UnspentOutput
		// is spent if the wallet is connected to a full node.
		dcr.log.Debugf("Output %s:%d that was not reported as unspent is considered SPENT, spv mode = false.",
			txHash, vout)
		return txOut, confs, 1, nil
	}

	// For SPV wallets, only consider the output spent if it pays to the wallet
	// because outputs that don't pay to the wallet may be unspent but still not
	// found by wallet.UnspentOutput. NOTE: Swap contracts never pay to wallet
	// (p2sh with no imported redeem script), so this is not an expected outcome
	// for swap contract outputs!
	//
	// for _, details := range tx.Details {
	// 	if details.Vout == vout && details.Category == wallet.CreditReceive.String() {
	// 		dcr.log.Tracef("Output %s:%d was not reported as unspent, pays to the wallet and is considered SPENT.",
	// 			txHash, vout)
	// 		return txOut, confs, 1, nil
	// 	}
	// }

	// Spend status is unknown.  Caller may scan block filters if needed.
	dcr.log.Tracef("Output %s:%d was not reported as unspent by SPV wallet. Spend status UNKNOWN.",
		txHash, vout)
	return txOut, confs, -1, nil // unknown spend status
}

// LockTimeExpired returns true if the specified locktime has expired, making it
// possible to redeem the locked coins.
func (dcr *ExchangeWallet) LockTimeExpired(ctx context.Context, lockTime time.Time) (bool, error) {
	tip := dcr.cachedBestBlock()
	if tip == nil {
		return false, errors.New("wallet tip not synced")
	}
	hdr, err := dcr.wallet.GetBlockHeader(ctx, tip.hash)
	if err != nil {
		return false, fmt.Errorf("unable to retrieve the block header: %w", err)
	}
	return time.Unix(hdr.MedianTime, 0).After(lockTime), nil
}

// findRedemptionsInMempool attempts to find spending info for the specified
// contracts by searching every input of all txs in the mempool.
// If spending info is found for any contract, the contract is purged from the
// findRedemptionQueue and the contract's secret (if successfully parsed) or any
// error that occurs during parsing is returned to the redemption finder via the
// registered result chan.
func (dcr *ExchangeWallet) findRedemptionsInMempool(contractOutpoints []outPoint) {
	contractsCount := len(contractOutpoints)
	dcr.log.Debugf("finding redemptions for %d contracts in mempool", contractsCount)

	var totalFound, totalCanceled int
	logAbandon := func(reason string) {
		// Do not remove the contracts from the findRedemptionQueue
		// as they could be subsequently redeemed in some mined tx(s),
		// which would be captured when a new tip is reported.
		if totalFound+totalCanceled > 0 {
			dcr.log.Debugf("%d redemptions found, %d canceled out of %d contracts in mempool",
				totalFound, totalCanceled, contractsCount)
		}
		dcr.log.Errorf("abandoning mempool redemption search for %d contracts because of %s",
			contractsCount-totalFound-totalCanceled, reason)
	}

	mempooler, is := dcr.wallet.(Mempooler)
	if !is || dcr.wallet.SpvMode() {
		return
	}

	mempoolTxs, err := mempooler.GetRawMempool(dcr.ctx)
	if err != nil {
		logAbandon(fmt.Sprintf("error retrieving transactions: %v", err))
		return
	}

	for _, txHash := range mempoolTxs {
		tx, err := dcr.wallet.GetTransaction(dcr.ctx, txHash)
		if err != nil {
			logAbandon(fmt.Sprintf("getrawtransaction error for tx hash %v: %v", txHash, err))
			return
		}
		found, canceled := dcr.findRedemptionsInTx("mempool", tx.MsgTx, contractOutpoints)
		totalFound += found
		totalCanceled += canceled
		if totalFound+totalCanceled == contractsCount {
			break
		}
	}

	dcr.log.Debugf("%d redemptions found, %d canceled out of %d contracts in mempool",
		totalFound, totalCanceled, contractsCount)
}

// findRedemptionsInBlockRange attempts to find spending info for the specified
// contracts by checking the cfilters of each block in the provided range for
// likely inclusion of ANY of the specified contracts' P2SH script. If a block's
// cfilters reports possible inclusion of ANY of the contracts' P2SH script,
// all inputs of the matching block's txs are checked to determine if any of the
// inputs spends any of the provided contracts.
// If spending info is found for any contract, the contract is purged from the
// findRedemptionQueue and the contract's secret (if successfully parsed) or any
// error that occurs during parsing is returned to the redemption finder via the
// registered result chan.
// If spending info is not found for any of these contracts after checking the
// specified block range, a mempool search is triggered to attempt finding unmined
// redemptions for the remaining contracts.
// NOTE:
// Any error encountered while checking a block's cfilters or fetching a matching
// block's txs compromises the redemption search for this set of contracts because
// subsequent attempts to find these contracts' redemption will not repeat any
// block in the specified range unless the contracts are first removed from the
// findRedemptionQueue. Thus, any such error will cause this set of contracts to
// be purged from the findRedemptionQueue. The error will be propagated to the
// redemption finder(s) and these may re-call dcr.FindRedemption to restart find
// redemption attempts for any of these contracts.
func (dcr *ExchangeWallet) findRedemptionsInBlockRange(startBlockHeight, endBlockHeight int64, contractOutpoints []outPoint) {
	totalContracts := len(contractOutpoints)
	dcr.log.Debugf("finding redemptions for %d contracts in blocks %d - %d",
		totalContracts, startBlockHeight, endBlockHeight)

	var lastScannedBlockHeight int64
	var totalFound, totalCanceled int

rangeBlocks:
	for blockHeight := startBlockHeight; blockHeight <= endBlockHeight; blockHeight++ {
		// Get the hash for this block.
		blockHash, err := dcr.wallet.GetBlockHash(dcr.ctx, blockHeight)
		if err != nil { // unable to get block hash is a fatal error
			err = fmt.Errorf("unable to get hash for block %d: %w", blockHeight, err)
			dcr.fatalFindRedemptionsError(err, contractOutpoints)
			return
		}

		// Combine the p2sh scripts for all contracts (excluding contracts whose redemption
		// have been found) to check against this block's cfilters.
		dcr.findRedemptionMtx.RLock()
		contractP2SHScripts := make([][]byte, 0)
		for _, contractOutpoint := range contractOutpoints {
			if req, stillInQueue := dcr.findRedemptionQueue[contractOutpoint]; stillInQueue {
				contractP2SHScripts = append(contractP2SHScripts, req.contractP2SHScript)
			}
		}
		dcr.findRedemptionMtx.RUnlock()

		bingo, err := dcr.wallet.MatchAnyScript(dcr.ctx, blockHash, contractP2SHScripts)
		if err != nil { // error retrieving a block's cfilters is a fatal error
			err = fmt.Errorf("MatchAnyScript error for block %d (%s): %w", blockHeight, blockHash, err)
			dcr.fatalFindRedemptionsError(err, contractOutpoints)
			return
		}

		if !bingo {
			lastScannedBlockHeight = blockHeight
			continue // block does not reference any of these contracts, continue to next block
		}

		// Pull the block info to confirm if any of its inputs spends a contract of interest.
		blk, err := dcr.wallet.GetBlock(dcr.ctx, blockHash)
		if err != nil { // error pulling a matching block's transactions is a fatal error
			err = fmt.Errorf("error retrieving transactions for block %d (%s): %w",
				blockHeight, blockHash, err)
			dcr.fatalFindRedemptionsError(err, contractOutpoints)
			return
		}

		lastScannedBlockHeight = blockHeight
		scanPoint := fmt.Sprintf("block %d", blockHeight)
		for _, tx := range append(blk.Transactions, blk.STransactions...) {
			found, canceled := dcr.findRedemptionsInTx(scanPoint, tx, contractOutpoints)
			totalFound += found
			totalCanceled += canceled
			if totalFound+totalCanceled == totalContracts {
				break rangeBlocks
			}
		}
	}

	dcr.log.Debugf("%d redemptions found, %d canceled out of %d contracts in blocks %d to %d",
		totalFound, totalCanceled, totalContracts, startBlockHeight, lastScannedBlockHeight)

	// Search for redemptions in mempool if there are yet unredeemed
	// contracts after searching this block range.
	pendingContractsCount := totalContracts - totalFound - totalCanceled
	if pendingContractsCount > 0 {
		dcr.findRedemptionMtx.RLock()
		pendingContracts := make([]outPoint, 0, pendingContractsCount)
		for _, contractOutpoint := range contractOutpoints {
			if _, pending := dcr.findRedemptionQueue[contractOutpoint]; pending {
				pendingContracts = append(pendingContracts, contractOutpoint)
			}
		}
		dcr.findRedemptionMtx.RUnlock()
		dcr.findRedemptionsInMempool(pendingContracts)
	}
}

// findRedemptionsInTx checks if any input of the passed tx spends any of the
// specified contract outpoints. If spending info is found for any contract, the
// contract's secret or any error encountered while trying to parse the secret
// is returned to the redemption finder via the registered result chan; and the
// contract is purged from the findRedemptionQueue.
// Returns the number of redemptions found and canceled.
func (dcr *ExchangeWallet) findRedemptionsInTx(scanPoint string, tx *wire.MsgTx, contractOutpoints []outPoint) (found, cancelled int) {
	dcr.findRedemptionMtx.Lock()
	defer dcr.findRedemptionMtx.Unlock()

	redeemTxHash := tx.TxHash()

	for _, contractOutpoint := range contractOutpoints {
		req, exists := dcr.findRedemptionQueue[contractOutpoint]
		if !exists {
			continue // no find request for this outpoint (impossible now?)
		}
		if req.canceled() {
			cancelled++
			delete(dcr.findRedemptionQueue, contractOutpoint)
			continue // this find request has been cancelled
		}

		for i, txIn := range tx.TxIn {
			prevOut := &txIn.PreviousOutPoint
			if prevOut.Index != contractOutpoint.vout || prevOut.Hash != contractOutpoint.txHash {
				continue // input doesn't redeem this contract, check next input
			}
			found++

			scriptHash := ExtractScriptHash(req.contractOutputScriptVer, req.contractP2SHScript)
			secret, err := FindKeyPush(req.contractOutputScriptVer, txIn.SignatureScript, scriptHash, dcr.chainParams)
			if err != nil {
				dcr.log.Errorf("Error parsing contract secret for %s from tx input %s:%d in %s: %v",
					contractOutpoint.String(), redeemTxHash, i, scanPoint, err)
				req.resultChan <- &findRedemptionResult{
					Err: err,
				}
			} else {
				dcr.log.Infof("Redemption for contract %s found in tx input %s:%d in %s",
					contractOutpoint.String(), redeemTxHash, i, scanPoint)
				req.resultChan <- &findRedemptionResult{
					RedemptionCoinID: ToCoinID(&redeemTxHash, uint32(i)),
					Secret:           secret,
				}
			}

			delete(dcr.findRedemptionQueue, contractOutpoint)
			break // stop checking inputs for this contract
		}
	}

	return
}

// fatalFindRedemptionsError should be called when an error occurs that prevents
// redemption search for the specified contracts from continuing reliably. The
// error will be propagated to the seeker(s) of these contracts' redemptions via
// the registered result channels and the contracts will be removed from the
// findRedemptionQueue.
func (dcr *ExchangeWallet) fatalFindRedemptionsError(err error, contractOutpoints []outPoint) {
	dcr.findRedemptionMtx.Lock()
	dcr.log.Debugf("stopping redemption search for %d contracts in queue: %v", len(contractOutpoints), err)
	for _, contractOutpoint := range contractOutpoints {
		req, exists := dcr.findRedemptionQueue[contractOutpoint]
		if !exists {
			continue
		}
		req.resultChan <- &findRedemptionResult{
			Err: err,
		}
		delete(dcr.findRedemptionQueue, contractOutpoint)
	}
	dcr.findRedemptionMtx.Unlock()
}

// DepositAddress returns an address for depositing funds into the exchange
// wallet.
func (dcr *ExchangeWallet) DepositAddress() (string, error) {
	acct := dcr.depositAccount()
	addr, err := dcr.wallet.ExternalAddress(dcr.ctx, acct)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

// NewAddress returns a new address from the wallet. This satisfies the
// NewAddresser interface.
func (dcr *ExchangeWallet) NewAddress() (string, error) {
	return dcr.DepositAddress()
}

// AddressUsed checks if a wallet address has been used.
func (dcr *ExchangeWallet) AddressUsed(addrStr string) (bool, error) {
	return dcr.wallet.AddressUsed(dcr.ctx, addrStr)
}

// Unlock unlocks the exchange wallet.
func (dcr *ExchangeWallet) Unlock(pw []byte) error {
	// Older SPV wallet potentially need an upgrade while we have a password.
	acctsToUnlock := dcr.allAccounts()
	if upgrader, is := dcr.wallet.(interface {
		upgradeAccounts(ctx context.Context, pw []byte) error
	}); is {
		if err := upgrader.upgradeAccounts(dcr.ctx, pw); err != nil {
			return fmt.Errorf("error upgrading accounts: %w", err)
		}
		// For the native wallet, we unlock all accounts regardless. Otherwise,
		// the accounts won't be properly unlocked after ConfigureFundsMixer
		// is called. We could consider taking a password for
		// ConfigureFundsMixer OR have Core take the password and call Unlock
		// after ConfigureFundsMixer.
		acctsToUnlock = nativeAccounts
	}

	// We must unlock all accounts, including any unmixed account, which is used
	// to supply keys to the refund path of the swap contract script.
	for _, acct := range acctsToUnlock {
		unlocked, err := dcr.wallet.AccountUnlocked(dcr.ctx, acct)
		if err != nil {
			return err
		}
		if unlocked {
			continue // attempt to unlock the other account
		}

		err = dcr.wallet.UnlockAccount(dcr.ctx, pw, acct)
		if err != nil {
			return err
		}
	}
	return nil
}

// Lock locks the exchange wallet.
func (dcr *ExchangeWallet) Lock() error {
	accts := dcr.wallet.Accounts()
	if accts.UnmixedAccount != "" {
		return fmt.Errorf("cannot lock RPC mixing wallet") // don't lock if mixing is enabled
	}
	return dcr.wallet.LockAccount(dcr.ctx, accts.PrimaryAccount)
}

// Locked will be true if the wallet is currently locked.
// Q: why are we ignoring RPC errors in this?
func (dcr *ExchangeWallet) Locked() bool {
	for _, acct := range dcr.allAccounts() {
		unlocked, err := dcr.wallet.AccountUnlocked(dcr.ctx, acct)
		if err != nil {
			dcr.log.Errorf("error checking account lock status %v", err)
			unlocked = false // assume wallet is unlocked?
		}
		if !unlocked {
			return true // Locked is true if any of the funding accounts is locked.
		}
	}
	return false
}

// SendTransaction broadcasts a valid fully-signed transaction.
func (dcr *ExchangeWallet) SendTransaction(rawTx []byte) ([]byte, error) {
	msgTx, err := msgTxFromBytes(rawTx)
	if err != nil {
		return nil, err
	}
	txHash, err := dcr.wallet.SendRawTransaction(dcr.ctx, msgTx, false)
	if err != nil {
		return nil, translateRPCCancelErr(err)
	}
	return ToCoinID(txHash, 0), nil
}

// Withdraw withdraws funds to the specified address. Fees are subtracted from
// the value. feeRate is in units of atoms/byte.
// Withdraw satisfies asset.Withdrawer.
func (dcr *ExchangeWallet) Withdraw(address string, value, feeRate uint64) (asset.Coin, error) {
	addr, err := stdaddr.DecodeAddress(address, dcr.chainParams)
	if err != nil {
		return nil, fmt.Errorf("invalid address: %s", address)
	}
	msgTx, sentVal, err := dcr.withdraw(addr, value, dcr.feeRateWithFallback(feeRate))
	if err != nil {
		return nil, err
	}

	selfSend, err := dcr.OwnsDepositAddress(address)
	if err != nil {
		dcr.log.Errorf("error checking if address %q is owned: %v", address, err)
	}
	txType := txdb.Send
	if selfSend {
		txType = txdb.SelfSend
	}

	dcr.addTxToHistory(&txdb.WalletTransaction{
		Type:      txType,
		ID:        msgTx.CachedTxHash().String(),
		Amount:    sentVal,
		Fees:      value - sentVal,
		Recipient: &address,
	}, msgTx.CachedTxHash())

	return newOutput(msgTx.CachedTxHash(), 0, sentVal, wire.TxTreeRegular), nil
}

// Send sends the exact value to the specified address. This is different from
// Withdraw, which subtracts the tx fees from the amount sent. feeRate is in
// units of atoms/byte.
func (dcr *ExchangeWallet) Send(address string, value, feeRate uint64) (asset.Coin, error) {
	addr, err := stdaddr.DecodeAddress(address, dcr.chainParams)
	if err != nil {
		return nil, fmt.Errorf("invalid address: %s", address)
	}
	msgTx, sentVal, fee, err := dcr.sendToAddress(addr, value, dcr.feeRateWithFallback(feeRate))
	if err != nil {
		return nil, err
	}

	selfSend, err := dcr.OwnsDepositAddress(address)
	if err != nil {
		dcr.log.Errorf("error checking if address %q is owned: %v", address, err)
	}
	txType := txdb.Send
	if selfSend {
		txType = txdb.SelfSend
	}

	dcr.addTxToHistory(&txdb.WalletTransaction{
		Type:      txType,
		ID:        msgTx.CachedTxHash().String(),
		Amount:    sentVal,
		Fees:      fee,
		Recipient: &address,
		Timestamp: uint64(time.Now().Unix()),
		Confirms:  &txdb.Confirms{Target: confTxFinality},
	}, msgTx.CachedTxHash())
	return newOutput(msgTx.CachedTxHash(), 0, sentVal, wire.TxTreeRegular), nil
}

// addInputCoins adds inputs to the MsgTx to spend the specified outputs.
func (dcr *ExchangeWallet) addInputCoins(msgTx *wire.MsgTx, coins asset.Coins) (uint64, error) {
	var totalIn uint64
	for _, coin := range coins {
		op, err := dcr.convertCoin(coin)
		if err != nil {
			return 0, err
		}
		if op.value == 0 {
			return 0, fmt.Errorf("zero-valued output detected for %s:%d", op.txHash(), op.vout())
		}
		if op.tree == wire.TxTreeUnknown { // Set the correct prevout tree if unknown.
			unspentPrevOut, err := dcr.wallet.UnspentOutput(dcr.ctx, op.txHash(), op.vout(), op.tree)
			if err != nil {
				return 0, fmt.Errorf("unable to determine tree for prevout %s: %v", op.pt, err)
			}
			op.tree = unspentPrevOut.Tree
		}
		totalIn += op.value
		prevOut := op.wireOutPoint()
		txIn := wire.NewTxIn(prevOut, int64(op.value), []byte{})
		msgTx.AddTxIn(txIn)
	}
	return totalIn, nil
}

func (dcr *ExchangeWallet) shutdown() {
	// Close all open channels for contract redemption searches
	// to prevent leakages and ensure goroutines that are started
	// to wait on these channels end gracefully.
	dcr.findRedemptionMtx.Lock()
	for contractOutpoint, req := range dcr.findRedemptionQueue {
		close(req.resultChan)
		delete(dcr.findRedemptionQueue, contractOutpoint)
	}
	dcr.findRedemptionMtx.Unlock()

	// Disconnect the wallet. For rpc wallets, this shuts down
	// the rpcclient.Client.
	if dcr.wallet != nil {
		dcr.wallet.Disconnect()
	}
}

// SyncStatus is information about the blockchain sync status.
func (dcr *ExchangeWallet) SyncStatus() (ss *asset.SyncStatus, err error) {
	defer func() {
		var synced bool
		if ss != nil {
			synced = ss.Synced
		}

		if wasSynced := dcr.previouslySynced.Swap(synced); synced && !wasSynced {
			tip := dcr.cachedBestBlock()
			if tip != nil {
				go dcr.syncTxHistory(dcr.ctx, uint64(tip.height))
			}
		}
	}()

	// If we have a rescan running, do different math.
	dcr.rescan.RLock()
	rescanProgress := dcr.rescan.progress
	dcr.rescan.RUnlock()

	if rescanProgress != nil {
		tip := dcr.cachedBestBlock()
		if tip == nil {
			return nil, errors.New("wallet tip not synced")
		}
		height := max(tip.height, rescanProgress.scannedThrough)
		txHeight := uint64(rescanProgress.scannedThrough)
		return &asset.SyncStatus{
			Synced:         false,
			TargetHeight:   uint64(height),
			StartingBlocks: dcr.startingBlocks.Load(),
			Blocks:         uint64(height),
			Transactions:   &txHeight,
		}, nil
	}

	// No rescan in progress. Ask wallet.
	ss, err = dcr.wallet.SyncStatus(dcr.ctx)
	if err != nil {
		return nil, err
	}
	ss.StartingBlocks = dcr.startingBlocks.Load()
	return ss, nil
}

// Combines the RPC type with the spending input information.
type compositeUTXO struct {
	rpc   *walletjson.ListUnspentResult
	input *SpendInfo
	confs int64
	// TODO: consider including isDexChange bool for consumer
}

// parseUTXOs constructs and returns a list of compositeUTXOs from the provided
// set of RPC utxos, including basic information required to spend each rpc utxo.
// The returned list is sorted by ascending value.
func (dcr *ExchangeWallet) parseUTXOs(unspents []*walletjson.ListUnspentResult) ([]*compositeUTXO, error) {
	utxos := make([]*compositeUTXO, 0, len(unspents))
	for _, utxo := range unspents {
		if !utxo.Spendable {
			continue
		}
		scriptPK, err := hex.DecodeString(utxo.ScriptPubKey)
		if err != nil {
			return nil, fmt.Errorf("error decoding pubkey script for %s, script = %s: %w", utxo.TxID, utxo.ScriptPubKey, err)
		}
		redeemScript, err := hex.DecodeString(utxo.RedeemScript)
		if err != nil {
			return nil, fmt.Errorf("error decoding redeem script for %s, script = %s: %w", utxo.TxID, utxo.RedeemScript, err)
		}

		// NOTE: listunspent does not indicate script version, so for the
		// purposes of our funding coins, we are going to assume 0.
		nfo, err := InputInfo(0, scriptPK, redeemScript, dcr.chainParams)
		if err != nil {
			if errors.Is(err, util.UnsupportedScriptError) {
				continue
			}
			return nil, fmt.Errorf("error reading asset info: %w", err)
		}
		if nfo.ScriptType == ScriptUnsupported || nfo.NonStandardScript {
			// InputInfo sets NonStandardScript for P2SH with non-standard
			// redeem scripts. Don't return these since they cannot fund
			// arbitrary txns.
			continue
		}
		utxos = append(utxos, &compositeUTXO{
			rpc:   utxo,
			input: nfo,
			confs: utxo.Confirmations,
		})
	}
	// Sort in ascending order by amount (smallest first).
	sort.Slice(utxos, func(i, j int) bool { return utxos[i].rpc.Amount < utxos[j].rpc.Amount })
	return utxos, nil
}

// lockedAtoms is the total value of locked outputs, as locked with LockUnspent.
func (dcr *ExchangeWallet) lockedAtoms(acct string) (uint64, error) {
	lockedOutpoints, err := dcr.wallet.LockedOutputs(dcr.ctx, acct)
	if err != nil {
		return 0, err
	}
	var sum uint64
	for _, op := range lockedOutpoints {
		sum += toAtoms(op.Amount)
	}
	return sum, nil
}

// convertCoin converts the asset.Coin to an output whose tree may be unknown.
// Use wallet.UnspentOutput to determine the output tree where necessary.
func (dcr *ExchangeWallet) convertCoin(coin asset.Coin) (*output, error) {
	op, ok := coin.(*output)
	if ok {
		return op, nil
	}
	txHash, vout, err := decodeCoinID(coin.ID())
	if err != nil {
		return nil, err
	}
	return newOutput(txHash, vout, coin.Value(), wire.TxTreeUnknown), nil
}

// withdraw sends the amount to the address. Fees are subtracted from the
// sent value.
func (dcr *ExchangeWallet) withdraw(addr stdaddr.Address, val, feeRate uint64) (*wire.MsgTx, uint64, error) {
	if val == 0 {
		return nil, 0, fmt.Errorf("cannot withdraw value = 0")
	}
	baseSize := uint32(MsgTxOverhead + P2PKHOutputSize*2)
	reportChange := dcr.wallet.Accounts().UnmixedAccount == "" // otherwise change goes to unmixed account
	enough := sendEnough(val, feeRate, true, baseSize, reportChange)
	coins, _, _, _, err := dcr.fund(0, enough)
	if err != nil {
		return nil, 0, fmt.Errorf("unable to withdraw %s DCR to address %s with feeRate %d atoms/byte: %w",
			amount(val), addr, feeRate, err)
	}

	msgTx, sentVal, err := dcr.sendCoins(coins, addr, nil, val, 0, feeRate, true)
	if err != nil {
		if _, retErr := dcr.returnCoins(coins); retErr != nil {
			dcr.log.Errorf("Failed to unlock coins: %v", retErr)
		}
		return nil, 0, err
	}
	return msgTx, sentVal, nil
}

// sendToAddress sends an exact amount to an address. Transaction fees will be
// in addition to the sent amount, and the output will be the zeroth output.
// TODO: Just use the sendtoaddress rpc since dcrwallet respects locked utxos!
func (dcr *ExchangeWallet) sendToAddress(addr stdaddr.Address, amt, feeRate uint64) (*wire.MsgTx, uint64, uint64, error) {
	baseSize := uint32(MsgTxOverhead + P2PKHOutputSize*2)      // may be extra if change gets omitted (see signTxAndAddChange)
	reportChange := dcr.wallet.Accounts().UnmixedAccount == "" // otherwise change goes to unmixed account
	enough := sendEnough(amt, feeRate, false, baseSize, reportChange)
	coins, _, _, _, err := dcr.fund(0, enough)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("unable to send %s DCR with fee rate of %d atoms/byte: %w",
			amount(amt), feeRate, err)
	}

	msgTx, sentVal, err := dcr.sendCoins(coins, addr, nil, amt, 0, feeRate, false)
	if err != nil {
		if _, retErr := dcr.returnCoins(coins); retErr != nil {
			dcr.log.Errorf("Failed to unlock coins: %v", retErr)
		}
		return nil, 0, 0, err
	}

	var totalOut uint64
	for _, txOut := range msgTx.TxOut {
		totalOut += uint64(txOut.Value)
	}
	var totalIn uint64
	for _, coin := range coins {
		totalIn += coin.Value()
	}

	return msgTx, sentVal, totalIn - totalOut, nil
}

// sendCoins sends the amount to the address as the zeroth output, spending the
// specified coins. If subtract is true, the transaction fees will be taken from
// the sent value, otherwise it will taken from the change output. If there is
// change, it will be at index 1.
//
// An optional second output may be generated with the second address and amount
// arguments, if addr2 is non-nil. Note that to omit the extra output, the
// *interface* must be nil, not just the concrete type, so be cautious with
// concrete address types because a nil pointer wrap into a non-nil std.Address!
func (dcr *ExchangeWallet) sendCoins(coins asset.Coins, addr, addr2 stdaddr.Address, val, val2, feeRate uint64,
	subtract bool) (*wire.MsgTx, uint64, error) {
	baseTx := wire.NewMsgTx()
	_, err := dcr.addInputCoins(baseTx, coins)
	if err != nil {
		return nil, 0, err
	}
	payScriptVer, payScript := addr.PaymentScript()
	txOut := newTxOut(int64(val), payScriptVer, payScript)
	baseTx.AddTxOut(txOut)
	if addr2 != nil {
		payScriptVer, payScript := addr2.PaymentScript()
		txOut := newTxOut(int64(val2), payScriptVer, payScript)
		baseTx.AddTxOut(txOut)
	}

	var feeSource int32 // subtract from vout 0
	if !subtract {
		feeSource = -1 // subtract from change
	}

	tx, err := dcr.sendWithReturn(baseTx, feeRate, feeSource)
	if err != nil {
		return nil, 0, err
	}
	return tx, uint64(tx.TxOut[0].Value), err
}

// sendAll sends the maximum sendable amount (total input amount minus fees) to
// the provided account as a single output, spending the specified coins.
func (dcr *ExchangeWallet) sendAll(coins asset.Coins, destAcct string) (*wire.MsgTx, uint64, error) {
	addr, err := dcr.wallet.InternalAddress(dcr.ctx, destAcct)
	if err != nil {
		return nil, 0, err
	}

	baseTx := wire.NewMsgTx()
	totalIn, err := dcr.addInputCoins(baseTx, coins)
	if err != nil {
		return nil, 0, err
	}
	payScriptVer, payScript := addr.PaymentScript()
	txOut := newTxOut(int64(totalIn), payScriptVer, payScript)
	baseTx.AddTxOut(txOut)

	feeRate := dcr.targetFeeRateWithFallback(2, 0)
	tx, err := dcr.sendWithReturn(baseTx, feeRate, 0) // subtract from vout 0
	return tx, uint64(txOut.Value), err
}

// newTxOut returns a new transaction output with the given parameters.
func newTxOut(amount int64, pkScriptVer uint16, pkScript []byte) *wire.TxOut {
	return &wire.TxOut{
		Value:    amount,
		Version:  pkScriptVer,
		PkScript: pkScript,
	}
}

// msgTxFromHex creates a wire.MsgTx by deserializing the hex transaction.
func msgTxFromHex(txHex string) (*wire.MsgTx, error) {
	msgTx := wire.NewMsgTx()
	if err := msgTx.Deserialize(hex.NewDecoder(strings.NewReader(txHex))); err != nil {
		return nil, err
	}
	return msgTx, nil
}

// msgTxFromBytes creates a wire.MsgTx by deserializing the transaction bytes.
func msgTxFromBytes(txB []byte) (*wire.MsgTx, error) {
	msgTx := wire.NewMsgTx()
	if err := msgTx.Deserialize(bytes.NewReader(txB)); err != nil {
		return nil, err
	}
	return msgTx, nil
}

func msgTxToHex(msgTx *wire.MsgTx) (string, error) {
	b, err := msgTx.Bytes()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (dcr *ExchangeWallet) makeChangeOut(ctx context.Context, changeAcct string, val uint64) (*wire.TxOut, stdaddr.Address, error) {
	changeAddr, err := dcr.wallet.InternalAddress(ctx, changeAcct)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating change address: %w", err)
	}
	changeScriptVersion, changeScript := changeAddr.PaymentScript()
	return newTxOut(int64(val), changeScriptVersion, changeScript), changeAddr, nil
}

// sendWithReturn sends the unsigned transaction, adding a change output unless
// the amount is dust. subtractFrom indicates the output from which fees should
// be subtracted, where -1 indicates fees should come out of a change output.
func (dcr *ExchangeWallet) sendWithReturn(baseTx *wire.MsgTx, feeRate uint64, subtractFrom int32) (*wire.MsgTx, error) {
	signedTx, _, _, _, err := dcr.signTxAndAddChange(dcr.ctx, baseTx, feeRate, subtractFrom, dcr.depositAccount())
	if err != nil {
		return nil, err
	}

	_, err = dcr.broadcastTx(dcr.ctx, signedTx, feeRate)
	return signedTx, err
}

// signTxAndAddChange signs the passed msgTx, adding a change output that pays
// an address from the specified changeAcct, unless the change amount is dust.
// subtractFrom indicates the output from which fees should be subtracted, where
// -1 indicates fees should come out of a change output. baseTx may be modified
// with an added change output or a reduced value of the subtractFrom output.
func (dcr *ExchangeWallet) signTxAndAddChange(ctx context.Context, baseTx *wire.MsgTx, feeRate uint64,
	subtractFrom int32, changeAcct string) (*wire.MsgTx, *output, string, uint64, error) {
	// Sign the transaction to get an initial size estimate and calculate
	// whether a change output would be dust.
	sigCycles := 1
	msgTx, err := dcr.wallet.SignRawTransaction(ctx, baseTx)
	if err != nil {
		return nil, nil, "", 0, err
	}

	totalIn, totalOut, remaining, _, size := reduceMsgTx(msgTx)
	if totalIn < totalOut {
		return nil, nil, "", 0, fmt.Errorf("unbalanced transaction")
	}

	minFee := feeRate * size
	if subtractFrom == -1 && minFee > remaining {
		return nil, nil, "", 0, fmt.Errorf("not enough funds to cover minimum fee rate of %v atoms/B: %s > %s remaining",
			feeRate, amount(minFee), amount(remaining))
	}
	if int(subtractFrom) >= len(baseTx.TxOut) {
		return nil, nil, "", 0, fmt.Errorf("invalid subtractFrom output %d for tx with %d outputs",
			subtractFrom, len(baseTx.TxOut))
	}

	// Add a change output if there is enough remaining.
	var changeAdded bool
	var changeAddress stdaddr.Address
	var changeOutput *wire.TxOut
	minFeeWithChange := (size + P2PKHOutputSize) * feeRate
	if remaining > minFeeWithChange {
		changeValue := remaining - minFeeWithChange
		if subtractFrom >= 0 {
			// Subtract the additional fee needed for the added change output
			// from the specified existing output.
			changeValue = remaining
		}
		if !IsDustVal(P2PKHOutputSize, changeValue, feeRate) {
			if subtractFrom >= 0 { // only subtract after dust check
				baseTx.TxOut[subtractFrom].Value -= int64(minFeeWithChange)
				remaining += minFeeWithChange
			}
			changeOutput, changeAddress, err = dcr.makeChangeOut(ctx, changeAcct, changeValue)
			if err != nil {
				return nil, nil, "", 0, err
			}
			dcr.log.Debugf("Change output size = %d, addr = %s", changeOutput.SerializeSize(), changeAddress.String())
			changeAdded = true
			baseTx.AddTxOut(changeOutput) // unsigned txn
			remaining -= changeValue
		}
	}

	lastFee := remaining

	// If change added or subtracting from an existing output, iterate on fees.
	if changeAdded || subtractFrom >= 0 {
		subtractee := changeOutput
		if subtractFrom >= 0 {
			subtractee = baseTx.TxOut[subtractFrom]
		}
		// The amount available for fees is the sum of what is presently
		// allocated to fees (lastFee) and the value of the subtractee output,
		// which add to fees or absorb excess fees from lastFee.
		reservoir := lastFee + uint64(subtractee.Value)

		// Find the best fee rate by closing in on it in a loop.
		tried := map[uint64]bool{}
		for {
			// Each cycle, sign the transaction and see if there is a need to
			// raise or lower the fees.
			sigCycles++
			msgTx, err = dcr.wallet.SignRawTransaction(ctx, baseTx)
			if err != nil {
				return nil, nil, "", 0, err
			}
			size = uint64(msgTx.SerializeSize())
			reqFee := feeRate * size
			if reqFee > reservoir {
				// IsDustVal check must be bugged.
				dcr.log.Errorf("reached the impossible place. in = %.8f, out = %.8f, reqFee = %.8f, lastFee = %.8f, raw tx = %x",
					toDCR(totalIn), toDCR(totalOut), toDCR(reqFee), toDCR(lastFee), dcr.wireBytes(msgTx))
				return nil, nil, "", 0, fmt.Errorf("change error")
			}

			// If 1) lastFee == reqFee, nothing changed since the last cycle.
			// And there is likely no room for improvement. If 2) The reqFee
			// required for a transaction of this size is less than the
			// currently signed transaction fees, but we've already tried it,
			// then it must have a larger serialize size, so the current fee is
			// as good as it gets.
			if lastFee == reqFee || (lastFee > reqFee && tried[reqFee]) {
				break
			}

			// The minimum fee for a transaction of this size is either higher or
			// lower than the fee in the currently signed transaction, and it hasn't
			// been tried yet, so try it now.
			tried[lastFee] = true
			subtractee.Value = int64(reservoir - reqFee) // next
			lastFee = reqFee
			if IsDust(subtractee, feeRate) {
				// Another condition that should be impossible, but check anyway in case
				// the maximum fee was underestimated causing the first check to be
				// missed.
				dcr.log.Errorf("reached the impossible place. in = %.8f, out = %.8f, reqFee = %.8f, lastFee = %.8f, raw tx = %x",
					toDCR(totalIn), toDCR(totalOut), toDCR(reqFee), toDCR(lastFee), dcr.wireBytes(msgTx))
				return nil, nil, "", 0, fmt.Errorf("dust error")
			}
			continue
		}
	}

	// Double check the resulting txns fee and fee rate.
	_, _, checkFee, checkRate, size := reduceMsgTx(msgTx)
	if checkFee != lastFee {
		return nil, nil, "", 0, fmt.Errorf("fee mismatch! %.8f != %.8f, raw tx: %x", toDCR(checkFee), toDCR(lastFee), dcr.wireBytes(msgTx))
	}
	// Ensure the effective fee rate is at least the required fee rate.
	if checkRate < feeRate {
		return nil, nil, "", 0, fmt.Errorf("final fee rate for %s, %d, is lower than expected, %d. raw tx: %x",
			msgTx.CachedTxHash(), checkRate, feeRate, dcr.wireBytes(msgTx))
	}
	// This is a last ditch effort to catch ridiculously high fees. Right now,
	// it's just erroring for fees more than triple the expected rate, which is
	// admittedly un-scientific. This should account for any signature length
	// related variation as well as a potential dust change output with no
	// subtractee specified, in which case the dust goes to the miner.
	if changeAdded && checkRate > feeRate*3 {
		return nil, nil, "", 0, fmt.Errorf("final fee rate for %s, %d, is seemingly outrageous, target = %d, raw tx = %x",
			msgTx.CachedTxHash(), checkRate, feeRate, dcr.wireBytes(msgTx))
	}

	txHash := msgTx.TxHash()
	dcr.log.Debugf("%d signature cycles to converge on fees for tx %s: "+
		"min rate = %d, actual fee rate = %d (%v for %v bytes), change = %v",
		sigCycles, txHash, feeRate, checkRate, checkFee, size, changeAdded)

	var change *output
	var changeAddr string
	if changeAdded {
		change = newOutput(&txHash, uint32(len(msgTx.TxOut)-1), uint64(changeOutput.Value), wire.TxTreeRegular)
		changeAddr = changeAddress.String()
	}

	return msgTx, change, changeAddr, lastFee, nil
}

// ValidateAddress checks that the provided address is valid.
func (dcr *ExchangeWallet) ValidateAddress(address string) bool {
	_, err := stdaddr.DecodeAddress(address, dcr.chainParams)
	return err == nil
}

// dummyP2PKHScript only has to be a valid 25-byte pay-to-pubkey-hash pkScript
// for EstimateSendTxFee when an empty or invalid address is provided.
var dummyP2PKHScript = []byte{0x76, 0xa9, 0x14, 0xe4, 0x28, 0x61, 0xa,
	0xfc, 0xd0, 0x4e, 0x21, 0x94, 0xf7, 0xe2, 0xcc, 0xf8,
	0x58, 0x7a, 0xc9, 0xe7, 0x2c, 0x79, 0x7b, 0x88, 0xac,
}

// EstimateSendTxFee returns a tx fee rate estimate for sending or withdrawing the
// provided amount using the provided feeRate.
func (dcr *ExchangeWallet) EstimateSendTxFee(address string, sendAmount, feeRate uint64, subtract, _ bool) (fee uint64, isValidAddress bool, err error) {
	if sendAmount == 0 {
		return 0, false, fmt.Errorf("cannot check fee: send amount = 0")
	}

	feeRate = dcr.feeRateWithFallback(feeRate)

	var pkScript []byte
	var payScriptVer uint16
	if addr, err := stdaddr.DecodeAddress(address, dcr.chainParams); err == nil {
		payScriptVer, pkScript = addr.PaymentScript()
		isValidAddress = true
	} else {
		// use a dummy 25-byte p2pkh script
		pkScript = dummyP2PKHScript
	}

	tx := wire.NewMsgTx()

	tx.AddTxOut(newTxOut(int64(sendAmount), payScriptVer, pkScript)) // payScriptVer is default zero

	utxos, err := dcr.spendableUTXOs()
	if err != nil {
		return 0, false, err
	}

	minTxSize := uint32(tx.SerializeSize())
	reportChange := dcr.wallet.Accounts().UnmixedAccount == ""
	enough := sendEnough(sendAmount, feeRate, subtract, minTxSize, reportChange)
	sum, _, inputsSize, _, _, _, err := tryFund(utxos, enough)
	if err != nil {
		return 0, false, err
	}

	txSize := uint64(minTxSize + inputsSize)
	estFee := txSize * feeRate
	remaining := sum - sendAmount

	// Check if there will be a change output if there is enough remaining.
	estFeeWithChange := (txSize + P2PKHOutputSize) * feeRate
	var changeValue uint64
	if remaining > estFeeWithChange {
		changeValue = remaining - estFeeWithChange
	}

	if subtract {
		// fees are already included in sendAmount, anything else is change.
		changeValue = remaining
	}

	var finalFee uint64
	if IsDustVal(P2PKHOutputSize, changeValue, feeRate) {
		// remaining cannot cover a non-dust change and the fee for the change.
		finalFee = estFee + remaining
	} else {
		// additional fee will be paid for non-dust change
		finalFee = estFeeWithChange
	}
	return finalFee, isValidAddress, nil
}

// StandardSendFee returns the fees for a simple send tx with one input and two
// outputs.
func (dcr *ExchangeWallet) StandardSendFee(feeRate uint64) uint64 {
	var baseSize uint64 = MsgTxOverhead + P2PKHOutputSize*2 + P2PKHInputSize
	return feeRate * baseSize
}

func (dcr *ExchangeWallet) isNative() bool {
	return dcr.walletType == walletTypeSPV
}

// currentAgendas gets the most recent agendas from the chain params. The caller
// must populate the CurrentChoice field of the agendas.
func currentAgendas(chainParams *chaincfg.Params) (agendas []*asset.TBAgenda) {
	var bestID uint32
	for deploymentID := range chainParams.Deployments {
		if bestID == 0 || deploymentID > bestID {
			bestID = deploymentID
		}
	}
	for _, deployment := range chainParams.Deployments[bestID] {
		v := deployment.Vote
		agenda := &asset.TBAgenda{
			ID:          v.Id,
			Description: v.Description,
		}
		for _, choice := range v.Choices {
			agenda.Choices = append(agenda.Choices, &asset.TBChoice{
				ID:          choice.Id,
				Description: choice.Description,
			})
		}
		agendas = append(agendas, agenda)
	}
	return
}

func (dcr *ExchangeWallet) StakeStatus() (*asset.TicketStakingStatus, error) {
	if !dcr.connected.Load() {
		return nil, errors.New("not connected, login first")
	}
	// Try to get tickets first, because this will error for older RPC + SPV
	// wallets.
	tickets, err := dcr.tickets(dcr.ctx)
	if err != nil {
		if errors.Is(err, errOldSPVWallet) {
			return nil, nil
		}
		return nil, fmt.Errorf("error retrieving tickets: %w", err)
	}
	sinfo, err := dcr.wallet.StakeInfo(dcr.ctx)
	if err != nil {
		return nil, err
	}

	isRPC := !dcr.isNative()
	var vspURL string
	if !isRPC {
		if v := dcr.vspV.Load(); v != nil {
			vsp, ok := v.(*vsp)
			if ok {
				vspURL = vsp.URL
			} else {
				dcr.log.Warnf("unexpected type for vsp: %T", v)
			}
		}
	} else {
		rpcW, ok := dcr.wallet.(*rpcWallet)
		if !ok {
			return nil, errors.New("wallet not an *rpcWallet")
		}
		walletInfo, err := rpcW.walletInfo(dcr.ctx)
		if err != nil {
			return nil, fmt.Errorf("error retrieving wallet info: %w", err)
		}
		vspURL = walletInfo.VSP
	}
	voteChoices, tSpends, treasuryPolicy, err := dcr.wallet.VotingPreferences(dcr.ctx)
	if err != nil {
		return nil, fmt.Errorf("error retrieving stances: %w", err)
	}
	agendas := currentAgendas(dcr.chainParams)
	for _, agenda := range agendas {
		for _, c := range voteChoices {
			if c.AgendaID == agenda.ID {
				agenda.CurrentChoice = c.ChoiceID
				break
			}
		}
	}

	tip := dcr.cachedBestBlock()
	votingSubsidy := uint64(0)
	if tip != nil {
		votingSubsidy = dcr.voteSubsidy(tip.height)
	}
	return &asset.TicketStakingStatus{
		TicketPrice:   uint64(sinfo.Sdiff),
		VotingSubsidy: votingSubsidy,
		VSP:           vspURL,
		IsRPC:         isRPC,
		Tickets:       tickets,
		Stances: asset.Stances{
			Agendas:        agendas,
			TreasurySpends: tSpends,
			TreasuryKeys:   treasuryPolicy,
		},
		Stats: dcr.ticketStatsFromStakeInfo(sinfo),
	}, nil
}

func (dcr *ExchangeWallet) ticketStatsFromStakeInfo(sinfo *wallet.StakeInfoData) asset.TicketStats {
	return asset.TicketStats{
		TotalRewards: uint64(sinfo.TotalSubsidy),
		TicketCount:  sinfo.OwnMempoolTix + sinfo.Unspent + sinfo.Immature + sinfo.Voted + sinfo.Revoked,
		Votes:        sinfo.Voted,
		Revokes:      sinfo.Revoked,
		Mempool:      sinfo.OwnMempoolTix,
		Queued:       uint32(dcr.ticketBuyer.remaining.Load()),
	}
}

func (dcr *ExchangeWallet) voteSubsidy(tipHeight int64) uint64 {
	// Chance of a given ticket voting in a block is
	// p = chainParams.TicketsPerBlock / (chainParams.TicketPoolSize * chainParams.TicketsPerBlock)
	//   = 1 / chainParams.TicketPoolSize
	// Expected number of blocks to vote is
	// 1 / p = chainParams.TicketPoolSize
	expectedBlocksToVote := int64(dcr.chainParams.TicketPoolSize)
	voteHeightExpectationValue := tipHeight + expectedBlocksToVote
	return uint64(dcr.subsidyCache.CalcStakeVoteSubsidyV3(voteHeightExpectationValue, blockchain.SSVDCP0012))
}

// tickets gets tickets from the wallet and changes the status of "unspent"
// tickets that haven't reached expiration "live".
// DRAFT NOTE: From dcrwallet:
//
//	TicketStatusUnspent is a matured ticket that has not been spent.  It
//	is only used under SPV mode where it is unknown if a ticket is live,
//	was missed, or expired.
//
// But if the ticket has not reached a certain number of confirmations, we
// can say for sure it's not expired. With auto-revocations, "missed" or
// "expired" tickets are actually "revoked", I think.
// The only thing I can't figure out is how SPV wallets set the spender in the
// case of an auto-revocation. It might be happening here
// https://github.com/decred/dcrwallet/blob/a87fa843495ec57c1d3b478c2ceb3876c3749af5/wallet/chainntfns.go#L770-L775
// If we're seeing auto-revocations, we're fine to make the changes in this
// method.
func (dcr *ExchangeWallet) tickets(ctx context.Context) ([]*asset.Ticket, error) {
	tickets, err := dcr.wallet.Tickets(ctx)
	if err != nil {
		return nil, fmt.Errorf("error retrieving tickets: %w", err)
	}
	// Adjust status for SPV tickets that aren't expired.
	tip := dcr.cachedBestBlock()
	var oldestTicketsBlock int64
	if tip != nil {
		oldestTicketsBlock = tip.height - int64(dcr.chainParams.TicketExpiry) - int64(dcr.chainParams.TicketMaturity)
	}
	for _, t := range tickets {
		if t.Status != asset.TicketStatusUnspent {
			continue
		}
		if t.Tx.BlockHeight == -1 || t.Tx.BlockHeight > oldestTicketsBlock {
			t.Status = asset.TicketStatusLive
		}
	}
	return tickets, nil
}

func vspInfo(ctx context.Context, uri string) (*vspdjson.VspInfoResponse, error) {
	suffix := "/api/v3/vspinfo"
	path, err := neturl.JoinPath(uri, suffix)
	if err != nil {
		return nil, err
	}
	var info vspdjson.VspInfoResponse
	return &info, dexnet.Get(ctx, path, &info)
}

// SetVSP sets the VSP provider. Ability to set can be checked with StakeStatus
// first. Only non-RPC (internal) wallets can be set. Part of the
// asset.TicketBuyer interface.
func (dcr *ExchangeWallet) SetVSP(url string) error {
	if !dcr.isNative() {
		return errors.New("cannot set vsp for external wallet")
	}
	info, err := vspInfo(dcr.ctx, url)
	if err != nil {
		return err
	}
	v := vsp{
		URL:           url,
		PubKey:        base64.StdEncoding.EncodeToString(info.PubKey),
		FeePercentage: info.FeePercentage,
	}
	b, err := json.Marshal(&v)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dcr.vspFilepath, b, 0666); err != nil {
		return err
	}
	dcr.vspV.Store(&v)
	return nil
}

// PurchaseTickets purchases n number of tickets. Part of the asset.TicketBuyer
// interface.
func (dcr *ExchangeWallet) PurchaseTickets(n int, feeSuggestion uint64) error {
	if n < 1 {
		return nil
	}
	if !dcr.connected.Load() {
		return errors.New("not connected, login first")
	}
	bal, err := dcr.Balance()
	if err != nil {
		return fmt.Errorf("error getting balance: %v", err)
	}
	isRPC := !dcr.isNative()
	if isRPC {
		rpcW, ok := dcr.wallet.(*rpcWallet)
		if !ok {
			return errors.New("wallet not an *rpcWallet")
		}
		walletInfo, err := rpcW.walletInfo(dcr.ctx)
		if err != nil {
			return fmt.Errorf("error retrieving wallet info: %w", err)
		}
		if walletInfo.SPV && walletInfo.VSP == "" {
			return errors.New("a vsp must best set to purchase tickets with an spv wallet")
		}
	}
	sinfo, err := dcr.wallet.StakeInfo(dcr.ctx)
	if err != nil {
		return fmt.Errorf("stakeinfo error: %v", err)
	}
	// I think we need to set this, otherwise we probably end up with default
	// of DefaultRelayFeePerKb = 1e4 => 10 atoms/byte.
	feePerKB := dcrutil.Amount(dcr.feeRateWithFallback(feeSuggestion) * 1000)
	if err := dcr.wallet.SetTxFee(dcr.ctx, feePerKB); err != nil {
		return fmt.Errorf("error setting wallet tx fee: %w", err)
	}

	// Get a minimum size assuming a single-input split tx.
	fees := feePerKB * minVSPTicketPurchaseSize / 1000
	ticketPrice := sinfo.Sdiff + fees
	total := uint64(n) * uint64(ticketPrice)
	if bal.Available < total {
		return fmt.Errorf("available balance %s is lower than projected cost %s for %d tickets",
			dcrutil.Amount(bal.Available), dcrutil.Amount(total), n)
	}
	remain := dcr.ticketBuyer.remaining.Add(int32(n))
	dcr.emit.Data(ticketDataRoute, &TicketPurchaseUpdate{Remaining: uint32(remain)})
	go dcr.runTicketBuyer()

	return nil
}

const ticketDataRoute = "ticketPurchaseUpdate"

// TicketPurchaseUpdate is an update from the asynchronous ticket purchasing
// loop.
type TicketPurchaseUpdate struct {
	Err       string             `json:"err,omitempty"`
	Remaining uint32             `json:"remaining"`
	Tickets   []*asset.Ticket    `json:"tickets"`
	Stats     *asset.TicketStats `json:"stats,omitempty"`
}

// runTicketBuyer attempts to buy requested tickets. Because of a dcrwallet bug,
// its possible that (Wallet).PurchaseTickets will purchase fewer tickets than
// requested, without error. To work around this bug, we add requested tickets
// to ExchangeWallet.ticketBuyer.remaining, and re-run runTicketBuyer every
// block.
func (dcr *ExchangeWallet) runTicketBuyer() {
	tb := &dcr.ticketBuyer
	if !tb.running.CompareAndSwap(false, true) {
		// already running
		return
	}
	defer tb.running.Store(false)
	var ok bool
	defer func() {
		if !ok {
			tb.remaining.Store(0)
		}
	}()

	if tb.unconfirmedTickets == nil {
		tb.unconfirmedTickets = make(map[chainhash.Hash]struct{})
	}

	remain := tb.remaining.Load()
	if remain < 1 {
		return
	}

	for txHash := range tb.unconfirmedTickets {
		tx, err := dcr.wallet.GetTransaction(dcr.ctx, &txHash)
		if err != nil {
			dcr.log.Errorf("GetTransaction error ticket tx %s: %v", txHash, err)
			dcr.emit.Data(ticketDataRoute, &TicketPurchaseUpdate{Err: err.Error()})
			return
		}
		if tx.Confirmations > 0 {
			delete(tb.unconfirmedTickets, txHash)
		}
	}
	if len(tb.unconfirmedTickets) > 0 {
		ok = true
		dcr.log.Tracef("Skipping ticket purchase attempt because there are still %d unconfirmed tickets", len(tb.unconfirmedTickets))
		return
	}

	dcr.log.Tracef("Attempting to purchase %d tickets", remain)

	bal, err := dcr.Balance()
	if err != nil {
		dcr.log.Errorf("GetBalance error: %v", err)
		dcr.emit.Data(ticketDataRoute, &TicketPurchaseUpdate{Err: err.Error()})
		return
	}
	sinfo, err := dcr.wallet.StakeInfo(dcr.ctx)
	if err != nil {
		dcr.log.Errorf("StakeInfo error: %v", err)
		dcr.emit.Data(ticketDataRoute, &TicketPurchaseUpdate{Err: err.Error()})
		return
	}
	if dcrutil.Amount(bal.Available) < sinfo.Sdiff*dcrutil.Amount(remain) {
		dcr.log.Errorf("Insufficient balance %s to purchase %d ticket at price %s: %v", dcrutil.Amount(bal.Available), remain, sinfo.Sdiff, err)
		dcr.emit.Data(ticketDataRoute, &TicketPurchaseUpdate{Err: "insufficient balance"})
		return
	}

	var tickets []*asset.Ticket
	if !dcr.isNative() {
		tickets, err = dcr.wallet.PurchaseTickets(dcr.ctx, int(remain), "", "", false)
	} else {
		v := dcr.vspV.Load()
		if v == nil {
			err = errors.New("no vsp set")
		} else {
			vInfo, ok := v.(*vsp)
			if !ok {
				err = errors.New("invalid vsp type")
			} else {
				tickets, err = dcr.wallet.PurchaseTickets(dcr.ctx, int(remain), vInfo.URL, vInfo.PubKey, dcr.mixing.Load())
			}
		}
	}
	if err != nil {
		dcr.log.Errorf("PurchaseTickets error: %v", err)
		dcr.emit.Data(ticketDataRoute, &TicketPurchaseUpdate{Err: err.Error()})
		return
	}
	purchased := int32(len(tickets))
	remain = tb.remaining.Add(-purchased)
	// sanity check
	if remain < 0 {
		remain = 0
		tb.remaining.Store(remain)
	}
	stats := dcr.ticketStatsFromStakeInfo(sinfo)
	stats.Mempool += uint32(len(tickets))
	stats.Queued = uint32(remain)
	dcr.emit.Data(ticketDataRoute, &TicketPurchaseUpdate{
		Tickets:   tickets,
		Remaining: uint32(remain),
		Stats:     &stats,
	})
	for _, ticket := range tickets {
		txHash, err := chainhash.NewHashFromStr(ticket.Tx.Hash)
		if err != nil {
			dcr.log.Errorf("NewHashFromStr error for ticket hash %s: %v", ticket.Tx.Hash, err)
			dcr.emit.Data(ticketDataRoute, &TicketPurchaseUpdate{Err: err.Error()})
			return
		}
		tb.unconfirmedTickets[*txHash] = struct{}{}
		dcr.addTxToHistory(&txdb.WalletTransaction{
			Type:      txdb.TicketPurchase,
			ID:        txHash.String(),
			Amount:    ticket.Tx.TicketPrice,
			Fees:      ticket.Tx.Fees,
			Timestamp: uint64(time.Now().Unix()),
			Confirms:  &txdb.Confirms{Target: confTxFinality},
		}, txHash)
	}
	ok = true
}

// SetVotingPreferences sets the vote choices for all active tickets and future
// tickets. Nil maps can be provided for no change. Part of the
// asset.TicketBuyer interface.
func (dcr *ExchangeWallet) SetVotingPreferences(choices map[string]string, tspendPolicy map[string]string, treasuryPolicy map[string]string) error {
	if !dcr.connected.Load() {
		return errors.New("not connected, login first")
	}
	return dcr.wallet.SetVotingPreferences(dcr.ctx, choices, tspendPolicy, treasuryPolicy)
}

// ListVSPs lists known available voting service providers.
func (dcr *ExchangeWallet) ListVSPs() ([]*asset.VotingServiceProvider, error) {
	if dcr.network == util.Simnet {
		const simnetVSPUrl = "http://127.0.0.1:19591"
		vspi, err := vspInfo(dcr.ctx, simnetVSPUrl)
		if err != nil {
			dcr.log.Warnf("Error getting simnet VSP info: %v", err)
			return []*asset.VotingServiceProvider{}, nil
		}
		return []*asset.VotingServiceProvider{{
			URL:           simnetVSPUrl,
			Network:       util.Simnet,
			Launched:      uint64(time.Now().Add(-time.Hour * 24 * 180).UnixMilli()),
			LastUpdated:   uint64(time.Now().Add(-time.Minute * 15).UnixMilli()),
			APIVersions:   vspi.APIVersions,
			FeePercentage: vspi.FeePercentage,
			Closed:        vspi.VspClosed,
			Voting:        vspi.Voting,
			Voted:         vspi.Voted,
			Revoked:       vspi.Expired + vspi.Missed,
			VSPDVersion:   vspi.VspdVersion,
			BlockHeight:   vspi.BlockHeight,
			NetShare:      vspi.NetworkProportion,
		}}, nil
	}

	// This struct is not quite compatible with vspdjson.VspInfoResponse.
	var res map[string]*struct {
		Network       string  `json:"network"`
		Launched      uint64  `json:"launched"`    // seconds
		LastUpdated   uint64  `json:"lastupdated"` // seconds
		APIVersions   []int64 `json:"apiversions"`
		FeePercentage float64 `json:"feepercentage"`
		Closed        bool    `json:"closed"`
		Voting        int64   `json:"voting"`
		Voted         int64   `json:"voted"`
		Revoked       int64   `json:"revoked"`
		VSPDVersion   string  `json:"vspdversion"`
		BlockHeight   uint32  `json:"blockheight"`
		NetShare      float32 `json:"estimatednetworkproportion"`
	}
	if err := dexnet.Get(dcr.ctx, "https://api.decred.org/?c=vsp", &res); err != nil {
		return nil, err
	}
	vspds := make([]*asset.VotingServiceProvider, 0)
	for host, v := range res {
		net, err := util.NetFromString(v.Network)
		if err != nil {
			dcr.log.Warnf("error parsing VSP network from %q", v.Network)
		}
		if net != dcr.network {
			continue
		}
		vspds = append(vspds, &asset.VotingServiceProvider{
			URL:           "https://" + host,
			Network:       net,
			Launched:      v.Launched * 1000,    // to milliseconds
			LastUpdated:   v.LastUpdated * 1000, // to milliseconds
			APIVersions:   v.APIVersions,
			FeePercentage: v.FeePercentage,
			Closed:        v.Closed,
			Voting:        v.Voting,
			Voted:         v.Voted,
			Revoked:       v.Revoked,
			VSPDVersion:   v.VSPDVersion,
			BlockHeight:   v.BlockHeight,
			NetShare:      v.NetShare,
		})
	}
	return vspds, nil
}

// TicketPage fetches a page of tickets within a range of block numbers with a
// target page size and optional offset. scanStart is the block in which to
// start the scan. The scan progresses in reverse block number order, starting
// at scanStart and going to progressively lower blocks. scanStart can be set to
// -1 to indicate the current chain tip.
func (dcr *ExchangeWallet) TicketPage(scanStart int32, n, skipN int) ([]*asset.Ticket, error) {
	if !dcr.connected.Load() {
		return nil, errors.New("not connected, login first")
	}
	pager, is := dcr.wallet.(ticketPager)
	if !is {
		return nil, errors.New("ticket pagination not supported for this wallet")
	}
	return pager.TicketPage(dcr.ctx, scanStart, n, skipN)
}

// AddressAccount returns the account number for an address.
func (dcr *ExchangeWallet) AddressAccount(address string) (bool, uint32, error) {
	return dcr.wallet.AddressAccount(dcr.ctx, address)
}

// SignMessage returns the signature of a signed message using an address'
// associated private key.
func (dcr *ExchangeWallet) SignMessage(msg string, addr string) ([]byte, error) {
	return dcr.wallet.SignMessage(dcr.ctx, msg, addr)
}

// CommittedTickets takes a list of tickets and returns a filtered list of
// tickets that are controlled by this wallet.
func (dcr *ExchangeWallet) CommittedTickets(tickets []*chainhash.Hash) ([]*chainhash.Hash, []stdaddr.Address, error) {
	return dcr.wallet.CommittedTickets(dcr.ctx, tickets)
}

// startPoliteia initializes the Politeia subsystem and starts a background
// goroutine that syncs proposals periodically. Only runs on mainnet. If
// Politeia is already running, it is stopped first.
//
// stopPoliteia blocks until the old goroutine's wg.Done() has fired, so
// the new wg.Add(1) here cannot race with the previous wg.Done().
func (dcr *ExchangeWallet) startPoliteia(ctx context.Context) {
	if dcr.network != util.Mainnet {
		return
	}

	// Ensure any previous instance is fully stopped before starting a new one.
	dcr.stopPoliteia()

	// Create the Politeia instance outside the lock to avoid blocking readers
	// (PoliteiaDetails, ProposalsAll, etc.) during filesystem I/O.
	// TODO: Route Politeia requests through Tor when TorProxy is configured.
	// The upstream piclient.Opts does not accept a custom http.Client.
	p, err := pi.New(ctx, pi.PoliteiaMainnetHost, filepath.Join(dcr.walletDir, "politeia"), dcr.log.SubLogger("Politeia"))
	if err != nil {
		dcr.log.Errorf("failed to set up politeia: %v", err)
		return
	}

	// pCtx is a child of the wallet's ctx so that cancelling either one shuts
	// down the sync goroutine. politeiaCancel allows stopPoliteia to cancel
	// only the Politeia goroutine without tearing down the whole wallet.
	pCtx, cancel := context.WithCancel(ctx)

	dcr.politeiaMtx.Lock()
	done := make(chan struct{})
	dcr.politeia = p
	dcr.politeiaCancel = cancel
	dcr.politeiaDone = done
	dcr.politeiaMtx.Unlock()

	dcr.wg.Add(1)
	go func() {
		defer dcr.wg.Done()
		defer close(done)
		defer func() {
			if err := p.Close(); err != nil {
				dcr.log.Errorf("error closing politeia: %v", err)
			}
			dcr.politeiaMtx.Lock()
			dcr.politeia = nil
			dcr.politeiaCancel = nil
			dcr.politeiaDone = nil
			dcr.politeiaMtx.Unlock()
		}()

		// Initiate first sync.
		dcr.politeiaSyncing.Store(true)
		if err := p.ProposalsSync(); err != nil {
			dcr.log.Errorf("politeia.ProposalsSync failed: %v", err)
		}
		dcr.politeiaSyncing.Store(false)

		tick := time.NewTicker(20 * time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				dcr.politeiaSyncing.Store(true)
				if err := p.ProposalsSync(); err != nil {
					dcr.log.Errorf("politeia.ProposalsSync failed: %v", err)
				}
				dcr.politeiaSyncing.Store(false)
			case <-pCtx.Done():
				return
			}
		}
	}()
}

// stopPoliteia shuts down the Politeia subsystem. Blocks until the background
// goroutine exits. Safe to call when Politeia is not running.
func (dcr *ExchangeWallet) stopPoliteia() {
	dcr.politeiaMtx.Lock()
	cancel := dcr.politeiaCancel
	done := dcr.politeiaDone
	// Nil out cancel so a concurrent stopPoliteia call is a no-op.
	dcr.politeiaCancel = nil
	dcr.politeiaMtx.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// PoliteiaEnabled returns whether Politeia sync is currently enabled.
func (dcr *ExchangeWallet) PoliteiaEnabled() bool {
	return dcr.config().politeiaEnabled
}

// PoliteiaDetails returns the Politeia URL, whether a sync is in progress, and
// the unix timestamp of the last completed sync. The URL is always returned for
// mainnet even when Politeia sync is disabled.
func (dcr *ExchangeWallet) PoliteiaDetails() (string, bool, int64) {
	var url string
	if dcr.network == util.Mainnet {
		url = pi.PoliteiaMainnetHost
	}

	dcr.politeiaMtx.RLock()
	p := dcr.politeia
	dcr.politeiaMtx.RUnlock()

	if p == nil {
		return url, false, 0
	}
	return url, dcr.politeiaSyncing.Load(), p.ProposalsLastSync()
}

// ProposalsAll fetches proposals from the local DB with optional filtering.
func (dcr *ExchangeWallet) ProposalsAll(offset, rowsCount int, searchPhrase string, filterByVoteStatus ...int) ([]*pi.Proposal, int, error) {
	dcr.politeiaMtx.RLock()
	p := dcr.politeia
	dcr.politeiaMtx.RUnlock()

	if p == nil {
		return nil, 0, fmt.Errorf("politeia not configured")
	}
	return p.ProposalsAll(offset, rowsCount, searchPhrase, filterByVoteStatus...)
}

// Proposal retrieves a single proposal by token. If the proposal is currently
// voting and the wallet has eligible tickets, vote details are populated.
func (dcr *ExchangeWallet) Proposal(token string) (*pi.Proposal, error) {
	dcr.politeiaMtx.RLock()
	p := dcr.politeia
	dcr.politeiaMtx.RUnlock()

	if p == nil {
		return nil, fmt.Errorf("politeia not configured")
	}

	proposal, err := p.ProposalByToken(token)
	if err != nil {
		return nil, err
	}

	if proposal.VoteStatus != tv1.VoteStatusStarted {
		return proposal, nil
	}

	tip := dcr.currentTip.Load()
	if tip == nil {
		// Wallet hasn't synced a tip yet; can't check voting eligibility.
		return proposal, nil
	}
	if tip.height > int64(proposal.EndBlockHeight) {
		return proposal, nil
	}

	// The wallet itself satisfies the VotingWallet interface.
	proposal.VoteDetails, err = p.WalletProposalVoteDetails(dcr, token)
	if err != nil {
		return nil, err
	}

	return proposal, nil
}

// ProposalsInProgress returns mini proposals for proposals currently in
// progress.
func (dcr *ExchangeWallet) ProposalsInProgress() ([]*pi.MiniProposal, error) {
	dcr.politeiaMtx.RLock()
	p := dcr.politeia
	dcr.politeiaMtx.RUnlock()

	if p == nil {
		return nil, nil
	}
	return p.ProposalsInProgress()
}

// CastVote casts votes on the proposal identified by token using the provided
// vote bit. The wallet must already be unlocked before calling this method.
func (dcr *ExchangeWallet) CastVote(token, bit string) error {
	if bit != pi.VoteBitYes && bit != pi.VoteBitNo {
		return errors.New("invalid vote bit")
	}

	dcr.politeiaMtx.RLock()
	p := dcr.politeia
	dcr.politeiaMtx.RUnlock()

	if p == nil {
		return fmt.Errorf("politeia not configured")
	}

	voteDetails, err := p.WalletProposalVoteDetails(dcr, token)
	if err != nil {
		return err
	}

	if len(voteDetails.EligibleTickets) == 0 {
		if len(voteDetails.Votes) > 0 {
			return fmt.Errorf("all eligible tickets have already voted on proposal (%s)", token)
		}
		return fmt.Errorf("no eligible tickets found for voting wallet")
	}

	return p.CastVotes(dcr, voteDetails.EligibleTickets, bit, token)
}

func (dcr *ExchangeWallet) broadcastTx(ctx context.Context, signedTx *wire.MsgTx, feeRate uint64) (*chainhash.Hash, error) {
	// Hard limit: Validate transaction size before broadcasting to prevent
	// transactions from getting stuck in mempool.
	// Use MaxStandardTxSize (100KB) not chainParams.MaxTxSize (393KB) because
	// the mempool enforces a 100KB policy limit for transaction relay.
	// Transactions larger than 100KB will be rejected by the mempool even though
	// they would be valid according to consensus rules.
	txSize := signedTx.SerializeSize()
	maxSize := uint64(MaxStandardTxSize)

	if uint64(txSize) > maxSize {
		return nil, fmt.Errorf(
			"transaction size (%d bytes) exceeds maximum standard size (%d bytes). "+
				"This transaction contains %d inputs and would be rejected by the mempool. "+
				"Please consolidate UTXOs or send a smaller amount",
			txSize, maxSize, len(signedTx.TxIn))
	}

	// Validate outputs are not dust to prevent mempool rejection.
	// Use fallback fee rate if not provided. This check acts as a final
	// safety net before broadcasting. See dcrd/mempool/policy isDust for
	// dust policy documentation.
	if feeRate == 0 {
		feeRate = dcr.targetFeeRateWithFallback(2, 0)
	}

	for i, txOut := range signedTx.TxOut {
		// Skip provably unspendable outputs (e.g., OP_RETURN) which are
		// allowed to have any value including zero. OP_RETURN is opcode 0x6a.
		if len(txOut.PkScript) > 0 && txOut.PkScript[0] == txscript.OP_RETURN {
			continue
		}

		if IsDust(txOut, feeRate) {
			// Calculate minimum non-dust value for error message.
			totalSize := uint64(txOut.SerializeSize()) + 165
			minValue := feeRate * totalSize * 3

			return nil, fmt.Errorf(
				"output %d is dust: value %d atoms is below dust threshold of %d atoms "+
					"at fee rate %d atoms/byte. Consider sending at least %.8f DCR or consolidating inputs",
				i, txOut.Value, minValue, feeRate, float64(minValue)/1e8)
		}
	}

	txHash, err := dcr.wallet.SendRawTransaction(ctx, signedTx, false)
	if err != nil {
		if broadcast.IsAlreadyBroadcastErr(err) {
			h := signedTx.CachedTxHash()
			dcr.log.Warnf("Transaction %s appears to already be broadcast: %v", h, err)
			return h, nil
		}
		return nil, fmt.Errorf("sendrawtx error: %w, raw tx: %x", err, dcr.wireBytes(signedTx))
	}
	checkHash := signedTx.TxHash()
	if *txHash != checkHash {
		return nil, fmt.Errorf("transaction sent, but received unexpected transaction ID back from RPC server. "+
			"expected %s, got %s, raw tx: %x", *txHash, checkHash, dcr.wireBytes(signedTx))
	}
	return txHash, nil
}

// createSig creates and returns the serialized raw signature and compressed
// pubkey for a transaction input signature.
func (dcr *ExchangeWallet) createSig(ctx context.Context, tx *wire.MsgTx, idx int, pkScript []byte, addr stdaddr.Address) (sig, pubkey []byte, err error) {
	sigType, err := AddressSigType(addr)
	if err != nil {
		return nil, nil, err
	}

	priv, err := dcr.wallet.AddressPrivKey(ctx, addr)
	if err != nil {
		return nil, nil, err
	}
	defer priv.Zero()

	sig, err = sign.RawTxInSignature(tx, idx, pkScript, txscript.SigHashAll, priv.Serialize(), sigType)
	if err != nil {
		return nil, nil, err
	}

	return sig, priv.PubKey().SerializeCompressed(), nil
}

func (dcr *ExchangeWallet) txDB() *btc.BadgerTxDB {
	db := dcr.txHistoryDB.Load()
	if db == nil {
		return nil
	}
	txdb, ok := db.(*btc.BadgerTxDB)
	if !ok {
		return nil
	}
	return txdb
}

func rpcTxFee(tx *ListTransactionsResult) uint64 {
	if tx.Fee != nil {
		if *tx.Fee < 0 {
			return toAtoms(-*tx.Fee)
		}
		return toAtoms(*tx.Fee)
	}
	return 0
}

func isRegularMix(tx *wire.MsgTx) (isMix bool, mixDenom int64) {
	if len(tx.TxOut) < 3 || len(tx.TxIn) < 3 {
		return false, 0
	}

	mixedOuts := make(map[int64]uint32)
	for _, o := range tx.TxOut {
		val := o.Value
		if _, ok := splitPointMap[val]; ok {
			mixedOuts[val]++
			continue
		}
	}

	var mixCount uint32
	for val, count := range mixedOuts {
		if count < 3 {
			continue
		}
		if val > mixDenom {
			mixDenom = val
			mixCount = count
		}
	}

	// TODO: revisit the input count requirements
	isMix = mixCount >= uint32(len(tx.TxOut)/2)
	return
}

// isMixedSplitTx tests if a transaction is a CSPP-mixed ticket split
// transaction (the transaction that creates appropriately-sized outputs to be
// spent by a ticket purchase). This dumbly checks for at least three outputs
// of the same size and three of other sizes. It could be smarter by checking
// for ticket price + ticket fee outputs, but it's impossible to know the fee
// after the fact although it`s probably the default fee.
func isMixedSplitTx(tx *wire.MsgTx) (isMix bool, tikPrice int64) {
	if len(tx.TxOut) < 6 || len(tx.TxIn) < 3 {
		return false, 0
	}
	values := make(map[int64]int)
	for _, o := range tx.TxOut {
		values[o.Value]++
	}

	var numPossibleTickets int
	for k, v := range values {
		if v > numPossibleTickets {
			numPossibleTickets = v
			tikPrice = k
		}
	}
	numOtherOut := len(tx.TxOut) - numPossibleTickets

	// NOTE: The numOtherOut requirement may be too strict,
	if numPossibleTickets < 3 || numOtherOut < 3 {
		return false, 0
	}

	return true, tikPrice
}

func isMixTx(tx *wire.MsgTx) (isMix bool, mixDenom int64) {
	if isMix, mixDenom = isRegularMix(tx); isMix {
		return
	}

	return isMixedSplitTx(tx)
}

// idUnknownTx identifies the type and details of a transaction either made
// or received by the wallet.
func (dcr *ExchangeWallet) idUnknownTx(ctx context.Context, ltxr *ListTransactionsResult) (*txdb.WalletTransaction, error) {
	txHash, err := chainhash.NewHashFromStr(ltxr.TxID)
	if err != nil {
		return nil, fmt.Errorf("error decoding tx hash %s: %v", ltxr.TxID, err)
	}
	tx, err := dcr.wallet.GetTransaction(ctx, txHash)
	if err != nil {
		return nil, fmt.Errorf("GetTransaction error: %v", err)
	}

	var totalIn uint64
	for _, txIn := range tx.MsgTx.TxIn {
		if txIn.ValueIn > 0 {
			totalIn += uint64(txIn.ValueIn)
		}
	}

	var totalOut uint64
	for _, txOut := range tx.MsgTx.TxOut {
		totalOut += uint64(txOut.Value)
	}

	fee := rpcTxFee(ltxr)
	if fee == 0 && totalIn > totalOut {
		fee = totalIn - totalOut
	}

	switch *ltxr.TxType {
	case walletjson.LTTTVote:
		return &txdb.WalletTransaction{
			Type:   txdb.TicketVote,
			ID:     ltxr.TxID,
			Amount: totalOut,
			Fees:   fee,
		}, nil
	case walletjson.LTTTRevocation:
		return &txdb.WalletTransaction{
			Type:   txdb.TicketRevocation,
			ID:     ltxr.TxID,
			Amount: totalOut,
			Fees:   fee,
		}, nil
	case walletjson.LTTTTicket:
		return &txdb.WalletTransaction{
			Type:   txdb.TicketPurchase,
			ID:     ltxr.TxID,
			Amount: totalOut,
			Fees:   fee,
		}, nil
	}

	// Any other P2SH may be a swap or a send. We cannot determine unless we
	// look up the transaction that spends this UTXO.
	txPaysToScriptHash := func(msgTx *wire.MsgTx) (v uint64) {
		for _, txOut := range msgTx.TxOut {
			if txscript.IsPayToScriptHash(txOut.PkScript) {
				v += uint64(txOut.Value)
			}
		}
		return
	}
	if v := txPaysToScriptHash(tx.MsgTx); v > 0 {
		return &txdb.WalletTransaction{
			Type:   txdb.SwapOrSend,
			ID:     ltxr.TxID,
			Amount: v,
			Fees:   fee,
		}, nil
	}

	// Helper function will help us identify inputs that spend P2SH contracts.
	containsContractAtPushIndex := func(msgTx *wire.MsgTx, idx int, isContract func(contract []byte) bool) bool {
	txinloop:
		for _, txIn := range msgTx.TxIn {
			// not segwit
			const scriptVer = 0
			tokenizer := txscript.MakeScriptTokenizer(scriptVer, txIn.SignatureScript)
			for i := 0; i <= idx; i++ { // contract is 5th item item in redemption and 4th in refund
				if !tokenizer.Next() {
					continue txinloop
				}
			}
			if isContract(tokenizer.Data()) {
				return true
			}
		}
		return false
	}

	// Swap redemptions and refunds
	contractIsSwap := func(contract []byte) bool {
		_, _, _, _, err := ExtractSwapDetails(contract, dcr.chainParams)
		return err == nil
	}
	redeemsSwap := func(msgTx *wire.MsgTx) bool {
		return containsContractAtPushIndex(msgTx, 4, contractIsSwap)
	}
	if redeemsSwap(tx.MsgTx) {
		return &txdb.WalletTransaction{
			Type:   txdb.Redeem,
			ID:     ltxr.TxID,
			Amount: totalOut + fee,
			Fees:   fee,
		}, nil
	}
	refundsSwap := func(msgTx *wire.MsgTx) bool {
		return containsContractAtPushIndex(msgTx, 3, contractIsSwap)
	}
	if refundsSwap(tx.MsgTx) {
		return &txdb.WalletTransaction{
			Type:   txdb.Refund,
			ID:     ltxr.TxID,
			Amount: totalOut + fee,
			Fees:   fee,
		}, nil
	}

	const scriptVersion = 0

	allOutputsPayUs := func(msgTx *wire.MsgTx) bool {
		for _, txOut := range msgTx.TxOut {
			_, addrs := stdscript.ExtractAddrs(scriptVersion, txOut.PkScript, dcr.chainParams)
			if len(addrs) != 1 { // sanity check
				return false
			}

			addr := addrs[0]
			owns, err := dcr.wallet.WalletOwnsAddress(ctx, addr)
			if err != nil {
				dcr.log.Errorf("walletOwnsAddress error: %w", err)
				return false
			}
			if !owns {
				return false
			}
		}

		return true
	}

	if ltxr.Send && allOutputsPayUs(tx.MsgTx) && len(tx.MsgTx.TxIn) == 1 {
		return &txdb.WalletTransaction{
			Type: txdb.Split,
			ID:   ltxr.TxID,
			Fees: fee,
		}, nil
	}

	if isMix, mixDenom := isMixTx(tx.MsgTx); isMix {
		var mixedAmount uint64
		for _, txOut := range tx.MsgTx.TxOut {
			if txOut.Value == mixDenom {
				_, addrs := stdscript.ExtractAddrs(scriptVersion, txOut.PkScript, dcr.chainParams)
				if err != nil {
					dcr.log.Errorf("ExtractAddrs error: %w", err)
					continue
				}
				if len(addrs) != 1 { // sanity check
					continue
				}

				addr := addrs[0]
				owns, err := dcr.wallet.WalletOwnsAddress(ctx, addr)
				if err != nil {
					dcr.log.Errorf("walletOwnsAddress error: %w", err)
					continue
				}

				if owns {
					mixedAmount += uint64(txOut.Value)
				}
			}
		}

		return &txdb.WalletTransaction{
			Type:   txdb.Mix,
			ID:     ltxr.TxID,
			Amount: mixedAmount,
			Fees:   fee,
		}, nil
	}

	getRecipient := func(msgTx *wire.MsgTx, receive bool) *string {
		for _, txOut := range msgTx.TxOut {
			_, addrs := stdscript.ExtractAddrs(scriptVersion, txOut.PkScript, dcr.chainParams)
			if err != nil {
				dcr.log.Errorf("ExtractAddrs error: %w", err)
				continue
			}
			if len(addrs) != 1 { // sanity check
				continue
			}

			addr := addrs[0]
			owns, err := dcr.wallet.WalletOwnsAddress(ctx, addr)
			if err != nil {
				dcr.log.Errorf("walletOwnsAddress error: %w", err)
				continue
			}

			if receive == owns {
				str := addr.String()
				return &str
			}
		}
		return nil
	}

	txOutDirection := func(msgTx *wire.MsgTx) (in, out uint64) {
		for _, txOut := range msgTx.TxOut {
			_, addrs := stdscript.ExtractAddrs(scriptVersion, txOut.PkScript, dcr.chainParams)
			if err != nil {
				dcr.log.Errorf("ExtractAddrs error: %w", err)
				continue
			}
			if len(addrs) != 1 { // sanity check
				continue
			}

			addr := addrs[0]
			owns, err := dcr.wallet.WalletOwnsAddress(ctx, addr)
			if err != nil {
				dcr.log.Errorf("walletOwnsAddress error: %w", err)
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

	in, out := txOutDirection(tx.MsgTx)

	if ltxr.Send {
		txType := txdb.Send
		amt := out
		if allOutputsPayUs(tx.MsgTx) {
			txType = txdb.SelfSend
			amt = in
		}
		return &txdb.WalletTransaction{
			Type:      txType,
			ID:        ltxr.TxID,
			Amount:    amt,
			Fees:      fee,
			Recipient: getRecipient(tx.MsgTx, false),
		}, nil
	}

	return &txdb.WalletTransaction{
		Type:      txdb.Receive,
		ID:        ltxr.TxID,
		Amount:    in,
		Fees:      fee,
		Recipient: getRecipient(tx.MsgTx, true),
	}, nil
}

// addUnknownTransactionsToHistory checks for any transactions the wallet has
// made or received that are not part of the transaction history. It scans
// from the last point to which it had previously scanned to the current tip.
func (dcr *ExchangeWallet) addUnknownTransactionsToHistory(tip uint64) {
	txHistoryDB := dcr.txDB()

	const blockQueryBuffer = 3
	var blockToQuery uint64
	lastQuery := dcr.receiveTxLastQuery.Load()
	if lastQuery == 0 {
		// TODO: use wallet birthday instead of block 0.
		// blockToQuery = 0
	} else if lastQuery < tip-blockQueryBuffer {
		blockToQuery = lastQuery - blockQueryBuffer
	} else {
		blockToQuery = tip - blockQueryBuffer
	}

	txs, err := dcr.wallet.ListSinceBlock(dcr.ctx, int32(blockToQuery))
	if err != nil {
		dcr.log.Errorf("Error listing transactions since block %d: %v", blockToQuery, err)
		return
	}

	for _, tx := range txs {
		if dcr.ctx.Err() != nil {
			return
		}
		txHash, err := chainhash.NewHashFromStr(tx.TxID)
		if err != nil {
			dcr.log.Errorf("Error decoding tx hash %s: %v", tx.TxID, err)
			continue
		}
		_, err = txHistoryDB.GetTx(txHash.String())
		if err == nil {
			continue
		}
		if !errors.Is(err, asset.CoinNotFoundError) {
			dcr.log.Errorf("Error getting tx %s: %v", txHash.String(), err)
			continue
		}
		wt, err := dcr.idUnknownTx(dcr.ctx, &tx)
		if err != nil {
			dcr.log.Errorf("error identifying transaction: %v", err)
			continue
		}

		if tx.BlockIndex != nil && *tx.BlockIndex > 0 && *tx.BlockIndex < int64(tip-blockQueryBuffer) {
			wt.BlockNumber = uint64(*tx.BlockIndex)
			wt.Timestamp = uint64(tx.BlockTime)
		} else {
			wt.Timestamp = uint64(time.Now().Unix())
		}

		// Don't send notifications for the initial sync to avoid spamming the
		// front end. A notification is sent at the end of the initial sync.
		dcr.addTxToHistory(wt, txHash, blockToQuery == 0)
	}

	dcr.receiveTxLastQuery.Store(tip)
	err = txHistoryDB.SetLastReceiveTxQuery(tip)
	if err != nil {
		dcr.log.Errorf("Error setting last query to %d: %v", tip, err)
	}

	if blockToQuery == 0 {
		dcr.emit.TransactionHistorySyncedNote()
	}
}

// syncTxHistory checks to see if there are any transactions which the wallet
// has made or received that are not part of the transaction history, then
// identifies and adds them. It also checks all the pending transactions to see
// if they have been mined into a block, and if so, updates the transaction
// history to reflect the block height.
func (dcr *ExchangeWallet) syncTxHistory(ctx context.Context, tip uint64) {
	if !dcr.syncingTxHistory.CompareAndSwap(false, true) {
		return
	}
	defer dcr.syncingTxHistory.Store(false)

	txHistoryDB := dcr.txDB()
	if txHistoryDB == nil {
		return
	}

	ss, err := dcr.SyncStatus()
	if err != nil {
		dcr.log.Errorf("Error getting sync status: %v", err)
		return
	}
	if ss == nil || !ss.Synced {
		return
	}

	dcr.addUnknownTransactionsToHistory(tip)

	pendingTxsCopy := make(map[chainhash.Hash]btc.ExtendedWalletTx, len(dcr.pendingTxs))
	dcr.pendingTxsMtx.RLock()
	for hash, tx := range dcr.pendingTxs {
		pendingTxsCopy[hash] = *tx
	}
	dcr.pendingTxsMtx.RUnlock()

	handlePendingTx := func(txHash chainhash.Hash, tx *btc.ExtendedWalletTx) {
		gtr, err := dcr.wallet.GetTransaction(ctx, &txHash)
		if errors.Is(err, asset.CoinNotFoundError) {
			err = txHistoryDB.RemoveTx(txHash.String())
			if err == nil {
				dcr.pendingTxsMtx.Lock()
				delete(dcr.pendingTxs, txHash)
				dcr.pendingTxsMtx.Unlock()
			} else {
				// Leave it in the pendingPendingTxs and attempt to remove it
				// again next time.
				dcr.log.Errorf("Error removing tx %s from the history store: %v", txHash.String(), err)
			}
			return
		}
		if err != nil {
			dcr.log.Errorf("Error getting transaction %s: %v", txHash, err)
			return
		}

		var updated bool
		if gtr.BlockHash != "" {
			blockHash, err := chainhash.NewHashFromStr(gtr.BlockHash)
			if err != nil {
				dcr.log.Errorf("Error decoding block hash %s: %v", gtr.BlockHash, err)
				return
			}
			block, err := dcr.wallet.GetBlockHeader(ctx, blockHash)
			if err != nil {
				dcr.log.Errorf("Error getting block height for %s: %v", blockHash, err)
				return
			}
			blockHeight := block.Height
			if tx.BlockNumber != uint64(blockHeight) || tx.Timestamp != uint64(block.Timestamp.Unix()) {
				tx.BlockNumber = uint64(blockHeight)
				tx.Timestamp = uint64(block.Timestamp.Unix())
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
				dcr.log.Errorf("Error updating tx %s: %v", txHash, err)
				return
			}

			dcr.pendingTxsMtx.Lock()
			if tx.Confirmed {
				delete(dcr.pendingTxs, txHash)
			} else {
				dcr.pendingTxs[txHash] = tx
			}
			dcr.pendingTxsMtx.Unlock()

			dcr.emit.TransactionNote(tx.WalletTransaction, false)
		}
	}

	for hash, tx := range pendingTxsCopy {
		if dcr.ctx.Err() != nil {
			return
		}
		handlePendingTx(hash, &tx)
	}
}

func (dcr *ExchangeWallet) addTxToHistory(wt *txdb.WalletTransaction, txHash *chainhash.Hash, skipNotes ...bool) {
	txHistoryDB := dcr.txDB()
	if txHistoryDB == nil {
		return
	}

	ewt := &btc.ExtendedWalletTx{
		WalletTransaction: wt,
	}

	if wt.BlockNumber == 0 {
		dcr.pendingTxsMtx.Lock()
		dcr.pendingTxs[*txHash] = ewt
		dcr.pendingTxsMtx.Unlock()
	}

	if err := txHistoryDB.StoreTx(ewt); err != nil {
		dcr.log.Errorf("failed to store tx in tx history db: %v", err)
	}

	skipNote := len(skipNotes) > 0 && skipNotes[0]
	if !skipNote {
		dcr.emit.TransactionNote(wt, true)
	}
}

func (dcr *ExchangeWallet) checkPeers(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	numPeers, err := dcr.wallet.PeerCount(ctx)
	if err != nil { // e.g. dcrd passthrough fail in non-SPV mode
		prevPeer := atomic.SwapUint32(&dcr.lastPeerCount, 0)
		if prevPeer != 0 {
			dcr.log.Errorf("Failed to get peer count: %v", err)
			dcr.peersChange(0, err)
		}
		return
	}
	prevPeer := atomic.SwapUint32(&dcr.lastPeerCount, numPeers)
	if prevPeer != numPeers {
		dcr.peersChange(numPeers, nil)
	}
}

func (dcr *ExchangeWallet) monitorPeers(ctx context.Context) {
	ticker := time.NewTicker(peerCountTicker)
	defer ticker.Stop()
	for {
		dcr.checkPeers(ctx)

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (dcr *ExchangeWallet) emitTipChange(height int64) {
	var data any
	sinfo, err := dcr.wallet.StakeInfo(dcr.ctx)
	if err != nil {
		dcr.log.Errorf("Error getting stake info for tip change notification data: %v", err)
	} else {
		data = &struct {
			TicketPrice   uint64            `json:"ticketPrice"`
			VotingSubsidy uint64            `json:"votingSubsidy"`
			Stats         asset.TicketStats `json:"stats"`
		}{
			TicketPrice:   uint64(sinfo.Sdiff),
			Stats:         dcr.ticketStatsFromStakeInfo(sinfo),
			VotingSubsidy: dcr.voteSubsidy(height),
		}
	}

	dcr.emit.TipChange(uint64(height), data)
	go dcr.runTicketBuyer()
}

func (dcr *ExchangeWallet) emitBalance() {
	if bal, err := dcr.Balance(); err != nil {
		dcr.log.Errorf("Error getting balance after mempool tx notification: %v", err)
	} else {
		dcr.emit.BalanceChange(bal)
	}
}

// monitorBlocks pings for new blocks and runs the tipChange callback function
// when the block changes. New blocks are also scanned for potential contract
// redeems.
func (dcr *ExchangeWallet) monitorBlocks(ctx context.Context) {
	ticker := time.NewTicker(blockTicker)
	defer ticker.Stop()

	var walletBlock <-chan *block
	if notifier, isNotifier := dcr.wallet.(tipNotifier); isNotifier {
		walletBlock = notifier.tipFeed()
	}

	// A polledBlock is a block found during polling, but whose broadcast has
	// been queued in anticipation of a wallet notification.
	type polledBlock struct {
		*block
		queue *time.Timer
	}

	// queuedBlock is the currently queued, polling-discovered block that will
	// be broadcast after a timeout if the wallet doesn't send the matching
	// notification.
	var queuedBlock *polledBlock

	// checkTip captures queuedBlock and walletBlock.
	checkTip := func() {
		ctxInternal, cancel0 := context.WithTimeout(ctx, 4*time.Second)
		defer cancel0()

		newTip, err := dcr.getBestBlock(ctxInternal)
		if err != nil {
			dcr.log.Errorf("failed to get best block: %v", err)
			return
		}

		tip := dcr.cachedBestBlock()
		if tip == nil || tip.hash.IsEqual(newTip.hash) {
			return
		}

		if walletBlock == nil {
			dcr.handleTipChange(ctx, newTip.hash, newTip.height)
			return
		}

		// Queue it for reporting, but don't send it right away. Give the wallet
		// a chance to provide their block update. SPV wallet may need more time
		// after storing the block header to fetch and scan filters and issue
		// the FilteredBlockConnected report.
		if queuedBlock != nil {
			queuedBlock.queue.Stop()
		}
		blockAllowance := walletBlockAllowance
		ctxInternal, cancel1 := context.WithTimeout(ctx, 4*time.Second)
		defer cancel1()
		ss, err := dcr.wallet.SyncStatus(ctxInternal)
		if err != nil {
			dcr.log.Errorf("Error retrieving sync status before queuing polled block: %v", err)
		} else if !ss.Synced {
			blockAllowance *= 10
		}
		queuedBlock = &polledBlock{
			block: newTip,
			queue: time.AfterFunc(blockAllowance, func() {
				if ss, _ := dcr.SyncStatus(); ss != nil && ss.Synced {
					dcr.log.Warnf("Reporting a block found in polling that the wallet apparently "+
						"never reported: %s (%d). If you see this message repeatedly, it may indicate "+
						"an issue with the wallet.", newTip.hash, newTip.height)
				}
				dcr.handleTipChange(ctx, newTip.hash, newTip.height)
			}),
		}
	}

	for {
		select {
		case <-ticker.C:
			checkTip()

		case walletTip := <-walletBlock:
			if walletTip == nil {
				// Mempool tx seen.
				dcr.emitBalance()

				tip := dcr.cachedBestBlock()
				if tip != nil {
					dcr.syncTxHistory(ctx, uint64(tip.height))
				}
				continue
			}
			if queuedBlock != nil && walletTip.height >= queuedBlock.height {
				if !queuedBlock.queue.Stop() && walletTip.hash == queuedBlock.hash {
					continue
				}
				queuedBlock = nil
			}
			dcr.handleTipChange(ctx, walletTip.hash, walletTip.height)
		case <-ctx.Done():
			return
		}

		// Ensure context cancellation takes priority before the next iteration.
		if ctx.Err() != nil {
			return
		}
	}
}

func (dcr *ExchangeWallet) handleTipChange(ctx context.Context, newTipHash *chainhash.Hash, newTipHeight int64) {
	if dcr.ctx.Err() != nil {
		return
	}

	// Lock to avoid concurrent handleTipChange execution for simplicity.
	dcr.handleTipMtx.Lock()
	defer dcr.handleTipMtx.Unlock()

	prevTip := dcr.currentTip.Swap(&block{newTipHeight, newTipHash})
	var prevHeight int64
	var prevHash *chainhash.Hash
	if prevTip != nil {
		prevHeight = prevTip.height
		prevHash = prevTip.hash
	}
	dcr.log.Tracef("tip change: %d (%s) => %d (%s)", prevHeight, prevHash, newTipHeight, newTipHash)

	dcr.emitTipChange(newTipHeight)

	if dcr.cycleMixer != nil {
		dcr.cycleMixer()
	}

	dcr.wg.Add(1)
	go func() {
		dcr.syncTxHistory(ctx, uint64(newTipHeight))
		dcr.wg.Done()
	}()

	// Search for contract redemption in new blocks if there
	// are contracts pending redemption.
	dcr.findRedemptionMtx.RLock()
	pendingContractsCount := len(dcr.findRedemptionQueue)
	contractOutpoints := make([]outPoint, 0, pendingContractsCount)
	for contractOutpoint := range dcr.findRedemptionQueue {
		contractOutpoints = append(contractOutpoints, contractOutpoint)
	}
	dcr.findRedemptionMtx.RUnlock()
	if pendingContractsCount == 0 {
		return
	}

	startHeight := prevTip.height + 1

	// Redemption search would be compromised if the starting point cannot
	// be determined, as searching just the new tip might result in blocks
	// being omitted from the search operation. If that happens, cancel all
	// find redemption requests in queue.
	notifyFatalFindRedemptionError := func(s string, a ...any) {
		dcr.fatalFindRedemptionsError(fmt.Errorf("tipChange handler - "+s, a...), contractOutpoints)
	}

	// Check if the previous tip is still part of the mainchain (prevTip confs >= 0).
	// Redemption search would typically resume from prevTip.height + 1 unless the
	// previous tip was re-orged out of the mainchain, in which case redemption
	// search will resume from the mainchain ancestor of the previous tip.
	prevTipHeader, isMainchain, _, err := dcr.blockHeader(ctx, prevTip.hash)
	if err != nil {
		// Redemption search cannot continue reliably without knowing if there
		// was a reorg, cancel all find redemption requests in queue.
		notifyFatalFindRedemptionError("blockHeader error for prev tip hash %s: %w",
			prevTip.hash, err)
		return
	}
	if !isMainchain {
		// The previous tip is no longer part of the mainchain. Crawl blocks
		// backwards until finding a mainchain block. Start with the block
		// that is the immediate ancestor to the previous tip.
		ancestorBlockHash, ancestorHeight, err := dcr.mainchainAncestor(ctx, &prevTipHeader.PrevBlock)
		if err != nil {
			notifyFatalFindRedemptionError("find mainchain ancestor for prev block: %s: %w", prevTipHeader.PrevBlock, err)
			return
		}

		dcr.log.Debugf("reorg detected during tip change from height %d (%s) to %d (%s)",
			ancestorHeight, ancestorBlockHash, newTipHeight, newTipHash)

		startHeight = ancestorHeight // have to recheck orphaned blocks again
	}

	// Run the redemption search from the startHeight determined above up
	// till the current tip height.
	dcr.findRedemptionsInBlockRange(startHeight, newTipHeight, contractOutpoints)
}

func (dcr *ExchangeWallet) getBestBlock(ctx context.Context) (*block, error) {
	hash, height, err := dcr.wallet.GetBestBlock(ctx)
	if err != nil {
		return nil, err
	}
	return &block{hash: hash, height: height}, nil
}

// mainchainAncestor crawls blocks backwards starting at the provided hash
// until finding a mainchain block. Returns the first mainchain block found.
func (dcr *ExchangeWallet) mainchainAncestor(ctx context.Context, blockHash *chainhash.Hash) (*chainhash.Hash, int64, error) {
	checkHash := blockHash
	for {
		checkBlock, isMainchain, _, err := dcr.blockHeader(ctx, checkHash)
		if err != nil {
			return nil, 0, fmt.Errorf("getblockheader error for block %s: %w", checkHash, err)
		}
		if isMainchain {
			// This is a mainchain block, return the hash and height.
			return checkHash, int64(checkBlock.Height), nil
		}
		if checkBlock.Height == 0 {
			// Crawled back to genesis block without finding a mainchain ancestor
			// for the previous tip. Should never happen!
			return nil, 0, fmt.Errorf("no mainchain ancestor found for block %s", blockHash)
		}
		checkHash = &checkBlock.PrevBlock
	}
}

// blockHeader returns the *BlockHeader for the specified block hash, and bools
// indicating if the block is mainchain, and approved by stakeholders.
// validMainchain will always be false if mainchain is false; mainchain can be
// true for an invalidated block.
func (dcr *ExchangeWallet) blockHeader(ctx context.Context, blockHash *chainhash.Hash) (blockHeader *BlockHeader, mainchain, validMainchain bool, err error) {
	blockHeader, err = dcr.wallet.GetBlockHeader(ctx, blockHash)
	if err != nil {
		return nil, false, false, fmt.Errorf("GetBlockHeader error for block %s: %w", blockHash, err)
	}
	if blockHeader.Confirmations < 0 { // not mainchain, really just == -1, but catch all unexpected
		dcr.log.Warnf("Block %v is a SIDE CHAIN block at height %d!", blockHash, blockHeader.Height)
		return blockHeader, false, false, nil
	}

	// It's mainchain. Now check if there is a validating block.
	if blockHeader.NextHash == nil { // we're at the tip
		return blockHeader, true, true, nil
	}

	nextHeader, err := dcr.wallet.GetBlockHeader(ctx, blockHeader.NextHash)
	if err != nil {
		return nil, false, false, fmt.Errorf("error fetching validating block: %w", err)
	}

	validMainchain = nextHeader.VoteBits&1 != 0
	if !validMainchain {
		dcr.log.Warnf("Block %v found in mainchain, but stakeholder DISAPPROVED!", blockHash)
	}
	return blockHeader, true, validMainchain, nil
}

func (dcr *ExchangeWallet) cachedBestBlock() *block {
	return dcr.currentTip.Load()
}

// wireBytes dumps the serialized transaction bytes.
func (dcr *ExchangeWallet) wireBytes(tx *wire.MsgTx) []byte {
	s, err := tx.Bytes()
	// wireBytes is just used for logging, and a serialization error is
	// extremely unlikely, so just log the error and return the nil bytes.
	if err != nil {
		dcr.log.Errorf("error serializing transaction: %v", err)
	}
	return s
}

// Convert the DCR value to atoms.
func toAtoms(v float64) uint64 {
	return uint64(math.Round(v * conventionalConversionFactor))
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

// reduceMsgTx computes the total input and output amounts, the resulting
// absolute fee and fee rate, and the serialized transaction size.
func reduceMsgTx(tx *wire.MsgTx) (in, out, fees, rate, size uint64) {
	for _, txIn := range tx.TxIn {
		in += uint64(txIn.ValueIn)
	}
	for _, txOut := range tx.TxOut {
		out += uint64(txOut.Value)
	}
	fees = in - out
	size = uint64(tx.SerializeSize())
	rate = fees / size
	return
}

// toDCR returns a float representation in conventional units for the given
// atoms.
func toDCR[V uint64 | int64](v V) float64 {
	return dcrutil.Amount(v).ToCoin()
}

// WalletTransaction returns a transaction that either the wallet has made or
// one in which the wallet has received funds. The txID should be either a
// coin ID or a transaction hash in hexadecimal form.
func (dcr *ExchangeWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	coinID, err := hex.DecodeString(txID)
	if err == nil {
		txHash, _, err := decodeCoinID(coinID)
		if err == nil {
			txID = txHash.String()
		}
	}

	txHistoryDB := dcr.txDB()
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
	gtr, err := dcr.wallet.GetTransaction(ctx, txHash)
	if err != nil {
		return nil, fmt.Errorf("error getting transaction %s: %w", txID, err)
	}
	if len(gtr.Details) == 0 {
		return nil, fmt.Errorf("no details found for transaction %s", txID)
	}

	var blockHeight, blockTime uint64
	if gtr.BlockHash != "" {
		blockHash, err := chainhash.NewHashFromStr(gtr.BlockHash)
		if err != nil {
			return nil, fmt.Errorf("error decoding block hash %s: %w", gtr.BlockHash, err)
		}
		blockHeader, err := dcr.wallet.GetBlockHeader(ctx, blockHash)
		if err != nil {
			return nil, fmt.Errorf("error getting block header for block %s: %w", blockHash, err)
		}
		blockHeight = uint64(blockHeader.Height)
		blockTime = uint64(blockHeader.Timestamp.Unix())
	}

	updated := tx == nil
	if tx == nil {
		blockIndex := int64(blockHeight)
		regularTx := walletjson.LTTTRegular
		tx, err = dcr.idUnknownTx(ctx, &ListTransactionsResult{
			TxID:       txID,
			BlockIndex: &blockIndex,
			BlockTime:  int64(blockTime),
			Send:       gtr.Details[0].Category == "send",
			TxType:     &regularTx,
		})
		if err != nil {
			return nil, fmt.Errorf("xerror identifying transaction: %v", err)
		}
	}

	if tx.BlockNumber != blockHeight || tx.Timestamp != blockTime {
		tx.BlockNumber = blockHeight
		tx.Timestamp = blockTime
		updated = true
	}

	if updated {
		dcr.addTxToHistory(tx, txHash)
	}

	// If the wallet knows about the transaction, it will be part of the
	// available balance, so we always return Confirmed = true.
	tx.Confirmed = true
	return tx, nil
}

// PendingTransactions loads wallet transactions that are not yet confirmed.
func (dcr *ExchangeWallet) PendingTransactions(ctx context.Context) []*txdb.WalletTransaction {
	dcr.pendingTxsMtx.RLock()
	defer dcr.pendingTxsMtx.RUnlock()
	txs := make([]*txdb.WalletTransaction, 0, len(dcr.pendingTxs))
	for _, tx := range dcr.pendingTxs {
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
func (dcr *ExchangeWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	txHistoryDB := dcr.txDB()
	if txHistoryDB == nil {
		return nil, fmt.Errorf("tx database not initialized")
	}

	return txHistoryDB.GetTxs(req)
}

// RedeemGeocode redeems funds from a geocode game tx to this wallet.
func (dcr *ExchangeWallet) RedeemGeocode(code []byte, msg string) (util.Bytes, uint64, error) {
	msgLen := len([]byte(msg))
	if msgLen > stdscript.MaxDataCarrierSizeV0 {
		return nil, 0, fmt.Errorf("message is too long. must be %d > %d", msgLen, stdscript.MaxDataCarrierSizeV0)
	}

	k, err := hdkeychain.NewMaster(code, dcr.chainParams)
	if err != nil {
		return nil, 0, fmt.Errorf("error generating key from code: %w", err)
	}
	gameKey, err := k.SerializedPrivKey()
	if err != nil {
		return nil, 0, fmt.Errorf("error serializing private key: %w", err)
	}

	gamePub := k.SerializedPubKey()
	gameAddr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(dcrutil.Hash160(gamePub), dcr.chainParams)
	if err != nil {
		return nil, 0, fmt.Errorf("error generating address: %w", err)
	}

	gameTxs, err := getDcrdataTxs(dcr.ctx, gameAddr.String(), dcr.network)
	if err != nil {
		return nil, 0, fmt.Errorf("error getting tx from dcrdata: %w", err)
	}

	_, gameScript := gameAddr.PaymentScript()

	feeRate, err := dcr.feeRate(2)
	if err != nil {
		return nil, 0, fmt.Errorf("error getting tx fee rate: %w", err)
	}

	redeemTx := wire.NewMsgTx()
	var redeemable int64
	for _, gameTx := range gameTxs {
		txHash := gameTx.TxHash()
		for vout, txOut := range gameTx.TxOut {
			if bytes.Equal(txOut.PkScript, gameScript) {
				redeemable += txOut.Value
				prevOut := wire.NewOutPoint(&txHash, uint32(vout), wire.TxTreeRegular)
				redeemTx.AddTxIn(wire.NewTxIn(prevOut, txOut.Value, gameScript))
			}
		}
	}

	if len(redeemTx.TxIn) == 0 {
		return nil, 0, fmt.Errorf("no spendable game outputs found in %d txs for address %s", len(gameTxs), gameAddr)
	}

	var txSize uint64 = MsgTxOverhead + uint64(len(redeemTx.TxIn))*P2PKHInputSize + P2PKHOutputSize
	if msgLen > 0 {
		txSize += TxOutOverhead + 1 /* opreturn */ + uint64(wire.VarIntSerializeSize(uint64(msgLen))) + uint64(msgLen)
	}
	fees := feeRate * txSize

	if uint64(redeemable) < fees {
		return nil, 0, fmt.Errorf("estimated fees %d are less than the redeemable value %d", fees, redeemable)
	}
	win := uint64(redeemable) - fees
	if IsDustVal(P2PKHOutputSize, win, feeRate) {
		return nil, 0, fmt.Errorf("received value is dust after fees: %d - %d = %d", redeemable, fees, win)
	}

	redeemAddr, err := dcr.wallet.ExternalAddress(dcr.ctx, dcr.depositAccount())
	if err != nil {
		return nil, 0, fmt.Errorf("error getting redeem address: %w", err)
	}
	_, redeemScript := redeemAddr.PaymentScript()

	redeemTx.AddTxOut(wire.NewTxOut(int64(win), redeemScript))
	if msgLen > 0 {
		msgScript, err := txscript.NewScriptBuilder().AddOp(txscript.OP_RETURN).AddData([]byte(msg)).Script()
		if err != nil {
			return nil, 0, fmt.Errorf("error building message script: %w", err)
		}
		redeemTx.AddTxOut(wire.NewTxOut(0, msgScript))
	}

	for vin, txIn := range redeemTx.TxIn {
		redeemInSig, err := sign.RawTxInSignature(redeemTx, vin, gameScript, txscript.SigHashAll,
			gameKey, dcrec.STEcdsaSecp256k1)
		if err != nil {
			return nil, 0, fmt.Errorf("error creating signature for input script: %w", err)
		}
		txIn.SignatureScript, err = txscript.NewScriptBuilder().AddData(redeemInSig).AddData(gamePub).Script()
		if err != nil {
			return nil, 0, fmt.Errorf("error building p2pkh sig script: %w", err)
		}
	}

	redeemHash, err := dcr.broadcastTx(dcr.ctx, redeemTx, feeRate)
	if err != nil {
		return nil, 0, fmt.Errorf("error broadcasting tx: %w", err)
	}

	return ToCoinID(redeemHash, 0), win, nil
}

// AbandonTransaction implements asset.TxAbandoner, marking an unconfirmed
// transaction and all its descendants as abandoned. This allows the wallet to
// forget about the transaction and potentially spend its inputs in a different
// transaction.
func (dcr *ExchangeWallet) AbandonTransaction(ctx context.Context, txID string) error {
	txHash, err := chainhash.NewHashFromStr(txID)
	if err != nil {
		return fmt.Errorf("invalid transaction ID %q: %w", txID, err)
	}

	return dcr.wallet.AbandonTransaction(ctx, txHash)
}

func getDcrdataTxs(ctx context.Context, addr string, net util.Network) (txs []*wire.MsgTx, _ error) {
	apiRoot := "https://dcrdata.decred.org/api/"
	switch net {
	case util.Testnet:
		apiRoot = "https://testnet.decred.org/api/"
	case util.Simnet:
		apiRoot = "http://127.0.0.1:17779/api/"
	}

	var resp struct {
		Txs []struct {
			TxID string `json:"txid"`
		} `json:"address_transactions"`
	}
	if err := dexnet.Get(ctx, apiRoot+"address/"+addr, &resp); err != nil {
		return nil, fmt.Errorf("error getting address info for address %q: %w", addr, err)
	}
	for _, tx := range resp.Txs {
		txID := tx.TxID

		// tx/hex response is a hex string but is not JSON encoded.
		r, err := dexnet.Client.Get(apiRoot + "tx/hex/" + txID)
		if err != nil {
			return nil, fmt.Errorf("error getting transaction %q: %w", txID, err)
		}
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("error reading response body: %w", err)
		}
		_ = r.Body.Close()
		hexTx := string(b)
		txB, err := hex.DecodeString(hexTx)
		if err != nil {
			return nil, fmt.Errorf("error decoding hex for tx %q: %w", txID, err)
		}

		tx, err := msgTxFromBytes(txB)
		if err != nil {
			return nil, fmt.Errorf("error deserializing tx %x: %w", txID, err)
		}
		txs = append(txs, tx)
	}

	return
}

// SendFundsToMultisig sends amounts to a multisig address, then creates a
// transaction that spends those funds. ErrMultisigPartialSend and partial
// results are returned if an error happens after sending funds.
func (dcr *ExchangeWallet) SendFundsToMultisig(ctx context.Context, pm *asset.PaymentMultisig) (pmTx *asset.PaymentMultisigTx, err error) {
	xpubs := make([]*secp256k1.PublicKey, len(pm.SignerXpubs))
	for i, b := range pm.SignerXpubs {
		key, err := secp256k1.ParsePubKey(b)
		if err != nil {
			return nil, err
		}
		xpubs[i] = key
	}

	addrToValue := make(map[stdaddr.Address]uint64)
	var value uint64
	for addrStr, val := range pm.AddrToVal {
		amt, err := dcrutil.NewAmount(val)
		if err != nil {
			return nil, err
		}
		addr, err := stdaddr.DecodeAddress(addrStr, dcr.chainParams)
		if err != nil {
			return nil, err
		}
		addrToValue[addr] = uint64(amt)
		value += uint64(amt)
	}

	feeRate := dcr.targetFeeRateWithFallback(2, 0)
	redeemTxSize := PaymentMultisigRedeemTxSize(int64(len(xpubs)), pm.NRequired, int64(len(addrToValue)))
	redeemFee := feeRate * redeemTxSize
	value += redeemFee

	bal, err := dcr.balance()
	if err != nil {
		return nil, err
	}

	// Not adding fee for initial send but sending will fail anyway in that
	// case.
	if bal.Available < value {
		return nil, errors.New("not enough funds to send to multisig")
	}

	locktime := time.Unix(pm.Locktime, 0).UTC()
	// TODO: Remove this 3 day check.
	if locktime.After(time.Now().Add(time.Hour * 72)) {
		return nil, errors.New("locktime more than three days in the future, make it shorter")
	}

	addr, err := dcr.wallet.InternalAddress(dcr.ctx, dcr.depositAccount())
	if err != nil {
		return nil, err
	}

	redeemScript, err := MakePaymentMultisig(addr.String(), xpubs, pm.NRequired, pm.Locktime, dcr.chainParams)
	if err != nil {
		return nil, fmt.Errorf("unable to make multisig redeem script: %v", err)
	}
	dcr.log.Infof("Multisig redeem script: %x", redeemScript)

	scriptAddr, err := stdaddr.NewAddressScriptHashV0(redeemScript, dcr.chainParams)
	if err != nil {
		return nil, fmt.Errorf("error encoding script address: %w", err)
	}

	// Funds are sent. Point of no return.
	msgTx, _, _, err := dcr.sendToAddress(scriptAddr, value, feeRate)
	if err != nil {
		return nil, err
	}

	// Add a data push value to the beginning of the script so that we can
	// parse in the same way as a script with signatures.
	redeemScriptWithDataPush, err := txscript.NewScriptBuilder().AddData(redeemScript).Script()
	if err != nil {
		return nil, err
	}

	// Create the transaction that spends the contract.
	redeemTx := wire.NewMsgTx()
	redeemTx.LockTime = uint32(pm.Locktime)
	// TODO: Change is always last output?
	hash := msgTx.TxHash()
	prevOut := wire.NewOutPoint(&hash, 0, wire.TxTreeRegular)
	txIn := wire.NewTxIn(prevOut, int64(value), redeemScriptWithDataPush)
	// Enable the OP_CHECKLOCKTIMEVERIFY opcode to be used.
	//
	// https://github.com/decred/dcrd/blob/8f5270b707daaa1ecf24a1ba02b3ff8a762674d3/txscript/opcode.go#L981-L998
	txIn.Sequence = wire.MaxTxInSequenceNum - 1
	redeemTx.AddTxIn(txIn)

	for addr, amt := range addrToValue {
		pkScriptVer, pkScript := addr.PaymentScript()
		txOut := newTxOut(int64(amt), pkScriptVer, pkScript)
		redeemTx.AddTxOut(txOut)
	}

	hasSigs := make([]bool, len(xpubs))
	b, err := redeemTx.Bytes()
	if err != nil {
		return &asset.PaymentMultisigTx{
			TxHex:   spew.Sdump(redeemTx),
			HasSigs: hasSigs,
		}, asset.ErrMultisigPartialSend
	}

	return &asset.PaymentMultisigTx{
		TxHex:   hex.EncodeToString(b),
		HasSigs: hasSigs,
	}, nil
}

func insert(a [][]byte, index int, value []byte) [][]byte {
	if len(a) == index { // nil or empty slice or after last element
		return append(a, value)
	}
	a = append(a[:index+1], a[index:]...) // index < len(a)
	a[index] = value
	return a
}

// SignMultisig signs the pmTx with the supplied privateKey and inserts the
// signature at idx among the other signatures.
func (dcr *ExchangeWallet) SignMultisig(ctx context.Context, pmTx *asset.PaymentMultisigTx, privKey []byte) (*asset.PaymentMultisigTx, error) {
	// Decode the spending tx and extract values from it.
	txB, err := hex.DecodeString(pmTx.TxHex)
	if err != nil {
		return nil, err
	}
	msgTx := new(wire.MsgTx)
	if err := msgTx.FromBytes(txB); err != nil {
		return nil, err
	}
	if len(msgTx.TxIn) < 1 {
		return nil, errors.New("spending tx does not have an input")
	}

	redeemScript, sigs, err := SigsFromPaymentMultisig(msgTx.TxIn[0].SignatureScript)
	if err != nil {
		return nil, err
	}

	nRequired, _, _, pubKeys, err := ExtractPaymentMultisigDetails(redeemScript)
	if err != nil {
		return nil, err
	}

	if len(pmTx.HasSigs) != len(pubKeys) {
		return nil, errors.New("has sigs and number of xpubs different")
	}

	if nRequired <= int64(len(sigs)) {
		return nil, errors.New("already has enough signatures to send")
	}

	// Find our signature's placement.
	priv := secp256k1.PrivKeyFromBytes(privKey)
	defer priv.Zero()
	pubKeyB := priv.PubKey().SerializeCompressed()
	sigIdx := -1
	sigOffset := 0
	for i, b := range pubKeys {
		if pmTx.HasSigs[i] {
			sigOffset += 1
		}
		if bytes.Equal(pubKeyB, b.SerializeCompressed()) {
			sigIdx = i
			break
		}
	}
	if sigIdx < 0 {
		return nil, fmt.Errorf("signing pubkey %x for private key not found in signer xpubs", pubKeyB)
	}
	if sigOffset > len(sigs) {
		return nil, fmt.Errorf("not as many sigs found as expected")
	}
	if pmTx.HasSigs[sigIdx] {
		return nil, fmt.Errorf("already have a signature for pubkey %x", pubKeyB)
	}
	// Create our signature.
	sig, err := sign.RawTxInSignature(msgTx, 0, redeemScript, txscript.SigHashAll, privKey, dcrec.STEcdsaSecp256k1)
	if err != nil {
		return nil, err
	}
	// Add our signature to the others and repace the sig script.
	sigScript, err := RedeemPaymentMultisig(redeemScript, insert(sigs, sigOffset, sig))
	if err != nil {
		return nil, err
	}
	msgTx.TxIn[0].SignatureScript = sigScript
	b, err := msgTx.Bytes()
	if err != nil {
		return nil, err
	}

	pmTx.HasSigs[sigIdx] = true
	return &asset.PaymentMultisigTx{
		TxHex:   hex.EncodeToString(b),
		HasSigs: pmTx.HasSigs,
	}, nil
}

// RefundMultisig refunds a multisig if it is after the locktime and we are the
// sender.
func (dcr *ExchangeWallet) RefundMultisig(ctx context.Context, pmTx *asset.PaymentMultisigTx) (string, error) {
	// Decode the spending tx and extract values from it.
	txB, err := hex.DecodeString(pmTx.TxHex)
	if err != nil {
		return "", err
	}
	msgTx := new(wire.MsgTx)
	if err := msgTx.FromBytes(txB); err != nil {
		return "", err
	}
	if len(msgTx.TxIn) < 1 {
		return "", errors.New("spending tx does not have an input")
	}

	redeemScript, _, err := SigsFromPaymentMultisig(msgTx.TxIn[0].SignatureScript)
	if err != nil {
		return "", err
	}

	_, sender, locktime, pubKeys, err := ExtractPaymentMultisigDetails(redeemScript)
	if err != nil {
		return "", err
	}

	expired, err := dcr.LockTimeExpired(dcr.ctx, time.Unix(locktime, 0))
	if err != nil {
		return "", err
	}
	if !expired {
		return "", fmt.Errorf("locktime not yet expired. Expires at %v", time.Unix(locktime, 0))
	}

	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(sender[:], dcr.chainParams)
	if err != nil {
		return "", fmt.Errorf("error generating address: %v", err)
	}

	size := PaymentMultisigRefundTxSize(int64(len(pubKeys)))
	feeRate := dcr.targetFeeRateWithFallback(2, 0)
	refundFee := int64(feeRate) * int64(size)
	pkScriptVer, pkScript := addr.PaymentScript()
	txOut := newTxOut(msgTx.TxIn[0].ValueIn-refundFee, pkScriptVer, pkScript)
	msgTx.TxOut = []*wire.TxOut{txOut}
	msgTx.TxOut[0].Value -= refundFee

	// Create our signature.
	redeemSig, pubkey, err := dcr.createSig(ctx, msgTx, 0, redeemScript, addr)
	if err != nil {
		return "", err
	}
	sigScript, err := RefundPaymentMultisig(redeemScript, pubkey, redeemSig)
	if err != nil {
		return "", err
	}
	msgTx.TxIn[0].SignatureScript = sigScript

	txHash, err := dcr.wallet.SendRawTransaction(dcr.ctx, msgTx, true)
	if err != nil {
		return "", err
	}
	return txHash.String(), nil
}

// createVinList returns a slice of JSON objects for the inputs of the passed
// transaction.
func createVinList(mtx *wire.MsgTx, isTreasuryEnabled bool) []chainjson.Vin {
	// Treasurybase transactions only have a single txin by definition.
	//
	// NOTE: This check MUST come before the coinbase check because a
	// treasurybase will be identified as a coinbase as well.
	vinList := make([]chainjson.Vin, len(mtx.TxIn))
	if isTreasuryEnabled && blockchain.IsTreasuryBase(mtx) {
		txIn := mtx.TxIn[0]
		vinEntry := &vinList[0]
		vinEntry.Treasurybase = true
		vinEntry.Sequence = txIn.Sequence
		vinEntry.AmountIn = dcrutil.Amount(txIn.ValueIn).ToCoin()
		vinEntry.BlockHeight = txIn.BlockHeight
		vinEntry.BlockIndex = txIn.BlockIndex
		return vinList
	}

	// Coinbase transactions only have a single txin by definition.
	if blockchain.IsCoinBaseTx(mtx, isTreasuryEnabled) {
		txIn := mtx.TxIn[0]
		vinEntry := &vinList[0]
		vinEntry.Coinbase = hex.EncodeToString(txIn.SignatureScript)
		vinEntry.Sequence = txIn.Sequence
		vinEntry.AmountIn = dcrutil.Amount(txIn.ValueIn).ToCoin()
		vinEntry.BlockHeight = txIn.BlockHeight
		vinEntry.BlockIndex = txIn.BlockIndex
		return vinList
	}

	// Treasury spend transactions only have a single txin by definition.
	if isTreasuryEnabled && stake.IsTSpend(mtx) {
		txIn := mtx.TxIn[0]
		vinEntry := &vinList[0]
		vinEntry.TreasurySpend = hex.EncodeToString(txIn.SignatureScript)
		vinEntry.Sequence = txIn.Sequence
		vinEntry.AmountIn = dcrutil.Amount(txIn.ValueIn).ToCoin()
		vinEntry.BlockHeight = txIn.BlockHeight
		vinEntry.BlockIndex = txIn.BlockIndex
		return vinList
	}

	// Stakebase transactions (votes) have two inputs: a null stake base
	// followed by an input consuming a ticket's stakesubmission.
	isSSGen := stake.IsSSGen(mtx)

	for i, txIn := range mtx.TxIn {
		// Handle only the null input of a stakebase differently.
		if isSSGen && i == 0 {
			vinEntry := &vinList[0]
			vinEntry.Stakebase = hex.EncodeToString(txIn.SignatureScript)
			vinEntry.Sequence = txIn.Sequence
			vinEntry.AmountIn = dcrutil.Amount(txIn.ValueIn).ToCoin()
			vinEntry.BlockHeight = txIn.BlockHeight
			vinEntry.BlockIndex = txIn.BlockIndex
			continue
		}

		// The disassembled string will contain [error] inline
		// if the script doesn't fully parse, so ignore the
		// error here.
		disbuf, _ := txscript.DisasmString(txIn.SignatureScript)

		vinEntry := &vinList[i]
		vinEntry.Txid = txIn.PreviousOutPoint.Hash.String()
		vinEntry.Vout = txIn.PreviousOutPoint.Index
		vinEntry.Tree = txIn.PreviousOutPoint.Tree
		vinEntry.Sequence = txIn.Sequence
		vinEntry.AmountIn = dcrutil.Amount(txIn.ValueIn).ToCoin()
		vinEntry.BlockHeight = txIn.BlockHeight
		vinEntry.BlockIndex = txIn.BlockIndex
		vinEntry.ScriptSig = &chainjson.ScriptSig{
			Asm: disbuf,
			Hex: hex.EncodeToString(txIn.SignatureScript),
		}
	}

	return vinList
}

// createVoutList returns a slice of JSON objects for the outputs of the passed
// transaction.
func createVoutList(mtx *wire.MsgTx, chainParams *chaincfg.Params) ([]chainjson.Vout, error) {

	txType := stake.DetermineTxType(mtx)
	voutList := make([]chainjson.Vout, 0, len(mtx.TxOut))
	for i, v := range mtx.TxOut {
		// The disassembled string will contain [error] inline if the
		// script doesn't fully parse, so ignore the error here.
		disbuf, _ := txscript.DisasmString(v.PkScript)

		// Attempt to extract addresses from the public key script.  In
		// the case of stake submission transactions, the odd outputs
		// contain a commitment address, so detect that case
		// accordingly.
		var addrs []stdaddr.Address
		var scriptType string
		var reqSigs uint16
		var commitAmt *dcrutil.Amount
		if txType == stake.TxTypeSStx && (i%2 != 0) {
			scriptType = sstxCommitmentString
			addr, err := stake.AddrFromSStxPkScrCommitment(v.PkScript,
				chainParams)
			if err != nil {
				return nil, fmt.Errorf("failed to decode ticket "+
					"commitment addr output for tx hash "+
					"%v, output idx %v", mtx.TxHash(), i)
			} else {
				addrs = []stdaddr.Address{addr}
			}
			amt, err := stake.AmountFromSStxPkScrCommitment(v.PkScript)
			if err != nil {
				return nil, fmt.Errorf("failed to decode ticket "+
					"commitment amt output for tx hash %v"+
					", output idx %v", mtx.TxHash(), i)
			} else {
				commitAmt = &amt
			}
		} else {
			// Attempt to extract known addresses associated with the script.
			var st stdscript.ScriptType
			st, addrs = stdscript.ExtractAddrs(v.Version, v.PkScript, chainParams)
			scriptType = st.String()

			// Determine the number of required signatures for known standard
			// dcrdtypes.
			reqSigs = stdscript.DetermineRequiredSigs(v.Version, v.PkScript)
		}

		encodedAddrs := make([]string, len(addrs))
		for j, addr := range addrs {
			encodedAddr := addr.String()
			encodedAddrs[j] = encodedAddr
		}

		var vout chainjson.Vout
		voutSPK := &vout.ScriptPubKey
		vout.N = uint32(i)
		vout.Value = dcrutil.Amount(v.Value).ToCoin()
		vout.Version = v.Version
		voutSPK.Addresses = encodedAddrs
		voutSPK.Asm = disbuf
		voutSPK.Hex = hex.EncodeToString(v.PkScript)
		voutSPK.Type = scriptType
		voutSPK.ReqSigs = int32(reqSigs)
		if commitAmt != nil {
			voutSPK.CommitAmt = dcrjson.Float64(commitAmt.ToCoin())
		}
		voutSPK.Version = v.Version

		voutList = append(voutList, vout)
	}

	return voutList, nil
}

// decodeTx decodes a transaction from its hex.
func (dcr *ExchangeWallet) decodeTx(hexStr string) (*chainjson.TxRawDecodeResult, error) {
	if len(hexStr)%2 != 0 {
		hexStr = "0" + hexStr
	}
	serializedTx, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, err
	}
	var mtx wire.MsgTx
	err = mtx.Deserialize(bytes.NewReader(serializedTx))
	if err != nil {
		return nil, err
	}

	voutList, err := createVoutList(&mtx, dcr.chainParams)
	if err != nil {
		return nil, err
	}

	isTreasuryEnabled := true

	// Create and return the result.
	return &chainjson.TxRawDecodeResult{
		Txid:     mtx.TxHash().String(),
		Version:  int32(mtx.Version),
		Locktime: mtx.LockTime,
		Expiry:   mtx.Expiry,
		Vin:      createVinList(&mtx, isTreasuryEnabled),
		Vout:     voutList,
	}, nil
}

type ViewPM struct {
	Tx        *chainjson.TxRawDecodeResult `json:"tx"`
	Locktime  time.Time                    `json:"locktime"`
	Pubkeys   []string                     `json:"pubkeys"`
	NSigs     int                          `json:"nsigs"`
	NRequired int64                        `json:"nrequired"`
	Sender    string                       `json:"sender"`
}

// ViewPaymentMultisig returns a tx hex in human readable json format.
func (dcr *ExchangeWallet) ViewPaymentMultisig(pmTx *asset.PaymentMultisigTx) (string, error) {
	txB, err := hex.DecodeString(pmTx.TxHex)
	if err != nil {
		return "", err
	}
	msgTx := new(wire.MsgTx)
	if err := msgTx.FromBytes(txB); err != nil {
		return "", err
	}
	if len(msgTx.TxIn) < 1 {
		return "", errors.New("spending tx does not have an input")
	}

	redeemScript, sigs, err := SigsFromPaymentMultisig(msgTx.TxIn[0].SignatureScript)
	if err != nil {
		return "", err
	}

	nRequired, sender, locktime, pubKeys, err := ExtractPaymentMultisigDetails(redeemScript)
	if err != nil {
		return "", err
	}

	pks := make([]string, len(pubKeys))
	for i, pk := range pubKeys {
		pks[i] = hex.EncodeToString(pk.SerializeCompressed())
	}

	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(sender[:], dcr.chainParams)
	if err != nil {
		return "", fmt.Errorf("error generating address: %v", err)
	}

	dTx, err := dcr.decodeTx(pmTx.TxHex)
	if err != nil {
		return "", err
	}

	viewPm := ViewPM{
		Tx:        dTx,
		Locktime:  time.Unix(locktime, 0),
		Pubkeys:   pks,
		NSigs:     len(sigs),
		NRequired: nRequired,
		Sender:    addr.String(),
	}

	b, err := json.MarshalIndent(viewPm, "", "\t")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
