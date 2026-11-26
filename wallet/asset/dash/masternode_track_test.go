// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package dash

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/dash/masternode"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
)

// vectorRaw is a transaction Dash Core made, from the masternode test data.
func vectorRaw(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("masternode", "testdata", "dash_special_txs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var all []struct{ Name, Hex string }
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	for _, v := range all {
		if v.Name == name {
			raw, err := hex.DecodeString(v.Hex)
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}
	}
	t.Fatalf("no vector %s", name)
	return nil
}

// fakeSource is a txSource.
type fakeSource struct {
	res    *wallet.GetTransactionsResult
	ch     chan *wallet.TransactionNotifications
	synced int32
	from   atomic.Int32 // the height Transactions was asked from
	used   atomic.Int32 // how many times Used was asked
	mu     sync.Mutex
	has    map[chainhash.Hash]bool
}

func newFakeSource(res *wallet.GetTransactionsResult) *fakeSource {
	return &fakeSource{res: res, ch: make(chan *wallet.TransactionNotifications)}
}

func (s *fakeSource) Notifications() (<-chan *wallet.TransactionNotifications, func()) {
	return s.ch, func() { close(s.ch) }
}

func (s *fakeSource) Transactions(height int32, _ <-chan struct{}) (*wallet.GetTransactionsResult, error) {
	s.from.Store(height)
	return s.res, nil
}

func (s *fakeSource) Synced() int32 { return s.synced }

func (s *fakeSource) Used() (bool, error) {
	s.used.Add(1)
	return len(s.res.MinedTransactions)+len(s.res.UnminedTransactions) > 0, nil
}

func (s *fakeSource) Has(id chainhash.Hash) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if has, ok := s.has[id]; ok {
		return has, nil
	}
	return true, nil // the wallet has what it was not told to have dropped
}

// switchKeys derives no key until it is switched on, as a wallet that is locked.
type switchKeys struct {
	on    atomic.Bool
	calls atomic.Int32
}

func (k *switchKeys) DeriveFromRootPath(p []uint32) (*btcec.PrivateKey, error) {
	k.calls.Add(1)
	if !k.on.Load() {
		return nil, errors.New("locked")
	}
	return seqKeys{}.DeriveFromRootPath(p)
}

// trackingManager is a Manager that has the owner key of the real registration
// vector's first share cached, and has not found the registration.
func trackingManager(t *testing.T, chain masternode.Chain, keys masternode.KeyDeriver) (*masternode.Manager, *masternode.Store) {
	t.Helper()
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(vectorRaw(t, "proregtx"))); err != nil {
		t.Fatal(err)
	}
	mn, err := masternode.RegistrationMN(tx, 798)
	if err != nil {
		t.Fatal(err)
	}
	store := newStore(t)
	if err := store.Save(masternode.Update{OwnerKeyIDs: map[uint32][20]byte{1: mn.Shares[0].KeyIDOwner}}); err != nil {
		t.Fatal(err)
	}
	m, err := masternode.NewManager(chain, store, keys, 1, RegressionNetParams)
	if err != nil {
		t.Fatal(err)
	}
	return m, store
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestProviderTxs(t *testing.T) {
	reg, dis := vectorRaw(t, "proregtx"), vectorRaw(t, "prodistx")
	other := vectorRaw(t, "coinbase")
	mined := []wallet.Block{{Height: 798, Transactions: []wallet.TransactionSummary{
		{Transaction: reg}, {Transaction: other},
	}}}
	unmined := []wallet.TransactionSummary{{Transaction: dis}, {Transaction: []byte{1}}}
	got := providerTxs(mined, unmined)
	if len(got) != 2 || !bytes.Equal(got[0].raw, reg) || got[0].height != 798 || !bytes.Equal(got[1].raw, dis) || got[1].height != 0 {
		t.Fatalf("%+v", got)
	}
}

// The wallet's registrations are given to the Manager, those it has and those
// that arrive, and a Refresh is asked for.
func TestTrackLoopGivesTheManagerTheWalletsTransactions(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	chain := &chainStub{}
	// A Discover has run, so only the tracking asks for the Refresh.
	mgr, store := trackingManager(t, chain, seqKeys{})
	if err := store.Save(masternode.Update{Cursor: &masternode.Cursor{Height: 910}}); err != nil {
		t.Fatal(err)
	}
	reg := vectorRaw(t, "proregtx")
	src := newFakeSource(&wallet.GetTransactionsResult{
		MinedTransactions: []wallet.Block{{Height: 798, Transactions: []wallet.TransactionSummary{
			{Transaction: reg}, {Transaction: vectorRaw(t, "coinbase")},
		}}},
	})

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dw.trackLoop(ctx, mgr, src)
	}()
	defer func() { cancel(); wg.Wait() }()

	tracked := func() int {
		snap, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		return len(snap.Tracked)
	}
	waitFor(t, "the registration to be tracked", func() bool { return tracked() == 1 })
	select {
	case <-dw.refresh:
	case <-time.After(time.Second):
		t.Fatal("no Refresh was asked for")
	}

	// A dissolution that arrives later is tracked too. This vector's is of a
	// masternode the wallet does not hold, so it is only kept.
	src.ch <- &wallet.TransactionNotifications{UnminedTransactions: []wallet.TransactionSummary{
		{Transaction: vectorRaw(t, "prodistx")},
	}}
	waitFor(t, "the dissolution to be tracked", func() bool { return tracked() == 2 })
}

// A registration of another wallet's is not kept, and not asked for again.
func TestTrackLoopIgnoresOthers(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	keys := &switchKeys{}
	keys.on.Store(true)
	store := newStore(t)
	mgr, err := masternode.NewManager(&chainStub{}, store, keys, 1, RegressionNetParams)
	if err != nil {
		t.Fatal(err)
	}
	src := newFakeSource(&wallet.GetTransactionsResult{
		MinedTransactions: []wallet.Block{{Height: 798, Transactions: []wallet.TransactionSummary{{Transaction: vectorRaw(t, "proregtx")}}}},
	})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dw.trackLoop(ctx, mgr, src)
	}()
	defer func() { cancel(); wg.Wait() }()

	// Looking for the owner keys in the registration derives every one the gap
	// reaches.
	waitFor(t, "the owner keys to be looked for", func() bool { return keys.calls.Load() >= 100 })
	for range 20 {
		snap, err := store.Load()
		if err != nil || len(snap.Tracked) != 0 {
			t.Fatalf("tracked what is not the wallet's: %v, %d", err, len(snap.Tracked))
		}
		time.Sleep(5 * time.Millisecond)
	}
	// It is not asked for again after an unlock.
	calls := keys.calls.Load()
	dw.requestRetryTrack()
	time.Sleep(50 * time.Millisecond)
	if got := keys.calls.Load(); got != calls {
		t.Fatalf("a registration that is not the wallet's was looked at again: %d keys", got-calls)
	}
}

// A registration that needs a key that is not cached waits for the unlock.
func TestTrackLoopWaitsForTheUnlock(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	keys := &switchKeys{}
	mgr, store := trackingManager(t, &chainStub{}, keys)
	src := newFakeSource(&wallet.GetTransactionsResult{
		MinedTransactions: []wallet.Block{{Height: 798, Transactions: []wallet.TransactionSummary{{Transaction: vectorRaw(t, "proregtx")}}}},
	})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dw.trackLoop(ctx, mgr, src)
	}()
	defer func() { cancel(); wg.Wait() }()

	waitFor(t, "a key to be asked for", func() bool { return keys.calls.Load() > 0 })
	snap, err := store.Load()
	if err != nil || len(snap.Tracked) != 0 {
		t.Fatalf("tracked while locked: %v, %d", err, len(snap.Tracked))
	}

	keys.on.Store(true)
	dw.requestRetryTrack()
	waitFor(t, "the registration to be tracked once unlocked", func() bool {
		snap, err := store.Load()
		return err == nil && len(snap.Tracked) == 1
	})
}

// Discover runs once, when the wallet has the keys it needs, and not before.
func TestDiscoverWaitsForKeysAndRunsOnce(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	chain := &chainStub{header: &wire.BlockHeader{}}
	mgr, _ := trackingManager(t, chain, seqKeys{})
	if need, err := mgr.NeedsKeys(); err != nil || !need {
		t.Fatalf("needs keys: %v %v", need, err)
	}

	if discovered(dw, t, mgr) || chain.tips.Load() != 0 {
		t.Fatalf("discovered while locked without keys: %d", chain.tips.Load())
	}
	if err := mgr.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	if !discovered(dw, t, mgr) || chain.tips.Load() == 0 {
		t.Fatalf("did not discover with the keys: %d", chain.tips.Load())
	}
	// A Refresh follows, to prove what it found.
	select {
	case <-dw.refresh:
	default:
		t.Fatal("no Refresh was asked for")
	}
}

// A server that will not return the history is a warning, not a failure, and
// is not asked again.
func TestDiscoverReportsAServerThatRefuses(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	chain := &chainStub{header: &wire.BlockHeader{}}
	mgr, _ := trackingManager(t, chain, seqKeys{})
	if err := mgr.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	if !discovered(dw, t, mgr) {
		t.Fatal("a refused discovery is retried")
	}
	if done, err := mgr.Discovered(); err != nil || done {
		t.Fatalf("a refused discovery counts as done: %v %v", done, err)
	}
}

// An unlock caches the keys, and asks the track loop to try again what waited
// for them, and for a Refresh.
func TestAfterUnlockAsksForARetry(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, _ := trackingManager(t, &chainStub{}, seqKeys{})
	dw.mn.Store(mgr)
	dw.afterUnlock()
	for name, c := range map[string]chan struct{}{"retry": dw.retryTrack, "refresh": dw.refresh} {
		select {
		case <-c:
		default:
			t.Errorf("no %s was asked for", name)
		}
	}
}

// A Discover that has read the history is not run again.
func TestDiscoverIsNotRepeated(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	chain := &chainStub{}
	mgr, store := trackingManager(t, chain, seqKeys{})
	if err := store.Save(masternode.Update{Cursor: &masternode.Cursor{Height: 910}}); err != nil {
		t.Fatal(err)
	}
	if !discovered(dw, t, mgr) || chain.tips.Load() != 0 {
		t.Fatalf("discovered again: %d", chain.tips.Load())
	}
}

func rawTxID(t *testing.T, raw []byte) chainhash.Hash {
	t.Helper()
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	return tx.TxHash()
}

// A registration the wallet gave and no longer has, because another transaction
// spent its inputs, is untracked, so the server is not asked for it on every
// block. The wallet does not say what it dropped: a notification lists what it
// still has unmined.
func TestTrackLoopUntracksWhatTheWalletDropped(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, store := trackingManager(t, &chainStub{}, seqKeys{})
	if err := store.Save(masternode.Update{Cursor: &masternode.Cursor{Height: 910}}); err != nil {
		t.Fatal(err)
	}
	reg := vectorRaw(t, "proregtx")
	id := rawTxID(t, reg)
	tracked := func() int {
		snap, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		return len(snap.Tracked)
	}
	start := func(src *fakeSource) func() {
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			dw.trackLoop(ctx, mgr, src)
		}()
		return func() { cancel(); wg.Wait() }
	}

	// While running: the wallet has it, then drops it, and the next
	// notification does not list it.
	src := newFakeSource(&wallet.GetTransactionsResult{
		UnminedTransactions: []wallet.TransactionSummary{{Transaction: reg}},
	})
	stop := start(src)
	waitFor(t, "the registration to be tracked", func() bool { return tracked() == 1 })
	src.ch <- &wallet.TransactionNotifications{}
	time.Sleep(50 * time.Millisecond)
	if tracked() != 1 {
		t.Fatal("untracked a transaction the wallet still has")
	}
	src.mu.Lock()
	src.has = map[chainhash.Hash]bool{id: false}
	src.mu.Unlock()
	src.ch <- &wallet.TransactionNotifications{}
	waitFor(t, "the dropped registration to be untracked", func() bool { return tracked() == 0 })
	stop()

	// At start: tracked by an earlier session, and not in the wallet.
	if err := mgr.TrackWallet(reg, 0); err != nil {
		t.Fatal(err)
	}
	src = newFakeSource(&wallet.GetTransactionsResult{})
	src.has = map[chainhash.Hash]bool{id: false}
	stop = start(src)
	defer stop()
	waitFor(t, "the dropped registration to be untracked at start", func() bool { return tracked() == 0 })
}

// A registration that is tracked but that the wallet never had, one put back
// after a reorg or added by id, is not untracked for being absent from it.
func TestTrackLoopKeepsWhatTheWalletNeverHad(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, store := trackingManager(t, &chainStub{}, seqKeys{})
	if err := store.Save(masternode.Update{Cursor: &masternode.Cursor{Height: 910}}); err != nil {
		t.Fatal(err)
	}
	reg := vectorRaw(t, "proregtx")
	if err := store.Save(masternode.Update{Track: [][]byte{reg}}); err != nil {
		t.Fatal(err)
	}
	src := newFakeSource(&wallet.GetTransactionsResult{})
	src.has = map[chainhash.Hash]bool{rawTxID(t, reg): false}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dw.trackLoop(ctx, mgr, src)
	}()
	defer func() { cancel(); wg.Wait() }()
	time.Sleep(50 * time.Millisecond)
	src.ch <- &wallet.TransactionNotifications{}
	time.Sleep(50 * time.Millisecond)
	snap, err := store.Load()
	if err != nil || len(snap.Tracked) != 1 {
		t.Fatalf("%v, %d tracked", err, len(snap.Tracked))
	}
}

// A restart reads only the wallet's transactions since the height it read
// through, less a margin for a replaced block.
func TestTrackLoopReadsFromWhereItStopped(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, store := trackingManager(t, &chainStub{}, seqKeys{})
	if err := store.Save(masternode.Update{Cursor: &masternode.Cursor{Height: 910}}); err != nil {
		t.Fatal(err)
	}
	run := func(src *fakeSource) {
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			dw.trackLoop(ctx, mgr, src)
		}()
		defer func() { cancel(); wg.Wait() }()
		waitFor(t, "the height to be saved", func() bool {
			snap, err := store.Load()
			return err == nil && snap.TxRead == src.synced
		})
	}
	first := newFakeSource(&wallet.GetTransactionsResult{})
	first.synced = 5000
	run(first)
	if got := first.from.Load(); got != 0 {
		t.Fatalf("first read from %d", got)
	}
	second := newFakeSource(&wallet.GetTransactionsResult{})
	second.synced = 5010
	run(second)
	if got := second.from.Load(); got != 5000-txReadMargin {
		t.Fatalf("second read from %d, want %d", got, 5000-txReadMargin)
	}
}

// A Discover that failed for a reason that may pass, such as headers that are
// not there yet, is not taken for one that ran: the loop tries it again.
func TestDiscoverIsTriedAgainAfterATransientFailure(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, _ := trackingManager(t, &chainStub{}, seqKeys{}) // no header at the tip
	if err := mgr.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	if discovered(dw, t, mgr) {
		t.Fatal("a Discover that could not read the tip was taken for done")
	}
	if done, err := mgr.Discovered(); err != nil || done {
		t.Fatalf("%v %v", done, err)
	}
}

// A transaction that could not be tracked holds back the height saved, so a
// restart gives it again.
func TestTrackLoopHoldsTheHeightForATransactionItCouldNotTrack(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, store := trackingManager(t, &chainStub{}, seqKeys{})
	if err := store.Save(masternode.Update{Cursor: &masternode.Cursor{Height: 910}}); err != nil {
		t.Fatal(err)
	}
	// It has the type of a registration and is not one.
	bad := []byte{0x03, 0x00, 0x01, 0x00, 0xff}
	if !masternode.IsProviderTx(bad) {
		t.Fatal("setup: not picked as a provider transaction")
	}
	src := newFakeSource(&wallet.GetTransactionsResult{
		MinedTransactions: []wallet.Block{{Height: 700, Transactions: []wallet.TransactionSummary{{Transaction: bad}}}},
	})
	src.synced = 5000
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dw.trackLoop(ctx, mgr, src)
	}()
	defer func() { cancel(); wg.Wait() }()
	time.Sleep(100 * time.Millisecond)
	snap, err := store.Load()
	if err != nil || snap.TxRead != 0 {
		t.Fatalf("saved %d past a transaction that was not tracked: %v", snap.TxRead, err)
	}
}

// The wallet records transactions while it synchronizes, before the track loop
// starts, and one it finds can be of a block below the height a restart reads
// from. The subscription made as the wallet opens is told of it, and the loop
// takes it.
func TestTrackLoopGetsWhatTheWalletRecordsBeforeItStarts(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, store := trackingManager(t, &chainStub{}, seqKeys{})
	if err := store.Save(masternode.Update{Cursor: &masternode.Cursor{Height: 910}}); err != nil {
		t.Fatal(err)
	}
	// An earlier session read through 5000, so a restart reads from 4900.
	if err := mgr.SetTxRead(5000); err != nil {
		t.Fatal(err)
	}
	src := newFakeSource(&wallet.GetTransactionsResult{})
	src.synced = 5000
	dw.early.Store(startFeed(src))

	// The synchronization records a registration at height 798, and the wallet
	// waits for the notification to be read: the loop is not running.
	src.ch <- &wallet.TransactionNotifications{AttachedBlocks: []wallet.Block{
		{Height: 798, Transactions: []wallet.TransactionSummary{{Transaction: vectorRaw(t, "proregtx")}}},
		{Height: 5000},
	}}

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dw.trackLoop(ctx, mgr, src)
	}()
	defer func() { cancel(); wg.Wait() }()
	if got := src.from.Load(); got > 4900 && got != 0 {
		t.Fatalf("read from %d", got)
	}
	waitFor(t, "the registration recorded before the loop started to be tracked", func() bool {
		snap, err := store.Load()
		return err == nil && len(snap.Tracked) == 1
	})
}

func discovered(dw *dashWallet, t *testing.T, mgr *masternode.Manager) bool {
	t.Helper()
	done, _ := dw.discover(t.Context(), mgr)
	return done
}

// A failing Discover or transaction waits longer each time, so a wake from a
// notification does not read the shared history again.
func TestBackoffGrowsToItsCap(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Fatalf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := backoff(50); got != retryMax {
		t.Fatalf("backoff(50) = %v, want %v", got, retryMax)
	}
}

// runLoop starts the track loop over src, and stops it when the test ends.
func runLoop(t *testing.T, dw *dashWallet, mgr *masternode.Manager, src *fakeSource) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dw.trackLoop(ctx, mgr, src)
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })
}

func wake(t *testing.T, src *fakeSource, n int) {
	t.Helper()
	for range n {
		src.ch <- &wallet.TransactionNotifications{}
	}
	time.Sleep(50 * time.Millisecond)
}

// A Discover that failed is not run again by a notification, which a block or
// an incoming payment each brings: it would read the shared history every time.
func TestTrackLoopDoesNotRetryDiscoverOnAWake(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	chain := &chainStub{} // no header at the tip: Discover fails
	mgr, _ := trackingManager(t, chain, seqKeys{})
	if err := mgr.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	src := newFakeSource(&wallet.GetTransactionsResult{
		UnminedTransactions: []wallet.TransactionSummary{{Transaction: vectorRaw(t, "coinbase")}},
	})
	runLoop(t, dw, mgr, src)
	waitFor(t, "the first Discover", func() bool { return chain.tips.Load() > 0 })
	before := chain.tips.Load()
	wake(t, src, 5)
	if got := chain.tips.Load(); got != before {
		t.Fatalf("Discover ran %d more times on wakes", got-before)
	}
}

// A wallet with no transaction is new, and the shared history is not read for
// it.
func TestTrackLoopDoesNotDiscoverForAnEmptyWallet(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	chain := &chainStub{}
	mgr, _ := trackingManager(t, chain, seqKeys{})
	if err := mgr.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	src := newFakeSource(&wallet.GetTransactionsResult{})
	runLoop(t, dw, mgr, src)
	wake(t, src, 3)
	if chain.tips.Load() != 0 {
		t.Fatal("discovered for a wallet with no transaction")
	}
}

// A wallet whose Discover has run is not read for its transactions again.
func TestTrackLoopDoesNotCheckTheHistoryOnceDiscovered(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, store := trackingManager(t, &chainStub{}, seqKeys{})
	if err := store.Save(masternode.Update{Cursor: &masternode.Cursor{Height: 910}}); err != nil {
		t.Fatal(err)
	}
	src := newFakeSource(&wallet.GetTransactionsResult{})
	runLoop(t, dw, mgr, src)
	wake(t, src, 2)
	if n := src.used.Load(); n != 0 {
		t.Fatalf("read the wallet's history %d times", n)
	}
}

// A transaction that could not be tracked is not tried on every wake, which
// would spend its tries in a few notifications, and when it is given up the
// height saved stops at its block, so a restart gives it again.
func TestTrackLoopGivesUpAtItsBlock(t *testing.T) {
	first, max := retryFirst, retryMax
	retryFirst, retryMax = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { retryFirst, retryMax = first, max })

	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, store := trackingManager(t, &chainStub{}, seqKeys{})
	if err := store.Save(masternode.Update{Cursor: &masternode.Cursor{Height: 910}}); err != nil {
		t.Fatal(err)
	}
	// It has the type of a registration and is not one.
	bad := []byte{0x03, 0x00, 0x01, 0x00, 0xff}
	src := newFakeSource(&wallet.GetTransactionsResult{
		MinedTransactions: []wallet.Block{{Height: 700, Transactions: []wallet.TransactionSummary{{Transaction: bad}}}},
	})
	src.synced = 5000
	runLoop(t, dw, mgr, src)
	waitFor(t, "the height to be saved at the given up block", func() bool {
		snap, err := store.Load()
		return err == nil && snap.TxRead == 700
	})
	wake(t, src, 3)
	if snap, err := store.Load(); err != nil || snap.TxRead != 700 {
		t.Fatalf("height %d past a transaction given up: %v", snap.TxRead, err)
	}
}
