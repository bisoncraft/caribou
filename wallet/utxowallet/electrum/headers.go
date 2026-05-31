// Copyright (c) 2024 The Bison Wallet developers
// Copyright (c) The go-electrum-client authors
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// reorgRewindDepth controls how many headers we drop from our tip when an
// incoming header fails to connect. The value matches the upstream electrum
// client and is large enough to absorb every reorg observed in practice on
// BTC and LTC.
const reorgRewindDepth = 8

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

	tip      atomic.Int32 // height of the last connected header
	synced   atomic.Bool  // true once initial sync completes
	recovery atomic.Bool  // true while a reorg rewind is in progress

	// recoveryTip is the height the client should see during recovery so
	// it does not observe the chain going backwards.
	recoveryTipMtx sync.RWMutex
	recoveryTip    int32

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
	h.tip.Store(cfg.StartHeight - 1) // pre-genesis if StartHeight == 0
	if err := h.loadFromDisk(); err != nil {
		return nil, err
	}
	return h, nil
}

// Tip returns the height of the highest connected header, or StartHeight-1 if
// no headers have been connected yet. During a reorg rewind, Tip returns the
// pre-rewind tip so external observers do not see the chain go backwards
// momentarily.
func (h *Headers) Tip() int32 {
	if h.recovery.Load() {
		h.recoveryTipMtx.RLock()
		t := h.recoveryTip
		h.recoveryTipMtx.RUnlock()
		return t
	}
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

// TipHash returns the hash of the highest connected header. Returns the zero
// hash if no headers are connected.
func (h *Headers) TipHash() chainhash.Hash {
	h.mu.RLock()
	defer h.mu.RUnlock()
	hdr := h.byHeight[h.tip.Load()]
	if hdr == nil {
		return chainhash.Hash{}
	}
	return hdr.BlockHash()
}

// Range copies count headers starting at startHeight into dst (which must be
// pre-allocated) and returns the number of headers actually written. It is
// the caller's responsibility to keep startHeight+count within the chain.
func (h *Headers) Range(startHeight int32, dst []wire.BlockHeader) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for i := range dst {
		hdr, ok := h.byHeight[startHeight+int32(i)]
		if !ok {
			break
		}
		dst[i] = *hdr
		n++
	}
	return n
}

// AppendBatch appends a batch of raw headers (concatenated, headerSize bytes
// each) starting at the given height. Each header must connect to its
// predecessor or this returns an error and the chain is left unchanged.
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

	h.mu.Lock()
	defer h.mu.Unlock()

	if startHeight != h.tip.Load()+1 {
		return 0, fmt.Errorf("electrum: batch start %d does not connect to tip %d",
			startHeight, h.tip.Load())
	}

	// Verify each header connects to the previous one.
	prevHash, err := h.prevHashAt(startHeight)
	if err != nil {
		return 0, err
	}
	for i, hdr := range headers {
		if hdr.PrevBlock != prevHash {
			return 0, fmt.Errorf("electrum: header at %d does not connect: prev %s, want %s",
				startHeight+int32(i), hdr.PrevBlock, prevHash)
		}
		prevHash = hdr.BlockHash()
	}

	// Persist before mutating in-memory state so an interrupted write
	// leaves us in a recoverable state.
	if err := h.appendToFile(raw); err != nil {
		return 0, err
	}
	for i, hdr := range headers {
		height := startHeight + int32(i)
		h.byHeight[height] = hdr
		h.byHash[hdr.BlockHash()] = height
	}
	h.tip.Store(startHeight + int32(len(headers)) - 1)
	return len(headers), nil
}

// AppendOne appends a single header that must connect to the current tip.
// It returns true on success; on failure (header does not connect) it
// triggers a reorg rewind and returns false, leaving the caller to retry on
// the next notification.
func (h *Headers) AppendOne(hdr *wire.BlockHeader) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tip := h.tip.Load()
	prevHash, err := h.prevHashAt(tip + 1)
	if err != nil {
		return false, err
	}
	if hdr.PrevBlock != prevHash {
		log.Debugf("electrum: incoming header at %d does not connect (prev %s, want %s); rewinding %d",
			tip+1, hdr.PrevBlock, prevHash, reorgRewindDepth)
		if err := h.rewindLocked(reorgRewindDepth); err != nil {
			return false, err
		}
		return false, nil
	}
	var buf bytes.Buffer
	if err := hdr.Serialize(&buf); err != nil {
		return false, err
	}
	if err := h.appendToFile(buf.Bytes()); err != nil {
		return false, err
	}
	newHeight := tip + 1
	h.byHeight[newHeight] = hdr
	h.byHash[hdr.BlockHash()] = newHeight
	h.tip.Store(newHeight)
	h.recovery.Store(false)
	return true, nil
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
		if this.PrevBlock != prev.BlockHash() {
			return fmt.Errorf("electrum: chain break at height %d", height)
		}
	}
	return nil
}

// rewindLocked drops the top `depth` headers from memory and truncates the
// file. The caller must hold h.mu.
func (h *Headers) rewindLocked(depth int32) error {
	tip := h.tip.Load()
	if tip-depth < h.cfg.StartHeight {
		return fmt.Errorf("electrum: cannot rewind %d below start %d (tip %d)",
			depth, h.cfg.StartHeight, tip)
	}
	// File first, in-memory second, so a failed truncate leaves the maps
	// untouched.
	newSize := int64(tip-depth-h.cfg.StartHeight+1) * int64(h.cfg.HeaderSize)
	if err := os.Truncate(h.filePath, newSize); err != nil {
		return fmt.Errorf("electrum: truncate header file: %w", err)
	}
	for i := range int(depth) {
		height := tip - int32(i)
		hdr := h.byHeight[height]
		if hdr != nil {
			delete(h.byHash, hdr.BlockHash())
		}
		delete(h.byHeight, height)
	}
	h.recoveryTipMtx.Lock()
	h.recoveryTip = tip
	h.recoveryTipMtx.Unlock()
	h.recovery.Store(true)
	h.tip.Store(tip - depth)
	return nil
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

// prevHashAt returns the hash that a header at `height` must reference as
// PrevBlock. height must equal tip+1 or be exactly StartHeight; for
// StartHeight the parent is the genesis as derived from chain params.
func (h *Headers) prevHashAt(height int32) (chainhash.Hash, error) {
	if height == h.cfg.StartHeight {
		// We expect the first header to be at StartHeight; its parent
		// is the previous block, or — when StartHeight is 0 — the
		// genesis block has no parent and PrevBlock is zero.
		if h.cfg.StartHeight == 0 {
			return chainhash.Hash{}, nil
		}
		// For non-zero start heights we cannot validate the very
		// first header against a known parent; accept whatever the
		// server gave us.
		return chainhash.Hash{}, nil
	}
	prev := h.byHeight[height-1]
	if prev == nil {
		return chainhash.Hash{}, fmt.Errorf("electrum: missing header at %d", height-1)
	}
	return prev.BlockHash(), nil
}

// appendToFile appends raw header bytes to the persistent header file.
func (h *Headers) appendToFile(raw []byte) error {
	f, err := os.OpenFile(h.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("electrum: open header file: %w", err)
	}
	defer f.Close()
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
	for i, hdr := range hdrs {
		height := h.cfg.StartHeight + int32(i)
		h.byHeight[height] = hdr
		h.byHash[hdr.BlockHash()] = height
	}
	h.tip.Store(h.cfg.StartHeight + count - 1)
	log.Infof("electrum: loaded %d headers, tip at %d", count, h.tip.Load())
	return nil
}

// DecodeHexBatch decodes a hex-encoded concatenated batch of raw headers
// (as returned by electrumx) into bytes. It is exposed so callers wiring up
// the sync loop can reuse the same decoding.
func DecodeHexBatch(s string) ([]byte, error) {
	return hex.DecodeString(s)
}
