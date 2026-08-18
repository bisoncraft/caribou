// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/keystore"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/decred/base58"
)

const (
	pollInterval = 2 * time.Second

	// fetchWorkers bounds how many getTransaction RPC calls run
	// concurrently when resolving a batch of signatures. Bounded rather
	// than unbounded so a large batch (up to 1000 signatures) cannot open
	// a thousand simultaneous connections to the RPC endpoint.
	fetchWorkers = 8
)

var (
	rpcWalletDefinition = &asset.WalletDefinition{
		Seeded:      true,
		Type:        walletTypeRPC,
		Tab:         "Native",
		Description: "Solana wallet",
		ConfigOpts: []*asset.ConfigOption{{
			Key:         "rpcprovider",
			DisplayName: "RPC Provider",
			Description: "Solana HTTP JSON-RPC endpoint URL",
		}, {
			Key:         "wsprovider",
			DisplayName: "WebSocket Provider",
			Description: "Optional Solana WebSocket endpoint URL",
		}},
	}

	WalletInfo = asset.WalletInfo{
		Name:              "Solana",
		SupportedVersions: []uint32{version},
		UnitInfo:          UnitInfo,
		AvailableWallets:  []*asset.WalletDefinition{rpcWalletDefinition},
		BlockchainClass:   asset.BlockchainClassAccount,
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
			Priority:      true,
		},
	}
)

func init() {
	asset.Register(BipID, &Driver{})
	registerToken(usdcTokenID, "The USDC SPL token.", util.Mainnet)
	registerToken(usdtTokenID, "The USDT SPL token.", util.Mainnet)
}

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
	if walletType != walletTypeRPC {
		return false, fmt.Errorf("unknown wallet type %q", walletType)
	}
	_, err := os.Stat(filepath.Join(dataDir, keyFileName))
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
	defer encode.ClearBytes(privKey)
	if err := os.MkdirAll(params.DataDir, 0o700); err != nil {
		return fmt.Errorf("error creating data directory: %w", err)
	}
	seed := privKey.Seed()
	defer encode.ClearBytes(seed)
	pubKey := privKey.Public().(ed25519.PublicKey)
	return keystore.Save(filepath.Join(params.DataDir, keyFileName), seed, pubKey, params.Pass)
}

type Wallet struct {
	log         util.Logger
	net         util.Network
	emit        *asset.WalletEmitter
	peersChange func(uint32, error)
	dataDir     string
	settings    map[string]string

	keyMtx  sync.RWMutex
	privKey ed25519.PrivateKey
	pubKey  ed25519.PublicKey
	address string

	// rpc, metaTable, and ctx are (re)set on every Connect, which Core
	// re-runs on reconnect (e.g. mobile background resume). Exported,
	// error-free methods such as Balance and Send are reachable from a UI
	// poll goroutine concurrently with a reconnect's Connect call, so
	// these are accessed atomically through client(), table(), and
	// context() rather than as plain fields.
	rpc       atomic.Pointer[rpcI]
	txDB      txdb.NetworkTxDB
	metaTable atomic.Pointer[txdb.StoreTable]
	ctx       atomic.Pointer[context.Context]

	tipMtx  sync.RWMutex
	tip     uint64
	tipTime time.Time

	// tokenAccountRentMtx guards tokenAccountRentLamports, the cached
	// rent-exempt minimum for a new SPL token account (splTokenAccountSize
	// bytes). It's a cluster-wide constant that essentially never changes,
	// so fetching it once per wallet session (rather than on every token
	// Send that creates a new destination ATA) avoids a redundant RPC
	// round-trip; 0 means not yet fetched.
	tokenAccountRentMtx      sync.Mutex
	tokenAccountRentLamports uint64
}

var _ asset.Wallet = (*Wallet)(nil)
var _ asset.Authenticator = (*Wallet)(nil)

// errNotConnected is returned by client() when the wallet's RPC client has
// not been set (Connect not yet run).
var errNotConnected = errors.New("wallet not connected")

// client returns the wallet's RPC client, or errNotConnected if Connect has
// not run. It reads the pointer atomically, so it is safe to call
// concurrently with Connect.
func (w *Wallet) client() (rpcI, error) {
	p := w.rpc.Load()
	if p == nil || *p == nil {
		return nil, errNotConnected
	}
	return *p, nil
}

// context returns the context Connect was called with, or a background
// context if Connect has not run yet. It reads the pointer atomically, so
// it is safe to call concurrently with Connect.
func (w *Wallet) context() context.Context {
	if p := w.ctx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// table returns the wallet's metadata table, or nil if Connect has not run
// or the wallet has no txDB. It reads the pointer atomically, so it is safe
// to call concurrently with Connect.
func (w *Wallet) table() txdb.StoreTable {
	if p := w.metaTable.Load(); p != nil {
		return *p
	}
	return nil
}

func newWallet(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (*Wallet, error) {
	pubKeyB, err := keystore.PubKey(filepath.Join(cfg.DataDir, keyFileName))
	if err != nil {
		return nil, fmt.Errorf("error loading key file: %w", err)
	}
	pubKey := ed25519.PublicKey(pubKeyB)
	return &Wallet{
		log:         logger,
		net:         network,
		emit:        cfg.Emit,
		peersChange: cfg.PeersChange,
		dataDir:     cfg.DataDir,
		settings:    cfg.Settings,
		pubKey:      pubKey,
		address:     base58.Encode(pubKey),
		txDB:        cfg.NetworkTxDB,
	}, nil
}

func (w *Wallet) Unlock(pw []byte) error {
	seed, err := keystore.Open(filepath.Join(w.dataDir, keyFileName), pw)
	if err != nil {
		return fmt.Errorf("error decrypting key: %w", err)
	}
	defer encode.ClearBytes(seed)
	privKey := ed25519.NewKeyFromSeed(seed)
	derivedPub := privKey.Public().(ed25519.PublicKey)
	if !derivedPub.Equal(w.pubKey) {
		encode.ClearBytes(privKey)
		return fmt.Errorf("decrypted key does not match stored public key")
	}
	w.keyMtx.Lock()
	w.privKey = privKey
	w.keyMtx.Unlock()
	return nil
}

func (w *Wallet) Lock() error {
	w.keyMtx.Lock()
	defer w.keyMtx.Unlock()
	if w.privKey != nil {
		encode.ClearBytes(w.privKey)
		w.privKey = nil
	}
	return nil
}

func (w *Wallet) Locked() bool {
	w.keyMtx.RLock()
	defer w.keyMtx.RUnlock()
	return w.privKey == nil
}

func (w *Wallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	w.ctx.Store(&ctx)
	var rpc rpcI = newRPCClient(w.rpcEndpoint(), w.wsEndpoint())
	w.rpc.Store(&rpc)
	if w.txDB != nil {
		mt, err := w.txDB.Table(metaTableName)
		if err != nil {
			return nil, fmt.Errorf("error opening meta table: %w", err)
		}
		w.metaTable.Store(&mt)
	}
	// Only do the fast synchronous part (tip probe + peer signal) at connect
	// time. History sync can take minutes on a first run and must not block.
	if err := w.updateTip(ctx); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Run immediately so history is populated before the first ticker fires.
		if err := w.syncHistory(ctx); err != nil {
			w.log.Debugf("syncHistory error: %v", err)
		}
		if err := w.checkPendingTxs(ctx); err != nil {
			w.log.Debugf("checkPendingTxs error: %v", err)
		}
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.refreshState(ctx); err != nil {
					w.log.Debugf("refreshState error: %v", err)
				}
			}
		}
	}()
	return &wg, nil
}

func (w *Wallet) updateTip(ctx context.Context) error {
	rpc, err := w.client()
	if err != nil {
		return err
	}
	slot, err := rpc.getSlot(ctx)
	if err != nil {
		if w.peersChange != nil {
			w.peersChange(0, err)
		}
		return fmt.Errorf("getSlot: %w", err)
	}
	if w.peersChange != nil {
		w.peersChange(1, nil)
	}
	w.tipMtx.Lock()
	oldTip := w.tip
	w.tip = slot
	w.tipTime = time.Now()
	w.tipMtx.Unlock()
	if slot != oldTip && w.emit != nil {
		w.emit.TipChange(slot)
	}
	return nil
}

func (w *Wallet) refreshState(ctx context.Context) error {
	if err := w.updateTip(ctx); err != nil {
		return err
	}
	if err := w.syncHistory(ctx); err != nil {
		return err
	}
	if err := w.checkPendingTxs(ctx); err != nil {
		return err
	}
	return nil
}

func (w *Wallet) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

func (w *Wallet) Balance() (*asset.Balance, error) {
	rpc, err := w.client()
	if err != nil {
		return nil, err
	}
	ctx := w.context()
	bal, err := rpc.getBalance(ctx, w.address)
	if err != nil {
		return nil, err
	}
	reserve, err := w.rentExemptReserve(ctx)
	if err != nil {
		return nil, err
	}
	var locked, available uint64
	if bal <= reserve {
		locked = bal
	} else {
		locked = reserve
		available = bal - reserve
	}
	return &asset.Balance{Available: available, Locked: locked}, nil
}

func (w *Wallet) DepositAddress() (string, error) {
	return w.address, nil
}

func (w *Wallet) OwnsDepositAddress(address string) (bool, error) {
	return address == w.address, nil
}

func (w *Wallet) Send(address string, value, _ uint64) (asset.Coin, error) {
	rpc, err := w.client()
	if err != nil {
		return nil, err
	}
	if !w.ValidateAddress(address) {
		return nil, fmt.Errorf("invalid address %q", address)
	}
	w.keyMtx.RLock()
	if w.privKey == nil {
		w.keyMtx.RUnlock()
		return nil, fmt.Errorf("wallet is locked")
	}
	privKey := append(ed25519.PrivateKey(nil), w.privKey...)
	w.keyMtx.RUnlock()
	defer encode.ClearBytes(privKey)

	ctx := w.context()
	balance, err := rpc.getBalance(ctx, w.address)
	if err != nil {
		return nil, fmt.Errorf("getBalance: %w", err)
	}
	reserve, err := w.rentExemptReserve(ctx)
	if err != nil {
		return nil, err
	}
	fee := w.StandardSendFee(0)
	if liveFee, err := rpc.getFeeForMessage(ctx, encodeTransferMessage(w.address, address, base58.Encode(make([]byte, 32)), value)); err == nil && liveFee > 0 {
		fee = liveFee
	}
	if fee > math.MaxUint64-value || balance < value+fee || balance-value-fee < reserve {
		return nil, asset.ErrInsufficientBalance
	}

	blockhash, lastValidBlockHeight, err := rpc.getLatestBlockhash(ctx)
	if err != nil {
		return nil, fmt.Errorf("getLatestBlockhash: %w", err)
	}
	if bh := base58.Decode(blockhash); len(bh) != 32 {
		return nil, fmt.Errorf("getLatestBlockhash: invalid blockhash length %d", len(bh))
	}
	_, txB, sig := signTransfer(w.address, address, blockhash, value, privKey)
	if _, err = rpc.sendTransaction(ctx, txB); err != nil {
		return nil, fmt.Errorf("sendTransaction: %w", err)
	}
	recipient := address
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        sig,
		Amount:    value,
		Fees:      fee,
		Recipient: &recipient,
		Confirms:  &txdb.Confirms{Current: 0, Target: 1},
	}
	setTxData(wtx, solTxData{
		LastValidBlockHeight: lastValidBlockHeight,
		SubmissionTime:       uint64(time.Now().Unix()),
		Blockhash:            blockhash,
	})
	if w.txDB != nil {
		if err := w.txDB.StoreTx(wtx); err != nil {
			w.log.Errorf("Error storing send tx: %v", err)
		}
	}
	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
	}

	sigB := base58.Decode(sig)
	if len(sigB) != 64 {
		return nil, fmt.Errorf("invalid signature length %d", len(sigB))
	}
	var coinSig [64]byte
	copy(coinSig[:], sigB)
	return &coin{signature: coinSig, value: value}, nil
}

func (w *Wallet) ValidateAddress(address string) bool {
	if address == "" {
		return false
	}
	pk := base58.Decode(address)
	return len(pk) == ed25519.PublicKeySize && base58.Encode(pk) == address
}

func (w *Wallet) SyncStatus() (*asset.SyncStatus, error) {
	w.tipMtx.RLock()
	tip := w.tip
	tipTime := w.tipTime
	w.tipMtx.RUnlock()
	return &asset.SyncStatus{
		Synced: time.Since(tipTime) < time.Duration(MaxBlockInterval)*time.Second,
		Blocks: tip,
	}, nil
}

func (w *Wallet) StandardSendFee(_ uint64) uint64 {
	return DefaultFee
}

// tokenAccountRent returns the rent-exempt minimum balance for a new SPL
// token account, caching it after the first successful fetch.
func (w *Wallet) tokenAccountRent(ctx context.Context) (uint64, error) {
	w.tokenAccountRentMtx.Lock()
	defer w.tokenAccountRentMtx.Unlock()
	if w.tokenAccountRentLamports > 0 {
		return w.tokenAccountRentLamports, nil
	}
	rpc, err := w.client()
	if err != nil {
		return 0, err
	}
	rent, err := rpc.getMinimumBalanceForRentExemption(ctx, splTokenAccountSize)
	if err != nil {
		return 0, err
	}
	w.tokenAccountRentLamports = rent
	return rent, nil
}

// FeeRate satisfies asset.Wallet, returning the standard per-transaction fee
// in lamports.
func (w *Wallet) FeeRate() uint64 {
	return w.StandardSendFee(0)
}

func (w *Wallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
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

func (w *Wallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	if w.txDB != nil {
		wt, err := w.txDB.GetTx(txID)
		if err != nil {
			return nil, err
		}
		if wt != nil {
			return wt, nil
		}
	}
	rpc, err := w.client()
	if err != nil {
		return nil, err
	}
	tx, err := rpc.getTransaction(ctx, txID)
	if err != nil {
		return nil, fmt.Errorf("getTransaction: %w", err)
	}
	if tx == nil {
		return nil, asset.CoinNotFoundError
	}
	wt := walletTransactionFromRPC(tx, w.address)
	if wt == nil {
		return nil, asset.CoinNotFoundError
	}
	return wt, nil
}

func (w *Wallet) PendingTransactions(_ context.Context) []*txdb.WalletTransaction {
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

// fetchResult is one getTransaction outcome from fetchTransactions, at the
// same index as the signature it was fetched for.
type fetchResult struct {
	tx  *rpcTransaction
	err error
}

// fetchTransactions resolves sigs to their transactions, up to fetchWorkers
// requests in flight at once, and returns results at the same index as the
// input signature. Bounding concurrency (rather than fetching all of a
// batch of up to 1000 signatures sequentially, one RPC round-trip at a
// time) keeps a sync cycle from running many multiples of pollInterval,
// while still bounded well short of one request per signature at once.
//
// Signatures are dispatched in windows of fetchWorkers rather than all at
// once. When stopOnFailure is true and a window contains a failed or nil
// result, no further windows are dispatched: a caller that only applies a
// contiguous successful prefix (cursor contiguity) would otherwise pay for
// up to the full batch (as large as 1000) of requests whose results are
// then discarded — precisely the wrong behavior against an endpoint that
// is failing because it is rate-limiting these requests. The window
// already in flight when the failure is found may still waste up to
// fetchWorkers-1 requests, but no more. A cancelled ctx also stops
// dispatch of further windows. Indices past a stopped point are left at
// their zero value, which callers already treat as "stop" (a nil tx).
func fetchTransactions(ctx context.Context, rpc rpcI, sigs []string, stopOnFailure bool) []fetchResult {
	results := make([]fetchResult, len(sigs))
	for start := 0; start < len(sigs); start += fetchWorkers {
		if ctx.Err() != nil {
			return results
		}
		end := min(start+fetchWorkers, len(sigs))
		var wg sync.WaitGroup
		for i := start; i < end; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i].tx, results[i].err = rpc.getTransaction(ctx, sigs[i])
			}(i)
		}
		wg.Wait()
		if !stopOnFailure {
			continue
		}
		for i := start; i < end; i++ {
			if results[i].err != nil || results[i].tx == nil {
				return results
			}
		}
	}
	return results
}

func (w *Wallet) syncHistory(ctx context.Context) error {
	if w.txDB == nil {
		return nil
	}
	rpc, err := w.client()
	if err != nil {
		return err
	}
	// Fetch any sigs older than the current batch that a previous page-capped
	// run left for later.
	if err := w.backfillHistory(ctx); err != nil {
		w.log.Warnf("backfillHistory: %v", err)
	}
	lastSig, err := w.getLastScannedSignature()
	if err != nil {
		return err
	}
	var until *string
	if lastSig != "" {
		until = &lastSig
	}
	var before *string
	var sigs []signatureInfo
	pageCapped := true
	for pages := 0; pages < 10; pages++ {
		batch, err := rpc.getSignaturesForAddress(ctx, w.address, before, until, 100)
		if err != nil && until != nil && isNotFoundRPCError(err) {
			// The cursor references a transaction that no longer exists (e.g.
			// after a ledger reset). Clear the stale cursor and do a full scan.
			_ = w.setLastScannedSignature("")
			until = nil
			batch, err = rpc.getSignaturesForAddress(ctx, w.address, before, nil, 100)
		}
		if err != nil {
			return fmt.Errorf("getSignaturesForAddress: %w", err)
		}
		if len(batch) == 0 {
			pageCapped = false
			break
		}
		sigs = append(sigs, batch...)
		if len(batch) < 100 {
			pageCapped = false
			break
		}
		next := batch[len(batch)-1].Signature
		before = &next
	}
	if len(sigs) == 0 {
		return nil
	}
	// Persist a backfill breadcrumb when more sigs may exist below the batch.
	if pageCapped {
		if err := w.setBeforeCursor(sigs[len(sigs)-1].Signature); err != nil {
			w.log.Warnf("setBeforeCursor: %v", err)
		}
		// Record the pre-advance scan cursor as the lower bound for backfill so
		// it doesn't re-walk history that was already stored.
		if lastSig != "" {
			if err := w.setBackfillUntil(lastSig); err != nil {
				w.log.Warnf("setBackfillUntil: %v", err)
			}
		}
	}
	// Resolve oldest-first, concurrently (fetchTransactions), but apply the
	// results in that same oldest-first order and stop at the first fetch
	// failure so the cursor only advances through a contiguous successful
	// run. A nil RPC response is treated the same as an error and stops the
	// cursor. When getTransaction succeeds but walletTransactionFromRPC
	// returns nil (the tx exists on-chain but is not relevant to this
	// wallet), the sig is still counted as successfully processed and
	// cursorSig advances past it.
	oldestFirst := make([]string, len(sigs))
	for i, s := range sigs {
		oldestFirst[len(sigs)-1-i] = s.Signature
	}
	results := fetchTransactions(ctx, rpc, oldestFirst, true)
	var wts []*txdb.WalletTransaction
	cursorSig := ""
	for i, res := range results {
		if res.err != nil || res.tx == nil {
			break
		}
		if wt := walletTransactionFromRPC(res.tx, w.address); wt != nil {
			wts = append(wts, wt)
		}
		cursorSig = oldestFirst[i]
	}
	if len(wts) > 0 {
		preservePendingData(w.txDB, nil, wts, w.log)
		if err := w.txDB.StoreTxs(wts); err != nil {
			return err
		}
	}
	if cursorSig == "" {
		return nil
	}
	return w.setLastScannedSignature(cursorSig)
}

// backfillHistory fetches sigs older than the batch from a previous page-capped
// syncHistory run, paginating up to 10 pages per call until the backfill is
// exhausted or another page cap is hit (in which case the breadcrumb is
// advanced for the next call).
func (w *Wallet) backfillHistory(ctx context.Context) error {
	rpc, err := w.client()
	if err != nil {
		return err
	}
	beforeCursor, err := w.getBeforeCursor()
	if err != nil || beforeCursor == "" {
		return err
	}
	backfillUntil, err := w.getBackfillUntil()
	if err != nil {
		return err
	}
	var until *string
	if backfillUntil != "" {
		until = &backfillUntil
	}
	before := &beforeCursor
	var sigs []signatureInfo
	pageCapped := true
	for pages := 0; pages < 10; pages++ {
		batch, err := rpc.getSignaturesForAddress(ctx, w.address, before, until, 100)
		if err != nil {
			return fmt.Errorf("getSignaturesForAddress backfill: %w", err)
		}
		if len(batch) == 0 {
			pageCapped = false
			break
		}
		sigs = append(sigs, batch...)
		if len(batch) < 100 {
			pageCapped = false
			break
		}
		next := batch[len(batch)-1].Signature
		before = &next
	}
	// Backfill is best-effort: fetch failures skip individual sigs rather than
	// stopping, because there is no cursor here to protect for contiguity.
	// Skipped sigs will be absent from history but will not cause the main
	// sync cursor to stall.
	oldestFirst := make([]string, len(sigs))
	for i, s := range sigs {
		oldestFirst[len(sigs)-1-i] = s.Signature
	}
	results := fetchTransactions(ctx, rpc, oldestFirst, false)
	var wts []*txdb.WalletTransaction
	for _, res := range results {
		if res.err != nil || res.tx == nil {
			continue
		}
		if wt := walletTransactionFromRPC(res.tx, w.address); wt != nil {
			wts = append(wts, wt)
		}
	}
	if len(wts) > 0 {
		preservePendingData(w.txDB, nil, wts, w.log)
		if err := w.txDB.StoreTxs(wts); err != nil {
			return err
		}
	}
	// Advance cursor only after a successful store so a write failure retries
	// the same batch next run rather than skipping it.
	if pageCapped && len(sigs) > 0 {
		_ = w.setBeforeCursor(sigs[len(sigs)-1].Signature)
	} else {
		_ = w.setBeforeCursor("")
		_ = w.setBackfillUntil("")
	}
	return nil
}

func (w *Wallet) checkPendingTxs(ctx context.Context) error {
	if w.txDB == nil {
		return nil
	}
	rpc, err := w.client()
	if err != nil {
		return err
	}
	// Scoped to the native asset only: unlike XRP, SOL sequencing is
	// per-transaction (a blockhash + nonce, not a shared account counter),
	// so there's no cross-asset reconciliation dependency here. Reparsing a
	// token wallet's pending tx from this wallet's own lamport-transfer
	// perspective (walletTransactionFromRPC below) would misclassify it and
	// overwrite the token record.
	pending, err := w.txDB.GetAssetPendingTxs(nil)
	if err != nil || len(pending) == 0 {
		return err
	}
	ids := make([]string, 0, len(pending))
	for _, tx := range pending {
		ids = append(ids, tx.ID)
	}
	const sigStatusBatchSize = 256
	statuses := make([]*signatureStatus, 0, len(ids))
	for i := 0; i < len(ids); i += sigStatusBatchSize {
		end := i + sigStatusBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch, err := rpc.getSignatureStatuses(ctx, ids[i:end])
		if err != nil {
			return err
		}
		if len(batch) != end-i {
			return fmt.Errorf("getSignatureStatuses: expected %d results, got %d", end-i, len(batch))
		}
		statuses = append(statuses, batch...)
	}
	var height uint64
	needHeight := false
	for i, tx := range pending {
		if statuses[i] == nil && txData(tx).LastValidBlockHeight > 0 {
			needHeight = true
			break
		}
	}
	if needHeight {
		var heightErr error
		height, heightErr = rpc.getBlockHeight(ctx)
		if heightErr != nil {
			w.log.Debugf("getBlockHeight: %v", heightErr)
		}
	}
	for i, tx := range pending {
		wasRejected := tx.Rejected
		status := statuses[i]
		if status == nil {
			if height > 0 {
				d := txData(tx)
				if d.LastValidBlockHeight > 0 && height > d.LastValidBlockHeight {
					// A lagging or load-balanced RPC node can return nil status
					// for a tx that actually landed. Confirm absence on-chain
					// before marking it rejected.
					onChain, err := rpc.getTransaction(ctx, tx.ID)
					if err != nil {
						w.log.Debugf("expiry confirm getTransaction: %v", err)
						continue // transient error — retry next poll, do not reject
					}
					if onChain != nil {
						// The tx landed but this node reports no status for it,
						// so nothing else will ever move it out of pending —
						// getSignatureStatuses is the only other confirmation
						// path and it is the thing that came back nil. Settle it
						// here off a finalized-commitment read, which is the same
						// evidence the "finalized" status branch below acts on.
						final, err := rpc.getTransactionAtCommitment(ctx, tx.ID, "finalized")
						if err != nil {
							w.log.Debugf("expiry confirm finalized getTransaction: %v", err)
							continue
						}
						if final == nil {
							continue // landed but not yet rooted — check again next poll
						}
						tx.Confirmed = true
						tx.Confirms = nil
						if final.Meta != nil && final.Meta.Err != nil {
							tx.Rejected = true
						}
						if final.Slot > 0 {
							tx.BlockNumber = final.Slot
						}
						if err := w.txDB.StoreTx(tx); err != nil {
							w.log.Errorf("StoreTx (nil-status finalized): %v", err)
						} else if w.emit != nil {
							w.emit.TransactionNote(tx, false)
						}
						continue
					}
					tx.Confirmed = true
					tx.Rejected = true
					tx.Confirms = nil
					if err := w.txDB.StoreTx(tx); err != nil {
						w.log.Errorf("StoreTx (expired): %v", err)
					} else if w.emit != nil {
						w.emit.TransactionNote(tx, false)
					}
				}
			}
			continue
		}
		if status.Err != nil {
			tx.Rejected = true
		}
		if status.Slot > 0 {
			tx.BlockNumber = status.Slot
		}
		if status.ConfirmationStatus == "finalized" {
			tx.Confirmed = true
			tx.Confirms = nil
		} else {
			tx.Confirmed = false
			current := uint32(0)
			if status.ConfirmationStatus == "confirmed" {
				current = 1
			}
			tx.Confirms = &txdb.Confirms{Current: current, Target: 1}
		}
		if onChain, err := rpc.getTransaction(ctx, tx.ID); err == nil && onChain != nil {
			if parsed := walletTransactionFromRPC(onChain, w.address); parsed != nil {
				mergePendingInto(parsed, tx)
				tx = parsed
			}
		}
		if err := w.txDB.StoreTx(tx); err != nil {
			w.log.Errorf("StoreTx (pending update): %v", err)
		} else if (tx.Confirmed || (!wasRejected && tx.Rejected)) && w.emit != nil {
			w.emit.TransactionNote(tx, false)
		}
	}
	return nil
}

func walletTransactionFromRPC(tx *rpcTransaction, walletAddr string) *txdb.WalletTransaction {
	if tx == nil || tx.Meta == nil || len(tx.Transaction.Signatures) == 0 {
		return nil
	}
	var ownIdx = -1
	for i, k := range tx.Transaction.Message.AccountKeys {
		if k.Pubkey == walletAddr {
			ownIdx = i
			break
		}
	}
	if ownIdx == -1 || ownIdx >= len(tx.Meta.PreBalances) || ownIdx >= len(tx.Meta.PostBalances) {
		return nil
	}
	// Leave a pure spl-token transfer to the token wallet's parser. It moves no
	// lamports of its own, so it would reach the balance-delta fallback below
	// and yield a native record (TokenID nil) under the same signature the
	// token wallet stores its record under. txdb keys on the signature and
	// replaces wholesale, so the two would overwrite each other on every poll.
	// checkPendingTxs guards the same hazard.
	//
	// The test is a transfer instruction specifically, not any spl-token
	// instruction, because that is exactly the set walletTransactionFromSPLRPC
	// claims. Every other spl-token instruction leaves the token parser
	// returning nil, so yielding here would drop the transaction from history
	// altogether: closeAccount refunds the ATA's rent to the wallet, and keyed
	// off the program alone that lamport credit was never recorded anywhere.
	//
	// A lamport credit is likewise kept even alongside a transfer. A token send
	// of ours only ever debits SOL — fee, plus rent when it creates the
	// destination ATA — so an increase is native movement the token record
	// cannot represent.
	hasTokenTransfer := false
	for _, inst := range tx.Transaction.Message.Instructions {
		if inst.Program != "spl-token" || inst.Parsed == nil {
			continue
		}
		if inst.Parsed.Type == "transfer" || inst.Parsed.Type == "transferChecked" {
			hasTokenTransfer = true
			break
		}
	}
	if hasTokenTransfer && tx.Meta.PostBalances[ownIdx] <= tx.Meta.PreBalances[ownIdx] {
		return nil
	}
	var (
		txType    = txdb.Unknown
		amount    uint64
		recipient *string
	)
	for _, inst := range tx.Transaction.Message.Instructions {
		if inst.Program != "system" || inst.Parsed == nil || inst.Parsed.Type != "transfer" {
			continue
		}
		info := inst.Parsed.Info
		switch {
		case info.Source == walletAddr && info.Destination == walletAddr:
			txType = txdb.SelfSend
			amount = info.Lamports
			r := walletAddr
			recipient = &r
		case info.Source == walletAddr:
			txType = txdb.Send
			amount = info.Lamports
			r := info.Destination
			recipient = &r
		case info.Destination == walletAddr:
			txType = txdb.Receive
			amount = info.Lamports
			r := walletAddr
			recipient = &r
		}
		if txType != txdb.Unknown {
			break
		}
	}
	if txType == txdb.Unknown {
		pre := tx.Meta.PreBalances[ownIdx]
		post := tx.Meta.PostBalances[ownIdx]
		if post > pre {
			txType = txdb.Receive
			amount = post - pre
			r := walletAddr
			recipient = &r
		} else if pre > post {
			delta := pre - post
			if delta <= tx.Meta.Fee {
				// Balance only dropped by the fee: this is a self-send.
				txType = txdb.SelfSend
				r := walletAddr
				recipient = &r
			} else {
				txType = txdb.Send
				amount = delta - tx.Meta.Fee
			}
		}
	}
	ts := uint64(0)
	if tx.BlockTime != nil && *tx.BlockTime > 0 {
		ts = uint64(*tx.BlockTime)
	}
	confirmed := true
	return &txdb.WalletTransaction{
		Type:        txType,
		ID:          tx.Transaction.Signatures[0],
		Amount:      amount,
		Fees:        tx.Meta.Fee,
		BlockNumber: tx.Slot,
		Timestamp:   ts,
		Recipient:   recipient,
		Rejected:    tx.Meta.Err != nil,
		Confirmed:   confirmed,
	}
}

// mergePendingInto carries the locally tracked state of pending record src onto
// dst, the same transaction reparsed from chain data.
//
// walletTransactionFromRPC and walletTransactionFromSPLRPC both report
// Confirmed unconditionally from a "confirmed"-commitment getTransaction and
// carry no RawTx, so storing their output verbatim over a Send's record drops
// the solTxData (kept in RawTx) that expiry detection needs and calls a
// confirmed-but-not-finalized transaction final. checkPendingTxs owns the
// finalized transition and distinguishes the two commitments deliberately.
//
// Rejected is OR'd rather than copied. It is monotonic — a transaction that
// failed does not later succeed — and both sides can be the one that knows:
// checkPendingTxs has just read it from the signature status, while a history
// scan has just parsed it from the transaction's own metadata and its pending
// record still says false. Copying either direction discards a rejection the
// other side had already established.
//
// Every caller merges the same field set, so adding a field to solTxData or to
// the tracked state only has to be handled here.
func mergePendingInto(dst, src *txdb.WalletTransaction) {
	dst.RawTx = src.RawTx
	dst.Confirmed = src.Confirmed
	dst.Confirms = src.Confirms
	dst.Rejected = dst.Rejected || src.Rejected
}

// preservePendingData applies mergePendingInto to every record in wts that is
// still pending, for a history scan that reparsed signatures it may already be
// tracking. Records from GetAssetPendingTxs are unconfirmed by construction —
// the pending index is keyed on !Confirmed — so this leaves them pending.
//
// It reads the pending set once rather than probing each record: a backfill
// page can carry a thousand transactions, and all but the handful still in
// flight would be a miss. assetID scopes that read to wts' own asset (nil
// for native SOL, a token's asset ID for an SPL token wallet) rather than
// every asset sharing this NetworkTxDB, since wts can only ever contain
// that one asset's records.
func preservePendingData(txDB txdb.NetworkTxDB, assetID *uint32, wts []*txdb.WalletTransaction, log util.Logger) {
	pending, err := txDB.GetAssetPendingTxs(assetID)
	if err != nil {
		log.Errorf("GetAssetPendingTxs: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	byID := make(map[string]*txdb.WalletTransaction, len(pending))
	for _, p := range pending {
		byID[p.ID] = p
	}
	for _, wt := range wts {
		existing, ok := byID[wt.ID]
		if !ok {
			continue
		}
		mergePendingInto(wt, existing)
	}
}

func (w *Wallet) rentExemptReserve(ctx context.Context) (uint64, error) {
	if cached, err := w.getCachedRentExemptReserve(); err == nil && cached > 0 {
		return cached, nil
	}
	rpc, err := w.client()
	if err != nil {
		return 0, err
	}
	reserve, err := rpc.getMinimumBalanceForRentExemption(ctx, 0)
	if err != nil {
		return 0, err
	}
	_ = w.setCachedRentExemptReserve(reserve)
	return reserve, nil
}

// isNotFoundRPCError returns true when the Solana RPC returns error code -32020
// ("Transaction not found"), which happens when a cursor signature has been
// pruned from the ledger.
func isNotFoundRPCError(err error) bool {
	return strings.Contains(err.Error(), "(-32020)")
}

func (w *Wallet) rpcEndpoint() string {
	if ep := w.settings["rpcprovider"]; ep != "" {
		return ep
	}
	if ep, ok := DefaultRPCEndpoints[w.net]; ok {
		return ep
	}
	return DefaultRPCEndpoints[util.Mainnet]
}

func (w *Wallet) wsEndpoint() string {
	if ep := w.settings["wsprovider"]; ep != "" {
		return ep
	}
	if ep, ok := DefaultWSEndpoints[w.net]; ok {
		return ep
	}
	return DefaultWSEndpoints[util.Mainnet]
}
