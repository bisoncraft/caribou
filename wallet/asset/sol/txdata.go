// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bisoncraft/caribou/wallet/txdb"
)

const (
	// metaTableName is the single meta table for this network, shared by the
	// native wallet and every token wallet on it. Per-asset state is separated
	// by key (see metaKey) rather than by table: a table per token costs a write
	// transaction on each wallet's Connect just to assert the table exists, and
	// on the mobile engine that is an fsync apiece.
	metaTableName = "meta"

	// Meta field names. These are field names, not keys - every record is
	// stored under metaKey, which scopes the field to one asset. Two assets
	// storing the same field is normal (each token tracks its own scan cursor),
	// so the asset scope is what keeps them from overwriting each other.
	lastScannedSigField = "lastScannedSignature"
	beforeCursorField   = "beforeCursor"
	backfillUntilField  = "backfillUntil"
	rentExemptField     = "rentExemptReserve0"
)

// metaKey is the meta-table key for one field of one asset: assetID(4) followed
// by the field name. The native wallet scopes its fields under BipID, each token
// under its own asset ID.
func metaKey(assetID uint32, field string) []byte {
	k := make([]byte, 4+len(field))
	binary.BigEndian.PutUint32(k, assetID)
	copy(k[4:], field)
	return k
}

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
	mt := w.table()
	if mt == nil {
		return "", nil
	}
	b, err := mt.Get(metaKey(BipID, lastScannedSigField))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (w *Wallet) setLastScannedSignature(sig string) error {
	mt := w.table()
	if mt == nil || sig == "" {
		return nil
	}
	return mt.Set(metaKey(BipID, lastScannedSigField), []byte(sig), txdb.WithReplace())
}

func (w *Wallet) getBeforeCursor() (string, error) {
	mt := w.table()
	if mt == nil {
		return "", nil
	}
	b, err := mt.Get(metaKey(BipID, beforeCursorField))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (w *Wallet) setBeforeCursor(sig string) error {
	mt := w.table()
	if mt == nil {
		return nil
	}
	if sig == "" {
		return mt.Delete(metaKey(BipID, beforeCursorField))
	}
	return mt.Set(metaKey(BipID, beforeCursorField), []byte(sig), txdb.WithReplace())
}

func (w *Wallet) getBackfillUntil() (string, error) {
	mt := w.table()
	if mt == nil {
		return "", nil
	}
	b, err := mt.Get(metaKey(BipID, backfillUntilField))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (w *Wallet) setBackfillUntil(sig string) error {
	mt := w.table()
	if mt == nil {
		return nil
	}
	if sig == "" {
		return mt.Delete(metaKey(BipID, backfillUntilField))
	}
	return mt.Set(metaKey(BipID, backfillUntilField), []byte(sig), txdb.WithReplace())
}

func (w *Wallet) getCachedRentExemptReserve() (uint64, error) {
	mt := w.table()
	if mt == nil {
		return 0, nil
	}
	b, err := mt.Get(metaKey(BipID, rentExemptField))
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
	mt := w.table()
	if mt == nil {
		return nil
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return mt.Set(metaKey(BipID, rentExemptField), b, txdb.WithReplace())
}
