// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bisoncraft/bosswallet/wallet/txdb"
)

const (
	metaTableName         = "meta"
	lastScannedSigKey     = "lastScannedSignature"
	beforeCursorKey       = "beforeCursor"
	backfillUntilKey      = "backfillUntil"
	rentExemptReserve0Key = "rentExemptReserve0"
)

type solTxData struct {
	LastValidBlockHeight uint64 `json:"lastValidBlockHeight,omitempty"`
	SubmissionTime       uint64 `json:"submissionTime,omitempty"`
	Blockhash            string `json:"blockhash,omitempty"`
}

func txData(wt *txdb.WalletTransaction) solTxData {
	var d solTxData
	if len(wt.RawTx) > 0 {
		_ = json.Unmarshal(wt.RawTx, &d)
	}
	return d
}

func setTxData(wt *txdb.WalletTransaction, d solTxData) {
	b, _ := json.Marshal(d)
	wt.RawTx = b
}

func (w *Wallet) getLastScannedSignature() (string, error) {
	if w.metaTable == nil {
		return "", nil
	}
	b, err := w.metaTable.Get([]byte(lastScannedSigKey))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (w *Wallet) setLastScannedSignature(sig string) error {
	if w.metaTable == nil || sig == "" {
		return nil
	}
	return w.metaTable.Set([]byte(lastScannedSigKey), []byte(sig), true)
}

func (w *Wallet) getBeforeCursor() (string, error) {
	if w.metaTable == nil {
		return "", nil
	}
	b, err := w.metaTable.Get([]byte(beforeCursorKey))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (w *Wallet) setBeforeCursor(sig string) error {
	if w.metaTable == nil {
		return nil
	}
	if sig == "" {
		return w.metaTable.Delete([]byte(beforeCursorKey))
	}
	return w.metaTable.Set([]byte(beforeCursorKey), []byte(sig), true)
}

func (w *Wallet) getBackfillUntil() (string, error) {
	if w.metaTable == nil {
		return "", nil
	}
	b, err := w.metaTable.Get([]byte(backfillUntilKey))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (w *Wallet) setBackfillUntil(sig string) error {
	if w.metaTable == nil {
		return nil
	}
	if sig == "" {
		return w.metaTable.Delete([]byte(backfillUntilKey))
	}
	return w.metaTable.Set([]byte(backfillUntilKey), []byte(sig), true)
}

func (w *Wallet) getCachedRentExemptReserve() (uint64, error) {
	if w.metaTable == nil {
		return 0, nil
	}
	b, err := w.metaTable.Get([]byte(rentExemptReserve0Key))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(b) != 8 {
		return 0, fmt.Errorf("invalid cached rent reserve length %d", len(b))
	}
	return binary.BigEndian.Uint64(b), nil
}

func (w *Wallet) setCachedRentExemptReserve(v uint64) error {
	if w.metaTable == nil {
		return nil
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return w.metaTable.Set([]byte(rentExemptReserve0Key), b, true)
}
