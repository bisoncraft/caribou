// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

const (
	// standardActions is the ZIP 317 grace number of logical actions: the
	// smallest number a fee is charged for.
	standardActions = 2
	// feeRounds bounds the rounds in which a partial withdrawal looks for
	// the fee of the amount that is left after the fee.
	feeRounds = 4
)

var (
	_ asset.Withdrawer     = (*zecWallet)(nil)
	_ asset.TxFeeEstimator = (*zecWallet)(nil)

	errNotSynced      = errors.New("wallet not synced")
	errBalanceChanged = errors.New("the balance changed; refresh and try again")
	errLeftBehind     = errors.New("the balance needs more than one transaction to send; withdraw a smaller amount first")
)

// FeeRate returns the one fee rate of the wallet: the ZIP 317 marginal fee
// per logical action.
func (w *zecWallet) FeeRate() uint64 {
	return feeRate
}

// StandardSendFee returns the fee of a payment with two logical actions,
// the ZIP 317 minimum, at the given rate, or at the wallet's rate for a zero
// rate. It is never zero.
func (w *zecWallet) StandardSendFee(rate uint64) uint64 {
	if rate == 0 {
		rate = feeRate
	}
	return standardActions * rate
}

// checkRate refuses every fee rate but the wallet's: a Zcash fee is fixed by
// the shape of the transaction.
func checkRate(rate uint64) error {
	if rate != feeRate {
		return fmt.Errorf("the Zcash fee rate is %d zatoshis per action, not %d", feeRate, rate)
	}
	return nil
}

// coin is the output a send paid.
type coin struct {
	// txid is in wire order, and display is the ID in display order.
	txid    []byte
	display string
	value   uint64
}

var _ asset.Coin = (*coin)(nil)

// ID is the transaction ID in wire order.
func (c *coin) ID() util.Bytes { return c.txid }

// String is the transaction ID in display order.
func (c *coin) String() string { return c.display }

// TxID is the transaction ID in display order, the key of its record.
func (c *coin) TxID() string { return c.display }

// Value is what the recipient got, in zatoshis.
func (c *coin) Value() uint64 { return c.value }

// coinFor returns the coin of the last transaction of a send, the payment.
// It runs after the transactions exist, so it never fails: an ID zeclib
// could not have given is logged and kept as it is.
func (w *zecWallet) coinFor(txids []string, value uint64) *coin {
	var id string
	if len(txids) > 0 {
		id = txids[len(txids)-1]
	}
	wire, err := hex.DecodeString(id)
	if err != nil || len(wire) != 32 {
		w.log.Errorf("The send returned the transaction IDs %q; the coin keeps the last as it is", txids)
		return &coin{txid: []byte(id), display: id, value: value}
	}
	slices.Reverse(wire)
	return &coin{txid: wire, display: id, value: value}
}

// sendGate holds what a send needs: the open backend, under the read lock
// of beMtx until release, the account, and a copy of the seed, erased on
// release.
type sendGate struct {
	be      backend
	account *zeclib.Account
	seed    []byte
	release func()
}

// openSendGate checks that the wallet is unlocked and synced. Synced is
// zeclib's state alone: an error can stay in a synced status.
func (w *zecWallet) openSendGate() (*sendGate, error) {
	be, account, release, err := w.backend()
	if err != nil {
		return nil, err
	}
	if be.SyncStatus().State != zeclib.SyncSynced {
		release()
		return nil, errNotSynced
	}
	seed, err := w.seedCopy()
	if err != nil {
		release()
		return nil, err
	}
	return &sendGate{be: be, account: account, seed: seed, release: func() {
		encode.ClearBytes(seed)
		release()
	}}, nil
}

// finishSend records a send that created transactions and refreshes the
// balance. Nothing in it can fail the send: the transactions exist, and a
// caller told of a failure would pay again. Its problems are logged and left
// to the worker. It releases the gate.
func (w *zecWallet) finishSend(g *sendGate, txids []string, broadcastErr error) {
	if broadcastErr != nil {
		w.log.Warnf("The first broadcast of %v failed; the wallet broadcasts again while it syncs: %v", txids, broadcastErr)
	}
	for _, id := range txids {
		w.recordSent(g.be, g.account.ID, id)
	}
	g.release()
	w.refresh()
}

// Send pays exactly value to address, with the fee on top. The record of the
// transaction is written before Send returns on a best-effort basis: when it
// cannot be (a failed read, a failed ownership lookup), the coin is still
// returned, and the worker writes the record on a later pass. The fee limit is
// the fee of an estimate made inside Send, not of the estimate a form showed
// before: if the inputs changed in between, the fee charged can differ from
// the one displayed. Core passes only the fee rate, so there is no displayed
// fee to hold the send to.
func (w *zecWallet) Send(address string, value, rate uint64) (_ asset.Coin, err error) {
	defer func() { err = w.explain(err) }()
	if err := checkRate(rate); err != nil {
		return nil, err
	}
	if _, err := w.decodeAddress(address); err != nil {
		return nil, err
	}
	g, err := w.openSendGate()
	if err != nil {
		return nil, err
	}
	est, err := g.be.EstimateTransfer(g.account.ID, address, value, nil)
	if err != nil {
		g.release()
		return nil, err
	}
	res, err := g.be.Send(w.connContext(), g.account.ID, g.seed, address, value, nil, est.Fee)
	if err != nil {
		g.release()
		return nil, err
	}
	w.finishSend(g, res.TxIDs, res.BroadcastErr)
	return w.coinFor(res.TxIDs, value), nil
}

// withdrawal is how a withdrawal of a value is made: either everything with
// a send max, or an amount whose fee makes up the rest of the value. fee is
// always the fee of a proposal that succeeded.
type withdrawal struct {
	sendMax     bool
	amount, fee uint64
}

// planWithdrawal finds how to withdraw value. whole means the whole balance,
// value at or above the available balance; checkValue, that value must match
// what the send max would debit.
//
// The whole balance goes in one send max. A value that is not what the send
// max would debit means the balance moved since it was read. Funds one
// transaction cannot take fail the withdrawal.
//
// A part of the balance is sent as value less its fee. The fee depends on
// the amount, so the amount is tried from a fee of the standard fee and
// moved to the fee each estimate gives, until the two agree. An estimate
// that the funds do not cover tells what it required; the required value
// less the amount tried is a candidate for the next fee, never a fee
// reported or passed on. If the rounds do not settle, the value less the
// highest fee seen is sent, at the fee of its own estimate, so the debit is
// at most the value.
func planWithdrawal(be backend, accountID, address string, value uint64, whole, checkValue bool) (*withdrawal, error) {
	if whole {
		est, err := be.EstimateSendMax(accountID, address, nil)
		if err != nil {
			return nil, err
		}
		left := est.LeftBehind.TransparentValue + est.LeftBehind.NoteValue
		if checkValue && est.Amount+est.Fee+left != value {
			return nil, errBalanceChanged
		}
		if left > 0 {
			return nil, errLeftBehind
		}
		return &withdrawal{sendMax: true, amount: est.Amount, fee: est.Fee}, nil
	}
	fee := uint64(standardActions * feeRate)
	highest := fee
	var lastErr error
	for range feeRounds {
		if value <= fee {
			return nil, fmt.Errorf("the value %d does not cover the fee %d", value, fee)
		}
		amount := value - fee
		est, err := be.EstimateTransfer(accountID, address, amount, nil)
		var funds *zeclib.InsufficientFundsError
		if errors.As(err, &funds) {
			lastErr = err
			candidate := funds.Required - min(funds.Required, amount)
			if candidate <= fee {
				return nil, err
			}
			fee, highest = candidate, max(highest, candidate)
			continue
		}
		if err != nil {
			return nil, err
		}
		highest = max(highest, est.Fee)
		if est.Fee == fee {
			return &withdrawal{amount: amount, fee: est.Fee}, nil
		}
		fee = est.Fee
	}
	if value <= highest {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("the value %d does not cover the fee %d", value, highest)
	}
	amount := value - highest
	est, err := be.EstimateTransfer(accountID, address, amount, nil)
	if err != nil {
		return nil, err
	}
	if est.Fee > highest {
		return nil, errors.New("the fee of the withdrawal could not be settled")
	}
	return &withdrawal{amount: amount, fee: est.Fee}, nil
}

// Withdraw sends value to address with the fee taken from it. A value at or
// above the available balance withdraws everything.
func (w *zecWallet) Withdraw(address string, value, rate uint64) (_ asset.Coin, err error) {
	defer func() { err = w.explain(err) }()
	if err := checkRate(rate); err != nil {
		return nil, err
	}
	if _, err := w.decodeAddress(address); err != nil {
		return nil, err
	}
	g, err := w.openSendGate()
	if err != nil {
		return nil, err
	}
	whole := value >= w.cache.heldBalance().Available
	plan, err := planWithdrawal(g.be, g.account.ID, address, value, whole, true)
	if err != nil {
		g.release()
		return nil, err
	}
	ctx := w.connContext()
	var txids []string
	var broadcastErr error
	if plan.sendMax {
		res, err := g.be.SendMax(ctx, g.account.ID, g.seed, address, nil, plan.fee, plan.amount)
		if err != nil {
			g.release()
			return nil, err
		}
		txids, broadcastErr = res.TxIDs, res.BroadcastErr
	} else {
		res, err := g.be.Send(ctx, g.account.ID, g.seed, address, plan.amount, nil, plan.fee)
		if err != nil {
			g.release()
			return nil, err
		}
		txids, broadcastErr = res.TxIDs, res.BroadcastErr
	}
	w.finishSend(g, txids, broadcastErr)
	return w.coinFor(txids, plan.amount), nil
}

// EstimateSendTxFee returns the fee of a send or a withdrawal, and whether
// the address is valid. A withdrawal of the whole balance (maxWithdraw, or
// subtract with a value at or above the available balance) fails as the
// withdrawal would: when the balance needs more than one transaction, or
// moved since it was read.
func (w *zecWallet) EstimateSendTxFee(address string, value, rate uint64, subtract, maxWithdraw bool) (_ uint64, _ bool, err error) {
	defer func() { err = w.explain(err) }()
	valid := w.ValidateAddress(address)
	if err := checkRate(rate); err != nil {
		return 0, valid, err
	}
	if !valid {
		return 0, false, fmt.Errorf("invalid address %q", address)
	}
	be, account, release, err := w.backend()
	if err != nil {
		return 0, valid, err
	}
	defer release()
	// Before zeclib is synced, an estimate can count notes a send cannot use
	// yet. Synced is the state alone, as for a send.
	if be.SyncStatus().State != zeclib.SyncSynced {
		return 0, valid, errNotSynced
	}
	whole := maxWithdraw || (subtract && value >= w.cache.heldBalance().Available)
	if whole || subtract {
		plan, err := planWithdrawal(be, account.ID, address, value, whole, !maxWithdraw)
		if err != nil {
			return 0, valid, err
		}
		return plan.fee, valid, nil
	}
	est, err := be.EstimateTransfer(account.ID, address, value, nil)
	if err != nil {
		return 0, valid, err
	}
	return est.Fee, valid, nil
}
