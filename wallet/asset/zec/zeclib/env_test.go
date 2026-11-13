// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decred/base58"

	"github.com/bisoncraft/caribou/util"
)

// testNetwork is a regtest chain with every upgrade active from height 1,
// like the simnet harness.
var testNetwork = Regtest(AllUpgradesAt(1))

// testSeed derives the account of most tests. Its default address has every
// receiver type.
var testSeed = bytes.Repeat([]byte{8}, 32)

const waitTimeout = 20 * time.Second

// backendKind is a way of making a backend for a test. The lifecycle and
// sync tests run once for each kind. The fake is always present. The file
// backend_ffi_test.go adds the Rust backend when it is compiled in.
type backendKind struct {
	name string
	open func(t *testing.T) backend
}

var backendKinds = []backendKind{{
	name: "fake",
	open: func(*testing.T) backend { return newFakeBackend() },
}}

// forEachBackend runs a test once per backend kind.
func forEachBackend(t *testing.T, test func(t *testing.T, kind backendKind)) {
	t.Helper()
	for _, kind := range backendKinds {
		t.Run(kind.name, func(t *testing.T) { test(t, kind) })
	}
}

// testLogWriter writes log lines to the test log.
type testLogWriter struct {
	t *testing.T
	// mtx guards done, which is set when the test ends. Logging to a
	// finished test panics.
	mtx  sync.Mutex
	done bool
}

func (w *testLogWriter) Write(line []byte) (int, error) {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	if !w.done {
		w.t.Log(strings.TrimSuffix(string(line), "\n"))
	}
	return len(line), nil
}

// newTestLogger returns a logger that writes every level to the test log.
func newTestLogger(t *testing.T) util.Logger {
	w := &testLogWriter{t: t}
	t.Cleanup(func() {
		w.mtx.Lock()
		w.done = true
		w.mtx.Unlock()
	})
	return util.NewLogger("ZEC", util.LevelTrace, w)
}

// statusRecorder collects the sync statuses a wallet reports.
type statusRecorder struct {
	mtx      sync.Mutex
	cond     *sync.Cond
	statuses []SyncStatus
}

func newStatusRecorder() *statusRecorder {
	r := &statusRecorder{}
	r.cond = sync.NewCond(&r.mtx)
	return r
}

func (r *statusRecorder) record(s SyncStatus) {
	r.mtx.Lock()
	r.statuses = append(r.statuses, s)
	r.mtx.Unlock()
	r.cond.Broadcast()
}

// all returns the statuses recorded so far.
func (r *statusRecorder) all() []SyncStatus {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	return append([]SyncStatus(nil), r.statuses...)
}

// reset forgets the statuses recorded so far, so that a later wait is for a
// status that is reported from now on.
//
// A status is reported once, when it changes. A test therefore calls reset
// before it does what leads to the status it then waits for. Called after,
// reset can discard that very status, and the wait never ends.
func (r *statusRecorder) reset() {
	r.mtx.Lock()
	r.statuses = nil
	r.mtx.Unlock()
}

// wait blocks until a recorded status satisfies ok and returns it. It fails
// the test after waitTimeout.
func (r *statusRecorder) wait(t *testing.T, what string, ok func(SyncStatus) bool) SyncStatus {
	t.Helper()
	timer := time.AfterFunc(waitTimeout, r.cond.Broadcast)
	defer timer.Stop()
	deadline := time.Now().Add(waitTimeout)

	r.mtx.Lock()
	defer r.mtx.Unlock()
	checked := 0
	for {
		for ; checked < len(r.statuses); checked++ {
			if ok(r.statuses[checked]) {
				return r.statuses[checked]
			}
		}
		if time.Now().After(deadline) {
			var last SyncStatus
			if len(r.statuses) > 0 {
				last = r.statuses[len(r.statuses)-1]
			}
			t.Fatalf("timed out waiting for %s; last status: %+v", what, last)
		}
		r.cond.Wait()
	}
}

// env is a wallet wired to a fake lightwalletd server.
type env struct {
	t        *testing.T
	srv      *fakeLwd
	be       backend
	w        *Wallet
	statuses *statusRecorder
	account  *Account
}

// newEnv starts a server with a chain of the given height and opens a wallet
// against it. The wallet has no account and is not connected.
func newEnv(t *testing.T, kind backendKind, chainHeight uint32, adjust func(*Config)) *env {
	t.Helper()
	e := &env{
		t:        t,
		srv:      newFakeLwd(t, chainHeight),
		be:       kind.open(t),
		statuses: newStatusRecorder(),
	}
	cfg := &Config{
		Network:      testNetwork,
		Server:       e.srv.addr,
		Plaintext:    true,
		Logger:       newTestLogger(t),
		OnSyncStatus: e.statuses.record,
		PollInterval: 20 * time.Millisecond,
	}
	if adjust != nil {
		adjust(cfg)
	}
	e.w = newWallet(cfg, e.be, nil)
	e.w.tune = func(o *syncOptions) {
		o.minBackoff = 10 * time.Millisecond
		o.maxBackoff = 40 * time.Millisecond
	}
	t.Cleanup(func() {
		if err := e.w.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return e
}

// createAccount adds an account with the given birthday.
func (e *env) createAccount(birthday uint32) *Account {
	e.t.Helper()
	account, err := e.w.CreateAccount(context.Background(), &AccountParams{
		Name:     "test",
		Seed:     testSeed,
		Birthday: birthday,
	})
	if err != nil {
		e.t.Fatalf("CreateAccount: %v", err)
	}
	e.account = account
	return account
}

// connect starts syncing and returns a function that stops it and waits for
// it to stop.
func (e *env) connect() (stop func()) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	wg, err := e.w.Connect(ctx)
	if err != nil {
		cancel()
		e.t.Fatalf("Connect: %v", err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			waitGroup(e.t, wg)
		})
	}
	e.t.Cleanup(stop)
	return stop
}

// waitSynced blocks until the wallet reports that it is synced to height.
func (e *env) waitSynced(height uint32) SyncStatus {
	e.t.Helper()
	return e.statuses.wait(e.t, fmt.Sprintf("sync to height %d", height), func(s SyncStatus) bool {
		// A failed attempt leaves the state as it was, so a status can
		// say synced and carry an error.
		return s.State == SyncSynced && s.Err == nil && s.FullyScanned == height && s.ChainTip == height
	})
}

// assertFollows checks that the wallet's scanned blocks from its birthday to
// the server's tip are the server's blocks.
func (e *env) assertFollows() {
	e.t.Helper()
	tip := e.srv.height()
	for h := e.account.Birthday; h <= tip; h++ {
		got, err := e.be.blockHash(h)
		if err != nil {
			e.t.Fatalf("blockHash(%d): %v", h, err)
		}
		if want := e.srv.hashAt(h); !bytes.Equal(got, want) {
			e.t.Fatalf("block %d: wallet has %x, server has %x", h, got, want)
		}
	}
	if highest, ok, err := e.be.maxScannedHeight(); err != nil || !ok || highest != tip {
		e.t.Fatalf("max scanned height is %d (%t, %v), want %d", highest, ok, err, tip)
	}
}

// waitGroup waits for wg and fails the test if it takes too long.
func waitGroup(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("timed out waiting for the wallet to stop")
	}
}

// eventually polls ok until it returns true, failing the test after
// waitTimeout.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// p2pkhScript returns the script that pays a transparent pay-to-public-key-
// hash address. Fake addresses, which do not decode, get a placeholder.
func p2pkhScript(address string) []byte {
	decoded := base58.Decode(address)
	// Two version bytes, a 20-byte hash and a 4-byte checksum.
	if len(decoded) != 26 {
		return []byte{0x51}
	}
	script := []byte{0x76, 0xa9, 0x14}
	script = append(script, decoded[2:22]...)
	return append(script, 0x88, 0xac)
}
