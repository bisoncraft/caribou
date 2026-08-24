// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/decred/base58"
)

const walletTypeToken = "token"

var _ asset.TokenMaster = (*Wallet)(nil)

// registerToken registers an SPL token with the asset package for the given
// networks.
func registerToken(tokenID uint32, desc string, allowedNets ...util.Network) {
	token, found := Tokens[tokenID]
	if !found {
		panic("token " + strconv.Itoa(int(tokenID)) + " not known")
	}
	netAddrs := make(map[util.Network]string)
	netVersions := make(map[util.Network][]uint32, len(allowedNets))
	for net, mint := range token.NetMints {
		if !slices.Contains(allowedNets, net) {
			continue
		}
		netAddrs[net] = mint
		netVersions[net] = []uint32{version}
	}
	asset.RegisterToken(tokenID, &util.Token{
		NetworkID: BipID,
		Name:      token.Name,
		UnitInfo:  token.UnitInfo,
	}, &asset.WalletDefinition{
		Type:        walletTypeToken,
		Tab:         "Solana token",
		Description: desc,
	}, netAddrs, netVersions)
}

// CreateTokenWallet just checks that the token exists; SPL token accounts
// are created lazily on first receive/send via the associated-token-account
// program's idempotent create instruction.
func (w *Wallet) CreateTokenWallet(tokenID uint32, _ map[string]string) error {
	if Tokens[tokenID] == nil {
		return fmt.Errorf("token not found for asset ID %d", tokenID)
	}
	return nil
}

// OpenTokenWallet opens an SPL token wallet sharing this wallet's keys and
// RPC connection.
func (w *Wallet) OpenTokenWallet(cfg *asset.TokenConfig) (asset.Wallet, error) {
	token, found := Tokens[cfg.AssetID]
	if !found {
		return nil, fmt.Errorf("token %d not found", cfg.AssetID)
	}
	mintAddr, ok := token.NetMints[w.net]
	if !ok {
		return nil, fmt.Errorf("no %s network mint for token %s (%d)", w.net, token.Name, cfg.AssetID)
	}
	mint := mustDecodePubKey(mintAddr)
	var owner [32]byte
	copy(owner[:], w.pubKey)
	ata, err := deriveATA(owner, mint)
	if err != nil {
		return nil, fmt.Errorf("deriveATA: %w", err)
	}
	return &TokenWallet{
		parent:      w,
		assetID:     cfg.AssetID,
		token:       token,
		mint:        mint,
		owner:       owner,
		ata:         ata,
		ataAddr:     base58.Encode(ata[:]),
		emit:        cfg.Emit,
		peersChange: cfg.PeersChange,
		txDB:        cfg.NetworkTxDB,
		wi: asset.WalletInfo{
			Name:              token.Name,
			SupportedVersions: []uint32{version},
			UnitInfo:          token.UnitInfo,
			BlockchainClass:   asset.BlockchainClassAccount,
			Network:           w.Info().Network,
		},
	}, nil
}

// TokenWallet is an SPL token wallet sharing its parent Wallet's keys and RPC
// connection. The deposit address is the associated token account (ATA) for
// the parent wallet's address and the token's mint, which differs per mint.
type TokenWallet struct {
	parent      *Wallet
	assetID     uint32
	token       *Token
	mint        [32]byte
	owner       [32]byte
	ata         [32]byte
	ataAddr     string
	emit        *asset.WalletEmitter
	peersChange func(uint32, error)
	txDB        txdb.NetworkTxDB
	wi          asset.WalletInfo

	metaMtx   sync.Mutex
	metaTable txdb.StoreTable
}

var _ asset.Wallet = (*TokenWallet)(nil)
var _ asset.TxFeeEstimator = (*TokenWallet)(nil)

func (w *TokenWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	// This checks w.parent.context(), not the ctx parameter above: Core
	// gives every wallet — this token wallet included — its own
	// ConnectionMaster, each deriving its own cancellable context, so
	// ctx here is cancelled only when *this* wallet disconnects and
	// carries no signal about the parent. w.parent.context() is
	// cancelled specifically when the parent disconnects, which is the
	// failure this check exists to catch: the parent (and the RPC
	// client this wallet shares with it) torn down while this wallet is
	// still trying to (re)connect.
	//
	// client() must be checked first: context() falls back to
	// context.Background() (whose Err() is always nil) when the parent
	// has never connected, so checking context().Err() alone would let
	// an unconnected parent pass silently.
	if _, err := w.parent.client(); err != nil {
		return nil, fmt.Errorf("parent wallet not connected: %w", err)
	}
	if err := w.parent.context().Err(); err != nil {
		return nil, fmt.Errorf("parent wallet not connected: %w", err)
	}
	// The token wallet shares the parent's already-verified RPC connection
	// and has no independent connectivity probe of its own, so report peers
	// once here. Without this, Core's per-asset peer counter for this token
	// never leaves its initial "unknown" state and Send is permanently
	// rejected by the no-connected-peers safety check, even though the
	// wallet is fully able to send and receive over the parent's RPC client.
	if w.peersChange != nil {
		w.peersChange(1, nil)
	}
	if w.txDB != nil {
		mt, err := w.txDB.Table(metaTableName)
		if err != nil {
			return nil, fmt.Errorf("error opening meta table: %w", err)
		}
		w.metaMtx.Lock()
		w.metaTable = mt
		w.metaMtx.Unlock()
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		// Watch this token's associated token account on the parent's
		// connection. The ATA need not exist yet: Solana accepts a subscription
		// to an account that has never been funded and fires when it is
		// created, which for a token wallet is its first ever deposit.
		refresh := make(chan struct{}, 1)
		watcher := &ataWatcher{
			account:  w.ataAddr,
			onChange: func() { signalRefresh(refresh) },
			current:  w.parent.ws.Load,
		}
		watcher.resync()
		defer watcher.stop()

		if err := w.syncHistory(ctx); err != nil {
			w.parent.log.Debugf("token %d syncHistory error: %v", w.assetID, err)
		}
		if err := w.checkPendingTxs(ctx); err != nil {
			w.parent.log.Debugf("token %d checkPendingTxs error: %v", w.assetID, err)
		}
		tokenRefreshLoop(ctx, refresh, pollInterval,
			func() time.Duration {
				// Resynced every pass, and the cadence read off whatever that
				// returns. A live subscription leaves the poll as pure backstop
				// against a dropped frame. Hence the unconditional false: a
				// failed pass here has no SyncStatus margin to protect either,
				// and so nothing to gain from retrying sooner.
				return refreshInterval(watcher.resync(), false)
			},
			func(ctx context.Context) {
				if err := w.syncHistory(ctx); err != nil {
					w.parent.log.Debugf("token %d syncHistory error: %v", w.assetID, err)
				}
				if err := w.checkPendingTxs(ctx); err != nil {
					w.parent.log.Debugf("token %d checkPendingTxs error: %v", w.assetID, err)
				}
			})
	}()
	return &wg, nil
}

// ataWatcher keeps one account registered on whichever wsClient the parent
// wallet currently has installed. current is the parent's loader, called fresh
// every resync rather than captured once: the parent installs a new client on
// every one of its connects and a token wallet's lifetime is independent of the
// parent's, so a client captured at Connect can be one the parent has since
// dropped. Staying on that client would leave the wallet waiting on
// notifications nothing sends, while reading liveness off the current one --
// the bug resync exists to prevent — would report the dead registration as
// covered and hold the slow cadence anyway.
//
// Not safe for concurrent use: ws is owned by the single goroutine that drives
// the refresh loop.
type ataWatcher struct {
	account  string
	onChange func()
	current  func() *wsClient

	ws *wsClient
}

// resync moves the registration onto the parent's current client if it has
// changed, and returns the client now carrying it, which is nil when the parent
// has none.
func (a *ataWatcher) resync() *wsClient {
	cur := a.current()
	if cur == a.ws {
		return a.ws
	}
	if a.ws != nil {
		a.ws.unwatch(a.account)
	}
	a.ws = cur
	if a.ws != nil {
		a.ws.watch(a.account, a.onChange)
	}
	return a.ws
}

// stop drops the registration, so the parent's connection stops carrying
// notifications nobody handles.
func (a *ataWatcher) stop() {
	if a.ws == nil {
		return
	}
	a.ws.unwatch(a.account)
	a.ws = nil
}

// tokenRefreshLoop runs pass until ctx is done, waking on a send to wake or
// once interval has elapsed, whichever comes first. interval is re-consulted
// every pass, which is also what gives the caller its per-pass hook.
//
// Not refreshLoop, deliberately. That one times its interval from the start of
// a pass so the tip cannot age past MaxBlockInterval, and accepts back-to-back
// passes as the price. A token wallet keeps no tip, so it has nothing to
// protect and no reason to pay that price: paging a long signature history
// twice with no gap is exactly what a phone should not do. Timing from the end
// of the previous pass instead guarantees a full interval of rest between
// passes.
//
// The wake path is held to the same guarantee by minGap, for the same reason
// and by the same means as refreshLoop's: the wake channel coalesces only while
// this loop is idle, so a notification landing mid-pass is ready the moment that
// pass ends, and an ATA taking a burst of transfers would otherwise page its
// history back to back. The wake is not dropped, only deferred.
func tokenRefreshLoop(ctx context.Context, wake <-chan struct{}, minGap time.Duration,
	interval func() time.Duration, pass func(context.Context)) {

	lastEnd := time.Now()
	var woke bool
	for {
		wait := interval()
		wakeCh := wake
		if woke {
			// Once a wake is in hand there is nothing left to wait for but the
			// floor, and no reason to keep listening: another notification
			// arriving in the meantime is asking for the same pass.
			wait = min(wait, minGap)
			wakeCh = nil
		}
		// From lastEnd rather than from now, so that holding a wake costs only
		// what the floor is short by. A notification arriving long after the
		// previous pass ended still runs immediately.
		timer := time.NewTimer(time.Until(lastEnd.Add(wait)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wakeCh:
			timer.Stop()
			woke = true
			continue
		case <-timer.C:
		}
		woke = false
		pass(ctx)
		lastEnd = time.Now()
	}
}

// checkPendingTxs resolves this token's pending sends: it confirms landed
// transactions and marks expired ones (past their LastValidBlockHeight and
// absent on-chain) rejected. Mirrors Wallet.checkPendingTxs, scoped to this
// token's own asset ID instead of the native asset, since a Send's pending
// record is tagged with the token's AssetID and syncHistory alone only learns about a tx
// once it has already landed.
func (w *TokenWallet) checkPendingTxs(ctx context.Context) error {
	if w.txDB == nil {
		return nil
	}
	rpc, err := w.parent.client()
	if err != nil {
		return err
	}
	pending, err := w.txDB.GetAssetPendingTxs(&w.assetID)
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
			w.parent.log.Debugf("token %d getBlockHeight: %v", w.assetID, heightErr)
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
						w.parent.log.Debugf("token %d expiry confirm getTransaction: %v", w.assetID, err)
						continue // transient error — retry next poll, do not reject
					}
					if onChain != nil {
						// Landed, but this node reports no status for it, so
						// nothing else will move it out of pending. Settle it
						// off a finalized read — see the native path in
						// sol.go's checkPendingTxs.
						final, err := rpc.getTransactionAtCommitment(ctx, tx.ID, "finalized")
						if err != nil {
							w.parent.log.Debugf("token %d expiry confirm finalized getTransaction: %v", w.assetID, err)
							continue
						}
						if final == nil {
							continue // landed but not yet rooted
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
							w.parent.log.Errorf("token %d StoreTx (nil-status finalized): %v", w.assetID, err)
						} else if w.emit != nil {
							w.emit.TransactionNote(tx, false)
						}
						continue
					}
					tx.Confirmed = true
					tx.Rejected = true
					tx.Confirms = nil
					if err := w.txDB.StoreTx(tx); err != nil {
						w.parent.log.Errorf("token %d StoreTx (expired): %v", w.assetID, err)
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
			if parsed := walletTransactionFromSPLRPC(onChain, w.ataAddr, w.assetID); parsed != nil {
				mergePendingInto(parsed, tx)
				tx = parsed
			}
		}
		if err := w.txDB.StoreTx(tx); err != nil {
			w.parent.log.Errorf("token %d StoreTx (pending update): %v", w.assetID, err)
		} else if (tx.Confirmed || (!wasRejected && tx.Rejected)) && w.emit != nil {
			w.emit.TransactionNote(tx, false)
		}
	}
	return nil
}

func (w *TokenWallet) Info() *asset.WalletInfo {
	wi := w.wi
	return &wi
}

func (w *TokenWallet) Balance() (*asset.Balance, error) {
	rpc, err := w.parent.client()
	if err != nil {
		return nil, err
	}
	amount, _, err := rpc.getTokenAccountBalance(w.parent.context(), w.ataAddr)
	if err != nil {
		return nil, fmt.Errorf("getTokenAccountBalance: %w", err)
	}
	return &asset.Balance{Available: amount}, nil
}

// DepositAddress returns the wallet's owner address, not the associated
// token account (ATA). This matches ecosystem convention (Phantom,
// exchanges, spl-token CLI): senders derive the destination ATA from the
// owner address themselves, same as Send does here. Returning the ATA
// instead would make a sender-side ATA derivation compute the ATA of an
// ATA, stranding funds in an account nothing in this codebase can recover.
func (w *TokenWallet) DepositAddress() (string, error) {
	return w.parent.address, nil
}

func (w *TokenWallet) OwnsDepositAddress(address string) (bool, error) {
	return w.parent.OwnsDepositAddress(address)
}

func (w *TokenWallet) ValidateAddress(address string) bool {
	// Callers supply the recipient's main wallet address, not their ATA; the
	// destination ATA is derived from it and the mint.
	return w.parent.ValidateAddress(address)
}

func (w *TokenWallet) SyncStatus() (*asset.SyncStatus, error) {
	return w.parent.SyncStatus()
}

func (w *TokenWallet) StandardSendFee(_ uint64) uint64 {
	return DefaultFee
}

// FeeRate satisfies asset.Wallet. Token transaction fees are paid in the
// parent asset, so the rate is the parent chain's.
func (w *TokenWallet) FeeRate() uint64 {
	return w.parent.FeeRate()
}

// EstimateSendTxFee estimates the SOL cost of sending value token atoms to
// address: the network fee, plus rent for a new destination associated token
// account when one doesn't exist yet. feeRate is ignored; Solana prices its
// own fee per message. An invalid address is reported via isValidAddress
// rather than an error.
func (w *TokenWallet) EstimateSendTxFee(address string, value, _ uint64, _, maxWithdraw bool) (uint64, bool, error) {
	rpc, err := w.parent.client()
	if err != nil {
		return 0, false, err
	}
	ctx := w.parent.context()
	valid := w.ValidateAddress(address)

	destOwner := w.owner
	if valid {
		destOwner = mustDecodePubKey(address)
	}
	destATA, err := deriveATA(destOwner, w.mint)
	if err != nil {
		return 0, false, fmt.Errorf("deriveATA: %w", err)
	}

	// These four RPC queries are mutually independent (none consumes another's
	// result), so fetch them concurrently instead of paying four sequential
	// round-trips on every debounced keystroke — mirrors Send's fetch below.
	var (
		wg            sync.WaitGroup
		balance       *asset.Balance
		balanceErr    error
		destATAExists = true
		destATAErr    error
		blockhash     string
		blockhashErr  error
		solBalance    uint64
		solBalanceErr error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		balance, balanceErr = w.Balance()
	}()
	go func() {
		defer wg.Done()
		blockhash, _, blockhashErr = rpc.getLatestBlockhash(ctx)
	}()
	go func() {
		defer wg.Done()
		solBalance, solBalanceErr = rpc.getBalance(ctx, w.parent.address)
	}()
	if valid {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, destATAExists, destATAErr = rpc.getTokenAccountBalance(ctx, base58.Encode(destATA[:]))
		}()
	}
	wg.Wait()

	if balanceErr != nil {
		return 0, false, balanceErr
	}
	if !maxWithdraw && balance.Available < value {
		return 0, false, asset.ErrInsufficientBalance
	}
	if destATAErr != nil {
		return 0, false, fmt.Errorf("getTokenAccountBalance: %w", destATAErr)
	}
	if blockhashErr != nil {
		return 0, false, fmt.Errorf("getLatestBlockhash: %w", blockhashErr)
	}
	if solBalanceErr != nil {
		return 0, false, fmt.Errorf("getBalance: %w", solBalanceErr)
	}

	bhBytes := base58.Decode(blockhash)
	if len(bhBytes) != 32 {
		return 0, false, fmt.Errorf("getLatestBlockhash: invalid blockhash length %d", len(bhBytes))
	}
	var bh [32]byte
	copy(bh[:], bhBytes)

	instrs := []splInstruction{
		createAssociatedTokenAccountIdempotentInstruction(w.owner, destATA, destOwner, w.mint),
		transferCheckedInstruction(w.ata, w.mint, destATA, w.owner, value, w.token.Decimals),
	}
	msg := compileSPLMessage(w.owner, instrs, bh)

	fee := w.StandardSendFee(0)
	if liveFee, err := rpc.getFeeForMessage(ctx, msg); err == nil && liveFee > 0 {
		fee = liveFee
	}
	required := fee
	if !destATAExists {
		rent, err := w.parent.tokenAccountRent(ctx)
		if err != nil {
			return 0, false, fmt.Errorf("getMinimumBalanceForRentExemption: %w", err)
		}
		required += rent
	}

	if solBalance < required {
		return 0, false, fmt.Errorf("insufficient SOL balance to cover network fee and new token account rent")
	}

	// required, not fee, is returned: the caller's real SOL cost includes any
	// new-ATA rent, unlike Send's stored Fees field, which records only fee.
	return required, valid, nil
}

func (w *TokenWallet) Send(address string, value, _ uint64) (asset.Coin, error) {
	rpc, err := w.parent.client()
	if err != nil {
		return nil, err
	}
	if !w.ValidateAddress(address) {
		return nil, fmt.Errorf("invalid address %q", address)
	}
	w.parent.keyMtx.RLock()
	if w.parent.privKey == nil {
		w.parent.keyMtx.RUnlock()
		return nil, fmt.Errorf("wallet is locked")
	}
	privKey := append(ed25519.PrivateKey(nil), w.parent.privKey...)
	w.parent.keyMtx.RUnlock()
	defer encode.ClearBytes(privKey)

	destOwner := mustDecodePubKey(address)
	destATA, err := deriveATA(destOwner, w.mint)
	if err != nil {
		return nil, fmt.Errorf("deriveATA: %w", err)
	}

	// The following four RPC queries are mutually independent (none consumes
	// another's result), so fetch them concurrently instead of paying four
	// sequential round-trips.
	var (
		wg                   sync.WaitGroup
		balance              *asset.Balance
		balanceErr           error
		destATAExists        bool
		destATAErr           error
		blockhash            string
		lastValidBlockHeight uint64
		blockhashErr         error
		solBalance           uint64
		solBalanceErr        error
	)
	ctx := w.parent.context()
	wg.Add(4)
	go func() {
		defer wg.Done()
		balance, balanceErr = w.Balance()
	}()
	go func() {
		defer wg.Done()
		_, destATAExists, destATAErr = rpc.getTokenAccountBalance(ctx, base58.Encode(destATA[:]))
	}()
	go func() {
		defer wg.Done()
		blockhash, lastValidBlockHeight, blockhashErr = rpc.getLatestBlockhash(ctx)
	}()
	go func() {
		defer wg.Done()
		solBalance, solBalanceErr = rpc.getBalance(ctx, w.parent.address)
	}()
	wg.Wait()

	if balanceErr != nil {
		return nil, balanceErr
	}
	if balance.Available < value {
		return nil, asset.ErrInsufficientBalance
	}
	if destATAErr != nil {
		return nil, fmt.Errorf("getTokenAccountBalance: %w", destATAErr)
	}
	if blockhashErr != nil {
		return nil, fmt.Errorf("getLatestBlockhash: %w", blockhashErr)
	}
	if solBalanceErr != nil {
		return nil, fmt.Errorf("getBalance: %w", solBalanceErr)
	}

	bhBytes := base58.Decode(blockhash)
	if len(bhBytes) != 32 {
		return nil, fmt.Errorf("getLatestBlockhash: invalid blockhash length %d", len(bhBytes))
	}
	var bh [32]byte
	copy(bh[:], bhBytes)

	instrs := []splInstruction{
		createAssociatedTokenAccountIdempotentInstruction(w.owner, destATA, destOwner, w.mint),
		transferCheckedInstruction(w.ata, w.mint, destATA, w.owner, value, w.token.Decimals),
	}
	msg := compileSPLMessage(w.owner, instrs, bh)

	// Native SOL must cover the tx fee, and possibly rent for a new
	// destination ATA.
	fee := w.StandardSendFee(0)
	if liveFee, err := rpc.getFeeForMessage(ctx, msg); err == nil && liveFee > 0 {
		fee = liveFee
	}
	required := fee
	if !destATAExists {
		rent, err := w.parent.tokenAccountRent(ctx)
		if err != nil {
			return nil, fmt.Errorf("getMinimumBalanceForRentExemption: %w", err)
		}
		required += rent
	}
	if solBalance < required {
		return nil, fmt.Errorf("insufficient SOL balance to cover network fee and new token account rent")
	}

	txB, sig := signSPLTx(msg, privKey)
	if _, err := rpc.sendTransaction(ctx, txB); err != nil {
		return nil, fmt.Errorf("sendTransaction: %w", err)
	}

	recipient := address
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        sig,
		Amount:    value,
		Fees:      fee,
		NetworkID: BipID,
		AssetID:   w.assetID,
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
			w.parent.log.Errorf("Error storing token send tx: %v", err)
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

func (w *TokenWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if w.txDB == nil || req == nil {
		return &txdb.TxHistoryResponse{}, nil
	}
	r := *req
	if r.N == 0 {
		r.N = 50
	}
	return w.txDB.GetAssetTxs(&w.assetID, &r)
}

func (w *TokenWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	if w.txDB != nil {
		wt, err := w.txDB.GetTx(txID)
		if err != nil {
			return nil, err
		}
		if wt != nil {
			return wt, nil
		}
	}
	rpc, err := w.parent.client()
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
	wt := walletTransactionFromSPLRPC(tx, w.ataAddr, w.assetID)
	if wt == nil {
		return nil, asset.CoinNotFoundError
	}
	return wt, nil
}

func (w *TokenWallet) PendingTransactions(_ context.Context) []*txdb.WalletTransaction {
	if w.txDB == nil {
		return nil
	}
	pending, err := w.txDB.GetAssetPendingTxs(&w.assetID)
	if err != nil {
		w.parent.log.Errorf("Error getting pending token txs: %v", err)
		return nil
	}
	return pending
}

// syncHistory scans the associated token account's signature history and
// stores any transfer transactions touching it.
func (w *TokenWallet) syncHistory(ctx context.Context) error {
	if w.txDB == nil {
		return nil
	}
	if w.parent.scanBackoff.blocked(time.Now()) {
		return nil
	}
	rpc, err := w.parent.client()
	if err != nil {
		return err
	}
	lastSig, err := w.getLastScannedSignature()
	if err != nil {
		return err
	}
	var until *string
	if lastSig != "" {
		until = &lastSig
	}
	sigs, err := rpc.getSignaturesForAddress(ctx, w.ataAddr, nil, until, 100)
	if err != nil {
		if isNotFoundRPCError(err) {
			return nil
		}
		if isRateLimitedRPCError(err) {
			w.parent.scanBackoff.rateLimited(time.Now())
		}
		return fmt.Errorf("getSignaturesForAddress: %w", err)
	}
	w.parent.scanBackoff.succeeded()
	if len(sigs) == 0 {
		return nil
	}
	// Resolve oldest-first, concurrently (fetchTransactions), applying
	// results in that order and stopping at the first failure so the
	// cursor only advances through a contiguous successful run — see
	// Wallet.syncHistory.
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
		if wt := walletTransactionFromSPLRPC(res.tx, w.ataAddr, w.assetID); wt != nil {
			wts = append(wts, wt)
			if wt.Type == txdb.Receive {
				w.parent.log.Infof("Detected incoming token %d transfer %s: %d atoms", w.assetID, wt.ID, wt.Amount)
			} else {
				w.parent.log.Infof("Detected historical token %d send %s: %d atoms", w.assetID, wt.ID, wt.Amount)
			}
		}
		cursorSig = oldestFirst[i]
	}
	if len(wts) > 0 {
		preservePendingData(w.txDB, &w.assetID, wts, w.parent.log)
		if err := w.txDB.StoreTxs(wts); err != nil {
			return err
		}
		w.emitBalanceChange()
	}
	if cursorSig == "" {
		return nil
	}
	return w.setLastScannedSignature(cursorSig)
}

// emitBalanceChange fetches the current token balance and pushes it to the
// UI. Called after syncHistory stores new transactions, since the front end
// doesn't re-poll balances on its own — without this, a detected incoming
// transfer shows up in the transaction list but the displayed balance never
// updates until the next full page load.
func (w *TokenWallet) emitBalanceChange() {
	if w.emit == nil {
		return
	}
	bal, err := w.Balance()
	if err != nil {
		w.parent.log.Warnf("Error emitting token balance change: %v", err)
		return
	}
	w.emit.BalanceChange(bal)
}

func (w *TokenWallet) getLastScannedSignature() (string, error) {
	w.metaMtx.Lock()
	mt := w.metaTable
	w.metaMtx.Unlock()
	if mt == nil {
		return "", nil
	}
	b, err := mt.Get(metaKey(w.assetID, lastScannedSigField))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (w *TokenWallet) setLastScannedSignature(sig string) error {
	w.metaMtx.Lock()
	mt := w.metaTable
	w.metaMtx.Unlock()
	if mt == nil || sig == "" {
		return nil
	}
	return mt.Set(metaKey(w.assetID, lastScannedSigField), []byte(sig), txdb.WithReplace())
}

// walletTransactionFromSPLRPC extracts a WalletTransaction from an SPL
// TransferChecked or Transfer instruction touching ataAddr.
func walletTransactionFromSPLRPC(tx *rpcTransaction, ataAddr string, tokenID uint32) *txdb.WalletTransaction {
	if tx == nil || tx.Meta == nil || len(tx.Transaction.Signatures) == 0 {
		return nil
	}
	var (
		txType    = txdb.Unknown
		amount    uint64
		recipient *string
	)
	for _, inst := range tx.Transaction.Message.Instructions {
		if inst.Program != "spl-token" || inst.Parsed == nil {
			continue
		}
		if inst.Parsed.Type != "transfer" && inst.Parsed.Type != "transferChecked" {
			continue
		}
		info := inst.Parsed.Info
		amt, err := splAmount(info)
		if err != nil {
			continue
		}
		switch {
		case info.Source == ataAddr && info.Destination == ataAddr:
			txType = txdb.SelfSend
			amount = amt
			r := ataAddr
			recipient = &r
		case info.Source == ataAddr:
			txType = txdb.Send
			amount = amt
			r := info.Destination
			recipient = &r
		case info.Destination == ataAddr:
			txType = txdb.Receive
			amount = amt
			r := ataAddr
			recipient = &r
		}
		if txType != txdb.Unknown {
			break
		}
	}
	if txType == txdb.Unknown {
		return nil
	}
	ts := uint64(0)
	if tx.BlockTime != nil && *tx.BlockTime > 0 {
		ts = uint64(*tx.BlockTime)
	}
	return &txdb.WalletTransaction{
		Type:        txType,
		ID:          tx.Transaction.Signatures[0],
		Amount:      amount,
		Fees:        tx.Meta.Fee,
		NetworkID:   BipID,
		AssetID:     tokenID,
		BlockNumber: tx.Slot,
		Timestamp:   ts,
		Recipient:   recipient,
		Rejected:    tx.Meta.Err != nil,
		Confirmed:   true,
	}
}

// splAmount extracts the atomic token amount from a parsed spl-token
// instruction's info field. TransferChecked reports it nested under
// tokenAmount.amount; the legacy Transfer instruction reports it directly.
func splAmount(info rpcInstructionInfo) (uint64, error) {
	if info.TokenAmount != nil {
		return strconv.ParseUint(info.TokenAmount.Amount, 10, 64)
	}
	if info.Amount != "" {
		return strconv.ParseUint(info.Amount, 10, 64)
	}
	return 0, fmt.Errorf("no amount in spl-token instruction info")
}
