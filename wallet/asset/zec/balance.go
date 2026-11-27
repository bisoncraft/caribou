// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

// Balance returns the balance read last from zeclib. It reads no database.
func (w *zecWallet) Balance() (*asset.Balance, error) {
	if !w.IsOpen() {
		return nil, errClosed
	}
	bal := w.cache.heldBalance()
	return &bal, nil
}

// refresh reads the balance from zeclib, stores it unless a later refresh
// has, and reports it to core if it changed.
func (w *zecWallet) refresh() {
	seq := w.cache.next()
	be, account, release, err := w.backend()
	if err != nil {
		return
	}
	// The guard is held until the balance is stored and reported, so a
	// Close cannot finish in between and leave it in a closed instance.
	defer release()
	summary, err := be.Summary()
	var spendable zeclib.SpendableValue
	if err == nil {
		spendable, err = be.Spendable(account.ID)
	}
	if err != nil {
		if !errors.Is(err, zeclib.ErrClosed) {
			w.log.Errorf("Reading the balance: %v", err)
		}
		return
	}
	bal, left := balanceOf(summary, account.ID, spendable)
	if left != (leftOut{}) {
		w.log.Debugf("Transparent value in no balance: %d in coinbase outputs, %d in outputs not worth spending",
			left.coinbase, left.uneconomic)
	}
	var emit func(*asset.Balance)
	if w.emit != nil {
		emit = w.emit.BalanceChange
	}
	w.cache.storeBalance(seq, bal, emit)
}

// leftOut is transparent value that is in no balance: the outputs of
// coinbase transactions, which a payment never spends, and outputs worth
// less than the fee of spending them.
type leftOut struct {
	coinbase, uneconomic uint64
}

// balanceOf computes the balance from the account's summary and the value a
// payment can spend now. A nil summary, before the wallet has scanned a
// block, is a zero balance.
//
//   - Available is what a payment can spend: Ironwood notes with their
//     confirmations and anchor, and regular transparent outputs worth
//     spending.
//   - Immature is what becomes spendable without anything else happening:
//     the rest of the Ironwood pool that is not locked, which includes change
//     and received notes still waiting for confirmations or for the wallet
//     to prove them, and transparent change and received value waiting for
//     confirmations.
//   - Locked is value reserved by an operation in progress.
//
// Transparent value waiting for confirmations is counted as Immature whole,
// although outputs worth less than spending them will be left out once
// confirmed: two pending outputs of 4,000 show 8,000 Immature. The summary
// groups pending value without telling the size of its outputs.
func balanceOf(summary *zeclib.Summary, accountID string, spendable zeclib.SpendableValue) (asset.Balance, leftOut) {
	if summary == nil {
		return asset.Balance{}, leftOut{}
	}
	ab, _ := summary.Balance(accountID)
	iw, tr := ab.Ironwood, ab.Transparent
	var immature uint64
	if spent := spendable.Ironwood + iw.Locked; iw.Total > spent {
		immature = iw.Total - spent
	}
	immature += tr.ChangePending + tr.ValuePending
	left := leftOut{coinbase: ab.Coinbase.Total, uneconomic: tr.Uneconomic}
	if tr.Spendable > spendable.Transparent {
		left.uneconomic += tr.Spendable - spendable.Transparent
	}
	return asset.Balance{
		Available: spendable.Ironwood + spendable.Transparent,
		Immature:  immature,
		Locked:    iw.Locked + tr.Locked,
	}, left
}
