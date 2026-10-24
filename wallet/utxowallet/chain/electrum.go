// Copyright (c) 2024 The Caribou Developers
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
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// ElectrumBackEnd is the BackEnd() string returned by ElectrumClient.
const ElectrumBackEnd = "electrum"

// ElectrumBackend is the optional capability surface for chain.Backend
// implementations backed by an electrumx server. It is currently a marker
// interface — callers that need electrum-specific extensions can grow it.
type ElectrumBackend interface {
	Backend
	// Network returns the underlying electrum network for diagnostics
	// and electrum-specific operations such as peer management.
	Network() ElectrumNetwork
}

// ElectrumClient is a chain.Backend implementation backed by an electrumx
// server. The shape mirrors NeutrinoClient: a notification queue goroutine
// translates events from the underlying network (tip changes, scripthash
// status changes, completed rescan steps) into chain.Backend notifications
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

	// probeCache parks get_history replies fetched by ProbeAddress (gap-limit
	// discovery) so the rescan that follows discovery doesn't refetch them,
	// keyed by scripthash. Each entry carries the electrum status derived from
	// its history; consumers holding a server-reported status may reuse the
	// parked history iff the statuses match, which makes staleness between
	// probe time and use time detectable exactly. Cleared once the rescan
	// setup has consumed it. Guarded by probeMtx (never held together with
	// addrCacheMtx or an entry's proc lock).
	probeMtx   sync.Mutex
	probeCache map[string]*probeHistory

	// rescanFloor is the confirmed-ingestion floor: the start height of the
	// most recent Rescan (the wallet's birthday block on initial sync).
	// History entries confirmed below it are contractually pre-birthday and
	// are skipped by the notification path (the rescan walk applies the same
	// bound via collectHistories). Zero until the first Rescan.
	rescanFloor atomic.Int32

	// dirtyMtx guards dirty/dirtyWake, the coalescing buffer between the
	// scripthash reader and processor goroutines. A scripthash status is
	// idempotent state (handleScripthashStatus re-fetches full history on
	// any change), so a burst of statuses for the same scripthash collapses
	// to its latest value here rather than queueing behind a slow
	// GetHistory round trip on a fixed-size channel and risking a drop of
	// the *latest* status — which would leave an incoming tx invisible
	// until unrelated future activity. The reader never blocks on the
	// network, so the upstream notify channels stay drained.
	dirtyMtx  sync.Mutex
	dirty     map[string]*electrum.ScripthashStatus
	dirtyWake chan struct{}

	// reorgMtx guards reorged — scripthashes whose history at or above a
	// rewound height was invalidated by a reorg and awaits a refresh
	// against the new chain (see invalidateReorged) — and reorgs, the
	// recent reorg log the status processor consults to detect a reorg
	// that landed during its history fetch (see reorgSince). reorgWake
	// asks the scripthash processor to run that refresh; the tip-change
	// loop never performs the fan-out itself.
	reorgMtx  sync.Mutex
	reorged   map[string]struct{}
	reorgs    []reorgEvent
	reorgGen  atomic.Uint64
	reorgWake chan struct{}

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

	// connMtx guards the live tip-delivery state below. It serializes the
	// rescan-finished handoff (goLive) against tipChangeLoop so live
	// BlockConnected notifications can't race the initial rescan.
	connMtx sync.Mutex
	// liveDelivery is false until the first rescan completes. While false,
	// tipChangeLoop suppresses BlockConnected — the rescan owns SyncedTo, and
	// a BlockConnected mined during the rescan would break PutSyncedTo's
	// prev-block chain (the wallet stores only recent hashes post-birthday).
	// goLive flips it true and delivers a contiguous catch-up.
	liveDelivery bool
	// lastConnected is the highest height delivered as BlockConnected. New
	// blocks are always delivered contiguously from lastConnected+1 to the
	// header-store tip, so a suppressed or skipped tip can't leave a gap.
	lastConnected int32
}

// reorgEvent records one reorg: the generation it advanced the counter to
// and its fork height.
type reorgEvent struct {
	gen  uint64
	fork int32
}

// maxReorgLog bounds the retained reorg events. A status processor's fetch
// spans one round trip; far fewer reorgs than this can land inside it.
const maxReorgLog = 32

// maxReorgRefetches bounds how many times the status processor re-fetches a
// history whose fetch was overtaken by a reorg before handing the scripthash
// to the tip-change refresh instead.
const maxReorgRefetches = 2

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
	addr btcutil.Address

	// proc serializes the read-modify-write in handleScripthashStatus.
	// handleScripthashStatus can run concurrently for the same scripthash
	// (once from the subscribe path priming the initial status, once from
	// scripthashLoop on a server notification). It reads lastStatus/
	// lastHistory, releases addrCacheMtx across a GetHistory round trip,
	// then writes back the diff result. addrCacheMtx alone only guards the
	// individual field accesses, not the whole logical sequence — two runs
	// could both read the same prev and the last writer would clobber the
	// other's emitted history, losing a RelevantTx. proc is held for the
	// duration of that sequence so the RMW is atomic per scripthash.
	proc sync.Mutex

	lastStatus  string
	lastHistory []electrum.HistoryEntry
	pending     []electrum.HistoryEntry
	ready       chan struct{}
}

var _ Backend = (*ElectrumClient)(nil)
var _ ElectrumBackend = (*ElectrumClient)(nil)
var _ AddressProber = (*ElectrumClient)(nil)
var _ AddressWatcher = (*ElectrumClient)(nil)

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
		probeCache:  make(map[string]*probeHistory),
		reorged:     make(map[string]struct{}),
	}, nil
}

// BackEnd returns the name of the backend.
func (c *ElectrumClient) BackEnd() string { return ElectrumBackEnd }

// EarliestBlockHeight returns the lowest height for which the backend can
// serve headers — the checkpoint anchor the header store starts at. Callers
// walking or bisecting the chain (e.g. the wallet's birthday-block search)
// must not probe below it.
func (c *ElectrumClient) EarliestBlockHeight() int32 { return c.net.StartHeight() }

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
	c.dirty = make(map[string]*electrum.ScripthashStatus)
	c.dirtyWake = make(chan struct{}, 1)
	c.reorgWake = make(chan struct{}, 1)
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

	c.wg.Add(4)
	go c.notificationHandler()
	go c.tipChangeLoop()
	go c.scripthashLoop()
	go c.scripthashProcessor()

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

// Shutdown fully tears down the electrum backend. Electrum has no client/service
// split, so it is identical to Stop; the chainBackend interface distinguishes
// the two so the neutrino backend can stop just its client (e.g. across a
// rescan) while keeping its synced chain service, reserving full teardown for
// Shutdown.
func (c *ElectrumClient) Shutdown() { c.Stop() }

// CanRescan reports false: the electrum network cannot be restarted in place, so
// the wallet cannot perform an in-place rescan over this backend.
func (c *ElectrumClient) CanRescan() bool { return false }

// WaitForShutdown blocks until Stop has fully completed.
func (c *ElectrumClient) WaitForShutdown() {
	c.wg.Wait()
	c.net.WaitForShutdown()
}

// Notifications returns the channel from which chain.Backend notifications
// are dequeued.
func (c *ElectrumClient) Notifications() <-chan any {
	return c.dequeueNotification
}

// ----------------------------------------------------------------------------
// chain.Backend — direct-from-headers methods.
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
	h := c.chainParams.HeaderHash(hdr)
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
	if start := c.net.StartHeight(); int32(height) < start {
		return nil, fmt.Errorf("electrum: %w: height %d, start %d", ErrBeforeAnchor, height, start)
	}
	hdr := c.net.HeaderByHeight(int32(height))
	if hdr == nil {
		return nil, fmt.Errorf("electrum: no header at height %d", height)
	}
	h := c.chainParams.HeaderHash(hdr)
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

// SyncHeight reports the locally-synced header tip, the target height the
// backend is syncing toward (the server's reported best), and whether the
// backend considers itself synced. It lets a wallet report sync progress
// without knowing the backend is electrum.
func (c *ElectrumClient) SyncHeight() (localTip, targetTip int32, synced bool) {
	return c.net.Tip(), c.net.TargetTip(), c.net.Synced()
}

// Connected reports whether the backend currently has a live connection. It
// abstracts the electrum network's Done channel so peer monitoring is
// backend-agnostic.
func (c *ElectrumClient) Connected() bool {
	select {
	case <-c.net.Done():
		return false
	default:
		return true
	}
}

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
func (c *ElectrumClient) SendRawTransaction(tx *bisonwire.Tx, _ bool) (*chainhash.Hash, error) {
	hex, err := serializeTxHex(tx)
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
	connected := c.applyTip(ev)
	if len(ev.Disconnected) > 0 {
		// Disconnected is newest-first; the last entry is the fork
		// height. Everything confirmed at or above it was in an orphaned
		// block.
		c.invalidateReorged(ev.Disconnected[len(ev.Disconnected)-1].Height)
	}
	// Review2 #6: a scripthash status may have arrived while these headers
	// were still in flight. Now that the headers are local and connected,
	// flush any deferred entries so confirmed txs aren't stuck unmined.
	// flushPending is called without connMtx held (it takes addrCacheMtx).
	if connected > 0 {
		c.flushPending()
	}
	// The reorg refresh fetches the history of every affected scripthash.
	// It runs on the scripthash processor: this loop feeds the header path
	// (a stalled consumer here backs up the network's tip-change channel),
	// so it must not wait on that fan-out.
	c.wakeReorgRefresh()
}

// wakeReorgRefresh asks the scripthash processor to service the reorg
// refresh queue if anything is in it. Non-blocking; a wake already pending
// covers this one.
func (c *ElectrumClient) wakeReorgRefresh() {
	c.reorgMtx.Lock()
	queued := len(c.reorged) > 0
	c.reorgMtx.Unlock()
	if !queued {
		return
	}
	select {
	case c.reorgWake <- struct{}{}:
	default:
	}
}

// applyTip processes a single network tip change under connMtx, returning the
// number of BlockConnected notifications delivered. While the initial rescan is
// still running (liveDelivery false) it suppresses connects — goLive will
// deliver the contiguous catch-up at RescanFinished — but NOT disconnects: the
// header store is already truncated by the time the event arrives and the
// rollback cannot be regenerated later (goLive's catch-up delivers connects
// only), so dropping one would leave the wallet holding orphaned blocks. The
// wallet's rollback path is safe at any point in the sync, and the rescan
// handlers re-advance SyncedTo from the wallet's own (rolled-back) position.
func (c *ElectrumClient) applyTip(ev *electrum.TipChange) int {
	c.connMtx.Lock()
	defer c.connMtx.Unlock()
	c.deliverDisconnectsLocked(ev.Disconnected)
	if !c.liveDelivery {
		return 0
	}
	return c.deliverConnectsLocked()
}

// deliverDisconnectsLocked emits a BlockDisconnected per reorged-out block and
// rolls lastConnected back so the subsequent re-connects stay contiguous. The
// metadata is carried by the event because the headers store has already
// removed these blocks from its maps by the time we see them. connMtx held.
func (c *ElectrumClient) deliverDisconnectsLocked(disc []electrum.DisconnectedBlock) {
	for _, d := range disc {
		ntfn := BlockDisconnected(wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: chainhash.Hash(d.Hash), Height: d.Height},
			Time:  time.Unix(d.Time, 0),
		})
		if err := c.enqueue(ntfn); err != nil {
			return
		}
		if d.Height-1 < c.lastConnected {
			c.lastConnected = d.Height - 1
		}
	}
}

// deliverConnectsLocked emits one BlockConnected per height from lastConnected+1
// up to the current header-store tip, advancing lastConnected as it goes. By
// always filling from lastConnected (rather than the event's delta), a tip that
// was suppressed during the initial rescan — or any other gap — is closed here,
// so waddrmgr.PutSyncedTo can chain prev-hash → hash for every block instead of
// being asked to jump forward and failing. connMtx held. Returns the count
// delivered.
func (c *ElectrumClient) deliverConnectsLocked() int {
	tip := c.net.Tip()
	n := 0
	for h := c.lastConnected + 1; h <= tip; h++ {
		hdr := c.net.HeaderByHeight(h)
		if hdr == nil {
			log.Warnf("electrum: connect at %d but no local header", h)
			break
		}
		ntfn := BlockConnected{
			Block: wtxmgr.Block{Hash: c.chainParams.HeaderHash(hdr), Height: h},
			Time:  hdr.Timestamp,
		}
		if err := c.enqueue(ntfn); err != nil {
			break
		}
		c.lastConnected = h
		n++
	}
	return n
}

// goLive transitions from rescan-driven sync to live tip delivery. It is called
// right after a RescanFinished is enqueued. On the first rescan it opens the
// gate at the rescan's tip and delivers a contiguous BlockConnected catch-up for
// any blocks mined during the rescan, so the wallet's SyncedTo advances to the
// real tip without a gap. On later rescans (already live) it only delivers any
// blocks not yet connected — a no-op when already caught up.
func (c *ElectrumClient) goLive(rescanTip int32) {
	c.connMtx.Lock()
	if !c.liveDelivery {
		c.liveDelivery = true
		c.lastConnected = rescanTip
	}
	n := c.deliverConnectsLocked()
	c.connMtx.Unlock()
	if n > 0 {
		c.flushPending()
	}
}

// scripthashLoop is the reader half of the scripthash pipeline. It drains the
// network's scripthash channel into the per-scripthash coalescing buffer and
// wakes the processor. It deliberately does no network I/O, so the upstream
// notify channels never back up and drop the latest status (review finding
// #4): a slow GetHistory in the processor can't stall this reader.
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
			c.dirtyMtx.Lock()
			// Latest-wins: a newer status for the same scripthash
			// supersedes an unprocessed older one. The processor
			// re-fetches full history regardless, so only the most
			// recent status matters.
			c.dirty[st.Scripthash] = st
			c.dirtyMtx.Unlock()
			select {
			case c.dirtyWake <- struct{}{}:
			default:
			}
		}
	}
}

// scripthashProcessor is the worker half of the scripthash pipeline. It drains
// the coalescing buffer and turns each scripthash status into RelevantTx
// notifications via handleScripthashStatus (which fetches and diffs the
// address history), then services the reorg refresh queue. Blocking on
// GetHistory here can't drop notifications because the reader has already
// parked the latest status in c.dirty.
func (c *ElectrumClient) scripthashProcessor() {
	defer c.wg.Done()
	for {
		select {
		case <-c.quit:
			return
		case <-c.dirtyWake:
		case <-c.reorgWake:
		}
		for {
			c.dirtyMtx.Lock()
			var st *electrum.ScripthashStatus
			for sh, s := range c.dirty {
				st = s
				delete(c.dirty, sh)
				break
			}
			c.dirtyMtx.Unlock()
			if st == nil {
				break
			}
			if err := c.handleScripthashStatus(st); err != nil {
				log.Debugf("electrum: handle scripthash %s: %v", st.Scripthash, err)
			}
		}
		c.refreshReorged()
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
	c.addrCacheMtx.Unlock()
	if !ok {
		log.Debugf("electrum: status for unknown scripthash %s", st.Scripthash)
		return nil
	}

	// Serialize the whole read-modify-write against concurrent invocations
	// for the same scripthash. Held across the GetHistory RPC so a second
	// caller can't read a stale prev and clobber our emitted history.
	entry.proc.Lock()
	defer entry.proc.Unlock()

	c.addrCacheMtx.Lock()
	if entry.lastStatus == st.Status {
		c.addrCacheMtx.Unlock()
		return nil
	}
	c.addrCacheMtx.Unlock()

	for attempt := 0; ; attempt++ {
		first := attempt == 0
		fetch := func() ([]electrum.HistoryEntry, error) {
			// A discovery probe may have parked this scripthash's
			// history; reuse it iff its derived status matches the
			// server-reported one (i.e. nothing changed since the
			// probe), else fetch fresh. Only on the first attempt: a
			// re-fetch after a reorg must reach the server, or the
			// parked reply is committed again as if it were fresh.
			if first {
				if hist, ok := c.probedHistory(st.Scripthash, st.Status); ok {
					log.Debugf("electrum: status handler using probed history for %s", st.Scripthash)
					return hist, nil
				}
			}
			return c.net.GetHistory(c.ctx, st.Scripthash)
		}
		fork, moved, err := c.refetchEntry(st.Scripthash, entry, fetch, st.Status)
		if err != nil {
			return fmt.Errorf("get history: %w", err)
		}
		if !moved {
			return nil
		}
		if attempt >= maxReorgRefetches {
			// Reorgs keep overtaking the fetch; leave the scripthash to
			// the reorg refresh, which runs once the chain settles.
			c.reorgMtx.Lock()
			c.reorged[st.Scripthash] = struct{}{}
			c.reorgMtx.Unlock()
			return nil
		}
		log.Debugf("electrum: reorg at %d overtook the history fetch for %s; "+
			"re-fetching", fork, st.Scripthash)
	}
}

// refetchEntry fetches the scripthash's history, diffs it against the
// entry's seen baseline (emitting what is new, see applyHistory) and commits
// the result. status is the value recorded as the entry's last status; ""
// derives it from the fetched history. Caller holds entry.proc.
//
// A reorg can land between the baseline read and the commit, in which case
// the history was judged against headers the reorg replaced and the
// wallet's rollback unconfirmed what was emitted. The commit detects that
// through the reorg generation: it then keeps only what the reorg could not
// have touched, clears the status so the entry awaits a refresh, and reports
// the fork height with moved set. The generation is read before the baseline
// and checked under addrCacheMtx, the lock invalidateReorged trims the
// baseline under after advancing the generation, so a reorg is either seen
// here or its trim lands after this commit.
func (c *ElectrumClient) refetchEntry(sh string, entry *scripthashEntry,
	fetch func() ([]electrum.HistoryEntry, error), status string) (fork int32, moved bool, err error) {

	gen := c.reorgGen.Load()
	c.addrCacheMtx.Lock()
	prev := append([]electrum.HistoryEntry(nil), entry.lastHistory...)
	c.addrCacheMtx.Unlock()

	hist, err := fetch()
	if err != nil {
		return 0, false, err
	}
	emitted, deferred := c.applyHistory(prev, hist)

	c.addrCacheMtx.Lock()
	defer c.addrCacheMtx.Unlock()
	// Confirm it's still the same entry before committing. A subscribed
	// entry is never deleted today (the only addrCache delete is the
	// subscribe-failure rollback, which can't apply to an entry being
	// processed), so this is defense-in-depth: if a future Unsubscribe
	// ever removes-and-replaces an entry across the fetch window, skip
	// rather than write a stale-derived result onto a fresh entry.
	if e, ok := c.addrCache[sh]; !ok || e != entry {
		return 0, false, nil
	}
	if fork, moved = c.reorgSince(gen); moved {
		entry.lastHistory = historyBelow(emitted, fork)
		entry.pending = historyBelow(mergePendingEntries(entry.pending, deferred), fork)
		entry.lastStatus = ""
		return fork, true, nil
	}
	if status == "" {
		status = statusFromHistory(hist)
	}
	entry.lastStatus = status
	entry.lastHistory = emitted
	entry.pending = mergePendingEntries(entry.pending, deferred)
	return 0, false, nil
}

// reorgSince reports whether a reorg has landed since generation gen and,
// if so, the lowest fork height among those that did. A generation older
// than the retained log is conservatively reported as reorged from the
// start height.
func (c *ElectrumClient) reorgSince(gen uint64) (fork int32, moved bool) {
	if c.reorgGen.Load() == gen {
		return 0, false
	}
	c.reorgMtx.Lock()
	defer c.reorgMtx.Unlock()
	if len(c.reorgs) == 0 || c.reorgs[0].gen > gen+1 {
		return c.net.StartHeight(), true
	}
	fork = -1
	for _, ev := range c.reorgs {
		if ev.gen > gen && (fork < 0 || ev.fork < fork) {
			fork = ev.fork
		}
	}
	return fork, fork >= 0
}

// historyBelow returns the entries confirmed below height, dropping mempool
// entries too when height is at or below zero (there is nothing below them).
// Electrum reports a mempool entry at height 0, or -1 when its parents are
// unconfirmed too, so the mempool is handled as a whole rather than by the
// height comparison.
func historyBelow(entries []electrum.HistoryEntry, height int32) []electrum.HistoryEntry {
	kept := entries[:0:0]
	if height <= 0 {
		return kept
	}
	for _, e := range entries {
		if e.Height < height {
			kept = append(kept, e)
		}
	}
	return kept
}

// applyHistory diffs a fresh history against the entries already processed
// (prev), emits a RelevantTx for each new entry whose block is local, and
// returns the new carry-forward list (prev's survivors plus what was
// emitted or skipped) and the entries deferred to pending: those whose
// header has not arrived, and those whose emission failed (a raw-tx or
// merkle fetch error), which the next tip change retries. A failed entry
// left out of both lists would be lost until the server's status next
// changed — which, for a transient failure during a rotation, could be
// never. The caller commits both under the entry's locks.
func (c *ElectrumClient) applyHistory(prev, hist []electrum.HistoryEntry) (emitted, deferred []electrum.HistoryEntry) {
	// Partition the new entries into "ready to emit" and "deferred until
	// the header arrives". Already-seen entries are not re-emitted but
	// stay in the carry-forward list.
	// Nothing below the header store's start exists locally, so entries
	// confirmed there are pre-birthday by construction (the anchor sits
	// below the birthday block) whether or not a rescan has set the floor.
	floor := max(c.rescanFloor.Load(), c.net.StartHeight())
	emitted = make([]electrum.HistoryEntry, 0, len(hist))
	var toEmit []electrum.HistoryEntry
	for _, h := range hist {
		if alreadySeen(prev, h.TxHash, h.Height) {
			emitted = append(emitted, h)
			continue
		}
		if h.Height > 0 && h.Height < floor {
			// Confirmed below the floor: contractually pre-birthday,
			// not ours. Mark seen without emitting so the entry
			// doesn't reappear on every future diff.
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
		toEmit = append(toEmit, h)
	}
	// Fetch the new txs' raw bytes concurrently and enqueue them; only the
	// successfully-emitted ones join lastHistory, and the rest are parked
	// for retry.
	done := c.emitRelevantTxs(toEmit)
	emitted = append(emitted, done...)
	if len(done) < len(toEmit) {
		ok := make(map[string]struct{}, len(done))
		for _, e := range done {
			ok[fmt.Sprintf("%s|%d", e.TxHash, e.Height)] = struct{}{}
		}
		for _, e := range toEmit {
			if _, did := ok[fmt.Sprintf("%s|%d", e.TxHash, e.Height)]; !did {
				deferred = append(deferred, e)
			}
		}
	}
	return emitted, deferred
}

// invalidateReorged handles a reorg whose fork height is fork: every
// scripthash with history confirmed at or above it loses those entries from
// its seen baseline (and its pending set) and is queued for a refresh
// against the new chain. The wallet's disconnect handler unconfirms every
// transaction from the fork height onward; without this, nothing would ever
// re-confirm them — a block re-mined with the same transactions leaves the
// scripthash status unchanged (it hashes txid:height pairs), so no
// notification arrives, and the seen baseline would suppress the entries
// even if one did.
//
// This runs on the tip-change loop and takes no entry's proc lock: a status
// processor holds that lock across its history fetch, and waiting on it
// here would stall header delivery behind network I/O. The trim is safe
// without it because the generation advances first and the processor
// checks it under addrCacheMtx at commit (see refetchEntry): a processor
// mid-fetch re-judges its result against the fork, and one that committed
// already has its baseline trimmed here, after the commit.
func (c *ElectrumClient) invalidateReorged(fork int32) {
	c.reorgMtx.Lock()
	gen := c.reorgGen.Add(1)
	c.reorgs = append(c.reorgs, reorgEvent{gen: gen, fork: fork})
	if len(c.reorgs) > maxReorgLog {
		c.reorgs = c.reorgs[len(c.reorgs)-maxReorgLog:]
	}
	c.reorgMtx.Unlock()

	atOrAbove := func(entries []electrum.HistoryEntry) bool {
		for _, e := range entries {
			if e.Height >= fork {
				return true
			}
		}
		return false
	}

	var affected []string
	c.addrCacheMtx.Lock()
	for sh, entry := range c.addrCache {
		if !atOrAbove(entry.lastHistory) && !atOrAbove(entry.pending) {
			continue
		}
		entry.lastHistory = historyBelow(entry.lastHistory, fork)
		entry.pending = historyBelow(entry.pending, fork)
		// Clearing the status lets a status notification for the new
		// chain through the equal-status short circuit, and marks the
		// entry as awaiting refresh (see refreshReorged).
		entry.lastStatus = ""
		affected = append(affected, sh)
	}
	c.addrCacheMtx.Unlock()
	if len(affected) == 0 {
		return
	}

	c.reorgMtx.Lock()
	for _, sh := range affected {
		c.reorged[sh] = struct{}{}
	}
	c.reorgMtx.Unlock()
	log.Infof("electrum: reorg at height %d invalidated confirmed history on "+
		"%d subscribed scripthashes; refreshing", fork, len(affected))
}

// refreshReorged re-fetches the history of every scripthash queued by
// invalidateReorged and runs the standard diff against its trimmed baseline,
// re-emitting transactions on the new chain (with their new block, or as
// unconfirmed if they fell back to the mempool). Entries whose header is not
// local yet defer to pending and flush with the next connect. A scripthash
// already refreshed by a live status notification is skipped; one whose
// fetch fails, or whose fetch another reorg overtook, stays queued for the
// next tip change. Runs on the scripthash processor.
func (c *ElectrumClient) refreshReorged() {
	c.reorgMtx.Lock()
	shs := make([]string, 0, len(c.reorged))
	for sh := range c.reorged {
		shs = append(shs, sh)
	}
	c.reorgMtx.Unlock()

	for _, sh := range shs {
		c.addrCacheMtx.Lock()
		entry := c.addrCache[sh]
		c.addrCacheMtx.Unlock()
		if entry == nil {
			c.clearReorged(sh)
			continue
		}
		entry.proc.Lock()
		c.addrCacheMtx.Lock()
		refreshed := entry.lastStatus != ""
		c.addrCacheMtx.Unlock()
		if refreshed {
			c.clearReorged(sh)
			entry.proc.Unlock()
			continue
		}
		fetch := func() ([]electrum.HistoryEntry, error) {
			return c.net.GetHistory(c.ctx, sh)
		}
		fork, moved, err := c.refetchEntry(sh, entry, fetch, "")
		switch {
		case err != nil:
			log.Debugf("electrum: reorg refresh of %s: get history: %v", sh, err)
		case moved:
			log.Debugf("electrum: reorg at %d overtook the refresh of %s; "+
				"deferring to the next tip change", fork, sh)
		default:
			// Cleared before proc is released: a status processor that
			// exhausts its re-fetch budget queues the scripthash while
			// holding proc, so a request made after this refresh cannot
			// be discarded by it.
			c.clearReorged(sh)
		}
		entry.proc.Unlock()
	}
}

func (c *ElectrumClient) clearReorged(sh string) {
	c.reorgMtx.Lock()
	delete(c.reorged, sh)
	c.reorgMtx.Unlock()
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
	// Snapshot the scripthashes that currently have pending entries. We
	// cannot hold addrCacheMtx while acquiring entry.proc — handleScripthashStatus
	// acquires proc first and then addrCacheMtx, so taking them in the
	// opposite order here would risk deadlock. Collect the keys, then process
	// each entry under its own proc lock.
	c.addrCacheMtx.Lock()
	shs := make([]string, 0, len(c.addrCache))
	for sh, entry := range c.addrCache {
		if len(entry.pending) > 0 {
			shs = append(shs, sh)
		}
	}
	c.addrCacheMtx.Unlock()

	for _, sh := range shs {
		c.addrCacheMtx.Lock()
		entry := c.addrCache[sh]
		c.addrCacheMtx.Unlock()
		if entry == nil {
			continue
		}

		// Hold proc across the partition + emit + lastHistory append so the
		// whole sequence is atomic against handleScripthashStatus's RMW for
		// this scripthash. Otherwise a concurrent status diff could read
		// lastHistory without the flushed entries (not appended yet) and
		// pending without them either (already trimmed), and re-emit them as
		// duplicates — exactly the lost-update the proc lock exists to
		// prevent.
		entry.proc.Lock()

		c.addrCacheMtx.Lock()
		var ready, remain []electrum.HistoryEntry
		for _, e := range entry.pending {
			if e.Height > 0 && c.net.HeaderByHeight(e.Height) == nil {
				remain = append(remain, e)
				continue
			}
			ready = append(ready, e)
		}
		entry.pending = remain
		c.addrCacheMtx.Unlock()

		// Only what was actually emitted joins the diff baseline; a
		// transient per-entry failure (a merkle or raw-tx fetch error)
		// returns to pending for the next flush. Committing an unemitted
		// entry would suppress it forever — everything in lastHistory is
		// treated as already seen, the wallet's synced height keeps
		// advancing past it, and no future rescan walks below that — and
		// pending now carries rescan-critical history (setup entries above
		// the walk tip and the deferred tail of an early-finished walk),
		// not just the rare live-path header race.
		emitted := c.emitRelevantTxs(ready)

		c.addrCacheMtx.Lock()
		entry.lastHistory = append(entry.lastHistory, emitted...)
		if len(emitted) < len(ready) {
			ok := make(map[string]struct{}, len(emitted))
			for _, e := range emitted {
				ok[fmt.Sprintf("%s|%d", e.TxHash, e.Height)] = struct{}{}
			}
			var failed []electrum.HistoryEntry
			for _, e := range ready {
				if _, did := ok[fmt.Sprintf("%s|%d", e.TxHash, e.Height)]; !did {
					failed = append(failed, e)
				}
			}
			entry.pending = mergePendingEntries(entry.pending, failed)
		}
		c.addrCacheMtx.Unlock()

		entry.proc.Unlock()
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

// buildRelevantTx fetches the raw transaction for a history entry and builds the
// corresponding RelevantTx (the network round-trip + decode, without enqueuing).
func (c *ElectrumClient) buildRelevantTx(h electrum.HistoryEntry) (RelevantTx, error) {
	rawHex, err := c.net.GetRawTransaction(c.ctx, h.TxHash)
	if err != nil {
		return RelevantTx{}, err
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return RelevantTx{}, fmt.Errorf("decode raw tx: %w", err)
	}
	// A 64-byte "transaction" can be a merkle interior node; reject it so a
	// proof cannot authenticate a fabricated node as a leaf (CVE-2017-12842
	// class), independent of the binding check below.
	if len(raw) == 64 {
		return RelevantTx{}, fmt.Errorf("electrum: refusing 64-byte tx %s", h.TxHash)
	}
	rec, err := wtxmgr.NewTxRecord(string(c.chain), raw, time.Now())
	if err != nil {
		return RelevantTx{}, fmt.Errorf("new tx record: %w", err)
	}
	// Bind the fetched bytes to the txid the merkle proof authenticated:
	// verifyInclusion proved h.TxHash is in the block, so a server serving
	// different bytes for that txid would otherwise slip an unproven tx into
	// the wallet as confirmed.
	if rec.Hash.String() != h.TxHash {
		return RelevantTx{}, fmt.Errorf("electrum: server returned tx %s for "+
			"requested %s", rec.Hash, h.TxHash)
	}
	var blockMeta *wtxmgr.BlockMeta
	if h.Height > 0 {
		hdr := c.net.HeaderByHeight(h.Height)
		if hdr != nil {
			blockMeta = &wtxmgr.BlockMeta{
				Block: wtxmgr.Block{Hash: c.chainParams.HeaderHash(hdr), Height: h.Height},
				Time:  hdr.Timestamp,
			}
		}
	}
	return RelevantTx{TxRecord: rec, Block: blockMeta}, nil
}

// maxRawTxFetches bounds the concurrent GetRawTransaction round-trips in
// emitRelevantTxs.
const maxRawTxFetches = 8

// emitRelevantTxs fetches the raw transactions for the given history entries
// CONCURRENTLY, then enqueues a RelevantTx notification for each in the original
// order, returning the entries that were successfully enqueued.
//
// The fetch is parallelized on purpose: GetRawTransaction is a per-tx network
// round-trip and the scripthash processor that calls this is single-threaded, so
// fetching serially made detection latency grow with the number of new
// transactions — e.g. several receives to the same address arriving close
// together would each wait behind the previous one's round-trip. Enqueue order
// is preserved so any in-batch parent→child dependency stays intact for the
// wallet.
// verifyInclusion checks a confirmed transaction's merkle proof against the
// LOCAL validated header at its height (multi-server rotation plan R7). The
// header chain is anchored, retarget-checked, and PoW-bounded, so a proof
// folding to its merkle root makes fabrication as hard as producing that
// work; a server that cannot produce a valid proof is lying. Mempool
// entries (height <= 0) have no proof and are not checked.
func (c *ElectrumClient) verifyInclusion(txid string, height int32) error {
	hdr := c.net.HeaderByHeight(height)
	if hdr == nil {
		return fmt.Errorf("electrum: no local header at %d for inclusion proof", height)
	}
	proof, err := c.net.GetMerkle(c.ctx, txid, height)
	if err != nil {
		return fmt.Errorf("electrum: get_merkle %s: %w", txid, err)
	}
	node, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return err
	}
	cur := *node
	pos := proof.Pos
	for _, sibHex := range proof.Merkle {
		sib, err := chainhash.NewHashFromStr(sibHex)
		if err != nil {
			return fmt.Errorf("electrum: bad merkle sibling: %w", err)
		}
		var cat [64]byte
		if pos&1 == 1 {
			copy(cat[:32], sib[:])
			copy(cat[32:], cur[:])
		} else {
			copy(cat[:32], cur[:])
			copy(cat[32:], sib[:])
		}
		cur = chainhash.DoubleHashH(cat[:])
		pos >>= 1
	}
	if cur != hdr.MerkleRoot {
		return fmt.Errorf("electrum: merkle proof for %s does not fold to the "+
			"validated root at %d", txid, height)
	}
	return nil
}

func (c *ElectrumClient) emitRelevantTxs(entries []electrum.HistoryEntry) []electrum.HistoryEntry {
	if len(entries) == 0 {
		return nil
	}
	built := make([]*RelevantTx, len(entries))
	sem := make(chan struct{}, maxRawTxFetches)
	var wg sync.WaitGroup
	for i := range entries {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			// Confirmed entries must prove inclusion against our own
			// validated header before they may enter the wallet; a
			// failed proof is a lying server and the entry is refused
			// (a future honest status diff re-delivers it).
			if h := entries[i].Height; h > 0 {
				if err := c.verifyInclusion(entries[i].TxHash, h); err != nil {
					log.Errorf("electrum: refusing tx %s at %d: %v",
						entries[i].TxHash, h, err)
					return
				}
			}
			rtx, err := c.buildRelevantTx(entries[i])
			if err != nil {
				log.Debugf("electrum: build relevant tx %s: %v", entries[i].TxHash, err)
				return
			}
			built[i] = &rtx
		}(i)
	}
	wg.Wait()

	emitted := make([]electrum.HistoryEntry, 0, len(entries))
	for i, rtx := range built {
		if rtx == nil {
			continue
		}
		select {
		case c.enqueueNotification <- *rtx:
			emitted = append(emitted, entries[i])
		case <-c.quit:
			return emitted
		}
	}
	return emitted
}

// ----------------------------------------------------------------------------
// notificationHandler — drains enqueueNotification into dequeueNotification.
// Mirrors NeutrinoClient.notificationHandler for compatibility.
// ----------------------------------------------------------------------------

func (c *ElectrumClient) notificationHandler() {
	defer c.wg.Done()

	// bs is the block stamp delivered to BlockStamp() callers. It is primed by
	// whichever of the following happens first, then kept current by subsequent
	// BlockConnected notifications:
	//
	//   1. GetBestBlock once the network reports Synced(). This matches
	//      NeutrinoClient, which stamps the current tip at handler startup so
	//      BlockStamp() returns as soon as the chain is synced — even on a
	//      quiescent chain that never delivers a new BlockConnected, and even
	//      before the rescan->live handoff opens the connect gate. We can't call
	//      GetBestBlock at startup the way Neutrino does, because the header sync
	//      now runs in the background and Tip() may still be -1; instead we poll
	//      with primeTicker and only stamp once IsCurrent() is true. GetBestBlock
	//      can still transiently error during a reorg rewind (Tip() pointing at a
	//      just-deleted header); we simply retry on the next tick.
	//
	//   2. A BlockConnected from the live-delivery path, which advances bs as new
	//      blocks arrive.
	//
	// Until bs is primed, BlockStamp() callers block (the nil send-case below is
	// never selectable), which is what callers expect pre-first-block.
	var bs *waddrmgr.BlockStamp

	// primeTicker drives the Synced()->GetBestBlock priming described above. It
	// only matters until bs is set (by either path), after which its select case
	// is disabled so it adds no ongoing overhead.
	primeTicker := time.NewTicker(200 * time.Millisecond)
	defer primeTicker.Stop()

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

		// Poll for Synced()->GetBestBlock priming only until bs is set.
		var primeCh <-chan time.Time
		if bs == nil {
			primeCh = primeTicker.C
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

		case <-primeCh:
			// The network has caught up but no BlockConnected has primed bs
			// yet (a quiescent chain, or the pre-rescan window where connects
			// are still gated). Stamp the current tip directly, matching
			// NeutrinoClient. IsCurrent guards against Tip() == -1; a reorg
			// rewind can still make GetBestBlock error transiently, so we just
			// retry on the next tick.
			if c.IsCurrent() {
				if hash, height, err := c.GetBestBlock(); err == nil {
					bs = &waddrmgr.BlockStamp{Hash: *hash, Height: height}
				}
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

// serializeTxHex returns the hex-encoded wire format of tx, including a Dash
// special transaction's type and extra payload.
func serializeTxHex(tx *bisonwire.Tx) (string, error) {
	var buf bytes.Buffer
	buf.Grow(tx.SerializeSize())
	if err := tx.Serialize(&buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf.Bytes()), nil
}
