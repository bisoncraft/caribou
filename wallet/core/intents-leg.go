// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// The 1Click deposit leg shared by cross-chain swaps (near-intents.go) and
// RHEA lending supplies (rhea-supply.go). Each engine owns its record type,
// lock, and notifications; the leg rules live here so both apply the same
// deadline timeout, inference, late-settlement correction, retirement, and
// deposit-funding sequence.

package core

import (
	"context"
	"fmt"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/intents"
)

// intentsLeg is the state of a 1Click deposit leg. Engines copy their
// record's leg fields in, apply a rule, and copy the result back under
// their own lock.
type intentsLeg struct {
	// State is QUOTED, DEPOSIT_SENT, a service status, EXPIRED, or
	// DEPOSIT_FAILED.
	State string
	// Inferred marks a terminal State inferred locally (a deadline
	// timeout) rather than reported by the service; InferredFrom is the
	// service status the inference gave up on.
	Inferred     bool
	InferredFrom string
	Deadline     time.Time
	DepositTxID  string
	RefundReason string
}

// legTerminal reports whether a leg state is final: a 1Click terminal
// status, or the local EXPIRED and DEPOSIT_FAILED.
func legTerminal(state string) bool {
	return state == IntentsStateExpired || state == IntentsStateDepositFailed || intents.TerminalStatus(state)
}

// legTimeout is the transition a deadline timeout made.
type legTimeout int

const (
	legTimeoutNone legTimeout = iota
	// legTimeoutExpired: a quote that was never funded became EXPIRED.
	legTimeoutExpired
	// legTimeoutUnobserved: a deposit the service never observed became
	// FAILED, inferred from PENDING_DEPOSIT.
	legTimeoutUnobserved
	// legTimeoutIncomplete: an incomplete deposit the service never
	// refunded became FAILED, inferred from INCOMPLETE_DEPOSIT.
	legTimeoutIncomplete
)

// applyTimeout moves a leg that never progressed to a terminal state once
// its deadline plus intentsExpiryGrace has passed. A quote never funded
// becomes EXPIRED (no funds moved; nothing the service could report later,
// so not inferred). A deposit the service never observed, or an incomplete
// deposit it never refunded, becomes FAILED marked Inferred: the service
// can still settle or refund past the local grace, so the leg keeps
// polling and yields to a later service status (canAdopt).
// KNOWN_DEPOSIT_TX and PROCESSING never time out locally: the service holds
// the deposit and is driving it. A zero deadline never times out.
func (l *intentsLeg) applyTimeout(now time.Time) legTimeout {
	if l.Deadline.IsZero() || now.Before(l.Deadline.Add(intentsExpiryGrace)) {
		return legTimeoutNone
	}
	switch l.State {
	case IntentsStateQuoted:
		l.State = IntentsStateExpired
		return legTimeoutExpired
	case IntentsStateDepositSent:
		l.State = intents.StatusFailed
		l.Inferred = true
		l.InferredFrom = intents.StatusPendingDeposit
		l.RefundReason = "the service did not observe a deposit before the quote deadline"
		return legTimeoutUnobserved
	case intents.StatusIncompleteDeposit:
		l.State = intents.StatusFailed
		l.Inferred = true
		l.InferredFrom = intents.StatusIncompleteDeposit
		l.RefundReason = "the service reported the deposit incomplete and did not refund it before the quote deadline"
		return legTimeoutIncomplete
	}
	return legTimeoutNone
}

// canAdopt is the late-settlement rule: whether a service-reported status
// may replace the leg's state. A service-confirmed terminal state is never
// regressed. A locally inferred one yields to the service's view, except to
// a restatement of PENDING_DEPOSIT or of the status the inference was made
// against, which reports nothing the inference did not already account for
// (adopting it would hand the leg back to the timeout, which would infer
// FAILED again, alternating every poll until the horizon).
func (l *intentsLeg) canAdopt(status string) bool {
	if !legTerminal(l.State) {
		return true
	}
	return l.Inferred && status != intents.StatusPendingDeposit && status != l.InferredFrom
}

// adopt takes a service-reported status, superseding any inference.
func (l *intentsLeg) adopt(status string) {
	l.State = status
	l.Inferred = false
	l.InferredFrom = ""
}

// retireDue reports whether an inferred terminal leg is past the window in
// which the service could still supersede it.
func (l *intentsLeg) retireDue(now time.Time) bool {
	return l.Inferred && now.After(l.Deadline.Add(intentsInferredHorizon))
}

// retire makes an inferred terminal state final. It reports whether
// anything changed.
func (l *intentsLeg) retire() bool {
	if !l.Inferred || !legTerminal(l.State) {
		return false
	}
	l.Inferred = false
	l.InferredFrom = ""
	return true
}

// applyRejection fails a DEPOSIT_SENT leg whose deposit transaction the
// origin chain rejected (see depositRejected). The service will never
// observe a deposit; the funds never left the wallet, though the network
// fee was spent.
func (l *intentsLeg) applyRejection() bool {
	if l.State != IntentsStateDepositSent {
		return false
	}
	l.State = IntentsStateDepositFailed
	l.RefundReason = "the deposit transaction was rejected by the origin chain; the deposited funds never left the wallet"
	return true
}

// depositRejected reports whether the origin wallet shows a leg's deposit
// transaction mined but rejected on chain. Any lookup failure reports
// false.
func (c *Core) depositRejected(ctx context.Context, l *intentsLeg, originAssetID uint32) bool {
	if l.State != IntentsStateDepositSent || l.DepositTxID == "" {
		return false
	}
	w, found := c.wallet(originAssetID)
	if !found {
		return false
	}
	wt, err := w.WalletTransaction(ctx, l.DepositTxID)
	return err == nil && wt != nil && wt.Rejected
}

// legDepositFeeRate checks that the origin wallet can fund a deposit before
// any record is persisted, so a knowable failure leaves no orphaned QUOTED
// record. Only UTXO wallets price a transaction to a specific rate; it must
// be a live estimate and pass the same bounds as a plain send (ceiling,
// live-rate band, mainnet USD cap) so a poisoned fee source cannot
// broadcast a runaway fee. Account-based wallets compute their own fees, so
// the rate is zero and Available, which already accounts for the wallet's
// own fee reserve, is the gate Send itself applies.
func (c *Core) legDepositFeeRate(w *xcWallet, depositAddress string, atoms uint64) (uint64, error) {
	assetID := w.AssetID
	if w.Info().BlockchainClass == asset.BlockchainClassUTXO {
		feeRate := w.usableFeeRate()
		if feeRate == 0 {
			return 0, fmt.Errorf("no live fee-rate estimate for %s deposit", unbip(assetID))
		}
		if err := c.validateSendFeeRate(w, depositAddress, atoms, feeRate, false); err != nil {
			return 0, fmt.Errorf("deposit fee rate rejected: %w", err)
		}
		return feeRate, nil
	}
	bal, err := w.Wallet.Balance()
	if err != nil {
		return 0, fmt.Errorf("error checking %s balance: %w", unbip(assetID), err)
	}
	if bal.Available < atoms {
		return 0, asset.ErrInsufficientBalance
	}
	return 0, nil
}

// legFunding is a deposit to a committed quote's deposit address.
type legFunding struct {
	wallet         *xcWallet
	depositAddress string
	atoms          uint64
	feeRate        uint64
	// persist stores the record in QUOTED before anything is sent. An
	// error aborts with nothing sent.
	persist func() error
	// recordSent records the broadcast deposit. The funds have moved by
	// the time it runs, so it must persist the transaction ID even when it
	// cannot reload the record, falling back to its local copy.
	recordSent func(txID string, fee uint64, feeUSD float64)
}

// fundLeg persists a committed quote's record, sends the deposit, and
// records the broadcast with the deposit's network fee from the wallet's
// history (zero when not yet known) priced in USD at send time. A Send
// error leaves the record QUOTED to expire unfunded; the caller should poke
// its poller so the deadline timeout runs. The message claims only that no
// deposit was recorded: a Send that errored after broadcasting would still
// be observed through the deposit address, and pays the wallet's own
// recipient in any case.
func (e *intentsEngine) fundLeg(ctx context.Context, f *legFunding) (string, error) {
	if err := f.persist(); err != nil {
		return "", err
	}
	coin, err := f.wallet.Wallet.Send(f.depositAddress, f.atoms, f.feeRate)
	if err != nil {
		return "", fmt.Errorf("deposit send failed (no deposit was recorded, the quote will expire): %w", err)
	}
	txID := coin.TxID()
	var fee uint64
	var feeUSD float64
	if tx, err := f.wallet.WalletTransaction(ctx, txID); err == nil && tx != nil && tx.Fees > 0 {
		fee = tx.Fees
		feeUSD = e.usdValue(ctx, intentsChainID(f.wallet.AssetID), fee)
	}
	f.recordSent(txID, fee, feeUSD)
	if err := e.client.SubmitDeposit(ctx, f.depositAddress, txID); err != nil {
		e.log.Warnf("Deposit hint submission failed (service will detect on-chain): %v", err)
	} else {
		e.log.Debugf("Deposit txid hint submitted for %s", f.depositAddress)
	}
	return txID, nil
}
