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

// reorgRewindDepth is the window, in headers, that one fork-search fetch
// compares against the server's chain when an incoming header fails to
// connect (Network.locateFork). The fork point is the highest header the two
// chains share; a window with no shared header moves down and the search
// repeats. The value matches the upstream electrum client and covers every
// reorg observed in practice on BTC and LTC in one window. The observer
// dissent audit uses the same window.
const reorgRewindDepth = 8

// maxOrphans bounds the headers retained after a rewind. A consumer rolling
// back a reorg resolves each disconnected block's parent by hash, and for
// every orphan but the deepest that parent is itself an orphan the store no
// longer serves; retaining them keeps the rollback resolvable. Reorgs on BTC
// and LTC are a few blocks deep, so the bound is generous, and the memory is
// a few KiB.
const maxOrphans = 64

// maxReorgDepth bounds how far below the tip one fork search looks during
// reorg recovery (Network.locateFork). It's far deeper than any reorg seen on
// BTC/LTC mainnet; a divergence beyond it almost certainly means a corrupt
// local store or a hostile/forked server, so it surfaces as an error rather
// than an unbounded search.
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

// errCheckpointMismatch is the cause of a RuleError for a header whose hash
// differs from the checkpoint at its height.
var errCheckpointMismatch = errors.New("checkpoint mismatch")

// errStoreMoved is returned by SwapBranch when the store's tip is no longer
// the one the caller located the fork against. Nothing changes; the caller
// locates the fork again.
var errStoreMoved = errors.New("electrum: header store moved since the fork was located")

// errInsufficientWork is returned by SwapBranch when a branch carries less
// work than the stored span it would replace. Nothing changes.
var errInsufficientWork = errors.New("electrum: branch carries less work than the stored chain")

// RuleError reports a header that breaks a rule the store enforces: a
// checkpoint hash, the target range, the consensus difficulty bits, the PoW
// hash, or linkage inside a batch or branch. Only the server that produced
// the header can cause one, so it is evidence against that server. Errors
// that a local condition or an honest reorg can produce as well (a first
// header that does not link to the store, a timestamp past the local clock's
// bound, a missing local input) are not RuleErrors.
type RuleError struct {
	// Height is the height of the offending header.
	Height int32
	// Err describes the violation.
	Err error
}

func (e *RuleError) Error() string {
	return fmt.Sprintf("electrum: header at %d: %v", e.Height, e.Err)
}

func (e *RuleError) Unwrap() error { return e.Err }

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
// the store holds it: a header stored at its height with its hash (see
// holdsCheckpointLocked). Synced is withheld until then (see
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

	// writeHeaders persists raw headers at the end of the header file. Nil
	// means appendToFile; tests substitute a failing writer to exercise
	// recovery from a failed append. Read under h.mu.
	writeHeaders func(raw []byte) error
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

// tipSnapshot returns the tip height and its hash read together, for a caller
// that later asks SwapBranch to act only if the store has not moved.
func (h *Headers) tipSnapshot() (int32, chainhash.Hash) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	tip := h.tip.Load()
	if hdr := h.byHeight[tip]; hdr != nil {
		return tip, h.headerHash(hdr)
	}
	return tip, chainhash.Hash{}
}

// lowestFork returns the lowest height SwapBranch accepts as a fork point:
// StartHeight, or the newest checkpoint's height once the store holds it.
func (h *Headers) lowestFork() int32 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if cp := h.newestCheckpoint(); cp != nil && h.holdsCheckpointLocked(cp) {
		return max(cp.Height, h.cfg.StartHeight)
	}
	return h.cfg.StartHeight
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
	// enough on a phone that holding the lock would stall readers. Headers at
	// or below the newest checkpoint are authenticated by linkage to it
	// instead (see Headers).
	if err := h.checkBranchPoW(startHeight, headers, h.newestCheckpointHeight()+1); err != nil {
		return 0, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if startHeight != h.tip.Load()+1 {
		return 0, fmt.Errorf("electrum: batch start %d does not connect to tip %d",
			startHeight, h.tip.Load())
	}
	if _, err := h.validateBranchLocked(startHeight-1, headers, h.clock()); err != nil {
		return 0, err
	}
	if err := h.appendLocked(startHeight, headers); err != nil {
		return 0, err
	}
	return len(headers), nil
}

// appendLocked persists hdrs as the headers at start and up, which must be
// Tip()+1, then indexes them. The file is written before the in-memory state
// changes, so an interrupted write leaves the store recoverable. The caller
// must hold h.mu for writing and has validated hdrs.
func (h *Headers) appendLocked(start int32, hdrs []*wire.BlockHeader) error {
	raw := make([]byte, 0, len(hdrs)*h.cfg.HeaderSize)
	for _, hdr := range hdrs {
		var buf bytes.Buffer
		if err := hdr.Serialize(&buf); err != nil {
			return err
		}
		rec := make([]byte, h.cfg.HeaderSize)
		copy(rec, buf.Bytes())
		raw = append(raw, rec...)
	}
	write := h.appendToFile
	if h.writeHeaders != nil {
		write = h.writeHeaders
	}
	if err := write(raw); err != nil {
		return err
	}
	for i, hdr := range hdrs {
		height := start + int32(i)
		h.byHeight[height] = hdr
		h.byHash[h.headerHash(hdr)] = height
	}
	h.tip.Store(start + int32(len(hdrs)) - 1)
	return nil
}

// validateBranchLocked validates hdrs as the headers at heights parent+1 and
// up, extending the stored header at parent, and returns their cumulative
// work. It checks linkage (the first header to the stored parent, or the
// anchor rule when parent+1 is a non-genesis StartHeight, then each header to
// the one before it), checkpoint hashes, the target range, the consensus
// difficulty bits and the future-timestamp bound. It does not check PoW
// hashes; callers run checkBranchPoW, which needs no lock, beforehand.
//
// The difficulty rule reads ancestors above parent only from hdrs, never from
// the store, which may hold a competing chain at those heights. Ancestors at
// or below parent come from the store, whose chain the branch shares there,
// and then from the anchor checkpoint's difficulty record.
//
// A first header that does not link to parent returns an error wrapping
// errHeaderDoesNotConnect, the reorg signal. Rule violations return a
// *RuleError. The caller must hold h.mu (read or write).
func (h *Headers) validateBranchLocked(parent int32, hdrs []*wire.BlockHeader, now time.Time) (*big.Int, error) {
	if len(hdrs) == 0 {
		return nil, errors.New("electrum: empty header branch")
	}
	if parent < h.cfg.StartHeight-1 || parent > h.tip.Load() {
		return nil, fmt.Errorf("electrum: branch parent %d outside the store [%d, %d]",
			parent, h.cfg.StartHeight-1, h.tip.Load())
	}
	prevHash, err := h.prevHashAt(parent + 1)
	if err != nil {
		return nil, err
	}
	work := new(big.Int)
	for i, hdr := range hdrs {
		height := parent + 1 + int32(i)
		switch {
		case h.anchoredStart(height):
			// First header of a checkpoint-anchored store: no parent
			// to link against; validateCheckpoint below pins it.
		case hdr.PrevBlock != prevHash && i == 0:
			return nil, fmt.Errorf("%w at %d: prev %s, want %s",
				errHeaderDoesNotConnect, height, hdr.PrevBlock, prevHash)
		case hdr.PrevBlock != prevHash:
			// A break inside the branch is not a reorg signal: the branch
			// links to the store and then contradicts itself. It must not
			// wrap errHeaderDoesNotConnect, or reorg recovery would chase
			// a fork that does not exist.
			return nil, &RuleError{Height: height, Err: fmt.Errorf(
				"branch does not link: prev %s, want %s", hdr.PrevBlock, prevHash)}
		}
		if err := h.validateCheckpoint(height, hdr); err != nil {
			return nil, err
		}
		if err := h.checkTarget(hdr); err != nil {
			return nil, &RuleError{Height: height, Err: err}
		}
		if err := h.validateBitsLocked(height, hdr, parent, hdrs); err != nil {
			return nil, err
		}
		if limit := now.Add(maxFutureBlockTime); hdr.Timestamp.After(limit) {
			return nil, fmt.Errorf("%w: header at %d has timestamp %s, local time %s",
				errTimeTooNew, height, hdr.Timestamp.UTC().Format(time.RFC3339),
				now.UTC().Format(time.RFC3339))
		}
		work.Add(work, blockchain.CalcWork(hdr.Bits))
		prevHash = h.headerHash(hdr)
	}
	return work, nil
}

// checkBranchPoW runs checkPoW on the headers of a batch or branch starting
// at start whose heights are at least from, across GOMAXPROCS workers. Heights
// ascend through hdrs, so the checked headers are a suffix of it. Workers stop
// taking headers after the first failure; the error reported, a *RuleError, is
// the lowest failing height found. It reads only the headers and the chain
// params, so it needs no lock.
func (h *Headers) checkBranchPoW(start int32, hdrs []*wire.BlockHeader, from int32) error {
	first := max(int(from-start), 0)
	if first >= len(hdrs) {
		return nil
	}
	check := hdrs[first:]
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
			return &RuleError{Height: start + int32(first+i), Err: err}
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

// newestCheckpoint returns the newest checkpoint in the chain params, or nil
// when there is none.
func (h *Headers) newestCheckpoint() *netparams.Checkpoint {
	var newest *netparams.Checkpoint
	if h.cfg.Params != nil {
		for i := range h.cfg.Params.Checkpoints {
			cp := &h.cfg.Params.Checkpoints[i]
			if cp.Hash != nil && (newest == nil || cp.Height > newest.Height) {
				newest = cp
			}
		}
	}
	return newest
}

// holdsCheckpointLocked reports whether the store holds cp: a header stored
// at its height whose hash is the checkpoint's. Height alone is not enough;
// a stored chain can reach a checkpoint's height on another branch, which is
// what loadFromDisk finds when a release adds a checkpoint below a stored
// tip. A nil cp, meaning the params carry no checkpoint, is held trivially.
// The caller must hold h.mu (read or write).
func (h *Headers) holdsCheckpointLocked(cp *netparams.Checkpoint) bool {
	if cp == nil {
		return true
	}
	hdr := h.byHeight[cp.Height]
	return hdr != nil && h.headerHash(hdr) == *cp.Hash
}

// HoldsNewestCheckpoint reports whether the store holds the newest
// checkpoint in the chain params (see holdsCheckpointLocked).
func (h *Headers) HoldsNewestCheckpoint() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.holdsCheckpointLocked(h.newestCheckpoint())
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
// instead of rewinding blindly. A rule violation, the PoW hash included, is a
// *RuleError. Every error leaves the chain untouched.
func (h *Headers) AppendOne(hdr *wire.BlockHeader) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	tip := h.tip.Load()
	branch := []*wire.BlockHeader{hdr}
	if _, err := h.validateBranchLocked(tip, branch, h.clock()); err != nil {
		return err
	}
	// AppendOne is the tip-update path, which follows initial sync, so the
	// new header normally lies above the newest checkpoint. The PoW check
	// runs regardless of height: it costs one hash.
	if err := h.checkPoW(hdr); err != nil {
		return &RuleError{Height: tip + 1, Err: err}
	}
	return h.appendLocked(tip+1, branch)
}

// SwapBranch replaces the stored headers above fork with branch, the headers
// at fork+1 and up on a competing chain, and returns the disconnected blocks
// (newest-first). Everything is checked before the store changes, and nothing
// changes when a check fails:
//
//   - the store's tip hash still equals wantTip, the tip the caller located
//     the fork against, else errStoreMoved;
//   - fork lies below the tip and at or above StartHeight, and once the store
//     holds the newest checkpoint, at or above that checkpoint (see
//     rewindLocked);
//   - branch validates from fork (validateBranchLocked), with the PoW hash
//     checked on every branch header, since a competing branch is exactly
//     what PoW must bound;
//   - once the store holds the newest checkpoint, the branch carries at least
//     as much work as the stored span it replaces, else errInsufficientWork.
//     Equal work is accepted, so the store follows a server that switched to
//     a same-height competitor.
//
// While the store does not hold the newest checkpoint, the work rule does not
// apply. That is a property of the stored chain, not of Synced, which is
// process state: a persisted, authenticated store starts each process
// unsynced. The stored span below the checkpoint was never hashed, so its
// claimed work is unauthenticated and counts for nothing; a span claiming
// more work than any honest branch could otherwise lock the store onto it.
// The checkpoint stays the authority for that span, and no new confirmation
// is verified until the store holds it. A hostile server can still swap in
// fully validated branches below the checkpoint; callers bound the attempts
// per catch-up.
//
// The rewind and the append are separate file operations. A crash between
// them leaves the store at the fork point, which resyncs normally. If the
// append fails after the rewind, the disconnected blocks are returned with
// the error so the caller can still publish them.
func (h *Headers) SwapBranch(fork int32, wantTip chainhash.Hash, branch []*wire.BlockHeader) ([]DisconnectedBlock, error) {
	if len(branch) == 0 {
		return nil, errors.New("electrum: empty header branch")
	}
	if err := h.checkBranchPoW(fork+1, branch, fork+1); err != nil {
		return nil, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	tip := h.tip.Load()
	if hdr := h.byHeight[tip]; hdr == nil || h.headerHash(hdr) != wantTip {
		return nil, errStoreMoved
	}
	if fork >= tip || fork < h.cfg.StartHeight {
		return nil, fmt.Errorf("electrum: fork %d outside the swappable range [%d, %d)",
			fork, h.cfg.StartHeight, tip)
	}
	cp := h.newestCheckpoint()
	holdsCheckpoint := h.holdsCheckpointLocked(cp)
	if holdsCheckpoint && cp != nil && fork < cp.Height {
		return nil, fmt.Errorf("electrum: fork %d lies below checkpoint %d", fork, cp.Height)
	}
	work, err := h.validateBranchLocked(fork, branch, h.clock())
	if err != nil {
		return nil, err
	}
	if holdsCheckpoint {
		if stored := h.workRangeLocked(fork+1, tip); work.Cmp(stored) < 0 {
			return nil, fmt.Errorf("%w: %s < %s over heights %d-%d",
				errInsufficientWork, work, stored, fork+1, tip)
		}
	}
	disc, err := h.rewindLocked(tip - fork)
	if err != nil {
		return nil, err
	}
	if err := h.appendLocked(fork+1, branch); err != nil {
		return disc, err
	}
	return disc, nil
}

// validateBranch validates hdrs as a branch extending the stored header at
// parent, with the PoW hash checked on every header, and returns its
// cumulative work without changing the store. See validateBranchLocked.
func (h *Headers) validateBranch(parent int32, hdrs []*wire.BlockHeader) (*big.Int, error) {
	if err := h.checkBranchPoW(parent+1, hdrs, parent+1); err != nil {
		return nil, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.validateBranchLocked(parent, hdrs, h.clock())
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
// (newest-first). It's the locked wrapper around rewindLocked. Reorg recovery
// goes through SwapBranch instead, which rewinds only with a validated
// replacement in hand. The dropped headers stay resolvable by hash for a
// while; see maxOrphans.
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
	// would accept unhashed headers while Synced is true. A store that does
	// not hold it rewinds as usual.
	// SwapBranch enforces the same invariant for fork points.
	if cp := h.newestCheckpoint(); cp != nil && h.holdsCheckpointLocked(cp) && tip-depth < cp.Height {
		return nil, fmt.Errorf("electrum: cannot rewind %d from tip %d below checkpoint %d",
			depth, tip, cp.Height)
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

// discardToHeldCheckpoint rewinds the store to the highest checkpoint it
// holds by hash, or to StartHeight when it holds none, and returns the
// disconnected blocks (newest-first). See
// Network.discardOnCheckpointMismatch.
func (h *Headers) discardToHeldCheckpoint() ([]DisconnectedBlock, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tip := h.tip.Load()
	floor := h.cfg.StartHeight
	if h.cfg.Params != nil {
		for i := range h.cfg.Params.Checkpoints {
			cp := &h.cfg.Params.Checkpoints[i]
			if cp.Hash != nil && cp.Height > floor && cp.Height <= tip && h.holdsCheckpointLocked(cp) {
				floor = cp.Height
			}
		}
	}
	if tip <= floor {
		return nil, nil
	}
	return h.rewindLocked(tip - floor)
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
// if no checkpoint applies. A mismatch is a *RuleError wrapping
// errCheckpointMismatch.
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
			return &RuleError{Height: height, Err: fmt.Errorf("%w: server gave %s, expected %s",
				errCheckpointMismatch, got, cp.Hash)}
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
// hdr belongs to a branch of headers at heights parent+1 and up, branch[0]
// first. Ancestors above parent resolve only from branch; the store may hold
// a competing chain at those heights. Ancestors at or below parent resolve
// from the store, then from the anchor checkpoint's Difficulty entries, which
// stand in for the blocks below the anchor that the rule reads when
// validating the blocks just above it. A missing input is an error, never a
// pass: a rule that cannot be evaluated has not been satisfied. Wrong bits
// are a *RuleError.
//
// Networks whose params carry no NextBits (testnets, regtest, and chains whose
// rule is not implemented) skip the check.
//
// The caller must hold h.mu (read or write).
func (h *Headers) validateBitsLocked(height int32, hdr *wire.BlockHeader,
	parent int32, branch []*wire.BlockHeader) error {

	if h.cfg.Params == nil || h.cfg.Params.NextBits == nil || h.anchoredStart(height) {
		return nil
	}
	at := func(ancestor int32) (netparams.BlockDifficulty, bool) {
		if ancestor > parent {
			if i := int(ancestor - parent - 1); i < len(branch) {
				return netparams.HeaderDifficulty(branch[i]), true
			}
			return netparams.BlockDifficulty{}, false
		}
		if hdr := h.byHeight[ancestor]; hdr != nil {
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
		return &RuleError{Height: height, Err: fmt.Errorf(
			"bits 0x%08x, consensus requires 0x%08x", hdr.Bits, want)}
	}
	return nil
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
	if err := h.truncateToHeldCheckpoint(); err != nil {
		return err
	}
	log.Infof("electrum: loaded %d headers, tip at %d", h.tip.Load()-h.cfg.StartHeight+1, h.tip.Load())
	return nil
}

// truncateToHeldCheckpoint compares every configured checkpoint the loaded
// chain spans with the stored header at its height. Releases add checkpoints
// below stored tips, and a stored chain that took another branch through a
// newly added checkpoint reaches its height without holding it. Left in
// place, that chain would satisfy the newest-checkpoint height, open the
// Synced gate and sit below the rewind floor for good. On any mismatch the
// store is truncated to the highest checkpoint it holds below the lowest
// mismatch, and resyncs from there. An anchored store always holds its anchor
// (loadFromDisk checks it first); a genesis-rooted store holding none below
// the mismatch is discarded whole. The comparison is a map lookup per checkpoint; nothing is hashed for
// PoW. The caller is NewHeaders, before the store is shared.
func (h *Headers) truncateToHeldCheckpoint() error {
	if h.cfg.Params == nil {
		return nil
	}
	tip := h.tip.Load()
	floor, mismatch := int32(-1), int32(-1)
	for _, cp := range h.cfg.Params.Checkpoints {
		if cp.Hash == nil || cp.Height < h.cfg.StartHeight || cp.Height > tip {
			continue
		}
		if h.holdsCheckpointLocked(&cp) {
			continue
		}
		if mismatch < 0 || cp.Height < mismatch {
			mismatch = cp.Height
		}
	}
	if mismatch < 0 {
		return nil
	}
	for _, cp := range h.cfg.Params.Checkpoints {
		if cp.Hash != nil && cp.Height >= h.cfg.StartHeight && cp.Height < mismatch &&
			cp.Height > floor && h.holdsCheckpointLocked(&cp) {
			floor = cp.Height
		}
	}
	if floor < 0 {
		// Only a genesis-rooted store (StartHeight 0) can lack a held
		// checkpoint below the mismatch; an anchored store holds its
		// anchor. Nothing stored is authenticated, so all of it goes.
		log.Warnf("electrum: stored %s headers disagree with the checkpoint at height %d "+
			"and hold none below it; discarding %d headers and resyncing",
			h.cfg.Chain, mismatch, tip-h.cfg.StartHeight+1)
		if err := os.Remove(h.filePath); err != nil {
			return fmt.Errorf("electrum: remove header file: %w", err)
		}
		clear(h.byHeight)
		clear(h.byHash)
		h.tip.Store(h.cfg.StartHeight - 1)
		return nil
	}
	log.Warnf("electrum: stored %s headers disagree with the checkpoint at height %d; "+
		"discarding %d headers above the checkpoint at %d and resyncing",
		h.cfg.Chain, mismatch, tip-floor, floor)
	_, err := h.rewindLocked(tip - floor)
	return err
}

// workRange returns the cumulative proof-of-work of the stored headers over
// [from, to], for comparison against a competing chain's work.
// Missing headers contribute nothing (the range is clamped to what exists).
func (h *Headers) workRange(from, to int32) *big.Int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.workRangeLocked(from, to)
}

// workRangeLocked is workRange for a caller holding h.mu.
func (h *Headers) workRangeLocked(from, to int32) *big.Int {
	work := new(big.Int)
	for height := from; height <= to; height++ {
		if hdr := h.byHeight[height]; hdr != nil {
			work.Add(work, blockchain.CalcWork(hdr.Bits))
		}
	}
	return work
}
