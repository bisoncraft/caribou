// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

func TestHeightForTime(t *testing.T) {
	// The fake chain's block at height h has the time 1,700,000,000 + h.
	e := newEnv(t, fakeKind, 1_000, nil)
	margin := MinBirthdayMargin
	at := func(secs int64) time.Time { return time.Unix(secs, 0).Add(margin) }
	tests := []struct {
		name   string
		t      time.Time
		margin time.Duration
		want   uint32
	}{
		{"a block's own time", at(1_700_000_500), margin, 500},
		{"between blocks", at(1_700_000_500).Add(500 * time.Millisecond), margin, 500},
		{"a longer margin", at(1_700_000_500), margin + 100*time.Second, 400},
		{"after the tip", at(1_800_000_000), margin, 1_000},
		{"before every block", at(1_600_000_000), margin, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := e.srv.callCount("GetBlock")
			got, err := e.w.HeightForTime(context.Background(), tt.t, tt.margin)
			if err != nil {
				t.Fatalf("HeightForTime: %v", err)
			}
			if got != tt.want {
				t.Errorf("height %d, want %d", got, tt.want)
			}
			if n := e.srv.callCount("GetBlock") - before; n > 6 {
				t.Errorf("%d blocks fetched", n)
			}
		})
	}
	for _, m := range []time.Duration{-time.Hour, 0, MinBirthdayMargin - time.Second} {
		if _, err := e.w.HeightForTime(context.Background(), at(1_700_000_500), m); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("a margin of %s: %v", m, err)
		}
	}
}

// Block times that are not evenly spaced still lead to the right height,
// with bisection where interpolation makes little progress.
func TestHeightForTimeWithUnevenBlockTimes(t *testing.T) {
	e := newEnv(t, fakeKind, 4_000, nil)
	// The first 3,000 blocks come a second apart, the rest an hour apart.
	e.srv.set(func(s *fakeLwd) {
		for h := 3_001; h < len(s.blocks); h++ {
			s.blocks[h].Time = s.blocks[3_000].Time + uint32(h-3_000)*3_600
		}
	})
	want := uint32(1_234)
	got, err := e.w.HeightForTime(context.Background(), time.Unix(1_700_000_000+int64(want), 0).Add(MinBirthdayMargin), MinBirthdayMargin)
	if err != nil || got != want {
		t.Fatalf("height %d, %v; want %d", got, err, want)
	}
	if n := e.srv.callCount("GetBlock"); n > 40 {
		t.Errorf("%d blocks fetched", n)
	}
	got, err = e.w.HeightForTime(context.Background(), time.Unix(int64(e.srv.blocks[3_500].Time)+10, 0).Add(MinBirthdayMargin), MinBirthdayMargin)
	if err != nil || got != 3_500 {
		t.Fatalf("height %d, %v; want 3500", got, err)
	}
}

// setBlockTimes gives the fake chain's blocks from height from on the
// given times.
func setBlockTimes(e *env, from int, times []uint32) {
	e.srv.set(func(s *fakeLwd) {
		for i, bt := range times {
			s.blocks[from+i].Time = bt
		}
	})
}

// On block times out of height order the search returns a height whose time
// is at most the target, or the floor, so that with the minimum margin no
// block below it is at or after the caller's time.
func TestHeightForTimeOnBlockTimesOutOfOrder(t *testing.T) {
	ctx := context.Background()
	t.Run("examples", func(t *testing.T) {
		for _, tc := range []struct {
			times  []uint32
			target int64
			valid  []uint32
		}{
			{[]uint32{1000, 1500, 1200, 1700, 2000}, 1250, []uint32{100, 102}},
			{[]uint32{1000, 1600, 1400, 1600, 2000}, 1500, []uint32{100, 102}},
		} {
			e := newEnv(t, fakeKind, 104, nil)
			// The floor is 100, and the blocks below it are earlier.
			setBlockTimes(e, 0, make([]uint32, 100))
			setBlockTimes(e, 100, tc.times)
			got, err := heightForTime(ctx, dialFake(t, e.srv), 100, tc.target)
			if err != nil {
				t.Fatalf("heightForTime: %v", err)
			}
			if !slices.Contains(tc.valid, got) {
				t.Errorf("times %v, target %d: height %d, want one of %v", tc.times, tc.target, got, tc.valid)
			}
		}
	})

	t.Run("consensus times", func(t *testing.T) {
		const n = 3_000
		const maxAhead = 90 * 60
		e := newEnv(t, fakeKind, n-1, nil)
		src := dialFake(t, e.srv)
		rng := rand.New(rand.NewPCG(1, 2))
		for round := range 20 {
			// Times that obey consensus: each above the median of the
			// eleven before it and at most 90 minutes above that median,
			// about 75 seconds apart on average.
			times := make([]uint32, n)
			times[0] = 1_700_000_000
			for h := 1; h < n; h++ {
				window := slices.Clone(times[max(0, h-11):h])
				slices.Sort(window)
				median := window[len(window)/2]
				ideal := times[0] + uint32(h)*75
				lo, hi := median+1, median+maxAhead
				bt := ideal + uint32(rng.IntN(2*maxAhead)) - maxAhead
				times[h] = min(max(bt, lo), hi)
			}
			setBlockTimes(e, 0, times)
			for range 10 {
				target := int64(times[0]) + rng.Int64N(int64(times[n-1]-times[0]))
				got, err := heightForTime(ctx, src, 1, target)
				if err != nil {
					t.Fatal(err)
				}
				if got != 1 && int64(times[got]) > target {
					t.Fatalf("round %d, target %d: height %d has the time %d", round, target, got, times[got])
				}
				callerTime := target + int64(MinBirthdayMargin/time.Second)
				for h := 1; h < int(got); h++ {
					if int64(times[h]) >= callerTime {
						t.Fatalf("round %d, target %d: block %d below the result %d has the time %d, at or after %d",
							round, target, h, got, times[h], callerTime)
					}
				}
			}
		}
	})
}

// The servers are tried in turn, and one on another consensus branch is
// passed over, as by CreateAccount.
func TestHeightForTimeChecksItsServer(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	servers[0].set(func(s *fakeLwd) { s.branch = otherBranch })
	got, err := e.w.HeightForTime(context.Background(), time.Unix(1_700_000_120, 0).Add(MinBirthdayMargin), MinBirthdayMargin)
	if err != nil || got != 120 {
		t.Fatalf("height %d, %v; want 120", got, err)
	}
	onlyInfo(t, "the server on another branch", servers[0])

	e, servers = serverEnv(t, 150, 1)
	servers[0].set(func(s *fakeLwd) { s.branch = otherBranch })
	if _, err := e.w.HeightForTime(context.Background(), time.Now(), MinBirthdayMargin); !errors.Is(err, ErrConsensusBranch) {
		t.Errorf("one server on another branch: %v", err)
	}
}

// A chain whose tip is below the floor gives the floor.
func TestHeightForTimeBelowTheFloor(t *testing.T) {
	e := newEnv(t, fakeKind, 50, nil)
	got, err := heightForTime(context.Background(), dialFake(t, e.srv), 80, 1_700_000_010)
	if err != nil || got != 80 {
		t.Fatalf("height %d, %v; want 80", got, err)
	}
}
