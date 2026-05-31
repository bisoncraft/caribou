// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

	// addrCache maps a scripthash hex string to the address it was
	// registered for, plus the last status we processed so we can detect
	// changes via the subscription notifications. Mutex-guarded.
	addrCacheMtx sync.Mutex
	addrCache    map[string]*scripthashEntry

	// outpointCache maps a watched outpoint to its address, used during
	// rescan / FilterBlocks. Mutex-guarded.
	outpointCacheMtx sync.Mutex
	outpointCache    map[wire.OutPoint]btcutil.Address

	enqueueNotification chan any
	dequeueNotification chan any
	currentBlock        chan *waddrmgr.BlockStamp

	ctx    context.Context
	cancel context.CancelFunc
	quit   chan struct{}

	startMtx sync.Mutex
	started  bool
	wg       sync.WaitGroup
}

// scripthashEntry captures per-scripthash state.
type scripthashEntry struct {
	addr        btcutil.Address
	lastStatus  string
	lastHistory []electrum.HistoryEntry
}

var _ Interface = (*ElectrumClient)(nil)
var _ ElectrumBackend = (*ElectrumClient)(nil)

// NewElectrumClient constructs an ElectrumClient backed by the given network.
// The network must not be started; ElectrumClient.Start will start it.
func NewElectrumClient(chainParams *netparams.ChainParams, network ElectrumNetwork) *ElectrumClient {
	return &ElectrumClient{
		net:           network,
		chainParams:   chainParams,
		btcParams:     chainParams.BTCDParams(),
		addrCache:     make(map[string]*scripthashEntry),
		outpointCache: make(map[wire.OutPoint]btcutil.Address),
	}
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

// tipChangeLoop translates electrum.Network tip-change events into
// chain.BlockConnected notifications.
func (c *ElectrumClient) tipChangeLoop() {
	defer c.wg.Done()
	ch := c.net.TipChangeChan()
	for {
		select {
		case <-c.quit:
			return
		case height, ok := <-ch:
			if !ok {
				return
			}
			c.dispatchTipChange(height)
		}
	}
}

func (c *ElectrumClient) dispatchTipChange(height int32) {
	hdr := c.net.HeaderByHeight(height)
	if hdr == nil {
		log.Warnf("electrum: tip-change to %d but no local header", height)
		return
	}
	ntfn := BlockConnected{
		Block: wtxmgr.Block{Hash: hdr.BlockHash(), Height: height},
		Time:  hdr.Timestamp,
	}
	select {
	case c.enqueueNotification <- ntfn:
	case <-c.quit:
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
// each newly-seen entry.
func (c *ElectrumClient) handleScripthashStatus(st *electrum.ScripthashStatus) error {
	c.addrCacheMtx.Lock()
	entry, ok := c.addrCache[st.Scripthash]
	c.addrCacheMtx.Unlock()
	if !ok {
		log.Debugf("electrum: status for unknown scripthash %s", st.Scripthash)
		return nil
	}
	if entry.lastStatus == st.Status {
		return nil
	}
	hist, err := c.net.GetHistory(c.ctx, st.Scripthash)
	if err != nil {
		return fmt.Errorf("get history: %w", err)
	}
	prev := entry.lastHistory
	for _, h := range hist {
		if alreadySeen(prev, h.TxHash, h.Height) {
			continue
		}
		if err := c.emitRelevantTx(h); err != nil {
			log.Debugf("electrum: emit %s: %v", h.TxHash, err)
		}
	}
	c.addrCacheMtx.Lock()
	entry.lastStatus = st.Status
	entry.lastHistory = hist
	c.addrCacheMtx.Unlock()
	return nil
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
	rec, err := wtxmgr.NewTxRecord(string(chainFromParams(c.chainParams)), raw, time.Now())
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

// chainFromParams returns the bisonwire.Chain implied by chainParams.Chain.
func chainFromParams(p *netparams.ChainParams) bisonwire.Chain {
	c, err := bisonwire.ChainFromString(p.Chain)
	if err != nil {
		// Fall back to BTC to avoid panicking; the wallet will surface
		// any real mismatch through tx-record validation later.
		return bisonwire.ChainBTC
	}
	return c
}

// ----------------------------------------------------------------------------
// notificationHandler — drains enqueueNotification into dequeueNotification.
// Mirrors NeutrinoClient.notificationHandler for compatibility.
// ----------------------------------------------------------------------------

func (c *ElectrumClient) notificationHandler() {
	defer c.wg.Done()
	hash, height, err := c.GetBestBlock()
	bs := &waddrmgr.BlockStamp{}
	if err == nil {
		bs = &waddrmgr.BlockStamp{Hash: *hash, Height: height}
	}

	var notifications []any
	enqueue := c.enqueueNotification
	var dequeue chan any
	var next any

	for {
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
			if len(notifications) == 0 {
				next = n
				dequeue = c.dequeueNotification
			}
			notifications = append(notifications, n)

		case dequeue <- next:
			if bc, ok := next.(BlockConnected); ok {
				bs = &waddrmgr.BlockStamp{Hash: bc.Hash, Height: bc.Height}
			}
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

		case c.currentBlock <- bs:

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
	for i, j := 0, len(sum)-1; i < j; i, j = i+1, j-1 {
		sum[i], sum[j] = sum[j], sum[i]
	}
	return hex.EncodeToString(sum[:]), nil
}

// serializeMsgTxHex returns the hex-encoded wire format of tx.
func serializeMsgTxHex(tx *wire.MsgTx) (string, error) {
	buf := make([]byte, 0, tx.SerializeSize())
	w := newByteWriter(buf)
	if err := tx.Serialize(w); err != nil {
		return "", err
	}
	return hex.EncodeToString(w.Bytes()), nil
}

// byteWriter is a tiny growable buffer that implements io.Writer without
// the extra surface of bytes.Buffer (and without importing it just for this).
type byteWriter struct{ b []byte }

func newByteWriter(b []byte) *byteWriter { return &byteWriter{b: b} }
func (w *byteWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}
func (w *byteWriter) Bytes() []byte { return w.b }
