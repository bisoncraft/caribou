// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// storeWith returns a Headers for params holding hdrs from height 0.
func storeWith(t *testing.T, params *netparams.ChainParams, hdrs []*wire.BlockHeader) *Headers {
	t.Helper()
	h := mustNewHeaders(t, &ChainConfig{Chain: bisonwire.ChainBTC, Params: params, HeaderSize: 80})
	if len(hdrs) > 0 {
		if _, err := h.AppendBatch(0, serializeBatch(t, hdrs)); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	}
	return h
}

// isRuleError reports whether err is a *RuleError, and its height.
func isRuleError(err error) (int32, bool) {
	var re *RuleError
	if errors.As(err, &re) {
		return re.Height, true
	}
	return 0, false
}

// relink returns a copy of hdrs with each header after index changed
// re-pointed at its predecessor and re-mined, so a change to that header keeps
// the rest linked. Headers up to and including changed are kept as they are.
func relink(hdrs []*wire.BlockHeader, changed int) []*wire.BlockHeader {
	out := make([]*wire.BlockHeader, len(hdrs))
	for i, hdr := range hdrs {
		c := *hdr
		if i > changed {
			c.PrevBlock = out[i-1].BlockHash()
			c.Nonce = 0
			mineHeader(&c)
		}
		out[i] = &c
	}
	return out
}

// extendRuleChain mines n headers on top of prefix whose bits follow
// p.NextBits, spaced spacing apart after the prefix's last timestamp.
func extendRuleChain(t *testing.T, p *netparams.ChainParams, prefix []*wire.BlockHeader,
	n int, spacing time.Duration, seed uint32) []*wire.BlockHeader {

	t.Helper()
	all := append([]*wire.BlockHeader(nil), prefix...)
	at := func(height int32) (netparams.BlockDifficulty, bool) {
		if height < 0 || int(height) >= len(all) {
			return netparams.BlockDifficulty{}, false
		}
		return netparams.HeaderDifficulty(all[height]), true
	}
	for i := range n {
		height := int32(len(all))
		bits, ok := p.NextBits(height, at)
		if !ok {
			t.Fatalf("NextBits(%d) inputs unavailable while mining", height)
		}
		hdr := &wire.BlockHeader{
			Version:   1,
			PrevBlock: all[len(all)-1].BlockHash(),
			Timestamp: all[len(all)-1].Timestamp.Add(spacing),
			Bits:      bits,
			Nonce:     seed + uint32(i)<<8,
		}
		mineHeader(hdr)
		all = append(all, hdr)
	}
	return all[len(prefix):]
}

// TestRuleErrorClassification: violations only the serving server can cause
// are RuleErrors carrying the offending height; a first header that does not
// link to the store (a reorg signal), a timestamp past the local bound and a
// missing local input are not.
func TestRuleErrorClassification(t *testing.T) {
	base := synthChain(10, 21)

	t.Run("checkpoint mismatch", func(t *testing.T) {
		p := paramsWithCheckpoints([]netparams.Checkpoint{{Height: 3, Hash: &chainhash.Hash{7}}})
		h := storeWith(t, p, nil)
		_, err := h.AppendBatch(0, serializeBatch(t, base))
		if height, ok := isRuleError(err); !ok || height != 3 || !errors.Is(err, errCheckpointMismatch) {
			t.Fatalf("got %v, want a RuleError at 3 wrapping errCheckpointMismatch", err)
		}
	})

	t.Run("interior break", func(t *testing.T) {
		forged := append([]*wire.BlockHeader(nil), base...)
		bad := *base[5]
		bad.PrevBlock = chainhash.Hash{9}
		mineHeader(&bad)
		forged[5] = &bad
		_, err := storeWith(t, paramsWithCheckpoints(nil), nil).AppendBatch(0, serializeBatch(t, forged))
		if height, ok := isRuleError(err); !ok || height != 5 || errors.Is(err, errHeaderDoesNotConnect) {
			t.Fatalf("got %v, want a RuleError at 5 that is not a reorg signal", err)
		}
	})

	t.Run("target range", func(t *testing.T) {
		p := paramsWithCheckpoints(nil)
		p.PowLimit = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 224), big.NewInt(1))
		_, err := storeWith(t, p, nil).AppendBatch(0, serializeBatch(t, base))
		if height, ok := isRuleError(err); !ok || height != 0 {
			t.Fatalf("got %v, want a RuleError at 0", err)
		}
	})

	t.Run("PoW", func(t *testing.T) {
		forged := append([]*wire.BlockHeader(nil), base[:4]...)
		forged = append(forged, unminedHeader(base[3].BlockHash(), base[4].Timestamp))
		_, err := storeWith(t, paramsWithCheckpoints(nil), nil).AppendBatch(0, serializeBatch(t, forged))
		if height, ok := isRuleError(err); !ok || height != 4 {
			t.Fatalf("AppendBatch: got %v, want a RuleError at 4", err)
		}
		h := storeWith(t, paramsWithCheckpoints(nil), base[:4])
		if height, ok := isRuleError(h.AppendOne(forged[4])); !ok || height != 4 {
			t.Fatal("AppendOne of an unmined header is not a RuleError at 4")
		}
	})

	t.Run("bits", func(t *testing.T) {
		p := intervalTestParams(nil, false)
		chain := synthRuleChain(t, p, 6)
		forged := append([]*wire.BlockHeader(nil), chain...)
		f := *chain[5]
		forged[5] = &f
		remine(forged[5], 0x207ffffe)
		_, err := storeWith(t, p, nil).AppendBatch(0, serializeBatch(t, forged))
		if height, ok := isRuleError(err); !ok || height != 5 || !strings.Contains(err.Error(), "consensus requires") {
			t.Fatalf("got %v, want a RuleError at 5 for bits", err)
		}
	})

	t.Run("first header does not link", func(t *testing.T) {
		h := storeWith(t, paramsWithCheckpoints(nil), base[:5])
		orphan := &wire.BlockHeader{Version: 1, PrevBlock: chainhash.Hash{3}, Bits: 0x207fffff,
			Timestamp: base[5].Timestamp}
		mineHeader(orphan)
		_, err := h.AppendBatch(5, serializeBatch(t, []*wire.BlockHeader{orphan}))
		if _, ok := isRuleError(err); ok || !errors.Is(err, errHeaderDoesNotConnect) {
			t.Fatalf("AppendBatch: got %v, want errHeaderDoesNotConnect and no RuleError", err)
		}
		if err := h.AppendOne(orphan); !errors.Is(err, errHeaderDoesNotConnect) {
			t.Fatalf("AppendOne: got %v, want errHeaderDoesNotConnect", err)
		} else if _, ok := isRuleError(err); ok {
			t.Fatal("AppendOne: a non-linking header is a RuleError")
		}
	})

	t.Run("timestamp too new", func(t *testing.T) {
		h := storeWith(t, paramsWithCheckpoints(nil), base[:5])
		late := &wire.BlockHeader{Version: 1, PrevBlock: base[4].BlockHash(), Bits: 0x207fffff,
			Timestamp: time.Now().Add(maxFutureBlockTime + time.Hour)}
		mineHeader(late)
		err := h.AppendOne(late)
		if _, ok := isRuleError(err); ok || !errors.Is(err, errTimeTooNew) {
			t.Fatalf("got %v, want errTimeTooNew and no RuleError", err)
		}
	})

	t.Run("missing input", func(t *testing.T) {
		p := intervalTestParams(nil, false)
		chain := synthRuleChain(t, p, testRetargetInterval+1)
		h := storeWith(t, p, chain[:testRetargetInterval])
		delete(h.byHeight, 0)
		_, err := h.AppendBatch(testRetargetInterval, serializeBatch(t, chain[testRetargetInterval:]))
		if _, ok := isRuleError(err); ok || err == nil || !strings.Contains(err.Error(), "inputs unavailable") {
			t.Fatalf("got %v, want an unavailable-input error that is not a RuleError", err)
		}
	})
}

// TestValidateBitsBranchOnly: ancestors above the branch parent come only from
// the branch, never from the store, which may hold a competing chain there.
func TestValidateBitsBranchOnly(t *testing.T) {
	p := intervalTestParams(nil, false)
	chain := synthRuleChain(t, p, testRetargetInterval+1)
	const boundary = testRetargetInterval
	h := storeWith(t, p, chain[:boundary])

	// The store holds the parent at boundary-1, but it lies above the
	// branch parent, so the rule may not read it from the store.
	h.mu.RLock()
	defer h.mu.RUnlock()
	if err := h.validateBitsLocked(boundary, chain[boundary], boundary-2, nil); err == nil ||
		!strings.Contains(err.Error(), "inputs unavailable") {
		t.Fatalf("got %v, want an unavailable-input error", err)
	}
	if err := h.validateBitsLocked(boundary, chain[boundary], boundary-2,
		[]*wire.BlockHeader{chain[boundary-1], chain[boundary]}); err != nil {
		t.Fatalf("parent supplied by the branch: %v", err)
	}
}

// TestSwapBranch covers the swap's checks: a heavier or equal-work branch
// replaces the stored span and reports its disconnects, and every refusal
// leaves the store untouched.
func TestSwapBranch(t *testing.T) {
	const forkAt = 10 // first height that differs; the fork point is forkAt-1
	unchanged := func(t *testing.T, h *Headers, a []*wire.BlockHeader) {
		t.Helper()
		if h.Tip() != int32(len(a)-1) || h.TipHash() != a[len(a)-1].BlockHash() {
			t.Fatalf("store moved: tip %d %s", h.Tip(), h.TipHash())
		}
	}

	t.Run("heavier branch", func(t *testing.T) {
		a, b := forkedChain(forkAt, 15, 17)
		h := storeWith(t, paramsWithCheckpoints(nil), a)
		disc, err := h.SwapBranch(forkAt-1, h.TipHash(), b[forkAt:])
		if err != nil {
			t.Fatalf("SwapBranch: %v", err)
		}
		if h.Tip() != 16 || h.TipHash() != b[16].BlockHash() {
			t.Fatalf("tip %d %s, want b's tip", h.Tip(), h.TipHash())
		}
		if len(disc) != 5 || disc[0].Height != 14 || disc[4].Height != forkAt {
			t.Fatalf("disconnects %+v, want heights 14 down to %d", disc, forkAt)
		}
		for _, d := range disc {
			hash := chainhash.Hash(d.Hash)
			if hash != a[d.Height].BlockHash() {
				t.Fatalf("disconnect at %d is not a's block", d.Height)
			}
			if height, _, ok := h.ByHash(&hash); !ok || height != d.Height {
				t.Fatalf("orphan at %d not resolvable by hash", d.Height)
			}
		}
		if err := h.VerifyAll(); err != nil {
			t.Fatalf("store does not link after the swap: %v", err)
		}
	})

	t.Run("equal work", func(t *testing.T) {
		a, b := forkedChain(forkAt, 15, 15)
		h := storeWith(t, paramsWithCheckpoints(nil), a)
		if _, err := h.SwapBranch(forkAt-1, h.TipHash(), b[forkAt:]); err != nil {
			t.Fatalf("equal-work branch refused: %v", err)
		}
		if h.TipHash() != b[14].BlockHash() {
			t.Fatal("store did not follow the equal-work branch")
		}
	})

	t.Run("less work", func(t *testing.T) {
		a, b := forkedChain(forkAt, 15, 13)
		h := storeWith(t, paramsWithCheckpoints(nil), a)
		if _, err := h.SwapBranch(forkAt-1, h.TipHash(), b[forkAt:]); !errors.Is(err, errInsufficientWork) {
			t.Fatalf("got %v, want errInsufficientWork", err)
		}
		unchanged(t, h, a)
	})

	t.Run("store moved", func(t *testing.T) {
		a, b := forkedChain(forkAt, 15, 17)
		h := storeWith(t, paramsWithCheckpoints(nil), a)
		if _, err := h.SwapBranch(forkAt-1, a[13].BlockHash(), b[forkAt:]); !errors.Is(err, errStoreMoved) {
			t.Fatalf("got %v, want errStoreMoved", err)
		}
		unchanged(t, h, a)
	})

	t.Run("fork outside the store", func(t *testing.T) {
		a, b := forkedChain(forkAt, 15, 17)
		h := storeWith(t, paramsWithCheckpoints(nil), a)
		for _, fork := range []int32{14, 15, -1} {
			if _, err := h.SwapBranch(fork, h.TipHash(), b[forkAt:]); err == nil {
				t.Fatalf("fork %d accepted", fork)
			}
		}
		unchanged(t, h, a)
	})

	t.Run("first branch header does not link", func(t *testing.T) {
		a, b := forkedChain(forkAt, 15, 17)
		h := storeWith(t, paramsWithCheckpoints(nil), a)
		// b[forkAt:] links to a[forkAt-1], not to a[forkAt-2].
		_, err := h.SwapBranch(forkAt-2, h.TipHash(), b[forkAt:])
		if _, ok := isRuleError(err); ok || !errors.Is(err, errHeaderDoesNotConnect) {
			t.Fatalf("got %v, want errHeaderDoesNotConnect and no RuleError", err)
		}
		unchanged(t, h, a)
	})

	t.Run("unmined branch header", func(t *testing.T) {
		a, b := forkedChain(forkAt, 15, 17)
		h := storeWith(t, paramsWithCheckpoints(nil), a)
		branch := append([]*wire.BlockHeader(nil), b[forkAt:]...)
		branch[2] = unminedHeader(branch[1].BlockHash(), branch[2].Timestamp)
		branch = relink(branch, 2)
		_, err := h.SwapBranch(forkAt-1, h.TipHash(), branch)
		if height, ok := isRuleError(err); !ok || height != forkAt+2 {
			t.Fatalf("got %v, want a RuleError at %d", err, forkAt+2)
		}
		unchanged(t, h, a)
	})

	t.Run("fork below a held checkpoint", func(t *testing.T) {
		a, b := forkedChain(forkAt, 15, 17)
		p := paramsWithCheckpoints([]netparams.Checkpoint{
			{Height: 0, Hash: ptrHash(a[0].BlockHash())},
			{Height: 12, Hash: ptrHash(a[12].BlockHash())},
		})
		h := storeWith(t, p, a)
		if _, err := h.SwapBranch(forkAt-1, h.TipHash(), b[forkAt:]); err == nil ||
			!strings.Contains(err.Error(), "below checkpoint") {
			t.Fatalf("got %v, want a refusal below the held checkpoint", err)
		}
		unchanged(t, h, a)
	})
}

// TestSwapBranchBelowNewestCheckpoint: while the store does not hold the
// newest checkpoint, its span there was never hashed and its claimed work
// counts for nothing, so a fully validated branch swaps in with less claimed
// work. The branch's own headers are still hashed.
func TestSwapBranchBelowNewestCheckpoint(t *testing.T) {
	const forkAt = 10
	a := synthChain(forkAt, 41)
	ts := a[forkAt-1].Timestamp
	// Stored span above the fork: unmined headers claiming far more work
	// than the branch, accepted because they lie below the newest
	// checkpoint and so are not hashed on append.
	for i := forkAt; i < 15; i++ {
		a = append(a, unminedHeader(a[i-1].BlockHash(), ts.Add(time.Duration(i)*time.Minute)))
	}
	p := paramsWithCheckpoints([]netparams.Checkpoint{
		{Height: 0, Hash: ptrHash(a[0].BlockHash())},
		{Height: 40, Hash: &chainhash.Hash{1}},
	})
	_, b := forkedChain(forkAt, forkAt, forkAt+3)
	copy(b, a[:forkAt])
	b = append(b[:forkAt], relink(append([]*wire.BlockHeader{a[forkAt-1]}, b[forkAt:]...), 0)[1:]...)

	h := storeWith(t, p, a)
	if h.workRange(forkAt, 14).Cmp(new(big.Int).Mul(blockchain.CalcWork(0x207fffff), big.NewInt(3))) <= 0 {
		t.Fatal("test setup: the stored span does not claim more work than the branch")
	}

	// A branch header that fails its hash is refused even below the
	// checkpoint.
	bad := append([]*wire.BlockHeader(nil), b[forkAt:]...)
	bad[1] = unminedHeader(bad[0].BlockHash(), bad[1].Timestamp)
	bad = relink(bad, 1)
	if height, ok := isRuleError(func() error {
		_, err := h.SwapBranch(forkAt-1, h.TipHash(), bad)
		return err
	}()); !ok || height != forkAt+1 {
		t.Fatalf("unmined branch header below the checkpoint was not a RuleError at %d", forkAt+1)
	}

	if _, err := h.SwapBranch(forkAt-1, h.TipHash(), b[forkAt:]); err != nil {
		t.Fatalf("validated branch below the newest checkpoint refused: %v", err)
	}
	if h.Tip() != forkAt+2 || h.TipHash() != b[forkAt+2].BlockHash() {
		t.Fatalf("tip %d, want the branch tip at %d", h.Tip(), forkAt+2)
	}
}

// TestSwapBranchDifficultyFromBranch: across a retarget boundary, the swapped
// branch's boundary header is judged by the branch's own ancestors. Bits that
// are correct for the stored chain's ancestors at that height are refused.
func TestSwapBranchDifficultyFromBranch(t *testing.T) {
	p := intervalTestParams(nil, false)
	const boundary = testRetargetInterval
	const fork = boundary - 3
	a := synthRuleChain(t, p, boundary+3) // 30 s blocks
	// The branch mines faster after the fork, so its boundary retarget
	// differs from the stored chain's and it carries more work.
	branch := extendRuleChain(t, p, a[:fork+1], 6, 5*time.Second, 77_000)
	if branch[boundary-fork-1].Bits == a[boundary].Bits {
		t.Fatal("test setup: the two chains retarget alike at the boundary")
	}

	h := storeWith(t, p, a)
	forged := append([]*wire.BlockHeader(nil), branch...)
	f := *forged[boundary-fork-1]
	forged[boundary-fork-1] = &f
	remine(forged[boundary-fork-1], a[boundary].Bits)
	forged = relink(forged, boundary-fork-1)
	if height, ok := isRuleError(func() error {
		_, err := h.SwapBranch(fork, h.TipHash(), forged)
		return err
	}()); !ok || height != boundary {
		t.Fatalf("branch carrying the stored chain's boundary bits was not a RuleError at %d", boundary)
	}

	if _, err := h.SwapBranch(fork, h.TipHash(), branch); err != nil {
		t.Fatalf("SwapBranch: %v", err)
	}
	if h.TipHash() != branch[len(branch)-1].BlockHash() {
		t.Fatal("store did not follow the branch")
	}
}

// TestValidateBranch: the read-only validator checks a branch against the
// store, including the anchor rule at a checkpoint-anchored StartHeight, and
// returns its work without changing anything.
func TestValidateBranch(t *testing.T) {
	chain := synthChain(20, 51)
	const anchor = 5
	p := paramsWithCheckpoints([]netparams.Checkpoint{{Height: anchor, Hash: ptrHash(chain[anchor].BlockHash())}})
	h := mustNewHeaders(t, &ChainConfig{Chain: bisonwire.ChainBTC, Params: p, HeaderSize: 80, StartHeight: anchor})
	if _, err := h.AppendBatch(anchor, serializeBatch(t, chain[anchor:15])); err != nil {
		t.Fatalf("seed: %v", err)
	}

	work, err := h.validateBranch(anchor-1, chain[anchor:18])
	if err != nil {
		t.Fatalf("branch rooted below the anchor: %v", err)
	}
	if want := new(big.Int).Mul(blockchain.CalcWork(0x207fffff), big.NewInt(18-anchor)); work.Cmp(want) != 0 {
		t.Fatalf("work %s, want %s", work, want)
	}
	if _, err := h.validateBranch(9, chain[10:18]); err != nil {
		t.Fatalf("branch rooted at a stored header: %v", err)
	}
	if h.Tip() != 14 {
		t.Fatalf("validateBranch moved the tip to %d", h.Tip())
	}

	notAnchor := relink(append([]*wire.BlockHeader{chain[anchor+1]}, chain[anchor+1:10]...), 0)
	if _, err := h.validateBranch(anchor-1, notAnchor); !errors.Is(err, errCheckpointMismatch) {
		t.Fatalf("branch not starting at the anchor: got %v, want errCheckpointMismatch", err)
	}
	if _, err := h.validateBranch(15, chain[16:18]); err == nil {
		t.Fatal("branch rooted above the tip accepted")
	}
}

// TestLoadCheckpointUpgrade: a release can add checkpoints below or above a
// stored tip. On load, a stored chain that took another branch through a new
// checkpoint is truncated to the highest checkpoint it holds below the
// mismatch and resyncs from there; a matching new checkpoint changes
// nothing; a new checkpoint above the tip leaves the store unchanged and not
// holding the newest checkpoint until it syncs through it.
func TestLoadCheckpointUpgrade(t *testing.T) {
	const anchor, forkAt = 2, 6
	stored, canonical := forkedChain(forkAt, 16, 16)
	cp := func(height int32, hdr *wire.BlockHeader) netparams.Checkpoint {
		return netparams.Checkpoint{Height: height, Hash: ptrHash(hdr.BlockHash())}
	}
	open := func(t *testing.T, dir string, cps ...netparams.Checkpoint) *Headers {
		t.Helper()
		h, err := NewHeaders(&ChainConfig{Chain: bisonwire.ChainBTC, HeaderSize: 80, StartHeight: anchor,
			Params: paramsWithCheckpoints(cps)}, dir)
		if err != nil {
			t.Fatalf("NewHeaders: %v", err)
		}
		return h
	}
	seed := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if _, err := open(t, dir, cp(anchor, stored[anchor])).AppendBatch(anchor,
			serializeBatch(t, stored[anchor:12])); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return dir
	}

	t.Run("mismatch below the tip", func(t *testing.T) {
		dir := seed(t)
		// Checkpoints at 4 (shared by both chains) and 8 (canonical only).
		h := open(t, dir, cp(anchor, stored[anchor]), cp(4, stored[4]), cp(8, canonical[8]))
		if h.Tip() != 4 || h.TipHash() != stored[4].BlockHash() {
			t.Fatalf("loaded tip %d, want the held checkpoint at 4", h.Tip())
		}
		if h.HoldsNewestCheckpoint() {
			t.Fatal("store holds a checkpoint it disagrees with")
		}
		if _, err := h.AppendBatch(5, serializeBatch(t, canonical[5:16])); err != nil {
			t.Fatalf("resync onto the canonical chain: %v", err)
		}
		if !h.HoldsNewestCheckpoint() || h.TipHash() != canonical[15].BlockHash() {
			t.Fatal("store did not recover onto the canonical chain")
		}
		// The truncation persisted: a fresh load finds the canonical chain.
		if h2 := open(t, dir, cp(anchor, stored[anchor]), cp(4, stored[4]), cp(8, canonical[8])); h2.Tip() != 15 {
			t.Fatalf("reloaded tip %d, want 15", h2.Tip())
		}
	})

	t.Run("lowest mismatch decides", func(t *testing.T) {
		dir := seed(t)
		h := open(t, dir, cp(anchor, stored[anchor]), cp(4, stored[4]),
			cp(7, canonical[7]), cp(10, canonical[10]))
		if h.Tip() != 4 {
			t.Fatalf("loaded tip %d, want 4", h.Tip())
		}
	})

	t.Run("matching checkpoint below the tip", func(t *testing.T) {
		dir := seed(t)
		h := open(t, dir, cp(anchor, stored[anchor]), cp(8, stored[8]))
		if h.Tip() != 11 || !h.HoldsNewestCheckpoint() {
			t.Fatalf("loaded tip %d (holds newest %v), want 11 holding it", h.Tip(), h.HoldsNewestCheckpoint())
		}
	})

	t.Run("checkpoint above the tip", func(t *testing.T) {
		dir := seed(t)
		h := open(t, dir, cp(anchor, stored[anchor]), cp(14, stored[14]))
		if h.Tip() != 11 || h.HoldsNewestCheckpoint() {
			t.Fatalf("loaded tip %d (holds newest %v), want 11 not holding it", h.Tip(), h.HoldsNewestCheckpoint())
		}
		if _, err := h.AppendBatch(12, serializeBatch(t, stored[12:16])); err != nil {
			t.Fatalf("sync through the new checkpoint: %v", err)
		}
		if !h.HoldsNewestCheckpoint() {
			t.Fatal("store does not hold the checkpoint it synced through")
		}
	})

	t.Run("genesis-rooted store", func(t *testing.T) {
		dir := t.TempDir()
		g, err := NewHeaders(&ChainConfig{Chain: bisonwire.ChainBTC, HeaderSize: 80,
			Params: paramsWithCheckpoints(nil)}, dir)
		if err != nil {
			t.Fatalf("NewHeaders: %v", err)
		}
		if _, err := g.AppendBatch(0, serializeBatch(t, stored[:12])); err != nil {
			t.Fatalf("seed: %v", err)
		}
		h, err := NewHeaders(&ChainConfig{Chain: bisonwire.ChainBTC, HeaderSize: 80,
			Params: paramsWithCheckpoints([]netparams.Checkpoint{cp(8, canonical[8])})}, dir)
		if err != nil {
			t.Fatalf("NewHeaders: %v", err)
		}
		if h.Tip() != -1 {
			t.Fatalf("loaded tip %d, want an empty store", h.Tip())
		}
		if _, err := h.AppendBatch(0, serializeBatch(t, canonical)); err != nil {
			t.Fatalf("resync: %v", err)
		}
	})
}

// TestCheckpointHeldByHash: the rewind floor and SwapBranch's work rule key on
// holding the newest checkpoint by hash. A stored chain at the checkpoint's
// height on another branch does not hold it, so neither applies.
func TestCheckpointHeldByHash(t *testing.T) {
	const forkAt = 10
	a, b := forkedChain(forkAt, 15, 13)
	p := paramsWithCheckpoints(nil)
	h := storeWith(t, p, a)
	// Install a checkpoint at the stored tip that the stored chain does not
	// match, bypassing the load-time check that would otherwise catch it.
	// The branch ends below it, so only the held-checkpoint rules see it.
	p.Checkpoints = []netparams.Checkpoint{{Height: 14, Hash: &chainhash.Hash{5}}}
	if h.HoldsNewestCheckpoint() {
		t.Fatal("store holds a checkpoint whose hash it does not match")
	}
	if _, err := h.SwapBranch(forkAt-1, h.TipHash(), b[forkAt:]); err != nil {
		t.Fatalf("lower-work branch refused while the newest checkpoint is not held: %v", err)
	}
	if _, err := h.Rewind(h.Tip() - 5); err != nil {
		t.Fatalf("rewind below an unheld checkpoint refused: %v", err)
	}
}
