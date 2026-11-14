// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

func TestSyncToTip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)

		if got := e.w.SyncStatus(); got.State != SyncStopped || got.Err != nil {
			t.Fatalf("status before Connect: %+v", got)
		}
		if summary, err := e.w.Summary(); err != nil || summary != nil {
			t.Fatalf("Summary of an empty wallet: %+v, %v", summary, err)
		}

		account := e.createAccount(100)
		if account.Birthday != 100 || account.Name != "test" || !account.Derived || account.HDIndex != 0 {
			t.Fatalf("account: %+v", account)
		}
		accounts, err := e.w.Accounts()
		if err != nil || len(accounts) != 1 || accounts[0].ID != account.ID {
			t.Fatalf("Accounts: %+v, %v", accounts, err)
		}

		stop := e.connect()
		synced := e.waitSynced(150)
		if synced.Err != nil {
			t.Fatalf("synced with an error: %v", synced.Err)
		}
		if got := synced.ScanProgress.Fraction(); got != 1 {
			t.Errorf("scan progress when synced: %v (%+v)", got, synced.ScanProgress)
		}
		e.assertFollows()

		// The states come in order: connecting, then syncing, then synced.
		var states []SyncState
		for _, s := range e.statuses.all() {
			if len(states) == 0 || states[len(states)-1] != s.State {
				states = append(states, s.State)
			}
		}
		want := []SyncState{SyncConnecting, SyncSyncing, SyncSynced}
		if len(states) < len(want) {
			t.Fatalf("states: %v, want prefix %v", states, want)
		}
		for i := range want {
			if states[i] != want[i] {
				t.Fatalf("states: %v, want prefix %v", states, want)
			}
		}

		summary, err := e.w.Summary()
		if err != nil || summary == nil {
			t.Fatalf("Summary: %+v, %v", summary, err)
		}
		if summary.ChainTip != 150 || summary.FullyScanned != 150 {
			t.Errorf("summary heights: tip %d, scanned %d", summary.ChainTip, summary.FullyScanned)
		}
		balance, ok := summary.Balance(account.ID)
		if !ok || balance.Total != 0 {
			t.Errorf("balance: %+v, %t", balance, ok)
		}
		if _, ok := summary.Balance("no such account"); ok {
			t.Error("a balance was found for an unknown account")
		}

		stop()
		if got := e.w.SyncStatus(); got.State != SyncStopped || got.Err != nil {
			t.Fatalf("status after stopping: %+v", got)
		}
		// The scan state outlives the connection.
		if got := e.w.SyncStatus(); got.FullyScanned != 150 || got.ChainTip != 150 {
			t.Errorf("heights after stopping: %+v", got)
		}
	})
}

func TestSyncFollowsNewBlocks(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 120, nil)
		e.createAccount(100)
		e.connect()
		e.waitSynced(120)

		for _, n := range []uint32{1, 5, 1} {
			e.srv.mine(n)
			e.waitSynced(e.srv.height())
			e.assertFollows()
		}
	})
}

func TestSyncInBatches(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, func(cfg *Config) { cfg.BatchSize = 7 })
		e.createAccount(100)
		e.connect()
		e.waitSynced(150)
		e.assertFollows()

		// 51 blocks in batches of at most 7.
		if got := e.srv.callCount("GetBlockRange"); got < 8 {
			t.Errorf("GetBlockRange was called %d times, want at least 8", got)
		}
		// Progress was reported between batches.
		partial := false
		for _, s := range e.statuses.all() {
			if s.State == SyncSyncing && s.FullyScanned > 0 && s.FullyScanned < 150 {
				partial = true
			}
		}
		if !partial {
			t.Error("no status with partial progress was reported")
		}
	})
}

func TestSyncByteLimitedBatches(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 130, nil)
		tune := e.w.tune
		e.w.tune = func(o *syncOptions) {
			tune(o)
			// Smaller than two blocks, so each batch holds one or two.
			o.maxBatchBytes = 100
		}
		e.createAccount(100)
		e.connect()
		e.waitSynced(130)
		e.assertFollows()
		if got := e.srv.callCount("GetBlockRange"); got < 15 {
			t.Errorf("GetBlockRange was called %d times, want at least 15", got)
		}
	})
}

func TestReorg(t *testing.T) {
	tests := []struct {
		name string
		// depth blocks are replaced by depth+grow new ones.
		depth, grow uint32
	}{
		{"tip replaced", 1, 0},
		{"same length", 3, 0},
		{"longer", 3, 2},
		{"deeper than one rewind", 25, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, kind backendKind) {
				e := newEnv(t, kind, 150, nil)
				e.createAccount(100)
				e.connect()
				e.waitSynced(150)
				old := e.srv.hashAt(150)

				e.srv.reorg(tt.depth, tt.grow)
				tip := e.srv.height()
				eventually(t, "the wallet to follow the new chain", func() bool {
					hash, err := e.be.blockHash(tip)
					if err != nil {
						t.Fatalf("blockHash: %v", err)
					}
					return sameHash(hash, e.srv.hashAt(tip)) && !sameHash(hash, old)
				})
				e.statuses.reset()
				e.srv.mine(1)
				e.waitSynced(tip + 1)
				e.assertFollows()
			})
		})
	}
}

func TestReorgToShorterChain(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		e.createAccount(100)
		e.connect()
		e.waitSynced(150)

		// The last 6 blocks are replaced by 2, leaving the tip at 146,
		// below what the wallet has scanned.
		e.srv.set(func(s *fakeLwd) {
			s.blocks = s.blocks[:len(s.blocks)-6]
			s.fork++
			s.mineLocked(2)
		})
		eventually(t, "the wallet to rewind", func() bool {
			highest, _, err := e.be.maxScannedHeight()
			if err != nil {
				t.Fatalf("maxScannedHeight: %v", err)
			}
			return highest <= 146
		})
		e.waitSynced(146)
		e.assertFollows()
	})
}

func TestLaggingServerDoesNotRewind(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		e.createAccount(100)
		e.connect()
		e.waitSynced(150)

		// The server reports an older block of the same chain as its tip,
		// first in the byte order of a compact block and then reversed.
		for _, reversed := range []bool{false, true} {
			hash := append([]byte(nil), e.srv.hashAt(140)...)
			if reversed {
				for i, j := 0, len(hash)-1; i < j; i, j = i+1, j-1 {
					hash[i], hash[j] = hash[j], hash[i]
				}
			}
			e.srv.set(func(s *fakeLwd) {
				s.tip = &walletrpc.BlockID{Height: 140, Hash: hash}
			})
			before := e.srv.callCount("GetLatestBlock")
			eventually(t, "a few polls", func() bool {
				return e.srv.callCount("GetLatestBlock") >= before+3
			})
			if highest, _, err := e.be.maxScannedHeight(); err != nil || highest != 150 {
				t.Fatalf("max scanned height is %d (%v), want 150", highest, err)
			}
			if st := e.w.SyncStatus(); st.Err != nil || st.State != SyncSynced {
				t.Fatalf("status with a lagging server: %+v", st)
			}
		}

		// A tip without a hash cannot be checked and is left alone.
		e.srv.set(func(s *fakeLwd) { s.tip = &walletrpc.BlockID{Height: 140} })
		before := e.srv.callCount("GetLatestBlock")
		eventually(t, "a few polls", func() bool {
			return e.srv.callCount("GetLatestBlock") >= before+3
		})
		if highest, _, err := e.be.maxScannedHeight(); err != nil || highest != 150 {
			t.Fatalf("max scanned height is %d (%v), want 150", highest, err)
		}

		e.srv.set(func(s *fakeLwd) { s.tip = nil })
		e.srv.mine(2)
		e.waitSynced(152)
	})
}

func TestStopAndRestart(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		e.createAccount(100)

		stop := e.connect()
		e.waitSynced(150)
		if _, err := e.w.Connect(context.Background()); !errors.Is(err, ErrConnected) {
			t.Fatalf("second Connect: %v, want ErrConnected", err)
		}
		stop()
		polls := e.srv.callCount("GetLatestBlock")

		// Nothing happens while stopped.
		e.srv.mine(10)
		time.Sleep(100 * time.Millisecond)
		if got := e.srv.callCount("GetLatestBlock"); got != polls {
			t.Fatalf("the server was polled %d times while stopped", got-polls)
		}
		if got := e.w.SyncStatus(); got.State != SyncStopped || got.FullyScanned != 150 {
			t.Fatalf("status while stopped: %+v", got)
		}
		// The wallet can still be read.
		if _, err := e.w.Accounts(); err != nil {
			t.Fatalf("Accounts while stopped: %v", err)
		}

		e.statuses.reset()
		stop = e.connect()
		e.waitSynced(160)
		e.assertFollows()
		stop()
		stop() // Stopping twice is harmless.
	})
}

func TestConnectWithCancelledContext(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		e.createAccount(100)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		wg, err := e.w.Connect(ctx)
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		waitGroup(t, wg)
		if got := e.w.SyncStatus(); got.State != SyncStopped {
			t.Fatalf("status: %+v", got)
		}
		// And it can be connected again.
		e.connect()
		e.waitSynced(150)
	})
}

func TestCloseStopsSync(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		account := e.createAccount(100)

		wg, err := e.w.Connect(context.Background())
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		e.waitSynced(150)

		if err := e.w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Close returns only once syncing has stopped.
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("the WaitGroup was not done when Close returned")
		}
		if got := e.w.SyncStatus(); got.State != SyncStopped {
			t.Fatalf("status after Close: %+v", got)
		}

		if err := e.w.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if _, err := e.w.Connect(context.Background()); !errors.Is(err, ErrClosed) {
			t.Errorf("Connect after Close: %v, want ErrClosed", err)
		}
		if _, err := e.w.Accounts(); !errors.Is(err, ErrClosed) {
			t.Errorf("Accounts after Close: %v, want ErrClosed", err)
		}
		if _, err := e.w.Summary(); !errors.Is(err, ErrClosed) {
			t.Errorf("Summary after Close: %v, want ErrClosed", err)
		}
		if _, err := e.w.Address(account.ID); !errors.Is(err, ErrClosed) {
			t.Errorf("Address after Close: %v, want ErrClosed", err)
		}
	})
}

func TestCloseWhileStopped(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 110, nil)
		e.createAccount(100)
		stop := e.connect()
		e.waitSynced(110)
		stop()
		if err := e.w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

func TestReconnectRightAfterCancel(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		// The statuses are recorded together with marks for the moments
		// at which the test connects and cancels.
		const connected, cancelling = SyncState(254), SyncState(255)
		var mtx sync.Mutex
		var events []SyncState
		record := func(s SyncState) {
			mtx.Lock()
			events = append(events, s)
			mtx.Unlock()
		}
		e := newEnv(t, kind, 110, func(cfg *Config) {
			cfg.OnSyncStatus = func(s SyncStatus) {
				// A slow receiver of the last status of a run, which
				// gives a Connect that is let through too early the
				// time to return first.
				if s.State == SyncStopped {
					time.Sleep(time.Millisecond)
				}
				record(s.State)
			}
		})
		e.createAccount(100)

		var wg *sync.WaitGroup
		cancel := func() {}
		for i := range 40 {
			record(cancelling)
			cancel()
			// Connect is refused until the previous run has reported
			// that it stopped, and for no other reason.
			var ctx context.Context
			ctx, cancel = context.WithCancel(context.Background())
			for {
				var err error
				if wg, err = e.w.Connect(ctx); err == nil {
					break
				}
				if !errors.Is(err, ErrConnected) {
					cancel()
					t.Fatalf("Connect %d: %v", i, err)
				}
				runtime.Gosched()
			}
			record(connected)
			// Every few rounds the run gets to sync first.
			if i%4 == 0 {
				eventually(t, "sync", func() bool { return e.w.SyncStatus().State == SyncSynced })
			}
		}
		record(cancelling)
		cancel()
		waitGroup(t, wg)

		// While a run is connected and not yet cancelled, the status is
		// never the stopped status of the run before it.
		mtx.Lock()
		defer mtx.Unlock()
		live := false
		for i, s := range events {
			switch s {
			case connected:
				live = true
			case cancelling:
				live = false
			case SyncStopped:
				if live {
					t.Fatalf("event %d: a stopped status while connected: %v", i, events[max(0, i-6):i+1])
				}
			}
		}
		if last := events[len(events)-1]; last != SyncStopped {
			t.Errorf("the last status is %v", last)
		}
	})
}

func TestCloseRightAfterCancel(t *testing.T) {
	for i := range 40 {
		statuses := newStatusRecorder()
		e, _ := newFakeEnv(t, 110, func(cfg *Config) {
			cfg.OnSyncStatus = func(s SyncStatus) {
				// A slow receiver of the last status, which a Close
				// that does not wait for it returns ahead of.
				if s.State == SyncStopped {
					time.Sleep(5 * time.Millisecond)
				}
				statuses.record(s)
			}
		})
		e.createAccount(100)
		ctx, cancel := context.WithCancel(context.Background())
		wg, err := e.w.Connect(ctx)
		if err != nil {
			cancel()
			t.Fatalf("Connect: %v", err)
		}
		if i%2 == 0 {
			statuses.wait(t, "sync", func(s SyncStatus) bool { return s.State == SyncSynced })
		}
		cancel()
		// The sync goroutine gets a head start of varying length, so that
		// Close finds it at different points of stopping.
		time.Sleep(time.Duration(i%4) * time.Millisecond)
		if err := e.w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// Close has waited for the sync goroutine whether or not the
		// wallet still counted as connected: the last status is out, and
		// nothing is reported after Close has returned.
		reported := statuses.all()
		if len(reported) == 0 || reported[len(reported)-1].State != SyncStopped {
			t.Fatalf("round %d: statuses when Close returned: %+v", i, reported)
		}
		waitGroup(t, wg)
		if after := statuses.all(); len(after) != len(reported) {
			t.Fatalf("round %d: %d statuses were reported after Close returned", i, len(after)-len(reported))
		}
	}
}

func TestConcurrentCloseAndUse(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 400, func(cfg *Config) { cfg.BatchSize = 5 })
		account := e.createAccount(100)
		e.connect()

		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for {
					_, err := e.w.Summary()
					if errors.Is(err, ErrClosed) {
						return
					}
					if err != nil {
						t.Errorf("Summary: %v", err)
						return
					}
					if _, err := e.w.Address(account.ID); err != nil && !errors.Is(err, ErrClosed) {
						t.Errorf("Address: %v", err)
						return
					}
					e.w.SyncStatus()
				}
			})
		}
		// Close in the middle of the initial sync.
		e.statuses.wait(t, "the first scanned batch", func(s SyncStatus) bool {
			return s.FullyScanned > 0
		})
		if err := e.w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		wg.Wait()
	})
}

// TestSyncedMeansScannedToTheTip checks that the wallet never reports
// SyncSynced together with a chain tip above the height it has scanned to.
func TestSyncedMeansScannedToTheTip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 110, nil)
		e.createAccount(100)
		e.connect()
		e.waitSynced(110)

		for height := uint32(111); height <= 115; height++ {
			e.srv.mine(1)
			e.waitSynced(height)
		}
		for _, s := range e.statuses.all() {
			if s.State == SyncSynced && s.ChainTip > s.FullyScanned {
				t.Fatalf("synced at tip %d with blocks scanned to %d only: %+v", s.ChainTip, s.FullyScanned, s)
			}
		}
	})
}

// slowCloseBackend is a backend whose close blocks until it is released.
type slowCloseBackend struct {
	backend
	closing chan struct{}
	release chan struct{}
}

func (b *slowCloseBackend) close() error {
	close(b.closing)
	<-b.release
	return b.backend.close()
}

// TestConcurrentClose checks that a Close call made while another one is
// still closing the wallet waits for it. Close promises that the wallet is
// closed when it returns, whichever call that is.
func TestConcurrentClose(t *testing.T) {
	be := &slowCloseBackend{
		backend: newFakeBackend(),
		closing: make(chan struct{}),
		release: make(chan struct{}),
	}
	w := newWallet(&Config{Network: testNetwork, Logger: newTestLogger(t)}, be, nil)

	first := make(chan error, 1)
	go func() { first <- w.Close() }()
	<-be.closing

	second := make(chan error, 1)
	go func() { second <- w.Close() }()
	select {
	case err := <-second:
		t.Fatalf("the second Close returned (%v) while the first was still closing the wallet", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(be.release)
	for name, done := range map[string]chan error{"first": first, "second": second} {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the %s Close: %v", name, err)
			}
		case <-time.After(waitTimeout):
			t.Fatalf("the %s Close did not return", name)
		}
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close of a closed wallet: %v", err)
	}
}

func TestServerUnreachableAtConnect(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		e.createAccount(100)
		e.srv.stop()

		// Connect succeeds without a server.
		e.connect()
		down := e.statuses.wait(t, "a disconnected status", func(s SyncStatus) bool {
			return s.State == SyncDisconnected
		})
		if down.Err == nil || !isServerError(down.Err) {
			t.Fatalf("error while the server is down: %v", down.Err)
		}

		e.srv.restart(t)
		synced := e.waitSynced(150)
		if synced.Err != nil {
			t.Fatalf("error after reconnecting: %v", synced.Err)
		}
	})
}

func TestServerOutageWhileSynced(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		e.createAccount(100)
		e.connect()
		e.waitSynced(150)

		e.srv.stop()
		e.statuses.wait(t, "a disconnected status", func(s SyncStatus) bool {
			return s.State == SyncDisconnected && s.Err != nil
		})
		// What the wallet knows is still there while the server is away.
		if got := e.w.SyncStatus(); got.FullyScanned != 150 {
			t.Fatalf("status while the server is down: %+v", got)
		}

		e.srv.mine(4)
		e.srv.restart(t)
		e.waitSynced(154)
		e.assertFollows()

		// The server is identified again after the reconnect.
		if got := e.srv.callCount("GetLightdInfo"); got < 2 {
			t.Errorf("GetLightdInfo was called %d times, want at least 2", got)
		}
	})
}

func TestTransientServerErrors(t *testing.T) {
	methods := []string{
		"GetLightdInfo", "GetSubtreeRoots", "GetLatestBlock", "GetAddressUtxosStream",
		"GetBlockRange", "GetTreeState",
	}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, kind backendKind) {
				e := newEnv(t, kind, 150, nil)
				e.createAccount(100)
				e.srv.set(func(s *fakeLwd) {
					s.fail[method] = status.Error(codes.Unavailable, "try later")
				})
				e.connect()
				failing := e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool {
					return s.Err != nil
				})
				if failing.State != SyncDisconnected || !isServerError(failing.Err) {
					t.Fatalf("status while %s fails: %+v", method, failing)
				}
				if status.Code(errors.Unwrap(failing.Err)) != codes.Unavailable {
					t.Errorf("error does not carry the gRPC status: %v", failing.Err)
				}
				// It keeps retrying.
				calls := e.srv.callCount(method)
				eventually(t, "retries", func() bool { return e.srv.callCount(method) >= calls+2 })

				e.srv.set(func(s *fakeLwd) { delete(s.fail, method) })
				synced := e.waitSynced(150)
				if synced.Err != nil {
					t.Fatalf("error after recovering: %v", synced.Err)
				}
				e.assertFollows()
			})
		})
	}
}

func TestWrongNetwork(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		e.createAccount(100)
		e.srv.set(func(s *fakeLwd) { s.chainName = "main" })

		e.connect()
		wrong := e.statuses.wait(t, "a wrong-network status", func(s SyncStatus) bool {
			return errors.Is(s.Err, ErrWrongNetwork)
		})
		if wrong.State != SyncDisconnected {
			t.Fatalf("state on the wrong network: %v", wrong.State)
		}
		// Nothing is downloaded from a server on another chain.
		calls := e.srv.callCount("GetLightdInfo")
		eventually(t, "retries", func() bool { return e.srv.callCount("GetLightdInfo") >= calls+2 })
		if got := e.srv.callCount("GetBlockRange"); got != 0 {
			t.Fatalf("GetBlockRange was called %d times", got)
		}

		e.srv.set(func(s *fakeLwd) { s.chainName = "regtest" })
		e.waitSynced(150)
	})
}

func TestCancelDuringDownload(t *testing.T) {
	for _, method := range []string{"GetLightdInfo", "GetLatestBlock", "GetBlockRange", "GetTreeState"} {
		t.Run(method, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, kind backendKind) {
				e := newEnv(t, kind, 150, nil)
				e.createAccount(100)
				entered := make(chan string, 64)
				gate := make(chan struct{})
				defer close(gate)
				e.srv.set(func(s *fakeLwd) {
					s.gate[method] = gate
					s.entered = entered
				})

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				wg, err := e.w.Connect(ctx)
				if err != nil {
					t.Fatalf("Connect: %v", err)
				}
				// Wait until the wallet is blocked in the call.
				for got := range entered {
					if got == method {
						break
					}
				}

				start := time.Now()
				cancel()
				waitGroup(t, wg)
				if elapsed := time.Since(start); elapsed > 2*time.Second {
					t.Errorf("stopping took %s", elapsed)
				}
				if got := e.w.SyncStatus(); got.State != SyncStopped || got.Err != nil {
					t.Fatalf("status after cancelling: %+v", got)
				}
			})
		})
	}
}

func TestCancelWhileWaiting(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		// A long poll interval and a long backoff: cancellation has to
		// interrupt the wait.
		e := newEnv(t, kind, 150, func(cfg *Config) { cfg.PollInterval = time.Hour })
		e.w.tune = func(o *syncOptions) {
			o.minBackoff = time.Hour
			o.maxBackoff = time.Hour
		}
		e.createAccount(100)

		stop := e.connect()
		e.waitSynced(150)
		start := time.Now()
		stop()
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("stopping a synced wallet took %s", elapsed)
		}

		// The same while backing off after an error.
		e.srv.stop()
		e.statuses.reset()
		stop = e.connect()
		e.statuses.wait(t, "a disconnected status", func(s SyncStatus) bool {
			return s.State == SyncDisconnected
		})
		start = time.Now()
		stop()
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("stopping a disconnected wallet took %s", elapsed)
		}
	})
}

func TestTransparentOutputs(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, func(cfg *Config) { cfg.MinConfirmations = 1 })
		account := e.createAccount(100)
		addresses, err := e.w.TransparentAddresses(account.ID)
		if err != nil || len(addresses) == 0 {
			t.Fatalf("TransparentAddresses: %v, %v", addresses, err)
		}
		address := addresses[0]
		txid := make([]byte, 32)
		txid[0] = 0xaa
		e.srv.set(func(s *fakeLwd) {
			s.utxos[address] = []*walletrpc.GetAddressUtxosReply{{
				Address:  address,
				Txid:     txid,
				Index:    1,
				Script:   p2pkhScript(address),
				ValueZat: 123_000,
				Height:   120,
			}}
		})

		e.connect()
		e.waitSynced(150)
		summary, err := e.w.Summary()
		if err != nil || summary == nil {
			t.Fatalf("Summary: %+v, %v", summary, err)
		}
		balance, ok := summary.Balance(account.ID)
		if !ok || balance.Transparent.Total != 123_000 || balance.Total != 123_000 {
			t.Fatalf("balance: %+v", balance)
		}

		// Every transparent address of the account was asked about, from a
		// height that covers the account's history.
		var query *walletrpc.GetAddressUtxosArg
		e.srv.set(func(s *fakeLwd) { query = s.utxoQueries[0] })
		if len(query.Addresses) != len(addresses) {
			t.Errorf("asked about %d addresses, the account has %d", len(query.Addresses), len(addresses))
		}
		if query.StartHeight > 120 {
			t.Errorf("asked for outputs from height %d", query.StartHeight)
		}
	})
}

func TestMalformedServerReplies(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		account := e.createAccount(100)
		addresses, err := e.w.TransparentAddresses(account.ID)
		if err != nil {
			t.Fatalf("TransparentAddresses: %v", err)
		}
		e.srv.set(func(s *fakeLwd) {
			s.utxos[addresses[0]] = []*walletrpc.GetAddressUtxosReply{{
				Address: addresses[0], Txid: make([]byte, 32), ValueZat: -1, Height: 120,
			}}
		})
		e.connect()
		bad := e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
		if !isServerError(bad.Err) || bad.State != SyncDisconnected {
			t.Fatalf("status with a malformed output: %+v", bad)
		}

		// A tip height that is not a block height.
		e.srv.set(func(s *fakeLwd) {
			delete(s.utxos, addresses[0])
			s.tip = &walletrpc.BlockID{Height: 1 << 40}
		})
		bad = e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool {
			return s.Err != nil && strings.Contains(s.Err.Error(), "out of range")
		})
		if !isServerError(bad.Err) {
			t.Fatalf("status with an out-of-range tip: %+v", bad)
		}

		// A tip beyond the blocks the server has: it returns nothing.
		e.srv.set(func(s *fakeLwd) { s.tip = &walletrpc.BlockID{Height: 5000} })
		bad = e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool {
			return s.Err != nil && s.ChainTip == 5000
		})
		if !isServerError(bad.Err) {
			t.Fatalf("status with missing blocks: %+v", bad)
		}

		e.srv.set(func(s *fakeLwd) { s.tip = nil })
		e.waitSynced(150)
	})
}

func TestOutOfOrderBlocks(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		e.createAccount(100)
		// The server's block at 120 claims to be block 121.
		var good *walletrpc.CompactBlock
		e.srv.set(func(s *fakeLwd) {
			good = s.blocks[120]
			bad := proto.Clone(good).(*walletrpc.CompactBlock)
			bad.Height = 121
			s.blocks[120] = bad
		})
		e.connect()
		bad := e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
		if !isServerError(bad.Err) {
			t.Fatalf("status with out-of-order blocks: %+v", bad)
		}

		e.srv.set(func(s *fakeLwd) { s.blocks[120] = good })
		e.waitSynced(150)
		e.assertFollows()
	})
}

func TestServerStreamsPastTheRange(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, func(cfg *Config) { cfg.BatchSize = 20 })
		e.createAccount(100)
		e.srv.set(func(s *fakeLwd) { s.rangeOverrun = 5 })
		e.connect()
		bad := e.statuses.wait(t, "a failed attempt", func(s SyncStatus) bool { return s.Err != nil })
		if !isServerError(bad.Err) {
			t.Fatalf("status with blocks past the range: %+v", bad)
		}
		// Nothing of the oversized batch was scanned.
		if _, ok, err := e.be.maxScannedHeight(); err != nil || ok {
			t.Fatalf("blocks were scanned: %t, %v", ok, err)
		}

		e.srv.set(func(s *fakeLwd) { s.rangeOverrun = 0 })
		e.waitSynced(150)
		e.assertFollows()
	})
}

func TestSubtreeRootsFromAServerWithoutThem(t *testing.T) {
	tests := []struct {
		name   string
		server func(*fakeLwd)
	}{{
		// A server built before the Ironwood pool.
		name: "unknown pool",
		server: func(s *fakeLwd) {
			s.rootsFail[walletrpc.ShieldedProtocol_ironwood] = status.Error(codes.InvalidArgument, "unknown shielded protocol")
		},
	}, {
		// A server built before the call.
		name: "unimplemented call",
		server: func(s *fakeLwd) {
			s.fail["GetSubtreeRoots"] = status.Error(codes.Unimplemented, "unknown method GetSubtreeRoots")
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, kind backendKind) {
				e := newEnv(t, kind, 150, nil)
				e.createAccount(100)
				e.srv.set(tt.server)
				e.connect()
				e.waitSynced(150)
				e.assertFollows()
				for _, s := range e.statuses.all() {
					if s.Err != nil {
						t.Fatalf("missing subtree roots failed the sync: %+v", s)
					}
				}
				if got := e.srv.callCount("GetSubtreeRoots"); got != 1 {
					t.Errorf("GetSubtreeRoots was called %d times, want 1", got)
				}
				if be, ok := e.be.(*fakeBackend); ok {
					be.set(func(b *fakeBackend) {
						if len(b.roots) != 0 {
							t.Errorf("stored subtree roots: %+v", b.roots)
						}
					})
				}
			})
		})
	}
}

func TestSubtreeRootsAreStored(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		e.createAccount(100)
		root := func(n byte, height uint64) *walletrpc.SubtreeRoot {
			hash := make([]byte, 32)
			hash[0] = n
			return &walletrpc.SubtreeRoot{RootHash: hash, CompletingBlockHeight: height}
		}
		// The server has roots for every pool. The wallet wants the
		// Ironwood ones only.
		e.srv.set(func(s *fakeLwd) {
			s.roots[walletrpc.ShieldedProtocol_sapling] = []*walletrpc.SubtreeRoot{root(1, 10), root(2, 20)}
			s.roots[walletrpc.ShieldedProtocol_orchard] = []*walletrpc.SubtreeRoot{root(3, 30)}
			s.roots[walletrpc.ShieldedProtocol_ironwood] = []*walletrpc.SubtreeRoot{root(4, 40), root(5, 50), root(6, 60)}
		})
		queries := func() []*walletrpc.GetSubtreeRootsArg {
			var queries []*walletrpc.GetSubtreeRootsArg
			e.srv.set(func(s *fakeLwd) { queries = append(queries, s.rootQueries...) })
			for _, q := range queries {
				if q.ShieldedProtocol != walletrpc.ShieldedProtocol_ironwood {
					t.Errorf("the wallet asked for %s subtree roots", q.ShieldedProtocol)
				}
			}
			return queries
		}

		stop := e.connect()
		e.waitSynced(150)
		first := queries()
		if len(first) != 1 || first[0].StartIndex != 0 {
			t.Fatalf("subtree root requests: %v, want one from index 0", first)
		}
		// The roots are not asked for again while connected.
		e.srv.mine(1)
		e.waitSynced(151)
		if got := e.srv.callCount("GetSubtreeRoots"); got != 1 {
			t.Errorf("GetSubtreeRoots was called %d times while connected, want 1", got)
		}
		if be, ok := e.be.(*fakeBackend); ok {
			be.set(func(b *fakeBackend) {
				if len(b.roots) != 1 || b.roots[0].start != 0 || len(b.roots[0].roots) != 3 {
					t.Errorf("stored subtree roots: %+v", b.roots)
				}
			})
		}
		stop()

		// After a restart the request starts where the wallet's roots end.
		e.srv.set(func(s *fakeLwd) { s.rootQueries = nil })
		e.statuses.reset()
		e.connect()
		e.waitSynced(151)
		if again := queries(); len(again) != 1 || again[0].StartIndex == 0 {
			t.Errorf("subtree root requests after a restart: %v, want one from a later index", again)
		}
	})
}

// TestAccountsBeginAtIronwood checks that no account has a birthday before
// NU6.3, the upgrade that adds the Ironwood pool.
func TestAccountsBeginAtIronwood(t *testing.T) {
	heights := AllUpgradesAt(1)
	heights.NU6_3, heights.NU7 = 120, 120
	e := newEnv(t, fakeKind, 150, func(cfg *Config) { cfg.Network = Regtest(heights) })
	ctx := context.Background()

	tests := []struct {
		name     string
		birthday uint32
		restore  bool
		want     uint32
	}{
		{"below the activation", 40, true, 120},
		// The default is a fixed depth below the tip, which is height 50.
		{"default", 0, false, 120},
		{"at the activation", 120, false, 120},
		{"above the activation", 130, true, 130},
	}
	for _, tt := range tests {
		account, err := e.w.CreateAccount(ctx, &AccountParams{
			Name: tt.name, Seed: testSeed, Birthday: tt.birthday, Restore: tt.restore,
		})
		if err != nil {
			t.Fatalf("%s: CreateAccount: %v", tt.name, err)
		}
		if account.Birthday != tt.want {
			t.Errorf("%s: birthday %d, want %d", tt.name, account.Birthday, tt.want)
		}
	}

	// A tree state from the caller fixes the birthday, so one from before
	// the activation is refused.
	state := &walletrpc.TreeState{Height: 118}
	if _, err := e.w.CreateAccountWithTreeState(&AccountParams{Seed: testSeed}, state, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("tree state before the activation: %v, want ErrInvalidArgument", err)
	}
	state.Height = 119
	account, err := e.w.CreateAccountWithTreeState(&AccountParams{Seed: testSeed}, state, 0)
	if err != nil || account.Birthday != 120 {
		t.Errorf("tree state of the block before the activation: %+v, %v", account, err)
	}
}

// TestNoAccountBeforeIronwood checks that an account cannot be created on a
// chain that has not reached NU6.3, and that the error says so.
func TestNoAccountBeforeIronwood(t *testing.T) {
	heights := AllUpgradesAt(1)
	heights.NU6_3, heights.NU7 = 120, 120
	e := newEnv(t, fakeKind, 118, func(cfg *Config) { cfg.Network = Regtest(heights) })
	ctx := context.Background()

	for _, p := range []*AccountParams{
		{Name: "default", Seed: testSeed},
		{Name: "early", Seed: testSeed, Birthday: 40},
		{Name: "restored", Seed: testSeed, Birthday: 40, Restore: true},
	} {
		_, err := e.w.CreateAccount(ctx, p)
		if !errors.Is(err, ErrInvalidArgument) ||
			!strings.Contains(err.Error(), "NU6.3 activates at height 120 and the chain tip is 118") {
			t.Errorf("%s: %v", p.Name, err)
		}
	}
	// The server was not asked for a tree state it does not have.
	if got := e.srv.callCount("GetTreeState"); got != 0 {
		t.Errorf("GetTreeState was called %d times", got)
	}
	if accounts, err := e.w.Accounts(); err != nil || len(accounts) != 0 {
		t.Errorf("Accounts: %+v, %v", accounts, err)
	}

	// With the block before the activation mined, an account can begin at
	// the activation.
	e.srv.mine(1)
	account, err := e.w.CreateAccount(ctx, &AccountParams{Name: "first", Seed: testSeed})
	if err != nil || account.Birthday != 120 {
		t.Errorf("CreateAccount one block before the activation: %+v, %v", account, err)
	}
}

func TestCreateAccountBirthdays(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		ctx := context.Background()

		// No birthday: a fixed depth below the tip.
		account, err := e.w.CreateAccount(ctx, &AccountParams{Name: "new", Seed: testSeed})
		if err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
		if account.Birthday != 150-newAccountDepth {
			t.Errorf("default birthday: %d, want %d", account.Birthday, 150-newAccountDepth)
		}

		// A second account of the same seed, with a key source.
		second, err := e.w.CreateAccount(ctx, &AccountParams{
			Name: "second", Seed: testSeed, Birthday: 120, Restore: true, KeySource: "hardware",
		})
		if err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
		if second.Birthday != 120 || second.HDIndex != 1 || second.KeySource != "hardware" ||
			second.ID == account.ID {
			t.Errorf("second account: %+v", second)
		}

		// A birthday at the first height that has a tree state: the server
		// cannot serve the state before it, so it is built from the
		// block's parent.
		treeStates := e.srv.callCount("GetTreeState")
		third, err := e.w.CreateAccount(ctx, &AccountParams{Name: "old", Seed: testSeed, Birthday: 1})
		if err != nil {
			t.Fatalf("CreateAccount at activation: %v", err)
		}
		if third.Birthday != 1 {
			t.Errorf("birthday at activation: %d", third.Birthday)
		}
		if got := e.srv.callCount("GetTreeState"); got != treeStates {
			t.Errorf("GetTreeState was called for the activation height")
		}

		accounts, err := e.w.Accounts()
		if err != nil || len(accounts) != 3 {
			t.Fatalf("Accounts: %+v, %v", accounts, err)
		}

		// The wallet syncs all of them from the earliest birthday.
		e.account = third
		e.connect()
		e.waitSynced(150)
		e.assertFollows()
	})
}

func TestCreateAccountOnShortChain(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		// The chain is shorter than the default birthday depth, so the
		// birthday is the activation height.
		e := newEnv(t, kind, 30, nil)
		account, err := e.w.CreateAccount(context.Background(), &AccountParams{Name: "new", Seed: testSeed})
		if err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
		if account.Birthday != 1 {
			t.Errorf("birthday: %d, want 1", account.Birthday)
		}
		e.account = account
		e.connect()
		e.waitSynced(30)
		e.assertFollows()
	})
}

func TestCreateAccountErrors(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		ctx := context.Background()

		if _, err := e.w.CreateAccount(ctx, nil); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("nil parameters: %v", err)
		}
		if _, err := e.w.CreateAccount(ctx, &AccountParams{Seed: []byte{1, 2, 3}, Birthday: 100}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("short seed: %v", err)
		}
		if _, err := e.w.CreateAccountWithTreeState(nil, &walletrpc.TreeState{}, 0); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("nil parameters with a tree state: %v", err)
		}
		if _, err := e.w.CreateAccountWithTreeState(&AccountParams{Seed: testSeed}, nil, 0); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("nil tree state: %v", err)
		}

		// A birthday above the chain tip: the server has no state for it.
		_, err := e.w.CreateAccount(ctx, &AccountParams{Seed: testSeed, Birthday: 1000})
		if !isServerError(err) {
			t.Errorf("birthday above the tip: %v", err)
		}

		// The server fails each of the calls involved.
		for _, method := range []string{"GetLightdInfo", "GetLatestBlock", "GetTreeState"} {
			e.srv.set(func(s *fakeLwd) { s.fail[method] = status.Error(codes.Unavailable, "down") })
			_, err := e.w.CreateAccount(ctx, &AccountParams{Seed: testSeed})
			if !isServerError(err) {
				t.Errorf("CreateAccount while %s fails: %v", method, err)
			}
			e.srv.set(func(s *fakeLwd) { delete(s.fail, method) })
		}
		e.srv.set(func(s *fakeLwd) { s.fail["GetBlock"] = status.Error(codes.Unavailable, "down") })
		if _, err := e.w.CreateAccount(ctx, &AccountParams{Seed: testSeed, Birthday: 1}); !isServerError(err) {
			t.Errorf("CreateAccount at activation while GetBlock fails: %v", err)
		}
		e.srv.set(func(s *fakeLwd) { delete(s.fail, "GetBlock") })

		// A cancelled context.
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := e.w.CreateAccount(cancelled, &AccountParams{Seed: testSeed}); !isServerError(err) {
			t.Errorf("CreateAccount with a cancelled context: %v", err)
		}

		// A server on another chain. Its tree state is not asked for.
		e.srv.set(func(s *fakeLwd) { s.chainName = "main" })
		treeStates := e.srv.callCount("GetTreeState")
		if _, err := e.w.CreateAccount(ctx, &AccountParams{Seed: testSeed, Birthday: 100}); !errors.Is(err, ErrWrongNetwork) {
			t.Errorf("CreateAccount with a server on another chain: %v", err)
		}
		if got := e.srv.callCount("GetTreeState"); got != treeStates {
			t.Errorf("the tree state of a server on another chain was fetched")
		}
		e.srv.set(func(s *fakeLwd) { s.chainName = "test" })

		// A tree state of another chain.
		foreign := &walletrpc.TreeState{Network: "main", Height: 99, Hash: strings.Repeat("00", 32)}
		if _, err := e.w.CreateAccountWithTreeState(&AccountParams{Seed: testSeed}, foreign, 0); !errors.Is(err, ErrWrongNetwork) {
			t.Errorf("CreateAccountWithTreeState with a tree state of another chain: %v", err)
		}

		if accounts, err := e.w.Accounts(); err != nil || len(accounts) != 0 {
			t.Fatalf("accounts after failed creations: %+v, %v", accounts, err)
		}
	})
}

func TestCreateAccountWithTreeState(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		// The tree state comes from the caller, so the server is not
		// needed.
		state, err := e.srv.GetTreeState(context.Background(), &walletrpc.BlockID{Height: 99})
		if err != nil {
			t.Fatalf("GetTreeState: %v", err)
		}
		e.srv.stop()

		account, err := e.w.CreateAccountWithTreeState(&AccountParams{Name: "offline", Seed: testSeed}, state, 140)
		if err != nil {
			t.Fatalf("CreateAccountWithTreeState: %v", err)
		}
		if account.Birthday != 100 || account.Name != "offline" {
			t.Fatalf("account: %+v", account)
		}

		e.srv.restart(t)
		e.account = account
		e.connect()
		e.waitSynced(150)
		e.assertFollows()
	})
}

func TestAddressesAndSeeds(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e := newEnv(t, kind, 150, nil)
		account := e.createAccount(100)

		address, err := e.w.Address(account.ID)
		if err != nil || address == "" {
			t.Fatalf("Address: %q, %v", address, err)
		}
		if again, err := e.w.Address(account.ID); err != nil || again != address {
			t.Errorf("Address is not stable: %q, %v", again, err)
		}
		shielded, err := e.w.NewAddress(account.ID, ReceiversShielded)
		if err != nil || shielded == "" || shielded == address {
			t.Errorf("NewAddress(shielded): %q, %v", shielded, err)
		}
		all, err := e.w.NewAddress(account.ID, ReceiversAll)
		if err != nil || all == "" || all == address || all == shielded {
			t.Errorf("NewAddress(all): %q, %v", all, err)
		}

		if _, err := e.w.Address("no-such-account"); err == nil {
			t.Error("Address of an unknown account succeeded")
		}
		if _, err := e.w.NewAddress("no-such-account", ReceiversAll); err == nil {
			t.Error("NewAddress of an unknown account succeeded")
		}
		if _, err := e.w.TransparentAddresses("no-such-account"); err == nil {
			t.Error("TransparentAddresses of an unknown account succeeded")
		}

		if ok, err := e.w.IsSeedRelevant(testSeed); err != nil || !ok {
			t.Errorf("IsSeedRelevant(own seed): %t, %v", ok, err)
		}
		other := make([]byte, 32)
		other[0] = 99
		if ok, err := e.w.IsSeedRelevant(other); err != nil || ok {
			t.Errorf("IsSeedRelevant(other seed): %t, %v", ok, err)
		}
		if e.w.Network() != testNetwork {
			t.Errorf("Network: %+v", e.w.Network())
		}
	})
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open(nil, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Open(nil): %v", err)
	}
}

func TestConnectWithBadServerAddress(t *testing.T) {
	for _, server := range []string{"", "no-port", "host:port:extra"} {
		w := newWallet(&Config{Network: testNetwork, Server: server, Plaintext: true}, newFakeBackend(), nil)
		if _, err := w.Connect(context.Background()); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Connect with server %q: %v", server, err)
		}
		if _, err := w.CreateAccount(context.Background(), &AccountParams{Seed: testSeed}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("CreateAccount with server %q: %v", server, err)
		}
		// A failed Connect leaves the wallet usable.
		if got := w.SyncStatus(); got.State != SyncStopped {
			t.Errorf("status after a failed Connect: %+v", got)
		}
		if err := w.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
}
