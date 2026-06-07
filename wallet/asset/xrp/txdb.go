// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/lexi"
	"github.com/bisoncraft/bosswallet/wallet/asset"
)

const (
	txsTableName   = "txs"
	metaTableName  = "meta"
	blockIndexName = "block"
	lastScannedKey = "lastScannedLedger"
)

// dbTx wraps asset.WalletTransaction with additional tracking fields.
type dbTx struct {
	*asset.WalletTransaction
	SubmissionTime     uint64 `json:"submissionTime"`               // unix seconds
	LastLedgerSequence uint32 `json:"lastLedgerSequence,omitempty"` // 0 for receives
	Expired            bool   `json:"expired,omitempty"`            // true when LLS passed without inclusion
}

func (tx *dbTx) MarshalBinary() ([]byte, error) {
	return json.Marshal(tx)
}

func (tx *dbTx) UnmarshalBinary(b []byte) error {
	if err := json.Unmarshal(b, tx); err != nil {
		return err
	}
	if tx.WalletTransaction == nil {
		tx.WalletTransaction = &asset.WalletTransaction{}
	}
	return nil
}

// xrpTxDB stores XRP wallet transactions using lexi.
type xrpTxDB struct {
	*lexi.DB
	txs        *lexi.Table
	blockIndex *lexi.Index
	meta       *lexi.Table
	log        util.Logger
}

func newTxDB(path string, log util.Logger) (*xrpTxDB, error) {
	ldb, err := lexi.New(&lexi.Config{
		Path: path,
		Log:  log,
	})
	if err != nil {
		return nil, err
	}

	var initErr error
	defer func() {
		if initErr != nil {
			ldb.Close()
		}
	}()

	txs, initErr := ldb.Table(txsTableName)
	if initErr != nil {
		return nil, initErr
	}

	blockIndex, initErr := txs.AddIndex(blockIndexName, func(k, v lexi.KV) ([]byte, error) {
		wt, ok := v.(*dbTx)
		if !ok {
			return nil, fmt.Errorf("expected *dbTx, got %T", v)
		}
		// Composite key: block number (8 bytes big-endian) + txID bytes.
		// Including the txID makes each entry uniquely seekable within a block,
		// fixing pagination when multiple transactions share the same ledger index.
		entry := make([]byte, 8+len(wt.ID))
		binary.BigEndian.PutUint64(entry, wt.BlockNumber)
		copy(entry[8:], wt.ID)
		return entry, nil
	})
	if initErr != nil {
		return nil, initErr
	}

	meta, initErr := ldb.Table(metaTableName)
	if initErr != nil {
		return nil, initErr
	}

	return &xrpTxDB{
		DB:         ldb,
		txs:        txs,
		blockIndex: blockIndex,
		meta:       meta,
		log:        log,
	}, nil
}

func (db *xrpTxDB) storeTx(wt *dbTx) error {
	return db.txs.Set([]byte(wt.ID), wt, lexi.WithReplace())
}

func (db *xrpTxDB) getTx(txID string) (*dbTx, error) {
	var wt dbTx
	if err := db.txs.Get([]byte(txID), &wt); err != nil {
		if errors.Is(err, lexi.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &wt, nil
}

func (db *xrpTxDB) getTxs(req *asset.TxHistoryRequest) (*asset.TxHistoryResponse, error) {
	n := req.N
	if n == 0 {
		n = 50
	}

	var opts []lexi.IterationOption
	if req.Past || req.RefID == nil {
		opts = append(opts, lexi.WithReverse())
	}

	// skipRef tracks whether we need to skip the referenced transaction itself
	// at the start of iteration. Both forward and reverse seeks land on the
	// RefID entry (lexi appends lastDBID to reverse seeks), so we always skip
	// the first entry when a RefID is provided.
	skipRef := req.RefID != nil

	if req.RefID != nil {
		wt, err := db.getTx(*req.RefID)
		if err != nil {
			return nil, err
		}
		// Expired transactions (BlockNumber==0, Expired==true) are valid anchors;
		// only reject a zero-block entry that is not yet expired/rejected.
		if wt == nil || (wt.BlockNumber == 0 && !wt.Expired) {
			return nil, asset.CoinNotFoundError
		}
		// Composite seek: block number (8 bytes) + txID bytes positions the
		// iterator exactly on this transaction, not merely at the block boundary.
		entry := make([]byte, 8+len(wt.ID))
		binary.BigEndian.PutUint64(entry, wt.BlockNumber)
		copy(entry[8:], wt.ID)
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
		if skipRef {
			skipRef = false
			if strings.EqualFold(wt.ID, *req.RefID) {
				return nil // skip the anchor entry itself
			}
			// Seek landed elsewhere (shouldn't happen); fall through normally.
		}
		if wt.BlockNumber == 0 && !wt.Expired {
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

func (db *xrpTxDB) getLastScannedLedger() (uint64, error) {
	b, err := db.meta.GetRaw([]byte(lastScannedKey))
	if errors.Is(err, lexi.ErrKeyNotFound) {
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

func (db *xrpTxDB) setLastScannedLedger(seq uint64) error {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, seq)
	return db.meta.Set([]byte(lastScannedKey), b, lexi.WithReplace())
}

// getPendingTxs collects unconfirmed transactions (BlockNumber == 0).
func (db *xrpTxDB) getPendingTxs() ([]*dbTx, error) {
	var txs []*dbTx
	err := db.blockIndex.Iterate(nil, func(it *lexi.Iter) error {
		wt := new(dbTx)
		if err := it.V(func(vB []byte) error {
			return wt.UnmarshalBinary(vB)
		}); err != nil {
			return err
		}
		// Pending transactions are stored with BlockNumber=0, which sorts
		// before all confirmed transactions (BlockNumber>0) under the
		// big-endian composite key. The first confirmed entry encountered
		// means all remaining entries are also confirmed.
		if wt.BlockNumber > 0 {
			return lexi.ErrEndIteration
		}
		if wt.Expired {
			return nil
		}
		txs = append(txs, wt)
		return nil
	})
	return txs, err
}
