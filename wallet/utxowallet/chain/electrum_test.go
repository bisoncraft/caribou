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

	tipCh        chan int32
	scripthashCh chan *electrum.ScripthashStatus
	done         chan struct{}
}

func newStubNetwork() *stubNetwork {
	return &stubNetwork{
		hashIdx:      map[chainhash.Hash]int32{},
		subs:         map[string]string{},
		history:      map[string][]electrum.HistoryEntry{},
		rawTxs:       map[string]string{},
		tipCh:        make(chan int32, 8),
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

func (s *stubNetwork) TipChangeChan() <-chan int32                       { return s.tipCh }
func (s *stubNetwork) ScripthashChan() <-chan *electrum.ScripthashStatus { return s.scripthashCh }

func (s *stubNetwork) SubscribeScripthash(_ context.Context, sh string) (*electrum.ScripthashStatus, error) {
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
// n blocks. The returned client has been Started; the caller is responsible
// for Stop()/WaitForShutdown.
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
	// First notification is always ClientConnected.
	first := drain(t, c, 1, 2*time.Second)[0]
	if _, ok := first.(ClientConnected); !ok {
		t.Fatalf("first ntfn = %T, want ClientConnected", first)
	}
	// Add a new block and push a tip-change.
	stub.addBlock(t, time.Unix(1_700_000_999, 0))
	stub.tipCh <- 3
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
	// Drain the initial ClientConnected.
	drain(t, c, 1, time.Second)

	// BlockStamp before any BlockConnected returns the best-block stamp.
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
	stub.tipCh <- 2
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
	drain(t, c, 1, time.Second) // ClientConnected

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
	drain(t, c, 1, time.Second) // ClientConnected

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

	// Heights 0, 1 produce BlockConnected; height 2 produces
	// FilteredBlockConnected; heights 3, 4 produce BlockConnected;
	// then RescanFinished.
	got := drain(t, c, 6, 5*time.Second)

	wantTypes := []string{
		"chain.BlockConnected",
		"chain.BlockConnected",
		"chain.FilteredBlockConnected",
		"chain.BlockConnected",
		"chain.BlockConnected",
		"*chain.RescanFinished",
	}
	for i, n := range got {
		typ := typeName(n)
		if typ != wantTypes[i] {
			t.Errorf("ntfn[%d] = %s, want %s", i, typ, wantTypes[i])
		}
	}

	fbc := got[2].(FilteredBlockConnected)
	if fbc.Block.Height != 2 {
		t.Fatalf("FilteredBlockConnected.Block.Height = %d", fbc.Block.Height)
	}
	if len(fbc.RelevantTxs) != 1 {
		t.Fatalf("FilteredBlockConnected.RelevantTxs = %d, want 1", len(fbc.RelevantTxs))
	}

	rf := got[5].(*RescanFinished)
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
