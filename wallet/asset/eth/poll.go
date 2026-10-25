// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
)

const (
	// activePollInterval is how often a wallet in use polls for a new tip.
	activePollInterval = 15 * time.Second
	// idlePollInterval is how often a wallet not in use polls for a new tip.
	idlePollInterval = 3 * time.Minute
	// simnetActivePollInterval and simnetIdlePollInterval replace the poll
	// intervals on simnet, where the node is local and requests cost nothing.
	simnetActivePollInterval = time.Second
	simnetIdlePollInterval   = 10 * time.Second
	// activeHold is how long a wallet stays active after the last sign of
	// use: an alert call, including Alert, or a poll that found a pending
	// tx.
	activeHold = 5 * time.Minute
)

// pollIntervals returns the active and idle poll intervals for the network.
func pollIntervals(net util.Network) (active, idle time.Duration) {
	if net == util.Simnet {
		return simnetActivePollInterval, simnetIdlePollInterval
	}
	return activePollInterval, idlePollInterval
}

var _ asset.Alerter = (*ETHWallet)(nil)
var _ asset.Alerter = (*TokenWallet)(nil)

// pollSchedule decides how often the wallet polls its network. The wallet is
// active for activeHold after the last sign of use and idle otherwise. A poll
// fetches the latest header and runs applyTipChange on it, and every refresh
// of on-chain state follows from that one call: core's balance refresh on
// TipChange, the pending tx check, and the pending approval check.
//
// The zero value polls at the default intervals but cannot be woken early.
// NewEVMWallet creates the wake channel.
type pollSchedule struct {
	// active, idle and hold replace the package defaults when non-zero.
	active, idle, hold time.Duration
	// now replaces time.Now when non-nil.
	now func() time.Time

	mtx         sync.Mutex
	activeUntil time.Time

	// wake requests an immediate poll. With a capacity of one, requests made
	// while one is already pending coalesce into a single poll.
	wake chan struct{}
}

func (s *pollSchedule) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// activeInterval is the poll interval while the wallet is active.
func (s *pollSchedule) activeInterval() time.Duration {
	return durationOr(s.active, activePollInterval)
}

// interval is the time until the next scheduled poll.
func (s *pollSchedule) interval() time.Duration {
	s.mtx.Lock()
	active := s.clock().Before(s.activeUntil)
	s.mtx.Unlock()
	if active {
		return s.activeInterval()
	}
	return durationOr(s.idle, idlePollInterval)
}

// extend keeps the wallet active until the hold period from now has passed,
// and reports whether it was idle before the call.
func (s *pollSchedule) extend() (wasIdle bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	now := s.clock()
	wasIdle = !now.Before(s.activeUntil)
	if until := now.Add(durationOr(s.hold, activeHold)); until.After(s.activeUntil) {
		s.activeUntil = until
	}
	return wasIdle
}

// requestPoll asks the poll loop to poll now. It never blocks.
func (s *pollSchedule) requestPoll() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Alert is the exported alert call, for signals of use that do not go through
// any other wallet method, such as the user viewing the asset. Unlike alert,
// it polls immediately even when the wallet is already active, which also
// restarts the poll schedule. Satisfies asset.Alerter. A token wallet shares
// its parent's schedule, so an alert to either applies to the chain.
func (w *baseWallet) Alert() {
	w.alert()
	w.poll.requestPoll()
}

// alert marks a sign of use: a call core makes on behalf of the user, or an
// event the user is waiting on, such as funds arriving. Every such method is
// an alert call and calls alert first. It keeps the wallet active and, if the
// wallet was idle, requests an immediate poll. It does not
// wait for the poll, so the call that triggered it may still see the old tip.
// It never blocks, so it is safe on the poll goroutine, where approval
// callbacks run.
//
// Nothing that core calls on a timer or on every tip change may call alert.
// Balance and SyncStatus in particular must not: core calls them on every tip
// change and peer count change, so the wallet would never go idle.
func (w *baseWallet) alert() {
	if w.poll.extend() {
		w.poll.requestPoll()
	}
}

// hasPendingWork reports whether the wallet is waiting on the network to
// finalize a tx. A tx waiting on a user decision, assumed lost, or already
// final but not yet pruned does not count, so that one stuck tx cannot hold
// the wallet active indefinitely.
//
// Pending approvals and bridges do not count on their own. An approval
// confirms before its tx is final, so its tx holds the wallet active for as
// long as the approval check needs new tips. Bridges progress on the bridge
// manager's own timer, and their txs are pending txs.
func (w *baseWallet) hasPendingWork() bool {
	w.nonceMtx.RLock()
	defer w.nonceMtx.RUnlock()
	for _, pendingTx := range w.pendingTxs {
		if !pendingTx.actionRequested && !pendingTx.AssumedLost && !pendingTx.Confirmed {
			return true
		}
	}
	return false
}

// holdIfPending keeps the wallet active while it has pending work.
func (w *baseWallet) holdIfPending() {
	if w.hasPendingWork() {
		w.poll.extend()
	}
}

// monitorBlocks polls for a new tip on the schedule set by eth.poll until ctx
// is canceled. Pending work found at startup or after any poll keeps the
// wallet active.
func (eth *ETHWallet) monitorBlocks(ctx context.Context) {
	eth.holdIfPending()
	timer := time.NewTimer(eth.poll.interval())
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
		case <-eth.poll.wake:
		case <-ctx.Done():
			return
		}
		eth.pollTip(ctx)
		eth.holdIfPending()
		// Reset after every poll, whatever woke the loop, so that an early
		// poll restarts the schedule.
		timer.Reset(eth.poll.interval())
	}
}

// pollTip returns recovered providers to service, then fetches the latest
// header and applies it. The node bounds each request to a provider. A
// deadline over the whole poll would expire while one provider stalls, and
// every provider tried after it would fail and be quarantined.
func (eth *ETHWallet) pollTip(ctx context.Context) {
	eth.node.probeFailedProviders(ctx)
	hdr, err := eth.node.bestHeader(ctx)
	if err != nil {
		if ctx.Err() == nil {
			eth.log.Errorf("failed to get best header: %v", err)
		}
		return
	}
	eth.applyTipChange(hdr)
}
