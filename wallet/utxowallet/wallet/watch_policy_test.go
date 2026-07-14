// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"testing"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/waddrmgr"
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
// leaving the failed address's skipped heights permanently unexamined.
func TestWatchPolicySweepPartialFailureFloor(t *testing.T) {
	var p watchPolicy
	ok, bad := tAddr(t, 10), tAddr(t, 11)
	p.demote(ok, 100)
	p.demote(bad, 100)

	p.bumpCold(200, []btcutil.Address{bad})

	p.mtx.Lock()
	okUpTo := p.cold[ok.String()].upTo
	badUpTo := p.cold[bad.String()].upTo
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

// TestDemoteFundedInternals verifies that a reaped internal address joins the
// cold sweep set only while it still holds an unspent credit: funded change
// is spendable by the same seed loaded in another wallet, and with the live
// watch dropped only the sweep can observe such a spend. Empty change is
// retired for good.
func TestDemoteFundedInternals(t *testing.T) {
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

	if err := w.demoteFundedInternals([]btcutil.Address{funded, empty}, 1234); err != nil {
		t.Fatalf("demoteFundedInternals: %v", err)
	}
	cold, from := w.watchPol.coldSet()
	if len(cold) != 1 || cold[0].String() != funded.String() {
		t.Fatalf("cold set = %v, want [%s]", cold, funded)
	}
	if from != 1234 {
		t.Fatalf("sweep floor = %d, want 1234", from)
	}
}
