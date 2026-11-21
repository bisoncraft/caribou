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
	"github.com/bisoncraft/caribou/wallet/asset/btc"
	"github.com/bisoncraft/caribou/wallet/asset/dash/masternode"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	_ "github.com/bisoncraft/caribou/wallet/utxowallet/walletdb/bdb"
)

func TestSharedMasternodesGate(t *testing.T) {
	cases := []struct {
		walletType string
		net        util.Network
		want       bool
	}{
		{walletTypeElectrum, util.Regtest, true},
		{walletTypeElectrum, util.Testnet, true},
		{walletTypeElectrum, util.Mainnet, sharedMasternodesOnMainnet},
		{walletTypeSPV, util.Regtest, false},
		{walletTypeSPV, util.Testnet, false},
		{walletTypeSPV, util.Mainnet, false},
	}
	for _, c := range cases {
		params, err := parseChainParams(c.net)
		if err != nil {
			t.Fatal(err)
		}
		if got := sharedMasternodesEnabled(c.walletType, params); got != c.want {
			t.Errorf("%s on %v: %v, want %v", c.walletType, c.net, got, c.want)
		}
		w, err := newWallet(&asset.WalletConfig{Type: c.walletType, DataDir: t.TempDir()}, util.Disabled, c.net)
		if err != nil {
			t.Fatal(err)
		}
		_, shared := w.(*dashWallet)
		_, plain := w.(*btc.NativeWallet)
		if shared != c.want || plain == c.want {
			t.Errorf("%s on %v: shared wallet %v, plain wallet %v", c.walletType, c.net, shared, plain)
		}
	}
}

// The coin type is the one for the network, which the clone params do not carry.
func TestSharedWalletCoinType(t *testing.T) {
	for _, c := range []struct {
		net  util.Network
		want uint32
	}{{util.Mainnet, 5}, {util.Testnet, 1}, {util.Regtest, 1}} {
		params, err := parseChainParams(c.net)
		if err != nil {
			t.Fatal(err)
		}
		dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, params, util.Disabled)
		if dw.coinType != c.want {
			t.Errorf("%v: coin type %d, want %d", c.net, dw.coinType, c.want)
		}
	}
}

func TestManagerUnavailable(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	if _, err := dw.manager(); !errors.Is(err, asset.ErrConnectionDown) {
		t.Fatalf("%v", err)
	}
	if _, err := dw.newManager(); err == nil {
		t.Fatal("built a manager for a wallet that is not open")
	}
	why := errors.New("no protx")
	dw.mnErr.Store(&why)
	if _, err := dw.manager(); !errors.Is(err, why) {
		t.Fatalf("%v", err)
	}
}

// A Manager that cannot be built does not fail the wallet: it is recorded, the
// masternode methods return it, and no loop is started.
func TestTrackMasternodesFailure(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	var wg sync.WaitGroup
	dw.trackMasternodes(t.Context(), &wg)
	wg.Wait()
	_, err := dw.manager()
	if err == nil || errors.Is(err, asset.ErrConnectionDown) {
		t.Fatalf("the reason is not recorded: %v", err)
	}
	if dw.mn.Load() != nil {
		t.Fatal("a manager is set")
	}
}

func TestOnTipCoalescesAndNeverBlocks(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			dw.onTip(int32(i))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("onTip blocked")
	}
	if len(dw.refresh) != 1 {
		t.Fatalf("%d requests pending", len(dw.refresh))
	}
}

// chainStub is a masternode.Chain that has no headers, so a Refresh fails, and
// counts the Refreshes.
type chainStub struct{ tips atomic.Int32 }

func (c *chainStub) Tip() int32                             { c.tips.Add(1); return 0 }
func (c *chainStub) StartHeight() int32                     { return 0 }
func (c *chainStub) HeaderByHeight(int32) *wire.BlockHeader { return nil }
func (c *chainStub) GetHistory(context.Context, string) ([]electrum.HistoryEntry, error) {
	return nil, errors.New("no history")
}
func (c *chainStub) GetTransactionVerbose(context.Context, string) (*electrum.VerboseTx, error) {
	return nil, errors.New("none")
}
func (c *chainStub) HeaderByHash(*chainhash.Hash) (int32, *wire.BlockHeader, bool) {
	return 0, nil, false
}
func (c *chainStub) GetRawTransaction(context.Context, string) (string, error) {
	return "", errors.New("none")
}
func (c *chainStub) GetMerkle(context.Context, string, int32) (*electrum.MerkleProof, error) {
	return nil, errors.New("none")
}
func (c *chainStub) ProTxInfo(context.Context, chainhash.Hash) (*electrum.ProTxInfo, error) {
	return nil, electrum.ErrProTxUnsupported
}

type noKeys struct{}

// seqKeys derives a key that depends on the path, as an unlocked wallet does.
type seqKeys struct{}

func (seqKeys) DeriveFromRootPath(p []uint32) (*btcec.PrivateKey, error) {
	b := make([]byte, 32)
	b[30], b[31] = byte(p[len(p)-1]>>8), byte(p[len(p)-1])
	b[0] = 1
	k, _ := btcec.PrivKeyFromBytes(b)
	return k, nil
}

func (noKeys) DeriveFromRootPath([]uint32) (*btcec.PrivateKey, error) {
	return nil, errors.New("locked")
}

func newStore(t *testing.T) *masternode.Store {
	t.Helper()
	db, err := walletdb.Create("bdb", filepath.Join(t.TempDir(), "mn.db"), true, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := masternode.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// stakedManager is a Manager that holds the masternode of the real registration
// vector, with the owner key id of its first share cached as the wallet's own.
func stakedManager(t *testing.T, chain masternode.Chain, keys masternode.KeyDeriver) (*masternode.Manager, int64) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("masternode", "testdata", "dash_special_txs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var all []struct{ Name, Hex string }
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for _, v := range all {
		if v.Name == "proregtx" {
			raw, _ = hex.DecodeString(v.Hex)
		}
	}
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	mn, err := masternode.RegistrationMN(tx, 798)
	if err != nil {
		t.Fatal(err)
	}
	store := newStore(t)
	err = store.Save(masternode.Update{
		OwnerKeyIDs:   map[uint32][20]byte{1: mn.Shares[0].KeyIDOwner},
		Registrations: []masternode.Registration{{Height: 798, Tx: raw}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := masternode.NewManager(chain, store, keys, 1, RegressionNetParams)
	if err != nil {
		t.Fatal(err)
	}
	return m, mn.Shares[0].Amount
}

func TestAdjustBalance(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	newBal := func() *asset.Balance {
		return &asset.Balance{Available: 7, Locked: 3, Other: make(map[asset.BalanceCategory]asset.CustomBalance)}
	}
	// Not connected: nothing to add.
	bal := newBal()
	dw.adjustBalance(bal)
	if bal.Locked != 3 || len(bal.Other) != 0 {
		t.Fatalf("%+v", bal)
	}

	// Connected with no staked share: still nothing.
	empty, err := masternode.NewManager(&chainStub{}, newStore(t), noKeys{}, 1, RegressionNetParams)
	if err != nil {
		t.Fatal(err)
	}
	dw.mn.Store(empty)
	bal = newBal()
	dw.adjustBalance(bal)
	if bal.Locked != 3 || len(bal.Other) != 0 {
		t.Fatalf("%+v", bal)
	}

	// A staked share is locked, and listed as staked as for DCR's tickets: not
	// subtracted from what is available, which never had it.
	mgr, amount := stakedManager(t, &chainStub{}, noKeys{})
	dw.mn.Store(mgr)
	bal = newBal()
	dw.adjustBalance(bal)
	if bal.Locked != 3+uint64(amount) || bal.Available != 7 {
		t.Fatalf("%+v", bal)
	}
	if got := bal.Other[asset.BalanceCategoryStaked]; got.Amount != uint64(amount) || got.Locked {
		t.Fatalf("%+v", got)
	}
}

// A locked wallet that has not cached its owner keys does not scan, which would
// fetch the history and fail on every block. Unlocking caches them and asks for
// the Refresh.
func TestRefreshWaitsForUnlock(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	chain := &chainStub{}
	mgr, _ := stakedManager(t, chain, seqKeys{})
	dw.mn.Store(mgr)
	if need, err := mgr.NeedsKeys(); err != nil || !need {
		t.Fatalf("needs keys: %v %v", need, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dw.refreshLoop(ctx, mgr)
	}()
	defer func() { cancel(); wg.Wait() }()
	waitTips := func(n int32) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if chain.tips.Load() >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%d refreshes, want %d", chain.tips.Load(), n)
	}
	// The initial run and a tip are skipped: the wallet is not open, so it is
	// locked.
	dw.onTip(1)
	time.Sleep(100 * time.Millisecond)
	if chain.tips.Load() != 0 {
		t.Fatalf("scanned while locked without keys: %d", chain.tips.Load())
	}

	// Unlocking caches the keys, and a Refresh follows with no further tip.
	dw.afterUnlock()
	if need, err := mgr.NeedsKeys(); err != nil || need {
		t.Fatalf("needs keys after the unlock: %v %v", need, err)
	}
	waitTips(1)
}

func TestRefreshLoop(t *testing.T) {
	dw := openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	chain := &chainStub{}
	mgr, _ := stakedManager(t, chain, seqKeys{})
	if err := mgr.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	dw.mn.Store(mgr)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dw.refreshLoop(ctx, mgr)
	}()
	waitTips := func(n int32) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if chain.tips.Load() >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%d refreshes, want %d", chain.tips.Load(), n)
	}
	// One at the start, then one per request.
	waitTips(1)
	if chain.tips.Load() != 1 {
		t.Fatalf("%d refreshes at the start", chain.tips.Load())
	}
	dw.onTip(1)
	waitTips(2)

	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh loop did not stop")
	}
	if dw.mn.Load() != nil {
		t.Fatal("the manager is still set after the loop ended")
	}
	if _, err := dw.manager(); !errors.Is(err, asset.ErrConnectionDown) {
		t.Fatalf("%v", err)
	}
}
