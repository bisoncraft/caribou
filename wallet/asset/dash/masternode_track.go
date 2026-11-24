// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package dash

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/dash/masternode"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// txReadMargin is how many blocks below the height it has read through a
// restart reads the wallet's transactions from, for a block that was replaced.
const txReadMargin = 100

// A Discover that could not finish, and a transaction that could not be tracked,
// are tried again after retryFirst, and then after twice as long each time, up
// to retryMax.
const (
	retryFirst = time.Minute
	retryMax   = 30 * time.Minute
	// trackTries is how many times a transaction that could not be tracked is
	// tried before it is given up.
	trackTries = 5
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
	// Transactions returns the transactions the wallet has from the block at
	// height on, and every one that is not mined, until cancel is closed.
	Transactions(height int32, cancel <-chan struct{}) (*wallet.GetTransactionsResult, error)
	// Synced is the height the wallet has processed blocks through.
	Synced() int32
	// Has reports whether the wallet has the transaction, mined or not.
	Has(id chainhash.Hash) (bool, error)
}

// walletSource is a txSource over the wallet.
type walletSource struct{ w *wallet.Wallet }

func (s walletSource) Notifications() (<-chan *wallet.TransactionNotifications, func()) {
	c := s.w.NtfnServer.TransactionNotifications()
	return c.C, c.Done
}

func (s walletSource) Transactions(height int32, cancel <-chan struct{}) (*wallet.GetTransactionsResult, error) {
	var start *wallet.BlockIdentifier
	if height > 0 {
		start = wallet.NewBlockIdentifierFromHeight(height)
	}
	return s.w.GetTransactions(start, nil, "", cancel)
}

func (s walletSource) Synced() int32 { return s.w.Manager.SyncedTo().Height }

func (s walletSource) Has(id chainhash.Hash) (bool, error) {
	_, err := s.w.GetTransaction(id)
	if errors.Is(err, wallet.ErrNoTx) {
		return false, nil
	}
	return err == nil, err
}

// walletTx is a registration or dissolution in the wallet, and the height of
// its block, or 0 if it is not mined.
type walletTx struct {
	raw    []byte
	height int32
	tries  int
}

// txQueue holds transactions for the track loop. The channel that notifications
// arrive on is unbuffered, so what is received is only queued, and the work is
// left to the loop.
type txQueue struct {
	mu      sync.Mutex
	txs     []walletTx
	recheck bool  // the wallet's transactions changed
	height  int32 // the highest block queued
	wake    chan struct{}
}

func newTxQueue() *txQueue { return &txQueue{wake: make(chan struct{}, 1)} }

func (q *txQueue) add(height int32, txs []walletTx, recheck bool) {
	q.mu.Lock()
	q.txs = append(q.txs, txs...)
	q.recheck = q.recheck || recheck
	q.height = max(q.height, height)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take returns what is queued, and the height of the highest block of it.
func (q *txQueue) take() (txs []walletTx, recheck bool, height int32) {
	q.mu.Lock()
	defer q.mu.Unlock()
	txs, recheck, height = q.txs, q.recheck, q.height
	q.txs, q.recheck = nil, false
	return
}

// blocksHeight is the height of the highest block of a notification.
func blocksHeight(blocks []wallet.Block) int32 {
	var h int32
	for _, b := range blocks {
		h = max(h, b.Height)
	}
	return h
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

// feed is the wallet's transaction notifications, queued as they arrive.
type feed struct {
	q    *txQueue
	stop func()
}

// startFeed subscribes to the wallet's notifications and queues what they
// bring. The wallet waits for a notification to be read, so it is read from
// here on, whether or not the track loop has started.
func startFeed(src txSource) *feed {
	ch, done := src.Notifications()
	q := newTxQueue()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := range ch {
			// The hashes in a notification are the transactions the wallet
			// still has unmined, not the ones it dropped, so a drop is found by
			// asking the wallet about what is tracked.
			q.add(blocksHeight(n.AttachedBlocks), providerTxs(n.AttachedBlocks, n.UnminedTransactions), true)
		}
	}()
	return &feed{q: q, stop: func() {
		done()
		wg.Wait()
	}}
}

// onOpen subscribes to the wallet as it opens, before it synchronizes, because
// the synchronization records transactions that are below the height a restart
// reads from, and only a subscription that is already there is told of them.
func (dw *dashWallet) onOpen(w *wallet.Wallet) {
	dw.dropEarlyFeed()
	dw.early.Store(startFeed(walletSource{w}))
}

// dropEarlyFeed ends a subscription that no track loop took.
func (dw *dashWallet) dropEarlyFeed() {
	if f := dw.early.Swap(nil); f != nil {
		f.stop()
	}
}

// trackLoop gives the Manager the registrations and dissolutions in the wallet,
// those it has now and those that arrive, until ctx ends, and runs the
// Discover that finds a wallet's masternodes that it has no transaction of, once.
//
// A registration is only the wallet's if one of its owner keys is, which may
// need a key that is not cached while the wallet is locked. It is tried again
// when the wallet is unlocked, which asks through retryTrack.
func (dw *dashWallet) trackLoop(ctx context.Context, mgr *masternode.Manager, src txSource) {
	f := dw.early.Swap(nil)
	if f == nil {
		f = startFeed(src)
	}
	q := f.q
	defer f.stop()

	// Only what the wallet has recorded since the last read is read: what is
	// older has been given to Track, and a registration it was waiting on is
	// proven from the store. What is tracked and the wallet no longer has is
	// dropped.
	cancel := make(chan struct{})
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			close(cancel)
		case <-finished:
		}
	}()
	var from int32
	if read, err := mgr.TxRead(); err != nil {
		dw.log.Errorf("Reading the height masternode transactions were read to: %v", err)
	} else if read > txReadMargin {
		from = read - txReadMargin
	}
	synced := src.Synced()
	loaded := false
	if res, err := src.Transactions(from, cancel); err != nil {
		dw.log.Errorf("Reading the wallet's transactions for masternodes: %v", err)
	} else {
		q.add(synced, providerTxs(res.MinedTransactions, res.UnminedTransactions), true)
		loaded = true
	}

	var waiting []walletTx // not the wallet's yet: its keys are not cached
	discovered := false
	var saved int32 // the height last saved
	if read, err := mgr.TxRead(); err == nil {
		saved = read
	}
	retryIn := retryFirst
	var retry <-chan time.Time
	for {
		queued, recheck, height := q.take()
		work := append(waiting, queued...)
		waiting = nil
		failing := false // a transaction could not be tracked, and is tried again
		tracked := false
		for _, t := range work {
			err := mgr.Track(t.raw, t.height)
			switch {
			case err == nil:
				tracked = true
			case errors.Is(err, masternode.ErrNotOurs), errors.Is(err, masternode.ErrInvalidRegistration):
				// Not a shared registration of the wallet's: an ordinary
				// masternode registration, or another wallet's.
			case errors.Is(err, masternode.ErrDeriveOwnerKey):
				dw.log.Debugf("Masternode transaction waits for the wallet to be unlocked: %v", err)
				waiting = append(waiting, t)
			default:
				// Not that it is not the wallet's, so it is not skipped: a
				// restart would not give it again.
				t.tries++
				if t.tries >= trackTries {
					dw.log.Errorf("Giving up tracking a masternode transaction: %v", err)
					break
				}
				dw.log.Warnf("Tracking a masternode transaction, will try again: %v", err)
				waiting = append(waiting, t)
				failing = true
			}
		}
		// A transaction the wallet gave and no longer has, such as one dropped
		// because another spent its inputs, is not asked the server for any
		// more. Those put back after a reorg, or added by id, are not the
		// wallet's to drop.
		if recheck {
			ids, err := mgr.Tracked()
			if err != nil {
				dw.log.Errorf("Reading the tracked masternode transactions: %v", err)
			}
			for _, id := range ids {
				if has, err := src.Has(id); err != nil {
					dw.log.Errorf("Checking the wallet for masternode transaction %s: %v", id, err)
				} else if !has {
					if err := mgr.Untrack(id); err != nil {
						dw.log.Errorf("Untracking masternode transaction %s: %v", id, err)
					}
				}
			}
		}
		if tracked {
			dw.requestRefresh()
		}
		// Everything up to the height has been given to Track, unless it
		// waits for an unlock, which a restart would not give it again.
		if loaded && len(waiting) == 0 && height > saved {
			if err := mgr.SetTxRead(height); err != nil {
				dw.log.Errorf("Saving the height masternode transactions were read to: %v", err)
			} else {
				saved = height
			}
		}
		if !discovered {
			discovered = dw.discover(ctx, mgr)
		}
		retry = nil
		if !discovered || failing {
			retry = time.After(retryIn)
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-dw.retryTrack:
		case <-retry:
			retryIn = min(retryIn*2, retryMax)
		}
	}
}

// discover runs the Discover that reads the history of the shared collateral
// script, if none has, and reports whether it ran. A wallet whose keys are not
// cached cannot, and waits for the unlock. A server that will not give the
// history is not asked again until the next Connect: the registrations in the
// wallet are tracked. A Discover that could not read it all, because the headers
// are behind the server or a request failed, is tried again by the loop.
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
		dw.log.Warnf("Masternode discovery: %v. Masternodes the wallet has the registration of are tracked, but one it has no transaction of is not found.", err)
	case errors.Is(err, masternode.ErrUnprovable):
		dw.log.Warnf("Masternode discovery: %v", err)
	case errors.Is(err, masternode.ErrDeriveOwnerKey) && dw.Locked():
		// A hit moved the scan past the keys cached for it. Nothing was saved,
		// and unlocking caches them and asks again.
		dw.log.Debugf("Masternode discovery waits for the wallet to be unlocked: %v", err)
		return false
	case errors.Is(err, masternode.ErrHeadersBehind):
		dw.log.Debugf("Masternode discovery waits for the headers: %v", err)
		return false
	default:
		// Not read to the end, and not marked as read: tried again.
		dw.log.Warnf("Masternode discovery will be tried again: %v", err)
		return false
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
