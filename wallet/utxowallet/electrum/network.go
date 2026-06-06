// Copyright (c) 2024 The Bison Wallet developers
// Copyright (c) The go-electrum-client authors
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// Config configures a Network.
type Config struct {
	// Chain identifies the chain and provides chain parameters.
	Chain *ChainConfig

	// TrustedServer is the initial server to dial. It is required.
	TrustedServer *ServerAddr

	// TorProxy is an optional SOCKS5 proxy address (host:port). When set,
	// onion services in TrustedServer/Servers are dialed via this proxy.
	TorProxy string

	// DataDir is where the header file is persisted. The package creates
	// `<DataDir>/electrum/<chain>/`.
	DataDir string

	// HeaderBatchSize controls the per-request count when catching up
	// from a cold start. The electrumx default cap is 2016. Defaults to
	// 2016 when zero.
	HeaderBatchSize int
}

// defaultHeaderBatchSize matches electrumx's recommended max chunk.
const defaultHeaderBatchSize = 2016

// Network is the public client entry point: a connected electrumx peer plus
// a synced header store. Phase 1 supports a single trusted server only;
// multi-peer failover will be added in a later phase.
//
// Lifecycle: NewNetwork constructs a Network in a not-started state. Start
// dials the trusted server, syncs headers, and subscribes to tip changes.
// Stop tears everything down; Done returns a channel that closes once
// teardown completes.
type Network struct {
	cfg     *Config
	headers *Headers

	mu      sync.RWMutex
	node    *Node
	started bool

	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	doneClosed sync.Once
	done       chan struct{}

	tipChangeChan chan *TipChange
	scripthashCh  chan *ScripthashStatus
}

// NewNetwork constructs a Network ready to be Started.
func NewNetwork(cfg *Config) (*Network, error) {
	if cfg == nil {
		return nil, errors.New("electrum: nil network config")
	}
	if cfg.Chain == nil {
		return nil, errors.New("electrum: network config missing Chain")
	}
	if cfg.TrustedServer == nil {
		return nil, errors.New("electrum: network config missing TrustedServer")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("electrum: network config missing DataDir")
	}
	if cfg.HeaderBatchSize == 0 {
		cfg.HeaderBatchSize = defaultHeaderBatchSize
	}
	h, err := NewHeaders(cfg.Chain, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &Network{
		cfg:           cfg,
		headers:       h,
		done:          make(chan struct{}),
		tipChangeChan: make(chan *TipChange, notifyBufSize),
		scripthashCh:  make(chan *ScripthashStatus, notifyBufSize),
	}, nil
}

// Headers returns the underlying header store. Useful for chain.Interface
// implementations that need direct read access.
func (n *Network) Headers() *Headers { return n.headers }

// Start connects to the trusted server, syncs headers, and begins listening
// for tip and scripthash notifications. The provided ctx is the network's
// lifetime: cancelling ctx is equivalent to calling Stop.
//
// On any error Start unwinds — cancels the internal context, closes the
// partially-initialised node (if any), and closes the done channel so a
// caller doing `defer net.WaitForShutdown()` doesn't deadlock.
func (n *Network) Start(parent context.Context) error {
	n.mu.Lock()
	if n.started {
		n.mu.Unlock()
		return errors.New("electrum: network already started")
	}
	// Stage the context but don't flip `started` until everything we
	// need is up — Stop relies on `started` to decide whether there's
	// anything to tear down.
	n.ctx, n.cancel = context.WithCancel(parent)
	n.mu.Unlock()

	failed := func(err error) error {
		if n.cancel != nil {
			n.cancel()
		}
		n.doneClosed.Do(func() {
			close(n.tipChangeChan)
			close(n.scripthashCh)
			close(n.done)
		})
		return err
	}

	node, err := NewNode(n.ctx, n.cfg.TrustedServer, n.cfg.TorProxy,
		n.cfg.Chain.Params.GenesisHash.String())
	if err != nil {
		return failed(err)
	}

	// Subscribe FIRST so syncHeaders knows the server-reported tip. That
	// height becomes the tipHint for AppendBatch: headers within the
	// trailing recentPoWWindow get per-header PoW, older headers are
	// validated only by checkpoint match + retarget structure (much
	// faster than per-header scrypt over millions of LTC blocks).
	//
	// Tip-change notifications that arrive during sync are buffered by
	// the server's headersNotify channel; we drain them via headersLoop
	// after sync completes.
	tip, err := node.SubscribeHeaders(n.ctx)
	if err != nil {
		node.Close()
		return failed(fmt.Errorf("electrum: subscribe headers: %w", err))
	}

	if err := n.syncHeaders(n.ctx, node, tip.Height); err != nil {
		node.Close()
		return failed(fmt.Errorf("electrum: initial header sync: %w", err))
	}

	// Commit: everything is up, publish the node and flip `started`.
	n.mu.Lock()
	n.node = node
	n.started = true
	n.mu.Unlock()

	// The first subscribe payload is the current tip. If we're already at
	// or above that height our sync is a no-op; otherwise the subscription
	// loop will pull us forward.
	n.wg.Add(2)
	go n.headersLoop(tip)
	go n.scripthashLoop()

	// Background watcher: when the node drops, tear the network down. A
	// future phase will rotate to a fallback peer here.
	n.wg.Add(1)
	go n.watchNode(node)

	return nil
}

// Stop initiates teardown. Done closes once teardown finishes. Safe to call
// on a network that never successfully started; in that case Stop just makes
// sure WaitForShutdown unblocks.
func (n *Network) Stop() {
	n.mu.Lock()
	if !n.started {
		n.mu.Unlock()
		// Never started, or already stopped. Either way ensure the
		// done channel is closed so WaitForShutdown does not hang.
		n.doneClosed.Do(func() {
			close(n.tipChangeChan)
			close(n.scripthashCh)
			close(n.done)
		})
		return
	}
	cancel := n.cancel
	node := n.node
	n.started = false
	n.mu.Unlock()

	if node != nil {
		node.Close()
	}
	if cancel != nil {
		cancel()
	}
}

// Done returns a channel that closes once Stop has fully completed.
func (n *Network) Done() <-chan struct{} { return n.done }

// WaitForShutdown blocks until all background goroutines have exited.
func (n *Network) WaitForShutdown() { <-n.done }

// TipChangeChan returns a channel that receives a TipChange event every time
// the chain advances or rewinds. The channel is closed when the network
// shuts down.
func (n *Network) TipChangeChan() <-chan *TipChange { return n.tipChangeChan }

// ScripthashChan returns a channel that receives scripthash status changes
// for any subscribed scripthash. The channel is closed when the network
// shuts down.
func (n *Network) ScripthashChan() <-chan *ScripthashStatus { return n.scripthashCh }

// ----------------------------------------------------------------------------
// Public read API delegating to Headers / Node
// ----------------------------------------------------------------------------

// Tip returns the current best block height as known to this network.
func (n *Network) Tip() int32 { return n.headers.Tip() }

// Synced reports whether the initial header sync has completed.
func (n *Network) Synced() bool { return n.headers.Synced() }

// HeaderByHeight returns the block header at height, or nil if unknown.
func (n *Network) HeaderByHeight(height int32) *wire.BlockHeader {
	return n.headers.ByHeight(height)
}

// HeaderByHash returns the height and header for the given block hash, plus
// ok=false if the hash is not in the local store.
func (n *Network) HeaderByHash(hash *chainhash.Hash) (int32, *wire.BlockHeader, bool) {
	return n.headers.ByHash(hash)
}

// TipHash returns the hash of the highest connected header (zero hash if
// none).
func (n *Network) TipHash() chainhash.Hash {
	return n.headers.TipHash()
}

// SubscribeScripthash subscribes to status changes for a scripthash and
// returns the current status.
func (n *Network) SubscribeScripthash(ctx context.Context, scripthash string) (*ScripthashStatus, error) {
	node, err := n.getNode()
	if err != nil {
		return nil, err
	}
	return node.SubscribeScripthash(ctx, scripthash)
}

// UnsubscribeScripthash undoes a previous subscription.
func (n *Network) UnsubscribeScripthash(ctx context.Context, scripthash string) {
	node, err := n.getNode()
	if err != nil {
		return
	}
	node.UnsubscribeScripthash(ctx, scripthash)
}

// GetHistory issues `blockchain.scripthash.get_history`.
func (n *Network) GetHistory(ctx context.Context, scripthash string) ([]HistoryEntry, error) {
	node, err := n.getNode()
	if err != nil {
		return nil, err
	}
	return node.GetHistory(ctx, scripthash)
}

// ListUnspent issues `blockchain.scripthash.listunspent`.
func (n *Network) ListUnspent(ctx context.Context, scripthash string) ([]UnspentEntry, error) {
	node, err := n.getNode()
	if err != nil {
		return nil, err
	}
	return node.ListUnspent(ctx, scripthash)
}

// GetRawTransaction issues `blockchain.transaction.get` with verbose=false.
func (n *Network) GetRawTransaction(ctx context.Context, txid string) (string, error) {
	node, err := n.getNode()
	if err != nil {
		return "", err
	}
	return node.GetRawTransaction(ctx, txid)
}

// GetTransactionVerbose issues `blockchain.transaction.get` with verbose=true.
func (n *Network) GetTransactionVerbose(ctx context.Context, txid string) (*VerboseTx, error) {
	node, err := n.getNode()
	if err != nil {
		return nil, err
	}
	return node.GetTransactionVerbose(ctx, txid)
}

// Broadcast issues `blockchain.transaction.broadcast`.
func (n *Network) Broadcast(ctx context.Context, rawTxHex string) (string, error) {
	node, err := n.getNode()
	if err != nil {
		return "", err
	}
	return node.Broadcast(ctx, rawTxHex)
}

// EstimateFee returns satoshis per kilobyte for confTarget confirmations.
func (n *Network) EstimateFee(ctx context.Context, confTarget int32) (int64, error) {
	node, err := n.getNode()
	if err != nil {
		return 0, err
	}
	return node.EstimateFee(ctx, confTarget)
}

// ----------------------------------------------------------------------------
// Internals
// ----------------------------------------------------------------------------

// errNotStarted is returned when API calls precede a successful Start.
var errNotStarted = errors.New("electrum: network not started")

func (n *Network) getNode() (*Node, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !n.started || n.node == nil {
		return nil, errNotStarted
	}
	return n.node, nil
}

// syncHeaders pulls headers from `Tip()+1` until the server reports a short
// batch (i.e. we've caught up). It runs as a single-threaded blocking call
// during Start. tipHint is the server-reported best height learned from the
// initial subscribe; it lets AppendBatch decide which trailing window of
// headers gets full per-header PoW.
func (n *Network) syncHeaders(ctx context.Context, node *Node, tipHint int32) error {
	for {
		nextHeight := max(n.headers.Tip()+1, n.cfg.Chain.StartHeight)
		batch, err := node.BlockHeaders(ctx, nextHeight, n.cfg.HeaderBatchSize)
		if err != nil {
			return err
		}
		if batch.Count == 0 {
			break
		}
		raw, err := hex.DecodeString(batch.HexConcat)
		if err != nil {
			return fmt.Errorf("electrum: decode header batch: %w", err)
		}
		if _, err := n.headers.AppendBatch(nextHeight, raw, tipHint); err != nil {
			return err
		}
		log.Debugf("electrum: appended %d headers at height %d (tip now %d)",
			batch.Count, nextHeight, n.headers.Tip())
		if batch.Count < n.cfg.HeaderBatchSize {
			break
		}
		// Brief breather between back-to-back batches. We're a polite
		// client; the public electrumx servers throttle aggressive
		// pullers and this keeps us comfortably under any soft limit.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if n.headers.Tip() < n.cfg.Chain.StartHeight {
		// The server returned an empty first batch from StartHeight,
		// leaving us with no headers at all. Marking synced here
		// would leave Tip() at -1 and any consumer relying on
		// GetBestBlock would see a stuck wallet that never errors.
		// Surface the failure so Start fails loudly instead.
		return fmt.Errorf("electrum: server returned no headers from height %d",
			n.cfg.Chain.StartHeight)
	}
	n.headers.SetSynced(true)
	log.Infof("electrum: header sync complete, tip at %d", n.headers.Tip())
	return nil
}

// headersLoop consumes tip notifications from the node and applies them to
// the headers store, fanning out structured TipChange events to subscribers.
func (n *Network) headersLoop(initialTip *HeadersTip) {
	defer n.wg.Done()

	// lastDeliveredTip is the highest height we've published in a
	// Connected entry. The first iteration uses the headers-store tip
	// after we apply the subscription's initial reply, then walks forward
	// from there as we receive notifications.
	lastDeliveredTip := n.headers.Tip()

	// Apply the subscription's initial reply, then emit an initial event
	// so subscribers always see at least one BlockConnected for the
	// starting tip — even if no replay was needed.
	n.applyTipNotification(initialTip, &lastDeliveredTip)
	if tip := n.headers.Tip(); tip >= n.cfg.Chain.StartHeight && tip != lastDeliveredTip-1 {
		// Synthesize the initial Connected event covering the
		// already-synced tip. We pass lastDeliveredTip = tip - 1 so
		// the helper emits exactly one Connected[tip].
		n.deliverConnectRange(tip-1, tip)
		lastDeliveredTip = tip
	}

	node := n.peekNode()
	if node == nil {
		return
	}
	ch := node.HeadersNotifyChan()
	for {
		select {
		case <-n.ctx.Done():
			return
		case t, ok := <-ch:
			if !ok {
				return
			}
			n.applyTipNotification(t, &lastDeliveredTip)
		}
	}
}

// deliverConnectRange emits a TipChange whose Connected slice is the
// per-block range (lastDelivered, newTip]. No-op if newTip <= lastDelivered.
func (n *Network) deliverConnectRange(lastDelivered, newTip int32) {
	if newTip <= lastDelivered {
		return
	}
	connected := make([]int32, 0, newTip-lastDelivered)
	for h := lastDelivered + 1; h <= newTip; h++ {
		connected = append(connected, h)
	}
	n.deliverTipChange(&TipChange{Connected: connected})
}

// peekNode returns the current node without an error path, for goroutines
// that have already verified Start succeeded and just want a snapshot.
func (n *Network) peekNode() *Node {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.node
}

// applyTipNotification processes one tip notification from the server and
// publishes a structured TipChange covering whatever happened. lastDelivered
// is the highest height we've already published in a Connected entry; we
// update it to the new tip after a successful apply so the next iteration
// emits only the incremental range.
func (n *Network) applyTipNotification(t *HeadersTip, lastDelivered *int32) {
	if t == nil {
		return
	}
	if t.Height < n.cfg.Chain.StartHeight {
		log.Warnf("electrum: server reported header below start height (%d < %d); ignoring",
			t.Height, n.cfg.Chain.StartHeight)
		return
	}
	ourTip := n.headers.Tip()
	switch {
	case t.Height <= ourTip:
		// Server is reporting a height we already have. Could be a
		// duplicate or a rewind on the server side that hasn't yet
		// touched us. Nothing to do.
		return
	case t.Height == ourTip+1:
		disc, err := n.connectOneTip(t.Hex)
		if err != nil {
			log.Debugf("electrum: connect tip at %d: %v", t.Height, err)
			return
		}
		if len(disc) > 0 {
			// Rewind triggered. Publish the disconnects so
			// consumers can roll back; the next server
			// notification will bring us forward on the new
			// chain. Update lastDelivered to the new (lower)
			// tip so the eventual replacement emits only the
			// connect range past it.
			newTip := n.headers.Tip()
			n.deliverTipChange(&TipChange{Disconnected: disc})
			*lastDelivered = newTip
			return
		}
	default:
		// Jumped ahead; catch up in chunks.
		if err := n.catchUp(t.Height); err != nil {
			log.Debugf("electrum: catch up to %d: %v", t.Height, err)
			return
		}
	}
	newTip := n.headers.Tip()
	if newTip > *lastDelivered {
		n.deliverConnectRange(*lastDelivered, newTip)
		*lastDelivered = newTip
	}
}

// connectOneTip parses a single header from a tip notification, appends it,
// and reports any disconnected headers that resulted from a reorg rewind.
func (n *Network) connectOneTip(hexHdr string) ([]DisconnectedBlock, error) {
	raw, err := hex.DecodeString(hexHdr)
	if err != nil {
		return nil, err
	}
	if len(raw) != n.cfg.Chain.HeaderSize {
		return nil, fmt.Errorf("unexpected header length %d", len(raw))
	}
	hdr := new(wire.BlockHeader)
	if err := hdr.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	_, disc, err := n.headers.AppendOne(hdr)
	if err != nil {
		return nil, err
	}
	return disc, nil
}

func (n *Network) catchUp(targetHeight int32) error {
	node, err := n.getNode()
	if err != nil {
		return err
	}
	for n.headers.Tip() < targetHeight {
		nextHeight := n.headers.Tip() + 1
		want := min(int(targetHeight-n.headers.Tip()), n.cfg.HeaderBatchSize)
		batch, err := node.BlockHeaders(n.ctx, nextHeight, want)
		if err != nil {
			return err
		}
		if batch.Count == 0 {
			break
		}
		raw, err := hex.DecodeString(batch.HexConcat)
		if err != nil {
			return err
		}
		// targetHeight is the latest server-reported tip we're
		// catching up to — pass it as tipHint so AppendBatch applies
		// per-header PoW only to headers within the trailing
		// recentPoWWindow.
		if _, err := n.headers.AppendBatch(nextHeight, raw, targetHeight); err != nil {
			return err
		}
	}
	return nil
}

// deliverTipChange publishes a TipChange event. Drops on a full channel
// rather than blocking the headers loop — consumers that fall behind get a
// warn-level log they can monitor.
func (n *Network) deliverTipChange(tc *TipChange) {
	if tc == nil {
		return
	}
	select {
	case n.tipChangeChan <- tc:
	case <-n.ctx.Done():
	default:
		log.Warnf("electrum: tip-change channel full, dropping event (connected=%d disconnected=%d)",
			len(tc.Connected), len(tc.Disconnected))
	}
}

// scripthashLoop fans out scripthash notifications from the node to the
// per-network channel.
func (n *Network) scripthashLoop() {
	defer n.wg.Done()
	node := n.peekNode()
	if node == nil {
		return
	}
	ch := node.ScripthashNotifyChan()
	for {
		select {
		case <-n.ctx.Done():
			return
		case st, ok := <-ch:
			if !ok {
				return
			}
			select {
			case n.scripthashCh <- st:
			case <-n.ctx.Done():
				return
			default:
				log.Warnf("electrum: scripthash channel full, dropping notify for %s",
					st.Scripthash)
			}
		}
	}
}

// watchNode waits for the node to drop and then tears the network down. A
// later phase will replace this with peer rotation.
//
// The final-shutdown bookkeeping runs in a child goroutine because
// `n.wg.Wait()` would otherwise deadlock — watchNode itself is part of the
// waitgroup. Returning lets our own `defer n.wg.Done()` fire so the wait can
// complete; the child then closes the public channels and signals done.
func (n *Network) watchNode(node *Node) {
	defer n.wg.Done()
	select {
	case <-node.Done():
		log.Warnf("electrum: trusted server %s disconnected; stopping network", node.Addr())
		n.Stop()
	case <-n.ctx.Done():
	}
	go func() {
		n.wg.Wait()
		n.doneClosed.Do(func() {
			close(n.tipChangeChan)
			close(n.scripthashCh)
			close(n.done)
		})
	}()
}
