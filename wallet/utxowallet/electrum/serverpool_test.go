// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"strings"
	"testing"
	"time"
)

func TestOrderCandidates(t *testing.T) {
	// Isolate from package-level state left by other tests.
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.mtx.Unlock()

	a := &ServerAddr{Proto: "ssl", Addr: "a:1"}
	b := &ServerAddr{Proto: "ssl", Addr: "b:1"}
	c := &ServerAddr{Proto: "ssl", Addr: "c:1"}
	servers := []*ServerAddr{a, b, c}

	names := func(out []*ServerAddr) string {
		var b strings.Builder
		for _, sv := range out {
			b.WriteString(sv.Addr)
			b.WriteByte(' ')
		}
		return b.String()
	}

	// No health data: configured order.
	if got := names(orderCandidates(servers)); got != "a:1 b:1 c:1 " {
		t.Fatalf("clean order = %q", got)
	}

	// A recent failure demotes below the healthy ones.
	recordServerFailure(a.Addr)
	if got := names(orderCandidates(servers)); got != "b:1 c:1 a:1 " {
		t.Fatalf("after a fails = %q", got)
	}

	// Two failures: least-recently-failed of the demoted goes first.
	recordServerFailure(b.Addr)
	if got := names(orderCandidates(servers)); got != "c:1 a:1 b:1 " {
		t.Fatalf("after a then b fail = %q", got)
	}

	// Success clears the record entirely.
	recordServerSuccess(a.Addr)
	if got := names(orderCandidates(servers)); got != "a:1 c:1 b:1 " {
		t.Fatalf("after a recovers = %q", got)
	}

	// A failure past the backoff window is healthy again (configured order
	// among healthy).
	serverHealth.mtx.Lock()
	serverHealth.m[b.Addr].lastFailure = time.Now().Add(-serverFailureBackoff - time.Minute)
	serverHealth.mtx.Unlock()
	if got := names(orderCandidates(servers)); got != "a:1 b:1 c:1 " {
		t.Fatalf("after backoff expires = %q", got)
	}
}
