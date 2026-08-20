// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"testing"
	"time"
)

// TestTipPollOnTick pins monitorBlocks' ticker decision. Two independent
// deadlines feed it — a dead-feed fallback and a cross-check for providers that
// never joined the feed — and getting either wrong is silent: too eager and the
// per-second poll the PR removes comes straight back, too lax and a wallet whose
// subscriptions have all died stops noticing new blocks.
func TestTipPollOnTick(t *testing.T) {
	// An arbitrary fixed instant. The decision is entirely relative, so nothing
	// here needs a real clock.
	now := time.Unix(1_700_000_000, 0)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	for _, tt := range []struct {
		name       string
		feed       bool
		lastWSHead time.Time
		lastPoll   time.Time
		want       tipPollDecision
	}{{
		// HTTP-only and simnet. The timestamps are irrelevant with no feed, and
		// the poll must run on the loop: there is nothing to hold up.
		name:       "no feed polls on the loop",
		lastWSHead: now,
		lastPoll:   now,
		want:       tipPollBlocking,
	}, {
		name:     "no feed at startup",
		lastPoll: now,
		want:     tipPollBlocking,
	}, {
		// The first tick of a wallet that does have a feed. lastWSHead is zero
		// until a real header lands, so the fallback holds the 1s cadence
		// rather than trusting a channel that has proved nothing yet.
		name:     "feed that has not delivered yet",
		feed:     true,
		lastPoll: now,
		want:     tipPollBackground,
	}, {
		name:       "feed delivering and recently polled",
		feed:       true,
		lastWSHead: ago(time.Second),
		lastPoll:   ago(time.Second),
		want:       tipPollNone,
	}, {
		// The steady state this whole change exists to produce: a live feed
		// with the cross-check not yet due does no work at all.
		name:       "feed delivering just inside both deadlines",
		feed:       true,
		lastWSHead: ago(wsHeadStaleness - time.Millisecond),
		lastPoll:   ago(headerCheckInterval - time.Millisecond),
		want:       tipPollNone,
	}, {
		// Every subscription on the shared channel may be dead.
		name:       "feed gone quiet",
		feed:       true,
		lastWSHead: ago(wsHeadStaleness),
		lastPoll:   ago(time.Second),
		want:       tipPollBackground,
	}, {
		// The feed is fine; this is the cross-check for a provider that fell
		// back to HTTP and so pushes nothing onto the channel.
		name:       "live feed due its cross-check",
		feed:       true,
		lastWSHead: ago(time.Second),
		lastPoll:   ago(headerCheckInterval),
		want:       tipPollBackground,
	}, {
		name:       "both deadlines past",
		feed:       true,
		lastWSHead: ago(2 * wsHeadStaleness),
		lastPoll:   ago(2 * headerCheckInterval),
		want:       tipPollBackground,
	}} {
		t.Run(tt.name, func(t *testing.T) {
			got := tipPollOnTick(tt.feed, tt.lastWSHead, tt.lastPoll, now)
			if got != tt.want {
				t.Fatalf("tipPollOnTick = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTipPollDeadlinesOrdered guards the relationship the two deadlines have to
// keep. The staleness fallback has to fire well before the cross-check, or a
// dead feed would be caught by the cross-check first and the fallback would
// never be what noticed — and its whole reason for existing is to bound
// blindness to something far shorter than headerCheckInterval.
func TestTipPollDeadlinesOrdered(t *testing.T) {
	if wsHeadStaleness >= headerCheckInterval {
		t.Fatalf("wsHeadStaleness %v is not shorter than headerCheckInterval %v",
			wsHeadStaleness, headerCheckInterval)
	}
	// It also has to exceed the slowest block time the EVM wallet drives,
	// ethereum's ~12s, by enough margin that ordinary variance does not read as
	// a dead feed.
	const slowestBlockTime = 12 * time.Second
	if wsHeadStaleness < 2*slowestBlockTime {
		t.Fatalf("wsHeadStaleness %v leaves less than 2x margin over a %v block time",
			wsHeadStaleness, slowestBlockTime)
	}
}
