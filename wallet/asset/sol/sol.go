// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/decred/base58"
	"golang.org/x/crypto/scrypt"
)

const (
	pollInterval  = 2 * time.Second
	scryptSaltLen = 32
	scryptR       = 8
	scryptP       = 1
)

// kdfScryptN is the scrypt work factor for seed encryption. 2^15 (~32 MiB)
// balances security with mobile device constraints. Package-level so tests can
// lower it.
var kdfScryptN = 1 << 15

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
		SupportedVersions: []uint32{0},
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
	return saveKeyFile(filepath.Join(params.DataDir, keyFileName), privKey, params.Pass)
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

	// rpc, metaTable, and ctx are set once in Connect before the wallet is
	// used and are never reassigned; no mutex is needed.
	rpc       rpcI
	txDB      txdb.NetworkTxDB
	metaTable txdb.StoreTable
	ctx       context.Context

	tipMtx  sync.RWMutex
	tip     uint64
	tipTime time.Time
}

var _ asset.Wallet = (*Wallet)(nil)
var _ asset.Authenticator = (*Wallet)(nil)

func newWallet(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (*Wallet, error) {
	kf, err := readKeyFile(filepath.Join(cfg.DataDir, keyFileName))
	if err != nil {
		return nil, fmt.Errorf("error loading key file: %w", err)
	}
	pubKey := ed25519.PublicKey(kf.PubKey)
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
	kf, err := readKeyFile(filepath.Join(w.dataDir, keyFileName))
	if err != nil {
		return fmt.Errorf("error reading key file: %w", err)
	}
	seed, err := decryptSeed(kf.EncryptedSeed, pw, kf.Salt)
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
	w.ctx = ctx
	w.rpc = newRPCClient(w.rpcEndpoint(), w.wsEndpoint())
	if w.txDB != nil {
		mt, err := w.txDB.Table(metaTableName)
		if err != nil {
			return nil, fmt.Errorf("error opening meta table: %w", err)
		}
		w.metaTable = mt
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
	slot, err := w.rpc.getSlot(ctx)
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
	if w.rpc == nil {
		return nil, fmt.Errorf("wallet not connected")
	}
	bal, err := w.rpc.getBalance(w.ctx, w.address)
	if err != nil {
		return nil, err
	}
	reserve, err := w.rentExemptReserve(w.ctx)
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
	if w.rpc == nil {
		return nil, fmt.Errorf("wallet not connected")
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

	balance, err := w.rpc.getBalance(w.ctx, w.address)
	if err != nil {
		return nil, fmt.Errorf("getBalance: %w", err)
	}
	reserve, err := w.rentExemptReserve(w.ctx)
	if err != nil {
		return nil, err
	}
	fee := w.StandardSendFee(0)
	if liveFee, err := w.rpc.getFeeForMessage(w.ctx, encodeTransferMessage(w.address, address, base58.Encode(make([]byte, 32)), value)); err == nil && liveFee > 0 {
		fee = liveFee
	}
	if fee > math.MaxUint64-value || balance < value+fee || balance-value-fee < reserve {
		return nil, asset.ErrInsufficientBalance
	}

	blockhash, lastValidBlockHeight, err := w.rpc.getLatestBlockhash(w.ctx)
	if err != nil {
		return nil, fmt.Errorf("getLatestBlockhash: %w", err)
	}
	if bh := base58.Decode(blockhash); len(bh) != 32 {
		return nil, fmt.Errorf("getLatestBlockhash: invalid blockhash length %d", len(bh))
	}
	_, txB, sig := signTransfer(w.address, address, blockhash, value, privKey)
	if _, err = w.rpc.sendTransaction(w.ctx, txB); err != nil {
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

func (w *Wallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if w.txDB == nil || req == nil {
		return &txdb.TxHistoryResponse{}, nil
	}
	r := *req
	if r.N == 0 {
		r.N = 50
	}
	return w.txDB.GetTxs(&r)
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
	tx, err := w.rpc.getTransaction(ctx, txID)
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
	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		w.log.Errorf("Error getting pending txs: %v", err)
		return nil
	}
	return pending
}

func (w *Wallet) syncHistory(ctx context.Context) error {
	if w.txDB == nil {
		return nil
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
		batch, err := w.rpc.getSignaturesForAddress(ctx, w.address, before, until, 100)
		if err != nil && until != nil && isNotFoundRPCError(err) {
			// The cursor references a transaction that no longer exists (e.g.
			// after a ledger reset). Clear the stale cursor and do a full scan.
			_ = w.setLastScannedSignature("")
			until = nil
			batch, err = w.rpc.getSignaturesForAddress(ctx, w.address, before, nil, 100)
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
	// Walk oldest-first and stop at the first fetch failure so the cursor only
	// advances through a contiguous successful run. A nil RPC response is
	// treated the same as an error and stops the cursor. When getTransaction
	// succeeds but walletTransactionFromRPC returns nil (the tx exists on-chain
	// but is not relevant to this wallet), the sig is still counted as
	// successfully processed and cursorSig advances past it.
	var wts []*txdb.WalletTransaction
	cursorSig := ""
	for i := len(sigs) - 1; i >= 0; i-- {
		tx, err := w.rpc.getTransaction(ctx, sigs[i].Signature)
		if err != nil || tx == nil {
			break
		}
		if wt := walletTransactionFromRPC(tx, w.address); wt != nil {
			wts = append(wts, wt)
		}
		cursorSig = sigs[i].Signature
	}
	if len(wts) > 0 {
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
		batch, err := w.rpc.getSignaturesForAddress(ctx, w.address, before, until, 100)
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
	var wts []*txdb.WalletTransaction
	for i := len(sigs) - 1; i >= 0; i-- {
		tx, err := w.rpc.getTransaction(ctx, sigs[i].Signature)
		if err != nil || tx == nil {
			continue
		}
		if wt := walletTransactionFromRPC(tx, w.address); wt != nil {
			wts = append(wts, wt)
		}
	}
	if len(wts) > 0 {
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
	pending, err := w.txDB.GetPendingTxs()
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
		batch, err := w.rpc.getSignatureStatuses(ctx, ids[i:end])
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
		height, heightErr = w.rpc.getBlockHeight(ctx)
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
					onChain, err := w.rpc.getTransaction(ctx, tx.ID)
					if err != nil {
						w.log.Debugf("expiry confirm getTransaction: %v", err)
						continue // transient error — retry next poll, do not reject
					}
					if onChain != nil {
						continue // tx landed, not expired
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
		if onChain, err := w.rpc.getTransaction(ctx, tx.ID); err == nil && onChain != nil {
			if parsed := walletTransactionFromRPC(onChain, w.address); parsed != nil {
				parsed.RawTx = tx.RawTx
				parsed.Confirmed = tx.Confirmed
				parsed.Confirms = tx.Confirms
				parsed.Rejected = tx.Rejected
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

func (w *Wallet) rentExemptReserve(ctx context.Context) (uint64, error) {
	if cached, err := w.getCachedRentExemptReserve(); err == nil && cached > 0 {
		return cached, nil
	}
	reserve, err := w.rpc.getMinimumBalanceForRentExemption(ctx, 0)
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

type keyFileData struct {
	PubKey        util.Bytes `json:"pubKey"`
	Salt          util.Bytes `json:"salt"`
	EncryptedSeed util.Bytes `json:"encryptedSeed"`
}

func saveKeyFile(path string, privKey ed25519.PrivateKey, pw []byte) error {
	seed := privKey.Seed()
	defer encode.ClearBytes(seed)

	salt := make([]byte, scryptSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generating salt: %w", err)
	}
	encrypted, err := encryptSeed(seed, pw, salt)
	if err != nil {
		return fmt.Errorf("error encrypting seed: %w", err)
	}
	pubKey := make([]byte, ed25519.PublicKeySize)
	copy(pubKey, privKey.Public().(ed25519.PublicKey))
	b, err := json.Marshal(keyFileData{PubKey: pubKey, Salt: salt, EncryptedSeed: encrypted})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func readKeyFile(path string) (*keyFileData, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data keyFileData
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// encryptSeed encrypts seed with pw and salt using scrypt + AES-256-GCM.
// Output: nonce(12) || ciphertext+tag.
func encryptSeed(seed, pw, salt []byte) ([]byte, error) {
	key, err := deriveAESKey(pw, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, seed, nil), nil
}

// decryptSeed decrypts a blob produced by encryptSeed.
func decryptSeed(ciphertext, pw, salt []byte) ([]byte, error) {
	if len(salt) == 0 {
		return nil, errors.New("key file missing salt")
	}
	key, err := deriveAESKey(pw, salt)
	if err != nil {
		return nil, fmt.Errorf("key derivation: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns {
		return nil, fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, ciphertext[:ns], ciphertext[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed (wrong password?): %w", err)
	}
	return plaintext, nil
}

// deriveAESKey stretches pw and salt to a 32-byte AES key using scrypt.
func deriveAESKey(pw, salt []byte) ([]byte, error) {
	return scrypt.Key(pw, salt, kdfScryptN, scryptR, scryptP, 32)
}
