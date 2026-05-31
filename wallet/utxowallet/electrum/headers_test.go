// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// testChainConfig returns a synthetic ChainConfig usable for tests. We use
// the simnet params for genesis but treat StartHeight=0 with raw connectivity
// checks only; PoW is not validated.
func testChainConfig() *ChainConfig {
	return &ChainConfig{
		Chain:       bisonwire.ChainBTC,
		Params:      netparams.NSimnetParams,
		HeaderSize:  80,
		StartHeight: 0,
	}
}

// synthChain produces a chain of `count` headers starting from a synthetic
// genesis whose PrevBlock is the zero hash. Each subsequent header chains via
// PrevBlock; nothing else is meaningful. The chain is deterministic given
// `seed`.
func synthChain(count int, seed uint32) []*wire.BlockHeader {
	out := make([]*wire.BlockHeader, count)
	ts := time.Unix(1_600_000_000, 0)
	var prev chainhash.Hash
	for i := range count {
		h := &wire.BlockHeader{
			Version:    1,
			PrevBlock:  prev,
			MerkleRoot: chainhash.Hash{},
			Timestamp:  ts.Add(time.Duration(i) * time.Minute),
			Bits:       0x207fffff,
			Nonce:      seed + uint32(i),
		}
		out[i] = h
		prev = h.BlockHash()
	}
	return out
}

func serializeBatch(t *testing.T, hdrs []*wire.BlockHeader) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, h := range hdrs {
		if err := h.Serialize(&buf); err != nil {
			t.Fatalf("serialize: %v", err)
		}
	}
	return buf.Bytes()
}

// newTestHeaders constructs a Headers rooted in a temp dir.
func newTestHeaders(t *testing.T) (*Headers, string) {
	t.Helper()
	dir := t.TempDir()
	h, err := NewHeaders(testChainConfig(), dir)
	if err != nil {
		t.Fatalf("NewHeaders: %v", err)
	}
	return h, dir
}

func TestHeadersEmpty(t *testing.T) {
	h, _ := newTestHeaders(t)
	if h.Tip() != -1 {
		t.Fatalf("empty Headers tip should be -1, got %d", h.Tip())
	}
	if h.Synced() {
		t.Fatal("empty Headers should not be synced")
	}
	if (h.TipHash() != chainhash.Hash{}) {
		t.Fatal("empty Headers TipHash should be zero")
	}
	if got := h.ByHeight(0); got != nil {
		t.Fatalf("expected no header at height 0, got %+v", got)
	}
}

func TestHeadersAppendBatch(t *testing.T) {
	h, _ := newTestHeaders(t)
	chain := synthChain(10, 1)
	raw := serializeBatch(t, chain)

	n, err := h.AppendBatch(0, raw)
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if n != 10 {
		t.Fatalf("expected 10 headers appended, got %d", n)
	}
	if h.Tip() != 9 {
		t.Fatalf("expected tip 9, got %d", h.Tip())
	}
	for i, want := range chain {
		got := h.ByHeight(int32(i))
		if got == nil {
			t.Fatalf("missing header at %d", i)
		}
		if got.BlockHash() != want.BlockHash() {
			t.Fatalf("hash mismatch at %d", i)
		}
	}
	// VerifyAll should succeed.
	if err := h.VerifyAll(); err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}

	// Batch from the wrong start height must fail.
	more := serializeBatch(t, synthChain(3, 99))
	if _, err := h.AppendBatch(5, more); err == nil {
		t.Fatal("expected error appending non-contiguous batch")
	}

	// Batch whose internal links are broken must fail.
	broken := []*wire.BlockHeader{
		{PrevBlock: chain[len(chain)-1].BlockHash(), Bits: 0x207fffff, Nonce: 100},
		{PrevBlock: chainhash.Hash{}, Bits: 0x207fffff, Nonce: 101}, // wrong prev
	}
	if _, err := h.AppendBatch(10, serializeBatch(t, broken)); err == nil {
		t.Fatal("expected error for batch with internal break")
	}
}

func TestHeadersAppendOne(t *testing.T) {
	h, _ := newTestHeaders(t)
	chain := synthChain(5, 7)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain)); err != nil {
		t.Fatalf("seed AppendBatch: %v", err)
	}
	// Append one connected header.
	next := &wire.BlockHeader{
		PrevBlock: chain[len(chain)-1].BlockHash(),
		Bits:      0x207fffff,
		Nonce:     500,
		Timestamp: time.Now(),
	}
	ok, err := h.AppendOne(next)
	if err != nil {
		t.Fatalf("AppendOne (good): %v", err)
	}
	if !ok {
		t.Fatal("AppendOne returned false for a connected header")
	}
	if h.Tip() != 5 {
		t.Fatalf("tip should be 5 after AppendOne, got %d", h.Tip())
	}

	// Feeding an unconnectable header on a chain too short to rewind
	// must surface an error (the alternative would be silently dropping
	// data, which would be a bug).
	bad := &wire.BlockHeader{
		PrevBlock: chainhash.Hash{31: 1}, // garbage
		Bits:      0x207fffff,
	}
	if _, err := h.AppendOne(bad); err == nil {
		t.Fatal("expected error when rewind would underflow")
	}
}

func TestHeadersReorgRewind(t *testing.T) {
	h, _ := newTestHeaders(t)
	chain := synthChain(20, 1)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tipBefore := h.Tip()
	hashAtRewind := chain[tipBefore-int32(reorgRewindDepth)].BlockHash()

	bad := &wire.BlockHeader{PrevBlock: chainhash.Hash{31: 1}, Bits: 0x207fffff}
	ok, err := h.AppendOne(bad)
	if err != nil {
		t.Fatalf("AppendOne: %v", err)
	}
	if ok {
		t.Fatal("expected rewind on unconnectable header")
	}
	if got := h.Tip(); got != tipBefore {
		// While in recovery, Tip() should report the pre-rewind tip.
		t.Fatalf("during recovery, Tip()=%d should equal pre-rewind tip %d", got, tipBefore)
	}
	// Hash at the new actual tip (post-rewind) should be the pre-rewind
	// hash at tipBefore - depth.
	if got := h.TipHash(); got != hashAtRewind {
		t.Fatalf("post-rewind hash mismatch: got %s, want %s", got, hashAtRewind)
	}

	// Reconnect with the correct next header for the new tip.
	good := &wire.BlockHeader{
		PrevBlock: hashAtRewind,
		Bits:      0x207fffff,
		Nonce:     12345,
	}
	ok, err = h.AppendOne(good)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if !ok {
		t.Fatal("expected reconnect to succeed")
	}
	// Recovery flag should clear; Tip() now reports the actual tip again.
	if got := h.Tip(); got != tipBefore-int32(reorgRewindDepth)+1 {
		t.Fatalf("post-reconnect tip wrong: got %d", got)
	}
}

func TestHeadersPersistence(t *testing.T) {
	h, dir := newTestHeaders(t)
	chain := synthChain(15, 42)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Drop the in-memory store, reopen from disk.
	h2, err := NewHeaders(testChainConfig(), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if h2.Tip() != 14 {
		t.Fatalf("reopen tip: got %d, want 14", h2.Tip())
	}
	for i, want := range chain {
		got := h2.ByHeight(int32(i))
		if got == nil || got.BlockHash() != want.BlockHash() {
			t.Fatalf("reopen header at %d mismatch", i)
		}
	}
	if err := h2.VerifyAll(); err != nil {
		t.Fatalf("VerifyAll after reopen: %v", err)
	}
	// And ByHash should work.
	height, hdr, ok := h2.ByHash(&chain[7].PrevBlock)
	if !ok {
		// chain[7].PrevBlock is chain[6]'s hash → look that up directly
		// to assert correctness.
		want := chain[6].BlockHash()
		if _, _, ok := h2.ByHash(&want); !ok {
			t.Fatal("ByHash failed for known hash")
		}
	} else if height != 6 || hdr.BlockHash() != chain[6].BlockHash() {
		t.Fatalf("ByHash gave wrong result: height=%d", height)
	}
}

func TestHeadersFilePathLayout(t *testing.T) {
	_, dir := newTestHeaders(t)
	want := filepath.Join(dir, "electrum", "btc", "blockchain_headers")
	if _, err := os.Stat(filepath.Dir(want)); err != nil {
		t.Fatalf("expected directory %s: %v", filepath.Dir(want), err)
	}
}

func TestHeadersRange(t *testing.T) {
	h, _ := newTestHeaders(t)
	chain := synthChain(10, 1)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dst := make([]wire.BlockHeader, 4)
	n := h.Range(2, dst)
	if n != 4 {
		t.Fatalf("Range wrote %d, want 4", n)
	}
	for i, want := range chain[2:6] {
		if dst[i].BlockHash() != want.BlockHash() {
			t.Fatalf("Range[%d] mismatch", i)
		}
	}
	// Reading past the tip yields a partial copy.
	dst2 := make([]wire.BlockHeader, 4)
	n = h.Range(8, dst2)
	if n != 2 {
		t.Fatalf("Range past tip wrote %d, want 2", n)
	}
}
