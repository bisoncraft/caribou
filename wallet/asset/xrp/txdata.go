// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bisoncraft/caribou/wallet/txdb"
)

const (
	metaTableName  = "meta"
	lastScannedKey = "lastScannedLedger"
)

// xrpTxData is the XRP-specific transaction data persisted in
// WalletTransaction.RawTx. The asset-agnostic fields live on the
// WalletTransaction itself; this carries only what the XRP wallet needs and the
// shared schema does not model.
type xrpTxData struct {
	// LastLedgerSequence is the ledger index after which a submitted transaction
	// can no longer be included. checkPendingTxs uses it to detect expiry. It is
	// 0 for received transactions.
	LastLedgerSequence uint32 `json:"lastLedgerSequence,omitempty"`
}

// xrpData decodes the XRP-specific data from a transaction's RawTx. A missing or
// empty RawTx yields the zero value.
func xrpData(wt *txdb.WalletTransaction) xrpTxData {
	var d xrpTxData
	if len(wt.RawTx) > 0 {
		_ = json.Unmarshal(wt.RawTx, &d)
	}
	return d
}

// setLastLedgerSequence encodes lls into wt.RawTx.
func setLastLedgerSequence(wt *txdb.WalletTransaction, lls uint32) {
	b, _ := json.Marshal(xrpTxData{LastLedgerSequence: lls})
	wt.RawTx = b
}

// getLastScannedLedger returns the last scanned ledger sequence, or 0 if unset.
func (w *xrpWallet) getLastScannedLedger() (uint64, error) {
	if w.metaTable == nil {
		return 0, nil
	}
	b, err := w.metaTable.Get([]byte(lastScannedKey))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(b) != 8 {
		return 0, fmt.Errorf("invalid lastScannedLedger length %d", len(b))
	}
	return binary.BigEndian.Uint64(b), nil
}

// setLastScannedLedger persists the last scanned ledger sequence.
func (w *xrpWallet) setLastScannedLedger(seq uint64) error {
	if w.metaTable == nil {
		return nil
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, seq)
	return w.metaTable.Set([]byte(lastScannedKey), b, txdb.WithReplace())
}
