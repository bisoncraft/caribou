//go:build !harness

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	"github.com/btcsuite/btcd/chaincfg"
)

func TestNativeWalletSettingsAndPeerConversion(t *testing.T) {
	cc := btcElectrumChainConfig(&chaincfg.MainNetParams)
	w := NewNativeWallet(cc, &asset.WalletConfig{Settings: map[string]string{
		"recoverywindow":     "12",
		"externalquietdepth": "3",
		"sweepinterval":      "4",
	}}, tLogger)
	if w.recoveryWindow != 12 || w.watchDepths != [2]int32{3, 4} {
		t.Fatalf("wallet settings = %d, %v", w.recoveryWindow, w.watchDepths)
	}
	peers := walletPeersToAsset([]wallet.Peer{
		{Addr: "default", Connected: true, Source: wallet.PeerDefault},
		{Addr: "user", Source: wallet.PeerUserAdded},
		{Addr: "found", Source: wallet.PeerSource(99)},
	})
	if len(peers) != 3 || peers[0].Source != asset.WalletDefault || peers[1].Source != asset.UserAdded || peers[2].Source != asset.Discovered {
		t.Fatalf("converted peers = %+v", peers)
	}
	if walletPeersToAsset(nil) != nil {
		t.Fatal("nil peers did not remain nil")
	}
}

func TestNativeWalletInvalidSettings(t *testing.T) {
	cc := btcElectrumChainConfig(&chaincfg.MainNetParams)
	w := NewNativeWallet(cc, &asset.WalletConfig{Settings: map[string]string{
		"recoverywindow":     "invalid",
		"externalquietdepth": "invalid",
		"sweepinterval":      "invalid",
	}}, tLogger)
	if w.recoveryWindow != cc.RecoveryWindow || w.watchDepths != [2]int32{} {
		t.Fatalf("wallet settings = %d, %v", w.recoveryWindow, w.watchDepths)
	}
}

// TestBlockNumberFromHeight guards the unmined-sentinel overflow: wtxmgr returns
// Block.Height == -1 for an unmined tx, and a raw uint64(-1) is ~1.8e19. That
// value used to be persisted as the tx's BlockNumber and corrupt history.
func TestBlockNumberFromHeight(t *testing.T) {
	tests := []struct {
		name string
		h    int32
		want uint64
	}{
		{"unmined sentinel", -1, 0},
		{"arbitrary negative", -1000, 0},
		{"genesis", 0, 0},
		{"height 1", 1, 1},
		{"typical height", 670, 670},
		{"max int32", math.MaxInt32, uint64(math.MaxInt32)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := blockNumberFromHeight(tt.h); got != tt.want {
				t.Fatalf("blockNumberFromHeight(%d) = %d, want %d", tt.h, got, tt.want)
			}
		})
	}
}

// TestBtcToSats guards the float-truncation bug: uint64(v*1e8) truncates because
// the product is not exactly representable (0.29 → 28999999), so history amounts
// were off by a satoshi for a large class of ordinary values.
func TestBtcToSats(t *testing.T) {
	tests := []struct {
		name string
		v    float64
		want uint64
	}{
		{"truncation case", 0.29, 29000000},
		{"another truncation case", 0.07, 7000000},
		{"whole coin", 1.0, 100000000},
		{"one satoshi", 0.00000001, 1},
		{"zero", 0.0, 0},
		{"negative guards to zero", -1.0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := btcToSats(tt.v); got != tt.want {
				t.Fatalf("btcToSats(%v) = %d, want %d", tt.v, got, tt.want)
			}
		})
	}
}

// TestScanFloor exercises the pending scan-floor register that keeps
// late-notified mined transactions (blocks below the scan cursor) reachable
// by the next history scan.
func TestScanFloor(t *testing.T) {
	w := &NativeWallet{}

	// No floor armed: the cursor passes through.
	if got, floor := w.peekScanFloor(100); got != 100 || floor != 0 {
		t.Fatalf("no-floor peek = (%d, %d), want (100, 0)", got, floor)
	}

	// Arming keeps the minimum.
	w.armScanFloor(90)
	w.armScanFloor(95) // higher: ignored
	w.armScanFloor(85) // lower: replaces
	got, floor := w.peekScanFloor(100)
	if got != 85 || floor != 85 {
		t.Fatalf("armed peek = (%d, %d), want (85, 85)", got, floor)
	}
	// Peeking does not consume: the scan has not run yet.
	if got, _ := w.peekScanFloor(100); got != 85 {
		t.Fatalf("second peek = %d, want 85; peek must not consume", got)
	}
	// Retired once the scan that used it completed.
	w.clearScanFloor(floor)
	if got, _ := w.peekScanFloor(100); got != 100 {
		t.Fatalf("post-clear peek = %d, want 100", got)
	}

	// A floor above the cursor never raises it, but a scan from the cursor
	// covers those blocks, so it is still retired.
	w.armScanFloor(150)
	got, floor = w.peekScanFloor(100)
	if got != 100 || floor != 150 {
		t.Fatalf("higher-floor peek = (%d, %d), want (100, 150)", got, floor)
	}
	w.clearScanFloor(floor)
	if armed := w.scanFloor.Load(); armed != 0 {
		t.Fatalf("floor still armed at %d after a scan that covered it", armed)
	}

	// A never-scanned wallet (cursor 0) already scans from the beginning; the
	// floor must not turn that into a partial scan.
	w.armScanFloor(85)
	if got, _ := w.peekScanFloor(0); got != 0 {
		t.Fatalf("initial-scan peek = %d, want 0", got)
	}
}

// TestScanFloorSurvivesFailedScan covers the paths where the scan the floor was
// peeked for never reaches the blocks it names. scanNewTransactions used to
// consume the register up front, so a ListSinceBlock error - or a context
// cancellation partway through - returned with the cursor still at its higher
// value and the override gone. Nothing rescans below the cursor after that, so
// the late-notified mined transaction the floor was armed for is dropped from tx
// history for the life of the process.
//
// Driven through the register rather than through scanNewTransactions: btcw is a
// concrete *wallet.Wallet, so ListSinceBlock cannot be made to fail without an
// interface seam that exists for nothing else.
func TestScanFloorSurvivesFailedScan(t *testing.T) {
	w := &NativeWallet{}

	// A late-notified tx in block 85 arms the floor under a cursor at 100.
	w.armScanFloor(85)
	from, floor := w.peekScanFloor(100)
	if from != 85 {
		t.Fatalf("scan start = %d, want 85", from)
	}

	// The scan fails, so clearScanFloor is never reached. The next one must
	// still reach block 85.
	if from, _ := w.peekScanFloor(100); from != 85 {
		t.Fatalf("scan start after a failed scan = %d, want 85", from)
	}

	// The retry succeeds and retires the floor.
	w.clearScanFloor(floor)
	if from, _ := w.peekScanFloor(100); from != 100 {
		t.Fatalf("scan start after a completed scan = %d, want 100", from)
	}

	// A floor armed while a scan is running names blocks below where that scan
	// started, so completing it must not retire the newer, lower value.
	w.armScanFloor(85)
	_, floor = w.peekScanFloor(100)
	w.armScanFloor(70) // a second late notification, mid-scan
	w.clearScanFloor(floor)
	if from, _ := w.peekScanFloor(100); from != 70 {
		t.Fatalf("scan start = %d, want 70; a floor armed mid-scan was retired by it", from)
	}
}

// TestNativeWalletChainIdentity guards the chain identity stamped on master
// transaction database records. This wallet type serves the clone assets
// (ltc, dash) too, so the IDs must come from ChainConfig.Info — records
// stamped with the btc package's own BipID (0) are rejected by every
// clone's network-scoped database.
func TestNativeWalletChainIdentity(t *testing.T) {
	const cloneID = 2 // ltc
	cc := ChainConfig{
		ChainParams: &chaincfg.MainNetParams,
		Info: &asset.WalletInfo{Network: &asset.Network{
			ID:            cloneID,
			NativeAssetID: cloneID,
		}},
	}
	log := util.StdOutLogger("TEST", util.LevelInfo)
	w := NewNativeWallet(cc, &asset.WalletConfig{}, log)
	if w.networkID != cloneID || w.assetID != cloneID {
		t.Fatalf("wallet identity = network %d, asset %d; want %d, %d",
			w.networkID, w.assetID, cloneID, cloneID)
	}
}

// fakeTxStore stands in for the wallet's transaction store under a history
// pass: receives are stored as they arrive, and a pass records what its
// snapshot (ListSinceBlock in syncTxHistory) held.
type fakeTxStore struct {
	mtx      sync.Mutex
	stored   []string
	recorded map[string]bool
	passes   int
	reruns   int
}

func newFakeTxStore() *fakeTxStore {
	return &fakeTxStore{recorded: map[string]bool{}}
}

func (s *fakeTxStore) store(txid string) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.stored = append(s.stored, txid)
}

func (s *fakeTxStore) snapshot() []string {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return append([]string(nil), s.stored...)
}

func (s *fakeTxStore) record(snap []string, rerun bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	for _, txid := range snap {
		s.recorded[txid] = true
	}
	s.passes++
	if rerun {
		s.reruns++
	}
}

func (s *fakeTxStore) unrecorded() []string {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	var missing []string
	for _, txid := range s.stored {
		if !s.recorded[txid] {
			missing = append(missing, txid)
		}
	}
	return missing
}

func (s *fakeTxStore) counts() (passes, reruns int) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.passes, s.reruns
}

// TestTxHistoryRerunAfterSnapshot: three receives delivered together, as from
// one electrum status diff, each request a history pass. The first pass is
// held after its snapshot, which holds only the first receive, while the other
// two are stored and make their requests. Those requests return at once, and
// the held pass records both receives in one rerun once released, with no
// further request such as a new block's. The guard is then free for the next
// request.
func TestTxHistoryRerunAfterSnapshot(t *testing.T) {
	var g rerunGuard
	s := newFakeTxStore()
	snapped, release := make(chan struct{}), make(chan struct{})
	var hold sync.Once
	pass := func(rerun bool) {
		snap := s.snapshot()
		hold.Do(func() {
			close(snapped)
			<-release
		})
		s.record(snap, rerun)
	}

	s.store("rx1")
	first := make(chan struct{})
	go func() {
		defer close(first)
		g.do(tCtx, pass)
	}()
	<-snapped
	for _, txid := range []string{"rx2", "rx3"} {
		s.store(txid)
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			g.do(tCtx, pass)
		}()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatalf("the request for %s blocked behind the running pass", txid)
		}
	}
	close(release)
	<-first

	if missing := s.unrecorded(); len(missing) > 0 {
		t.Fatalf("receives %v unrecorded after the held pass finished", missing)
	}
	if passes, reruns := s.counts(); passes != 2 || reruns != 1 {
		t.Fatalf("%d passes, %d reruns; want the first pass and one rerun", passes, reruns)
	}

	s.store("rx4")
	g.do(tCtx, pass)
	if missing := s.unrecorded(); len(missing) > 0 {
		t.Fatalf("receive %v unrecorded by a request made after the guard was released", missing)
	}
}

// TestTxHistoryRequestsNotLost: receives stored and requested from many
// goroutines at once, as blockLoop spawns one per notification, are all
// recorded by the time every request has returned, and no two passes ever
// run at once. A request that lands as a pass finishes is either seen by that
// pass or runs its own. The interleavings that break this, a request recorded
// just after the running pass checked for one and before it released the
// guard, or a release that clears a later owner's claim, are narrow, so a
// guard that allows them fails here in many runs, not every run.
func TestTxHistoryRequestsNotLost(t *testing.T) {
	for round := range 5000 {
		var g rerunGuard
		s := newFakeTxStore()
		var active, overlaps atomic.Int32
		pass := func(rerun bool) {
			if active.Add(1) > 1 {
				overlaps.Add(1)
			}
			s.record(s.snapshot(), rerun)
			active.Add(-1)
		}
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Go(func() {
				s.store(fmt.Sprintf("rx%d", i))
				g.do(tCtx, pass)
			})
		}
		wg.Wait()
		if missing := s.unrecorded(); len(missing) > 0 {
			t.Fatalf("round %d: receives %v unrecorded after every request returned", round, missing)
		}
		if n := overlaps.Load(); n > 0 {
			t.Fatalf("round %d: %d passes started while another was running", round, n)
		}
	}
}

// TestTxHistoryRerunSkippedAfterCancel: a request made while a pass is running
// gets no rerun once the context is done, and the guard is free again for the
// next caller.
func TestTxHistoryRerunSkippedAfterCancel(t *testing.T) {
	var g rerunGuard
	ctx, cancel := context.WithCancel(tCtx)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	var passes atomic.Int32
	var hold sync.Once
	pass := func(bool) {
		passes.Add(1)
		hold.Do(func() {
			close(started)
			<-release
		})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		g.do(ctx, pass)
	}()
	<-started
	g.do(ctx, pass)
	cancel()
	close(release)
	<-done
	if n := passes.Load(); n != 1 {
		t.Fatalf("%d passes after cancellation, want 1", n)
	}
	g.do(tCtx, pass)
	if n := passes.Load(); n != 2 {
		t.Fatalf("%d passes after a cancelled rerun, want 2; the guard was not released", n)
	}
}

// TestTxHistoryCancelledContextRunsNoPass: a request made with a context that
// is already done runs no pass and leaves the guard free.
func TestTxHistoryCancelledContextRunsNoPass(t *testing.T) {
	var g rerunGuard
	ctx, cancel := context.WithCancel(tCtx)
	cancel()
	g.do(ctx, func(bool) { t.Error("pass ran under a cancelled context") })
	ran := false
	g.do(tCtx, func(bool) { ran = true })
	if !ran {
		t.Fatal("the guard was left busy by a cancelled request")
	}
}

// TestTxHistoryGuardReleasedAfterPanic: a pass that panics, with the panic
// recovered by the caller, leaves the guard free for the next request, and a
// request recorded during the failed pass does not carry over into it as a
// rerun.
func TestTxHistoryGuardReleasedAfterPanic(t *testing.T) {
	var g rerunGuard
	recovered := func() (r any) {
		defer func() { r = recover() }()
		g.do(tCtx, func(bool) {
			g.do(tCtx, func(bool) { t.Error("a request made during a pass ran its own pass") })
			panic("pass failed")
		})
		return nil
	}()
	if recovered != "pass failed" {
		t.Fatalf("recovered %v, want the pass's panic", recovered)
	}
	var reruns []bool
	g.do(tCtx, func(rerun bool) { reruns = append(reruns, rerun) })
	if len(reruns) != 1 || reruns[0] {
		t.Fatalf("passes after the panic %v, want one first pass", reruns)
	}
}
