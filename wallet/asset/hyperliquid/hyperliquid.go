// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/keygen"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/keystore"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/decred/dcrd/hdkeychain/v3"
	"golang.org/x/net/proxy"
)

// ErrSendOutcomeUnknown is returned when a send encounters a transport error
// after the request was dispatched. The transaction may or may not have landed.
// Callers must not retry without first checking PendingTransactions.
var ErrSendOutcomeUnknown = errors.New("send outcome unknown")

var (
	seedDerivationPath = []uint32{
		hdkeychain.HardenedKeyStart + 44,    // purpose 44'
		hdkeychain.HardenedKeyStart + 10001, // HYPE coin type 10001'
		hdkeychain.HardenedKeyStart,         // account 0'
		0,                                   // branch 0
		0,                                   // index 0
	}

	// evmAddressRE matches a 0x-prefixed 40-hex-char Ethereum-style address.
	evmAddressRE = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

	walletDefinition = &asset.WalletDefinition{
		Seeded:      true,
		Type:        "rpc",
		Tab:         "Native",
		Description: "Hyperliquid HYPE wallet",
		ConfigOpts: []*asset.ConfigOption{{
			Key:         "apiendpoint",
			DisplayName: "API Endpoint",
			Description: "Hyperliquid API base URL (optional override)",
		}, {
			Key:         "wsendpoint",
			DisplayName: "WebSocket Endpoint",
			Description: "Hyperliquid WebSocket URL (optional override)",
		}},
	}

	WalletInfo = asset.WalletInfo{
		Name:              "Hyperliquid",
		SupportedVersions: []uint32{0},
		UnitInfo:          UnitInfo,
		AvailableWallets:  []*asset.WalletDefinition{walletDefinition},
		BlockchainClass:   asset.BlockchainClassAccount,
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
		},
	}
)

func init() {
	asset.Register(BipID, &Driver{})
}

// Driver implements asset.Driver and asset.Creator.
type Driver struct{}

var _ asset.Driver = (*Driver)(nil)
var _ asset.Creator = (*Driver)(nil)

func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	return newWallet(cfg, logger, network)
}

func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	return decodeCoinID(coinID)
}

func (d *Driver) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

func (d *Driver) Exists(walletType, dataDir string, _ map[string]string, _ util.Network) (bool, error) {
	if walletType != "rpc" {
		return false, fmt.Errorf("unknown wallet type %q", walletType)
	}
	_, err := os.Stat(filepath.Join(dataDir, keyFileName))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (d *Driver) Create(params *asset.CreateWalletParams) error {
	if params.Type != "rpc" {
		return fmt.Errorf("unknown wallet type %q", params.Type)
	}
	if len(params.Seed) == 0 {
		return fmt.Errorf("wallet seed cannot be empty")
	}

	privKey, addr, zero, err := privKeyFromSeed(params.Seed)
	if err != nil {
		return err
	}
	defer zero()

	if err := os.MkdirAll(params.DataDir, 0700); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}

	_ = addr
	raw := gethcrypto.FromECDSA(privKey)
	defer encode.ClearBytes(raw)
	pubKey := gethcrypto.CompressPubkey(&privKey.PublicKey)
	return keystore.Save(filepath.Join(params.DataDir, keyFileName), raw, pubKey, params.Pass)
}

// HyperWallet implements asset.Wallet for the Hyperliquid L1.
type HyperWallet struct {
	log           util.Logger
	net           util.Network
	emit          *asset.WalletEmitter
	peersChange   func(uint32, error)
	lastPeerCount uint32 // accessed via sync/atomic; 0 = disconnected, 1 = connected
	dataDir       string
	settings      map[string]string

	keyMtx  sync.RWMutex
	privKey []byte // nil when locked; 32-byte secp256k1 private key
	pubKey  []byte // compressed secp256k1 public key (33 bytes)
	address string // "0x..." lowercase hex

	sendMtx       sync.Mutex
	nextSendNonce uint64

	hypeTokenID string // "HYPE:<tokenId>", resolved on Connect

	running        atomic.Bool
	ctx            context.Context    // set in Connect; cancelled on shutdown
	ctxCancel      context.CancelFunc // cancels ctx
	rpc            *rpcClient
	txDB           txdb.NetworkTxDB // nil when not provided (e.g. in tests)
	metaTable      txdb.StoreTable  // set in Connect; nil if txDB is nil
	pendingIDIndex txdb.TxIndex     // set in Connect; nil if txDB is nil

	ledgerMtx sync.Mutex
	scanMtx   sync.Mutex
	tipMtx    sync.RWMutex
	tip       uint64    // unix milliseconds of last successful API contact
	tipTime   time.Time // wall time of last tip update

	// lastScannedMS is the unix millisecond timestamp up to which we have
	// scanned ledger updates for incoming transactions.
	lastScannedMS int64
}

var _ asset.Wallet = (*HyperWallet)(nil)
var _ asset.Authenticator = (*HyperWallet)(nil)
var _ asset.Withdrawer = (*HyperWallet)(nil)
var _ asset.Rescanner = (*HyperWallet)(nil)

func newWallet(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (*HyperWallet, error) {
	pubKey, err := keystore.PubKey(filepath.Join(cfg.DataDir, keyFileName))
	if err != nil {
		return nil, fmt.Errorf("loading key file: %w", err)
	}

	return &HyperWallet{
		log:         logger,
		net:         network,
		emit:        cfg.Emit,
		peersChange: cfg.PeersChange,
		dataDir:     cfg.DataDir,
		settings:    cfg.Settings,
		pubKey:      pubKey,
		address:     pubKeyToAddress(pubKey),
		txDB:        cfg.NetworkTxDB,
	}, nil
}

// Unlock decrypts the private key with the wallet password.
func (w *HyperWallet) Unlock(pw []byte) error {
	privBytes, err := keystore.Open(filepath.Join(w.dataDir, keyFileName), pw)
	if err != nil {
		return fmt.Errorf("decrypting key: %w", err)
	}

	// Verify the decrypted key matches the stored public key.
	priv, err := gethcrypto.ToECDSA(privBytes)
	if err != nil {
		encode.ClearBytes(privBytes)
		return fmt.Errorf("parsing private key: %w", err)
	}
	derivedPub := gethcrypto.CompressPubkey(&priv.PublicKey)
	if !bytes.Equal(derivedPub, w.pubKey) {
		encode.ClearBytes(privBytes)
		return fmt.Errorf("decrypted key does not match stored public key")
	}
	priv = nil

	w.keyMtx.Lock()
	encode.ClearBytes(w.privKey)
	w.privKey = privBytes
	w.keyMtx.Unlock()
	return nil
}

// Lock zeros the private key from memory.
func (w *HyperWallet) Lock() error {
	w.keyMtx.Lock()
	defer w.keyMtx.Unlock()
	encode.ClearBytes(w.privKey)
	w.privKey = nil
	return nil
}

// Locked returns true if the private key is not in memory.
func (w *HyperWallet) Locked() bool {
	w.keyMtx.RLock()
	defer w.keyMtx.RUnlock()
	return w.privKey == nil
}

// Connect establishes the API connection, resolves the HYPE token ID, and
// starts the tip poller.
func (w *HyperWallet) Connect(ctx context.Context) (_ *sync.WaitGroup, err error) {
	if w.running.Load() {
		return nil, fmt.Errorf("wallet already connected")
	}
	w.ctx, w.ctxCancel = context.WithCancel(ctx)
	defer func() {
		if err != nil {
			w.ctxCancel()
		}
	}()
	rpc, err := newRPCClient(w.apiEndpoint(), w.settings["torproxy"], w.log)
	if err != nil {
		return nil, fmt.Errorf("creating RPC client: %w", err)
	}
	w.rpc = rpc

	// Resolve HYPE token identifier from spot meta.
	meta, err := rpc.spotMeta(w.ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching spot meta: %w", err)
	}
	hypeID, err := hypeTokenIDFromMeta(meta)
	if err != nil {
		return nil, fmt.Errorf("resolving HYPE token ID: %w", err)
	}
	w.hypeTokenID = hypeID
	w.log.Debugf("HYPE token ID: %s", hypeID)

	if w.txDB != nil {
		mt, err := w.txDB.Table(metaTableName)
		if err != nil {
			return nil, fmt.Errorf("opening meta table: %w", err)
		}
		w.metaTable = mt

		idx, err := w.txDB.AddUniqueTxIndex(pendingIDIndexName, pendingIDIndexKey)
		if err != nil {
			return nil, fmt.Errorf("creating pending-ID index: %w", err)
		}
		w.pendingIDIndex = idx
	}
	lastScannedMS, err := w.getLastScannedMS()
	if err != nil {
		return nil, fmt.Errorf("loading persisted scan watermark: %w", err)
	}
	maxPendingNonce, err := w.highestPendingNonce()
	if err != nil {
		return nil, fmt.Errorf("loading pending send state: %w", err)
	}

	// Seed initial tip from local clock; spotMeta already confirmed connectivity.
	t := time.Now()
	w.tipMtx.Lock()
	w.tip = uint64(t.UnixMilli())
	w.tipTime = t
	w.lastScannedMS = lastScannedMS
	w.tipMtx.Unlock()
	if uint64(lastScannedMS) > maxPendingNonce {
		maxPendingNonce = uint64(lastScannedMS)
	}
	w.sendMtx.Lock()
	w.nextSendNonce = maxPendingNonce
	w.sendMtx.Unlock()

	w.running.Store(true)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer w.running.Store(false)
		defer w.ctxCancel()
		w.tipPoller(w.ctx)
	}()

	return &wg, nil
}

func (w *HyperWallet) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

func (w *HyperWallet) Balance() (*asset.Balance, error) {
	if w.rpc == nil {
		return nil, asset.ErrConnectionDown
	}
	available, held, err := w.rpc.spotBalance(w.ctx, w.address)
	if err != nil {
		return nil, err
	}
	return &asset.Balance{
		Available: available,
		Locked:    held,
	}, nil
}

func (w *HyperWallet) DepositAddress() (string, error) {
	return w.address, nil
}

func (w *HyperWallet) OwnsDepositAddress(address string) (bool, error) {
	return strings.EqualFold(address, w.address), nil
}

func (w *HyperWallet) Send(address string, value, _ uint64) (asset.Coin, error) {
	return w.send(address, value)
}

func (w *HyperWallet) Withdraw(address string, value, _ uint64) (asset.Coin, error) {
	return w.send(address, value)
}

func (w *HyperWallet) send(address string, value uint64) (asset.Coin, error) {
	if w.rpc == nil {
		return nil, asset.ErrConnectionDown
	}
	if !w.ValidateAddress(address) {
		return nil, fmt.Errorf("invalid Hyperliquid address %q", address)
	}
	if value == 0 {
		return nil, fmt.Errorf("cannot send zero value")
	}

	w.keyMtx.RLock()
	if w.privKey == nil {
		w.keyMtx.RUnlock()
		return nil, fmt.Errorf("wallet is locked")
	}
	privKeyCopy := make([]byte, len(w.privKey))
	copy(privKeyCopy, w.privKey)
	w.keyMtx.RUnlock()
	defer encode.ClearBytes(privKeyCopy)

	priv, err := gethcrypto.ToECDSA(privKeyCopy)
	if err != nil {
		return nil, fmt.Errorf("loading private key: %w", err)
	}

	nonce := w.nextSpotSendNonce()
	amtStr := atomsToHYPE(value)
	chainName := networkChainName(w.net)
	chainID := networkChainID(w.net)

	action := sendAssetAction{
		Type:             "sendAsset",
		HyperliquidChain: chainName,
		SignatureChainID: fmt.Sprintf("0x%x", chainID),
		Destination:      address,
		SourceDex:        "spot",
		DestinationDex:   "spot",
		Token:            w.hypeTokenID,
		Amount:           amtStr,
		FromSubAccount:   "",
		Nonce:            nonce,
	}

	hash := sendAssetEIP712Hash(w.net, address, "spot", "spot", w.hypeTokenID, amtStr, "", nonce)
	sig, err := signHash(priv, hash)
	if err != nil {
		return nil, fmt.Errorf("signing action: %w", err)
	}

	// Use a nonce-based hex string as the pending tx ID. The tip poller
	// replaces it with the real hash from the ledger once confirmed.
	txID := fmt.Sprintf("0x%016x", nonce)
	recipient := address
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        txID,
		Amount:    value,
		Fees:      0,
		Recipient: &recipient,
		AdditionalData: map[string]string{
			"nonce": strconv.FormatUint(nonce, 10),
		},
	}

	// Store the pending record before broadcasting so a storeTx failure
	// after a successful send can never leave the transaction untracked.
	if w.txDB != nil {
		if err := setSubmissionTime(wtx, uint64(time.Now().Unix())); err != nil {
			return nil, fmt.Errorf("encoding submission time: %w", err)
		}
		if err := w.txDB.StoreTx(wtx); err != nil {
			return nil, fmt.Errorf("recording pending send: %w", err)
		}
	}

	if err := w.rpc.sendAsset(w.ctx, action, sig, nonce); err != nil {
		// Only remove the pending record on an explicit server rejection. For
		// transport errors (network drop, timeout) the tx may have landed, so
		// we leave the record for scanLedger to resolve.
		var rejection *exchangeRejection
		if errors.As(err, &rejection) {
			if w.txDB != nil {
				if delErr := w.txDB.RemoveTx(txID); delErr != nil {
					w.log.Errorf("Cleaning up pending tx %s after rejection: %v", txID, delErr)
				}
			}
			return nil, fmt.Errorf("spot send: %w", err)
		}
		return nil, fmt.Errorf("%w: spot send: %w", ErrSendOutcomeUnknown, err)
	}

	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
	}

	return &coin{txID: txID, value: value}, nil
}

func (w *HyperWallet) ValidateAddress(address string) bool {
	return evmAddressRE.MatchString(address)
}

func (w *HyperWallet) SyncStatus() (*asset.SyncStatus, error) {
	w.tipMtx.RLock()
	tipTime := w.tipTime
	w.tipMtx.RUnlock()

	synced := !tipTime.IsZero() && time.Since(tipTime) < time.Duration(MaxBlockInterval)*time.Second
	return &asset.SyncStatus{Synced: synced}, nil
}

func (w *HyperWallet) StandardSendFee(_ uint64) uint64 { return 0 }
func (w *HyperWallet) FeeRate() uint64                 { return 0 }

func (w *HyperWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if w.txDB == nil || req == nil {
		return &txdb.TxHistoryResponse{}, nil
	}
	if req.RefID != nil {
		wt, err := w.txDB.GetTx(*req.RefID)
		if err != nil {
			return nil, err
		}
		if wt == nil {
			return nil, asset.CoinNotFoundError
		}
		if !wt.Confirmed {
			return nil, ErrTxIsPending
		}
	}
	// NetworkTxDB.GetTxs indexes all transactions, including pending sends.
	// Accumulate confirmed-only entries across pages so that pending sends
	// never consume the N budget or leave MoreAvailable in an inconsistent
	// state. GetTxs uses an inclusive seek (RefID tx is the first result),
	// so internal page-advance iterations skip the cursor tx to avoid
	// duplicates. Pending sends are exposed separately via PendingTransactions.
	//
	// getTxs computes: reverse := req.Past || req.RefID == nil. For the default
	// request (Past=false, RefID=nil) reverse=true (newest-first). Internal
	// pages always carry a cursor, so capture the effective direction now so
	// those pages also walk newest-first.
	if req.N <= 0 {
		resp, err := w.txDB.GetTxs(req)
		if err != nil {
			return nil, err
		}
		all := resp.Txs[:0]
		for _, wt := range resp.Txs {
			if wt.Confirmed {
				all = append(all, wt)
			}
		}
		return &txdb.TxHistoryResponse{Txs: all}, nil
	}
	past := req.Past || req.RefID == nil
	confirmed := make([]*txdb.WalletTransaction, 0, req.N)
	cursor := req.RefID
	skipFirst := false
	for len(confirmed) < req.N {
		pageReq := &txdb.TxHistoryRequest{
			N:           req.N,
			RefID:       cursor,
			Past:        past,
			IgnoreTypes: req.IgnoreTypes,
		}
		resp, err := w.txDB.GetTxs(pageReq)
		if err != nil {
			return nil, err
		}
		anyProcessed := false
		for i, wt := range resp.Txs {
			if i == 0 && skipFirst {
				continue
			}
			anyProcessed = true
			if !wt.Confirmed {
				continue
			}
			confirmed = append(confirmed, wt)
			if len(confirmed) == req.N {
				more := i < len(resp.Txs)-1 || resp.MoreAvailable
				return &txdb.TxHistoryResponse{Txs: confirmed, MoreAvailable: more}, nil
			}
		}
		if !anyProcessed || !resp.MoreAvailable {
			break
		}
		last := resp.Txs[len(resp.Txs)-1]
		cursor = &last.ID
		skipFirst = true
	}
	return &txdb.TxHistoryResponse{Txs: confirmed}, nil
}

func (w *HyperWallet) WalletTransaction(_ context.Context, txID string) (*txdb.WalletTransaction, error) {
	if w.txDB != nil {
		for _, lookupID := range walletTransactionLookupIDs(txID) {
			wt, err := w.txDB.GetTx(lookupID)
			if err != nil {
				return nil, err
			}
			if wt != nil {
				return wt, nil
			}
		}
		for _, lookupID := range walletTransactionLookupIDs(txID) {
			wt, err := w.getTxByPendingID(lookupID)
			if err != nil {
				return nil, err
			}
			if wt != nil {
				return wt, nil
			}
		}
	}
	return nil, asset.CoinNotFoundError
}

func (w *HyperWallet) PendingTransactions(_ context.Context) []*txdb.WalletTransaction {
	if w.txDB == nil {
		return nil
	}
	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		w.log.Errorf("Getting pending txs: %v", err)
		return nil
	}
	return pending
}

func (w *HyperWallet) nextSpotSendNonce() uint64 {
	nonce := uint64(time.Now().UnixMilli())

	w.sendMtx.Lock()
	defer w.sendMtx.Unlock()

	if nonce <= w.nextSendNonce {
		nonce = w.nextSendNonce + 1
	}
	w.nextSendNonce = nonce
	return nonce
}

func (w *HyperWallet) Rescan(ctx context.Context, _ uint64) error {
	if w.rpc == nil {
		return asset.ErrConnectionDown
	}
	updates, err := w.rpc.ledgerUpdates(ctx, w.address, 0)
	if err != nil {
		return fmt.Errorf("fetching ledger updates: %w", err)
	}
	for _, u := range updates {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.processLedgerUpdate(u)
	}
	if err := w.advanceLastScannedMS(maxLedgerUpdateTime(updates) + 1); err != nil {
		return fmt.Errorf("persisting rescan watermark: %w", err)
	}
	return nil
}

// tipPoller maintains a WebSocket connection for real-time ledger events.
// On each (re)connect it first calls scanLedger to catch up on any events
// missed while offline, then enters the live message loop.
func (w *HyperWallet) tipPoller(ctx context.Context) {
	defer func() {
		if atomic.SwapUint32(&w.lastPeerCount, 0) != 0 && w.peersChange != nil {
			w.peersChange(0, nil)
		}
	}()

	var netDial func(context.Context, string, string) (net.Conn, error)
	if torProxy := w.settings["torproxy"]; torProxy != "" {
		d, err := proxy.SOCKS5("tcp", torProxy, nil, proxy.Direct)
		if err != nil {
			w.log.Errorf("WS proxy dialer: %v", err)
		} else {
			netDial = func(_ context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
		}
	}

	wsc := &wsClient{
		url:     w.wsEndpoint(),
		user:    w.address,
		log:     w.log,
		netDial: netDial,
		onEvent: w.processLedgerUpdate,
		onLive: func() {
			t := time.Now()
			w.tipMtx.Lock()
			w.tip = uint64(t.UnixMilli())
			w.tipTime = t
			tip := w.tip
			w.tipMtx.Unlock()
			if w.emit != nil {
				w.emit.TipChange(tip)
			}
			if atomic.SwapUint32(&w.lastPeerCount, 1) != 1 && w.peersChange != nil {
				w.peersChange(1, nil)
			}
		},
		onReconnect: w.scanLedger,
	}
	wsc.run(ctx)
}

func (w *HyperWallet) wsEndpoint() string {
	if ep, ok := w.settings["wsendpoint"]; ok && ep != "" {
		return ep
	}
	if ep, ok := DefaultWSEndpoints[w.net]; ok {
		return ep
	}
	return DefaultWSEndpoints[util.Mainnet]
}

// scanLedger fetches ledger updates since lastScannedMS and processes them.
// Returns an error if the API call fails; on success the caller should update
// the tip timestamp.
func (w *HyperWallet) scanLedger(ctx context.Context) error {
	// Snapshot the watermark before the HTTP call; do not hold the lock
	// across the network round-trip.
	w.tipMtx.RLock()
	since := w.lastScannedMS
	w.tipMtx.RUnlock()

	updates, err := w.rpc.ledgerUpdates(ctx, w.address, since)
	if err != nil {
		return err
	}

	for _, u := range updates {
		w.processLedgerUpdate(u)
	}

	if err := w.advanceLastScannedMS(maxLedgerUpdateTime(updates) + 1); err != nil {
		return fmt.Errorf("persisting scan watermark: %w", err)
	}
	return nil
}

func maxLedgerUpdateTime(updates []ledgerUpdate) int64 {
	var maxTime int64 = -1
	for _, u := range updates {
		if int64(u.Time) > maxTime {
			maxTime = int64(u.Time)
		}
	}
	return maxTime
}

func (w *HyperWallet) advanceLastScannedMS(next int64) error {
	if next <= 0 {
		return nil
	}

	w.scanMtx.Lock()
	defer w.scanMtx.Unlock()

	w.tipMtx.RLock()
	current := w.lastScannedMS
	w.tipMtx.RUnlock()
	if next <= current {
		return nil
	}

	if err := w.setLastScannedMS(next); err != nil {
		return err
	}

	w.tipMtx.Lock()
	if next > w.lastScannedMS {
		w.lastScannedMS = next
	}
	w.tipMtx.Unlock()
	return nil
}

// processLedgerUpdate handles a single ledger entry: confirms pending sent txs
// and stores newly detected incoming transfers. ledgerMtx serializes concurrent
// callers (Rescan and the WS goroutine) to prevent duplicate confirms/stores.
func (w *HyperWallet) processLedgerUpdate(u ledgerUpdate) {
	w.ledgerMtx.Lock()
	defer w.ledgerMtx.Unlock()

	if w.txDB == nil {
		return
	}

	if u.Delta.Token != "HYPE" {
		return
	}

	switch u.Delta.Type {
	case "send", "spotTransfer", "transfer", "deposit", "internalTransfer":
		w.handleTransferUpdate(u)
	}
}

func (w *HyperWallet) handleTransferUpdate(u ledgerUpdate) {
	amount, err := parseHYPE(u.Delta.Amount)
	if err != nil {
		w.log.Warnf("Parsing ledger amount %q: %v", u.Delta.Amount, err)
		return
	}

	isOutgoing := strings.EqualFold(u.Delta.User, w.address)
	isIncoming := strings.EqualFold(u.Delta.Destination, w.address)

	if isOutgoing {
		// Try to match and confirm a pending send tx by nonce proximity.
		w.confirmPendingSend(u, amount)
		return
	}

	if !isIncoming {
		return
	}

	// Incoming transfer not already in the DB.
	if existing, _ := w.txDB.GetTx(u.Hash); existing != nil {
		return
	}

	w.tipMtx.RLock()
	tip := w.tip
	w.tipMtx.RUnlock()

	addr := w.address
	wtx := &txdb.WalletTransaction{
		Type:        txdb.Receive,
		ID:          u.Hash,
		Amount:      amount,
		BlockNumber: tip,
		Timestamp:   u.Time / 1000,
		Confirmed:   true,
		Recipient:   &addr,
		AdditionalData: map[string]string{
			"time":   strconv.FormatUint(u.Time, 10),
			"sender": u.Delta.User,
		},
	}

	if err := setSubmissionTime(wtx, uint64(time.Now().Unix())); err != nil {
		w.log.Errorf("Encoding submission time for receive tx %s: %v", u.Hash, err)
		return
	}
	if err := w.txDB.StoreTx(wtx); err != nil {
		w.log.Errorf("Storing receive tx %s: %v", u.Hash, err)
		return
	}

	w.log.Infof("Incoming HYPE transfer %s: %s HYPE from %s", u.Hash, u.Delta.Amount, u.Delta.User)
	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
	}
}

// confirmPendingSend finds the pending send matching ledger update u and
// updates it with the confirmed state. Candidates are filtered by amount and
// recipient; when multiple sends share both, the one whose nonce is numerically
// closest to u.Time is chosen. Nonce proximity is a tiebreaker only — a single
// candidate always matches regardless of how much time elapsed between nonce
// mint and ledger echo.
// Pending entries older than pendingTTL are expired opportunistically.
func (w *HyperWallet) confirmPendingSend(u ledgerUpdate, amount uint64) {
	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		w.log.Errorf("Getting pending txs: %v", err)
		return
	}

	cutoff := uint64(time.Now().Unix()) - pendingTTL
	var best *txdb.WalletTransaction
	var bestAbsDiff int64
	for _, p := range pending {
		subTime := hyperData(p).SubmissionTime
		if subTime > 0 && subTime < cutoff {
			w.log.Infof("Expiring stale pending send %s (submitted %ds ago)",
				p.ID, uint64(time.Now().Unix())-subTime)
			if err := w.txDB.RemoveTx(p.ID); err != nil {
				w.log.Errorf("Deleting expired pending tx %s: %v", p.ID, err)
			}
			continue
		}

		nonceStr, ok := p.AdditionalData["nonce"]
		if !ok {
			continue
		}
		nonce, err := strconv.ParseUint(nonceStr, 10, 64)
		if err != nil {
			continue
		}
		if p.Amount != amount {
			continue
		}
		if p.Recipient == nil || !strings.EqualFold(*p.Recipient, u.Delta.Destination) {
			continue
		}
		absDiff := int64(u.Time) - int64(nonce)
		if absDiff < 0 {
			absDiff = -absDiff
		}
		if best == nil || absDiff < bestAbsDiff {
			best, bestAbsDiff = p, absDiff
		}
	}

	if best == nil {
		return
	}

	w.tipMtx.RLock()
	tip := w.tip
	w.tipMtx.RUnlock()

	oldID := best.ID

	// Build the confirmed record as a new struct rather than mutating the
	// pointer GetPendingTxs returned — guards against any future cache layer.
	addlData := make(map[string]string, len(best.AdditionalData)+2)
	for k, v := range best.AdditionalData {
		addlData[k] = v
	}
	addlData[pendingIDKey] = oldID
	addlData["txHash"] = u.Hash
	confirmed := &txdb.WalletTransaction{
		Type:           best.Type,
		ID:             u.Hash,
		Amount:         best.Amount,
		Fees:           best.Fees,
		Recipient:      best.Recipient,
		BlockNumber:    tip,
		Timestamp:      u.Time / 1000,
		Confirmed:      true,
		RawTx:          best.RawTx,
		AdditionalData: addlData,
	}

	// Store the confirmed entry before deleting the pending one so the tx
	// is never lost if the store fails.
	if err := w.txDB.StoreTx(confirmed); err != nil {
		w.log.Errorf("Storing confirmed send tx %s: %v", u.Hash, err)
		return
	}
	if err := w.txDB.RemoveTx(oldID); err != nil {
		w.log.Warnf("Removing stale pending tx %s: %v", oldID, err)
	}

	w.log.Infof("Send tx confirmed: %s (%s HYPE)", u.Hash, u.Delta.Amount)
	if w.emit != nil {
		w.emit.TransactionNote(confirmed, false)
	}
}

func walletTransactionLookupIDs(txID string) []string {
	ids := []string{txID}
	if pendingID, ok := pendingIDFromCoinString(txID); ok && pendingID != txID {
		ids = append(ids, pendingID)
	}
	return ids
}

func (w *HyperWallet) getLastScannedMS() (int64, error) {
	if w.metaTable == nil {
		return 0, nil
	}
	b, err := w.metaTable.Get([]byte(lastScannedMSKey))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(b) != 8 {
		return 0, fmt.Errorf("invalid lastScannedMS length %d", len(b))
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

func (w *HyperWallet) setLastScannedMS(ms int64) error {
	if w.metaTable == nil {
		return nil
	}
	if ms < 0 {
		return fmt.Errorf("negative lastScannedMS %d", ms)
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(ms))
	return w.metaTable.Set([]byte(lastScannedMSKey), b, true)
}

func (w *HyperWallet) highestPendingNonce() (uint64, error) {
	if w.txDB == nil {
		return 0, nil
	}
	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		return 0, err
	}
	var max uint64
	for _, wt := range pending {
		nonceStr := wt.AdditionalData["nonce"]
		if nonceStr == "" {
			continue
		}
		n, err := strconv.ParseUint(nonceStr, 10, 64)
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max, nil
}

func (w *HyperWallet) getTxByPendingID(pendingID string) (*txdb.WalletTransaction, error) {
	if w.pendingIDIndex == nil {
		return nil, nil
	}
	var found *txdb.WalletTransaction
	if err := w.pendingIDIndex.Iterate([]byte(pendingID), func(wt *txdb.WalletTransaction) error {
		found = wt
		return txdb.ErrEndIteration
	}, false, nil); err != nil {
		return nil, err
	}
	return found, nil
}

func (w *HyperWallet) apiEndpoint() string {
	if ep, ok := w.settings["apiendpoint"]; ok && ep != "" {
		return ep
	}
	if ep, ok := DefaultAPIEndpoints[w.net]; ok {
		return ep
	}
	return DefaultAPIEndpoints[util.Mainnet]
}

// privKeyFromSeed derives the secp256k1 private key and Ethereum address from
// a wallet seed.
func privKeyFromSeed(seed []byte) (*ecdsa.PrivateKey, string, func(), error) {
	extKey, err := keygen.GenDeepChild(seed, seedDerivationPath)
	if err != nil {
		return nil, "", nil, fmt.Errorf("deriving key: %w", err)
	}

	rawBytes, err := extKey.SerializedPrivKey()
	if err != nil {
		extKey.Zero()
		return nil, "", nil, fmt.Errorf("serializing private key: %w", err)
	}
	// Copy before zeroing: SerializedPrivKey returns a slice backed by extKey's
	// internal storage, so Zero() would corrupt the bytes in-place.
	privBytes := make([]byte, len(rawBytes))
	copy(privBytes, rawBytes)
	extKey.Zero()

	priv, err := gethcrypto.ToECDSA(privBytes)
	if err != nil {
		encode.ClearBytes(privBytes)
		return nil, "", nil, fmt.Errorf("parsing private key: %w", err)
	}

	addr := strings.ToLower(gethcrypto.PubkeyToAddress(priv.PublicKey).Hex())

	zero := func() {
		encode.ClearBytes(privBytes)
	}

	return priv, addr, zero, nil
}

// pubKeyToAddress derives the 0x-prefixed lowercase Ethereum address from a
// compressed secp256k1 public key (33 bytes).
func pubKeyToAddress(compressedPub []byte) string {
	pub, err := gethcrypto.DecompressPubkey(compressedPub)
	if err != nil {
		return ""
	}
	return strings.ToLower(gethcrypto.PubkeyToAddress(*pub).Hex())
}

// --- EIP-712 signing ---

// sendAssetTypeHashBytes is the keccak256 of the SendAsset EIP-712 type string.
var sendAssetTypeHashBytes = gethcrypto.Keccak256([]byte(
	"HyperliquidTransaction:SendAsset(string hyperliquidChain,string destination,string sourceDex,string destinationDex,string token,string amount,string fromSubAccount,uint64 nonce)",
))

// networkChainID returns the EIP-712 signing chain ID for network, falling
// back to mainnet for any network not explicitly in the map.
func networkChainID(network util.Network) uint64 {
	if id, ok := eip712ChainIDs[network]; ok {
		return id
	}
	return eip712ChainIDs[util.Mainnet]
}

// networkChainName returns the hyperliquidChain string for network, falling
// back to mainnet for any network not explicitly in the map.
func networkChainName(network util.Network) string {
	if name, ok := eip712ChainNames[network]; ok {
		return name
	}
	return eip712ChainNames[util.Mainnet]
}

// SendAssetEIP712Hash computes the EIP-712 hash for a SendAsset action. It is
// exported for use by the Hyperliquid mock server, which must verify
// signatures using the same hashing logic as the wallet.
func SendAssetEIP712Hash(network util.Network, destination, sourceDex, destinationDex, token, amount, fromSubAccount string, nonce uint64) []byte {
	return sendAssetEIP712Hash(network, destination, sourceDex, destinationDex, token, amount, fromSubAccount, nonce)
}

// sendAssetEIP712Hash computes the EIP-712 hash for a SendAsset action.
func sendAssetEIP712Hash(network util.Network, destination, sourceDex, destinationDex, token, amount, fromSubAccount string, nonce uint64) []byte {
	domain := eip712DomainHashFor(networkChainID(network))
	messageHash := sendAssetMessageHash(network, destination, sourceDex, destinationDex, token, amount, fromSubAccount, nonce)
	input := make([]byte, 0, 2+32+32)
	input = append(input, 0x19, 0x01)
	input = append(input, domain...)
	input = append(input, messageHash...)
	return gethcrypto.Keccak256(input)
}

func eip712DomainHashFor(chainID uint64) []byte {
	domainTypeHash := gethcrypto.Keccak256([]byte(
		"EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)",
	))

	chainIDBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(chainIDBytes[24:], chainID)

	encoded := make([]byte, 0, 5*32)
	encoded = append(encoded, domainTypeHash...)
	encoded = append(encoded, gethcrypto.Keccak256([]byte("HyperliquidSignTransaction"))...)
	encoded = append(encoded, gethcrypto.Keccak256([]byte("1"))...)
	encoded = append(encoded, chainIDBytes...)
	encoded = append(encoded, make([]byte, 32)...) // zero address

	return gethcrypto.Keccak256(encoded)
}

func sendAssetMessageHash(network util.Network, destination, sourceDex, destinationDex, token, amount, fromSubAccount string, nonce uint64) []byte {
	chainName := networkChainName(network)
	nonceBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(nonceBytes[24:], nonce)

	encoded := make([]byte, 0, 9*32)
	encoded = append(encoded, sendAssetTypeHashBytes...)
	encoded = append(encoded, gethcrypto.Keccak256([]byte(chainName))...)
	encoded = append(encoded, gethcrypto.Keccak256([]byte(destination))...)
	encoded = append(encoded, gethcrypto.Keccak256([]byte(sourceDex))...)
	encoded = append(encoded, gethcrypto.Keccak256([]byte(destinationDex))...)
	encoded = append(encoded, gethcrypto.Keccak256([]byte(token))...)
	encoded = append(encoded, gethcrypto.Keccak256([]byte(amount))...)
	encoded = append(encoded, gethcrypto.Keccak256([]byte(fromSubAccount))...)
	encoded = append(encoded, nonceBytes...)

	return gethcrypto.Keccak256(encoded)
}

// signHash signs a 32-byte hash with a secp256k1 private key.
// Returns a 65-byte compact signature [R(32)||S(32)||V(1)] with V = 27 or 28.
func signHash(privKey *ecdsa.PrivateKey, hash []byte) ([65]byte, error) {
	sig, err := gethcrypto.Sign(hash, privKey)
	if err != nil {
		return [65]byte{}, err
	}
	// go-ethereum returns V as 0 or 1; Hyperliquid expects 27 or 28.
	sig[64] += 27
	return [65]byte(sig), nil
}
