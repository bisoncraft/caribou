// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/lexi"
)

const (
	txsTableName   = "txs"
	blockIndexName = "block"
)

// dbTx wraps asset.WalletTransaction with additional tracking
// fields for the NEAR wallet.
type dbTx struct {
	*asset.WalletTransaction
	SubmissionTime uint64 `json:"submissionTime"` // unix seconds
}

func (tx *dbTx) MarshalBinary() ([]byte, error) {
	return json.Marshal(tx)
}

func (tx *dbTx) UnmarshalBinary(b []byte) error {
	return json.Unmarshal(b, tx)
}

// nearTxDB is a transaction database for storing NEAR transactions.
type nearTxDB struct {
	*lexi.DB
	txs        *lexi.Table
	blockIndex *lexi.Index // confirmed txs, ordered by block number
	log        dex.Logger
}

func newTxDB(path string, log dex.Logger) (*nearTxDB, error) {
	ldb, err := lexi.New(&lexi.Config{
		Path: path,
		Log:  log,
	})
	if err != nil {
		return nil, err
	}

	txs, err := ldb.Table(txsTableName)
	if err != nil {
		return nil, err
	}

	blockIndex, err := txs.AddIndex(blockIndexName, func(k, v lexi.KV) ([]byte, error) {
		wt, ok := v.(*dbTx)
		if !ok {
			return nil, fmt.Errorf("expected *dbTx, got %T", v)
		}
		entry := make([]byte, 8)
		binary.BigEndian.PutUint64(entry, wt.BlockNumber)
		return entry, nil
	})
	if err != nil {
		return nil, err
	}

	return &nearTxDB{
		DB:         ldb,
		txs:        txs,
		blockIndex: blockIndex,
		log:        log,
	}, nil
}

// storeTx stores a transaction. Existing transactions with the same key are
// replaced.
func (db *nearTxDB) storeTx(wt *dbTx) error {
	return db.txs.Set([]byte(wt.ID), wt, lexi.WithReplace())
}

// getTx retrieves a single transaction by ID. Returns nil if not found.
func (db *nearTxDB) getTx(txID string) (*dbTx, error) {
	var wt dbTx
	if err := db.txs.Get([]byte(txID), &wt); err != nil {
		if errors.Is(err, lexi.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &wt, nil
}

// getTxs fetches confirmed transactions according to the request parameters.
func (db *nearTxDB) getTxs(req *asset.TxHistoryRequest) (*asset.TxHistoryResponse, error) {
	n := req.N

	var opts []lexi.IterationOption
	if req.Past || req.RefID == nil {
		opts = append(opts, lexi.WithReverse())
	}

	if req.RefID != nil {
		wt, err := db.getTx(*req.RefID)
		if err != nil {
			return nil, err
		}
		if wt == nil || wt.BlockNumber == 0 {
			return nil, asset.CoinNotFoundError
		}
		entry := make([]byte, 8)
		binary.BigEndian.PutUint64(entry, wt.BlockNumber)
		opts = append(opts, lexi.WithSeek(entry))
	}

	txs := make([]*asset.WalletTransaction, 0, n)
	var moreAvailable bool
	ignoreTypes := req.IngoreTypesLookup()

	iterFunc := func(it *lexi.Iter) error {
		wt := new(dbTx)
		if err := it.V(func(vB []byte) error {
			return wt.UnmarshalBinary(vB)
		}); err != nil {
			return err
		}
		if wt.BlockNumber == 0 {
			return nil
		}
		if ignoreTypes[wt.Type] {
			return nil
		}
		if n > 0 && len(txs) == n {
			moreAvailable = true
			return lexi.ErrEndIteration
		}
		txs = append(txs, wt.WalletTransaction)
		return nil
	}

	if err := db.blockIndex.Iterate(nil, iterFunc, opts...); err != nil {
		return nil, err
	}

	return &asset.TxHistoryResponse{
		Txs:           txs,
		MoreAvailable: moreAvailable,
	}, nil
}

// getPendingTxs iterates the block index forward, collecting entries with
// BlockNumber == 0 and stopping at the first confirmed transaction.
func (db *nearTxDB) getPendingTxs() ([]*dbTx, error) {
	var txs []*dbTx
	err := db.blockIndex.Iterate(nil, func(it *lexi.Iter) error {
		wt := new(dbTx)
		if err := it.V(func(vB []byte) error {
			return wt.UnmarshalBinary(vB)
		}); err != nil {
			return err
		}
		if wt.BlockNumber > 0 {
			return lexi.ErrEndIteration
		}
		txs = append(txs, wt)
		return nil
	})
	return txs, err
}

