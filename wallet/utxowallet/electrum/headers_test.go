// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
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

// TestValidateRetargetLocked exercises the retarget-structure trust check at a
// difficulty boundary — the decision that's NEVER reached by the rest of the
// suite because every other test runs with PoWNoRetargeting=true. This is the
// defence against a server forging difficulty across a 2016-block boundary on
// the historical span that isn't per-header PoW-checked; computeNextWorkRequired
// covers the math, this covers accept vs reject and the skip paths.
func TestValidateRetargetLocked(t *testing.T) {
	// Params with retargeting ENABLED (unlike NSimnetParams).
	params := &netparams.ChainParams{
		TargetTimespan:           14 * 24 * time.Hour, // 1,209,600s
		RetargetAdjustmentFactor: 4,
		PowLimit:                 new(big.Int).Lsh(big.NewInt(1), 255),
		PoWNoRetargeting:         false,
	}
	const boundary = int32(retargetInterval) // first retarget height

	priorBits := blockchain.BigToCompact(new(big.Int).Lsh(big.NewInt(1), 240))
	start := time.Unix(1_600_000_000, 0)
	chunkStart := &wire.BlockHeader{Bits: priorBits, Timestamp: start}
	// actualTimespan = 10 days, inside [expected/4, expected*4] so the retarget
	// is a real (non-clamped) adjustment rather than the identity.
	prior := &wire.BlockHeader{Bits: priorBits, Timestamp: start.Add(10 * 24 * time.Hour)}
	want := computeNextWorkRequired(params, prior, chunkStart)
	if want == priorBits || want == 0x207fffff {
		t.Fatalf("test setup: want 0x%08x not distinct from prior/forged bits", want)
	}

	newH := func(p *netparams.ChainParams, byHeight map[int32]*wire.BlockHeader) *Headers {
		return &Headers{
			cfg:      &ChainConfig{Params: p, StartHeight: 0, HeaderSize: 80},
			byHeight: byHeight,
		}
	}
	full := map[int32]*wire.BlockHeader{0: chunkStart, boundary - 1: prior}

	good := &wire.BlockHeader{Bits: want}
	bad := &wire.BlockHeader{Bits: 0x207fffff} // trivial-difficulty forgery

	// Accept: the boundary header carries the correctly-computed Bits.
	if err := newH(params, full).validateRetargetLocked(boundary, good, nil, 0); err != nil {
		t.Fatalf("valid retarget rejected: %v", err)
	}
	// Reject: forged easy Bits at the boundary.
	if err := newH(params, full).validateRetargetLocked(boundary, bad, nil, 0); err == nil {
		t.Fatal("forged retarget Bits accepted")
	} else if !strings.Contains(err.Error(), "retarget mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Skip — PoWNoRetargeting disables the check entirely. Construct a fresh
	// params (can't copy *params: ChainParams holds a sync.Once).
	noRT := &netparams.ChainParams{
		TargetTimespan:           params.TargetTimespan,
		RetargetAdjustmentFactor: params.RetargetAdjustmentFactor,
		PowLimit:                 params.PowLimit,
		PoWNoRetargeting:         true,
	}
	if err := newH(noRT, full).validateRetargetLocked(boundary, bad, nil, 0); err != nil {
		t.Fatalf("PoWNoRetargeting should skip: %v", err)
	}
	// Skip — non-boundary height is not checked.
	if err := newH(params, full).validateRetargetLocked(boundary-1, bad, nil, 0); err != nil {
		t.Fatalf("non-boundary height should skip: %v", err)
	}
	// Soft-pass — missing chunk-start header (predates our range).
	if err := newH(params, map[int32]*wire.BlockHeader{boundary - 1: prior}).
		validateRetargetLocked(boundary, bad, nil, 0); err != nil {
		t.Fatalf("missing chunk start should soft-pass: %v", err)
	}

	// inBatch path: prior/chunkStart resolved from the in-flight batch rather
	// than byHeight — the headerAt branch AppendBatch uses when a batch crosses
	// a boundary. The intermediate entries are never dereferenced.
	batch := make([]*wire.BlockHeader, boundary)
	batch[0] = chunkStart
	batch[boundary-1] = prior
	hEmpty := newH(params, map[int32]*wire.BlockHeader{})
	if err := hEmpty.validateRetargetLocked(boundary, good, batch, 0); err != nil {
		t.Fatalf("inBatch valid retarget rejected: %v", err)
	}
	if err := hEmpty.validateRetargetLocked(boundary, bad, batch, 0); err == nil {
		t.Fatal("inBatch forged retarget accepted")
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

// TestHeadersAnchorMove covers loadFromDisk's anchor identity check. The
// header file carries no record of which height its first entry belongs to —
// entries are indexed as StartHeight+i on every load. When a release moves
// the anchor checkpoint, a file persisted under the old anchor would be
// silently re-labeled: headers really covering blocks N..M come back indexed
// as StartHeight..StartHeight+count-1, typically overshooting the real tip so
// the first sync request returns empty and stale headers get served as
// current. loadFromDisk must instead notice that the first stored header does
// not hash to the anchor checkpoint and discard the file for a clean resync.
func TestHeadersAnchorMove(t *testing.T) {
	chain := synthChain(30, 7)
	cfgAt := func(anchor int32) *ChainConfig {
		cpHash := chain[anchor].BlockHash()
		return &ChainConfig{
			Chain:       bisonwire.ChainBTC,
			Params:      paramsWithCheckpoints([]chaincfg.Checkpoint{{Height: anchor, Hash: &cpHash}}),
			HeaderSize:  80,
			StartHeight: anchor,
		}
	}
	const seedAnchor = int32(10)
	tip := int32(len(chain) - 1)
	// seed persists headers seedAnchor..tip under the seed anchor and
	// returns the data dir for a reopen under a possibly-moved anchor.
	seed := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		h, err := NewHeaders(cfgAt(seedAnchor), dir)
		if err != nil {
			t.Fatalf("NewHeaders (seed): %v", err)
		}
		if _, err := h.AppendBatch(seedAnchor, serializeBatch(t, chain[seedAnchor:]), 0); err != nil {
			t.Fatalf("seed AppendBatch: %v", err)
		}
		return dir
	}

	t.Run("same anchor keeps stored headers", func(t *testing.T) {
		dir := seed(t)
		h, err := NewHeaders(cfgAt(seedAnchor), dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if h.Tip() != tip {
			t.Fatalf("reopen tip = %d, want %d", h.Tip(), tip)
		}
		if got := h.ByHeight(seedAnchor); got == nil || got.BlockHash() != chain[seedAnchor].BlockHash() {
			t.Fatal("anchor header missing or mismatched after reopen")
		}
	})

	for _, tc := range []struct {
		name      string
		newAnchor int32
	}{
		{"anchor bumped up discards stale file", 20},
		{"anchor bumped down discards stale file", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := seed(t)
			h, err := NewHeaders(cfgAt(tc.newAnchor), dir)
			if err != nil {
				t.Fatalf("reopen with moved anchor: %v", err)
			}
			// The stale file must be discarded, not re-indexed: the
			// reopened store starts fresh at the new anchor.
			if got, want := h.Tip(), tc.newAnchor-1; got != want {
				t.Fatalf("tip after anchor move = %d, want %d (fresh store)", got, want)
			}
			if hdr := h.ByHeight(tc.newAnchor); hdr != nil {
				t.Fatalf("unexpected header at new anchor height %d", tc.newAnchor)
			}
			// The normal first sync must be able to repopulate from the
			// new anchor.
			if _, err := h.AppendBatch(tc.newAnchor, serializeBatch(t, chain[tc.newAnchor:]), 0); err != nil {
				t.Fatalf("resync after discard: %v", err)
			}
			if h.Tip() != tip {
				t.Fatalf("resync tip = %d, want %d", h.Tip(), tip)
			}
			if got := h.ByHeight(tc.newAnchor); got == nil || got.BlockHash() != chain[tc.newAnchor].BlockHash() {
				t.Fatal("resynced anchor header mismatched")
			}
		})
	}
}

func TestHeadersFilePathLayout(t *testing.T) {
	_, dir := newTestHeaders(t)
	want := filepath.Join(dir, "electrum", "btc", "blockchain_headers")
	if _, err := os.Stat(filepath.Dir(want)); err != nil {
		t.Fatalf("expected directory %s: %v", filepath.Dir(want), err)
	}
}
