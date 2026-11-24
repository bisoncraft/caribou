// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
	"google.golang.org/protobuf/proto"
)

// Calls are at least a second apart during a cycle, and a change still
// waiting at the end of the cycle is reported then.
func TestOnChangeIsCoalesced(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	calls := 0
	p := &processor{opts: syncOptions{
		onChange: func() { calls++ },
		now:      func() time.Time { return now },
	}}
	p.noteChange()
	if calls != 1 {
		t.Fatalf("%d calls after the first change", calls)
	}
	for range 10 {
		now = now.Add(50 * time.Millisecond)
		p.noteChange()
	}
	if calls != 1 {
		t.Fatalf("%d calls within a second", calls)
	}
	now = now.Add(500 * time.Millisecond)
	p.noteChange()
	if calls != 2 {
		t.Fatalf("%d calls after a second", calls)
	}
	now = now.Add(100 * time.Millisecond)
	p.noteChange()
	p.flushChange()
	if calls != 3 {
		t.Fatalf("%d calls after the end of the cycle", calls)
	}
	// Nothing waits: nothing is reported.
	p.flushChange()
	if calls != 3 {
		t.Fatalf("%d calls after an empty end of cycle", calls)
	}
	// Without a callback nothing happens.
	(&processor{opts: syncOptions{now: time.Now}}).noteChange()
}

// The sync loop reports scanned blocks, a rewind and recorded outputs, and
// nothing while it has nothing to write.
func TestOnChangeReportsWrites(t *testing.T) {
	var calls atomic.Int32
	e := newEnv(t, fakeKind, 150, func(cfg *Config) { cfg.OnChange = func() { calls.Add(1) } })
	account := e.createAccount(100)
	e.connect()
	e.waitSynced(150)
	eventually(t, "a call for the scanned blocks", func() bool { return calls.Load() >= 1 })

	// Cycles with nothing to write report nothing.
	idle := calls.Load()
	time.Sleep(200 * time.Millisecond)
	if n := calls.Load(); n != idle {
		t.Errorf("%d calls while idle", n-idle)
	}

	// A recorded output.
	before := calls.Load()
	plantFunded(t, e, fakeKind, fakeAddress(account.ID, false, 0), 70_000, 120, 7)
	e.srv.mine(1)
	eventually(t, "a call for the output", func() bool { return calls.Load() > before })

	// A rewind.
	before = calls.Load()
	be := e.be.(*fakeBackend)
	rewinds := len(be.rewinds)
	e.srv.reorg(3, 4)
	eventually(t, "the rewind", func() bool {
		var n int
		be.set(func(b *fakeBackend) { n = len(b.rewinds) })
		return n > rewinds
	})
	eventually(t, "a call for the rewind", func() bool { return calls.Load() > before })
}

// testAccount adds an account born at 100 to a fake backend that knows the
// chain tip 150.
func testAccount(t *testing.T, be *fakeBackend, seed byte) *Account {
	t.Helper()
	state, err := proto.Marshal(&walletrpc.TreeState{Height: 99})
	if err != nil {
		t.Fatal(err)
	}
	account, err := be.createAccount("test", bytes.Repeat([]byte{seed}, 32), state, nil, "")
	if err != nil {
		t.Fatalf("createAccount: %v", err)
	}
	if err := be.updateChainTip(150); err != nil {
		t.Fatalf("updateChainTip: %v", err)
	}
	return account
}

// Every store reports a change: a transaction of the address walk, one
// answering an address request, also when closing the request fails
// afterwards, and a funding transaction. An output recorded again by the
// same processor reports none.
func TestOnChangeAtEveryStore(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := newFakeBackend()
	account := testAccount(t, be, 1)
	now := time.Unix(1_700_000_000, 0)
	p := newTestProcessor(t, srv, be, &now)
	calls := 0
	p.opts.onChange = func() { calls++ }
	ctx := context.Background()
	paying := func(address string, salt byte) []byte {
		return testTx(
			[]testOutPoint{{txid: bytes.Repeat([]byte{salt}, 32)}},
			[]testOutput{{script: p2pkhScript(address), value: 1_000}},
		)
	}
	// expect checks that the changes so far made n calls, once the one
	// waiting, if any, is reported.
	expect := func(what string, n int) {
		t.Helper()
		p.flushChange()
		if calls != n {
			t.Fatalf("%s: %d calls, want %d", what, calls, n)
		}
	}

	walked := fakeAddress(account.ID, false, 3)
	srv.set(func(s *fakeLwd) {
		s.addrTxs[walked] = []*walletrpc.RawTransaction{{Data: paying(walked, 0xa1), Height: 120}}
	})
	if _, failed, err := p.walkAddresses(ctx, 150); failed != nil || err != nil {
		t.Fatalf("walkAddresses: %v, %v", failed, err)
	}
	expect("the walk", 1)

	requested := fakeAddress(account.ID, false, 20)
	srv.set(func(s *fakeLwd) {
		s.addrTxs[requested] = []*walletrpc.RawTransaction{{Data: paying(requested, 0xa2), Height: 121}}
	})
	be.set(func(b *fakeBackend) {
		b.requests = []txRequest{{kind: txRequestAddress, address: requested, start: 100}}
		b.failOnce["notifyAddressChecked"] = errors.New("boom")
	})
	if err := p.enhance(ctx, 150); err != nil {
		t.Fatalf("enhance: %v", err)
	}
	expect("an address request that failed after its store", 2)

	// A funding transaction, whose output this processor recorded before,
	// so that only the store reports a change.
	funded := fakeAddress(account.ID, false, 0)
	raw := paying(funded, 0xa3)
	id := sha256.Sum256(raw)
	srv.set(func(s *fakeLwd) {
		s.txs[string(id[:])] = &walletrpc.RawTransaction{Data: raw, Height: 122}
		s.utxos[funded] = []*walletrpc.GetAddressUtxosReply{{
			Address: funded, Txid: id[:], Script: p2pkhScript(funded), ValueZat: 1_000, Height: 122,
		}}
	})
	be.set(func(b *fakeBackend) { b.requests = nil })
	p.recordedUTXOs = map[string]struct{}{fmt.Sprintf("%x:%d:%d", id[:], 0, 122): {}}
	if failed, err := p.refreshUTXOs(ctx, 150, srv.hashAt(150)); failed != nil || err != nil {
		t.Fatalf("refreshUTXOs: %v, %v", failed, err)
	}
	expect("a funding transaction", 3)

	// A new output of a transaction the wallet holds: only the output
	// reports a change.
	other := fakeAddress(account.ID, false, 1)
	raw2 := paying(other, 0xa4)
	id2 := sha256.Sum256(raw2)
	if _, err := be.decryptAndStoreTransaction(raw2, nil, nil); err != nil {
		t.Fatal(err)
	}
	srv.set(func(s *fakeLwd) {
		s.utxos[other] = []*walletrpc.GetAddressUtxosReply{{
			Address: other, Txid: id2[:], Script: p2pkhScript(other), ValueZat: 1_000, Height: 123,
		}}
	})
	gets := srv.callCount("GetTransaction")
	if failed, err := p.refreshUTXOs(ctx, 151, srv.hashAt(150)); failed != nil || err != nil {
		t.Fatalf("refreshUTXOs: %v, %v", failed, err)
	}
	if n := srv.callCount("GetTransaction") - gets; n != 0 {
		t.Fatalf("a held transaction was fetched %d times", n)
	}
	expect("an output", 4)

	// The same outputs again, at another tip, write nothing new.
	if failed, err := p.refreshUTXOs(ctx, 152, srv.hashAt(150)); failed != nil || err != nil {
		t.Fatalf("refreshUTXOs: %v, %v", failed, err)
	}
	p.flushChange()
	if calls != 4 {
		t.Fatalf("outputs recorded again made %d calls", calls-4)
	}
}

// OnRewind is called once for each rewind, with the height the wallet
// rewound to.
func TestOnRewind(t *testing.T) {
	var mtx sync.Mutex
	var heights []uint32
	e := newEnv(t, fakeKind, 150, func(cfg *Config) {
		cfg.OnRewind = func(h uint32) {
			mtx.Lock()
			heights = append(heights, h)
			mtx.Unlock()
		}
	})
	e.createAccount(100)
	e.connect()
	e.waitSynced(150)
	mtx.Lock()
	if len(heights) != 0 {
		t.Errorf("OnRewind called %d times without a reorganization", len(heights))
	}
	mtx.Unlock()

	be := e.be.(*fakeBackend)
	e.srv.reorg(3, 4)
	e.waitSynced(e.srv.height())
	var rewinds []uint32
	be.set(func(b *fakeBackend) { rewinds = append(rewinds, b.rewinds...) })
	mtx.Lock()
	defer mtx.Unlock()
	if len(rewinds) == 0 || len(heights) != len(rewinds) {
		t.Fatalf("%d rewinds, %d calls", len(rewinds), len(heights))
	}
	for i := range rewinds {
		if heights[i] != rewinds[i] {
			t.Errorf("rewind %d to %d reported as %d", i, rewinds[i], heights[i])
		}
	}
}
