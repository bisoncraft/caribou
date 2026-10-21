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
