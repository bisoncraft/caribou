// Copyright (c) 2024 The Caribou Developers
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
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// Surgical wallet-server rotation (docs/electrum-surgical-rotation-plan.md
// R4): quarantine or demote the failed wallet server, promote a live
// observer (or dial a fresh candidate), catch up headers, replay the
// subscription set, and attach a new loop group — with nothing visible above
// ElectrumNetwork. Lock order: rotMtx → mu → obsMtx → pool.mtx.

// rotMtx serializes rotations against each other; Stop wins any race via the
// context check at each step.
type rotationGuard struct {
	mtx sync.Mutex
}

// electionHint steers a rotation triggered by a verified divergence toward
// the chain that won the audit. Rotations triggered by connection loss or lag
// pass nil.
type electionHint struct {
	// preferred lists successors to try first, in order: the dissenting
	// observer whose chain won, then the live observers agreeing with it.
	// An entry outside the wallet-eligible tiers is ignored.
	preferred []string
	// wantHeight and wantHash identify the winning header at the conflict
	// height. A candidate serving a different header there is skipped, so
	// the election does not land on another server stuck on the losing
	// chain — which would drag the header store back onto it and repeat
	// the divergence on the next observer tick.
	wantHeight int32
	wantHash   chainhash.Hash
}

// rotate replaces the dead (or misbehaving) wallet node. It runs OUTSIDE the
// node's loop group — it joins that group, and a member joining itself would
// deadlock — as an n.wg-tracked goroutine spawned by the trigger sites.
//
// Every exit path guarantees teardown ownership: either a new loop group is
// installed (its watchNode owns the eventual close), or the closer is
// spawned here (Stop/failure paths). The old group's watchNode deliberately
// does not spawn the closer when handing off to rotation.
func (n *Network) rotate(dead *Node, hint *electionHint, reason string) {
	n.rot.mtx.Lock()
	defer n.rot.mtx.Unlock()

	if n.ctx.Err() != nil {
		// Stop won the race; the old watchNode skipped the closer when
		// it handed off to us, so it is ours to spawn.
		n.spawnCloser()
		return
	}

	// A rotation away from a LIVE node (the lag and divergence paths)
	// closes it below, which wakes that node's own watchNode, which hands
	// off a duplicate rotate for the same node. By the time the duplicate
	// wins rotMtx, the node has already been replaced — a successor group
	// is installed, or the slot was cleared for the degraded loop — and
	// joining the CURRENT loop group here would block until the healthy
	// successor itself dies, holding rotMtx and wedging every rotation
	// after it. Teardown is owned downstream in both replacement shapes
	// (the successor's watchNode, or the degraded loop), so a stale
	// rotation has nothing to do. Reading node and group under one lock
	// also guarantees the group joined below belongs to `dead`, never to
	// a successor.
	n.mu.Lock()
	current, group := n.node, n.loops
	n.mu.Unlock()
	if current != dead {
		return
	}

	log.Warnf("electrum: rotating away from wallet server %s: %s",
		dead.Addr().Addr, reason)

	// Make sure the old node is fully dead (the lag path hands us a live
	// one) and its loop group has drained before any successor attaches —
	// two groups must never interleave header appends or status forwards.
	//
	// join() is held under rotMtx intentionally. It waits for the old
	// loops to exit; a loop parked in a blocking send unblocks when the
	// consumer drains. That consumer is the chain client's
	// notificationHandler, which buffers into an unbounded slice and
	// always drains unless the wallet above it has itself stopped
	// consuming — so the wait is bounded in a healthy wallet. Aborting the
	// sends instead would drop an in-flight reorg Disconnected that the
	// successor's header sync rewinds at the store level but does not
	// re-emit to the consumer, so the bounded wait is the safe choice.
	// Start's group also carries the initial-sync member (which runs
	// headersLoop inline on success); its exit is bounded by the Close
	// above — a mid-sync BlockHeaders call fails fast on the dead
	// connection.
	dead.Close()
	if group != nil {
		group.join()
	}
	if n.ctx.Err() != nil {
		n.spawnCloser()
		return
	}

	// Successor preference: a live observer already vetted for
	// reachability, genesis, and chain agreement — restricted to the
	// wallet-eligible tier (shipped first; discovered only under the
	// gated fallback) — else a fresh dial down the same election order.
	// A divergence hint moves the winning side to the front IF the tiers
	// admit it and skips candidates serving the losing chain; a preferred
	// server outside the eligible set is ignored.
	if n.tryElect(dead.Addr().Addr, hint) {
		return
	}

	// No candidate answered. Enter the degraded state instead of dying:
	// wallet slot empty (getNode callers error), observers as they are,
	// and an internal timer re-runs the election until a candidate
	// answers. Channels close only on Stop; the degraded loop owns the
	// closer while no loop group exists.
	log.Errorf("electrum: no usable wallet server; degraded — retrying every %v",
		degradedRetryInterval)
	n.mu.Lock()
	n.node = nil
	n.loops = nil
	n.mu.Unlock()
	degradedEntries.Add(1)
	n.wg.Add(1)
	go n.degradedLoop()
}

// tryElect walks the wallet-eligible election order — the hint's preferred
// servers first when the tiers admit them, exclude skipped — and attaches
// the first candidate that answers (and, under a hint, serves the winning
// chain), preferring adoption of a live observer over a fresh dial. Caller
// holds the rotation mutex. Returns true when a successor is installed.
func (n *Network) tryElect(exclude string, hint *electionHint) bool {
	candidates := n.pool.walletCandidates()
	if hint != nil {
		candidates = promoteCandidates(candidates, hint.preferred)
	}
	for _, sv := range candidates {
		if sv.Addr == exclude {
			continue
		}
		node, tip := n.adoptObserver(sv.Addr)
		if node == nil {
			var err error
			node, err = NewNode(n.ctx, sv, n.cfg.TorProxy,
				n.cfg.Chain.Params.GenesisHash.String())
			if err != nil {
				log.Debugf("electrum: election: %s: %v", sv.Addr, err)
				n.pool.recordFailure(sv.Addr)
				continue
			}
			// The connection itself lives on the network context (dial
			// derives the socket's lifetime from the context handed to
			// NewNode); only the probe round trips are bounded.
			ctx, cancel := n.rpcCtx()
			t, err := node.SubscribeHeaders(ctx)
			cancel()
			if err != nil {
				log.Debugf("electrum: election: %s: subscribe headers: %v", sv.Addr, err)
				node.Close()
				n.pool.recordFailure(sv.Addr)
				continue
			}
			tip = t
		}
		if hint != nil {
			ctx, cancel := n.rpcCtx()
			agrees, err := n.servesWinningChain(ctx, node, tip, hint)
			cancel()
			if err != nil {
				log.Debugf("electrum: election: %s: %v", sv.Addr, err)
				node.Close()
				n.pool.recordFailure(sv.Addr)
				continue
			}
			if !agrees {
				// Chain disagreement is not a dial failure: no health
				// demerit. A server on the losing side of a fork is
				// behind, not misbehaving, and reorgs on its own.
				log.Debugf("electrum: election: skipping %s: serves a "+
					"conflicting header at height %d", sv.Addr, hint.wantHeight)
				node.Close()
				continue
			}
		}
		if err := n.attachNode(node, tip); err != nil {
			log.Warnf("electrum: rotation to %s failed: %v", sv.Addr, err)
			node.Close()
			n.pool.recordFailure(sv.Addr)
			continue
		}
		n.pool.recordSuccess(sv.Addr)
		log.Infof("electrum: rotated wallet server to %s (tip %d)",
			sv.Addr, tip.Height)
		return true
	}
	return false
}

// promoteCandidates moves the preferred addresses that appear in candidates
// to the front, in preferred order, keeping the rest in election order.
func promoteCandidates(candidates []*ServerAddr, preferred []string) []*ServerAddr {
	if len(preferred) == 0 {
		return candidates
	}
	byAddr := make(map[string]*ServerAddr, len(candidates))
	for _, sv := range candidates {
		byAddr[sv.Addr] = sv
	}
	out := make([]*ServerAddr, 0, len(candidates))
	moved := make(map[string]bool, len(preferred))
	for _, addr := range preferred {
		if sv := byAddr[addr]; sv != nil && !moved[addr] {
			out = append(out, sv)
			moved[addr] = true
		}
	}
	for _, sv := range candidates {
		if !moved[sv.Addr] {
			out = append(out, sv)
		}
	}
	return out
}

// servesWinningChain reports whether a candidate serves the hint's winning
// header at the conflict height. A candidate whose tip is below that height
// cannot be judged and is admitted: attaching to it keeps the store's chain
// (its catch-up has nothing to add), and the observers keep policing it.
func (n *Network) servesWinningChain(ctx context.Context, node *Node, tip *HeadersTip, hint *electionHint) (bool, error) {
	switch {
	case tip.Height < hint.wantHeight:
		return true, nil
	case tip.Height == hint.wantHeight:
		raw, err := hex.DecodeString(tip.Hex)
		if err != nil {
			return false, fmt.Errorf("tip header: %w", err)
		}
		var hdr wire.BlockHeader
		if err := hdr.Deserialize(bytes.NewReader(raw)); err != nil {
			return false, fmt.Errorf("tip header: %w", err)
		}
		return n.headers.headerHash(&hdr) == hint.wantHash, nil
	}
	hdr, err := n.fetchHeaderAt(ctx, node, hint.wantHeight)
	if err != nil {
		return false, err
	}
	return hdr != nil && n.headers.headerHash(hdr) == hint.wantHash, nil
}

// rpcCtx returns the network context bounded by rpcTimeout, for a single
// control-path round trip.
func (n *Network) rpcCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(n.ctx, rpcTimeout)
}

// fetchHeaderAt fetches the single header node serves at height, or nil when
// the server has none there.
func (n *Network) fetchHeaderAt(ctx context.Context, node *Node, height int32) (*wire.BlockHeader, error) {
	res, err := node.BlockHeaders(ctx, height, 1)
	if err != nil {
		return nil, fmt.Errorf("header at %d: %w", height, err)
	}
	if res.Count == 0 {
		return nil, nil
	}
	raw, err := hex.DecodeString(res.HexConcat)
	if err != nil {
		return nil, fmt.Errorf("header at %d: %w", height, err)
	}
	hdrs, err := n.headers.parseBatch(raw)
	if err != nil || len(hdrs) == 0 {
		return nil, fmt.Errorf("header at %d: undecodable reply", height)
	}
	return hdrs[0], nil
}

// degradedRetryInterval paces re-elections while no wallet server exists. A
// variable so tests can tighten it.
var degradedRetryInterval = 30 * time.Second

// degradedEntries counts transitions into the degraded state. Observability
// hook: the state itself (an empty wallet slot) can be entered and re-exited
// within one polling interval, so tests assert on the transition count.
var degradedEntries atomic.Int64

// degradedLoop re-runs the election until a candidate answers or the network
// stops. While it runs, no loop group exists, so it owns the closer on the
// Stop path.
func (n *Network) degradedLoop() {
	defer n.wg.Done()
	ticker := time.NewTicker(degradedRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-n.ctx.Done():
			n.spawnCloser()
			return
		case <-ticker.C:
		}
		n.rot.mtx.Lock()
		if n.ctx.Err() != nil {
			n.rot.mtx.Unlock()
			n.spawnCloser()
			return
		}
		ok := n.tryElect("", nil)
		n.rot.mtx.Unlock()
		if ok {
			log.Infof("electrum: recovered from degraded state")
			return
		}
	}
}

// adoptObserver removes the observer at addr from the set (if live) and
// returns its node plus a HeadersTip describing the observer's last-known
// tip. The observer's drain goroutine is stopped so the new headersLoop is
// the channel's only reader.
func (n *Network) adoptObserver(addr string) (*Node, *HeadersTip) {
	n.obsMtx.Lock()
	o := n.observers[addr]
	if o == nil {
		n.obsMtx.Unlock()
		return nil, nil
	}
	delete(n.observers, addr)
	n.obsMtx.Unlock()

	close(o.stopDrain)
	height, hdr := o.tip()
	if hdr == nil {
		// Never saw a tip; treat as a fresh dial candidate elsewhere.
		o.node.Close()
		return nil, nil
	}
	var buf bytes.Buffer
	hdr.Serialize(&buf) //nolint:errcheck
	return o.node, &HeadersTip{Height: height, Hex: hex.EncodeToString(buf.Bytes())}
}

// maxReconcileRounds bounds attachNode's tip-conflict reconciliation. Each
// round re-reads the header the server currently serves at the snapshot
// height, so rounds are consumed only when the server's answer keeps
// changing: an honest reorg mid-attach costs one extra round, while a
// flapping or hostile server exhausts the budget and the election moves on.
// Depth within a round is bounded by catchUpFrom's maxReorgDepth.
const maxReconcileRounds = 4

// attachNode makes node the wallet server: header catch-up, subscription
// replay, publication, and a fresh loop group.
func (n *Network) attachNode(node *Node, tip *HeadersTip) error {
	// Catch up headers first so replayed confirmed activity has its
	// headers local (statuses replayed below may reference new blocks).
	if err := n.syncHeaders(n.ctx, node, tip.Height); err != nil {
		return err
	}

	// A successor whose tip CONFLICTS at our height (the winning side of a
	// verified divergence) needs forced reconciliation: the passive path
	// keeps first-seen on equal work, but this node's histories reference
	// ITS chain — the store must follow or replayed confirmed entries
	// would bind to the wrong block hashes. Rewind to the fork point and
	// refill from the successor via the standard reorg-recovery catch-up,
	// publishing the disconnects.
	//
	// The exit test compares the store against the header the server
	// serves NOW — the snapshot in `tip` only short-circuits the common
	// already-aligned case. The snapshot goes stale the moment the server
	// reorgs again (an adopted observer's last-drained tip, or a lying
	// server), and reconciling toward a hash the server no longer serves
	// makes zero progress per round: each rewind refills the same blocks
	// from the server's current chain, forever, under rotMtx, flooding the
	// consumer with disconnect batches. Rounds are consumed only when the
	// server's answer keeps changing — an honest mid-attach reorg costs
	// one extra round — so the small budget is purely a flap/hostility
	// backstop; depth within a round is bounded by catchUpFrom's
	// maxReorgDepth.
	rawTip, err := hex.DecodeString(tip.Hex)
	if err != nil {
		return fmt.Errorf("electrum: successor tip header: %w", err)
	}
	var tipHdr wire.BlockHeader
	if err := tipHdr.Deserialize(bytes.NewReader(rawTip)); err != nil {
		return fmt.Errorf("electrum: successor tip header: %w", err)
	}
	claimed := n.headers.headerHash(&tipHdr)
	fetch := nodeFetcher(n.ctx, node)
	for round := 0; n.ctx.Err() == nil && tip.Height == n.headers.Tip() &&
		n.headers.TipHash() != claimed; round++ {
		raw, count, err := fetch(tip.Height, 1)
		if err != nil {
			return fmt.Errorf("electrum: reconcile tip fetch: %w", err)
		}
		if count == 0 {
			// The server no longer serves this height; headersLoop
			// reconciles from live notifications once attached.
			break
		}
		var served wire.BlockHeader
		if err := served.Deserialize(bytes.NewReader(raw)); err != nil {
			return fmt.Errorf("electrum: reconcile tip header: %w", err)
		}
		if n.headers.headerHash(&served) == n.headers.TipHash() {
			// The store already follows the chain the server is serving;
			// the snapshot was stale.
			break
		}
		if round >= maxReconcileRounds {
			return fmt.Errorf("electrum: successor %s still conflicts at "+
				"height %d after %d reconcile rounds",
				node.Addr(), tip.Height, round)
		}
		disc, _, err := n.rewindToFork(fetch)
		if err != nil {
			return fmt.Errorf("electrum: reconcile rewind: %w", err)
		}
		// Deliver the disconnects before any error return: the store is
		// already truncated, and an undelivered rollback permanently
		// diverges the consumer's view from the store.
		more, cerr := n.catchUpFrom(tip.Height, fetch)
		disc = append(disc, more...)
		if len(disc) > 0 && !n.deliverTipChange(&TipChange{Disconnected: disc}) {
			return errors.New("electrum: shutdown during reconciliation")
		}
		if cerr != nil {
			return fmt.Errorf("electrum: reconcile catch-up: %w", cerr)
		}
	}

	if err := n.replaySubscriptions(n.ctx, node); err != nil {
		return err
	}

	n.serverTipMu.Lock()
	if tip.Height > n.serverTip {
		n.serverTip = tip.Height
	}
	n.serverTipMu.Unlock()

	group := &loopGroup{node: node}
	n.mu.Lock()
	n.node = node
	n.loops = group
	n.mu.Unlock()

	// Record the new wallet address for observer bookkeeping and purge any
	// observer already sitting at it, in ONE obsMtx section. fillObservers
	// inserts under the same lock after re-reading obsWalletAddr, so every
	// interleaving converges: an insert that won the lock first is purged
	// here; one that runs after sees the new address and skips. (The
	// mu-read pre-checks in fillObservers are only dial-saving filters;
	// this section is what makes a self-observer impossible.) adoptObserver
	// already removed the promoted observer itself, so anything found here
	// is a duplicate connection. stopDrain is closed before the node so the
	// drain goroutine exits without recording a spurious failure against
	// the new wallet server's address.
	n.obsMtx.Lock()
	n.obsWalletAddr = node.Addr().Addr
	if o, ok := n.observers[node.Addr().Addr]; ok {
		delete(n.observers, node.Addr().Addr)
		close(o.stopDrain)
		o.node.Close()
	}
	n.obsMtx.Unlock()

	n.spawnLoop(group, func() { n.scripthashLoop(node) })
	n.spawnLoop(group, func() { n.watchNode(node) })
	n.spawnLoop(group, func() { n.headersLoop(node, tip) })
	return nil
}

// spawnCloser launches the final-shutdown bookkeeping: wait for every
// goroutine, then close the public channels exactly once. Owned by whichever
// path determines no further loop group will exist.
func (n *Network) spawnCloser() {
	go func() {
		n.wg.Wait()
		n.doneClosed.Do(func() {
			// Persist any reputation still waiting on the pool's
			// coalescing delay; every writer has exited by now.
			n.pool.flush()
			close(n.tipChangeChan)
			close(n.scripthashCh)
			close(n.done)
		})
	}()
}

// currentWalletAddr returns the current wallet server's address, or "".
func (n *Network) currentWalletAddr() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.node == nil {
		return ""
	}
	return n.node.Addr().Addr
}

// lagRotate is the lag-trigger entry: demote (never quarantine — slowness is
// not hostility) and rotate.
func (n *Network) lagRotate(lagBlocks int32, lagFor time.Duration) {
	n.mu.Lock()
	node := n.node
	n.mu.Unlock()
	if node == nil {
		return
	}
	n.pool.recordFailure(node.Addr().Addr)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.rotate(node, nil, "lagging observers by "+lagFor.Round(time.Second).String())
	}()
	_ = lagBlocks
}
