// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
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

	// Trailing bytes after a valid tx must be rejected. Deserialize stops at
	// the tx boundary, so without the explicit trailing-length check a server
	// could append arbitrary bytes to a valid response undetected.
	if err := verifyTxidMatchesHex(honestTxid, honestHex+"deadbeef"); err == nil {
		t.Fatal("expected error for trailing bytes after the transaction")
	} else if !strings.Contains(err.Error(), "trailing bytes") {
		t.Fatalf("unexpected error for trailing bytes: %v", err)
	}
}

// TestVerifyTxidMatchesHexSegWit is the regression for the review-flagged bug:
// the previous implementation hashed the raw network serialization, which for
// a segwit tx is the wtxid, not the txid — so every segwit tx was rejected.
// The honest segwit serialization (marker/flag/witness present) must verify
// against the tx's real txid.
func TestVerifyTxidMatchesHexSegWit(t *testing.T) {
	// A v2 tx with a witness on its input. Its full serialization includes
	// the segwit marker/flag and the witness stack; TxHash() (and therefore
	// the txid) hashes the witness-stripped form.
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhashFromByte(0xab), Index: 0},
		Witness: wire.TxWitness{
			[]byte{0x30, 0x44, 0x02, 0x20}, // dummy signature bytes
			[]byte{0x02, 0x03, 0x04, 0x05}, // dummy pubkey bytes
		},
	})
	tx.AddTxOut(&wire.TxOut{Value: 4242, PkScript: []byte{0x00, 0x14, 0x01, 0x02}})

	var full bytes.Buffer
	if err := tx.Serialize(&full); err != nil { // witness-inclusive
		t.Fatalf("serialize: %v", err)
	}
	// Sanity: the witness-inclusive serialization must differ from the
	// witness-stripped one, otherwise the test wouldn't exercise the bug.
	var stripped bytes.Buffer
	if err := tx.SerializeNoWitness(&stripped); err != nil {
		t.Fatalf("serialize no witness: %v", err)
	}
	if bytes.Equal(full.Bytes(), stripped.Bytes()) {
		t.Fatal("test tx has no witness; cannot exercise segwit path")
	}

	fullHex := hex.EncodeToString(full.Bytes())
	if err := verifyTxidMatchesHex(tx.TxHash().String(), fullHex); err != nil {
		t.Fatalf("honest segwit tx rejected: %v", err)
	}
}

// chainhashFromByte builds a deterministic hash filled with b for test fixtures.
func chainhashFromByte(b byte) (h chainhash.Hash) {
	for i := range h {
		h[i] = b
	}
	return h
}

// TestServerPeersBounded: a server advertising more peers than
// maxAdvertisedPeers gets only the cap parsed; the rest of the reply is
// ignored rather than becoming pool candidates.
func TestServerPeersBounded(t *testing.T) {
	f, params := newFakeServer(t, 3)
	peers := make([][]interface{}, 0, maxAdvertisedPeers*3)
	for i := 0; i < maxAdvertisedPeers*3; i++ {
		peers = append(peers, []interface{}{
			"203.0.113.1",
			fmt.Sprintf("peer%d.example", i),
			[]interface{}{"v1.4", "s50002"},
		})
	}
	f.mtx.Lock()
	f.peers = peers
	f.mtx.Unlock()

	node, err := NewNode(t.Context(), f.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	defer node.Close()
	got, err := node.ServerPeers(t.Context())
	if err != nil {
		t.Fatalf("ServerPeers: %v", err)
	}
	if len(got) != maxAdvertisedPeers {
		t.Fatalf("parsed %d peers, want the %d cap", len(got), maxAdvertisedPeers)
	}
}

// TestOversizedFrameDropsConnection: a single reply frame past maxFrameSize
// must not grow the read buffer without bound. The read loop treats it as
// hostile: the in-flight request fails and the connection is dropped (the
// reconnect/rotation machinery owns replacement).
func TestOversizedFrameDropsConnection(t *testing.T) {
	f, params := newFakeServer(t, 3)

	node, err := NewNode(t.Context(), f.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	defer node.Close()

	// Enough advertised peers to push one server.peers reply past
	// maxFrameSize (each entry is ~140 bytes of JSON). Installed after the
	// handshake so NewNode itself is unaffected.
	entry := []interface{}{
		"203.0.113.1",
		strings.Repeat("a", 100) + ".example",
		[]interface{}{"v1.4", "s50002"},
	}
	peers := make([][]interface{}, maxFrameSize/140+1024)
	for i := range peers {
		peers[i] = entry
	}
	f.mtx.Lock()
	f.peers = peers
	f.mtx.Unlock()

	if _, err := node.ServerPeers(t.Context()); err == nil {
		t.Fatal("oversized reply returned no error")
	}
	select {
	case <-node.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("connection not dropped after an oversized frame")
	}
}
