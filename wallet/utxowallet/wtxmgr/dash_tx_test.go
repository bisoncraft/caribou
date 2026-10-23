// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package wtxmgr

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// A DIP-2 special transaction: version 3, type 5, a 10-byte extra payload. The
// vector and txid are the ones bisonwire's own tests use; the txid commits to
// the packed version|type and the payload, so a plain Bitcoin serialization
// of the same transaction hashes to something else.
const (
	dashSpecialTxHex     = "03000500010000000000000000000000000000000000000000000000000000000000000000ffffffff0403abcdefffffffff010065cd1d000000001976a914111111111111111111111111111111111111111188ac000000000a0102030405060708090a"
	dashSpecialTxID      = "fbf8d764239596655ac584f166bd889ef532dbd850778345a30e814d0f11980c"
	dashSpecialExtraData = "0102030405060708090a"
)

func decodeDashSpecialTx(t *testing.T) (*bisonwire.Tx, []byte) {
	t.Helper()
	raw, err := hex.DecodeString(dashSpecialTxHex)
	if err != nil {
		t.Fatal(err)
	}
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	return tx, raw
}

// TestNewTxRecordBisonTxKeepsDashSpecialFields guards the publish path: a
// record built from a special transaction must carry its type and payload and
// serialize to the bytes that will be broadcast. Dropping them stores the
// transaction under the wrong txid and broadcasts bytes the network rejects.
func TestNewTxRecordBisonTxKeepsDashSpecialFields(t *testing.T) {
	t.Parallel()

	tx, raw := decodeDashSpecialTx(t)

	rec, err := NewTxRecordBisonTx("dash", tx, timeNow())
	if err != nil {
		t.Fatal(err)
	}

	if rec.Tx.Type != tx.Type {
		t.Errorf("record type = %d, want %d", rec.Tx.Type, tx.Type)
	}
	if got := hex.EncodeToString(rec.Tx.ExtraPayload); got != dashSpecialExtraData {
		t.Errorf("record extra payload = %q, want %q", got, dashSpecialExtraData)
	}
	if !bytes.Equal(rec.SerializedTx, raw) {
		t.Errorf("record bytes = %x, want %x", rec.SerializedTx, raw)
	}
	if got := rec.Hash.String(); got != dashSpecialTxID {
		t.Errorf("record hash = %s, want %s", got, dashSpecialTxID)
	}
	// The stored hash must also be what the record's own transaction hashes
	// to, since the store re-derives it from rec.Tx.
	if got := rec.Tx.TxHash().String(); got != dashSpecialTxID {
		t.Errorf("record tx hash = %s, want %s", got, dashSpecialTxID)
	}
}

// TestStoreRoundTripsDashSpecialTx guards the store: when a record carries no
// serialized bytes the store serializes the transaction itself, and reading it
// back must return the same type, payload and txid.
func TestStoreRoundTripsDashSpecialTx(t *testing.T) {
	t.Parallel()

	s, db, teardown, err := testStoreFor(assets.DashParams["simnet"])
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	dbtx, err := db.BeginReadWriteTx()
	if err != nil {
		t.Fatal(err)
	}
	defer dbtx.Commit()
	ns := dbtx.ReadWriteBucket(namespaceKey)

	_, raw := decodeDashSpecialTx(t)
	rec, err := NewTxRecord("dash", raw, timeNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTx(ns, stripSerializedTx(rec), nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	details, err := s.UniqueTxDetails(ns, &rec.Hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if details == nil {
		t.Fatal("transaction not found after insert")
	}

	if details.Tx.Type != rec.Tx.Type {
		t.Errorf("stored type = %d, want %d", details.Tx.Type, rec.Tx.Type)
	}
	if got := hex.EncodeToString(details.Tx.ExtraPayload); got != dashSpecialExtraData {
		t.Errorf("stored extra payload = %q, want %q", got, dashSpecialExtraData)
	}
	if got := details.Tx.TxHash().String(); got != dashSpecialTxID {
		t.Errorf("stored tx hash = %s, want %s", got, dashSpecialTxID)
	}
}

// TestDependencySortUsesMapKeysAsHashes guards ordering for transactions whose
// real txid is not the hash of their embedded MsgTx, as for a Dash special
// transaction. A child spends its parent by the real txid, so the sort must
// treat the map key as the hash or it never links the two and may return the
// child first.
func TestDependencySortUsesMapKeysAsHashes(t *testing.T) {
	t.Parallel()

	parent := wire.NewMsgTx(1)
	parent.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0}})
	parent.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	parentID := chainhash.Hash{0xaa} // not parent.TxHash()

	child := wire.NewMsgTx(1)
	child.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: parentID}})
	child.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	childID := chainhash.Hash{0xbb}

	// Map iteration order is random, so repeat to make an unlinked pair fail
	// reliably.
	for i := 0; i < 64; i++ {
		sorted := DependencySort(map[chainhash.Hash]*wire.MsgTx{
			childID:  child,
			parentID: parent,
		})
		if len(sorted) != 2 || sorted[0] != parent || sorted[1] != child {
			t.Fatalf("iteration %d: child sorted before its parent", i)
		}
	}
}

// TestUnminedBisonTxsKeepsDashSpecialFields guards the rebroadcast source: the
// unmined transactions the wallet resends after a restart must still carry
// their type and payload.
func TestUnminedBisonTxsKeepsDashSpecialFields(t *testing.T) {
	t.Parallel()

	s, db, teardown, err := testStoreFor(assets.DashParams["simnet"])
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	dbtx, err := db.BeginReadWriteTx()
	if err != nil {
		t.Fatal(err)
	}
	defer dbtx.Commit()
	ns := dbtx.ReadWriteBucket(namespaceKey)

	_, raw := decodeDashSpecialTx(t)
	rec, err := NewTxRecord("dash", raw, timeNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTx(ns, rec, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	txs, err := s.UnminedBisonTxs(ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 1 {
		t.Fatalf("got %d unmined txs, want 1", len(txs))
	}
	if txs[0].Type != 5 || hex.EncodeToString(txs[0].ExtraPayload) != dashSpecialExtraData {
		t.Errorf("unmined tx type=%d payload=%x, want type 5 and the payload",
			txs[0].Type, txs[0].ExtraPayload)
	}
}
