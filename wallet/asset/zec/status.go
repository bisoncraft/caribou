// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
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
// count, a new chain tip of a synced wallet, and errors in the log. Every
// report is made under the guard of the backend, so a close cannot complete
// in between and an instance never reports after it closed. The peer count
// goes to core's callback, which is safe to call there: it only starts a
// goroutine, so it neither blocks nor calls back into the wallet on this
// goroutine.
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

// noteStatus updates the cache with a status, logs, and sends TipChange. It
// returns whether the peer count changed, the count, and whether the tip
// moved. The caller holds the guard of the backend.
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
	unrecorded := errors.Is(st.Err, zeclib.ErrOutputUnrecorded)
	warnUnrecorded := unrecorded && !c.unrecorded
	c.unrecorded = unrecorded
	var errText string
	if st.Err != nil {
		errText = st.Err.Error()
	}
	logErr := errText != c.errText && errText != "" && !unrecorded
	c.errText = errText
	c.mtx.Unlock()

	if warnUnrecorded {
		w.log.Warnf("Transparent funds are missing from the balance until the server serves them: %v", st.Err)
	}
	if logErr {
		w.log.Debugf("Sync status error: %v", st.Err)
	}
	if newTip != 0 && w.emit != nil {
		w.emit.TipChange(uint64(newTip))
	}
	return reportPeers, peers, tipMoved
}
