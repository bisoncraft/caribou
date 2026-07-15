// Copyright (c) 2024 The Bison Wallet developers
// Copyright (c) The go-electrum-client authors
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// reorgRewindDepth controls how many headers we drop from our tip on each
// step when an incoming header fails to connect. The value matches the
// upstream electrum client and is large enough to absorb every reorg observed
// in practice on BTC and LTC in a single step.
const reorgRewindDepth = 8

// maxReorgDepth bounds how far catchUp will rewind, in total, chasing a fork
// point during reorg recovery. It's far deeper than any reorg seen on BTC/LTC
// mainnet; a divergence beyond it almost certainly means a corrupt local store
// or a hostile/forked server, so we surface a clear error rather than rewind
// unboundedly.
const maxReorgDepth = 200

// errHeaderDoesNotConnect is returned (wrapped) by AppendBatch when the first
// header of a batch fails to link to the current tip. catchUp treats it as a
// reorg signal: the server's chain diverges below our tip, so it rewinds and
// retries rather than aborting the sync.
var errHeaderDoesNotConnect = errors.New("electrum: header does not connect")

// recentPoWWindow is the number of recent headers (counting back from the
// server-reported tip at sync time) that get full per-header PoW
// validation. Older headers are validated by checkpoint match plus
// retarget-structure: the difficulty Bits at each retarget boundary must
// be the value computed from the prior chunk's timestamp delta. Per-header
// PoW on deep history would force a full scrypt walk over millions of LTC
// blocks (tens of minutes) for no meaningful additional safety beyond
// what checkpoints + retarget structure already provide.
const recentPoWWindow = 2016

// retargetInterval is the number of blocks between difficulty adjustments
// on BTC and LTC. It's a power-of-two convenience for both chains.
const retargetInterval = 2016

// headerFileName is the on-disk header file relative to the chain data dir.
const headerFileName = "blockchain_headers"

// Headers is a chain of block headers synced from an electrumx server,
// persisted to disk and indexed in-memory by both height and block hash.
//
// Headers is safe for concurrent use. Mutation (Append, Truncate) is
// serialized by an internal lock; read methods (Tip, ByHeight, ByHash) take
// a read lock.
type Headers struct {
	cfg *ChainConfig
	// dir is the directory in which blockchain_headers lives.
	dir string
	// filePath is the absolute path to the header file.
	filePath string

	tip    atomic.Int32 // height of the last connected header
	synced atomic.Bool  // true once initial sync completes

	mu       sync.RWMutex
	byHeight map[int32]*wire.BlockHeader
	byHash   map[chainhash.Hash]int32
}

// NewHeaders constructs a Headers backed by dir/electrum/<chain>/. The
// directory is created if it does not exist. Existing headers (if any) are
// loaded and verified before returning.
//
// NewHeaders does NOT contact any server; it only restores local state. Call
// Sync on the returned Headers (or have a Node sync into it) to fetch new
// headers from a peer.
func NewHeaders(cfg *ChainConfig, dataDir string) (*Headers, error) {
	if cfg == nil {
		return nil, errors.New("electrum: nil chain config")
	}
	dir := filepath.Join(dataDir, "electrum", string(cfg.Chain))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("electrum: create %s: %w", dir, err)
	}
	h := &Headers{
		cfg:      cfg,
		dir:      dir,
		filePath: filepath.Join(dir, headerFileName),
		byHeight: make(map[int32]*wire.BlockHeader, 4096),
		byHash:   make(map[chainhash.Hash]int32, 4096),
	}
	// A non-zero StartHeight skips the linkage check for its first header
	// (there is no parent in the store), so its identity MUST be pinned by
	// a checkpoint at exactly that height — otherwise a hostile server
	// could seed an arbitrary chain.
	if cfg.StartHeight > 0 && h.anchorCheckpointHash() == nil {
		return nil, fmt.Errorf("electrum: StartHeight %d has no checkpoint anchor",
			cfg.StartHeight)
	}
	h.tip.Store(cfg.StartHeight - 1) // pre-genesis if StartHeight == 0
	if err := h.loadFromDisk(); err != nil {
		return nil, err
	}
	return h, nil
}

// Tip returns the height of the highest connected header, or StartHeight-1
// if no headers have been connected yet.
//
// Tip reflects the truth about the in-memory store — including a momentary
// dip during a reorg rewind. The old implementation masked the dip behind a
// "recovery tip" but that broke recovery in subtle ways (review2 #2): the
// masked tip pointed at a header that had just been deleted, so
// HeaderByHeight(Tip()) returned nil and the recovery path's catch-up
// floored at the masked height instead of the real one. Consumers that
// don't want to see the dip should subscribe to TipChange events and use
// the Disconnected entries to drive their own rollback.
func (h *Headers) Tip() int32 {
	return h.tip.Load()
}

// Synced reports whether the initial sync has completed.
func (h *Headers) Synced() bool { return h.synced.Load() }

// SetSynced marks the header chain as synced. Called once after the first
// catch-up sync completes successfully.
func (h *Headers) SetSynced(b bool) { h.synced.Store(b) }

// StartHeight is the configured lowest height we accept.
func (h *Headers) StartHeight() int32 { return h.cfg.StartHeight }

// ByHeight returns the block header at height, or nil if unknown.
func (h *Headers) ByHeight(height int32) *wire.BlockHeader {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.byHeight[height]
}

// ByHash returns the height and header for a given block hash, and ok=false
// if the hash is unknown.
func (h *Headers) ByHash(hash *chainhash.Hash) (int32, *wire.BlockHeader, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	height, ok := h.byHash[*hash]
	if !ok {
		return 0, nil, false
	}
	return height, h.byHeight[height], true
}

// headerHash returns the chain's canonical block-header hash (block identity).
// For Dash this is X11; for BTC/LTC it is the standard double-SHA256. All
// linkage, indexing, and lookup must go through this rather than
// wire.BlockHeader.BlockHash() directly, so chains whose identity is not
// double-SHA256 (Dash) link and index correctly. It is nil-safe via
// ChainParams.HeaderHash, so tests without params still get the default.
func (h *Headers) headerHash(hdr *wire.BlockHeader) chainhash.Hash {
	return h.cfg.Params.HeaderHash(hdr)
}

// TipHash returns the hash of the highest connected header. Returns the zero
// hash if no headers are connected.
func (h *Headers) TipHash() chainhash.Hash {
	h.mu.RLock()
	defer h.mu.RUnlock()
	hdr := h.byHeight[h.tip.Load()]
	if hdr == nil {
		return chainhash.Hash{}
	}
	return h.headerHash(hdr)
}

// AppendBatch appends a batch of raw headers (concatenated, headerSize bytes
// each) starting at the given height. Each header must connect to its
// predecessor or this returns an error and the chain is left unchanged.
//
// startHeight must equal Tip()+1; otherwise an error is returned.
//
// tipHint is the server-reported best height the caller is syncing toward.
// Headers within the trailing recentPoWWindow of tipHint get full
// per-header PoW; older headers are validated by checkpoint match plus
// retarget-structure (cheap, catches trivial-bits forgery without forcing
// scrypt on millions of LTC blocks). Pass tipHint=0 to validate every
// header — useful for bench/test paths that want the strict mode.
func (h *Headers) AppendBatch(startHeight int32, raw []byte, tipHint int32) (int, error) {
	headerSize := h.cfg.HeaderSize
	if len(raw)%headerSize != 0 {
		return 0, fmt.Errorf("electrum: header batch length %d not a multiple of %d",
			len(raw), headerSize)
	}
	headers, err := h.parseBatch(raw)
	if err != nil {
		return 0, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if startHeight != h.tip.Load()+1 {
		return 0, fmt.Errorf("electrum: batch start %d does not connect to tip %d",
			startHeight, h.tip.Load())
	}

	// Validate each header. We always check linkage and checkpoint hashes;
	// retarget Bits are checked at boundaries; per-header PoW only applies
	// to headers within recentPoWWindow of tipHint (or all headers when
	// tipHint==0, the strict mode).
	prevHash, err := h.prevHashAt(startHeight)
	if err != nil {
		return 0, err
	}
	for i, hdr := range headers {
		height := startHeight + int32(i)
		if h.anchoredStart(height) {
			// First header of a checkpoint-anchored store: no parent
			// to link against; validateCheckpoint below pins it.
		} else if hdr.PrevBlock != prevHash {
			return 0, fmt.Errorf("%w at %d: prev %s, want %s",
				errHeaderDoesNotConnect, height, hdr.PrevBlock, prevHash)
		}
		if err := h.validateCheckpoint(height, hdr); err != nil {
			return 0, err
		}
		if err := h.validateRetargetLocked(height, hdr, headers, startHeight); err != nil {
			return 0, err
		}
		if h.shouldCheckPoW(height, tipHint) {
			if err := h.checkPoW(hdr); err != nil {
				return 0, fmt.Errorf("electrum: header at %d: %w", height, err)
			}
		}
		prevHash = h.headerHash(hdr)
	}

	// Persist before mutating in-memory state so an interrupted write
	// leaves us in a recoverable state.
	if err := h.appendToFile(raw); err != nil {
		return 0, err
	}
	for i, hdr := range headers {
		height := startHeight + int32(i)
		h.byHeight[height] = hdr
		h.byHash[h.headerHash(hdr)] = height
	}
	h.tip.Store(startHeight + int32(len(headers)) - 1)
	return len(headers), nil
}

// shouldCheckPoW reports whether the header at `height` falls within the
// trailing recentPoWWindow of the server-reported tipHint. Returns true also
// when tipHint == 0 (strict mode — used by AppendOne, by tests, and as a
// safe default when the caller doesn't know the tip).
func (h *Headers) shouldCheckPoW(height, tipHint int32) bool {
	if tipHint <= 0 {
		return true
	}
	return height > tipHint-recentPoWWindow
}

// AppendOne appends a single header that must connect to the current tip.
//
// Return values:
//   - connected=true, disconnected=nil, err=nil — header appended cleanly.
//   - connected=false, disconnected!=nil, err=nil — header did NOT connect;
//     a reorg rewind was performed and the caller should expect a fresh
//     headers subscription notification to bring us to the new chain. The
//     disconnected slice (newest-first) is what was rolled back, so the
//     caller can surface BlockDisconnected events.
//   - connected=false, disconnected=nil, err!=nil — fatal: write/persist
//     failure or PoW violation. The chain is left untouched.
func (h *Headers) AppendOne(hdr *wire.BlockHeader) (connected bool, disconnected []DisconnectedBlock, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tip := h.tip.Load()
	prevHash, err := h.prevHashAt(tip + 1)
	if err != nil {
		return false, nil, err
	}
	if hdr.PrevBlock != prevHash && !h.anchoredStart(tip+1) {
		log.Debugf("electrum: incoming header at %d does not connect (prev %s, want %s); rewinding %d",
			tip+1, hdr.PrevBlock, prevHash, reorgRewindDepth)
		disc, err := h.rewindLocked(reorgRewindDepth)
		if err != nil {
			return false, nil, err
		}
		return false, disc, nil
	}
	newHeight := tip + 1
	if err := h.validateCheckpoint(newHeight, hdr); err != nil {
		return false, nil, err
	}
	// AppendOne is the tip-update path: the incoming header is the new
	// chain tip. Always run per-header PoW here regardless of any
	// caller-side tipHint logic — this is the strongest cheap signal we
	// have that the new tip is honest.
	if err := h.validateRetargetLocked(newHeight, hdr, nil, 0); err != nil {
		return false, nil, err
	}
	if err := h.checkPoW(hdr); err != nil {
		return false, nil, fmt.Errorf("electrum: header at %d: %w", newHeight, err)
	}
	var buf bytes.Buffer
	if err := hdr.Serialize(&buf); err != nil {
		return false, nil, err
	}
	if err := h.appendToFile(buf.Bytes()); err != nil {
		return false, nil, err
	}
	h.byHeight[newHeight] = hdr
	h.byHash[h.headerHash(hdr)] = newHeight
	h.tip.Store(newHeight)
	return true, nil, nil
}

// VerifyAll walks the in-memory chain backwards from tip to StartHeight,
// checking that each header's PrevBlock matches the previous header's hash.
// This is mostly useful as a fixture / post-load sanity check.
func (h *Headers) VerifyAll() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	tip := h.tip.Load()
	for height := tip; height > h.cfg.StartHeight; height-- {
		this := h.byHeight[height]
		prev := h.byHeight[height-1]
		if this == nil || prev == nil {
			return fmt.Errorf("electrum: gap at height %d", height)
		}
		if this.PrevBlock != h.headerHash(prev) {
			return fmt.Errorf("electrum: chain break at height %d", height)
		}
	}
	return nil
}

// Rewind drops the top `depth` headers and returns the disconnected blocks
// (newest-first). It's the locked wrapper around rewindLocked used by reorg
// recovery in catchUp.
func (h *Headers) Rewind(depth int32) ([]DisconnectedBlock, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rewindLocked(depth)
}

// rewindLocked drops the top `depth` headers from memory and truncates the
// file. The caller must hold h.mu.
//
// Returns the disconnected blocks in newest-first order. The returned
// metadata is captured BEFORE the in-memory state mutates so callers can
// surface BlockDisconnected events even though the in-memory entries are
// gone by the time they look.
func (h *Headers) rewindLocked(depth int32) ([]DisconnectedBlock, error) {
	tip := h.tip.Load()
	if tip-depth < h.cfg.StartHeight {
		return nil, fmt.Errorf("electrum: cannot rewind %d below start %d (tip %d)",
			depth, h.cfg.StartHeight, tip)
	}
	// Snapshot disconnected metadata before mutating anything.
	disc := make([]DisconnectedBlock, 0, depth)
	for i := range int(depth) {
		height := tip - int32(i)
		hdr := h.byHeight[height]
		if hdr == nil {
			// Should not happen — the maps are populated alongside
			// the file. If it does, skip rather than block recovery.
			continue
		}
		disc = append(disc, DisconnectedBlock{
			Hash:   h.headerHash(hdr),
			Height: height,
			Time:   hdr.Timestamp.Unix(),
		})
	}
	// File first, in-memory second, so a failed truncate leaves the maps
	// untouched.
	newSize := int64(tip-depth-h.cfg.StartHeight+1) * int64(h.cfg.HeaderSize)
	if err := os.Truncate(h.filePath, newSize); err != nil {
		return nil, fmt.Errorf("electrum: truncate header file: %w", err)
	}
	for i := range int(depth) {
		height := tip - int32(i)
		hdr := h.byHeight[height]
		if hdr != nil {
			delete(h.byHash, h.headerHash(hdr))
		}
		delete(h.byHeight, height)
	}
	h.tip.Store(tip - depth)
	return disc, nil
}

// parseBatch deserializes a concatenated raw header batch.
func (h *Headers) parseBatch(raw []byte) ([]*wire.BlockHeader, error) {
	r := bytes.NewReader(raw)
	count := len(raw) / h.cfg.HeaderSize
	out := make([]*wire.BlockHeader, 0, count)
	for i := range count {
		hdr := new(wire.BlockHeader)
		if err := hdr.Deserialize(r); err != nil {
			return nil, fmt.Errorf("electrum: deserialize header %d/%d: %w", i, count, err)
		}
		// The header is a fixed-width record; advance past any chain-
		// specific trailing bytes (none for BTC/LTC, which are both
		// 80 bytes).
		if h.cfg.HeaderSize > 80 {
			if _, err := io.CopyN(io.Discard, r, int64(h.cfg.HeaderSize-80)); err != nil {
				return nil, err
			}
		}
		out = append(out, hdr)
	}
	return out, nil
}

// validateCheckpoint verifies that, if height matches one of the configured
// checkpoints, the header's hash matches the checkpoint hash. Returns nil
// if no checkpoint applies. A checkpoint mismatch is a strong hostile-server
// signal and fails fast.
func (h *Headers) validateCheckpoint(height int32, hdr *wire.BlockHeader) error {
	if h.cfg.Params == nil {
		return nil
	}
	for _, cp := range h.cfg.Params.Checkpoints {
		if cp.Height != height {
			continue
		}
		got := h.headerHash(hdr)
		if cp.Hash == nil {
			continue
		}
		if got != *cp.Hash {
			return fmt.Errorf("electrum: checkpoint mismatch at %d: server gave %s, expected %s",
				height, got, cp.Hash)
		}
	}
	return nil
}

// validateRetargetLocked verifies the difficulty Bits at a retarget boundary
// matches the value computed from the prior retarget chunk's timestamp
// delta. The check is skipped on chains with PoWNoRetargeting (regtest) and
// for heights that aren't retarget boundaries.
//
// `inBatch` and `batchStart` let the validator find a chunk's first header
// when it's still in the current batch (not yet committed to byHeight).
// AppendOne calls with inBatch=nil and batchStart=0 — in steady-state tip
// updates the prior chunk is always already in byHeight.
//
// The caller must hold h.mu (read or write).
func (h *Headers) validateRetargetLocked(height int32, hdr *wire.BlockHeader,
	inBatch []*wire.BlockHeader, batchStart int32) error {

	if h.cfg.Params == nil || h.cfg.Params.PoWNoRetargeting {
		return nil
	}
	if height == 0 || height%retargetInterval != 0 {
		// Not a retarget boundary; nothing to check (and the prior
		// header's Bits propagates unchanged, but we don't enforce
		// that here — it'd reject legitimate downward adjustments on
		// chains where bits can change off-cycle. The retarget check
		// at the next boundary will catch a tampered chunk).
		return nil
	}
	prior := h.headerAt(height-1, inBatch, batchStart)
	// Bitcoin samples the retarget timespan from the first block OF the
	// period (height-2016). Litecoin deliberately goes back one block
	// further (height-2017, its 51%-attack fix), except at the first-ever
	// retarget — a consensus difference that shows up as a slightly wrong
	// computed target at every boundary if ignored.
	chunkStartHeight := height - retargetInterval
	if h.cfg.Chain == bisonwire.ChainLTC && height != retargetInterval {
		chunkStartHeight--
	}
	chunkStart := h.headerAt(chunkStartHeight, inBatch, batchStart)
	if prior == nil || chunkStart == nil {
		// Can't validate without the prior chunk; soft-pass rather
		// than reject. This triggers at the first boundary above a
		// checkpoint-anchored StartHeight (LTC's sample block is one
		// below the anchor) and for batches predating loadFromDisk's
		// range; the anchor checkpoint and the tip PoW window still
		// guard those.
		return nil
	}
	want := computeNextWorkRequired(h.cfg.Params, prior, chunkStart)
	if hdr.Bits != want {
		return fmt.Errorf("electrum: retarget mismatch at %d: header bits 0x%08x, computed 0x%08x",
			height, hdr.Bits, want)
	}
	return nil
}

// headerAt returns the header at `height`, looking first in `inBatch`
// (which is indexed from batchStart) and falling back to byHeight.
func (h *Headers) headerAt(height int32, inBatch []*wire.BlockHeader, batchStart int32) *wire.BlockHeader {
	if inBatch != nil && height >= batchStart && int(height-batchStart) < len(inBatch) {
		return inBatch[height-batchStart]
	}
	return h.byHeight[height]
}

// computeNextWorkRequired implements the standard Bitcoin/Litecoin retarget
// rule: new_target = prev_target * actual_timespan / expected_timespan,
// clamped to [expected/factor, expected*factor] on the timespan and to
// PowLimit on the target.
//
// This mirrors btcd's blockchain.calcNextRequiredDifficulty. We can't call
// btcd's directly: its standalone form is unexported, and the exported
// CalcNextRequiredDifficulty is a method on *blockchain.BlockChain that needs
// a full instantiated chain with a HeaderCtx node graph — neither of which a
// headers-only electrum client maintains. The duplication is consensus-
// sensitive, so TestComputeNextWorkRequired pins this implementation's output
// against hand-computed expectations (unclamped, both timespan clamps, and the
// PowLimit clamp) to catch drift.
func computeNextWorkRequired(params *netparams.ChainParams, prior, chunkStart *wire.BlockHeader) uint32 {
	expectedTimespan := int64(params.TargetTimespan / time.Second)
	if expectedTimespan == 0 {
		// Params don't define retarget timing (shouldn't happen for
		// BTC/LTC). Accept whatever the server gave us.
		return prior.Bits
	}
	factor := params.RetargetAdjustmentFactor
	if factor == 0 {
		factor = 4 // BTC/LTC standard.
	}
	minTimespan := expectedTimespan / factor
	maxTimespan := expectedTimespan * factor

	actualTimespan := prior.Timestamp.Unix() - chunkStart.Timestamp.Unix()
	actualTimespan = max(min(actualTimespan, maxTimespan), minTimespan)

	prevTarget := blockchain.CompactToBig(prior.Bits)
	newTarget := new(big.Int).Mul(prevTarget, big.NewInt(actualTimespan))
	newTarget.Div(newTarget, big.NewInt(expectedTimespan))
	if params.PowLimit != nil && newTarget.Cmp(params.PowLimit) > 0 {
		newTarget = new(big.Int).Set(params.PowLimit)
	}
	return blockchain.BigToCompact(newTarget)
}

// checkPoW validates that hdr satisfies its own claimed target. For chains
// that supply a custom CheckPoW hook on their netparams (e.g. Litecoin's
// scrypt) the hook is delegated to; otherwise we run the standard Bitcoin
// double-SHA256 check.
//
// We intentionally do not validate retargeting or PowLimit clamps here — a
// hostile server could still claim arbitrary difficulty per header. The
// per-header check is the cheapest meaningful bound: trivial-difficulty
// forgeries fail it, and any cheaper attack would require breaking SHA256d
// (or scrypt) preimage resistance.
func (h *Headers) checkPoW(hdr *wire.BlockHeader) error {
	if h.cfg.Params != nil && h.cfg.Params.CheckPoW != nil {
		return h.cfg.Params.CheckPoW(hdr)
	}
	target := blockchain.CompactToBig(hdr.Bits)
	hash := hdr.BlockHash()
	if blockchain.HashToBig(&hash).Cmp(target) > 0 {
		return fmt.Errorf("block hash %s exceeds target for bits 0x%08x", hash, hdr.Bits)
	}
	return nil
}

// anchorCheckpointHash returns the checkpoint hash pinning the header at
// StartHeight, or nil when no checkpoint applies there. NewHeaders requires
// an anchor checkpoint for any non-zero StartHeight, so nil means
// StartHeight is 0 (regtest and no-checkpoint test configs), where genesis
// linkage authenticates the chain instead.
func (h *Headers) anchorCheckpointHash() *chainhash.Hash {
	if h.cfg.Params == nil {
		return nil
	}
	for _, cp := range h.cfg.Params.Checkpoints {
		if cp.Height == h.cfg.StartHeight && cp.Hash != nil {
			return cp.Hash
		}
	}
	return nil
}

// anchoredStart reports whether height is a non-genesis sync anchor: the
// first stored header, whose parent is deliberately absent. Linkage cannot be
// checked there; the checkpoint at that height authenticates it instead
// (enforced in NewHeaders).
func (h *Headers) anchoredStart(height int32) bool {
	return height == h.cfg.StartHeight && h.cfg.StartHeight > 0
}

func (h *Headers) prevHashAt(height int32) (chainhash.Hash, error) {
	if height == h.cfg.StartHeight {
		// Genesis when StartHeight is 0 (its PrevBlock really is the
		// zero hash); for a non-zero anchor callers skip the linkage
		// comparison via anchoredStart.
		return chainhash.Hash{}, nil
	}
	prev := h.byHeight[height-1]
	if prev == nil {
		return chainhash.Hash{}, fmt.Errorf("electrum: missing header at %d", height-1)
	}
	return h.headerHash(prev), nil
}

// appendToFile appends raw header bytes to the persistent header file. The
// Close error is captured via a named return so a failed flush on a sketchy
// filesystem (NFS, full disk) surfaces as an error rather than a silent
// disagreement between memory and disk on the next restart.
func (h *Headers) appendToFile(raw []byte) (err error) {
	f, err := os.OpenFile(h.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("electrum: open header file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("electrum: close header file: %w", cerr)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("electrum: write header file: %w", err)
	}
	return nil
}

// loadFromDisk reads the persisted header file and populates the in-memory
// indices.
func (h *Headers) loadFromDisk() error {
	st, err := os.Stat(h.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("electrum: stat header file: %w", err)
	}
	size := st.Size()
	if size%int64(h.cfg.HeaderSize) != 0 {
		return fmt.Errorf("electrum: header file size %d not a multiple of header size %d",
			size, h.cfg.HeaderSize)
	}
	count := int32(size / int64(h.cfg.HeaderSize))
	if count == 0 {
		return nil
	}
	raw, err := os.ReadFile(h.filePath)
	if err != nil {
		return fmt.Errorf("electrum: read header file: %w", err)
	}
	hdrs, err := h.parseBatch(raw)
	if err != nil {
		return err
	}
	// The file carries no record of which height its first entry belongs
	// to — entries are indexed as StartHeight+i on every load. If the
	// configured anchor moved since the file was written (the checkpoint
	// lists are refreshed at each release), that re-indexing silently
	// mislabels every stored header; with a newer anchor the relabeled tip
	// typically overshoots the real chain, so the first sync request comes
	// back empty and stale headers are served as current. Pin the file's
	// identity the same way the append path does: the first stored header
	// must hash to the anchor checkpoint. On mismatch (either bump
	// direction), discard the file and start clean from the configured
	// anchor; the normal first sync repopulates it.
	if want := h.anchorCheckpointHash(); want != nil {
		if got := hdrs[0].BlockHash(); got != *want {
			log.Warnf("electrum: stored %s header file does not match the anchor "+
				"checkpoint at height %d (stored %s, want %s); the anchor has "+
				"moved — discarding %d stale headers and resyncing",
				h.cfg.Chain, h.cfg.StartHeight, got, want, count)
			if err := os.Remove(h.filePath); err != nil {
				return fmt.Errorf("electrum: remove stale header file: %w", err)
			}
			return nil
		}
	}
	for i, hdr := range hdrs {
		height := h.cfg.StartHeight + int32(i)
		h.byHeight[height] = hdr
		h.byHash[h.headerHash(hdr)] = height
	}
	h.tip.Store(h.cfg.StartHeight + count - 1)
	log.Infof("electrum: loaded %d headers, tip at %d", count, h.tip.Load())
	return nil
}

// workRange returns the cumulative proof-of-work of the stored headers over
// [from, to], for comparison against a dissenting chain's claimed work.
// Missing headers contribute nothing (the range is clamped to what exists).
func (h *Headers) workRange(from, to int32) *big.Int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	work := new(big.Int)
	for height := from; height <= to; height++ {
		if hdr := h.byHeight[height]; hdr != nil {
			work.Add(work, blockchain.CalcWork(hdr.Bits))
		}
	}
	return work
}
