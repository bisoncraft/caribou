// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
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
// PrevBlock and is "mined" against the regtest PowLimit (Bits=0x207fffff) by
// incrementing Nonce until the block hash satisfies the target. On regtest
// this loop terminates in ~2 iterations on average.
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
		mineHeader(h)
		out[i] = h
		prev = h.BlockHash()
	}
	return out
}

// mineHeader bumps Nonce until BlockHash() satisfies hdr.Bits. For the
// regtest PowLimit (0x207fffff) this is ~2 iterations on average; a few
// hundred at worst.
func mineHeader(h *wire.BlockHeader) {
	for {
		hash := h.BlockHash()
		// 0x207fffff target is ~2^255: any hash whose top byte's
		// high bit is clear satisfies it. Use the full check via the
		// same path the production code uses so the test exercises
		// the real predicate.
		hdrs := &Headers{cfg: testChainConfig()}
		if err := hdrs.checkPoW(h); err == nil {
			_ = hash
			return
		}
		h.Nonce++
	}
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

// TestComputeNextWorkRequired pins the retarget math (review #7) so it can't
// silently drift from btcd's consensus rule. btcd's implementation isn't
// reusable standalone (unexported, or a *BlockChain method), so we validate
// our copy against the formula directly across the unclamped path, both
// timespan clamps, and the PowLimit clamp.
func TestComputeNextWorkRequired(t *testing.T) {
	const expected = int64(1_209_600) // 14 days of 10-min blocks (2016).
	const factor = int64(4)

	// Pick a prior target well below PowLimit. We canonicalize via the
	// compact round trip so expectations use the same precision the
	// production code does.
	bigPowLimit := new(big.Int).Lsh(big.NewInt(1), 255) // ~2^255, never clamps below.
	priorBits := blockchain.BigToCompact(new(big.Int).Lsh(big.NewInt(1), 240))
	priorTarget := blockchain.CompactToBig(priorBits)

	mkParams := func(powLimit *big.Int, timespan time.Duration) *netparams.ChainParams {
		return &netparams.ChainParams{
			TargetTimespan:           timespan,
			RetargetAdjustmentFactor: factor,
			PowLimit:                 powLimit,
		}
	}
	mkHeaders := func(actualTimespan int64) (prior, chunkStart *wire.BlockHeader) {
		start := time.Unix(1_600_000_000, 0)
		chunkStart = &wire.BlockHeader{Timestamp: start}
		prior = &wire.BlockHeader{Bits: priorBits, Timestamp: start.Add(time.Duration(actualTimespan) * time.Second)}
		return prior, chunkStart
	}
	// scaled returns BigToCompact(priorTarget * num / den), clamped to
	// PowLimit — the formula re-derived independently of the function body.
	scaled := func(num, den int64) uint32 {
		tgt := new(big.Int).Mul(priorTarget, big.NewInt(num))
		tgt.Div(tgt, big.NewInt(den))
		if tgt.Cmp(bigPowLimit) > 0 {
			tgt = bigPowLimit
		}
		return blockchain.BigToCompact(tgt)
	}

	tests := []struct {
		name           string
		params         *netparams.ChainParams
		actualTimespan int64
		want           uint32
	}{{
		name:           "unclamped identity (actual == expected)",
		params:         mkParams(bigPowLimit, time.Duration(expected)*time.Second),
		actualTimespan: expected,
		want:           scaled(expected, expected), // == priorBits
	}, {
		name:           "clamped to max (slow blocks)",
		params:         mkParams(bigPowLimit, time.Duration(expected)*time.Second),
		actualTimespan: expected * 100, // way over max → clamp to expected*factor.
		want:           scaled(factor, 1),
	}, {
		name:           "clamped to min (fast blocks)",
		params:         mkParams(bigPowLimit, time.Duration(expected)*time.Second),
		actualTimespan: 1, // way under min → clamp to expected/factor.
		want:           scaled(1, factor),
	}, {
		name:           "PowLimit clamp",
		params:         mkParams(new(big.Int).Lsh(big.NewInt(1), 241), time.Duration(expected)*time.Second), // 2*priorTarget
		actualTimespan: expected * 100,                                                                      // wants *4, exceeds PowLimit.
		want:           blockchain.BigToCompact(new(big.Int).Lsh(big.NewInt(1), 241)),
	}, {
		// Non-clamped, non-integer-ratio retarget — exercises the
		// multiply/divide + BigToCompact rounding that the identity and
		// clean-multiple cases don't. want is re-derived from the formula
		// with the exact actualTimespan.
		name:           "partial retarget (no clamp)",
		params:         mkParams(bigPowLimit, time.Duration(expected)*time.Second),
		actualTimespan: expected + 123457, // within [expected/4, expected*4]
		want: func() uint32 {
			tgt := new(big.Int).Mul(priorTarget, big.NewInt(expected+123457))
			tgt.Div(tgt, big.NewInt(expected))
			return blockchain.BigToCompact(tgt)
		}(),
	}, {
		name:           "zero timespan guard returns prior bits",
		params:         mkParams(bigPowLimit, 0),
		actualTimespan: expected,
		want:           priorBits,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prior, chunkStart := mkHeaders(tt.actualTimespan)
			got := computeNextWorkRequired(tt.params, prior, chunkStart)
			if got != tt.want {
				t.Fatalf("got 0x%08x, want 0x%08x", got, tt.want)
			}
		})
	}
}

// TestHeadersRejectsTrivialDifficultyForgery is the regression for review2 #8:
// a server that links headers correctly but advertises an absurdly low
// difficulty (high Bits target). With the per-header PoW check the chain
// rejects them; without it the wallet accepts a hostile chain as best.
func TestHeadersRejectsTrivialDifficultyForgery(t *testing.T) {
	h, _ := newTestHeaders(t)
	// Tight bits that almost no random hash will satisfy.
	hdr := &wire.BlockHeader{
		Version:   1,
		PrevBlock: chainhash.Hash{},
		Bits:      0x1d00ffff, // BTC mainnet starting difficulty
		Timestamp: time.Unix(1_600_000_000, 0),
		Nonce:     0,
	}
	// We did not mine it; the hash almost certainly exceeds the target.
	raw, err := serializeHeader(hdr)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if _, err := h.AppendBatch(0, raw, 0); err == nil {
		t.Fatal("expected PoW error for unmined header at tight target")
	}
}

// paramsWithCheckpoints returns a *netparams.ChainParams suitable for the
// validation tests, carrying the supplied checkpoints. We can't shallow-copy
// netparams.NSimnetParams (it has a sync.Once); construct the minimal set
// of fields the validators actually read.
func paramsWithCheckpoints(cps []chaincfg.Checkpoint) *netparams.ChainParams {
	src := netparams.NSimnetParams
	return &netparams.ChainParams{
		Chain:            src.Chain,
		Net:              src.Net,
		GenesisBlock:     src.GenesisBlock,
		GenesisHash:      src.GenesisHash,
		PowLimit:         src.PowLimit,
		PowLimitBits:     src.PowLimitBits,
		PoWNoRetargeting: src.PoWNoRetargeting,
		Checkpoints:      cps,
	}
}

// TestHeadersCheckpointMismatchRejected covers the checkpoint validator:
// when the configured params carry a checkpoint at a height in the batch
// but its hash doesn't match the appended header, AppendBatch refuses.
func TestHeadersCheckpointMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	chain := synthChain(3, 41)
	bogus := chainhash.Hash{31: 0xab}
	cfg := &ChainConfig{
		Chain:       bisonwire.ChainBTC,
		Params:      paramsWithCheckpoints([]chaincfg.Checkpoint{{Height: 2, Hash: &bogus}}),
		HeaderSize:  80,
		StartHeight: 0,
	}

	h, err := NewHeaders(cfg, dir)
	if err != nil {
		t.Fatalf("NewHeaders: %v", err)
	}
	if _, err := h.AppendBatch(0, serializeBatch(t, chain), 0); err == nil {
		t.Fatal("expected checkpoint mismatch error")
	}
}

// TestHeadersCheckpointMatchAccepted is the positive companion: configuring
// a correct checkpoint hash lets the chain through.
func TestHeadersCheckpointMatchAccepted(t *testing.T) {
	dir := t.TempDir()
	chain := synthChain(3, 42)
	good := chain[2].BlockHash()
	cfg := &ChainConfig{
		Chain:       bisonwire.ChainBTC,
		Params:      paramsWithCheckpoints([]chaincfg.Checkpoint{{Height: 2, Hash: &good}}),
		HeaderSize:  80,
		StartHeight: 0,
	}

	h, err := NewHeaders(cfg, dir)
	if err != nil {
		t.Fatalf("NewHeaders: %v", err)
	}
	if _, err := h.AppendBatch(0, serializeBatch(t, chain), 0); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
}

// TestHeadersPoWWindowSkipsDeepHistory exercises the per-header PoW window:
// with tipHint set far above the batch, headers in the batch get
// retarget+checkpoint validation only; per-header PoW is skipped. We assert
// this by feeding unmined headers and verifying they're accepted because
// they fall outside the window.
//
// Because the test ChainParams has PoWNoRetargeting=true (regtest), the
// retarget validator is a no-op. Combined with no checkpoints, this batch
// passes purely on linkage + skip-PoW.
func TestHeadersPoWWindowSkipsDeepHistory(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHeaders(testChainConfig(), dir)
	if err != nil {
		t.Fatalf("NewHeaders: %v", err)
	}
	ts := time.Unix(1_600_000_000, 0)
	var prev chainhash.Hash
	var hdrs []*wire.BlockHeader
	for i := range 5 {
		hdr := &wire.BlockHeader{
			Version:   1,
			PrevBlock: prev,
			Bits:      0x1d00ffff, // tight target — fails checkPoW
			Timestamp: ts.Add(time.Duration(i) * time.Minute),
			Nonce:     uint32(i),
		}
		hdrs = append(hdrs, hdr)
		prev = hdr.BlockHash()
	}
	// tipHint=recentPoWWindow+10 puts every header in the batch (heights
	// 0..4) outside the per-header PoW window.
	if _, err := h.AppendBatch(0, serializeBatch(t, hdrs), recentPoWWindow+10); err != nil {
		t.Fatalf("AppendBatch (deep-history skip-PoW): %v", err)
	}
}

func serializeHeader(hdr *wire.BlockHeader) ([]byte, error) {
	var buf bytes.Buffer
	if err := hdr.Serialize(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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

	n, err := h.AppendBatch(0, raw, 0)
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
	if _, err := h.AppendBatch(5, more, 0); err == nil {
		t.Fatal("expected error appending non-contiguous batch")
	}

	// Batch whose internal links are broken must fail.
	broken := []*wire.BlockHeader{
		{PrevBlock: chain[len(chain)-1].BlockHash(), Bits: 0x207fffff, Nonce: 100},
		{PrevBlock: chainhash.Hash{}, Bits: 0x207fffff, Nonce: 101}, // wrong prev
	}
	if _, err := h.AppendBatch(10, serializeBatch(t, broken), 0); err == nil {
		t.Fatal("expected error for batch with internal break")
	}
}

func TestHeadersAppendOne(t *testing.T) {
	h, _ := newTestHeaders(t)
	chain := synthChain(5, 7)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain), 0); err != nil {
		t.Fatalf("seed AppendBatch: %v", err)
	}
	// Append one connected header.
	next := &wire.BlockHeader{
		PrevBlock: chain[len(chain)-1].BlockHash(),
		Bits:      0x207fffff,
		Nonce:     500,
		Timestamp: time.Now(),
	}
	mineHeader(next)
	ok, _, err := h.AppendOne(next)
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
	if _, _, err := h.AppendOne(bad); err == nil {
		t.Fatal("expected error when rewind would underflow")
	}
}

func TestHeadersReorgRewind(t *testing.T) {
	h, _ := newTestHeaders(t)
	chain := synthChain(20, 1)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain), 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tipBefore := h.Tip()
	hashAtRewind := chain[tipBefore-int32(reorgRewindDepth)].BlockHash()

	bad := &wire.BlockHeader{PrevBlock: chainhash.Hash{31: 1}, Bits: 0x207fffff}
	ok, disc, err := h.AppendOne(bad)
	if err != nil {
		t.Fatalf("AppendOne: %v", err)
	}
	if ok {
		t.Fatal("expected rewind on unconnectable header")
	}
	// rewind should surface the disconnected blocks (review2 #1).
	if len(disc) != reorgRewindDepth {
		t.Fatalf("expected %d disconnected blocks, got %d", reorgRewindDepth, len(disc))
	}
	// Disconnected blocks are newest-first.
	if disc[0].Height != tipBefore {
		t.Fatalf("first disconnected height = %d, want %d", disc[0].Height, tipBefore)
	}
	// Tip() now reflects the truth: pre-rewind tip minus depth (review2 #2).
	if got := h.Tip(); got != tipBefore-int32(reorgRewindDepth) {
		t.Fatalf("post-rewind Tip()=%d, want %d", got, tipBefore-int32(reorgRewindDepth))
	}
	if got := h.TipHash(); got != hashAtRewind {
		t.Fatalf("post-rewind hash mismatch: got %s, want %s", got, hashAtRewind)
	}

	// Reconnect with the correct next header for the new tip.
	good := &wire.BlockHeader{
		PrevBlock: hashAtRewind,
		Bits:      0x207fffff,
		Nonce:     12345,
	}
	mineHeader(good)
	ok, _, err = h.AppendOne(good)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if !ok {
		t.Fatal("expected reconnect to succeed")
	}
	if got := h.Tip(); got != tipBefore-int32(reorgRewindDepth)+1 {
		t.Fatalf("post-reconnect tip wrong: got %d", got)
	}
}

func TestHeadersPersistence(t *testing.T) {
	h, dir := newTestHeaders(t)
	chain := synthChain(15, 42)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain), 0); err != nil {
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
