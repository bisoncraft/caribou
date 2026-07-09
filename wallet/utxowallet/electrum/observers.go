// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"encoding/hex"
	"sync"
	"time"

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

	// observerCompareInterval is how often observer tips are compared
	// against the local chain.
	observerCompareInterval = 30 * time.Second

	// lagWindow converts to the lag threshold in blocks: the wallet server
	// is a lag candidate when the best observer is at least this much work
	// time ahead at the chain's target spacing. Two blocks mined seconds
	// apart and seen by an observer first must not trigger a switch, so
	// the lag must also persist for lagPatience before rotating.
	lagWindow   = 20 * time.Minute
	lagPatience = 10 * time.Minute
)

// ObserverCount reports the number of live observer connections; primarily
// a test and diagnostics hook.
func (n *Network) ObserverCount() int { return int(n.observerCount.Load()) }

// observer is one header-only server connection.
type observer struct {
	node *Node

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
func (n *Network) runObservers(walletAddr string) {
	defer n.wg.Done()

	obs := make(map[string]*observer)
	defer func() {
		for _, o := range obs {
			o.node.Close()
		}
	}()

	var lagSince time.Time
	ticker := time.NewTicker(observerCompareInterval)
	defer ticker.Stop()

	for {
		n.fillObservers(obs, walletAddr)

		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
		}

		// Drop dead observers (demoted at death by their drain
		// goroutines); fillObservers replaces them next pass.
		for addr, o := range obs {
			select {
			case <-o.node.Done():
				delete(obs, addr)
				n.observerCount.Add(-1)
			default:
			}
		}

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
				recordServerFailure(walletAddr)
				n.Stop()
				return
			}
		} else {
			lagSince = time.Time{}
		}
	}
}

// fillObservers tops the set up to maxObservers from the candidates not in
// use, in health-scored order.
func (n *Network) fillObservers(obs map[string]*observer, walletAddr string) {
	if len(obs) >= maxObservers {
		return
	}
	for _, sv := range orderCandidates(n.candidates()) {
		if len(obs) >= maxObservers {
			break
		}
		if sv.Addr == walletAddr {
			continue
		}
		if _, ok := obs[sv.Addr]; ok {
			continue
		}
		node, err := NewNode(n.ctx, sv, n.cfg.TorProxy,
			n.cfg.Chain.Params.GenesisHash.String())
		if err != nil {
			recordServerFailure(sv.Addr)
			continue
		}
		tip, err := node.SubscribeHeaders(n.ctx)
		if err != nil {
			node.Close()
			recordServerFailure(sv.Addr)
			continue
		}
		o := &observer{node: node}
		o.setTip(tip)
		obs[sv.Addr] = o
		n.observerCount.Add(1)
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
				case <-o.node.Done():
					recordServerFailure(o.node.Addr().Addr)
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
	h, _ := o.tip()
	start := h - reorgRewindDepth
	if start < n.headers.StartHeight() {
		start = n.headers.StartHeight()
	}
	res, err := o.node.BlockHeaders(n.ctx, start, int(h-start)+1)
	if err != nil {
		recordServerFailure(o.node.Addr().Addr)
		o.node.Close()
		return
	}
	raw, err := hex.DecodeString(res.HexConcat)
	if err != nil || len(raw) < n.cfg.Chain.HeaderSize {
		recordServerFailure(o.node.Addr().Addr)
		o.node.Close()
		return
	}
	for off := 0; off+n.cfg.Chain.HeaderSize <= len(raw); off += n.cfg.Chain.HeaderSize {
		var hdr wire.BlockHeader
		if err := hdr.Deserialize(bytes.NewReader(raw[off : off+n.cfg.Chain.HeaderSize])); err != nil {
			recordServerFailure(o.node.Addr().Addr)
			o.node.Close()
			return
		}
		if err := n.headers.checkPoW(&hdr); err != nil {
			// Fabricated chain: demote and drop the observer, keep
			// the wallet stream untouched.
			log.Warnf("electrum: observer %s served a chain without valid "+
				"work; demoting: %v", o.node.Addr().Addr, err)
			recordServerFailure(o.node.Addr().Addr)
			o.node.Close()
			return
		}
	}
	// The dissenting chain carries real work: our wallet server may be on
	// the wrong chain. Suspend spends and rotate; the rebuilt network
	// re-elects and re-validates.
	log.Warnf("electrum: observer %s presents a conflicting chain with valid "+
		"work at height %d; suspending spends and rotating", o.node.Addr().Addr, h)
	n.spendSuspect.Store(true)
	recordServerFailure(walletAddr)
	n.Stop()
}
