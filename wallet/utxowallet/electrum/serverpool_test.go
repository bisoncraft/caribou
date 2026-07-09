// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
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
	// reputation.
	p2 := newServerPool(dir, shipped)
	p2.mtx.Lock()
	e := p2.entries["disc1:1"]
	p2.mtx.Unlock()
	if e == nil || len(e.Recommenders) != 2 || e.Shipped {
		t.Fatalf("reloaded disc1 = %+v", e)
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
