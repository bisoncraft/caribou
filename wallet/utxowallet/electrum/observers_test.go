// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// mineTestHeader produces a header whose hash beats its own claimed target —
// trivial at regtest-grade bits, which is exactly the attack auditDissent's
// callers must defeat via the cumulative-work comparison.
func mineTestHeader(t *testing.T, prev chainhash.Hash, bits uint32, seed uint32) *wire.BlockHeader {
	t.Helper()
	target := blockchain.CompactToBig(bits)
	for nonce := seed; ; nonce++ {
		hdr := &wire.BlockHeader{Version: 4, PrevBlock: prev, Bits: bits, Nonce: nonce}
		hash := hdr.BlockHash()
		if blockchain.HashToBig(&hash).Cmp(target) <= 0 {
			return hdr
		}
	}
}

// newUnstartedNetwork builds a Network wired to the fake server with a live
// context and an initialized observer set, without running Start — the
// minimal scaffolding for driving fillObservers and attachNode directly, the
// way the observer tick loop and tryElect invoke them.
func newUnstartedNetwork(t *testing.T, f *fakeServer, params *netparams.ChainParams) *Network {
	t.Helper()
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	n, err := NewNetwork(&Config{
		Chain:         chainCfg,
		TrustedServer: f.addr(),
		DataDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	n.ctx, n.cancel = context.WithCancel(t.Context())
	n.obsMtx.Lock()
	n.observers = make(map[string]*observer)
	n.obsMtx.Unlock()
	t.Cleanup(func() {
		n.cancel()
		n.wg.Wait()
		// Disarm any coalesced pool write before the TempDir is removed.
		n.pool.flush()
	})
	return n
}

// TestFillObserversSkipsCurrentWallet: the walletAddr argument is a snapshot
// that a mid-pass rotation can invalidate, so fillObservers re-checks the
// current wallet address at insertion time. With a stale snapshot that no
// longer names the wallet server, the wallet server must still not be
// inserted as its own observer.
func TestFillObserversSkipsCurrentWallet(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	f, params := newFakeServer(t, 5)
	n := newUnstartedNetwork(t, f, params)

	// Make the fake the CURRENT wallet server.
	node, err := NewNode(n.ctx, f.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	defer node.Close()
	n.mu.Lock()
	n.node = node
	n.mu.Unlock()

	// A snapshot from before the "rotation": it no longer matches the
	// wallet server, so the fake passes the snapshot filter and only the
	// insertion-time re-check stands between the wallet server and a slot
	// in its own observer panel.
	n.fillObservers("stale.example:50002")
	if count := n.ObserverCount(); count != 0 {
		t.Fatalf("wallet server inserted as its own observer (%d observers)", count)
	}
}

// TestFillObserversDialCap: a flooded candidate list must not wedge a
// fillObservers pass — dial attempts stop at maxObserverDialsPerPass and the
// remainder waits for the next tick.
func TestFillObserversDialCap(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	f, params := newFakeServer(t, 5)
	n := newUnstartedNetwork(t, f, params)

	// Unreachable candidates: loopback ports grabbed and released, so each
	// dial fails fast with connection-refused.
	dead := make([]*ServerAddr, 0, maxObserverDialsPerPass+4)
	for i := 0; i < cap(dead); i++ {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve port: %v", err)
		}
		addr := lis.Addr().String()
		lis.Close()
		dead = append(dead, &ServerAddr{Proto: "tcp", Addr: addr})
	}
	n.pool.addDiscovered("recA:1", dead[:len(dead)/2])
	n.pool.addDiscovered("recB:1", dead[len(dead)/2:])

	// The snapshot names the only reachable candidate (the shipped fake),
	// so every dial in this pass hits a dead server.
	n.fillObservers(f.addr().Addr)

	if count := n.ObserverCount(); count != 0 {
		t.Fatalf("%d observers connected through dead candidates", count)
	}
	failures := 0
	n.pool.mtx.Lock()
	for _, e := range n.pool.entries {
		failures += e.Failures
	}
	n.pool.mtx.Unlock()
	if failures != maxObserverDialsPerPass {
		t.Fatalf("pass dialed %d dead candidates, want the %d cap",
			failures, maxObserverDialsPerPass)
	}
}

// TestAttachNodePurgesSelfObserver: an observer at the successor's address
// inserted concurrently with the attach (a fillObservers pass racing the
// publication) is purged when the successor is published, and its drain
// release channel is closed so no spurious failure lands on the new wallet
// server's address.
func TestAttachNodePurgesSelfObserver(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	f, params := newFakeServer(t, 8)
	n := newUnstartedNetwork(t, f, params)

	// The racing observer: a live second connection to the same address.
	obsNode, err := NewNode(n.ctx, f.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode(observer): %v", err)
	}
	o := &observer{node: obsNode, stopDrain: make(chan struct{})}
	n.obsMtx.Lock()
	n.observers[f.addr().Addr] = o
	n.obsMtx.Unlock()

	node, err := NewNode(n.ctx, f.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode(successor): %v", err)
	}
	tip, err := node.SubscribeHeaders(n.ctx)
	if err != nil {
		t.Fatalf("SubscribeHeaders: %v", err)
	}
	if err := n.attachNode(node, tip); err != nil {
		t.Fatalf("attachNode: %v", err)
	}

	if count := n.ObserverCount(); count != 0 {
		t.Fatalf("observer at the new wallet address survived the attach (%d observers)", count)
	}
	select {
	case <-o.stopDrain:
	default:
		t.Fatal("stopDrain not closed on purge; the drain goroutine would record a failure")
	}
	select {
	case <-obsNode.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("purged observer connection not closed")
	}
}

func TestAuditDissent(t *testing.T) {
	const easyBits = 0x207fffff   // regtest-grade: nearly free to "mine"
	const harderBits = 0x1f00ffff // 128x more work per header

	headerHash := func(hdr *wire.BlockHeader) chainhash.Hash {
		return hdr.BlockHash()
	}

	pow := func(hdr *wire.BlockHeader) error {
		hash := hdr.BlockHash()
		if blockchain.HashToBig(&hash).Cmp(blockchain.CompactToBig(hdr.Bits)) > 0 {
			return errors.New("pow")
		}
		return nil
	}

	mkChain := func(n int, bits uint32) []*wire.BlockHeader {
		var out []*wire.BlockHeader
		prev := chainhash.Hash{}
		for i := 0; i < n; i++ {
			hdr := mineTestHeader(t, prev, bits, uint32(i)*1000)
			out = append(out, hdr)
			prev = hdr.BlockHash()
		}
		return out
	}

	linked := mkChain(4, easyBits)
	tip := linked[len(linked)-1].BlockHash()

	// A linked, PoW-valid batch audits clean and reports its work.
	work, err := auditDissent(linked, tip, headerHash, pow)
	if err != nil {
		t.Fatalf("linked batch rejected: %v", err)
	}

	// Self-attested trivial difficulty MUST lose the work comparison the
	// caller performs: 4 easy headers carry less work than one harder one.
	if harder := blockchain.CalcWork(harderBits); work.Cmp(harder) >= 0 {
		t.Fatalf("4 easy headers (%s) should not outweigh one harder header (%s)",
			work, harder)
	}

	// Broken linkage is rejected.
	broken := mkChain(4, easyBits)
	broken[2].PrevBlock = chainhash.Hash{0xde, 0xad}
	if _, err := auditDissent(broken, broken[3].BlockHash(), headerHash, pow); err == nil {
		t.Fatal("unlinked batch accepted")
	}

	// A batch not ending at the advertised tip is rejected — the observer
	// must be audited on the chain it actually advertised.
	if _, err := auditDissent(linked, chainhash.Hash{1}, headerHash, pow); err == nil {
		t.Fatal("wrong-tip batch accepted")
	}

	// Empty batches are rejected.
	if _, err := auditDissent(nil, tip, headerHash, pow); err == nil {
		t.Fatal("empty batch accepted")
	}
}
