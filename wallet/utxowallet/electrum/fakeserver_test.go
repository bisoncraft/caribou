// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// fakeServer is a scriptable in-process electrumx: enough of the protocol for
// Network/Node to run against it, plus fault injection (kill connections, lag
// the tip, fork the chain, error per method). It is the test bed for the
// surgical-rotation work (docs/electrum-surgical-rotation-plan.md R0) and the
// broadcast/observer unit tests.
type fakeServer struct {
	t   *testing.T
	lis net.Listener

	mtx        sync.Mutex
	chain      []*wire.BlockHeader       // height-indexed; [0] is genesis
	hist       map[string][]HistoryEntry // scripthash -> history
	txs        map[string]string         // txid -> raw hex
	peers      [][]interface{}           // server.peers.subscribe reply
	errs       map[string]string         // method -> scripted error message
	muted      map[string]bool           // method -> swallow the request, never reply
	broadcasts []string                  // raw hexes received
	conns      map[net.Conn]*fakeConnState
	closed     bool
	// claimTip, when non-zero, is the height headers.subscribe reports in
	// place of the real tip.
	claimTip int
	// onBlockHeaders, when set, runs before each blockchain.block.headers
	// reply with mtx held, and may replace chain.
	onBlockHeaders func(call, start, count int)
	headerCalls    int
}

type fakeConnState struct {
	mtx        sync.Mutex
	enc        *json.Encoder
	headerSub  bool
	scripthash map[string]bool
}

// easyBits is regtest-grade difficulty: mineTestHeader succeeds in a few
// iterations, and checkPoW passes because the hash beats the claimed target.
const easyBits = 0x207fffff

// newFakeServer starts a fake with a freshly mined chain of length n
// (including genesis at height 0) and returns it with a matching
// netparams.ChainParams whose GenesisHash pins the fake chain.
func newFakeServer(t *testing.T, n int) (*fakeServer, *netparams.ChainParams) {
	t.Helper()
	f := &fakeServer{
		t:     t,
		hist:  make(map[string][]HistoryEntry),
		txs:   make(map[string]string),
		errs:  make(map[string]string),
		muted: make(map[string]bool),
		conns: make(map[net.Conn]*fakeConnState),
	}
	prev := chainhash.Hash{}
	for i := 0; i < n; i++ {
		hdr := mineTestHeader(t, prev, easyBits, uint32(i)*7919)
		f.chain = append(f.chain, hdr)
		prev = hdr.BlockHash()
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake listen: %v", err)
	}
	f.lis = lis
	go f.acceptLoop()
	t.Cleanup(f.Close)

	genesis := f.chain[0].BlockHash()
	params := &netparams.ChainParams{
		Chain:              "btc",
		Net:                chaincfg.RegressionNetParams.Net,
		GenesisBlock:       chaincfg.RegressionNetParams.GenesisBlock,
		GenesisHash:        &genesis,
		PoWNoRetargeting:   true,
		TargetTimePerBlock: 10 * time.Minute,
	}
	return f, params
}

func (f *fakeServer) addr() *ServerAddr {
	return &ServerAddr{Proto: "tcp", Addr: f.lis.Addr().String()}
}

// Close shuts the listener and every connection.
func (f *fakeServer) Close() {
	f.mtx.Lock()
	if f.closed {
		f.mtx.Unlock()
		return
	}
	f.closed = true
	conns := make([]net.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mtx.Unlock()
	f.lis.Close()
	for _, c := range conns {
		c.Close()
	}
}

// KillConns drops every live connection (the server keeps listening), the
// "server restarted / connection lost" fault.
func (f *fakeServer) KillConns() {
	f.mtx.Lock()
	conns := make([]net.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mtx.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// FailMethod scripts an error reply for a method ("" clears).
func (f *fakeServer) FailMethod(method, errMsg string) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	if errMsg == "" {
		delete(f.errs, method)
	} else {
		f.errs[method] = errMsg
	}
}

// MuteMethod scripts a method to be swallowed: the request is read and never
// answered, while every other method (server.ping included) keeps working,
// so the connection stays alive and the caller's only way out is its
// context.
func (f *fakeServer) MuteMethod(method string, on bool) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	if on {
		f.muted[method] = true
	} else {
		delete(f.muted, method)
	}
}

// MineBlocks appends n blocks and pushes header notifications to subscribers.
func (f *fakeServer) MineBlocks(n int) {
	f.mtx.Lock()
	for i := 0; i < n; i++ {
		prev := f.chain[len(f.chain)-1].BlockHash()
		hdr := mineTestHeader(f.t, prev, easyBits, uint32(len(f.chain))*104729)
		f.chain = append(f.chain, hdr)
	}
	tip := len(f.chain) - 1
	hdr := f.chain[tip]
	f.mtx.Unlock()
	f.notifyHeader(int32(tip), hdr)
}

// Fork rewrites the chain from height h (exclusive) with n new blocks and
// notifies subscribers of the new tip.
func (f *fakeServer) Fork(h int32, n int) {
	f.mtx.Lock()
	f.chain = f.chain[:h+1]
	for i := 0; i < n; i++ {
		prev := f.chain[len(f.chain)-1].BlockHash()
		hdr := mineTestHeader(f.t, prev, easyBits, uint32(len(f.chain))*31337+1)
		f.chain = append(f.chain, hdr)
	}
	tip := len(f.chain) - 1
	hdr := f.chain[tip]
	f.mtx.Unlock()
	f.notifyHeader(int32(tip), hdr)
}

// ForkWithBits is Fork with the new blocks mined at bits instead of easyBits.
// Bits encoding a target above easyBits's build a fork carrying less work
// per block, which the fake params allow: they set no PowLimit and no
// difficulty rule.
func (f *fakeServer) ForkWithBits(h int32, n int, bits uint32) {
	f.mtx.Lock()
	f.chain = f.chain[:h+1]
	for i := 0; i < n; i++ {
		prev := f.chain[len(f.chain)-1].BlockHash()
		hdr := mineTestHeader(f.t, prev, bits, uint32(len(f.chain))*7333+3)
		f.chain = append(f.chain, hdr)
	}
	tip := len(f.chain) - 1
	hdr := f.chain[tip]
	f.mtx.Unlock()
	f.notifyHeader(int32(tip), hdr)
}

// Tip returns the fake chain's tip height.
func (f *fakeServer) Tip() int32 {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	return int32(len(f.chain) - 1)
}

// SetHistory replaces a scripthash's history and notifies its subscribers.
func (f *fakeServer) SetHistory(sh string, entries []HistoryEntry) {
	f.mtx.Lock()
	f.hist[sh] = entries
	f.mtx.Unlock()
	status := fakeStatus(entries)
	f.eachConn(func(cs *fakeConnState) {
		if cs.scripthash[sh] {
			cs.send(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "blockchain.scripthash.subscribe",
				"params":  []interface{}{sh, status},
			})
		}
	})
}

// SetTx registers a raw transaction hex for blockchain.transaction.get.
func (f *fakeServer) SetTx(txid, rawHex string) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.txs[txid] = rawHex
}

// Broadcasts returns the raw hexes received via transaction.broadcast.
func (f *fakeServer) Broadcasts() []string {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	return append([]string(nil), f.broadcasts...)
}

func (f *fakeServer) notifyHeader(height int32, hdr *wire.BlockHeader) {
	var buf bytes.Buffer
	hdr.Serialize(&buf) //nolint:errcheck
	f.eachConn(func(cs *fakeConnState) {
		if cs.headerSub {
			cs.send(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "blockchain.headers.subscribe",
				"params": []interface{}{map[string]interface{}{
					"height": height, "hex": hex.EncodeToString(buf.Bytes()),
				}},
			})
		}
	})
}

func (f *fakeServer) eachConn(fn func(*fakeConnState)) {
	f.mtx.Lock()
	states := make([]*fakeConnState, 0, len(f.conns))
	for _, cs := range f.conns {
		states = append(states, cs)
	}
	f.mtx.Unlock()
	for _, cs := range states {
		fn(cs)
	}
}

func (cs *fakeConnState) send(v interface{}) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	cs.enc.Encode(v) //nolint:errcheck
}

func (f *fakeServer) acceptLoop() {
	for {
		conn, err := f.lis.Accept()
		if err != nil {
			return
		}
		cs := &fakeConnState{
			enc:        json.NewEncoder(conn),
			scripthash: make(map[string]bool),
		}
		f.mtx.Lock()
		if f.closed {
			f.mtx.Unlock()
			conn.Close()
			return
		}
		f.conns[conn] = cs
		f.mtx.Unlock()
		go f.serveConn(conn, cs)
	}
}

func (f *fakeServer) serveConn(conn net.Conn, cs *fakeConnState) {
	defer func() {
		conn.Close()
		f.mtx.Lock()
		delete(f.conns, conn)
		f.mtx.Unlock()
	}()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		var req struct {
			ID     interface{}       `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		f.mtx.Lock()
		scriptedErr := f.errs[req.Method]
		muted := f.muted[req.Method]
		f.mtx.Unlock()
		if muted {
			continue
		}
		if scriptedErr != "" {
			cs.send(map[string]interface{}{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]interface{}{"code": 1, "message": scriptedErr},
			})
			continue
		}
		result, errMsg := f.handle(cs, req.Method, req.Params)
		reply := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID}
		if errMsg != "" {
			reply["error"] = map[string]interface{}{"code": 1, "message": errMsg}
		} else {
			reply["result"] = result
		}
		cs.send(reply)
	}
}

func (f *fakeServer) handle(cs *fakeConnState, method string, params []json.RawMessage) (interface{}, string) {
	str := func(i int) string {
		var s string
		if i < len(params) {
			json.Unmarshal(params[i], &s) //nolint:errcheck
		}
		return s
	}
	num := func(i int) int {
		var n int
		if i < len(params) {
			json.Unmarshal(params[i], &n) //nolint:errcheck
		}
		return n
	}
	f.mtx.Lock()
	defer f.mtx.Unlock()
	switch method {
	case "server.version":
		return []string{"FakeX 0.1", "1.4"}, ""
	case "server.features":
		return map[string]interface{}{
			"genesis_hash": f.chain[0].BlockHash().String(),
		}, ""
	case "server.peers.subscribe":
		return f.peers, ""
	case "blockchain.headers.subscribe":
		cs.headerSub = true
		tip := len(f.chain) - 1
		var buf bytes.Buffer
		f.chain[tip].Serialize(&buf) //nolint:errcheck
		claimed := tip
		if f.claimTip != 0 {
			claimed = f.claimTip
		}
		return map[string]interface{}{
			"height": claimed, "hex": hex.EncodeToString(buf.Bytes()),
		}, ""
	case "blockchain.block.headers":
		start, count := num(0), num(1)
		f.headerCalls++
		if f.onBlockHeaders != nil {
			f.onBlockHeaders(f.headerCalls, start, count)
		}
		var buf bytes.Buffer
		n := 0
		for h := start; h < len(f.chain) && n < count; h++ {
			f.chain[h].Serialize(&buf) //nolint:errcheck
			n++
		}
		return map[string]interface{}{
			"count": n, "hex": hex.EncodeToString(buf.Bytes()), "max": 2016,
		}, ""
	case "blockchain.scripthash.subscribe":
		sh := str(0)
		cs.scripthash[sh] = true
		if len(f.hist[sh]) == 0 {
			return nil, ""
		}
		return fakeStatus(f.hist[sh]), ""
	case "blockchain.scripthash.unsubscribe":
		delete(cs.scripthash, str(0))
		return true, ""
	case "blockchain.scripthash.get_history":
		return f.hist[str(0)], ""
	case "blockchain.transaction.get":
		if raw, ok := f.txs[str(0)]; ok {
			return raw, ""
		}
		return nil, "tx not found"
	case "blockchain.transaction.broadcast":
		raw := str(0)
		f.broadcasts = append(f.broadcasts, raw)
		b, err := hex.DecodeString(raw)
		if err != nil {
			return nil, "bad hex"
		}
		tx := bisonwire.Tx{Chain: "btc"}
		if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
			return nil, "bad tx"
		}
		return tx.TxHash().String(), ""
	case "blockchain.transaction.get_merkle":
		// Single-tx block: empty branch, root == txid. Tests that need
		// real inclusion checking set the header root to match.
		return map[string]interface{}{
			"block_height": num(1), "merkle": []string{}, "pos": 0,
		}, ""
	case "blockchain.estimatefee":
		return 0.0001, ""
	}
	return nil, fmt.Sprintf("unhandled method %s", method)
}

// fakeStatus mirrors the electrum status derivation: sha256 over concatenated
// "txid:height:" in history order.
func fakeStatus(hist []HistoryEntry) string {
	var b bytes.Buffer
	for _, h := range hist {
		fmt.Fprintf(&b, "%s:%d:", h.TxHash, h.Height)
	}
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

// TestFakeServerSmoke drives a full Network lifecycle against the fake: dial,
// handshake, header sync from genesis, live block notification, scripthash
// subscribe + notification.
func TestFakeServerSmoke(t *testing.T) {
	f, params := newFakeServer(t, 25)
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:         chainCfg,
		TrustedServer: f.addr(),
		DataDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopAndWait(net_)

	waitFor(t, "header sync", func() bool { return net_.Synced() && net_.Tip() == 24 })

	f.MineBlocks(2)
	waitFor(t, "tip notification", func() bool { return net_.Tip() == 26 })

	sh := "aa" + fakeStatus(nil)[2:] // arbitrary 32-byte hex
	status, err := net_.SubscribeScripthash(t.Context(), sh)
	if err != nil {
		t.Fatalf("SubscribeScripthash: %v", err)
	}
	if status.Status != "" {
		t.Fatalf("fresh scripthash status = %q, want empty", status.Status)
	}
	f.SetHistory(sh, []HistoryEntry{{Height: 26, TxHash: "ab"}})
	waitFor(t, "scripthash notification", func() bool {
		select {
		case st := <-net_.ScripthashChan():
			return st.Scripthash == sh && st.Status == fakeStatus([]HistoryEntry{{Height: 26, TxHash: "ab"}})
		default:
			return false
		}
	})
}

// stopAndWait stops the network and blocks until teardown finishes. The
// closer flushes the server pool's coalesced save as its last write, so a
// test that removes its TempDir without waiting on Done races that write.
func stopAndWait(net_ *Network) {
	net_.Stop()
	<-net_.Done()
}

// waitFor polls cond until true or fails the test after 10 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReplaySubscriptions covers rotation plan R2: subscriptions registered
// through the Network are replayed onto a fresh node, and activity that
// occurred while blind is delivered through the replayed status.
func TestReplaySubscriptions(t *testing.T) {
	fA, params := newFakeServer(t, 12)
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:         chainCfg,
		TrustedServer: fA.addr(),
		DataDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopAndWait(net_)
	waitFor(t, "sync", func() bool { return net_.Synced() })

	// Subscribe two scripthashes; one unsubscribes again and must NOT be
	// replayed.
	sh1, sh2 := fakeStatus([]HistoryEntry{{TxHash: "s1"}}), fakeStatus([]HistoryEntry{{TxHash: "s2"}})
	for _, sh := range []string{sh1, sh2} {
		if _, err := net_.SubscribeScripthash(t.Context(), sh); err != nil {
			t.Fatalf("subscribe %s: %v", sh, err)
		}
	}
	net_.UnsubscribeScripthash(t.Context(), sh2)

	// Server B saw a payment to sh1 that A's subscription never reported
	// (the blind gap).
	fB, _ := newFakeServer(t, 12)
	missed := []HistoryEntry{{Height: 9, TxHash: "feed"}}
	fB.mtx.Lock()
	fB.hist[sh1] = missed
	fB.mtx.Unlock()

	nodeB, err := NewNode(t.Context(), fB.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode(B): %v", err)
	}
	defer nodeB.Close()

	if err := net_.replaySubscriptions(t.Context(), nodeB); err != nil {
		t.Fatalf("replaySubscriptions: %v", err)
	}

	waitFor(t, "replayed status for sh1", func() bool {
		select {
		case st := <-net_.ScripthashChan():
			return st.Scripthash == sh1 && st.Status == fakeStatus(missed)
		default:
			return false
		}
	})

	// The unsubscribed scripthash must not have been re-established on B.
	fB.mtx.Lock()
	defer fB.mtx.Unlock()
	for _, cs := range fB.conns {
		if cs.scripthash[sh2] {
			t.Fatal("unsubscribed scripthash was replayed")
		}
		if !cs.scripthash[sh1] {
			t.Fatal("sh1 not subscribed on successor")
		}
	}
}

// TestSurgicalRotation kills the wallet server mid-session and asserts the
// full R4 story: the live observer is promoted in place, the subscription
// replay delivers a payment the dead server never reported, the header
// stream continues from the successor, and nothing above ElectrumNetwork
// observes the swap (no channel close).
func TestSurgicalRotation(t *testing.T) {
	// Deterministic mining makes two independently constructed fakes carry
	// the identical chain — B is a same-chain peer of A.
	fA, params := newFakeServer(t, 15)
	fB, _ := newFakeServer(t, 15)

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fA.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopAndWait(net_)
	waitFor(t, "sync", func() bool { return net_.Synced() })
	waitFor(t, "observer on B", func() bool { return net_.ObserverCount() == 1 })

	sh := fakeStatus([]HistoryEntry{{TxHash: "rot"}})
	if _, err := net_.SubscribeScripthash(t.Context(), sh); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// A payment lands that only B knows about — A is about to die and
	// never reports it.
	missed := []HistoryEntry{{Height: 14, TxHash: "cafe"}}
	fB.mtx.Lock()
	fB.hist[sh] = missed
	fB.mtx.Unlock()

	fA.Close()

	waitFor(t, "rotation to B", func() bool {
		return net_.currentWalletAddr() == fB.addr().Addr
	})
	waitFor(t, "replayed missed payment", func() bool {
		select {
		case st, ok := <-net_.ScripthashChan():
			if !ok {
				t.Fatal("scripthash channel closed during rotation")
			}
			return st.Scripthash == sh && st.Status == fakeStatus(missed)
		default:
			return false
		}
	})

	// The header stream continues from the successor.
	tipBefore := net_.Tip()
	fB.MineBlocks(2)
	waitFor(t, "tip advances via B", func() bool { return net_.Tip() == tipBefore+2 })

	select {
	case <-net_.Done():
		t.Fatal("network shut down during surgical rotation")
	default:
	}
}

// resetServerHealth clears the package-level health and quarantine state
// left by other tests.
func resetServerHealth() {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()
}

// tightenObserverLoop shortens the observer compare and degraded retry
// intervals for the test's lifetime.
func tightenObserverLoop(t *testing.T) {
	t.Helper()
	oldInterval, oldRetry := observerCompareInterval, degradedRetryInterval
	observerCompareInterval = 50 * time.Millisecond
	degradedRetryInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		observerCompareInterval = oldInterval
		degradedRetryInterval = oldRetry
	})
}

// isQuarantined reports the process-lifetime quarantine flag for addr.
func isQuarantined(addr string) bool {
	serverHealth.mtx.Lock()
	defer serverHealth.mtx.Unlock()
	return serverHealth.quarantined[addr]
}

// startDivergentPair starts a network whose wallet server A and observer B
// carry equal-work competing chains: B forks three blocks below the tip and
// mines back to the same height. Returns the network and the fakes.
func startDivergentPair(t *testing.T) (*Network, *fakeServer, *fakeServer) {
	t.Helper()
	fA, params := newFakeServer(t, 15)
	fB, _ := newFakeServer(t, 15)
	fB.Fork(11, 3)
	if fB.Tip() != fA.Tip() {
		t.Fatalf("fork setup: tips %d vs %d", fB.Tip(), fA.Tip())
	}
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fA.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { stopAndWait(net_) })
	waitFor(t, "sync on A's chain", func() bool { return net_.Synced() && net_.Tip() == 14 })
	waitFor(t, "observer on B", func() bool { return net_.ObserverCount() == 1 })
	return net_, fA, fB
}

// TestDivergenceTie: an observer serving an equal-work competing block at our
// tip height is a tie, not a loss. Nothing rotates, nobody is demoted or
// quarantined, and the observer keeps its slot. Acting on a tie chose a side
// at random and the other side's observers reversed it on the next rotation,
// walking through every shipped server.
func TestDivergenceTie(t *testing.T) {
	tightenObserverLoop(t)
	resetServerHealth()
	net_, fA, fB := startDivergentPair(t)
	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()

	// Let the compare loop audit the tie repeatedly.
	time.Sleep(12 * observerCompareInterval)

	if got := net_.currentWalletAddr(); got != fA.addr().Addr {
		t.Fatalf("wallet server rotated to %s on an equal-work tie", got)
	}
	if isQuarantined(fA.addr().Addr) || isQuarantined(fB.addr().Addr) {
		t.Fatal("a server was quarantined on an equal-work tie")
	}
	serverHealth.mtx.Lock()
	demoted := serverHealth.m[fA.addr().Addr] != nil
	serverHealth.mtx.Unlock()
	if demoted {
		t.Fatal("wallet server demoted on an equal-work tie")
	}
	if net_.ObserverCount() != 1 {
		t.Fatalf("observer demoted on an equal-work tie (%d observers)", net_.ObserverCount())
	}
}

// TestDivergenceTieBrokenByExtension covers rotation plan R5, corrected: an
// equal-work tie is settled by the next block. Once the dissenting observer's
// chain extends, the wallet server has lost a verified divergence — it is
// demoted (never quarantined: being behind on a fork is slowness), the
// dissenter is promoted, and the header store reorgs onto its chain rewinding
// exactly to the fork.
func TestDivergenceTieBrokenByExtension(t *testing.T) {
	tightenObserverLoop(t)
	resetServerHealth()
	net_, fA, fB := startDivergentPair(t)

	var discMtx sync.Mutex
	var disc []DisconnectedBlock
	go func() {
		for tc := range net_.TipChangeChan() {
			discMtx.Lock()
			disc = append(disc, tc.Disconnected...)
			discMtx.Unlock()
		}
	}()

	// B pulls ahead by one block: its chain now carries more work.
	fB.MineBlocks(1)
	fB.mtx.Lock()
	wantTip := fB.chain[15].BlockHash()
	fB.mtx.Unlock()

	waitFor(t, "rotation to the dissenter", func() bool {
		return net_.currentWalletAddr() == fB.addr().Addr
	})
	waitFor(t, "reorg onto B's chain", func() bool {
		return net_.Tip() == 15 && net_.TipHash() == wantTip
	})

	// Exactly the orphaned blocks (14, 13, 12) were disconnected — not a
	// blind window's worth, which would have rolled the wallet's tx store
	// back through five innocent blocks.
	discMtx.Lock()
	got := append([]DisconnectedBlock(nil), disc...)
	discMtx.Unlock()
	if len(got) != 3 || got[0].Height != 14 || got[1].Height != 13 || got[2].Height != 12 {
		heights := make([]int32, 0, len(got))
		for _, d := range got {
			heights = append(heights, d.Height)
		}
		t.Fatalf("disconnected heights = %v, want [14 13 12]", heights)
	}

	// The loser is demoted, not quarantined.
	if isQuarantined(fA.addr().Addr) {
		t.Fatal("losing wallet server quarantined")
	}
	serverHealth.mtx.Lock()
	demoted := serverHealth.m[fA.addr().Addr] != nil
	serverHealth.mtx.Unlock()
	if !demoted {
		t.Fatal("losing wallet server not demoted")
	}

	// The divergence path rotates away from a LIVE node, so rotate itself
	// closes it, waking the dead node's watchNode, which hands off a
	// duplicate rotate. That duplicate must bail instead of joining the
	// successor's healthy loop group — a join there holds the rotation
	// mutex until the successor dies, wedging every future rotation.
	waitFor(t, "rotation mutex released", func() bool {
		if !net_.rot.mtx.TryLock() {
			return false
		}
		net_.rot.mtx.Unlock()
		return true
	})
	select {
	case <-net_.Done():
		t.Fatal("network shut down during reorg promotion")
	default:
	}
}

// TestDivergencePatience: a verified loss must persist across two ticks
// before the wallet server is rotated away from. The tie-breaking block
// reaches observers and the wallet server seconds apart, and a tick landing
// in that gap must not rotate away from a server about to catch up.
func TestDivergencePatience(t *testing.T) {
	resetServerHealth()

	fA, params := newFakeServer(t, 15) // wallet server
	fB, _ := newFakeServer(t, 15)      // observer on a heavier fork
	fB.Fork(11, 3)
	fB.MineBlocks(1)

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fA.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	o, tipA, tipB := installWalletAndObserver(t, net_, fA, fB, params)

	st := &observerLoopState{lagBlocks: 2}
	demoted := func() bool {
		serverHealth.mtx.Lock()
		defer serverHealth.mtx.Unlock()
		return serverHealth.m[fA.addr().Addr] != nil
	}

	// First tick: the loss is recorded, nothing acts on it.
	net_.observerTick(st)
	if st.pendingLoss == nil {
		t.Fatal("verified loss not recorded on the first tick")
	}
	if demoted() || net_.currentWalletAddr() != fA.addr().Addr {
		t.Fatal("acted on a verified loss after a single tick")
	}

	// The conflict clears before the next tick (the observer now reports
	// A's chain): the pending loss is dropped, nothing rotates.
	o.setTip(tipA)
	net_.observerTick(st)
	if st.pendingLoss != nil {
		t.Fatal("pending loss survived a tick without conflict")
	}
	if demoted() || net_.currentWalletAddr() != fA.addr().Addr {
		t.Fatal("rotated on a transient divergence")
	}

	// The conflict returns and persists: the second consecutive tick acts.
	o.setTip(tipB)
	net_.observerTick(st)
	if st.pendingLoss == nil {
		t.Fatal("verified loss not re-recorded")
	}
	net_.observerTick(st)
	waitFor(t, "rotation to the winning observer", func() bool {
		return net_.currentWalletAddr() == fB.addr().Addr
	})
	if !demoted() {
		t.Fatal("losing wallet server not demoted on a persisted loss")
	}
	if isQuarantined(fA.addr().Addr) {
		t.Fatal("losing wallet server quarantined")
	}
}

// installWalletAndObserver wires an unstarted network by hand for tests that
// drive observerTick directly: fA takes the wallet slot with the store synced
// to its chain, fB is installed as a live observer at its own tip. Returns
// the observer and both subscription tips.
func installWalletAndObserver(t *testing.T, net_ *Network, fA, fB *fakeServer,
	params *netparams.ChainParams) (o *observer, tipA, tipB *HeadersTip) {

	t.Helper()
	net_.ctx, net_.cancel = context.WithCancel(t.Context())
	net_.obsMtx.Lock()
	net_.observers = make(map[string]*observer)
	net_.obsMtx.Unlock()
	t.Cleanup(func() {
		net_.cancel()
		net_.wg.Wait()
		net_.pool.flush()
	})
	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()

	nodeA, err := NewNode(net_.ctx, fA.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode(A): %v", err)
	}
	tipA, err = nodeA.SubscribeHeaders(net_.ctx)
	if err != nil {
		t.Fatalf("SubscribeHeaders(A): %v", err)
	}
	if err := net_.syncHeaders(net_.ctx, nodeA); err != nil {
		t.Fatalf("syncHeaders: %v", err)
	}
	net_.mu.Lock()
	net_.node = nodeA
	net_.mu.Unlock()

	nodeB, err := NewNode(net_.ctx, fB.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode(B): %v", err)
	}
	tipB, err = nodeB.SubscribeHeaders(net_.ctx)
	if err != nil {
		t.Fatalf("SubscribeHeaders(B): %v", err)
	}
	o = &observer{node: nodeB, stopDrain: make(chan struct{})}
	o.setTip(tipB)
	net_.obsMtx.Lock()
	net_.observers[fB.addr().Addr] = o
	net_.obsMtx.Unlock()
	return o, tipA, tipB
}

// TestObserverTickSkipsStalledObserver: an observer one block ahead is probed
// at the local tip every tick. One that accepts the request and never answers
// (while still answering pings, so its socket stays alive) must not park the
// tick — the same loop runs lag detection, observer replacement and peer
// discovery — and is not a divergence.
func TestObserverTickSkipsStalledObserver(t *testing.T) {
	resetServerHealth()
	oldTimeout := rpcTimeout
	rpcTimeout = 200 * time.Millisecond
	t.Cleanup(func() { rpcTimeout = oldTimeout })

	fA, params := newFakeServer(t, 15) // wallet server
	fB, _ := newFakeServer(t, 16)      // observer, same chain, one block ahead
	fB.MuteMethod("blockchain.block.headers", true)

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fA.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	installWalletAndObserver(t, net_, fA, fB, params)

	st := &observerLoopState{lagBlocks: 1}
	start := time.Now()
	net_.observerTick(st)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("observerTick took %v against a stalled observer; probe unbounded", elapsed)
	}
	if st.pendingLoss != nil {
		t.Fatal("a stalled probe was recorded as a divergence")
	}
	if st.lagSince.IsZero() {
		t.Fatal("lag bookkeeping did not advance on the same tick")
	}
	if net_.currentWalletAddr() != fA.addr().Addr {
		t.Fatal("wallet slot changed on a stalled probe")
	}
	serverHealth.mtx.Lock()
	demoted := serverHealth.m[fB.addr().Addr] != nil
	serverHealth.mtx.Unlock()
	if demoted {
		t.Fatal("stalled observer demoted by a probe timeout")
	}
}

// TestSameHeightReorg: the wallet server switching sides of a same-height
// tie notifies a header at the store's own height with a different hash. The
// store must follow at once, rewinding exactly the losing block, rather than
// wait for the next block to fail to link.
func TestSameHeightReorg(t *testing.T) {
	f, params := newFakeServer(t, 15)
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:         chainCfg,
		TrustedServer: f.addr(),
		DataDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopAndWait(net_)
	waitFor(t, "sync", func() bool { return net_.Synced() && net_.Tip() == 14 })

	var discMtx sync.Mutex
	var disc []DisconnectedBlock
	go func() {
		for tc := range net_.TipChangeChan() {
			discMtx.Lock()
			disc = append(disc, tc.Disconnected...)
			discMtx.Unlock()
		}
	}()

	oldTip := net_.TipHash()
	f.Fork(13, 1) // same height, new hash
	f.mtx.Lock()
	wantTip := f.chain[14].BlockHash()
	f.mtx.Unlock()
	if wantTip == oldTip {
		t.Fatal("fork setup: tip unchanged")
	}
	waitFor(t, "same-height reorg", func() bool {
		return net_.Tip() == 14 && net_.TipHash() == wantTip
	})
	discMtx.Lock()
	got := append([]DisconnectedBlock(nil), disc...)
	discMtx.Unlock()
	if len(got) != 1 || got[0].Height != 14 || got[0].Hash != oldTip {
		t.Fatalf("disconnected = %+v, want exactly the old block 14", got)
	}
}

// TestElectionPrefersWinningChain: with two live observers, one on the
// wallet server's (losing) chain and one on the winning chain, the
// divergence rotation lands on the winning observer even though the other
// precedes it in election order. Landing on the loser would drag the store
// back onto the losing chain and repeat the divergence next tick.
func TestElectionPrefersWinningChain(t *testing.T) {
	if maxObservers < 2 {
		// The mobile build keeps one observer; the preference between
		// two live observers cannot arise there.
		t.Skipf("needs two observer slots, build has %d", maxObservers)
	}
	tightenObserverLoop(t)
	resetServerHealth()

	fA, params := newFakeServer(t, 15) // wallet server
	fC, _ := newFakeServer(t, 15)      // same chain as A, first in election order
	fB, _ := newFakeServer(t, 15)      // forks, then wins
	fB.Fork(11, 3)

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fA.addr(), fC.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopAndWait(net_)
	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()
	waitFor(t, "sync on A's chain", func() bool { return net_.Synced() && net_.Tip() == 14 })
	waitFor(t, "observers on B and C", func() bool { return net_.ObserverCount() == 2 })

	fB.MineBlocks(1)
	fB.mtx.Lock()
	wantTip := fB.chain[15].BlockHash()
	fB.mtx.Unlock()
	waitFor(t, "rotation to the winning observer", func() bool {
		return net_.currentWalletAddr() == fB.addr().Addr && net_.TipHash() == wantTip
	})
	if isQuarantined(fC.addr().Addr) {
		t.Fatal("bystander observer on the losing chain quarantined")
	}
}

// TestTryElectSkipsLosingChain: under a divergence hint, a fresh-dialed
// candidate serving the losing header at the conflict height is skipped even
// though it precedes the winning server in election order, and the election
// attaches to the winner, reorging the store onto its chain.
func TestTryElectSkipsLosingChain(t *testing.T) {
	resetServerHealth()

	fC, params := newFakeServer(t, 15) // losing chain, first in election order
	fB, _ := newFakeServer(t, 15)      // winning chain
	fB.Fork(11, 3)
	fB.MineBlocks(1)

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fC.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	net_.ctx, net_.cancel = context.WithCancel(t.Context())
	net_.obsMtx.Lock()
	net_.observers = make(map[string]*observer)
	net_.obsMtx.Unlock()
	t.Cleanup(func() {
		net_.cancel()
		net_.wg.Wait()
		net_.pool.flush()
	})
	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()

	// Seed the store with the losing chain.
	seed, err := NewNode(net_.ctx, fC.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if err := net_.syncHeaders(net_.ctx, seed); err != nil {
		t.Fatalf("syncHeaders: %v", err)
	}
	seed.Close()

	fB.mtx.Lock()
	winner := fB.chain[14].BlockHash()
	wantTip := fB.chain[15].BlockHash()
	fB.mtx.Unlock()
	hint := &electionHint{wantHeight: 14, wantHash: winner}

	net_.rot.mtx.Lock()
	ok := net_.tryElect("", hint)
	net_.rot.mtx.Unlock()
	if !ok {
		t.Fatal("election found no successor")
	}
	if got := net_.currentWalletAddr(); got != fB.addr().Addr {
		t.Fatalf("elected %s, want the winning server %s", got, fB.addr().Addr)
	}
	if net_.Tip() != 15 || net_.TipHash() != wantTip {
		t.Fatalf("store at %d/%s, want 15/%s", net_.Tip(), net_.TipHash(), wantTip)
	}
	if isQuarantined(fC.addr().Addr) {
		t.Fatal("skipped candidate quarantined")
	}
}

// TestTryElectSkipsStalledCandidate: a candidate that completes the handshake
// and answers pings but never answers the winning-chain probe must not hold
// the election — and with it the rotation mutex — on the network context.
// The election moves on to the next candidate within the probe budget.
func TestTryElectSkipsStalledCandidate(t *testing.T) {
	resetServerHealth()
	oldTimeout := rpcTimeout
	rpcTimeout = 200 * time.Millisecond
	t.Cleanup(func() { rpcTimeout = oldTimeout })

	fC, params := newFakeServer(t, 16) // stalled, first in election order
	fB, _ := newFakeServer(t, 16)      // healthy, same chain
	fC.MuteMethod("blockchain.block.headers", true)

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fC.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	net_.ctx, net_.cancel = context.WithCancel(t.Context())
	net_.obsMtx.Lock()
	net_.observers = make(map[string]*observer)
	net_.obsMtx.Unlock()
	t.Cleanup(func() {
		net_.cancel()
		net_.wg.Wait()
		net_.pool.flush()
	})
	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()

	seed, err := NewNode(net_.ctx, fB.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if err := net_.syncHeaders(net_.ctx, seed); err != nil {
		t.Fatalf("syncHeaders: %v", err)
	}
	seed.Close()

	// Both tips sit above the conflict height, so the candidate is judged
	// by fetching the header there — the request fC swallows.
	fB.mtx.Lock()
	winner := fB.chain[14].BlockHash()
	fB.mtx.Unlock()
	hint := &electionHint{wantHeight: 14, wantHash: winner}

	start := time.Now()
	net_.rot.mtx.Lock()
	ok := net_.tryElect("", hint)
	net_.rot.mtx.Unlock()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("election took %v against a stalled candidate; probe unbounded", elapsed)
	}
	if !ok {
		t.Fatal("election found no successor")
	}
	if got := net_.currentWalletAddr(); got != fB.addr().Addr {
		t.Fatalf("elected %s, want the healthy server %s", got, fB.addr().Addr)
	}
}

// TestDegradedSelfHealing covers rotation plan R6: when every candidate is
// down, the network idles degraded — wallet slot empty, channels open — and
// recovers by itself once a server answers again.
func TestDegradedSelfHealing(t *testing.T) {
	oldRetry := degradedRetryInterval
	degradedRetryInterval = 50 * time.Millisecond
	t.Cleanup(func() { degradedRetryInterval = oldRetry })
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	fA, params := newFakeServer(t, 10)
	deadAddr := &ServerAddr{Proto: "tcp", Addr: "127.0.0.1:1"}

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fA.addr(), deadAddr},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopAndWait(net_)
	waitFor(t, "sync", func() bool { return net_.Synced() })

	// Drop A's connections. rotate excludes the just-dead server and the
	// only other candidate refuses, so the network degrades — then the
	// retry loop re-elects A (its listener is still up).
	fA.KillConns()

	waitFor(t, "self-healed back onto A", func() bool {
		return net_.currentWalletAddr() == fA.addr().Addr
	})
	tipBefore := net_.Tip()
	fA.MineBlocks(1)
	waitFor(t, "tip advances after recovery", func() bool {
		return net_.Tip() == tipBefore+1
	})
	select {
	case <-net_.Done():
		t.Fatal("network shut down while degraded")
	default:
	}
}

// TestRotationRecoversReorg: the wallet server dies and the chain reorgs
// below our stored tip before a successor attaches, so every candidate's
// headers fail to connect. attachNode's header sync must rewind to the fork
// and follow the successor's chain — without that, every candidate on the
// honest chain fails identically and the network degrades permanently (and
// a restart dies the same way in Start's sync).
func TestRotationRecoversReorg(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	// Deterministic mining: A and B carry identical chains.
	fA, params := newFakeServer(t, 15)
	fB, _ := newFakeServer(t, 15)

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fA.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopAndWait(net_)
	waitFor(t, "sync", func() bool { return net_.Synced() && net_.Tip() == 14 })

	// Drain tip events so reorg disconnect delivery can't block.
	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()

	// B reorgs one block below our tip and extends past it, then A dies.
	// The successor's chain now conflicts below every candidate's first
	// header batch.
	fB.Fork(13, 2)
	fB.mtx.Lock()
	wantTip := fB.chain[15].BlockHash()
	fB.mtx.Unlock()
	fA.Close()

	waitFor(t, "rotation onto the reorged successor", func() bool {
		return net_.currentWalletAddr() == fB.addr().Addr &&
			net_.Tip() == 15 && net_.TipHash() == wantTip
	})
	select {
	case <-net_.Done():
		t.Fatal("network shut down during reorg rotation")
	default:
	}
}

// TestAttachNodeReconcileTerminates: attachNode's tip-conflict reconciliation
// is handed a tip snapshot whose hash the server does not serve (a stale
// observer snapshot after a server-side reorg, or a lying server). The loop
// must terminate — by aligning with the chain the server serves NOW or by
// erroring out — rather than rewinding and refilling the same blocks against
// the stale target forever while flooding the consumer with disconnects.
func TestAttachNodeReconcileTerminates(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	fB, params := newFakeServer(t, 15)
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:         chainCfg,
		TrustedServer: fB.addr(),
		DataDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	// No Start: drive attachNode directly against a live node, the shape
	// tryElect uses under rotMtx.
	net_.ctx, net_.cancel = context.WithCancel(t.Context())
	defer net_.cancel()

	node, err := NewNode(net_.ctx, fB.addr(), "",
		params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	defer node.Close()
	if _, err := node.SubscribeHeaders(net_.ctx); err != nil {
		t.Fatalf("SubscribeHeaders: %v", err)
	}
	if err := net_.syncHeaders(net_.ctx, node); err != nil {
		t.Fatalf("syncHeaders: %v", err)
	}
	if net_.Tip() != 14 {
		t.Fatalf("tip = %d, want 14", net_.Tip())
	}

	// The server reorgs at our tip height (same height, new hash), and the
	// snapshot handed to attachNode claims a THIRD hash neither chain has:
	// the loop's exit test can never match it.
	fB.Fork(13, 1)
	bogus := mineTestHeader(t, net_.TipHash(), easyBits, 424242)
	var buf bytes.Buffer
	if err := bogus.Serialize(&buf); err != nil {
		t.Fatalf("serialize bogus header: %v", err)
	}
	staleTip := &HeadersTip{Height: 14, Hex: hex.EncodeToString(buf.Bytes())}

	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()

	attachDone := make(chan error, 1)
	go func() { attachDone <- net_.attachNode(node, staleTip) }()
	select {
	case err := <-attachDone:
		if err != nil {
			t.Fatalf("attachNode: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("attachNode still spinning after 15s")
	}

	// Terminated by following the server's CURRENT chain.
	fB.mtx.Lock()
	wantTip := fB.chain[14].BlockHash()
	fB.mtx.Unlock()
	if net_.TipHash() != wantTip {
		t.Fatalf("tip hash = %s, want the server's current tip %s",
			net_.TipHash(), wantTip)
	}
	net_.cancel()
	net_.wg.Wait()
}

// TestStartEmptyCandidates: Start with every shipped server quarantined must
// fail with an error, not panic. A divergence audit quarantines the wallet
// server for 24 hours (persisted), and the wallet rebuilds the Network on
// every reconnect — on a chain with a short shipped list the election can
// come up empty, and Start's dial loop leaves its tip nil in that case.
func TestStartEmptyCandidates(t *testing.T) {
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	f, params := newFakeServer(t, 10)
	serverHealth.mtx.Lock()
	serverHealth.quarantined[f.addr().Addr] = true
	serverHealth.mtx.Unlock()

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:         chainCfg,
		TrustedServer: f.addr(),
		DataDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err == nil {
		net_.Stop()
		t.Fatal("Start succeeded with every candidate quarantined")
	}
}

// TestLagRotation covers the CONC-2 regression: a wallet server that lags the
// observers rotates surgically (demote + promote), it does NOT tear the
// network down (the pre-fix behavior called Stop directly).
func TestLagRotation(t *testing.T) {
	oldInterval, oldPatience := observerCompareInterval, lagPatience
	observerCompareInterval = 50 * time.Millisecond
	lagPatience = 100 * time.Millisecond
	t.Cleanup(func() { observerCompareInterval = oldInterval; lagPatience = oldPatience })
	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	fA, params := newFakeServer(t, 12) // wallet server, will lag
	fB, _ := newFakeServer(t, 12)      // observer, mines ahead

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fA.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopAndWait(net_)
	waitFor(t, "sync on A", func() bool { return net_.Synced() && net_.Tip() == 11 })
	waitFor(t, "observer on B", func() bool { return net_.ObserverCount() == 1 })

	// B (deterministic same chain) mines several blocks ahead; A stays put,
	// so the wallet server lags past the 2-block threshold.
	fB.MineBlocks(4)

	waitFor(t, "rotation to the caught-up server", func() bool {
		return net_.currentWalletAddr() == fB.addr().Addr && net_.Tip() == 15
	})
	select {
	case <-net_.Done():
		t.Fatal("network torn down on lag instead of rotating")
	default:
	}
}

// syncOnce runs syncHeaders against f on a fresh, unstarted network with the
// given params and returns the network and the sync error.
func syncOnce(t *testing.T, f *fakeServer, params *netparams.ChainParams) (*Network, error) {
	t.Helper()
	net_ := newUnstartedNetwork(t, f, params)
	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()
	node, err := NewNode(net_.ctx, f.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	defer node.Close()
	return net_, net_.syncHeaders(net_.ctx, node)
}

// TestSyncRequiresNewestCheckpoint: a server whose chain ends below the newest
// checkpoint fails the sync and the store is not marked synced; once the
// chain passes the checkpoint, the sync succeeds.
func TestSyncRequiresNewestCheckpoint(t *testing.T) {
	resetServerHealth()
	f, params := newFakeServer(t, 20)
	f.mtx.Lock()
	params.Checkpoints = []netparams.Checkpoint{
		{Height: 0, Hash: ptrHash(f.chain[0].BlockHash())},
		{Height: 15, Hash: ptrHash(f.chain[15].BlockHash())},
	}
	full := f.chain
	f.chain = full[:12]
	f.mtx.Unlock()

	net_, err := syncOnce(t, f, params)
	if err == nil || !strings.Contains(err.Error(), "without the checkpoint at 15") {
		t.Fatalf("sync of a chain ending at 11: got %v", err)
	}
	if net_.Synced() {
		t.Fatal("store marked synced below the newest checkpoint")
	}

	f.mtx.Lock()
	f.chain = full
	f.mtx.Unlock()
	net_, err = syncOnce(t, f, params)
	if err != nil {
		t.Fatalf("sync past the checkpoint: %v", err)
	}
	if !net_.Synced() || net_.Tip() != 19 {
		t.Fatalf("synced %v at tip %d, want synced at 19", net_.Synced(), net_.Tip())
	}
}

// TestSyncPoWIgnoresClaimedTip: a server claiming a tip of 2^31-1 gets no
// headers past the PoW check. A tip-relative PoW window would place every
// synced header far below the claimed tip and skip the hash check entirely.
func TestSyncPoWIgnoresClaimedTip(t *testing.T) {
	resetServerHealth()
	f, params := newFakeServer(t, 12)
	const bad = 6
	f.mtx.Lock()
	f.claimTip = math.MaxInt32
	f.chain[bad] = unminedHeader(f.chain[bad-1].BlockHash(), time.Unix(1_600_000_000, 0))
	for i := bad + 1; i < len(f.chain); i++ {
		f.chain[i] = mineTestHeader(t, f.chain[i-1].BlockHash(), easyBits, uint32(i)*7919)
	}
	f.mtx.Unlock()

	net_ := newUnstartedNetwork(t, f, params)
	node, err := NewNode(net_.ctx, f.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	defer node.Close()
	tip, err := node.SubscribeHeaders(net_.ctx)
	if err != nil {
		t.Fatalf("SubscribeHeaders: %v", err)
	}
	if tip.Height != math.MaxInt32 {
		t.Fatalf("test setup: server claims tip %d", tip.Height)
	}
	err = net_.syncHeaders(net_.ctx, node)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("header at %d:", bad)) {
		t.Fatalf("sync with an unmined header at %d: got %v", bad, err)
	}
	if net_.Synced() || net_.Tip() >= bad {
		t.Fatalf("synced %v at tip %d after a PoW failure at %d", net_.Synced(), net_.Tip(), bad)
	}
}

// TestTimeTooNewNoServerPenalty: an initial sync that fails on a header dated
// past the future bound records no failure against the server, since a slow
// local clock produces the same error with every honest server.
func TestTimeTooNewNoServerPenalty(t *testing.T) {
	resetServerHealth()
	f, params := newFakeServer(t, 8)
	f.mtx.Lock()
	tip := &wire.BlockHeader{Version: 4, PrevBlock: f.chain[6].BlockHash(), Bits: easyBits,
		Timestamp: time.Now().Add(maxFutureBlockTime + time.Hour)}
	for target := blockchain.CompactToBig(easyBits); ; tip.Nonce++ {
		if hash := tip.BlockHash(); blockchain.HashToBig(&hash).Cmp(target) <= 0 {
			break
		}
	}
	f.chain[7] = tip
	f.mtx.Unlock()

	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{f.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopAndWait(net_)
	select {
	case <-net_.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("network did not stop after the failed sync")
	}
	if net_.Synced() {
		t.Fatal("synced past a future-dated header")
	}
	serverHealth.mtx.Lock()
	r := serverHealth.m[f.addr().Addr]
	serverHealth.mtx.Unlock()
	if r != nil && r.failures > 0 {
		t.Fatalf("server charged %d failures for a future-dated header", r.failures)
	}
}

// TestSyncAfterCheckpointUpgrade: a store synced under one checkpoint set is
// reopened under a release that adds a checkpoint below its tip on another
// branch. The load truncates the store to the checkpoint it holds, and the
// sync follows the server's chain through the new checkpoint to Synced.
func TestSyncAfterCheckpointUpgrade(t *testing.T) {
	resetServerHealth()
	fA, params := newFakeServer(t, 15)
	fB, _ := newFakeServer(t, 15) // identical chain until forked
	fB.Fork(6, 10)
	dataDir := t.TempDir()

	run := func(p *netparams.ChainParams, f *fakeServer) *Network {
		t.Helper()
		chainCfg, err := NewChainConfig(bisonwire.ChainBTC, p)
		if err != nil {
			t.Fatalf("NewChainConfig: %v", err)
		}
		net_, err := NewNetwork(&Config{Chain: chainCfg, Servers: []*ServerAddr{f.addr()}, DataDir: dataDir})
		if err != nil {
			t.Fatalf("NewNetwork: %v", err)
		}
		if err := net_.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		return net_
	}
	first := run(params, fA)
	waitFor(t, "sync on A's chain", func() bool { return first.Synced() && first.Tip() == 14 })
	stopAndWait(first)

	fB.mtx.Lock()
	cp8, tipB := fB.chain[8].BlockHash(), fB.chain[16].BlockHash()
	fB.mtx.Unlock()
	upgraded := &netparams.ChainParams{
		Chain:              params.Chain,
		Net:                params.Net,
		GenesisBlock:       params.GenesisBlock,
		GenesisHash:        params.GenesisHash,
		PoWNoRetargeting:   true,
		TargetTimePerBlock: params.TargetTimePerBlock,
		Checkpoints: []netparams.Checkpoint{
			{Height: 0, Hash: params.GenesisHash},
			{Height: 8, Hash: &cp8},
		},
	}
	second := run(upgraded, fB)
	defer stopAndWait(second)
	waitFor(t, "sync through the new checkpoint", func() bool {
		return second.Synced() && second.Tip() == 16 && second.TipHash() == tipB
	})
}

// lowWorkBits encodes a target near 2^256, so a block mined at it carries
// about one hash of work, far less than an easyBits block.
const lowWorkBits = 0x2100ffff

// tipEvents collects the TipChange events a network publishes, in order.
type tipEvents struct {
	mtx    sync.Mutex
	events []*TipChange
}

func collectTipEvents(net_ *Network) *tipEvents {
	te := &tipEvents{}
	go func() {
		for tc := range net_.TipChangeChan() {
			te.mtx.Lock()
			te.events = append(te.events, tc)
			te.mtx.Unlock()
		}
	}()
	return te
}

func (te *tipEvents) snapshot() []*TipChange {
	te.mtx.Lock()
	defer te.mtx.Unlock()
	return append([]*TipChange(nil), te.events...)
}

// disconnected returns every disconnected height published, in order.
func (te *tipEvents) disconnected() []int32 {
	var out []int32
	for _, tc := range te.snapshot() {
		for _, d := range tc.Disconnected {
			out = append(out, d.Height)
		}
	}
	return out
}

// startSynced starts a network on f alone and waits for it to sync to tip.
func startSynced(t *testing.T, f *fakeServer, params *netparams.ChainParams, tip int32) *Network {
	t.Helper()
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{Chain: chainCfg, TrustedServer: f.addr(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { stopAndWait(net_) })
	waitFor(t, "sync", func() bool { return net_.Synced() && net_.Tip() == tip })
	return net_
}

// TestHeavierBranchSwapped: a heavier branch from the wallet server is swapped
// in, its disconnects are published exactly once, and the connect cursor
// resets to the fork so the new chain is published from there.
func TestHeavierBranchSwapped(t *testing.T) {
	resetServerHealth()
	f, params := newFakeServer(t, 15)
	net_ := startSynced(t, f, params, 14)
	events := collectTipEvents(net_)

	f.Fork(11, 5) // replaces 12-14 and extends to 16
	f.mtx.Lock()
	wantTip := f.chain[16].BlockHash()
	f.mtx.Unlock()
	waitFor(t, "swap onto the heavier branch", func() bool {
		return net_.Tip() == 16 && net_.TipHash() == wantTip
	})
	waitFor(t, "connects published", func() bool {
		evs := events.snapshot()
		return len(evs) > 0 && len(evs[len(evs)-1].Connected) > 0 &&
			evs[len(evs)-1].Connected[len(evs[len(evs)-1].Connected)-1] == 16
	})
	if got := events.disconnected(); len(got) != 3 || got[0] != 14 || got[2] != 12 {
		t.Fatalf("disconnected heights %v, want [14 13 12] once", got)
	}
	var connected []int32
	sawDisconnect := false
	for _, tc := range events.snapshot() {
		if len(tc.Disconnected) > 0 {
			sawDisconnect = true
			continue
		}
		if sawDisconnect {
			connected = append(connected, tc.Connected...)
		}
	}
	if len(connected) == 0 || connected[0] != 12 || connected[len(connected)-1] != 16 {
		t.Fatalf("connects after the swap %v, want 12 through 16", connected)
	}
}

// TestLowerWorkBranchRefused: a longer branch from the wallet server carrying
// less work is refused; the store and the consumer are untouched.
func TestLowerWorkBranchRefused(t *testing.T) {
	resetServerHealth()
	f, params := newFakeServer(t, 15)
	net_ := startSynced(t, f, params, 14)
	events := collectTipEvents(net_)
	oldTip := net_.TipHash()

	// Seven blocks to tip 17 carry 7 units of work against the store's 8
	// over 11-14 (CalcWork is 1 at lowWorkBits and 2 at easyBits).
	if blockchain.CalcWork(lowWorkBits).Int64() != 1 || blockchain.CalcWork(easyBits).Int64() != 2 {
		t.Fatal("test setup: unexpected per-block work")
	}
	f.ForkWithBits(10, 7, lowWorkBits)
	waitFor(t, "notification applied", func() bool { return net_.TargetTip() == 17 })
	time.Sleep(200 * time.Millisecond) // let the catch-up run to its refusal
	if net_.Tip() != 14 || net_.TipHash() != oldTip {
		t.Fatalf("store moved to %d onto a lower-work branch", net_.Tip())
	}
	if got := events.disconnected(); len(got) != 0 {
		t.Fatalf("published disconnects %v for a refused branch", got)
	}
}

// TestSwapDisconnectsPublishedOnAppendError: when the append fails after a
// swap's rewind, the store has already dropped the old blocks, so their
// disconnects reach the consumer anyway. The store then resyncs from the fork
// once persistence recovers.
func TestSwapDisconnectsPublishedOnAppendError(t *testing.T) {
	resetServerHealth()
	f, params := newFakeServer(t, 15)
	net_ := startSynced(t, f, params, 14)
	events := collectTipEvents(net_)

	h := net_.headers
	h.mu.Lock()
	h.writeHeaders = func([]byte) error { return errors.New("injected write failure") }
	h.mu.Unlock()

	f.Fork(12, 3) // replaces 13-14 and extends to 15
	waitFor(t, "disconnects published", func() bool {
		got := events.disconnected()
		return len(got) == 2 && got[0] == 14 && got[1] == 13
	})
	if net_.Tip() != 12 {
		t.Fatalf("tip %d after the failed append, want the fork at 12", net_.Tip())
	}

	h.mu.Lock()
	h.writeHeaders = nil
	h.mu.Unlock()
	f.MineBlocks(1)
	f.mtx.Lock()
	wantTip := f.chain[16].BlockHash()
	f.mtx.Unlock()
	waitFor(t, "resync after persistence recovers", func() bool {
		return net_.Tip() == 16 && net_.TipHash() == wantTip
	})
	if got := events.disconnected(); len(got) != 2 {
		t.Fatalf("disconnected heights %v, want the two blocks once", got)
	}
}

// divergenceRig is a network whose store holds fA's chain, with fA's node in
// the wallet slot and an observer on fB, for driving handleDivergence and
// observerTick directly.
type divergenceRig struct {
	n      *Network
	fA, fB *fakeServer
	obs    *observer
}

// newDivergenceRig builds a rig on fakes of length aLen. shape rewrites fB's
// chain before the observer subscribes, and cps, when non-nil, become the
// params' checkpoints before the store is built.
func newDivergenceRig(t *testing.T, aLen int, shape func(fB *fakeServer),
	cps func(fA *fakeServer) []netparams.Checkpoint) *divergenceRig {

	t.Helper()
	resetServerHealth()
	fA, params := newFakeServer(t, aLen)
	fB, _ := newFakeServer(t, aLen)
	if shape != nil {
		shape(fB)
	}
	if cps != nil {
		params.Checkpoints = cps(fA)
	}
	n := newUnstartedNetwork(t, fA, params)
	fA.mtx.Lock()
	chain := append([]*wire.BlockHeader(nil), fA.chain...)
	fA.mtx.Unlock()
	if _, err := n.headers.AppendBatch(0, serializeBatch(t, chain)); err != nil {
		t.Fatalf("load fA's chain: %v", err)
	}
	walletNode, err := NewNode(n.ctx, fA.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode(A): %v", err)
	}
	t.Cleanup(walletNode.Close)
	n.mu.Lock()
	n.node = walletNode
	n.mu.Unlock()
	obsNode, err := NewNode(n.ctx, fB.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode(B): %v", err)
	}
	t.Cleanup(obsNode.Close)
	tip, err := obsNode.SubscribeHeaders(n.ctx)
	if err != nil {
		t.Fatalf("SubscribeHeaders(B): %v", err)
	}
	o := &observer{node: obsNode, stopDrain: make(chan struct{})}
	o.setTip(tip)
	return &divergenceRig{n: n, fA: fA, fB: fB, obs: o}
}

// demoted reports whether addr has a failure on record.
func demoted(addr string) bool {
	serverHealth.mtx.Lock()
	defer serverHealth.mtx.Unlock()
	r := serverHealth.m[addr]
	return r != nil && r.failures > 0
}

// unpunished fails the test if the observer was demoted, quarantined or
// disconnected.
func (r *divergenceRig) unpunished(t *testing.T) {
	t.Helper()
	addr := r.fB.addr().Addr
	if demoted(addr) || isQuarantined(addr) {
		t.Fatal("observer penalized")
	}
	select {
	case <-r.obs.node.Done():
		t.Fatal("observer disconnected")
	default:
	}
}

// TestDivergenceDeepFork drives handleDivergence on an observer whose fork
// lies below the shallow audit window. A heavier deep fork is followed to a
// verified loss, a lighter one demotes the observer, and the inconclusive
// outcomes cost the observer nothing.
func TestDivergenceDeepFork(t *testing.T) {
	const aLen = 20 // store tip 19; the shallow window starts at 11

	t.Run("heavier", func(t *testing.T) {
		r := newDivergenceRig(t, aLen, func(fB *fakeServer) { fB.Fork(4, 17) }, nil)
		loss := r.n.handleDivergence(r.obs, 19)
		if loss == nil {
			t.Fatal("no loss against a heavier observer forked 15 below the tip")
		}
		r.fB.mtx.Lock()
		want := r.fB.chain[19].BlockHash()
		r.fB.mtx.Unlock()
		if loss.hint.wantHeight != 19 || loss.hint.wantHash != want {
			t.Fatalf("hint %d %s, want the observer's header at 19", loss.hint.wantHeight, loss.hint.wantHash)
		}
		r.unpunished(t)
	})

	t.Run("lighter", func(t *testing.T) {
		r := newDivergenceRig(t, aLen, func(fB *fakeServer) { fB.ForkWithBits(4, 16, lowWorkBits) }, nil)
		if loss := r.n.handleDivergence(r.obs, 19); loss != nil {
			t.Fatal("loss against a lighter observer")
		}
		if !demoted(r.fB.addr().Addr) {
			t.Fatal("lighter deep observer not demoted")
		}
	})

	t.Run("no shared header within the bound", func(t *testing.T) {
		// The fork at 5 lies below the deepest supported fork (229-200).
		r := newDivergenceRig(t, 230, func(fB *fakeServer) { fB.Fork(5, 226) }, nil)
		if loss := r.n.handleDivergence(r.obs, 229); loss != nil {
			t.Fatal("loss beyond the supported reorg depth")
		}
		r.unpunished(t)
	})

	t.Run("seam between requests", func(t *testing.T) {
		r := newDivergenceRig(t, aLen, func(fB *fakeServer) { fB.Fork(4, 17) }, nil)
		r.fB.mtx.Lock()
		moved := divergeFrom(r.fB.chain, 8, 14, 900_000) // differs from 8 up
		r.fB.onBlockHeaders = func(call, _, _ int) {
			if call == 2 { // the widened request
				r.fB.chain = moved
			}
		}
		r.fB.mtx.Unlock()
		if loss := r.n.handleDivergence(r.obs, 19); loss != nil {
			t.Fatal("loss from responses that do not link")
		}
		r.unpunished(t)
	})

	t.Run("store moves during the audit", func(t *testing.T) {
		r := newDivergenceRig(t, aLen, func(fB *fakeServer) { fB.Fork(4, 17) }, nil)
		next := mineTestHeader(t, r.n.headers.TipHash(), easyBits, 555)
		r.fB.mtx.Lock()
		r.fB.onBlockHeaders = func(call, _, _ int) {
			if call == 1 {
				if err := r.n.headers.AppendOne(next); err != nil {
					t.Errorf("move the store: %v", err)
				}
			}
		}
		r.fB.mtx.Unlock()
		if loss := r.n.handleDivergence(r.obs, 19); loss != nil {
			t.Fatal("verdict against a store that moved during the audit")
		}
		r.unpunished(t)
	})

	t.Run("batch not ending at the advertised tip", func(t *testing.T) {
		r := newDivergenceRig(t, aLen, func(fB *fakeServer) { fB.Fork(4, 17) }, nil)
		r.fB.mtx.Lock()
		stale := mineTestHeader(t, r.fB.chain[20].BlockHash(), easyBits, 4242)
		r.fB.mtx.Unlock()
		var buf bytes.Buffer
		if err := stale.Serialize(&buf); err != nil {
			t.Fatal(err)
		}
		r.obs.setTip(&HeadersTip{Height: 21, Hex: hex.EncodeToString(buf.Bytes())})
		if loss := r.n.handleDivergence(r.obs, 19); loss != nil {
			t.Fatal("loss from a batch that does not end at the advertised tip")
		}
		r.unpunished(t)
	})

	t.Run("contradicts a held checkpoint", func(t *testing.T) {
		r := newDivergenceRig(t, aLen, func(fB *fakeServer) { fB.Fork(4, 17) },
			func(fA *fakeServer) []netparams.Checkpoint {
				fA.mtx.Lock()
				defer fA.mtx.Unlock()
				genesis, cp := fA.chain[0].BlockHash(), fA.chain[10].BlockHash()
				return []netparams.Checkpoint{{Height: 0, Hash: &genesis}, {Height: 10, Hash: &cp}}
			})
		if loss := r.n.handleDivergence(r.obs, 19); loss != nil {
			t.Fatal("loss from an observer contradicting a checkpoint")
		}
		if !isQuarantined(r.fB.addr().Addr) {
			t.Fatal("observer contradicting a held checkpoint not quarantined")
		}
	})
}

// TestObserverTickBeforeCheckpointHeld: while the store does not hold the
// newest checkpoint, a heavier dissenting observer yields no work verdict.
// Once it does, the same observer yields a pending loss.
func TestObserverTickBeforeCheckpointHeld(t *testing.T) {
	for _, held := range []bool{false, true} {
		r := newDivergenceRig(t, 20, func(fB *fakeServer) { fB.Fork(4, 17) },
			func(fA *fakeServer) []netparams.Checkpoint {
				if held {
					return nil
				}
				// Genesis anchors the store at 0; the checkpoint at 30
				// lies above the stored tip and is not yet held.
				fA.mtx.Lock()
				genesis := fA.chain[0].BlockHash()
				fA.mtx.Unlock()
				return []netparams.Checkpoint{{Height: 0, Hash: &genesis}, {Height: 30, Hash: &chainhash.Hash{3}}}
			})
		r.n.obsMtx.Lock()
		r.n.observers[r.fB.addr().Addr] = r.obs
		r.n.obsMtx.Unlock()
		st := &observerLoopState{lagBlocks: 1000}
		r.n.observerTick(st)
		if got := st.pendingLoss != nil; got != held {
			t.Fatalf("checkpoint held %v: pending loss %v", held, got)
		}
		r.unpunished(t)
	}
}

// TestDeepForkObserverWins: a wallet server forked below the shallow audit
// window, against a heavier honest observer. The observer is not demoted, the
// loss persists across ticks, and the network rotates to the observer and
// swaps the store onto its chain through the election hint.
func TestDeepForkObserverWins(t *testing.T) {
	tightenObserverLoop(t)
	resetServerHealth()
	fA, params := newFakeServer(t, 20)
	fB, _ := newFakeServer(t, 20)
	fB.Fork(4, 17) // heights 5-21, two blocks of work more than A's 5-19
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fA.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { stopAndWait(net_) })
	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()

	fB.mtx.Lock()
	wantTip := fB.chain[21].BlockHash()
	fB.mtx.Unlock()
	waitFor(t, "rotation onto the deep fork", func() bool {
		return net_.currentWalletAddr() == fB.addr().Addr &&
			net_.Tip() == 21 && net_.TipHash() == wantTip
	})
	if demoted(fB.addr().Addr) || isQuarantined(fB.addr().Addr) {
		t.Fatal("winning observer penalized")
	}
	if !demoted(fA.addr().Addr) {
		t.Fatal("losing wallet server not demoted")
	}
}

// startPair starts a network electing fA as wallet server, with fB as the
// only other candidate, and waits for it to sync to tip on fA with fB
// observing.
func startPair(t *testing.T, fA, fB *fakeServer, params *netparams.ChainParams, tip int32) *Network {
	t.Helper()
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net_, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{fA.addr(), fB.addr()},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := net_.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { stopAndWait(net_) })
	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()
	waitFor(t, "sync on A", func() bool {
		return net_.Synced() && net_.Tip() == tip && net_.currentWalletAddr() == fA.addr().Addr
	})
	waitFor(t, "observer on B", func() bool { return net_.ObserverCount() == 1 })
	return net_
}

// TestLiveRuleErrorQuarantines: a tip header from the wallet server that
// fails its PoW hash quarantines the server and rotates away from it, and the
// quarantine keeps it out of the next election.
func TestLiveRuleErrorQuarantines(t *testing.T) {
	tightenObserverLoop(t)
	resetServerHealth()
	fA, params := newFakeServer(t, 15)
	fB, _ := newFakeServer(t, 15)
	net_ := startPair(t, fA, fB, params, 14)
	tipHash := net_.TipHash()

	fA.notifyHeader(15, unminedHeader(tipHash, time.Unix(1_600_000_000, 0)))
	waitFor(t, "rotation away from the rule breaker", func() bool {
		return net_.currentWalletAddr() == fB.addr().Addr
	})
	if !isQuarantined(fA.addr().Addr) {
		t.Fatal("rule breaker not quarantined")
	}
	if net_.Tip() != 14 || net_.TipHash() != tipHash {
		t.Fatal("store took the refused header")
	}

	// With B gone, the only other candidate is the quarantined A: the
	// network degrades rather than electing it.
	entries := degradedEntries.Load()
	fB.Close()
	waitFor(t, "degraded without B", func() bool { return degradedEntries.Load() > entries })
	time.Sleep(4 * degradedRetryInterval)
	if got := net_.currentWalletAddr(); got == fA.addr().Addr {
		t.Fatal("quarantined server re-elected")
	}
}

// TestLiveInsufficientWorkRotatesWithHint: a wallet server whose reorg branch
// carries less work than the stored chain records a failure, not a
// quarantine, and the rotation lands on a server serving the store's tip.
func TestLiveInsufficientWorkRotatesWithHint(t *testing.T) {
	tightenObserverLoop(t)
	resetServerHealth()
	fA, params := newFakeServer(t, 15)
	fB, _ := newFakeServer(t, 15)
	net_ := startPair(t, fA, fB, params, 14)
	tipHash := net_.TipHash()

	fA.ForkWithBits(10, 7, lowWorkBits) // tip 17, less work than 11-14
	waitFor(t, "rotation away from the lighter branch", func() bool {
		return net_.currentWalletAddr() == fB.addr().Addr
	})
	if isQuarantined(fA.addr().Addr) || !demoted(fA.addr().Addr) {
		t.Fatal("lighter branch must demote, not quarantine")
	}
	if net_.Tip() != 14 || net_.TipHash() != tipHash {
		t.Fatal("store left its chain for the lighter branch")
	}
}

// TestLiveNoPenaltyErrors: a future-dated tip header and a server that agrees
// at the tip but serves a next header that does not link (errForkNotFound)
// cost the wallet server nothing and rotate nothing.
func TestLiveNoPenaltyErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakFn func(f *fakeServer, tip chainhash.Hash)
	}{
		{"timestamp too new", func(f *fakeServer, tip chainhash.Hash) {
			hdr := &wire.BlockHeader{Version: 4, PrevBlock: tip, Bits: easyBits,
				Timestamp: time.Now().Add(maxFutureBlockTime + time.Hour)}
			mineHeader(hdr)
			f.notifyHeader(15, hdr)
		}},
		{"fork not found", func(f *fakeServer, _ chainhash.Hash) {
			orphan := mineTestHeader(t, chainhash.Hash{0x0f}, easyBits, 123)
			f.mtx.Lock()
			f.chain = append(f.chain, orphan)
			f.mtx.Unlock()
			f.notifyHeader(15, orphan)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tightenObserverLoop(t)
			resetServerHealth()
			fA, params := newFakeServer(t, 15)
			fB, _ := newFakeServer(t, 15)
			net_ := startPair(t, fA, fB, params, 14)
			tc.breakFn(fA, net_.TipHash())
			waitFor(t, "notification applied", func() bool { return net_.TargetTip() == 15 })
			time.Sleep(300 * time.Millisecond)
			if got := net_.currentWalletAddr(); got != fA.addr().Addr {
				t.Fatalf("rotated to %s", got)
			}
			if demoted(fA.addr().Addr) || isQuarantined(fA.addr().Addr) {
				t.Fatal("wallet server penalized")
			}
		})
	}
}

// chainWithUnmined returns a fake chain of length n whose header at bad fails
// its PoW hash, with every later header relinked and mined.
func chainWithUnmined(t *testing.T, f *fakeServer, bad int) {
	t.Helper()
	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.chain[bad] = unminedHeader(f.chain[bad-1].BlockHash(), time.Unix(1_600_000_000, 0))
	for i := bad + 1; i < len(f.chain); i++ {
		f.chain[i] = mineTestHeader(t, f.chain[i-1].BlockHash(), easyBits, uint32(i)*7919)
	}
}

// TestInitialSyncRuleErrorQuarantines: a rule violation during Start's initial
// sync quarantines the server and stops the network, and the next network the
// wallet builds elects around it.
func TestInitialSyncRuleErrorQuarantines(t *testing.T) {
	resetServerHealth()
	fBad, params := newFakeServer(t, 15)
	fGood, _ := newFakeServer(t, 15)
	chainWithUnmined(t, fBad, 5)

	build := func(servers ...*ServerAddr) *Network {
		chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
		if err != nil {
			t.Fatalf("NewChainConfig: %v", err)
		}
		net_, err := NewNetwork(&Config{Chain: chainCfg, Servers: servers, DataDir: t.TempDir()})
		if err != nil {
			t.Fatalf("NewNetwork: %v", err)
		}
		return net_
	}
	first := build(fBad.addr())
	if err := first.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-first.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("network did not stop after the failed sync")
	}
	if !isQuarantined(fBad.addr().Addr) {
		t.Fatal("server breaking a rule during initial sync not quarantined")
	}

	second := build(fBad.addr(), fGood.addr())
	if err := second.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopAndWait(second)
	if got := second.currentWalletAddr(); got != fGood.addr().Addr {
		t.Fatalf("elected %s, want the unquarantined server", got)
	}
	waitFor(t, "sync on the good server", func() bool { return second.Synced() && second.Tip() == 14 })
}

// TestAttachRuleErrorQuarantines: a successor whose catch-up breaks a header
// rule during attach is quarantined.
func TestAttachRuleErrorQuarantines(t *testing.T) {
	tightenObserverLoop(t)
	resetServerHealth()
	fA, params := newFakeServer(t, 15)
	fB, _ := newFakeServer(t, 16) // A's chain plus one block
	chainWithUnmined(t, fB, 15)   // ... whose header at 15 fails its hash
	net_ := startPair(t, fA, fB, params, 14)

	fA.Close()
	waitFor(t, "successor refused", func() bool { return isQuarantined(fB.addr().Addr) })
	if got := net_.currentWalletAddr(); got == fB.addr().Addr {
		t.Fatal("rule-breaking successor attached")
	}
}

// TestInitialSyncCheckpointMismatchDiscards: a store synced from a server on
// another branch is reopened under a checkpoint above its tip that the server
// contradicts. The sync discards back to the highest checkpoint the store
// holds, publishes the discard as one TipChange, and quarantines the server.
func TestInitialSyncCheckpointMismatchDiscards(t *testing.T) {
	resetServerHealth()
	honest, params := newFakeServer(t, 20)
	fBad, _ := newFakeServer(t, 20)
	fBad.mtx.Lock()
	fBad.chain = fBad.chain[:13]
	fBad.mtx.Unlock()
	fBad.Fork(6, 6) // tip 12 on its own branch from 7
	dataDir := t.TempDir()

	run := func(p *netparams.ChainParams) *Network {
		chainCfg, err := NewChainConfig(bisonwire.ChainBTC, p)
		if err != nil {
			t.Fatalf("NewChainConfig: %v", err)
		}
		net_, err := NewNetwork(&Config{Chain: chainCfg, Servers: []*ServerAddr{fBad.addr()}, DataDir: dataDir})
		if err != nil {
			t.Fatalf("NewNetwork: %v", err)
		}
		return net_
	}
	first := run(params)
	if err := first.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "sync on the bad branch", func() bool { return first.Synced() && first.Tip() == 12 })
	stopAndWait(first)

	fBad.MineBlocks(8) // reaches past the new checkpoint at 15
	honest.mtx.Lock()
	genesis, cp15 := honest.chain[0].BlockHash(), honest.chain[15].BlockHash()
	honest.mtx.Unlock()
	upgraded := &netparams.ChainParams{
		Chain:              params.Chain,
		Net:                params.Net,
		GenesisBlock:       params.GenesisBlock,
		GenesisHash:        params.GenesisHash,
		PoWNoRetargeting:   true,
		TargetTimePerBlock: params.TargetTimePerBlock,
		Checkpoints:        []netparams.Checkpoint{{Height: 0, Hash: &genesis}, {Height: 15, Hash: &cp15}},
	}
	second := run(upgraded)
	events := collectTipEvents(second)
	if err := second.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-second.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("network did not stop after the checkpoint mismatch")
	}
	if !isQuarantined(fBad.addr().Addr) {
		t.Fatal("server contradicting a checkpoint not quarantined")
	}
	if second.Tip() != 0 {
		t.Fatalf("store tip %d after the discard, want the genesis checkpoint", second.Tip())
	}
	var discards int
	for _, tc := range events.snapshot() {
		if len(tc.Disconnected) > 0 {
			discards++
			// The batch crossing the checkpoint is refused whole, so the
			// store still ends at 12 and discards 12 down to 1.
			if lowest, _ := tc.LowestDisconnected(); len(tc.Disconnected) != 12 || lowest != 1 {
				t.Fatalf("discard of %d blocks down to %d, want 12 down to 1", len(tc.Disconnected), lowest)
			}
		}
	}
	if discards != 1 {
		t.Fatalf("%d discard events, want one", discards)
	}
}

// TestObserverAuditPenalties: an observer whose own batch breaks inside is
// quarantined; one whose header fetch fails is demoted, never quarantined; and
// nothing is recorded while the network shuts down.
func TestObserverAuditPenalties(t *testing.T) {
	t.Run("break inside a response", func(t *testing.T) {
		r := newDivergenceRig(t, 20, func(fB *fakeServer) {
			fB.Fork(4, 17)
			fB.mtx.Lock()
			bad := *fB.chain[15]
			bad.PrevBlock = chainhash.Hash{0xaa}
			fB.chain[15] = mineTestHeader(t, bad.PrevBlock, easyBits, 31) // 16 no longer links
			fB.mtx.Unlock()
		}, nil)
		r.n.handleDivergence(r.obs, 19)
		if !isQuarantined(r.fB.addr().Addr) {
			t.Fatal("observer serving a self-contradicting batch not quarantined")
		}
	})
	t.Run("fetch failure", func(t *testing.T) {
		r := newDivergenceRig(t, 20, func(fB *fakeServer) { fB.Fork(4, 17) }, nil)
		r.fB.FailMethod("blockchain.block.headers", "unavailable")
		r.n.handleDivergence(r.obs, 19)
		if isQuarantined(r.fB.addr().Addr) || !demoted(r.fB.addr().Addr) {
			t.Fatal("fetch failure must demote, not quarantine")
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		r := newDivergenceRig(t, 20, func(fB *fakeServer) { fB.ForkWithBits(4, 16, lowWorkBits) }, nil)
		r.n.cancel()
		r.n.handleDivergence(r.obs, 19)
		if demoted(r.fB.addr().Addr) || isQuarantined(r.fB.addr().Addr) {
			t.Fatal("observer penalized during shutdown")
		}
	})
}

// TestShutdownRecordsNoFailures: once the network is shutting down, an
// election, an observer fill and a lag rotation record no failure against
// any server. Stop can land mid-dial, and a connection cut short by it says
// nothing about the server.
func TestShutdownRecordsNoFailures(t *testing.T) {
	resetServerHealth()
	f, params := newFakeServer(t, 5)
	other, _ := newFakeServer(t, 5)
	n := newUnstartedNetwork(t, f, params)
	n.cfg.Servers = []*ServerAddr{f.addr(), other.addr()}
	walletNode, err := NewNode(n.ctx, f.addr(), "", params.GenesisHash.String())
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	t.Cleanup(walletNode.Close)
	n.mu.Lock()
	n.node = walletNode
	n.mu.Unlock()

	n.cancel()
	if n.tryElect("", nil) {
		t.Fatal("election succeeded after shutdown")
	}
	n.fillObservers(f.addr().Addr)
	n.lagRotate(10, time.Minute)
	n.divergenceRotate(&divergenceLoss{walletAddr: f.addr().Addr,
		hint: &electionHint{wantHeight: 4}})
	n.wg.Wait()
	for _, addr := range []string{f.addr().Addr, other.addr().Addr} {
		if demoted(addr) || isQuarantined(addr) {
			t.Fatalf("%s penalized during shutdown", addr)
		}
	}
}
