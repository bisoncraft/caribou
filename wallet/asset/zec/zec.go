// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/wallet/txdb"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/config"
	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/rpcclient/v8"
)

const (
	BipID = 133
	// The default fee is passed to the user as part of the asset.WalletInfo
	// structure.
	defaultFee          = 10
	defaultFeeRateLimit = 1000
	minNetworkVersion   = 6110050 // v6.11.0
	walletTypeRPC       = "zcashdRPC"

	// defaultConfTarget is the amount of confirmations required to consider
	// a transaction is confirmed for tx history.
	defaultConfTarget = 1

	// transparentAcctNumber = 0
	shieldedAcctNumber = 0

	transparentAddressType = "p2pkh"
	orchardAddressType     = "orchard"
	saplingAddressType     = "sapling"
	unifiedAddressType     = "unified"

	minOrchardConfs = 1

	blockTicker     = time.Second
	peerCountTicker = 5 * time.Second

	// confTxFinality is the amount of confirms a redeem or refund
	// transaction needs before the trade is considered confirmed. The
	// tx is monitored until this number of confirms is reached.
	confTxFinality = 1

	depositAddrPrefix = "unified:"
)

var (
	configOpts = []*asset.ConfigOption{
		{
			Key:         "rpcuser",
			DisplayName: "JSON-RPC Username",
			Description: "Zcash's 'rpcuser' setting",
		},
		{
			Key:         "rpcpassword",
			DisplayName: "JSON-RPC Password",
			Description: "Zcash's 'rpcpassword' setting",
			NoEcho:      true,
		},
		{
			Key:         "rpcbind",
			DisplayName: "JSON-RPC Address",
			Description: "<addr> or <addr>:<port> (default 'localhost')",
		},
		{
			Key:         "rpcport",
			DisplayName: "JSON-RPC Port",
			Description: "Port for RPC connections (if not set in Address)",
		},
		{
			Key:          "fallbackfee",
			DisplayName:  "Fallback fee rate",
			Description:  "Zcash's 'fallbackfee' rate. Units: ZEC/kB",
			DefaultValue: strconv.FormatFloat(defaultFee*1000/1e8, 'f', -1, 64),
		},
		{
			Key:         "feeratelimit",
			DisplayName: "Highest acceptable fee rate",
			Description: "This is the highest network fee rate you are willing to " +
				"pay on swap transactions. If feeratelimit is lower than a market's " +
				"maxfeerate, you will not be able to trade on that market with this " +
				"wallet.  Units: BTC/kB",
			DefaultValue: strconv.FormatFloat(defaultFeeRateLimit*1000/1e8, 'f', -1, 64),
		},
		{
			Key:         "txsplit",
			DisplayName: "Pre-split funding inputs",
			Description: "When placing an order, create a \"split\" transaction to fund the order without locking more of the wallet balance than " +
				"necessary. Otherwise, excess funds may be reserved to fund the order until the first swap contract is broadcast " +
				"during match settlement, or the order is canceled. This an extra transaction for which network mining fees are paid. " +
				"Used only for standing-type orders, e.g. limit orders without immediate time-in-force.",
			IsBoolean: true,
		},
	}
	rpcWalletDefinition = &asset.WalletDefinition{
		Type:              walletTypeRPC,
		Tab:               "External",
		Description:       "Connect to zcashd",
		DefaultConfigPath: btc.SystemConfigPath("zcash"),
		ConfigOpts:        configOpts,
		NoAuth:            true,
	}
	// WalletInfo defines some general information about a Zcash wallet.
	WalletInfo = &asset.WalletInfo{
		Name:             "Zcash",
		UnitInfo:         UnitInfo,
		AvailableWallets: []*asset.WalletDefinition{rpcWalletDefinition},
		BlockchainClass:  asset.BlockchainClassUTXO,
		// ZEC fee markets run orders of magnitude below this; the ceiling
		// exists to catch units mixups, not to track the market.
		MaxFeeRate: 5_000,
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

// Open creates the ZEC exchange wallet. Start the wallet with its Run method.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	return NewWallet(cfg, logger, network)
}

// DecodeCoinID creates a human-readable representation of a coin ID for
// Zcash.
func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	// Zcash shielded transactions don't have transparent outputs, so the coinID
	// will just be the tx hash.
	if len(coinID) == chainhash.HashSize {
		var txHash chainhash.Hash
		copy(txHash[:], coinID)
		return txHash.String(), nil
	}
	// For transparent transactions, Zcash and Bitcoin have the same tx hash
	// and output format.
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
	return MinLotSize(maxFeeRate)
}

// WalletConfig are wallet-level configuration settings.
type WalletConfig struct {
	UseSplitTx       bool   `ini:"txsplit"`
	RedeemConfTarget uint64 `ini:"redeemconftarget"`
	ActivelyUsed     bool   `ini:"special_activelyUsed"` // injected by core
}

// rpcConnConfig builds the zcashd connection config, routing it through
// torProxy unless the node is on a loopback or private address.
func rpcConnConfig(cfg *btc.RPCConfig, torProxy string) *rpcclient.ConnConfig {
	connCfg := &rpcclient.ConnConfig{
		HTTPPostMode: true,
		DisableTLS:   true,
		Host:         cfg.RPCBind,
		User:         cfg.RPCUser,
		Pass:         cfg.RPCPass,
	}
	if torProxy = dexnet.ProxyFor(cfg.RPCBind, torProxy); torProxy != "" {
		// In HTTP POST mode the proxy is a URL, and its userinfo is sent as
		// SOCKS credentials, which isolates this connection's Tor circuit.
		user, pass := dexnet.IsolationCreds()
		connCfg.Proxy = (&url.URL{
			Scheme: "socks5",
			User:   url.UserPassword(user, pass),
			Host:   torProxy,
		}).String()
	}
	return connCfg
}

func newRPCConnection(cfg *btc.RPCConfig, torProxy string) (*rpcclient.Client, error) {
	return rpcclient.New(rpcConnConfig(cfg, torProxy), nil)
}

// NewWallet is the exported constructor by which the DEX will import the
// exchange wallet. The wallet will shut down when the provided context is
// canceled. The configPath can be an empty string, in which case the standard
// system location of the zcashd config file is assumed.
func NewWallet(cfg *asset.WalletConfig, logger util.Logger, net util.Network) (asset.Wallet, error) {
	var btcParams *chaincfg.Params
	var addrParams *AddressParams
	switch net {
	case util.Mainnet:
		btcParams = MainNetParams
		addrParams = MainNetAddressParams
	case util.Testnet:
		btcParams = TestNet4Params
		addrParams = TestNet4AddressParams
	case util.Regtest:
		btcParams = RegressionNetParams
		addrParams = RegressionNetAddressParams
	default:
		return nil, fmt.Errorf("unknown network ID %v", net)
	}

	// Designate the clone ports. These will be overwritten by any explicit
	// settings in the configuration file.
	ports := btc.NetPorts{
		Mainnet: "8232",
		Testnet: "18232",
		Simnet:  "18232",
	}

	var walletCfg WalletConfig
	err := config.Unmapify(cfg.Settings, &walletCfg)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("error creating data directory at %q: %w", cfg.DataDir, err)
	}

	ar, err := newAddressRecycler(filepath.Join(cfg.DataDir, "recycled-addrs.txt"), logger)
	if err != nil {
		return nil, fmt.Errorf("error creating address recycler: %w", err)
	}

	var rpcCfg btc.RPCConfig
	if err := config.Unmapify(cfg.Settings, &rpcCfg); err != nil {
		return nil, fmt.Errorf("error reading settings: %w", err)
	}

	if err := btc.CheckRPCConfig(&rpcCfg, "Zcash", net, ports); err != nil {
		return nil, fmt.Errorf("rpc config error: %v", err)
	}

	cl, err := newRPCConnection(&rpcCfg, cfg.TorProxy)
	if err != nil {
		return nil, fmt.Errorf("error constructing rpc client: %w", err)
	}

	zw := &zecWallet{
		peersChange: cfg.PeersChange,
		emit:        cfg.Emit,
		log:         logger,
		net:         net,
		btcParams:   btcParams,
		addrParams:  addrParams,
		decodeAddr: func(addr string, net *chaincfg.Params) (btcutil.Address, error) {
			return DecodeAddress(addr, addrParams, btcParams)
		},
		ar:         ar,
		node:       cl,
		walletDir:  cfg.DataDir,
		pendingTxs: make(map[chainhash.Hash]*btc.ExtendedWalletTx),
		netTxDB:    cfg.NetworkTxDB,
	}
	zw.walletCfg.Store(&walletCfg)
	zw.prepareCoinManager()
	zw.prepareRedemptionFinder()
	return zw, nil
}

type rpcCaller interface {
	CallRPC(method string, args []any, thing any) error
}

// zecWallet is an asset.Wallet for Zcash. zecWallet is a shielded-first wallet.
// This has a number of implications.
//  1. zecWallet will, by default, keep funds in the shielded pool.
//  2. If you send funds with z_sendmany, the change will go to the shielded pool.
//     This means that we cannot maintain a transparent pool at all, since this
//     behavior is unavoidable in the Zcash API, so sending funds to ourselves,
//     for instance, would probably result in sending more into shielded than
//     we wanted. This does not apply to fully transparent transactions such as
//     swaps and redemptions.
//  3. When funding is requested for an order, we will generally generate a
//     "split transaction", but one that moves funds from the shielded pool to
//     a transparent receiver. This will be default behavior for Zcash now, and
//     cannot be disabled via configuration.
//  4. ...
type zecWallet struct {
	ctx           context.Context
	log           util.Logger
	net           util.Network
	lastAddress   atomic.Pointer[string]
	btcParams     *chaincfg.Params
	addrParams    *AddressParams
	node          rawRequester
	ar            *addressRecycler
	rf            *redemptionFinder
	decodeAddr    btc.AddressDecoder
	lastPeerCount uint32
	peersChange   func(uint32, error)
	emit          *asset.WalletEmitter
	walletCfg     atomic.Value // *WalletConfig
	walletDir     string

	// Coins returned by Fund are cached for quick reference.
	cm *coinManager

	tipAtConnect atomic.Uint64

	tipMtx     sync.RWMutex
	currentTip *blockVector

	pendingTxsMtx sync.RWMutex
	pendingTxs    map[chainhash.Hash]*btc.ExtendedWalletTx

	receiveTxLastQuery atomic.Uint64

	txHistoryDB      atomic.Pointer[btc.BadgerTxDB]
	syncingTxHistory atomic.Bool
	// netTxDB is the master transaction database handle supplied by Core,
	// scoped to this wallet's network. It backs txHistoryDB.
	netTxDB txdb.NetworkTxDB
}

var _ asset.Wallet = (*zecWallet)(nil)
var _ asset.NewAddresser = (*zecWallet)(nil)

// TODO: Implement LiveReconfigurer
// var _ asset.LiveReconfigurer = (*zecWallet)(nil)

func (w *zecWallet) CallRPC(method string, args []any, thing any) error {
	return call(w.ctx, w.node, method, args, thing)
}

// FeeRate returns the asset standard fee rate for Zcash.
func (w *zecWallet) FeeRate() uint64 {
	return 5000 // per logical action
}

func (w *zecWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	w.ctx = ctx

	if err := w.connectRPC(ctx); err != nil {
		return nil, err
	}

	fullTBalance, err := getBalance(w)
	if err != nil {
		return nil, fmt.Errorf("error getting account-wide t-balance: %w", err)
	}

	const minConfs = 0
	acctBal, err := zGetBalanceForAccount(w, shieldedAcctNumber, minConfs)
	if err != nil {
		return nil, fmt.Errorf("error getting account balance: %w", err)
	}

	locked, err := w.lockedZats()
	if err != nil {
		return nil, fmt.Errorf("error getting locked zats: %w", err)
	}

	if acctBal.Transparent+locked != fullTBalance { // TODO(zec) fix failure here..
		return nil, errors.New(
			"there appears to be some transparent balance that is not in the zeroth account. " +
				"To operate correctly, all balance must be in the zeroth account. " +
				"Move all balance to the zeroth account to use the Zcash wallet",
		)
	}

	// Initialize the best block.
	bestBlockHdr, err := getBestBlockHeader(w)
	if err != nil {
		return nil, fmt.Errorf("error initializing best block: %w", err)
	}
	bestBlockHash, err := chainhash.NewHashFromStr(bestBlockHdr.Hash)
	if err != nil {
		return nil, fmt.Errorf("invalid best block hash from node: %v", err)
	}

	bestBlock := &blockVector{Height: bestBlockHdr.Height, Hash: *bestBlockHash}
	w.log.Infof("Connected wallet with current best block %v (%d)", bestBlock.Hash, bestBlock.Height)
	w.tipMtx.Lock()
	w.currentTip = bestBlock
	w.tipMtx.Unlock()
	w.tipAtConnect.Store(uint64(w.currentTip.Height))

	wg, err := w.startTxHistoryDB(ctx)
	if err != nil {
		return nil, err
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		w.ar.WriteRecycledAddrsToFile()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.watchBlocks(ctx)
		w.rf.CancelRedemptionSearches()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.monitorPeers(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.syncTxHistory(uint64(w.currentTip.Height))
	}()

	return wg, nil
}

func (w *zecWallet) monitorPeers(ctx context.Context) {
	ticker := time.NewTicker(peerCountTicker)
	defer ticker.Stop()
	for {
		w.checkPeers()

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (w *zecWallet) checkPeers() {
	numPeers, err := peerCount(w)
	if err != nil {
		prevPeer := atomic.SwapUint32(&w.lastPeerCount, 0)
		if prevPeer != 0 {
			w.log.Errorf("Failed to get peer count: %v", err)
			w.peersChange(0, err)
		}
		return
	}
	prevPeer := atomic.SwapUint32(&w.lastPeerCount, numPeers)
	if prevPeer != numPeers {
		w.peersChange(numPeers, nil)
	}
}

func (w *zecWallet) prepareCoinManager() {
	w.cm = newCoinManager(
		w.log,
		w.btcParams,
		func(val, lots, maxFeeRate uint64, reportChange bool) enoughFunc { // orderEnough
			return func(inputCount, inputsSize, sum uint64) (bool, uint64) {
				req := RequiredOrderFunds(val, inputCount, inputsSize, lots)
				if sum >= req {
					excess := sum - req
					if !reportChange || isDust(val, btc.P2SHOutputSize) {
						excess = 0
					}
					return true, excess
				}
				return false, 0
			}
		},
		func() ([]*listUnspentResult, error) { // list
			return listUnspent(w)
		},
		func(unlock bool, ops []*btc.Output) error { // lock
			return lockUnspent(w, unlock, ops)
		},
		func() ([]*rpcOutpoint, error) { // listLocked
			return listLockUnspent(w, w.log)
		},
		func(txHash *chainhash.Hash, vout uint32) (*wire.TxOut, error) {
			walletTx, err := getWalletTransaction(w, txHash)
			if err != nil {
				return nil, err
			}
			tx, err := DeserializeTx(walletTx.Bytes)
			if err != nil {
				w.log.Warnf("Invalid transaction %v (%x): %v", txHash, walletTx.Bytes, err)
				return nil, nil
			}
			if vout >= uint32(len(tx.TxOut)) {
				w.log.Warnf("Invalid vout %d for %v", vout, txHash)
				return nil, nil
			}
			return tx.TxOut[vout], nil
		},
		func(addr btcutil.Address) (string, error) {
			return EncodeAddress(addr, w.addrParams)
		},
	)
}

func (w *zecWallet) prepareRedemptionFinder() {
	w.rf = newRedemptionFinder(
		w.log,
		func(h *chainhash.Hash) (*getTransactionResult, error) {
			tx, err := getWalletTransaction(w, h)
			if err != nil {
				return nil, err
			}
			if tx.Confirmations < 0 {
				tx.Confirmations = 0
			}
			return &getTransactionResult{
				Confirmations: uint64(tx.Confirmations),
				BlockHash:     tx.BlockHash,
				BlockTime:     tx.BlockTime,
				TxID:          tx.TxID,
				Time:          tx.Time,
				TimeReceived:  tx.TimeReceived,
				Bytes:         tx.Bytes,
			}, nil
		},
		func(h *chainhash.Hash) (int32, error) {
			return getBlockHeight(w, h)
		},
		func(h chainhash.Hash) (*wire.MsgBlock, error) {
			blk, err := getBlock(w, h)
			if err != nil {
				return nil, err
			}
			return &blk.MsgBlock, nil
		},
		func(h *chainhash.Hash) (hdr *btc.BlockHeader, mainchain bool, err error) {
			return getVerboseBlockHeader(w, h)
		},
		hashTx,
		deserializeTx,
		func() (int32, error) {
			return getBestBlockHeight(w)
		},
		func(ctx context.Context, reqs map[btc.OutPoint]*findRedemptionReq, blockHash chainhash.Hash) (discovered map[btc.OutPoint]*findRedemptionResult) {
			blk, err := getBlock(w, blockHash)
			if err != nil {
				w.log.Errorf("RPC GetBlock error: %v", err)
				return
			}
			return searchBlockForRedemptions(ctx, reqs, &blk.MsgBlock, false, hashTx, w.btcParams)
		},
		func(h int64) (*chainhash.Hash, error) {
			return getBlockHash(w, h)
		},
		func(ctx context.Context, reqs map[btc.OutPoint]*findRedemptionReq) (discovered map[btc.OutPoint]*findRedemptionResult) {
			getRawMempool := func() ([]*chainhash.Hash, error) {
				return getRawMempool(w)
			}
			getMsgTx := func(txHash *chainhash.Hash) (*wire.MsgTx, error) {
				tx, err := getZecTransaction(w, txHash)
				if err != nil {
					return nil, err
				}
				return tx.MsgTx, nil
			}
			return findRedemptionsInMempool(ctx, w.log, reqs, getRawMempool, getMsgTx, false, hashTx, w.btcParams)
		},
	)
}

func (w *zecWallet) connectRPC(ctx context.Context) error {
	netVer, _, err := getVersion(w)
	if err != nil {
		return fmt.Errorf("error getting version: %w", err)
	}
	if netVer < minNetworkVersion {
		return fmt.Errorf("reported node version %d is less than minimum %d", netVer, minNetworkVersion)
	}
	chainInfo, err := getBlockchainInfo(w)
	if err != nil {
		return fmt.Errorf("getblockchaininfo error: %w", err)
	}
	if !chainOK(w.net, chainInfo.Chain) {
		return errors.New("wrong net")
	}
	// Make sure we have zeroth and first account or are able to create them.
	accts, err := zListAccounts(w)
	if err != nil {
		return fmt.Errorf("error listing Zcash accounts: %w", err)
	}

	createAccount := func(n uint32) error {
		for _, acct := range accts {
			if acct.Number == n {
				return nil
			}
		}
		acctNumber, err := zGetNewAccount(w)
		if err != nil {
			if strings.Contains(err.Error(), "zcashd-wallet-tool") {
				return fmt.Errorf("account %d does not exist and cannot be created because wallet seed backup has not been acknowledged with the zcashd-wallet-tool utility", n)
			}
			return fmt.Errorf("error creating account %d: %w", n, err)
		}
		if acctNumber != n {
			return fmt.Errorf("no account %d found and newly created account has unexpected account number %d", n, acctNumber)
		}
		return nil
	}
	if err := createAccount(shieldedAcctNumber); err != nil {
		return err
	}
	return nil
}

// watchBlocks pings for new blocks and runs the tipChange callback function
// when the block changes.
func (w *zecWallet) watchBlocks(ctx context.Context) {
	ticker := time.NewTicker(blockTicker)
	defer ticker.Stop()

	for {
		select {

		// Poll for the block. If the wallet offers tip reports, delay reporting
		// the tip to give the wallet a moment to request and scan block data.
		case <-ticker.C:
			newTipHdr, err := getBestBlockHeader(w)
			if err != nil {
				w.log.Errorf("failed to get best block header from node: %v", err)
				continue
			}
			newTipHash, err := chainhash.NewHashFromStr(newTipHdr.Hash)
			if err != nil {
				w.log.Errorf("invalid best block hash from node: %v", err)
				continue
			}

			w.tipMtx.RLock()
			sameTip := w.currentTip.Hash == *newTipHash
			w.tipMtx.RUnlock()
			if sameTip {
				continue
			}

			newTip := &blockVector{Height: newTipHdr.Height, Hash: *newTipHash}
			w.reportNewTip(ctx, newTip)

		case <-ctx.Done():
			return
		}

		// Ensure context cancellation takes priority before the next iteration.
		if ctx.Err() != nil {
			return
		}
	}
}

// reportNewTip sets the currentTip. The tipChange callback function is invoked
// and a goroutine is started to check if any contracts in the
// findRedemptionQueue are redeemed in the new blocks.
func (w *zecWallet) reportNewTip(ctx context.Context, newTip *blockVector) {
	w.tipMtx.Lock()
	defer w.tipMtx.Unlock()

	prevTip := w.currentTip
	w.currentTip = newTip
	w.log.Tracef("tip change: %d (%s) => %d (%s)", prevTip.Height, prevTip.Hash, newTip.Height, newTip.Hash)
	w.emit.TipChange(uint64(newTip.Height))

	w.rf.ReportNewTip(ctx, prevTip, newTip)

	w.syncTxHistory(uint64(newTip.Height))
}

func decodeCoinID(coinID util.Bytes) (*chainhash.Hash, uint32, error) {
	if len(coinID) != 36 {
		return nil, 0, fmt.Errorf("coin ID wrong length. expected 36, got %d", len(coinID))
	}
	var txHash chainhash.Hash
	copy(txHash[:], coinID[:32])
	return &txHash, binary.BigEndian.Uint32(coinID[32:]), nil
}

type balanceBreakdown struct {
	avail     uint64
	maturing  uint64
	noteCount uint32
}

type balances struct {
	orchard     *balanceBreakdown
	sapling     uint64
	transparent *balanceBreakdown
}

func (w *zecWallet) balances() (*balances, error) {
	zeroConf, err := zGetBalanceForAccount(w, shieldedAcctNumber, 0)
	if err != nil {
		return nil, fmt.Errorf("z_getbalanceforaccount (0) error: %w", err)
	}
	mature, err := zGetBalanceForAccount(w, shieldedAcctNumber, minOrchardConfs)
	if err != nil {
		return nil, fmt.Errorf("z_getbalanceforaccount (3) error: %w", err)
	}
	noteCounts, err := zGetNotesCount(w)
	if err != nil {
		return nil, fmt.Errorf("z_getnotescount error: %w", err)
	}
	return &balances{
		orchard: &balanceBreakdown{
			avail:     mature.Orchard,
			maturing:  zeroConf.Orchard - mature.Orchard,
			noteCount: noteCounts.Orchard,
		},
		sapling: zeroConf.Sapling,
		transparent: &balanceBreakdown{
			avail:    mature.Transparent,
			maturing: zeroConf.Transparent - mature.Transparent,
		},
	}, nil
}

type depositAddressJSON struct {
	Unified     string `json:"unified"`
	Transparent string `json:"transparent"`
	Orchard     string `json:"orchard"`
	Sapling     string `json:"sapling,omitempty"`
}

func (w *zecWallet) DepositAddress() (string, error) {
	addrRes, err := zGetAddressForAccount(w, shieldedAcctNumber, []string{transparentAddressType, orchardAddressType})
	if err != nil {
		return "", err
	}
	receivers, err := zGetUnifiedReceivers(w, addrRes.Address)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(&depositAddressJSON{
		Unified:     addrRes.Address,
		Transparent: receivers.Transparent,
		Orchard:     receivers.Orchard,
		Sapling:     receivers.Sapling,
	})
	if err != nil {
		return "", fmt.Errorf("error encoding address JSON: %w", err)
	}
	return depositAddrPrefix + string(b), nil

}

func (w *zecWallet) NewAddress() (string, error) {
	return w.DepositAddress()
}

// AddressUsed checks if a wallet address has been used.
func (w *zecWallet) AddressUsed(addrStr string) (bool, error) {
	// TODO: Resolve with new unified address encoding in https://github.com/decred/dcrdex/pull/2675
	recv, err := getReceivedByAddress(w, addrStr)
	return recv != 0, err
}

func (w *zecWallet) Info() *asset.WalletInfo {
	return WalletInfo
}

func (w *zecWallet) OwnsDepositAddress(addrStr string) (bool, error) {
	if strings.HasPrefix(addrStr, depositAddrPrefix) {
		var addrs depositAddressJSON
		if err := json.Unmarshal([]byte(addrStr[len(depositAddrPrefix):]), &addrs); err != nil {
			return false, fmt.Errorf("error decoding unified address info: %w", err)
		}
		addrStr = addrs.Unified
	}
	res, err := zValidateAddress(w, addrStr)
	if err != nil {
		return false, fmt.Errorf("error validating address: %w", err)
	}
	return res.IsMine, nil
}

func sendEnough(val uint64) enoughFunc {
	return func(inputCount, inputsSize, sum uint64) (bool, uint64) {
		fees := TxFeesZIP317(inputCount*btc.RedeemP2PKHInputSize+1, 2*btc.P2PKHOutputSize+1, 0, 0, 0, 0)
		total := fees + val
		if sum >= total {
			return true, sum - total
		}
		return false, 0
	}
}

// fundOrchard checks whether a send of the specified amount can be funded from
// the Orchard pool alone.
func (w *zecWallet) fundOrchard(amt uint64) (sum, fees uint64, n int, funded bool, err error) {
	unfiltered, err := zListUnspent(w)
	if err != nil {
		return 0, 0, 0, false, err
	}
	unspents := make([]*zListUnspentResult, 0, len(unfiltered))
	for _, u := range unfiltered {
		if u.Account == shieldedAcctNumber && u.Confirmations >= minOrchardConfs {
			unspents = append(unspents, u)
		}
	}
	sort.Slice(unspents, func(i, j int) bool {
		return unspents[i].Amount < unspents[j].Amount
	})
	var u *zListUnspentResult
	for n, u = range unspents {
		sum += toZats(u.Amount)
		fees = TxFeesZIP317(0, 0, 0, 0, 0, uint64(n))
		if sum > amt+fees {
			funded = true
			return
		}
	}
	return
}

func (w *zecWallet) EstimateSendTxFee(
	addrStr string, amt, _ /* feeRate*/ uint64, _ /* subtract */, maxWithdraw bool,
) (
	fees uint64, isValidAddress bool, err error,
) {

	res, err := zValidateAddress(w, addrStr)
	isValidAddress = err == nil && res.IsValid

	if maxWithdraw {
		addrType := transparentAddressType
		if isValidAddress {
			addrType = res.AddressType
		}
		fees, err = w.maxWithdrawFees(addrType)
		return
	}

	orchardSum, orchardFees, orchardN, orchardFunded, err := w.fundOrchard(amt)
	if err != nil {
		return 0, false, fmt.Errorf("error checking orchard funding: %w", err)
	}
	if orchardFunded {
		return orchardFees, isValidAddress, nil
	}

	bal, err := zGetBalanceForAccount(w, shieldedAcctNumber, 0)
	if err != nil {
		return 0, false, fmt.Errorf("z_getbalanceforaccount (0) error: %w", err)
	}

	remain := amt - orchardSum - bal.Sapling /* sapling fees not accounted for */

	const minConfs = 0
	_, _, spents, _, inputsSize, _, err := w.cm.Fund(0, minConfs, false, sendEnough(remain))
	if err != nil {
		return 0, false, fmt.Errorf("error funding value %d with %d transparent and %d orchard balances: %w",
			amt, bal.Transparent, orchardSum, err)
	}

	fees = TxFeesZIP317(inputsSize+uint64(wire.VarIntSerializeSize(uint64(len(spents)))), 2*btc.P2PKHOutputSize+1, 0, 0, 0, uint64(orchardN))
	return
}

func (w *zecWallet) maxWithdrawFees(addrType string) (uint64, error) {
	unfiltered, err := listUnspent(w)
	if err != nil {
		return 0, fmt.Errorf("listunspent error: %w", err)
	}
	numInputs := uint64(len(unfiltered))
	noteCounts, err := zGetNotesCount(w)
	if err != nil {
		return 0, fmt.Errorf("balances error: %w", err)
	}

	txInsSize := numInputs*btc.RedeemP2PKHInputSize + uint64(wire.VarIntSerializeSize(numInputs))
	nActionsOrchard := uint64(noteCounts.Orchard)
	var txOutsSize, nOutputsSapling uint64
	switch addrType {
	case unifiedAddressType, orchardAddressType:
		nActionsOrchard++
	case saplingAddressType:
		nOutputsSapling = 1
	default: // transparent
		txOutsSize = btc.P2PKHOutputSize + 1
	}
	// TODO: Do we get nJoinSplit from noteCounts.Sprout somehow?
	return TxFeesZIP317(txInsSize, txOutsSize, uint64(noteCounts.Sapling), nOutputsSapling, 0, nActionsOrchard), nil
}

type txCoin struct {
	txHash *chainhash.Hash
	v      uint64
}

var _ asset.Coin = (*txCoin)(nil)

// ID is a unique identifier for this coin.
func (c *txCoin) ID() util.Bytes {
	return c.txHash[:]
}

// String is a string representation of the coin.
func (c *txCoin) String() string {
	return c.txHash.String()
}

// Value is the available quantity, in atoms/satoshi.
func (c *txCoin) Value() uint64 {
	return c.v
}

// TxID is the ID of the transaction that created the coin.
func (c *txCoin) TxID() string {
	return c.txHash.String()
}

func (w *zecWallet) Send(addr string, value, feeRate uint64) (asset.Coin, error) {
	txHash, err := w.sendShielded(w.ctx, addr, value)
	if err != nil {
		return nil, err
	}

	selfSend, err := w.OwnsDepositAddress(addr)
	if err != nil {
		w.log.Errorf("error checking if address %q is owned: %v", addr, err)
	}
	txType := txdb.Send
	if selfSend {
		txType = txdb.SelfSend
	}

	tx, err := getTransaction(w, txHash)
	if err != nil {
		return nil, fmt.Errorf("unable to find tx after send %s: %v", txHash, err)
	}

	w.addTxToHistory(&txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		Type:      txType,
		ID:        txHash.String(),
		Amount:    value,
		Fees:      tx.RequiredTxFeesZIP317(),
	}, txHash)

	return &txCoin{
		txHash: txHash,
		v:      value,
	}, nil
}

// StandardSendFee returns the fees for a simple send tx with one input and two
// outputs.
func (w *zecWallet) StandardSendFee(feeRate uint64) uint64 {
	return TxFeesZIP317(btc.RedeemP2PKHInputSize+1, 2*btc.P2PKHOutputSize+1, 0, 0, 0, 0)
}

// TransactionConfirmations gets the number of confirmations for the specified
// transaction.
func (w *zecWallet) TransactionConfirmations(ctx context.Context, txID string) (confs uint32, err error) {
	txHash, err := chainhash.NewHashFromStr(txID)
	if err != nil {
		return 0, fmt.Errorf("error decoding txid %q: %w", txID, err)
	}
	tx, err := getWalletTransaction(w, txHash)
	if err != nil {
		return 0, err
	}
	if tx.Confirmations < 0 {
		tx.Confirmations = 0
	}

	return uint32(tx.Confirmations), nil
}

func (w *zecWallet) SyncStatus() (*asset.SyncStatus, error) {
	ss, err := syncStatus(w)
	if err != nil {
		return nil, err
	}
	ss.StartingBlocks = w.tipAtConnect.Load()

	if ss.TargetHeight == 0 { // do not say progress = 1
		return &asset.SyncStatus{}, nil
	}
	if ss.Synced {
		numPeers, err := peerCount(w)
		if err != nil {
			return nil, err
		}
		ss.Synced = numPeers > 0
	}
	return ss, nil
}

func (w *zecWallet) ValidateAddress(addr string) bool {
	res, err := zValidateAddress(w, addr)
	if err != nil {
		w.log.Errorf("z_validateaddress error for address %q: %v", addr, err)
		return false
	}
	return res.IsValid
}

func (w *zecWallet) lastShieldedAddress() (addr string, err error) {
	if addrPtr := w.lastAddress.Load(); addrPtr != nil {
		return *addrPtr, nil
	}
	accts, err := zListAccounts(w)
	if err != nil {
		return "", err
	}
	for _, acct := range accts {
		if acct.Number != shieldedAcctNumber {
			continue
		}
		if len(acct.Addresses) == 0 {
			break // generate first address
		}
		lastAddr := acct.Addresses[len(acct.Addresses)-1].UnifiedAddr
		w.lastAddress.Store(&lastAddr)
		return lastAddr, nil // Orchard = Unified for account 1
	}
	return w.newShieldedAddress()
}

// Balance adds a sum of shielded pool balances to the transparent balance info.
//
// Since v5.4.0, the getnewaddress RPC is deprecated if favor of using unified
// addresses from accounts generated with z_getnewaccount. Addresses are
// generated from account 0. Any addresses previously generated using
// getnewaddress belong to a legacy account that is not listed with
// z_listaccount, nor addressable with e.g. z_getbalanceforaccount. For
// transparent addresses, we still use the getbalance RPC, which combines
// transparent balance from both legacy and generated accounts. This matches the
// behavior of the listunspent RPC, and conveniently makes upgrading simple. So
// even though we ONLY use account 0 to generate t-addresses, any account's
// transparent outputs are eligible for trading. To minimize confusion, we don't
// add transparent receivers to addresses generated from the shielded account.
// This doesn't preclude a user doing something silly with zcash-cli.
func (w *zecWallet) Balance() (*asset.Balance, error) {

	bals, err := w.balances()
	if err != nil {
		return nil, codedError(errBalanceRetrieval, err)
	}
	locked, err := w.lockedZats()
	if err != nil {
		return nil, err
	}

	bal := &asset.Balance{
		Available: bals.orchard.avail + bals.transparent.avail,
		Immature:  bals.orchard.maturing + bals.transparent.maturing,
		Locked:    locked,
		Other:     make(map[asset.BalanceCategory]asset.CustomBalance),
	}

	bal.Other[asset.BalanceCategoryShielded] = asset.CustomBalance{
		Amount: bals.orchard.avail, // + bals.orchard.maturing,
	}

	return bal, nil
}

// lockedSats is the total value of locked outputs, as locked with LockUnspent.
func (w *zecWallet) lockedZats() (uint64, error) {
	lockedOutpoints, err := listLockUnspent(w, w.log)
	if err != nil {
		return 0, err
	}
	var sum uint64
	for _, rpcOP := range lockedOutpoints {
		txHash, err := chainhash.NewHashFromStr(rpcOP.TxID)
		if err != nil {
			return 0, err
		}
		pt := btc.NewOutPoint(txHash, rpcOP.Vout)
		utxo := w.cm.LockedOutput(pt)
		if utxo != nil {
			sum += utxo.Amount
			continue
		}
		tx, err := getWalletTransaction(w, txHash)
		if err != nil {
			return 0, err
		}
		txOut, err := txOutFromTxBytes(tx.Bytes, rpcOP.Vout, deserializeTx, hashTx)
		if err != nil {
			return 0, err
		}
		sum += uint64(txOut.Value)
	}
	return sum, nil
}

// newShieldedAddress creates a new shielded address.
func (w *zecWallet) newShieldedAddress() (string, error) {
	// An orchard address is the same as a unified address with only an orchard
	// receiver.
	addrRes, err := zGetAddressForAccount(w, shieldedAcctNumber, []string{orchardAddressType})
	if err != nil {
		return "", err
	}
	addr := addrRes.Address
	w.lastAddress.Store(&addr)
	return addr, nil
}

// sendOne is a helper function for doing a z_sendmany with a single recipient.
func (w *zecWallet) sendOne(ctx context.Context, fromAddr, toAddr string, amt uint64, priv privacyPolicy) (*chainhash.Hash, error) {
	recip := singleSendManyRecipient(toAddr, amt)

	operationID, err := zSendMany(w, fromAddr, recip, priv)
	if err != nil {
		return nil, fmt.Errorf("z_sendmany error: %w", err)
	}

	return w.awaitSendManyOperation(ctx, w, operationID)
}

// awaitSendManyOperation waits for the asynchronous result from a z_sendmany
// operation.
func (w *zecWallet) awaitSendManyOperation(ctx context.Context, c rpcCaller, operationID string) (*chainhash.Hash, error) {
	for {
		res, err := zGetOperationResult(c, operationID)
		if err != nil && !errors.Is(err, ErrEmptyOpResults) {
			return nil, fmt.Errorf("error getting operation result: %w", err)
		}
		if res != nil {
			switch res.Status {
			case "failed":
				return nil, fmt.Errorf("z_sendmany operation failed: %s", res.Error.Message)

			case "success":
				if res.Result == nil {
					return nil, errors.New("async operation result = 'success' but no Result field")
				}
				txHash, err := chainhash.NewHashFromStr(res.Result.TxID)
				if err != nil {
					return nil, fmt.Errorf("error decoding txid: %w", err)
				}
				return txHash, nil
			default:
				w.log.Warnf("unexpected z_getoperationresult status %q: %+v", res.Status)
			}

		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (w *zecWallet) sendOneShielded(ctx context.Context, toAddr string, amt uint64, priv privacyPolicy) (*chainhash.Hash, error) {
	lastAddr, err := w.lastShieldedAddress()
	if err != nil {
		return nil, err
	}
	return w.sendOne(ctx, lastAddr, toAddr, amt, priv)
}

func (w *zecWallet) sendShielded(ctx context.Context, toAddr string, amt uint64) (*chainhash.Hash, error) {
	res, err := zValidateAddress(w, toAddr)
	if err != nil {
		return nil, fmt.Errorf("error validating address: %w", err)
	}

	if !res.IsValid {
		return nil, fmt.Errorf("invalid address %q", toAddr)
	}

	// TODO: We're using NoPrivacy for everything except orchard-to-orchard
	// sends. This can potentially be tightened up, but why? The RPC provides
	// no insight into calculating what privacy level is needed, so we would
	// have to do a bunch of calculations regarding sources of funds and
	// potential for change outputs. In the end, it changes nothing, and zcashd
	// will optimize privacy the best it can.
	priv := NoPrivacy
	if res.AddressType == unifiedAddressType {
		r, err := zGetUnifiedReceivers(w, toAddr)
		if err != nil {
			return nil, fmt.Errorf("error getting unified receivers: %w", err)
		}
		if r.Orchard != "" {
			if _, _, _, isFunded, err := w.fundOrchard(amt); err != nil {
				return nil, fmt.Errorf("error checking orchard funding: %w", err)
			} else if isFunded {
				priv = FullPrivacy
			}
		}
	}

	txHash, err := w.sendOneShielded(ctx, toAddr, amt, priv)
	if err != nil {
		return nil, err
	}

	return txHash, nil
}

func zecTx(tx *wire.MsgTx) *Tx {
	return NewTxFromMsgTx(tx, MaxExpiryHeight)
}

// isDust returns true if the output will be rejected as dust.
func isDust(val, outputSize uint64) bool {
	// See https://github.com/zcash/zcash/blob/5066efbb98bc2af5eed201212d27c77993950cee/src/primitives/transaction.h#L630
	// https://github.com/zcash/zcash/blob/5066efbb98bc2af5eed201212d27c77993950cee/src/primitives/transaction.cpp#L127
	// Also see informative comments hinting towards future changes at
	// https://github.com/zcash/zcash/blob/master/src/policy/policy.h
	sz := outputSize + 148                        // 148 accounts for an input on spending tx
	const oneThirdDustThresholdRate = 100         // zats / kB
	nFee := oneThirdDustThresholdRate * sz / 1000 // This is different from BTC
	if nFee == 0 {
		nFee = oneThirdDustThresholdRate
	}

	return val < 3*nFee
}

// Convert the ZEC value to satoshi.
func toZats(v float64) uint64 {
	return uint64(math.Round(v * 1e8))
}

func hashTx(tx *wire.MsgTx) *chainhash.Hash {
	h := zecTx(tx).TxHash()
	return &h
}

func deserializeTx(b []byte) (*wire.MsgTx, error) {
	tx, err := DeserializeTx(b)
	if err != nil {
		return nil, err
	}
	return tx.MsgTx, nil
}

func (w *zecWallet) txDB() *btc.BadgerTxDB {
	db := w.txHistoryDB.Load()
	if db == nil {
		return nil
	}
	return db
}

func (w *zecWallet) listSinceBlock(start int64) ([]btcjson.ListTransactionsResult, error) {
	hash, err := getBlockHash(w, start)
	if err != nil {
		return nil, err
	}

	res, err := listSinceBlock(w, hash)
	if err != nil {
		return nil, err
	}

	return res, nil
}

func (w *zecWallet) addTxToHistory(wt *txdb.WalletTransaction, txHash *chainhash.Hash, skipNotes ...bool) {
	txHistoryDB := w.txDB()
	if txHistoryDB == nil {
		return
	}

	ewt := &btc.ExtendedWalletTx{
		WalletTransaction: wt,
	}

	if wt.BlockNumber == 0 {
		w.pendingTxsMtx.Lock()
		w.pendingTxs[*txHash] = ewt
		w.pendingTxsMtx.Unlock()
	}

	err := txHistoryDB.StoreTx(ewt)
	if err != nil {
		w.log.Errorf("failed to store tx in tx history db: %v", err)
	}

	skipNote := len(skipNotes) > 0 && skipNotes[0]
	if !skipNote {
		w.emit.TransactionNote(wt, true)
	}
}

func (w *zecWallet) startTxHistoryDB(ctx context.Context) (*sync.WaitGroup, error) {
	// The transaction database is the master tx DB handle supplied by Core,
	// scoped to this wallet's network. Core owns its lifecycle, so there is no
	// per-wallet database to open or close here and nothing to wait on.
	var wg sync.WaitGroup
	if w.netTxDB == nil {
		return &wg, nil
	}

	db, err := btc.NewBadgerTxDB(w.netTxDB, w.log)
	if err != nil {
		return nil, err
	}
	w.txHistoryDB.Store(db)

	pendingTxs, err := db.GetPendingTxs()
	if err != nil {
		return nil, fmt.Errorf("failed to load unconfirmed txs: %v", err)
	}

	w.pendingTxsMtx.Lock()
	for _, tx := range pendingTxs {
		txHash, err := chainhash.NewHashFromStr(tx.ID)
		if err != nil {
			w.log.Errorf("Invalid txid %v from tx history db: %v", tx.ID, err)
			continue
		}
		w.pendingTxs[*txHash] = tx
	}
	w.pendingTxsMtx.Unlock()

	lastQuery, err := db.GetLastReceiveTxQuery()
	if errors.Is(err, btc.ErrNeverQueried) {
		lastQuery = 0
	} else if err != nil {
		return nil, fmt.Errorf("failed to load last query time: %v", err)
	}

	w.receiveTxLastQuery.Store(lastQuery)

	return &wg, nil
}

// WalletTransaction returns a transaction that either the wallet has made or
// one in which the wallet has received funds.
func (w *zecWallet) WalletTransaction(_ context.Context, txID string) (*txdb.WalletTransaction, error) {
	coinID, err := hex.DecodeString(txID)
	if err == nil {
		txHash, _, err := decodeCoinID(coinID)
		if err == nil {
			txID = txHash.String()
		}
	}

	txHistoryDB := w.txDB()
	if txHistoryDB == nil {
		return nil, fmt.Errorf("tx database not initialized")
	}
	tx, err := txHistoryDB.GetTx(txID)
	if err != nil && !errors.Is(err, asset.CoinNotFoundError) {
		return nil, err
	}

	if tx == nil {
		txHash, err := chainhash.NewHashFromStr(txID)
		if err != nil {
			return nil, fmt.Errorf("error decoding txid %s: %w", txID, err)
		}

		gtr, err := getTransaction(w, txHash)
		if err != nil {
			return nil, fmt.Errorf("error getting transaction %s: %w", txID, err)
		}

		var (
			blockHeight int32
			blockTime   int64
		)
		if gtr.blockHash != nil && *gtr.blockHash != (chainhash.Hash{}) {
			block, _, err := getVerboseBlockHeader(w, gtr.blockHash)
			if err != nil {
				return nil, fmt.Errorf("error getting block height for %s: %v", gtr.blockHash, err)
			}
			blockHeight = int32(block.Height)
			blockTime = block.Time
		}

		tx, err = w.idUnknownTx(&btcjson.ListTransactionsResult{
			BlockHeight: &blockHeight,
			BlockTime:   blockTime,
			TxID:        txID,
			Category:    gtr.category,
		})
		if err != nil {
			return nil, fmt.Errorf("error identifying transaction: %v", err)
		}

		tx.BlockNumber = uint64(blockHeight)
		tx.Timestamp = uint64(blockTime)
		w.addTxToHistory(tx, txHash, false)
	}

	// If the wallet knows about the transaction, it will be part of the
	// available balance, so we always return Confirmed = true. tx may be the
	// same *txdb.WalletTransaction stored in w.pendingTxs, so return a copy
	// rather than mutating it in place.
	txCopy := *tx
	txCopy.Confirmed = true

	return &txCopy, nil
}

// PendingTransactions loads wallet transactions that are not yet confirmed.
func (w *zecWallet) PendingTransactions(ctx context.Context) []*txdb.WalletTransaction {
	w.pendingTxsMtx.RLock()
	defer w.pendingTxsMtx.RUnlock()
	txs := make([]*txdb.WalletTransaction, 0, len(w.pendingTxs))
	for _, tx := range w.pendingTxs {
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
func (w *zecWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	txHistoryDB := w.txDB()
	if txHistoryDB == nil {
		return nil, fmt.Errorf("tx database not initialized")
	}

	return txHistoryDB.GetTxs(req)
}

const (
	sendCategory    = "send"
	receiveCategory = "receive"
)

// idUnknownTx identifies the type and details of a transaction either made
// or received by the wallet.
func (w *zecWallet) idUnknownTx(tx *btcjson.ListTransactionsResult) (*txdb.WalletTransaction, error) {
	txHash, err := chainhash.NewHashFromStr(tx.TxID)
	if err != nil {
		return nil, fmt.Errorf("error decoding tx hash %s: %v", tx.TxID, err)
	}
	msgTx, err := getTransaction(w, txHash)
	if err != nil {
		return nil, err
	}

	var totalOut uint64
	for _, txOut := range msgTx.TxOut {
		totalOut += uint64(txOut.Value)
	}

	fee := msgTx.RequiredTxFeesZIP317()

	// Any other P2SH may be a swap or a send. We cannot determine unless we
	// look up the transaction that spends this UTXO.
	txPaysToScriptHash := func(msgTx *zTx) (v uint64) {
		for _, txOut := range msgTx.TxOut {
			if txscript.IsPayToScriptHash(txOut.PkScript) {
				v += uint64(txOut.Value)
			}
		}
		return
	}
	if v := txPaysToScriptHash(msgTx); v > 0 {
		return &txdb.WalletTransaction{
			NetworkID: BipID,
			AssetID:   BipID,
			Type:      txdb.SwapOrSend,
			ID:        tx.TxID,
			Amount:    v,
			Fees:      fee,
		}, nil
	}

	// Helper function will help us identify inputs that spend P2SH contracts.
	containsContractAtPushIndex := func(msgTx *zTx, idx int, isContract func(contract []byte) bool) bool {
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
		_, _, _, _, err := btc.ExtractSwapDetails(contract, false /* segwit */, w.btcParams)
		return err == nil
	}
	redeemsSwap := func(msgTx *zTx) bool {
		return containsContractAtPushIndex(msgTx, 4, contractIsSwap)
	}
	if redeemsSwap(msgTx) {
		return &txdb.WalletTransaction{
			NetworkID: BipID,
			AssetID:   BipID,
			Type:      txdb.Redeem,
			ID:        tx.TxID,
			Amount:    totalOut + fee,
			Fees:      fee,
		}, nil
	}
	refundsSwap := func(msgTx *zTx) bool {
		return containsContractAtPushIndex(msgTx, 3, contractIsSwap)
	}
	if refundsSwap(msgTx) {
		return &txdb.WalletTransaction{
			NetworkID: BipID,
			AssetID:   BipID,
			Type:      txdb.Refund,
			ID:        tx.TxID,
			Amount:    totalOut + fee,
			Fees:      fee,
		}, nil
	}

	allOutputsPayUs := func(msgTx *zTx) bool {
		for _, txOut := range msgTx.TxOut {
			_, addrs, _, err := txscript.ExtractPkScriptAddrs(txOut.PkScript, w.btcParams)
			if err != nil {
				w.log.Errorf("ExtractAddrs error: %v", err)
				return false
			}
			if len(addrs) != 1 { // sanity check
				return false
			}

			addr, err := EncodeAddress(addrs[0], w.addrParams)
			if err != nil {
				w.log.Errorf("unable to encode address: %v", err)
				return false
			}
			owns, err := w.OwnsDepositAddress(addr)
			if err != nil {
				w.log.Errorf("w.OwnsDepositAddress error: %v", err)
				return false
			}
			if !owns {
				return false
			}
		}

		return true
	}

	if tx.Category == sendCategory && allOutputsPayUs(msgTx) && len(msgTx.TxIn) == 1 {
		return &txdb.WalletTransaction{
			NetworkID: BipID,
			AssetID:   BipID,
			Type:      txdb.Split,
			ID:        tx.TxID,
			Fees:      fee,
		}, nil
	}

	txOutDirection := func(msgTx *zTx) (in, out uint64) {
		for _, txOut := range msgTx.TxOut {
			_, addrs, _, err := txscript.ExtractPkScriptAddrs(txOut.PkScript, w.btcParams)
			if err != nil {
				w.log.Errorf("ExtractAddrs error: %v", err)
				continue
			}
			if len(addrs) != 1 { // sanity check
				continue
			}

			addr, err := EncodeAddress(addrs[0], w.addrParams)
			if err != nil {
				w.log.Errorf("unable to encode address: %v", err)
				continue
			}
			owns, err := w.OwnsDepositAddress(addr)
			if err != nil {
				w.log.Errorf("w.OwnsDepositAddress error: %v", err)
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

	getRecipient := func(msgTx *zTx, receive bool) *string {
		for _, txOut := range msgTx.TxOut {
			_, addrs, _, err := txscript.ExtractPkScriptAddrs(txOut.PkScript, w.btcParams)
			if err != nil {
				w.log.Errorf("ExtractAddrs error: %v", err)
				continue
			}
			if len(addrs) != 1 { // sanity check
				continue
			}

			addr, err := EncodeAddress(addrs[0], w.addrParams)
			if err != nil {
				w.log.Errorf("unable to encode address: %v", err)
				continue
			}
			owns, err := w.OwnsDepositAddress(addr)
			if err != nil {
				w.log.Errorf("w.OwnsDepositAddress error: %v", err)
				continue
			}

			if receive == owns {
				return &addr
			}
		}
		return nil
	}

	if tx.Category == sendCategory {
		txType := txdb.Send
		if allOutputsPayUs(msgTx) {
			txType = txdb.SelfSend
		}
		return &txdb.WalletTransaction{
			NetworkID: BipID,
			AssetID:   BipID,
			Type:      txType,
			ID:        tx.TxID,
			Amount:    out,
			Fees:      fee,
			Recipient: getRecipient(msgTx, false),
		}, nil
	}

	return &txdb.WalletTransaction{
		NetworkID: BipID,
		AssetID:   BipID,
		Type:      txdb.Receive,
		ID:        tx.TxID,
		Amount:    in,
		Fees:      fee,
		Recipient: getRecipient(msgTx, true),
	}, nil
}

// addUnknownTransactionsToHistory checks for any transactions the wallet has
// made or received that are not part of the transaction history. It scans
// from the last point to which it had previously scanned to the current tip.
func (w *zecWallet) addUnknownTransactionsToHistory(tip uint64) {
	txHistoryDB := w.txDB()

	// Zcash has a maximum reorg length of 100 blocks.
	const blockQueryBuffer = 100
	var blockToQuery uint64
	lastQuery := w.receiveTxLastQuery.Load()
	if lastQuery == 0 {
		// TODO: use wallet birthday instead of block 0.
		// blockToQuery = 0
	} else if lastQuery < tip-blockQueryBuffer {
		blockToQuery = lastQuery - blockQueryBuffer
	} else {
		blockToQuery = tip - blockQueryBuffer
	}

	txs, err := w.listSinceBlock(int64(blockToQuery))
	if err != nil {
		w.log.Errorf("Error listing transactions since block %d: %v", blockToQuery, err)
		return
	}

	for _, tx := range txs {
		if w.ctx.Err() != nil {
			return
		}
		txHash, err := chainhash.NewHashFromStr(tx.TxID)
		if err != nil {
			w.log.Errorf("Error decoding tx hash %s: %v", tx.TxID, err)
			continue
		}
		_, err = txHistoryDB.GetTx(txHash.String())
		if err == nil {
			continue
		}
		if !errors.Is(err, asset.CoinNotFoundError) {
			w.log.Errorf("Error getting tx %s: %v", txHash.String(), err)
			continue
		}
		wt, err := w.idUnknownTx(&tx)
		if err != nil {
			w.log.Errorf("error identifying transaction: %v", err)
			continue
		}

		if tx.BlockIndex != nil && *tx.BlockIndex > 0 && *tx.BlockIndex < int64(tip-blockQueryBuffer) {
			wt.BlockNumber = uint64(*tx.BlockIndex)
			wt.Timestamp = uint64(tx.BlockTime)
		}

		// Don't send notifications for the initial sync to avoid spamming the
		// front end. A notification is sent at the end of the initial sync.
		w.addTxToHistory(wt, txHash, blockToQuery == 0)
	}

	w.receiveTxLastQuery.Store(tip)
	err = txHistoryDB.SetLastReceiveTxQuery(tip)
	if err != nil {
		w.log.Errorf("Error setting last query to %d: %v", tip, err)
	}

	if blockToQuery == 0 {
		w.emit.TransactionHistorySyncedNote()
	}
}

// syncTxHistory checks to see if there are any transactions which the wallet
// has made or received that are not part of the transaction history, then
// identifies and adds them. It also checks all the pending transactions to see
// if they have been mined into a block, and if so, updates the transaction
// history to reflect the block height.
func (w *zecWallet) syncTxHistory(tip uint64) {
	if !w.syncingTxHistory.CompareAndSwap(false, true) {
		return
	}
	defer w.syncingTxHistory.Store(false)

	txHistoryDB := w.txDB()
	if txHistoryDB == nil {
		// It's actually impossible to get here, because we error and return
		// early in Connect if startTxHistoryDB returns an error, but we'll
		// log this for good measure anyway.
		w.log.Error("Transaction history database was not initialized")
		return
	}

	ss, err := w.SyncStatus()
	if err != nil {
		w.log.Errorf("Error getting sync status: %v", err)
		return
	}
	if ss == nil || !ss.Synced {
		return
	}

	w.addUnknownTransactionsToHistory(tip)

	pendingTxsCopy := make(map[chainhash.Hash]btc.ExtendedWalletTx, len(w.pendingTxs))
	w.pendingTxsMtx.RLock()
	for hash, tx := range w.pendingTxs {
		pendingTxsCopy[hash] = *tx
	}
	w.pendingTxsMtx.RUnlock()

	handlePendingTx := func(txHash chainhash.Hash, tx *btc.ExtendedWalletTx) {
		gtr, err := getTransaction(w, &txHash)
		if errors.Is(err, asset.CoinNotFoundError) {
			err = txHistoryDB.RemoveTx(txHash.String())
			if err == nil {
				w.pendingTxsMtx.Lock()
				delete(w.pendingTxs, txHash)
				w.pendingTxsMtx.Unlock()
			} else {
				// Leave it in the pendingPendingTxs and attempt to remove it
				// again next time.
				w.log.Errorf("Error removing tx %s from the history store: %v", txHash.String(), err)
			}
			return
		}
		if err != nil {
			if w.ctx.Err() != nil {
				return
			}
			w.log.Errorf("Error getting transaction %s: %v", txHash, err)
			return
		}

		if gtr.blockHash != nil && *gtr.blockHash != (chainhash.Hash{}) {
			block, _, err := getVerboseBlockHeader(w, gtr.blockHash)
			if err != nil {
				w.log.Errorf("Error getting block height for %s: %v", gtr.blockHash, err)
				return
			}
			blockHeight := block.Height
			if tx.BlockNumber != uint64(blockHeight) || tx.Timestamp != uint64(block.Time) {
				tx.BlockNumber = uint64(blockHeight)
				tx.Timestamp = uint64(block.Time)
			}
		} else if gtr.blockHash == nil && tx.BlockNumber != 0 {
			tx.BlockNumber = 0
			tx.Timestamp = 0
		}

		var confs uint64
		if tx.BlockNumber > 0 && tip >= tx.BlockNumber {
			confs = tip - tx.BlockNumber + 1
		}

		var updated bool
		if confs >= defaultConfTarget {
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
				w.log.Errorf("Error updating tx %s: %v", txHash, err)
				return
			}

			w.pendingTxsMtx.Lock()
			if tx.Confirmed {
				delete(w.pendingTxs, txHash)
			} else {
				w.pendingTxs[txHash] = tx
			}
			w.pendingTxsMtx.Unlock()

			w.emit.TransactionNote(tx.WalletTransaction, false)
		}
	}

	for hash, tx := range pendingTxsCopy {
		handlePendingTx(hash, &tx)
	}
}
