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

	log.Warnf("electrum: rotating away from wallet server %s: %s",
		dead.Addr().Addr, reason)

	// Make sure the old node is fully dead (the lag path hands us a live
	// one) and its loop group has drained before any successor attaches.
	dead.Close()
	n.mu.Lock()
	group := n.loops
	n.mu.Unlock()
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
		if sv.Addr == dead.Addr().Addr {
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
		return
	}

	// No candidate answered. Until degraded self-healing (R6), this falls
	// back to a full stop; the closer is ours.
	log.Errorf("electrum: rotation found no usable wallet server; stopping")
	n.Stop()
	n.spawnCloser()
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
	for n.ctx.Err() == nil && tip.Height == n.headers.Tip() &&
		n.headers.TipHash() != claimed {
		disc, err := n.headers.Rewind(reorgRewindDepth)
		if err != nil {
			return fmt.Errorf("electrum: reconcile rewind: %w", err)
		}
		more, err := n.catchUpFrom(tip.Height, fetch)
		if err != nil {
			return fmt.Errorf("electrum: reconcile catch-up: %w", err)
		}
		disc = append(disc, more...)
		if len(disc) > 0 && !n.deliverTipChange(&TipChange{Disconnected: disc}) {
			return errors.New("electrum: shutdown during reconciliation")
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
