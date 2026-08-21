// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
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
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/keygen"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/erc20"
	multibal "github.com/bisoncraft/caribou/wallet/asset/eth/contracts/multibalance"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/bisoncraft/go-bip39"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/decred/dcrd/hdkeychain/v3"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	ethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

func registerToken(tokenID uint32, desc string) {
	token, found := Tokens[tokenID]
	if !found {
		panic("token " + strconv.Itoa(int(tokenID)) + " not known")
	}
	netAddrs := make(map[util.Network]string)
	netAssetVersions := make(map[util.Network][]uint32, 3)
	for net, netToken := range token.NetTokens {
		netAddrs[net] = netToken.Address.String()
		netAssetVersions[net] = WalletInfo.SupportedVersions
	}
	asset.RegisterToken(tokenID, token.Token, &asset.WalletDefinition{
		Type:        walletTypeToken,
		Tab:         "Ethereum token",
		Description: desc,
	}, netAddrs, netAssetVersions)
}

var (
	// Ethereum
	ethID, _        = util.BipSymbolID("eth")
	usdcEthID, _    = util.BipSymbolID("usdc.eth")
	usdtEthID, _    = util.BipSymbolID("usdt.eth")
	maticEthID, _   = util.BipSymbolID("matic.eth")
	polygonEthID, _ = util.BipSymbolID("polygon.eth")
	linkEthID, _    = util.BipSymbolID("link.eth")
	paxgEthID, _    = util.BipSymbolID("paxg.eth")
	xautEthID, _    = util.BipSymbolID("xaut.eth")
	// Polygon
	polygonID, _     = util.BipSymbolID("polygon")
	wethPolygonID, _ = util.BipSymbolID("weth.polygon")
	usdcPolygonID, _ = util.BipSymbolID("usdc.polygon")
	usdtPolygonID, _ = util.BipSymbolID("usdt.polygon")
	// Base
	baseID, _     = util.BipSymbolID("base")
	usdcBaseID, _ = util.BipSymbolID("usdc.base")
	usdtBaseID, _ = util.BipSymbolID("usdt.base")
)

func init() {
	MaybeReadSimnetAddrs()
	asset.Register(BipID, &Driver{})
	registerToken(usdcEthID, "The USDC Ethereum ERC20 token.")
	registerToken(usdtEthID, "The USDT Ethereum ERC20 token.")
	registerToken(maticEthID, "The MATIC Ethereum ERC20 token.")
	registerToken(polygonEthID, "The POL Ethereum ERC20 token.")
	registerToken(linkEthID, "The LINK Chainlink Ethereum ERC20 token.")
	registerToken(paxgEthID, "The PAXG PAX Gold Ethereum ERC20 token.")
	registerToken(xautEthID, "The XAUT Tether Gold Ethereum ERC20 token.")
}

const (
	// BipID is the BIP-0044 asset ID for Ethereum.
	BipID               = 60
	defaultGasFee       = 82  // gwei
	defaultGasFeeLimit  = 200 // gwei
	defaultSendGasLimit = 21_000
	// liveGasLimitBufferPct and liveGasLimitBufferMin are the multiplicative
	// and additive buffers applied to a live eth_estimateGas result to produce
	// a transaction's gas limit. See bufferedGasLimit.
	liveGasLimitBufferPct = 20
	liveGasLimitBufferMin = 20_000

	walletTypeGeth  = "geth"
	walletTypeRPC   = "rpc"
	walletTypeToken = "token"

	providersKey = "providers"

	// onChainDataFetchTimeout is the max amount of time allocated to fetching
	// on-chain data. Testing on testnet has shown spikes up to 2.5 seconds
	// (but on internet, with Tor, it could actually take up to 30 seconds easily).
	// Setting it to 10 seconds for now until https://github.com/decred/dcrdex/issues/3184
	// is resolved.
	onChainDataFetchTimeout = 10 * time.Second

	// coinIDTakerFoundMakerRedemption is a prefix to identify one of CoinID formats,
	// see DecodeCoinID func for details.
	coinIDTakerFoundMakerRedemption = "TakerFoundMakerRedemption:"

	LiveEstimateFailedError = util.ErrorKind("live gas estimate failed")

	// txAgeOut is the amount of time after which we forego any tx
	// synchronization efforts for unconfirmed pending txs.
	txAgeOut = 2 * time.Hour
	// chainOpTick is used by explicit chain wait/recheck loops that are not
	// part of the steady-state tip monitoring path.
	chainOpTick = time.Second
	// wsHeadStaleness is how long the shared header feed may go quiet before
	// monitorBlocks resumes polling the tip itself. Every provider pushes to that
	// one channel, so this expires only when all of their subscriptions have gone
	// silent. It has to exceed the block time of every chain the EVM wallet
	// drives, ethereum's ~12s being the slowest, by enough margin that ordinary
	// variance never reads as a dead feed. At 30s a genuinely dead feed costs at
	// most half a minute of blindness, against a bestHeader call and a no-op
	// applyTipChange behind it every single second for the whole session
	// otherwise.
	wsHeadStaleness = 30 * time.Second
	// maxUnindexedTxs is the number of pending txs we will allow to be
	// unverified on-chain before we halt broadcasting of new txs.
	maxUnindexedTxs = 10
	peerCountTicker = 5 * time.Second // no rpc calls here
)

// bufferedGasLimit converts a live eth_estimateGas result into a gas limit:
// the greater of estimate + liveGasLimitBufferPct percent and estimate +
// liveGasLimitBufferMin.
//
// eth_estimateGas binary-searches for the lowest limit at which the call
// succeeds against current state (geth allows itself ~1.5% overestimate), so
// any headroom has to be added by the caller. The buffer covers state drift
// between estimation and inclusion. The dominant hazard for ERC20 transfer and
// approve is a recipient balance or spender allowance slot that is non-zero at
// estimation time but zero at inclusion: the SSTORE then costs 20,000 instead
// of 2,900 (+2,100 cold access), +17,100 gas. A percentage alone does not cover
// that on a ~50k-gas call, hence the additive floor. The percentage covers
// upgradable token contracts and nested-call (EIP-150 63/64) slack.
//
// Wallet and tooling convention ranges from 1.1x (OpenZeppelin Relayer) through
// 1.2x (Uniswap interface) and 1.3x (Foundry) to 1.5x (MetaMask, Rabby);
// libraries (geth bind, ethers, viem, web3.js) add nothing. Unused gas is
// refunded, so the buffer only raises the balance reserved up front, not the
// fee paid.
func bufferedGasLimit(estimate uint64) uint64 {
	return max(estimate*(100+liveGasLimitBufferPct)/100, estimate+liveGasLimitBufferMin)
}

// GasFeeConfigOption returns the gasfeelimit ConfigOption with the supplied
// default (gwei / gas).
func GasFeeConfigOption(defaultGwei uint64) *asset.ConfigOption {
	return &asset.ConfigOption{
		Key:         "gasfeelimit",
		DisplayName: "Gas Fee Limit",
		Description: "This is the highest network fee rate you are willing to " +
			"pay on swap transactions. If gasfeelimit is lower than a market's " +
			"maxfeerate, you will not be able to trade on that market with this " +
			"wallet. Units: gwei / gas",
		DefaultValue: strconv.FormatUint(defaultGwei, 10),
	}
}

var (
	walletOpts = []*asset.ConfigOption{
		GasFeeConfigOption(defaultGasFeeLimit),
	}
	RPCOpts = []*asset.ConfigOption{
		{
			Key:         providersKey,
			DisplayName: "RPC Provider",
			Description: "Specify one or more RPC providers. For infrastructure " +
				"providers, prefer using wss address. Only url-based authentication " +
				"is supported. For a local node, use the filepath to an IPC file.",
			Repeatable:   providerDelimiter,
			RepeatN:      2,
			DefaultValue: "",
		},
	}
	rpcWalletDefinition = &asset.WalletDefinition{
		Type:        walletTypeRPC,
		Tab:         "RPC",
		Description: "Infrastructure providers (e.g. Infura) or local nodes",
		ConfigOpts:  append(RPCOpts, walletOpts...),
		Seeded:      true,
		GuideLink:   "https://github.com/decred/dcrdex/wiki/Wallet#rpc-providers-for-evm-compatible-networks",
	}

	// WalletInfo defines some general information about a Ethereum wallet.
	WalletInfo = asset.WalletInfo{
		Name: "Ethereum",
		// SupportedVersions: For Ethereum, the server backend maintains a
		// single protocol version, so tokens and ETH have the same set of
		// supported versions. Even though the SupportedVersions are made
		// accessible for tokens via (*TokenWallet).Info, the versions are not
		// exposed though any Driver methods or assets/driver functions. Use the
		// network wallet's WalletInfo via (*Driver).Info if you need a token's
		// supported versions before a wallet is available.
		SupportedVersions: []uint32{0, 1},
		UnitInfo:          UnitInfo,
		AvailableWallets: []*asset.WalletDefinition{
			// {
			// 	Type:        walletTypeGeth,
			// 	Tab:         "Native",
			// 	Description: "Use the built-in DEX wallet (geth light node)",
			// 	ConfigOpts:  WalletOpts,
			// 	Seeded:      true,
			// },
			rpcWalletDefinition,
		},
		BlockchainClass: asset.BlockchainClassEVM,
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
			Priority:      true,
		},
	}

	// unlimitedAllowance is the maximum supported allowance for an erc20
	// contract, and is effectively unlimited.
	unlimitedAllowance = ethmath.MaxBig256
	// unlimitedAllowanceReplenishThreshold is the threshold below which we will
	// require a new approval. In practice, this will never be hit, but any
	// allowance below this will signal that WE didn't set it, and we'll require
	// an upgrade to unlimited (since we don't support limited allowance yet).
	unlimitedAllowanceReplenishThreshold = new(big.Int).Div(unlimitedAllowance, big.NewInt(2))

	seedDerivationPath = []uint32{
		hdkeychain.HardenedKeyStart + 44, // purpose 44' for HD wallets
		hdkeychain.HardenedKeyStart + 60, // eth coin type 60'
		hdkeychain.HardenedKeyStart,      // account 0'
		0,                                // branch 0
		0,                                // index 0
	}
)

// safeConfs returns the confirmations for a given tip and block number,
// returning 0 if the block number is zero or if the tip is lower than the
// block number.
func safeConfs(tip, blockNum uint64) uint64 {
	if blockNum == 0 {
		return 0
	}
	if tip < blockNum {
		return 0
	}
	return tip - blockNum + 1
}

// WalletConfig are wallet-level configuration settings.
type WalletConfig struct {
	GasFeeLimit uint64 `ini:"gasfeelimit"`
}

// parseWalletConfig parses the settings map into a *WalletConfig.
func parseWalletConfig(settings map[string]string) (cfg *WalletConfig, err error) {
	cfg = new(WalletConfig)
	err = config.Unmapify(settings, &cfg)
	if err != nil {
		return nil, fmt.Errorf("error parsing wallet config: %w", err)
	}
	return cfg, nil
}

// Driver implements asset.Driver.
type Driver struct{}

// Check that Driver implements Driver and Creator.
var _ asset.Driver = (*Driver)(nil)
var _ asset.Creator = (*Driver)(nil)

// Open opens the ETH exchange wallet. Start the wallet with its Run method.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	return newWallet(cfg, logger, network)
}

// DecodeCoinID creates a human-readable representation of a coin ID for Ethereum.
// These are supported coin ID formats:
//  1. A transaction hash. 32 bytes
//  2. An encoded ETH funding coin id which includes the account address and
//     amount. 20 + 8 = 28 bytes
//  3. An encoded token funding coin id which includes the account address,
//     a token value, and fees. 20 + 8 + 8 = 36 bytes
//  4. A byte encoded string of the account address. 40 or 42 (with 0x) bytes
//  5. A byte encoded string which represents specific case where Taker found
//     Maker redemption on his own (while Maker failed to notify him about it
//     first). 26 (`TakerFoundMakerRedemption:` prefix) + 42 (Maker address
//     with 0x) bytes
func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	switch len(coinID) {
	case common.HashLength:
		var txHash common.Hash
		copy(txHash[:], coinID)
		return txHash.String(), nil
	case fundingCoinIDSize:
		c, err := decodeFundingCoin(coinID)
		if err != nil {
			return "", err
		}
		return c.String(), nil
	case tokenFundingCoinIDSize:
		c, err := decodeTokenFundingCoin(coinID)
		if err != nil {
			return "", err
		}
		return c.String(), nil
	case common.AddressLength * 2, common.AddressLength*2 + 2:
		hexAddr := string(coinID)
		if !common.IsHexAddress(hexAddr) {
			return "", fmt.Errorf("invalid hex address %q", coinID)
		}
		return common.HexToAddress(hexAddr).String(), nil
	case len(coinIDTakerFoundMakerRedemption) + common.AddressLength*2 + 2:
		coinIDStr := string(coinID)
		if !strings.HasPrefix(coinIDStr, coinIDTakerFoundMakerRedemption) {
			return "", fmt.Errorf("coinID %q has no %s prefix", coinID, coinIDTakerFoundMakerRedemption)
		}
		return coinIDStr, nil
	}

	return "", fmt.Errorf("unknown coin ID format: %x", coinID)
}

// Info returns basic information about the wallet and asset.
func (d *Driver) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

// Exists checks the existence of the wallet.
func (d *Driver) Exists(walletType, dataDir string, settings map[string]string, net util.Network) (bool, error) {
	switch walletType {
	case walletTypeGeth, walletTypeRPC:
	default:
		return false, fmt.Errorf("wallet type %q unrecognized", walletType)
	}

	keyStoreDir := filepath.Join(getWalletDir(dataDir, net), "keystore")
	ks := keystore.NewKeyStore(keyStoreDir, keystore.LightScryptN, keystore.LightScryptP)
	// NOTE: If the keystore did not exist, a warning from an internal KeyStore
	// goroutine may be printed to this effect. Not an issue.
	return len(ks.Wallets()) > 0, nil
}

func (d *Driver) Create(cfg *asset.CreateWalletParams) error {
	comp, err := NetworkCompatibilityData(cfg.Net)
	if err != nil {
		return fmt.Errorf("error finding compatibility data: %v", err)
	}
	return CreateEVMWallet(ChainIDs[cfg.Net], cfg, &comp, false)
}

// Balance is the current balance, including information about the pending
// balance.
type Balance struct {
	Current, PendingIn, PendingOut *big.Int
}

// ethFetcher represents a blockchain information fetcher. In practice, it is
// satisfied by *multiRPCClient. For testing, it can be satisfied by a stub.
type ethFetcher interface {
	address() common.Address
	addressBalance(ctx context.Context, addr common.Address) (*big.Int, error)
	// addressBalanceAt is addressBalance pinned to a specific block, used by
	// the incoming-transfer scan's balance-diff gate (scan.go) so the balance
	// compared against always corresponds exactly to the block range being
	// scanned, rather than whatever the node considers "latest" at query
	// time.
	addressBalanceAt(ctx context.Context, addr common.Address, blockNumber *big.Int) (*big.Int, error)
	bestHeader(ctx context.Context) (*types.Header, error)
	chainConfig() *params.ChainConfig
	connect(ctx context.Context) error
	peerCount() uint32
	contractBackend() bind.ContractBackend
	headerByHash(ctx context.Context, txHash common.Hash) (*types.Header, error)
	lock() error
	locked() bool
	shutdown()
	sendSignedTransaction(ctx context.Context, tx *types.Transaction, filts ...acceptabilityFilter) error
	sendTransaction(ctx context.Context, txOpts *bind.TransactOpts, to *common.Address, data []byte, filts ...acceptabilityFilter) (*types.Transaction, error)
	EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error)
	signData(data []byte) (sig, pubKey []byte, err error)
	signHash(hash []byte) (sig, pubKey []byte, err error)
	syncProgress(context.Context) (progress *ethereum.SyncProgress, tipTime uint64, err error)
	transactionConfirmations(context.Context, common.Hash) (uint32, error)
	getTransaction(context.Context, common.Hash) (*types.Transaction, int64, error)
	txOpts(ctx context.Context, val, maxGas uint64, maxFeeRate, tipCap, nonce *big.Int) (*bind.TransactOpts, error)
	currentFees(ctx context.Context) (baseFees, tipCap *big.Int, err error)
	unlock(pw string) error
	getConfirmedNonce(context.Context) (uint64, error)
	transactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
	transactionAndReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, *types.Transaction, error)
	nonce(ctx context.Context) (confirmed, next *big.Int, err error)
	// FilterLogs and blockByNumber are used by the incoming-transfer scan
	// (scan.go) to discover ERC20 Transfer events and native-value transfers
	// respectively.
	FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error)
	blockByNumber(ctx context.Context, n *big.Int) (*types.Block, error)
	// l1FeeForCalldata estimates the L1 fee for the given calldata using
	// the OP Stack GasPriceOracle precompile.
	l1FeeForCalldata(ctx context.Context, calldata []byte) (*big.Int, error)
	// l1FeeFromReceipt returns the L1 fee actually paid for a transaction,
	// extracted from the receipt via eth_getTransactionReceipt.
	// NOTE: Uses the deprecated l1Fee receipt field. See multirpc.go.
	l1FeeFromReceipt(ctx context.Context, txHash common.Hash) (*big.Int, error)
}

// txPoolFetcher can be implemented by node types that support fetching of
// txpool transactions.
type txPoolFetcher interface {
	pendingTransactions() ([]*types.Transaction, error)
}

type pendingApproval struct {
	txHash    common.Hash
	onConfirm func()
}

type cachedBalance struct {
	stamp  time.Time
	height uint64
	bal    *big.Int
}

// txFetchResult holds pre-fetched data for a pending tx confirmation check,
// allowing transactionAndReceipt (and headerByHash) to run outside nonceMtx.
type txFetchResult struct {
	receipt *types.Receipt
	tx      *types.Transaction
	// hdr is non-nil when a block header was fetched (needed when Timestamp==0
	// or when the receipt's EffectiveGasPrice is nil).
	hdr *types.Header
	err error
}

// Check that assetWallet satisfies the asset.Wallet interface.
var _ asset.Wallet = (*ETHWallet)(nil)
var _ asset.Wallet = (*TokenWallet)(nil)
var _ asset.TokenMaster = (*ETHWallet)(nil)
var _ asset.WalletRestorer = (*ETHWallet)(nil)
var _ asset.LiveReconfigurer = (*ETHWallet)(nil)
var _ asset.LiveReconfigurer = (*TokenWallet)(nil)
var _ asset.TxFeeEstimator = (*ETHWallet)(nil)
var _ asset.TxFeeEstimator = (*TokenWallet)(nil)
var _ asset.Authenticator = (*ETHWallet)(nil)

type baseWallet struct {
	// The asset subsystem starts with Connect(ctx). This ctx will be initialized
	// in parent ETHWallet once and re-used in child TokenWallet instances.
	ctx        context.Context
	net        util.Network
	node       ethFetcher
	addr       common.Address
	log        util.Logger
	dir        string
	walletType string
	torProxy   string

	// privKeyFn derives the raw private key from the wallet seed. Defaults to
	// the package's own BIP-39-based derivation; overridden by chains (e.g.
	// hyperevm) that must derive the same key as another registered asset.
	privKeyFn func([]byte) ([]byte, func(), error)

	finalizeConfs uint64

	multiBalanceAddress  common.Address
	multiBalanceContract *multibal.MultiBalanceV0

	baseChainID  uint32
	chainCfg     *params.ChainConfig
	chainID      int64
	compat       *CompatibilityData
	tokens       map[uint32]*Token
	maxTxFeeGwei uint64
	isOpStack    bool

	startingBlocks atomic.Uint64

	tipMtx     sync.RWMutex
	currentTip *types.Header

	settingsMtx sync.RWMutex
	settings    map[string]string

	gasFeeLimitV uint64 // atomic

	walletsMtx sync.RWMutex
	wallets    map[uint32]*assetWallet

	nonceMtx            sync.RWMutex
	pendingTxs          []*extendedWalletTx
	confirmedNonceAt    *big.Int
	pendingNonceAt      *big.Int
	recoveryRequestSent bool

	balances struct {
		sync.Mutex
		m map[uint32]*cachedBalance
	}

	currentFees struct {
		sync.Mutex
		blockNum uint64
		baseRate *big.Int
		tipRate  *big.Int
	}

	pendingTxCheckRunning atomic.Bool
	pendingTxCheckQueued  atomic.Bool

	l1Fees struct {
		sync.Mutex
		sendBlockNum uint64
		sendL1Fee    uint64 // gwei, for a simple send tx
	}

	txDB txDB
	// netTxDB is the master transaction database handle supplied by Core,
	// scoped to this wallet's base-chain network. It backs txDB.
	netTxDB txdb.NetworkTxDB
	// metaTable holds auxiliary per-chain state, such as the incoming-transfer
	// scan cursor (see scan.go), that isn't itself a transaction.
	metaTable txdb.StoreTable

	bridges       map[string]bridge
	bridgeManager *bridgeManager
}

// assetWallet is a wallet backend for Ethereum and Eth tokens. The backend is
// how Caribou communicates with the Ethereum blockchain and wallet.
// assetWallet satisfies the util.Wallet interface.
type assetWallet struct {
	*baseWallet
	assetID   uint32
	emit      *asset.WalletEmitter
	log       util.Logger
	ui        util.UnitInfo
	connected atomic.Bool
	wi        asset.WalletInfo
	tokenAddr common.Address // empty address for base chain asset

	// gasEstimate is the token's typical gas usage, used for fee estimation
	// only. nil for the base-chain asset.
	gasEstimate *Gases

	lockedFunds struct {
		mtx                sync.RWMutex
		initiateReserves   uint64
		redemptionReserves uint64
		refundReserves     uint64
	}

	approvalsMtx     sync.RWMutex
	pendingApprovals map[common.Address]*pendingApproval
	approvalCache    map[common.Address]bool

	lastPeerCount uint32
	peersChange   func(uint32, error)

	// tokenContractor interacts with the token's ERC20 contract for balance,
	// transfer, and tx-history features. nil for the base-chain asset.
	tokenContractor tokenContractor

	evmify  func(uint64) *big.Int
	atomize func(*big.Int) uint64

	// pendingTxCheckBal is protected by the nonceMtx. We use this field
	// as a secondary check to see if we need to request confirmations for
	// pending txs, since tips are cached for up to 10 seconds. We check the
	// status of pending txs if the tip has changed OR if the balance has
	// changed.
	pendingTxCheckBal *big.Int
}

// ETHWallet implements some Ethereum-specific methods.
type ETHWallet struct {
	// 64-bit atomic variables first. See
	// https://golang.org/pkg/sync/atomic/#pkg-note-BUG
	tipAtConnect     int64
	defaultProviders []string
	pollingTip       atomic.Bool

	*assetWallet

	// lastScanBalance is the confirmed native balance as of the last
	// incoming-transfer scan tick (see scan.go). It is only read and written
	// by the single scanIncomingTransfers goroutine, so it needs no lock.
	lastScanBalance *big.Int

	// nativeScanBackoff and erc20ScanBackoff cool down each half of the
	// incoming-transfer scan independently after a failure (see scan.go).
	nativeScanBackoff scanBackoff
	erc20ScanBackoff  scanBackoff
}

// TokenWallet implements some token-specific methods.
type TokenWallet struct {
	*assetWallet

	cfg      *tokenWalletConfig
	parent   *assetWallet
	token    *Token
	netToken *NetToken
}

// Info returns basic information about the wallet and asset.
func (w *assetWallet) Info() *asset.WalletInfo {
	wi := w.wi
	return &wi
}

// genWalletSeed uses the wallet seed passed from core as the entropy for
// generating a BIP-39 mnemonic. Then it returns the wallet seed generated
// from the mnemonic which can be used to derive a private key.
func genWalletSeed(entropy []byte) ([]byte, error) {
	if len(entropy) < 32 || len(entropy) > 64 {
		return nil, fmt.Errorf("wallet entropy must be 32 to 64 bytes long")
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, fmt.Errorf("error deriving mnemonic: %w", err)
	}
	return bip39.NewSeed(mnemonic, ""), nil
}

func privKeyFromSeed(seed []byte) (pk []byte, zero func(), err error) {
	walletSeed, err := genWalletSeed(seed)
	if err != nil {
		return nil, nil, err
	}
	defer encode.ClearBytes(walletSeed)

	extKey, err := keygen.GenDeepChild(walletSeed, seedDerivationPath)
	if err != nil {
		return nil, nil, err
	}
	// defer extKey.Zero()
	pk, err = extKey.SerializedPrivKey()
	if err != nil {
		extKey.Zero()
		return nil, nil, err
	}
	return pk, extKey.Zero, nil
}

// CreateEVMWallet creates a wallet using the package's standard BIP-39-based
// key derivation. See CreateEVMWalletKeyed for chains that must derive a
// different key.
func CreateEVMWallet(chainID int64, createWalletParams *asset.CreateWalletParams, compat *CompatibilityData, skipConnect bool) error {
	return CreateEVMWalletKeyed(chainID, createWalletParams, compat, skipConnect, privKeyFromSeed)
}

// CreateEVMWalletKeyed is CreateEVMWallet for chains whose keys are not
// derived by the standard eth path. keyFn derives the raw private key from
// the wallet seed.
func CreateEVMWalletKeyed(chainID int64, createWalletParams *asset.CreateWalletParams, compat *CompatibilityData, skipConnect bool, keyFn func([]byte) ([]byte, func(), error)) error {
	switch createWalletParams.Type {
	case walletTypeGeth:
		return asset.ErrWalletTypeDisabled
	case walletTypeRPC:
	default:
		return fmt.Errorf("wallet type %q unrecognized", createWalletParams.Type)
	}

	walletDir := getWalletDir(createWalletParams.DataDir, createWalletParams.Net)

	privateKey, zero, err := keyFn(createWalletParams.Seed)
	if err != nil {
		return err
	}
	defer zero()

	switch createWalletParams.Type {
	case walletTypeRPC:
		// Make the wallet dir if it does not exist, otherwise we may fail to
		// write the compliant-providers.json file. Create the keystore
		// subdirectory as well to avoid a "failed to watch keystore folder"
		// error from the keystore's internal account cache supervisor.
		keystoreDir := filepath.Join(walletDir, "keystore")
		if err := os.MkdirAll(keystoreDir, 0700); err != nil {
			return err
		}

		// TODO: This procedure may actually work for walletTypeGeth too.
		ks := keystore.NewKeyStore(keystoreDir, keystore.LightScryptN, keystore.LightScryptP)

		priv, err := crypto.ToECDSA(privateKey)
		if err != nil {
			return err
		}

		// If the user supplied endpoints, check them now.
		providerDef := createWalletParams.Settings[providersKey]
		if !skipConnect && len(providerDef) > 0 {
			endpoints := strings.Split(providerDef, providerDelimiter)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := createAndCheckProviders(ctx, walletDir, endpoints,
				big.NewInt(chainID), compat, createWalletParams.Net, createWalletParams.Logger, false, createWalletParams.TorProxy); err != nil {
				return fmt.Errorf("create and check providers: %v", err)
			}
		}
		return importKeyToKeyStore(ks, priv, createWalletParams.Pass)
	}

	return fmt.Errorf("unknown wallet type %q", createWalletParams.Type)
}

// newWallet is the constructor for an Ethereum asset.Wallet.
func newWallet(assetCFG *asset.WalletConfig, logger util.Logger, net util.Network) (w asset.Wallet, err error) {
	chainCfg, err := ChainConfig(net)
	if err != nil {
		return nil, fmt.Errorf("failed to locate Ethereum genesis configuration for network %s: %v", net, err)
	}
	comp, err := NetworkCompatibilityData(net)
	if err != nil {
		return nil, fmt.Errorf("failed to locate Ethereum compatibility data: %s", net)
	}
	var defaultProviders []string
	switch net {
	case util.Simnet:
		defaultProviders = []string{"http://127.0.0.1:38556"}
	case util.Testnet:
		defaultProviders = []string{
			// Verified working (2025-12-26)
			"https://ethereum-sepolia-rpc.publicnode.com",          // PublicNode - no rate limits
			"https://sepolia.drpc.org",                             // dRPC - verified working
			"https://endpoints.omniatech.io/v1/eth/sepolia/public", // Omniatech - verified working
			"https://rpc-sepolia.rockx.com",                        // RockX - verified working
		}
	case util.Mainnet:
		defaultProviders = []string{
			// Verified working (2025-12-26)
			"https://eth-mainnet.nodereal.io/v1/1659dfb40aa24bbb8153a677b98064d7", // NodeReal - verified working
			"https://eth.api.onfinality.io/public",                                // OnFinality - verified working
			"https://eth-mainnet.public.blastapi.io",                              // Blast API - verified working
			"https://ethereum-rpc.publicnode.com",                                 // PublicNode - verified working
		}
	}

	return NewEVMWallet(&EVMWalletConfig{
		BaseChainID:      BipID,
		ChainCfg:         chainCfg,
		AssetCfg:         assetCFG,
		CompatData:       &comp,
		Tokens:           Tokens,
		FinalizeConfs:    3,
		Logger:           logger,
		MultiBalAddress:  MultiBalanceAddresses[net],
		WalletInfo:       WalletInfo,
		Net:              net,
		DefaultProviders: defaultProviders,
		MaxTxFeeGwei:     GweiFactor, // 1 ETH
	})
}

// EVMWalletConfig is the configuration for an evm-compatible wallet.
type EVMWalletConfig struct {
	BaseChainID      uint32
	ChainCfg         *params.ChainConfig
	AssetCfg         *asset.WalletConfig
	CompatData       *CompatibilityData
	Tokens           map[uint32]*Token
	FinalizeConfs    uint64
	Logger           util.Logger
	DefaultProviders []string
	MultiBalAddress  common.Address // If empty, separate calls for N tokens + 1
	WalletInfo       asset.WalletInfo
	Net              util.Network
	// MaxTxFeeGwei is the absolute maximum fees we will allow for a single tx.
	// It should be set to a relatively large value.
	MaxTxFeeGwei uint64
	// IsOpStack is true for OP Stack L2s (Base, etc.) that have an additional
	// L1 security fee for posting calldata to Ethereum.
	IsOpStack bool
	// PrivKeyFromSeed overrides how the wallet's private key is derived from
	// the wallet seed. Defaults to the package's own BIP-39-based derivation
	// when nil. Set this when the chain must derive the same key as another
	// registered asset (e.g. hyperevm sharing hyperliquid's address).
	PrivKeyFromSeed func([]byte) ([]byte, func(), error)
}

func NewEVMWallet(cfg *EVMWalletConfig) (w *ETHWallet, err error) {
	assetID := cfg.BaseChainID
	chainID := cfg.ChainCfg.ChainID.Int64()

	// var cl ethFetcher
	switch cfg.AssetCfg.Type {
	case walletTypeGeth:
		return nil, asset.ErrWalletTypeDisabled
	case walletTypeRPC:
		if providerDef := cfg.AssetCfg.Settings[providersKey]; len(providerDef) == 0 && len(cfg.DefaultProviders) == 0 {
			return nil, errors.New("no providers specified")
		}
	default:
		return nil, fmt.Errorf("unknown wallet type %q", cfg.AssetCfg.Type)
	}

	wCfg, err := parseWalletConfig(cfg.AssetCfg.Settings)
	if err != nil {
		return nil, err
	}

	gasFeeLimit := wCfg.GasFeeLimit
	if gasFeeLimit == 0 {
		gasFeeLimit = defaultGasFeeLimit
	}
	privKeyFn := cfg.PrivKeyFromSeed
	if privKeyFn == nil {
		privKeyFn = privKeyFromSeed
	}
	eth := &baseWallet{
		privKeyFn:           privKeyFn,
		net:                 cfg.Net,
		baseChainID:         cfg.BaseChainID,
		chainCfg:            cfg.ChainCfg,
		chainID:             chainID,
		compat:              cfg.CompatData,
		tokens:              cfg.Tokens,
		log:                 cfg.Logger,
		dir:                 cfg.AssetCfg.DataDir,
		walletType:          cfg.AssetCfg.Type,
		torProxy:            cfg.AssetCfg.TorProxy,
		finalizeConfs:       cfg.FinalizeConfs,
		settings:            cfg.AssetCfg.Settings,
		gasFeeLimitV:        gasFeeLimit,
		wallets:             make(map[uint32]*assetWallet),
		multiBalanceAddress: cfg.MultiBalAddress,
		maxTxFeeGwei:        cfg.MaxTxFeeGwei,
		isOpStack:           cfg.IsOpStack,
		netTxDB:             cfg.AssetCfg.NetworkTxDB,
	}

	aw := &assetWallet{
		baseWallet:        eth,
		log:               cfg.Logger,
		assetID:           assetID,
		emit:              cfg.AssetCfg.Emit,
		pendingApprovals:  make(map[common.Address]*pendingApproval),
		approvalCache:     make(map[common.Address]bool),
		peersChange:       cfg.AssetCfg.PeersChange,
		evmify:            GweiToWei,
		atomize:           WeiToGwei,
		ui:                UnitInfo,
		pendingTxCheckBal: new(big.Int),
		wi:                cfg.WalletInfo,
	}

	aw.wallets = map[uint32]*assetWallet{
		assetID: aw,
	}

	return &ETHWallet{
		assetWallet: aw,
		// Capped here rather than at each use, so both the Connect and the
		// reconfigure path get the same list. Only the built-in defaults are
		// capped; an explicit providers setting bypasses this field entirely.
		defaultProviders: limitDefaultProviders(cfg.DefaultProviders),
	}, nil
}

func getWalletDir(dataDir string, network util.Network) string {
	return filepath.Join(dataDir, network.String())
}

const (
	acrossBridgeName  string = "across"
	usdcBridgeName    string = "usdc"
	polygonBridgeName string = "polygon"
)

// Connect connects to the node RPC server. Satisfies util.Connector.
func (w *ETHWallet) Connect(ctx context.Context) (_ *sync.WaitGroup, err error) {
	var cl ethFetcher
	switch w.walletType {
	case walletTypeGeth:
		// cl, err = newNodeClient(getWalletDir(w.dir, w.net), w.net, w.log.SubLogger("NODE"))
		// if err != nil {
		// 	return nil, err
		// }
		return nil, asset.ErrWalletTypeDisabled
	case walletTypeRPC:
		w.settingsMtx.RLock()
		defer w.settingsMtx.RUnlock()
		endpoints := w.defaultProviders
		if providerDef, found := w.settings[providersKey]; found && len(providerDef) > 0 {
			endpoints = strings.Split(providerDef, " ")
		}
		rpcCl, err := newMultiRPCClient(w.dir, endpoints, w.log.SubLogger("RPC"), w.chainCfg, w.finalizeConfs, w.net, w.torProxy)
		if err != nil {
			return nil, err
		}
		rpcCl.finalizeConfs = w.finalizeConfs
		cl = rpcCl
	default:
		return nil, fmt.Errorf("unknown wallet type %q", w.walletType)
	}

	w.node = cl
	w.addr = cl.address()
	w.ctx = ctx // TokenWallet will re-use this ctx.

	err = w.node.connect(ctx)
	if err != nil {
		return nil, err
	}

	if w.multiBalanceAddress != (common.Address{}) {
		w.multiBalanceContract, err = multibal.NewMultiBalanceV0(w.multiBalanceAddress, cl.contractBackend())
		if err != nil {
			w.log.Errorf("Error loading MultiBalance contract: %v", err)
		}
	}

	// The transaction database is the master tx DB handle supplied by Core,
	// scoped to this wallet's base-chain network. Core owns its lifecycle.
	w.txDB, err = NewTxDB(w.netTxDB, w.log.SubLogger("TXDB"), w.baseChainID)
	if err != nil {
		return nil, err
	}

	w.metaTable, err = w.netTxDB.Table(scanMetaTableName)
	if err != nil {
		return nil, fmt.Errorf("error opening meta table: %w", err)
	}

	pendingTxs, err := w.txDB.getPendingTxs()
	if err != nil {
		return nil, err
	}

	sort.Slice(pendingTxs, func(i, j int) bool {
		return pendingTxs[i].Nonce.Cmp(pendingTxs[j].Nonce) < 0
	})

	// Initialize the best block.
	bestHdr, err := w.node.bestHeader(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting best block hash: %w", err)
	}
	confirmedNonce, nextNonce, err := w.node.nonce(ctx)
	if err != nil {
		return nil, fmt.Errorf("error establishing nonce: %w", err)
	}

	w.tipMtx.Lock()
	w.currentTip = bestHdr
	w.tipMtx.Unlock()
	w.startingBlocks.Store(bestHdr.Number.Uint64())

	w.nonceMtx.Lock()
	w.pendingTxs = pendingTxs
	w.confirmedNonceAt = confirmedNonce
	w.pendingNonceAt = nextNonce
	w.nonceMtx.Unlock()

	w.bridges = make(map[string]bridge)
	if acrossBridge, err := newAcrossBridge(ctx, w.node.contractBackend(), w.node, w.assetID, w.net, w.addr, w.log); err != nil {
		w.log.Warnf("Failed to initialize Across bridge: %v", err)
	} else {
		w.bridges[acrossBridgeName] = acrossBridge
	}

	/* if usdcBridge, err := newUsdcBridge(w.assetID, w.net, w.node.contractBackend(), w.addr, w.node); err != nil {
		w.log.Warnf("Failed to initialize USDC bridge: %v", err)
	} else {
		w.bridges[usdcBridgeName] = usdcBridge
	} */

	switch w.assetID {
	case ethID:
		if polygonBridge, err := newPolygonBridgeEth(ctx, w.node.contractBackend(), w.net, w.addr, w.node, w.log); err != nil {
			w.log.Warnf("Failed to initialize Polygon bridge for ETH: %v", err)
		} else {
			w.bridges[polygonBridgeName] = polygonBridge
		}
	case polygonID:
		if polygonBridge, err := newPolygonBridgePolygon(w.node.contractBackend(), w.net, w.log); err != nil {
			w.log.Warnf("Failed to initialize Polygon bridge for Polygon: %v", err)
		} else {
			w.bridges[polygonBridgeName] = polygonBridge
		}
	}

	// Register simnet bridge for testing
	if w.net == util.Simnet {
		if simnetBridge, err := newSimnetBridge(w.assetID, w.net, w.node.contractBackend(), w.addr, w.node.chainConfig().ChainID, w.log); err != nil {
			w.log.Debugf("Simnet bridge not available: %v", err)
		} else {
			w.bridges[simnetBridgeName] = simnetBridge
		}
	}

	w.bridgeManager, err = newBridgeManager(&bridgeManagerConfig{
		ctx:             ctx,
		baseChainID:     w.baseChainID,
		bridges:         w.bridges,
		emit:            w.emit,
		txDB:            w.txDB,
		monitorInterval: time.Minute,
		log:             w.log,
		getTxFunc:       w.withLocalTxRead,
	})
	if err != nil {
		return nil, err
	}

	if w.log.Level() <= util.LevelDebug {
		var highestPendingNonce, lowestPendingNonce uint64
		for _, pendingTx := range pendingTxs {
			n := pendingTx.Nonce.Uint64()
			if n > highestPendingNonce {
				highestPendingNonce = n
			}
			if lowestPendingNonce == 0 || n < lowestPendingNonce {
				lowestPendingNonce = n
			}
		}
		w.log.Debugf("Synced with header %s and confirmed nonce %s, pending nonce %s, %d pending txs from nonce %d to nonce %d",
			bestHdr.Number, confirmedNonce, nextNonce, len(pendingTxs), highestPendingNonce, lowestPendingNonce)
	}

	height := bestHdr.Number
	// NOTE: We should be using the tipAtConnect to set Progress in SyncStatus.
	atomic.StoreInt64(&w.tipAtConnect, height.Int64())
	w.log.Infof("Connected to eth (%s), at height %d", w.walletType, height)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.monitorBlocks(ctx)
		w.node.shutdown()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.monitorPeers(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.scanIncomingTransfers(ctx)
	}()

	w.connected.Store(true)
	go func() {
		<-ctx.Done()
		w.connected.Store(false)
	}()

	return &wg, nil
}

// Connect waits for context cancellation and closes the WaitGroup. Satisfies
// util.Connector.
func (w *TokenWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	if w.parent.ctx == nil || w.parent.ctx.Err() != nil {
		return nil, fmt.Errorf("parent wallet not connected")
	}

	err := w.loadTokenContractor()
	if err != nil {
		return nil, err
	}

	w.connected.Store(true)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
		case <-w.parent.ctx.Done():
			w.connected.Store(false)
		}
	}()

	return &wg, nil
}

func (w *baseWallet) tip() *types.Header {
	w.tipMtx.RLock()
	defer w.tipMtx.RUnlock()
	return w.currentTip
}

// tipHeight gets the current best header's tip height.
func (w *baseWallet) tipHeight() uint64 {
	return w.tip().Number.Uint64()
}

// sendL1Fee returns the estimated L1 fee (in gwei) for a simple ETH send
// transaction on OP Stack chains. For non-OP Stack chains, returns 0.
// The value is cached per block.
func (w *baseWallet) sendL1Fee() uint64 {
	if !w.isOpStack {
		return 0
	}

	tipNum := w.tipHeight()

	// Check cache.
	w.l1Fees.Lock()
	if w.l1Fees.sendBlockNum == tipNum && tipNum > 0 {
		fee := w.l1Fees.sendL1Fee
		w.l1Fees.Unlock()
		return fee
	}
	staleFee := w.l1Fees.sendL1Fee
	w.l1Fees.Unlock()

	// Fetch without holding the lock.
	l1Fee, err := w.node.l1FeeForCalldata(w.ctx, nil)
	if err != nil {
		w.log.Warnf("sendL1Fee: error getting L1 fee: %v", err)
		return staleFee
	}

	// Update cache.
	fee := WeiToGweiCeil(l1Fee)
	w.l1Fees.Lock()
	w.l1Fees.sendL1Fee = fee
	w.l1Fees.sendBlockNum = tipNum
	w.l1Fees.Unlock()
	return fee
}

// Reconfigure attempts to reconfigure the wallet.
func (w *ETHWallet) Reconfigure(ctx context.Context, cfg *asset.WalletConfig, currentAddress string) (restart bool, err error) {
	walletCfg, err := parseWalletConfig(cfg.Settings)
	if err != nil {
		return false, err
	}

	gasFeeLimit := walletCfg.GasFeeLimit
	if walletCfg.GasFeeLimit == 0 {
		gasFeeLimit = defaultGasFeeLimit
	}

	// For now, we only are supporting multiRPCClient nodes. If we re-implement
	// P2P nodes, we'll have to add protection to the node field to allow for
	// reconfiguration of type.
	// We also need to consider how to handle the pendingTxs if we switch node
	// types, since right now we only use that map for multiRPCClient.

	if rpc, is := w.node.(*multiRPCClient); is {
		walletDir := getWalletDir(w.dir, w.net)
		providerDef := cfg.Settings[providersKey]
		var defaultProviders bool
		var endpoints []string
		if len(providerDef) > 0 {
			endpoints = strings.Split(providerDef, " ")
		} else {
			endpoints = w.defaultProviders
			defaultProviders = true
		}

		if err := rpc.reconfigure(ctx, endpoints, w.compat, walletDir, defaultProviders); err != nil {
			return false, err
		}
	}

	w.settingsMtx.Lock()
	w.settings = cfg.Settings
	w.settingsMtx.Unlock()

	atomic.StoreUint64(&w.baseWallet.gasFeeLimitV, gasFeeLimit)

	return false, nil
}

// Reconfigure attempts to reconfigure the wallet. The token wallet has
// no configurations.
func (w *TokenWallet) Reconfigure(context.Context, *asset.WalletConfig, string) (bool, error) {
	return false, nil
}

func (eth *baseWallet) connectedWallets() []*assetWallet {
	eth.walletsMtx.RLock()
	defer eth.walletsMtx.RUnlock()

	m := make([]*assetWallet, 0, len(eth.wallets))

	for _, w := range eth.wallets {
		if w.connected.Load() {
			m = append(m, w)
		}
	}
	return m
}

func (eth *baseWallet) wallet(assetID uint32) *assetWallet {
	eth.walletsMtx.RLock()
	defer eth.walletsMtx.RUnlock()
	return eth.wallets[assetID]
}

func (eth *baseWallet) GasFeeLimit() uint64 {
	return atomic.LoadUint64(&eth.gasFeeLimitV)
}

type genBridgeTxResult struct {
	counterpartAssetID         uint32
	counterpartTxID            string
	followUpData               []byte
	requiresFollowUp           bool
	previousBridgeCompletionID string
	bridgeName                 string
}

type genTxResult struct {
	tx        *types.Transaction
	txType    txdb.TransactionType
	amt       uint64
	recipient *string
	bridge    *genBridgeTxResult
}

// transactionGenerator is an action that uses a nonce and returns a tx, it's
// type specifier, and its value.
type transactionGenerator func(nonce *big.Int) (*genTxResult, error)

// withNonce is called with a function intended to generate a new transaction
// using the next available nonce. If the function returns a non-nil tx, the
// nonce will be treated as used, and an extendedWalletTransaction will be
// generated, stored, and queued for monitoring.
func (w *assetWallet) withNonce(ctx context.Context, f transactionGenerator) (err error) {
	w.nonceMtx.Lock()
	defer w.nonceMtx.Unlock()
	if err = nonceIsSane(w.pendingTxs, w.pendingNonceAt); err != nil {
		return err
	}
	nonce := func() *big.Int {
		n := new(big.Int).Set(w.confirmedNonceAt)
		for _, pendingTx := range w.pendingTxs {
			if pendingTx.Nonce.Cmp(n) < 0 {
				continue
			}
			if pendingTx.Nonce.Cmp(n) == 0 {
				n.Add(n, big.NewInt(1))
			} else {
				break
			}
		}
		return n
	}

	n := nonce()
	w.log.Trace("Nonce chosen for tx generator =", n)

	// Make a first attempt with our best-known nonce.
	genTxResult, submitErr := f(n)
	if submitErr != nil && strings.Contains(submitErr.Error(), "nonce too low") {
		w.log.Warnf("Too-low nonce detected. Attempting recovery")
		confirmedNonceAt, pendingNonceAt, err := w.node.nonce(ctx)
		if err != nil {
			return fmt.Errorf("error during too-low nonce recovery: %v", err)
		}
		w.confirmedNonceAt = confirmedNonceAt
		w.pendingNonceAt = pendingNonceAt
		newNonce := nonce()
		if newNonce == n {
			return fmt.Errorf("best RPC nonce %d not better than our best nonce %d", newNonce, n)
		}
		n = newNonce
		// Try again.
		genTxResult, submitErr = f(n)
		if submitErr == nil {
			w.log.Info("Nonce recovered and transaction broadcast")
		}
	}
	if submitErr != nil {
		return submitErr
	}

	if genTxResult != nil {
		et := w.extendedTx(genTxResult)
		et.Confirms = &txdb.Confirms{Target: uint32(w.finalizeConfs)}
		et.Timestamp = uint64(time.Now().Unix())
		w.pendingTxs = append(w.pendingTxs, et)
		if n.Cmp(w.pendingNonceAt) >= 0 {
			w.pendingNonceAt.Add(n, big.NewInt(1))
		}
		w.emitTransactionNote(et.WalletTransaction, true)
		w.log.Tracef("Transaction %s generated for nonce %s", et.ID, n)
	}

	return nil
}

// nonceIsSane performs sanity checks on pending txs.
func nonceIsSane(pendingTxs []*extendedWalletTx, pendingNonceAt *big.Int) error {
	if len(pendingTxs) == 0 && pendingNonceAt == nil {
		return errors.New("no pending txs and no best nonce")
	}
	var lastNonce uint64
	var numNotIndexed, confirmedTip int
	for i, pendingTx := range pendingTxs {
		if !pendingTx.savedToDB {
			return errors.New("tx database problem detected")
		}
		nonce := pendingTx.Nonce.Uint64()
		if nonce < lastNonce {
			return fmt.Errorf("pending txs not sorted")
		}
		if pendingTx.Confirmed || pendingTx.BlockNumber != 0 {
			if confirmedTip != i {
				return fmt.Errorf("confirmed tx sequence error. pending tx %s is confirmed but older txs were not", pendingTx.ID)

			}
			confirmedTip = i + 1
			continue
		}
		lastNonce = nonce
		age := pendingTx.age()
		// Only allow a handful of txs that we haven't been seen on-chain yet.
		if age > chainOpTick*10 {
			numNotIndexed++
		}
		if age >= txAgeOut {
			// If any tx is unindexed and aged out, wait for user to fix it.
			return fmt.Errorf("tx %s is aged out. waiting for user to take action", pendingTx.ID)
		}

	}
	if numNotIndexed >= maxUnindexedTxs {
		return fmt.Errorf("%d unindexed txs has reached the limit of %d", numNotIndexed, maxUnindexedTxs)
	}
	return nil
}

// tokenWalletConfig is the configuration options for token wallets.
type tokenWalletConfig struct {
	// LimitAllowance disabled for now.
	// See https://github.com/decred/dcrdex/pull/1394#discussion_r780479402.
	// LimitAllowance bool `ini:"limitAllowance"`
}

// parseTokenWalletConfig parses the settings map into a *tokenWalletConfig.
func parseTokenWalletConfig(settings map[string]string) (cfg *tokenWalletConfig, err error) {
	cfg = new(tokenWalletConfig)
	err = config.Unmapify(settings, cfg)
	if err != nil {
		return nil, fmt.Errorf("error parsing wallet config: %w", err)
	}
	return cfg, nil
}

// CreateTokenWallet "creates" a wallet for a token. There is really nothing
// to do, except check that the token exists.
func (w *baseWallet) CreateTokenWallet(tokenID uint32, _ map[string]string) error {
	// Just check that the token exists for now.
	if w.tokens[tokenID] == nil {
		return fmt.Errorf("token not found for asset ID %d", tokenID)
	}
	return nil
}

type pendingBridge struct {
	bridgeName     string
	sourceAssetID  uint32
	destAssetID    uint32
	amount         uint64
	completionData atomic.Value // []byte
}

// bridgeManager manages pending bridges. It periodically checks if the bridges
// are ready for completion by querying the bridge interface for completion
// data. When a bridge is ready, it emits a BridgeReadyToComplete notification.
// The manager handles adding and removing pending bridges and automatically
// starts and stops monitoring based on the presence of pending bridges.
type bridgeManager struct {
	ctx             context.Context
	emit            *asset.WalletEmitter
	log             util.Logger
	monitorInterval time.Duration
	txDB            txDB
	bridges         map[string]bridge
	getTxFunc       func(common.Hash, func(*extendedWalletTx)) bool

	mtx            sync.RWMutex
	stopMonitoring context.CancelFunc
	pendingBridges map[string]*pendingBridge
}

type bridgeManagerConfig struct {
	ctx             context.Context
	baseChainID     uint32
	bridges         map[string]bridge
	emit            *asset.WalletEmitter
	txDB            txDB
	monitorInterval time.Duration
	log             util.Logger
	getTxFunc       func(common.Hash, func(*extendedWalletTx)) bool
}

func newBridgeManager(cfg *bridgeManagerConfig) (*bridgeManager, error) {
	manager := &bridgeManager{
		ctx:             cfg.ctx,
		emit:            cfg.emit,
		txDB:            cfg.txDB,
		getTxFunc:       cfg.getTxFunc,
		bridges:         cfg.bridges,
		pendingBridges:  make(map[string]*pendingBridge),
		log:             cfg.log.SubLogger("BridgeManager"),
		monitorInterval: cfg.monitorInterval,
	}

	pendingBridges, err := cfg.txDB.getPendingBridges(nil)
	if err != nil {
		return nil, fmt.Errorf("error getting pending bridges: %v", err)
	}

	// Filter the pending bridges that originated from assetID.
	for _, tx := range pendingBridges {
		if tx.BridgeCounterpartTx == nil {
			manager.log.Errorf("Bridge tx %s has no counterpart tx. This should have been added when the transaction was created", tx.ID)
			continue
		}

		sourceAssetID := cfg.baseChainID
		if tx.TokenID != nil {
			sourceAssetID = *tx.TokenID
		}

		manager.pendingBridges[tx.ID] = &pendingBridge{
			bridgeName:    tx.BridgeName,
			sourceAssetID: sourceAssetID,
			destAssetID:   tx.BridgeCounterpartTx.AssetID,
			amount:        tx.Amount,
		}
	}

	if len(manager.pendingBridges) > 0 {
		manager.startMonitoring()
	}

	return manager, nil
}

func (bm *bridgeManager) addPendingBridge(initiationTxID string, sourceAssetID, destAssetID uint32, amount uint64, bridgeName string) {
	bm.mtx.Lock()
	defer bm.mtx.Unlock()

	bm.pendingBridges[initiationTxID] = &pendingBridge{
		bridgeName:    bridgeName,
		sourceAssetID: sourceAssetID,
		destAssetID:   destAssetID,
		amount:        amount,
	}

	bm.startMonitoring()
}

// checkPendingBridges checks if completion data is available for any pending
// bridges, and if so, emits a BridgeReadyToComplete notification. The
// completion data is cached to avoid fetching it multiple times for the same
// bridge.
func (bm *bridgeManager) checkPendingBridges(ctx context.Context) {
	bm.mtx.RLock()
	pendingBridges := util.CopyMap(bm.pendingBridges)
	bm.mtx.RUnlock()

	for initiationTxID, pendingBridge := range pendingBridges {
		var initiationTx *extendedWalletTx
		if !bm.getTxFunc(common.HexToHash(initiationTxID), func(tx *extendedWalletTx) {
			initiationTx = tx
		}) {
			bm.log.Errorf("unable to get initiation tx %s", initiationTxID)
			continue
		}

		if !initiationTx.Confirmed {
			continue
		}

		var completionData []byte
		if dataVal := pendingBridge.completionData.Load(); dataVal != nil {
			cd, ok := dataVal.([]byte)
			if ok {
				completionData = cd
			}
		} else {
			bridge, ok := bm.bridges[pendingBridge.bridgeName]
			if !ok {
				bm.log.Errorf("bridge %s not found", pendingBridge.bridgeName)
				continue
			}

			var err error
			completionData, err = bridge.getCompletionData(ctx, pendingBridge.sourceAssetID, initiationTxID)
			if err != nil {
				bm.log.Infof("Unable to get mint info for bridge with initiation tx ID %s: %v", initiationTxID, err)
				continue
			}
			if completionData == nil {
				continue
			}
			pendingBridge.completionData.Store(completionData)
		}

		bm.emit.BridgeReadyToComplete(pendingBridge.destAssetID, initiationTxID, pendingBridge.amount, completionData, pendingBridge.bridgeName)
	}
}

// markBridgeComplete is called when the destination wallet has confirmed the
// completion of a bridge. The pending bridge is removed from the manager and
// the db is updated to reflect the completion.
func (bm *bridgeManager) markBridgeComplete(initiationTxID string, completionTxIDs []string, amtReceived, fees uint64, isComplete bool) {
	var deletePendingBridge bool
	defer func() {
		if !deletePendingBridge {
			return
		}

		bm.mtx.Lock()
		delete(bm.pendingBridges, initiationTxID)
		if len(bm.pendingBridges) == 0 && bm.stopMonitoring != nil {
			bm.stopMonitoring()
			bm.stopMonitoring = nil
		}
		bm.mtx.Unlock()
	}()

	bridgeTx, err := bm.txDB.getTx(common.HexToHash(initiationTxID))
	if err != nil {
		bm.log.Errorf("error getting bridge tx: %v", err)
		return
	} else if bridgeTx == nil {
		bm.log.Errorf("could not find initiation tx %v", initiationTxID)
		return
	} else if bridgeTx.BridgeCounterpartTx == nil {
		bm.log.Errorf("Initiation tx %s has no counterpart tx. This should have been added when the transaction was created", initiationTxID)
		return
	}

	bridgeTx.BridgeCounterpartTx.IDs = completionTxIDs
	bridgeTx.BridgeCounterpartTx.Complete = isComplete
	bridgeTx.BridgeCounterpartTx.AmountReceived = amtReceived
	bridgeTx.BridgeCounterpartTx.Fees = fees
	if err := bm.txDB.storeTx(bridgeTx); err != nil {
		bm.log.Errorf("error storing completed bridge tx: %v", err)
		return
	}

	deletePendingBridge = isComplete
}

// bridgeManager.mtx MUST be held while calling this function.
func (bm *bridgeManager) startMonitoring() {
	if bm.stopMonitoring != nil { // already running
		return
	}

	ctx, cancel := context.WithCancel(bm.ctx)
	bm.stopMonitoring = cancel

	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				bm.checkPendingBridges(ctx)
				timer.Reset(bm.monitorInterval)
			}
		}
	}()
}

var _ asset.Bridger = (*assetWallet)(nil)

// OpenTokenWallet creates a new TokenWallet.
func (w *ETHWallet) OpenTokenWallet(tokenCfg *asset.TokenConfig) (asset.Wallet, error) {
	token, found := w.tokens[tokenCfg.AssetID]
	if !found {
		return nil, fmt.Errorf("token %d not found", tokenCfg.AssetID)
	}

	cfg, err := parseTokenWalletConfig(tokenCfg.Settings)
	if err != nil {
		return nil, err
	}

	netToken := token.NetTokens[w.net]
	if netToken == nil {
		return nil, fmt.Errorf("no %s network configuration for token %s (%d)", w.net, token.Name, tokenCfg.AssetID)
	}
	if netToken.GasEstimate == nil {
		return nil, fmt.Errorf("no gas estimate for token %s (%d) on %s", token.Name, tokenCfg.AssetID, w.net)
	}

	aw := &assetWallet{
		baseWallet:       w.baseWallet,
		log:              w.baseWallet.log.SubLogger(strings.ToUpper(util.BipIDSymbol(tokenCfg.AssetID))),
		assetID:          tokenCfg.AssetID,
		gasEstimate:      netToken.GasEstimate,
		emit:             tokenCfg.Emit,
		peersChange:      tokenCfg.PeersChange,
		pendingApprovals: make(map[common.Address]*pendingApproval),
		approvalCache:    make(map[common.Address]bool),
		evmify:           token.AtomicToEVM,
		atomize:          token.EVMToAtomic,
		ui:               token.UnitInfo,
		wi: asset.WalletInfo{
			Name:              token.Name,
			SupportedVersions: w.wi.SupportedVersions,
			UnitInfo:          token.UnitInfo,
			BlockchainClass:   asset.BlockchainClassEVM,
			Network:           w.wi.Network,
		},
		tokenAddr:         netToken.Address,
		pendingTxCheckBal: new(big.Int),
	}

	w.baseWallet.walletsMtx.Lock()
	w.baseWallet.wallets[tokenCfg.AssetID] = aw
	w.baseWallet.walletsMtx.Unlock()

	return &TokenWallet{
		assetWallet: aw,
		cfg:         cfg,
		parent:      w.assetWallet,
		token:       token,
		netToken:    netToken,
	}, nil
}

// OwnsDepositAddress indicates if an address belongs to the wallet. The address
// need not be a EIP55-compliant formatted address. It may or may not have a 0x
// prefix, and case is not important.
//
// In Ethereum, an address is an account.
func (eth *baseWallet) OwnsDepositAddress(address string) (bool, error) {
	if !common.IsHexAddress(address) {
		return false, errors.New("invalid address")
	}
	addr := common.HexToAddress(address)
	return addr == eth.addr, nil
}

// amountLocked returns the total amount currently locked.
func (w *assetWallet) amountLocked() uint64 {
	w.lockedFunds.mtx.RLock()
	defer w.lockedFunds.mtx.RUnlock()
	return w.lockedFunds.initiateReserves + w.lockedFunds.redemptionReserves + w.lockedFunds.refundReserves
}

// Balance returns the available and locked funds (token or eth).
func (w *assetWallet) Balance() (*asset.Balance, error) {
	return w.balance()
}

// balance returns the total available funds in the account.
func (w *assetWallet) balance() (*asset.Balance, error) {
	bal, err := w.balanceWithTxPool()
	if err != nil {
		return nil, fmt.Errorf("pending balance error: %w", err)
	}

	locked := w.amountLocked()
	// Subtract pending-out from current in wei before atomizing so the sub-gwei
	// remainder is floored once, rather than flooring each term independently
	// (which can over-report the spendable balance by up to 1 gwei when
	// pending-out has a fractional gwei part). locked is already in atomic
	// units, so subtract it after atomizing.
	spendable := w.atomize(new(big.Int).Sub(bal.Current, bal.PendingOut))
	var available uint64
	if spendable > locked {
		available = spendable - locked
	}

	return &asset.Balance{
		Available: available,
		Locked:    locked,
		Immature:  w.atomize(bal.PendingIn),
	}, nil
}

// coin implements the asset.Coin interface for ETH
type coin struct {
	txHash common.Hash
	// the value can be determined from the coin id, but for some
	// coin ids a lookup would be required from the blockchain to
	// determine its value, so this field is used as a cache.
	value uint64
}

// ID is the ETH coins ID. For functions related to funding an order,
// the ID must contain an encoded fundingCoinID, but when returned from
// Swap, it will contain the transaction hash used to initiate the swap.
func (c *coin) ID() util.Bytes {
	return c.txHash[:]
}

func (c *coin) TxID() string {
	return c.txHash.String()
}

// String is a string representation of the coin.
func (c *coin) String() string {
	return c.TxID()
}

// Value returns the value in gwei of the coin.
func (c *coin) Value() uint64 {
	return c.value
}

var _ asset.Coin = (*coin)(nil)

// approvalGasLimit returns the gas limit for an ERC20 approve transaction that
// sets spender's allowance to amount: a live eth_estimateGas result plus the
// standard buffer (see bufferedGasLimit). The estimate is made against the
// actual spender because the allowance slot's current state (zero vs. non-zero)
// changes the SSTORE cost, so a stand-in spender would mis-estimate.
func (w *assetWallet) approvalGasLimit(ctx context.Context, spender common.Address, amount *big.Int) (uint64, error) {
	tc, err := w.tokenContractorOrErr()
	if err != nil {
		return 0, err
	}
	est, err := estimateGas(ctx, w.addr, tc.tokenAddress(), erc20.ERC20ABI, w.node.contractBackend(), new(big.Int), "approve", spender, amount)
	if err != nil {
		return 0, fmt.Errorf("error estimating approve gas: %w", err)
	}
	return bufferedGasLimit(est), nil
}

// tokenContractorOrErr returns the wallet's token contractor or an error if it
// is not set (e.g. for the base-chain asset).
func (w *assetWallet) tokenContractorOrErr() (tokenContractor, error) {
	if w.tokenContractor == nil {
		return nil, fmt.Errorf("no token contractor for asset %d", w.assetID)
	}
	return w.tokenContractor, nil
}

// recoverPubkey recovers the uncompressed public key from the signature and the
// message hash that was signed. The signature should be a compact signature
// generated by a geth wallet, with the format [R || S || V], with the recover
// bit V at the end. See go-ethereum/crypto.Sign.
func recoverPubkey(msgHash, sig []byte) ([]byte, error) {
	// Using Decred's ecdsa.RecoverCompact requires moving the recovery byte to
	// the beginning of the serialized compact signature and adding back in the
	// compactSigMagicOffset scalar.
	sigBTC := make([]byte, 65)
	sigBTC[0] = sig[64] + 27 // compactSigMagicOffset
	copy(sigBTC[1:], sig)
	pubKey, _, err := ecdsa.RecoverCompact(sigBTC, msgHash)
	if err != nil {
		return nil, err
	}
	return pubKey.SerializeUncompressed(), nil
}

// tokenBalance checks the token balance of the account handled by the wallet.
func (w *assetWallet) tokenBalance() (bal *big.Int, err error) {
	tc, err := w.tokenContractorOrErr()
	if err != nil {
		return nil, err
	}
	return tc.balance(w.ctx)
}

func (w *assetWallet) approvalStatus(contract common.Address, checkAllowance func() (*big.Int, error)) (asset.ApprovalStatus, error) {
	if w.assetID == w.baseChainID {
		return asset.Approved, nil
	}

	// If the result has been cached, return what is in the cache.
	// The cache is cleared if an approval/unapproval tx is done.
	w.approvalsMtx.Lock()
	defer w.approvalsMtx.Unlock()

	if approved, cached := w.approvalCache[contract]; cached {
		if approved {
			return asset.Approved, nil
		} else {
			return asset.NotApproved, nil
		}
	}

	if _, pending := w.pendingApprovals[contract]; pending {
		return asset.Pending, nil
	}

	currentAllowance, err := checkAllowance()
	if err != nil {
		return asset.NotApproved, fmt.Errorf("error retrieving current allowance: %w", err)
	}
	if currentAllowance.Cmp(unlimitedAllowanceReplenishThreshold) >= 0 {
		w.approvalCache[contract] = true
		return asset.Approved, nil
	}
	w.approvalCache[contract] = false

	return asset.NotApproved, nil
}

func (w *assetWallet) bridgeContractApprovalStatus(ctx context.Context, bridge bridge) (asset.ApprovalStatus, error) {
	if !bridge.requiresBridgeContractApproval(w.assetID) {
		return asset.Approved, nil
	}
	bridgeAddr, err := bridge.bridgeContractAddr(ctx, w.assetID)
	if err != nil {
		return asset.NotApproved, fmt.Errorf("error getting bridge contract address: %w", err)
	}
	return w.approvalStatus(bridgeAddr, func() (*big.Int, error) {
		return bridge.bridgeContractAllowance(ctx, w.assetID)
	})
}

// BridgeContractApprovalStatus returns whether the bridge contract has been
// approved to spend tokens on behalf of the account handled by the wallet.
func (w *assetWallet) BridgeContractApprovalStatus(ctx context.Context, bridgeName string) (asset.ApprovalStatus, error) {
	bridge, ok := w.bridges[bridgeName]
	if !ok {
		return asset.NotApproved, fmt.Errorf("bridge %s not found", bridgeName)
	}
	return w.bridgeContractApprovalStatus(ctx, bridge)
}

func (w *assetWallet) approveBridgeContract(ctx context.Context, bridge bridge, onConfirm func()) (string, error) {
	approvalStatus, err := w.bridgeContractApprovalStatus(ctx, bridge)
	if err != nil {
		return "", fmt.Errorf("error checking approval status: %w", err)
	}
	if approvalStatus == asset.Approved {
		return "", fmt.Errorf("bridge contract is already approved")
	}
	if approvalStatus == asset.Pending {
		return "", fmt.Errorf("approval is already pending")
	}

	maxFeeRate, tipRate, err := w.recommendedMaxFeeRate(ctx)
	if err != nil {
		return "", fmt.Errorf("error calculating approval fee rate: %w", err)
	}
	feeRateGwei := WeiToGweiCeil(maxFeeRate)

	bridgeAddr, err := bridge.bridgeContractAddr(ctx, w.assetID)
	if err != nil {
		return "", fmt.Errorf("error getting bridge contract address: %w", err)
	}
	approvalGas, err := w.approvalGasLimit(ctx, bridgeAddr, unlimitedAllowance)
	if err != nil {
		return "", fmt.Errorf("error calculating approval gas: %w", err)
	}

	baseAssetWallet, err := w.baseAssetWallet()
	if err != nil {
		return "", fmt.Errorf("error getting base asset wallet: %w", err)
	}
	ethBal, err := baseAssetWallet.balance()
	if err != nil {
		return "", fmt.Errorf("error getting eth balance: %w", err)
	}
	if ethBal.Available < approvalGas*feeRateGwei {
		return "", fmt.Errorf("insufficient fee balance for approval. required: %d, available: %d",
			approvalGas*feeRateGwei, ethBal.Available)
	}

	var txID string
	return txID, w.withNonce(ctx, func(nonce *big.Int) (*genTxResult, error) {
		txOpts, err := w.node.txOpts(ctx, 0, approvalGas, maxFeeRate, tipRate, nonce)
		if err != nil {
			return nil, fmt.Errorf("txOpts error: %w", err)
		}

		tx, err := bridge.approveBridgeContract(txOpts, unlimitedAllowance, w.assetID)
		if err != nil {
			return nil, fmt.Errorf("error approving bridge contract: %w", err)
		}

		txID = tx.Hash().Hex()

		w.approvalsMtx.Lock()
		delete(w.approvalCache, bridgeAddr)
		w.pendingApprovals[bridgeAddr] = &pendingApproval{
			txHash:    tx.Hash(),
			onConfirm: onConfirm,
		}
		w.approvalsMtx.Unlock()

		return &genTxResult{
			tx:     tx,
			txType: txdb.ApproveToken,
			amt:    w.atomize(unlimitedAllowance),
		}, nil
	})
}

// ApproveBridgeContract approves the bridge contract to spend tokens on behalf
// of the account handled by the wallet.
func (w *assetWallet) ApproveBridgeContract(ctx context.Context, bridgeName string, onConfirm func()) (string, error) {
	bridge, ok := w.bridges[bridgeName]
	if !ok {
		return "", fmt.Errorf("bridge %s not found", bridgeName)
	}
	return w.approveBridgeContract(ctx, bridge, onConfirm)
}

func (w *assetWallet) unapproveBridgeContract(ctx context.Context, bridge bridge, onConfirm func()) (string, error) {
	approvalStatus, err := w.bridgeContractApprovalStatus(ctx, bridge)
	if err != nil {
		return "", fmt.Errorf("error checking approval status: %w", err)
	}
	if approvalStatus != asset.Approved {
		return "", fmt.Errorf("bridge contract is not approved")
	}

	maxFeeRate, tipRate, err := w.recommendedMaxFeeRate(ctx)
	if err != nil {
		return "", fmt.Errorf("error calculating approval fee rate: %w", err)
	}
	feeRateGwei := WeiToGweiCeil(maxFeeRate)

	bridgeAddr, err := bridge.bridgeContractAddr(ctx, w.assetID)
	if err != nil {
		return "", fmt.Errorf("error getting bridge contract address: %w", err)
	}
	approvalGas, err := w.approvalGasLimit(ctx, bridgeAddr, new(big.Int))
	if err != nil {
		return "", fmt.Errorf("error calculating approval gas: %w", err)
	}

	baseAssetWallet, err := w.baseAssetWallet()
	if err != nil {
		return "", fmt.Errorf("error getting base asset wallet: %w", err)
	}
	ethBal, err := baseAssetWallet.balance()
	if err != nil {
		return "", fmt.Errorf("error getting eth balance: %w", err)
	}
	if ethBal.Available < approvalGas*feeRateGwei {
		return "", fmt.Errorf("insufficient fee balance for approval. required: %d, available: %d",
			approvalGas*feeRateGwei, ethBal.Available)
	}

	var txID string
	return txID, w.withNonce(ctx, func(nonce *big.Int) (*genTxResult, error) {
		txOpts, err := w.node.txOpts(ctx, 0, approvalGas, maxFeeRate, tipRate, nonce)
		if err != nil {
			return nil, fmt.Errorf("txOpts error: %w", err)
		}

		tx, err := bridge.approveBridgeContract(txOpts, new(big.Int), w.assetID)
		if err != nil {
			return nil, fmt.Errorf("error approving bridge contract: %w", err)
		}

		txID = tx.Hash().Hex()

		w.approvalsMtx.Lock()
		delete(w.approvalCache, bridgeAddr)
		w.pendingApprovals[bridgeAddr] = &pendingApproval{
			txHash:    tx.Hash(),
			onConfirm: onConfirm,
		}
		w.approvalsMtx.Unlock()

		return &genTxResult{
			tx:     tx,
			txType: txdb.ApproveToken,
			amt:    w.atomize(new(big.Int)),
		}, nil
	})
}

// UnapproveBridgeContract removes the approval for the bridge contract.
func (w *assetWallet) UnapproveBridgeContract(ctx context.Context, bridgeName string, onConfirm func()) (string, error) {
	bridge, ok := w.bridges[bridgeName]
	if !ok {
		return "", fmt.Errorf("bridge %s not found", bridgeName)
	}
	return w.unapproveBridgeContract(ctx, bridge, onConfirm)
}

func (w *assetWallet) initiateBridge(ctx context.Context, amt uint64, dest uint32, bridgeName string) (txID string, err error) {
	bridge, ok := w.bridges[bridgeName]
	if !ok {
		return "", fmt.Errorf("bridge %s not found", bridgeName)
	}

	// Check that the bridge contract is approved
	approvalStatus, err := w.bridgeContractApprovalStatus(ctx, bridge)
	if err != nil {
		return "", fmt.Errorf("error checking approval status: %w", err)
	}
	if approvalStatus != asset.Approved {
		return "", fmt.Errorf("bridge contract is not approved")
	}

	// Make sure that the amount is within the bridge's limits
	bigMin, bigMax, hasLimits, err := bridge.bridgeLimits(w.assetID, dest)
	if err != nil {
		return "", fmt.Errorf("error getting bridge limits: %w", err)
	}
	min := w.atomize(bigMin)
	max := w.atomize(bigMax)
	if hasLimits {
		if amt < min {
			return "", fmt.Errorf("amount %d is less than the minimum limit %d", amt, min)
		}
		if amt > max {
			return "", fmt.Errorf("amount %d is greater than the maximum limit %d", amt, max)
		}
	}

	// Initiate the bridge
	maxFeeRate, tipRate, err := w.recommendedMaxFeeRate(ctx)
	if err != nil {
		return "", fmt.Errorf("error calculating bridge fee rate: %w", err)
	}
	return txID, w.withNonce(ctx, func(nonce *big.Int) (*genTxResult, error) {
		txOpts, err := w.node.txOpts(ctx, 0, bridge.initiateBridgeGas(w.assetID), maxFeeRate, tipRate, nonce)
		if err != nil {
			return nil, fmt.Errorf("txOpts error: %w", err)
		}

		var tx *types.Transaction
		tx, err = bridge.initiateBridge(txOpts, w.assetID, dest, w.evmify(amt))
		if err != nil {
			return nil, err
		}

		txID = tx.Hash().Hex()

		return &genTxResult{
			tx:     tx,
			txType: txdb.InitiateBridge,
			amt:    amt,
			bridge: &genBridgeTxResult{
				counterpartAssetID: dest,
				bridgeName:         bridgeName,
			},
		}, nil
	})
}

// InitiateBridge initiates bridging funds from one chain to another.
func (w *assetWallet) InitiateBridge(ctx context.Context, amt uint64, dest uint32, bridgeName string) (string, error) {
	balance, err := w.balance()
	if err != nil {
		return "", fmt.Errorf("error getting balance: %w", err)
	}
	if balance.Available < amt {
		return "", fmt.Errorf("insufficient balance: %d < %d", balance.Available, amt)
	}

	// Calculate the fee for the bridge initiation
	bridge, ok := w.bridges[bridgeName]
	if !ok {
		return "", fmt.Errorf("bridge %s not found", bridgeName)
	}
	initiateGas := bridge.initiateBridgeGas(w.assetID)
	maxFeeRateWei, _, err := w.recommendedMaxFeeRate(ctx)
	if err != nil {
		return "", fmt.Errorf("error calculating bridge fee rate: %w", err)
	}
	maxFeeRateGwei := WeiToGweiCeil(maxFeeRateWei)
	fee := initiateGas * maxFeeRateGwei

	// Check fee balance
	isToken := w.assetID != w.baseChainID
	if isToken {
		// For tokens, the fee is paid in the base asset
		w.walletsMtx.RLock()
		baseWallet := w.wallets[w.baseChainID]
		w.walletsMtx.RUnlock()
		if baseWallet == nil {
			return "", fmt.Errorf("base wallet not found")
		}
		baseBal, err := baseWallet.balance()
		if err != nil {
			return "", fmt.Errorf("error getting base asset balance: %w", err)
		}
		if baseBal.Available < fee {
			return "", fmt.Errorf("insufficient fee balance: required %d, available %d", fee, baseBal.Available)
		}
	} else {
		// For base assets, the fee comes from the same balance as the amount
		if balance.Available < amt+fee {
			return "", fmt.Errorf("insufficient balance for amount + fee: required %d, available %d", amt+fee, balance.Available)
		}
	}

	txID, err := w.initiateBridge(ctx, amt, dest, bridgeName)
	if err != nil {
		return "", err
	}

	w.bridgeManager.addPendingBridge(txID, w.assetID, dest, amt, bridgeName)

	return txID, nil
}

// MarkBridgeComplete is called when the bridge completion transaction has
// been confirmed on the destination chain.
func (w *assetWallet) MarkBridgeComplete(initiationTxID string, completionTxIDs []string, amtReceived, fees uint64, complete bool) {
	w.bridgeManager.markBridgeComplete(initiationTxID, completionTxIDs, amtReceived, fees, complete)
}

func (w *assetWallet) pendingBridges() ([]*txdb.WalletTransaction, error) {
	pendingBridges, err := w.txDB.getPendingBridges(&w.assetID)
	if err != nil {
		return nil, fmt.Errorf("error getting pending bridges: %w", err)
	}

	wt := make([]*txdb.WalletTransaction, 0, len(pendingBridges))
	for _, pb := range pendingBridges {
		wt = append(wt, pb.WalletTransaction)
	}

	return wt, nil
}

// PendingBridges lists all uncompleted bridge transactions on the blockchain.
func (w *assetWallet) PendingBridges() ([]*txdb.WalletTransaction, error) {
	return w.pendingBridges()
}

func (w *assetWallet) bridgeHistory(n int, refID *string, past bool) ([]*txdb.WalletTransaction, error) {
	var refHash *common.Hash
	if refID != nil {
		rh := common.HexToHash(*refID)
		refHash = &rh
	}
	return w.txDB.getBridges(&w.assetID, n, refHash, past)
}

// BridgeHistory retrieves a record of bridge initiations on the blockchain.
//
// When n <= 0:
// - getTxs returns all transactions, ordered in reverse by completion time.
//
// When no refID is provided:
// - Returns the n most recent bridge transactions, ordered in reverse by completion time.
// - The 'past' argument is ignored.
//
// When refID is provided:
// - Returns n transactions, starting with the referenced transaction.
// - If past = false: Results are ordered by completion time (ascending).
// - If past = true: Results are ordered by completion time (descending).
// - The referenced transaction is included in the results.
// - Returns asset.CoinNotFoundError if the refID is not found.
func (w *assetWallet) BridgeHistory(n int, refID *string, past bool) ([]*txdb.WalletTransaction, error) {
	return w.bridgeHistory(n, refID, past)
}

func (w *assetWallet) completeBridge(ctx context.Context, data []byte, bridgeTx *txdb.BridgeCounterpartTx, amount uint64, bridge bridge, bridgeName string) (txID string, err error) {
	maxFeeRate, tipRate, err := w.recommendedMaxFeeRate(ctx)
	if err != nil {
		return "", fmt.Errorf("error calculating bridge fee rate: %w", err)
	}

	return txID, w.withNonce(ctx, func(nonce *big.Int) (*genTxResult, error) {
		txOpts, err := w.node.txOpts(ctx, 0, bridge.completeBridgeGas(w.assetID), maxFeeRate, tipRate, nonce)
		if err != nil {
			return nil, fmt.Errorf("txOpts error: %w", err)
		}

		tx, err := bridge.completeBridge(txOpts, w.assetID, data)
		if err != nil {
			return nil, err
		}

		txID = tx.Hash().Hex()

		// If this transasction requires a follow-up completion, the amount
		// in the transaction will be 0, because no funds were allocated
		// to the wallet due to this transaction.
		var amtInTx uint64
		if !bridge.requiresFollowUpCompletion(w.assetID) {
			amtInTx = amount
		}

		return &genTxResult{
			tx:     tx,
			txType: txdb.CompleteBridge,
			amt:    amtInTx,
			bridge: &genBridgeTxResult{
				counterpartAssetID: bridgeTx.AssetID,
				counterpartTxID:    bridgeTx.IDs[0], // Reference to initiation transaction
				requiresFollowUp:   bridge.requiresFollowUpCompletion(w.assetID),
				bridgeName:         bridgeName,
			},
		}, nil
	})
}

// completeBridgeFollowUp submits a follow-up completion transaction for a bridge.
// This is only required for withdrawing POL from Polygon POS to Ethereum.
func (w *assetWallet) completeBridgeFollowUp(ctx context.Context, data []byte, initialCompletionID string, amount uint64, bridgeTx *txdb.BridgeCounterpartTx, bridge bridge, bridgeName string) (txID string, err error) {
	maxFeeRate, tipRate, err := w.recommendedMaxFeeRate(ctx)
	if err != nil {
		return "", fmt.Errorf("error calculating bridge fee rate: %w", err)
	}

	return txID, w.withNonce(ctx, func(nonce *big.Int) (*genTxResult, error) {
		txOpts, err := w.node.txOpts(ctx, 0, bridge.followUpCompleteBridgeGas(), maxFeeRate, tipRate, nonce)
		if err != nil {
			return nil, fmt.Errorf("txOpts error: %w", err)
		}

		tx, err := bridge.completeFollowUpBridge(txOpts, data)
		if err != nil {
			return nil, err
		}

		txID = tx.Hash().Hex()

		return &genTxResult{
			tx:     tx,
			txType: txdb.CompleteBridge,
			amt:    amount,
			bridge: &genBridgeTxResult{
				counterpartAssetID:         bridgeTx.AssetID,
				counterpartTxID:            bridgeTx.IDs[0], // Reference to initiation transaction
				followUpData:               data,
				previousBridgeCompletionID: initialCompletionID,
				bridgeName:                 bridgeName,
			},
		}, nil
	})
}

// getLatestBridgeCompletion returns the latest completion transaction for a
// bridge. Generally this will return the only completion transaction for a
// bridge, except for withdrawing POL from Polygon POS to Ethereum, in which
// case it may return either the initial or follow-up completion transaction.
func (w *assetWallet) getLatestBridgeCompletion(initiationTxID string) (*extendedWalletTx, error) {
	// Check if the completion is still pending
	w.nonceMtx.RLock()
	for _, tx := range w.pendingTxs {
		if tx.Type == txdb.CompleteBridge {
			if tx.BridgeCounterpartTx == nil || len(tx.BridgeCounterpartTx.IDs) < 1 {
				w.log.Warnf("bridge completion transaction %s has no counterpart transaction", tx.ID)
				continue
			}
			if tx.BridgeCounterpartTx.IDs[0] == initiationTxID {
				return tx, nil
			}
		}
	}
	w.nonceMtx.RUnlock()

	// If not pending, check if it's in the DB
	completions, err := w.txDB.getBridgeCompletions(initiationTxID)
	if err != nil {
		return nil, fmt.Errorf("error getting bridge completions: %w", err)
	}
	if len(completions) > 0 {
		return completions[len(completions)-1], nil
	}

	return nil, nil
}

// completeBridgeIfNeeded sends a completion transaction for a bridge if it is
// required. There are three types of bridges we currently support:
//
//  1. Bridges that do not require any completion transactions. The wallet
//     just needs to check that the funds have been allocated on the destination
//     chain, and the bridge can be marked as complete.
//  2. Bridges that require a single completion transaction. In this case, the
//     wallet just needs to send the completion transaction, and once it is
//     confirmed, the bridge can be marked as complete.
//  3. Withdrawing POL from Polygon POS to Ethereum. This is a special case,
//     as it uses an ancient plasma bridge technology. It requires an initial
//     completion transaction that must be completed by the user, then a follow-up
//     transaction that may or may not be required, and even if it is done, the
//     bridge may not be complete. The first transaction puts the funds into an
//     exit queue, which may be processed by anyone. The "processExits" function
//     on the contract will clear the first few exits from the queue, and if the
//     user's exit is one of the first few, the bridge will be completed, but if
//     it is not an additional "processExits" call will be required. In practice,
//     generally only a single call is required, so we make the initial call
//     (unless someone else has already called it), and if our exit has not been
//     processed, we just wait until someone else calls it again. This will
//     handle the vast majority of cases.
func (w *assetWallet) completeBridgeIfNeeded(ctx context.Context, bridgeTx *txdb.BridgeCounterpartTx, amount uint64, data []byte, bridgeName string) error {
	bridge, ok := w.bridges[bridgeName]
	if !ok {
		return fmt.Errorf("bridge %s not found", bridgeName)
	}

	var completionTxIDs []string
	var fees uint64
	var sendNote, isComplete bool
	defer func() {
		if sendNote {
			w.emit.BridgeCompleted(bridgeTx.AssetID, bridgeTx.IDs[0], completionTxIDs, amount, fees, isComplete)
		}
	}()

	// Some bridges do not require any completion transactions. The wallet just
	// needs to check that the funds have been allocated on the destination
	// chain.
	if !bridge.requiresCompletion(w.assetID) {
		var err error
		isComplete, err = bridge.verifyBridgeCompletion(ctx, data)
		if isComplete {
			sendNote = true
		}
		return err
	}

	// If this bridge requires a completion transaction, get the latest
	// completion transaction for this bridge. Some bridges require multiple
	// completion transactions.
	var initiationTxID string
	if len(bridgeTx.IDs) > 0 {
		initiationTxID = bridgeTx.IDs[0]
	}
	wt, err := w.getLatestBridgeCompletion(initiationTxID)
	if err != nil {
		return fmt.Errorf("error getting bridge completion: %w", err)
	}

	// If no completion transaction has been done yet, complete the bridge
	// based on the mint data.
	if wt == nil {
		txID, err := w.completeBridge(ctx, data, bridgeTx, amount, bridge, bridgeName)
		if err != nil {
			return fmt.Errorf("error completing bridge: %w", err)
		}
		// Send notification with isComplete=false. Fees are 0 since tx isn't confirmed.
		completionTxIDs = []string{txID}
		sendNote = true
		return nil
	}
	if !wt.Confirmed {
		return nil
	}

	// If the completion transaction is confirmed and the bridge does not
	// require a follow-up completion, emit the bridge completed notification.
	if !bridge.requiresFollowUpCompletion(w.assetID) {
		isComplete = true
		completionTxIDs = []string{wt.ID}
		fees = wt.Fees
		sendNote = true
		return nil
	}

	// If BridgeFollowUpVerificationData is not nil, that means that this
	// transaction is a follow-up transaction which needs to be verified.
	if wt.BridgeFollowUpData != nil {
		complete, err := bridge.verifyBridgeCompletion(ctx, wt.BridgeFollowUpData)
		if err != nil {
			return fmt.Errorf("error getting follow-up completion data: %w", err)
		}
		completionTxIDs = []string{wt.PreviousBridgeCompletionID, wt.ID}
		isComplete = complete
		if isComplete {
			fees = wt.Fees
			if prevTx, err := w.txDB.getTx(common.HexToHash(wt.PreviousBridgeCompletionID)); err == nil && prevTx != nil {
				fees += prevTx.Fees
			}
			sendNote = true
		}
		return nil
	}

	// If the bridge requires a follow-up completion, complete the follow-up
	// bridge. The follow-up completion may not be required, as it may have
	// been completed by another user.
	required, followUpData, err := bridge.getFollowUpCompletionData(ctx, wt.ID)
	if err != nil {
		return fmt.Errorf("error getting follow-up completion data: %w", err)
	}
	if !required {
		completionTxIDs = []string{wt.ID}
		fees = wt.Fees
		isComplete = true
		sendNote = true
		return nil
	}

	txID, err := w.completeBridgeFollowUp(ctx, followUpData, wt.ID, amount, bridgeTx, bridge, bridgeName)
	if err != nil {
		return fmt.Errorf("error completing bridge: %w", err)
	}
	// Send notification with isComplete=false. Fees are 0 since follow-up tx isn't confirmed.
	completionTxIDs = []string{wt.ID, txID}
	sendNote = true
	return nil
}

// CompleteBridge completes a bridge by submitting a transaction that mints
// or unlocks coins on the destination chain.
func (w *assetWallet) CompleteBridge(ctx context.Context, bridgeTx *txdb.BridgeCounterpartTx, amount uint64, data []byte, bridgeName string) error {
	return w.completeBridgeIfNeeded(ctx, bridgeTx, amount, data, bridgeName)
}

// SupportedDestinations returns the list of asset IDs that the wallet can bridge funds to.
func (w *assetWallet) SupportedDestinations() map[uint32][]string {
	allBridgeDestinations := map[uint32][]string{}

	for bridgeName, bridge := range w.bridges {
		destinations := bridge.supportedDestinations(w.assetID)
		if len(destinations) == 0 {
			continue
		}
		for _, destination := range destinations {
			if _, ok := allBridgeDestinations[destination]; !ok {
				allBridgeDestinations[destination] = []string{}
			}
			allBridgeDestinations[destination] = append(allBridgeDestinations[destination], bridgeName)
		}
	}
	return allBridgeDestinations
}

// BridgeInitiationFeesAndLimits returns the estimated fees, limits, and whether the
// bridge has limits.
func (w *assetWallet) BridgeInitiationFeesAndLimits(bridgeName string, destAssetID uint32) (fees uint64, limits [2]uint64, hasLimits bool, err error) {
	bridge, found := w.bridges[bridgeName]
	if !found {
		return 0, [2]uint64{}, false, fmt.Errorf("bridge %s not found", bridgeName)
	}

	initiateGas := bridge.initiateBridgeGas(w.assetID)

	maxFeeRateWei, _, err := w.recommendedMaxFeeRate(w.ctx)
	if err != nil {
		return 0, [2]uint64{}, false, fmt.Errorf("error calculating bridge fee rate: %w", err)
	}
	maxFeeRateGwei := WeiToGweiCeil(maxFeeRateWei)

	minBig, maxBig, hasLimits, err := bridge.bridgeLimits(w.assetID, destAssetID)
	if err != nil {
		return 0, [2]uint64{}, false, fmt.Errorf("error getting bridge limits: %w", err)
	}

	min := w.atomize(minBig)
	max := w.atomize(maxBig)

	return initiateGas * maxFeeRateGwei, [2]uint64{min, max}, hasLimits, nil
}

// BridgeCompletionFees returns the estimated fees for completing a bridge,
// and whether the wallet has sufficient balance available to pay those fees.
func (w *assetWallet) BridgeCompletionFees(bridgeName string) (uint64, bool, error) {
	bridge, found := w.bridges[bridgeName]
	if !found {
		return 0, false, fmt.Errorf("bridge %s not found", bridgeName)
	}

	completeGas := bridge.completeBridgeGas(w.assetID)
	followUpCompleteGas := bridge.followUpCompleteBridgeGas()

	maxFeeRateWei, _, err := w.recommendedMaxFeeRate(w.ctx)
	if err != nil {
		return 0, false, fmt.Errorf("error calculating bridge fee rate: %w", err)
	}
	maxFeeRateGwei := WeiToGweiCeil(maxFeeRateWei)

	fees := (completeGas + followUpCompleteGas) * maxFeeRateGwei

	// Get available fee balance
	var availableFeeBalance uint64
	if w.assetID == w.baseChainID {
		// Base asset - fees come from own balance
		bal, err := w.balance()
		if err != nil {
			return fees, false, fmt.Errorf("error getting balance: %w", err)
		}
		availableFeeBalance = bal.Available
	} else {
		// Token - fees come from parent asset
		w.walletsMtx.RLock()
		baseWallet := w.wallets[w.baseChainID]
		w.walletsMtx.RUnlock()
		if baseWallet == nil {
			return fees, false, fmt.Errorf("base wallet not found")
		}
		bal, err := baseWallet.balance()
		if err != nil {
			return fees, false, fmt.Errorf("error getting base asset balance: %w", err)
		}
		availableFeeBalance = bal.Available
	}

	return fees, availableFeeBalance >= fees, nil
}

// DepositAddress returns an address for the exchange wallet. This implementation
// is idempotent, always returning the same address for a given assetWallet.
func (eth *baseWallet) DepositAddress() (string, error) {
	return eth.addr.String(), nil
}

// Unlock unlocks the exchange wallet.
func (eth *ETHWallet) Unlock(pw []byte) error {
	return eth.node.unlock(string(pw))
}

// Lock locks the exchange wallet.
func (eth *ETHWallet) Lock() error {
	return eth.node.lock()
}

// Locked will be true if the wallet is currently locked.
func (eth *ETHWallet) Locked() bool {
	return eth.node.locked()
}

// SendTransaction broadcasts a valid fully-signed transaction.
func (eth *baseWallet) SendTransaction(rawTx []byte) ([]byte, error) {
	tx := new(types.Transaction)
	err := tx.UnmarshalBinary(rawTx)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal transaction: %w", err)
	}
	if err := eth.node.sendSignedTransaction(eth.ctx, tx); err != nil {
		return nil, err
	}
	return tx.Hash().Bytes(), nil
}

// ValidateAddress checks whether the provided address is a valid hex-encoded
// Ethereum address.
func (w *ETHWallet) ValidateAddress(address string) bool {
	return common.IsHexAddress(address)
}

// ValidateAddress checks whether the provided address is a valid hex-encoded
// Ethereum address.
func (w *TokenWallet) ValidateAddress(address string) bool {
	return common.IsHexAddress(address)
}

// isValidSend is a helper function for both token and ETH wallet. It returns an
// error if subtract is true, addr is invalid or value is zero.
func isValidSend(addr string, value uint64, subtract bool) error {
	if value == 0 {
		return fmt.Errorf("cannot send zero amount")
	}
	if subtract {
		return fmt.Errorf("wallet does not support subtracting network fee from send amount")
	}
	if !common.IsHexAddress(addr) {
		return fmt.Errorf("invalid hex address %q", addr)
	}
	return nil
}

// canSend ensures that the wallet has enough to cover send value and returns
// the fee rate and max fee required for the send tx. If isPreEstimate is false,
// wallet balance must be enough to cover total spend.
func (w *ETHWallet) canSend(value uint64, verifyBalance, isPreEstimate bool) (maxFee uint64, maxFeeRate, tipRate *big.Int, err error) {
	maxFeeRate, tipRate, err = w.recommendedMaxFeeRate(w.ctx)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("error getting max fee rate: %w", err)
	}
	maxFeeRateGwei := WeiToGweiCeil(maxFeeRate)

	maxFee = defaultSendGasLimit*maxFeeRateGwei + w.sendL1Fee()

	if isPreEstimate {
		maxFee = maxFee * 12 / 10 // 20% buffer
	}

	if verifyBalance {
		bal, err := w.Balance()
		if err != nil {
			return 0, nil, nil, err
		}
		avail := bal.Available
		if avail < value {
			return 0, nil, nil, fmt.Errorf("not enough funds to send: have %d gwei need %d gwei", avail, value)
		}

		if avail < value+maxFee {
			return 0, nil, nil, fmt.Errorf("available funds %d gwei cannot cover value being sent: need %d gwei + %d gwei max fee", avail, value, maxFee)
		}
	}
	return
}

// checkTokenBalance returns an error if the wallet's available token balance
// cannot cover value.
func (w *TokenWallet) checkTokenBalance(value uint64) error {
	bal, err := w.Balance()
	if err != nil {
		return err
	}
	if bal.Available < value {
		return fmt.Errorf("not enough tokens: have %[1]d %[3]s need %[2]d %[3]s", bal.Available, value, w.ui.AtomicUnit)
	}
	return nil
}

// canSend returns the fee rate and max fee required for a token send with the
// specified gas limit, and if verifyBalance is true, ensures the parent wallet
// can cover that fee. The token balance itself is the caller's check (see
// checkTokenBalance), made before any live gas estimate so that an
// over-balance amount is reported as such rather than as a simulated revert.
// gasLimit is the token's GasEstimate for a pre-estimate or the buffered live
// estimate for an actual send.
func (w *TokenWallet) canSend(gasLimit uint64, verifyBalance, isPreEstimate bool) (maxFee uint64, maxFeeRate, tipRate *big.Int, err error) {
	maxFeeRate, tipRate, err = w.recommendedMaxFeeRate(w.ctx)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("error getting max fee rate: %w", err)
	}
	maxFeeRateGwei := WeiToGweiCeil(maxFeeRate)

	maxFee = maxFeeRateGwei*gasLimit + w.sendL1Fee()

	if isPreEstimate {
		maxFee = maxFee * 12 / 10 // 20% buffer
	}

	if verifyBalance {
		ethBal, err := w.parent.Balance()
		if err != nil {
			return 0, nil, nil, fmt.Errorf("error getting base chain balance: %w", err)
		}

		if ethBal.Available < maxFee {
			return 0, nil, nil, fmt.Errorf("insufficient balance to cover token transfer fees. %d < %d",
				ethBal.Available, maxFee)
		}
	}
	return
}

// EstimateSendTxFee returns a tx fee rate estimate for a send tx. The provided fee
// rate is ignored since all sends will use an internally derived fee rate. If
// an address is provided, it will ensure wallet has enough to cover total
// spend.
func (w *ETHWallet) EstimateSendTxFee(addr string, value, _ uint64, _, maxWithdraw bool) (uint64, bool, error) {
	if err := isValidSend(addr, value, maxWithdraw); err != nil && addr != "" { // fee rate estimate for a send tx.
		return 0, false, err
	}
	maxFee, _, _, err := w.canSend(value, addr != "", true)
	if err != nil {
		return 0, false, err
	}
	return maxFee, w.ValidateAddress(addr), nil
}

// StandardSendFee returns the fees for a simple send tx.
func (w *ETHWallet) StandardSendFee(feeRate uint64) uint64 {
	return defaultSendGasLimit*feeRate + w.sendL1Fee()
}

// EstimateSendTxFee returns a tx fee rate estimate for a send tx. The provided fee
// rate is ignored since all sends will use an internally derived fee rate. If
// an address is provided, it will ensure wallet has enough to cover total
// spend.
func (w *TokenWallet) EstimateSendTxFee(addr string, value, _ uint64, _, maxWithdraw bool) (fee uint64, isValidAddress bool, err error) {
	if err := isValidSend(addr, value, maxWithdraw); err != nil && addr != "" { // fee rate estimate for a send tx.
		return 0, false, err
	}
	if addr != "" {
		if err := w.checkTokenBalance(value); err != nil {
			return 0, false, err
		}
	}
	maxFee, _, _, err := w.canSend(w.estimateSendGasLimit(addr, value), addr != "", true)
	if err != nil {
		return 0, false, err
	}
	return maxFee, w.ValidateAddress(addr), nil
}

// estimateSendGasLimit returns the gas limit a transfer of value to addr would
// be sent with, for fee estimation. With a recipient known, that is the same
// buffered live estimate Send uses. Without one, or when the node cannot
// produce an estimate, the token's static GasEstimate stands in. Estimation
// fails when the simulated transfer reverts, typically because the account's
// token balance cannot cover value; falling back keeps a fee figure available
// alongside the balance error canSend reports.
func (w *TokenWallet) estimateSendGasLimit(addr string, value uint64) uint64 {
	if addr == "" {
		return w.gasEstimate.Transfer
	}
	gasLimit, err := w.transferGasLimit(common.HexToAddress(addr), value)
	if err != nil {
		w.log.Debugf("Live transfer gas estimate failed, using static estimate %d: %v", w.gasEstimate.Transfer, err)
		return w.gasEstimate.Transfer
	}
	return gasLimit
}

// StandardSendFee returns the fees for a simple send tx.
func (w *TokenWallet) StandardSendFee(feeRate uint64) uint64 {
	return w.gasEstimate.Transfer*feeRate + w.sendL1Fee()
}

// RestorationInfo returns information about how to restore the wallet in
// various external wallets.
func (w *ETHWallet) RestorationInfo(seed []byte) ([]*asset.WalletRestoration, error) {
	privateKey, zero, err := w.privKeyFn(seed)
	if err != nil {
		return nil, err
	}
	defer zero()

	return []*asset.WalletRestoration{
		{
			Target:   "MetaMask",
			Seed:     hex.EncodeToString(privateKey),
			SeedName: "Private Key",
			Instructions: "Accounts can be imported by private key only if MetaMask has already be initialized. " +
				"If this is your first time installing MetaMask, create a new wallet and secret recovery phrase. " +
				"Then, to import your DEX account into MetaMask, follow the steps below:\n" +
				`1. Open the settings menu
				 2. Select "Import Account"
				 3. Make sure "Private Key" is selected, and enter the private key above into the box`,
		},
	}, nil
}

// Send sends the exact value to the specified address. The provided fee rate is
// ignored since all sends will use an internally derived fee rate.
func (w *ETHWallet) Send(addr string, value, _ uint64) (asset.Coin, error) {
	if err := isValidSend(addr, value, false); err != nil {
		return nil, err
	}

	_ /* maxFee */, maxFeeRate, tipRate, err := w.canSend(value, true, false)
	if err != nil {
		return nil, err
	}
	// TODO: Subtract option.
	// if avail < value+maxFee {
	// 	value -= maxFee
	// }

	tx, err := w.sendToAddr(common.HexToAddress(addr), value, maxFeeRate, tipRate)
	if err != nil {
		return nil, err
	}

	// nonceMtx is released by sendToAddr, so balance() is safe to call here.
	// Emit an immediate balance update so the UI reflects the pending deduction
	// without waiting for the next block.
	if bal, err := w.balance(); err == nil {
		w.emit.BalanceChange(bal)
	}

	return &coin{txHash: tx.Hash(), value: value}, nil
}

// Send sends the exact value to the specified address. Fees are taken from the
// parent wallet. The provided fee rate is ignored since all sends will use an
// internally derived fee rate.
func (w *TokenWallet) Send(addr string, value, _ uint64) (asset.Coin, error) {
	if err := isValidSend(addr, value, false); err != nil {
		return nil, err
	}

	// Reject an over-balance send before asking the node for a gas estimate.
	// The transfer would revert during estimation, which surfaces as an opaque
	// execution error rather than a balance error.
	if err := w.checkTokenBalance(value); err != nil {
		return nil, err
	}

	to := common.HexToAddress(addr)
	gasLimit, err := w.transferGasLimit(to, value)
	if err != nil {
		return nil, err
	}

	_ /* maxFee */, maxFeeRate, tipRate, err := w.canSend(gasLimit, true, false)
	if err != nil {
		return nil, err
	}

	tx, err := w.sendToAddr(to, value, gasLimit, maxFeeRate, tipRate)
	if err != nil {
		return nil, err
	}

	if bal, err := w.balance(); err == nil {
		w.emit.BalanceChange(bal)
	}
	if bal, err := w.parent.balance(); err == nil {
		w.parent.emit.BalanceChange(bal)
	}

	return &coin{txHash: tx.Hash(), value: value}, nil
}

// SyncStatus is information about the blockchain sync status.
//
// TODO: Since the merge, the sync status from a geth full node, namely the
// prog.CurrentBlock prog.HighestBlock, always seem to be the same number.
// Initial sync will always be zero. Later when restarting the node they move
// together but never indicate the highest known block on the chain. Further
// more, requesting the best block header starts to fail after a few tries
// during initial sync. Investigate how to get correct sync progress.
func (eth *baseWallet) SyncStatus() (*asset.SyncStatus, error) {
	prog, tipTime, err := eth.node.syncProgress(eth.ctx)
	if err != nil {
		return nil, err
	}
	checkHeaderTime := func() bool {
		// Time in the header is in seconds.
		timeDiff := time.Now().Unix() - int64(tipTime)
		if timeDiff > MaxBlockInterval && eth.net != util.Simnet {
			eth.log.Infof("Time since block (%d sec) exceeds %d sec. "+
				"Assuming not in sync. Ensure your computer's system clock "+
				"is correct.", timeDiff, MaxBlockInterval)
			return false
		}
		return true
	}
	return &asset.SyncStatus{
		Synced:         checkHeaderTime() && eth.node.peerCount() > 0,
		StartingBlocks: eth.startingBlocks.Load(),
		TargetHeight:   prog.HighestBlock,
		Blocks:         prog.CurrentBlock,
	}, nil
}

// currentNetworkFees give the current base fee rate (from the best header),
// and recommended tip cap.
func (w *baseWallet) currentNetworkFees(ctx context.Context) (baseRate, tipRate *big.Int, err error) {
	tip := w.tipHeight()
	c := &w.currentFees
	c.Lock()
	defer c.Unlock()
	if tip > 0 && c.blockNum == tip {
		return c.baseRate, c.tipRate, nil
	}
	c.baseRate, c.tipRate, err = w.node.currentFees(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting net fee state: %v", err)
	}
	c.blockNum = tip
	return c.baseRate, c.tipRate, nil
}

// recommendedMaxFeeRate finds a recommended max fee rate using the somewhat
// standard baseRate * 2 + tip formula.
func (eth *baseWallet) recommendedMaxFeeRate(ctx context.Context) (maxFeeRate, tipRate *big.Int, err error) {
	base, tip, err := eth.currentNetworkFees(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting net fee state: %v", err)
	}

	return new(big.Int).Add(
		tip,
		new(big.Int).Mul(base, big.NewInt(2)),
	), tip, nil
}

// recommendedMaxFeeRateGwei gets the recommended max fee rate and converts it
// to gwei.
func (w *baseWallet) recommendedMaxFeeRateGwei(ctx context.Context) (uint64, error) {
	feeRate, _, err := w.recommendedMaxFeeRate(ctx)
	if err != nil {
		return 0, err
	}
	return WeiToGweiSafe(feeRate)
}

// FeeRate satisfies asset.Wallet.
func (eth *baseWallet) FeeRate() uint64 {
	r, err := eth.recommendedMaxFeeRateGwei(eth.ctx)
	if err != nil {
		eth.log.Errorf("Error getting max fee recommendation: %v", err)
		return 0
	}
	return r
}

// baseAssetWallet returns the assetWallet of the base asset on the chain.
func (eth *baseWallet) baseAssetWallet() (*assetWallet, error) {
	eth.walletsMtx.RLock()
	defer eth.walletsMtx.RUnlock()

	wallet, ok := eth.wallets[eth.baseChainID]
	if !ok {
		return nil, fmt.Errorf("base asset wallet not found")
	}

	return wallet, nil
}

func (eth *ETHWallet) checkPeers() {
	numPeers := eth.node.peerCount()

	for _, w := range eth.connectedWallets() {
		prevPeer := atomic.SwapUint32(&w.lastPeerCount, numPeers)
		if prevPeer != numPeers {
			w.peersChange(numPeers, nil)
		}
	}
}

func (eth *ETHWallet) monitorPeers(ctx context.Context) {
	ticker := time.NewTicker(peerCountTicker)
	defer ticker.Stop()
	for {
		eth.checkPeers()

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// tipPollDecision is what monitorBlocks does with one ticker tick.
type tipPollDecision int

const (
	// tipPollNone leaves the tick to the header feed, which is delivering and
	// not yet due its cross-check. The poll would only re-read a header
	// applyTipChange has already seen.
	tipPollNone tipPollDecision = iota
	// tipPollBlocking reads the tip on the loop goroutine. There is no feed to
	// hold up, so there is nothing to gain from a goroutine.
	tipPollBlocking
	// tipPollBackground reads the tip off the loop, so a slow RPC call cannot
	// delay a header the feed is about to deliver.
	tipPollBackground
)

// tipPollOnTick decides whether a ticker tick should read the tip, and on which
// goroutine. feed reports whether a shared header channel exists at all,
// lastWSHead is when a header last arrived on it, and lastPoll when the tip was
// last read across every provider.
//
// Split out of monitorBlocks because it is the subtlest state in that loop and
// the only part of it that is decidable from four values. The three cases:
//
//   - No feed, so the tick is the only source of tip changes. HTTP-only and
//     simnet configurations live here permanently.
//   - A feed that has gone quiet for longer than any of these chains' block
//     times, so every subscription on it may be dead, or a feed that is
//     delivering and merely due its cross-check for a provider that has dropped
//     off it. A provider that could not be websocket-subscribed pushes nothing
//     onto the shared channel, so a feed kept live by the subscribed providers
//     alone would never look at its cache.
//   - Otherwise the feed covers tip detection on its own.
//
// lastWSHead zero on the first tick is deliberate rather than incidental: it
// makes the staleness test true until a real header has arrived, so a
// configuration that never delivers one keeps the full 1s cadence.
func tipPollOnTick(feed bool, lastWSHead, lastPoll, now time.Time) tipPollDecision {
	if !feed {
		return tipPollBlocking
	}
	if now.Sub(lastWSHead) >= wsHeadStaleness || now.Sub(lastPoll) >= headerCheckInterval {
		return tipPollBackground
	}
	return tipPollNone
}

// monitorBlocks pings for new blocks and runs the tipChange callback function
// when the block changes. New blocks are also scanned for potential contract
// redeems. The providers' WebSocket subscriptions are the primary driver,
// delivering headers over the shared newHeadCh as they are mined; the ticker
// polls only while no subscription exists at all, the feed has fallen quiet for
// wsHeadStaleness, or a live feed is due its periodic cross-check for a provider
// that has dropped off it.
func (eth *ETHWallet) monitorBlocks(ctx context.Context) {
	ticker := time.NewTicker(chainOpTick)
	defer ticker.Stop()

	// WebSocket subscriptions deliver headers directly. Selecting on this
	// channel gives instant tip-change detection; the ticker is the fallback
	// for HTTP-only connections and a safety net if WS drops.
	var newHeadCh <-chan *types.Header
	if m, ok := eth.node.(*multiRPCClient); ok {
		newHeadCh = m.newHeadCh
	}

	// lastWSHead is when a header last arrived over the feed. Every provider
	// pushes to the one shared newHeadCh, so while any of them is delivering the
	// tick below has nothing to add: the header it would read is one
	// applyTipChange has already seen, and a provider that is ahead has already
	// won on the channel rather than needing the poll to notice. Tracking
	// freshness rather than merely whether newHeadCh is non-nil keeps the 1s
	// fallback for the case that needs it, every subscription silent at once.
	// Zero to begin with, so the first tick polls until one has proved itself.
	//
	// What the suppressed tick was costing is a local read, not a round trip:
	// bestHeader returns the highest header any provider has cached, and
	// cachedTip holds one for a minute once that provider has seen a websocket
	// head, so a warm cache never reaches the wire. Cheap, but it and the no-op
	// applyTipChange behind it ran once a second for the life of the session
	// with nothing to report.
	//
	// lastPoll is when the tip was last read across every provider. The poll is
	// still not purely a fallback for a dead feed, because an endpoint set can be
	// mixed: a provider that could not be websocket-subscribed falls back to HTTP
	// and gets a refreshHeader goroutine instead (multirpc.go), and the cache that
	// writes is reachable only through bestHeader. Nothing pushes those providers
	// onto newHeadCh, so a feed kept live by the subscribed providers alone would
	// never look at them. Hence a live feed still polls, just at
	// headerCheckInterval: that is the cadence refreshHeader writes on, and
	// reading those caches faster than they are written gains nothing. With every
	// provider subscribed there is no such cache and the poll is a no-op, which is
	// the cost of keeping one rule for both cases.
	var lastWSHead, lastPoll time.Time

	checkNewBlocks := func() {
		defer eth.pollingTip.Store(false)
		innerCtx, cancel := context.WithTimeout(ctx, onChainDataFetchTimeout)
		defer cancel()
		bestHdr, err := eth.node.bestHeader(innerCtx)
		if err != nil {
			eth.log.Errorf("failed to get best header: %v", err)
			return
		}
		eth.applyTipChange(bestHdr)
	}

	for {
		select {
		case hdr := <-newHeadCh:
			if hdr != nil {
				// Only a real header counts as proof of life; a run of nils
				// must not suppress the fallback poll below.
				lastWSHead = time.Now()
				eth.applyTipChange(hdr)
			}
		case <-ticker.C:
			switch tipPollOnTick(newHeadCh != nil, lastWSHead, lastPoll, time.Now()) {
			case tipPollBlocking:
				checkNewBlocks()
			case tipPollBackground:
				// Poll in a goroutine so a slow or blocked RPC call cannot hold
				// up delivery of the header that would prove the feed alive.
				// applyTipChange is safe to call concurrently (same-hash calls
				// are no-ops).
				//
				// Stamped inside the swap, so a tick that finds an earlier poll
				// still running has not consumed the cross-check: the next tick
				// retries a second later rather than waiting out another whole
				// headerCheckInterval against a stale lastPoll.
				if eth.pollingTip.CompareAndSwap(false, true) {
					lastPoll = time.Now()
					go checkNewBlocks()
				}
			}
		case <-ctx.Done():
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// applyTipChange updates the current tip if hdr represents a new best header.
// When a tip change is detected, pending txs and approvals are rechecked and a
// TipChange notification is emitted to each connected wallet.
func (eth *ETHWallet) applyTipChange(bestHdr *types.Header) {
	bestHash := bestHdr.Hash()
	// This method is called frequently. Don't hold write lock
	// unless tip has changed.
	currentTip := eth.tip()
	currentTipHash := currentTip.Hash()
	if currentTipHash == bestHash {
		return
	}

	eth.tipMtx.Lock()
	currentTip = eth.currentTip
	if currentTip.Hash() == bestHash || bestHdr.Number.Cmp(currentTip.Number) < 0 {
		eth.tipMtx.Unlock()
		return
	}
	eth.currentTip = bestHdr
	eth.tipMtx.Unlock()

	// Run RPC-heavy pending tx checks in a goroutine so TipChange is emitted
	// without waiting for receipt fetches. checkPendingTxs serializes
	// internally via nonceMtx, so concurrent calls are safe.
	eth.queuePendingTxCheck()

	for _, w := range eth.connectedWallets() {
		w.checkPendingApprovals()
		w.emit.TipChange(bestHdr.Number.Uint64())
	}
}

func (w *baseWallet) queuePendingTxCheck() {
	if w == nil || w.ctx == nil {
		return
	}
	w.pendingTxCheckQueued.Store(true)
	if !w.pendingTxCheckRunning.CompareAndSwap(false, true) {
		return
	}
	go w.runPendingTxChecks()
}

func (w *baseWallet) runPendingTxChecks() {
	if w == nil || w.ctx == nil {
		return
	}
	defer func() {
		w.pendingTxCheckRunning.Store(false)
		if w.ctx != nil && w.pendingTxCheckQueued.Load() && w.pendingTxCheckRunning.CompareAndSwap(false, true) {
			go w.runPendingTxChecks()
		}
	}()

	for {
		w.pendingTxCheckQueued.Store(false)
		w.checkPendingTxs()
		if w.ctx.Err() != nil || !w.pendingTxCheckQueued.Load() {
			return
		}
	}
}

// withLocalTxRead runs a function that reads a pending or DB tx under
// read-lock. Certain DB transactions in undeterminable states will not be
// used.
func (w *baseWallet) withLocalTxRead(txHash common.Hash, f func(*extendedWalletTx)) bool {
	withPendingTxRead := func(txHash common.Hash, f func(*extendedWalletTx)) bool {
		w.nonceMtx.RLock()
		defer w.nonceMtx.RUnlock()
		for _, pendingTx := range w.pendingTxs {
			if pendingTx.txHash == txHash {
				f(pendingTx)
				return true
			}
		}
		return false
	}
	if withPendingTxRead(txHash, f) {
		return true
	}

	// Could be finalized and in the database.
	if confirmedTx, err := w.txDB.getTx(txHash); err != nil {
		w.log.Errorf("Error getting DB transaction: %v", err)
	} else if confirmedTx != nil {
		if !confirmedTx.Confirmed && confirmedTx.Receipt == nil && !confirmedTx.AssumedLost && confirmedTx.NonceReplacement == "" {
			// If it's in the db but not in pendingTxs, and we know nothing
			// about the tx, don't use it.
			return false
		}
		f(confirmedTx)
		return true
	}
	return false
}

// walletTxStatus is data copied from an extendedWalletTx.
type walletTxStatus struct {
	confirmed        bool
	blockNum         uint64
	nonceReplacement string
	feeReplacement   bool
	receipt          *types.Receipt
	assumedLost      bool
	relayTxID        string
	rejected         bool
	submissionTime   uint64
}

// localTxStatus looks for an extendedWalletTx and copies critical values to
// a walletTxStatus for use without mutex protection.
func (w *baseWallet) localTxStatus(txHash common.Hash) (_ bool, s *walletTxStatus) {
	return w.withLocalTxRead(txHash, func(wt *extendedWalletTx) {
		s = &walletTxStatus{
			confirmed:        wt.Confirmed,
			blockNum:         wt.BlockNumber,
			nonceReplacement: wt.NonceReplacement,
			feeReplacement:   wt.FeeReplacement,
			receipt:          wt.Receipt,
			assumedLost:      wt.AssumedLost,
			relayTxID:        wt.RelayTxID,
			rejected:         wt.Rejected,
			submissionTime:   wt.SubmissionTime,
		}
	}), s
}

func (w *assetWallet) checkPendingApprovals() {
	var confirmed []func()

	w.approvalsMtx.Lock()
	for version, pendingApproval := range w.pendingApprovals {
		if w.ctx.Err() != nil {
			break
		}
		var isConfirmed bool
		if found, txData := w.localTxStatus(pendingApproval.txHash); found {
			isConfirmed = txData.blockNum > 0 && txData.blockNum <= w.tipHeight()
		} else {
			confs, err := w.node.transactionConfirmations(w.ctx, pendingApproval.txHash)
			if err != nil && !errors.Is(err, asset.CoinNotFoundError) {
				w.log.Errorf("error getting confirmations for tx %s: %v", pendingApproval.txHash, err)
				continue
			}
			isConfirmed = confs > 0
		}
		if isConfirmed {
			confirmed = append(confirmed, pendingApproval.onConfirm)
			delete(w.pendingApprovals, version)
		}
	}
	w.approvalsMtx.Unlock()

	// Run the callbacks after releasing approvalsMtx, and on the caller's
	// goroutine rather than a detached one, so a callback cannot outlive the
	// tip-processing pass that observed the confirmation. The approval is already
	// deleted above, so a callback dropped during shutdown is never retried.
	// This narrows the window rather than closing it: applyTipChange runs on the
	// Connect-tracked monitorBlocks goroutine for WS headers and for the
	// HTTP-only poll, but on an untracked goroutine via the `go checkNewBlocks()`
	// ticker fallback taken while a WS subscription is active.
	for _, onConfirm := range confirmed {
		onConfirm()
	}
}

// sumPendingTxs sums the expected incoming and outgoing values in pending
// transactions stored in pendingTxs. Not used if the node is a
// txPoolFetcher.
func (w *assetWallet) sumPendingTxs() (out, in uint64) {
	isToken := w.assetID != w.baseChainID

	sumPendingTx := func(pendingTx *extendedWalletTx) {
		// Already confirmed, but still in the unconfirmed txs map waiting for
		// txConfsNeededToConfirm confirmations.
		if pendingTx.BlockNumber != 0 {
			return
		}
		if pendingTx.AssumedLost {
			return
		}

		txAssetID := w.baseChainID
		if pendingTx.TokenID != nil {
			txAssetID = *pendingTx.TokenID
		}

		var txOut, txIn uint64
		if txAssetID == w.assetID {
			if txdb.IncomingTxType(pendingTx.Type) {
				txIn = pendingTx.Amount
			} else {
				txOut = pendingTx.Amount
			}
		}
		if !isToken {
			txOut += pendingTx.Fees
		}
		if txOut > 0 || txIn > 0 {
			w.log.Tracef("sumPendingTxs: tx %s type=%s amount=%d fees=%d out=%d in=%d",
				pendingTx.ID, pendingTx.Type, pendingTx.Amount, pendingTx.Fees, txOut, txIn)
		}
		out += txOut
		in += txIn
	}

	w.nonceMtx.RLock()
	defer w.nonceMtx.RUnlock()

	for _, pendingTx := range w.pendingTxs {
		sumPendingTx(pendingTx)
	}

	return
}

func (w *assetWallet) getConfirmedBalance() (*big.Int, error) {
	now := time.Now()
	tip := w.tipHeight()

	w.balances.Lock()
	defer w.balances.Unlock()

	if w.balances.m == nil {
		w.balances.m = make(map[uint32]*cachedBalance)
	}
	// Check to see if we already have one up-to-date
	cached := w.balances.m[w.assetID]
	if cached != nil && cached.height == tip && time.Since(cached.stamp) < time.Minute {
		return cached.bal, nil
	}

	if w.multiBalanceContract == nil {
		var bal *big.Int
		var err error
		if w.assetID == w.baseChainID {
			bal, err = w.node.addressBalance(w.ctx, w.addr)
		} else {
			bal, err = w.tokenBalance()
		}
		if err != nil {
			return nil, err
		}
		w.balances.m[w.assetID] = &cachedBalance{
			stamp:  now,
			height: tip,
			bal:    bal,
		}
		return bal, nil
	}

	// Either not cached, or outdated. Fetch anew.
	var tokenAddrs []common.Address
	idIndexes := map[int]uint32{
		0: w.baseChainID,
	}
	i := 1
	for assetID, tkn := range w.tokens {
		netToken := tkn.NetTokens[w.net]
		if netToken == nil || netToken.Address == (common.Address{}) {
			continue
		}
		tokenAddrs = append(tokenAddrs, netToken.Address)
		idIndexes[i] = assetID
		i++
	}
	callOpts := &bind.CallOpts{
		From:    w.addr,
		Context: w.ctx,
	}

	bals, err := w.multiBalanceContract.Balances(callOpts, w.addr, tokenAddrs)
	if err != nil {
		return nil, fmt.Errorf("error fetching multi-balance: %w", err)
	}
	if len(bals) != len(idIndexes) {
		return nil, fmt.Errorf("wrong number of balances in multi-balance result. wanted %d, got %d", len(idIndexes), len(bals))
	}
	var reqBal *big.Int
	for i, bal := range bals {
		assetID := idIndexes[i]
		if assetID == w.assetID {
			reqBal = bal
		}
		w.balances.m[assetID] = &cachedBalance{
			stamp:  now,
			height: tip,
			bal:    bal,
		}
	}
	if reqBal == nil {
		return nil, fmt.Errorf("requested asset not in multi-balance result: %v", err)
	}
	return reqBal, nil
}

func (w *assetWallet) balanceWithTxPool() (*Balance, error) {
	isToken := w.assetID != w.baseChainID
	confirmed, err := w.getConfirmedBalance()
	if err != nil {
		return nil, fmt.Errorf("balance error: %v", err)
	}

	txPoolNode, is := w.node.(txPoolFetcher)
	if !is {
		for {
			out, in := w.sumPendingTxs()
			checkBal, err := w.getConfirmedBalance()
			if err != nil {
				return nil, fmt.Errorf("balance consistency check error: %v", err)
			}
			outEVM := w.evmify(out)
			// If our balance has gone down in the interim, we'll use the lower
			// balance, but ensure that we're not setting up an underflow for
			// available balance.
			if checkBal.Cmp(confirmed) != 0 {
				w.log.Debugf("balance changed while checking pending txs. Trying again.")
				confirmed = checkBal
				continue
			}
			if outEVM.Cmp(confirmed) > 0 {
				// This can happen transiently when a tx has just
				// been broadcast and the max fee estimate exceeds
				// the remaining balance. Clamp to zero rather than
				// erroring - the tx will confirm shortly.
				w.log.Warnf("Pending out (%s) > balance (%s). Clamping available balance to zero.", outEVM, confirmed)
				outEVM = confirmed
			}

			return &Balance{
				Current:    confirmed,
				PendingOut: outEVM,
				PendingIn:  w.evmify(in),
			}, nil
		}
	}

	pendingTxs, err := txPoolNode.pendingTransactions()
	if err != nil {
		return nil, fmt.Errorf("error getting pending txs: %w", err)
	}

	outgoing := new(big.Int)
	incoming := new(big.Int)
	zero := new(big.Int)

	addFees := func(tx *types.Transaction) {
		gas := new(big.Int).SetUint64(tx.Gas())
		// For legacy transactions, GasFeeCap returns gas price
		if gasFeeCap := tx.GasFeeCap(); gasFeeCap != nil {
			outgoing.Add(outgoing, new(big.Int).Mul(gas, gasFeeCap))
		} else {
			w.log.Errorf("unable to calculate fees for tx %s", tx.Hash())
		}
	}

	ethSigner := types.LatestSigner(w.node.chainConfig()) // "latest" good for pending

	for _, tx := range pendingTxs {
		from, _ := ethSigner.Sender(tx)
		if from != w.addr {
			continue
		}

		if !isToken {
			// Add tx fees
			addFees(tx)
		}

		var contractOut uint64
		if w.tokenContractor != nil {
			in, out, err := w.tokenContractor.value(w.ctx, tx)
			if err != nil {
				w.log.Errorf("token contractor value error: %v", err)
			} else {
				contractOut += out
				if in > 0 {
					incoming.Add(incoming, w.evmify(in))
				}
			}
		}
		if contractOut > 0 {
			outgoing.Add(outgoing, w.evmify(contractOut))
		} else if !isToken {
			// Count withdraws and sends for ETH.
			v := tx.Value()
			if v.Cmp(zero) > 0 {
				outgoing.Add(outgoing, v)
			}
		}
	}

	return &Balance{
		Current:    confirmed,
		PendingOut: outgoing,
		PendingIn:  incoming,
	}, nil
}

// Uncomment here and in sendToAddr to test actionTypeLostNonce.
// var nonceBorked atomic.Bool
// func (w *ETHWallet) borkNonce(tx *types.Transaction) {
// 	fmt.Printf("\n##### losing tx for lost nonce testing\n\n")
// 	txHash := tx.Hash()
// 	v := big.NewInt(GweiFactor)
// 	spoofTx := types.NewTransaction(tx.Nonce(), w.addr, v, defaultSendGasLimit, v, nil)
// 	spoofHash := tx.Hash()
// 	pendingSpoofTx := w.extendedTx(spoofTx, txdb.SelfSend, GweiFactor)
// 	w.nonceMtx.Lock()
// 	for i, pendingTx := range w.pendingTxs {
// 		if pendingTx.txHash == txHash {
// 			w.pendingTxs[i] = pendingSpoofTx
// 			w.tryStoreDBTx(pendingSpoofTx)
// 			fmt.Printf("\n##### Replaced pending tx %s with spoof tx %s, nonce %d \n\n", txHash, spoofHash, tx.Nonce())
// 			break
// 		}
// 	}
// 	w.nonceMtx.Unlock()
// }

// Uncomment here and in sendToAddr to test actionTypeMissingNonces.
// var nonceFuturized atomic.Bool

// Uncomment here and in sendToAddr to test actionTypeTooCheap
// var nonceSkimped atomic.Bool

// sendToAddr sends funds to the address.
func (w *ETHWallet) sendToAddr(addr common.Address, amt uint64, maxFeeRate, tipRate *big.Int) (*types.Transaction, error) {

	// Uncomment here and above to test actionTypeLostNonce.
	// Also change txAgeOut to like 1 minute.
	// if nonceBorked.CompareAndSwap(false, true) {
	// 	defer w.borkNonce(tx)
	// }

	recipient := addr.Hex()
	res := &genTxResult{
		tx:        nil,
		txType:    txdb.Send,
		amt:       amt,
		recipient: &recipient,
	}
	if addr == w.addr {
		res.txType = txdb.SelfSend
	}

	return res.tx, w.withNonce(w.ctx, func(nonce *big.Int) (*genTxResult, error) {

		// Uncomment here and above to test actionTypeMissingNonces.
		// if nonceFuturized.CompareAndSwap(false, true) {
		// 	fmt.Printf("\n##### advancing nonce for missing nonce testing\n\n")
		// 	nonce.Add(nonce, big.NewInt(3))
		// }

		// Uncomment here and above to test actionTypeTooCheap.
		// if nonceSkimped.CompareAndSwap(false, true) {
		// 	fmt.Printf("\n##### lower max fee rate to test cheap tx handling\n\n")
		// 	maxFeeRate.SetUint64(1)
		// 	tipRate.SetUint64(0)
		// }

		txOpts, err := w.node.txOpts(w.ctx, amt, defaultSendGasLimit, maxFeeRate, tipRate, nonce)
		if err != nil {
			return nil, err
		}
		res.tx, err = w.node.sendTransaction(w.ctx, txOpts, &addr, nil, allowAlreadyKnownFilter)
		if err != nil {
			return nil, err
		}
		return res, nil
	})
}

// transferGasLimit returns the gas limit for an ERC20 transfer of amt atomic
// units to addr: a live eth_estimateGas result plus the standard buffer (see
// bufferedGasLimit). The estimate targets the actual recipient and amount
// because a transfer into an empty balance slot costs more than one into a
// funded slot.
func (w *TokenWallet) transferGasLimit(addr common.Address, amt uint64) (uint64, error) {
	tc, err := w.tokenContractorOrErr()
	if err != nil {
		return 0, err
	}
	est, err := tc.estimateTransferGas(w.ctx, addr, w.evmify(amt))
	if err != nil {
		return 0, fmt.Errorf("error estimating transfer gas: %w", err)
	}
	return bufferedGasLimit(est), nil
}

// sendToAddr sends tokens to the address with the specified gas limit.
func (w *TokenWallet) sendToAddr(addr common.Address, amt, gasLimit uint64, maxFeeRate, tipRate *big.Int) (*types.Transaction, error) {
	recipient := addr.Hex()
	res := &genTxResult{
		tx:        nil,
		txType:    txdb.Send,
		amt:       amt,
		recipient: &recipient,
	}
	if addr == w.addr {
		res.txType = txdb.SelfSend
	}

	tc, err := w.tokenContractorOrErr()
	if err != nil {
		return nil, err
	}

	return res.tx, w.withNonce(w.ctx, func(nonce *big.Int) (*genTxResult, error) {
		txOpts, err := w.node.txOpts(w.ctx, 0, gasLimit, maxFeeRate, tipRate, nonce)
		if err != nil {
			return nil, err
		}
		res.tx, err = tc.transfer(txOpts, addr, w.evmify(amt))
		if err != nil {
			return nil, err
		}
		return res, nil
	})

}

// loadTokenContractor constructs the standalone ERC20 token contractor for the
// wallet's token, keyed on the token's own ERC20 contract address.
func (w *assetWallet) loadTokenContractor() error {
	token, found := w.tokens[w.assetID]
	if !found {
		return fmt.Errorf("token %d not found", w.assetID)
	}
	netToken, found := token.NetTokens[w.net]
	if !found {
		return fmt.Errorf("token %d not found", w.assetID)
	}

	c, err := newERC20TokenContractor(w.net, token, w.addr, w.node.contractBackend())
	if err != nil {
		return fmt.Errorf("error constructing token %s contractor: %w", token.Name, err)
	}
	if netToken.Address != c.tokenAddress() {
		return fmt.Errorf("wrong %s token address. expected %s, got %s", token.Name, netToken.Address, c.tokenAddress())
	}
	w.tokenContractor = c
	return nil
}

// emitTransactionNote sends a TransactionNote to the base asset wallet and
// also the wallet, if applicable.
func (w *baseWallet) emitTransactionNote(tx *txdb.WalletTransaction, new bool) {
	w.walletsMtx.RLock()
	baseWallet, found := w.wallets[w.baseChainID]
	var tokenWallet *assetWallet
	if tx.TokenID != nil {
		tokenWallet = w.wallets[*tx.TokenID]
	}
	w.walletsMtx.RUnlock()

	if found {
		baseWallet.emit.TransactionNote(tx, new)
	} else {
		w.log.Error("emitTransactionNote: base asset wallet not found")
	}
	if tokenWallet != nil {
		tokenWallet.emit.TransactionNote(tx, new)
	}
}

func findMissingNonces(confirmedAt, pendingAt *big.Int, pendingTxs []*extendedWalletTx) (ns []uint64) {
	pendingTxMap := make(map[uint64]struct{})
	// It's not clear whether all providers will update PendingNonceAt if
	// there are gaps. geth doesn't do it on simnet, apparently. We'll use
	// our own pendingTxs max nonce as a backup.
	nonceHigh := big.NewInt(-1)
	for _, pendingTx := range pendingTxs {
		if pendingTx.indexed && pendingTx.Nonce.Cmp(nonceHigh) > 0 {
			nonceHigh.Set(pendingTx.Nonce)
		}
		pendingTxMap[pendingTx.Nonce.Uint64()] = struct{}{}
	}
	nonceHigh.Add(nonceHigh, big.NewInt(1))
	if pendingAt.Cmp(nonceHigh) > 0 {
		nonceHigh.Set(pendingAt)
	}
	for n := confirmedAt.Uint64(); n < nonceHigh.Uint64(); n++ {
		if _, found := pendingTxMap[n]; !found {
			ns = append(ns, n)
		}
	}
	return
}

func (w *baseWallet) missingNoncesActionID() string {
	return fmt.Sprintf("missingNonces_%d", w.baseChainID)
}

// updatePendingTx applies a fetch result to a pending tx, updating its
// confirmation state. fetched carries pre-fetched receipt/tx/header data so
// the RPC calls can happen outside nonceMtx; when fetched is nil the calls
// are made inline (fallback for txs that appear between phase-1 and phase-3
// of checkPendingTxs, e.g. a tx added by a concurrent Send).
//
// w.nonceMtx must be held (write lock).
func (w *baseWallet) updatePendingTx(tip uint64, pendingTx *extendedWalletTx, fetched *txFetchResult) {
	if pendingTx.Confirmed && pendingTx.savedToDB {
		return
	}
	confs := safeConfs(tip, pendingTx.BlockNumber)
	waitingOnConfs := pendingTx.BlockNumber > 0 && confs < w.finalizeConfs
	if waitingOnConfs {
		// We're just waiting on confs. Don't check again until we expect to be
		// finalized.
		confsChanged := pendingTx.Confirms == nil || pendingTx.Confirms.Current != uint32(confs)
		pendingTx.Confirms = &txdb.Confirms{
			Current: uint32(confs),
			Target:  uint32(w.finalizeConfs),
		}
		if confsChanged {
			w.emitTransactionNote(pendingTx.WalletTransaction, false)
		}
		return
	}
	// Use >= so a goroutine carrying an older tip cannot overwrite lastCheck
	// set by a concurrent goroutine that already processed a newer tip.
	if pendingTx.lastCheck >= tip {
		return
	}
	pendingTx.lastCheck = tip

	var updated bool
	defer func() {
		if updated || !pendingTx.savedToDB {
			w.tryStoreDBTx(pendingTx)
			w.emitTransactionNote(pendingTx.WalletTransaction, false)
		}
	}()

	var receipt *types.Receipt
	var tx *types.Transaction
	var err error
	if fetched != nil {
		receipt, tx, err = fetched.receipt, fetched.tx, fetched.err
	} else {
		innerCtx, cancel := context.WithTimeout(w.ctx, onChainDataFetchTimeout)
		receipt, tx, err = w.node.transactionAndReceipt(innerCtx, pendingTx.txHash)
		cancel()
	}
	if w.log.Level() == util.LevelTrace {
		w.log.Tracef("Attempted to fetch tx and receipt for %s. receipt = %+v, tx = %+v, err = %+v", pendingTx.txHash, receipt, tx, err)
	}
	// A node that reports neither a receipt nor an error has told us nothing
	// about this tx, which is what not-found means. Say so rather than trusting
	// the nil: everything below the error check dereferences the receipt, so a
	// node that ever returns this pair - a bug, but node is an interface with
	// implementations we don't control - would take down the wallet from a
	// background tip-change goroutine, where there is no caller to recover.
	if err == nil && receipt == nil {
		w.log.Warnf("Node reported no receipt and no error for tx %s; treating as not found", pendingTx.txHash)
		err = asset.CoinNotFoundError
	}
	if err != nil {
		if errors.Is(err, asset.CoinNotFoundError) {
			pendingTx.indexed = tx != nil
			// transactionAndReceipt will return a CoinNotFound for either no
			// reciept or no tx. If they report the tx, we'll consider the tx
			// to be "indexed", and won't send lost tx action-required requests.
			if pendingTx.BlockNumber > 0 {
				w.log.Warnf("Transaction %s was previously mined but now cannot be confirmed. Might be normal.", pendingTx.txHash)
				pendingTx.BlockNumber = 0
				pendingTx.Timestamp = 0
				updated = true
			}
		} else {
			w.log.Errorf("Error getting confirmations for pending tx %s: %v", pendingTx.txHash, err)
			// If the receipt is persistently unretrievable (not just
			// missing but unparseable), mark as lost after age-out so
			// it stops contributing to the pending balance.
			if pendingTx.age() >= txAgeOut && pendingTx.Receipt == nil {
				w.log.Warnf("Marking persistently unretrievable tx %s as lost after age-out.", pendingTx.txHash)
				pendingTx.AssumedLost = true
				updated = true
			}
		}
		return
	}

	pendingTx.Receipt = receipt
	pendingTx.indexed = true
	pendingTx.Rejected = receipt.Status != types.ReceiptStatusSuccessful
	updated = true

	if receipt.BlockNumber == nil || receipt.BlockNumber.Cmp(new(big.Int)) == 0 {
		if pendingTx.BlockNumber > 0 {
			w.log.Warnf("Transaction %s was previously mined but is now unconfirmed", pendingTx.txHash)
			pendingTx.Timestamp = 0
			pendingTx.BlockNumber = 0
		}
		return
	}
	effectiveGasPrice := receipt.EffectiveGasPrice
	// NOTE: Would be nice if the receipt contained the block time so we could
	// set the timestamp without having to fetch the header. We could use
	// SubmissionTime, I guess. Less accurate, but probably not by much.
	// NOTE: I don't really know why effectiveGasPrice would be nil, but the
	// code I'm replacing got it from the header, so I'm gonna add this check
	// just in case.
	if pendingTx.Timestamp == 0 || effectiveGasPrice == nil {
		var hdr *types.Header
		if fetched != nil && fetched.hdr != nil {
			hdr = fetched.hdr
		} else if fetched != nil {
			// Header was pre-fetched outside the lock but failed; skip this
			// update cycle to avoid a blocking RPC under nonceMtx. The next
			// tip will trigger a retry. Clear updated so the defer does not
			// spam DB writes with incomplete state on every tip.
			updated = false
			w.log.Warnf("Skipping header-dependent update for %s: header not available, will retry on next tip", pendingTx.txHash)
			return
		} else {
			// No pre-fetch (tx added after phase 1, e.g. by concurrent Send);
			// fall back to inline fetch.
			hdrCtx, hdrCancel := context.WithTimeout(w.ctx, onChainDataFetchTimeout)
			hdr, err = w.node.headerByHash(hdrCtx, receipt.BlockHash)
			hdrCancel()
			if err != nil {
				w.log.Errorf("Error getting header for hash %v: %v", receipt.BlockHash, err)
				return
			}
			if hdr == nil {
				w.log.Errorf("Header for hash %v is nil", receipt.BlockHash)
				return
			}
		}
		gasTip, err := tx.EffectiveGasTip(hdr.BaseFee)
		if err != nil {
			w.log.Errorf("Error getting effective gas tip: %v", err)
			return
		}
		pendingTx.Timestamp = hdr.Time
		if effectiveGasPrice == nil {
			effectiveGasPrice = new(big.Int).Add(hdr.BaseFee, gasTip)
		}
	}

	bigFees := new(big.Int).Mul(effectiveGasPrice, big.NewInt(int64(receipt.GasUsed)))
	if w.isOpStack {
		l1Fee, err := w.node.l1FeeFromReceipt(w.ctx, pendingTx.txHash)
		if err != nil {
			w.log.Warnf("Error getting L1 fee for %s: %v", pendingTx.txHash, err)
		} else if l1Fee != nil && l1Fee.Sign() > 0 {
			bigFees.Add(bigFees, l1Fee)
		}
	}
	pendingTx.Fees = WeiToGweiCeil(bigFees)
	pendingTx.BlockNumber = receipt.BlockNumber.Uint64()
	if confs := safeConfs(tip, pendingTx.BlockNumber); confs >= w.finalizeConfs {
		updated = !pendingTx.Confirmed
		pendingTx.Confirmed = true
		pendingTx.Confirms = nil
	} else {
		pendingTx.Confirmed = false
		var oldConfs uint32
		if pendingTx.Confirms != nil {
			oldConfs = pendingTx.Confirms.Current
		}
		updated = confs != uint64(oldConfs)
		pendingTx.Confirms = &txdb.Confirms{
			Current: uint32(confs),
			Target:  uint32(w.finalizeConfs),
		}
	}
}

// checkPendingTxs checks the confirmation status of all pending transactions.
// It runs in three phases to keep nonceMtx free during RPC calls:
//
//  1. RLock  — identify which txs need a receipt fetch
//  2. no lock — call transactionAndReceipt (and headerByHash when needed)
//  3. WLock  — apply results and run the existing cleanup logic
func (w *baseWallet) checkPendingTxs() {
	tip := w.tipHeight()

	// Phase 1: identify txs that need a receipt fetch (RLock, read-only).
	type pendingFetch struct {
		pendingTx   *extendedWalletTx
		needsHeader bool // true when Timestamp==0, so we'll also need headerByHash
	}
	w.nonceMtx.RLock()
	var toFetch []pendingFetch
	for _, pendingTx := range w.pendingTxs {
		if pendingTx.Confirmed && pendingTx.savedToDB {
			continue
		}
		confs := safeConfs(tip, pendingTx.BlockNumber)
		if pendingTx.BlockNumber > 0 && confs < w.finalizeConfs {
			continue // waiting for confs only, no fetch needed
		}
		if pendingTx.lastCheck >= tip {
			continue
		}
		toFetch = append(toFetch, pendingFetch{pendingTx, pendingTx.Timestamp == 0})
	}
	w.nonceMtx.RUnlock()

	// Phase 2: fetch receipts (and headers when needed) outside any lock.
	preFetched := make(map[*extendedWalletTx]*txFetchResult, len(toFetch))
	for _, f := range toFetch {
		if w.ctx.Err() != nil {
			break
		}
		res := &txFetchResult{}
		innerCtx, cancel := context.WithTimeout(w.ctx, onChainDataFetchTimeout)
		res.receipt, res.tx, res.err = w.node.transactionAndReceipt(innerCtx, f.pendingTx.txHash)
		cancel()
		if res.err == nil && res.receipt != nil &&
			res.receipt.BlockNumber != nil && res.receipt.BlockNumber.Sign() > 0 &&
			(f.needsHeader || res.receipt.EffectiveGasPrice == nil) {
			hdrCtx, hdrCancel := context.WithTimeout(w.ctx, onChainDataFetchTimeout)
			var hdrErr error
			res.hdr, hdrErr = w.node.headerByHash(hdrCtx, res.receipt.BlockHash)
			hdrCancel()
			if hdrErr != nil {
				w.log.Debugf("Pre-fetch of header for block %v failed: %v", res.receipt.BlockHash, hdrErr)
			}
		}
		preFetched[f.pendingTx] = res
	}

	// Phase 3: apply all updates and run cleanup under write lock.
	w.nonceMtx.Lock()
	defer w.nonceMtx.Unlock()

	// If we have pending txs, trace log the before and after.
	if w.log.Level() == util.LevelTrace {
		if nPending := len(w.pendingTxs); nPending > 0 {
			defer func() {
				w.log.Tracef("Checked %d pending txs. Finalized %d", nPending, nPending-len(w.pendingTxs))
			}()
		}
	}

	// keepFromIndex will be the index of the first un-finalized tx.
	// lastConfirmed, will be the index of the last confirmed tx. All txs with
	// nonces lower that lastConfirmed should also be confirmed, or else
	// something isn't right and we may need to request user input.
	var keepFromIndex int
	var lastConfirmed int = -1
	for i, pendingTx := range w.pendingTxs {
		if w.ctx.Err() != nil {
			return
		}
		w.updatePendingTx(tip, pendingTx, preFetched[pendingTx])

		if pendingTx.Confirmed {
			lastConfirmed = i
			if pendingTx.Nonce.Cmp(w.confirmedNonceAt) == 0 {
				w.confirmedNonceAt.Add(w.confirmedNonceAt, big.NewInt(1))
			}
			if pendingTx.savedToDB {
				if i == keepFromIndex {
					// This is what we expect. No tx should be confirmed before a
					// tx with a lower nonce. We'll delete at least up to this
					// one.
					keepFromIndex = i + 1
				}
			}
			// This transaction is finalized. If we had previously sought action
			// on it, cancel that request.
			if pendingTx.actionRequested {
				pendingTx.actionRequested = false
				w.resolveAction(pendingTx.ID, pendingTx.TokenID)
			}
		}
	}

	// If we have missing nonces, send an alert.
	if !w.recoveryRequestSent && len(findMissingNonces(w.confirmedNonceAt, w.pendingNonceAt, w.pendingTxs)) != 0 {
		w.recoveryRequestSent = true
		w.requestAction(actionTypeMissingNonces, w.missingNoncesActionID(), nil, nil)
	}

	// Loop again, classifying problems and sending action requests.
	for i, pendingTx := range w.pendingTxs {
		if pendingTx.Confirmed || pendingTx.BlockNumber > 0 {
			continue
		}
		if time.Since(pendingTx.actionIgnored) < txAgeOut {
			// They asked us to keep waiting.
			continue
		}
		age := pendingTx.age()
		// i < lastConfirmed means unconfirmed nonce < a confirmed nonce.
		if (i < lastConfirmed) ||
			w.confirmedNonceAt.Cmp(pendingTx.Nonce) > 0 ||
			(age >= txAgeOut && pendingTx.Receipt == nil && !pendingTx.indexed) {

			// The tx in our records wasn't accepted. Where's the right one?
			req := newLostNonceNote(*pendingTx.WalletTransaction, pendingTx.Nonce.Uint64())
			pendingTx.actionRequested = true
			w.requestAction(actionTypeLostNonce, pendingTx.ID, req, pendingTx.TokenID)
			continue
		}
		// Recheck fees periodically.
		const feeCheckInterval = time.Minute * 5
		if time.Since(pendingTx.lastFeeCheck) < feeCheckInterval {
			continue
		}
		pendingTx.lastFeeCheck = time.Now()
		tx, err := pendingTx.tx()
		if err != nil {
			w.log.Errorf("Error decoding raw tx %s for fee check: %v", pendingTx.ID, err)
			continue
		}
		txCap := tx.GasFeeCap()
		baseRate, tipRate, err := w.currentNetworkFees(w.ctx)
		if err != nil {
			w.log.Errorf("Error getting base fee: %w", err)
			continue
		}
		if txCap.Cmp(baseRate) < 0 {
			maxFees := new(big.Int).Add(tipRate, new(big.Int).Mul(baseRate, big.NewInt(2)))
			maxFees.Mul(maxFees, new(big.Int).SetUint64(tx.Gas()))
			req := newLowFeeNote(*pendingTx.WalletTransaction, WeiToGweiCeil(maxFees))
			pendingTx.actionRequested = true
			w.requestAction(actionTypeTooCheap, pendingTx.ID, req, pendingTx.TokenID)
			continue
		}
		// Fees look good and there's no reason to believe this tx will
		// not be mined. Do we do anything?
		// actionRequired(actionTypeStuckTx, pendingTx)
	}

	// Delete finalized txs from local tracking.
	w.pendingTxs = w.pendingTxs[keepFromIndex:]

	// Re-broadcast any txs that are not indexed and haven't been re-broadcast
	// in a while, logging any errors as warnings.
	const rebroadcastPeriod = time.Minute * 5
	for _, pendingTx := range w.pendingTxs {
		if pendingTx.Confirmed || pendingTx.BlockNumber > 0 ||
			pendingTx.actionRequested || // Waiting on action
			pendingTx.indexed || // Provider knows about it
			time.Since(pendingTx.lastBroadcast) < rebroadcastPeriod {

			continue
		}
		pendingTx.lastBroadcast = time.Now()
		tx, err := pendingTx.tx()
		if err != nil {
			w.log.Errorf("Error decoding raw tx %s for rebroadcast: %v", pendingTx.ID, err)
			continue
		}
		if err := w.node.sendSignedTransaction(w.ctx, tx, allowAlreadyKnownFilter); err != nil {
			w.log.Warnf("Error rebroadcasting tx %s: %v", pendingTx.ID, err)
		} else {
			w.log.Infof("Rebroadcasted un-indexed transaction %s", pendingTx.ID)
		}
	}
}

// Required Actions: Extraordinary conditions that might require user input.

var _ asset.ActionTaker = (*assetWallet)(nil)

const (
	actionTypeMissingNonces = "missingNonces"
	actionTypeLostNonce     = "lostNonce"
	actionTypeTooCheap      = "tooCheap"
)

// TransactionActionNote is used to request user action on transactions in
// abnormal states.
type TransactionActionNote struct {
	Tx      *txdb.WalletTransaction `json:"tx"`
	Nonce   uint64                  `json:"nonce,omitempty"`
	NewFees uint64                  `json:"newFees,omitempty"`
}

// newLostNonceNote is information regarding a tx that appears to be lost.
func newLostNonceNote(tx txdb.WalletTransaction, nonce uint64) *TransactionActionNote {
	return &TransactionActionNote{
		Tx:    &tx,
		Nonce: nonce,
	}
}

// newLowFeeNote is data about a tx that is stuck in mempool with too-low fees.
func newLowFeeNote(tx txdb.WalletTransaction, newFees uint64) *TransactionActionNote {
	return &TransactionActionNote{
		Tx:      &tx,
		NewFees: newFees,
	}
}

// parse the pending tx and index from the slice.
func pendingTxWithID(txID string, pendingTxs []*extendedWalletTx) (int, *extendedWalletTx) {
	for i, pendingTx := range pendingTxs {
		if pendingTx.ID == txID {
			return i, pendingTx
		}
	}
	return 0, nil
}

// amendPendingTx is called with a function that intends to modify a pendingTx
// under mutex lock.
func (w *assetWallet) amendPendingTx(txID string, f func(common.Hash, *types.Transaction, *extendedWalletTx, int) error) error {
	txHash := common.HexToHash(txID)
	if txHash == (common.Hash{}) {
		return fmt.Errorf("invalid tx ID %q", txID)
	}
	w.nonceMtx.Lock()
	defer w.nonceMtx.Unlock()
	idx, pendingTx := pendingTxWithID(txID, w.pendingTxs)
	if pendingTx == nil {
		// Nothing to do anymore.
		return nil
	}
	tx, err := pendingTx.tx()
	if err != nil {
		return fmt.Errorf("error decoding transaction: %w", err)
	}
	if err := f(txHash, tx, pendingTx, idx); err != nil {
		return err
	}
	w.emit.ActionResolved(txID)
	pendingTx.actionRequested = false
	return nil
}

// userActionBumpFees is a request by a user to resolve a actionTypeTooCheap
// condition.
func (w *assetWallet) userActionBumpFees(actionB []byte) error {
	var action struct {
		TxID string `json:"txID"`
		Bump *bool  `json:"bump"`
	}
	if err := json.Unmarshal(actionB, &action); err != nil {
		return fmt.Errorf("error unmarshaling bump action: %v", err)
	}
	if action.Bump == nil {
		return errors.New("no bump value specified")
	}
	return w.amendPendingTx(action.TxID, func(txHash common.Hash, tx *types.Transaction, pendingTx *extendedWalletTx, idx int) error {
		if !*action.Bump {
			pendingTx.actionIgnored = time.Now()
			return nil
		}

		nonce := new(big.Int).SetUint64(tx.Nonce())
		maxFeeRate, tipCap, err := w.recommendedMaxFeeRate(w.ctx)
		if err != nil {
			return fmt.Errorf("error getting new fee rate: %w", err)
		}
		txOpts, err := w.node.txOpts(w.ctx, 0 /* set below */, tx.Gas(), maxFeeRate, tipCap, nonce)
		if err != nil {
			return fmt.Errorf("error preparing tx opts: %w", err)
		}
		txOpts.Value = tx.Value()
		addr := tx.To()
		if addr == nil {
			return errors.New("pending tx has no recipient?")
		}

		newTx, err := w.node.sendTransaction(w.ctx, txOpts, addr, tx.Data(), allowAlreadyKnownFilter)
		if err != nil {
			return fmt.Errorf("error sending bumped-fee transaction: %w", err)
		}

		var bridgeTx *genBridgeTxResult
		if pendingTx.BridgeCounterpartTx != nil {
			bridgeTx = &genBridgeTxResult{
				counterpartAssetID:         pendingTx.BridgeCounterpartTx.AssetID,
				counterpartTxID:            pendingTx.BridgeCounterpartTx.IDs[0],
				requiresFollowUp:           pendingTx.RequiresFollowUp,
				previousBridgeCompletionID: pendingTx.PreviousBridgeCompletionID,
				followUpData:               pendingTx.BridgeFollowUpData,
				bridgeName:                 pendingTx.BridgeName,
			}
		}

		res := &genTxResult{
			tx:        newTx,
			txType:    pendingTx.Type,
			amt:       pendingTx.Amount,
			recipient: pendingTx.Recipient,
			bridge:    bridgeTx,
		}
		newPendingTx := w.extendedTx(res)

		pendingTx.NonceReplacement = newPendingTx.ID
		pendingTx.FeeReplacement = true

		w.tryStoreDBTx(pendingTx)
		w.tryStoreDBTx(newPendingTx)

		w.pendingTxs[idx] = newPendingTx
		return nil
	})
}

// tryStoreDBTx attempts to store the DB tx and logs errors internally. This
// method sets the savedToDB flag, so if this tx is in pendingTxs, the nonceMtx
// must be held for reading.
func (w *baseWallet) tryStoreDBTx(wt *extendedWalletTx) {
	err := w.txDB.storeTx(wt)
	if err != nil {
		w.log.Errorf("Error storing tx %s to DB: %v", wt.txHash, err)
	}
	wt.savedToDB = err == nil
}

// userActionNonceReplacement is a request by a user to resolve a
// actionTypeLostNonce condition.
func (w *assetWallet) userActionNonceReplacement(actionB []byte) error {
	var action struct {
		TxID          string `json:"txID"`
		Abandon       *bool  `json:"abandon"`
		ReplacementID string `json:"replacementID"`
	}
	if err := json.Unmarshal(actionB, &action); err != nil {
		return fmt.Errorf("error unmarshaling user action: %v", err)
	}
	if action.Abandon == nil {
		return fmt.Errorf("no abandon value provided for user action for tx %s", action.TxID)
	}
	abandon := *action.Abandon
	if !abandon && action.ReplacementID == "" { // keep waiting
		return w.amendPendingTx(action.TxID, func(_ common.Hash, _ *types.Transaction, pendingTx *extendedWalletTx, idx int) error {
			pendingTx.actionIgnored = time.Now()
			return nil
		})
	}
	if abandon { // abandon
		return w.amendPendingTx(action.TxID, func(txHash common.Hash, _ *types.Transaction, wt *extendedWalletTx, idx int) error {
			w.log.Infof("Abandoning transaction %s via user action", txHash)
			wt.AssumedLost = true
			w.tryStoreDBTx(wt)
			pendingTx := w.pendingTxs[idx]
			if pendingTx.Nonce.Cmp(w.confirmedNonceAt) == 0 {
				w.pendingNonceAt.Add(w.pendingNonceAt, big.NewInt(-1))
			}
			copy(w.pendingTxs[idx:], w.pendingTxs[idx+1:])
			w.pendingTxs = w.pendingTxs[:len(w.pendingTxs)-1]
			return nil
		})
	}

	replacementHash := common.HexToHash(action.ReplacementID)
	replacementTx, _, err := w.node.getTransaction(w.ctx, replacementHash)
	if err != nil {
		return fmt.Errorf("error fetching nonce replacement tx: %v", err)
	}

	from, err := types.LatestSigner(w.node.chainConfig()).Sender(replacementTx)
	if err != nil {
		return fmt.Errorf("error parsing originator address from specified replacement tx %s: %w", from, err)
	}
	if from != w.addr {
		return fmt.Errorf("specified replacement tx originator %s is not you", from)
	}

	return w.amendPendingTx(action.TxID, func(txHash common.Hash, oldTx *types.Transaction, pendingTx *extendedWalletTx, idx int) error {
		if replacementTx.Nonce() != pendingTx.Nonce.Uint64() {
			return fmt.Errorf("nonce replacement doesn't have the right nonce. %d != %s", replacementTx.Nonce(), pendingTx.Nonce)
		}
		recipient := w.addr.Hex()
		newPendingTx := w.extendedTx(&genTxResult{
			tx:        replacementTx,
			txType:    txdb.Unknown,
			amt:       0,
			recipient: &recipient,
		})
		pendingTx.NonceReplacement = newPendingTx.ID
		var oldTo, newTo common.Address
		if oldAddr := oldTx.To(); oldAddr != nil {
			oldTo = *oldAddr
		}
		if newAddr := replacementTx.To(); newAddr != nil {
			newTo = *newAddr
		}
		if bytes.Equal(oldTx.Data(), replacementTx.Data()) && oldTo == newTo {
			pendingTx.FeeReplacement = true
		}
		w.tryStoreDBTx(pendingTx)
		w.tryStoreDBTx(newPendingTx)
		w.pendingTxs[idx] = newPendingTx
		return nil
	})
}

// userActionRecoverNonces, if recover is true, examines our confirmed and
// pending nonces and our pendingTx set and sends zero-value txs to ourselves
// to fill any gaps or replace any rogue transactions. This should never happen
// if we're only running one instance of this wallet.
func (w *assetWallet) userActionRecoverNonces(actionB []byte) error {
	var action struct {
		Recover *bool `json:"recover"`
	}
	if err := json.Unmarshal(actionB, &action); err != nil {
		return fmt.Errorf("error unmarshaling user action: %v", err)
	}
	if action.Recover == nil {
		return errors.New("no recover value specified")
	}
	if !*action.Recover {
		// Don't reset recoveryRequestSent. They won't see this message again until
		// they reboot.
		w.emit.ActionResolved(w.missingNoncesActionID())
		return nil
	}
	maxFeeRate, tipRate, err := w.recommendedMaxFeeRate(w.ctx)
	if err != nil {
		return fmt.Errorf("error getting max fee rate for nonce resolution: %v", err)
	}
	w.nonceMtx.Lock()
	defer w.nonceMtx.Unlock()
	missingNonces := findMissingNonces(w.confirmedNonceAt, w.pendingNonceAt, w.pendingTxs)
	if len(missingNonces) == 0 {
		w.emit.ActionResolved(w.missingNoncesActionID())
		return nil
	}
	for i, n := range missingNonces {
		nonce := new(big.Int).SetUint64(n)
		txOpts, err := w.node.txOpts(w.ctx, 0, defaultSendGasLimit, maxFeeRate, tipRate, nonce)
		if err != nil {
			return fmt.Errorf("error getting tx opts for nonce resolution: %v", err)
		}
		var skip bool
		tx, err := w.node.sendTransaction(w.ctx, txOpts, &w.addr, nil, func(err error) (discard, propagate, fail bool) {
			if errorFilter(err, "replacement transaction underpriced") {
				skip = true
				return true, false, false
			}
			if errorFilter(err, "already known", "known transaction") {
				return true, false, false // already in mempool = success
			}
			return false, false, true
		})
		if err != nil {
			return fmt.Errorf("error sending tx %d for nonce resolution: %v", nonce, err)
		}
		if skip {
			w.log.Warnf("skipping storing underpriced replacement tx for nonce %d", nonce)
		} else {
			recipient := w.addr.Hex()
			pendingTx := w.extendAndStoreTx(&genTxResult{
				tx:        tx,
				txType:    txdb.SelfSend,
				amt:       0,
				recipient: &recipient,
			}, nil)
			w.emitTransactionNote(pendingTx.WalletTransaction, true)
			w.pendingTxs = append(w.pendingTxs, pendingTx)
			sort.Slice(w.pendingTxs, func(i, j int) bool {
				return w.pendingTxs[i].Nonce.Cmp(w.pendingTxs[j].Nonce) < 0
			})
		}
		if i < len(missingNonces)-1 {
			select {
			case <-time.After(time.Second * 1):
			case <-w.ctx.Done():
				return nil
			}
		}
	}
	w.emit.ActionResolved(w.missingNoncesActionID())
	return nil
}

// requestAction sends a ActionRequired notification up the chain of command.
// nonceMtx must be locked.
func (w *baseWallet) requestAction(actionID, uniqueID string, req *TransactionActionNote, tokenID *uint32) {
	assetID := w.baseChainID
	if tokenID != nil {
		assetID = *tokenID
	}
	aw := w.wallet(assetID)
	if aw == nil { // sanity
		return
	}
	aw.emit.ActionRequired(uniqueID, actionID, req)
}

// resolveAction sends a ActionResolved notification up the chain of command.
// nonceMtx must be locked.
func (w *baseWallet) resolveAction(uniqueID string, tokenID *uint32) {
	assetID := w.baseChainID
	if tokenID != nil {
		assetID = *tokenID
	}
	aw := w.wallet(assetID)
	if aw == nil { // sanity
		return
	}
	aw.emit.ActionResolved(uniqueID)
}

// TakeAction satisfies asset.ActionTaker. This handles responses from the
// user for an ActionRequired request, usually for a stuck tx or otherwise
// abnormal condition.
func (w *assetWallet) TakeAction(actionID string, actionB []byte) error {
	switch actionID {
	case actionTypeTooCheap:
		return w.userActionBumpFees(actionB)
	case actionTypeMissingNonces:
		return w.userActionRecoverNonces(actionB)
	case actionTypeLostNonce:
		return w.userActionNonceReplacement(actionB)
	default:
		return fmt.Errorf("unknown action %q", actionID)
	}
}

const txHistoryNonceKey = "Nonce"

// transactionFeeLimit calculates the maximum tx fees that are allowed by a tx.
func transactionFeeLimit(tx *types.Transaction) *big.Int {
	fees := new(big.Int)
	feeCap, tipCap := tx.GasFeeCap(), tx.GasTipCap()
	if feeCap != nil && tipCap != nil {
		fees.Add(fees, feeCap)
		fees.Add(fees, tipCap)
		fees.Mul(fees, new(big.Int).SetUint64(tx.Gas()))
	}
	return fees
}

// extendedTx generates an *extendedWalletTx for a newly-broadcasted tx and
// stores it in the DB.
func (w *assetWallet) extendedTx(genTxResult *genTxResult) *extendedWalletTx {
	var tokenAssetID *uint32
	if w.assetID != w.baseChainID {
		tokenAssetID = &w.assetID
	}
	return w.baseWallet.extendAndStoreTx(genTxResult, tokenAssetID)
}

func (w *baseWallet) extendAndStoreTx(genTxResult *genTxResult, tokenAssetID *uint32) *extendedWalletTx {
	nonce := genTxResult.tx.Nonce()
	rawTx, err := genTxResult.tx.MarshalBinary()
	if err != nil {
		w.log.Errorf("Error marshaling tx %q: %v", genTxResult.tx.Hash(), err)
	}

	if genTxResult.recipient == nil {
		if to := genTxResult.tx.To(); to != nil {
			s := to.String()
			genTxResult.recipient = &s
		}
	}

	now := time.Now()

	fees := WeiToGweiCeil(transactionFeeLimit(genTxResult.tx))
	if w.isOpStack {
		if l1, err := w.node.l1FeeForCalldata(w.ctx, genTxResult.tx.Data()); err != nil {
			w.log.Warnf("Error estimating L1 fee for %s: %v", genTxResult.tx.Hash(), err)
		} else if l1 != nil && l1.Sign() > 0 {
			fees += WeiToGweiCeil(l1)
		}
	}

	wt := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			Type:      genTxResult.txType,
			ID:        genTxResult.tx.Hash().String(),
			Amount:    genTxResult.amt,
			Fees:      fees, // updated later with actual receipt fees
			TokenID:   tokenAssetID,
			Recipient: genTxResult.recipient,
			AdditionalData: map[string]string{
				txHistoryNonceKey: strconv.FormatUint(nonce, 10),
			},
		},
		SubmissionTime: uint64(now.Unix()),
		CallData:       genTxResult.tx.Data(),
		RawTx:          rawTx,
		Nonce:          big.NewInt(int64(nonce)),
		txHash:         genTxResult.tx.Hash(),
		savedToDB:      true,
		lastBroadcast:  now,
		lastFeeCheck:   now,
	}

	if genTxResult.bridge != nil {
		wt.BridgeCounterpartTx = &txdb.BridgeCounterpartTx{
			AssetID: genTxResult.bridge.counterpartAssetID,
		}
		if genTxResult.bridge.counterpartTxID != "" {
			wt.BridgeCounterpartTx.IDs = []string{genTxResult.bridge.counterpartTxID}
		}
		wt.BridgeFollowUpData = genTxResult.bridge.followUpData
		wt.PreviousBridgeCompletionID = genTxResult.bridge.previousBridgeCompletionID
		wt.BridgeName = genTxResult.bridge.bridgeName
	}

	w.tryStoreDBTx(wt)

	return wt
}

// TxHistory returns all the transactions the wallet has made. This
// includes the ETH wallet and all token wallets. If refID is nil, then
// transactions starting from the most recent are returned (past is ignored).
// If past is true, the transactions prior to the refID are returned, otherwise
// the transactions after the refID are returned. n is the number of
// transactions to return. If n is <= 0, all the transactions will be returned.
func (w *ETHWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	return w.txDB.getTxs(nil, req)
}

// TxHistory returns all the transactions the token wallet has made. If refID
// is nil, then transactions starting from the most recent are returned (past
// is ignored). If past is true, the transactions prior to the refID are
// returned, otherwise the transactions after the refID are returned. n is the
// number of transactions to return. If n is <= 0, all the transactions will be
// returned.
func (w *TokenWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	return w.txDB.getTxs(&w.assetID, req)
}

func (w *ETHWallet) getReceivingTransaction(ctx context.Context, txHash common.Hash) (*txdb.WalletTransaction, error) {
	tx, blockHeight, err := w.node.getTransaction(ctx, txHash)
	if err != nil {
		return nil, err
	}
	// Same guard as the TokenWallet twin: no transaction and no error is
	// not-found. To() is separately nil for a contract creation, which is a
	// real on-chain shape rather than a node bug, and never this wallet's
	// receive anyway.
	if tx == nil || tx.To() == nil {
		return nil, asset.CoinNotFoundError
	}

	if *tx.To() != w.addr {
		return nil, asset.CoinNotFoundError
	}

	addr := w.addr.String()
	return &txdb.WalletTransaction{
		Type:      txdb.Receive,
		ID:        tx.Hash().String(),
		Amount:    w.atomize(tx.Value()),
		Recipient: &addr,
		AdditionalData: map[string]string{
			txHistoryNonceKey: strconv.FormatUint(tx.Nonce(), 10),
		},
		// For receiving transactions, if the transaction is mined, it is
		// confirmed confirmed, because the value received will be part of
		// the available balance.
		Confirmed: blockHeight > 0,
	}, nil
}

// WalletTransaction returns a transaction that either the wallet has made or
// one in which the wallet has received funds.
func (w *ETHWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	txHash := common.HexToHash(txID)
	var localTx txdb.WalletTransaction
	if w.withLocalTxRead(txHash, func(wt *extendedWalletTx) {
		localTx = *wt.WalletTransaction
	}) {
		// Do not modify tx in pending tx map. Only modify copy.
		// Confirmed should be true, because the value is part of the available
		// balance.
		if localTx.BlockNumber > 0 {
			localTx.Confirmed = true
		}
		return &localTx, nil
	}
	return w.getReceivingTransaction(ctx, txHash)
}

// extractValueFromTransferLog checks the Transfer event logs in the
// transaction, finds the log that sends tokens to the wallet's address,
// and returns the value of the transfer.
func (w *TokenWallet) extractValueFromTransferLog(receipt *types.Receipt) (uint64, error) {
	tc, err := w.tokenContractorOrErr()
	if err != nil {
		return 0, err
	}
	return tc.parseTransfer(receipt)
}

func (w *TokenWallet) getReceivingTransaction(ctx context.Context, txHash common.Hash) (*txdb.WalletTransaction, error) {
	receipt, _, err := w.node.transactionAndReceipt(ctx, txHash)
	if err != nil {
		return nil, err
	}
	// Same guard as updatePendingTx: no receipt and no error is not-found. The
	// block number is separately optional - transactionReceipt checks it for
	// nil before using it - and this is the receiving tx of a swap, so the
	// caller can't be handed a zero height it would read as unmined either.
	if receipt == nil || receipt.BlockNumber == nil {
		return nil, asset.CoinNotFoundError
	}

	blockHeight := receipt.BlockNumber.Int64()
	value, err := w.extractValueFromTransferLog(receipt)
	if err != nil {
		w.log.Errorf("Error extracting value from transfer log: %v", err)
		return &txdb.WalletTransaction{
			Type:        txdb.Unknown,
			ID:          txHash.String(),
			BlockNumber: uint64(blockHeight),
			Confirmed:   blockHeight > 0,
		}, nil
	}

	addr := w.addr.String()
	return &txdb.WalletTransaction{
		Type:        txdb.Receive,
		ID:          txHash.String(),
		BlockNumber: uint64(blockHeight),
		TokenID:     &w.assetID,
		Amount:      value,
		Recipient:   &addr,
		// For receiving transactions, if the transaction is mined, it is
		// confirmed confirmed, because the value received will be part of
		// the available balance.
		Confirmed: blockHeight > 0,
	}, nil
}

func (w *TokenWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	txHash := common.HexToHash(txID)
	var localTx txdb.WalletTransaction
	if w.withLocalTxRead(txHash, func(wt *extendedWalletTx) {
		localTx = *wt.WalletTransaction
	}) {
		// Do not modify tx in pending tx map. Only modify copy.
		// Confirmed should be true, because the value is part of the available
		// balance.
		if localTx.BlockNumber > 0 {
			localTx.Confirmed = true
		}
		return &localTx, nil
	}
	return w.getReceivingTransaction(ctx, txHash)
}

// PendingTransactions loads wallet transactions that are not yet confirmed.
func (w *assetWallet) PendingTransactions(ctx context.Context) []*txdb.WalletTransaction {
	w.nonceMtx.RLock()
	defer w.nonceMtx.RUnlock()
	txs := make([]*txdb.WalletTransaction, 0)
	for _, tx := range w.pendingTxs {
		txAssetID := w.baseChainID
		if tx.TokenID != nil {
			txAssetID = *tx.TokenID
		}
		if w.assetID != txAssetID {
			continue
		}
		txCopy := *tx.WalletTransaction
		txs = append(txs, &txCopy)
	}
	return txs
}

// providersFile reads a file located at ~/dextest/credentials.json.
// The file contains seed and provider information for wallets used for
// getgas, deploy, and nodeclient testing. If simnet providers are not
// specified, getFileCredentials will add the simnet alpha node.
type providersFile struct {
	Seed      util.Bytes                                                  `json:"seed"`
	Providers map[string] /* symbol */ map[string] /* network */ []string `json:"providers"`
	Relays    map[string] /* symbol */ map[string] /* network */ string   `json:"relays"`
}

// getFileCredentials reads the file at path and extracts the seed and the
// provider for the network.
func getFileCredentials(chain, path string, net util.Network) (seed []byte, providers []string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("error reading credentials file: %v", err)
	}
	var p providersFile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, nil, fmt.Errorf("error parsing credentials file: %v", err)
	}
	if len(p.Seed) == 0 {
		return nil, nil, fmt.Errorf("must provide both seeds in credentials file")
	}
	seed = p.Seed
	assetName := chain
	if assetName == "base" {
		assetName = "weth.base"
	}
	for _, uri := range p.Providers[assetName][net.String()] {
		if !strings.HasPrefix(uri, "#") && !strings.HasPrefix(uri, ";") {
			providers = append(providers, uri)
		}
	}
	if len(providers) == 0 {
		return nil, nil, fmt.Errorf("no providers in the file at %s for chain %s, network %s", path, chain, net)
	}
	if net == util.Simnet && len(providers) == 0 {
		switch chain {
		case "base":
			providers = []string{"http://127.0.0.1:39556"}
		case "polygon":
			providers = []string{"http://127.0.0.1:48296"}
		default:
			providers = []string{"http://127.0.0.1:38556"}
		}
	}
	return
}

// waitForConfirmation waits for the specified transaction to have > 0
// confirmations.
func waitForConfirmation(ctx context.Context, desc string, cl ethFetcher, txHash common.Hash, log util.Logger) error {
	bestHdr, err := cl.bestHeader(ctx)
	if err != nil {
		return fmt.Errorf("error getting best header: %w", err)
	}
	ticker := time.NewTicker(chainOpTick)
	lastReport := time.Now()
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			hdr, _ := cl.bestHeader(ctx)
			if hdr != nil && hdr.Number.Cmp(bestHdr.Number) > 0 {
				bestHdr = hdr
				confs, err := cl.transactionConfirmations(ctx, txHash)
				if err != nil {
					if !errors.Is(err, asset.CoinNotFoundError) {
						return fmt.Errorf("error getting transaction confirmations: %v", err)
					}
					continue
				}
				if confs > 0 {
					return nil
				}
				if time.Since(lastReport) > time.Second*30 {
					log.Infof("Awaiting confirmations for %s tx %s", desc, txHash)
					lastReport = time.Now()
				}
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type getGas byte

// GetGas provides access to the gas estimation utilities.
var GetGas getGas

// GetGasWalletParams are the configuration parameters required to estimate
// swap contract gas usage.
type GetGasWalletParams struct {
	ChainCfg     *params.ChainConfig
	Gas          *Gases
	Token        *Token
	UnitInfo     *util.UnitInfo
	BaseUnitInfo *util.UnitInfo
	Compat       *CompatibilityData
	ContractAddr common.Address // Base chain contract addr.
}

// ReadCredentials reads the credentials for the network from the credentials
// file.
func (getGas) ReadCredentials(chain, credentialsPath string, net util.Network) (addr string, providers []string, err error) {
	seed, providers, err := getFileCredentials(chain, credentialsPath, net)
	if err != nil {
		return "", nil, err
	}
	privB, zero, err := privKeyFromSeed(seed)
	if err != nil {
		return "", nil, err
	}
	defer zero()
	privateKey, err := crypto.ToECDSA(privB)
	if err != nil {
		return "", nil, err
	}

	addr = crypto.PubkeyToAddress(privateKey.PublicKey).String()
	return
}

func (getGas) returnFunds(
	ctx context.Context,
	cl *multiRPCClient,
	maxFeeRate *big.Int,
	tipRate *big.Int,
	returnAddr common.Address,
	token *Token, // nil for base chain
	ui *util.UnitInfo,
	log util.Logger,
	net util.Network,
) error {

	bigEthBal, err := cl.addressBalance(ctx, cl.address())
	if err != nil {
		return fmt.Errorf("error getting eth balance: %v", err)
	}
	ethBal := WeiToGwei(bigEthBal)

	if token != nil {
		tc, err := newERC20TokenContractor(net, token, cl.address(), cl.contractBackend())
		if err != nil {
			return fmt.Errorf("error constructing token contractor: %w", err)
		}

		bigTokenBal, err := tc.balance(ctx)
		if err != nil {
			return fmt.Errorf("error getting token balance: %w", err)
		}
		if bigTokenBal.Sign() == 0 {
			log.Infof("No %s balance to return", token.Name)
			return nil
		}

		est, err := tc.estimateTransferGas(ctx, returnAddr, bigTokenBal)
		if err != nil {
			return fmt.Errorf("error estimating transfer gas: %w", err)
		}
		gasLimit := bufferedGasLimit(est)
		fees := gasLimit * WeiToGweiCeil(maxFeeRate)
		if fees > ethBal {
			return fmt.Errorf("not enough base chain balance (%s) to cover fees (%s)",
				UnitInfo.ConventionalString(ethBal), UnitInfo.ConventionalString(fees))
		}

		txOpts, err := cl.txOpts(ctx, 0, gasLimit, maxFeeRate, tipRate, nil)
		if err != nil {
			return fmt.Errorf("error generating tx opts: %w", err)
		}

		tx, err := tc.transfer(txOpts, returnAddr, bigTokenBal)
		if err != nil {
			return fmt.Errorf("error transferring tokens : %w", err)
		}
		log.Infof("Sent %s in transaction %s", ui.ConventionalString(token.EVMToAtomic(bigTokenBal)), tx.Hash())
		return nil
	}

	bigFees := new(big.Int).Mul(new(big.Int).SetUint64(defaultSendGasLimit), maxFeeRate)

	fees := WeiToGweiCeil(bigFees)

	ethFmt := ui.ConventionalString
	if fees >= ethBal {
		return fmt.Errorf("balance is lower than projected fees: %s < %s", ethFmt(ethBal), ethFmt(fees))
	}

	remainder := ethBal - fees

	txOpts, err := cl.txOpts(ctx, remainder, defaultSendGasLimit, maxFeeRate, tipRate, nil)
	if err != nil {
		return fmt.Errorf("error generating tx opts: %w", err)
	}
	tx, err := cl.sendTransaction(ctx, txOpts, &returnAddr, nil, allowAlreadyKnownFilter)
	if err != nil {
		return fmt.Errorf("error sending funds: %w", err)
	}
	log.Infof("Sent %s in transaction %s", ui.ConventionalString(remainder), tx.Hash())
	return nil
}

// newTxOpts is a constructor for a TransactOpts.
func newTxOpts(ctx context.Context, from common.Address, val, maxGas uint64, maxFeeRate, gasTipCap *big.Int) *bind.TransactOpts {
	// We'll enforce MinGasTipCap since the server does, but this isn't
	// necessarily a constant for all networks or under all conditions.
	minGasWei := GweiToWei(MinGasTipCap)
	if gasTipCap.Cmp(minGasWei) < 0 {
		gasTipCap.Set(minGasWei)
	}
	// This is enforced by concensus. We shouldn't be able to get here with a
	// swap tx.
	if gasTipCap.Cmp(maxFeeRate) > 0 {
		gasTipCap.Set(maxFeeRate)
	}
	return &bind.TransactOpts{
		Context:   ctx,
		From:      from,
		Value:     GweiToWei(val),
		GasFeeCap: maxFeeRate,
		GasTipCap: gasTipCap,
		GasLimit:  maxGas,
	}
}
