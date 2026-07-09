// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// ExtendedWalletTx wraps the asset-agnostic WalletTransaction with btc-specific
// tracking.
type ExtendedWalletTx struct {
	*txdb.WalletTransaction
}

const (
	metaTableName = "meta"
	lastQueryKey  = "lastReceiveTxQuery"
)

// ErrNeverQueried is returned by GetLastReceiveTxQuery before any query block
// has been recorded.
const ErrNeverQueried = util.ErrorKind("never queried")

// BadgerTxDB adapts the master transaction database (NetworkTxDB) to the method
// surface the btc wallet uses. The name is retained because the wallet refers
// to it widely; the underlying storage is the shared master tx DB (Badger on
// desktop, bbolt on mobile), not a btc-owned Badger instance.
type BadgerTxDB struct {
	db   txdb.NetworkTxDB
	meta txdb.StoreTable
	log  util.Logger
}

// NewBadgerTxDB wraps a NetworkTxDB handle. The auxiliary meta table holds the
// last receive-scan query height.
func NewBadgerTxDB(db txdb.NetworkTxDB, log util.Logger) (*BadgerTxDB, error) {
	mt, err := db.Table(metaTableName)
	if err != nil {
		return nil, err
	}
	return &BadgerTxDB{db: db, meta: mt, log: log}, nil
}

// StoreTx stores (or replaces) a submitted transaction.
func (db *BadgerTxDB) StoreTx(tx *ExtendedWalletTx) error {
	return db.db.StoreTx(tx.WalletTransaction)
}

// GetTxs returns transaction history. Every stored transaction is submitted.
func (db *BadgerTxDB) GetTxs(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	return db.db.GetTxs(req)
}

// GetTx returns a single transaction by ID, or asset.CoinNotFoundError if it is
// not present.
func (db *BadgerTxDB) GetTx(txID string) (*txdb.WalletTransaction, error) {
	wt, err := db.db.GetTx(txID)
	if err != nil {
		return nil, err
	}
	if wt == nil {
		return nil, asset.CoinNotFoundError
	}
	return wt, nil
}

// GetPendingTxs returns all transactions that have not yet been confirmed.
func (db *BadgerTxDB) GetPendingTxs() ([]*ExtendedWalletTx, error) {
	wts, err := db.db.GetPendingTxs()
	if err != nil {
		return nil, err
	}
	txs := make([]*ExtendedWalletTx, len(wts))
	for i, wt := range wts {
		txs[i] = &ExtendedWalletTx{WalletTransaction: wt}
	}
	return txs, nil
}

// RemoveTx removes a transaction from the database. It is not an error if the
// transaction does not exist.
func (db *BadgerTxDB) RemoveTx(txID string) error {
	return db.db.RemoveTx(txID)
}

// SetLastReceiveTxQuery stores the last block height the wallet was queried for
// receive transactions, so it knows how far back to scan after being offline.
func (db *BadgerTxDB) SetLastReceiveTxQuery(block uint64) error {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, block)
	return db.meta.Set([]byte(lastQueryKey), b, true)
}

// GetLastReceiveTxQuery retrieves the last receive-scan query height, returning
// ErrNeverQueried if none has been recorded.
func (db *BadgerTxDB) GetLastReceiveTxQuery() (uint64, error) {
	b, err := db.meta.Get([]byte(lastQueryKey))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return 0, ErrNeverQueried
	}
	if err != nil {
		return 0, err
	}
	if len(b) != 8 {
		return 0, fmt.Errorf("invalid lastReceiveTxQuery length %d", len(b))
	}
	return binary.BigEndian.Uint64(b), nil
}
