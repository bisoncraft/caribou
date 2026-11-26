// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
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

// testRetargetInterval is the retarget period of intervalTestParams, short
// so tests can cross several boundaries cheaply.
const testRetargetInterval = 16

// intervalTestParams returns params that enforce an interval difficulty rule
// (Bitcoin's, or Litecoin's sampling when litecoin is set) with a
// testRetargetInterval-block period and PowLimit 2^255-1, so mining stays a
// few hashes per header.
func intervalTestParams(cps []netparams.Checkpoint, litecoin bool) *netparams.ChainParams {
	src := netparams.NSimnetParams
	p := &netparams.ChainParams{
		Chain:                    src.Chain,
		Name:                     "intervaltest",
		Net:                      src.Net,
		GenesisHash:              src.GenesisHash,
		PowLimit:                 new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1)),
		PowLimitBits:             0x207fffff,
		TargetTimePerBlock:       time.Minute,
		TargetTimespan:           testRetargetInterval * time.Minute,
		RetargetAdjustmentFactor: 4,
		Checkpoints:              cps,
	}
	if litecoin {
		p.NextBits = assets.LitecoinNextBits(p)
		p.DifficultyLookback = 1
	} else {
		p.NextBits = assets.BitcoinNextBits(p)
	}
	return p
}

// synthRuleChain mines count headers from height 0 whose bits follow
// p.NextBits. Blocks arrive every 30 s against a one-minute target, so each
// boundary raises difficulty and a boundary header differs from its parent.
func synthRuleChain(t *testing.T, p *netparams.ChainParams, count int) []*wire.BlockHeader {
	t.Helper()
	out := make([]*wire.BlockHeader, 0, count)
	at := func(height int32) (netparams.BlockDifficulty, bool) {
		if height < 0 || int(height) >= len(out) {
			return netparams.BlockDifficulty{}, false
		}
		return netparams.HeaderDifficulty(out[height]), true
	}
	ts := time.Unix(1_600_000_000, 0)
	var prev chainhash.Hash
	for i := range count {
		bits, ok := p.NextBits(int32(i), at)
		if !ok {
			t.Fatalf("NextBits(%d) inputs unavailable while mining", i)
		}
		h := &wire.BlockHeader{
			Version:   1,
			PrevBlock: prev,
			Timestamp: ts.Add(time.Duration(i) * 30 * time.Second),
			Bits:      bits,
		}
		mineHeader(h)
		out = append(out, h)
		prev = h.BlockHash()
	}
	return out
}

// remine replaces hdr's bits and re-mines it in place, so only the bits rule
// can reject it.
func remine(hdr *wire.BlockHeader, bits uint32) {
	hdr.Bits = bits
	hdr.Nonce = 0
	mineHeader(hdr)
}

// TestValidateBitsLocked covers the contextual bits check: a boundary header
// must carry the retarget the rule computes, an off-boundary header must
// carry its parent's bits, inputs resolve from the batch, the store and the
// anchor checkpoint's difficulty record, and a missing input is an error.
func TestValidateBitsLocked(t *testing.T) {
	p := intervalTestParams(nil, false)
	chain := synthRuleChain(t, p, 2*testRetargetInterval+2)
	const boundary = testRetargetInterval
	if chain[boundary].Bits == chain[boundary-1].Bits {
		t.Fatal("test setup: boundary does not retarget")
	}

	newH := func(p *netparams.ChainParams, start int32, stored []*wire.BlockHeader) *Headers {
		h := &Headers{
			cfg:      &ChainConfig{Params: p, StartHeight: start, HeaderSize: 80},
			byHeight: make(map[int32]*wire.BlockHeader),
		}
		for i, hdr := range stored {
			h.byHeight[start+int32(i)] = hdr
		}
		return h
	}
	withBits := func(hdr *wire.BlockHeader, bits uint32) *wire.BlockHeader {
		c := *hdr
		c.Bits = bits
		return &c
	}

	h := newH(p, 0, chain[:boundary])
	if err := h.validateBitsLocked(boundary, chain[boundary], boundary-1, nil); err != nil {
		t.Fatalf("valid retarget rejected: %v", err)
	}
	if err := h.validateBitsLocked(boundary, withBits(chain[boundary], chain[boundary-1].Bits), boundary-1, nil); err == nil {
		t.Fatal("boundary header without its retarget accepted")
	}
	if err := h.validateBitsLocked(boundary, withBits(chain[boundary], p.PowLimitBits), boundary-1, nil); err == nil {
		t.Fatal("boundary header at PowLimit accepted")
	}
	h = newH(p, 0, chain[:boundary+1])
	if err := h.validateBitsLocked(boundary+1, chain[boundary+1], boundary, nil); err != nil {
		t.Fatalf("valid off-boundary header rejected: %v", err)
	}
	if err := h.validateBitsLocked(boundary+1, withBits(chain[boundary+1], p.PowLimitBits), boundary, nil); err == nil {
		t.Fatal("off-boundary bits change accepted")
	}

	// Inputs from the batch being appended, with nothing stored.
	empty := newH(p, 0, nil)
	if err := empty.validateBitsLocked(boundary, chain[boundary], -1, chain[:boundary]); err != nil {
		t.Fatalf("valid retarget rejected with inputs in the batch: %v", err)
	}
	if err := empty.validateBitsLocked(boundary, withBits(chain[boundary], p.PowLimitBits), -1, chain[:boundary]); err == nil {
		t.Fatal("forged retarget accepted with inputs in the batch")
	}

	// A missing input is an error, not a pass: the sample block at
	// boundary-interval, and separately the parent.
	for _, missing := range []int32{boundary - testRetargetInterval, boundary - 1} {
		h := newH(p, 0, chain[:boundary])
		delete(h.byHeight, missing)
		if err := h.validateBitsLocked(boundary, chain[boundary], boundary-1, nil); err == nil ||
			!strings.Contains(err.Error(), "inputs unavailable") {
			t.Fatalf("block %d missing: got %v, want an unavailable-input error", missing, err)
		}
	}
	h = newH(p, 0, chain[:boundary+1])
	delete(h.byHeight, boundary)
	if err := h.validateBitsLocked(boundary+1, chain[boundary+1], boundary, nil); err == nil ||
		!strings.Contains(err.Error(), "inputs unavailable") {
		t.Fatalf("off-boundary parent missing: got %v, want an unavailable-input error", err)
	}

	// Params without a rule skip the check.
	if err := newH(paramsWithCheckpoints(nil), 0, chain[:boundary]).
		validateBitsLocked(boundary, withBits(chain[boundary], p.PowLimitBits), boundary-1, nil); err != nil {
		t.Fatalf("nil NextBits should skip: %v", err)
	}

	// Litecoin sampling anchored at a boundary checkpoint: the first
	// retarget above the anchor samples the block below it, which only the
	// checkpoint's difficulty record supplies.
	ltc := intervalTestParams(nil, true)
	lchain := synthRuleChain(t, ltc, 2*testRetargetInterval+2)
	anchor := int32(testRetargetInterval)
	next := anchor + testRetargetInterval
	cp := netparams.Checkpoint{
		Height: anchor,
		Hash:   ptrHash(lchain[anchor].BlockHash()),
		Header: lchain[anchor],
	}
	noRecord := intervalTestParams([]netparams.Checkpoint{cp}, true)
	cp.Difficulty = []netparams.BlockDifficulty{netparams.HeaderDifficulty(lchain[anchor-1])}
	withRecord := intervalTestParams([]netparams.Checkpoint{cp}, true)
	for _, tc := range []struct {
		name    string
		params  *netparams.ChainParams
		wantErr bool
	}{
		{"with difficulty record", withRecord, false},
		{"without difficulty record", noRecord, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewHeaders(&ChainConfig{Params: tc.params, StartHeight: anchor, HeaderSize: 80,
				Chain: bisonwire.ChainLTC}, t.TempDir())
			if err != nil {
				t.Fatalf("NewHeaders: %v", err)
			}
			if _, err := h.AppendBatch(anchor, serializeBatch(t, lchain[anchor:next])); err != nil {
				t.Fatalf("AppendBatch below the first retarget: %v", err)
			}
			err = h.AppendOne(lchain[next])
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "inputs unavailable") {
					t.Fatalf("first retarget above the anchor without its sample block: got %v", err)
				}
			} else if err != nil {
				t.Fatalf("first retarget above the anchor: %v", err)
			}
		})
	}
}

// TestAppendRejectsOffBoundaryBits: a correctly mined, correctly linked
// header whose bits differ from its parent's off a retarget boundary is
// refused by both append paths.
func TestAppendRejectsOffBoundaryBits(t *testing.T) {
	p := intervalTestParams(nil, false)
	chain := synthRuleChain(t, p, testRetargetInterval/2)
	newH := func() *Headers {
		h, err := NewHeaders(&ChainConfig{Params: p, HeaderSize: 80, Chain: bisonwire.ChainBTC}, t.TempDir())
		if err != nil {
			t.Fatalf("NewHeaders: %v", err)
		}
		return h
	}
	easier := blockchain.BigToCompact(p.PowLimit)
	if easier == chain[3].Bits {
		easier = 0x207ffffe
	}

	// AppendBatch: the change sits inside the batch.
	forged := make([]*wire.BlockHeader, 4)
	copy(forged, chain[:3])
	f := *chain[3]
	forged[3] = &f
	remine(forged[3], easier)
	h := newH()
	if _, err := h.AppendBatch(0, serializeBatch(t, forged)); err == nil ||
		!strings.Contains(err.Error(), "consensus requires") {
		t.Fatalf("AppendBatch with an off-boundary bits change: got %v", err)
	}
	if h.Tip() != -1 {
		t.Fatalf("rejected batch changed the tip to %d", h.Tip())
	}

	// AppendOne: the change is the new tip.
	h = newH()
	if _, err := h.AppendBatch(0, serializeBatch(t, chain[:3])); err != nil {
		t.Fatalf("seed AppendBatch: %v", err)
	}
	if err := h.AppendOne(forged[3]); err == nil || !strings.Contains(err.Error(), "consensus requires") {
		t.Fatalf("AppendOne with an off-boundary bits change: got %v", err)
	}
	if err := h.AppendOne(chain[3]); err != nil {
		t.Fatalf("AppendOne with the parent's bits: %v", err)
	}
}

func ptrHash(h chainhash.Hash) *chainhash.Hash { return &h }

// mustNewHeaders constructs a Headers for cfg rooted in a temp dir.
func mustNewHeaders(t *testing.T, cfg *ChainConfig) *Headers {
	t.Helper()
	h, err := NewHeaders(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("NewHeaders: %v", err)
	}
	return h
}

// unminedHeader returns a header linked to prev whose hash fails the tight
// target it claims (2^-32 of hashes would pass; the nonce is bumped past any
// that do).
func unminedHeader(prev chainhash.Hash, ts time.Time) *wire.BlockHeader {
	hdr := &wire.BlockHeader{Version: 1, PrevBlock: prev, Bits: 0x1d00ffff, Timestamp: ts}
	for (&Headers{cfg: testChainConfig()}).checkPoW(hdr) == nil {
		hdr.Nonce++
	}
	return hdr
}

// TestHeadersTargetRange: a header whose bits encode a target outside
// (0, PowLimit] is refused by both append paths, whether or not its height is
// PoW-checked, even when its hash meets the target it claims.
func TestHeadersTargetRange(t *testing.T) {
	btcMainPowLimit := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 224), big.NewInt(1))
	chain := synthChain(1, 7) // mined at 0x207fffff, about 2^255
	cases := []struct {
		name string
		cps  []netparams.Checkpoint
	}{
		{"PoW-checked height", nil},
		// A checkpoint at height 0 puts the header at or below the newest
		// checkpoint, where the PoW hash is not checked.
		{"height at the newest checkpoint", []netparams.Checkpoint{{Height: 0, Hash: ptrHash(chain[0].BlockHash())}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := paramsWithCheckpoints(tc.cps)
			p.PowLimit = btcMainPowLimit
			h := mustNewHeaders(t, &ChainConfig{Chain: bisonwire.ChainBTC, Params: p, HeaderSize: 80})
			_, err := h.AppendBatch(0, serializeBatch(t, chain))
			if err == nil || !strings.Contains(err.Error(), "outside (0, PowLimit]") {
				t.Fatalf("AppendBatch of a mined 0x207fffff header under the BTC mainnet PowLimit: got %v", err)
			}
		})
	}

	// AppendOne, with a seed header inside a PowLimit of 2^248.
	p := paramsWithCheckpoints(nil)
	p.PowLimit = new(big.Int).Lsh(big.NewInt(1), 248)
	seed := &wire.BlockHeader{Version: 1, Bits: 0x1f00ffff, Timestamp: time.Unix(1_600_000_000, 0)}
	mineHeader(seed)
	h := mustNewHeaders(t, &ChainConfig{Chain: bisonwire.ChainBTC, Params: p, HeaderSize: 80})
	if _, err := h.AppendBatch(0, serializeBatch(t, []*wire.BlockHeader{seed})); err != nil {
		t.Fatalf("seed: %v", err)
	}
	easy := &wire.BlockHeader{Version: 1, PrevBlock: seed.BlockHash(), Bits: 0x207fffff,
		Timestamp: seed.Timestamp.Add(time.Minute)}
	mineHeader(easy)
	if err := h.AppendOne(easy); err == nil || !strings.Contains(err.Error(), "outside (0, PowLimit]") {
		t.Fatalf("AppendOne of a header above PowLimit: got %v", err)
	}

	// Zero, negative and overflowing compact encodings.
	hdrs := &Headers{cfg: &ChainConfig{Params: p}}
	for _, bits := range []uint32{0, 0x01003456, 0x04923456, 0x23000001, p.PowLimitBits + 1} {
		if err := hdrs.checkTarget(&wire.BlockHeader{Bits: bits}); err == nil {
			t.Errorf("bits 0x%08x accepted", bits)
		}
	}
	if err := hdrs.checkTarget(&wire.BlockHeader{Bits: 0x1f00ffff}); err != nil {
		t.Fatalf("in-range bits rejected: %v", err)
	}
}

// TestHeadersPoWAboveNewestCheckpoint: headers above the newest checkpoint
// are PoW-checked whatever their position in the batch, and headers at or
// below it are not.
func TestHeadersPoWAboveNewestCheckpoint(t *testing.T) {
	const cpHeight = 5
	ts := time.Unix(1_600_000_000, 0)
	// Heights 1 through cpHeight are unmined; the rest are mined.
	chain := synthChain(1, 11)
	for i := 1; i <= cpHeight; i++ {
		chain = append(chain, unminedHeader(chain[i-1].BlockHash(), ts.Add(time.Duration(i)*time.Minute)))
	}
	for i := cpHeight + 1; i < 30; i++ {
		hdr := &wire.BlockHeader{Version: 1, PrevBlock: chain[i-1].BlockHash(), Bits: 0x207fffff,
			Timestamp: ts.Add(time.Duration(i) * time.Minute)}
		mineHeader(hdr)
		chain = append(chain, hdr)
	}
	p := paramsWithCheckpoints([]netparams.Checkpoint{
		{Height: 0, Hash: ptrHash(chain[0].BlockHash())},
		{Height: cpHeight, Hash: ptrHash(chain[cpHeight].BlockHash())},
	})
	newH := func() *Headers {
		return mustNewHeaders(t, &ChainConfig{Chain: bisonwire.ChainBTC, Params: p, HeaderSize: 80})
	}

	h := newH()
	if _, err := h.AppendBatch(0, serializeBatch(t, chain[:cpHeight+1])); err != nil {
		t.Fatalf("unmined headers at or below the newest checkpoint rejected: %v", err)
	}
	above := unminedHeader(chain[cpHeight].BlockHash(), ts.Add(time.Hour))
	if err := h.AppendOne(above); err == nil || !strings.Contains(err.Error(), "exceeds target") {
		t.Fatalf("AppendOne of an unmined header above the checkpoint: got %v", err)
	}
	if _, err := h.AppendBatch(cpHeight+1, serializeBatch(t, []*wire.BlockHeader{above})); err == nil {
		t.Fatal("AppendBatch of an unmined header above the checkpoint accepted")
	}

	// One unmined header deep inside a batch that spans the checkpoint,
	// checked by the parallel workers.
	const bad = 17
	forged := append([]*wire.BlockHeader(nil), chain...)
	forged[bad] = unminedHeader(chain[bad-1].BlockHash(), chain[bad].Timestamp)
	for i := bad + 1; i < len(forged); i++ {
		c := *forged[i]
		c.PrevBlock = forged[i-1].BlockHash()
		c.Nonce = 0
		mineHeader(&c)
		forged[i] = &c
	}
	h = newH()
	_, err := h.AppendBatch(0, serializeBatch(t, forged))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("header at %d:", bad)) {
		t.Fatalf("batch with an unmined header at %d: got %v", bad, err)
	}
	if h.Tip() != -1 {
		t.Fatalf("rejected batch moved the tip to %d", h.Tip())
	}
	if _, err := h.AppendBatch(0, serializeBatch(t, chain)); err != nil {
		t.Fatalf("valid batch spanning the checkpoint: %v", err)
	}
}

// TestHeadersFutureTimestamp: a header timestamped up to two hours past local
// time is accepted, and one second later is refused with errTimeTooNew by both
// append paths.
func TestHeadersFutureTimestamp(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	mk := func(prev chainhash.Hash, ts time.Time) *wire.BlockHeader {
		hdr := &wire.BlockHeader{Version: 1, PrevBlock: prev, Bits: 0x207fffff, Timestamp: ts}
		mineHeader(hdr)
		return hdr
	}
	h, _ := newTestHeaders(t)
	h.now = func() time.Time { return now }
	g := mk(chainhash.Hash{}, now.Add(maxFutureBlockTime))
	if _, err := h.AppendBatch(0, serializeBatch(t, []*wire.BlockHeader{g})); err != nil {
		t.Fatalf("header at now+2h rejected: %v", err)
	}
	late := mk(g.BlockHash(), now.Add(maxFutureBlockTime+time.Second))
	if err := h.AppendOne(late); !errors.Is(err, errTimeTooNew) {
		t.Fatalf("AppendOne at now+2h+1s: got %v, want errTimeTooNew", err)
	}
	if _, err := h.AppendBatch(1, serializeBatch(t, []*wire.BlockHeader{late})); !errors.Is(err, errTimeTooNew) {
		t.Fatalf("AppendBatch at now+2h+1s: got %v, want errTimeTooNew", err)
	}
	if err := h.AppendOne(mk(g.BlockHash(), now.Add(maxFutureBlockTime))); err != nil {
		t.Fatalf("AppendOne at now+2h: %v", err)
	}
}

// TestHeadersRewindFloor: once the store holds the newest checkpoint it never
// rewinds below it; below the checkpoint, rewinds proceed.
func TestHeadersRewindFloor(t *testing.T) {
	const cpHeight = 10
	chain := synthChain(20, 13)
	p := paramsWithCheckpoints([]netparams.Checkpoint{
		{Height: 0, Hash: ptrHash(chain[0].BlockHash())},
		{Height: cpHeight, Hash: ptrHash(chain[cpHeight].BlockHash())},
	})
	h := mustNewHeaders(t, &ChainConfig{Chain: bisonwire.ChainBTC, Params: p, HeaderSize: 80})
	if _, err := h.AppendBatch(0, serializeBatch(t, chain[:cpHeight])); err != nil {
		t.Fatalf("seed below the checkpoint: %v", err)
	}
	if _, err := h.Rewind(3); err != nil {
		t.Fatalf("rewind below an unreached checkpoint: %v", err)
	}
	if _, err := h.AppendBatch(cpHeight-3, serializeBatch(t, chain[cpHeight-3:])); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if _, err := h.Rewind(int32(len(chain)-1-cpHeight) + 1); err == nil ||
		!strings.Contains(err.Error(), "below checkpoint") {
		t.Fatalf("rewind below a held checkpoint: got %v", err)
	}
	if h.Tip() != int32(len(chain)-1) {
		t.Fatalf("refused rewind moved the tip to %d", h.Tip())
	}
	if _, err := h.Rewind(int32(len(chain) - 1 - cpHeight)); err != nil {
		t.Fatalf("rewind to the checkpoint: %v", err)
	}
	if h.Tip() != cpHeight {
		t.Fatalf("tip %d after rewinding to the checkpoint", h.Tip())
	}
}

// paramsWithCheckpoints returns a *netparams.ChainParams suitable for the
// validation tests, carrying the supplied checkpoints. We can't shallow-copy
// netparams.NSimnetParams (it has a sync.Once); construct the minimal set
// of fields the validators actually read.
func paramsWithCheckpoints(cps []netparams.Checkpoint) *netparams.ChainParams {
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
		Params:      paramsWithCheckpoints([]netparams.Checkpoint{{Height: 2, Hash: &bogus}}),
		HeaderSize:  80,
		StartHeight: 0,
	}

	h, err := NewHeaders(cfg, dir)
	if err != nil {
		t.Fatalf("NewHeaders: %v", err)
	}
	if _, err := h.AppendBatch(0, serializeBatch(t, chain)); err == nil {
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
		Params:      paramsWithCheckpoints([]netparams.Checkpoint{{Height: 2, Hash: &good}}),
		HeaderSize:  80,
		StartHeight: 0,
	}

	h, err := NewHeaders(cfg, dir)
	if err != nil {
		t.Fatalf("NewHeaders: %v", err)
	}
	if _, err := h.AppendBatch(0, serializeBatch(t, chain)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
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
	mineHeader(next)
	if err := h.AppendOne(next); err != nil {
		t.Fatalf("AppendOne (good): %v", err)
	}
	if h.Tip() != 5 {
		t.Fatalf("tip should be 5 after AppendOne, got %d", h.Tip())
	}

	// A header that does not link is refused with errHeaderDoesNotConnect
	// and the store is untouched: the fork is resolved by the catch-up
	// path against the server's chain, never by a blind rewind here.
	bad := &wire.BlockHeader{
		PrevBlock: chainhash.Hash{31: 1}, // garbage
		Bits:      0x207fffff,
	}
	if err := h.AppendOne(bad); !errors.Is(err, errHeaderDoesNotConnect) {
		t.Fatalf("AppendOne(bad) = %v, want errHeaderDoesNotConnect", err)
	}
	if h.Tip() != 5 {
		t.Fatalf("tip moved to %d on a refused header", h.Tip())
	}
}

// TestHeadersReorgRewind: Rewind drops exactly the requested depth, reports
// the disconnected blocks newest-first, and keeps the rewound headers
// resolvable by hash at their former heights — the wallet's disconnect
// handler looks up each orphan's parent, which for all but the deepest
// orphan is itself an orphan.
func TestHeadersReorgRewind(t *testing.T) {
	h, _ := newTestHeaders(t)
	chain := synthChain(20, 1)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tipBefore := h.Tip()
	const depth = 3
	hashAtRewind := chain[tipBefore-depth].BlockHash()

	disc, err := h.Rewind(depth)
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	// rewind should surface the disconnected blocks (review2 #1).
	if len(disc) != depth {
		t.Fatalf("expected %d disconnected blocks, got %d", depth, len(disc))
	}
	// Disconnected blocks are newest-first.
	if disc[0].Height != tipBefore {
		t.Fatalf("first disconnected height = %d, want %d", disc[0].Height, tipBefore)
	}
	// Tip() now reflects the truth: pre-rewind tip minus depth (review2 #2).
	if got := h.Tip(); got != tipBefore-depth {
		t.Fatalf("post-rewind Tip()=%d, want %d", got, tipBefore-depth)
	}
	if got := h.TipHash(); got != hashAtRewind {
		t.Fatalf("post-rewind hash mismatch: got %s, want %s", got, hashAtRewind)
	}
	for i := range int32(depth) {
		height := tipBefore - i
		orphan := chain[height]
		hash := orphan.BlockHash()
		gotHeight, got, ok := h.ByHash(&hash)
		if !ok || gotHeight != height || got.BlockHash() != hash {
			t.Fatalf("rewound header at %d not resolvable by hash (ok=%v, height=%d)",
				height, ok, gotHeight)
		}
		if h.ByHeight(height) != nil {
			t.Fatalf("rewound height %d still served by height", height)
		}
	}

	// Reconnect with the correct next header for the new tip.
	good := &wire.BlockHeader{
		PrevBlock: hashAtRewind,
		Bits:      0x207fffff,
		Nonce:     12345,
	}
	mineHeader(good)
	if err := h.AppendOne(good); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if got := h.Tip(); got != tipBefore-depth+1 {
		t.Fatalf("post-reconnect tip wrong: got %d", got)
	}
}

// TestHeadersOrphanRetentionBounded: rewound headers are retained for hash
// lookups only up to maxOrphans, oldest evicted first.
func TestHeadersOrphanRetentionBounded(t *testing.T) {
	h, _ := newTestHeaders(t)
	chain := synthChain(maxOrphans+10, 3)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, err := h.Rewind(5)
	if err != nil {
		t.Fatalf("first Rewind: %v", err)
	}
	second, err := h.Rewind(maxOrphans)
	if err != nil {
		t.Fatalf("second Rewind: %v", err)
	}
	for _, d := range first {
		hash := chainhash.Hash(d.Hash)
		if _, _, ok := h.ByHash(&hash); ok {
			t.Fatalf("orphan at %d survived eviction", d.Height)
		}
	}
	for _, d := range second {
		hash := chainhash.Hash(d.Hash)
		if height, _, ok := h.ByHash(&hash); !ok || height != d.Height {
			t.Fatalf("recent orphan at %d not resolvable (ok=%v, height=%d)",
				d.Height, ok, height)
		}
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
			Params:      paramsWithCheckpoints([]netparams.Checkpoint{{Height: anchor, Hash: &cpHash}}),
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
		if _, err := h.AppendBatch(seedAnchor, serializeBatch(t, chain[seedAnchor:])); err != nil {
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
			if _, err := h.AppendBatch(tc.newAnchor, serializeBatch(t, chain[tc.newAnchor:])); err != nil {
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

// TestHeadersAppendBatchInteriorBreak: a batch whose first header links to
// the tip but which breaks internally is a server fault, not a reorg. It
// must be refused with an error that reorg recovery does not treat as a fork
// signal, and the store must be untouched.
func TestHeadersAppendBatchInteriorBreak(t *testing.T) {
	h, _ := newTestHeaders(t)
	chain := synthChain(12, 5)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain[:8])); err != nil {
		t.Fatalf("seed: %v", err)
	}
	broken := append([]*wire.BlockHeader(nil), chain[8:]...)
	bad := *broken[2]
	bad.PrevBlock = chainhash.Hash{31: 7}
	broken[2] = &bad
	_, err := h.AppendBatch(8, serializeBatch(t, broken))
	if err == nil {
		t.Fatal("interior break accepted")
	}
	if errors.Is(err, errHeaderDoesNotConnect) {
		t.Fatalf("interior break reported as a reorg signal: %v", err)
	}
	if h.Tip() != 7 {
		t.Fatalf("tip = %d after a refused batch, want 7", h.Tip())
	}
}

// BenchmarkCheckBatchPoW measures the PoW hash check AppendBatch runs on
// headers above the newest checkpoint, per header, using a real mainnet
// checkpoint header repeated b.N times. Run with -cpu to compare one worker
// against GOMAXPROCS workers, e.g.
//
//	go test -run '^$' -bench CheckBatchPoW -cpu 1,8 ./wallet/utxowallet/electrum/
func BenchmarkCheckBatchPoW(b *testing.B) {
	for _, bc := range []struct {
		name   string
		params *netparams.ChainParams
	}{
		{"ltc-scrypt", assets.LTCParams["mainnet"]},
		{"dash-x11", assets.DashParams["mainnet"]},
		{"btc-sha256d", assets.BTCParams["mainnet"]},
	} {
		b.Run(bc.name, func(b *testing.B) {
			cp := bc.params.Checkpoints[len(bc.params.Checkpoints)-1]
			h := &Headers{cfg: &ChainConfig{Params: bc.params, HeaderSize: 80}}
			hdrs := make([]*wire.BlockHeader, b.N)
			for i := range hdrs {
				hdrs[i] = cp.Header
			}
			b.ResetTimer()
			if err := h.checkBranchPoW(cp.Height+1, hdrs, cp.Height+1); err != nil {
				b.Fatal(err)
			}
		})
	}
}
