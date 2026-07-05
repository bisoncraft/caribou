// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package feeratefetcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
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
	f.shelfLife = 0 // consider a refresh on every call; Period still gates fetches.

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
	f.shelfLife = 0

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
	f.shelfLife = 0

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
	f.shelfLife = 0

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
	f.shelfLife = 0

	f.Rate(context.Background())
	f.Rate(context.Background())
	if c != 1 {
		t.Fatalf("expected a single fetch during backoff, got %d", c)
	}
}
