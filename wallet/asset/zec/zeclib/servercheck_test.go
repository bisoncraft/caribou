// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

// otherBranch is a consensus branch that no wallet of the tests has, the one
// of NU6.
const otherBranch = "c8e71055"

// forgetCalls clears the call counts of servers.
func forgetCalls(servers ...*fakeLwd) {
	for _, srv := range servers {
		srv.set(func(s *fakeLwd) { clear(s.calls) })
	}
}

// onlyInfo checks that a server was asked about itself and for nothing else.
func onlyInfo(t *testing.T, name string, srv *fakeLwd) {
	t.Helper()
	srv.set(func(s *fakeLwd) {
		for method, n := range s.calls {
			if method != "GetLightdInfo" && n != 0 {
				t.Errorf("%s: %d calls of %s", name, n, method)
			}
		}
	})
}

// tuned adds to the sync options that the tests set for the wallet of e.
func tuned(e *env, more func(o *syncOptions)) {
	tune := e.w.tune
	e.w.tune = func(o *syncOptions) {
		tune(o)
		more(o)
	}
}

func TestCheckConsensusBranch(t *testing.T) {
	be := newFakeBackend()
	be.branchFrom = 120
	boom := wrapf(ErrDatabase, "disk full")
	tests := []struct {
		name   string
		branch string
		height uint64
		fail   error
		want   error
	}{
		{name: "the wallet's branch", branch: "37a5165b", height: 119},
		{name: "in capitals", branch: "37A5165B", height: 119},
		{name: "not reported", branch: "", height: 119},
		{name: "another branch", branch: otherBranch, height: 119, want: ErrConsensusBranch},
		{name: "not a number", branch: "nu6.3", height: 119, want: ErrConsensusBranch},
		{name: "too long", branch: "37a5165b00", height: 119, want: ErrConsensusBranch},
		{name: "the branch before an upgrade, at its height", branch: "37a5165b", height: 120, want: ErrConsensusBranch},
		{name: "the branch of an upgrade, at its height", branch: "77190ad9", height: 120},
		{name: "the branch of an upgrade, below it", branch: "77190ad9", height: 119, want: ErrConsensusBranch},
		{name: "a backend error", branch: "37a5165b", height: 119, fail: boom, want: boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			be.set(func(b *fakeBackend) {
				clear(b.fail)
				if tt.fail != nil {
					b.fail["consensusBranchID"] = tt.fail
				}
			})
			err := checkConsensusBranch(&walletrpc.LightdInfo{ConsensusBranchId: tt.branch, BlockHeight: tt.height}, be)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}

	err := checkConsensusBranch(&walletrpc.LightdInfo{ConsensusBranchId: "37a5165b", BlockHeight: 1 << 32}, be)
	if !isServerError(err) {
		t.Errorf("a height out of range: %v", err)
	}
}

func TestServerLag(t *testing.T) {
	for _, tt := range []struct{ height, estimated, want uint64 }{
		{100, 0, 0},
		{100, 99, 0},
		{100, 100, 0},
		{100, 101, 1},
		{100, 124, 24},
	} {
		got := serverLag(&walletrpc.LightdInfo{BlockHeight: tt.height, EstimatedHeight: tt.estimated})
		if got != tt.want {
			t.Errorf("height %d, estimated %d: lag %d, want %d", tt.height, tt.estimated, got, tt.want)
		}
	}
}

// TestServerOnAnotherConsensusBranch checks that a wallet leaves a server
// that reports another consensus branch for its tip than the wallet has, and
// that it does so before it asks that server for anything else.
func TestServerOnAnotherConsensusBranch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e, servers := serverEnvFor(t, kind, 150, 2)
		e.createAccount(100)
		servers[0].set(func(s *fakeLwd) { s.branch = otherBranch })
		forgetCalls(servers...)

		e.connect()
		e.waitSynced(150)
		if got := e.w.servers.get().address; got != servers[1].addr {
			t.Errorf("the wallet uses %s", got)
		}
		onlyInfo(t, "the server on another branch", servers[0])
	})
}

// TestEveryServerOnAnotherConsensusBranch is the case of a wallet that has
// missed a network upgrade. It reports ErrConsensusBranch, keeps asking the
// servers about themselves and for nothing else, and syncs once a server
// agrees with it.
func TestEveryServerOnAnotherConsensusBranch(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	e.createAccount(100)
	branch := servers[1].branch
	for _, srv := range servers {
		srv.set(func(s *fakeLwd) { s.branch = otherBranch })
	}
	forgetCalls(servers...)

	e.connect()
	e.statuses.wait(t, "the other branch", func(s SyncStatus) bool {
		return s.State == SyncDisconnected && errors.Is(s.Err, ErrConsensusBranch)
	})
	eventually(t, "every server to be asked twice", func() bool {
		return servers[0].callCount("GetLightdInfo") >= 2 && servers[1].callCount("GetLightdInfo") >= 2
	})
	onlyInfo(t, "the first server", servers[0])
	onlyInfo(t, "the second server", servers[1])

	e.statuses.reset()
	servers[1].set(func(s *fakeLwd) { s.branch = branch })
	e.waitSynced(150)
	if got := e.w.servers.get().address; got != servers[1].addr {
		t.Errorf("the wallet uses %s", got)
	}
	onlyInfo(t, "the first server", servers[0])
}

// TestServerThatMissedAnUpgrade checks that a server is checked again while
// the wallet is connected. The wallet has a network upgrade at height 152.
// The first server passes that height and goes on reporting the branch from
// before it.
func TestServerThatMissedAnUpgrade(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	tuned(e, func(o *syncOptions) { o.serverCheckInterval = 20 * time.Millisecond })
	e.createAccount(100)
	e.connect()
	e.waitSynced(150)
	if got := e.w.servers.get().address; got != servers[0].addr {
		t.Fatalf("the wallet uses %s", got)
	}

	e.statuses.reset()
	e.be.(*fakeBackend).set(func(b *fakeBackend) { b.branchFrom = 152 })
	servers[1].set(func(s *fakeLwd) {
		s.branch = fmt.Sprintf("%08x", uint32(laterBranch))
		s.mineLocked(2)
	})
	servers[0].mine(2)
	e.statuses.wait(t, "the other branch", func(s SyncStatus) bool { return errors.Is(s.Err, ErrConsensusBranch) })
	e.waitSynced(152)
	if got := e.w.servers.get().address; got != servers[1].addr {
		t.Errorf("the wallet uses %s", got)
	}
}

// TestCreateAccountChecksTheConsensusBranch checks that an account does not
// take its birthday tree state from a server on another consensus branch.
func TestCreateAccountChecksTheConsensusBranch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e, servers := serverEnvFor(t, kind, 150, 2)
		servers[0].set(func(s *fakeLwd) { s.branch = otherBranch })
		e.createAccount(100)
		if got := e.w.servers.get().address; got != servers[1].addr {
			t.Errorf("the wallet uses %s", got)
		}
		onlyInfo(t, "the server on another branch", servers[0])
	})

	e, servers := serverEnv(t, 150, 1)
	servers[0].set(func(s *fakeLwd) { s.branch = otherBranch })
	_, err := e.w.CreateAccount(context.Background(), &AccountParams{Name: "test", Seed: testSeed, Birthday: 100})
	if !errors.Is(err, ErrConsensusBranch) {
		t.Errorf("CreateAccount with one server on another branch: %v", err)
	}
	if accounts, err := e.w.Accounts(); err != nil || len(accounts) != 0 {
		t.Errorf("accounts: %v, %v", accounts, err)
	}
	onlyInfo(t, "the server on another branch", servers[0])
}

// TestStaleServerIsPassedOver checks that a wallet leaves a server whose
// node estimates the chain to be half an hour of blocks ahead of its own
// tip, without asking it for anything else, and stays with one that is a
// block short of that.
func TestStaleServerIsPassedOver(t *testing.T) {
	for _, tt := range []struct {
		lag  uint64
		want int
	}{
		{defaultStaleLag - 1, 0},
		{defaultStaleLag, 1},
	} {
		t.Run(fmt.Sprintf("%d blocks behind", tt.lag), func(t *testing.T) {
			e, servers := serverEnv(t, 150, 2)
			tuned(e, func(o *syncOptions) { o.staleLag = defaultStaleLag })
			e.createAccount(100)
			servers[0].set(func(s *fakeLwd) { s.lag = tt.lag })
			forgetCalls(servers...)

			e.connect()
			e.waitSynced(150)
			if got := e.w.servers.get().address; got != servers[tt.want].addr {
				t.Errorf("the wallet uses %s, want server %d", got, tt.want)
			}
			if tt.want == 1 {
				onlyInfo(t, "the stale server", servers[0])
				if n := servers[0].callCount("GetLightdInfo"); n != 1 {
					t.Errorf("the stale server was asked about itself %d times", n)
				}
			} else if n := calls(servers[1]); n != 0 {
				t.Errorf("the second server was called %d times", n)
			}
		})
	}
}

// TestEveryServerStale is the case of a chain on which no block was found
// for a long time. Each server is asked about itself once, and the wallet
// then syncs with the one it started with. The others have learned nothing
// of it.
func TestEveryServerStale(t *testing.T) {
	e, servers := serverEnv(t, 150, 3)
	tuned(e, func(o *syncOptions) { o.staleLag = defaultStaleLag })
	e.createAccount(100)
	for _, srv := range servers {
		srv.set(func(s *fakeLwd) { s.lag = 40 })
	}
	forgetCalls(servers...)

	e.connect()
	status := e.waitSynced(150)
	if status.Err != nil {
		t.Errorf("status: %+v", status)
	}
	if got := e.w.servers.get().address; got != servers[0].addr {
		t.Errorf("the wallet uses %s", got)
	}
	for i, srv := range servers[1:] {
		onlyInfo(t, fmt.Sprintf("server %d", i+1), srv)
		if n := srv.callCount("GetLightdInfo"); n != 1 {
			t.Errorf("server %d was asked about itself %d times", i+1, n)
		}
	}
	if n := servers[0].callCount("GetLightdInfo"); n != 2 {
		t.Errorf("the first server was asked about itself %d times, want 2", n)
	}

	// The wallet goes on following the server it settled on.
	servers[0].mine(1)
	e.waitSynced(151)
	if got := e.w.servers.get().address; got != servers[0].addr {
		t.Errorf("the wallet uses %s after a new block", got)
	}
}

// TestTheOnlyServerIsStale checks that a wallet with a single server syncs
// with it however far behind its node is.
func TestTheOnlyServerIsStale(t *testing.T) {
	e, servers := serverEnv(t, 150, 1)
	tuned(e, func(o *syncOptions) { o.staleLag = defaultStaleLag })
	e.createAccount(100)
	servers[0].set(func(s *fakeLwd) { s.lag = 1000 })

	e.connect()
	if status := e.waitSynced(150); status.Err != nil {
		t.Errorf("status: %+v", status)
	}
}

// TestServerThatFallsBehind checks that a server is found stale while the
// wallet is connected to it, and that the wallet leaves it at once: a server
// that has been working gets no second attempt when its node is behind.
func TestServerThatFallsBehind(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	tuned(e, func(o *syncOptions) {
		o.staleLag = defaultStaleLag
		o.serverCheckInterval = 20 * time.Millisecond
	})
	e.createAccount(100)
	e.connect()
	e.waitSynced(150)
	if got := e.w.servers.get().address; got != servers[0].addr {
		t.Fatalf("the wallet uses %s", got)
	}

	e.statuses.reset()
	servers[1].mine(3)
	servers[0].set(func(s *fakeLwd) {
		s.lag = 30
		clear(s.calls)
	})
	e.statuses.wait(t, "the stale server", func(s SyncStatus) bool {
		return s.State == SyncDisconnected && errors.Is(s.Err, ErrServerStale)
	})
	e.waitSynced(153)
	if got := e.w.servers.get().address; got != servers[1].addr {
		t.Errorf("the wallet uses %s", got)
	}
	if n := servers[0].callCount("GetLightdInfo"); n != 1 {
		t.Errorf("the stale server was asked about itself %d times, want 1", n)
	}
}

// TestStaleServerAmongUnreachableOnes checks that one tour of the servers is
// enough to settle on a stale server when no other can be reached.
func TestStaleServerAmongUnreachableOnes(t *testing.T) {
	e, servers := serverEnv(t, 150, 3)
	tuned(e, func(o *syncOptions) { o.staleLag = defaultStaleLag })
	e.createAccount(100)
	servers[0].set(func(s *fakeLwd) { s.lag = 40 })
	down(servers[1])
	down(servers[2])
	forgetCalls(servers...)

	e.connect()
	e.waitSynced(150)
	if got := e.w.servers.get().address; got != servers[0].addr {
		t.Errorf("the wallet uses %s", got)
	}
	if n := servers[0].callCount("GetLightdInfo"); n != 2 {
		t.Errorf("the stale server was asked about itself %d times, want 2", n)
	}
	for i, srv := range servers[1:] {
		if n := calls(srv); n != 1 {
			t.Errorf("server %d was called %d times, want 1", i+1, n)
		}
	}
}

// TestStaleServerAndOneThatCannotServe checks that a server whose node is not
// behind does not end the tour by saying so. The second server reports a
// current node and fails the request for its chain tip. The wallet comes
// back to the stale server, which can serve it.
func TestStaleServerAndOneThatCannotServe(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	tuned(e, func(o *syncOptions) { o.staleLag = defaultStaleLag })
	e.createAccount(100)
	servers[0].set(func(s *fakeLwd) { s.lag = 40 })
	servers[1].set(func(s *fakeLwd) { s.fail["GetLatestBlock"] = errServerDown })
	forgetCalls(servers...)

	e.connect()
	e.waitSynced(150)
	if got := e.w.servers.get().address; got != servers[0].addr {
		t.Errorf("the wallet uses %s", got)
	}
	if n := servers[0].callCount("GetLightdInfo"); n != 2 {
		t.Errorf("the stale server was asked about itself %d times, want 2", n)
	}
	if n := servers[1].callCount("GetLatestBlock"); n != 1 {
		t.Errorf("the server that cannot serve was asked for its tip %d times, want 1", n)
	}
	for _, method := range []string{"GetBlockRange", "GetTaddressTxids", "GetAddressUtxosStream", "GetTransaction"} {
		if n := servers[1].callCount(method); n != 0 {
			t.Errorf("the server that cannot serve: %d calls of %s", n, method)
		}
	}
}

// TestSecondStaleTour checks that a tour which ended at a working server is
// over. The first server is stale and the wallet syncs with the second. When
// the second falls behind as well, a new tour begins there, finds the first
// still stale, and ends where it began.
func TestSecondStaleTour(t *testing.T) {
	e, servers := serverEnv(t, 150, 2)
	tuned(e, func(o *syncOptions) {
		o.staleLag = defaultStaleLag
		o.serverCheckInterval = 20 * time.Millisecond
	})
	e.createAccount(100)
	servers[0].set(func(s *fakeLwd) { s.lag = 40 })
	e.connect()
	e.waitSynced(150)
	if got := e.w.servers.get().address; got != servers[1].addr {
		t.Fatalf("the wallet uses %s", got)
	}

	e.statuses.reset()
	servers[0].set(func(s *fakeLwd) { clear(s.calls) })
	servers[1].set(func(s *fakeLwd) {
		s.lag = 40
		s.mineLocked(1)
	})
	e.waitSynced(151)
	if got := e.w.servers.get().address; got != servers[1].addr {
		t.Errorf("the wallet uses %s", got)
	}
	eventually(t, "the first server to be asked about itself", func() bool {
		return servers[0].callCount("GetLightdInfo") > 0
	})
	onlyInfo(t, "the first server", servers[0])
}

// agingSource is a chainSource on which the calls of one method take time on
// the clock of a test.
type agingSource struct {
	chainSource
	now    *time.Time
	method string
	calls  int
	// after is called after each call of the method, with their number.
	after func(calls int)
}

func (s *agingSource) age(method string) {
	if method != s.method {
		return
	}
	s.calls++
	*s.now = s.now.Add(4 * time.Minute)
	if s.after != nil {
		s.after(s.calls)
	}
}

func (s *agingSource) latestBlock(ctx context.Context) (*walletrpc.BlockID, error) {
	defer s.age("GetLatestBlock")
	return s.chainSource.latestBlock(ctx)
}

func (s *agingSource) blockRange(ctx context.Context, start, end uint32, fn func(*walletrpc.CompactBlock) error) error {
	defer s.age("GetBlockRange")
	return s.chainSource.blockRange(ctx, start, end, fn)
}

func (s *agingSource) addressTransactions(ctx context.Context, address string, start, end uint32, fn func(*walletrpc.RawTransaction) error) error {
	defer s.age("GetTaddressTxids")
	return s.chainSource.addressTransactions(ctx, address, start, end, fn)
}

func (s *agingSource) transaction(ctx context.Context, txid []byte) (*walletrpc.RawTransaction, error) {
	defer s.age("GetTransaction")
	return s.chainSource.transaction(ctx, txid)
}

// TestServerIsCheckedDuringALongSyncAttempt checks that the server is checked
// again when ten minutes have passed within one sync attempt, before the
// next request of each kind that tells it something of the wallet and before
// the next batch of blocks. Every call of one method takes four minutes
// here, and the server changes to another consensus branch with the third.
// The fourth is not made.
func TestServerIsCheckedDuringALongSyncAttempt(t *testing.T) {
	for _, method := range []string{"GetTaddressTxids", "GetBlockRange", "GetTransaction"} {
		t.Run(method, func(t *testing.T) {
			e, be := newFakeEnv(t, 150, nil)
			e.createAccount(100)
			var requests []txRequest
			for i := range 6 {
				requests = append(requests, txRequest{kind: txRequestStatus, txid: bytes.Repeat([]byte{byte(i + 1)}, 32)})
			}
			be.set(func(b *fakeBackend) { b.requests = requests })
			now := time.Unix(1_700_000_000, 0)
			p := newTestProcessor(t, e.srv, be, &now)
			p.opts.batchSize = 10
			p.src = &agingSource{chainSource: p.src, now: &now, method: method, after: func(calls int) {
				if calls == 3 {
					e.srv.set(func(s *fakeLwd) { s.branch = otherBranch })
				}
			}}
			forgetCalls(e.srv)

			_, err := p.cycle(context.Background())
			if !errors.Is(err, ErrConsensusBranch) {
				t.Fatalf("cycle: %v", err)
			}
			if n := e.srv.callCount(method); n != 3 {
				t.Errorf("%d calls of %s, want 3", n, method)
			}
			if n := e.srv.callCount("GetLightdInfo"); n != 2 {
				t.Errorf("the server was asked about itself %d times, want 2", n)
			}
		})
	}

	// The query for unspent outputs. In a second attempt the wallet has no
	// address left to ask the history of, and a new block makes it ask for
	// the unspent outputs again. The time passes while it asks for the tip.
	t.Run("GetAddressUtxosStream", func(t *testing.T) {
		e, be := newFakeEnv(t, 150, nil)
		e.createAccount(100)
		now := time.Unix(1_700_000_000, 0)
		p := newTestProcessor(t, e.srv, be, &now)
		if _, err := p.cycle(context.Background()); err != nil {
			t.Fatalf("the first cycle: %v", err)
		}
		e.srv.mine(1)
		p.src = &agingSource{chainSource: p.src, now: &now, method: "GetLatestBlock", after: func(int) {
			now = now.Add(time.Hour)
			e.srv.set(func(s *fakeLwd) { s.branch = otherBranch })
		}}
		forgetCalls(e.srv)

		_, err := p.cycle(context.Background())
		if !errors.Is(err, ErrConsensusBranch) {
			t.Fatalf("the second cycle: %v", err)
		}
		for _, method := range []string{"GetTaddressTxids", "GetAddressUtxosStream", "GetBlockRange"} {
			if n := e.srv.callCount(method); n != 0 {
				t.Errorf("%d calls of %s after the check was due", n, method)
			}
		}
	})

	// A server that stays as it is passes the checks, and the attempt ends.
	t.Run("unchanged", func(t *testing.T) {
		e, be := newFakeEnv(t, 150, nil)
		e.createAccount(100)
		now := time.Unix(1_700_000_000, 0)
		p := newTestProcessor(t, e.srv, be, &now)
		p.opts.batchSize = 10
		p.src = &agingSource{chainSource: p.src, now: &now, method: "GetBlockRange"}
		forgetCalls(e.srv)

		if _, err := p.cycle(context.Background()); err != nil {
			t.Fatalf("cycle: %v", err)
		}
		// Six batches of four minutes: checked at the start and after the
		// third.
		if n := e.srv.callCount("GetLightdInfo"); n != 2 {
			t.Errorf("the server was asked about itself %d times, want 2", n)
		}
		if got, ok, err := be.maxScannedHeight(); err != nil || !ok || got != 150 {
			t.Errorf("scanned up to %d, %t, %v", got, ok, err)
		}
	})
}

func TestStaleCheckIsOffOnRegtest(t *testing.T) {
	for _, tt := range []struct {
		network Network
		want    uint64
	}{
		{Mainnet, defaultStaleLag},
		{Testnet, defaultStaleLag},
		{testNetwork, 0},
	} {
		opts := syncOptionsFor(&Config{Network: tt.network})
		if opts.staleLag != tt.want || opts.serverCheckInterval != defaultServerCheckInterval {
			t.Errorf("%s: stale lag %d, check interval %s", tt.network, opts.staleLag, opts.serverCheckInterval)
		}
	}
}
