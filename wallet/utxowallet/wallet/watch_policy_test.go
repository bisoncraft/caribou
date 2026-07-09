// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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
	if got := p.reap(100); len(got) != 0 {
		t.Fatalf("reap(100) = %v, want none", got)
	}

	// a1 reaps once tip-lastMined+1 >= watchFinalityDepth; a2 and a3 have
	// unmined activity and must not.
	tip := int32(100 + watchFinalityDepth - 1)
	got := p.reap(tip)
	if len(got) != 1 || got[0].String() != a1.String() {
		t.Fatalf("reap(%d) = %v, want [%s]", tip, got, a1)
	}
	// Reaped addresses are forgotten.
	if got := p.reap(tip + 100); len(got) != 0 {
		t.Fatalf("second reap returned %v, want none", got)
	}

	// a3's unmined tx confirms; both remaining reap at depth.
	p.note(a3, true, tx2, 200)
	p.note(a2, true, tx2, 200)
	tip = int32(200 + watchFinalityDepth - 1)
	if got := p.reap(tip); len(got) != 2 {
		t.Fatalf("reap(%d) = %v, want a2+a3", tip, got)
	}

	// Re-noting after a reap re-tracks (spend-from-cold re-watch path).
	p.note(a1, true, tx2, 0)
	if got := p.reap(tip + 100); len(got) != 0 {
		t.Fatalf("unmined re-track reaped: %v", got)
	}
	p.note(a1, true, tx2, 300)
	if got := p.reap(300 + watchFinalityDepth - 1); len(got) != 1 {
		t.Fatalf("re-tracked a1 did not reap")
	}
}

func TestWatchPolicyReapExternal(t *testing.T) {
	var p watchPolicy
	ext := tAddr(t, 4)
	tx := chainhash.Hash{3}

	// External addresses reap at externalQuietDepth, not watchFinalityDepth.
	p.note(ext, false, tx, 500)
	if got := p.reap(500 + watchFinalityDepth); len(got) != 0 {
		t.Fatalf("external reaped at internal depth: %v", got)
	}
	if got := p.reap(500 + externalQuietDepth - 1); len(got) != 1 {
		t.Fatalf("external did not reap at externalQuietDepth")
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
// leaving the failed address's skipped heights permanently unexamined.
func TestWatchPolicySweepPartialFailureFloor(t *testing.T) {
	var p watchPolicy
	ok, bad := tAddr(t, 10), tAddr(t, 11)
	p.demote(ok, 100)
	p.demote(bad, 100)

	p.bumpCold(200, []btcutil.Address{bad})

	p.mtx.Lock()
	okUpTo := p.coldExternal[ok.String()].upTo
	badUpTo := p.coldExternal[bad.String()].upTo
	p.mtx.Unlock()
	if okUpTo != 200 {
		t.Fatalf("verified entry floor = %d, want 200", okUpTo)
	}
	if badUpTo != 100 {
		t.Fatalf("failed entry floor advanced to %d, want 100", badUpTo)
	}
	// The next sweep must look from the failed entry's old floor.
	if _, from := p.coldSet(); from != 100 {
		t.Fatalf("next sweep floor = %d, want 100", from)
	}
}
