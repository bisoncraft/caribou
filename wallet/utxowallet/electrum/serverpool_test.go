// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/wire"
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

func TestBroadcastDuplicate(t *testing.T) {
	dups := []string{
		"transaction already in block chain",
		"258: txn-already-known",
		"Transaction already in the mempool",
		"tx already known",
	}
	for _, msg := range dups {
		if !broadcastDuplicate(errors.New(msg)) {
			t.Errorf("%q not classified as duplicate", msg)
		}
	}
	for _, msg := range []string{"insufficient fee", "dust", "bad-txns-inputs-missingorspent"} {
		if broadcastDuplicate(errors.New(msg)) {
			t.Errorf("%q wrongly classified as duplicate", msg)
		}
	}
}

func TestTxidFromRaw(t *testing.T) {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 1}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(5e8, []byte{0x51}))
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := txidFromRaw(bisonwire.ChainBTC, hex.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatalf("txidFromRaw: %v", err)
	}
	if want := tx.TxHash().String(); got != want {
		t.Fatalf("txid = %s, want %s", got, want)
	}
}

func TestServerPoolTiering(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.mtx.Unlock()

	dir := t.TempDir()
	shipped := []*ServerAddr{{Proto: "ssl", Addr: "ship1:1"}, {Proto: "ssl", Addr: "ship2:1"}}
	p := newServerPool(dir, shipped)

	// Discovered peers: aged+multi-recommended, aged+single, fresh.
	p.addDiscovered("recA:1", []*ServerAddr{{Proto: "ssl", Addr: "disc1:1"}, {Proto: "ssl", Addr: "disc2:1"}})
	p.addDiscovered("recB:1", []*ServerAddr{{Proto: "ssl", Addr: "disc1:1"}})
	p.mtx.Lock()
	p.entries["disc1:1"].FirstSeen = time.Now().Add(-2 * discoveredAgeMin)
	p.entries["disc2:1"].FirstSeen = time.Now().Add(-2 * discoveredAgeMin)
	p.mtx.Unlock()

	// Healthy shipped: wallet candidates are shipped-only.
	wc := p.walletCandidates()
	if len(wc) != 2 || wc[0].Addr != "ship1:1" {
		t.Fatalf("healthy wallet candidates = %v", wc)
	}

	// All shipped failing, but window not yet elapsed: still shipped-only.
	p.recordFailure("ship1:1")
	p.recordFailure("ship2:1")
	if wc := p.walletCandidates(); len(wc) != 2 {
		t.Fatalf("fallback opened before window: %v", wc)
	}

	// Backdate the sustained-down marker: fallback opens, but only for
	// disc1 (multi-recommender); disc2 (single, no successes) and fresh
	// servers stay ineligible.
	p.mtx.Lock()
	p.shippedDownSince = time.Now().Add(-shippedDownWindow - time.Minute)
	p.mtx.Unlock()
	wc = p.walletCandidates()
	if len(wc) != 3 || wc[2].Addr != "disc1:1" {
		t.Fatalf("fallback candidates = %v", wc)
	}

	// A shipped success closes the fallback.
	p.recordSuccess("ship1:1")
	if wc := p.walletCandidates(); len(wc) != 2 {
		t.Fatalf("fallback still open after shipped recovery: %v", wc)
	}

	// Observer candidates: shipped first, then discovered.
	oc := p.observerCandidates()
	if len(oc) != 4 || oc[0].Addr != "ship1:1" {
		t.Fatalf("observer candidates = %v", oc)
	}

	// Persistence round-trip: reload keeps the discovered entries and
	// reputation. Routine writes are coalesced, so force the pending one.
	p.flush()
	p2 := newServerPool(dir, shipped)
	p2.mtx.Lock()
	e := p2.entries["disc1:1"]
	p2.mtx.Unlock()
	if e == nil || len(e.Recommenders) != 2 || e.Shipped {
		t.Fatalf("reloaded disc1 = %+v", e)
	}
}

// TestServerPoolDiscoveredCaps covers the pool's flood bounds: a single
// recommender may introduce at most maxDiscoveredPerRecommender entries, the
// discovered set never grows past maxDiscovered, and entries already in the
// pool are preferred over new ones when a bound is hit.
func TestServerPoolDiscoveredCaps(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	p := newServerPool(t.TempDir(), []*ServerAddr{{Proto: "ssl", Addr: "ship:1"}})

	mk := func(prefix string, n int) []*ServerAddr {
		out := make([]*ServerAddr, n)
		for i := range out {
			out[i] = &ServerAddr{Proto: "ssl", Addr: fmt.Sprintf("%s%d:1", prefix, i)}
		}
		return out
	}
	discoveredCount := func() int {
		p.mtx.Lock()
		defer p.mtx.Unlock()
		count := 0
		for _, e := range p.entries {
			if !e.Shipped {
				count++
			}
		}
		return count
	}

	// One recommender advertising far more than its cap introduces only
	// maxDiscoveredPerRecommender entries...
	p.addDiscovered("hostile:1", mk("sybil", 100))
	if got := discoveredCount(); got != maxDiscoveredPerRecommender {
		t.Fatalf("one recommender introduced %d entries, want %d",
			got, maxDiscoveredPerRecommender)
	}
	// ...and re-advertising doesn't grow the set (the cap counts entries
	// introduced, not calls).
	p.addDiscovered("hostile:1", mk("sybil", 100))
	if got := discoveredCount(); got != maxDiscoveredPerRecommender {
		t.Fatalf("re-advertisement grew the set to %d entries", got)
	}

	// Many distinct recommenders (a sybil recommender ring) cannot grow
	// past the total cap.
	for i := 0; i < 64; i++ {
		p.addDiscovered(fmt.Sprintf("rec%d:1", i),
			mk(fmt.Sprintf("d%d-", i), maxDiscoveredPerRecommender))
	}
	if got := discoveredCount(); got != maxDiscovered {
		t.Fatalf("total discovered = %d, want cap %d", got, maxDiscovered)
	}

	// Existing entries were preferred over the flood: the first
	// recommender's entries all survived.
	p.mtx.Lock()
	survived := p.entries["sybil0:1"] != nil && p.entries["sybil15:1"] != nil
	p.mtx.Unlock()
	if !survived {
		t.Fatal("existing entries evicted by a later flood")
	}
}

// TestServerPoolLoadCap: an on-disk database holding more discovered entries
// than maxDiscovered (written before the caps, or flooded on disk) is trimmed
// at load, keeping actively quarantined entries over the rest.
func TestServerPoolLoadCap(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	dir := t.TempDir()
	shipped := []*ServerAddr{{Proto: "ssl", Addr: "ship:1"}}
	p := newServerPool(dir, shipped)
	now := time.Now()
	p.mtx.Lock()
	// Bypass addDiscovered's caps to fabricate an oversized database.
	for i := 0; i < maxDiscovered*2; i++ {
		addr := fmt.Sprintf("flood%d:1", i)
		p.entries[addr] = &poolEntry{
			Server:    &ServerAddr{Proto: "ssl", Addr: addr},
			FirstSeen: now,
			LastSeen:  now,
		}
	}
	p.entries["flood500:1"].QuarantinedUntil = now.Add(time.Hour)
	p.dirty = true
	p.flushLocked()
	p.mtx.Unlock()

	p2 := newServerPool(dir, shipped)
	p2.mtx.Lock()
	defer p2.mtx.Unlock()
	count := 0
	for _, e := range p2.entries {
		if !e.Shipped {
			count++
		}
	}
	if count != maxDiscovered {
		t.Fatalf("loaded %d discovered entries, want trim to %d", count, maxDiscovered)
	}
	if p2.entries["flood500:1"] == nil {
		t.Fatal("actively quarantined entry dropped by the load trim")
	}
	if p2.entries["ship:1"] == nil {
		t.Fatal("shipped entry dropped by the load trim")
	}
}

// TestServerPoolRuntimePrune: discovered entries past discoveredExpiry are
// pruned by later discovery passes, not only at load time — a long-lived
// process never reloads the pool.
func TestServerPoolRuntimePrune(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	p := newServerPool(t.TempDir(), []*ServerAddr{{Proto: "ssl", Addr: "ship:1"}})
	p.addDiscovered("rec:1", []*ServerAddr{{Proto: "ssl", Addr: "stale:1"}})
	p.mtx.Lock()
	e := p.entries["stale:1"]
	e.FirstSeen = time.Now().Add(-discoveredExpiry - time.Hour)
	e.LastSeen = e.FirstSeen
	p.mtx.Unlock()

	p.addDiscovered("rec:1", []*ServerAddr{{Proto: "ssl", Addr: "fresh:1"}})
	p.mtx.Lock()
	_, staleKept := p.entries["stale:1"]
	_, freshKept := p.entries["fresh:1"]
	_, shipKept := p.entries["ship:1"]
	p.mtx.Unlock()
	if staleKept {
		t.Fatal("expired discovered entry not pruned at runtime")
	}
	if !freshKept || !shipKept {
		t.Fatal("prune removed a live entry")
	}
}

// TestServerPoolSaveDebounce: bursts of routine reputation updates coalesce
// into a bounded number of file writes, while a quarantine is written
// through immediately.
func TestServerPoolSaveDebounce(t *testing.T) {
	oldDelay := saveDelay
	saveDelay = 20 * time.Millisecond
	t.Cleanup(func() { saveDelay = oldDelay })
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	dir := t.TempDir()
	shipped := []*ServerAddr{{Proto: "ssl", Addr: "dbn1:1"}, {Proto: "ssl", Addr: "dbn2:1"}}
	p := newServerPool(dir, shipped)

	writes := func() int {
		p.mtx.Lock()
		defer p.mtx.Unlock()
		return p.saveCount
	}

	// A burst of updates: far fewer writes than updates, and the full
	// count eventually persists.
	const burst = 100
	for i := 0; i < burst; i++ {
		p.recordFailure("dbn1:1")
	}
	if w := writes(); w > 1 {
		t.Fatalf("burst of %d updates wrote %d times before the delay elapsed", burst, w)
	}
	waitFor(t, "coalesced write to land", func() bool {
		p2 := newServerPool(dir, shipped)
		p2.mtx.Lock()
		defer p2.mtx.Unlock()
		return p2.entries["dbn1:1"].Failures == burst
	})
	if w := writes(); w > 2 {
		t.Fatalf("burst of %d updates took %d writes, want coalescing", burst, w)
	}

	// Quarantine persists synchronously — the write count bumps before the
	// call returns, no delay to survive.
	before := writes()
	p.quarantine("dbn2:1", "debounce test")
	if w := writes(); w != before+1 {
		t.Fatalf("quarantine wrote %d times, want exactly 1 immediate write", w-before)
	}
	p3 := newServerPool(dir, shipped)
	p3.mtx.Lock()
	reason := p3.entries["dbn2:1"].QuarantineReason
	p3.mtx.Unlock()
	if reason != "debounce test" {
		t.Fatalf("quarantine not persisted immediately: reason = %q", reason)
	}
}

func TestServerPoolQuarantine(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	dir := t.TempDir()
	shipped := []*ServerAddr{{Proto: "ssl", Addr: "q1:1"}, {Proto: "ssl", Addr: "q2:1"}}
	p := newServerPool(dir, shipped)

	p.quarantine("q1:1", "checkpoint mismatch")

	// Excluded from both elections outright — no least-recently-failed
	// mercy.
	if wc := p.walletCandidates(); len(wc) != 1 || wc[0].Addr != "q2:1" {
		t.Fatalf("wallet candidates with quarantine = %v", wc)
	}
	if oc := p.observerCandidates(); len(oc) != 1 || oc[0].Addr != "q2:1" {
		t.Fatalf("observer candidates with quarantine = %v", oc)
	}

	// In-process, the blacklist holds even past the persisted term.
	p.mtx.Lock()
	p.entries["q1:1"].QuarantinedUntil = time.Now().Add(-time.Minute)
	p.mtx.Unlock()
	if wc := p.walletCandidates(); len(wc) != 1 {
		t.Fatalf("process-lifetime quarantine did not hold: %v", wc)
	}

	// The persisted term survives a reload (fresh process: in-memory
	// blacklist cleared). The pool file still carries the original
	// 24-hour term written at quarantine time.
	serverHealth.mtx.Lock()
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()
	p2 := newServerPool(dir, shipped)
	if wc := p2.walletCandidates(); len(wc) != 1 || wc[0].Addr != "q2:1" {
		t.Fatalf("reloaded wallet candidates = %v", wc)
	}
	p2.mtx.Lock()
	if r := p2.entries["q1:1"].QuarantineReason; r != "checkpoint mismatch" {
		t.Fatalf("persisted reason = %q", r)
	}
	// An expired persisted term readmits (new process, >24h later).
	p2.entries["q1:1"].QuarantinedUntil = time.Now().Add(-time.Minute)
	p2.mtx.Unlock()
	if wc := p2.walletCandidates(); len(wc) != 2 {
		t.Fatalf("expired quarantine still excluding: %v", wc)
	}
}

// TestFallbackOpensOnQuarantine: quarantined shipped servers count as down
// for the discovered-fallback gate, and an all-quarantined shipped list
// opens it without waiting out shippedDownWindow. A quarantined server is
// never re-dialed, so its failure record aged out of the backoff window and
// the gate never opened — the wallet sat degraded with usable discovered
// servers on file.
func TestFallbackOpensOnQuarantine(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	s1 := &ServerAddr{Proto: "ssl", Addr: "s1:1"}
	s2 := &ServerAddr{Proto: "ssl", Addr: "s2:1"}
	p := newServerPool(t.TempDir(), []*ServerAddr{s1, s2})
	t.Cleanup(p.flush)

	// An aged, previously successful discovered server: fallback-eligible
	// on its own merits.
	aged := &ServerAddr{Proto: "ssl", Addr: "aged:1"}
	p.addDiscovered("rec:1", []*ServerAddr{aged})
	p.mtx.Lock()
	p.entries[aged.Addr].FirstSeen = time.Now().Add(-discoveredAgeMin - time.Hour)
	p.entries[aged.Addr].Successes = 1
	p.mtx.Unlock()

	names := func(out []*ServerAddr) string {
		var b strings.Builder
		for _, sv := range out {
			b.WriteString(sv.Addr)
			b.WriteByte(' ')
		}
		return b.String()
	}

	if got := names(p.walletCandidates()); got != "s1:1 s2:1 " {
		t.Fatalf("healthy shipped list = %q", got)
	}

	// One quarantined, one freshly failing: every shipped server is down,
	// but the window has not elapsed.
	p.quarantine(s1.Addr, "test")
	p.recordFailure(s2.Addr)
	if got := names(p.walletCandidates()); got != "s2:1 " {
		t.Fatalf("inside the window = %q", got)
	}

	// Window elapsed: the aged discovered server joins the election.
	p.mtx.Lock()
	p.shippedDownSince = time.Now().Add(-shippedDownWindow - time.Minute)
	p.mtx.Unlock()
	if got := names(p.walletCandidates()); got != "s2:1 aged:1 " {
		t.Fatalf("after the window = %q", got)
	}

	// Every shipped server quarantined: the fallback opens at once.
	p.mtx.Lock()
	p.shippedDownSince = time.Time{}
	p.mtx.Unlock()
	p.quarantine(s2.Addr, "test")
	if got := names(p.walletCandidates()); got != "aged:1 " {
		t.Fatalf("all quarantined = %q", got)
	}
}
