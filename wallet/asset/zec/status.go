// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

const (
	// BalanceWarningTransparentStale is the code of the wallet's balance
	// warning while the server fails the requests about the wallet's
	// transparent addresses. The transparent balance can be out of date:
	// a payment received at a transparent address can be missing from it.
	BalanceWarningTransparentStale = "transparentStale"
	// BalanceWarningTransparentUnrecorded is the code while the server lists
	// unspent transparent outputs that the wallet could not record. Those
	// funds are missing from the balance. It is the code when both
	// conditions hold.
	BalanceWarningTransparentUnrecorded = "transparentUnrecorded"
)

// SyncStatus reports how far the wallet has synced. Synced follows zeclib's
// state alone: zeclib can keep an error in its status while it is synced.
func (w *zecWallet) SyncStatus() (*asset.SyncStatus, error) {
	be, account, release, err := w.backend()
	if err != nil {
		return nil, err
	}
	st := be.SyncStatus()
	release()
	return syncStatusOf(st, account.Birthday), nil
}

// syncStatusOf converts a zeclib status. The blocks scanned are estimated
// from the scan progress between the account's birthday and the chain tip.
func syncStatusOf(st zeclib.SyncStatus, birthday uint32) *asset.SyncStatus {
	ss := &asset.SyncStatus{
		Synced:         st.State == zeclib.SyncSynced,
		TargetHeight:   uint64(st.ChainTip),
		StartingBlocks: uint64(birthday),
		Blocks:         uint64(birthday),
	}
	switch {
	case ss.Synced:
		ss.Blocks = ss.TargetHeight
	case ss.TargetHeight > ss.StartingBlocks:
		span := ss.TargetHeight - ss.StartingBlocks
		ss.Blocks = ss.StartingBlocks + uint64(st.ScanProgress.Fraction()*float64(span))
		ss.Blocks = min(ss.Blocks, ss.TargetHeight)
	}
	return ss
}

// handleStatus reads zeclib's status and reports what changed: the peer
// count, a new chain tip of a synced wallet, the balance warning, and errors
// in the log. Every report is made under the guard of the backend, so a
// close cannot complete in between and an instance never reports after it
// closed. The peer count goes to core's callback, which is safe to call
// there: it only starts a goroutine, so it neither blocks nor calls back
// into the wallet on this goroutine.
func (w *zecWallet) handleStatus() {
	be, _, release, err := w.backend()
	if err != nil {
		return
	}
	st := be.SyncStatus()
	reportPeers, peers, tipMoved := w.noteStatus(st)
	if reportPeers && w.peersChange != nil {
		var err error
		if peers == 0 {
			err = st.Err
		}
		w.peersChange(uint32(peers), err)
	}
	release()
	if tipMoved {
		// Confirmations, and with them the balance, change with the tip.
		w.refresh()
	}
}

// noteStatus updates the cache with a status, logs, and sends TipChange and
// a change of the balance warning. It returns whether the peer count changed,
// the count, and whether the tip moved. The caller holds the guard of the
// backend.
func (w *zecWallet) noteStatus(st zeclib.SyncStatus) (reportPeers bool, peers int, tipMoved bool) {
	c := &w.cache
	c.mtx.Lock()
	tipMoved = st.ChainTip != c.tip
	c.tip = st.ChainTip
	// The peer count follows the state only. An error can stay in the
	// status of a synced wallet, and core refuses every send at zero peers.
	// Within a connection, SyncStopped is seen only before zeclib's sync
	// has started and after it has ended, so it is not reported, nor is
	// SyncConnecting.
	peers = c.peers
	switch st.State {
	case zeclib.SyncSyncing, zeclib.SyncSynced:
		peers = 1
	case zeclib.SyncDisconnected:
		peers = 0
	}
	reportPeers = peers != c.peers && peers >= 0
	c.peers = peers
	var newTip uint32
	if st.State == zeclib.SyncSynced && st.ChainTip != c.tipEmitted {
		newTip = st.ChainTip
		c.tipEmitted = st.ChainTip
	}
	warning := balanceWarningOf(st.TransparentErr)
	prevWarning := c.warning
	c.warning = warning
	var errText string
	if st.Err != nil {
		errText = st.Err.Error()
	}
	logErr := errText != c.errText && errText != ""
	c.errText = errText
	c.mtx.Unlock()

	w.reportWarning(prevWarning, warning, st.State != zeclib.SyncStopped)
	if logErr {
		w.log.Debugf("Sync status error: %v", st.Err)
	}
	if newTip != 0 && w.emit != nil {
		w.emit.TipChange(uint64(newTip))
	}
	return reportPeers, peers, tipMoved
}

// balanceWarningOf converts the error zeclib reports for the transparent
// addresses into the wallet's balance warning, nil for no error.
func balanceWarningOf(err error) *asset.BalanceWarning {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, zeclib.ErrOutputUnrecorded):
		return &asset.BalanceWarning{Code: BalanceWarningTransparentUnrecorded, Detail: plain(err)}
	default:
		return &asset.BalanceWarning{Code: BalanceWarningTransparentStale, Detail: plain(err)}
	}
}

// reportWarning reports a change of the balance warning from prev to
// warning: a note to core for any change, and a line in the log when the
// warning begins, changes its code, or ends. A change of the detail alone is
// not logged: zeclib logs each failure itself. Nor is a warning that ends
// because the sync stopped, which running tells. The caller holds the guard
// of the backend.
func (w *zecWallet) reportWarning(prev, warning *asset.BalanceWarning, running bool) {
	if prev == nil && warning == nil {
		return
	}
	if prev != nil && warning != nil && *prev == *warning {
		return
	}
	switch {
	case warning == nil:
		if running {
			w.log.Infof("The transparent funds in the balance are up to date again")
		}
	case prev != nil && prev.Code == warning.Code:
	case warning.Code == BalanceWarningTransparentUnrecorded:
		w.log.Warnf("Transparent funds are missing from the balance until the server serves them: %s", warning.Detail)
	default:
		w.log.Warnf("The transparent balance can be out of date until the server answers for the transparent addresses: %s",
			warning.Detail)
	}
	w.sendWarning(warning)
}

// sendWarning sends the balance warning to core in a note of its own copy,
// nil for a warning that has ended.
func (w *zecWallet) sendWarning(warning *asset.BalanceWarning) {
	if w.emit == nil {
		return
	}
	var payload *asset.BalanceWarning
	if warning != nil {
		sent := *warning
		payload = &sent
	}
	w.emit.Data(asset.BalanceWarningRoute, payload)
}

// BalanceWarning returns the warning for transparent funds that can be
// missing from the balance, nil when there is none. It is that of the last
// sync status handled, and nil for a wallet that is not connected.
func (w *zecWallet) BalanceWarning() *asset.BalanceWarning {
	c := &w.cache
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.warning == nil {
		return nil
	}
	warning := *c.warning
	return &warning
}

// endWarning ends the balance warning when a connection stops, since the
// status that would end it may never be handled. A closed wallet has reset
// its cache and reports nothing.
func (w *zecWallet) endWarning() {
	_, _, release, err := w.backend()
	if err != nil {
		return
	}
	defer release()
	c := &w.cache
	c.mtx.Lock()
	prev := c.warning
	c.warning = nil
	c.mtx.Unlock()
	if prev != nil {
		w.sendWarning(nil)
	}
}
