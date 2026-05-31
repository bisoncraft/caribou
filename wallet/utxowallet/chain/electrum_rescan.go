// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"bytes"
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

// Rescan walks the chain from startHash to tip, fetching all transactions
// that involve any of the supplied addresses or outpoints and emitting the
// chain.Interface notifications a fresh wallet sync expects.
//
// The semantics mirror what NeutrinoClient.Rescan delivers: a stream of
// BlockConnected notifications (for blocks with no relevant txs),
// FilteredBlockConnected notifications (for blocks that touched our addresses),
// periodic RescanProgress notifications, and a terminating RescanFinished.
//
// Unlike NeutrinoClient.Rescan, ElectrumClient.Rescan is synchronous — it
// returns only after RescanFinished has been delivered (or the client has
// shut down). This matches what the wallet's rescanRPCHandler expects:
// notifications drive the wallet state; the call's error is incidental.
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
	scripthashes := make([]string, 0, len(addrs))
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
	// And outpoint addresses too.
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

	tip := c.net.Tip()
	if tip < startHeight {
		// Nothing to walk; the chain is shorter than the start point.
		return c.dispatchRescanFinished(tip)
	}

	for height := startHeight; height <= tip; height++ {
		select {
		case <-c.quit:
			return errClientShutDown
		default:
		}
		hdr := c.net.HeaderByHeight(height)
		if hdr == nil {
			return fmt.Errorf("electrum: rescan missing header at %d", height)
		}
		block := wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: hdr.BlockHash(), Height: height},
			Time:  hdr.Timestamp,
		}
		entries := matches[height]
		if len(entries) == 0 {
			// Block had no activity; only emit BlockConnected.
			if err := c.enqueue(BlockConnected(block)); err != nil {
				return err
			}
		} else if err := c.emitFilteredBlock(block, entries); err != nil {
			return err
		}
		if height%rescanProgressInterval == 0 && height != startHeight {
			if err := c.enqueueRescanProgress(hdr, height); err != nil {
				return err
			}
		}
	}
	return c.dispatchRescanFinished(tip)
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
// records.
func (c *ElectrumClient) emitFilteredBlock(block wtxmgr.BlockMeta, entries []electrum.HistoryEntry) error {
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
	chain := chainFromParams(c.chainParams)
	for _, e := range uniq {
		rawHex, err := c.net.GetRawTransaction(c.ctx, e.TxHash)
		if err != nil {
			log.Warnf("electrum: rescan fetch %s: %v", e.TxHash, err)
			continue
		}
		raw, err := hex.DecodeString(rawHex)
		if err != nil {
			log.Warnf("electrum: decode raw %s: %v", e.TxHash, err)
			continue
		}
		rec, err := wtxmgr.NewTxRecord(string(chain), raw, block.Time)
		if err != nil {
			log.Warnf("electrum: tx record %s: %v", e.TxHash, err)
			continue
		}
		ntfn.RelevantTxs = append(ntfn.RelevantTxs, rec)
	}
	return c.enqueue(ntfn)
}

func (c *ElectrumClient) enqueueRescanProgress(hdr *wire.BlockHeader, height int32) error {
	return c.enqueue(&RescanProgress{
		Hash:   hdr.BlockHash(),
		Height: height,
		Time:   hdr.Timestamp,
	})
}

func (c *ElectrumClient) dispatchRescanFinished(tip int32) error {
	hdr := c.net.HeaderByHeight(tip)
	if hdr == nil {
		return fmt.Errorf("electrum: rescan finished but missing header at %d", tip)
	}
	h := hdr.BlockHash()
	return c.enqueue(&RescanFinished{
		Hash:   &h,
		Height: tip,
		Time:   hdr.Timestamp,
	})
}

// enqueue pushes a notification onto the queue; respects shutdown.
func (c *ElectrumClient) enqueue(n any) error {
	select {
	case c.enqueueNotification <- n:
		return nil
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

	// Per-height bucket of (txid, scripthash) so we can fetch each tx
	// once and know which address triggered it.
	type txid string
	type hit struct {
		tx  txid
		sh  string
		hdr electrum.HistoryEntry
	}
	hitsByHeight := make(map[int32][]hit)
	for _, r := range refs {
		hist, err := c.fetchHistory(r.sh)
		if err != nil {
			return nil, err
		}
		for _, h := range hist {
			if h.Height <= 0 {
				continue
			}
			if !heightSet[h.Height] {
				continue
			}
			hitsByHeight[h.Height] = append(hitsByHeight[h.Height], hit{
				tx: txid(h.TxHash), sh: r.sh, hdr: h,
			})
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
		seen := make(map[txid]struct{}, len(hits))
		uniqTxs := make([]txid, 0, len(hits))
		for _, h := range hits {
			if _, dup := seen[h.tx]; dup {
				continue
			}
			seen[h.tx] = struct{}{}
			uniqTxs = append(uniqTxs, h.tx)
		}

		// Fetch the txs and decode them as bisonwire.Tx so the
		// existing BlockFilterer machinery can inspect them.
		chain := chainFromParams(c.chainParams)
		bw := make([]*bisonwire.Tx, 0, len(uniqTxs))
		for _, t := range uniqTxs {
			rawHex, err := c.net.GetRawTransaction(c.ctx, string(t))
			if err != nil {
				return nil, fmt.Errorf("electrum: fetch %s: %w", t, err)
			}
			raw, err := hex.DecodeString(rawHex)
			if err != nil {
				return nil, fmt.Errorf("electrum: decode %s: %w", t, err)
			}
			tx := &bisonwire.Tx{Chain: string(chain)}
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
