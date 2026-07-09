// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// Observer connections implement multi-server phase 2
// (docs/electrum-multi-server-plan.md): header-only subscriptions to
// additional servers, used to detect a lagging or lying wallet server. They
// carry no address-level traffic (no privacy cost) and their lifecycles are
// independent — an observer death demotes it and elects a replacement
// without touching the wallet stream.

const (
	// maxObservers bounds the observer connections per network.
	maxObservers = 3

	// lagWindow converts to the lag threshold in blocks: the wallet server
	// is a lag candidate when the best observer is at least this much work
	// time ahead at the chain's target spacing. Two blocks mined seconds
	// apart and seen by an observer first must not trigger a switch, so
	// the lag must also persist for lagPatience before rotating.
	lagWindow = 20 * time.Minute
)

// lagPatience is how long a lag must persist before rotating. A variable so
// rotation tests can tighten it.
var lagPatience = 10 * time.Minute

// ObserverCount reports the number of live observer connections; primarily
// a test and diagnostics hook.
func (n *Network) ObserverCount() int {
	n.obsMtx.Lock()
	defer n.obsMtx.Unlock()
	return len(n.observers)
}

// observerNodes snapshots the live observer connections, for broadcast
// fan-out.
func (n *Network) observerNodes() []*Node {
	n.obsMtx.Lock()
	defer n.obsMtx.Unlock()
	out := make([]*Node, 0, len(n.observers))
	for _, o := range n.observers {
		out = append(out, o.node)
	}
	return out
}

// observerCompareInterval is how often observer tips are compared against
// the local chain. A variable rather than a constant so rotation tests can
// tighten the loop.
var observerCompareInterval = 30 * time.Second

// observer is one header-only server connection.
type observer struct {
	node *Node
	// stopDrain, when closed, releases the drain goroutine so an adopted
	// node's notify channel has exactly one reader (the new headersLoop).
	stopDrain chan struct{}

	mtx       sync.Mutex
	tipHeight int32
	tipHeader *wire.BlockHeader
}

func (o *observer) tip() (int32, *wire.BlockHeader) {
	o.mtx.Lock()
	defer o.mtx.Unlock()
	return o.tipHeight, o.tipHeader
}

func (o *observer) setTip(t *HeadersTip) {
	raw, err := hex.DecodeString(t.Hex)
	if err != nil {
		return
	}
	var hdr wire.BlockHeader
	if err := hdr.Deserialize(bytes.NewReader(raw)); err != nil {
		return
	}
	o.mtx.Lock()
	o.tipHeight, o.tipHeader = t.Height, &hdr
	o.mtx.Unlock()
}

// runObservers maintains the observer set and the tip-comparison loop for
// the network's lifetime. Started by Start after the wallet-server election;
// exits on context cancellation. Wallet-server rotation is delegated to the
// C1 mechanism: demote and Stop, and the wallet-level reconnect elects a
// better candidate.
func (n *Network) runObservers() {
	defer n.wg.Done()

	n.obsMtx.Lock()
	n.observers = make(map[string]*observer)
	n.obsMtx.Unlock()
	defer func() {
		n.obsMtx.Lock()
		for _, o := range n.observers {
			o.node.Close()
		}
		n.observers = nil
		n.obsMtx.Unlock()
	}()

	var lagSince time.Time
	ticker := time.NewTicker(observerCompareInterval)
	defer ticker.Stop()

	// Discovery (multi-server phase 4): poll server.peers.subscribe on the
	// connected servers, folding advertised peers into the persistent pool.
	// Hourly, plus once at startup.
	discoverEvery := int(time.Hour / observerCompareInterval)
	if discoverEvery < 1 {
		discoverEvery = 1
	}
	tick := 0

	for {
		walletAddr := n.currentWalletAddr()
		n.fillObservers(walletAddr)
		if tick%discoverEvery == 0 {
			n.discoverPeers()
		}
		tick++

		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
		}

		// Drop dead observers (demoted at death by their drain
		// goroutines); fillObservers replaces them next pass.
		n.obsMtx.Lock()
		for addr, o := range n.observers {
			select {
			case <-o.node.Done():
				delete(n.observers, addr)
			default:
			}
		}
		obs := make([]*observer, 0, len(n.observers))
		for _, o := range n.observers {
			obs = append(obs, o)
		}
		n.obsMtx.Unlock()

		localTip := n.headers.Tip()
		bestObs := int32(-1)
		for _, o := range obs {
			h, hdr := o.tip()
			if hdr == nil {
				continue
			}
			if h > bestObs {
				bestObs = h
			}
			// Divergence probe: an observer tip at a height we have,
			// with a different hash, means one of us is on another
			// chain. The dissenter must demonstrate work before any
			// action is taken (see handleDivergence).
			if h <= localTip {
				ours := n.headers.ByHeight(h)
				if ours != nil && ours.BlockHash() != hdr.BlockHash() {
					n.handleDivergence(o, walletAddr)
				}
			}
		}

		// Lag: the best observer is lagWindow's worth of blocks ahead at
		// target spacing, sustained for lagPatience.
		lagBlocks := int32(lagWindow / n.cfg.Chain.Params.TargetTimePerBlock)
		if lagBlocks < 1 {
			lagBlocks = 1
		}
		if bestObs >= 0 && bestObs-localTip >= lagBlocks {
			if lagSince.IsZero() {
				lagSince = time.Now()
			} else if time.Since(lagSince) >= lagPatience {
				log.Warnf("electrum: wallet server %s lagging %d blocks behind "+
					"observers for %v; rotating", walletAddr, bestObs-localTip,
					time.Since(lagSince).Round(time.Second))
				n.lagRotate(bestObs-localTip, time.Since(lagSince))
				lagSince = time.Time{}
			}
		} else {
			lagSince = time.Time{}
		}
	}
}

// fillObservers tops the set up to maxObservers from the candidates not in
// use, in health-scored order.
func (n *Network) fillObservers(walletAddr string) {
	have := func(addr string) (int, bool) {
		n.obsMtx.Lock()
		defer n.obsMtx.Unlock()
		_, ok := n.observers[addr]
		return len(n.observers), ok
	}
	if count, _ := have(""); count >= maxObservers {
		return
	}
	for _, sv := range n.pool.observerCandidates() {
		count, ok := have(sv.Addr)
		if count >= maxObservers {
			break
		}
		if sv.Addr == walletAddr || ok {
			continue
		}
		node, err := NewNode(n.ctx, sv, n.cfg.TorProxy,
			n.cfg.Chain.Params.GenesisHash.String())
		if err != nil {
			n.pool.recordFailure(sv.Addr)
			continue
		}
		tip, err := node.SubscribeHeaders(n.ctx)
		if err != nil {
			node.Close()
			n.pool.recordFailure(sv.Addr)
			continue
		}
		o := &observer{node: node, stopDrain: make(chan struct{})}
		o.setTip(tip)
		n.obsMtx.Lock()
		n.observers[sv.Addr] = o
		n.obsMtx.Unlock()
		log.Debugf("electrum: observer connected: %s (tip %d)", sv.Addr, tip.Height)

		// Drain tip notifications for this observer's lifetime.
		n.wg.Add(1)
		go func(o *observer) {
			defer n.wg.Done()
			for {
				select {
				case t, ok := <-o.node.HeadersNotifyChan():
					if !ok {
						return
					}
					o.setTip(t)
				case <-o.stopDrain:
					// Adopted as wallet server; failure handling
					// belongs to its watchNode now.
					return
				case <-o.node.Done():
					n.pool.recordFailure(o.node.Addr().Addr)
					return
				case <-n.ctx.Done():
					return
				}
			}
		}(o)
	}
}

// handleDivergence reacts to an observer whose chain conflicts with ours.
// The dissenter must demonstrate work: its recent headers are fetched and
// PoW-checked. Unverified dissent demotes the observer — a lone malicious
// server must not be able to freeze the wallet. Verified dissent suspends
// spend-affecting operations (Broadcast errors; nothing proactive in the UI)
// and rotates away from the wallet server so the reconnect can resolve which
// chain is canonical.
func (n *Network) handleDivergence(o *observer, walletAddr string) {
	// One snapshot: the drain goroutine advances the observer tip
	// concurrently, and reading height and header separately could fetch a
	// batch for the old height while auditing against the new tip hash,
	// spuriously demoting an honest observer.
	h, tipHdr := o.tip()
	if tipHdr == nil {
		return
	}
	start := h - reorgRewindDepth
	if start < n.headers.StartHeight() {
		start = n.headers.StartHeight()
	}
	res, err := o.node.BlockHeaders(n.ctx, start, int(h-start)+1)
	if err != nil {
		log.Warnf("electrum: observer %s diverged at height %d but failed the "+
			"work audit (header fetch: %v); demoting", o.node.Addr().Addr, h, err)
		n.pool.recordFailure(o.node.Addr().Addr)
		o.node.Close()
		return
	}
	raw, err := hex.DecodeString(res.HexConcat)
	if err != nil || len(raw) < n.cfg.Chain.HeaderSize {
		log.Warnf("electrum: observer %s diverged at height %d but returned an "+
			"unusable header batch; demoting", o.node.Addr().Addr, h)
		n.pool.recordFailure(o.node.Addr().Addr)
		o.node.Close()
		return
	}
	batch := make([]*wire.BlockHeader, 0, len(raw)/n.cfg.Chain.HeaderSize)
	for off := 0; off+n.cfg.Chain.HeaderSize <= len(raw); off += n.cfg.Chain.HeaderSize {
		var hdr wire.BlockHeader
		if err := hdr.Deserialize(bytes.NewReader(raw[off : off+n.cfg.Chain.HeaderSize])); err != nil {
			log.Warnf("electrum: observer %s diverged at height %d but served an "+
				"undecodable header; demoting: %v", o.node.Addr().Addr, h, err)
			n.pool.recordFailure(o.node.Addr().Addr)
			o.node.Close()
			return
		}
		batch = append(batch, &hdr)
	}
	dissentWork, err := auditDissent(batch, tipHdr.BlockHash(), n.headers.checkPoW)
	if err != nil {
		// Fabricated or incoherent chain: demote and drop the observer,
		// keep the wallet stream untouched.
		log.Warnf("electrum: observer %s diverged at height %d but failed the "+
			"work audit; demoting: %v", o.node.Addr().Addr, h, err)
		n.pool.recordFailure(o.node.Addr().Addr)
		o.node.Close()
		return
	}
	// Self-attested Bits are only meaningful in comparison: a header's PoW
	// check proves the hash beats its OWN claimed target, so a liar can
	// claim trivial difficulty and "pass". The dissent is credible only if
	// its cumulative work over the contested span meets or exceeds our own
	// chain's — claiming low difficulty then loses the comparison, and
	// claiming high difficulty fails the hash check.
	localWork := n.headers.workRange(start, n.headers.Tip())
	if dissentWork.Cmp(localWork) < 0 {
		log.Warnf("electrum: observer %s presents a linked, PoW-valid chain at "+
			"height %d with less cumulative work than ours (%s < %s); stale or "+
			"losing fork, demoting", o.node.Addr().Addr, h, dissentWork, localWork)
		n.pool.recordFailure(o.node.Addr().Addr)
		o.node.Close()
		return
	}
	// The dissenting chain carries at least as much real work as ours: the
	// wallet server lost a verified divergence. Suspend broadcasts,
	// quarantine the loser (misbehavior, not slowness), and rotate
	// surgically — preferring the dissenter, whose chain won the
	// comparison and whose fork segment just passed the audit. The header
	// reorg rides the successor's initial tip application through the
	// existing rewind machinery; the suspension lifts when a rotation
	// succeeds.
	log.Errorf("electrum: observer %s presents a conflicting chain with valid "+
		"work at height %d — possible eclipse or fork; suspending broadcasts "+
		"and rotating away from %s", o.node.Addr().Addr, h, walletAddr)
	n.spendSuspect.Store(true)
	n.pool.quarantine(walletAddr, fmt.Sprintf(
		"lost a verified divergence at height %d to %s", h, o.node.Addr().Addr))
	n.mu.Lock()
	walletNode := n.node
	n.mu.Unlock()
	if walletNode == nil {
		return
	}
	dissenter := o.node.Addr().Addr
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.rotate(walletNode, dissenter, "lost verified divergence")
	}()
}

// discoverPeers polls server.peers.subscribe on every connected server and
// folds the advertised peers into the persistent pool. Recommendations
// convey no trust (candidates prove themselves at handshake); they gate
// which servers may ever be elected, per the tier rules in serverpool.go.
func (n *Network) discoverPeers() {
	nodes := n.observerNodes()
	if wn, err := n.getNode(); err == nil {
		nodes = append(nodes, wn)
	}
	for _, node := range nodes {
		peers, err := node.ServerPeers(n.ctx)
		if err != nil {
			log.Debugf("electrum: peer discovery via %s: %v", node.Addr().Addr, err)
			continue
		}
		// Onion peers are undialable without a Tor proxy; admitting them
		// burns a dial timeout and a failure record per election cycle.
		if n.cfg.TorProxy == "" {
			kept := peers[:0]
			for _, sv := range peers {
				if !strings.HasSuffix(strings.Split(sv.Addr, ":")[0], ".onion") {
					kept = append(kept, sv)
				}
			}
			peers = kept
		}
		if len(peers) > 0 {
			n.pool.addDiscovered(node.Addr().Addr, peers)
			log.Debugf("electrum: %s advertised %d usable peers", node.Addr().Addr, len(peers))
		}
	}
}

// auditDissent validates a dissenting header batch's internal coherence and
// returns its cumulative work. Requirements: non-empty, internally linked
// (each header's PrevBlock is the previous header's hash), terminating in
// claimedTip (the batch actually ends at the chain the observer advertised),
// and every header passing the hash-beats-claimed-target check. The CALLER
// must compare the returned work against the local chain's over the same
// span — self-attested difficulty is only bounded by that comparison.
func auditDissent(batch []*wire.BlockHeader, claimedTip chainhash.Hash,
	checkPoW func(*wire.BlockHeader) error) (*big.Int, error) {

	if len(batch) == 0 {
		return nil, errors.New("electrum: empty dissent batch")
	}
	work := new(big.Int)
	for i, hdr := range batch {
		if i > 0 && hdr.PrevBlock != batch[i-1].BlockHash() {
			return nil, fmt.Errorf("electrum: dissent batch does not link at index %d", i)
		}
		if err := checkPoW(hdr); err != nil {
			return nil, err
		}
		work.Add(work, blockchain.CalcWork(hdr.Bits))
	}
	if got := batch[len(batch)-1].BlockHash(); got != claimedTip {
		return nil, fmt.Errorf("electrum: dissent batch ends at %s, not the "+
			"advertised tip %s", got, claimedTip)
	}
	return work, nil
}
