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

// maxObservers, which bounds the observer connections per network, lives in
// observers_desktop.go and observers_mobile.go: a phone cannot afford the
// desktop count.

const (
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

// divergenceLoss identifies a verified divergence the wallet server lost: an
// observer demonstrated strictly more work than the store holds. The loss
// must be observed on two consecutive ticks before the loop acts on it. The
// block that breaks a same-height tie reaches the observers and the wallet
// server seconds apart; a tick landing in that gap would otherwise rotate
// away from a server that is about to catch up on its own, and the lag
// path already waits out exactly this kind of propagation skew.
type divergenceLoss struct {
	walletAddr string
	height     int32          // conflict height
	loser      chainhash.Hash // the store's header at height
	hint       *electionHint
}

func (l *divergenceLoss) same(o *divergenceLoss) bool {
	return o != nil && l.walletAddr == o.walletAddr && l.height == o.height &&
		l.loser == o.loser
}

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

	st := &observerLoopState{
		lagBlocks: int32(lagWindow / n.cfg.Chain.Params.TargetTimePerBlock),
	}
	if st.lagBlocks < 1 {
		st.lagBlocks = 1
	}

	for {
		n.fillObservers(n.currentWalletAddr())
		if tick%discoverEvery == 0 {
			n.discoverPeers()
		}
		tick++

		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
		}
		n.observerTick(st)
	}
}

// observerLoopState carries the comparison loop's memory between ticks.
type observerLoopState struct {
	lagBlocks   int32
	lagSince    time.Time
	pendingLoss *divergenceLoss
}

// observerTick is one pass of the comparison loop: prune dead observers,
// audit divergences, and check for lag. Split from runObservers so tests can
// drive ticks deterministically.
func (n *Network) observerTick(st *observerLoopState) {
	// Drop dead observers (demoted at death by their drain goroutines);
	// fillObservers replaces them next pass.
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
		if h, hdr := o.tip(); hdr != nil && h > bestObs {
			bestObs = h
		}
	}

	// Divergence probe: an observer serving a different header at a height
	// we have means one of us is on another chain. The dissenter must
	// demonstrate work before any action is taken (see handleDivergence).
	// An observer ahead of us is probed at our tip height — its extension
	// is what breaks an equal-work tie — up to the reorg budget; beyond
	// that the store is stale enough that the lag path's rotation is the
	// right answer.
	var loss *divergenceLoss
	for _, o := range obs {
		h, hdr := o.tip()
		if hdr == nil {
			continue
		}
		switch {
		case h <= localTip:
			ours := n.headers.ByHeight(h)
			if ours != nil && n.headers.headerHash(ours) != n.headers.headerHash(hdr) {
				loss = n.handleDivergence(o, h)
			}
		case h-localTip <= maxReorgDepth:
			ctx, cancel := n.rpcCtx()
			conflicts := n.observerConflictsAt(ctx, o, localTip)
			cancel()
			if conflicts {
				loss = n.handleDivergence(o, localTip)
			}
		}
		if loss != nil {
			break
		}
	}
	switch {
	case loss == nil:
		st.pendingLoss = nil
	case loss.same(st.pendingLoss):
		st.pendingLoss = nil
		n.divergenceRotate(loss)
	default:
		log.Infof("electrum: wallet server %s serves a losing block at height "+
			"%d (%s); rotating if it persists next tick", loss.walletAddr,
			loss.height, loss.loser)
		st.pendingLoss = loss
	}

	// Lag: the best observer is lagWindow's worth of blocks ahead at
	// target spacing, sustained for lagPatience.
	if bestObs >= 0 && bestObs-localTip >= st.lagBlocks {
		if st.lagSince.IsZero() {
			st.lagSince = time.Now()
		} else if time.Since(st.lagSince) >= lagPatience {
			log.Warnf("electrum: wallet server %s lagging %d blocks behind "+
				"observers for %v; rotating", n.currentWalletAddr(),
				bestObs-localTip, time.Since(st.lagSince).Round(time.Second))
			n.lagRotate(bestObs-localTip, time.Since(st.lagSince))
			st.lagSince = time.Time{}
		}
	} else {
		st.lagSince = time.Time{}
	}
}

// observerConflictsAt reports whether the observer serves a different header
// than the store at height. Used for observers ahead of the local tip, whose
// own tip cannot be compared directly. A probe that fails, including on the
// caller's deadline, is no conflict: the observer is re-probed next tick.
func (n *Network) observerConflictsAt(ctx context.Context, o *observer, height int32) bool {
	ours := n.headers.ByHeight(height)
	if ours == nil {
		return false
	}
	theirs, err := n.fetchHeaderAt(ctx, o.node, height)
	if err != nil {
		log.Debugf("electrum: observer %s: %v", o.node.Addr().Addr, err)
		return false
	}
	return theirs != nil && n.headers.headerHash(theirs) != n.headers.headerHash(ours)
}

// maxObserverDialsPerPass caps connection attempts per fillObservers pass.
// The candidate list can be flooded with unreachable sybils, each failed
// dial costs a timeout, and fillObservers runs on the observer tick loop —
// which also drives divergence and lag detection — so an unbounded pass
// would wedge those checks behind serial dial timeouts. Unfilled slots are
// retried next tick. 8 gives a full pass (maxObservers slots) several
// failures of headroom while keeping the worst-case pass short.
const maxObserverDialsPerPass = 8

// fillObservers tops the set up to maxObservers from the candidates not in
// use, in health-scored order. walletAddr is the caller's snapshot of the
// wallet server; because a rotation may complete while this runs, the wallet
// address is re-checked at insertion time.
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
	dials := 0
	for _, sv := range n.pool.observerCandidates() {
		count, ok := have(sv.Addr)
		if count >= maxObservers {
			break
		}
		if sv.Addr == walletAddr || ok {
			continue
		}
		if dials == maxObserverDialsPerPass {
			break
		}
		dials++
		node, err := NewNode(n.ctx, sv, n.cfg.TorProxy,
			n.cfg.Chain.Params.GenesisHash.String())
		if err != nil {
			n.pool.recordFailure(sv.Addr)
			continue
		}
		ctx, cancel := n.rpcCtx()
		tip, err := node.SubscribeHeaders(ctx)
		cancel()
		if err != nil {
			node.Close()
			n.pool.recordFailure(sv.Addr)
			continue
		}
		o := &observer{node: node, stopDrain: make(chan struct{})}
		o.setTip(tip)
		// The walletAddr snapshot can go stale during the dials above (a
		// rotation completing mid-pass), and the wallet server must never
		// be its own observer. currentWalletAddr is re-read with no lock
		// held (it takes mu, and the documented rotMtx → mu → obsMtx order
		// forbids acquiring mu after obsMtx) purely to save the insert
		// work; the authoritative check is obsWalletAddr inside the insert
		// section below, which attachNode writes in the same obsMtx section
		// that purges a conflicting observer — the pair cannot interleave
		// into a self-observer.
		if sv.Addr == n.currentWalletAddr() {
			node.Close()
			continue
		}
		n.obsMtx.Lock()
		if sv.Addr == n.obsWalletAddr {
			n.obsMtx.Unlock()
			node.Close()
			continue
		}
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

// handleDivergence audits an observer whose chain conflicts with ours at
// conflictHeight. The dissenter must demonstrate work: its recent headers
// are fetched and PoW-checked, and their cumulative work over the contested
// span must strictly exceed the store's. Unverified dissent demotes the
// observer — a lone malicious server must not be able to freeze the wallet.
// Equal work is a tie, not a verdict: same-height competing blocks carry
// identical difficulty, so acting on a tie chose a side at random, and the
// other side's observers reversed it on the next rotation (on LTC mainnet
// that walked through every shipped server and left the wallet degraded for
// hours). Returns the verified loss for the tick loop to act on once it has
// persisted, or nil.
func (n *Network) handleDivergence(o *observer, conflictHeight int32) *divergenceLoss {
	// One snapshot: the drain goroutine advances the observer tip
	// concurrently, and reading height and header separately could fetch a
	// batch for the old height while auditing against the new tip hash,
	// spuriously demoting an honest observer.
	h, tipHdr := o.tip()
	if tipHdr == nil || h < conflictHeight {
		return nil
	}
	addr := o.node.Addr().Addr
	start := conflictHeight - reorgRewindDepth
	if start < n.headers.StartHeight() {
		start = n.headers.StartHeight()
	}
	ctx, cancel := n.rpcCtx()
	res, err := o.node.BlockHeaders(ctx, start, int(h-start)+1)
	cancel()
	if err != nil {
		log.Warnf("electrum: observer %s diverged at height %d but failed the "+
			"work audit (header fetch: %v); demoting", addr, conflictHeight, err)
		n.pool.recordFailure(addr)
		o.node.Close()
		return nil
	}
	raw, err := hex.DecodeString(res.HexConcat)
	if err != nil || len(raw) < n.cfg.Chain.HeaderSize {
		log.Warnf("electrum: observer %s diverged at height %d but returned an "+
			"unusable header batch; demoting", addr, conflictHeight)
		n.pool.recordFailure(addr)
		o.node.Close()
		return nil
	}
	batch := make([]*wire.BlockHeader, 0, len(raw)/n.cfg.Chain.HeaderSize)
	for off := 0; off+n.cfg.Chain.HeaderSize <= len(raw); off += n.cfg.Chain.HeaderSize {
		var hdr wire.BlockHeader
		if err := hdr.Deserialize(bytes.NewReader(raw[off : off+n.cfg.Chain.HeaderSize])); err != nil {
			log.Warnf("electrum: observer %s diverged at height %d but served an "+
				"undecodable header; demoting: %v", addr, conflictHeight, err)
			n.pool.recordFailure(addr)
			o.node.Close()
			return nil
		}
		batch = append(batch, &hdr)
	}
	dissentWork, err := auditDissent(batch, n.headers.headerHash(tipHdr),
		n.headers.headerHash, n.headers.checkPoW)
	if err != nil {
		// Fabricated or incoherent chain: demote and drop the observer,
		// keep the wallet stream untouched.
		log.Warnf("electrum: observer %s diverged at height %d but failed the "+
			"work audit; demoting: %v", addr, conflictHeight, err)
		n.pool.recordFailure(addr)
		o.node.Close()
		return nil
	}
	if int(conflictHeight-start) >= len(batch) {
		log.Warnf("electrum: observer %s diverged at height %d but its batch "+
			"stops short of it; demoting", addr, conflictHeight)
		n.pool.recordFailure(addr)
		o.node.Close()
		return nil
	}
	theirs := n.headers.headerHash(batch[conflictHeight-start])

	// The audit above involved network round trips; re-read the store
	// before judging. A rotation completing meanwhile reconciles the store
	// onto its successor's chain, and a live reorg can move it too — in
	// either case the conflict may be gone, and the judgment must be
	// against whatever the store holds NOW, not the snapshot the tick
	// started from.
	ours := n.headers.ByHeight(conflictHeight)
	if ours == nil || n.headers.headerHash(ours) == theirs {
		log.Debugf("electrum: divergence with %s at height %d resolved during "+
			"the audit", addr, conflictHeight)
		return nil
	}
	oursHash := n.headers.headerHash(ours)

	// Self-attested Bits are only meaningful in comparison: a header's PoW
	// check proves the hash beats its OWN claimed target, so a liar can
	// claim trivial difficulty and "pass". The dissent is credible only if
	// its cumulative work over the contested span exceeds our own chain's —
	// claiming low difficulty then loses the comparison, and claiming high
	// difficulty fails the hash check.
	localWork := n.headers.workRange(start, n.headers.Tip())
	switch dissentWork.Cmp(localWork) {
	case -1:
		log.Warnf("electrum: observer %s presents a linked, PoW-valid chain at "+
			"height %d with less cumulative work than ours (%s < %s); stale or "+
			"losing fork, demoting", addr, conflictHeight, dissentWork, localWork)
		n.pool.recordFailure(addr)
		o.node.Close()
		return nil
	case 0:
		// Competing blocks at one height. Neither side has demonstrated
		// more work; the next block decides, and the next tick sees it
		// either as our tip advancing (the observer then loses) or as
		// the observer pulling ahead (probed at our tip height).
		log.Debugf("electrum: observer %s serves a competing block at height "+
			"%d with equal work (%s vs ours %s); tie, waiting for the next block",
			addr, conflictHeight, theirs, oursHash)
		return nil
	}

	// The dissenting chain carries more real work than ours: the wallet
	// server, whatever holds the slot now, is serving the losing chain
	// the store holds. Hand the loss to the tick loop; it acts once the
	// loss has persisted across a tick.
	n.mu.Lock()
	walletNode := n.node
	n.mu.Unlock()
	if walletNode == nil {
		// Degraded: no wallet server to rotate away from. The degraded
		// loop's next election lands on whatever answers, and the
		// observers police it from there.
		return nil
	}
	log.Warnf("electrum: observer %s presents a chain with more work at height "+
		"%d (%s vs ours %s); wallet server %s is on the losing side", addr,
		conflictHeight, theirs, oursHash, walletNode.Addr().Addr)
	return &divergenceLoss{
		walletAddr: walletNode.Addr().Addr,
		height:     conflictHeight,
		loser:      oursHash,
		hint: &electionHint{
			preferred:  append([]string{addr}, n.agreeingObservers(o, conflictHeight, theirs)...),
			wantHeight: conflictHeight,
			wantHash:   theirs,
		},
	}
}

// divergenceRotate acts on a persisted verified loss: demote the wallet
// server (a failure record — it deprioritizes the server for the backoff
// window, as the lag path does; being behind on a fork is slowness, not the
// misbehavior quarantine is for) and rotate surgically, preferring the
// dissenter and the observers that agree with it, whose chain won the
// comparison and whose fork segment passed the audit, and skipping
// candidates on the losing chain. The header reorg rides the successor's
// attach through the fork-point rewind. No-op if the slot has changed hands
// since the loss was recorded.
func (n *Network) divergenceRotate(loss *divergenceLoss) {
	n.mu.Lock()
	walletNode := n.node
	n.mu.Unlock()
	if walletNode == nil || walletNode.Addr().Addr != loss.walletAddr {
		return
	}
	log.Warnf("electrum: wallet server %s still serves the losing chain at "+
		"height %d; rotating toward %s", loss.walletAddr, loss.height,
		loss.hint.wantHash)
	n.pool.recordFailure(loss.walletAddr)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.rotate(walletNode, loss.hint, "lost verified divergence")
	}()
}

// agreeingObservers returns the live observers, other than the dissenter,
// whose advertised tip is on the dissenter's chain: the same tip, or the
// winning header itself at the conflict height.
func (n *Network) agreeingObservers(dissenter *observer, conflictHeight int32, winner chainhash.Hash) []string {
	dh, dhdr := dissenter.tip()
	var dtip chainhash.Hash
	if dhdr != nil {
		dtip = n.headers.headerHash(dhdr)
	}
	n.obsMtx.Lock()
	defer n.obsMtx.Unlock()
	var out []string
	for addr, o := range n.observers {
		if o == dissenter {
			continue
		}
		h, hdr := o.tip()
		if hdr == nil {
			continue
		}
		hash := n.headers.headerHash(hdr)
		if (h == conflictHeight && hash == winner) || (dhdr != nil && h == dh && hash == dtip) {
			out = append(out, addr)
		}
	}
	return out
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
		ctx, cancel := n.rpcCtx()
		peers, err := node.ServerPeers(ctx)
		cancel()
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
// headerHash supplies the chain's block identity (PrevBlock linkage is X11
// on Dash, for example, so raw wire.BlockHeader.BlockHash cannot be used).
func auditDissent(batch []*wire.BlockHeader, claimedTip chainhash.Hash,
	headerHash func(*wire.BlockHeader) chainhash.Hash,
	checkPoW func(*wire.BlockHeader) error) (*big.Int, error) {

	if len(batch) == 0 {
		return nil, errors.New("electrum: empty dissent batch")
	}
	work := new(big.Int)
	for i, hdr := range batch {
		if i > 0 && hdr.PrevBlock != headerHash(batch[i-1]) {
			return nil, fmt.Errorf("electrum: dissent batch does not link at index %d", i)
		}
		if err := checkPoW(hdr); err != nil {
			return nil, err
		}
		work.Add(work, blockchain.CalcWork(hdr.Bits))
	}
	if got := headerHash(batch[len(batch)-1]); got != claimedTip {
		return nil, fmt.Errorf("electrum: dissent batch ends at %s, not the "+
			"advertised tip %s", got, claimedTip)
	}
	return work, nil
}
