// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"sync"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/decred/base58"
)

const (
	walletTypeToken = "token"

	// ftTransferGas is the gas budget attached to an ft_transfer call, per
	// NEP-141 convention (well under the 300 Tgas per-tx limit).
	ftTransferGas = 30_000_000_000_000
	// storageDepositGas is the gas budget attached to a storage_deposit call.
	storageDepositGas = 30_000_000_000_000
	// ftTransferDeposit is the 1 yoctoNEAR deposit NEP-141 requires on
	// ft_transfer, to force a full-access-key signature.
	ftTransferDepositYocto = 1

	// standardStorageDepositYocto is the widely-used standard NEP-141
	// storage registration cost (0.00125 NEAR). Using this fixed constant
	// avoids an extra storage_balance_bounds view call per send; contracts
	// with a nonstandard (higher) minimum will reject the deposit with a
	// clear on-chain error rather than silently under-registering.
	standardStorageDepositYocto = "1250000000000000000000"
)

// standardStorageDepositAmount is standardStorageDepositYocto parsed once at
// package init, rather than on every Send.
var standardStorageDepositAmount = func() *big.Int {
	amt, ok := new(big.Int).SetString(standardStorageDepositYocto, 10)
	if !ok {
		panic("invalid standardStorageDepositYocto constant")
	}
	return amt
}()

var _ asset.TokenMaster = (*NearWallet)(nil)

// registerToken registers a NEP-141 token with the asset package for the
// given networks.
func registerToken(tokenID uint32, desc string, allowedNets ...util.Network) {
	token, found := Tokens[tokenID]
	if !found {
		panic("token " + strconv.Itoa(int(tokenID)) + " not known")
	}
	netAddrs := make(map[util.Network]string)
	for net, contract := range token.NetContracts {
		if !slices.Contains(allowedNets, net) {
			continue
		}
		netAddrs[net] = contract
	}
	asset.RegisterToken(tokenID, &util.Token{
		NetworkID: BipID,
		Name:      token.Name,
		UnitInfo:  token.UnitInfo,
	}, &asset.WalletDefinition{
		Type:        walletTypeToken,
		Tab:         "NEAR token",
		Description: desc,
	}, netAddrs)
}

// CreateTokenWallet just checks that the token exists; NEP-141 tokens
// additionally require the receiving account to be storage-registered on
// the token contract, which Send handles automatically for the recipient.
func (w *NearWallet) CreateTokenWallet(tokenID uint32, _ map[string]string) error {
	if Tokens[tokenID] == nil {
		return fmt.Errorf("token not found for asset ID %d", tokenID)
	}
	return nil
}

// OpenTokenWallet opens a NEP-141 token wallet sharing this wallet's keys,
// account, and RPC connection.
func (w *NearWallet) OpenTokenWallet(cfg *asset.TokenConfig) (asset.Wallet, error) {
	token, found := Tokens[cfg.AssetID]
	if !found {
		return nil, fmt.Errorf("token %d not found", cfg.AssetID)
	}
	contractID, ok := token.NetContracts[w.net]
	if !ok {
		return nil, fmt.Errorf("no %s network contract for token %s (%d)", w.net, token.Name, cfg.AssetID)
	}
	tw := &TokenWallet{
		parent:     w,
		assetID:    cfg.AssetID,
		token:      token,
		contractID: contractID,
		emit:       cfg.Emit,
		txDB:       cfg.NetworkTxDB,
		wi: asset.WalletInfo{
			Name:            token.Name,
			UnitInfo:        token.UnitInfo,
			BlockchainClass: asset.BlockchainClassAccount,
			Network:         w.Info().Network,
		},
	}
	return tw, nil
}

// TokenWallet is a NEP-141 fungible token wallet sharing its parent
// NearWallet's keys, account ID, and RPC connection. The deposit address is
// the same as the native NEAR account; the recipient must be storage
// registered on the token contract before it can hold a balance, which
// this wallet's Send establishes automatically via storage_deposit.
type TokenWallet struct {
	parent     *NearWallet
	assetID    uint32
	token      *Token
	contractID string
	emit       *asset.WalletEmitter
	txDB       txdb.NetworkTxDB
	wi         asset.WalletInfo
}

var _ asset.Wallet = (*TokenWallet)(nil)

func (w *TokenWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	if _, err := w.parent.client(); err != nil {
		return nil, fmt.Errorf("parent wallet not connected: %w", err)
	}

	// Registered so the parent's receive scan (the only poller either
	// wallet has) can notify this token wallet's own emitter when it
	// detects an incoming transfer, the same emitter Send already uses.
	// Done here rather than in OpenTokenWallet so a reconnect on the same
	// TokenWallet (Core reconnects the existing instance, it does not
	// call OpenTokenWallet again) re-registers it too — registration and
	// the deregistration below are then symmetric with Connect/ctx
	// lifetime instead of Open/ctx lifetime.
	w.parent.tokenWalletsMtx.Lock()
	if w.parent.tokenWallets == nil {
		w.parent.tokenWallets = make(map[uint32]*TokenWallet)
	}
	w.parent.tokenWallets[w.assetID] = w
	w.parent.tokenWalletsMtx.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		// Drop this wallet's entry from the parent's receive-scan
		// registry so a closed token wallet is not kept alive
		// indefinitely and does not keep receiving TransactionNotes
		// after it is gone.
		w.parent.tokenWalletsMtx.Lock()
		if w.parent.tokenWallets[w.assetID] == w {
			delete(w.parent.tokenWallets, w.assetID)
		}
		w.parent.tokenWalletsMtx.Unlock()
	}()
	return &wg, nil
}

func (w *TokenWallet) Info() *asset.WalletInfo {
	wi := w.wi
	return &wi
}

// Balance queries ft_balance_of for this wallet's account. An account that
// has never been storage-registered on the token contract reports a zero
// balance, not an error.
func (w *TokenWallet) Balance() (*asset.Balance, error) {
	atomic, err := w.ftBalanceOf(w.parent.accountID)
	if err != nil {
		return nil, err
	}
	return &asset.Balance{Available: atomic}, nil
}

func (w *TokenWallet) ftBalanceOf(accountID string) (uint64, error) {
	rpc, err := w.parent.client()
	if err != nil {
		return 0, err
	}
	raw, err := rpc.callFunction(w.parent.ctx, w.contractID, "ft_balance_of", map[string]string{"account_id": accountID})
	if err != nil {
		return 0, fmt.Errorf("ft_balance_of: %w", err)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("error decoding ft_balance_of result %q: %w", raw, err)
	}
	atomic, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("error parsing ft_balance_of value %q: %w", s, err)
	}
	return atomic, nil
}

// storageRegistered reports whether accountID is registered to hold this
// token, per storage_balance_of.
func (w *TokenWallet) storageRegistered(accountID string) (bool, error) {
	rpc, err := w.parent.client()
	if err != nil {
		return false, err
	}
	raw, err := rpc.callFunction(w.parent.ctx, w.contractID, "storage_balance_of", map[string]string{"account_id": accountID})
	if err != nil {
		return false, fmt.Errorf("storage_balance_of: %w", err)
	}
	return string(raw) != "null", nil
}

func (w *TokenWallet) DepositAddress() (string, error) {
	return w.parent.accountID, nil
}

func (w *TokenWallet) OwnsDepositAddress(address string) (bool, error) {
	return w.parent.OwnsDepositAddress(address)
}

func (w *TokenWallet) ValidateAddress(address string) bool {
	return w.parent.ValidateAddress(address)
}

func (w *TokenWallet) SyncStatus() (*asset.SyncStatus, error) {
	return w.parent.SyncStatus()
}

func (w *TokenWallet) StandardSendFee(feeRate uint64) uint64 {
	return w.parent.StandardSendFee(feeRate)
}

// FeeRate satisfies asset.Wallet. Token transaction fees are paid in the
// parent asset, so the rate is the parent chain's.
func (w *TokenWallet) FeeRate() uint64 {
	return w.parent.FeeRate()
}

func (w *TokenWallet) Send(address string, value, _ uint64) (asset.Coin, error) {
	if !w.ValidateAddress(address) {
		return nil, fmt.Errorf("invalid NEAR address %q", address)
	}
	if value == 0 {
		return nil, fmt.Errorf("cannot send zero value")
	}

	w.parent.keyMtx.RLock()
	if w.parent.privKey == nil {
		w.parent.keyMtx.RUnlock()
		return nil, fmt.Errorf("wallet is locked")
	}
	privKey := w.parent.privKey
	w.parent.keyMtx.RUnlock()

	balance, err := w.Balance()
	if err != nil {
		return nil, err
	}
	if balance.Available < value {
		return nil, asset.ErrInsufficientBalance
	}

	registered, err := w.storageRegistered(address)
	if err != nil {
		return nil, err
	}

	amountArgs, err := json.Marshal(map[string]string{
		"receiver_id": address,
		"amount":      strconv.FormatUint(value, 10),
	})
	if err != nil {
		return nil, fmt.Errorf("error encoding ft_transfer args: %w", err)
	}

	actions := make([]action, 0, 2)
	// attachedYocto is the parent-NEAR deposit the transaction spends
	// (separate from the token amount, which comes out of the token
	// balance): 1 yoctoNEAR for ft_transfer, plus the storage registration
	// deposit if the recipient isn't registered yet. sendActions checks it
	// against the parent account's spendable NEAR, the same guard native
	// sends get -- without it, an account sitting at its storage floor
	// would sign, burn a nonce, and fail on-chain with a raw RPC error
	// instead of asset.ErrInsufficientBalance. gasBudget is this
	// transaction's actual attached gas, priced into that same check --
	// a bare transfer's gas would under-reserve a function-call
	// transaction's fee by orders of magnitude -- and reused below for the
	// fee estimate if the broadcast outcome is ambiguous.
	attachedYocto := big.NewInt(ftTransferDepositYocto)
	gasBudget := int64(ftTransferGas)
	if !registered {
		depositArgs, err := json.Marshal(map[string]string{"account_id": address})
		if err != nil {
			return nil, fmt.Errorf("error encoding storage_deposit args: %w", err)
		}
		actions = append(actions, functionCallAction("storage_deposit", depositArgs, storageDepositGas, standardStorageDepositAmount))
		attachedYocto = new(big.Int).Add(attachedYocto, standardStorageDepositAmount)
		gasBudget += storageDepositGas
	}
	actions = append(actions, functionCallAction("ft_transfer", amountArgs, ftTransferGas, big.NewInt(ftTransferDepositYocto)))

	rpc, err := w.parent.client()
	if err != nil {
		return nil, err
	}

	// Shared with the native send path: nonce reservation, the balance
	// pre-flight, and a single fresh-nonce retry on an authoritative
	// InvalidNonce rejection.
	result, hash, refHeight, err := w.parent.sendActions(w.parent.ctx, rpc, privKey, w.contractID, actions, attachedYocto, gasBudget)

	txID := base58.Encode(hash[:])

	var rejected bool
	var failureMsg string
	switch {
	case err == nil:
		if result.Status.Failure != nil {
			// Executed but the actions failed: terminal, nonce consumed.
			// Record it as confirmed-and-rejected so the burnt gas is
			// visible in history, then report the failure.
			rejected = true
			failureMsg = string(result.Status.Failure)
		}
	case errors.Is(err, errBroadcastAmbiguous):
		// The broadcast may or may not have been accepted. Record the
		// transaction as pending; the tip poller resolves the final status.
		// Returning an error here would invite a retry that could
		// double-send. The nonce was already reserved, so it is not reused.
		w.parent.log.Warnf("Token transfer %s broadcast outcome unknown; recording as pending: %v", txID, err)
	default:
		return nil, fmt.Errorf("error broadcasting transaction: %w", err)
	}

	// The transaction fee is paid in NEAR gas, so it is recorded in the
	// parent asset's units — which is how token transaction fees are
	// displayed. Prefer the actual burnt gas; fall back to an estimate only
	// when the execution outcome is not yet known (ambiguous broadcast),
	// priced at this transaction's attached gas budget — not a bare
	// transfer's, which is orders of magnitude lower — until the pending
	// poller replaces it with the actual burn.
	var fees uint64
	if err == nil {
		fees = result.feeDrops()
	}
	if fees == 0 {
		fees = w.parent.estimateFeeAtGas(gasBudget)
	}

	recipient := address
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        txID,
		Amount:    value,
		Fees:      fees,
		NetworkID: BipID,
		AssetID:   w.assetID,
		Recipient: &recipient,
		Confirmed: rejected,
		Rejected:  rejected,
		// The reference height lets the pending poller prove the
		// transaction dead if the network never sees it.
		AdditionalData: map[string]string{refHeightKey: strconv.FormatUint(refHeight, 10)},
	}
	if rejected {
		wtx.BlockNumber, wtx.Timestamp = w.parent.resolveBlock(w.parent.ctx, rpc, result.TransactionOutcome.BlockHash)
	}
	if w.txDB != nil {
		if err := w.txDB.StoreTx(wtx); err != nil {
			w.parent.log.Errorf("Error storing token send tx: %v", err)
		}
	}
	if w.emit != nil {
		w.emit.TransactionNote(wtx, true)
	}

	if rejected {
		return nil, fmt.Errorf("token transfer failed on-chain: %s", failureMsg)
	}
	return &coin{txHash: hash, value: value}, nil
}

func (w *TokenWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if w.txDB == nil || req == nil {
		return &txdb.TxHistoryResponse{}, nil
	}
	return w.txDB.GetAssetTxs(&w.assetID, req)
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
	return nil, asset.CoinNotFoundError
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
