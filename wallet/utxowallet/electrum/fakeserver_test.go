// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
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
	broadcasts []string                  // raw hexes received
	conns      map[net.Conn]*fakeConnState
	closed     bool
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
		f.mtx.Unlock()
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
		return map[string]interface{}{
			"height": tip, "hex": hex.EncodeToString(buf.Bytes()),
		}, ""
	case "blockchain.block.headers":
		start, count := num(0), num(1)
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
	defer net_.Stop()

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
	defer net_.Stop()
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
	defer net_.Stop()
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

// TestDivergenceReorgPromotion covers rotation plan R5: an observer serving
// an equal-work conflicting chain passes the dissent audit; the wallet
// server is quarantined, broadcasts suspend, the dissenter is promoted, the
// header store reorgs onto its chain, and the suspension lifts.
func TestDivergenceReorgPromotion(t *testing.T) {
	oldInterval := observerCompareInterval
	observerCompareInterval = 50 * time.Millisecond
	t.Cleanup(func() { observerCompareInterval = oldInterval })

	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.quarantined = make(map[string]bool)
	serverHealth.mtx.Unlock()

	fA, params := newFakeServer(t, 15)
	fB, _ := newFakeServer(t, 15)
	// B forks three blocks below the tip and mines back to the SAME height:
	// equal cumulative work, which the audit treats as credible dissent.
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
	defer net_.Stop()
	waitFor(t, "sync on A's chain", func() bool { return net_.Synced() && net_.Tip() == 14 })

	// Drain tip notifications so the reorg delivery below can't block on
	// the unbuffered-ish channel.
	go func() {
		for range net_.TipChangeChan() { //nolint:revive
		}
	}()

	fBTip := fB.Tip()
	fB.mtx.Lock()
	wantTip := fB.chain[fBTip].BlockHash()
	fB.mtx.Unlock()

	// The observer connects to B, sees the conflicting tip at a height we
	// have, and the divergence machinery takes it from there. The compare
	// loop ticks every observerCompareInterval (30s) — nudge it by forcing
	// an immediate pass through fill+compare via a fresh notification.
	waitFor(t, "rotation to the dissenter", func() bool {
		return net_.currentWalletAddr() == fB.addr().Addr
	})
	waitFor(t, "reorg onto B's chain", func() bool {
		return net_.TipHash() == wantTip
	})
	if !serverHealth.quarantined[fA.addr().Addr] {
		t.Fatal("losing wallet server not quarantined")
	}
	waitFor(t, "broadcast suspension lifted", func() bool {
		return !net_.spendSuspect.Load()
	})
	select {
	case <-net_.Done():
		t.Fatal("network shut down during reorg promotion")
	default:
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
	defer net_.Stop()
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
