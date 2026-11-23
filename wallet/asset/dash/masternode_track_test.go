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
	res *wallet.GetTransactionsResult
	ch  chan *wallet.TransactionNotifications
}

func newFakeSource(res *wallet.GetTransactionsResult) *fakeSource {
	return &fakeSource{res: res, ch: make(chan *wallet.TransactionNotifications)}
}

func (s *fakeSource) Notifications() (<-chan *wallet.TransactionNotifications, func()) {
	return s.ch, func() { close(s.ch) }
}

func (s *fakeSource) Transactions() (*wallet.GetTransactionsResult, error) { return s.res, nil }

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
	chain := &chainStub{}
	mgr, _ := trackingManager(t, chain, seqKeys{})
	if need, err := mgr.NeedsKeys(); err != nil || !need {
		t.Fatalf("needs keys: %v %v", need, err)
	}

	if dw.discover(t.Context(), mgr) || chain.tips.Load() != 0 {
		t.Fatalf("discovered while locked without keys: %d", chain.tips.Load())
	}
	if err := mgr.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	if !dw.discover(t.Context(), mgr) || chain.tips.Load() == 0 {
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
	if !dw.discover(t.Context(), mgr) {
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
	if !dw.discover(t.Context(), mgr) || chain.tips.Load() != 0 {
		t.Fatalf("discovered again: %d", chain.tips.Load())
	}
}
