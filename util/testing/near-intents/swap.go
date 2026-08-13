// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"context"
	"encoding/json"
	"math/big"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/wallet/intents"
)

// Forced outcome values for the /test/force endpoint.
const (
	forceRefund = "REFUND"
	forceFail   = "FAIL"
	forceStall  = "STALL"
	forceResume = "RESUME"
)

// depositPollInterval is how often a swap's goroutine checks the origin
// chain for the deposit.
const depositPollInterval = 2 * time.Second

// swapState is one committed quote and its lifecycle. A goroutine started
// at quote time (run) advances status; HTTP handlers read and poke it. The
// mutex guards all mutable fields and is never held across a driver call.
type swapState struct {
	server         *Server
	correlationID  string
	depositAddress string
	origin, dest   *chainSpec
	originDrv      Driver
	destDrv        Driver
	amountIn       *big.Int // required deposit, origin base units
	amountOut      *big.Int // payout, destination base units
	refundFee      *big.Int // origin base units, withheld on refund
	recipient      string
	refundTo       string
	deadline       time.Time
	envelope       []byte // signed quote response, echoed in status
	createdAt      time.Time
	poke           chan struct{}

	mtx           sync.Mutex
	status        string
	deposited     *big.Int
	depositTxHint string // txid from POST /v0/deposit/submit
	depositTx     string // txid actually reported in status
	nearTxHashes  []string
	originTxs     []string
	destTxs       []string
	realizedOut   string
	refunded      string
	refundReason  string
	forced        string
	updatedAt     time.Time
}

func (sw *swapState) logf(format string, args ...any) {
	sw.server.log.Printf("swap %s: "+format, append([]any{sw.correlationID}, args...)...)
}

func (sw *swapState) setStatus(status string) {
	sw.mtx.Lock()
	sw.status = status
	sw.updatedAt = time.Now()
	sw.mtx.Unlock()
	sw.logf("status -> %s", status)
}

func (sw *swapState) setDepositHint(txid string) {
	sw.mtx.Lock()
	sw.depositTxHint = txid
	sw.mtx.Unlock()
	select {
	case sw.poke <- struct{}{}:
	default:
	}
}

func (sw *swapState) setForced(outcome string) {
	sw.mtx.Lock()
	sw.forced = outcome
	sw.mtx.Unlock()
	sw.logf("forced outcome set to %q", outcome)
}

func (sw *swapState) forcedOutcome() string {
	sw.mtx.Lock()
	defer sw.mtx.Unlock()
	return sw.forced
}

// sleep pauses for d, returning false if ctx is canceled first.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// run drives the swap to a terminal state. It is the only writer of status
// after construction.
func (sw *swapState) run(ctx context.Context) {
	late, ok := sw.awaitDeposit(ctx)
	if !ok {
		return // canceled, or abandoned with no deposit
	}

	// Deposit acknowledged. Report it and confirm it on the origin chain so
	// the depositing wallet sees the transaction mined.
	sw.mtx.Lock()
	txid := sw.depositTxHint
	if txid == "" {
		txid = sw.depositTx
	}
	if txid == "" {
		txid = "simnet-deposit-" + sw.depositAddress
	}
	sw.depositTx = txid
	sw.originTxs = append(sw.originTxs, txid)
	sw.mtx.Unlock()
	sw.setStatus(intents.StatusKnownDepositTx)
	if err := sw.originDrv.Confirm(); err != nil {
		sw.logf("origin confirm error: %v", err)
	}

	if !sleep(ctx, sw.server.cfg.ProcessingDelay) {
		return
	}
	sw.mtx.Lock()
	sw.nearTxHashes = append(sw.nearTxHashes, "simnet-near-"+sw.correlationID)
	sw.mtx.Unlock()
	sw.setStatus(intents.StatusProcessing)
	if !sleep(ctx, sw.server.cfg.ProcessingDelay) {
		return
	}

	// Hold while stalled.
	for sw.forcedOutcome() == forceStall {
		if !sleep(ctx, 500*time.Millisecond) {
			return
		}
	}

	switch {
	case late:
		sw.refund("deposit received after the quote deadline")
	case sw.forcedOutcome() == forceRefund:
		sw.refund("refund forced via /test/force")
	case sw.forcedOutcome() == forceFail:
		sw.setStatus(intents.StatusFailed)
	default:
		sw.payout()
	}
}

// awaitDeposit polls the origin chain until the full deposit lands, the
// context is canceled, or the deadline (plus the late window) passes. late
// reports whether the deposit landed after the deadline; ok is false when
// there is nothing to act on (cancelation, or no deposit within the late
// window — the record then stays in its last pre-deposit state forever,
// matching the service's behavior for never-funded quotes).
func (sw *swapState) awaitDeposit(ctx context.Context) (late, ok bool) {
	for {
		select {
		case <-ctx.Done():
			return false, false
		case <-sw.poke:
		case <-time.After(depositPollInterval):
		}

		amt, txid, err := sw.originDrv.CheckDeposit(sw.depositAddress)
		if err != nil {
			sw.logf("CheckDeposit error: %v", err)
			continue
		}
		now := time.Now()
		pastDeadline := now.After(sw.deadline)

		if amt.Cmp(sw.amountIn) >= 0 {
			sw.mtx.Lock()
			sw.deposited = amt
			sw.depositTx = txid
			sw.mtx.Unlock()
			return pastDeadline, true
		}
		if amt.Sign() > 0 {
			sw.mtx.Lock()
			sw.deposited = amt
			if txid != "" {
				sw.depositTx = txid
			}
			partial := sw.status == intents.StatusPendingDeposit
			sw.mtx.Unlock()
			if partial {
				sw.setStatus(intents.StatusIncompleteDeposit)
			}
			if pastDeadline {
				// A short deposit at the deadline is refunded.
				return true, true
			}
		}
		if now.After(sw.deadline.Add(sw.server.cfg.LateWindow)) {
			sw.mtx.Lock()
			abandoned := sw.deposited.Sign() == 0
			sw.mtx.Unlock()
			if abandoned {
				sw.logf("no deposit within the late window; abandoning")
				return false, false
			}
			return true, true
		}
	}
}

// refund returns the deposit (minus the quoted refund fee) to refundTo on
// the origin chain and finalizes as REFUNDED.
func (sw *swapState) refund(reason string) {
	sw.mtx.Lock()
	refundAmt := new(big.Int).Sub(sw.deposited, sw.refundFee)
	sw.mtx.Unlock()
	if refundAmt.Sign() <= 0 {
		sw.mtx.Lock()
		sw.refundReason = "deposit too small to refund: " + reason
		sw.mtx.Unlock()
		sw.setStatus(intents.StatusFailed)
		return
	}
	txid, err := sw.originDrv.Send(sw.refundTo, refundAmt)
	if err != nil {
		sw.logf("refund send error: %v", err)
		sw.mtx.Lock()
		sw.refundReason = reason + " (refund transaction failed: " + err.Error() + ")"
		sw.mtx.Unlock()
		sw.setStatus(intents.StatusFailed)
		return
	}
	if err := sw.originDrv.Confirm(); err != nil {
		sw.logf("origin confirm error: %v", err)
	}
	sw.mtx.Lock()
	sw.originTxs = append(sw.originTxs, txid)
	sw.refunded = refundAmt.String()
	sw.refundReason = reason
	sw.mtx.Unlock()
	sw.setStatus(intents.StatusRefunded)
}

// payout delivers amountOut to the recipient on the destination chain and
// finalizes as SUCCESS.
func (sw *swapState) payout() {
	txid, err := sw.destDrv.Send(sw.recipient, sw.amountOut)
	if err != nil {
		sw.logf("payout send error: %v", err)
		sw.mtx.Lock()
		sw.refundReason = "destination payout failed: " + err.Error()
		sw.mtx.Unlock()
		sw.setStatus(intents.StatusFailed)
		return
	}
	if err := sw.destDrv.Confirm(); err != nil {
		sw.logf("destination confirm error: %v", err)
	}
	sw.mtx.Lock()
	sw.destTxs = append(sw.destTxs, txid)
	sw.realizedOut = sw.amountOut.String()
	sw.mtx.Unlock()
	sw.setStatus(intents.StatusSuccess)
}

// statusResponse renders the GET /v0/status body for this swap.
func (sw *swapState) statusResponse() map[string]any {
	sw.mtx.Lock()
	defer sw.mtx.Unlock()
	details := map[string]any{
		"depositedAmount":          sw.deposited.String(),
		"amountIn":                 sw.amountIn.String(),
		"amountOut":                sw.realizedOut,
		"slippage":                 0,
		"refundedAmount":           sw.refunded,
		"refundReason":             sw.refundReason,
		"refundFee":                sw.refundFee.String(),
		"intentHashes":             []string{},
		"nearTxHashes":             sw.nearTxHashes,
		"originChainTxHashes":      sw.originTxs,
		"destinationChainTxHashes": sw.destTxs,
	}
	return map[string]any{
		"status":        sw.status,
		"updatedAt":     sw.updatedAt.UTC().Format(timestampFormat),
		"correlationId": sw.correlationID,
		"swapDetails":   details,
		"quoteResponse": json.RawMessage(sw.envelope),
	}
}

// debugSummary renders the GET /test/swaps entry for this swap.
func (sw *swapState) debugSummary() map[string]any {
	sw.mtx.Lock()
	defer sw.mtx.Unlock()
	return map[string]any{
		"correlationId":  sw.correlationID,
		"depositAddress": sw.depositAddress,
		"status":         sw.status,
		"origin":         sw.origin.slug,
		"destination":    sw.dest.slug,
		"amountIn":       sw.amountIn.String(),
		"amountOut":      sw.amountOut.String(),
		"deposited":      sw.deposited.String(),
		"recipient":      sw.recipient,
		"refundTo":       sw.refundTo,
		"deadline":       sw.deadline.UTC().Format(timestampFormat),
		"forced":         sw.forced,
		"createdAt":      sw.createdAt.UTC().Format(timestampFormat),
		"updatedAt":      sw.updatedAt.UTC().Format(timestampFormat),
	}
}
