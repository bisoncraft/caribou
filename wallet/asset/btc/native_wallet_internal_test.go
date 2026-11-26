//go:build !harness

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
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
	if from, floor := w.takeScanFloor(100); from != 100 || floor != 0 {
		t.Fatalf("no-floor take = (%d, %d), want (100, 0)", from, floor)
	}

	// Arming keeps the minimum, and a height of 0 names no block.
	w.armScanFloor(90)
	w.armScanFloor(95) // higher: ignored
	w.armScanFloor(85) // lower: replaces
	w.armScanFloor(0)  // ignored
	from, floor := w.takeScanFloor(100)
	if from != 85 || floor != 85 {
		t.Fatalf("armed take = (%d, %d), want (85, 85)", from, floor)
	}
	// Taking empties the register, so a height armed from here on is left
	// for the next scan.
	if armed := w.scanFloor.Load(); armed != 0 {
		t.Fatalf("floor still armed at %d after a scan claimed it", armed)
	}

	// A floor above the cursor never raises the start height.
	w.armScanFloor(150)
	if from, floor := w.takeScanFloor(100); from != 100 || floor != 150 {
		t.Fatalf("higher-floor take = (%d, %d), want (100, 150)", from, floor)
	}

	// A never-scanned wallet (cursor 0) already scans from the beginning; the
	// floor must not turn that into a partial scan.
	w.armScanFloor(85)
	if from, _ := w.takeScanFloor(0); from != 0 {
		t.Fatalf("initial-scan take = %d, want 0", from)
	}
}

// snapshotHookDB runs afterView each time a read transaction on the wallet
// database completes. With the wallet not started, a history scan's only read
// of that database is its ListSinceBlock snapshot, so the hook runs right
// after the scan takes its snapshot and before it records anything or
// advances its cursor.
type snapshotHookDB struct {
	walletdb.DB
	afterView func()
}

func (d *snapshotHookDB) View(f func(walletdb.ReadTx) error, reset func()) error {
	err := d.DB.View(f, reset)
	if d.afterView != nil {
		d.afterView()
	}
	return err
}

// newScanFloorWallet returns an offline wallet, opened over a snapshotHookDB
// and not started, whose history scans run directly against its own
// transaction store, with the scan cursor at cursor.
func newScanFloorWallet(t *testing.T, cursor uint64) (*NativeWallet, *snapshotHookDB) {
	t.Helper()
	cc := btcElectrumChainConfig(&chaincfg.RegressionNetParams)
	dir := t.TempDir()
	seed, err := hdkeychain.GenerateSeed(hdkeychain.RecommendedSeedLen)
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateElectrumWallet(cc, []byte("pass"), seed, time.Now(), dir, tLogger, 0, 0, wallet.WithScryptOptions(&waddrmgr.FastScryptOptions)); err != nil {
		t.Fatal(err)
	}
	bdb, err := walletdb.Open("bdb", filepath.Join(dir, wallet.WalletDBName), true, dbTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bdb.Close() })
	db := &snapshotHookDB{DB: bdb}
	btcw, err := wallet.Open(db, []byte(wallet.InsecurePubPassphrase), cc.NetParams, 0)
	if err != nil {
		t.Fatal(err)
	}
	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), tLogger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	w := NewNativeWallet(cc, &asset.WalletConfig{}, tLogger)
	w.btcw = btcw
	w.txdb = master.NetworkTxDB(BipID)
	w.emit = asset.NewWalletEmitter(make(chan asset.WalletNotification, 64), BipID, tLogger)
	w.receiveTxLastQuery.Store(cursor)
	return w, db
}

// storeMinedCredit stores a transaction mined at height that pays the wallet
// one output, as the chain backend's relevant-transaction notification does,
// and returns its txid. Each seed gives a distinct transaction.
func storeMinedCredit(t *testing.T, w *NativeWallet, seed byte, height int32) string {
	t.Helper()
	msgTx := wire.NewMsgTx(2)
	msgTx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{seed}}})
	pkScript := append([]byte{txscript.OP_0, txscript.OP_DATA_20}, bytes.Repeat([]byte{seed}, 20)...)
	msgTx.AddTxOut(&wire.TxOut{Value: 100_000, PkScript: pkScript})
	rec, err := wtxmgr.NewTxRecordFromMsgTx("btc", msgTx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	block := &wtxmgr.BlockMeta{
		Block: wtxmgr.Block{Hash: chainhash.Hash{byte(height), byte(height >> 8), 0xb1}, Height: height},
		Time:  time.Now(),
	}
	err = walletdb.Update(w.btcw.Database(), func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket([]byte("wtxmgr"))
		if err := w.btcw.TxStore.InsertTx(ns, rec, block); err != nil {
			return err
		}
		return w.btcw.TxStore.AddCredit(ns, rec, block, 0, false)
	})
	if err != nil {
		t.Fatalf("storing transaction mined at %d: %v", height, err)
	}
	return rec.Hash.String()
}

// recorded reports whether the wallet's transaction history holds txid.
func recorded(w *NativeWallet, txid string) bool {
	wt, err := w.txdb.GetTx(txid)
	return err == nil && wt != nil
}

// TestScanFloorArmedAfterSnapshot: while a history scan runs from a floor
// armed earlier, a transaction mined below the next scan's normal lookback is
// stored after the scan's snapshot and arms the floor at its height. The scan
// cannot record it, so its height must outlive that scan, whether it equals
// the floor being scanned, lies above it or below it, and the next scan
// records it and retires the floor.
func TestScanFloorArmedAfterSnapshot(t *testing.T) {
	const cursor = 100 // the next scan's normal start is 97
	const earlier = 50
	for _, tt := range []struct {
		name string
		late int32
	}{
		{"same height", earlier},
		{"higher", 60},
		{"lower", 40},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, db := newScanFloorWallet(t, cursor)
			first := storeMinedCredit(t, w, 1, earlier)
			w.armScanFloor(earlier)
			var late string
			db.afterView = func() {
				if late == "" {
					late = storeMinedCredit(t, w, 2, tt.late)
					w.armScanFloor(uint64(tt.late))
				}
			}
			w.scanNewTransactions(tCtx, cursor)
			db.afterView = nil
			if late == "" {
				t.Fatal("the scan's snapshot held no transaction")
			}
			if !recorded(w, first) || recorded(w, late) {
				t.Fatalf("after the first scan: earlier transaction recorded = %t, late one = %t; want true, false",
					recorded(w, first), recorded(w, late))
			}

			w.scanNewTransactions(tCtx, cursor)
			if !recorded(w, late) {
				t.Fatalf("transaction mined at %d, stored after the first scan's snapshot, not recorded by the next scan", tt.late)
			}
			if armed := w.scanFloor.Load(); armed != 0 {
				t.Fatalf("floor still armed at %d after the scan that reached it", armed)
			}
		})
	}
}

// TestScanFloorRestoredAfterStoppedScan: a scan whose context ends after its
// snapshot records nothing and leaves the cursor where it was, so the floor it
// claimed goes back, merged with any height armed meanwhile for a transaction
// stored after the snapshot. The next scan records both the transaction the
// stopped scan held and the late one, including when the stopped scan had
// claimed no floor at all.
func TestScanFloorRestoredAfterStoppedScan(t *testing.T) {
	const cursor = 100 // the next scan's normal start is 97
	for _, tt := range []struct {
		name       string
		floor      uint64 // armed before the scan, 0 for none
		held, late int32
	}{
		{"same height", 50, 50, 50},
		{"higher", 50, 50, 60},
		{"lower", 50, 50, 40},
		{"no floor claimed", 0, 98, 60},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, db := newScanFloorWallet(t, cursor)
			held := storeMinedCredit(t, w, 1, tt.held)
			w.armScanFloor(tt.floor)
			ctx, cancel := context.WithCancel(tCtx)
			defer cancel()
			var late string
			db.afterView = func() {
				if late == "" {
					late = storeMinedCredit(t, w, 2, tt.late)
					w.armScanFloor(uint64(tt.late))
					cancel()
				}
			}
			// A tip above the cursor, so a stopped scan that advanced the
			// cursor anyway would show.
			w.scanNewTransactions(ctx, cursor+1)
			db.afterView = nil
			if late == "" {
				t.Fatal("the scan took no snapshot")
			}
			if got := w.receiveTxLastQuery.Load(); got != cursor || recorded(w, held) {
				t.Fatalf("stopped scan: cursor = %d, held transaction recorded = %t; want %d, false",
					got, recorded(w, held), cursor)
			}

			w.scanNewTransactions(tCtx, cursor)
			for _, tx := range []struct {
				what, txid string
				height     int32
			}{
				{"held by the stopped scan's snapshot", held, tt.held},
				{"stored after that snapshot", late, tt.late},
			} {
				if !recorded(w, tx.txid) {
					t.Errorf("transaction mined at %d, %s, not recorded by the next scan", tx.height, tx.what)
				}
			}
		})
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
