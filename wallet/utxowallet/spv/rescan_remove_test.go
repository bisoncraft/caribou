// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package spv

import (
	"bytes"
	"testing"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv/headerfs"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// testWitnessAddr builds a distinct p2wkh address (and its script) from a
// single seed byte.
func testWitnessAddr(t *testing.T, seed byte) (btcutil.Address, []byte) {
	t.Helper()
	var h20 [20]byte
	h20[0] = seed
	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		h20[:], &chaincfg.RegressionNetParams,
	)
	if err != nil {
		t.Fatalf("NewAddressWitnessPubKeyHash: %v", err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript: %v", err)
	}
	return addr, script
}

// TestUpdateFilterRemoveAddrs verifies that a RemoveAddrs update set-differences
// the dropped address (and its script) out of the live watchAddrs/watchList
// while leaving the rest of the watch set intact. This is the mechanism that
// lets an aged wallet shed cold addresses from the per-block cfilter match.
func TestUpdateFilterRemoveAddrs(t *testing.T) {
	addr1, script1 := testWitnessAddr(t, 0x01)
	addr2, script2 := testWitnessAddr(t, 0x02)
	addr3, script3 := testWitnessAddr(t, 0x03)

	ro := &rescanOptions{}

	// Seed the filter with all three addresses via an add-only update
	// (rewind 0, so the chain argument is never dereferenced).
	add := defaultUpdateOptions()
	AddAddrs(addr1, addr2, addr3)(add)
	if _, err := ro.updateFilter(nil, add, &headerfs.BlockStamp{}, &wire.BlockHeader{}); err != nil {
		t.Fatalf("updateFilter(add): %v", err)
	}
	if len(ro.watchAddrs) != 3 || len(ro.watchList) != 3 {
		t.Fatalf("after add: watchAddrs=%d watchList=%d, want 3/3",
			len(ro.watchAddrs), len(ro.watchList))
	}

	// Drop the middle address.
	rm := defaultUpdateOptions()
	RemoveAddrs(addr2)(rm)
	if _, err := ro.updateFilter(nil, rm, &headerfs.BlockStamp{}, &wire.BlockHeader{}); err != nil {
		t.Fatalf("updateFilter(remove): %v", err)
	}

	// addr2 is gone; addr1 and addr3 remain, order preserved.
	if len(ro.watchAddrs) != 2 {
		t.Fatalf("watchAddrs=%d, want 2", len(ro.watchAddrs))
	}
	if ro.watchAddrs[0].EncodeAddress() != addr1.EncodeAddress() ||
		ro.watchAddrs[1].EncodeAddress() != addr3.EncodeAddress() {
		t.Fatalf("watchAddrs=%v, want [%s %s]", ro.watchAddrs,
			addr1.EncodeAddress(), addr3.EncodeAddress())
	}
	for _, addr := range ro.watchAddrs {
		if addr.EncodeAddress() == addr2.EncodeAddress() {
			t.Fatalf("removed addr %s still watched", addr2.EncodeAddress())
		}
	}

	// The corresponding script is gone from the raw watchList too, and the
	// survivors' scripts are still present.
	if len(ro.watchList) != 2 {
		t.Fatalf("watchList=%d, want 2", len(ro.watchList))
	}
	var have1, have3, have2 bool
	for _, s := range ro.watchList {
		switch {
		case bytes.Equal(s, script1):
			have1 = true
		case bytes.Equal(s, script2):
			have2 = true
		case bytes.Equal(s, script3):
			have3 = true
		}
	}
	if have2 {
		t.Fatal("removed script still in watchList")
	}
	if !have1 || !have3 {
		t.Fatalf("survivor scripts missing: have1=%v have3=%v", have1, have3)
	}
}

// TestUpdateFilterRemoveAddrsPrunesInputs verifies that a RemoveAddrs update
// also drops the input watches whose prevout script belongs to a removed
// address, so demoting an address doesn't strand unreachable watchInputs
// entries (the watchList gate would never match their blocks again).
func TestUpdateFilterRemoveAddrsPrunesInputs(t *testing.T) {
	addr1, script1 := testWitnessAddr(t, 0x01)
	addr2, script2 := testWitnessAddr(t, 0x02)
	op1 := wire.OutPoint{Index: 1}
	op2 := wire.OutPoint{Index: 2}

	ro := &rescanOptions{}
	add := defaultUpdateOptions()
	AddAddrs(addr1, addr2)(add)
	AddInputs(
		InputWithScript{OutPoint: op1, PkScript: script1},
		InputWithScript{OutPoint: op2, PkScript: script2},
	)(add)
	if _, err := ro.updateFilter(nil, add, &headerfs.BlockStamp{}, &wire.BlockHeader{}); err != nil {
		t.Fatalf("updateFilter(add): %v", err)
	}
	if len(ro.watchInputs) != 2 || len(ro.watchList) != 4 {
		t.Fatalf("after add: watchInputs=%d watchList=%d, want 2/4",
			len(ro.watchInputs), len(ro.watchList))
	}

	rm := defaultUpdateOptions()
	RemoveAddrs(addr1)(rm)
	if _, err := ro.updateFilter(nil, rm, &headerfs.BlockStamp{}, &wire.BlockHeader{}); err != nil {
		t.Fatalf("updateFilter(remove): %v", err)
	}

	if len(ro.watchInputs) != 1 || ro.watchInputs[0].OutPoint != op2 {
		t.Fatalf("watchInputs=%v, want [%v]", ro.watchInputs, op2)
	}
	if len(ro.watchList) != 2 {
		t.Fatalf("watchList=%d, want 2", len(ro.watchList))
	}
	for _, s := range ro.watchList {
		if bytes.Equal(s, script1) {
			t.Fatal("removed script still in watchList")
		}
	}
}

// TestUpdateFilterRemoveUnknownAddr verifies that removing an address that was
// never watched is a harmless no-op — the existing watch set is untouched.
func TestUpdateFilterRemoveUnknownAddr(t *testing.T) {
	addr1, _ := testWitnessAddr(t, 0x11)
	other, _ := testWitnessAddr(t, 0x22)

	ro := &rescanOptions{}
	add := defaultUpdateOptions()
	AddAddrs(addr1)(add)
	if _, err := ro.updateFilter(nil, add, &headerfs.BlockStamp{}, &wire.BlockHeader{}); err != nil {
		t.Fatalf("updateFilter(add): %v", err)
	}

	rm := defaultUpdateOptions()
	RemoveAddrs(other)(rm)
	if _, err := ro.updateFilter(nil, rm, &headerfs.BlockStamp{}, &wire.BlockHeader{}); err != nil {
		t.Fatalf("updateFilter(remove-unknown): %v", err)
	}

	if len(ro.watchAddrs) != 1 || ro.watchAddrs[0].EncodeAddress() != addr1.EncodeAddress() {
		t.Fatalf("watchAddrs=%v, want [%s]", ro.watchAddrs, addr1.EncodeAddress())
	}
	if len(ro.watchList) != 1 {
		t.Fatalf("watchList=%d, want 1", len(ro.watchList))
	}
}
