// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/txdb"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/util/keygen"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/keystore"
	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/decred/dcrd/hdkeychain/v3"
)

const (
	version = 0

	// tipPollInterval is how often the poll fallback queries serverInfo for the
	// latest validated ledger. This keeps the wallet alive when the websocket
	// subscription is unavailable. Must be less than MaxBlockInterval (30 s) so
	// SyncStatus stays true on a working RPC-only connection.
	tipPollInterval = 20 * time.Second

	// lastLedgerWindow is the number of ledgers ahead of the current open ledger
	// to set LastLedgerSequence. ~5 min at 4 s/ledger; rippled's recommended default.
	lastLedgerWindow = 75

	// lsfRequireDestTag is the AccountFlags bit indicating the account requires
	// a DestinationTag on all incoming payments.
	lsfRequireDestTag = 0x00020000

	// xrpBase58Alphabet is the custom base58 alphabet used by the XRP Ledger.
	// See https://xrpl.org/base58-encodings.html
	xrpBase58Alphabet = "rpshnaf39wBUDNEGHJKLM4PQRST7VWXYZ2bcdeCg65jkm8oFqi1tuvAxyz"

	// maxBase58InputLen caps decodeXRPBase58 input to prevent O(n²) big.Int ops.
	// XRP addresses are 25–35 chars; this provides generous slack.
	maxBase58InputLen = 64

	// XRPL binary serialization field headers.
	// See https://xrpl.org/serialization.html
	fieldHdrTransactionType = 0x12 // UInt16 field 2 — TransactionType
	fieldHdrFlags           = 0x22 // UInt32 field 2 — Flags
	fieldHdrSequence        = 0x24 // UInt32 field 4 — Sequence
	fieldHdrDestTag         = 0x2E // UInt32 field 14 — DestinationTag
	fieldHdrLastLedgerSeq0  = 0x20 // UInt32 type nibble — LastLedgerSequence (2-byte header)
	fieldHdrLastLedgerSeq1  = 0x1B // field 27 low byte — LastLedgerSequence
	fieldHdrAmount          = 0x61 // Amount field 1 — Amount
	fieldHdrFee             = 0x68 // Amount field 8 — Fee
	fieldHdrSigningPubKey   = 0x73 // Blob field 3 — SigningPubKey
	fieldHdrTxnSignature    = 0x74 // Blob field 4 — TxnSignature
	fieldHdrAccount         = 0x81 // AccountID field 1 — Account
	fieldHdrDestination     = 0x83 // AccountID field 3 — Destination
)

// xrpBase58AlphabetDecodeMap is a precomputed byte-indexed reverse-lookup for
// decoding. Values of -1 indicate the byte is not in the XRP base58 alphabet.
var xrpBase58AlphabetDecodeMap [256]int8

func init() {
	for i := range xrpBase58AlphabetDecodeMap {
		xrpBase58AlphabetDecodeMap[i] = -1
	}
	for i := 0; i < len(xrpBase58Alphabet); i++ {
		xrpBase58AlphabetDecodeMap[xrpBase58Alphabet[i]] = int8(i)
	}
}

var (
	// txSigningPrefix is prepended before hashing for ECDSA signing ("STX\0").
	txSigningPrefix = []byte{0x53, 0x54, 0x58, 0x00}

	// txHashPrefix is the XRPL transaction ID hash prefix ("TXN\0").
	txHashPrefix = []byte{0x54, 0x58, 0x4E, 0x00}

	rpcWalletDefinition = &asset.WalletDefinition{
		Seeded:      true,
		Type:        walletTypeRPC,
		Tab:         "Native",
		Description: "XRP Ledger wallet",
		ConfigOpts: []*asset.ConfigOption{{
			Key:         "rpcprovider",
			DisplayName: "RPC Provider",
			Description: "rippled HTTP RPC endpoint URL. Only connect to a trusted node; a malicious endpoint can report false balances and withhold or front-run transactions.",
		}, {
			Key:         "wsprovider",
			DisplayName: "WebSocket Provider",
			Description: "Optional rippled WebSocket endpoint URL for live ledger and account updates. Leave empty to use the network default or derive from the RPC endpoint.",
		}},
	}

	WalletInfo = asset.WalletInfo{
		Name:              "XRP",
		SupportedVersions: []uint32{version},
		UnitInfo:          UnitInfo,
		AvailableWallets:  []*asset.WalletDefinition{rpcWalletDefinition},
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
		},
	}

	// BIP44 derivation path: m/44'/144'/0'/0/0
	seedDerivationPath = []uint32{
		hdkeychain.HardenedKeyStart + 44,  // purpose 44'
		hdkeychain.HardenedKeyStart + 144, // XRP coin type 144'
		hdkeychain.HardenedKeyStart,       // account 0'
		0,                                 // external chain
		0,                                 // first address
	}
)

func init() {
	asset.Register(BipID, &Driver{})
	registerToken(usdcTokenID, "Circle's USDC issued currency on the XRP Ledger.", util.Mainnet)
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

func (d *Driver) Exists(walletType, dataDir string, settings map[string]string, net util.Network) (bool, error) {
	if walletType != walletTypeRPC {
		return false, fmt.Errorf("unknown wallet type %q", walletType)
	}
	keyFile := filepath.Join(dataDir, keyFileName)
	_, err := os.Stat(keyFile)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (d *Driver) Create(params *asset.CreateWalletParams) error {
	if params.Type != walletTypeRPC {
		return fmt.Errorf("unknown wallet type %q", params.Type)
	}
	if len(params.Seed) == 0 {
		return fmt.Errorf("wallet seed cannot be empty")
	}

	privKey, zero, err := privKeyFromSeed(params.Seed)
	if err != nil {
		return err
	}
	defer zero()

	if err := os.MkdirAll(params.DataDir, 0700); err != nil {
		return fmt.Errorf("error creating data directory: %w", err)
	}

	privKeyBytes := privKey.Serialize()
	defer encode.ClearBytes(privKeyBytes)
	pubKey := privKey.PubKey().SerializeCompressed()
	return keystore.Save(filepath.Join(params.DataDir, keyFileName), privKeyBytes, pubKey, params.Pass)
}

// xrpWallet implements asset.Wallet and asset.Authenticator for the XRP Ledger.
type xrpWallet struct {
	log         util.Logger
	net         util.Network
	emit        *asset.WalletEmitter
	peersChange func(uint32, error)
	dataDir     string
	settings    map[string]string

	keyMtx  sync.RWMutex
	privKey *btcec.PrivateKey // nil when locked; guarded by keyMtx
	pubKey  *btcec.PublicKey  // write-once in newWallet; safe to read without keyMtx
	address string            // classic 'r...' address; write-once in newWallet

	ctx       context.Context    // set in Connect; cancelled on shutdown
	ctxCancel context.CancelFunc // cancels ctx
	rpc       *rpcClient
	txDB      txdb.NetworkTxDB
	metaTable txdb.StoreTable // auxiliary table for lastScannedLedger

	tipMtx  sync.RWMutex
	tip     uint64
	tipTime time.Time

	seqMtx   sync.Mutex
	nextSeq  uint32   // 0 = uninitialized; advance locally after each submit
	freeSeqs []uint32 // sorted ascending; sequences reserved then rolled back, reused before minting new ones

	reserveMtx   sync.RWMutex
	baseReserve  uint64 // drops; from server_state, falls back to BaseReserveDrops
	ownerReserve uint64 // drops per owned object; from server_state

	lastScannedLedger atomic.Uint64
	scanMtx           sync.Mutex // prevents re-entrant scanAccountTxs

	tipCh chan<- uint64 // send-side of the tip worker channel; nil before Connect

	running atomic.Bool // true after Connect, false after shutdown
	opWG    sync.WaitGroup

	peersUp atomic.Bool // tracks RPC connectivity for peersChange callbacks
}

var _ asset.Wallet = (*xrpWallet)(nil)
var _ asset.Authenticator = (*xrpWallet)(nil)

func newWallet(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (*xrpWallet, error) {
	keyFile := filepath.Join(cfg.DataDir, keyFileName)
	pubKeyB, err := keystore.PubKey(keyFile)
	if err != nil {
		return nil, fmt.Errorf("error loading key file: %w", err)
	}

	pubKey, err := btcec.ParsePubKey(pubKeyB)
	if err != nil {
		return nil, fmt.Errorf("error parsing public key: %w", err)
	}

	address := pubKeyToAddress(pubKey)

	return &xrpWallet{
		log:          logger,
		net:          network,
		emit:         cfg.Emit,
		peersChange:  cfg.PeersChange,
		dataDir:      cfg.DataDir,
		settings:     cfg.Settings,
		pubKey:       pubKey,
		address:      address,
		baseReserve:  BaseReserveDrops,
		ownerReserve: OwnerReserveDrops,
		txDB:         cfg.NetworkTxDB,
	}, nil
}

// Unlock decrypts the private key using the wallet password.
func (w *xrpWallet) Unlock(pw []byte) error {
	keyFile := filepath.Join(w.dataDir, keyFileName)
	privKeyBytes, err := keystore.Open(keyFile, pw)
	if err != nil {
		return fmt.Errorf("error decrypting key: %w", err)
	}
	defer encode.ClearBytes(privKeyBytes)

	privKey, pubKey := btcec.PrivKeyFromBytes(privKeyBytes)

	// Verify decrypted key matches the stored public key.
	if !pubKey.IsEqual(w.pubKey) {
		privKey.Zero()
		return fmt.Errorf("decrypted key does not match stored public key")
	}

	w.keyMtx.Lock()
	w.privKey = privKey
	w.keyMtx.Unlock()
	return nil
}

// Lock zeros the private key.
func (w *xrpWallet) Lock() error {
	w.keyMtx.Lock()
	defer w.keyMtx.Unlock()
	if w.privKey != nil {
		w.privKey.Zero()
		w.privKey = nil
	}
	return nil
}

// Locked returns true if the private key is not loaded.
func (w *xrpWallet) Locked() bool {
	w.keyMtx.RLock()
	defer w.keyMtx.RUnlock()
	return w.privKey == nil
}

func (w *xrpWallet) Connect(ctx context.Context) (_ *sync.WaitGroup, err error) {
	if w.running.Load() {
		return nil, fmt.Errorf("wallet already connected")
	}
	w.ctx, w.ctxCancel = context.WithCancel(ctx)
	defer func() {
		if err != nil {
			w.ctxCancel()
		}
	}()
	endpoint := w.rpcEndpoint()
	rpc := newRPCClient(endpoint, w.log)
	w.rpc = rpc

	// The transaction database (w.txDB) is the master tx DB handle supplied by
	// Core via WalletConfig.NetworkTxDB; the wallet does not own its lifecycle.
	// The auxiliary meta table holds the persisted scan position.
	if w.txDB != nil {
		mt, err := w.txDB.Table(metaTableName)
		if err != nil {
			return nil, fmt.Errorf("error opening meta table: %w", err)
		}
		w.metaTable = mt
	}

	// Fetch initial chain state.
	si, err := w.rpc.serverInfo(w.ctx)
	if err != nil {
		return nil, fmt.Errorf("error fetching server info: %w", err)
	}

	w.tipMtx.Lock()
	w.tip = si.Info.ValidatedLedger.Seq
	w.tipTime = time.Now()
	w.tipMtx.Unlock()

	// Restore scan position from the last session.
	lastScanned, err := w.getLastScannedLedger()
	if err != nil {
		w.log.Warnf("Could not load lastScannedLedger: %v", err)
	}
	w.lastScannedLedger.Store(lastScanned)

	w.fetchReserves(w.ctx)

	ch := make(chan uint64, 2)
	w.tipCh = ch

	w.running.Store(true)

	// bgWg tracks the three background goroutines that write to txDB. The
	// returned wg completes once they have exited and in-flight ops have
	// drained, so the wallet is quiescent before Core closes the master tx DB.
	var bgWg sync.WaitGroup
	bgWg.Add(3)
	go func() {
		defer bgWg.Done()
		w.tipWorker(w.ctx, ch)
	}()
	go func() {
		defer bgWg.Done()
		w.subscriptionLoop(w.ctx)
	}()
	go func() {
		defer bgWg.Done()
		w.pollTips(w.ctx)
	}()

	// Ordered shutdown: wait for background goroutines to exit first (they
	// write to txDB), then drain externally-initiated ops.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-w.ctx.Done()
		bgWg.Wait()
		w.shutdown()
	}()

	return &wg, nil
}

// shutdown stops new public calls and waits for in-flight ops to drain. The
// transaction database is owned by Core, which closes it only after all wallets
// have disconnected, so the wallet does not close it here.
func (w *xrpWallet) shutdown() {
	w.ctxCancel()
	w.running.Store(false)
	w.opWG.Wait()
}

// tipWorker runs the initial account scan on startup, then processes ledger
// tip events from the ws subscription and poll fallback. Tip events are
// coalesced: if multiple tips queue while a scan is running, only the highest
// is processed. This keeps blocking RPC work off the websocket read goroutine.
func (w *xrpWallet) tipWorker(ctx context.Context, tipCh <-chan uint64) {
	w.tipMtx.RLock()
	initialTip := w.tip
	w.tipMtx.RUnlock()
	// Emit immediately so the UI shows the current balance before the scan starts.
	// Re-emit after the scan only if new txs were found.
	w.emitBalanceChange()
	if initialTip > 0 {
		if w.scanAccountTxs(ctx, initialTip) {
			w.emitBalanceChange()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case tip := <-tipCh:
		drain:
			for {
				select {
				case newer := <-tipCh:
					if newer > tip {
						tip = newer
					}
				default:
					break drain
				}
			}
			w.handleValidatedTip(ctx, tip)
		}
	}
}

// pollTips periodically fetches the latest validated ledger via serverInfo and
// forwards the sequence to tipCh. This is the fallback path for when the
// websocket subscription is unavailable; the tip worker coalesces redundant
// tips so the poller and the ws subscription can both run without conflict.
func (w *xrpWallet) pollTips(ctx context.Context) {
	ticker := time.NewTicker(tipPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			si, err := w.rpc.serverInfo(ctx)
			if err != nil {
				w.log.Warnf("XRP tip poll error: %v", err)
				continue
			}
			seq := si.Info.ValidatedLedger.Seq
			if seq == 0 {
				continue
			}
			select {
			case w.tipCh <- seq:
			default:
			}
		}
	}
}

func (w *xrpWallet) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

func (w *xrpWallet) Balance() (*asset.Balance, error) {
	if w.rpc == nil {
		return nil, fmt.Errorf("wallet not connected")
	}
	w.opWG.Add(1)
	defer w.opWG.Done()
	if !w.running.Load() {
		return nil, fmt.Errorf("wallet not running")
	}

	// Fetch validated and current ledger account state concurrently.
	// Validated gives confirmed balance; current accounts for pending outgoing
	// sends. Both calls are independent so parallelising halves RPC latency.
	type infoResult struct {
		info *accountInfoResult
		err  error
	}
	validatedCh := make(chan infoResult, 1)
	currentCh := make(chan infoResult, 1)
	go func() {
		info, err := w.rpc.accountInfo(w.ctx, w.address)
		validatedCh <- infoResult{info, err}
	}()
	go func() {
		info, err := w.rpc.accountInfoCurrent(w.ctx, w.address)
		currentCh <- infoResult{info, err}
	}()

	vr := <-validatedCh
	if vr.err != nil {
		<-currentCh // drain before returning
		if isAccountNotFound(vr.err) {
			return &asset.Balance{}, nil
		}
		return nil, fmt.Errorf("error querying account: %w", vr.err)
	}
	validated := vr.info

	cr := <-currentCh
	current := validated // fallback: use validated for both
	if cr.err == nil {
		current = cr.info
	}

	validatedBalance, err := strconv.ParseUint(validated.AccountData.Balance, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("error parsing balance %q: %w", validated.AccountData.Balance, err)
	}
	currentBalance, _ := strconv.ParseUint(current.AccountData.Balance, 10, 64)
	if currentBalance == 0 {
		currentBalance = validatedBalance
	}

	w.reserveMtx.RLock()
	baseReserve := w.baseReserve
	ownerReservePerItem := w.ownerReserve
	w.reserveMtx.RUnlock()

	reserve := baseReserve + ownerReservePerItem*uint64(validated.AccountData.OwnerCount)

	var available uint64
	if currentBalance > reserve {
		available = currentBalance - reserve
	}

	return &asset.Balance{
		Available: available,
		Locked:    reserve,
	}, nil
}

func (w *xrpWallet) DepositAddress() (string, error) {
	return w.address, nil
}

func (w *xrpWallet) OwnsDepositAddress(address string) (bool, error) {
	return address == w.address, nil
}

// reserveSequence reserves an account sequence number, querying the network
// to initialize it on first use. It returns the reserved sequence and a
// rollback function that returns it if the transaction is known to have
// never been submitted (or was deterministically rejected).
//
// A rolled-back sequence is added to freeSeqs and handed out again before any
// new sequence is minted, so a rollback that races with a concurrent
// reservation (which will have already advanced nextSeq past it) doesn't
// strand the sequence forever — XRPL requires an account's sequences to be
// consumed in order, so an un-reused gap would block every later send until
// reconciled.
func (w *xrpWallet) reserveSequence() (sequence uint32, rollback func(), err error) {
	// Fetch from the network before taking the lock so seqMtx is not held
	// across a blocking RPC call.
	var firstSendInfo *accountInfoResult
	w.seqMtx.Lock()
	if w.nextSeq == 0 {
		w.seqMtx.Unlock()
		firstSendInfo, err = w.rpc.accountInfoCurrent(w.ctx, w.address)
		if err != nil {
			return 0, nil, fmt.Errorf("error querying account info: %w", err)
		}
		w.seqMtx.Lock()
	}
	// Re-check after re-acquiring the lock; a concurrent send may have
	// initialized nextSeq while we were fetching.
	if w.nextSeq == 0 {
		w.nextSeq = nextAccountSequence(firstSendInfo)
	}
	// Drop any freeSeqs entries a stale, cross-frame rollback planted at or
	// beyond the current mint frame (see the rollback closure below); a
	// valid entry was always minted as sequence < nextSeq, so anything
	// outside that bound cannot be safely reused without risking
	// tefPAST_SEQ. This is a cheap, lock-only backstop for the ceiling side
	// of the invariant; the floor side (already-consumed sequences) is
	// purged against live account state in reconcileSeq.
	w.freeSeqs = slices.DeleteFunc(w.freeSeqs, func(seq uint32) bool { return seq >= w.nextSeq })

	// Reuse a rolled-back sequence before minting a new one, so concurrent
	// sends each still get a unique value without holding the lock across I/O.
	if len(w.freeSeqs) > 0 {
		sequence = w.freeSeqs[0]
		w.freeSeqs = w.freeSeqs[1:]
	} else {
		sequence = w.nextSeq
		w.nextSeq++
	}
	w.seqMtx.Unlock()

	rollback = func() {
		w.seqMtx.Lock()
		i, found := slices.BinarySearch(w.freeSeqs, sequence)
		if !found {
			w.freeSeqs = slices.Insert(w.freeSeqs, i, sequence)
		}
		w.seqMtx.Unlock()
	}
	return sequence, rollback, nil
}

func (w *xrpWallet) Send(address string, value, _ uint64) (asset.Coin, error) {
	w.opWG.Add(1)
	defer w.opWG.Done()
	if !w.running.Load() {
		return nil, fmt.Errorf("wallet not running")
	}

	// X-addresses (starting with 'X' on mainnet or 'T' on testnet) encode a
	// destination tag inline, but this wallet does not yet implement the
	// X-address codec. Users should provide a classic r... address.
	if len(address) > 0 && (address[0] == 'X' || address[0] == 'T') {
		return nil, fmt.Errorf("x-addresses are not supported; provide a classic r... address")
	}
	if !w.ValidateAddress(address) {
		return nil, fmt.Errorf("invalid XRP address %q", address)
	}
	if value == 0 {
		return nil, fmt.Errorf("cannot send zero value")
	}
	if w.rpc == nil {
		return nil, fmt.Errorf("wallet not connected")
	}

	// Check whether the wallet is locked before we do any network I/O or
	// reserve a sequence number. This avoids burning a sequence when the common
	// locked-wallet error is hit. A concurrent lock between this check and the
	// actual signing is handled by the re-check inside keyMtx below.
	w.keyMtx.RLock()
	if w.privKey == nil {
		w.keyMtx.RUnlock()
		return nil, fmt.Errorf("wallet is locked")
	}
	w.keyMtx.RUnlock()

	// Check whether the destination requires a DestinationTag. A payment
	// without one would be rejected on-chain with tecDST_TAG_NEEDED.
	// An actNotFound error means the destination account has never been funded;
	// enforce the base-reserve minimum to avoid a fee-burning tecNO_DST_INSUF_XRP.
	destInfo, err := w.rpc.accountInfo(w.ctx, address)
	if err != nil && !isAccountNotFound(err) {
		return nil, fmt.Errorf("error checking destination account info: %w", err)
	}
	if err == nil && destInfo.AccountData.Flags&lsfRequireDestTag != 0 {
		return nil, fmt.Errorf("destination %q requires a destination tag, which this wallet does not yet support", address)
	}
	if isAccountNotFound(err) {
		w.reserveMtx.RLock()
		minActivation := w.baseReserve
		w.reserveMtx.RUnlock()
		if value < minActivation {
			return nil, fmt.Errorf("destination is unfunded: sending %d drops but at least %d drops (base reserve) required to activate the account", value, minActivation)
		}
	}

	feeInfo, err := w.rpc.fee(w.ctx)
	if err != nil {
		return nil, fmt.Errorf("error querying fee: %w", err)
	}
	fee := feeInfo.feeDrops()
	if fee == 0 {
		w.log.Warnf("fee RPC returned unparseable open_ledger_fee=%q; using default %d drops",
			feeInfo.Drops.OpenLedgerFee, DefaultFeeDrops)
		fee = DefaultFeeDrops
	}
	lastLedgerSeq := uint32(feeInfo.LedgerCurrentIndex + lastLedgerWindow)

	sequence, rollbackSeq, err := w.reserveSequence()
	if err != nil {
		return nil, err
	}

	// addressToAccountID calls below are effectively unreachable (the address
	// was already validated at entry and w.address is set at wallet creation),
	// but we roll back defensively since the sequence has been reserved.
	destAccountID, err := addressToAccountID(address)
	if err != nil {
		rollbackSeq()
		return nil, fmt.Errorf("error decoding destination address: %w", err)
	}
	srcAccountID, err := addressToAccountID(w.address)
	if err != nil {
		rollbackSeq()
		return nil, fmt.Errorf("error decoding source address: %w", err)
	}

	p := &xrpPayment{
		account:       [20]byte(srcAccountID),
		destination:   [20]byte(destAccountID),
		amount:        value,
		fee:           fee,
		sequence:      sequence,
		lastLedgerSeq: lastLedgerSeq,
		signingPubKey: w.pubKey.SerializeCompressed(),
	}

	w.keyMtx.RLock()
	if w.privKey == nil {
		// Wallet was locked concurrently between the pre-check and signing.
		w.keyMtx.RUnlock()
		rollbackSeq()
		return nil, fmt.Errorf("wallet is locked")
	}
	sig, err := signPayment(p, w.privKey)
	w.keyMtx.RUnlock()
	if err != nil {
		rollbackSeq()
		return nil, fmt.Errorf("error signing transaction: %w", err)
	}
	p.txnSig = sig

	rawTxBytes := encodePayment(p, false)
	txBlob := strings.ToUpper(hex.EncodeToString(rawTxBytes))
	txHash := computeTxHash(rawTxBytes)
	txID := strings.ToUpper(hex.EncodeToString(txHash[:]))

	recipient := address
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        txID,
		Amount:    value,
		Fees:      fee,
		Recipient: &recipient,
	}
	// Carry the XRP-specific LastLedgerSequence in RawTx so checkPendingTxs can
	// detect expiry.
	setLastLedgerSequence(wtx, lastLedgerSeq)

	// Persist before submit so the tx is tracked even if submit has a network
	// error. checkPendingTxs resolves the outcome on the next tip.
	if w.txDB != nil {
		if err := w.txDB.StoreTx(wtx); err != nil {
			w.log.Errorf("Error pre-storing send tx: %v", err)
		}
	}

	sr, err := w.rpc.submit(w.ctx, txBlob)
	if err != nil {
		// Network error: the node may have received the signed blob. Do NOT
		// roll back the sequence — reusing it would cause tefPAST_SEQ if the
		// node did process the tx. The pre-stored entry will be resolved by
		// checkPendingTxs (confirmed or expired via LastLedgerSequence).
		w.log.Warnf("Submit network error (tx %s outcome unknown): %v", txID, err)
		return nil, fmt.Errorf("error submitting transaction: %w", err)
	}

	// XRPL result classes:
	//   tes* — tentative success; sequence consumed
	//   tec* — tx included in ledger but action failed; fee and sequence consumed
	//   ter* — provisional (e.g. terQUEUED); sequence will be consumed on inclusion
	//   tef* / tem* / tel* — rejected without consuming sequence
	class := ""
	if len(sr.EngineResult) >= 3 {
		class = sr.EngineResult[:3]
	}
	switch class {
	case "tes", "tec", "ter":
		if class != "tes" {
			w.log.Warnf("Submit result %s (%s): tracking as pending", sr.EngineResult, sr.EngineResultMessage)
		}
	default:
		// Deterministic rejection: sequence was not consumed. This is terminal,
		// so mark Confirmed (to leave the pending set) and Rejected.
		rollbackSeq()
		wtx.Confirmed = true
		wtx.Rejected = true
		if w.txDB != nil {
			if err := w.txDB.StoreTx(wtx); err != nil {
				w.log.Errorf("Error updating rejected tx %s: %v", txID, err)
			}
		}
		return nil, fmt.Errorf("transaction rejected: %s — %s", sr.EngineResult, sr.EngineResultMessage)
	}

	// Sanity-check: verify the server's reported hash matches our locally
	// computed hash. A mismatch indicates a bug in our encoding.
	if sr.TxJSON.Hash != "" {
		hashBytes, err := hex.DecodeString(sr.TxJSON.Hash)
		if err != nil || len(hashBytes) != 32 {
			w.log.Warnf("server returned invalid tx hash %q", sr.TxJSON.Hash)
		} else if !bytes.Equal(hashBytes, txHash[:]) {
			w.log.Warnf("server hash %s != computed hash %s; using computed", sr.TxJSON.Hash, txID)
		}
	}

	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
	}

	return &coin{txHash: txHash, value: value}, nil
}

func (w *xrpWallet) ValidateAddress(address string) bool {
	_, err := addressToAccountID(address)
	return err == nil
}

func (w *xrpWallet) SyncStatus() (*asset.SyncStatus, error) {
	w.tipMtx.RLock()
	tip := w.tip
	tipTime := w.tipTime
	w.tipMtx.RUnlock()

	synced := time.Since(tipTime) < time.Duration(MaxBlockInterval)*time.Second

	return &asset.SyncStatus{
		Synced: synced,
		Blocks: tip,
	}, nil
}

func (w *xrpWallet) StandardSendFee(_ uint64) uint64 {
	if w.rpc == nil {
		return DefaultFeeDrops
	}
	fr, err := w.rpc.fee(w.ctx)
	if err != nil {
		return DefaultFeeDrops
	}
	fee := fr.feeDrops()
	if fee == 0 {
		return DefaultFeeDrops
	}
	return fee
}

// FeeRate satisfies asset.Wallet, returning the current open-ledger fee in
// drops (falling back to the default when the live fee is unavailable).
func (w *xrpWallet) FeeRate() uint64 {
	return w.StandardSendFee(0)
}

func (w *xrpWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	w.opWG.Add(1)
	defer w.opWG.Done()
	if !w.running.Load() {
		return &txdb.TxHistoryResponse{}, nil
	}
	if w.txDB == nil || req == nil {
		return &txdb.TxHistoryResponse{}, nil
	}
	r := *req
	if r.N == 0 {
		r.N = 50
	}
	// nil selects this network's native-asset records, excluding any token
	// wallet's transactions sharing this NetworkTxDB.
	return w.txDB.GetAssetTxs(nil, &r)
}

func (w *xrpWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	if w.rpc == nil {
		return nil, fmt.Errorf("wallet not connected")
	}
	w.opWG.Add(1)
	defer w.opWG.Done()
	if !w.running.Load() {
		return nil, fmt.Errorf("wallet not running")
	}

	txID = strings.ToUpper(txID) // normalize hash case before DB lookup

	if w.txDB != nil {
		wt, err := w.txDB.GetTx(txID)
		if err != nil {
			return nil, err
		}
		if wt != nil {
			return wt, nil
		}
	}

	tr, err := w.rpc.tx(ctx, txID)
	if err != nil {
		if isTxNotFound(err) {
			return nil, asset.CoinNotFoundError
		}
		return nil, fmt.Errorf("error looking up transaction %s: %w", txID, err)
	}
	wt := walletTransactionFromTx(tr, w.address, tr.Meta, tr.Validated, tr.LedgerIndex)
	if wt == nil {
		return nil, asset.CoinNotFoundError
	}
	return wt, nil
}

func (w *xrpWallet) PendingTransactions(_ context.Context) []*txdb.WalletTransaction {
	w.opWG.Add(1)
	defer w.opWG.Done()
	if !w.running.Load() {
		return nil
	}
	if w.txDB == nil {
		return nil
	}
	pending, err := w.txDB.GetAssetPendingTxs(nil)
	if err != nil {
		w.log.Errorf("Error getting pending txs: %v", err)
		return nil
	}
	return pending
}

// checkPendingTxs is deliberately unfiltered (every asset sharing this
// NetworkTxDB, not just the native one): a token wallet's TrustSet/Payment
// consumes this account's sequence space too, so this loop must see and
// resolve token pending txs for sequence accounting to stay correct (see
// reserveSequence/reconcileSeq). This is safe because it never re-derives a
// tx's type/amount from the wallet's own address — it only updates
// BlockNumber/Timestamp/Confirmed/Rejected via meta looked up by the tx's
// own ID.
func (w *xrpWallet) checkPendingTxs(ctx context.Context) (balanceChanged bool) {
	if w.txDB == nil {
		return
	}
	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		w.log.Errorf("Error getting pending txs: %v", err)
		return
	}
	w.tipMtx.RLock()
	tip := w.tip
	w.tipMtx.RUnlock()

	for _, tx := range pending {
		lls := xrpData(tx).LastLedgerSequence
		tr, err := w.rpc.tx(ctx, tx.ID)
		if err != nil {
			if isTxNotFound(err) && lls > 0 && uint64(lls) < tip {
				// Expired: never included before its LastLedgerSequence. This is
				// terminal, so mark Confirmed (to leave the pending set) and
				// Rejected.
				tx.Confirmed = true
				tx.Rejected = true

				// H2: only reset nextSeq if no other pending txs remain.
				// Counting remaining (still-pending) entries prevents a blanket
				// reset that would race with concurrent Sends.
				remaining := 0
				for _, p := range pending {
					if p.ID != tx.ID && p.BlockNumber == 0 && !p.Confirmed {
						remaining++
					}
				}
				if remaining == 0 {
					w.seqMtx.Lock()
					w.nextSeq = 0
					w.freeSeqs = nil
					w.seqMtx.Unlock()
				}

				if storeErr := w.txDB.StoreTx(tx); storeErr != nil {
					w.log.Errorf("Error marking expired tx %s: %v", tx.ID, storeErr)
				} else {
					balanceChanged = true
					w.log.Warnf("Transaction %s expired: LLS %d < tip %d", tx.ID, lls, tip)
					if w.emit != nil {
						w.emit.TransactionNote(tx, false)
					}
				}
			} else {
				w.log.Tracef("Pending tx %s not yet confirmed: %v", tx.ID, err)
			}
			continue
		}
		if !tr.Validated {
			continue
		}
		tx.BlockNumber = tr.LedgerIndex
		tx.Confirmed = true
		if ct := tr.closeTime(); ct > 0 {
			tx.Timestamp = ct
		}
		if strings.HasPrefix(tr.Meta.TransactionResult, "tec") {
			tx.Rejected = true
		}
		if err := w.txDB.StoreTx(tx); err != nil {
			w.log.Errorf("Error updating confirmed tx %s: %v", tx.ID, err)
			continue
		}
		balanceChanged = true
		w.log.Infof("Transaction %s confirmed at ledger %d (result: %s)", tx.ID, tr.LedgerIndex, tr.Meta.TransactionResult)
		if w.emit != nil {
			w.emit.TransactionNote(tx, false)
		}
	}
	return
}

// nextAccountSequence derives the next safe sender sequence from the current
// open-ledger account state plus queued transactions. If the queue already
// starts at the validated Sequence, new sends must append after the highest
// queued sequence. If there is a gap, the next send must reuse the current
// validated Sequence to fill it so the queue can make progress.
func nextAccountSequence(info *accountInfoResult) uint32 {
	if info == nil {
		return 0
	}
	next := info.AccountData.Sequence
	queue := info.QueueData
	if queue == nil || queue.TxnCount == 0 {
		return next
	}
	if queue.LowestSequence == next && queue.HighestSequence >= next {
		if queue.HighestSequence == ^uint32(0) {
			return next // overflow guard: sequence space exhausted
		}
		return queue.HighestSequence + 1
	}
	return next
}

// fetchReserves queries server_state for the current base and per-owner
// reserve amounts and caches them. Falls back silently to the compiled-in
// constants if the call fails.
func (w *xrpWallet) fetchReserves(ctx context.Context) {
	ss, err := w.rpc.serverState(ctx)
	if err != nil {
		w.log.Warnf("Error fetching reserves from server_state: %v", err)
		return
	}
	b := ss.State.ValidatedLedger.ReserveBase
	inc := ss.State.ValidatedLedger.ReserveInc
	if b == 0 || inc == 0 {
		return
	}
	w.reserveMtx.Lock()
	w.baseReserve = b
	w.ownerReserve = inc
	w.reserveMtx.Unlock()
}

// reconcileSeq reads current open-ledger account state and updates nextSeq to
// a queue-aware safe value. This lets the wallet append after contiguous local
// queue state or refill a gap when an earlier transaction expired.
func (w *xrpWallet) reconcileSeq(ctx context.Context) {
	info, err := w.rpc.accountInfoCurrent(ctx, w.address)
	if err != nil {
		if !isAccountNotFound(err) {
			w.log.Warnf("Error reconciling sequence: %v", err)
		}
		return
	}
	next := nextAccountSequence(info)
	w.seqMtx.Lock()
	if w.nextSeq == 0 || next > w.nextSeq {
		w.nextSeq = next
		// Any tracked rollbacks are for sequences now behind the resynced
		// truth; they no longer mean anything.
		w.freeSeqs = nil
	} else {
		// A rollback can race a concurrent reset of nextSeq (e.g.
		// checkPendingTxs zeroing it when it can't see an in-flight
		// reservation that hasn't been stored yet) and later re-insert a
		// sequence the chain has since actually consumed. Such an entry is
		// invisible to the nextSeq-only check above whenever next <=
		// nextSeq, so purge anything below the account's live next
		// sequence here — it's the only place with a ground-truth floor.
		w.freeSeqs = slices.DeleteFunc(w.freeSeqs, func(seq uint32) bool { return seq < next })
	}
	w.seqMtx.Unlock()
}

const maxScanPages = 1000 // safety cap; a duplicate or runaway marker stops the loop

func (w *xrpWallet) scanAccountTxs(ctx context.Context, newTip uint64) (balanceChanged bool) {
	w.scanMtx.Lock()
	defer w.scanMtx.Unlock()

	start := w.lastScannedLedger.Load()
	if w.txDB == nil || newTip <= start {
		return
	}

	minLedger := start + 1
	maxLedger := newTip

	var (
		marker     json.RawMessage
		prevMarker json.RawMessage
	)

	for page := 0; ; page++ {
		// H3: respect context cancellation inside long backfills.
		select {
		case <-ctx.Done():
			return balanceChanged
		default:
		}

		// M2: guard against a server echoing the same marker indefinitely.
		if page >= maxScanPages {
			w.log.Errorf("account_tx exceeded %d pages; scan aborted", maxScanPages)
			return balanceChanged
		}

		atr, err := w.rpc.accountTx(ctx, w.address, minLedger, maxLedger, marker)
		if err != nil {
			if isAccountNotFound(err) {
				// No account yet; treat entire range as scanned.
				w.advanceLastScanned(newTip)
			} else {
				w.log.Warnf("Error scanning account txs: %v", err)
			}
			return balanceChanged
		}

		// H3: per-page checkpointing — only advance past ledgers where every
		// storeTx call succeeded.
		pageHigh := uint64(0)
		pageAllOK := true

		for i := range atr.Transactions {
			entry := &atr.Transactions[i]
			tr := &entry.Tx
			txID := strings.ToUpper(tr.Hash)

			// M10: handle getTx errors instead of silently discarding them.
			existing, err := w.txDB.GetTx(txID)
			if err != nil {
				w.log.Errorf("getTx %s: %v", txID, err)
				pageAllOK = false
				continue
			}
			if existing != nil {
				ledgerIdx := entry.LedgerIndex
				if ledgerIdx == 0 {
					ledgerIdx = tr.LedgerIndex
				}
				if ledgerIdx > pageHigh {
					pageHigh = ledgerIdx
				}
				continue
			}

			ledgerIdx := entry.LedgerIndex
			if ledgerIdx == 0 {
				ledgerIdx = tr.LedgerIndex
			}

			wtx := walletTransactionFromTx(tr, w.address, entry.Meta, entry.Validated, ledgerIdx)
			if wtx == nil {
				// Non-payment tx: not stored but still marks progress.
				if ledgerIdx > pageHigh {
					pageHigh = ledgerIdx
				}
				continue
			}

			if err := w.storeNewTx(wtx, tr); err != nil {
				w.log.Errorf("Error storing tx %s: %v", txID, err)
				pageAllOK = false
				continue
			}
			balanceChanged = true

			if ledgerIdx > pageHigh {
				pageHigh = ledgerIdx
			}

			if wtx.Type == txdb.Receive {
				w.log.Infof("Detected incoming transfer %s: %d drops from %s", txID, wtx.Amount, tr.Account)
			} else {
				recipient := ""
				if wtx.Recipient != nil {
					recipient = *wtx.Recipient
				}
				w.log.Infof("Detected historical send %s: %d drops to %s", txID, wtx.Amount, recipient)
			}
			if w.emit != nil {
				w.emit.TransactionNote(wtx, true)
			}
		}

		// H3: advance the persisted checkpoint after each page where all
		// stores succeeded. Use pageHigh-1 so the next scan re-includes
		// pageHigh in case more txs land in that ledger before the next tip.
		if pageAllOK && pageHigh > 0 {
			checkpoint := pageHigh - 1
			if checkpoint > w.lastScannedLedger.Load() {
				if err := w.setLastScannedLedger(checkpoint); err != nil {
					w.log.Warnf("Error persisting scan checkpoint: %v", err)
				} else {
					w.lastScannedLedger.Store(checkpoint)
				}
			}
		}

		if atr.Marker == nil {
			// Final page: advance to the full tip.
			w.advanceLastScanned(newTip)
			return balanceChanged
		}

		// M2: duplicate-marker guard — a stuck server would loop forever.
		if prevMarker != nil && bytes.Equal(atr.Marker, prevMarker) {
			w.log.Errorf("account_tx returned duplicate marker; scan aborted")
			return balanceChanged
		}
		marker = atr.Marker
		prevMarker = marker
	}
}

func (w *xrpWallet) handleValidatedTip(ctx context.Context, newSeq uint64) {
	w.tipMtx.Lock()
	newTip := newSeq > w.tip
	if newTip {
		w.tip = newSeq
		// Use local time when we observe the tip — monotonic, avoids
		// server-reported age flipping Synced=false on a healthy network.
		w.tipTime = time.Now()
	}
	w.tipMtx.Unlock()
	if !newTip {
		return
	}

	if w.emit != nil {
		w.emit.TipChange(newSeq)
	}
	pendingChanged := w.checkPendingTxs(ctx)
	w.reconcileSeq(ctx)
	// Emit before the scan so confirmed/expired sends appear in the UI
	// without waiting for the (potentially slow) account_tx scan to finish.
	if pendingChanged {
		w.emitBalanceChange()
	}
	if w.scanAccountTxs(ctx, newSeq) {
		w.emitBalanceChange()
	}
}

func (w *xrpWallet) emitBalanceChange() {
	if w.emit == nil || !w.running.Load() {
		return
	}
	bal, err := w.Balance()
	if err != nil {
		w.log.Warnf("Error emitting balance change: %v", err)
		return
	}
	w.emit.BalanceChange(bal)
}

// storeNewTx persists wtx, setting its Timestamp from tr's close time and
// falling back to the current wall clock when close_time_iso is absent.
func (w *xrpWallet) storeNewTx(wtx *txdb.WalletTransaction, tr *txResult) error {
	ts := tr.closeTime()
	if ts == 0 {
		ts = uint64(time.Now().Unix())
	}
	wtx.Timestamp = ts
	return w.txDB.StoreTx(wtx)
}

// advanceLastScanned persists and updates lastScannedLedger to seq if seq is
// greater than the current value.
func (w *xrpWallet) advanceLastScanned(seq uint64) {
	if seq <= w.lastScannedLedger.Load() {
		return
	}
	if err := w.setLastScannedLedger(seq); err != nil {
		w.log.Warnf("Error persisting lastScannedLedger: %v", err)
		return
	}
	w.lastScannedLedger.Store(seq)
}

func walletTransactionFromTx(tr *txResult, walletAddr string, meta txMeta, validated bool, ledgerIndex uint64) *txdb.WalletTransaction {
	if tr == nil || tr.TransactionType != "Payment" {
		return nil
	}

	isSend := tr.Account == walletAddr
	isReceive := tr.Destination == walletAddr && tr.Account != walletAddr
	if !isSend && !isReceive {
		return nil
	}

	rejected := strings.HasPrefix(meta.TransactionResult, "tec")
	feeParsed, _ := strconv.ParseUint(tr.Fee, 10, 64)

	if isReceive {
		// Failed inbound payments did not credit this wallet.
		if rejected {
			return nil
		}
		amount := tr.amountDrops()
		if d := meta.deliveredAmountDrops(); d > 0 {
			amount = d
		}
		if amount == 0 {
			return nil // ignore IOU or malformed non-XRP amounts
		}
		recipient := walletAddr
		return &txdb.WalletTransaction{
			Type:        txdb.Receive,
			ID:          strings.ToUpper(tr.Hash),
			Amount:      amount,
			Fees:        feeParsed,
			BlockNumber: ledgerIndex,
			Confirmed:   validated,
			Recipient:   &recipient,
		}
	}

	amount := tr.amountDrops()
	if amount == 0 {
		return nil // ignore non-XRP payments for this native XRP wallet
	}
	recipient := tr.Destination
	return &txdb.WalletTransaction{
		Type:        txdb.Send,
		ID:          strings.ToUpper(tr.Hash),
		Amount:      amount,
		Fees:        feeParsed,
		BlockNumber: ledgerIndex,
		Confirmed:   validated,
		Rejected:    rejected,
		Recipient:   &recipient,
	}
}

func (w *xrpWallet) rpcEndpoint() string {
	ep := w.settings["rpcprovider"]
	if ep == "" {
		if e, ok := DefaultRPCEndpoints[w.net]; ok {
			ep = e
		} else {
			ep = DefaultRPCEndpoints[util.Mainnet]
		}
	}

	// L1: warn when connecting to a non-local host over plain HTTP; a MITM can
	// forge submit responses or read balances.
	if w.net != util.Simnet {
		if u, err := url.Parse(ep); err == nil {
			host := u.Hostname()
			if u.Scheme != "https" && host != "127.0.0.1" && host != "localhost" {
				w.log.Warnf("rpcprovider %q uses plain HTTP to a non-local host; "+
					"wallet addresses and balances are exposed to the network", ep)
			}
		}
	}

	return ep
}

func (w *xrpWallet) wsEndpoint() string {
	if ep := w.settings["wsprovider"]; ep != "" {
		return ep
	}

	// Prefer the well-known default WS endpoint for this network regardless of
	// what RPC endpoint the user configured, avoiding brittle string equality.
	if defaultWS, found := DefaultWSEndpoints[w.net]; found {
		return defaultWS
	}

	// Custom/unrecognised network: try to derive a WS endpoint from the RPC URL.
	ep, err := deriveWSEndpoint(w.rpcEndpoint())
	if err != nil {
		w.log.Warnf("Could not derive XRP websocket endpoint from %q: %v", w.rpcEndpoint(), err)
		return ""
	}
	return ep
}

// ---- XRP binary transaction encoding ----

// xrpPayment holds the fields needed to build a signed Payment transaction.
type xrpPayment struct {
	account        [20]byte
	destination    [20]byte
	amount         uint64
	fee            uint64
	sequence       uint32
	lastLedgerSeq  uint32
	destinationTag *uint32 // optional; nil when not set
	signingPubKey  []byte  // 33-byte compressed pubkey
	txnSig         []byte  // DER-encoded ECDSA signature
}

// encodePaymentFields serializes the common shape of a Payment transaction
// (native XRP or issued-currency) in XRP binary format, deferring the Amount
// field to writeAmount. When forSigning is true, the TxnSignature field is
// omitted. Shared by encodePayment and encodeIOUPayment so their field order
// can't drift out of sync with each other.
func encodePaymentFields(
	account, destination [20]byte,
	sequence uint32,
	destinationTag *uint32,
	lastLedgerSeq uint32,
	writeAmount func(*bytes.Buffer) error,
	fee uint64,
	signingPubKey, txnSig []byte,
	forSigning bool,
) ([]byte, error) {
	var b bytes.Buffer

	b.WriteByte(fieldHdrTransactionType)
	writeUInt16(&b, 0) // Payment = 0

	b.WriteByte(fieldHdrFlags)
	writeUInt32(&b, 0)

	b.WriteByte(fieldHdrSequence)
	writeUInt32(&b, sequence)

	if destinationTag != nil {
		b.WriteByte(fieldHdrDestTag)
		writeUInt32(&b, *destinationTag)
	}

	b.WriteByte(fieldHdrLastLedgerSeq0)
	b.WriteByte(fieldHdrLastLedgerSeq1)
	writeUInt32(&b, lastLedgerSeq)

	b.WriteByte(fieldHdrAmount)
	if err := writeAmount(&b); err != nil {
		return nil, err
	}

	b.WriteByte(fieldHdrFee)
	writeXRPAmount(&b, fee)

	b.WriteByte(fieldHdrSigningPubKey)
	writeVarBlob(&b, signingPubKey)

	if !forSigning && len(txnSig) > 0 {
		b.WriteByte(fieldHdrTxnSignature)
		writeVarBlob(&b, txnSig)
	}

	b.WriteByte(fieldHdrAccount)
	writeVarBlob(&b, account[:])

	b.WriteByte(fieldHdrDestination)
	writeVarBlob(&b, destination[:])

	return b.Bytes(), nil
}

// encodePayment serializes a native-XRP Payment transaction in XRP binary
// format. When forSigning is true, the TxnSignature field is omitted.
func encodePayment(p *xrpPayment, forSigning bool) []byte {
	b, _ := encodePaymentFields(p.account, p.destination, p.sequence, p.destinationTag, p.lastLedgerSeq,
		func(b *bytes.Buffer) error { writeXRPAmount(b, p.amount); return nil },
		p.fee, p.signingPubKey, p.txnSig, forSigning)
	return b
}

func writeUInt16(b *bytes.Buffer, v uint16) {
	b.WriteByte(byte(v >> 8))
	b.WriteByte(byte(v))
}

func writeUInt32(b *bytes.Buffer, v uint32) {
	buf := [4]byte{}
	binary.BigEndian.PutUint32(buf[:], v)
	b.Write(buf[:])
}

// writeXRPAmount encodes drops in XRP native amount format (8 bytes).
// Bit 62 set = positive; bit 63 = 0 for native XRP.
func writeXRPAmount(b *bytes.Buffer, drops uint64) {
	v := uint64(0x4000000000000000) | drops
	buf := [8]byte{}
	binary.BigEndian.PutUint64(buf[:], v)
	b.Write(buf[:])
}

// writeVarBlob writes the XRP VarInt length prefix followed by the data.
func writeVarBlob(b *bytes.Buffer, data []byte) {
	n := len(data)
	switch {
	case n <= 192:
		b.WriteByte(byte(n))
	case n <= 12480:
		n -= 193
		b.WriteByte(byte(193 + (n >> 8)))
		b.WriteByte(byte(n & 0xFF))
	default:
		n -= 12481
		b.WriteByte(byte(241 + (n >> 16)))
		b.WriteByte(byte((n >> 8) & 0xFF))
		b.WriteByte(byte(n & 0xFF))
	}
	b.Write(data)
}

// signTxBytes computes the XRPL signing hash (STX\0 prefix, SHA-512Half) over
// an unsigned transaction blob and returns the DER-encoded ECDSA signature.
// Shared by signPayment, signIOUPayment, and signTrustSet so a future change
// to the signing procedure can't be applied to one transaction type and
// missed on another.
func signTxBytes(txBytes []byte, privKey *btcec.PrivateKey) []byte {
	h := sha512.New()
	h.Write(txSigningPrefix)
	h.Write(txBytes)
	msgHash := h.Sum(nil)[:32] // SHA-512Half
	sig := ecdsa.Sign(privKey, msgHash)
	return sig.Serialize()
}

// signPayment computes the signing hash and returns the DER-encoded ECDSA sig.
func signPayment(p *xrpPayment, privKey *btcec.PrivateKey) ([]byte, error) {
	return signTxBytes(encodePayment(p, true), privKey), nil
}

// computeTxHash returns the XRPL transaction ID (SHA-512Half of "TXN\0" ||
// fully-signed binary). Must be called on the signed encoding (forSigning=false).
func computeTxHash(rawTxBytes []byte) [32]byte {
	h := sha512.New()
	h.Write(txHashPrefix)
	h.Write(rawTxBytes)
	var hash [32]byte
	copy(hash[:], h.Sum(nil)[:32]) // SHA-512Half
	return hash
}

// BuildSignedPaymentBlob constructs, signs, and encodes an XRP Payment
// transaction. Exported for the harness sendxrp helper, which signs with the
// standalone genesis key directly (bypassing BIP-44 derivation). Returns the
// uppercase hex tx blob ready for the submit RPC call.
func BuildSignedPaymentBlob(privKey *btcec.PrivateKey, pubKey *btcec.PublicKey, from, to string, drops, fee uint64, sequence, lastLedgerSeq uint32) (string, error) {
	srcID, err := addressToAccountID(from)
	if err != nil {
		return "", fmt.Errorf("invalid source address: %w", err)
	}
	dstID, err := addressToAccountID(to)
	if err != nil {
		return "", fmt.Errorf("invalid destination address: %w", err)
	}
	p := &xrpPayment{
		account:       [20]byte(srcID),
		destination:   [20]byte(dstID),
		amount:        drops,
		fee:           fee,
		sequence:      sequence,
		lastLedgerSeq: lastLedgerSeq,
		signingPubKey: pubKey.SerializeCompressed(),
	}
	sig, err := signPayment(p, privKey)
	if err != nil {
		return "", fmt.Errorf("error signing payment: %w", err)
	}
	p.txnSig = sig
	return strings.ToUpper(hex.EncodeToString(encodePayment(p, false))), nil
}

// ---- XRP address encode/decode ----

// pubKeyToAddress derives the classic XRP 'r...' address from a 33-byte
// compressed secp256k1 public key.
func pubKeyToAddress(pubKey *btcec.PublicKey) string {
	// RIPEMD160(SHA256(compressed_pubkey)) = 20-byte account ID
	accountID := btcutil.Hash160(pubKey.SerializeCompressed())
	return accountIDToAddress(accountID)
}

// accountIDToAddress encodes a 20-byte account ID as an XRP classic address.
func accountIDToAddress(accountID []byte) string {
	// 25 bytes: 1 version + 20 account ID + 4 checksum
	full := make([]byte, 25)
	full[0] = 0x00 // account type prefix
	copy(full[1:21], accountID)

	h1 := sha256.Sum256(full[:21])
	h2 := sha256.Sum256(h1[:])
	copy(full[21:], h2[:4])

	return encodeXRPBase58(full)
}

// addressToAccountID decodes an XRP classic address into a 20-byte account ID.
func addressToAccountID(addr string) ([]byte, error) {
	decoded, err := decodeXRPBase58(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid XRP address encoding: %w", err)
	}
	if len(decoded) != 25 {
		return nil, fmt.Errorf("invalid XRP address length %d (want 25 bytes)", len(decoded))
	}
	if decoded[0] != 0x00 {
		return nil, fmt.Errorf("unexpected XRP address version byte 0x%02x", decoded[0])
	}

	payload := decoded[:21]
	h1 := sha256.Sum256(payload)
	h2 := sha256.Sum256(h1[:])
	if !bytes.Equal(h2[:4], decoded[21:]) {
		return nil, fmt.Errorf("invalid XRP address checksum")
	}

	return payload[1:], nil
}

// encodeXRPBase58 encodes bytes using the XRP custom base58 alphabet.
func encodeXRPBase58(b []byte) string {
	leadingZeros := 0
	for _, v := range b {
		if v != 0 {
			break
		}
		leadingZeros++
	}

	n := new(big.Int).SetBytes(b)
	base := big.NewInt(58)
	mod := new(big.Int)

	var result []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		result = append(result, xrpBase58Alphabet[mod.Int64()])
	}
	for i := 0; i < leadingZeros; i++ {
		result = append(result, xrpBase58Alphabet[0])
	}

	// Reverse.
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return string(result)
}

// decodeXRPBase58 decodes a string encoded with the XRP custom base58 alphabet.
// The input length is capped to prevent O(n²) big.Int work on crafted inputs.
func decodeXRPBase58(s string) ([]byte, error) {
	if len(s) == 0 || len(s) > maxBase58InputLen {
		return nil, fmt.Errorf("invalid base58 input length %d", len(s))
	}

	// Count leading 'r's (encoded zeros); process only the remaining chars in
	// the big.Int loop to keep the accumulator semantics correct.
	leadingZeros := 0
	for leadingZeros < len(s) && s[leadingZeros] == xrpBase58Alphabet[0] {
		leadingZeros++
	}

	n := new(big.Int)
	base := big.NewInt(58)
	for i := leadingZeros; i < len(s); i++ {
		idx := xrpBase58AlphabetDecodeMap[s[i]]
		if idx < 0 {
			return nil, fmt.Errorf("invalid base58 character: %c", s[i])
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(idx)))
	}

	nBytes := n.Bytes()
	result := make([]byte, leadingZeros+len(nBytes))
	copy(result[leadingZeros:], nBytes)
	return result, nil
}

// ---- Key management ----

func privKeyFromSeed(seed []byte) (*btcec.PrivateKey, func(), error) {
	extKey, err := keygen.GenDeepChild(seed, seedDerivationPath)
	if err != nil {
		return nil, nil, fmt.Errorf("error deriving key: %w", err)
	}

	privKeyBytes, err := extKey.SerializedPrivKey()
	if err != nil {
		extKey.Zero()
		return nil, nil, fmt.Errorf("error serializing private key: %w", err)
	}

	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)

	zero := func() {
		extKey.Zero()
		encode.ClearBytes(privKeyBytes)
	}

	return privKey, zero, nil
}
