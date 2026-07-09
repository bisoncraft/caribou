// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package feeratefetcher

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
)

// constSource returns a source that always yields rate and counts its calls.
func constSource(name string, rank uint, rate uint64, calls *int) *SourceConfig {
	return &SourceConfig{
		Name:   name,
		Rank:   rank,
		Period: time.Hour,
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			*calls++
			return rate, 0, nil
		},
	}
}

func TestComposite(t *testing.T) {
	// Two equally-ranked sources are averaged.
	var c1, c2 int
	f := New([]*SourceConfig{
		constSource("s1", 1, 4, &c1),
		constSource("s2", 1, 6, &c2),
	}, util.Disabled)

	if r := f.Rate(context.Background()); r != 5 {
		t.Fatalf("expected composite 5, got %d", r)
	}
	// A second call within the source Period reuses the cached rates.
	if r := f.Rate(context.Background()); r != 5 {
		t.Fatalf("expected cached composite 5, got %d", r)
	}
	if c1 != 1 || c2 != 1 {
		t.Fatalf("expected one fetch per source, got %d and %d", c1, c2)
	}
}

func TestPrefersHigherRank(t *testing.T) {
	// When the rank-1 group has data, the rank-2 group is never queried.
	var hi, lo int
	f := New([]*SourceConfig{
		constSource("hi", 1, 3, &hi),
		constSource("lo", 2, 9, &lo),
	}, util.Disabled)

	if r := f.Rate(context.Background()); r != 3 {
		t.Fatalf("expected 3 from rank-1 source, got %d", r)
	}
	if lo != 0 {
		t.Fatalf("expected rank-2 source untouched, got %d calls", lo)
	}
}

func TestRankFallback(t *testing.T) {
	// The rank-1 source errors, so the rank-2 source provides the rate.
	failing := &SourceConfig{
		Name:   "down",
		Rank:   1,
		Period: time.Hour,
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			return 0, time.Minute, errors.New("down")
		},
	}
	var lo int
	f := New([]*SourceConfig{failing, constSource("lo", 2, 9, &lo)}, util.Disabled)

	if r := f.Rate(context.Background()); r != 9 {
		t.Fatalf("expected fallback rate 9, got %d", r)
	}
	if lo != 1 {
		t.Fatalf("expected rank-2 source fetched once, got %d", lo)
	}
}

func TestZeroRateIsFailure(t *testing.T) {
	// A source returning a zero rate is treated as having no data.
	var c int
	f := New([]*SourceConfig{constSource("z", 1, 0, &c)}, util.Disabled)

	if r := f.Rate(context.Background()); r != 0 {
		t.Fatalf("expected 0 from zero-rate source, got %d", r)
	}
}

func TestErrorBackoff(t *testing.T) {
	// After an error, the source is not retried until its backoff elapses.
	var c int
	src := &SourceConfig{
		Name:   "flaky",
		Rank:   1,
		Period: time.Hour,
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			c++
			return 0, time.Hour, errors.New("boom")
		},
	}
	f := New([]*SourceConfig{src}, util.Disabled)

	f.Rate(context.Background())
	f.Rate(context.Background())
	if c != 1 {
		t.Fatalf("expected a single fetch during backoff, got %d", c)
	}
}

func TestCanceledContextDoesNotBackOff(t *testing.T) {
	// A canceled parent context says nothing about source health: the source
	// must be retried on the next call rather than sitting in backoff.
	var c int
	src := &SourceConfig{
		Name:   "healthy",
		Rank:   1,
		Period: time.Hour,
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			c++
			if c == 1 {
				return 0, time.Hour, fmt.Errorf("request aborted: %w", context.Canceled)
			}
			return 7, 0, nil
		},
	}
	f := New([]*SourceConfig{src}, util.Disabled)

	if r := f.Rate(context.Background()); r != 0 {
		t.Fatalf("expected no rate from the canceled fetch, got %d", r)
	}
	if r := f.Rate(context.Background()); r != 7 {
		t.Fatalf("expected an immediate refetch after cancellation, got %d", r)
	}
	if c != 2 {
		t.Fatalf("expected 2 fetches, got %d", c)
	}
}

func TestErroredSourceServesCachedRate(t *testing.T) {
	// A source whose most recent fetch errored still contributes its last good
	// rate until that rate expires; the error only defers the next fetch.
	s := &source{
		SourceConfig: &SourceConfig{Name: "s", Rank: 1, Period: time.Hour},
		log:          util.Disabled,
		rate:         5,
		stamp:        time.Now(),
		failUntil:    time.Now().Add(time.Minute * 10), // recently errored, in backoff
	}
	if wr, w := s.weightedRate(); w == 0 || wr != 5 {
		t.Fatalf("fresh cached rate should be served despite error backoff, got rate=%v weight=%v", wr, w)
	}
	if s.due() {
		t.Fatal("source in error backoff should not be due for refetch")
	}
	// Once the cached rate is older than the expiration window, it drops out.
	s.stamp = time.Now().Add(-expiration)
	if _, w := s.weightedRate(); w != 0 {
		t.Fatal("expired cached rate should not contribute")
	}
}

func TestConcurrentCallerDoesNotBlock(t *testing.T) {
	// While one caller is blocked in a slow refresh, a concurrent caller with a
	// populated cache must return the cached composite immediately rather than
	// block on the refresh.
	enter := make(chan struct{})
	release := make(chan struct{})
	var calls int
	slow := &SourceConfig{
		Name: "slow",
		Rank: 1,
		// Period 0: due on every refresh, so the second Rate call re-fetches
		// even though the first has already populated the cache.
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			calls++
			if calls == 1 {
				return 7, 0, nil
			}
			enter <- struct{}{} // signal the fetch has started
			<-release           // block until the test releases it
			return 9, 0, nil
		},
	}
	f := New([]*SourceConfig{slow}, util.Disabled)

	if r := f.Rate(context.Background()); r != 7 { // populate the cache
		t.Fatalf("expected 7 from the priming refresh, got %d", r)
	}

	go f.Rate(context.Background()) // caller A: holds refreshMtx, blocks in F
	<-enter                         // A is now inside the fetch

	done := make(chan uint64, 1)
	go func() { done <- f.Rate(context.Background()) }() // caller B
	select {
	case r := <-done:
		if r != 7 {
			t.Fatalf("expected cached 7 while refresh in flight, got %d", r)
		}
	case <-time.After(time.Second * 2):
		t.Fatal("concurrent Rate() blocked behind an in-flight refresh")
	}
	close(release)
}

func TestColdCacheWaitsForRefresh(t *testing.T) {
	// A caller that loses the refresh race on a COLD cache has no rate to
	// return, so it must wait for the in-flight refresh rather than report
	// zero while a real rate is seconds away.
	enter := make(chan struct{})
	release := make(chan struct{})
	slow := &SourceConfig{
		Name:   "slow",
		Rank:   1,
		Period: time.Hour,
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			enter <- struct{}{} // signal the fetch has started
			<-release           // block until the test releases it
			return 7, 0, nil
		},
	}
	f := New([]*SourceConfig{slow}, util.Disabled)

	rateA := make(chan uint64, 1)
	go func() { rateA <- f.Rate(context.Background()) }() // caller A refreshes
	<-enter                                               // A is now inside the fetch

	rateB := make(chan uint64, 1)
	go func() { rateB <- f.Rate(context.Background()) }() // caller B must wait
	select {
	case r := <-rateB:
		t.Fatalf("cold-cache caller returned %d before the refresh completed", r)
	case <-time.After(time.Millisecond * 50):
	}

	close(release)
	if r := <-rateA; r != 7 {
		t.Fatalf("refreshing caller got %d, want 7", r)
	}
	if r := <-rateB; r != 7 {
		t.Fatalf("waiting caller got %d, want 7", r)
	}
}
