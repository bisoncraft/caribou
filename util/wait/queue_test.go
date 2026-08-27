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
// hold, some re-queuing and some blocked in Wait(). Every one of them must
// still receive exactly one ExpireFunc call, and Run must return promptly
// instead of hanging in wg.Wait().
func TestTaperingQueueShutdownExpiresAllWaiters(t *testing.T) {
	const waiterCount = 32 // > the queueWaiter buffer size of 16

	q := NewTaperingTickerQueue(time.Millisecond, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		q.Run(ctx)
	}()

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

	// Let the queue churn through a few ticks so waiters are spread across
	// the waiters slice, in-flight runWaiter goroutines, and the buffered
	// channel before shutdown begins.
	time.Sleep(5 * time.Millisecond)
	cancel()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation; likely deadlocked in wg.Wait()")
	}

	if got := expiredCount.Load(); got != waiterCount {
		t.Fatalf("expected all %d waiters to be expired on shutdown, got %d", waiterCount, got)
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
