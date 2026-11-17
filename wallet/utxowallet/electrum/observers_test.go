// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"context"
	"errors"
	"math/big"
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
	// A zero Timestamp serializes as a uint32 wrap of year 1 (2042), which
	// the future-timestamp bound rejects; any fixed past time works.
	ts := time.Unix(1_600_000_000, 0)
	for nonce := seed; ; nonce++ {
		hdr := &wire.BlockHeader{Version: 4, PrevBlock: prev, Bits: bits, Nonce: nonce, Timestamp: ts}
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

// TestAuditDissent: the dissent audit validates the observer's batch as a
// branch of the store rooted below the audit window and reports its work. It
// refuses a batch that does not end at the advertised tip, does not link to
// the store below the window, breaks inside, fails a hash, or crosses a
// checkpoint on another branch.
func TestAuditDissent(t *testing.T) {
	const forkAt, start = 6, 4
	ours, theirs := forkedChain(forkAt, 12, 14)
	newNet := func(p *netparams.ChainParams) *Network {
		return &Network{cfg: &Config{Chain: testChainConfig()}, headers: storeWith(t, p, ours)}
	}
	n := newNet(paramsWithCheckpoints(nil))
	batch := theirs[start:]
	tip := theirs[len(theirs)-1].BlockHash()

	work, err := n.auditDissent(start, batch, tip)
	if err != nil {
		t.Fatalf("valid dissent rejected: %v", err)
	}
	if want := new(big.Int).Mul(blockchain.CalcWork(0x207fffff), big.NewInt(int64(len(batch)))); work.Cmp(want) != 0 {
		t.Fatalf("work %s, want %s", work, want)
	}

	if _, err := n.auditDissent(start, batch, chainhash.Hash{1}); err == nil {
		t.Fatal("batch not ending at the advertised tip accepted")
	}
	if _, err := n.auditDissent(start, nil, tip); err == nil {
		t.Fatal("empty batch accepted")
	}
	// Rooted one height too high, the batch's first header does not link
	// to the stored header below the window.
	if _, err := n.auditDissent(start+1, batch, tip); !errors.Is(err, errHeaderDoesNotConnect) {
		t.Fatalf("batch not linking to the store: got %v, want errHeaderDoesNotConnect", err)
	}

	broken := append([]*wire.BlockHeader(nil), batch...)
	bad := *broken[4]
	bad.PrevBlock = chainhash.Hash{0xde, 0xad}
	mineHeader(&bad)
	broken[4] = &bad
	broken = relink(broken, 4)
	if _, err := n.auditDissent(start, broken, broken[len(broken)-1].BlockHash()); err == nil {
		t.Fatal("batch breaking inside accepted")
	} else if height, ok := isRuleError(err); !ok || height != start+4 {
		t.Fatalf("interior break: got %v, want a RuleError at %d", err, start+4)
	}

	unmined := append([]*wire.BlockHeader(nil), batch...)
	unmined[5] = unminedHeader(unmined[4].BlockHash(), unmined[5].Timestamp)
	unmined = relink(unmined, 5)
	if height, ok := isRuleError(func() error {
		_, err := n.auditDissent(start, unmined, unmined[len(unmined)-1].BlockHash())
		return err
	}()); !ok || height != start+5 {
		t.Fatalf("unmined header was not a RuleError at %d", start+5)
	}

	// A checkpoint the store holds at 8; the dissent crosses it on its own
	// branch.
	cp := newNet(paramsWithCheckpoints([]netparams.Checkpoint{{Height: 8, Hash: ptrHash(ours[8].BlockHash())}}))
	if _, err := cp.auditDissent(start, batch, tip); !errors.Is(err, errCheckpointMismatch) {
		t.Fatalf("dissent across a checkpoint: got %v, want errCheckpointMismatch", err)
	}
}

// TestFillObserversRespectsCap: given more reachable candidates than there are
// slots, one pass fills exactly maxObservers and stops. maxObservers is
// build-tagged (see observers_desktop.go and observers_mobile.go), so the
// assertion is against the constant and holds for either build.
func TestFillObserversRespectsCap(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	const chainLen = 5
	f, params := newFakeServer(t, chainLen)
	n := newUnstartedNetwork(t, f, params)

	// Two more reachable candidates than the cap, so stopping is observable
	// rather than incidental. mineTestHeader is deterministic, so every fake
	// built to the same length serves the same chain under the same genesis
	// hash, which is what lets these pass a new node's genesis check.
	extra := make([]*ServerAddr, 0, maxObservers+2)
	for i := 0; i < cap(extra); i++ {
		peer, _ := newFakeServer(t, chainLen)
		extra = append(extra, peer.addr())
	}
	n.pool.addDiscovered("recA:1", extra)

	// The snapshot names the wallet server, the shipped fake, so every candidate
	// added above is eligible for a slot.
	n.fillObservers(f.addr().Addr)

	if count := n.ObserverCount(); count != maxObservers {
		t.Fatalf("filled %d observers, want the %d cap", count, maxObservers)
	}
}
