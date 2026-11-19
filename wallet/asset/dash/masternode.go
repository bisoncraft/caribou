// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package dash

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
	"github.com/bisoncraft/caribou/wallet/asset/dash/masternode"
	"github.com/bisoncraft/caribou/wallet/utxowallet/chain"
)

// sharedMasternodesOnMainnet turns on shared masternode support on mainnet,
// where Dash Core v24 shared masternodes are not live yet. Without it only the
// electrum wallet on a test network has it.
const sharedMasternodesOnMainnet = false

// sharedMasternodesEnabled reports whether a wallet of this type on this network
// tracks shared masternodes. It needs an electrum server: the masternodes are
// found and proven through its history and merkle proofs.
func sharedMasternodesEnabled(walletType string, params *chaincfg.Params) bool {
	if walletType != walletTypeElectrum {
		return false
	}
	return params.Name != MainNetParams.Name || sharedMasternodesOnMainnet
}

// dashWallet is the native wallet with shared masternode support. The
// masternode Manager lives from Connect until the Connect context ends.
type dashWallet struct {
	*btc.NativeWallet
	log      util.Logger
	net      *chaincfg.Params
	coinType uint32

	// mn is the Manager while connected, nil otherwise.
	mn atomic.Pointer[masternode.Manager]
	// mnErr is why there is no Manager although the wallet is connected.
	mnErr atomic.Pointer[error]
	// refresh asks the refresh loop for a Refresh. One pending request is
	// enough, so a burst of blocks makes one.
	refresh chan struct{}
	// retryTrack asks the track loop to try again what waited for the unlock.
	retryTrack chan struct{}
	// early is the subscription to the wallet's transactions made as it opens,
	// which the track loop takes.
	early atomic.Pointer[feed]

	// offerMu serialises the changes of offers: an offer's state is read,
	// changed and stored as one step.
	offerMu sync.Mutex
	// fw replaces the inner wallet as the funding wallet in tests.
	fw fundingWallet
	// now replaces the clock in tests.
	now func() time.Time
}

// openDashSharedWallet constructs the electrum wallet with shared masternode
// support.
func openDashSharedWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *dashWallet {
	dw := &dashWallet{
		log: log,
		net: chainParams,
		// The DIP-9 coin type is the one the assets table gives. The clone
		// params leave HDCoinType at 0.
		coinType: dashTableNetParams(chainParams).HDCoinType,
		refresh:  make(chan struct{}, 1),

		retryTrack: make(chan struct{}, 1),
	}
	cc := dashElectrumChainConfig(chainParams)
	cc.OnTip = dw.onTip
	cc.OnOpen = dw.onOpen
	cc.AdjustBalance = dw.adjustBalance
	dw.NativeWallet = btc.NewNativeWallet(cc, cfg, log)
	return dw
}

// Connect connects the wallet and then starts tracking masternodes. A failure
// to do that does not fail Connect: the wallet is still a wallet, and the
// masternode methods return the error.
func (dw *dashWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	dw.mnErr.Store(nil)
	wg, err := dw.NativeWallet.Connect(ctx)
	if err != nil {
		dw.dropEarlyFeed()
		return nil, err
	}
	dw.trackMasternodes(ctx, wg)
	return wg, nil
}

// trackMasternodes builds the Manager and starts its refresh loop in wg. If it
// cannot build it, it records why.
func (dw *dashWallet) trackMasternodes(ctx context.Context, wg *sync.WaitGroup) {
	mgr, err := dw.newManager()
	if err != nil {
		err = fmt.Errorf("shared masternodes are unavailable: %w", err)
		dw.mnErr.Store(&err)
		dw.log.Errorf("%v", err)
		dw.dropEarlyFeed()
		return
	}
	dw.mn.Store(mgr)
	wg.Add(2)
	go func() {
		defer wg.Done()
		dw.refreshLoop(ctx, mgr)
	}()
	go func() {
		defer wg.Done()
		dw.trackLoop(ctx, mgr, walletSource{dw.InnerWallet()})
	}()
}

func (dw *dashWallet) newManager() (*masternode.Manager, error) {
	inner := dw.InnerWallet()
	if inner == nil {
		return nil, errors.New("wallet is not open")
	}
	// The wallet wraps the electrum client, so this is not a *chain.ElectrumClient
	// but has its Network method.
	ec, ok := inner.ChainClient().(interface{ Network() chain.ElectrumNetwork })
	if !ok {
		return nil, errors.New("wallet has no electrum client")
	}
	store, err := masternode.NewStore(inner.Database())
	if err != nil {
		return nil, err
	}
	return masternode.NewManager(ec.Network(), store, inner, dw.coinType, dw.net)
}

// manager returns the Manager, or why there is none.
func (dw *dashWallet) manager() (*masternode.Manager, error) {
	if mgr := dw.mn.Load(); mgr != nil {
		return mgr, nil
	}
	if err := dw.mnErr.Load(); err != nil {
		return nil, *err
	}
	return nil, asset.ErrConnectionDown
}

// Unlock unlocks the wallet and caches the owner key ids a masternode scan looks
// at, which the scan needs and cannot derive while the wallet is locked. Failing
// to cache them does not fail the unlock.
func (dw *dashWallet) Unlock(pw []byte) error {
	if err := dw.NativeWallet.Unlock(pw); err != nil {
		return err
	}
	dw.afterUnlock()
	return nil
}

// afterUnlock caches the owner keys and asks for the Refresh that a locked
// wallet could not do.
func (dw *dashWallet) afterUnlock() {
	mgr := dw.mn.Load()
	if mgr == nil {
		return
	}
	if err := mgr.PrimeOwnerKeys(); err != nil {
		dw.log.Errorf("Caching the masternode owner keys: %v", err)
		return
	}
	dw.requestRetryTrack()
	dw.requestRefresh()
}

// adjustBalance adds the staked shares to a balance. They are not outputs of
// the wallet, so they are in none of its other amounts. As for DCR's tickets,
// they are locked, and listed as Staked.
func (dw *dashWallet) adjustBalance(bal *asset.Balance) {
	mgr := dw.mn.Load()
	if mgr == nil {
		return
	}
	staked := mgr.StakedAmount()
	if staked <= 0 {
		return
	}
	bal.Locked += uint64(staked)
	bal.Other[asset.BalanceCategoryStaked] = asset.CustomBalance{Amount: uint64(staked)}
}

// onTip asks for a Refresh. It is called on the wallet's notification loop and
// never blocks.
func (dw *dashWallet) onTip(int32) { dw.requestRefresh() }

// requestRefresh asks the refresh loop for a Refresh and never blocks.
func (dw *dashWallet) requestRefresh() {
	select {
	case dw.refresh <- struct{}{}:
	default:
	}
}

// refreshLoop refreshes the masternodes once and then on each request until ctx
// ends. A change in the staked amount is pushed as a balance.
//
// A Refresh proves the transactions the track loop gave the Manager and reads
// the history of the refund scripts of the wallet's shares, which is the
// wallet's own. It does not read the shared collateral script, so it needs no
// key, and runs while the wallet is locked.
func (dw *dashWallet) refreshLoop(ctx context.Context, mgr *masternode.Manager) {
	defer dw.mn.CompareAndSwap(mgr, nil)
	run := func() {
		before := mgr.StakedAmount()
		err := mgr.Refresh(ctx)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return
		case errors.Is(err, masternode.ErrUnprovable):
			dw.log.Warnf("Masternode refresh: %v", err)
		default:
			dw.log.Errorf("Masternode refresh: %v", err)
		}
		if mgr.StakedAmount() != before {
			dw.PushBalance()
		}
	}
	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-dw.refresh:
			run()
		}
	}
}
