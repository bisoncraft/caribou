// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

// fakeKind is the fake backend, for tests that inspect or steer it.
var fakeKind = backendKinds[0]

// newFakeEnv is newEnv with the fake backend, which it also returns.
func newFakeEnv(t *testing.T, chainHeight uint32, adjust func(*Config)) (*env, *fakeBackend) {
	t.Helper()
	e := newEnv(t, fakeKind, chainHeight, adjust)
	return e, e.be.(*fakeBackend)
}

func TestBackendErrorsAreRetried(t *testing.T) {
	// Each of these is called in every sync cycle of a wallet with one
	// account and one transparent output.
	methods := []string{
		"summary", "putSubtreeRoots", "maxScannedHeight", "updateChainTip", "accounts",
		"transparentReceivers", "utxoQueryHeight", "putUTXO", "suggestScanRanges", "scanBlocks",
		"transactionDataRequests",
	}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			e, be := newFakeEnv(t, 150, nil)
			account := e.createAccount(100)
			address := fakeAddress(account.ID, false, 0)
			e.srv.set(func(s *fakeLwd) {
				s.utxos[address] = []*walletrpc.GetAddressUtxosReply{{
					Address: address, Txid: make([]byte, 32), ValueZat: 5, Height: 110,
				}}
			})
			boom := wrapf(ErrDatabase, "disk full")
			be.set(func(b *fakeBackend) { b.fail[method] = boom })

			e.connect()
			failing := e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
			if !errors.Is(failing.Err, ErrDatabase) {
				t.Fatalf("error: %v", failing.Err)
			}
			// A database error is not a connection problem.
			if failing.State == SyncDisconnected {
				t.Errorf("state after a database error: %v", failing.State)
			}
			calls := be.callCount(method)
			eventually(t, "retries", func() bool { return be.callCount(method) >= calls+2 })

			be.set(func(b *fakeBackend) { delete(b.fail, method) })
			synced := e.waitSynced(150)
			if synced.Err != nil {
				t.Fatalf("error after recovering: %v", synced.Err)
			}
		})
	}
}

func TestBackendErrorDuringTipCheck(t *testing.T) {
	e, be := newFakeEnv(t, 150, nil)
	e.createAccount(100)
	e.connect()
	e.waitSynced(150)

	// The stored block hash is read once the wallet is at the tip.
	e.statuses.reset()
	be.set(func(b *fakeBackend) { b.fail["blockHash"] = wrapf(ErrDatabase, "disk full") })
	failing := e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
	if !errors.Is(failing.Err, ErrDatabase) {
		t.Fatalf("error: %v", failing.Err)
	}
	e.statuses.reset()
	be.set(func(b *fakeBackend) { delete(b.fail, "blockHash") })
	e.statuses.wait(t, "recovery", func(s SyncStatus) bool { return s.State == SyncSynced && s.Err == nil })
}

func TestScanWithoutProgressIsAnError(t *testing.T) {
	e, be := newFakeEnv(t, 150, nil)
	e.createAccount(100)
	be.set(func(b *fakeBackend) { b.stuck = true })
	e.connect()
	failing := e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
	if !errors.Is(failing.Err, ErrInternal) {
		t.Fatalf("error: %v", failing.Err)
	}
	be.set(func(b *fakeBackend) { b.stuck = false })
	e.waitSynced(150)
}

func TestRangesAboveTheServerTipWait(t *testing.T) {
	e, be := newFakeEnv(t, 150, nil)
	e.createAccount(100)
	// The wallet was told of a higher tip before, by a server that was
	// further along.
	if err := be.updateChainTip(400); err != nil {
		t.Fatalf("updateChainTip: %v", err)
	}
	e.connect()
	synced := e.statuses.wait(t, "sync to the server's tip", func(s SyncStatus) bool {
		return s.State == SyncSynced && s.ChainTip == 150
	})
	if synced.Err != nil || synced.FullyScanned != 150 {
		t.Fatalf("status: %+v", synced)
	}
	e.assertFollows()

	// The blocks are scanned once the server has them.
	e.srv.mine(20)
	e.statuses.wait(t, "sync to the new tip", func(s SyncStatus) bool {
		return s.State == SyncSynced && s.ChainTip == 170 && s.FullyScanned == 170
	})
}

func TestBatchEnd(t *testing.T) {
	p := &processor{net: testNetwork, opts: syncOptions{batchSize: 1000}}
	tests := []struct {
		name       string
		start, end uint32 // The range, end exclusive.
		tip        uint32
		batch      uint32
		want       uint32
	}{
		{"whole range far below the tip", 100, 150, 5000, 1000, 149},
		{"batch size", 100, 5000, 9000, 1000, 1099},
		{"range above the tip is cut at the tip", 100, 500, 110, 1000, 110},
		{"range that ends at the tip", 100, 151, 150, 1000, 150},
		{"single block", 150, 151, 150, 1000, 150},
		{"batch size of one", 100, 150, 150, 1, 100},
		{"range one block longer than the batch size", 100, 1101, 1400, 1000, 1099},
		{"range as long as the batch size", 100, 1100, 1400, 1000, 1099},
	}
	for _, tt := range tests {
		p.opts.batchSize = tt.batch
		got := p.batchEnd(scanRange{start: tt.start, end: tt.end}, tt.tip)
		if got != tt.want {
			t.Errorf("%s: batch end %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestRewindFallsBackToTheSafeHeight(t *testing.T) {
	e, be := newFakeEnv(t, 150, nil)
	e.createAccount(100)
	e.connect()
	e.waitSynced(150)

	// The wallet cannot rewind below 145, which is above where the first
	// rewind aims and below the blocks that were replaced.
	be.set(func(b *fakeBackend) { b.minRewind = 145 })
	e.srv.reorg(2, 1)
	e.waitSynced(151)
	e.assertFollows()
	var rewinds []uint32
	be.set(func(b *fakeBackend) { rewinds = append(rewinds, b.rewinds...) })
	if len(rewinds) < 2 || rewinds[len(rewinds)-1] != 145 {
		t.Errorf("rewind requests: %v, want the last one at the safe height 145", rewinds)
	}
}

func TestReorgTooDeep(t *testing.T) {
	for _, noSafe := range []bool{false, true} {
		t.Run(fmt.Sprintf("noSafe=%t", noSafe), func(t *testing.T) {
			e, be := newFakeEnv(t, 150, nil)
			e.createAccount(100)
			e.connect()
			e.waitSynced(150)

			// The wallet cannot rewind below 149, and the chain changed
			// from 146 on.
			be.set(func(b *fakeBackend) {
				b.minRewind = 149
				b.noSafeRewind = noSafe
			})
			e.srv.reorg(5, 1)
			failing := e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
			if !errors.Is(failing.Err, ErrReorgTooDeep) {
				t.Fatalf("error: %v", failing.Err)
			}
		})
	}
}

func TestRewindErrors(t *testing.T) {
	e, be := newFakeEnv(t, 150, nil)
	e.createAccount(100)
	e.connect()
	e.waitSynced(150)

	boom := wrapf(ErrDatabase, "disk full")
	be.set(func(b *fakeBackend) { b.fail["rewindToHeight"] = boom })
	e.srv.reorg(2, 1)
	failing := e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
	if !errors.Is(failing.Err, ErrDatabase) {
		t.Fatalf("error: %v", failing.Err)
	}

	// The tip is told again after a rewind. That can fail too.
	be.set(func(b *fakeBackend) {
		delete(b.fail, "rewindToHeight")
		b.failOnce["updateChainTip"] = boom
	})
	e.waitSynced(151)
	e.assertFollows()
}

func TestRepeatedChainErrorsRewindFurther(t *testing.T) {
	e, be := newFakeEnv(t, 400, nil)
	e.createAccount(100)
	e.connect()
	e.waitSynced(400)

	// Every scan reports a discontinuity, so each rewind fails to help and
	// the next one reaches further back, up to the limit.
	e.srv.mine(1)
	be.set(func(b *fakeBackend) {
		b.rewinds = nil
		b.fail["scanBlocks"] = &chainContinuityError{height: 401, reason: "test"}
	})
	eventually(t, "several rewinds", func() bool {
		n := 0
		be.set(func(b *fakeBackend) { n = len(b.rewinds) })
		return n >= 12
	})
	var rewinds []uint32
	be.set(func(b *fakeBackend) {
		rewinds = append(rewinds, b.rewinds...)
		delete(b.fail, "scanBlocks")
	})
	for i, target := range rewinds[:12] {
		distance := uint32(min(rewindDistance*(i+1), maxRewindDistance))
		if want := 400 - distance; target != want {
			t.Errorf("rewind %d aimed at %d, want %d", i+1, target, want)
		}
	}

	e.waitSynced(401)
	e.assertFollows()
}

func TestEnhancement(t *testing.T) {
	e, be := newFakeEnv(t, 150, nil)
	e.createAccount(100)
	now := time.Now()

	txid := func(n byte) []byte { return bytes.Repeat([]byte{n}, 32) }
	raw := func(n byte, height uint64) *walletrpc.RawTransaction {
		return &walletrpc.RawTransaction{Data: []byte{n}, Height: height}
	}
	e.srv.set(func(s *fakeLwd) {
		s.txs[string(txid(1))] = raw(1, 120)            // mined
		s.txs[string(txid(2))] = raw(2, 0)              // in the mempool
		s.txs[string(txid(3))] = raw(3, math.MaxUint64) // mined on a fork
		s.txs[string(txid(5))] = raw(5, 130)
		s.txs[string(txid(6))] = raw(6, 0)
		s.addrTxs["taddr-bounded"] = []*walletrpc.RawTransaction{raw(10, 105), raw(11, 125), raw(12, 149)}
		s.addrTxs["taddr-open"] = []*walletrpc.RawTransaction{raw(20, 150)}
	})
	be.set(func(b *fakeBackend) {
		b.requests = []txRequest{
			{kind: txRequestStatus, txid: txid(1)},
			{kind: txRequestStatus, txid: txid(2)},
			{kind: txRequestStatus, txid: txid(3)},
			{kind: txRequestStatus, txid: txid(4)}, // unknown to the server
			{kind: txRequestEnhancement, txid: txid(5)},
			{kind: txRequestEnhancement, txid: txid(6)},
			{kind: txRequestEnhancement, txid: txid(7)}, // unknown to the server
			{kind: txRequestAddress, address: "taddr-bounded", start: 110, end: 141, bounded: true},
			{kind: txRequestAddress, address: "taddr-open", start: 100},
			{kind: txRequestAddress, address: "taddr-later", start: 100, requestAt: now.Add(time.Hour)},
			{kind: txRequestAddress, address: "taddr-due", start: 100, requestAt: now.Add(-time.Hour)},
			{kind: txRequestAddress, address: "taddr-future", start: 151, end: 160, bounded: true},
			{kind: txRequestAddress, address: "taddr-empty", start: 0, end: 0, bounded: true},
		}
	})

	e.connect()
	e.waitSynced(150)

	be.set(func(b *fakeBackend) {
		wantStatus := map[byte]txStatus{
			1: {kind: txMined, height: 120},
			2: {kind: txNotInMainChain},
			3: {kind: txNotInMainChain},
			4: {kind: txNotRecognized},
			7: {kind: txNotRecognized},
		}
		if len(b.statuses) != len(wantStatus) {
			t.Errorf("%d statuses were set, want %d: %+v", len(b.statuses), len(wantStatus), b.statuses)
		}
		for n, want := range wantStatus {
			if got, ok := b.statuses[string(txid(n))]; !ok || got != want {
				t.Errorf("status of transaction %d: %+v (%t), want %+v", n, got, ok, want)
			}
		}

		// Full transactions: the two enhancements, the two in the bounded
		// range and the one of the open-ended address.
		stored := make(map[byte]*uint32)
		for _, tx := range b.stored {
			stored[tx.data[0]] = tx.height
		}
		wantStored := map[byte]uint32{5: 130, 6: 0, 11: 125, 20: 150}
		if len(stored) != len(wantStored) {
			t.Errorf("stored transactions: %v", stored)
		}
		for n, height := range wantStored {
			got, ok := stored[n]
			switch {
			case !ok:
				t.Errorf("transaction %d was not stored", n)
			case height == 0 && got != nil:
				t.Errorf("transaction %d was stored as mined at %d", n, *got)
			case height != 0 && (got == nil || *got != height):
				t.Errorf("transaction %d was stored with height %v, want %d", n, got, height)
			}
		}

		// Requests are closed at the last height they covered.
		asOf := make(map[string]uint32)
		for _, n := range b.notified {
			asOf[n.req.address] = n.asOf
		}
		wantAsOf := map[string]uint32{"taddr-bounded": 140, "taddr-open": 150, "taddr-due": 150}
		if len(asOf) != len(wantAsOf) {
			t.Errorf("closed requests: %v", asOf)
		}
		for address, want := range wantAsOf {
			if got, ok := asOf[address]; !ok || got != want {
				t.Errorf("%s was closed at %d (%t), want %d", address, got, ok, want)
			}
		}
		// Only the standing checks are rescheduled.
		if len(b.scheduled) != 2 || b.scheduled["taddr-open"] != defaultAddressCheckInterval ||
			b.scheduled["taddr-due"] != defaultAddressCheckInterval {
			t.Errorf("scheduled checks: %v", b.scheduled)
		}
	})

	// The server was asked about the bounded range as given.
	e.srv.set(func(s *fakeLwd) {
		for _, q := range s.addrQueries {
			if q.Address == "taddr-bounded" && (q.Range.Start.Height != 110 || q.Range.End.Height != 140) {
				t.Errorf("bounded query range: %d to %d", q.Range.Start.Height, q.Range.End.Height)
			}
			if q.Address == "taddr-later" || q.Address == "taddr-future" || q.Address == "taddr-empty" {
				t.Errorf("%s was queried", q.Address)
			}
		}
	})
}

func TestEnhancementAnswersEachRequestOncePerCycle(t *testing.T) {
	e, be := newFakeEnv(t, 150, nil)
	e.createAccount(100)
	id := bytes.Repeat([]byte{9}, 32)
	// The fake backend keeps returning the request, as the wallet does for
	// one it still considers open.
	be.set(func(b *fakeBackend) { b.requests = []txRequest{{kind: txRequestStatus, txid: id}} })
	e.connect()
	e.waitSynced(150)

	// One answer per sync cycle, however often the wallet repeats it.
	polls := e.srv.callCount("GetLatestBlock")
	answers := e.srv.callCount("GetTransaction")
	if answers > polls {
		t.Fatalf("%d answers in %d cycles", answers, polls)
	}
	eventually(t, "more cycles", func() bool { return e.srv.callCount("GetLatestBlock") >= polls+3 })
	if got, cycles := e.srv.callCount("GetTransaction"), e.srv.callCount("GetLatestBlock"); got > cycles {
		t.Errorf("%d answers in %d cycles", got, cycles)
	}
}

func TestEnhancementErrors(t *testing.T) {
	id := bytes.Repeat([]byte{9}, 32)
	tests := []struct {
		name string
		req  txRequest
		// Either a server method or a backend method is made to fail.
		serverMethod, backendMethod string
	}{
		{"status fetch", txRequest{kind: txRequestStatus, txid: id}, "GetTransaction", ""},
		{"status store", txRequest{kind: txRequestStatus, txid: id}, "", "setTransactionStatus"},
		{"enhancement fetch", txRequest{kind: txRequestEnhancement, txid: id}, "GetTransaction", ""},
		{"enhancement store", txRequest{kind: txRequestEnhancement, txid: id}, "", "decryptAndStoreTransaction"},
		{"enhancement not found", txRequest{kind: txRequestEnhancement, txid: bytes.Repeat([]byte{8}, 32)}, "", "setTransactionStatus"},
		{"address fetch", txRequest{kind: txRequestAddress, address: "taddr", start: 100}, "GetTaddressTxids", ""},
		{"address store", txRequest{kind: txRequestAddress, address: "taddr", start: 100}, "", "decryptAndStoreTransaction"},
		{"address close", txRequest{kind: txRequestAddress, address: "taddr", start: 100}, "", "notifyAddressChecked"},
		{"address schedule", txRequest{kind: txRequestAddress, address: "taddr", start: 100}, "", "scheduleNextCheck"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, be := newFakeEnv(t, 150, nil)
			e.createAccount(100)
			e.srv.set(func(s *fakeLwd) {
				s.txs[string(id)] = &walletrpc.RawTransaction{Data: []byte{1}, Height: 120}
				s.addrTxs["taddr"] = []*walletrpc.RawTransaction{{Data: []byte{2}, Height: 120}}
				if tt.serverMethod != "" {
					s.fail[tt.serverMethod] = status.Error(codes.Unavailable, "down")
				}
			})
			be.set(func(b *fakeBackend) {
				b.requests = []txRequest{tt.req}
				if tt.backendMethod != "" {
					b.fail[tt.backendMethod] = wrapf(ErrDatabase, "disk full")
				}
			})

			e.connect()
			failing := e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
			if tt.serverMethod != "" {
				if !isServerError(failing.Err) {
					t.Fatalf("error: %v", failing.Err)
				}
			} else if !errors.Is(failing.Err, ErrDatabase) {
				t.Fatalf("error: %v", failing.Err)
			}
			// The blocks were scanned all the same.
			if failing.FullyScanned != 150 {
				t.Errorf("fully scanned height with a failing enhancement: %d", failing.FullyScanned)
			}

			e.statuses.reset()
			e.srv.set(func(s *fakeLwd) { delete(s.fail, tt.serverMethod) })
			be.set(func(b *fakeBackend) { delete(b.fail, tt.backendMethod) })
			e.statuses.wait(t, "recovery", func(s SyncStatus) bool { return s.State == SyncSynced && s.Err == nil })
		})
	}
}

func TestFailingRequestsDoNotFailTheCycle(t *testing.T) {
	bad := bytes.Repeat([]byte{9}, 32)
	good := bytes.Repeat([]byte{1}, 32)
	goodStatus := txRequest{kind: txRequestStatus, txid: good}
	goodAddress := txRequest{kind: txRequestAddress, address: "taddr-ok", start: 100}
	tests := []struct {
		name string
		// req fails, and healthy has to be answered all the same.
		req, healthy txRequest
		server       func(*fakeLwd)
		backend      func(*fakeBackend)
	}{{
		name: "the server rejects one transaction ID",
		req:  txRequest{kind: txRequestStatus, txid: bad}, healthy: goodStatus,
		server: func(s *fakeLwd) { s.txFail[string(bad)] = status.Error(codes.InvalidArgument, "bad txid") },
	}, {
		name: "the server fails internally on one transaction",
		req:  txRequest{kind: txRequestEnhancement, txid: bad}, healthy: goodStatus,
		server: func(s *fakeLwd) { s.txFail[string(bad)] = status.Error(codes.Internal, "boom") },
	}, {
		name: "the wallet cannot read a transaction",
		req:  txRequest{kind: txRequestEnhancement, txid: bad}, healthy: goodAddress,
		server: func(s *fakeLwd) {
			s.txs[string(bad)] = &walletrpc.RawTransaction{Data: []byte{7}, Height: 120}
		},
		backend: func(b *fakeBackend) {
			b.fail["decryptAndStoreTransaction"] = wrapf(ErrInvalidArgument, "not a transaction")
		},
	}, {
		name: "the server rejects address queries",
		req:  txRequest{kind: txRequestAddress, address: "taddr-bad", start: 100}, healthy: goodStatus,
		server: func(s *fakeLwd) {
			s.fail["GetTaddressTxids"] = status.Error(codes.InvalidArgument, "bad address")
		},
	}, {
		name: "a request of a kind the package does not know",
		req:  txRequest{kind: 99}, healthy: goodStatus,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, be := newFakeEnv(t, 150, nil)
			e.createAccount(100)
			e.srv.set(func(s *fakeLwd) {
				s.txs[string(good)] = &walletrpc.RawTransaction{Data: []byte{1}, Height: 120}
				if tt.server != nil {
					tt.server(s)
				}
			})
			be.set(func(b *fakeBackend) {
				// The failing request comes first.
				b.requests = []txRequest{tt.req, tt.healthy}
				if tt.backend != nil {
					tt.backend(b)
				}
			})

			e.connect()
			e.waitSynced(150)
			// A few more cycles, in which the failing request is retried.
			polls := e.srv.callCount("GetLatestBlock")
			eventually(t, "more cycles", func() bool { return e.srv.callCount("GetLatestBlock") >= polls+5 })
			for _, s := range e.statuses.all() {
				if s.Err != nil || s.State == SyncDisconnected {
					t.Fatalf("a failing request failed the cycle: %+v", s)
				}
			}
			be.set(func(b *fakeBackend) {
				if tt.healthy.kind == txRequestStatus {
					if got, ok := b.statuses[string(good)]; !ok || got.kind != txMined {
						t.Errorf("the healthy request was not answered: %+v (%t)", got, ok)
					}
				} else if len(b.notified) == 0 || b.notified[0].req.address != tt.healthy.address {
					t.Errorf("the healthy request was not answered: %+v", b.notified)
				}
				if _, ok := b.statuses[string(bad)]; ok {
					t.Error("the failing request was answered")
				}
			})
		})
	}
}

// newTestProcessor returns a processor over a fake backend and a connection
// to a fake server, for tests that drive single steps of a sync cycle. Its
// clock reads *now.
func newTestProcessor(t *testing.T, srv *fakeLwd, be *fakeBackend, now *time.Time) *processor {
	t.Helper()
	opts := syncOptionsFor(&Config{})
	opts.minBackoff = time.Minute
	opts.maxBackoff = 4 * time.Minute
	opts.now = func() time.Time { return *now }
	update := func(f func(*SyncStatus)) { f(new(SyncStatus)) }
	return newProcessor(newTestLogger(t), testNetwork, be, dialFake(t, srv), opts, update)
}

func TestDeferredRequests(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := newFakeBackend()
	now := time.Unix(1_700_000_000, 0)
	p := newTestProcessor(t, srv, be, &now)
	ctx := context.Background()

	id := bytes.Repeat([]byte{9}, 32)
	failing := txRequest{kind: txRequestStatus, txid: id}
	healthy := txRequest{kind: txRequestAddress, address: "taddr", start: 100}
	be.requests = []txRequest{failing, healthy}
	srv.set(func(s *fakeLwd) { s.txFail[string(id)] = status.Error(codes.InvalidArgument, "bad txid") })

	// step runs one enhancement after the clock has advanced, and checks
	// how often the failing request has been made by then and when it is
	// due again.
	step := func(advance time.Duration, wantCalls, wantFailures int, wantWait time.Duration) {
		t.Helper()
		now = now.Add(advance)
		if err := p.enhance(ctx, 150); err != nil {
			t.Fatalf("enhance: %v", err)
		}
		if got := srv.callCount("GetTransaction"); got != wantCalls {
			t.Fatalf("the failing request was made %d times, want %d", got, wantCalls)
		}
		d, ok := p.deferred[failing.key()]
		if wantFailures == 0 {
			if ok || len(p.deferred) != 0 {
				t.Fatalf("deferred requests: %+v", p.deferred)
			}
			return
		}
		if !ok || d.failures != wantFailures || !d.retryAt.Equal(now.Add(wantWait)) {
			t.Fatalf("deferred: %+v (%t), want %d failures and a retry in %s", d, ok, wantFailures, wantWait)
		}
	}
	step(0, 1, 1, time.Minute)
	// Not tried again before its time. The clock has moved, so the retry
	// time is compared with the one set before.
	now = now.Add(59 * time.Second)
	if err := p.enhance(ctx, 150); err != nil {
		t.Fatalf("enhance: %v", err)
	}
	if got := srv.callCount("GetTransaction"); got != 1 {
		t.Fatalf("the deferred request was made %d times before its time", got)
	}
	// The delay doubles up to the limit.
	step(time.Second, 2, 2, 2*time.Minute)
	step(2*time.Minute, 3, 3, 4*time.Minute)
	step(4*time.Minute, 4, 4, 4*time.Minute)
	// The healthy request was answered every time.
	if got := srv.callCount("GetTaddressTxids"); got != 5 {
		t.Errorf("the healthy request was made %d times in 5 cycles", got)
	}

	// Once the request succeeds it is no longer deferred.
	srv.set(func(s *fakeLwd) {
		delete(s.txFail, string(id))
		s.txs[string(id)] = &walletrpc.RawTransaction{Data: []byte{1}, Height: 120}
	})
	step(4*time.Minute, 5, 0, 0)
	if got := be.statuses[string(id)]; got.kind != txMined || got.height != 120 {
		t.Errorf("status after the request succeeded: %+v", got)
	}

	// A deferred request that the wallet stops making is forgotten.
	srv.set(func(s *fakeLwd) { s.txFail[string(id)] = status.Error(codes.Internal, "boom") })
	step(0, 6, 1, time.Minute)
	be.requests = []txRequest{healthy}
	step(0, 6, 0, 0)
}

func TestAddressRequestIsDueAfterItsScheduledSecond(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := newFakeBackend()
	scheduled := time.Unix(1_700_000_000, 0)
	now := scheduled.Add(-time.Second)
	p := newTestProcessor(t, srv, be, &now)
	be.requests = []txRequest{{kind: txRequestAddress, address: "taddr", start: 100, requestAt: scheduled}}

	steps := []struct {
		now   time.Time
		calls int
	}{
		{scheduled.Add(-time.Second), 0},
		// Within the scheduled second the wallet would not reschedule the
		// check, so it is not made yet.
		{scheduled, 0},
		{scheduled.Add(999 * time.Millisecond), 0},
		{scheduled.Add(time.Second), 1},
	}
	for _, step := range steps {
		now = step.now
		if err := p.enhance(context.Background(), 150); err != nil {
			t.Fatalf("enhance: %v", err)
		}
		if got := srv.callCount("GetTaddressTxids"); got != step.calls {
			t.Errorf("%s after the scheduled time: %d address queries, want %d",
				step.now.Sub(scheduled), got, step.calls)
		}
	}
}

func TestErrorsThatEndEnhancement(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := newFakeBackend()
	now := time.Now()
	p := newTestProcessor(t, srv, be, &now)
	ctx := context.Background()
	id := bytes.Repeat([]byte{9}, 32)
	be.requests = []txRequest{{kind: txRequestStatus, txid: id}}

	// A failure of the connection or of the server as a whole.
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.ResourceExhausted} {
		srv.set(func(s *fakeLwd) { s.fail["GetTransaction"] = status.Error(code, "no") })
		if err := p.enhance(ctx, 150); !isServerError(err) || status.Code(err) != code {
			t.Errorf("%s: %v", code, err)
		}
	}
	srv.set(func(s *fakeLwd) { delete(s.fail, "GetTransaction") })

	// A failure of the wallet database.
	be.fail["setTransactionStatus"] = wrapf(ErrDatabase, "disk full")
	if err := p.enhance(ctx, 150); !errors.Is(err, ErrDatabase) {
		t.Errorf("database error: %v", err)
	}
	delete(be.fail, "setTransactionStatus")
	be.fail["transactionDataRequests"] = wrapf(ErrDatabase, "disk full")
	if err := p.enhance(ctx, 150); !errors.Is(err, ErrDatabase) {
		t.Errorf("database error listing the requests: %v", err)
	}
	delete(be.fail, "transactionDataRequests")

	// A cancelled context, before a request and during one.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.enhance(cancelled, 150); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled before a request: %v", err)
	}
	gate := make(chan struct{})
	defer close(gate)
	entered := make(chan string, 1)
	srv.set(func(s *fakeLwd) {
		s.gate["GetTransaction"] = gate
		s.entered = entered
	})
	during, cancel := context.WithCancel(ctx)
	go func() {
		<-entered
		cancel()
	}()
	if err := p.enhance(during, 150); err == nil || during.Err() == nil {
		t.Errorf("cancelled during a request: %v", err)
	}

	if len(p.deferred) != 0 {
		t.Errorf("requests were deferred by errors that end the cycle: %+v", p.deferred)
	}
}

func TestTransparentOutputsAreQueriedWhenTheyCanHaveChanged(t *testing.T) {
	e, be := newFakeEnv(t, 150, nil)
	e.createAccount(100)
	e.connect()
	e.waitSynced(150)

	queries := func() int { return e.srv.callCount("GetAddressUtxosStream") }
	// idle lets a few sync cycles pass at an unchanged chain tip.
	idle := func() {
		t.Helper()
		polls := e.srv.callCount("GetLatestBlock")
		eventually(t, "more cycles", func() bool { return e.srv.callCount("GetLatestBlock") >= polls+4 })
	}
	idle()
	if got := queries(); got != 1 {
		t.Fatalf("%d queries while the tip did not move, want 1", got)
	}

	// A new block.
	e.srv.mine(1)
	e.waitSynced(151)
	idle()
	if got := queries(); got != 2 {
		t.Fatalf("%d queries after one new block, want 2", got)
	}

	// A second account at the same tip. Only its addresses are asked about.
	second, err := e.w.CreateAccount(context.Background(), &AccountParams{Seed: bytes.Repeat([]byte{3}, 32), Birthday: 100})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	eventually(t, "a query for the new account", func() bool { return queries() >= 3 })
	idle()
	if got := queries(); got != 3 {
		t.Fatalf("%d queries after adding an account, want 3", got)
	}
	e.srv.set(func(s *fakeLwd) {
		last := s.utxoQueries[len(s.utxoQueries)-1]
		if len(last.Addresses) != 15 || last.Addresses[0] != fakeAddress(second.ID, false, 0) {
			t.Errorf("the query after adding an account was about %v", last.Addresses)
		}
	})

	// After a failed cycle both accounts are asked about again, although
	// the tip is the same.
	e.srv.set(func(s *fakeLwd) { s.fail["GetLatestBlock"] = status.Error(codes.Unavailable, "down") })
	e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
	e.statuses.reset()
	e.srv.set(func(s *fakeLwd) { delete(s.fail, "GetLatestBlock") })
	e.waitSynced(151)
	idle()
	if got := queries(); got != 5 {
		t.Fatalf("%d queries after a failed cycle, want 5", got)
	}

	// A rewind forgets where outputs were mined, so it is followed by a
	// query although the tip is the one that was just asked about. The
	// replaced block is found by scanning, after the query for the new tip.
	be.set(func(b *fakeBackend) { b.rewinds = nil })
	e.srv.reorg(1, 1)
	e.waitSynced(152)
	e.assertFollows()
	be.set(func(b *fakeBackend) {
		if len(b.rewinds) != 1 {
			t.Errorf("rewinds: %v, want one", b.rewinds)
		}
	})
	idle()
	if got := queries(); got != 9 {
		t.Fatalf("%d queries after a reorganization found by scanning, want 9", got)
	}
}

func TestCancelDuringEnhancement(t *testing.T) {
	e, be := newFakeEnv(t, 150, nil)
	e.createAccount(100)
	var requests []txRequest
	for i := range 50 {
		requests = append(requests, txRequest{kind: txRequestStatus, txid: bytes.Repeat([]byte{byte(i)}, 32)})
	}
	be.set(func(b *fakeBackend) { b.requests = requests })
	gate := make(chan struct{})
	defer close(gate)
	entered := make(chan string, 64)
	e.srv.set(func(s *fakeLwd) {
		s.gate["GetTransaction"] = gate
		s.entered = entered
	})

	stop := e.connect()
	for got := range entered {
		if got == "GetTransaction" {
			break
		}
	}
	start := time.Now()
	stop()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("stopping took %s", elapsed)
	}
}

func TestBackoff(t *testing.T) {
	lo, hi := 2*time.Second, 30*time.Second
	seconds := []int{2, 2, 4, 8, 16, 30, 30, 30}
	for n, secs := range seconds {
		want := time.Duration(secs) * time.Second
		if got := backoff(n, lo, hi); got != want {
			t.Errorf("backoff(%d): %s, want %s", n, got, want)
		}
	}
	if got := backoff(1000, lo, hi); got != hi {
		t.Errorf("backoff(1000): %s", got)
	}
	if got := backoff(3, time.Minute, time.Second); got != time.Second {
		t.Errorf("backoff with lo above hi: %s", got)
	}
}

func TestJitter(t *testing.T) {
	const d = 20 * time.Second
	lo, hi := d, d
	for range 1000 {
		got := jitter(d)
		if got < d*3/4 || got > d*5/4 {
			t.Fatalf("jitter(%s): %s", d, got)
		}
		lo, hi = min(lo, got), max(hi, got)
	}
	if hi-lo < d/4 {
		t.Errorf("jitter barely varies: %s to %s", lo, hi)
	}
}

func TestSameHash(t *testing.T) {
	a := fakeBlockHash(7, 1)
	reversed := append([]byte(nil), a...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	if !sameHash(a, a) || !sameHash(a, reversed) || !sameHash(reversed, a) {
		t.Error("a hash does not match itself or its reverse")
	}
	if sameHash(a, fakeBlockHash(8, 1)) || sameHash(a, nil) {
		t.Error("different hashes match")
	}
	if !bytes.Equal(a, fakeBlockHash(7, 1)) {
		t.Error("sameHash modified its argument")
	}
}

func TestMinedHeight(t *testing.T) {
	for _, height := range []uint64{0, math.MaxUint64, math.MaxUint32 + 1} {
		if h := minedHeight(&walletrpc.RawTransaction{Height: height}); h != nil {
			t.Errorf("minedHeight(%d): %d", height, *h)
		}
	}
	for _, height := range []uint64{1, 150, math.MaxUint32} {
		if h := minedHeight(&walletrpc.RawTransaction{Height: height}); h == nil || uint64(*h) != height {
			t.Errorf("minedHeight(%d): %v", height, h)
		}
	}
}

func TestSyncStatusEqual(t *testing.T) {
	base := SyncStatus{State: SyncSyncing, ChainTip: 10, FullyScanned: 5, ScanProgress: Ratio{1, 2}}
	same := base
	if !base.equal(&same) {
		t.Error("a status does not equal its copy")
	}
	changes := []func(*SyncStatus){
		func(s *SyncStatus) { s.State = SyncSynced },
		func(s *SyncStatus) { s.ChainTip++ },
		func(s *SyncStatus) { s.FullyScanned++ },
		func(s *SyncStatus) { s.ScanProgress.Numerator++ },
		func(s *SyncStatus) { s.RecoveryProgress.Denominator++ },
		func(s *SyncStatus) { s.Recovering = true },
		func(s *SyncStatus) { s.Err = errors.New("x") },
	}
	for i, change := range changes {
		other := base
		change(&other)
		if base.equal(&other) || other.equal(&base) {
			t.Errorf("change %d is not seen", i)
		}
	}
	a, b := base, base
	a.Err, b.Err = errors.New("x"), errors.New("x")
	if !a.equal(&b) {
		t.Error("statuses with the same error text differ")
	}
	b.Err = errors.New("y")
	if a.equal(&b) {
		t.Error("statuses with different errors are equal")
	}
}

func TestSyncStateString(t *testing.T) {
	want := map[SyncState]string{
		SyncStopped: "stopped", SyncConnecting: "connecting", SyncSyncing: "syncing",
		SyncSynced: "synced", SyncDisconnected: "disconnected", SyncState(77): "SyncState(77)",
	}
	for state, s := range want {
		if got := state.String(); got != s {
			t.Errorf("%d: %q, want %q", uint8(state), got, s)
		}
	}
}

func TestSyncOptionsDefaults(t *testing.T) {
	opts := syncOptionsFor(&Config{})
	if opts.pollInterval != defaultPollInterval || opts.batchSize != defaultBatchSize ||
		opts.maxBatchBytes != defaultMaxBatchBytes || opts.minBackoff != defaultMinBackoff ||
		opts.maxBackoff != defaultMaxBackoff || opts.addressCheckInterval != defaultAddressCheckInterval ||
		opts.now == nil {
		t.Errorf("defaults: %+v", opts)
	}
	opts = syncOptionsFor(&Config{PollInterval: time.Second, BatchSize: 5, MinConfirmations: 2})
	if opts.pollInterval != time.Second || opts.batchSize != 5 || opts.minConfirmations != 2 {
		t.Errorf("configured: %+v", opts)
	}
	if opts := syncOptionsFor(&Config{PollInterval: -time.Second}); opts.pollInterval != defaultPollInterval {
		t.Errorf("negative poll interval: %s", opts.pollInterval)
	}
}
