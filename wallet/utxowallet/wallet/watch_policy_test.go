// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/chain"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func tAddr(t *testing.T, b byte) btcutil.Address {
	t.Helper()
	var h [20]byte
	h[0] = b
	addr, err := btcutil.NewAddressWitnessPubKeyHash(h[:], &chaincfg.RegressionNetParams)
	if err != nil {
		t.Fatalf("NewAddressWitnessPubKeyHash: %v", err)
	}
	return addr
}

func TestWatchPolicyReap(t *testing.T) {
	var p watchPolicy
	a1, a2, a3 := tAddr(t, 1), tAddr(t, 2), tAddr(t, 3)
	tx1, tx2 := chainhash.Hash{1}, chainhash.Hash{2}

	// a1: mined at 100. a2: unmined. a3: mined at 100, then another unmined.
	p.note(a1, true, tx1, 100)
	p.note(a2, true, tx2, 0)
	p.note(a3, true, tx1, 100)
	p.note(a3, true, tx2, 0)

	// One block after mining: nothing is deep enough, unmined never reaps.
	if got, _ := p.reap(100); len(got) != 0 {
		t.Fatalf("reap(100) = %v, want none", got)
	}

	// a1 reaps once tip-lastMined+1 >= watchFinalityDepth; a2 and a3 have
	// unmined activity and must not.
	tip := int32(100 + watchFinalityDepth - 1)
	got, internals := p.reap(tip)
	if len(got) != 1 || got[0].String() != a1.String() {
		t.Fatalf("reap(%d) = %v, want [%s]", tip, got, a1)
	}
	// Reaped internals are returned for the caller's funded-credit check,
	// and are NOT demoted here.
	if len(internals) != 1 || internals[0].String() != a1.String() {
		t.Fatalf("internals = %v, want [%s]", internals, a1)
	}
	if cold, _ := p.coldSet(); len(cold) != 0 {
		t.Fatalf("reap demoted internal(s) %v", cold)
	}
	// Reaped addresses are forgotten.
	if got, _ := p.reap(tip + 100); len(got) != 0 {
		t.Fatalf("second reap returned %v, want none", got)
	}

	// a3's unmined tx confirms; both remaining reap at depth.
	p.note(a3, true, tx2, 200)
	p.note(a2, true, tx2, 200)
	tip = int32(200 + watchFinalityDepth - 1)
	if got, _ := p.reap(tip); len(got) != 2 {
		t.Fatalf("reap(%d) = %v, want a2+a3", tip, got)
	}

	// Re-noting after a reap re-tracks (spend-from-cold re-watch path).
	p.note(a1, true, tx2, 0)
	if got, _ := p.reap(tip + 100); len(got) != 0 {
		t.Fatalf("unmined re-track reaped: %v", got)
	}
	p.note(a1, true, tx2, 300)
	if got, _ := p.reap(300 + watchFinalityDepth - 1); len(got) != 1 {
		t.Fatalf("re-tracked a1 did not reap")
	}
}

func TestWatchPolicyReapExternal(t *testing.T) {
	var p watchPolicy
	ext := tAddr(t, 4)
	tx := chainhash.Hash{3}

	// External addresses reap at externalQuietDepth, not watchFinalityDepth.
	p.note(ext, false, tx, 500)
	if got, _ := p.reap(500 + watchFinalityDepth); len(got) != 0 {
		t.Fatalf("external reaped at internal depth: %v", got)
	}
	got, internals := p.reap(500 + externalQuietDepth - 1)
	if len(got) != 1 {
		t.Fatalf("external did not reap at externalQuietDepth")
	}
	if len(internals) != 0 {
		t.Fatalf("external returned as internal: %v", internals)
	}

	// A reaped external lands in the cold set awaiting sweeps; a sweep hit
	// promotes it back out.
	cold, from := p.coldSet()
	if len(cold) != 1 || cold[0].String() != ext.String() {
		t.Fatalf("cold set = %v, want [%s]", cold, ext)
	}
	// The reap tip was recorded as known-up-to.
	if want := int32(500 + externalQuietDepth - 1); from != want {
		t.Fatalf("sweep fromHeight = %d, want %d", from, want)
	}
	p.promote(cold)
	if got, _ := p.coldSet(); len(got) != 0 {
		t.Fatalf("cold set after promote = %v, want empty", got)
	}
}

// TestWatchPolicySweepPartialFailureFloor pins the fund-loss regression where
// a sweep with a failed per-address check advanced every cold entry's floor,
// leaving the failed address's skipped heights permanently unexamined — and
// the snapshot rule: an entry demoted after the sweep launched was not
// examined by it, so the bump must not touch it either.
func TestWatchPolicySweepPartialFailureFloor(t *testing.T) {
	var p watchPolicy
	ok, bad := tAddr(t, 10), tAddr(t, 11)
	p.demote(ok, 100, false)
	p.demote(bad, 100, false)

	_, swept := p.sweepSet(false)

	// Demoted mid-sweep: not in the snapshot, floor must survive the bump.
	late := tAddr(t, 12)
	p.demote(late, 150, false)

	p.bumpCold(200, swept, []btcutil.Address{bad})

	p.mtx.Lock()
	okUpTo := p.cold[ok.String()].upTo
	badUpTo := p.cold[bad.String()].upTo
	lateUpTo := p.cold[late.String()].upTo
	p.mtx.Unlock()
	if okUpTo != 200 {
		t.Fatalf("verified entry floor = %d, want 200", okUpTo)
	}
	if badUpTo != 100 {
		t.Fatalf("failed entry floor advanced to %d, want 100", badUpTo)
	}
	if lateUpTo != 150 {
		t.Fatalf("mid-sweep demotion floor = %d, want 150", lateUpTo)
	}
	// The next sweep must look from the failed entry's old floor.
	if _, from := p.coldSet(); from != 100 {
		t.Fatalf("next sweep floor = %d, want 100", from)
	}
}

// TestDemoteKeepsLowerFloor pins demote's floor merge: re-demoting an address
// that is already cold must never RAISE its floor — a duplicate demotion
// (e.g. the reaper re-judging an address whose known transaction a sweep
// block re-delivered) carries no new verification, and raising the floor
// would lift it past unexamined heights.
func TestDemoteKeepsLowerFloor(t *testing.T) {
	var p watchPolicy
	a := tAddr(t, 13)
	p.demote(a, 100, false)
	p.demote(a, 500, true)
	p.mtx.Lock()
	upTo, slow := p.cold[a.String()].upTo, p.cold[a.String()].slow
	p.mtx.Unlock()
	if upTo != 100 {
		t.Fatalf("floor after re-demotion = %d, want 100", upTo)
	}
	// The tier merge keeps the FASTER cadence too: a slow re-judgment must
	// not delay an entry another demotion wants examined sooner.
	if slow {
		t.Fatal("fast entry downgraded to slow by a re-demotion")
	}
	// A lower re-demotion (e.g. a discovery seed) does take effect.
	p.demote(a, 50, false)
	p.mtx.Lock()
	upTo = p.cold[a.String()].upTo
	p.mtx.Unlock()
	if upTo != 50 {
		t.Fatalf("floor after lower re-demotion = %d, want 50", upTo)
	}
}

// TestTierInternals verifies that every reaped internal address joins the
// cold sweep set, tiered by whether it still holds an unspent credit: funded
// change — spendable by the same seed loaded in another wallet — at the fast
// cadence, empty change at the slow cadence. Nothing is left unobserved.
func TestTierInternals(t *testing.T) {
	w, cleanup := testWallet(t)
	defer cleanup()

	newChange := func() (btcutil.Address, []byte) {
		a, err := w.NewChangeAddress(0, waddrmgr.KeyScopeBIP0084)
		if err != nil {
			t.Fatalf("NewChangeAddress: %v", err)
		}
		pk, err := txscript.PayToAddrScript(a)
		if err != nil {
			t.Fatalf("PayToAddrScript: %v", err)
		}
		return a, pk
	}
	funded, fundedPk := newChange()
	empty, _ := newChange()

	// A confirmed credit paying only the funded address.
	incoming := wire.NewMsgTx(1)
	incoming.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, nil, nil))
	incoming.AddTxOut(wire.NewTxOut(1e8, fundedPk))
	addUtxo(t, w, incoming)

	w.tierInternals([]btcutil.Address{funded, empty}, 1234)
	p := &w.watchPol
	p.mtx.Lock()
	fundedEntry := p.cold[funded.String()]
	emptyEntry := p.cold[empty.String()]
	p.mtx.Unlock()
	if fundedEntry == nil || emptyEntry == nil {
		t.Fatalf("cold entries missing: funded=%v empty=%v", fundedEntry, emptyEntry)
	}
	if fundedEntry.slow {
		t.Fatal("funded internal filed in the slow tier")
	}
	if !emptyEntry.slow {
		t.Fatal("empty internal not filed in the slow tier")
	}
	if fundedEntry.upTo != 1234 || emptyEntry.upTo != 1234 {
		t.Fatalf("floors = %d/%d, want 1234", fundedEntry.upTo, emptyEntry.upTo)
	}
}

// sweepStub is a chain.Backend with the AddressWatcher capability whose
// Sweep records its entry arguments and returns canned hits.
type sweepStub struct {
	*mockChainClient
	mtx   sync.Mutex
	calls [][]chain.SweepEntry
	hits  []btcutil.Address
}

func (s *sweepStub) Unwatch([]btcutil.Address) error { return nil }

func (s *sweepStub) Sweep(entries []chain.SweepEntry) (
	hits, failed []btcutil.Address, err error) {

	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.calls = append(s.calls, append([]chain.SweepEntry(nil), entries...))
	return s.hits, nil, nil
}

func (s *sweepStub) setHits(hits []btcutil.Address) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.hits = hits
}

func (s *sweepStub) sweepCalls() [][]chain.SweepEntry {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return append([][]chain.SweepEntry(nil), s.calls...)
}

// TestMaybeSweepWatermark verifies the persisted cold-set watermark: a
// completed sweep records the post-bump set minimum, an all-promoted sweep
// and an empty-set interval record the launch tip, and the Sweep call itself
// dips coldSweepReorgMargin below the verified floor.
func TestMaybeSweepWatermark(t *testing.T) {
	w, cleanup := testWallet(t)
	defer cleanup()
	p := &w.watchPol

	waitWatermark := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var got int32
			_ = walletdb.View(w.db, func(tx walletdb.ReadTx) error {
				got, _ = coldSweptTo(tx, false)
				return nil
			})
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("watermark = %d, want %d", got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitIdle := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			p.mtx.Lock()
			sweeping := p.sweeping
			p.mtx.Unlock()
			if !sweeping {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("sweep goroutine did not finish")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	stub := &sweepStub{mockChainClient: &mockChainClient{}}
	a1, a2 := tAddr(t, 20), tAddr(t, 21)
	p.demote(a1, 100, false)
	p.demote(a2, 200, false)

	// First due sweep (lastSweepTip zero). No hits: floors bump to the
	// launch tip and the watermark persists as the post-bump set minimum.
	w.maybeSweep(stub, 500)
	waitWatermark(500)
	waitIdle()

	// The backend was asked to look a reorg margin below each entry's OWN
	// floor — never below a set-wide minimum, which would re-hit every
	// higher entry's already-ingested history on every sweep.
	calls := stub.sweepCalls()
	if len(calls) != 1 || len(calls[0]) != 2 {
		t.Fatalf("sweep calls = %v, want one call with two entries", calls)
	}
	froms := make(map[string]int32, 2)
	for _, e := range calls[0] {
		froms[e.Addr.String()] = e.FromHeight
	}
	if got := froms[a1.String()]; got != 100-coldSweepReorgMargin {
		t.Fatalf("a1 fromHeight = %d, want %d", got, 100-coldSweepReorgMargin)
	}
	if got := froms[a2.String()]; got != 200-coldSweepReorgMargin {
		t.Fatalf("a2 fromHeight = %d, want %d", got, 200-coldSweepReorgMargin)
	}

	// All-promoted sweep: the cold set empties and the watermark records
	// the launch tip.
	stub.setHits([]btcutil.Address{a1, a2})
	w.maybeSweep(stub, 700)
	waitWatermark(700)
	waitIdle()
	if cold, _ := p.coldSet(); len(cold) != 0 {
		t.Fatalf("cold set = %v, want empty after promotion", cold)
	}

	// Empty-set interval: no Sweep call, but the watermark stays current so
	// the next startup's demotion floors don't regress to the last sweep.
	w.maybeSweep(stub, 900)
	waitWatermark(900)
	if calls := stub.sweepCalls(); len(calls) != 2 {
		t.Fatalf("sweep calls = %d, want 2 (empty set must not sweep)", len(calls))
	}
}

// TestSweepDueChainScaled pins the sweep-cadence resolution: the explicit
// test knob wins, then the chain-derived day (24h / target block time, the
// same value driving the external quiet depth), then the BTC-scale fallback.
func TestSweepDueChainScaled(t *testing.T) {
	var p watchPolicy
	if got := p.sweepDue(); got != sweepInterval {
		t.Fatalf("fallback sweepDue = %d, want %d", got, sweepInterval)
	}
	p.extQuietDefault = 576 // an LTC-scale day
	if got := p.sweepDue(); got != 576 {
		t.Fatalf("chain-scaled sweepDue = %d, want 576", got)
	}
	p.sweepEvery = 10
	if got := p.sweepDue(); got != 10 {
		t.Fatalf("override sweepDue = %d, want 10", got)
	}
}

// TestMaybeSweepSlowTier pins the slow tier's cadence: an empty internal in
// the slow tier is excluded from ordinary intervals and joins every
// slowSweepFactor'th one, and the slow watermark advances only when its tier
// was actually swept.
func TestMaybeSweepSlowTier(t *testing.T) {
	w, cleanup := testWallet(t)
	defer cleanup()
	p := &w.watchPol
	p.sweepEvery = 10

	waitIdle := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			p.mtx.Lock()
			sweeping := p.sweeping
			p.mtx.Unlock()
			if !sweeping {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("sweep goroutine did not finish")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	entriesOf := func(call []chain.SweepEntry) map[string]bool {
		got := make(map[string]bool, len(call))
		for _, e := range call {
			got[e.Addr.String()] = true
		}
		return got
	}

	stub := &sweepStub{mockChainClient: &mockChainClient{}}
	fast, slow := tAddr(t, 30), tAddr(t, 31)
	p.demote(fast, 100, false)
	p.demote(slow, 100, true)

	// First sweep: both cadences start at zero, so the slow tier joins.
	w.maybeSweep(stub, 1000)
	waitIdle()
	calls := stub.sweepCalls()
	if len(calls) != 1 || len(calls[0]) != 2 {
		t.Fatalf("first sweep = %v, want both tiers", calls)
	}

	// Next interval: fast due, slow not — the slow entry sits out.
	w.maybeSweep(stub, 1010)
	waitIdle()
	calls = stub.sweepCalls()
	if len(calls) != 2 {
		t.Fatalf("sweep calls = %d, want 2", len(calls))
	}
	got := entriesOf(calls[1])
	if !got[fast.String()] || got[slow.String()] {
		t.Fatalf("interval sweep = %v, want fast only", calls[1])
	}

	// The slow watermark held at the first sweep's tip while the tier sat
	// out; the fast watermark tracked the second sweep.
	_ = walletdb.View(w.db, func(tx walletdb.ReadTx) error {
		if h, _ := coldSweptTo(tx, true); h != 1000 {
			t.Fatalf("slow watermark = %d, want 1000", h)
		}
		if h, _ := coldSweptTo(tx, false); h != 1010 {
			t.Fatalf("fast watermark = %d, want 1010", h)
		}
		return nil
	})

	// At slowSweepFactor intervals past the slow tier's last sweep, it
	// rejoins.
	w.maybeSweep(stub, 1000+slowSweepFactor*10)
	waitIdle()
	calls = stub.sweepCalls()
	if len(calls) != 3 {
		t.Fatalf("sweep calls = %d, want 3", len(calls))
	}
	got = entriesOf(calls[2])
	if !got[fast.String()] || !got[slow.String()] {
		t.Fatalf("factor-due sweep = %v, want both tiers", calls[2])
	}
	_ = walletdb.View(w.db, func(tx walletdb.ReadTx) error {
		if h, _ := coldSweptTo(tx, true); h != 1000+slowSweepFactor*10 {
			t.Fatalf("slow watermark = %d, want %d", h, 1000+slowSweepFactor*10)
		}
		return nil
	})
}
