// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package wait

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

// TryDirective is a response that a Waiter's TryFunc can return to instruct
// the queue to continue trying or to quit.
type TryDirective bool

const (
	// TryAgain, when returned from the Waiter's TryFunc, instructs the ticker
	// queue to try again after the configured delay.
	TryAgain TryDirective = false
	// DontTryAgain, when returned from the Waiter's TryFunc, instructs the
	// ticker queue to quit trying and quit tracking the Waiter.
	DontTryAgain TryDirective = true
)

// Waiter is a function to run every recheckInterval until completion or
// expiration. Completion is indicated when the TryFunc returns DontTryAgain.
// Expiration occurs when TryAgain is returned after Expiration time.
type Waiter struct {
	// Expiration time is checked after the function returns TryAgain. If the
	// current time > Expiration, ExpireFunc will be run and the waiter will be
	// un-queued.
	Expiration time.Time
	// TryFunc is the function to run periodically until DontTryAgain is
	// returned or Waiter expires.
	TryFunc func() TryDirective
	// ExpireFunc is a function to run in the case that the Waiter expires.
	ExpireFunc func()

	// Consider: EndFunc that runs after: (1) TryFunc returns DontTryAgain, (2)
	// ExpireFunc is run, or (3) the queue shuts down.
}

// TickerQueue is a Waiter manager that checks a function periodically until
// DontTryAgain is indicated.
type TickerQueue struct {
	waiterMtx       sync.RWMutex
	waiters         []*Waiter
	recheckInterval time.Duration
}

// NewTickerQueue is the constructor for a new TickerQueue.
func NewTickerQueue(recheckInterval time.Duration) *TickerQueue {
	return &TickerQueue{
		recheckInterval: recheckInterval,
		waiters:         make([]*Waiter, 0, 256),
	}
}

// Wait attempts to run the (*Waiter).TryFunc until either 1) the function
// returns the value DontTryAgain, or 2) the function's Expiration time has
// passed. In the case of 2, the (*Waiter).ExpireFunc will be run.
func (q *TickerQueue) Wait(w *Waiter) {
	if time.Now().After(w.Expiration) {
		log.Error("wait.TickerQueue: Waiter given expiration before present")
		return
	}
	// Check to see if it passes right away.
	if w.TryFunc() == DontTryAgain {
		return
	}
	q.waiterMtx.Lock()
	q.waiters = append(q.waiters, w)
	q.waiterMtx.Unlock()
}

// Run runs the primary wait loop until the context is canceled.
func (q *TickerQueue) Run(ctx context.Context) {
	// Expire any waiters left on shutdown.
	defer func() {
		q.waiterMtx.Lock()
		for _, w := range q.waiters {
			w.ExpireFunc()
		}
		q.waiters = q.waiters[:0]
		q.waiterMtx.Unlock()
	}()
	// The latencyTicker triggers a check of all waitFunc functions.
	latencyTicker := time.NewTicker(q.recheckInterval)
	defer latencyTicker.Stop()

	runWaiters := func() {
		q.waiterMtx.Lock()
		defer q.waiterMtx.Unlock()
		agains := make([]*Waiter, 0)
		// Grab new waiters
		tNow := time.Now()
		for _, w := range q.waiters {
			if ctx.Err() != nil {
				return
			}
			if w.TryFunc() == DontTryAgain {
				continue
			}
			// If this waiter has expired, issue the timeout error to the client
			// and do not append to the agains slice.
			if w.Expiration.Before(tNow) {
				w.ExpireFunc()
				continue
			}
			agains = append(agains, w)
		}
		q.waiters = agains
	}
out:
	for {
		select {
		case <-latencyTicker.C:
			runWaiters()
		case <-ctx.Done():
			break out
		}
	}
}

// tick speed is piecewise linear, constant at fastestInterval at or below
// fullSpeedTicks, linear from fastestInterval to slowestInterval between
// fullSpeedTicks and fullyTapered, and slowestInterval beyond that.
const (
	// fullSpeedTicks is the number of attempts that will be made with the
	// configured fastestInterval delay. After fullSpeedTicks, the retry speed
	// will be tapered off.
	fullSpeedTicks = 3
	// Once the number of attempts has reached fullyTapered, the delay between
	// attempts will be set to slowestInterval.
	fullyTapered = 15
)

type taperingWaiter struct {
	*Waiter
	// tick tracks the number of attempts that have been made and is used to
	// calculate the tapered delay.
	tick int
	// nextTick is used to sort the waiters.
	nextTick time.Time
}

// TaperingTickerQueue is a queue that will run Waiters according to a tapering-
// delay schedule. The first attempts will be more frequent, but if they are
// not successful, the delay between attempts will grow longer and longer up
// to a configurable maximum.
type TaperingTickerQueue struct {
	fastestInterval time.Duration
	slowestInterval time.Duration
	queueWaiter     chan *taperingWaiter

	// mu guards done and the decision to register in inFlight below. It is
	// held only across that fast check-and-register step, never across the
	// send itself: the send can legitimately block on a full buffer, and
	// Run's shutdown must be able to close done while that block is
	// outstanding, so mu cannot be the thing a blocked send is holding.
	//
	// A select checking done and then sending to queueWaiter cannot be made
	// race-free by reordering alone: a Wait() that passes the done check
	// can still be descheduled before its send, landing in queueWaiter
	// after Run's drain loop already stopped reading it, silently dropping
	// the waiter. inFlight closes that gap: Run's defer closes done and
	// then waits on inFlight before draining, and every Wait() that
	// observed done still open registered in inFlight (under mu, so
	// atomically with that observation) before attempting its send — so
	// Run's drain cannot start until every such Wait() has either landed in
	// queueWaiter or taken the done branch itself.
	mu       sync.Mutex
	done     chan struct{}
	inFlight sync.WaitGroup
}

// NewTaperingTickerQueue is a constructor for a TaperingTicketQueue. The
// arguments fasterInterval and slowestInterval define how the Waiter attempt
// speed is tapered. Initially, attempts will be tried every fastestInterval.
// After fullSpeedTicks, the delays will be increased until it reaches
// slowestInterval (at fullyTapered).
func NewTaperingTickerQueue(fastestInterval, slowestInterval time.Duration) *TaperingTickerQueue {
	return &TaperingTickerQueue{
		fastestInterval: fastestInterval,
		slowestInterval: slowestInterval,
		queueWaiter:     make(chan *taperingWaiter, 16),
		done:            make(chan struct{}),
	}
}

// Wait attempts to run the (*Waiter).TryFunc until either 1) the function
// returns the value DontTryAgain, or 2) the function's Expiration time has
// passed. In the case of 2, the (*Waiter).ExpireFunc will be run.
func (q *TaperingTickerQueue) Wait(waiter *Waiter) {
	if time.Now().After(waiter.Expiration) {
		log.Error("wait.TickerQueue: Waiter given expiration before present")
		return
	}
	// We don't want the caller to hang here, so we won't call TryFunc. Instead
	// set the nextTick as now and the run loop will call it in a goroutine
	// immediately.
	q.mu.Lock()
	done := q.done
	select {
	case <-done:
		q.mu.Unlock()
		waiter.ExpireFunc()
		return
	default:
	}
	q.inFlight.Add(1)
	q.mu.Unlock()
	defer q.inFlight.Done()

	// done is the snapshot taken under mu above, not a fresh read of
	// q.done: Run may restart and swap q.done concurrently, and this select
	// must keep watching the generation this call registered against in
	// inFlight, not whatever Run has moved on to.
	select {
	case q.queueWaiter <- &taperingWaiter{Waiter: waiter, nextTick: time.Now()}:
	case <-done:
		waiter.ExpireFunc()
	}
}

// Run runs the primary wait loop until the context is canceled. Run may be
// called again after a previous call has returned, e.g. across a reconnect.
func (q *TaperingTickerQueue) Run(ctx context.Context) {
	q.mu.Lock()
	// Swap in a fresh done only on a genuine restart, when the previous
	// cycle's done is already closed. On the first call, q.done is still
	// the channel NewTaperingTickerQueue created and is reused as-is:
	// unconditionally creating a new one here would orphan it if a Wait()
	// call raced in — via its own mu.Lock() — and read q.done before this
	// swap, since that call would then watch a channel this Run cycle
	// never closes, and could block on it forever.
	select {
	case <-q.done:
		q.done = make(chan struct{})
	default:
	}
	done := q.done
	q.mu.Unlock()

	var wg sync.WaitGroup
	defer func() {
		// Taking mu here makes closing done atomic with every Wait() call's
		// check-and-register: any Wait() that observes done still open under
		// mu is guaranteed to have registered in inFlight before this close
		// is visible to it.
		q.mu.Lock()
		close(done)
		q.mu.Unlock()
		// Wait for every Wait() call registered above to finish its enqueue
		// attempt (send or expire) before draining — otherwise a send that
		// wins its race with the close below would land in queueWaiter after
		// the drain already stopped reading it, right back to the bug this
		// closes. This cannot deadlock against a backpressured send: done is
		// already closed above, so every such select has a ready branch.
		q.inFlight.Wait()
		// Let every in-flight runWaiter either send back to queueWaiter or
		// observe ctx.Done() and expire on its own.
		wg.Wait()
		// Anything left in queueWaiter — from runWaiter above or a Wait()
		// that won its send race with the close above — never gets picked
		// back up: expire it.
		for {
			select {
			case w := <-q.queueWaiter:
				w.ExpireFunc()
			default:
				return
			}
		}
	}()

	runWaiter := func(w *taperingWaiter) {
		defer wg.Done()

		if w.TryFunc() == DontTryAgain {
			return
		}
		// If this waiter has expired, issue the timeout error to the client
		// and don't re-insert.
		if w.Expiration.Before(time.Now()) {
			w.ExpireFunc()
			return
		}

		w.tick++
		w.nextTick = nextTick(w.tick, q.slowestInterval, q.fastestInterval,
			time.Now(), w.Expiration)

		select {
		case q.queueWaiter <- w: // send it back to the queue
		case <-ctx.Done():
			w.ExpireFunc() // early, but still ending prior to DontTryAgain
		}
	}

	waiters := make([]*taperingWaiter, 0, 100) // only used in the loop
	var timer *time.Timer
	for {
		var tick <-chan time.Time
		if len(waiters) > 0 {
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(time.Until(waiters[0].nextTick))
			tick = timer.C
		}

		select {
		case <-tick:
			// Remove the next waiter from the slice. runWaiter will re-insert
			// with a new nextTick time if it sees TryAgain.
			w := waiters[0]
			waiters = waiters[1:]
			wg.Add(1)
			go runWaiter(w)

		case w := <-q.queueWaiter:
			// A little optimization if this waiter would fire immediately, but
			// it works to append regardless.
			if time.Until(w.nextTick) <= 0 {
				wg.Add(1)
				go runWaiter(w)
				continue
			}

			waiters = append(waiters, w)
			sort.Slice(waiters, func(i, j int) bool {
				return waiters[i].nextTick.Before(waiters[j].nextTick) // ascending, next tick first
			})

		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			for _, w := range waiters {
				w.ExpireFunc() // early, but still ending prior to DontTryAgain
			}
			return
		}
	}
}

func nextTick(ticksPassed int, slowestInterval, fastestInterval time.Duration,
	now, expiration time.Time) time.Time {
	var nextTickTime time.Time
	switch {
	case ticksPassed < fullSpeedTicks:
		nextTickTime = now.Add(fastestInterval)
	case ticksPassed < fullyTapered: // ramp up the interval
		prog := float64(ticksPassed+1-fullSpeedTicks) / (fullyTapered - fullSpeedTicks)
		taper := float64(slowestInterval - fastestInterval)
		interval := fastestInterval + time.Duration(math.Round(prog*taper))
		nextTickTime = now.Add(interval)
	default:
		nextTickTime = now.Add(slowestInterval)
	}

	if nextTickTime.After(expiration) {
		return expiration
	}
	return nextTickTime
}
