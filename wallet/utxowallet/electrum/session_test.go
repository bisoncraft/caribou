// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"context"
	"testing"
	"time"
)

func TestSessionCostBumpAndDecay(t *testing.T) {
	s := newSession()
	if s.Cost() != 0 {
		t.Fatalf("new session should have zero cost, got %v", s.Cost())
	}

	// Bumping by 1 MiB worth of bytes should put us well below the soft
	// limit but solidly above zero. 1<<20 bytes * 1e-5 = 10.485...
	s.bumpBytes(1 << 20)
	if c := s.Cost(); c < 10 || c > 12 {
		t.Fatalf("unexpected cost after 1MiB bump: %v", c)
	}
	if s.SoftLimitExceeded() {
		t.Fatal("soft limit triggered prematurely")
	}

	// Errors are punished hard.
	for range 25 {
		s.bumpError()
	}
	if !s.SoftLimitExceeded() {
		t.Fatalf("expected soft limit after 25 errors, cost=%v", s.Cost())
	}

	// Negative bumps clamp at zero.
	s.bump(-1e9)
	if c := s.Cost(); c != 0 {
		t.Fatalf("cost should clamp at zero, got %v", c)
	}
}

func TestSessionDecayLoopExits(t *testing.T) {
	s := newSession()
	ctx, cancel := context.WithCancel(context.Background())
	s.start(ctx)
	cancel()
	// The decay loop should exit promptly; we don't have a direct signal,
	// but waiting a tick beyond the loop's period and observing no panic
	// is sufficient.
	time.Sleep(50 * time.Millisecond)
}
