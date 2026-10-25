// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Recording of known incoming transactions. EVM wallets cannot observe
// unsolicited incoming transfers on their own: standard RPC has no
// per-address transaction index, and a contract-internal native transfer
// emits no logs at all. But when an external source — e.g. a cross-chain
// swap service reporting its payout — supplies the transaction hash, the
// transfer can be attributed and recorded. Attribution prefers chain
// truth and degrades gracefully:
//
//  1. A direct transfer (tx.to is the wallet) credits tx.value.
//  2. An ERC-20 payout is read from the token contract's standard
//     Transfer logs, which every conforming token emits for any balance
//     movement (transfer, transferFrom, or mint) at any call depth.
//  3. A contract-internal native payout is read from the delivering
//     bridge's own event when the bridge is a known one (the Omni
//     bridge's FinTransfer today).
//  4. Failing all of that, the caller's reported amount is recorded,
//     marked in AdditionalData so the provenance survives with the
//     record.
//
// Recorded transactions are always stored confirmed (recording requires a
// mined, successful transaction), which keeps these foreign-nonce records
// out of the pending-transaction machinery: getPendingTxs loads only
// unconfirmed records.

package eth

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// txHistoryAmountSourceKey marks a record whose amount could not be
// attributed on-chain. The value names the source: "reported" is the
// caller's stated amount (e.g. a swap service's payout report).
const txHistoryAmountSourceKey = "AmountSource"

func mustABIType(t string) abi.Type {
	typ, err := abi.NewType(t, "", nil)
	if err != nil {
		panic(fmt.Sprintf("bad static ABI type %q: %v", t, err))
	}
	return typ
}

// The Omni bridge's finalization event (Near-One/omni-bridge,
// BridgeTypes.sol):
//
//	event FinTransfer(uint8 indexed originChain, uint64 indexed originNonce,
//	    address tokenAddress, uint128 amount, address recipient,
//	    string feeRecipient)
//
// OmniBridge.finTransfer delivers native coin with an internal
// call{value} to the recipient — invisible in the outer transaction and
// unlogged by the chain itself — with tokenAddress the zero address.
var (
	omniFinTransferTopic = crypto.Keccak256Hash(
		[]byte("FinTransfer(uint8,uint64,address,uint128,address,string)"))
	omniFinTransferArgs = abi.Arguments{
		{Type: mustABIType("address")}, // tokenAddress
		{Type: mustABIType("uint128")}, // amount
		{Type: mustABIType("address")}, // recipient
		{Type: mustABIType("string")},  // feeRecipient
	}
)

// parseNativeBridgePayout scans receipt logs for known bridge events
// reporting a contract-internal native-coin payout to addr, returning the
// total wei credited. ok is false when no known event pays addr.
func parseNativeBridgePayout(receipt *types.Receipt, addr common.Address) (wei *big.Int, ok bool) {
	total := new(big.Int)
	for _, l := range receipt.Logs {
		if len(l.Topics) == 0 || l.Topics[0] != omniFinTransferTopic {
			continue
		}
		vals, err := omniFinTransferArgs.Unpack(l.Data)
		if err != nil || len(vals) < 3 {
			continue
		}
		tokenAddress, okToken := vals[0].(common.Address)
		amt, okAmt := vals[1].(*big.Int)
		recipient, okRecip := vals[2].(common.Address)
		if !okToken || !okAmt || !okRecip {
			continue
		}
		// A non-zero tokenAddress is a token payout, delivered by the
		// token contract and covered by its standard Transfer logs.
		if tokenAddress != (common.Address{}) || recipient != addr {
			continue
		}
		total.Add(total, amt)
	}
	if total.Sign() <= 0 {
		return nil, false
	}
	return total, true
}

// incomingTxBase builds the common fields of an incoming-tx record from
// its mined receipt, erroring on a failed transaction, which paid nothing.
func (w *assetWallet) incomingTxBase(txHash common.Hash, receipt *types.Receipt, nonce uint64) (*txdb.WalletTransaction, error) {
	// A nil receipt with a nil error, and a receipt not yet bound to a
	// block, are known provider states: the transaction is not settled in
	// this provider's view. Report not-found so the recording retry loop
	// comes back to it.
	if receipt == nil || receipt.BlockNumber == nil {
		return nil, asset.CoinNotFoundError
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return nil, fmt.Errorf("transaction %s failed on-chain", txHash)
	}
	addr := w.addr.String()
	return &txdb.WalletTransaction{
		Type:        txdb.Receive,
		ID:          txHash.String(),
		NetworkID:   w.baseChainID,
		AssetID:     w.assetID,
		Recipient:   &addr,
		BlockNumber: receipt.BlockNumber.Uint64(),
		Confirmed:   true,
		AdditionalData: map[string]string{
			txHistoryNonceKey: strconv.FormatUint(nonce, 10),
		},
	}, nil
}

// fetchIncomingTx attributes an incoming native-coin payment: a direct
// transfer, a known bridge's internal payout, or the reported amtHint.
func (w *ETHWallet) fetchIncomingTx(ctx context.Context, txHash common.Hash, amtHint uint64) (*txdb.WalletTransaction, error) {
	receipt, tx, err := w.node.transactionAndReceipt(ctx, txHash)
	if err != nil {
		return nil, err
	}
	// No transaction and no error is not-found, matching the receipt
	// guard in incomingTxBase.
	if tx == nil {
		return nil, asset.CoinNotFoundError
	}
	wt, err := w.incomingTxBase(txHash, receipt, tx.Nonce())
	if err != nil {
		return nil, err
	}
	if to := tx.To(); to != nil && *to == w.addr {
		wt.Amount = w.atomize(tx.Value())
	} else if wei, ok := parseNativeBridgePayout(receipt, w.addr); ok {
		wt.Amount = w.atomize(wei)
	} else if amtHint > 0 {
		wt.Amount = amtHint
		wt.AdditionalData[txHistoryAmountSourceKey] = "reported"
	}
	if wt.Amount == 0 {
		return nil, fmt.Errorf("no attributable payment to this wallet in %s", txHash)
	}
	return wt, nil
}

// fetchIncomingTx attributes an incoming token payment from the token
// contract's standard Transfer logs, falling back to the reported
// amtHint.
func (w *TokenWallet) fetchIncomingTx(ctx context.Context, txHash common.Hash, amtHint uint64) (*txdb.WalletTransaction, error) {
	receipt, tx, err := w.node.transactionAndReceipt(ctx, txHash)
	if err != nil {
		return nil, err
	}
	// No transaction and no error is not-found, matching the receipt
	// guard in incomingTxBase.
	if tx == nil {
		return nil, asset.CoinNotFoundError
	}
	wt, err := w.incomingTxBase(txHash, receipt, tx.Nonce())
	if err != nil {
		return nil, err
	}
	if value, err := w.extractValueFromTransferLog(receipt); err == nil && value > 0 {
		wt.Amount = value
	} else if amtHint > 0 {
		wt.Amount = amtHint
		wt.AdditionalData[txHistoryAmountSourceKey] = "reported"
	}
	if wt.Amount == 0 {
		return nil, fmt.Errorf("no attributable payment to this wallet in %s", txHash)
	}
	return wt, nil
}

// recordIncomingTx stores an attributed incoming transaction, skipping
// hashes already in the history, and emits the transaction note.
func (w *assetWallet) recordIncomingTx(txHash common.Hash, fetch func() (*txdb.WalletTransaction, error)) error {
	if et, err := w.txDB.getTx(txHash); err != nil {
		return fmt.Errorf("error checking history for %s: %w", txHash, err)
	} else if et != nil {
		return nil // already recorded
	}
	wt, err := fetch()
	if err != nil {
		return err
	}
	// Recording happens moments after the payout mines, so the recording
	// time is a serviceable stand-in for the block time, which would cost
	// another RPC round trip to resolve.
	now := uint64(time.Now().Unix())
	wt.Timestamp = now
	ewt := &extendedWalletTx{
		WalletTransaction: wt,
		SubmissionTime:    now,
		txHash:            txHash,
	}
	w.tryStoreDBTx(ewt)
	if !ewt.savedToDB {
		return fmt.Errorf("error storing incoming transaction %s", txHash)
	}
	w.emitTransactionNote(ewt.WalletTransaction, true)
	return nil
}

// RecordIncomingTx implements asset.IncomingTxRecorder. A recorded payout
// is funds the user is waiting on, so it is an alert call: an idle wallet
// polls at once and reports the new balance.
func (w *ETHWallet) RecordIncomingTx(ctx context.Context, txID string, amtHint uint64) error {
	w.alert()
	txHash := common.HexToHash(txID)
	return w.recordIncomingTx(txHash, func() (*txdb.WalletTransaction, error) {
		return w.fetchIncomingTx(ctx, txHash, amtHint)
	})
}

// RecordIncomingTx implements asset.IncomingTxRecorder. A recorded payout
// is funds the user is waiting on, so it is an alert call: an idle wallet
// polls at once and reports the new balance.
func (w *TokenWallet) RecordIncomingTx(ctx context.Context, txID string, amtHint uint64) error {
	w.alert()
	txHash := common.HexToHash(txID)
	return w.recordIncomingTx(txHash, func() (*txdb.WalletTransaction, error) {
		return w.fetchIncomingTx(ctx, txHash, amtHint)
	})
}
