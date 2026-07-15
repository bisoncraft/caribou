// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

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

// rotate replaces the dead (or misbehaving) wallet node. It runs OUTSIDE the
// node's loop group — it joins that group, and a member joining itself would
// deadlock — as an n.wg-tracked goroutine spawned by the trigger sites.
//
// Every exit path guarantees teardown ownership: either a new loop group is
// installed (its watchNode owns the eventual close), or the closer is
// spawned here (Stop/failure paths). The old group's watchNode deliberately
// does not spawn the closer when handing off to rotation.
func (n *Network) rotate(dead *Node, preferred, reason string) {
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
	// A caller-preferred successor (the winning side of a verified
	// divergence) moves to the front IF the tiers admit it; a preferred
	// server outside the eligible set is ignored, and convergence comes
	// from wrong-chain servers repeatedly losing divergence audits until
	// a heavier-chain server holds the slot.
	if n.tryElect(dead.Addr().Addr, preferred) {
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
	n.wg.Add(1)
	go n.degradedLoop()
}

// tryElect walks the wallet-eligible election order — preferred first when
// the tiers admit it, exclude skipped — and attaches the first candidate
// that answers, preferring adoption of a live observer over a fresh dial.
// Caller holds the rotation mutex. Returns true when a successor is
// installed.
func (n *Network) tryElect(exclude, preferred string) bool {
	candidates := n.pool.walletCandidates()
	if preferred != "" {
		for i, sv := range candidates {
			if sv.Addr == preferred && i > 0 {
				candidates = append([]*ServerAddr{sv},
					append(append([]*ServerAddr{}, candidates[:i]...), candidates[i+1:]...)...)
				break
			}
		}
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
				n.pool.recordFailure(sv.Addr)
				continue
			}
			t, err := node.SubscribeHeaders(n.ctx)
			if err != nil {
				node.Close()
				n.pool.recordFailure(sv.Addr)
				continue
			}
			tip = t
		}
		if err := n.attachNode(node, tip); err != nil {
			log.Warnf("electrum: rotation to %s failed: %v", sv.Addr, err)
			node.Close()
			n.pool.recordFailure(sv.Addr)
			continue
		}
		n.pool.recordSuccess(sv.Addr)
		// Any successful attachment re-validated a chain end to end
		// (anchored headers, subscriptions replayed); a divergence
		// suspension no longer reflects the wallet's view.
		if n.spendSuspect.CompareAndSwap(true, false) {
			log.Infof("electrum: broadcast suspension lifted after rotation")
		}
		log.Infof("electrum: rotated wallet server to %s (tip %d)",
			sv.Addr, tip.Height)
		return true
	}
	return false
}

// degradedRetryInterval paces re-elections while no wallet server exists. A
// variable so tests can tighten it.
var degradedRetryInterval = 30 * time.Second

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
		ok := n.tryElect("", "")
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

	// A successor whose tip CONFLICTS at our height (a verified-divergence
	// tie) needs forced reconciliation: the passive path keeps first-seen
	// on equal work, but this node's histories reference ITS chain — the
	// store must follow or replayed confirmed entries would bind to the
	// wrong block hashes. Rewind and refill from the successor via the
	// standard reorg-recovery catch-up, publishing the disconnects.
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
	claimed := tipHdr.BlockHash()
	fetch := func(startHeight, count int32) ([]byte, int, error) {
		batch, err := node.BlockHeaders(n.ctx, startHeight, int(count))
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
		if served.BlockHash() == n.headers.TipHash() {
			// The store already follows the chain the server is serving;
			// the snapshot was stale.
			break
		}
		if round >= maxReconcileRounds {
			return fmt.Errorf("electrum: successor %s still conflicts at "+
				"height %d after %d reconcile rounds",
				node.Addr(), tip.Height, round)
		}
		disc, err := n.headers.Rewind(reorgRewindDepth)
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
		n.rotate(node, "", "lagging observers by "+lagFor.Round(time.Second).String())
	}()
	_ = lagBlocks
}
