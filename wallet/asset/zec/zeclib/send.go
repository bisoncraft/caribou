// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"context"
	"encoding/hex"
	"slices"
)

// createdTx is a transaction the backend created and stored.
type createdTx struct {
	// txid is in the byte order of the wire.
	txid []byte
	// raw is the serialized transaction, nil if it could not be read back.
	raw []byte
}

// PaidPool is the pool a payment lands in.
type PaidPool uint8

const (
	// PaidIronwood is a payment to an Orchard receiver, which lands in the
	// Ironwood pool.
	PaidIronwood PaidPool = iota + 1
	// PaidTransparent is a payment to a transparent receiver.
	PaidTransparent
)

func (p PaidPool) String() string {
	switch p {
	case PaidIronwood:
		return "Ironwood"
	case PaidTransparent:
		return "transparent"
	default:
		return "unknown"
	}
}

// TransferEstimate is the result of EstimateTransfer.
type TransferEstimate struct {
	// Fee is the fee in zatoshis.
	Fee uint64
	// PaidAddress is the address the payment would name, and PaidPool the
	// pool it would land in, as in SendResult.
	PaidAddress string
	PaidPool    PaidPool
}

// sendResult is what the backend returns for a send.
type sendResult struct {
	// transactions are in broadcast order.
	transactions []createdTx
	fee          uint64
	paidAddress  string
	paidPool     PaidPool
	// readErr says why a raw transaction is missing, if one is.
	readErr string
}

// unminedTx is a transaction the wallet created that is neither mined nor
// expired.
type unminedTx struct {
	// txid is in the byte order of the wire.
	txid []byte
	raw  []byte
	// expiryHeight is the last height the transaction can be mined at. Zero
	// means it does not expire.
	expiryHeight uint32
}

// SpendableValue is the value an account can spend now, in zatoshis.
type SpendableValue struct {
	// Ironwood is the value of the Ironwood notes a payment can spend.
	Ironwood uint64
	// Transparent is the value of the transparent outputs a payment can
	// spend: outputs of more than 5,000 zatoshis that are not coinbase
	// outputs. One transaction spends at most 1,333 of them.
	Transparent uint64
}

// SendResult describes a payment made by Send.
type SendResult struct {
	// TxIDs are the IDs of the created transactions in display byte order,
	// in the order they are broadcast. Each is pending from the moment it
	// is created: its inputs count as spent until it is mined or expires.
	TxIDs []string
	// Fee is the total fee in zatoshis.
	Fee uint64
	// PaidAddress is the address the payment names, which the wallet stores
	// as the recipient of the output: the address given to Send, or for a
	// unified address without an Orchard receiver, its transparent receiver.
	PaidAddress string
	// PaidPool is the pool the payment lands in. Zero, which reads as
	// unknown, means the backend reported a pool this package does not
	// know. It is not Ironwood.
	PaidPool PaidPool
	// BroadcastErr is the outcome of the first broadcast, nil if every
	// transaction was accepted. A transaction that was not accepted is
	// broadcast again while the wallet syncs, until it is mined or
	// expires. The outcomes, by errors.Is:
	//   - ErrBroadcastRejected: the server's node refused a transaction.
	//   - ErrServerUnreachable: the server could not be reached.
	//   - ErrServerFailure: the server failed in another way.
	//   - ErrInternal: a created transaction could not be read back from
	//     the database. It is broadcast from the database later.
	//   - The configuration's server list could not be read, the error that
	//     Open would report for it.
	//   - The context of the call was done.
	BroadcastErr error
}

// LeftBehind describes spendable funds that one send max transaction could
// not take because of the limits on its inputs.
type LeftBehind struct {
	// TransparentCount and TransparentValue are the number and value, in
	// zatoshis, of the spendable transparent outputs beyond the 1,333 one
	// transaction spends. The smallest ones are left.
	TransparentCount, TransparentValue uint64
	// NoteCount and NoteValue are the number and value of the spendable
	// Ironwood notes beyond the wallet's limit of Ironwood actions in one
	// transaction. The smallest ones are left.
	//
	// The funds stay spendable by a later transaction, although on their own
	// they may not cover its fee.
	NoteCount, NoteValue uint64
}

// SendMaxEstimate is the result of EstimateSendMax.
type SendMaxEstimate struct {
	// Amount is what the recipient would get and Fee the fee, in zatoshis.
	Amount, Fee uint64
	// PaidAddress and PaidPool are as in SendResult.
	PaidAddress string
	PaidPool    PaidPool
	LeftBehind  LeftBehind
}

// sendMaxResult is what the backend returns for a send max.
type sendMaxResult struct {
	sendResult
	amount     uint64
	leftBehind LeftBehind
}

// SendMaxResult describes a payment made by SendMax.
type SendMaxResult struct {
	SendResult
	// Amount is what the recipient gets, in zatoshis.
	Amount     uint64
	LeftBehind LeftBehind
}

func displayTxID(txid []byte) string {
	display := slices.Clone(txid)
	slices.Reverse(display)
	return hex.EncodeToString(display)
}

// EstimateTransfer returns the fee of a payment of amount zatoshis to address
// from the account, and the address and pool the payment would name. The
// payment is proposed as Send would propose it and then discarded. The
// arguments and errors are those of Send.
func (w *Wallet) EstimateTransfer(accountID, address string, amount uint64, memo []byte) (*TransferEstimate, error) {
	return w.be.estimateTransfer(accountID, address, amount, memo, w.cfg.MinConfirmations)
}

// Send pays amount zatoshis to address from the account and broadcasts the
// payment to the server the wallet syncs with.
//
// A unified address is paid at its Orchard receiver, in the Ironwood pool, if
// it has one, and otherwise at its transparent receiver. The payment spends
// the account's Ironwood notes and its transparent outputs other than
// coinbase outputs, transparent outputs first, and its change goes to the
// Ironwood pool. Inputs need the confirmations of Config.MinConfirmations.
// memo is the memo of the recipient's output, up to 512 bytes, and nil for
// none. An empty non-nil memo is a memo: an empty one for a shielded
// recipient, and ErrMemoNotAllowed for a transparent one. seed is the ZIP 32
// seed of the account. Nothing is created if the fee would exceed maxFee.
//
// One transaction spends at most 1,333 transparent outputs, the selector's
// default cap. A payment that needs more fails with *InsufficientFundsError,
// whose Available is what the capped selection holds, and a smaller payment
// succeeds.
//
// Send returns an error only when nothing was created. The errors include
// *InsufficientFundsError, *FeeTooHighError, *AmountBelowMinimumError,
// ErrNotSynced, ErrTooLarge, ErrAddressExpired, ErrUnsupportedRecipient,
// ErrMemoNotAllowed, ErrSeedMismatch and ErrDatabase. Once the payment is
// created it is pending whatever the broadcast returns, and the outcome of
// the broadcast is reported in SendResult.BroadcastErr. A transaction that
// was not accepted is broadcast again by the sync loop of a connected wallet.
// A failed broadcast is never answered by creating a second transaction.
//
// ctx governs the broadcast only. A done ctx does not stop the payment from
// being created, and the broadcast then fails with the context's error. On a
// server that does not answer, the broadcast of a transaction can take up to
// 30 seconds before it fails.
func (w *Wallet) Send(ctx context.Context, accountID string, seed []byte, address string, amount uint64, memo []byte, maxFee uint64) (*SendResult, error) {
	res, err := w.be.send(accountID, seed, address, amount, memo, maxFee, w.cfg.MinConfirmations)
	if err != nil {
		return nil, err
	}
	return w.finishSend(ctx, res), nil
}

// finishSend broadcasts created transactions and reports them.
func (w *Wallet) finishSend(ctx context.Context, res *sendResult) *SendResult {
	out := &SendResult{Fee: res.fee, PaidAddress: res.paidAddress, PaidPool: res.paidPool}
	for _, tx := range res.transactions {
		out.TxIDs = append(out.TxIDs, displayTxID(tx.txid))
	}
	out.BroadcastErr = w.broadcast(ctx, res)
	if out.BroadcastErr != nil {
		w.log.Warnf("The broadcast of transactions %v failed, and they will be broadcast again: %v", out.TxIDs, out.BroadcastErr)
	}
	return out
}

// EstimateSendMax returns what SendMax would send to address: the amount, the
// fee, the paid address and pool, and the spendable funds that one
// transaction could not take. It builds and validates the proposal without
// creating it. It checks neither a seed nor a fee limit nor an expected
// amount, which it does not take.
func (w *Wallet) EstimateSendMax(accountID, address string, memo []byte) (*SendMaxEstimate, error) {
	return w.be.estimateSendMax(accountID, address, memo, w.cfg.MinConfirmations)
}

// SendMax sends everything the account can spend in one transaction to
// address and broadcasts it as Send does.
//
// The inputs are every input Send could select. One transaction takes at
// most 1,333 transparent outputs and as many Ironwood notes as the wallet's
// limit of Ironwood actions allows, the largest of each, and the rest is
// reported in LeftBehind. Those funds stay spendable by a later transaction,
// although on their own they may not cover its fee. The transaction has no
// change: the recipient gets the inputs less the fee. It fails with
// *InsufficientFundsError when the inputs do not cover the fee and a payment
// of at least one zatoshi, or for a transparent recipient the smallest
// output nodes relay.
//
// expectedAmount, if not zero, is the amount the caller approved, such as
// the Amount of an earlier EstimateSendMax. The spendable inputs can change
// in between. If the amount differs, SendMax fails with *AmountChangedError
// and creates nothing. An input that becomes spendable after the estimate
// raises the fee and the amount together, and the fee limit is checked
// first, so *FeeTooHighError can mean the same thing. For either error the
// remedy is a new estimate.
//
// The other arguments, the errors, and the handling of the broadcast and of
// ctx are those of Send.
func (w *Wallet) SendMax(ctx context.Context, accountID string, seed []byte, address string, memo []byte, maxFee, expectedAmount uint64) (*SendMaxResult, error) {
	res, err := w.be.sendMax(accountID, seed, address, memo, maxFee, expectedAmount, w.cfg.MinConfirmations)
	if err != nil {
		return nil, err
	}
	return &SendMaxResult{
		SendResult: *w.finishSend(ctx, &res.sendResult),
		Amount:     res.amount,
		LeftBehind: res.leftBehind,
	}, nil
}

// broadcast sends created transactions to the server the wallet syncs with,
// in order, on a connection of its own. It never uses the connection of the
// sync goroutine. It stops at the first failure: a later transaction can
// depend on an earlier one.
func (w *Wallet) broadcast(ctx context.Context, res *sendResult) error {
	for _, tx := range res.transactions {
		if tx.raw == nil {
			return wrapf(ErrInternal, "transaction %s could not be read back after it was created: %s",
				displayTxID(tx.txid), res.readErr)
		}
	}
	if w.serversErr != nil {
		return w.serversErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	src, err := w.dial(w.servers.get())
	if err != nil {
		return err
	}
	defer src.close()
	for _, tx := range res.transactions {
		if err := src.sendTransaction(ctx, tx.raw); err != nil {
			// A done context is reported as such, not as the server
			// failure it shows up as.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
	}
	return nil
}

// Spendable returns the value the account can spend now: the value of the
// inputs a payment can select under Config.MinConfirmations. It is exact once
// the wallet is synced to the tip. During a scan it can include notes that a
// payment cannot select yet. It can be lower than the spendable balances of a
// Summary, which count transparent coinbase outputs, judge the transparent
// outputs of an address together rather than one by one, and can count a
// note before a payment can use it. It is zero until the wallet knows the
// chain tip.
func (w *Wallet) Spendable(accountID string) (SpendableValue, error) {
	return w.be.spendable(accountID, w.cfg.MinConfirmations)
}

// OwnsAddress returns the ID of the account an address belongs to, and false
// if it belongs to none. A transparent address matches only if the wallet
// generated it. A unified address matches the account that can derive one of
// its shielded receivers, generated or not, such as an address reduced to the
// Orchard receiver of one of the wallet's addresses. An address that does not
// decode, or is for another network, fails with ErrInvalidArgument.
func (w *Wallet) OwnsAddress(address string) (string, bool, error) {
	return w.be.ownsAddress(address)
}
