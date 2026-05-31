// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/electrum"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// rescanProgressInterval is how often a RescanProgress notification is
// emitted during a Rescan walk.
const rescanProgressInterval = 10_000

// NotifyReceived subscribes to status changes for the given addresses. New
// activity on any subscribed address arrives as a RelevantTx notification on
// the channel returned by Notifications().
//
// Subscription is idempotent — re-subscribing an address that is already
// being watched is a no-op.
func (c *ElectrumClient) NotifyReceived(addrs []btcutil.Address) error {
	for _, addr := range addrs {
		if err := c.subscribeAddress(addr); err != nil {
			return err
		}
	}
	return nil
}

// subscribeAddress computes the scripthash for addr, subscribes if not
// already, and primes the cached history so future notifications can diff
// against it.
func (c *ElectrumClient) subscribeAddress(addr btcutil.Address) error {
	sh, err := addrScripthash(addr)
	if err != nil {
		return err
	}
	c.addrCacheMtx.Lock()
	_, exists := c.addrCache[sh]
	if !exists {
		c.addrCache[sh] = &scripthashEntry{addr: addr}
	}
	c.addrCacheMtx.Unlock()
	if exists {
		return nil
	}
	status, err := c.net.SubscribeScripthash(c.ctx, sh)
	if err != nil {
		// Roll back the cache entry so a retry can succeed.
		c.addrCacheMtx.Lock()
		delete(c.addrCache, sh)
		c.addrCacheMtx.Unlock()
		return fmt.Errorf("electrum: subscribe scripthash for %s: %w", addr, err)
	}
	c.addrCacheMtx.Lock()
	if entry, ok := c.addrCache[sh]; ok {
		entry.lastStatus = status.Status
	}
	c.addrCacheMtx.Unlock()
	return nil
}

// Rescan kicks off a chain rescan: walking from startHash to the current tip,
// fetching every transaction that involves any of the supplied addresses or
// outpoints and emitting the chain.Interface notifications a fresh wallet
// sync expects (BlockConnected for inert blocks, FilteredBlockConnected for
// blocks with matches, periodic RescanProgress, terminating RescanFinished).
//
// Rescan is asynchronous to match NeutrinoClient: it does the synchronous
// setup (scripthash subscriptions, history fetches), kicks off a walk
// goroutine, and returns nil immediately. A subsequent Rescan call cancels
// any in-flight walk before starting the new one.
func (c *ElectrumClient) Rescan(startHash *chainhash.Hash,
	addrs []btcutil.Address,
	outpoints map[wire.OutPoint]btcutil.Address) error {

	// Record outpoints for the duration of this rescan. They are used by
	// FilterBlocks to detect spends from watched outputs.
	c.outpointCacheMtx.Lock()
	maps.Copy(c.outpointCache, outpoints)
	c.outpointCacheMtx.Unlock()

	// Subscribe to every address we plan to scan so we keep receiving
	// updates after the rescan completes.
	scripthashes := make([]string, 0, len(addrs)+len(outpoints))
	for _, addr := range addrs {
		sh, err := addrScripthash(addr)
		if err != nil {
			return err
		}
		scripthashes = append(scripthashes, sh)
		if err := c.subscribeAddress(addr); err != nil {
			return err
		}
	}
	for _, addr := range outpoints {
		sh, err := addrScripthash(addr)
		if err != nil {
			return err
		}
		scripthashes = append(scripthashes, sh)
		if err := c.subscribeAddress(addr); err != nil {
			return err
		}
	}

	// Resolve startHash to a height. If it's the zero hash, start from
	// the very beginning (genesis).
	startHeight := int32(0)
	if startHash != nil && (*startHash != chainhash.Hash{}) {
		h, _, ok := c.net.HeaderByHash(startHash)
		if !ok {
			return fmt.Errorf("electrum: rescan start hash %s not in headers", startHash)
		}
		startHeight = h
	}

	// Build a map of {height: []history entries} aggregated across all
	// watched scripthashes. We do one GetHistory per scripthash up-front
	// and bucket the results so the walk that follows is purely local.
	matches, err := c.collectHistories(scripthashes, startHeight)
	if err != nil {
		return err
	}

	// Cancel any in-flight rescan and wait for its goroutine to exit
	// before starting ours. This keeps the notification stream coherent.
	c.rescanMtx.Lock()
	if c.rescanCancel != nil {
		c.rescanCancel()
		prev := c.rescanDone
		c.rescanMtx.Unlock()
		<-prev
		c.rescanMtx.Lock()
	}
	ctx, cancel := context.WithCancel(c.ctx)
	done := make(chan struct{})
	c.rescanCancel = cancel
	c.rescanDone = done
	c.rescanMtx.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(done)
		c.runRescan(ctx, startHeight, matches)
	}()

	return nil
}

// runRescan executes the rescan walk on the rescan goroutine. It returns
// silently on cancellation; on completion it enqueues RescanFinished. Errors
// during the walk are logged but not returned — the wallet observes progress
// through the notification stream, not via Rescan's call site.
func (c *ElectrumClient) runRescan(ctx context.Context, startHeight int32,
	matches map[int32][]electrum.HistoryEntry) {

	tip := c.net.Tip()
	if tip < startHeight {
		c.tryDispatchRescanFinished(tip)
		return
	}

	for height := startHeight; height <= tip; height++ {
		if ctx.Err() != nil {
			return
		}
		hdr := c.net.HeaderByHeight(height)
		if hdr == nil {
			log.Errorf("electrum: rescan missing header at %d", height)
			return
		}
		block := wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: hdr.BlockHash(), Height: height},
			Time:  hdr.Timestamp,
		}
		entries := matches[height]
		var err error
		if len(entries) == 0 {
			err = c.enqueueCtx(ctx, BlockConnected(block))
		} else {
			err = c.emitFilteredBlock(ctx, block, entries)
		}
		if err != nil {
			return // shutdown or rescan cancelled
		}
		if height%rescanProgressInterval == 0 && height != startHeight {
			if err := c.enqueueCtx(ctx, &RescanProgress{
				Hash:   hdr.BlockHash(),
				Height: height,
				Time:   hdr.Timestamp,
			}); err != nil {
				return
			}
		}
	}
	c.tryDispatchRescanFinished(tip)
}

// tryDispatchRescanFinished enqueues a RescanFinished notification, logging
// (but not propagating) any error so the caller doesn't have to.
func (c *ElectrumClient) tryDispatchRescanFinished(tip int32) {
	hdr := c.net.HeaderByHeight(tip)
	if hdr == nil {
		log.Warnf("electrum: rescan finished but no header at %d", tip)
		return
	}
	h := hdr.BlockHash()
	if err := c.enqueue(&RescanFinished{
		Hash:   &h,
		Height: tip,
		Time:   hdr.Timestamp,
	}); err != nil {
		log.Debugf("electrum: enqueue RescanFinished: %v", err)
	}
}

// collectHistories fetches the history for every scripthash and groups the
// entries by block height. Entries below startHeight are discarded.
func (c *ElectrumClient) collectHistories(scripthashes []string, startHeight int32) (map[int32][]electrum.HistoryEntry, error) {
	out := make(map[int32][]electrum.HistoryEntry)
	for _, sh := range scripthashes {
		hist, err := c.net.GetHistory(c.ctx, sh)
		if err != nil {
			return nil, fmt.Errorf("electrum: get history for %s: %w", sh, err)
		}
		// Cache the history so the live notification path can diff
		// against it.
		c.addrCacheMtx.Lock()
		if entry, ok := c.addrCache[sh]; ok {
			entry.lastHistory = hist
		}
		c.addrCacheMtx.Unlock()
		for _, h := range hist {
			if h.Height < startHeight {
				continue
			}
			// Mempool entries (height == 0) are not part of the
			// rescan walk; they get delivered separately via the
			// scripthash notification loop.
			if h.Height <= 0 {
				continue
			}
			out[h.Height] = append(out[h.Height], h)
		}
	}
	return out, nil
}

// emitFilteredBlock fetches each matching tx, decodes it, and emits a single
// FilteredBlockConnected notification carrying the block plus relevant
// records. ctx allows the rescan to be cancelled mid-block.
func (c *ElectrumClient) emitFilteredBlock(ctx context.Context, block wtxmgr.BlockMeta, entries []electrum.HistoryEntry) error {
	// Deduplicate txids — two scripthashes can match the same tx.
	seen := make(map[string]struct{}, len(entries))
	uniq := make([]electrum.HistoryEntry, 0, len(entries))
	for _, e := range entries {
		if _, dup := seen[e.TxHash]; dup {
			continue
		}
		seen[e.TxHash] = struct{}{}
		uniq = append(uniq, e)
	}
	// Sort by txid for determinism.
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].TxHash < uniq[j].TxHash })

	ntfn := FilteredBlockConnected{Block: &block}
	for _, e := range uniq {
		rawHex, err := c.net.GetRawTransaction(ctx, e.TxHash)
		if err != nil {
			log.Warnf("electrum: rescan fetch %s: %v", e.TxHash, err)
			continue
		}
		raw, err := hex.DecodeString(rawHex)
		if err != nil {
			log.Warnf("electrum: decode raw %s: %v", e.TxHash, err)
			continue
		}
		rec, err := wtxmgr.NewTxRecord(string(c.chain), raw, block.Time)
		if err != nil {
			log.Warnf("electrum: tx record %s: %v", e.TxHash, err)
			continue
		}
		ntfn.RelevantTxs = append(ntfn.RelevantTxs, rec)
	}
	return c.enqueueCtx(ctx, ntfn)
}

// enqueue pushes a notification onto the queue, blocking only on shutdown.
func (c *ElectrumClient) enqueue(n any) error {
	select {
	case c.enqueueNotification <- n:
		return nil
	case <-c.quit:
		return errClientShutDown
	}
}

// enqueueCtx is like enqueue but also unblocks when ctx is cancelled — used
// by the rescan walk so a superseded rescan exits promptly.
func (c *ElectrumClient) enqueueCtx(ctx context.Context, n any) error {
	select {
	case c.enqueueNotification <- n:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.quit:
		return errClientShutDown
	}
}

// FilterBlocks scans the requested blocks for activity on the requested
// addresses, returning a response for the first block that matched (or nil
// if none did).
//
// Implementation strategy: we batch-fetch GetHistory for each requested
// address's scripthash (cached when possible), bucket the results by block
// height, then for each requested block test for hits. When a block has
// hits, we fetch the matching transactions and run them through the standard
// BlockFilterer to populate the response in the shape callers expect.
func (c *ElectrumClient) FilterBlocks(req *FilterBlocksRequest) (*FilterBlocksResponse, error) {
	if len(req.Blocks) == 0 {
		return nil, nil
	}

	// Combine external + internal addresses for scripthash lookup.
	type addrRef struct {
		addr btcutil.Address
		sh   string
	}
	refs := make([]addrRef, 0, len(req.ExternalAddrs)+len(req.InternalAddrs))
	for _, a := range req.ExternalAddrs {
		sh, err := addrScripthash(a)
		if err != nil {
			return nil, err
		}
		refs = append(refs, addrRef{addr: a, sh: sh})
	}
	for _, a := range req.InternalAddrs {
		sh, err := addrScripthash(a)
		if err != nil {
			return nil, err
		}
		refs = append(refs, addrRef{addr: a, sh: sh})
	}

	// Build a set of heights of interest so we can short-circuit blocks
	// with no matching scripthashes.
	heightSet := make(map[int32]bool, len(req.Blocks))
	for _, blk := range req.Blocks {
		heightSet[blk.Height] = true
	}

	// Per-height bucket of matching txids. We fetch each tx at most once
	// even if multiple watched scripthashes touch the same tx.
	hitsByHeight := make(map[int32][]string)
	for _, r := range refs {
		hist, err := c.fetchHistory(r.sh)
		if err != nil {
			return nil, err
		}
		for _, h := range hist {
			if h.Height <= 0 || !heightSet[h.Height] {
				continue
			}
			hitsByHeight[h.Height] = append(hitsByHeight[h.Height], h.TxHash)
		}
	}

	if len(hitsByHeight) == 0 {
		// No address activity in any requested block.
		return nil, nil
	}

	for i, blk := range req.Blocks {
		hits := hitsByHeight[blk.Height]
		if len(hits) == 0 {
			continue
		}
		// Dedup txids — multiple scripthashes can land on the same tx.
		seen := make(map[string]struct{}, len(hits))
		uniqTxs := make([]string, 0, len(hits))
		for _, t := range hits {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			uniqTxs = append(uniqTxs, t)
		}

		// Fetch the txs and decode them as bisonwire.Tx so the
		// existing BlockFilterer machinery can inspect them.
		bw := make([]*bisonwire.Tx, 0, len(uniqTxs))
		for _, t := range uniqTxs {
			rawHex, err := c.net.GetRawTransaction(c.ctx, t)
			if err != nil {
				return nil, fmt.Errorf("electrum: fetch %s: %w", t, err)
			}
			raw, err := hex.DecodeString(rawHex)
			if err != nil {
				return nil, fmt.Errorf("electrum: decode %s: %w", t, err)
			}
			tx := &bisonwire.Tx{Chain: string(c.chain)}
			if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
				return nil, fmt.Errorf("electrum: deserialize %s: %w", t, err)
			}
			bw = append(bw, tx)
		}

		bf := NewBlockFilterer(c.btcParams, req)
		// Run each fetched tx through the filterer to populate the
		// scope/index maps and outpoint set the response wants.
		var hasRelevant bool
		for _, tx := range bw {
			if bf.FilterTx(tx) {
				bf.RelevantTxns = append(bf.RelevantTxns, tx)
				hasRelevant = true
			}
		}
		if !hasRelevant {
			continue
		}
		return &FilterBlocksResponse{
			BatchIndex:         uint32(i),
			BlockMeta:          blk,
			FoundExternalAddrs: bf.FoundExternal,
			FoundInternalAddrs: bf.FoundInternal,
			FoundOutPoints:     bf.FoundOutPoints,
			RelevantTxns:       bf.RelevantTxns,
		}, nil
	}
	return nil, nil
}

// fetchHistory returns a scripthash's history, using the per-address cache
// when available to avoid redundant network round-trips.
func (c *ElectrumClient) fetchHistory(scripthash string) ([]electrum.HistoryEntry, error) {
	c.addrCacheMtx.Lock()
	if entry, ok := c.addrCache[scripthash]; ok && entry.lastHistory != nil {
		hist := entry.lastHistory
		c.addrCacheMtx.Unlock()
		return hist, nil
	}
	c.addrCacheMtx.Unlock()

	hist, err := c.net.GetHistory(c.ctx, scripthash)
	if err != nil {
		return nil, err
	}
	c.addrCacheMtx.Lock()
	if entry, ok := c.addrCache[scripthash]; ok {
		entry.lastHistory = hist
	}
	c.addrCacheMtx.Unlock()
	return hist, nil
}

// errClientShutDown is returned by rescan-related calls when the client is
// being torn down. Wrap with %w to preserve identity for callers that want
// to errors.Is it.
var errClientShutDown = fmt.Errorf("electrum: client shut down during rescan")
