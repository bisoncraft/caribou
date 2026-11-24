// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"context"
	"errors"
	"time"
)

const (
	// defaultRebroadcastMinDelay is the delay before the wallet first
	// checks that a transaction it created is known to the server, and
	// defaultRebroadcastMaxDelay the longest delay between two checks. After
	// the nth check of a transaction the delay is the minimum times 2^n.
	defaultRebroadcastMinDelay = 30 * time.Second
	defaultRebroadcastMaxDelay = 10 * time.Minute
)

// rebroadcastState is what the processor remembers of a transaction in the
// rebroadcast inventory.
type rebroadcastState struct {
	// checks counts the times the server was asked about the transaction.
	checks int
	// nextCheck is when it is asked next.
	nextCheck time.Time
}

// rebroadcastDue reports whether a pass is due between the scanned batches
// of a cycle: when a transaction of the inventory is due for a check, or when
// the inventory has not been read for rebroadcastMinDelay, so that a
// transaction created during a long cycle is found within that delay. It does
// not read the inventory.
//
// While checks are held after an unreachable server, only the refresh of
// the inventory can make a pass due, and that pass makes no checks.
func (p *processor) rebroadcastDue() bool {
	now := p.opts.now()
	if !p.checksHeld(now) && !p.rebroadcastAt.IsZero() && !now.Before(p.rebroadcastAt) {
		return true
	}
	return !now.Before(p.inventoryReadAt.Add(p.opts.rebroadcastMinDelay))
}

// checksHeld reports whether checks are held at now, after a check found the
// server unreachable.
func (p *processor) checksHeld(now time.Time) bool {
	return now.Before(p.checksHeldUntil)
}

// rebroadcast makes sure the server knows the transactions the wallet created
// that are neither mined nor expired. It runs at the start of every cycle,
// after the chain tip is updated, so that a transaction past its expiry at the
// new tip is no longer in the inventory, and between the scanned batches of a
// long cycle when a check is due.
//
// Each transaction is checked on a schedule of its own whose delay doubles
// with each check. A transaction the server does not know, or knows only in a
// block that is not in its main chain, is sent again as the same raw
// transaction. One the server has in its mempool or reports mined is left as
// it is.
//
// The pass writes nothing to the wallet. A server's report that a
// transaction is mined is not taken as the wallet's own knowledge: the
// transaction stays in the inventory, and is checked again on its schedule,
// until the wallet's scan finds it mined, a status request of the wallet
// resolves it, or it expires. A wrong report then costs only a delayed
// resend.
//
// A server that refuses a transaction or fails does not fail the cycle: the
// transaction is tried again at its next check. A check that finds the server
// unreachable holds every check, of any transaction, until the minimum delay
// after that check returned. Without the hold, each pass, at the start of a
// cycle or between its batches, would wait out the server's timeout again.
// The hold neither counts as a check nor moves a transaction's schedule; the
// inventory is still read while it lasts, so a new or expired transaction is
// noticed. Only a failure of the wallet database or a cancelled context ends
// the cycle.
func (p *processor) rebroadcast(ctx context.Context) error {
	txs, err := p.be.rebroadcastInventory()
	if err != nil {
		return err
	}
	now := p.opts.now()
	p.inventoryReadAt = now
	listed := make(map[string]struct{}, len(txs))
	stopped := p.checksHeld(now)
	for i := range txs {
		tx := &txs[i]
		key := string(tx.txid)
		listed[key] = struct{}{}
		st, ok := p.rebroadcasts[key]
		if !ok {
			if p.rebroadcasts == nil {
				p.rebroadcasts = make(map[string]*rebroadcastState)
			}
			// A transaction that has just been created was broadcast by the
			// call that created it.
			st = &rebroadcastState{nextCheck: now.Add(p.opts.rebroadcastMinDelay)}
			p.rebroadcasts[key] = st
		}
		if stopped || now.Before(st.nextCheck) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		scheduled := st.nextCheck
		st.checks++
		st.nextCheck = now.Add(backoff(st.checks+1, p.opts.rebroadcastMinDelay, p.opts.rebroadcastMaxDelay))
		wait := st.nextCheck.Sub(now).Round(time.Second)
		err := p.recheck(ctx, tx)
		var rejected *BroadcastRejectedError
		switch {
		case err == nil:
		case ctx.Err() != nil || errors.Is(err, ErrDatabase) || errors.Is(err, ErrClosed):
			return err
		case errors.Is(err, ErrServerUnreachable):
			// The check did not happen: its schedule is as it was. Every
			// check is held from the time the call returned, which can be
			// long after the pass began.
			st.checks--
			st.nextCheck = scheduled
			p.checksHeldUntil = p.opts.now().Add(p.opts.rebroadcastMinDelay)
			stopped = true
			p.log.Warnf("Transaction %s could not be checked, the server is unreachable; checks are held for %s: %v",
				displayTxID(tx.txid), p.opts.rebroadcastMinDelay, err)
		case errors.As(err, &rejected):
			p.log.Warnf("Transaction %s was refused by the server's node and is sent again in %s: %v",
				displayTxID(tx.txid), wait, err)
		default:
			p.log.Warnf("Transaction %s could not be checked or sent and is tried again in %s: %v",
				displayTxID(tx.txid), wait, err)
		}
	}
	p.rebroadcastAt = time.Time{}
	for key, st := range p.rebroadcasts {
		if _, ok := listed[key]; !ok {
			delete(p.rebroadcasts, key)
			continue
		}
		if p.rebroadcastAt.IsZero() || st.nextCheck.Before(p.rebroadcastAt) {
			p.rebroadcastAt = st.nextCheck
		}
	}
	return nil
}

// recheck asks the server about one transaction of the inventory, and sends
// it again if the server has it neither in its mempool nor in its main chain.
func (p *processor) recheck(ctx context.Context, tx *unminedTx) error {
	known, err := p.src.transaction(ctx, tx.txid)
	if err != nil {
		return err
	}
	if known != nil {
		if h := minedHeight(known); h != nil {
			p.log.Debugf("The server reports transaction %s mined at height %d", displayTxID(tx.txid), *h)
			return nil
		}
		// A height of zero is the mempool. Any other height that is not a
		// mined height is a block on a fork, and the transaction is sent
		// again for the main chain.
		if known.Height == 0 {
			return nil
		}
	}
	p.log.Infof("Broadcasting transaction %s again", displayTxID(tx.txid))
	return p.src.sendTransaction(ctx, tx.raw)
}
