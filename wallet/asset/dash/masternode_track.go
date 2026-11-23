// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package dash

import (
	"context"
	"errors"
	"sync"

	"github.com/bisoncraft/caribou/wallet/asset/dash/masternode"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
)

// A shared registration spends the funds of every share owner, so it is among
// the wallet's own transactions, and so is a dissolution that it signs. The
// Manager is given those, and proves them once they are mined, instead of
// reading the history of the shared collateral script, which every shared
// masternode on the network adds to.

// txSource is where the wallet's transactions come from.
type txSource interface {
	// Notifications returns the channel the wallet's transaction notifications
	// arrive on, and a func to stop them, which must be called once. The
	// channel is closed after it. A notification is sent without buffering,
	// so the channel has to be read promptly.
	Notifications() (<-chan *wallet.TransactionNotifications, func())
	// Transactions returns every transaction the wallet has, mined and not.
	Transactions() (*wallet.GetTransactionsResult, error)
}

// walletSource is a txSource over the wallet.
type walletSource struct{ w *wallet.Wallet }

func (s walletSource) Notifications() (<-chan *wallet.TransactionNotifications, func()) {
	c := s.w.NtfnServer.TransactionNotifications()
	return c.C, c.Done
}

func (s walletSource) Transactions() (*wallet.GetTransactionsResult, error) {
	return s.w.GetTransactions(nil, nil, "", nil)
}

// walletTx is a registration or dissolution in the wallet, and the height of
// its block, or 0 if it is not mined.
type walletTx struct {
	raw    []byte
	height int32
}

// txQueue holds transactions for the track loop. The channel that notifications
// arrive on is unbuffered, so what is received is only queued, and the work is
// left to the loop.
type txQueue struct {
	mu   sync.Mutex
	txs  []walletTx
	wake chan struct{}
}

func newTxQueue() *txQueue { return &txQueue{wake: make(chan struct{}, 1)} }

func (q *txQueue) add(txs ...walletTx) {
	if len(txs) == 0 {
		return
	}
	q.mu.Lock()
	q.txs = append(q.txs, txs...)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *txQueue) take() []walletTx {
	q.mu.Lock()
	defer q.mu.Unlock()
	txs := q.txs
	q.txs = nil
	return txs
}

func providerTxs(mined []wallet.Block, unmined []wallet.TransactionSummary) []walletTx {
	var out []walletTx
	for _, b := range mined {
		for _, s := range b.Transactions {
			if masternode.IsProviderTx(s.Transaction) {
				out = append(out, walletTx{raw: s.Transaction, height: b.Height})
			}
		}
	}
	for _, s := range unmined {
		if masternode.IsProviderTx(s.Transaction) {
			out = append(out, walletTx{raw: s.Transaction})
		}
	}
	return out
}

// trackLoop gives the Manager the registrations and dissolutions in the wallet,
// those it has now and those that arrive, until ctx ends, and runs the
// Discover that finds a wallet's masternodes that it has no transaction of, once.
//
// A registration is only the wallet's if one of its owner keys is, which may
// need a key that is not cached while the wallet is locked. It is tried again
// when the wallet is unlocked, which asks through retryTrack.
func (dw *dashWallet) trackLoop(ctx context.Context, mgr *masternode.Manager, src txSource) {
	ch, stop := src.Notifications()
	q := newTxQueue()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := range ch {
			q.add(providerTxs(n.AttachedBlocks, n.UnminedTransactions)...)
		}
	}()
	defer func() {
		stop()
		wg.Wait()
	}()

	if res, err := src.Transactions(); err != nil {
		dw.log.Errorf("Reading the wallet's transactions for masternodes: %v", err)
	} else {
		q.add(providerTxs(res.MinedTransactions, res.UnminedTransactions)...)
	}

	var waiting []walletTx // not the wallet's yet: its keys are not cached
	discovered := false
	for {
		work := append(waiting, q.take()...)
		waiting = nil
		tracked := false
		for _, t := range work {
			err := mgr.Track(t.raw, t.height)
			switch {
			case err == nil:
				tracked = true
			case errors.Is(err, masternode.ErrNotOurs):
			case errors.Is(err, masternode.ErrDeriveOwnerKey):
				dw.log.Debugf("Masternode transaction waits for the wallet to be unlocked: %v", err)
				waiting = append(waiting, t)
			default:
				dw.log.Errorf("Tracking a masternode transaction: %v", err)
			}
		}
		if tracked {
			dw.requestRefresh()
		}
		if !discovered {
			discovered = dw.discover(ctx, mgr)
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-dw.retryTrack:
		}
	}
}

// discover runs the Discover that reads the history of the shared collateral
// script, if none has, and reports whether it ran. A wallet whose keys are not
// cached cannot, and waits for the unlock. A server that will not give the
// history is not asked again until the next Connect: the registrations in the
// wallet are tracked, and a masternode the wallet has no transaction of can be
// added with TrackTxID.
func (dw *dashWallet) discover(ctx context.Context, mgr *masternode.Manager) bool {
	done, err := mgr.Discovered()
	if err != nil {
		dw.log.Errorf("Masternode discovery: %v", err)
		return false
	}
	if done {
		return true
	}
	if dw.Locked() {
		need, err := mgr.NeedsKeys()
		if err != nil {
			dw.log.Errorf("Masternode discovery: checking the cached owner keys: %v", err)
			return false
		}
		if need {
			dw.log.Debugf("Masternode discovery waits for the wallet to be unlocked")
			return false
		}
	}
	before := mgr.StakedAmount()
	err = mgr.Discover(ctx)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return false
	case errors.Is(err, masternode.ErrSharedHistory):
		dw.log.Warnf("Masternode discovery: %v. Masternodes the wallet has the registration of are tracked, and others can be added by their transaction id.", err)
	case errors.Is(err, masternode.ErrUnprovable):
		dw.log.Warnf("Masternode discovery: %v", err)
	case errors.Is(err, masternode.ErrDeriveOwnerKey) && dw.Locked():
		// A hit moved the scan past the keys cached for it. Nothing was saved,
		// and unlocking caches them and asks again.
		dw.log.Debugf("Masternode discovery waits for the wallet to be unlocked: %v", err)
		return false
	default:
		dw.log.Errorf("Masternode discovery: %v", err)
	}
	if mgr.StakedAmount() != before {
		dw.PushBalance()
	}
	dw.requestRefresh()
	return true
}

// requestRetryTrack asks the track loop to try again what waited for the unlock.
// It never blocks.
func (dw *dashWallet) requestRetryTrack() {
	select {
	case dw.retryTrack <- struct{}{}:
	default:
	}
}
