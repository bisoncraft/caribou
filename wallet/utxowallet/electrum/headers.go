// Copyright (c) 2024 The Caribou Developers
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
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// reorgRewindDepth is the search window, in headers below the tip, that
// reorg recovery compares against the server's chain when an incoming header
// fails to connect. The store rewinds exactly to the highest header the two
// chains share within the window; a window with no shared header rewinds
// whole and the search repeats from the lowered tip. The value matches the
// upstream electrum client and covers every reorg observed in practice on BTC
// and LTC in a single step.
const reorgRewindDepth = 8

// maxOrphans bounds the headers retained after a rewind. A consumer rolling
// back a reorg resolves each disconnected block's parent by hash, and for
// every orphan but the deepest that parent is itself an orphan the store no
// longer serves; retaining them keeps the rollback resolvable. Reorgs on BTC
// and LTC are a few blocks deep, so the bound is generous, and the memory is
// a few KiB.
const maxOrphans = 64

// maxReorgDepth bounds how far catchUp will rewind, in total, chasing a fork
// point during reorg recovery. It's far deeper than any reorg seen on BTC/LTC
// mainnet; a divergence beyond it almost certainly means a corrupt local store
// or a hostile/forked server, so we surface a clear error rather than rewind
// unboundedly.
const maxReorgDepth = 200

// errHeaderDoesNotConnect is returned (wrapped) by AppendBatch and AppendOne
// when the first header fails to link to the current tip. catchUp treats it
// as a reorg signal: the server's chain diverges at or below our tip, so it
// rewinds to the fork and retries rather than aborting the sync. A break
// INSIDE a batch is a different error: the server contradicts itself.
var errHeaderDoesNotConnect = errors.New("electrum: header does not connect")

// maxFutureBlockTime is how far past local time a header's timestamp may lie,
// the consensus value (MAX_FUTURE_BLOCK_TIME) on BTC, LTC and Dash.
const maxFutureBlockTime = 2 * time.Hour

// errTimeTooNew is returned (wrapped) for a header timestamped more than
// maxFutureBlockTime past local time. Local clock skew produces the same
// error, so it is no evidence against the server.
var errTimeTooNew = errors.New("electrum: header timestamp too far in the future")

// headerFileName is the on-disk header file relative to the chain data dir.
const headerFileName = "blockchain_headers"

// Headers is a chain of block headers synced from an electrumx server,
// persisted to disk and indexed in-memory by both height and block hash.
//
// Validation is split by height around the newest checkpoint in the chain
// params. Every header passes linkage, checkpoint hashes, the target range
// (0 < target <= PowLimit), the contextual bits rule (NextBits) and the
// future-timestamp bound (maxFutureBlockTime). Headers above the newest
// checkpoint also pass the PoW hash check. Headers at or below it are
// authenticated by hash linkage to that checkpoint instead, which holds once
// the store reaches it; Synced is withheld until then (see
// Network.syncHeaders), and a store that holds the checkpoint never rewinds
// below it (see rewindLocked). Which headers are hashed therefore depends
// only on the shipped params, never on the tip a server claims. The hashed
// span is at most one checkpoint interval plus release lag, and AppendBatch
// spreads it across GOMAXPROCS workers.
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

	// orphans retains recently rewound headers, keyed by hash, so ByHash can
	// still resolve them for consumers rolling back a reorg (see maxOrphans).
	// orphanOrder is the insertion order used for eviction.
	orphans     map[chainhash.Hash]orphanedHeader
	orphanOrder []chainhash.Hash

	// anchorDifficulty holds the anchor checkpoint's Difficulty entries by
	// height: the difficulty fields of the blocks just below StartHeight,
	// which the store never holds. Immutable after NewHeaders.
	anchorDifficulty map[int32]netparams.BlockDifficulty

	// now is the local clock for the future-timestamp bound. Nil means
	// time.Now; tests substitute a fixed clock.
	now func() time.Time
}

// orphanedHeader is a rewound header and the height it occupied.
type orphanedHeader struct {
	height int32
	hdr    *wire.BlockHeader
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
		orphans:  make(map[chainhash.Hash]orphanedHeader),
	}
	// A non-zero StartHeight skips the linkage check for its first header
	// (there is no parent in the store), so its identity MUST be pinned by
	// a checkpoint at exactly that height — otherwise a hostile server
	// could seed an arbitrary chain.
	if cfg.StartHeight > 0 && h.anchorCheckpoint() == nil {
		return nil, fmt.Errorf("electrum: StartHeight %d has no checkpoint anchor",
			cfg.StartHeight)
	}
	if cp := h.anchorCheckpoint(); cp != nil {
		h.anchorDifficulty = make(map[int32]netparams.BlockDifficulty, len(cp.Difficulty))
		for i, d := range cp.Difficulty {
			h.anchorDifficulty[cp.Height-int32(len(cp.Difficulty)-i)] = d
		}
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

// Synced reports whether the initial sync has completed. Once it has, every
// stored header is either at or below a checkpoint the store holds, and so
// authenticated by hash linkage to it, or PoW-checked.
func (h *Headers) Synced() bool { return h.synced.Load() }

// SetSynced marks the header chain as synced. Called once after the first
// catch-up sync completes successfully, which requires the store to reach the
// newest checkpoint.
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
// if the hash is unknown. Headers rewound by a recent reorg still resolve
// (with the height they occupied) while retained; see maxOrphans.
func (h *Headers) ByHash(hash *chainhash.Hash) (int32, *wire.BlockHeader, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if height, ok := h.byHash[*hash]; ok {
		return height, h.byHeight[height], true
	}
	if o, ok := h.orphans[*hash]; ok {
		return o.height, o.hdr, true
	}
	return 0, nil, false
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
// predecessor and pass the validation described on Headers, or this returns
// an error and the chain is left unchanged.
//
// startHeight must equal Tip()+1; otherwise an error is returned.
func (h *Headers) AppendBatch(startHeight int32, raw []byte) (int, error) {
	headerSize := h.cfg.HeaderSize
	if len(raw)%headerSize != 0 {
		return 0, fmt.Errorf("electrum: header batch length %d not a multiple of %d",
			len(raw), headerSize)
	}
	headers, err := h.parseBatch(raw)
	if err != nil {
		return 0, err
	}

	// The PoW hash check reads nothing from the store, so it runs before
	// the write lock is taken: hashing a batch of scrypt headers takes long
	// enough on a phone that holding the lock would stall readers.
	if err := h.checkBatchPoW(startHeight, headers); err != nil {
		return 0, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if startHeight != h.tip.Load()+1 {
		return 0, fmt.Errorf("electrum: batch start %d does not connect to tip %d",
			startHeight, h.tip.Load())
	}

	prevHash, err := h.prevHashAt(startHeight)
	if err != nil {
		return 0, err
	}
	now := h.clock()
	for i, hdr := range headers {
		height := startHeight + int32(i)
		switch {
		case h.anchoredStart(height):
			// First header of a checkpoint-anchored store: no parent
			// to link against; validateCheckpoint below pins it.
		case hdr.PrevBlock != prevHash && i == 0:
			return 0, fmt.Errorf("%w at %d: prev %s, want %s",
				errHeaderDoesNotConnect, height, hdr.PrevBlock, prevHash)
		case hdr.PrevBlock != prevHash:
			// A break inside the batch is not a reorg signal — the
			// batch links to our tip and then contradicts itself — so
			// it must not wrap errHeaderDoesNotConnect, or reorg
			// recovery would rewind canonical blocks chasing a fork
			// that does not exist.
			return 0, fmt.Errorf("electrum: header batch does not link at %d: "+
				"prev %s, want %s", height, hdr.PrevBlock, prevHash)
		}
		if err := h.validateHeaderLocked(height, hdr, headers, startHeight, now); err != nil {
			return 0, err
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

// validateHeaderLocked runs the checks every header passes regardless of
// height: checkpoint hash, target range, contextual bits and the
// future-timestamp bound. Linkage and the PoW hash are checked by the caller.
// The caller must hold h.mu.
func (h *Headers) validateHeaderLocked(height int32, hdr *wire.BlockHeader,
	inBatch []*wire.BlockHeader, batchStart int32, now time.Time) error {

	if err := h.validateCheckpoint(height, hdr); err != nil {
		return err
	}
	if err := h.checkTarget(hdr); err != nil {
		return fmt.Errorf("electrum: header at %d: %w", height, err)
	}
	if err := h.validateBitsLocked(height, hdr, inBatch, batchStart); err != nil {
		return err
	}
	if limit := now.Add(maxFutureBlockTime); hdr.Timestamp.After(limit) {
		return fmt.Errorf("%w: header at %d has timestamp %s, local time %s",
			errTimeTooNew, height, hdr.Timestamp.UTC().Format(time.RFC3339),
			now.UTC().Format(time.RFC3339))
	}
	return nil
}

// checkBatchPoW runs checkPoW on every header of a batch starting at
// startHeight that lies above the newest checkpoint, across GOMAXPROCS
// workers. Heights ascend through the batch, so the checked headers are a
// suffix of it. Workers stop taking headers after the first failure; the
// error reported is the lowest failing height found.
func (h *Headers) checkBatchPoW(startHeight int32, headers []*wire.BlockHeader) error {
	first := max(int(h.newestCheckpointHeight()-startHeight+1), 0)
	if first >= len(headers) {
		return nil
	}
	check := headers[first:]
	errs := make([]error, len(check))
	var next atomic.Int64
	var failed atomic.Bool
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(check)) {
		wg.Go(func() {
			for !failed.Load() {
				i := int(next.Add(1) - 1)
				if i >= len(check) {
					return
				}
				if err := h.checkPoW(check[i]); err != nil {
					errs[i] = err
					failed.Store(true)
				}
			}
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("electrum: header at %d: %w", startHeight+int32(first+i), err)
		}
	}
	return nil
}

// newestCheckpointHeight returns the height of the newest checkpoint in the
// chain params, or -1 when there is none, in which case every header is
// PoW-checked.
func (h *Headers) newestCheckpointHeight() int32 {
	newest := int32(-1)
	if h.cfg.Params != nil {
		for _, cp := range h.cfg.Params.Checkpoints {
			if cp.Hash != nil {
				newest = max(newest, cp.Height)
			}
		}
	}
	return newest
}

// clock returns the local time for the future-timestamp bound.
func (h *Headers) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// AppendOne appends a single header that must connect to the current tip.
//
// A header that does not link to the tip returns an error wrapping
// errHeaderDoesNotConnect and leaves the store untouched: the server's chain
// diverges at or below the tip, and the caller resolves the fork through the
// catch-up path, which locates the fork point against the server's chain
// instead of rewinding blindly. Any other error is fatal — a write/persist
// failure or a PoW violation — and also leaves the chain untouched.
func (h *Headers) AppendOne(hdr *wire.BlockHeader) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	tip := h.tip.Load()
	prevHash, err := h.prevHashAt(tip + 1)
	if err != nil {
		return err
	}
	if hdr.PrevBlock != prevHash && !h.anchoredStart(tip+1) {
		return fmt.Errorf("%w at %d: prev %s, want %s",
			errHeaderDoesNotConnect, tip+1, hdr.PrevBlock, prevHash)
	}
	newHeight := tip + 1
	if err := h.validateHeaderLocked(newHeight, hdr, nil, 0, h.clock()); err != nil {
		return err
	}
	// AppendOne is the tip-update path, which follows initial sync, so the
	// new header normally lies above the newest checkpoint. The PoW check
	// runs regardless of height: it costs one hash.
	if err := h.checkPoW(hdr); err != nil {
		return fmt.Errorf("electrum: header at %d: %w", newHeight, err)
	}
	var buf bytes.Buffer
	if err := hdr.Serialize(&buf); err != nil {
		return err
	}
	if err := h.appendToFile(buf.Bytes()); err != nil {
		return err
	}
	h.byHeight[newHeight] = hdr
	h.byHash[h.headerHash(hdr)] = newHeight
	h.tip.Store(newHeight)
	return nil
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
// recovery (Network.rewindToFork). The dropped headers stay resolvable by
// hash for a while; see maxOrphans.
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
	// A store that holds the newest checkpoint never rewinds below it. An
	// honest chain cannot reorg across a checkpoint, and the headers at or
	// below it carry no PoW check, so re-appending them after such a rewind
	// would accept unhashed headers while Synced is true. Below the
	// checkpoint (initial sync, Synced false) rewinds proceed as usual.
	// Branch swaps must keep the same invariant: once the store holds the
	// newest checkpoint, no fork point lies below it.
	if cp := h.newestCheckpointHeight(); tip >= cp && tip-depth < cp {
		return nil, fmt.Errorf("electrum: cannot rewind %d from tip %d below checkpoint %d",
			depth, tip, cp)
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
			hash := h.headerHash(hdr)
			delete(h.byHash, hash)
			h.retainOrphanLocked(hash, height, hdr)
		}
		delete(h.byHeight, height)
	}
	h.tip.Store(tip - depth)
	return disc, nil
}

// retainOrphanLocked records a rewound header for ByHash lookups, evicting
// the oldest retained header past maxOrphans. The caller must hold h.mu.
func (h *Headers) retainOrphanLocked(hash chainhash.Hash, height int32, hdr *wire.BlockHeader) {
	if _, ok := h.orphans[hash]; !ok {
		h.orphanOrder = append(h.orphanOrder, hash)
	}
	h.orphans[hash] = orphanedHeader{height: height, hdr: hdr}
	for len(h.orphanOrder) > maxOrphans {
		delete(h.orphans, h.orphanOrder[0])
		h.orphanOrder = h.orphanOrder[1:]
	}
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

// validateBitsLocked verifies that hdr carries the difficulty bits consensus
// requires at height, as computed by the chain's NextBits rule from the
// difficulty fields of its ancestors. It runs at every height above the
// anchor, deep history included: it compares integers and hashes nothing, and
// it is what keeps a server from lowering difficulty anywhere on the chain.
// The anchor header is skipped, since its checkpoint pins it.
//
// Ancestors resolve from inBatch (the headers being appended, indexed from
// batchStart), then the store, then the anchor checkpoint's Difficulty
// entries, which stand in for the blocks below the anchor that the rule reads
// when validating the blocks just above it. A missing input is an error, never
// a pass: a rule that cannot be evaluated has not been satisfied.
//
// Networks whose params carry no NextBits (testnets, regtest, and chains whose
// rule is not implemented) skip the check.
//
// The caller must hold h.mu (read or write).
func (h *Headers) validateBitsLocked(height int32, hdr *wire.BlockHeader,
	inBatch []*wire.BlockHeader, batchStart int32) error {

	if h.cfg.Params == nil || h.cfg.Params.NextBits == nil || h.anchoredStart(height) {
		return nil
	}
	at := func(ancestor int32) (netparams.BlockDifficulty, bool) {
		if hdr := h.headerAt(ancestor, inBatch, batchStart); hdr != nil {
			return netparams.HeaderDifficulty(hdr), true
		}
		d, ok := h.anchorDifficulty[ancestor]
		return d, ok
	}
	want, ok := h.cfg.Params.NextBits(height, at)
	if !ok {
		return fmt.Errorf("electrum: header at %d: difficulty rule inputs unavailable", height)
	}
	if hdr.Bits != want {
		return fmt.Errorf("electrum: header at %d has bits 0x%08x, consensus requires 0x%08x",
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

// checkTarget rejects a header whose target is not in (0, PowLimit], matching
// the range check of CheckProofOfWork in Bitcoin Core and Dash Core. The
// compact encoding can express zero, negative and overflowing targets, and a
// target above PowLimit is easier than consensus allows anywhere on the
// chain. It reads only the header, so it runs on every header, deep history
// included. Params without a PowLimit (some test configurations) skip it.
func (h *Headers) checkTarget(hdr *wire.BlockHeader) error {
	if h.cfg.Params == nil || h.cfg.Params.PowLimit == nil {
		return nil
	}
	if target := blockchain.CompactToBig(hdr.Bits); target.Sign() <= 0 ||
		target.Cmp(h.cfg.Params.PowLimit) > 0 {
		return fmt.Errorf("bits 0x%08x encode a target outside (0, PowLimit]", hdr.Bits)
	}
	return nil
}

// checkPoW validates hdr's target range (checkTarget) and that its PoW hash
// meets the target it claims. Chains that supply a CheckPoW hook on their
// netparams (Litecoin's scrypt, Dash's X11) delegate the hash check to it;
// otherwise the standard double-SHA256 check runs. Whether the claimed target
// is the one consensus requires at that height is validateBitsLocked's job.
func (h *Headers) checkPoW(hdr *wire.BlockHeader) error {
	if err := h.checkTarget(hdr); err != nil {
		return err
	}
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

// anchorCheckpoint returns the checkpoint pinning the header at StartHeight,
// or nil when no checkpoint applies there. NewHeaders requires an anchor
// checkpoint for any non-zero StartHeight, so nil means StartHeight is 0
// (regtest and no-checkpoint test configs), where genesis linkage
// authenticates the chain instead.
func (h *Headers) anchorCheckpoint() *netparams.Checkpoint {
	if h.cfg.Params == nil {
		return nil
	}
	for i := range h.cfg.Params.Checkpoints {
		cp := &h.cfg.Params.Checkpoints[i]
		if cp.Height == h.cfg.StartHeight && cp.Hash != nil {
			return cp
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
// filesystem (NFS, full disk) surfaces alongside a write error rather than
// a silent disagreement between memory and disk on the next restart.
func (h *Headers) appendToFile(raw []byte) (err error) {
	f, err := os.OpenFile(h.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("electrum: open header file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			closeErr := fmt.Errorf("electrum: close header file: %w", cerr)
			if err != nil {
				err = errors.Join(err, closeErr)
			} else {
				err = closeErr
			}
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
	// configured anchor moved since the file was written (the wallet
	// birthday selected a different checkpoint), that re-indexing silently
	// mislabels every stored header; with a newer anchor the relabeled tip
	// typically overshoots the real chain, so the first sync request comes
	// back empty and stale headers are served as current. Pin the file's
	// identity the same way the append path does: the first stored header
	// must hash to the anchor checkpoint. On mismatch (either bump
	// direction), discard the file and start clean from the configured
	// anchor; the normal first sync repopulates it.
	if cp := h.anchorCheckpoint(); cp != nil {
		if got, want := h.headerHash(hdrs[0]), cp.Hash; got != *want {
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
