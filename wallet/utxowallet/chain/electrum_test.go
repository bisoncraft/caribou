// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/electrum"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// stubNetwork is a hand-rolled ElectrumNetwork that the tests can drive
// deterministically. It owns its own header chain and notification channels;
// no actual electrumx server is contacted.
type stubNetwork struct {
	lyingMerkle bool
	mu          sync.Mutex
	headers     []*wire.BlockHeader
	hashIdx     map[chainhash.Hash]int32
	synced      bool
	subs        map[string]string // scripthash → status
	history     map[string][]electrum.HistoryEntry
	rawTxs      map[string]string // txid → hex
	bcasted     []string
	bcastErr    error

	// subscribeHook, if non-nil, is invoked at the top of each
	// SubscribeScripthash call (under s.mu). The returned error is
	// returned to the caller. count is the 1-based call index. Tests use
	// this to inject failures and to gate ordering of concurrent
	// subscribers.
	subscribeHook func(count int, sh string) error
	subscribeN    int

	tipCh        chan *electrum.TipChange
	scripthashCh chan *electrum.ScripthashStatus
	done         chan struct{}
}

// pushConnected enqueues a TipChange with a single Connected height. Tests
// use this in place of the prior "send an int32 height" form.
func (s *stubNetwork) pushConnected(t *testing.T, h int32) {
	t.Helper()
	s.tipCh <- &electrum.TipChange{Connected: []int32{h}}
}

// pushDisconnected enqueues a TipChange with the given disconnected blocks
// (newest-first), driving the chain client's BlockDisconnected emission path.
func (s *stubNetwork) pushDisconnected(t *testing.T, blocks ...electrum.DisconnectedBlock) {
	t.Helper()
	s.tipCh <- &electrum.TipChange{Disconnected: blocks}
}

func newStubNetwork() *stubNetwork {
	return &stubNetwork{
		hashIdx:      map[chainhash.Hash]int32{},
		subs:         map[string]string{},
		history:      map[string][]electrum.HistoryEntry{},
		rawTxs:       map[string]string{},
		tipCh:        make(chan *electrum.TipChange, 8),
		scripthashCh: make(chan *electrum.ScripthashStatus, 8),
		done:         make(chan struct{}),
	}
}

// addBlock appends a header to the stub chain. The header is synthesized so
// that PrevBlock chains correctly.
func (s *stubNetwork) addBlock(t *testing.T, ts time.Time) *wire.BlockHeader {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var prev chainhash.Hash
	if len(s.headers) > 0 {
		prev = s.headers[len(s.headers)-1].BlockHash()
	}
	hdr := &wire.BlockHeader{
		Version:   1,
		PrevBlock: prev,
		Timestamp: ts,
		Bits:      0x207fffff,
		Nonce:     uint32(len(s.headers)),
	}
	s.hashIdx[hdr.BlockHash()] = int32(len(s.headers))
	s.headers = append(s.headers, hdr)
	return hdr
}

// rewindTo drops every header above `height`, modelling a reorg in which the
// server's chain diverges below our tip. After this, Tip() reports `height` and
// the dropped heights are unknown to HeaderByHeight — so the chain client's
// contiguous reconnect path delivers the NEW chain that addBlock appends next.
func (s *stubNetwork) rewindTo(t *testing.T, height int32) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := int(height) + 1; i < len(s.headers); i++ {
		delete(s.hashIdx, s.headers[i].BlockHash())
	}
	s.headers = s.headers[:height+1]
}

func (s *stubNetwork) Start(_ context.Context) error { return nil }
func (s *stubNetwork) Stop()                         { close(s.done) }
func (s *stubNetwork) Done() <-chan struct{}         { return s.done }
func (s *stubNetwork) WaitForShutdown()              { <-s.done }

func (s *stubNetwork) StartHeight() int32 { return 0 }

func (s *stubNetwork) Tip() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int32(len(s.headers)) - 1
}

func (s *stubNetwork) Synced() bool { return s.synced }

// TargetTip returns the stub's server-reported tip (same as Tip for the stub,
// since there is no separate notion of "server ahead of local" in tests that
// don't exercise the sync-in-progress scenario).
func (s *stubNetwork) TargetTip() int32 { return s.Tip() }

func (s *stubNetwork) HeaderByHeight(height int32) *wire.BlockHeader {
	s.mu.Lock()
	defer s.mu.Unlock()
	if height < 0 || int(height) >= len(s.headers) {
		return nil
	}
	// Fix the header's merkle root before handing it out, then return a
	// COPY. The real Network serves headers read-only; the stub instead
	// derives each seeded height's root from its leaves. Stamping here — the
	// single choke point every header consumer passes through, including the
	// single-threaded pre-fan-out defer/flush checks and both concurrent
	// fan-out readers — and returning a copy keeps the s.headers mutation
	// entirely under s.mu, so the lock-free header serialization in
	// buildRelevantTx never races a concurrent stamp.
	s.stampMerkleRootLocked(height)
	hdr := *s.headers[height]
	return &hdr
}

// leavesAtHeight returns the distinct txids the stub serves at the given
// height, sorted for determinism. Callers must hold s.mu.
func (s *stubNetwork) leavesAtHeight(height int32) ([]chainhash.Hash, error) {
	seen := map[string]struct{}{}
	var leaves []chainhash.Hash
	for _, entries := range s.history {
		for _, e := range entries {
			if e.Height != height {
				continue
			}
			if _, ok := seen[e.TxHash]; ok {
				continue
			}
			seen[e.TxHash] = struct{}{}
			h, err := chainhash.NewHashFromStr(e.TxHash)
			if err != nil {
				return nil, err
			}
			leaves = append(leaves, *h)
		}
	}
	sort.Slice(leaves, func(i, j int) bool {
		return leaves[i].String() < leaves[j].String()
	})
	return leaves, nil
}

// stampMerkleRootLocked fixes the header at height to the merkle root of the
// tree over that height's leaves, so a GetMerkle proof folds to it. It is a
// no-op for a height with no seeded leaves, preserving any root a test set
// directly. Callers must hold s.mu.
func (s *stubNetwork) stampMerkleRootLocked(height int32) {
	if height < 0 || int(height) >= len(s.headers) {
		return
	}
	leaves, err := s.leavesAtHeight(height)
	if err != nil || len(leaves) == 0 {
		return
	}
	root, _, _, err := stubMerkle(leaves, leaves[0].String())
	if err != nil {
		return
	}
	s.headers[height].MerkleRoot = root
}

func (s *stubNetwork) HeaderByHash(h *chainhash.Hash) (int32, *wire.BlockHeader, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.hashIdx[*h]
	if !ok {
		return 0, nil, false
	}
	return idx, s.headers[idx], true
}

func (s *stubNetwork) TipHash() chainhash.Hash {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.headers) == 0 {
		return chainhash.Hash{}
	}
	return s.headers[len(s.headers)-1].BlockHash()
}

func (s *stubNetwork) TipChangeChan() <-chan *electrum.TipChange         { return s.tipCh }
func (s *stubNetwork) ScripthashChan() <-chan *electrum.ScripthashStatus { return s.scripthashCh }

func (s *stubNetwork) SubscribeScripthash(_ context.Context, sh string) (*electrum.ScripthashStatus, error) {
	s.mu.Lock()
	s.subscribeN++
	hook := s.subscribeHook
	n := s.subscribeN
	s.mu.Unlock()
	if hook != nil {
		if err := hook(n, sh); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.subs[sh]
	return &electrum.ScripthashStatus{Scripthash: sh, Status: status}, nil
}

func (s *stubNetwork) UnsubscribeScripthash(context.Context, string) {}

func (s *stubNetwork) GetHistory(_ context.Context, sh string) ([]electrum.HistoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]electrum.HistoryEntry(nil), s.history[sh]...), nil
}

func (s *stubNetwork) ListUnspent(_ context.Context, _ string) ([]electrum.UnspentEntry, error) {
	return nil, nil
}

// GetMerkle returns a genuine inclusion proof against a real merkle tree the
// stub builds from every txid it serves at the height — so distinct txids in
// one block fold, via distinct branches, to a single shared root. It is a pure
// reader: the header's merkle root is stamped by HeaderByHeight (which every
// verifyInclusion call goes through first), so GetMerkle never mutates a header
// and cannot race the concurrent header serialization in buildRelevantTx. Tests
// exercising a LYING server override stub behavior instead.
func (s *stubNetwork) GetMerkle(_ context.Context, txid string, height int32) (*electrum.MerkleProof, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lyingMerkle {
		// Empty branch: the fold yields the txid, deliberately NOT the
		// header root, modeling a server that cannot prove inclusion.
		return &electrum.MerkleProof{BlockHeight: height, Pos: 0}, nil
	}
	if height < 0 || int(height) >= len(s.headers) {
		return nil, fmt.Errorf("stub: no header at %d", height)
	}
	leaves, err := s.leavesAtHeight(height)
	if err != nil {
		return nil, err
	}
	_, branch, pos, err := stubMerkle(leaves, txid)
	if err != nil {
		return nil, err
	}
	sib := make([]string, len(branch))
	for i, b := range branch {
		sib[i] = b.String()
	}
	return &electrum.MerkleProof{BlockHeight: height, Merkle: sib, Pos: pos}, nil
}

// stubMerkle builds a bitcoin-style merkle tree over leaves and returns the
// root plus the inclusion branch and position for target.
func stubMerkle(leaves []chainhash.Hash, target string) (chainhash.Hash, []chainhash.Hash, uint32, error) {
	tgt, err := chainhash.NewHashFromStr(target)
	if err != nil {
		return chainhash.Hash{}, nil, 0, err
	}
	pos := -1
	for i, l := range leaves {
		if l == *tgt {
			pos = i
			break
		}
	}
	if pos < 0 {
		return chainhash.Hash{}, nil, 0, fmt.Errorf("stub: %s not among height leaves", target)
	}
	level := append([]chainhash.Hash(nil), leaves...)
	var branch []chainhash.Hash
	idx := pos
	for len(level) > 1 {
		if len(level)%2 == 1 {
			level = append(level, level[len(level)-1])
		}
		sibIdx := idx ^ 1
		branch = append(branch, level[sibIdx])
		var next []chainhash.Hash
		for i := 0; i < len(level); i += 2 {
			var cat [64]byte
			copy(cat[:32], level[i][:])
			copy(cat[32:], level[i+1][:])
			next = append(next, chainhash.DoubleHashH(cat[:]))
		}
		level = next
		idx /= 2
	}
	return level[0], branch, uint32(pos), nil
}

func (s *stubNetwork) GetRawTransaction(_ context.Context, txid string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hex, ok := s.rawTxs[txid]
	if !ok {
		return "", errors.New("no such tx: " + txid)
	}
	return hex, nil
}

func (s *stubNetwork) GetTransactionVerbose(context.Context, string) (*electrum.VerboseTx, error) {
	return nil, errors.New("not implemented in stub")
}

func (s *stubNetwork) Broadcast(_ context.Context, raw string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bcastErr != nil {
		return "", s.bcastErr
	}
	s.bcasted = append(s.bcasted, raw)
	return "", nil
}

func (s *stubNetwork) EstimateFee(context.Context, int32) (int64, error) {
	return 1000, nil
}

// ----------------------------------------------------------------------------

// testParams returns a minimal *netparams.ChainParams suitable for these
// tests. Only the fields the chain package actually touches are populated;
// notably we set Net so BTCDParams() builds a usable *chaincfg.Params that
// txscript.ExtractPkScriptAddrs accepts.
func testParams() *netparams.ChainParams {
	return &netparams.ChainParams{
		Chain:                   "btc",
		Net:                     chaincfg.SimNetParams.Net,
		GenesisBlock:            chaincfg.SimNetParams.GenesisBlock,
		GenesisHash:             chaincfg.SimNetParams.GenesisHash,
		PubKeyHashAddrID:        chaincfg.SimNetParams.PubKeyHashAddrID,
		ScriptHashAddrID:        chaincfg.SimNetParams.ScriptHashAddrID,
		WitnessPubKeyHashAddrID: chaincfg.SimNetParams.WitnessPubKeyHashAddrID,
		WitnessScriptHashAddrID: chaincfg.SimNetParams.WitnessScriptHashAddrID,
		Bech32HRPSegwit:         chaincfg.SimNetParams.Bech32HRPSegwit,
	}
}

// makeClient constructs an ElectrumClient against a stubNetwork seeded with
// n blocks. The returned client has been Started, the initial
// ClientConnected and BlockConnected (mirroring what the real *electrum.Network
// emits at Start time) have already been drained from the notification
// channel, and the caller is responsible only for Stop()/WaitForShutdown.
func makeClient(t *testing.T, blocks int) (*ElectrumClient, *stubNetwork) {
	t.Helper()
	stub := newStubNetwork()
	for i := range blocks {
		stub.addBlock(t, time.Unix(int64(1_700_000_000+i*60), 0))
	}
	stub.synced = true

	c, err := NewElectrumClient(testParams(), stub)
	if err != nil {
		t.Fatalf("NewElectrumClient: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		c.Stop()
		c.WaitForShutdown()
	})
	// ClientConnected is always enqueued by Start; drain it so tests
	// start from a stable baseline.
	drain(t, c, 1, time.Second)
	// These unit tests exercise live tip delivery and reorgs, not the
	// initial-sync rescan gate (covered by the harness). Mark the client live
	// at one block behind the tip — as the first rescan's handoff would — so the
	// initial tip-change below and the tests' subsequent pushes are dispatched.
	c.connMtx.Lock()
	c.liveDelivery = true
	c.lastConnected = stub.Tip() - 1
	c.connMtx.Unlock()
	if blocks > 0 {
		// Mirror network.headersLoop's "initial tip-change after
		// applying the subscription's initial reply" emission, and
		// drain the resulting BlockConnected. This guarantees bs is
		// populated by the time the caller looks at it.
		stub.pushConnected(t, stub.Tip())
		drain(t, c, 1, time.Second)
	}
	return c, stub
}

// drain blocks until n notifications have been read or until timeout.
func drain(t *testing.T, c *ElectrumClient, n int, d time.Duration) []any {
	t.Helper()
	out := make([]any, 0, n)
	timeout := time.After(d)
	for len(out) < n {
		select {
		case ntfn, ok := <-c.Notifications():
			if !ok {
				t.Fatalf("notifications channel closed after %d ntfns", len(out))
			}
			out = append(out, ntfn)
		case <-timeout:
			t.Fatalf("timeout waiting for %d notifications; got %d", n, len(out))
		}
	}
	return out
}

// newGatedClient starts a client in its real initial state: the live-tip gate is
// CLOSED (liveDelivery false), as it is before the first rescan completes. Unlike
// makeClient it does not mark the client live, so tests can exercise tip
// suppression and the rescan->live handoff.
func newGatedClient(t *testing.T, blocks int) (*ElectrumClient, *stubNetwork) {
	t.Helper()
	stub := newStubNetwork()
	for i := range blocks {
		stub.addBlock(t, time.Unix(int64(1_700_000_000+i*60), 0))
	}
	stub.synced = true
	c, err := NewElectrumClient(testParams(), stub)
	if err != nil {
		t.Fatalf("NewElectrumClient: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		c.Stop()
		c.WaitForShutdown()
	})
	drain(t, c, 1, time.Second) // ClientConnected
	return c, stub
}

// assertNoNtfn fails if any notification arrives within d. Used to assert the
// live-tip gate is suppressing BlockConnected.
func assertNoNtfn(t *testing.T, c *ElectrumClient, d time.Duration) {
	t.Helper()
	select {
	case n, ok := <-c.Notifications():
		if ok {
			t.Fatalf("unexpected notification while gate closed: %#v", n)
		}
	case <-time.After(d):
	}
}

func TestElectrumBackEndAndCapability(t *testing.T) {
	c, _ := makeClient(t, 1)
	if c.BackEnd() != "electrum" {
		t.Fatalf("BackEnd = %q", c.BackEnd())
	}
	if _, ok := any(c).(ElectrumBackend); !ok {
		t.Fatal("ElectrumClient should satisfy ElectrumBackend")
	}
	found := false
	for _, b := range BackEnds() {
		if b == ElectrumBackEnd {
			found = true
		}
	}
	if !found {
		t.Fatal("electrum not in BackEnds()")
	}
}

func TestElectrumGetBestBlock(t *testing.T) {
	c, stub := makeClient(t, 5)
	hash, height, err := c.GetBestBlock()
	if err != nil {
		t.Fatalf("GetBestBlock: %v", err)
	}
	if height != 4 {
		t.Fatalf("height = %d, want 4", height)
	}
	want := stub.headers[4].BlockHash()
	if *hash != want {
		t.Fatalf("hash mismatch: got %s, want %s", hash, want)
	}
}

func TestElectrumGetBlockHashAndHeader(t *testing.T) {
	c, stub := makeClient(t, 3)
	for i := range int64(3) {
		h, err := c.GetBlockHash(i)
		if err != nil {
			t.Fatalf("GetBlockHash(%d): %v", i, err)
		}
		want := stub.headers[i].BlockHash()
		if *h != want {
			t.Fatalf("hash[%d] mismatch", i)
		}
		hdr, err := c.GetBlockHeader(h)
		if err != nil {
			t.Fatalf("GetBlockHeader: %v", err)
		}
		if hdr.BlockHash() != want {
			t.Fatalf("header[%d] hash mismatch", i)
		}
	}
	if _, err := c.GetBlockHash(99); err == nil {
		t.Fatal("expected error for out-of-range height")
	}
	bad := chainhash.Hash{31: 1}
	if _, err := c.GetBlockHeader(&bad); err == nil {
		t.Fatal("expected error for unknown hash")
	}
}

func TestElectrumGetBlockUnimplemented(t *testing.T) {
	c, _ := makeClient(t, 1)
	if _, err := c.GetBlock(&chainhash.Hash{}); !errors.Is(err, ErrUnimplemented) {
		t.Fatalf("expected ErrUnimplemented, got %v", err)
	}
}

func TestElectrumTipChangeBecomesBlockConnected(t *testing.T) {
	c, stub := makeClient(t, 3)
	// makeClient already drained the initial ClientConnected and
	// startup BlockConnected.
	// Add a new block and push a tip-change.
	stub.addBlock(t, time.Unix(1_700_000_999, 0))
	stub.pushConnected(t, 3)
	// Expect a BlockConnected with height 3.
	got := drain(t, c, 1, 2*time.Second)[0]
	bc, ok := got.(BlockConnected)
	if !ok {
		t.Fatalf("ntfn = %T, want BlockConnected", got)
	}
	if bc.Height != 3 {
		t.Fatalf("BlockConnected.Height = %d, want 3", bc.Height)
	}
}

func TestElectrumBlockStamp(t *testing.T) {
	c, stub := makeClient(t, 2)
	// After makeClient, the initial BlockConnected at Tip()==1 has been
	// drained; bs is the resulting block stamp.
	bs, err := c.BlockStamp()
	if err != nil {
		t.Fatalf("BlockStamp: %v", err)
	}
	if bs.Height != 1 {
		t.Fatalf("initial BlockStamp.Height = %d, want 1", bs.Height)
	}

	// Push a tip-change; BlockStamp should advance after we read the
	// BlockConnected notification.
	stub.addBlock(t, time.Unix(1_700_001_000, 0))
	stub.pushConnected(t, 2)
	drain(t, c, 1, time.Second) // consume BlockConnected
	bs, err = c.BlockStamp()
	if err != nil {
		t.Fatalf("BlockStamp after tip: %v", err)
	}
	if bs.Height != 2 {
		t.Fatalf("BlockStamp.Height = %d, want 2", bs.Height)
	}
}

func TestElectrumNotifyReceivedAndRelevantTx(t *testing.T) {
	c, stub := makeClient(t, 3)
	// makeClient already drained initial ClientConnected + BlockConnected.

	addr, sh := newSimnetAddr(t)

	// Pre-seed the stub: subscribe will return empty status, then a
	// status-change notification triggers history fetch + RelevantTx.
	txid, rawHex := synthTx(t)
	stub.history[sh] = []electrum.HistoryEntry{{Height: 2, TxHash: txid}}
	stub.rawTxs[txid] = rawHex

	if err := c.NotifyReceived([]btcutil.Address{addr}); err != nil {
		t.Fatalf("NotifyReceived: %v", err)
	}

	// Push a status notification — the client should fetch the history
	// and emit a RelevantTx.
	stub.scripthashCh <- &electrum.ScripthashStatus{Scripthash: sh, Status: "changed"}

	ntfns := drain(t, c, 1, 2*time.Second)
	rt, ok := ntfns[0].(RelevantTx)
	if !ok {
		t.Fatalf("ntfn = %T, want RelevantTx", ntfns[0])
	}
	if rt.Block == nil || rt.Block.Height != 2 {
		t.Fatalf("RelevantTx.Block = %+v, want height 2", rt.Block)
	}
	if rt.TxRecord == nil {
		t.Fatal("RelevantTx.TxRecord nil")
	}
}

// TestElectrumSubscribeEmitsInitialPendingTx is the regression for review2 #5:
// when a tx is already in the server's history at subscribe time, the wallet
// must see it. Pre-fix, SubscribeScripthash returned the current status
// synchronously, no initial notification was enqueued, and a mempool tx would
// stay invisible until it confirmed.
func TestElectrumSubscribeEmitsInitialPendingTx(t *testing.T) {
	c, stub := makeClient(t, 3)

	addr, sh := newSimnetAddr(t)

	// Pre-seed history with a pending (height=0) tx.
	txid, rawHex := synthTxToAddr(t, addr)
	stub.history[sh] = []electrum.HistoryEntry{{Height: 0, TxHash: txid}}
	stub.rawTxs[txid] = rawHex
	// And a non-empty status so subscribeAddress triggers the initial
	// diff path.
	stub.subs[sh] = "initial"

	if err := c.NotifyReceived([]btcutil.Address{addr}); err != nil {
		t.Fatalf("NotifyReceived: %v", err)
	}

	// The first emission after subscribe should be a RelevantTx for the
	// pending tx, with no block meta.
	ntfns := drain(t, c, 1, 2*time.Second)
	rt, ok := ntfns[0].(RelevantTx)
	if !ok {
		t.Fatalf("ntfn = %T, want RelevantTx", ntfns[0])
	}
	if rt.Block != nil {
		t.Fatalf("RelevantTx.Block = %+v, want nil for mempool", rt.Block)
	}
	if rt.TxRecord == nil || rt.TxRecord.Hash.String() != txid {
		t.Fatalf("RelevantTx.TxRecord txid = %v, want %s", rt.TxRecord, txid)
	}
}

func TestElectrumNotifyBlocksIsNoop(t *testing.T) {
	c, _ := makeClient(t, 1)
	if err := c.NotifyBlocks(); err != nil {
		t.Fatalf("NotifyBlocks returned %v", err)
	}
}

func TestElectrumSendRawTransaction(t *testing.T) {
	c, stub := makeClient(t, 1)
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0}})
	tx.AddTxOut(&wire.TxOut{Value: 100, PkScript: []byte{0x51}})
	hash, err := c.SendRawTransaction(tx, false)
	if err != nil {
		t.Fatalf("SendRawTransaction: %v", err)
	}
	want := tx.TxHash()
	if *hash != want {
		t.Fatal("returned hash mismatch")
	}
	if len(stub.bcasted) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(stub.bcasted))
	}
}

// TestElectrumTipChangeMultiBlock is the regression for review2 #3: a single
// network tip-change event covering N blocks must produce N
// BlockConnected notifications, not just one for the final height.
// waddrmgr.PutSyncedTo chains prev-hash → hash, so a single multi-block
// notification would roll back and leave the wallet stuck at the old tip.
func TestElectrumTipChangeMultiBlock(t *testing.T) {
	c, stub := makeClient(t, 2)
	// Add 3 more blocks; deliver a single TipChange covering all of them.
	for i := range 3 {
		stub.addBlock(t, time.Unix(int64(1_700_000_000+(2+i)*60), 0))
	}
	stub.tipCh <- &electrum.TipChange{Connected: []int32{2, 3, 4}}

	got := drain(t, c, 3, 2*time.Second)
	for i, n := range got {
		bc, ok := n.(BlockConnected)
		if !ok {
			t.Fatalf("ntfn[%d] = %T, want BlockConnected", i, n)
		}
		if want := int32(2 + i); bc.Height != want {
			t.Errorf("ntfn[%d].Height = %d, want %d", i, bc.Height, want)
		}
	}
}

// TestElectrumBlockDisconnectedOnRewind is the regression for review2 #1:
// when the network emits a TipChange with Disconnected entries, the chain
// client must emit a BlockDisconnected for each — otherwise the wallet's
// orphan rollback path (disconnectBlock → TxStore.Rollback) is never
// driven and orphaned txs stay confirmed until the next process restart.
func TestElectrumBlockDisconnectedOnRewind(t *testing.T) {
	c, stub := makeClient(t, 5)

	// A pure rewind with no replacement chain yet: the tip drops to height 1, so
	// the contiguous reconnect path has nothing to re-deliver and we see only
	// the disconnects.
	stub.rewindTo(t, 1)
	disc := []electrum.DisconnectedBlock{
		{Hash: [32]byte{1}, Height: 4, Time: 1_700_001_000},
		{Hash: [32]byte{2}, Height: 3, Time: 1_700_000_940},
		{Hash: [32]byte{3}, Height: 2, Time: 1_700_000_880},
	}
	stub.pushDisconnected(t, disc...)

	got := drain(t, c, 3, 2*time.Second)
	for i, n := range got {
		bd, ok := n.(BlockDisconnected)
		if !ok {
			t.Fatalf("ntfn[%d] = %T, want BlockDisconnected", i, n)
		}
		if bd.Height != disc[i].Height {
			t.Errorf("ntfn[%d].Height = %d, want %d", i, bd.Height, disc[i].Height)
		}
		if bd.Hash != chainhash.Hash(disc[i].Hash) {
			t.Errorf("ntfn[%d].Hash = %s, want %x", i, bd.Hash, disc[i].Hash)
		}
	}
}

// TestElectrumReorgEvent confirms a TipChange carrying BOTH a rewind and a
// replacement connect range produces disconnects first, connects second.
// The wallet's disconnectBlock must run before the new connects so syncedTo
// is on the common ancestor before any new blocks are recorded.
func TestElectrumReorgEvent(t *testing.T) {
	c, stub := makeClient(t, 5)

	// Two existing blocks are being replaced by two new ones.
	stub.addBlock(t, time.Unix(1_700_001_000, 0)) // new height 5
	stub.tipCh <- &electrum.TipChange{
		Disconnected: []electrum.DisconnectedBlock{
			{Hash: [32]byte{0xff}, Height: 4, Time: 1_700_000_960},
			{Hash: [32]byte{0xfe}, Height: 3, Time: 1_700_000_900},
		},
		Connected: []int32{5},
	}

	got := drain(t, c, 5, 2*time.Second)
	if _, ok := got[0].(BlockDisconnected); !ok {
		t.Fatalf("got[0] = %T, want BlockDisconnected", got[0])
	}
	if _, ok := got[1].(BlockDisconnected); !ok {
		t.Fatalf("got[1] = %T, want BlockDisconnected", got[1])
	}
	// After rolling back past the reorg point, the chain reconnects every block
	// from the common ancestor up to the new tip — contiguously, so
	// PutSyncedTo's prev-block chain holds — ending at the new tip (height 5).
	for i, wantHeight := range []int32{3, 4, 5} {
		bc, ok := got[2+i].(BlockConnected)
		if !ok {
			t.Fatalf("got[%d] = %T, want BlockConnected", 2+i, got[2+i])
		}
		if bc.Height != wantHeight {
			t.Fatalf("got[%d] BlockConnected.Height = %d, want %d", 2+i, bc.Height, wantHeight)
		}
	}
}

// TestElectrumRescanLiveHandoff is the regression for the rescan->live sync gap:
// blocks mined DURING the initial rescan must not be dropped. Until the first
// rescan completes the client suppresses live tip changes (the rescan owns
// SyncedTo); at the handoff (goLive) it delivers a contiguous BlockConnected
// catch-up for every block mined during the rescan, so SyncedTo advances to the
// real tip without a gap. Before the fix, a block mined mid-rescan broke
// waddrmgr.PutSyncedTo's prev-block chain and the wallet hung forever.
func TestElectrumRescanLiveHandoff(t *testing.T) {
	c, stub := newGatedClient(t, 6) // heights 0..5, tip=5, gate CLOSED

	// Two blocks are mined "during the rescan", arriving as live tip changes
	// while the gate is still closed. They must be suppressed, not delivered.
	stub.addBlock(t, time.Unix(1_700_001_000, 0)) // tip 6
	stub.addBlock(t, time.Unix(1_700_001_060, 0)) // tip 7
	stub.pushConnected(t, 6)
	stub.pushConnected(t, 7)
	assertNoNtfn(t, c, 300*time.Millisecond)

	// The rescan finished at the tip it froze (5). goLive hands off and must
	// deliver the two blocks mined during the rescan, contiguously from 6.
	c.goLive(5)
	got := drain(t, c, 2, 2*time.Second)
	for i, n := range got {
		bc, ok := n.(BlockConnected)
		if !ok {
			t.Fatalf("handoff ntfn[%d] = %T, want BlockConnected", i, n)
		}
		if want := int32(6 + i); bc.Height != want {
			t.Fatalf("handoff ntfn[%d].Height = %d, want %d", i, bc.Height, want)
		}
	}

	// Now live: a subsequent block is delivered normally and contiguously.
	stub.addBlock(t, time.Unix(1_700_001_120, 0)) // tip 8
	stub.pushConnected(t, 8)
	got = drain(t, c, 1, 2*time.Second)
	if bc, ok := got[0].(BlockConnected); !ok || bc.Height != 8 {
		t.Fatalf("post-handoff ntfn = %#v, want BlockConnected height 8", got[0])
	}
}

// TestElectrumGoLiveIdempotent confirms the handoff happens once: a later
// rescan's goLive (already live, possibly at a lower tip) re-opens nothing and
// re-delivers nothing, so SyncedTo isn't perturbed once the wallet is live.
func TestElectrumGoLiveIdempotent(t *testing.T) {
	c, stub := newGatedClient(t, 4) // heights 0..3, tip=3, gate CLOSED

	stub.addBlock(t, time.Unix(1_700_001_000, 0)) // tip 4
	c.goLive(3)                                   // first handoff: delivers 4
	got := drain(t, c, 1, 2*time.Second)
	if bc, ok := got[0].(BlockConnected); !ok || bc.Height != 4 {
		t.Fatalf("first goLive ntfn = %#v, want BlockConnected height 4", got[0])
	}

	// A later rescan finishes (at the same or a lower height). goLive must be a
	// no-op: it neither resets the delivered watermark nor re-delivers blocks.
	c.goLive(2)
	assertNoNtfn(t, c, 300*time.Millisecond)

	// And live delivery still continues contiguously from where it left off.
	stub.addBlock(t, time.Unix(1_700_001_060, 0)) // tip 5
	stub.pushConnected(t, 5)
	got = drain(t, c, 1, 2*time.Second)
	if bc, ok := got[0].(BlockConnected); !ok || bc.Height != 5 {
		t.Fatalf("post-idempotent ntfn = %#v, want BlockConnected height 5", got[0])
	}
}

// TestElectrumReorgReconnectsNewChain exercises a realistic reorg: the server's
// chain diverges below our tip (headers dropped), so after emitting a
// BlockDisconnected per orphaned block the client must reconnect the WHOLE new
// chain from the fork point up to the new tip — contiguously, so PutSyncedTo's
// prev-block chain holds across the reorg, not just deliver the new tip.
func TestElectrumReorgReconnectsNewChain(t *testing.T) {
	c, stub := makeClient(t, 6) // heights 0..5, tip=5, live, lastConnected=5

	// Reorg: drop 3,4,5 and build a different chain to height 4.
	stub.rewindTo(t, 2)
	stub.addBlock(t, time.Unix(1_700_002_000, 0)) // new height 3
	stub.addBlock(t, time.Unix(1_700_002_060, 0)) // new height 4
	stub.tipCh <- &electrum.TipChange{
		Disconnected: []electrum.DisconnectedBlock{
			{Hash: [32]byte{0x05}, Height: 5, Time: 1_700_001_300},
			{Hash: [32]byte{0x04}, Height: 4, Time: 1_700_001_240},
			{Hash: [32]byte{0x03}, Height: 3, Time: 1_700_001_180},
		},
	}

	// 3 disconnects (5,4,3 newest-first), then a contiguous reconnect of the new
	// chain from the fork point (3,4).
	got := drain(t, c, 5, 2*time.Second)
	for i, wantHeight := range []int32{5, 4, 3} {
		bd, ok := got[i].(BlockDisconnected)
		if !ok {
			t.Fatalf("got[%d] = %T, want BlockDisconnected", i, got[i])
		}
		if bd.Height != wantHeight {
			t.Fatalf("got[%d] BlockDisconnected.Height = %d, want %d", i, bd.Height, wantHeight)
		}
	}
	newTip := stub.Tip()
	for i, wantHeight := range []int32{3, 4} {
		bc, ok := got[3+i].(BlockConnected)
		if !ok {
			t.Fatalf("got[%d] = %T, want BlockConnected", 3+i, got[3+i])
		}
		if bc.Height != wantHeight {
			t.Fatalf("got[%d] BlockConnected.Height = %d, want %d", 3+i, bc.Height, wantHeight)
		}
		// The reconnected blocks must be the NEW chain (from the post-rewind
		// header store), not the orphaned ones.
		if hdr := stub.HeaderByHeight(wantHeight); hdr == nil || bc.Hash != hdr.BlockHash() {
			t.Fatalf("got[%d] reconnect hash != new-chain header at height %d", 3+i, wantHeight)
		}
	}
	if got[4].(BlockConnected).Height != newTip {
		t.Fatalf("final reconnect height = %d, want new tip %d", got[4].(BlockConnected).Height, newTip)
	}
}

// TestElectrumScripthashAheadOfHeaders is the regression for review2 #6:
// when a scripthash status notification reports a tx at a confirmed height
// whose header hasn't been appended locally yet, the chain client must NOT
// emit a RelevantTx with blockMeta=nil (which would record the tx as
// unmined forever, since the next status-diff would treat it as
// already-seen). The tx must be deferred and flushed once the tip catches
// up.
func TestElectrumScripthashAheadOfHeaders(t *testing.T) {
	c, stub := makeClient(t, 2)

	addr, sh := newSimnetAddr(t)
	// Synthesize a tx that the wallet will see as relevant once the
	// header at height=5 lands.
	txid, rawHex := synthTxToAddr(t, addr)
	stub.history[sh] = []electrum.HistoryEntry{{Height: 5, TxHash: txid}}
	stub.rawTxs[txid] = rawHex

	if err := c.NotifyReceived([]btcutil.Address{addr}); err != nil {
		t.Fatalf("NotifyReceived: %v", err)
	}

	// Push the scripthash status BEFORE the header arrives. Pre-fix, the
	// chain client would have emitted RelevantTx(blockMeta=nil), persist
	// the tx as unmined, and never self-heal.
	stub.scripthashCh <- &electrum.ScripthashStatus{Scripthash: sh, Status: "h5"}

	// Confirm no premature emission within a short window.
	select {
	case n := <-c.Notifications():
		t.Fatalf("unexpected early notification %T while header missing", n)
	case <-time.After(50 * time.Millisecond):
	}

	// Verify the entry parked in pending.
	c.addrCacheMtx.Lock()
	entry := c.addrCache[sh]
	pendCount := 0
	if entry != nil {
		pendCount = len(entry.pending)
	}
	c.addrCacheMtx.Unlock()
	if pendCount != 1 {
		t.Fatalf("expected 1 deferred entry, got %d", pendCount)
	}

	// Now the headers catch up. The tip-change should drive a flush and
	// emit the deferred RelevantTx with proper block meta.
	for i := 2; i <= 5; i++ {
		stub.addBlock(t, time.Unix(int64(1_700_001_000+i*60), 0))
	}
	stub.tipCh <- &electrum.TipChange{Connected: []int32{2, 3, 4, 5}}

	got := drain(t, c, 5, 2*time.Second) // 4 BlockConnected + 1 RelevantTx
	var rt *RelevantTx
	for _, n := range got {
		if r, ok := n.(RelevantTx); ok {
			rt = &r
			break
		}
	}
	if rt == nil {
		t.Fatal("expected a RelevantTx after tip catch-up")
	}
	if rt.Block == nil {
		t.Fatal("RelevantTx.Block = nil; expected confirmed block meta")
	}
	if rt.Block.Height != 5 {
		t.Fatalf("RelevantTx.Block.Height = %d, want 5", rt.Block.Height)
	}
}

// TestElectrumNotificationHandlerSurvivesNoInitialTip is the regression for
// review2 #4: the handler used to call GetBestBlock at startup and tear the
// whole client down (close dequeue + go Stop()) if it failed. The wallet's
// chain-notification loop then exits silently and the wallet never syncs.
// The fix is to never call GetBestBlock here and to populate bs from the
// first enqueued BlockConnected instead.
func TestElectrumNotificationHandlerSurvivesNoInitialTip(t *testing.T) {
	stub := newStubNetwork()
	// No blocks: stub.Tip() returns -1 so GetBestBlock would fail.
	stub.synced = true

	c, err := NewElectrumClient(testParams(), stub)
	if err != nil {
		t.Fatalf("NewElectrumClient: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		c.Stop()
		c.WaitForShutdown()
	})

	// Mark the client live (as the first rescan's handoff would) so the
	// tip-change below is dispatched. No blocks yet, so lastConnected is -1.
	c.connMtx.Lock()
	c.liveDelivery = true
	c.lastConnected = stub.Tip()
	c.connMtx.Unlock()

	// ClientConnected must be deliverable: the handler is alive.
	got := drain(t, c, 1, 2*time.Second)
	if _, ok := got[0].(ClientConnected); !ok {
		t.Fatalf("first ntfn = %T, want ClientConnected", got[0])
	}

	// Now feed the first BlockConnected via a tip change. BlockStamp
	// should subsequently return non-blocking with that height.
	stub.addBlock(t, time.Unix(1_700_000_000, 0))
	stub.pushConnected(t, 0)
	drain(t, c, 1, 2*time.Second) // BlockConnected

	bs, err := c.BlockStamp()
	if err != nil {
		t.Fatalf("BlockStamp: %v", err)
	}
	if bs.Height != 0 {
		t.Fatalf("BlockStamp.Height = %d, want 0", bs.Height)
	}
}

// TestElectrumRescanOnStoppedClient is the regression for M2: Rescan on a
// client whose Stop has already run should return an error rather than
// silently no-op.
func TestElectrumRescanOnStoppedClient(t *testing.T) {
	c, _ := makeClient(t, 1)
	c.Stop()
	c.WaitForShutdown()
	err := c.Rescan(nil, nil, nil)
	if err == nil {
		t.Fatal("expected error from Rescan on stopped client, got nil")
	}
}

// TestElectrumScripthashStatusRace exercises H1: concurrent scripthash
// status notifications and a Rescan that writes the same entry's
// lastHistory. Race-detector runs would have caught the original bug.
func TestElectrumScripthashStatusRace(t *testing.T) {
	c, stub := makeClient(t, 3)
	// makeClient already drained initial ClientConnected + BlockConnected.

	addr, sh := newSimnetAddr(t)
	// Pre-seed enough history that GetHistory/get_raw paths see real data.
	for i := range 6 {
		txid, rawHex := synthTxToAddr(t, addr)
		stub.history[sh] = append(stub.history[sh], electrum.HistoryEntry{
			Height: int32(1 + i%2), TxHash: txid,
		})
		stub.rawTxs[txid] = rawHex
	}
	if err := c.NotifyReceived([]btcutil.Address{addr}); err != nil {
		t.Fatalf("NotifyReceived: %v", err)
	}

	// Drive concurrent status notifications and Rescan calls so the
	// addrCache is read and written from multiple goroutines.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 50 {
			stub.scripthashCh <- &electrum.ScripthashStatus{
				Scripthash: sh, Status: "v" + string(rune('a'+i%26)),
			}
		}
	}()
	go func() {
		defer wg.Done()
		hash := stub.headers[0].BlockHash()
		for range 5 {
			if err := c.Rescan(&hash, []btcutil.Address{addr}, nil); err != nil {
				t.Errorf("Rescan: %v", err)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	wg.Wait()

	// Drain any leftover notifications so test cleanup doesn't deadlock
	// the notification handler. The exact count isn't important here;
	// we're testing for races, not message contents.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case <-c.Notifications():
		case <-deadline:
			return
		}
	}
}

// TestElectrumSubscribeAddressConcurrentFailure is the regression for the
// review-flagged Medium bug: when the first caller's SubscribeScripthash RPC
// fails, concurrent waiters that observed the in-flight entry must NOT
// silently return nil (which would leave the wallet layer believing the
// address was subscribed). They must either retry and observe a real
// subscription, or surface the error.
func TestElectrumSubscribeAddressConcurrentFailure(t *testing.T) {
	c, stub := makeClient(t, 1)
	// makeClient already drained initial ClientConnected + BlockConnected.

	addr, sh := newSimnetAddr(t)

	// Gate the first SubscribeScripthash call so the second caller is
	// guaranteed to enqueue while the first is still in flight, then fail
	// it. Subsequent calls succeed normally.
	release := make(chan struct{})
	enqueued := make(chan struct{}, 1)
	stub.mu.Lock()
	stub.subscribeHook = func(n int, _ string) error {
		if n == 1 {
			// Tell the test the first call has entered; then
			// block until the test releases it.
			enqueued <- struct{}{}
			<-release
			return errors.New("injected subscribe failure")
		}
		return nil
	}
	stub.mu.Unlock()

	type result struct {
		who string
		err error
	}
	results := make(chan result, 2)

	go func() {
		err := c.NotifyReceived([]btcutil.Address{addr})
		results <- result{"A", err}
	}()

	// Wait until A is inside the SubscribeScripthash RPC so B's entry
	// will observe the in-flight ready channel rather than racing past
	// it.
	select {
	case <-enqueued:
	case <-time.After(2 * time.Second):
		t.Fatal("first subscribe never reached the server")
	}

	go func() {
		err := c.NotifyReceived([]btcutil.Address{addr})
		results <- result{"B", err}
	}()

	// Give B a moment to enter trySubscribeAddress and start waiting on
	// ready before we unblock A.
	time.Sleep(20 * time.Millisecond)
	close(release)

	// Both calls must complete; neither should leave the address
	// unsubscribed and report success.
	deadline := time.After(5 * time.Second)
	for range 2 {
		select {
		case r := <-results:
			if r.err != nil {
				// Surfacing the error is an acceptable outcome
				// (caller knows the address isn't subscribed).
				continue
			}
			// Success is only acceptable if the address is actually
			// subscribed in the cache after the call returned.
			c.addrCacheMtx.Lock()
			_, ok := c.addrCache[sh]
			c.addrCacheMtx.Unlock()
			if !ok {
				t.Fatalf("caller %s returned nil but address %s was not subscribed (the bug)", r.who, addr)
			}
		case <-deadline:
			t.Fatal("subscribe calls did not return")
		}
	}
}

func TestElectrumMapRPCErr(t *testing.T) {
	c, _ := makeClient(t, 1)
	cases := []struct {
		in   string
		want error
	}{
		{"electrum: code 1: \"transaction already in block chain\"", ErrTxAlreadyConfirmed},
		{"electrum: code 1: \"missing inputs\"", ErrMissingInputs},
		{"electrum: code 1: \"txn-mempool-conflict\"", ErrMempoolConflict},
		{"completely unrelated message", ErrUndefined},
	}
	for _, tc := range cases {
		got := c.MapRPCErr(errors.New(tc.in))
		if !errors.Is(got, tc.want) {
			t.Fatalf("MapRPCErr(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if err := c.MapRPCErr(nil); err != nil {
		t.Fatal("MapRPCErr(nil) should return nil")
	}
}

func TestElectrumRescanEmitsBlocksAndFilteredAndFinished(t *testing.T) {
	c, stub := makeClient(t, 5)
	// makeClient already drained initial ClientConnected + BlockConnected.

	addr, sh := newSimnetAddr(t)
	// Place a tx at height 2.
	txid, rawHex := synthTx(t)
	stub.history[sh] = []electrum.HistoryEntry{{Height: 2, TxHash: txid}}
	stub.rawTxs[txid] = rawHex

	startHash := stub.headers[0].BlockHash()
	// Rescan is async: it returns nil immediately and emits notifications
	// from a goroutine.
	if err := c.Rescan(&startHash, []btcutil.Address{addr}, nil); err != nil {
		t.Fatalf("Rescan: %v", err)
	}

	// Review2 #11: empty blocks no longer produce BlockConnected. The
	// only notifications from a 5-block walk with a single match at
	// height 2 are FilteredBlockConnected(2) + RescanFinished.
	got := drain(t, c, 2, 5*time.Second)

	wantTypes := []string{
		"chain.FilteredBlockConnected",
		"*chain.RescanFinished",
	}
	for i, n := range got {
		typ := typeName(n)
		if typ != wantTypes[i] {
			t.Errorf("ntfn[%d] = %s, want %s", i, typ, wantTypes[i])
		}
	}

	fbc := got[0].(FilteredBlockConnected)
	if fbc.Block.Height != 2 {
		t.Fatalf("FilteredBlockConnected.Block.Height = %d", fbc.Block.Height)
	}
	if len(fbc.RelevantTxs) != 1 {
		t.Fatalf("FilteredBlockConnected.RelevantTxs = %d, want 1", len(fbc.RelevantTxs))
	}

	rf := got[1].(*RescanFinished)
	if rf.Height != 4 {
		t.Fatalf("RescanFinished.Height = %d, want 4", rf.Height)
	}
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

func typeName(v any) string {
	switch v.(type) {
	case ClientConnected:
		return "chain.ClientConnected"
	case BlockConnected:
		return "chain.BlockConnected"
	case BlockDisconnected:
		return "chain.BlockDisconnected"
	case FilteredBlockConnected:
		return "chain.FilteredBlockConnected"
	case RelevantTx:
		return "chain.RelevantTx"
	case *RescanProgress:
		return "*chain.RescanProgress"
	case *RescanFinished:
		return "*chain.RescanFinished"
	}
	return "unknown"
}

// newSimnetAddr returns a synthetic P2PKH address on btcd's simnet params
// plus its electrum scripthash.
func newSimnetAddr(t *testing.T) (btcutil.Address, string) {
	t.Helper()
	// 20-byte pubkey hash; the bytes don't matter for these tests.
	pkh := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a,
		0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14,
	}
	addr, err := btcutil.NewAddressPubKeyHash(pkh, &chaincfg.SimNetParams)
	if err != nil {
		t.Fatalf("NewAddressPubKeyHash: %v", err)
	}
	sh, err := addrScripthash(addr)
	if err != nil {
		t.Fatalf("addrScripthash: %v", err)
	}
	return addr, sh
}

func TestElectrumFilterBlocks(t *testing.T) {
	c, stub := makeClient(t, 6)

	addr, sh := newSimnetAddr(t)
	// The tx must actually pay to addr so the in-tree BlockFilterer
	// recognises it as "relevant" — FilterBlocks doesn't trust the
	// server's history blindly.
	txid, rawHex := synthTxToAddr(t, addr)
	stub.history[sh] = []electrum.HistoryEntry{{Height: 3, TxHash: txid}}
	stub.rawTxs[txid] = rawHex

	scopedIdx := waddrmgr.ScopedIndex{Scope: waddrmgr.KeyScope{Purpose: 84, Coin: 0}}
	req := &FilterBlocksRequest{
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcutil.Address{scopedIdx: addr},
	}
	for h, hdr := range stub.headers {
		req.Blocks = append(req.Blocks, wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: hdr.BlockHash(), Height: int32(h)},
			Time:  hdr.Timestamp,
		})
	}

	resp, err := c.FilterBlocks(req)
	if err != nil {
		t.Fatalf("FilterBlocks: %v", err)
	}
	if resp == nil {
		t.Fatal("expected a response for a block with activity, got nil")
	}
	if resp.BlockMeta.Height != 3 {
		t.Fatalf("matched block height = %d, want 3", resp.BlockMeta.Height)
	}
	if len(resp.RelevantTxns) != 1 {
		t.Fatalf("RelevantTxns = %d, want 1", len(resp.RelevantTxns))
	}

	// And a miss case: no addresses, no hits.
	miss, err := c.FilterBlocks(&FilterBlocksRequest{
		Blocks:        req.Blocks,
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcutil.Address{},
	})
	if err != nil {
		t.Fatalf("FilterBlocks miss: %v", err)
	}
	if miss != nil {
		t.Fatalf("expected nil response for empty addr list, got %+v", miss)
	}
}

// TestElectrumFilterBlocksSurfacesWatchedOutPointSpends is the regression
// for the Codex P1: a transaction that spends a wallet-owned outpoint but
// does not also pay one of the current lookahead addresses must still be
// fetched and surfaced by FilterBlocks. Pre-fix, the candidate scripthash
// set was built only from ExternalAddrs and InternalAddrs, so spend-only
// txs at watched outpoints went undiscovered and the wallet balance went
// stale.
func TestElectrumFilterBlocksSurfacesWatchedOutPointSpends(t *testing.T) {
	c, stub := makeClient(t, 6)

	owner, sh := newSimnetAddr(t)

	// Synthesize a tx that SPENDS a watched outpoint (an input whose
	// PreviousOutPoint maps to owner via WatchedOutPoints). The tx pays
	// to OP_TRUE — owner is NOT in the output set, so without the fix
	// this tx would not be on owner's scripthash watch path and the
	// spend would never reach BlockFilterer.FilterTx.
	prevHash := chainhash.Hash{31: 0xaa}
	watchedOutpoint := wire.OutPoint{Hash: prevHash, Index: 0}
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: watchedOutpoint,
		SignatureScript:  []byte{0x51},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(&wire.TxOut{Value: 1234, PkScript: []byte{0x51}}) // OP_TRUE
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	rawHex := hex.EncodeToString(buf.Bytes())
	txid := tx.TxHash().String()

	// Electrumx returns history for owner's scripthash because the
	// spend's input references an output paying that scripthash —
	// scripthash histories cover both directions of address activity.
	stub.history[sh] = []electrum.HistoryEntry{{Height: 4, TxHash: txid}}
	stub.rawTxs[txid] = rawHex

	req := &FilterBlocksRequest{
		// Crucially, no lookahead addresses — the only signal that
		// this tx is relevant is the watched outpoint.
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcutil.Address{},
		InternalAddrs: map[waddrmgr.ScopedIndex]btcutil.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcutil.Address{
			watchedOutpoint: owner,
		},
	}
	for h, hdr := range stub.headers {
		req.Blocks = append(req.Blocks, wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: hdr.BlockHash(), Height: int32(h)},
			Time:  hdr.Timestamp,
		})
	}

	resp, err := c.FilterBlocks(req)
	if err != nil {
		t.Fatalf("FilterBlocks: %v", err)
	}
	if resp == nil {
		t.Fatal("expected a response: the spend-only tx should have been surfaced via WatchedOutPoints")
	}
	if resp.BlockMeta.Height != 4 {
		t.Fatalf("matched block height = %d, want 4", resp.BlockMeta.Height)
	}
	if len(resp.RelevantTxns) != 1 {
		t.Fatalf("RelevantTxns = %d, want 1", len(resp.RelevantTxns))
	}
	// Verify the relevant tx actually spends the watched outpoint —
	// otherwise we'd be passing for the wrong reason.
	got := resp.RelevantTxns[0]
	if len(got.TxIn) == 0 || got.TxIn[0].PreviousOutPoint != watchedOutpoint {
		t.Fatalf("relevant tx does not spend the watched outpoint; got input %+v",
			got.TxIn[0].PreviousOutPoint)
	}
}

// TestElectrumFilterBlocksSameBlockParentChild is the regression for review
// #2: when a wallet receives funds and re-spends them within the same block,
// the child tx is only recognized as relevant if its same-block parent was
// filtered first (so the parent's outpoint is in FoundOutPoints). electrumx
// history is not in block order, so FilterBlocks must topologically order the
// fetched txs. We deliberately feed the child ahead of the parent in history
// to exercise the ordering; without orderTxsByDependency the child's spend is
// missed and only one relevant tx is returned.
func TestElectrumFilterBlocksSameBlockParentChild(t *testing.T) {
	c, stub := makeClient(t, 6)

	addr, sh := newSimnetAddr(t)
	payScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript: %v", err)
	}

	// Parent pays our address (relevant via output) and creates outpoint 0.
	parent := wire.NewMsgTx(wire.TxVersion)
	parent.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{31: 0xc1}, Index: 0},
		SignatureScript:  []byte{0x51},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	parent.AddTxOut(&wire.TxOut{Value: 5000, PkScript: payScript})
	parentTxid := parent.TxHash().String()

	// Child spends the parent's output 0 and pays OP_TRUE (not our address),
	// so the ONLY thing making it relevant is the same-block spend.
	child := wire.NewMsgTx(wire.TxVersion)
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: parent.TxHash(), Index: 0},
		SignatureScript:  []byte{0x51},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	child.AddTxOut(&wire.TxOut{Value: 4000, PkScript: []byte{0x51}})
	childTxid := child.TxHash().String()

	serialize := func(tx *wire.MsgTx) string {
		var buf bytes.Buffer
		if err := tx.Serialize(&buf); err != nil {
			t.Fatalf("Serialize: %v", err)
		}
		return hex.EncodeToString(buf.Bytes())
	}
	stub.rawTxs[parentTxid] = serialize(parent)
	stub.rawTxs[childTxid] = serialize(child)

	// Both appear in our scripthash history at the same height. Child is
	// listed FIRST to force the out-of-order case.
	stub.history[sh] = []electrum.HistoryEntry{
		{Height: 3, TxHash: childTxid},
		{Height: 3, TxHash: parentTxid},
	}

	scopedIdx := waddrmgr.ScopedIndex{Scope: waddrmgr.KeyScope{Purpose: 84, Coin: 0}}
	req := &FilterBlocksRequest{
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcutil.Address{scopedIdx: addr},
	}
	for h, hdr := range stub.headers {
		req.Blocks = append(req.Blocks, wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: hdr.BlockHash(), Height: int32(h)},
			Time:  hdr.Timestamp,
		})
	}

	resp, err := c.FilterBlocks(req)
	if err != nil {
		t.Fatalf("FilterBlocks: %v", err)
	}
	if resp == nil {
		t.Fatal("expected a response for the block with activity")
	}
	// Both parent (pays us) and child (spends our same-block output) must be
	// surfaced. Pre-fix, the child filtered first sees an empty
	// FoundOutPoints and is dropped, yielding only the parent.
	if len(resp.RelevantTxns) != 2 {
		t.Fatalf("RelevantTxns = %d, want 2 (parent + same-block child)", len(resp.RelevantTxns))
	}
	// Assert the CHILD specifically is present — that's the spend that gets
	// missed without the topological ordering. Checking only the count could
	// pass for the wrong reason if a future refactor changed which tx survives.
	var sawChild bool
	for _, rt := range resp.RelevantTxns {
		if rt.TxHash().String() == childTxid {
			sawChild = true
		}
	}
	if !sawChild {
		t.Fatal("same-block child spend not surfaced in RelevantTxns")
	}
	// And the parent's outpoint must be recorded as found/spent.
	parentOutpoint := wire.OutPoint{Hash: parent.TxHash(), Index: 0}
	if _, ok := resp.FoundOutPoints[parentOutpoint]; !ok {
		t.Fatal("parent outpoint not in FoundOutPoints")
	}
}

// synthTxNonce makes each synthTx return a distinct transaction so successive
// calls in the same test (or shared fixtures) don't produce colliding txids.
var synthTxNonce atomic.Uint32

// synthTx returns a minimally-valid wire tx, its txid hex, and its raw hex
// serialization. The tx pays to OP_TRUE; use synthTxToAddr when the caller
// needs the output to belong to a specific address.
func synthTx(t *testing.T) (txid, rawHex string) {
	t.Helper()
	script, err := txscript.NewScriptBuilder().AddOp(txscript.OP_TRUE).Script()
	if err != nil {
		t.Fatalf("scriptBuilder: %v", err)
	}
	return synthTxWithScript(t, script)
}

// synthTxToAddr returns a minimally-valid wire tx that pays its single output
// to addr, plus the txid and hex serialization. Use this when the test needs
// the tx to be filterable as "relevant" to addr.
func synthTxToAddr(t *testing.T, addr btcutil.Address) (txid, rawHex string) {
	t.Helper()
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript: %v", err)
	}
	return synthTxWithScript(t, script)
}

func synthTxWithScript(t *testing.T, pkScript []byte) (txid, rawHex string) {
	t.Helper()
	nonce := synthTxNonce.Add(1)
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{31: byte(nonce)}, Index: 0},
		SignatureScript:  []byte{0x51},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(&wire.TxOut{Value: 1000 + int64(nonce), PkScript: pkScript})
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return tx.TxHash().String(), hex.EncodeToString(buf.Bytes())
}

// TestVerifyInclusionRejectsFabrication covers rotation plan R7: a proof that
// does not fold to the validated local header's merkle root is refused, so a
// server cannot inject a confirmed transaction that was never mined.
func TestVerifyInclusionRejectsFabrication(t *testing.T) {
	stub := newStubNetwork()
	// Two real headers; height 1 carries a known merkle root.
	genesis := &wire.BlockHeader{}
	realRoot := chainhash.DoubleHashH([]byte("real-block"))
	blk1 := &wire.BlockHeader{PrevBlock: genesis.BlockHash(), MerkleRoot: realRoot}
	stub.headers = []*wire.BlockHeader{genesis, blk1}
	stub.hashIdx = map[chainhash.Hash]int32{
		genesis.BlockHash(): 0, blk1.BlockHash(): 1,
	}

	stub.synced = true
	c, err := NewElectrumClient(testParams(), stub)
	if err != nil {
		t.Fatalf("NewElectrumClient: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Stop)

	// A tx whose (empty-branch) proof folds to txid != realRoot must be
	// refused. lyingMerkle returns an empty branch for any txid, so the
	// fold yields the txid, which cannot equal realRoot.
	stub.lyingMerkle = true
	fake := chainhash.DoubleHashH([]byte("never-mined")).String()
	if err := c.verifyInclusion(fake, 1); err == nil {
		t.Fatal("fabricated transaction accepted")
	}

	// The honest single-tx case: set the header root to the txid so the
	// empty-branch proof folds cleanly.
	honest := chainhash.DoubleHashH([]byte("mined")).String()
	h, _ := chainhash.NewHashFromStr(honest)
	blk1.MerkleRoot = *h
	if err := c.verifyInclusion(honest, 1); err != nil {
		t.Fatalf("honest inclusion rejected: %v", err)
	}
}

// TestBuildRelevantTxBindsTxid covers SEC-1: a server that returns raw bytes
// whose hash differs from the requested (merkle-proven) txid is refused, so
// the merkle authentication cannot be bypassed at the fetch hop.
func TestBuildRelevantTxBindsTxid(t *testing.T) {
	stub := newStubNetwork()
	stub.synced = true
	c, err := NewElectrumClient(testParams(), stub)
	if err != nil {
		t.Fatalf("NewElectrumClient: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Stop)

	// A real transaction the stub will serve.
	realTx := wire.NewMsgTx(2)
	realTx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 3}, nil, nil))
	realTx.AddTxOut(wire.NewTxOut(1e8, []byte{0x51}))
	var buf bytes.Buffer
	if err := realTx.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	realHex := hex.EncodeToString(buf.Bytes())

	// The server answers get_raw for the WRONG txid with realTx's bytes.
	wanted := chainhash.DoubleHashH([]byte("some-other-txid")).String()
	stub.mu.Lock()
	stub.rawTxs[wanted] = realHex
	stub.mu.Unlock()

	if _, err := c.buildRelevantTx(electrum.HistoryEntry{TxHash: wanted, Height: 1}); err == nil {
		t.Fatal("accepted raw bytes whose hash differs from the requested txid")
	}

	// Sanity: the correct txid for those bytes is accepted.
	stub.mu.Lock()
	stub.rawTxs[realTx.TxHash().String()] = realHex
	stub.mu.Unlock()
	if _, err := c.buildRelevantTx(electrum.HistoryEntry{
		TxHash: realTx.TxHash().String(), Height: 1,
	}); err != nil {
		t.Fatalf("rejected the correct bytes: %v", err)
	}
}
