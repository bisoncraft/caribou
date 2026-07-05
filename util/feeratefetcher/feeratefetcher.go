// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package feeratefetcher composites transaction fee-rate estimates from a set
// of ranked external sources.
//
// Sources are grouped by Rank; a lower Rank is a higher priority. The rate
// returned is a time-decay-weighted average of the valid rates in the
// highest-ranked group that has any valid data. Lower-ranked groups are only
// consulted when every source in the higher-ranked groups is errored or
// expired.
//
// Unlike a background poller, the fetcher refreshes lazily: sources are only
// queried when Rate is called and the cached data is stale, and only for as
// many rank groups as are needed to produce a value. This suits a wallet's
// on-demand fee-rate lookups, which do not warrant continuous background
// traffic to third-party APIs.
package feeratefetcher

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/bisoncraft/bosswallet/util"
)

// FetchFunc fetches a fee rate in the asset's atoms-per-byte unit. When an
// error is encountered it returns a delay indicating how long to wait before
// trying the source again (for example, longer after a rate-limit response).
type FetchFunc func(ctx context.Context) (rate uint64, errDelay time.Duration, err error)

// SourceConfig defines a fee-rate source.
type SourceConfig struct {
	// F fetches the fee rate.
	F FetchFunc
	// Name identifies the source in logs.
	Name string
	// Period is the minimum interval between successful fetches from this
	// source. A cached rate younger than Period is reused without a network
	// request.
	Period time.Duration
	// Rank controls which priority group the source is in. A lower Rank is a
	// higher priority. Rates from similarly-ranked sources are averaged into a
	// composite. Lower-ranked groups are not consulted until every source in
	// the higher-ranked groups is errored or expired.
	Rank uint
}

const (
	fetchTimeout  = time.Second * 10
	minErrorDelay = time.Minute
	// A rate is fully weighted for fullValidity, then its weight decays linearly
	// to zero over the following decayValidity. Rates older than the sum of the
	// two are not considered.
	fullValidity  = time.Minute * 5
	decayValidity = time.Minute * 10
	expiration    = fullValidity + decayValidity
	// defaultShelfLife coalesces bursts of Rate calls: after an attempt, further
	// calls reuse the cached composite for this long before another refresh pass
	// is considered. Per-source Period still governs actual network requests.
	defaultShelfLife = time.Second * 20
)

type source struct {
	*SourceConfig
	log util.Logger

	mtx       sync.Mutex
	rate      uint64
	stamp     time.Time
	failUntil time.Time
}

// due reports whether the source should be refreshed: it is not in an error
// backoff and its last successful rate is at least Period old.
func (s *source) due() bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	now := time.Now()
	if now.Before(s.failUntil) {
		return false
	}
	return s.stamp.IsZero() || now.Sub(s.stamp) >= s.Period
}

// update fetches a fresh rate, recording the result or scheduling a backoff. A
// failed or zero-valued fetch leaves any previously-cached rate in place but
// marks the source failed so it is excluded from the composite until its next
// successful refresh.
func (s *source) update(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	rate, errDelay, err := s.F(ctx)

	s.mtx.Lock()
	defer s.mtx.Unlock()
	switch {
	case err != nil:
		s.log.Meter("fetch-error", time.Minute*30).Errorf("Fetch error: %v", err)
		s.failUntil = time.Now().Add(max(minErrorDelay, errDelay))
	case rate == 0:
		s.log.Meter("zero-rate", time.Minute*30).Errorf("Fee rate of zero")
		s.failUntil = time.Now().Add(minErrorDelay)
	default:
		s.failUntil = time.Time{}
		s.stamp = time.Now()
		s.rate = rate
		s.log.Tracef("New rate %d", rate)
	}
}

// weightedRate returns the source's decay-weighted rate contribution and its
// weight. A zero weight means the source has no valid data to contribute.
func (s *source) weightedRate() (weightedRate, weight float64) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if !s.failUntil.IsZero() || s.stamp.IsZero() {
		return 0, 0
	}
	age := time.Since(s.stamp)
	if age >= expiration {
		return 0, 0
	}
	if age < fullValidity {
		return float64(s.rate), 1
	}
	w := 1 - float64(age-fullValidity)/float64(decayValidity)
	return w * float64(s.rate), w
}

// groupRate returns the decay-weighted average rate of the group, or zero if no
// source in the group has valid data.
func groupRate(group []*source) uint64 {
	var weightedRate, weight float64
	for _, s := range group {
		wr, w := s.weightedRate()
		weightedRate += wr
		weight += w
	}
	if weight == 0 {
		return 0
	}
	return max(1, uint64(math.Round(weightedRate/weight)))
}

// Fetcher composites fee rates from a set of ranked sources, refreshing them
// lazily on demand.
type Fetcher struct {
	log       util.Logger
	groups    [][]*source
	shelfLife time.Duration

	mtx         sync.Mutex
	lastAttempt time.Time
}

// New creates a Fetcher for the provided sources.
func New(sources []*SourceConfig, log util.Logger) *Fetcher {
	return &Fetcher{
		log:       log,
		groups:    groupSources(sources, log),
		shelfLife: defaultShelfLife,
	}
}

// groupSources partitions sources into rank groups ordered by ascending Rank.
func groupSources(sources []*SourceConfig, log util.Logger) [][]*source {
	groups := make([][]*source, 0)
next:
	for _, cfg := range sources {
		src := &source{SourceConfig: cfg, log: log.SubLogger(cfg.Name)}
		for i, group := range groups {
			if group[0].Rank == src.Rank {
				groups[i] = append(group, src)
				continue next
			}
		}
		groups = append(groups, []*source{src})
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i][0].Rank < groups[j][0].Rank
	})
	return groups
}

// Rate returns the current composite fee rate in the asset's atoms-per-byte
// unit, refreshing stale sources as needed. It returns zero if no source can
// currently provide a rate; callers should treat zero as "unavailable" and use
// a fallback.
func (f *Fetcher) Rate(ctx context.Context) uint64 {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	if time.Since(f.lastAttempt) >= f.shelfLife {
		f.lastAttempt = time.Now()
		f.refresh(ctx)
	}
	return f.compositeRate()
}

// refresh queries due sources one rank group at a time, stopping at the first
// group that yields a composite rate. Lower-ranked groups are only touched if
// the higher-ranked ones produce nothing.
func (f *Fetcher) refresh(ctx context.Context) {
	for _, group := range f.groups {
		var wg sync.WaitGroup
		for _, s := range group {
			if !s.due() {
				continue
			}
			wg.Add(1)
			go func(s *source) {
				defer wg.Done()
				s.update(ctx)
			}(s)
		}
		wg.Wait()
		if groupRate(group) > 0 {
			return
		}
	}
}

// compositeRate returns the rate of the highest-ranked group with valid data.
func (f *Fetcher) compositeRate() uint64 {
	for _, group := range f.groups {
		if r := groupRate(group); r > 0 {
			return r
		}
	}
	return 0
}
