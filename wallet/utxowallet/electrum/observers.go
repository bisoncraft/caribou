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
	// An observer ahead of us is probed at our tip height (its extension
	// is what breaks an equal-work tie) up to the reorg budget; beyond
	// that the store is stale enough that the lag path's rotation is the
	// right answer.
	//
	// Work verdicts need the store to hold the newest checkpoint: below it
	// the stored headers were never hashed, so their claimed work counts
	// for nothing (see Headers.SwapBranch). Observers run from Start,
	// before the initial sync, so the probe waits for the store; the lag
	// path below does not.
	var loss *divergenceLoss
	if !n.headers.HoldsNewestCheckpoint() {
		obs = nil
	}
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
		if n.ctx.Err() != nil {
			return
		}
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
			n.scoreFailure(sv.Addr)
			continue
		}
		ctx, cancel := n.rpcCtx()
		tip, err := node.SubscribeHeaders(ctx)
		cancel()
		if err != nil {
			node.Close()
			n.scoreFailure(sv.Addr)
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
					n.scoreFailure(o.node.Addr().Addr)
					return
				case <-n.ctx.Done():
					return
				}
			}
		}(o)
	}
}

// handleDivergence audits an observer whose chain conflicts with ours at
// conflictHeight. The dissenter must demonstrate work: its headers above the
// fork point are validated as a branch of the store (auditDissent), and their
// cumulative work must strictly exceed the store's over the same span.
// Equal work is a tie, not a verdict: same-height competing blocks carry
// identical difficulty, so acting on a tie chose a side at random, and the
// other side's observers reversed it on the next rotation (on LTC mainnet
// that walked through every shipped server and left the wallet degraded for
// hours). Returns the verified loss for the tick loop to act on once it has
// persisted, or nil.
//
// The fork point is found from the observer's own headers: first the window
// of reorgRewindDepth headers below the conflict, then, when that window
// shares no header with the store, one more request down to the deepest fork
// a verdict can act on (a successor's attach follows a fork at most
// maxReorgDepth below the tip, never below a held checkpoint). A fork deeper
// than the window is exactly what a wallet server would choose to escape the
// observers, so it is followed, not held against the observer.
//
// Only evidence against the observer costs it anything. A rule violation in
// its headers quarantines it; a failed fetch or less work demotes it. Outcomes a moving
// chain or a local condition can produce as well are inconclusive and
// re-probed next tick: the observer's chain changing between requests, its
// batch not ending at its advertised tip, the store moving during the audit,
// a timestamp past the local bound, and no header shared with the store down
// to the deepest supported fork, which says the store may need a resync.
func (n *Network) handleDivergence(o *observer, conflictHeight int32) *divergenceLoss {
	// One snapshot: the drain goroutine advances the observer tip
	// concurrently, and reading height and header separately could fetch a
	// batch for the old height while auditing against the new tip hash.
	h, tipHdr := o.tip()
	if tipHdr == nil || h < conflictHeight {
		return nil
	}
	addr := o.node.Addr().Addr
	claimedTip := n.headers.headerHash(tipHdr)
	// demote drops the observer and penalizes it for cause: a RuleError
	// quarantines, anything else (a failed fetch, less work) records a
	// failure. A nil cause is a failure. Nothing is recorded during
	// shutdown.
	demote := func(cause error, format string, args ...any) *divergenceLoss {
		p := penaltyNone
		switch {
		case cause != nil:
			p = n.penalize(addr, cause, penaltyFailure)
		case n.ctx.Err() == nil:
			p = penaltyFailure
			n.scoreFailure(addr)
		}
		verdict := "demoting"
		if p == penaltyQuarantine {
			verdict = "quarantined"
		}
		log.Warnf("electrum: observer %s diverged at height %d but "+format+"; "+verdict,
			append([]any{addr, conflictHeight}, args...)...)
		o.node.Close()
		return nil
	}
	inconclusive := func(format string, args ...any) *divergenceLoss {
		log.Infof("electrum: divergence audit of observer %s at height %d is "+
			"inconclusive: "+format+"; re-probing next tick",
			append([]any{addr, conflictHeight}, args...)...)
		return nil
	}

	// The store state the verdict is judged against. A store that moves
	// before the verdict makes the audit inconclusive.
	localTip, localHash := n.headers.tipSnapshot()
	floor := max(n.headers.lowestFork(), localTip-maxReorgDepth)
	if conflictHeight <= floor {
		return inconclusive("the conflict lies at or below the deepest supported fork %d", floor)
	}
	// Advertised heights are untrusted. With h at most maxReorgDepth above
	// the tip and every request starting at or above floor, no request
	// exceeds 2*maxReorgDepth+1 headers.
	if h > localTip+maxReorgDepth {
		return inconclusive("advertised tip %d is more than %d above ours", h, maxReorgDepth)
	}
	start := max(conflictHeight-reorgRewindDepth, floor)
	shallow, err := n.fetchObserverHeaders(o, start, h-start+1)
	if err != nil {
		return demote(err, "failed the work audit (header fetch: %v)", err)
	}
	if err := n.linkedWithin(start, shallow); err != nil {
		return demote(err, "failed the work audit: %v", err)
	}
	if len(shallow) != int(h-start)+1 || n.headers.headerHash(shallow[len(shallow)-1]) != claimedTip {
		return inconclusive("the batch for heights %d-%d does not end at the advertised tip", start, h)
	}

	headers, from := shallow, start
	fork, found := n.sharedHeight(from, headers, conflictHeight-1)
	if !found && start > floor {
		lower, err := n.fetchObserverHeaders(o, floor, start-floor)
		if err != nil {
			return demote(err, "failed the work audit (header fetch: %v)", err)
		}
		if err := n.linkedWithin(floor, lower); err != nil {
			return demote(err, "failed the work audit: %v", err)
		}
		if len(lower) != int(start-floor) {
			return inconclusive("the batch for heights %d-%d holds %d headers", floor, start-1, len(lower))
		}
		// The two responses are separate snapshots of the observer's
		// chain. A seam between them is the observer reorging between
		// requests, not a rule violation.
		if shallow[0].PrevBlock != n.headers.headerHash(lower[len(lower)-1]) {
			return inconclusive("its chain moved between the requests")
		}
		headers, from = append(lower, shallow...), floor
		fork, found = n.sharedHeight(from, headers, conflictHeight-1)
	}
	if !found {
		// A dissenter that contradicts a checkpoint is at fault.
		for i, hdr := range headers {
			if err := n.headers.validateCheckpoint(from+int32(i), hdr); err != nil {
				return demote(err, "failed the work audit: %v", err)
			}
		}
		log.Errorf("electrum: the header store shares no header with observer %s "+
			"from height %d to %d, the deepest fork a reorg can follow; if the "+
			"observer is right, the store is on a deeper fork and needs a resync",
			addr, from, conflictHeight)
		return nil
	}

	dissentWork, err := n.auditDissent(fork+1, headers[fork+1-from:], claimedTip)
	var ruleErr *RuleError
	switch {
	case errors.As(err, &ruleErr):
		return demote(err, "failed the work audit: %v", err)
	case err != nil:
		// errTimeTooNew (local clock skew produces it as well), or the
		// store moved under the branch's root.
		return inconclusive("%v", err)
	}
	theirs := n.headers.headerHash(headers[conflictHeight-from])
	ours := n.headers.ByHeight(conflictHeight)
	localWork := n.headers.workRange(fork+1, localTip)
	if tip, hash := n.headers.tipSnapshot(); tip != localTip || hash != localHash {
		return inconclusive("the header store moved during the audit")
	}
	if ours == nil || n.headers.headerHash(ours) == theirs {
		log.Debugf("electrum: divergence with %s at height %d resolved during "+
			"the audit", addr, conflictHeight)
		return nil
	}
	oursHash := n.headers.headerHash(ours)

	// Self-attested Bits are only meaningful in comparison: a header's PoW
	// check proves the hash beats its OWN claimed target, so a liar can
	// claim trivial difficulty and "pass". The dissent is credible only if
	// its cumulative work above the fork exceeds our own chain's there;
	// claiming low difficulty then loses the comparison, and claiming high
	// difficulty fails the hash check.
	switch dissentWork.Cmp(localWork) {
	case -1:
		return demote(nil, "presents a linked, PoW-valid chain with less cumulative work "+
			"above fork %d than ours (%s < %s); stale or losing fork", fork, dissentWork, localWork)
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
	log.Warnf("electrum: observer %s presents a chain with more work above fork %d "+
		"(%s vs ours %s at height %d); wallet server %s is on the losing side", addr,
		fork, theirs, oursHash, conflictHeight, walletNode.Addr().Addr)
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

// fetchObserverHeaders fetches up to count headers from the observer starting
// at from.
func (n *Network) fetchObserverHeaders(o *observer, from, count int32) ([]*wire.BlockHeader, error) {
	ctx, cancel := n.rpcCtx()
	res, err := o.node.BlockHeaders(ctx, from, int(count))
	cancel()
	if err != nil {
		return nil, err
	}
	if res.Count == 0 {
		return nil, nil
	}
	raw, err := hex.DecodeString(res.HexConcat)
	if err != nil {
		return nil, fmt.Errorf("undecodable header batch: %w", err)
	}
	if len(raw)%n.cfg.Chain.HeaderSize != 0 {
		return nil, fmt.Errorf("header batch length %d not a multiple of %d", len(raw), n.cfg.Chain.HeaderSize)
	}
	return n.headers.parseBatch(raw)
}

// linkedWithin returns a *RuleError for the first header of hdrs, the
// observer's headers at heights from and up in one response, that does not
// link to the header before it. A response that contradicts itself is the
// observer's fault.
func (n *Network) linkedWithin(from int32, hdrs []*wire.BlockHeader) error {
	for i := 1; i < len(hdrs); i++ {
		if hdrs[i].PrevBlock != n.headers.headerHash(hdrs[i-1]) {
			return &RuleError{Height: from + int32(i), Err: errors.New("observer batch does not link")}
		}
	}
	return nil
}

// sharedHeight returns the highest height at or below top whose header in
// hdrs, the observer's headers at heights from and up, is also the store's.
func (n *Network) sharedHeight(from int32, hdrs []*wire.BlockHeader, top int32) (int32, bool) {
	for height := min(top, from+int32(len(hdrs))-1); height >= from; height-- {
		ours := n.headers.ByHeight(height)
		if ours != nil && n.headers.headerHash(ours) == n.headers.headerHash(hdrs[height-from]) {
			return height, true
		}
	}
	return 0, false
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
	n.scoreFailure(loss.walletAddr)
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

// auditDissent validates a dissenting observer's header batch, its headers at
// heights start and up, as a branch of the store rooted at the stored header
// below start (Headers.validateBranch): linkage to that header and through
// the batch, checkpoints, the target range, the consensus difficulty bits,
// the timestamp bound and the PoW hash of every header. Rooting at the store
// keeps the caller's work comparison over one span: a batch that does not
// link there belongs to a fork deeper than the audit window, and its work
// says nothing about the window. The batch must also end at claimedTip, the
// chain the observer advertised. It returns the batch's cumulative work,
// which the caller compares against the store's over the same span; a
// header's PoW only proves its hash beats its own claimed target.
func (n *Network) auditDissent(start int32, batch []*wire.BlockHeader, claimedTip chainhash.Hash) (*big.Int, error) {
	if len(batch) == 0 {
		return nil, errors.New("electrum: empty dissent batch")
	}
	if got := n.headers.headerHash(batch[len(batch)-1]); got != claimedTip {
		return nil, fmt.Errorf("electrum: dissent batch ends at %s, not the "+
			"advertised tip %s", got, claimedTip)
	}
	return n.headers.validateBranch(start-1, batch)
}
