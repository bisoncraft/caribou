// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"encoding/json"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// ErrTxIsPending is returned by TxHistory when the caller-supplied RefID
// names a pending (unconfirmed) transaction. The RefID cannot be used as a
// pagination cursor in that state.
const ErrTxIsPending = util.ErrorKind("tx is pending")

const (
	pendingIDIndexName = "pendingid"
	metaTableName      = "meta"
	lastScannedMSKey   = "lastScannedMS"
	pendingIDKey       = "pendingID"
)

// hyperTxData is the HYPE-specific payload stored in WalletTransaction.RawTx.
type hyperTxData struct {
	SubmissionTime uint64 `json:"submissionTime"` // unix seconds; non-zero for pending sends
}

// hyperData decodes the HYPE-specific RawTx payload. Returns zero value on
// empty or malformed input.
func hyperData(wt *txdb.WalletTransaction) hyperTxData {
	var d hyperTxData
	if len(wt.RawTx) > 0 {
		_ = json.Unmarshal(wt.RawTx, &d)
	}
	return d
}

// setSubmissionTime encodes t into wt.RawTx as the HYPE-specific payload.
func setSubmissionTime(wt *txdb.WalletTransaction, t uint64) error {
	var err error
	wt.RawTx, err = json.Marshal(hyperTxData{SubmissionTime: t})
	return err
}

// pendingIDIndexKey is the index function for the unique pending-ID secondary
// index. It is shared between Connect and test helpers to prevent drift.
func pendingIDIndexKey(wt *txdb.WalletTransaction) ([]byte, error) {
	pid := wt.AdditionalData[pendingIDKey]
	if pid == "" {
		return nil, txdb.ErrNotIndexed
	}
	return []byte(pid), nil
}
