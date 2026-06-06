// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/electrum"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// ElectrumBackEnd is the BackEnd() string returned by ElectrumClient.
const ElectrumBackEnd = "electrum"

// ElectrumBackend is the optional capability surface for chain.Interface
// implementations backed by an electrumx server. It is currently a marker
// interface — callers that need electrum-specific extensions can grow it.
type ElectrumBackend interface {
	Interface
	// Network returns the underlying electrum network for diagnostics
	// and electrum-specific operations such as peer management.
	Network() ElectrumNetwork
}

// ElectrumClient is a chain.Interface implementation backed by an electrumx
// server. The shape mirrors NeutrinoClient: a notification queue goroutine
// translates events from the underlying network (tip changes, scripthash
// status changes, completed rescan steps) into chain.Interface notifications
// drained via Notifications().
//
// ElectrumClient does NOT take a parent context on Start; like NeutrinoClient,
// it owns an internal context that is cancelled on Stop. This keeps the
// surface drop-in compatible with the existing wallet wiring.
type ElectrumClient struct {
	net         ElectrumNetwork
	chainParams *netparams.ChainParams
	btcParams   *chaincfg.Params
	chain       bisonwire.Chain

	// addrCache maps a scripthash hex string to the address it was
	// registered for, plus the last status we processed so we can detect
	// changes via the subscription notifications. Mutex-guarded.
	addrCacheMtx sync.Mutex
	addrCache    map[string]*scripthashEntry

	enqueueNotification chan any
	dequeueNotification chan any
	currentBlock        chan *waddrmgr.BlockStamp

	ctx    context.Context
	cancel context.CancelFunc
	quit   chan struct{}

	startMtx sync.Mutex
	started  bool
	wg       sync.WaitGroup

	// rescanMtx serializes Rescan calls: a new Rescan cancels and waits
	// for any in-flight walk before starting its own. The cancel func is
	// nil when no walk is running.
	rescanMtx    sync.Mutex
	rescanCancel context.CancelFunc
	rescanDone   chan struct{}
}

// scripthashEntry captures per-scripthash state.
//
// ready is non-nil only while the first SubscribeScripthash call for this
// scripthash is still in flight. Subsequent subscribeAddress callers for the
// same scripthash wait on it; the field is set back to nil and closed once
// the RPC completes (success or failure).
//
// pending holds history entries whose confirmed height has not yet been
// connected locally. handleScripthashStatus defers them rather than
// emitting a RelevantTx with blockMeta=nil (which would record a confirmed
// tx as unmined forever and never self-heal — review2 #6). The chain
// client's tip-change dispatch flushes pending entries once their headers
// arrive.
type scripthashEntry struct {
	addr        btcutil.Address
	lastStatus  string
	lastHistory []electrum.HistoryEntry
	pending     []electrum.HistoryEntry
	ready       chan struct{}
}

var _ Interface = (*ElectrumClient)(nil)
var _ ElectrumBackend = (*ElectrumClient)(nil)

// NewElectrumClient constructs an ElectrumClient backed by the given network.
// The network must not be started; ElectrumClient.Start will start it.
//
// chainParams.Chain must be a value recognised by bisonwire.ChainFromString
// (currently "btc" or "ltc"); otherwise an error is returned. We validate
// here rather than at every call site so the internal helpers can assume the
// chain string is good.
func NewElectrumClient(chainParams *netparams.ChainParams, network ElectrumNetwork) (*ElectrumClient, error) {
	if chainParams == nil {
		return nil, errors.New("electrum: nil chain params")
	}
	if network == nil {
		return nil, errors.New("electrum: nil network")
	}
	chain, err := bisonwire.ChainFromString(chainParams.Chain)
	if err != nil {
		return nil, fmt.Errorf("electrum: %w", err)
	}
	return &ElectrumClient{
		net:         network,
		chainParams: chainParams,
		btcParams:   chainParams.BTCDParams(),
		chain:       chain,
		addrCache:   make(map[string]*scripthashEntry),
	}, nil
}

// BackEnd returns the name of the backend.
func (c *ElectrumClient) BackEnd() string { return ElectrumBackEnd }

// Network returns the underlying ElectrumNetwork. Useful for asset-layer
// callers that need direct access to electrum-specific operations.
func (c *ElectrumClient) Network() ElectrumNetwork { return c.net }

// Start opens the network connection, syncs headers, and begins relaying
// notifications. It is safe to call multiple times; subsequent calls are
// no-ops while the client is running.
func (c *ElectrumClient) Start() error {
	c.startMtx.Lock()
	defer c.startMtx.Unlock()
	if c.started {
		return nil
	}

	c.ctx, c.cancel = context.WithCancel(context.Background())
	if err := c.net.Start(c.ctx); err != nil {
		c.cancel()
		return fmt.Errorf("electrum: network start: %w", err)
	}

	c.enqueueNotification = make(chan any)
	c.dequeueNotification = make(chan any)
	c.currentBlock = make(chan *waddrmgr.BlockStamp)
	c.quit = make(chan struct{})
	c.started = true

	// Initial ClientConnected notification.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		select {
		case c.enqueueNotification <- ClientConnected{}:
		case <-c.quit:
		}
	}()

	c.wg.Add(3)
	go c.notificationHandler()
	go c.tipChangeLoop()
	go c.scripthashLoop()

	return nil
}

// Stop initiates shutdown. WaitForShutdown blocks until all goroutines and
// the underlying network have stopped.
func (c *ElectrumClient) Stop() {
	c.startMtx.Lock()
	defer c.startMtx.Unlock()
	if !c.started {
		return
	}
	close(c.quit)
	c.cancel()
	c.net.Stop()
	c.started = false
}

// WaitForShutdown blocks until Stop has fully completed.
func (c *ElectrumClient) WaitForShutdown() {
	c.wg.Wait()
	c.net.WaitForShutdown()
}

// Notifications returns the channel from which chain.Interface notifications
// are dequeued.
func (c *ElectrumClient) Notifications() <-chan any {
	return c.dequeueNotification
}

// ----------------------------------------------------------------------------
// chain.Interface — direct-from-headers methods.
// ----------------------------------------------------------------------------

// GetBestBlock returns the hash and height of the chain tip as we know it.
func (c *ElectrumClient) GetBestBlock() (*chainhash.Hash, int32, error) {
	tip := c.net.Tip()
	if tip < 0 {
		return nil, 0, errors.New("electrum: no headers synced")
	}
	hdr := c.net.HeaderByHeight(tip)
	if hdr == nil {
		return nil, 0, fmt.Errorf("electrum: missing header at tip %d", tip)
	}
	h := hdr.BlockHash()
	return &h, tip, nil
}

// GetBlock returns ErrUnimplemented because electrumx does not serve full
// blocks. If a real consumer surfaces we can implement a slow fallback
// (id_from_pos + per-tx fetch), but `wallet/utxowallet/wallet/` does not
// require it.
func (c *ElectrumClient) GetBlock(*chainhash.Hash) (*bisonwire.BlockWithHeight, error) {
	return nil, ErrUnimplemented
}

// GetBlockHash returns the block hash at height.
func (c *ElectrumClient) GetBlockHash(height int64) (*chainhash.Hash, error) {
	if height < 0 {
		return nil, fmt.Errorf("electrum: negative height %d", height)
	}
	hdr := c.net.HeaderByHeight(int32(height))
	if hdr == nil {
		return nil, fmt.Errorf("electrum: no header at height %d", height)
	}
	h := hdr.BlockHash()
	return &h, nil
}

// GetBlockHeader returns the block header for the given hash.
func (c *ElectrumClient) GetBlockHeader(hash *chainhash.Hash) (*wire.BlockHeader, error) {
	_, hdr, ok := c.net.HeaderByHash(hash)
	if !ok {
		return nil, fmt.Errorf("electrum: unknown block hash %s", hash)
	}
	return hdr, nil
}

// IsCurrent reports whether our header chain has caught up to the server.
func (c *ElectrumClient) IsCurrent() bool { return c.net.Synced() }

// BlockStamp returns the latest block notified by the client, or an error
// if the client has been shut down.
func (c *ElectrumClient) BlockStamp() (*waddrmgr.BlockStamp, error) {
	select {
	case bs := <-c.currentBlock:
		return bs, nil
	case <-c.quit:
		return nil, errors.New("electrum: client shut down")
	}
}

// SendRawTransaction serializes and broadcasts a transaction. allowHighFees
// is ignored — electrumx servers don't accept a high-fee bypass flag.
func (c *ElectrumClient) SendRawTransaction(tx *wire.MsgTx, _ bool) (*chainhash.Hash, error) {
	hex, err := serializeMsgTxHex(tx)
	if err != nil {
		return nil, err
	}
	if _, err := c.net.Broadcast(c.ctx, hex); err != nil {
		return nil, c.MapRPCErr(err)
	}
	h := tx.TxHash()
	return &h, nil
}

// NotifyBlocks is a no-op: the electrum.Network always has an active
// blockchain.headers.subscribe on the leader connection.
func (c *ElectrumClient) NotifyBlocks() error { return nil }

// MapRPCErr translates an electrumx server error string into one of the
// canonical RPCErr values used elsewhere in the wallet. Falls back to the
// shared BtcdErrMap.
func (c *ElectrumClient) MapRPCErr(rpcErr error) error {
	if rpcErr == nil {
		return nil
	}
	for pattern, mapped := range electrumErrMap {
		if matchErrStr(rpcErr, pattern) {
			return mapped
		}
	}
	for pattern, mapped := range BtcdErrMap {
		if matchErrStr(rpcErr, pattern) {
			return mapped
		}
	}
	return fmt.Errorf("%w: %v", ErrUndefined, rpcErr)
}

// electrumErrMap maps error-message substrings observed from electrumx
// servers to canonical RPCErr values. The list grows organically as we
// encounter new patterns in integration tests.
var electrumErrMap = map[string]error{
	"transaction already in block chain": ErrTxAlreadyConfirmed,
	"transaction already in mempool":     ErrTxAlreadyInMempool,
	"txn-already-known":                  ErrTxAlreadyKnown,
	"txn-mempool-conflict":               ErrMempoolConflict,
	"missing inputs":                     ErrMissingInputs,
	"insufficient priority":              ErrInsufficientFee,
	"min relay fee not met":              ErrMempoolMinFeeNotMet,
}

// ----------------------------------------------------------------------------
// Notification fan-out.
// ----------------------------------------------------------------------------

// tipChangeLoop translates electrum.Network TipChange events into the
// per-block chain.BlockDisconnected / chain.BlockConnected notifications
// the wallet's chainntfns loop expects.
//
// Review2 #1+#3: a single network event can describe both a reorg rewind
// and a multi-block catch-up. We dispatch disconnects first (so the wallet's
// disconnectBlock path rolls back to the common ancestor before we record
// new connects) and then connects per block (so waddrmgr.PutSyncedTo can
// chain prev-hash → hash for every step, instead of being asked to jump N
// blocks forward and failing).
func (c *ElectrumClient) tipChangeLoop() {
	defer c.wg.Done()
	ch := c.net.TipChangeChan()
	for {
		select {
		case <-c.quit:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			c.dispatchTipChange(ev)
		}
	}
}

func (c *ElectrumClient) dispatchTipChange(ev *electrum.TipChange) {
	if ev == nil {
		return
	}
	// Disconnects first (review2 #1). The metadata is carried by the
	// event because the headers store has already removed these blocks
	// from its maps by the time we see them here.
	for _, d := range ev.Disconnected {
		ntfn := BlockDisconnected(wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: chainhash.Hash(d.Hash), Height: d.Height},
			Time:  time.Unix(d.Time, 0),
		})
		if err := c.enqueue(ntfn); err != nil {
			return
		}
	}
	// Then connects, one BlockConnected per height (review2 #3).
	for _, h := range ev.Connected {
		hdr := c.net.HeaderByHeight(h)
		if hdr == nil {
			log.Warnf("electrum: tip-change connect at %d but no local header", h)
			continue
		}
		ntfn := BlockConnected{
			Block: wtxmgr.Block{Hash: hdr.BlockHash(), Height: h},
			Time:  hdr.Timestamp,
		}
		if err := c.enqueue(ntfn); err != nil {
			return
		}
	}
	// Review2 #6: a scripthash status may have arrived while these
	// headers were still in flight. Now that the headers are local,
	// flush any deferred entries so confirmed txs aren't stuck unmined.
	if len(ev.Connected) > 0 {
		c.flushPending()
	}
}

// scripthashLoop translates scripthash-status notifications into RelevantTx
// notifications. It fetches the address's history on each change, diffs it
// against the previous history, and emits a RelevantTx for each new entry.
func (c *ElectrumClient) scripthashLoop() {
	defer c.wg.Done()
	ch := c.net.ScripthashChan()
	for {
		select {
		case <-c.quit:
			return
		case st, ok := <-ch:
			if !ok {
				return
			}
			if err := c.handleScripthashStatus(st); err != nil {
				log.Debugf("electrum: handle scripthash %s: %v", st.Scripthash, err)
			}
		}
	}
}

// handleScripthashStatus fetches the latest history for the scripthash,
// diffs against the cached history, and emits a RelevantTx notification for
// each newly-seen entry whose confirmed-block header is already local.
// Confirmed entries whose header hasn't arrived yet are deferred to
// entry.pending and flushed by the tip-change path once their header lands.
//
// Both reads of and writes to entry.lastStatus / entry.lastHistory /
// entry.pending happen under addrCacheMtx; the cache lock is released
// across the GetHistory RPC to avoid holding it during a network round-trip.
func (c *ElectrumClient) handleScripthashStatus(st *electrum.ScripthashStatus) error {
	c.addrCacheMtx.Lock()
	entry, ok := c.addrCache[st.Scripthash]
	if !ok {
		c.addrCacheMtx.Unlock()
		log.Debugf("electrum: status for unknown scripthash %s", st.Scripthash)
		return nil
	}
	if entry.lastStatus == st.Status {
		c.addrCacheMtx.Unlock()
		return nil
	}
	prev := append([]electrum.HistoryEntry(nil), entry.lastHistory...)
	c.addrCacheMtx.Unlock()

	hist, err := c.net.GetHistory(c.ctx, st.Scripthash)
	if err != nil {
		return fmt.Errorf("get history: %w", err)
	}
	// Partition the new entries into "ready to emit" and "deferred until
	// the header arrives". Already-seen entries are not re-emitted but
	// stay in the carry-forward list.
	emitted := make([]electrum.HistoryEntry, 0, len(hist))
	var deferred []electrum.HistoryEntry
	for _, h := range hist {
		if alreadySeen(prev, h.TxHash, h.Height) {
			emitted = append(emitted, h)
			continue
		}
		if h.Height > 0 && c.net.HeaderByHeight(h.Height) == nil {
			// Review2 #6: do not emit a RelevantTx with
			// blockMeta=nil for a confirmed tx. That would record
			// the tx as unmined and the next status-change diff
			// would treat it as already-seen, leaving it stuck
			// unmined forever. Park it and let the tip-change
			// dispatcher flush it once the header arrives.
			deferred = append(deferred, h)
			continue
		}
		if err := c.emitRelevantTx(h); err != nil {
			log.Debugf("electrum: emit %s: %v", h.TxHash, err)
			continue
		}
		emitted = append(emitted, h)
	}
	c.addrCacheMtx.Lock()
	entry.lastStatus = st.Status
	entry.lastHistory = emitted
	entry.pending = mergePendingEntries(entry.pending, deferred)
	c.addrCacheMtx.Unlock()
	return nil
}

// mergePendingEntries adds entries from src into dst, skipping any (txid,
// height) duplicates already present.
func mergePendingEntries(dst, src []electrum.HistoryEntry) []electrum.HistoryEntry {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]struct{}, len(dst))
	for _, e := range dst {
		seen[fmt.Sprintf("%s|%d", e.TxHash, e.Height)] = struct{}{}
	}
	for _, e := range src {
		key := fmt.Sprintf("%s|%d", e.TxHash, e.Height)
		if _, dup := seen[key]; dup {
			continue
		}
		dst = append(dst, e)
		seen[key] = struct{}{}
	}
	return dst
}

// flushPendingForHeights drains scripthash entries pending on heights now
// known to the local headers store. Called after every TipChange.Connected
// range so review2 #6's deferred emissions catch up automatically.
func (c *ElectrumClient) flushPending() {
	type readyBatch struct {
		sh    string
		ready []electrum.HistoryEntry
	}
	var batches []readyBatch

	c.addrCacheMtx.Lock()
	for sh, entry := range c.addrCache {
		if len(entry.pending) == 0 {
			continue
		}
		var ready, remain []electrum.HistoryEntry
		for _, e := range entry.pending {
			if e.Height > 0 && c.net.HeaderByHeight(e.Height) == nil {
				remain = append(remain, e)
				continue
			}
			ready = append(ready, e)
		}
		if len(ready) > 0 {
			entry.pending = remain
			batches = append(batches, readyBatch{sh: sh, ready: ready})
		}
	}
	c.addrCacheMtx.Unlock()

	for _, b := range batches {
		for _, e := range b.ready {
			if err := c.emitRelevantTx(e); err != nil {
				log.Debugf("electrum: flush emit %s: %v", e.TxHash, err)
			}
		}
		c.addrCacheMtx.Lock()
		if entry, ok := c.addrCache[b.sh]; ok {
			entry.lastHistory = append(entry.lastHistory, b.ready...)
		}
		c.addrCacheMtx.Unlock()
	}
}

// alreadySeen reports whether the (txid, height) was already in the prior
// history slice.
func alreadySeen(prev []electrum.HistoryEntry, txid string, height int32) bool {
	for _, p := range prev {
		if p.TxHash == txid && p.Height == height {
			return true
		}
	}
	return false
}

// emitRelevantTx fetches the raw transaction and enqueues a chain.RelevantTx
// notification for it.
func (c *ElectrumClient) emitRelevantTx(h electrum.HistoryEntry) error {
	rawHex, err := c.net.GetRawTransaction(c.ctx, h.TxHash)
	if err != nil {
		return err
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return fmt.Errorf("decode raw tx: %w", err)
	}
	rec, err := wtxmgr.NewTxRecord(string(c.chain), raw, time.Now())
	if err != nil {
		return fmt.Errorf("new tx record: %w", err)
	}
	var blockMeta *wtxmgr.BlockMeta
	if h.Height > 0 {
		hdr := c.net.HeaderByHeight(h.Height)
		if hdr != nil {
			blockMeta = &wtxmgr.BlockMeta{
				Block: wtxmgr.Block{Hash: hdr.BlockHash(), Height: h.Height},
				Time:  hdr.Timestamp,
			}
		}
	}
	select {
	case c.enqueueNotification <- RelevantTx{TxRecord: rec, Block: blockMeta}:
	case <-c.quit:
	}
	return nil
}

// ----------------------------------------------------------------------------
// notificationHandler — drains enqueueNotification into dequeueNotification.
// Mirrors NeutrinoClient.notificationHandler for compatibility.
// ----------------------------------------------------------------------------

func (c *ElectrumClient) notificationHandler() {
	defer c.wg.Done()

	// Do NOT prime bs by calling GetBestBlock here. That call would race
	// against the network's tip-subscription delivering the first
	// BlockConnected, and it has two failure modes — server returning an
	// empty first batch while Tip() is still -1, and the brief window
	// during a reorg rewind where Tip() points at a header we've already
	// deleted. The previous implementation tore the whole handler down on
	// either of those, which made Start() silently advertise success
	// while leaving the wallet unable to sync. Instead we let bs stay nil
	// until the first BlockConnected arrives; BlockStamp callers wait
	// (which is what NeutrinoClient does pre-first-block as well).
	var bs *waddrmgr.BlockStamp

	var notifications []any
	enqueue := c.enqueueNotification
	var dequeue chan any
	var next any

	for {
		// Only let BlockStamp callers proceed once we have a real bs.
		// `nil` send-case is never selectable, which is exactly what we
		// want.
		var blockStampCh chan *waddrmgr.BlockStamp
		if bs != nil {
			blockStampCh = c.currentBlock
		}

		select {
		case n, ok := <-enqueue:
			if !ok {
				if len(notifications) == 0 {
					close(c.dequeueNotification)
					return
				}
				enqueue = nil
				continue
			}
			// Update bs as soon as a BlockConnected is enqueued, not
			// at dequeue: BlockStamp() callers may be the consumer
			// blocking the dequeue, so deferring bs until consume
			// would deadlock. Semantically this means bs reflects
			// the latest block this client has heard about, even if
			// the wallet hasn't drained the notification yet.
			if bc, ok := n.(BlockConnected); ok {
				bs = &waddrmgr.BlockStamp{Hash: bc.Hash, Height: bc.Height}
			}
			if len(notifications) == 0 {
				next = n
				dequeue = c.dequeueNotification
			}
			notifications = append(notifications, n)

		case dequeue <- next:
			notifications[0] = nil
			notifications = notifications[1:]
			if len(notifications) != 0 {
				next = notifications[0]
			} else {
				if enqueue == nil {
					close(c.dequeueNotification)
					return
				}
				dequeue = nil
			}

		case blockStampCh <- bs:

		case <-c.quit:
			close(c.dequeueNotification)
			return
		}
	}
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

// addrScripthash computes the electrum scripthash for a wallet address. The
// scripthash is the SHA-256 of the address's scriptPubKey, reversed (little-
// endian to display order).
func addrScripthash(addr btcutil.Address) (string, error) {
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return "", fmt.Errorf("pay to addr script: %w", err)
	}
	sum := sha256.Sum256(script)
	slices.Reverse(sum[:])
	return hex.EncodeToString(sum[:]), nil
}

// serializeMsgTxHex returns the hex-encoded wire format of tx.
func serializeMsgTxHex(tx *wire.MsgTx) (string, error) {
	var buf bytes.Buffer
	buf.Grow(tx.SerializeSize())
	if err := tx.Serialize(&buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf.Bytes()), nil
}
