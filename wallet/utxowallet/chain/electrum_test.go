// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
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
	mu       sync.Mutex
	headers  []*wire.BlockHeader
	hashIdx  map[chainhash.Hash]int32
	synced   bool
	subs     map[string]string // scripthash → status
	history  map[string][]electrum.HistoryEntry
	rawTxs   map[string]string // txid → hex
	bcasted  []string
	bcastErr error

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

func (s *stubNetwork) Start(_ context.Context) error { return nil }
func (s *stubNetwork) Stop()                         { close(s.done) }
func (s *stubNetwork) Done() <-chan struct{}         { return s.done }
func (s *stubNetwork) WaitForShutdown()              { <-s.done }

func (s *stubNetwork) Tip() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int32(len(s.headers)) - 1
}

func (s *stubNetwork) Synced() bool { return s.synced }

func (s *stubNetwork) HeaderByHeight(height int32) *wire.BlockHeader {
	s.mu.Lock()
	defer s.mu.Unlock()
	if height < 0 || int(height) >= len(s.headers) {
		return nil
	}
	return s.headers[height]
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

	got := drain(t, c, 3, 2*time.Second)
	if _, ok := got[0].(BlockDisconnected); !ok {
		t.Fatalf("got[0] = %T, want BlockDisconnected", got[0])
	}
	if _, ok := got[1].(BlockDisconnected); !ok {
		t.Fatalf("got[1] = %T, want BlockDisconnected", got[1])
	}
	bc, ok := got[2].(BlockConnected)
	if !ok {
		t.Fatalf("got[2] = %T, want BlockConnected", got[2])
	}
	if bc.Height != 5 {
		t.Fatalf("BlockConnected.Height = %d, want 5", bc.Height)
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

	scopedIdx := waddrmgr.ScopedIndex{Scope: waddrmgr.KeyScopeBIP0084}
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
