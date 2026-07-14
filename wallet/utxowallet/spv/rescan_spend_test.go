// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package spv

import (
	"testing"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// testP2WPKHSpend builds a p2wkh address, its pkScript, and a transaction
// whose sole input spends an output paying that address — the witness carries
// the pubkey, so txscript.ComputePkScript can reconstruct the prevout script.
func testP2WPKHSpend(t *testing.T, seed byte) (btcutil.Address, []byte, *bisonwire.Tx) {
	t.Helper()
	pubKey := make([]byte, 33)
	pubKey[0] = 0x02
	pubKey[1] = seed
	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(pubKey), &chaincfg.RegressionNetParams,
	)
	if err != nil {
		t.Fatalf("NewAddressWitnessPubKeyHash: %v", err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript: %v", err)
	}

	dummySig := make([]byte, 71)
	dummySig[0] = 0x30
	prevOut := wire.OutPoint{Hash: chainhash.Hash{31: seed}, Index: 1}
	msgTx := wire.NewMsgTx(2)
	msgTx.AddTxIn(wire.NewTxIn(&prevOut, nil, wire.TxWitness{dummySig, pubKey}))
	msgTx.AddTxOut(wire.NewTxOut(1e8, []byte{txscript.OP_TRUE}))

	return addr, script, &bisonwire.Tx{MsgTx: *msgTx, Chain: "btc"}
}

// TestSpendsWatchedInputAddrScript verifies that a transaction spending an
// output that pays a watched address is matched from the address script
// alone, with no explicit outpoint watch. This is what keeps spends of
// sweep-ingested and re-promoted addresses' UTXOs observable: those outpoints
// never pass through paysWatchedAddr, so watchInputs has no entry for them.
func TestSpendsWatchedInputAddrScript(t *testing.T) {
	_, script, spend := testP2WPKHSpend(t, 0x5a)
	_, otherScript, _ := testP2WPKHSpend(t, 0x77)

	ro := &rescanOptions{}
	if !ro.spendsWatchedInput(spend, map[string]struct{}{string(script): {}}) {
		t.Fatal("spend of watched address script not matched")
	}
	if ro.spendsWatchedInput(spend, map[string]struct{}{string(otherScript): {}}) {
		t.Fatal("spend matched against an unrelated watched script")
	}
	if ro.spendsWatchedInput(spend, nil) {
		t.Fatal("spend matched with nothing watched")
	}
}

// TestSpendsWatchedInputExplicitWatches verifies the pre-existing matching
// paths — a zero-outpoint script watch and an exact outpoint watch — still
// hit with no watched-address scripts supplied.
func TestSpendsWatchedInputExplicitWatches(t *testing.T) {
	_, script, spend := testP2WPKHSpend(t, 0x5a)

	// Zero outpoint: match on the reconstructed prevout script.
	ro := &rescanOptions{watchInputs: []InputWithScript{{
		OutPoint: wire.OutPoint{}, PkScript: script,
	}}}
	if !ro.spendsWatchedInput(spend, nil) {
		t.Fatal("zero-outpoint script watch not matched")
	}

	// Exact outpoint: match regardless of the script bytes.
	ro = &rescanOptions{watchInputs: []InputWithScript{{
		OutPoint: spend.TxIn[0].PreviousOutPoint, PkScript: []byte{0xde},
	}}}
	if !ro.spendsWatchedInput(spend, nil) {
		t.Fatal("outpoint watch not matched")
	}
}
