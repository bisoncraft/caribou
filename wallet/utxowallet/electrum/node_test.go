// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/wire"
)

// TestVerifyTxidMatchesHex covers GetRawTransaction's defence against a server
// substituting different bytes for a requested txid.
func TestVerifyTxidMatchesHex(t *testing.T) {
	// Synthesize a real tx and compute its honest txid.
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0}})
	tx.AddTxOut(&wire.TxOut{Value: 100, PkScript: []byte{0x51}})
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	honestHex := hex.EncodeToString(buf.Bytes())
	honestTxid := tx.TxHash().String()

	if err := verifyTxidMatchesHex(honestTxid, honestHex); err != nil {
		t.Fatalf("honest tx rejected: %v", err)
	}

	// Wrong-hex-for-txid: rotate the requested txid by flipping one
	// hex digit.
	wrong := []byte(honestTxid)
	if wrong[0] == '0' {
		wrong[0] = '1'
	} else {
		wrong[0] = '0'
	}
	err := verifyTxidMatchesHex(string(wrong), honestHex)
	if err == nil {
		t.Fatal("expected error when bytes do not hash to requested txid")
	}
	if !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Malformed hex.
	if err := verifyTxidMatchesHex(honestTxid, "notahexstring"); err == nil {
		t.Fatal("expected error for malformed hex")
	}
}
