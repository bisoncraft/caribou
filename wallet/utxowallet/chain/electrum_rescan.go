// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
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
// already, and primes the cached entry so future notifications can diff
// against it.
//
// Concurrent calls for the same address are serialized via a per-scripthash
// inflight wait: the first caller does the subscribe RPC while later callers
// block on its completion. This prevents the previous race where a second
// caller saw the cache entry before the first caller's subscription had
// reached the server.
//
// If the in-flight subscribe fails, waiters discover the cache entry was
// rolled back and retry themselves rather than returning nil — otherwise the
// wallet layer would believe the address was subscribed when the server never
// actually heard about it. Retries are bounded so a persistently failing
// server eventually surfaces an error to the caller.
func (c *ElectrumClient) subscribeAddress(addr btcutil.Address) error {
	const maxAttempts = 3
	for range maxAttempts {
		retry, err := c.trySubscribeAddress(addr)
		if err != nil {
			return err
		}
		if !retry {
			return nil
		}
	}
	return fmt.Errorf("electrum: subscribe scripthash for %s: in-flight subscribe failed", addr)
}

// trySubscribeAddress performs a single subscribe attempt. It returns
// (retry=true, nil) when the caller observed an in-flight subscribe that
// failed and bailed; the outer loop should re-enter.
func (c *ElectrumClient) trySubscribeAddress(addr btcutil.Address) (retry bool, err error) {
	sh, err := addrScripthash(addr)
	if err != nil {
		return false, err
	}
	c.addrCacheMtx.Lock()
	if entry, ok := c.addrCache[sh]; ok {
		ready := entry.ready
		c.addrCacheMtx.Unlock()
		if ready == nil {
			// Already subscribed.
			return false, nil
		}
		<-ready
		// First caller finished. If it succeeded the entry is still
		// present and we're done; if it failed the entry was deleted
		// and we should retry rather than silently return success.
		c.addrCacheMtx.Lock()
		_, survived := c.addrCache[sh]
		c.addrCacheMtx.Unlock()
		if survived {
			return false, nil
		}
		return true, nil
	}
	ready := make(chan struct{})
	c.addrCache[sh] = &scripthashEntry{addr: addr, ready: ready}
	c.addrCacheMtx.Unlock()

	status, err := c.net.SubscribeScripthash(c.ctx, sh)
	if err != nil {
		// Roll back so a retry can succeed, then release any blocked
		// waiters. They observe the missing entry and retry rather
		// than returning nil.
		c.addrCacheMtx.Lock()
		delete(c.addrCache, sh)
		c.addrCacheMtx.Unlock()
		close(ready)
		return false, fmt.Errorf("electrum: subscribe scripthash for %s: %w", addr, err)
	}
	// Release waiters first so they see a stable cache entry, then
	// run the initial-history emission below. lastStatus stays empty so
	// the synthesized status change below is processed as a real
	// transition rather than short-circuited.
	c.addrCacheMtx.Lock()
	if entry, ok := c.addrCache[sh]; ok {
		entry.ready = nil
	}
	c.addrCacheMtx.Unlock()
	close(ready)

	// Review2 #5: SubscribeScripthash returns the current status
	// synchronously but never enqueues an initial notification. Without
	// this, transactions already pending (or confirmed) at subscribe time
	// would only surface after the server's next status change — pending
	// mempool txs in particular would stay invisible until they confirm.
	// Synthesize an initial status notification so the existing
	// diff-and-emit path runs once. If the server reported no activity
	// (empty status) there's nothing to do.
	if status.Status != "" {
		if err := c.handleScripthashStatus(status); err != nil {
			log.Debugf("electrum: prime scripthash %s: %v", sh, err)
		}
	}
	return false, nil
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

	// M2: surface an error rather than silently no-op if the client has
	// already been stopped.
	if err := c.ctx.Err(); err != nil {
		return fmt.Errorf("electrum: rescan on stopped client: %w", err)
	}

	// Subscribe to every address we plan to scan so we keep receiving
	// updates after the rescan completes. Watched outpoints arrive on
	// every FilterBlocksRequest, so we don't cache them here — the
	// scripthash subscriptions are the durable state.
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
//
// Notification shape (review2 #11): only emit FilteredBlockConnected for
// blocks containing watched-address activity, plus periodic RescanProgress
// notifications to advance the wallet's syncedTo, plus a terminating
// RescanFinished. Emitting a BlockConnected per empty block during a deep
// rescan would force a per-block walletdb.Update — fine for shallow
// birthdays but unnecessarily expensive for imported / deep-birthday wallets.
// catchUpHashes in chainntfns.go walks syncedTo forward on every
// RescanProgress, so the wallet's progress is still recorded.
func (c *ElectrumClient) runRescan(ctx context.Context, startHeight int32,
	matches map[int32][]electrum.HistoryEntry) {

	tip := c.net.Tip()
	if tip < startHeight {
		if ctx.Err() == nil {
			c.tryDispatchRescanFinished(tip)
		}
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
		if entries := matches[height]; len(entries) > 0 {
			block := wtxmgr.BlockMeta{
				Block: wtxmgr.Block{Hash: hdr.BlockHash(), Height: height},
				Time:  hdr.Timestamp,
			}
			if err := c.emitFilteredBlock(ctx, block, entries); err != nil {
				return // shutdown or rescan cancelled
			}
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
	// M4: don't emit RescanFinished if a newer Rescan cancelled this one
	// between the last iteration and here. That newer Rescan will dispatch
	// its own RescanFinished when its walk completes.
	if ctx.Err() != nil {
		return
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
		// fetchAndCacheHistory caches the history (under the entry's proc
		// lock) so the live notification path can diff against it without a
		// lost-update race.
		hist, err := c.fetchAndCacheHistory(sh)
		if err != nil {
			return nil, fmt.Errorf("electrum: get history for %s: %w", sh, err)
		}
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

// dedupStrings returns s with duplicate values removed, preserving first-seen
// order.
func dedupStrings(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// dedupHistoryByTxid returns the entries with duplicate txids removed (two
// scripthashes can match the same tx), preserving first-seen order.
func dedupHistoryByTxid(entries []electrum.HistoryEntry) []electrum.HistoryEntry {
	seen := make(map[string]struct{}, len(entries))
	uniq := make([]electrum.HistoryEntry, 0, len(entries))
	for _, e := range entries {
		if _, dup := seen[e.TxHash]; dup {
			continue
		}
		seen[e.TxHash] = struct{}{}
		uniq = append(uniq, e)
	}
	return uniq
}

// fetchRawTx fetches a transaction by txid and returns its decoded raw bytes.
// The server's hex is verified to hash to txid inside GetRawTransaction.
func (c *ElectrumClient) fetchRawTx(ctx context.Context, txid string) ([]byte, error) {
	rawHex, err := c.net.GetRawTransaction(ctx, txid)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil, fmt.Errorf("electrum: decode raw %s: %w", txid, err)
	}
	return raw, nil
}

// emitFilteredBlock fetches each matching tx, decodes it, and emits a single
// FilteredBlockConnected notification carrying the block plus relevant
// records. ctx allows the rescan to be cancelled mid-block.
func (c *ElectrumClient) emitFilteredBlock(ctx context.Context, block wtxmgr.BlockMeta, entries []electrum.HistoryEntry) error {
	uniq := dedupHistoryByTxid(entries)
	// Sort by txid for determinism.
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].TxHash < uniq[j].TxHash })

	ntfn := FilteredBlockConnected{Block: &block}
	for _, e := range uniq {
		// L1: fast-fail on cancellation rather than spend a round of
		// doomed RPC calls — each GetRawTransaction would just return
		// ctx.Err() anyway.
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := c.fetchRawTx(ctx, e.TxHash)
		if err != nil {
			log.Warnf("electrum: rescan fetch %s: %v", e.TxHash, err)
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

// orderTxsByDependency reorders a block's transactions so that any parent (a
// tx whose output is spent by another tx in the same set) precedes its
// children. It performs a depth-first post-order walk: before emitting a tx it
// recurses into the producers of its inputs, so producers are appended first.
//
// We deliberately do not reuse wtxmgr.DependencySort here: it returns
// []*wire.MsgTx (we need []*bisonwire.Tx) and, more importantly, it identifies
// nodes via wire.MsgTx.TxHash(), which is the wrong txid for LTC (bisonwire
// uses litcoinTxHash for MWEB-bearing txs). Keying on bisonwire.Tx.TxHash()
// here keeps the ordering correct across chains.
//
// A valid block can't contain a cycle (a tx can't spend a descendant), so the
// recursion always terminates. Output order is deterministic for a given input
// order. Txs whose inputs reference outpoints outside this set (the common
// case — parents in earlier blocks) simply have no in-set dependency and keep
// their relative position.
func orderTxsByDependency(txs []*bisonwire.Tx) []*bisonwire.Tx {
	idxByHash := make(map[chainhash.Hash]int, len(txs))
	for i, tx := range txs {
		idxByHash[tx.TxHash()] = i
	}
	visited := make([]bool, len(txs))
	order := make([]*bisonwire.Tx, 0, len(txs))
	var visit func(i int)
	visit = func(i int) {
		if visited[i] {
			return
		}
		visited[i] = true
		for _, in := range txs[i].TxIn {
			if p, ok := idxByHash[in.PreviousOutPoint.Hash]; ok {
				visit(p)
			}
		}
		order = append(order, txs[i])
	}
	for i := range txs {
		visit(i)
	}
	return order
}

// FilterBlocks scans the requested blocks for activity on the requested
// addresses and watched outpoints, returning a response for the first block
// that matched (or nil if none did).
//
// Implementation strategy: we batch-fetch GetHistory for each requested
// address's scripthash (cached when possible), bucket the results by block
// height, then for each requested block test for hits. When a block has
// hits, we fetch the matching transactions and run them through the standard
// BlockFilterer to populate the response in the shape callers expect.
//
// req.WatchedOutPoints values are the addresses owning those outpoints. We
// include their scripthashes in the lookup set because an electrumx
// scripthash history covers both directions of an address: receives AND
// spends. Without this, a tx that spends a wallet-owned outpoint but does
// not also pay one of the lookahead addresses would never be fetched, and
// BlockFilterer.FilterTx would never see it — leaving the spend
// undiscovered and the wallet balance stale. The neutrino backend includes
// the same set in its cfilter watchlist.
func (c *ElectrumClient) FilterBlocks(req *FilterBlocksRequest) (*FilterBlocksResponse, error) {
	if len(req.Blocks) == 0 {
		return nil, nil
	}

	// Combine external + internal addresses + watched-outpoint addresses
	// for scripthash lookup. Dedup by scripthash to avoid redundant
	// GetHistory round-trips when the same address appears in multiple
	// sets (e.g. a watched outpoint paid to a current lookahead address).
	type addrRef struct {
		addr btcutil.Address
		sh   string
	}
	refs := make([]addrRef, 0,
		len(req.ExternalAddrs)+len(req.InternalAddrs)+len(req.WatchedOutPoints))
	seenSH := make(map[string]struct{},
		len(req.ExternalAddrs)+len(req.InternalAddrs)+len(req.WatchedOutPoints))
	addRef := func(a btcutil.Address) error {
		sh, err := addrScripthash(a)
		if err != nil {
			return err
		}
		if _, dup := seenSH[sh]; dup {
			return nil
		}
		seenSH[sh] = struct{}{}
		refs = append(refs, addrRef{addr: a, sh: sh})
		return nil
	}
	for _, a := range req.ExternalAddrs {
		if err := addRef(a); err != nil {
			return nil, err
		}
	}
	for _, a := range req.InternalAddrs {
		if err := addRef(a); err != nil {
			return nil, err
		}
	}
	for _, a := range req.WatchedOutPoints {
		if err := addRef(a); err != nil {
			return nil, err
		}
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
		uniqTxs := dedupStrings(hits)

		// Fetch the txs and decode them as bisonwire.Tx so the
		// existing BlockFilterer machinery can inspect them.
		bw := make([]*bisonwire.Tx, 0, len(uniqTxs))
		for _, t := range uniqTxs {
			raw, err := c.fetchRawTx(c.ctx, t)
			if err != nil {
				return nil, fmt.Errorf("electrum: fetch %s: %w", t, err)
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
		//
		// Order matters: BlockFilterer.FilterTx only recognizes a spend of
		// an output created in the same block if the creating (parent) tx
		// was filtered first — that's when its outpoint lands in
		// FoundOutPoints. electrumx history is bucketed by height but is not
		// guaranteed to be in block/topological order, so we restore the
		// parent-before-child invariant explicitly. Without this, a wallet
		// that receives then re-spends within one block could leave the
		// outpoint unmarked. Neutrino gets this for free by iterating
		// block.Transactions in order.
		var hasRelevant bool
		for _, tx := range orderTxsByDependency(bw) {
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
	return c.fetchAndCacheHistory(scripthash)
}

// fetchAndCacheHistory fetches a scripthash's history and stores it as the
// cached lastHistory baseline. The cache write is serialized with the live
// notification path's read-modify-write through the entry's proc lock (held
// across the GetHistory round trip, proc→addrCacheMtx order), so a
// rescan/FilterBlocks fetch can't clobber a concurrent handleScripthashStatus
// result and cause a later duplicate emission. If the scripthash isn't cached,
// it just fetches.
func (c *ElectrumClient) fetchAndCacheHistory(scripthash string) ([]electrum.HistoryEntry, error) {
	c.addrCacheMtx.Lock()
	entry := c.addrCache[scripthash]
	c.addrCacheMtx.Unlock()

	if entry != nil {
		entry.proc.Lock()
		defer entry.proc.Unlock()
	}
	hist, err := c.net.GetHistory(c.ctx, scripthash)
	if err != nil {
		return nil, err
	}
	if entry != nil {
		c.addrCacheMtx.Lock()
		entry.lastHistory = hist
		c.addrCacheMtx.Unlock()
	}
	return hist, nil
}

// errClientShutDown is returned by rescan-related calls when the client is
// being torn down. Wrap with %w to preserve identity for callers that want
// to errors.Is it.
var errClientShutDown = fmt.Errorf("electrum: client shut down during rescan")
