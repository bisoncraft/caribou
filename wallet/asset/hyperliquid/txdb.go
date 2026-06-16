// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/lexi"
	"github.com/bisoncraft/bosswallet/wallet/asset"
)

// ErrTxIsPending is returned by TxHistory when a RefID names a pending tx
// (BlockNumber == 0). Distinct from asset.CoinNotFoundError so callers can
// tell apart "tx does not exist" from "tx exists but is not yet confirmed".
var ErrTxIsPending = errors.New("ref tx is pending")

const (
	txsTableName       = "txs"
	blockIndexName     = "block"
	pendingIDIndexName = "pendingid"
	metaTableName      = "meta"

	lastScannedMSKey = "lastScannedMS"
	pendingIDKey     = "pendingID"
)

// dbTx wraps asset.WalletTransaction with the submission timestamp for
// pending-tx age tracking.
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

// hyperTxDB stores Hyperliquid wallet transactions using lexi.
type hyperTxDB struct {
	*lexi.DB
	txs            *lexi.Table
	meta           *lexi.Table
	blockIndex     *lexi.Index // ordered by BlockNumber; BlockNumber==0 for pending
	pendingIDIndex *lexi.Index // unique index on AdditionalData["pendingID"]
	log            util.Logger
}

func newTxDB(path string, log util.Logger) (*hyperTxDB, error) {
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

	meta, err := ldb.Table(metaTableName)
	if err != nil {
		return nil, err
	}

	blockIndex, err := txs.AddIndex(blockIndexName, func(_, v lexi.KV) ([]byte, error) {
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

	pendingIDIndex, err := txs.AddUniqueIndex(pendingIDIndexName, func(_, v lexi.KV) ([]byte, error) {
		wt, ok := v.(*dbTx)
		if !ok {
			return nil, fmt.Errorf("expected *dbTx, got %T", v)
		}
		pid := wt.AdditionalData[pendingIDKey]
		if pid == "" {
			return nil, lexi.ErrNotIndexed
		}
		return []byte(pid), nil
	})
	if err != nil {
		return nil, err
	}

	return &hyperTxDB{
		DB:             ldb,
		txs:            txs,
		meta:           meta,
		blockIndex:     blockIndex,
		pendingIDIndex: pendingIDIndex,
		log:            log,
	}, nil
}

func (db *hyperTxDB) storeTx(wt *dbTx) error {
	return db.txs.Set([]byte(wt.ID), wt, lexi.WithReplace())
}

func (db *hyperTxDB) getTx(txID string) (*dbTx, error) {
	var wt dbTx
	if err := db.txs.Get([]byte(txID), &wt); err != nil {
		if errors.Is(err, lexi.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &wt, nil
}

func (db *hyperTxDB) deleteTx(txID string) error {
	return db.txs.Delete([]byte(txID))
}

func (db *hyperTxDB) getTxByPendingID(pendingID string) (*dbTx, error) {
	var found *dbTx
	err := db.pendingIDIndex.Iterate([]byte(pendingID), func(it *lexi.Iter) error {
		wt := new(dbTx)
		if err := it.V(func(vB []byte) error {
			return wt.UnmarshalBinary(vB)
		}); err != nil {
			return err
		}
		found = wt
		return lexi.ErrEndIteration
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

func (db *hyperTxDB) getLastScannedMS() (int64, error) {
	b, err := db.meta.GetRaw([]byte(lastScannedMSKey))
	if err != nil {
		if errors.Is(err, lexi.ErrKeyNotFound) {
			return 0, nil
		}
		return 0, err
	}
	if len(b) != 8 {
		return 0, fmt.Errorf("invalid lastScannedMS length %d", len(b))
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

func (db *hyperTxDB) setLastScannedMS(lastScannedMS int64) error {
	if lastScannedMS < 0 {
		return fmt.Errorf("negative lastScannedMS %d", lastScannedMS)
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(lastScannedMS))
	return db.meta.Set([]byte(lastScannedMSKey), b, lexi.WithReplace())
}

func (db *hyperTxDB) highestPendingNonce() (uint64, error) {
	pending, err := db.getPendingTxs()
	if err != nil {
		return 0, err
	}

	var maxNonce uint64
	for _, tx := range pending {
		if tx.AdditionalData == nil {
			continue
		}
		nonceStr, ok := tx.AdditionalData["nonce"]
		if !ok {
			continue
		}
		nonce, err := strconv.ParseUint(nonceStr, 10, 64)
		if err != nil {
			continue
		}
		if nonce > maxNonce {
			maxNonce = nonce
		}
	}
	return maxNonce, nil
}

func (db *hyperTxDB) getTxs(req *asset.TxHistoryRequest) (*asset.TxHistoryResponse, error) {
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
		if wt == nil {
			return nil, asset.CoinNotFoundError
		}
		if wt.BlockNumber == 0 {
			return nil, ErrTxIsPending
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

func (db *hyperTxDB) getPendingTxs() ([]*dbTx, error) {
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
