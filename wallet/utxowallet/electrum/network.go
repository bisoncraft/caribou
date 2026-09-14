// Copyright (c) 2024 The Caribou Developers
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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// Config configures a Network.
type Config struct {
	// Chain identifies the chain and provides chain parameters.
	Chain *ChainConfig

	// TrustedServer is the preferred server to dial. At least one of
	// TrustedServer/Servers is required.
	TrustedServer *ServerAddr

	// Servers are additional candidate servers, in preference order. Start
	// elects one connection: candidates are tried in health-scored order
	// (recent failures deprioritized; see serverpool.go) until a dial and
	// handshake succeed, and a server whose connection later fails is
	// demoted so the next (re)connect rotates instead of retrying it.
	Servers []*ServerAddr

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
	// launched records that Start got far enough to spawn the background
	// goroutines (including watchNode). Once set it stays set. It lets the
	// !started Stop path tell "never started, no goroutines, safe to close
	// the channels directly" from "started then stopped, goroutines still
	// draining, let watchNode's wg.Wait()-gated close own it" — closing a
	// sender channel out from under a live headersLoop/scripthashLoop send
	// would panic.
	launched bool

	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	doneClosed sync.Once
	done       chan struct{}

	tipChangeChan chan *TipChange
	scripthashCh  chan *ScripthashStatus

	// serverTipMu guards serverTip, which tracks the highest block height
	// reported by the electrumx server (from SubscribeHeaders at start and
	// from tip-change notifications thereafter). It is used by TargetTip to
	// report meaningful sync progress before the local store catches up.
	serverTipMu sync.RWMutex
	serverTip   int32

	// obsMtx guards observers, the live observer connections maintained by
	// runObservers (observers.go), and obsWalletAddr, the wallet server's
	// address as visible to observer bookkeeping. The wallet server must
	// never be its own observer; obsWalletAddr is written in the same
	// obsMtx section that purges a conflicting observer (attachNode) and
	// read in the same obsMtx section that inserts one (fillObservers), so
	// the pair cannot interleave into a self-observer no matter how the
	// mu-guarded publication and the insert race. nil outside the observer
	// loop's lifetime.
	obsMtx        sync.Mutex
	observers     map[string]*observer
	obsWalletAddr string

	// pool is the persistent, tiered server database (serverpool.go).
	pool *serverPool

	// subsMtx guards subs, the scripthashes with live subscriptions. The
	// Network brokers every subscribe/unsubscribe, so this registry is the
	// authoritative set a rotation must replay onto a successor node —
	// server-side session state dies with the old connection.
	subsMtx sync.Mutex
	subs    map[string]struct{}

	// rot serializes rotations (rotation.go).
	rot rotationGuard

	// loops is the goroutine group bound to the current wallet node
	// (scripthashLoop, watchNode, and headersLoop — which in Start's group
	// is preceded, in the same member, by the initial header sync).
	// Rotation joins the old group before attaching a successor so two
	// groups never interleave header appends or status forwards. Guarded
	// by mu, and always published together with node, with every member's
	// count already reserved: a group observable before its full count is
	// raised can transiently drain to zero under a joiner's Wait, making
	// the remaining Add an add-from-zero during Wait (sync.WaitGroup
	// misuse). Start reserves then spawns; attachNode spawns under rotMtx
	// before any joiner can run.
	loops *loopGroup
}

// loopGroup tracks the goroutines bound to one wallet node. join blocks
// until every loop in the group has exited — they do so when their node's
// notify channels close (node death) or the network context ends; Start's
// initial-sync member instead exits when syncHeaders errors on the dead
// connection.
type loopGroup struct {
	node *Node
	wg   sync.WaitGroup
}

func (g *loopGroup) join() { g.wg.Wait() }

// spawnLoop runs fn tracked by both the group (rotation joins it) and the
// network WaitGroup (WaitForShutdown waits for it).
func (n *Network) spawnLoop(g *loopGroup, fn func()) {
	g.wg.Add(1)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer g.wg.Done()
		fn()
	}()
}

// NewNetwork constructs a Network ready to be Started.
func NewNetwork(cfg *Config) (*Network, error) {
	if cfg == nil {
		return nil, errors.New("electrum: nil network config")
	}
	if cfg.Chain == nil {
		return nil, errors.New("electrum: network config missing Chain")
	}
	if cfg.TrustedServer == nil && len(cfg.Servers) == 0 {
		return nil, errors.New("electrum: network config has no servers")
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
	n := &Network{
		cfg:           cfg,
		headers:       h,
		done:          make(chan struct{}),
		tipChangeChan: make(chan *TipChange, notifyBufSize),
		scripthashCh:  make(chan *ScripthashStatus, notifyBufSize),
	}
	// The persistent server pool lives beside the header store (created by
	// NewHeaders above): shipped seeds plus discovered peers with durable
	// reputation (multi-server phase 4).
	n.pool = newServerPool(
		filepath.Join(cfg.DataDir, "electrum", string(cfg.Chain.Chain)),
		n.candidates())
	return n, nil
}

// Headers returns the underlying header store. Useful for chain.Backend
// implementations that need direct read access.
func (n *Network) Headers() *Headers { return n.headers }

// candidates returns the deduplicated configured server pool.
func (n *Network) candidates() []*ServerAddr {
	out := make([]*ServerAddr, 0, len(n.cfg.Servers)+1)
	seen := make(map[string]bool)
	for _, sv := range append([]*ServerAddr{n.cfg.TrustedServer}, n.cfg.Servers...) {
		if sv == nil || seen[sv.Addr] {
			continue
		}
		seen[sv.Addr] = true
		out = append(out, sv)
	}
	return out
}

// Start connects to the trusted server, subscribes to header notifications,
// and returns promptly. The initial header chain download (which can take
// minutes on a cold start) runs in a background goroutine so the caller is
// not blocked. Synced() / Tip() report progress; TargetTip() reports the
// server's current best height so callers can show meaningful progress.
//
// What happens synchronously (fast, verifies server reachability):
//   - NewNode: dial + version handshake with the electrumx server.
//   - SubscribeHeaders: one round trip that confirms the connection and returns
//     the server's current chain tip height.
//
// Any error from those two steps is returned directly, so a genuinely
// unreachable or incompatible server still fails Start quickly rather than
// silently.
//
// What happens in the background (after Start returns):
//   - syncHeaders: downloads the header chain batch-by-batch.
//   - headersLoop: processes ongoing tip-change notifications once sync is done.
//
// Sync failure after Start returns is handled the same way a node-drop is: via
// Stop() / watchNode, NOT via the failed() closure (which is only used in the
// synchronous phase, before launched is set, so it cannot race the goroutines).
//
// On any synchronous error Start unwinds — cancels the internal context,
// closes the partially-initialised node (if any), and closes the done channel
// so a caller doing `defer net.WaitForShutdown()` doesn't deadlock.
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

	// failed is only called during the synchronous phase (before launched is
	// set). Once launched is true, teardown ownership transfers to watchNode,
	// and callers must use Stop() instead to avoid double-closing channels.
	failed := func(err error) error {
		if n.cancel != nil {
			n.cancel()
		}
		n.doneClosed.Do(func() {
			// Persist failures recorded by the dial loop that are still
			// waiting on the pool's coalescing delay.
			n.pool.flush()
			close(n.tipChangeChan)
			close(n.scripthashCh)
			close(n.done)
		})
		return err
	}

	// Elect one connection from the candidate pool: dial in health-scored
	// order until a handshake and header subscription succeed. Failures are
	// recorded so subsequent elections (including full wallet reconnects,
	// which rebuild this Network) rotate away from dead servers.
	//
	// The subscribe happens FIRST so syncHeaders knows the server-reported
	// tip. That height becomes the tipHint for AppendBatch: headers within
	// the trailing recentPoWWindow get per-header PoW, older headers are
	// validated only by checkpoint match + retarget structure (much faster
	// than per-header scrypt over millions of LTC blocks). Tip-change
	// notifications that arrive during the background sync are buffered by
	// the server's headersNotify channel; headersLoop drains them after
	// sync completes.
	candidates := n.pool.walletCandidates()
	if len(candidates) == 0 {
		// Every shipped server is quarantined (divergence audits persist
		// quarantine for 24 hours, and the discovered fallback opens only
		// on recorded failures, not quarantine). tryElect and the degraded
		// loop tolerate an empty election; Start must too, or the dial
		// loop below leaves tip nil and the publish dereferences it.
		return failed(errors.New(
			"electrum: no eligible wallet servers (all quarantined)"))
	}
	var node *Node
	var tip *HeadersTip
	var dialErr error
	for _, sv := range candidates {
		node, dialErr = NewNode(n.ctx, sv, n.cfg.TorProxy,
			n.cfg.Chain.Params.GenesisHash.String())
		if dialErr != nil {
			n.pool.recordFailure(sv.Addr)
			log.Warnf("electrum: server %s failed: %v", sv.Addr, dialErr)
			continue
		}
		tip, dialErr = node.SubscribeHeaders(n.ctx)
		if dialErr != nil {
			node.Close()
			n.pool.recordFailure(sv.Addr)
			log.Warnf("electrum: server %s failed: subscribe headers: %v", sv.Addr, dialErr)
			continue
		}
		n.pool.recordSuccess(sv.Addr)
		break
	}
	if dialErr != nil {
		return failed(fmt.Errorf("electrum: all %d candidate servers failed, last: %w",
			len(candidates), dialErr))
	}

	// Record the server's reported tip so TargetTip() can report meaningful
	// progress from the moment Start returns.
	n.serverTipMu.Lock()
	n.serverTip = tip.Height
	n.serverTipMu.Unlock()

	// Commit: publish the node and its loop group together, with every
	// member's group count RESERVED up front. From here on, watchNode owns
	// the channel-close — never call failed() after this point.
	//
	// The reservation and the single publication section both matter. A
	// node that dies instantly fires watchNode's rotate the moment the
	// member spawns; rotate reads (n.node, n.loops) and joins the group.
	// Publishing the node before the group would let that rotation observe
	// a nil group and skip the join — proceeding while this node's loops
	// spawn around it. Publishing the group with fewer than its final
	// member count would let the counter transiently drain to zero with
	// the join in Wait, making the remaining spawn's Add an add-from-zero
	// concurrent with Wait, which sync.WaitGroup forbids. attachNode needs
	// neither precaution: its spawns complete under rotMtx before any
	// joiner can observe the group.
	group := &loopGroup{node: node}
	group.wg.Add(3)
	n.mu.Lock()
	n.node = node
	n.started = true
	n.launched = true
	n.loops = group
	n.mu.Unlock()
	n.spawnReserved(group, func() { n.scripthashLoop(node) })
	n.spawnReserved(group, func() { n.watchNode(node) })

	// Seed the observer-side wallet address before the observer loop can
	// run its first fill (see obsWalletAddr).
	n.obsMtx.Lock()
	n.obsWalletAddr = node.Addr().Addr
	n.obsMtx.Unlock()

	// Observer connections (multi-server phase 2): header-only
	// subscriptions on the remaining candidates, watching for a lagging or
	// lying wallet server. Only meaningful when there is anyone else to
	// ask.
	if len(n.candidates()) > 1 {
		n.wg.Add(1)
		go n.runObservers()
	}

	// The background sync runs as a member of the node's loop group, with
	// headersLoop running inline in the same member once sync succeeds
	// (headersLoop must not race the initial batch-writes to the header
	// store). Membership means a rotation's join() waits out a
	// still-running initial sync; that wait is bounded because rotate
	// closes the node before joining and BlockHeaders fails fast on a dead
	// connection. On sync failure this member calls Stop() — the same
	// teardown path a node-drop uses — but only while this node still
	// holds the wallet slot: a rotation may already have replaced a node
	// that died mid-sync, and tearing the network down then would kill a
	// healthy successor. Stop itself never waits on the group, so a member
	// may call it; failed() is off-limits here because launched is already
	// true and it would double-close the channels.
	n.spawnReserved(group, func() {
		if err := n.syncHeaders(n.ctx, node, tip.Height); err != nil {
			if n.ctx.Err() == nil {
				// Context is still live, so this is a real sync error
				// (not a graceful shutdown) — bad or hostile server.
				// Demote it so the reconnect rotates.
				n.pool.recordFailure(node.Addr().Addr)
				log.Errorf("electrum: initial header sync failed: %v", err)
			}
			n.mu.Lock()
			current := n.node
			n.mu.Unlock()
			if current == node {
				n.Stop()
			}
			return
		}
		n.headersLoop(node, tip)
	})

	return nil
}

// spawnReserved runs fn as a member of a group whose counter was reserved at
// construction (see Start); it adds only the network-level tracking.
func (n *Network) spawnReserved(g *loopGroup, fn func()) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer g.wg.Done()
		fn()
	}()
}

// Stop initiates teardown. Done closes once teardown finishes. Safe to call
// on a network that never successfully started; in that case Stop just makes
// sure WaitForShutdown unblocks.
func (n *Network) Stop() {
	n.mu.Lock()
	if !n.started {
		launched := n.launched
		n.mu.Unlock()
		// If the goroutines were ever launched, watchNode's child
		// goroutine closes the channels after wg.Wait() — doing it here
		// could close a channel out from under a live sender and panic.
		// Only the genuinely-never-started case (no goroutines, no
		// watchNode) closes directly; there are no senders to race.
		if !launched {
			n.doneClosed.Do(func() {
				n.pool.flush()
				close(n.tipChangeChan)
				close(n.scripthashCh)
				close(n.done)
			})
		}
		return
	}
	cancel := n.cancel
	node := n.node
	n.started = false
	n.mu.Unlock()

	// Cancel BEFORE closing the node. Senders (headersLoop/scripthashLoop)
	// park on a blocking channel send; cancelling first makes n.ctx.Done()
	// ready so they exit via that arm rather than racing node.Done() ->
	// watchNode -> teardown. It also steers watchNode's own select to its
	// ctx.Done() case, so it doesn't re-enter Stop's !started path.
	if cancel != nil {
		cancel()
	}
	if node != nil {
		node.Close()
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

// StartHeight returns the lowest header height the store syncs from — the
// checkpoint anchor on mainnet/testnet, 0 on regtest. Headers below it do
// not exist locally.
func (n *Network) StartHeight() int32 { return n.headers.StartHeight() }

// Synced reports whether the initial header sync has completed.
func (n *Network) Synced() bool { return n.headers.Synced() }

// TargetTip returns the highest block height reported by the electrumx server.
// It is set from the SubscribeHeaders response in Start and updated as new
// tip-change notifications arrive, so it reflects the server's live best
// height rather than the locally-synced tip. Returns 0 if the server height
// is not yet known (i.e. before Start returns).
//
// Use Tip() for the locally-synced height and TargetTip() for the server's
// reported best height to compute meaningful sync progress.
func (n *Network) TargetTip() int32 {
	n.serverTipMu.RLock()
	defer n.serverTipMu.RUnlock()
	return n.serverTip
}

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
	st, err := node.SubscribeScripthash(ctx, scripthash)
	if err != nil {
		return nil, err
	}
	n.subsMtx.Lock()
	if n.subs == nil {
		n.subs = make(map[string]struct{})
	}
	n.subs[scripthash] = struct{}{}
	n.subsMtx.Unlock()
	return st, nil
}

// replaySubscriptions re-establishes every registered scripthash
// subscription on node — a rotation successor holds none — and forwards each
// returned status through the normal notification channel. The consumer's
// status/history diff then delivers exactly the activity missed while blind:
// an unchanged status is a hash commitment that nothing happened, a changed
// one triggers a full-history diff. Returns the first error so the caller
// can treat the successor as failed.
func (n *Network) replaySubscriptions(ctx context.Context, node *Node) error {
	n.subsMtx.Lock()
	subs := make([]string, 0, len(n.subs))
	for sh := range n.subs {
		subs = append(subs, sh)
	}
	n.subsMtx.Unlock()
	for _, sh := range subs {
		st, err := node.SubscribeScripthash(ctx, sh)
		if err != nil {
			return fmt.Errorf("electrum: replay subscribe %s: %w", sh, err)
		}
		if st.Status == "" {
			continue // no history; nothing to reconcile
		}
		select {
		case n.scripthashCh <- st:
		case <-n.ctx.Done():
			return n.ctx.Err()
		}
	}
	if len(subs) > 0 {
		log.Infof("electrum: replayed %d scripthash subscriptions", len(subs))
	}
	return nil
}

// GetMerkle fetches the merkle inclusion proof for a confirmed transaction.
func (n *Network) GetMerkle(ctx context.Context, txid string, height int32) (*MerkleProof, error) {
	node, err := n.getNode()
	if err != nil {
		return nil, err
	}
	return node.GetMerkle(ctx, txid, height)
}

// UnsubscribeScripthash undoes a previous subscription.
func (n *Network) UnsubscribeScripthash(ctx context.Context, scripthash string) {
	n.subsMtx.Lock()
	delete(n.subs, scripthash)
	n.subsMtx.Unlock()
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

	// Broadcast fan-out (multi-server phase 3): the wallet server's answer
	// is authoritative, but a lone server can accept a transaction and
	// silently never relay it, so the observers get it too — censorship
	// then requires every connected operator to collude. The transaction is
	// being published to the whole network, so fanning it out costs no
	// privacy.
	observers := n.observerNodes()
	txid, err := node.Broadcast(ctx, rawTxHex)
	if err == nil {
		for _, on := range observers {
			// Deliberately NOT wg-tracked: Broadcast can race Stop, and
			// wg.Add concurrent with the teardown path's wg.Wait is
			// WaitGroup misuse. These goroutines touch no Network
			// channels and their requests die with n.ctx/node.Close.
			go func(on *Node) {
				if _, oerr := on.Broadcast(n.ctx, rawTxHex); oerr != nil &&
					!broadcastDuplicate(oerr) {
					log.Debugf("electrum: observer %s broadcast fan-out: %v",
						on.Addr().Addr, oerr)
				}
			}(on)
		}
		return txid, nil
	}
	// A duplicate response means the network already has the transaction —
	// success for (re)broadcast purposes.
	if broadcastDuplicate(err) {
		log.Debugf("electrum: broadcast: %s reports tx already known", node.Addr().Addr)
		return txidFromRaw(n.cfg.Chain.Chain, rawTxHex)
	}
	// The wallet server refused. Try the observers synchronously — an
	// accept here both publishes the transaction and flags the wallet
	// server's refusal as suspect.
	log.Warnf("electrum: wallet server %s rejected broadcast (%v); trying %d observers",
		node.Addr().Addr, err, len(observers))
	for _, on := range observers {
		otxid, oerr := on.Broadcast(ctx, rawTxHex)
		if oerr == nil {
			log.Warnf("electrum: observer %s accepted a broadcast the wallet "+
				"server rejected", on.Addr().Addr)
			return otxid, nil
		}
		if broadcastDuplicate(oerr) {
			return txidFromRaw(n.cfg.Chain.Chain, rawTxHex)
		}
	}
	// Unanimous rejection: surface the wallet server's error.
	return "", err
}

// broadcastDuplicate reports whether a broadcast error means the transaction
// is already known to the server — success for our purposes. Server
// implementations word this differently (electrumx, Fulcrum, and the daemons
// behind them), so this is substring matching over the known variants rather
// than error codes.
func broadcastDuplicate(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"already known",
		"already in block chain",
		"already in the mempool",
		"already in mempool",
		"txn-already",
		"duplicate transaction",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// txidFromRaw computes the transaction id of a raw hex transaction, used
// when every server reported the transaction as already known and none
// returned an id.
func txidFromRaw(chain bisonwire.Chain, rawTxHex string) (string, error) {
	raw, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return "", fmt.Errorf("electrum: undecodable raw tx: %w", err)
	}
	tx := bisonwire.Tx{Chain: string(chain)}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return "", fmt.Errorf("electrum: undecodable raw tx: %w", err)
	}
	return tx.TxHash().String(), nil
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
// during Start and during attachNode's rotation catch-up. tipHint is the
// server-reported best height learned from the initial subscribe; it lets
// AppendBatch decide which trailing window of headers gets full per-header
// PoW.
//
// When the server's chain diverges below the stored tip — a reorg landed
// while no node was attached: a successor after a fork, or a restart against
// a moved chain — the first batch fails to connect. That is recoverable, not
// fatal: rewind to the fork point (bounded like catchUpFrom) and retry,
// surfacing the disconnects so consumers roll back what the store dropped.
// Treating it as fatal made every candidate on the honest chain fail
// identically after an ordinary one-block reorg, leaving rotation degraded
// forever and Start unable to open the wallet until the header file was
// deleted by hand.
func (n *Network) syncHeaders(ctx context.Context, node *Node, tipHint int32) error {
	var rewound int32
	fetch := nodeFetcher(ctx, node)
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
			if !errors.Is(err, errHeaderDoesNotConnect) {
				return err
			}
			disc, depth, rerr := n.rewindToFork(fetch)
			if rerr != nil {
				return fmt.Errorf("electrum: reorg rewind: %w", rerr)
			}
			rewound += depth
			if len(disc) > 0 && !n.deliverTipChange(&TipChange{Disconnected: disc}) {
				return ctx.Err()
			}
			if rewound > maxReorgDepth {
				return fmt.Errorf("electrum: reorg deeper than %d blocks "+
					"near height %d; resync required: %w",
					maxReorgDepth, nextHeight, err)
			}
			continue
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
func (n *Network) headersLoop(node *Node, initialTip *HeadersTip) {
	// lastDeliveredTip is the highest height we've published in a
	// Connected entry. The first iteration uses the headers-store tip
	// after we apply the subscription's initial reply, then walks forward
	// from there as we receive notifications.
	tipBefore := n.headers.Tip()
	lastDeliveredTip := tipBefore

	// Apply the subscription's initial reply. If the server's initial tip
	// was one we already had, applyTipNotification delivers nothing and
	// leaves the tip unchanged; in that case we synthesize a single
	// Connected[tip] below so subscribers always get a baseline for the
	// already-synced tip. If applyTipNotification instead advanced or
	// rewound the tip, it already delivered the corresponding range, so we
	// must NOT synthesize again — the previous `tip != lastDeliveredTip-1`
	// guard always fired and double-delivered the tip after a startup
	// catch-up.
	n.applyTipNotification(initialTip, &lastDeliveredTip)
	if tip := n.headers.Tip(); tip == tipBefore && tip >= n.cfg.Chain.StartHeight {
		if n.deliverConnectRange(tip-1, tip) {
			lastDeliveredTip = tip
		}
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
// per-block range (lastDelivered, newTip]. No-op (and reports success) if
// newTip <= lastDelivered. Returns the delivery result so the caller only
// advances lastDelivered when the heights were actually published.
func (n *Network) deliverConnectRange(lastDelivered, newTip int32) bool {
	if newTip <= lastDelivered {
		return true
	}
	connected := make([]int32, 0, newTip-lastDelivered)
	for h := lastDelivered + 1; h <= newTip; h++ {
		connected = append(connected, h)
	}
	return n.deliverTipChange(&TipChange{Connected: connected})
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
	// Keep the server-reported target height current so TargetTip() always
	// reflects the latest best height the server has advertised. Track it
	// unconditionally rather than as a running max: on a server-side reorg the
	// tip can move strictly lower, and a monotonic max would pin TargetTip()
	// above the real tip, leaving sync progress stuck below 100% until a new
	// block climbed back past the stale value. (Notifications arrive in order on
	// the single server connection, and the start-height guard above already
	// rejects garbage, so there is no stale-low value to defend against.)
	n.serverTipMu.Lock()
	n.serverTip = t.Height
	n.serverTipMu.Unlock()
	ourTip := n.headers.Tip()
	if t.Height < ourTip {
		// The server is reporting a height below ours: a rewind on its
		// side that has not yet produced a replacement chain. Nothing
		// to do until it does.
		return
	}
	sameHeight := false
	if t.Height == ourTip {
		// A header at our own height with a different hash is the
		// common way a same-height tie resolves for the wallet: the
		// server switched sides. Rewind the losing block and refill;
		// otherwise the store would stay on the losing side until the
		// next block failed to link. The same hash is a duplicate.
		conflict, err := n.tipConflicts(t.Hex)
		if err != nil {
			log.Debugf("electrum: tip header at %d: %v", t.Height, err)
			return
		}
		if !conflict {
			return
		}
		sameHeight = true
	}
	reorg := false
	if t.Height == ourTip+1 {
		err := n.connectOneTip(t.Hex)
		if err != nil && !errors.Is(err, errHeaderDoesNotConnect) {
			log.Debugf("electrum: connect tip at %d: %v", t.Height, err)
			return
		}
		// A header that does not link means the server reorged at or
		// below our tip; the catch-up path locates the fork point and
		// refills from it.
		reorg = err != nil
	}
	if t.Height > ourTip+1 || reorg || sameHeight {
		// Jumped ahead, or reorged; catch up in chunks. catchUp also
		// recovers from a reorg discovered mid-catch-up (the server's
		// chain diverges below our tip), returning whatever it had to
		// disconnect — even when it ultimately errors after a partial
		// rewind.
		disc, err := n.catchUp(t.Height, sameHeight)
		if len(disc) > 0 {
			// Publish the disconnects BEFORE the error check. catchUp's
			// Rewind calls have already truncated the headers store (on
			// disk and in memory); returning without surfacing these
			// would leave downstream believing the orphaned blocks are
			// still connected, permanently diverging its view from the
			// store. disc is newest-first and contiguous, so its last
			// element is the lowest (fork) height; drop lastDelivered
			// just below it so the connect-range emission re-publishes
			// the new chain from the fork forward.
			n.deliverTipChange(&TipChange{Disconnected: disc})
			lowest := disc[len(disc)-1].Height
			if *lastDelivered >= lowest {
				*lastDelivered = lowest - 1
			}
		}
		if err != nil {
			// Couldn't fully resync this round (a reorg deeper than
			// maxReorgDepth, or a server serving an incoherent chain).
			// Any rollback is already surfaced above; the next
			// notification continues catching up from the lowered tip,
			// and a server that never delivers a usable chain falls to
			// the lag rotation.
			if n.ctx.Err() == nil {
				log.Warnf("electrum: catch up to %d: %v", t.Height, err)
			}
			return
		}
	}
	newTip := n.headers.Tip()
	if newTip > *lastDelivered {
		// Only advance lastDelivered if the Connected range was actually
		// published. If the channel was full and the event dropped,
		// leaving lastDelivered where it is lets the next notification
		// re-emit the missed heights — advancing past dropped data would
		// strand those BlockConnected events permanently (review #5).
		if n.deliverConnectRange(*lastDelivered, newTip) {
			*lastDelivered = newTip
		}
	}
}

// connectOneTip parses a single header from a tip notification and appends
// it. A header that does not link to the tip returns an error wrapping
// errHeaderDoesNotConnect; the store is untouched.
func (n *Network) connectOneTip(hexHdr string) error {
	hdr, err := n.parseTipHeader(hexHdr)
	if err != nil {
		return err
	}
	return n.headers.AppendOne(hdr)
}

// tipConflicts reports whether a notification header at the store's tip
// height differs from the stored tip.
func (n *Network) tipConflicts(hexHdr string) (bool, error) {
	hdr, err := n.parseTipHeader(hexHdr)
	if err != nil {
		return false, err
	}
	return n.headers.headerHash(hdr) != n.headers.TipHash(), nil
}

func (n *Network) parseTipHeader(hexHdr string) (*wire.BlockHeader, error) {
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
	return hdr, nil
}

// catchUp syncs the store to targetHeight from the wallet server. With
// rewindFirst set the server is known to conflict with the store at its
// tip, so the fork point is located and rewound before refilling; catchUpFrom
// alone would find nothing to fetch at an equal height.
func (n *Network) catchUp(targetHeight int32, rewindFirst bool) ([]DisconnectedBlock, error) {
	node, err := n.getNode()
	if err != nil {
		return nil, err
	}
	fetch := nodeFetcher(n.ctx, node)
	var disconnected []DisconnectedBlock
	if rewindFirst {
		disc, _, err := n.rewindToFork(fetch)
		if err != nil {
			return nil, err
		}
		disconnected = disc
	}
	more, err := n.catchUpFrom(targetHeight, fetch)
	return append(disconnected, more...), err
}

// headerFetcher returns up to `count` raw concatenated headers starting at
// `startHeight`, along with how many were returned (0 signals the server has
// no more). It's the seam catchUpFrom is tested through.
type headerFetcher func(startHeight, count int32) (raw []byte, n int, err error)

// nodeFetcher adapts a node's blockchain.block.headers call to headerFetcher.
func nodeFetcher(ctx context.Context, node *Node) headerFetcher {
	return func(startHeight, count int32) ([]byte, int, error) {
		batch, err := node.BlockHeaders(ctx, startHeight, int(count))
		if err != nil {
			return nil, 0, err
		}
		if batch.Count == 0 {
			return nil, 0, nil
		}
		raw, err := hex.DecodeString(batch.HexConcat)
		if err != nil {
			return nil, 0, err
		}
		return raw, batch.Count, nil
	}
}

// errForkNotFound is returned by rewindToFork when the server serves the
// store's own tip header yet its next header does not link to it. That is
// not a reorg, and callers treat it as a server fault rather than a rewind.
var errForkNotFound = errors.New("electrum: server agrees at the tip but its chain does not link")

// rewindToFork rewinds the store to the fork point between its chain and the
// chain fetch serves: the highest height, within reorgRewindDepth below the
// tip, at which the two agree. It returns the disconnected blocks
// (newest-first) and the depth rewound. When no header in the window matches
// the whole window is rewound; the caller's loop retries from the lowered
// tip and searches again, so the total is bounded by its maxReorgDepth
// budget rather than by this window. Called after a batch or tip header
// failed to link to the tip, so the fork lies at or below it.
//
// A blind fixed-depth rewind disconnected reorgRewindDepth blocks for every
// one-block reorg. Each disconnect rolls the wallet's transaction store back
// to that height, and nothing re-confirmed the innocent blocks' transactions
// afterwards.
func (n *Network) rewindToFork(fetch headerFetcher) ([]DisconnectedBlock, int32, error) {
	tip := n.headers.Tip()
	start := max(tip-reorgRewindDepth+1, n.headers.StartHeight())
	if start > tip {
		return nil, 0, fmt.Errorf("electrum: cannot rewind below start height %d (tip %d)",
			n.headers.StartHeight(), tip)
	}
	raw, count, err := fetch(start, tip-start+1)
	if err != nil {
		return nil, 0, fmt.Errorf("electrum: fork search fetch: %w", err)
	}
	depth := tip - start + 1 // no shared header in the window
	if count > 0 {
		theirs, err := n.headers.parseBatch(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("electrum: fork search batch: %w", err)
		}
		for i := len(theirs) - 1; i >= 0; i-- {
			height := start + int32(i)
			if height > tip {
				continue
			}
			ours := n.headers.ByHeight(height)
			if ours != nil && n.headers.headerHash(ours) == n.headers.headerHash(theirs[i]) {
				depth = tip - height
				break
			}
		}
	}
	if depth == 0 {
		// The server agrees with the store at its tip yet served a next
		// header that does not link to it: an inconsistent or flapping
		// server, not a reorg. Rewinding anything here would disconnect
		// a canonical block for every retry.
		return nil, 0, errForkNotFound
	}
	disc, err := n.headers.Rewind(depth)
	if err != nil {
		return nil, 0, err
	}
	log.Debugf("electrum: reorg: rewound %d headers to fork point %d", depth, tip-depth)
	return disc, depth, nil
}

// catchUpFrom is the chunked sync + reorg-recovery loop. It pulls headers via
// fetch and appends them until Tip() reaches targetHeight. If the server's
// chain has diverged below our tip, AppendBatch reports errHeaderDoesNotConnect
// and we rewind to the fork point (searching reorgRewindDepth at a time, up to
// maxReorgDepth total), then resume. The returned disconnected blocks
// (newest-first, accumulated across steps) let the caller surface
// BlockDisconnected events and reset its delivery cursor. A reorg deeper than
// maxReorgDepth is treated as unrecoverable and surfaced as an error.
//
// Note: the rewind budget is per-call. A persistently flapping server that
// serves a transient fork on each notification can therefore make us
// truncate-and-reappend a little on every call without any single call
// crossing maxReorgDepth. Each such call still publishes its disconnects and
// makes forward progress, so the chain stays consistent, but the churn isn't
// bounded across calls. A cross-call rewind budget would be the fuller fix if
// that ever shows up against a real peer.
func (n *Network) catchUpFrom(targetHeight int32, fetch headerFetcher) ([]DisconnectedBlock, error) {
	var disconnected []DisconnectedBlock
	var rewound int32
	for n.headers.Tip() < targetHeight {
		nextHeight := n.headers.Tip() + 1
		want := int32(min(int(targetHeight-n.headers.Tip()), n.cfg.HeaderBatchSize))
		raw, count, err := fetch(nextHeight, want)
		if err != nil {
			return disconnected, err
		}
		if count == 0 {
			break
		}
		// targetHeight is the latest server-reported tip we're
		// catching up to — pass it as tipHint so AppendBatch applies
		// per-header PoW only to headers within the trailing
		// recentPoWWindow.
		_, err = n.headers.AppendBatch(nextHeight, raw, targetHeight)
		if err == nil {
			continue
		}
		if !errors.Is(err, errHeaderDoesNotConnect) {
			return disconnected, err
		}
		// Reorg: the batch doesn't link to our tip. Rewind to the fork
		// point (or a window's worth when the fork is deeper) and retry;
		// the next fetch starts from the lowered tip and connects once
		// the fork point is reached.
		disc, depth, rerr := n.rewindToFork(fetch)
		if rerr != nil {
			return disconnected, fmt.Errorf("electrum: reorg rewind: %w", rerr)
		}
		disconnected = append(disconnected, disc...)
		rewound += depth
		if rewound > maxReorgDepth {
			return disconnected, fmt.Errorf(
				"electrum: reorg deeper than %d blocks near height %d; resync required: %w",
				maxReorgDepth, nextHeight, err)
		}
	}
	return disconnected, nil
}

// deliverTipChange publishes a TipChange event, blocking until the consumer
// accepts it (bounded backpressure) or the network shuts down. It must NOT
// drop on a full channel: a Disconnected event cannot be regenerated once the
// underlying headers have been rewound, so dropping one would permanently
// strand the rollback downstream. Blocking here cannot deadlock: the sole
// consumer (the chain client's tipChangeLoop) drains into an effectively
// unbounded notification queue, and the network I/O it does perform (pending
// flushes and reorg refreshes against the wallet server) never waits on this
// send. A server that stalls those requests stalls header delivery for the
// request's lifetime, not the network's.
//
// Returns true iff the event was delivered; false only when ctx was cancelled
// (shutdown), in which case the caller should not advance its delivery cursor.
func (n *Network) deliverTipChange(tc *TipChange) bool {
	if tc == nil {
		return true
	}
	select {
	case n.tipChangeChan <- tc:
		return true
	case <-n.ctx.Done():
		return false
	}
}

// scripthashLoop fans out scripthash notifications from the node to the
// per-network channel.
func (n *Network) scripthashLoop(node *Node) {
	ch := node.ScripthashNotifyChan()
	for {
		select {
		case <-n.ctx.Done():
			return
		case st, ok := <-ch:
			if !ok {
				return
			}
			// Block until delivered rather than dropping: a scripthash
			// status is the address's latest state, and dropping the
			// last one leaves an incoming tx invisible until unrelated
			// future activity. This can't stall — the chain client's
			// scripthash reader drains scripthashCh into a coalescing
			// map with no network I/O, so it's always ready to receive.
			select {
			case n.scripthashCh <- st:
			case <-n.ctx.Done():
				return
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
	select {
	case <-node.Done():
		if n.ctx.Err() == nil {
			// Unexpected death with the network still live: demote and
			// hand off to surgical rotation, which owns teardown from
			// here (installs a successor group or spawns the closer).
			// rotate runs outside this loop group — it joins the group,
			// and a member joining itself would deadlock.
			n.pool.recordFailure(node.Addr().Addr)
			log.Warnf("electrum: wallet server %s disconnected; rotating", node.Addr())
			n.wg.Add(1)
			go func() {
				defer n.wg.Done()
				n.rotate(node, nil, "connection lost")
			}()
			return
		}
	case <-n.ctx.Done():
	}
	n.spawnCloser()
}
