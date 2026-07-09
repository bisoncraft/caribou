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
