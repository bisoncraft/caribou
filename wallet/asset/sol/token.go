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

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/decred/base58"
)

const (
	tokenMetaTableName     = "meta"
	tokenLastScannedSigKey = "lastScannedSignature"
	walletTypeToken        = "token"
)

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
		netVersions[net] = []uint32{0}
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
			SupportedVersions: []uint32{0},
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

func (w *TokenWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	if w.parent.ctx == nil || w.parent.ctx.Err() != nil {
		return nil, fmt.Errorf("parent wallet not connected")
	}
	if w.txDB != nil {
		mt, err := w.txDB.Table(tokenMetaTableName)
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
		if err := w.syncHistory(ctx); err != nil {
			w.parent.log.Debugf("token %d syncHistory error: %v", w.assetID, err)
		}
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.syncHistory(ctx); err != nil {
					w.parent.log.Debugf("token %d syncHistory error: %v", w.assetID, err)
				}
			}
		}
	}()
	return &wg, nil
}

func (w *TokenWallet) Info() *asset.WalletInfo {
	wi := w.wi
	return &wi
}

func (w *TokenWallet) Balance() (*asset.Balance, error) {
	amount, _, err := w.parent.rpc.getTokenAccountBalance(w.parent.ctx, w.ataAddr)
	if err != nil {
		return nil, fmt.Errorf("getTokenAccountBalance: %w", err)
	}
	return &asset.Balance{Available: amount}, nil
}

func (w *TokenWallet) DepositAddress() (string, error) {
	return w.ataAddr, nil
}

func (w *TokenWallet) OwnsDepositAddress(address string) (bool, error) {
	return address == w.ataAddr, nil
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

func (w *TokenWallet) Send(address string, value, _ uint64) (asset.Coin, error) {
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

	balance, err := w.Balance()
	if err != nil {
		return nil, err
	}
	if balance.Available < value {
		return nil, asset.ErrInsufficientBalance
	}

	destOwner := mustDecodePubKey(address)
	destATA, err := deriveATA(destOwner, w.mint)
	if err != nil {
		return nil, fmt.Errorf("deriveATA: %w", err)
	}
	_, destATAExists, err := w.parent.rpc.getTokenAccountBalance(w.parent.ctx, base58.Encode(destATA[:]))
	if err != nil {
		return nil, fmt.Errorf("getTokenAccountBalance: %w", err)
	}

	blockhash, lastValidBlockHeight, err := w.parent.rpc.getLatestBlockhash(w.parent.ctx)
	if err != nil {
		return nil, fmt.Errorf("getLatestBlockhash: %w", err)
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
	if liveFee, err := w.parent.rpc.getFeeForMessage(w.parent.ctx, msg); err == nil && liveFee > 0 {
		fee = liveFee
	}
	required := fee
	if !destATAExists {
		rent, err := w.parent.rpc.getMinimumBalanceForRentExemption(w.parent.ctx, splTokenAccountSize)
		if err != nil {
			return nil, fmt.Errorf("getMinimumBalanceForRentExemption: %w", err)
		}
		required += rent
	}
	solBalance, err := w.parent.rpc.getBalance(w.parent.ctx, w.parent.address)
	if err != nil {
		return nil, fmt.Errorf("getBalance: %w", err)
	}
	if solBalance < required {
		return nil, fmt.Errorf("insufficient SOL balance to cover network fee and new token account rent")
	}

	txB, sig := signSPLTx(msg, privKey)
	if _, err = w.parent.rpc.sendTransaction(w.parent.ctx, txB); err != nil {
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
	return w.txDB.GetTxs(&r)
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
	tx, err := w.parent.rpc.getTransaction(ctx, txID)
	if err != nil {
		return nil, fmt.Errorf("getTransaction: %w", err)
	}
	if tx == nil {
		return nil, asset.CoinNotFoundError
	}
	wt := walletTransactionFromSPLRPC(tx, w.ataAddr)
	if wt == nil {
		return nil, asset.CoinNotFoundError
	}
	return wt, nil
}

func (w *TokenWallet) PendingTransactions(_ context.Context) []*txdb.WalletTransaction {
	if w.txDB == nil {
		return nil
	}
	pending, err := w.txDB.GetPendingTxs()
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
	lastSig, err := w.getLastScannedSignature()
	if err != nil {
		return err
	}
	var until *string
	if lastSig != "" {
		until = &lastSig
	}
	sigs, err := w.parent.rpc.getSignaturesForAddress(ctx, w.ataAddr, nil, until, 100)
	if err != nil {
		if isNotFoundRPCError(err) {
			return nil
		}
		return fmt.Errorf("getSignaturesForAddress: %w", err)
	}
	if len(sigs) == 0 {
		return nil
	}
	var wts []*txdb.WalletTransaction
	cursorSig := ""
	for i := len(sigs) - 1; i >= 0; i-- {
		tx, err := w.parent.rpc.getTransaction(ctx, sigs[i].Signature)
		if err != nil || tx == nil {
			break
		}
		if wt := walletTransactionFromSPLRPC(tx, w.ataAddr); wt != nil {
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

func (w *TokenWallet) getLastScannedSignature() (string, error) {
	w.metaMtx.Lock()
	mt := w.metaTable
	w.metaMtx.Unlock()
	if mt == nil {
		return "", nil
	}
	b, err := mt.Get([]byte(tokenLastScannedSigKey))
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
	return mt.Set([]byte(tokenLastScannedSigKey), []byte(sig), true)
}

// walletTransactionFromSPLRPC extracts a WalletTransaction from an SPL
// TransferChecked or Transfer instruction touching ataAddr.
func walletTransactionFromSPLRPC(tx *rpcTransaction, ataAddr string) *txdb.WalletTransaction {
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
