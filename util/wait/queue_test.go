package wait

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTaperingQueueExpiration(t *testing.T) {
	ctx := t.Context()

	q := NewTaperingTickerQueue(time.Millisecond, time.Millisecond*10)
	go q.Run(ctx)

	var expirationTime time.Time
	var expiration sync.WaitGroup
	expiration.Add(1)

	wantExpirationTime := time.Now().Add(time.Millisecond * 10)
	q.Wait(&Waiter{
		Expiration: wantExpirationTime,
		TryFunc: func() TryDirective {
			return TryAgain
		},
		ExpireFunc: func() {
			expirationTime = time.Now()
			expiration.Done()
		},
	})

	expiration.Wait()

	if expirationTime.Before(wantExpirationTime) {
		t.Fatalf("expired at: %v - sooner than expected: %v", expirationTime, wantExpirationTime)
	}
}

func TestTaperingQueue(t *testing.T) {
	const fastestInterval, slowestInterval = 1 * time.Millisecond, 2 * time.Millisecond
	expiration := time.Now().Add(time.Minute)

	q := NewTaperingTickerQueue(fastestInterval, slowestInterval)

	// waiterTriesTimedMtx protects waiterTriesTimed from concurrent access.
	var waiterTriesTimedMtx sync.Mutex
	// waiterTriesTimed maps waiter to a list of tries, each try is represented
	// by timestamp (that reflects when waiter try starts executing).
	waiterTriesTimed := make(map[int][]time.Time, 5)
	var wgWaiters sync.WaitGroup
	addWaiter := func(waiterNumber, numTryAgains int) {
		var numTrys int
		q.Wait(&Waiter{
			Expiration: expiration,
			TryFunc: func() TryDirective {
				waiterTriesTimedMtx.Lock()
				// Record when try func was called to check it later.
				waiterTriesTimed[waiterNumber] = append(waiterTriesTimed[waiterNumber], time.Now())
				waiterTriesTimedMtx.Unlock()
				numTrys++
				if numTrys > numTryAgains {
					wgWaiters.Done()
					return DontTryAgain
				}
				return TryAgain
			},
			// We don't expect expire func being called in this test, leaving it
			// undefined so that we'll get a panic in case it gets called.
			//ExpireFunc: func() {},
		})
	}

	wgWaiters.Add(5)
	addWaiter(0, 20)
	addWaiter(1, 0)
	addWaiter(2, 10)
	addWaiter(3, 3)
	addWaiter(4, 1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	var wgQ sync.WaitGroup
	wgQ.Add(1)
	go func() {
		defer wgQ.Done()
		q.Run(ctx)
	}()

	// Wait for each waiter to get done, then stop the ticker queue itself and
	// wait for it.
	wgWaiters.Wait()
	cancel()
	wgQ.Wait()

	calcExpectedTicks := func(totalTicks int) []time.Time {
		return expTickSchedule(time.Now(), fastestInterval, slowestInterval)[:totalTicks]
	}
	// There is always at least one tick per waiter we expect (hence +1).
	expWaiterTriesTimes := map[int][]time.Time{
		0: calcExpectedTicks(20 + 1),
		1: calcExpectedTicks(0 + 1),
		2: calcExpectedTicks(10 + 1),
		3: calcExpectedTicks(3 + 1),
		4: calcExpectedTicks(1 + 1),
	}
	for waiterNumber, wantTriesTimes := range expWaiterTriesTimes {
		gotTriesTimed := waiterTriesTimed[waiterNumber]
		if len(gotTriesTimed) != len(wantTriesTimes) {
			t.Fatalf("expected waiter %d to execute %d tries, got %d instead",
				waiterNumber, len(wantTriesTimes), len(gotTriesTimed))
		}
		var (
			prevWantTryTime time.Time
			prevGotTryTimed time.Time
		)
		for i, wantTryTime := range wantTriesTimes {
			gotTryTimed := gotTriesTimed[i]
			if i == 0 {
				// Can't compare try difference for first try since there is nothing
				// to compare against.
				prevWantTryTime = wantTryTime
				prevGotTryTimed = gotTryTimed
				continue
			}
			// Check that waiter tapering works, in other words each waiter try attempt
			// doesn't execute sooner than we expect.
			// We compare the actual observed time difference between two adjacent waiter
			// try-attempts with synthetically calculated one (in wantTryDiff var), the
			// actual observed should be higher-or-equal because there is additional
			// code executing (scheduling/executing retry-attempts and such).
			wantTryDiff := wantTryTime.Sub(prevWantTryTime)
			gotTryDiff := gotTryTimed.Sub(prevGotTryTimed)
			if gotTryDiff < wantTryDiff {
				t.Fatalf("expected waiter %d to have time difference between tries %d-%d be > %v, "+
					"got time difference: %v", waiterNumber, i, i-1, wantTryDiff, gotTryDiff)
			}
			prevWantTryTime = wantTryTime
			prevGotTryTimed = gotTryTimed
		}
	}
}

// TestTaperingQueueShutdownExpiresAllWaiters cancels the queue's context
// while more waiters are in flight than the queueWaiter buffer (16) can
// hold, forcing genuine backpressure on the re-queue send in runWaiter.
// Every one of them must still receive exactly one ExpireFunc call, and Run
// must return promptly instead of hanging in wg.Wait().
//
// tryBlock holds every runWaiter goroutine inside TryFunc until every
// waiter has actually reached it, so all waiterCount goroutines are
// contending for the 16-slot buffer at the moment of cancellation instead
// of racing to finish first. Without that, waiterCount waiters at a
// 1ms interval reliably finish and requeue well before a fixed sleep
// elapses, so the test never actually created backpressure and would pass
// on any revision regardless of whether the shutdown fix worked.
func TestTaperingQueueShutdownExpiresAllWaiters(t *testing.T) {
	const waiterCount = 32 // > the queueWaiter buffer size of 16

	q := NewTaperingTickerQueue(time.Millisecond, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		q.Run(ctx)
	}()

	tryBlock := make(chan struct{})
	var inFlight atomic.Int32
	var expiredCount atomic.Int32
	for i := 0; i < waiterCount; i++ {
		q.Wait(&Waiter{
			Expiration: time.Now().Add(time.Hour),
			TryFunc: func() TryDirective {
				inFlight.Add(1)
				<-tryBlock
				return TryAgain
			},
			ExpireFunc: func() {
				expiredCount.Add(1)
			},
		})
	}

	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < waiterCount {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d waiters reached TryFunc before timeout", inFlight.Load(), waiterCount)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	close(tryBlock)

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation; likely deadlocked in wg.Wait()")
	}

	if got := expiredCount.Load(); got != waiterCount {
		t.Fatalf("expected all %d waiters to be expired on shutdown, got %d", waiterCount, got)
	}
}

// TestTaperingQueueWaitRaceWithShutdownNeverDrops races concurrent Wait()
// calls against Run's shutdown across many iterations. Every waiter must
// receive exactly one ExpireFunc call. This is the case the check-then-send
// shape in Wait could not close by reordering alone: a Wait() that passed
// the done check could still be descheduled before its send and land in
// queueWaiter after Run's drain loop had already stopped reading it,
// silently dropping the waiter — reproduced at roughly a 1-2% rate before
// mu made the check-then-send and the shutdown close-and-drain mutually
// exclusive.
func TestTaperingQueueWaitRaceWithShutdownNeverDrops(t *testing.T) {
	const iterations = 300
	const concurrency = 64

	for iter := 0; iter < iterations; iter++ {
		q := NewTaperingTickerQueue(time.Millisecond, time.Millisecond)

		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan struct{})
		go func() {
			defer close(runDone)
			q.Run(ctx)
		}()

		var expired atomic.Int32
		var started sync.WaitGroup
		started.Add(concurrency)
		var waiters sync.WaitGroup
		waiters.Add(concurrency)
		for i := 0; i < concurrency; i++ {
			go func() {
				defer waiters.Done()
				started.Done()
				q.Wait(&Waiter{
					Expiration: time.Now().Add(time.Hour),
					TryFunc: func() TryDirective {
						return TryAgain
					},
					ExpireFunc: func() {
						expired.Add(1)
					},
				})
			}()
		}
		// Only guarantees the goroutines have started, not that any have
		// reached Wait's critical section yet — cancel races the rest, which
		// is the point.
		started.Wait()
		cancel()

		waitersDone := make(chan struct{})
		go func() {
			defer close(waitersDone)
			waiters.Wait()
		}()
		select {
		case <-waitersDone:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: not all Wait() calls returned", iter)
		}

		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: Run did not return", iter)
		}

		if got := expired.Load(); got != concurrency {
			t.Fatalf("iteration %d: %d of %d waiters were never expired (silently dropped)",
				iter, got, concurrency)
		}
	}
}

// TestTaperingQueueWaitAfterRunReturnedExpiresImmediately covers Wait()
// called once Run has already returned, as opposed to
// TestTaperingQueueShutdownExpiresAllWaiters above, which only exercises
// waiters already in flight when the context is canceled. done is checked
// first and on its own in Wait(), so this must be deterministic rather than
// a coin flip between the done and queueWaiter cases of a single select.
func TestTaperingQueueWaitAfterRunReturnedExpiresImmediately(t *testing.T) {
	q := NewTaperingTickerQueue(time.Millisecond, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		q.Run(ctx)
	}()
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	const waiterCount = 400
	var expiredCount atomic.Int32
	for i := 0; i < waiterCount; i++ {
		q.Wait(&Waiter{
			Expiration: time.Now().Add(time.Hour),
			TryFunc: func() TryDirective {
				return TryAgain
			},
			ExpireFunc: func() {
				expiredCount.Add(1)
			},
		})
	}

	if got := expiredCount.Load(); got != waiterCount {
		t.Fatalf("expected all %d post-shutdown waiters to be expired, got %d", waiterCount, got)
	}
}

// TestTaperingQueueRunIsRestartable covers running the same queue again after
// a previous Run has returned, the shape a reconnect or re-login needs.
func TestTaperingQueueRunIsRestartable(t *testing.T) {
	q := NewTaperingTickerQueue(time.Millisecond, time.Millisecond)

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan struct{})
		go func() {
			defer close(runDone)
			q.Run(ctx)
		}()

		var expired atomic.Bool
		q.Wait(&Waiter{
			Expiration: time.Now().Add(time.Hour),
			TryFunc: func() TryDirective {
				return TryAgain
			},
			ExpireFunc: func() {
				expired.Store(true)
			},
		})

		cancel()
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Fatalf("Run did not return on iteration %d", i)
		}
		if !expired.Load() {
			t.Fatalf("waiter from iteration %d was never expired", i)
		}
	}
}

func Test_nextTick(t *testing.T) {
	const fastestInterval, slowestInterval = 100 * time.Millisecond, 500 * time.Millisecond
	var gotTicks []time.Time
	now := time.Now()
	expiration := now.Add(time.Hour)

	// First tick happens right away.
	gotTicks = append(gotTicks, now)
	for tick := 1; tick <= 19; tick++ {
		gotTicks = append(gotTicks, nextTick(tick, slowestInterval, fastestInterval,
			gotTicks[tick-1], expiration))
	}
	wantTicks := expTickSchedule(now, fastestInterval, slowestInterval)

	// To check expiration on the last tick.
	gotTicks = append(gotTicks, nextTick(20, slowestInterval, fastestInterval,
		expiration, expiration))
	wantTicks[len(wantTicks)-1] = expiration

	for i, want := range wantTicks {
		got := gotTicks[i]
		if want != got {
			t.Fatalf("expected tick %d to be: %v, got: %v", i, want, got)
		}
	}
}

// expTickSchedule returns expected tick schedule with a certain startTime.
func expTickSchedule(startTime time.Time, fastestInterval, slowestInterval time.Duration) []time.Time {
	expectedTicks := [21]time.Time{} // 21 element should be enough for all our needs in these tests.
	expectedTicks[0] = startTime
	expectedTicks[1] = expectedTicks[0].Add(fastestInterval)
	expectedTicks[2] = expectedTicks[1].Add(fastestInterval)

	taper := func(i int) time.Duration {
		const linearCnt = fullyTapered - fullSpeedTicks
		ramp := float64(slowestInterval - fastestInterval)
		return time.Duration(math.Round(float64(i) / linearCnt * ramp))
	}
	for i := fullSpeedTicks; i < fullyTapered-1; i++ {
		expectedTicks[i] = expectedTicks[i-1].Add(fastestInterval + taper(i-2))
	}
	for i := fullyTapered - 1; i < len(expectedTicks); i++ {
		expectedTicks[i] = expectedTicks[i-1].Add(slowestInterval)
	}
	return expectedTicks[:]
}
