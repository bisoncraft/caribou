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

	tipChangeChan chan int32
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
		tipChangeChan: make(chan int32, notifyBufSize),
		scripthashCh:  make(chan *ScripthashStatus, notifyBufSize),
	}, nil
}

// Headers returns the underlying header store. Useful for chain.Interface
// implementations that need direct read access.
func (n *Network) Headers() *Headers { return n.headers }

// Start connects to the trusted server, syncs headers, and begins listening
// for tip and scripthash notifications. The provided ctx is the network's
// lifetime: cancelling ctx is equivalent to calling Stop.
func (n *Network) Start(parent context.Context) error {
	n.mu.Lock()
	if n.started {
		n.mu.Unlock()
		return errors.New("electrum: network already started")
	}
	n.ctx, n.cancel = context.WithCancel(parent)
	n.started = true
	n.mu.Unlock()

	node, err := NewNode(n.ctx, n.cfg.TrustedServer, n.cfg.TorProxy,
		n.cfg.Chain.Params.GenesisHash.String())
	if err != nil {
		n.cancel()
		return err
	}
	n.mu.Lock()
	n.node = node
	n.mu.Unlock()

	if err := n.syncHeaders(n.ctx, node); err != nil {
		node.Close()
		n.cancel()
		return fmt.Errorf("electrum: initial header sync: %w", err)
	}

	tip, err := node.SubscribeHeaders(n.ctx)
	if err != nil {
		node.Close()
		n.cancel()
		return fmt.Errorf("electrum: subscribe headers: %w", err)
	}
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

// Stop initiates teardown. Done closes once teardown finishes.
func (n *Network) Stop() {
	n.mu.Lock()
	if !n.started {
		n.mu.Unlock()
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

// TipChangeChan returns a channel that receives the new tip height every time
// the chain advances. The channel is closed when the network shuts down.
func (n *Network) TipChangeChan() <-chan int32 { return n.tipChangeChan }

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
// during Start.
func (n *Network) syncHeaders(ctx context.Context, node *Node) error {
	for {
		nextHeight := n.headers.Tip() + 1
		if nextHeight < n.cfg.Chain.StartHeight {
			nextHeight = n.cfg.Chain.StartHeight
		}
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
		if _, err := n.headers.AppendBatch(nextHeight, raw); err != nil {
			return err
		}
		log.Debugf("electrum: appended %d headers at height %d (tip now %d)",
			batch.Count, nextHeight, n.headers.Tip())
		if batch.Count < n.cfg.HeaderBatchSize {
			break
		}
		// Brief breather to keep server session cost low.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	n.headers.SetSynced(true)
	log.Infof("electrum: header sync complete, tip at %d", n.headers.Tip())
	return nil
}

// headersLoop consumes tip notifications from the node and applies them to
// the headers store, fanning out tip-change notifications to subscribers.
func (n *Network) headersLoop(initialTip *HeadersTip) {
	defer n.wg.Done()

	// Apply the subscription's initial reply as if it were a notification.
	n.applyTipNotification(initialTip)

	ch := n.cfg.TrustedServerNodeHeadersChan(n.getNodeUnsafe())
	for {
		select {
		case <-n.ctx.Done():
			return
		case t, ok := <-ch:
			if !ok {
				return
			}
			n.applyTipNotification(t)
		}
	}
}

// TrustedServerNodeHeadersChan is a helper that returns the active node's
// headers channel, isolated so we can rotate the underlying node when we add
// failover.
func (cfg *Config) TrustedServerNodeHeadersChan(node *Node) <-chan *HeadersTip {
	if node == nil {
		// closed channel so callers exit immediately
		ch := make(chan *HeadersTip)
		close(ch)
		return ch
	}
	return node.HeadersNotifyChan()
}

func (n *Network) getNodeUnsafe() *Node {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.node
}

func (n *Network) applyTipNotification(t *HeadersTip) {
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
		// Already have it; nothing to do.
		return
	case t.Height == ourTip+1:
		if err := n.connectOneTip(t.Hex); err != nil {
			log.Debugf("electrum: connect tip at %d: %v", t.Height, err)
			return
		}
	default:
		// Jumped ahead; catch up in chunks.
		if err := n.catchUp(t.Height); err != nil {
			log.Debugf("electrum: catch up to %d: %v", t.Height, err)
			return
		}
	}
	n.deliverTipChange(n.headers.Tip())
}

func (n *Network) connectOneTip(hexHdr string) error {
	raw, err := hex.DecodeString(hexHdr)
	if err != nil {
		return err
	}
	if len(raw) != n.cfg.Chain.HeaderSize {
		return fmt.Errorf("unexpected header length %d", len(raw))
	}
	hdr := new(wire.BlockHeader)
	if err := hdr.Deserialize(bytes.NewReader(raw)); err != nil {
		return err
	}
	ok, err := n.headers.AppendOne(hdr)
	if err != nil {
		return err
	}
	if !ok {
		// Rewind triggered; next notification will resync.
		return nil
	}
	return nil
}

func (n *Network) catchUp(targetHeight int32) error {
	node, err := n.getNode()
	if err != nil {
		return err
	}
	for n.headers.Tip() < targetHeight {
		nextHeight := n.headers.Tip() + 1
		want := int(targetHeight - n.headers.Tip())
		if want > n.cfg.HeaderBatchSize {
			want = n.cfg.HeaderBatchSize
		}
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
		if _, err := n.headers.AppendBatch(nextHeight, raw); err != nil {
			return err
		}
	}
	return nil
}

func (n *Network) deliverTipChange(tip int32) {
	select {
	case n.tipChangeChan <- tip:
	case <-n.ctx.Done():
	default:
		log.Warnf("electrum: tip-change channel full, dropping notify for height %d", tip)
	}
}

// scripthashLoop fans out scripthash notifications from the node to the
// per-network channel.
func (n *Network) scripthashLoop() {
	defer n.wg.Done()
	node := n.getNodeUnsafe()
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
func (n *Network) watchNode(node *Node) {
	defer n.wg.Done()
	select {
	case <-node.Done():
		log.Warnf("electrum: trusted server %s disconnected; stopping network", node.Addr())
		n.Stop()
	case <-n.ctx.Done():
	}
	// Wait for the other goroutines to settle then signal final shutdown.
	go func() {
		n.wg.Wait()
		n.doneClosed.Do(func() {
			close(n.tipChangeChan)
			close(n.scripthashCh)
			close(n.done)
		})
	}()
}
