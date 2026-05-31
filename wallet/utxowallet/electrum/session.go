// Copyright (c) 2024 The Bison Wallet developers
// Copyright (c) The go-electrum-client authors
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ElectrumX anti-DDoS cost tracking. The server tracks a per-session "cost"
// derived from bytes sent/received, error count, and concurrent in-flight
// requests. Above a soft limit the server delays responses; above a hard limit
// it disconnects. We can't query our cost, so we estimate it locally and warn
// when the connection is at risk so the caller can rotate to a fresh node.
//
// See electrumx/server/session.py upstream for the original parameters.

const (
	costSoftLimit   = 2_000.0
	costHardLimit   = 10_000.0
	costPerByte     = 1.0 / 100_000.0
	costPerError    = 100.0
	costDecayPerSec = costHardLimit / 3_600.0

	// costDecayDivisor reduces the decay rate. We are intentionally
	// conservative so our estimate stays an upper bound on the server's
	// view.
	costDecayDivisor = 10
)

// session tracks an estimated per-connection electrumx cost.
type session struct {
	cost atomic.Uint64 // float64 bits, accessed via math.Float64{bits,frombits}
	mu   sync.Mutex    // serializes bump+decay (read-modify-write)
}

func newSession() *session {
	return &session{}
}

// start launches the cost-decay loop. It returns immediately; the loop exits
// when ctx is cancelled.
func (s *session) start(ctx context.Context) {
	go s.decayLoop(ctx)
}

func (s *session) decayLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if tick%costDecayDivisor == 0 {
				s.bump(-costDecayPerSec)
			}
			tick++
		}
	}
}

// Cost returns the current estimated session cost.
func (s *session) Cost() float64 {
	return math.Float64frombits(s.cost.Load())
}

// SoftLimitExceeded reports whether the estimated cost has crossed the soft
// limit. Callers may wish to rotate connections when this returns true.
func (s *session) SoftLimitExceeded() bool {
	return s.Cost() > costSoftLimit
}

func (s *session) bump(delta float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := math.Float64frombits(s.cost.Load()) + delta
	if c < 0 {
		c = 0
	}
	s.cost.Store(math.Float64bits(c))
}

func (s *session) bumpBytes(n int)      { s.bump(float64(n) * costPerByte) }
func (s *session) bumpString(s2 string) { s.bumpBytes(utf8.RuneCountInString(s2)) }
func (s *session) bumpStruct(v any) {
	b, _ := json.Marshal(v)
	s.bumpBytes(len(b))
}
func (s *session) bumpError() { s.bump(costPerError) }
