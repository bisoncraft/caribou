// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// extendedWalletTx is a txdb.WalletTransaction extended with additional
// fields used for tracking transactions. The persisted extension fields are
// encoded into txdb.WalletTransaction.RawTx (see ethTxData); the remaining
// fields are in-memory only.
type extendedWalletTx struct {
	*txdb.WalletTransaction
	BlockSubmitted uint64         `json:"blockSubmitted"`
	SubmissionTime uint64         `json:"timeStamp"` // seconds
	Nonce          *big.Int       `json:"nonce"`
	Receipt        *types.Receipt `json:"receipt,omitempty"`
	RawTx          util.Bytes     `json:"rawTx"`
	CallData       util.Bytes     `json:"callData"`
	// NonceReplacement is a transaction with the same nonce that was accepted
	// by the network, meaning this tx was not applied.
	NonceReplacement string `json:"nonceReplacement,omitempty"`
	// FeeReplacement is true if the NonceReplacement is the same tx as this
	// one, just with higher fees.
	FeeReplacement bool `json:"feeReplacement,omitempty"`
	// AssumedLost will be set to true if a transaction is assumed to be lost.
	// This typically requires feedback from the user in response to an
	// ActionRequiredNote.
	AssumedLost bool `json:"assumedLost,omitempty"`

	// The following fields are used for bridge completions that require a follow-up
	// transaction. This is currently only required for withdrawing POL from Polygon
	// POS to Ethereum. Hopefully (probably) no other bridges will require this and
	// eventually Polygon will upgrade their system to not require it in the
	// future as well.

	// PreviousBridgeCompletionID will be set to the ID of the initial bridge
	// completion. It is only populated for follow-up bridge completions.
	PreviousBridgeCompletionID string `json:"previousBridgeCompletionID,omitempty"`
	// BridgeFollowUpData is the data required to submit and verify a follow-up
	// bridge completion. It is only populated for follow-up bridge completions.
	BridgeFollowUpData util.Bytes `json:"bridgeFollowUpData,omitempty"`
	// RequiresFollowUp is true if the bridge requires a follow-up completion. It
	// is set to true for the initial bridge completion.
	RequiresFollowUp bool `json:"requiresFollowUp,omitempty"`

	txHash          common.Hash
	lastCheck       uint64
	savedToDB       bool
	lastBroadcast   time.Time
	lastFeeCheck    time.Time
	actionRequested bool
	actionIgnored   time.Time
	indexed         bool
}

const (
	allAssetIndexName              = "allAssets"
	assetIndexName                 = "asset"
	bridgeInitiationIndexName      = "bridgeinit"
	bridgeInitiationAssetIndexName = "bridgeinitasset"
	bridgeCompletionIndexName      = "bridgecompletion"
)

// ethTxData is the eth-specific transaction data persisted in
// txdb.WalletTransaction.RawTx. The asset-agnostic fields live on the
// WalletTransaction itself.
type ethTxData struct {
	BlockSubmitted             uint64         `json:"blockSubmitted"`
	SubmissionTime             uint64         `json:"timeStamp"`
	Nonce                      *big.Int       `json:"nonce"`
	Receipt                    *types.Receipt `json:"receipt,omitempty"`
	RawTx                      util.Bytes     `json:"rawTx"`
	CallData                   util.Bytes     `json:"callData"`
	NonceReplacement           string         `json:"nonceReplacement,omitempty"`
	FeeReplacement             bool           `json:"feeReplacement,omitempty"`
	AssumedLost                bool           `json:"assumedLost,omitempty"`
	PreviousBridgeCompletionID string         `json:"previousBridgeCompletionID,omitempty"`
	BridgeFollowUpData         util.Bytes     `json:"bridgeFollowUpData,omitempty"`
	RequiresFollowUp           bool           `json:"requiresFollowUp,omitempty"`
}

func (t *extendedWalletTx) age() time.Duration {
	return time.Since(time.Unix(int64(t.SubmissionTime), 0))
}

func (t *extendedWalletTx) tx() (*types.Transaction, error) {
	if t.IsRelay {
		return nil, fmt.Errorf("cannot get raw tx for relay tx")
	}
	tx := new(types.Transaction)
	return tx, tx.UnmarshalBinary(t.RawTx)
}

// encodeRawTx packs the eth-specific fields into wt.WalletTransaction.RawTx.
func (wt *extendedWalletTx) encodeRawTx() error {
	b, err := json.Marshal(&ethTxData{
		BlockSubmitted:             wt.BlockSubmitted,
		SubmissionTime:             wt.SubmissionTime,
		Nonce:                      wt.Nonce,
		Receipt:                    wt.Receipt,
		RawTx:                      wt.RawTx,
		CallData:                   wt.CallData,
		NonceReplacement:           wt.NonceReplacement,
		FeeReplacement:             wt.FeeReplacement,
		AssumedLost:                wt.AssumedLost,
		PreviousBridgeCompletionID: wt.PreviousBridgeCompletionID,
		BridgeFollowUpData:         wt.BridgeFollowUpData,
		RequiresFollowUp:           wt.RequiresFollowUp,
	})
	if err != nil {
		return err
	}
	wt.WalletTransaction.RawTx = b
	return nil
}

// decodeETHData decodes the eth-specific fields from a WalletTransaction's
// RawTx. An empty RawTx yields the zero value.
func decodeETHData(raw []byte) (ethTxData, error) {
	var d ethTxData
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &d); err != nil {
			return d, fmt.Errorf("error decoding eth tx data: %w", err)
		}
	}
	return d, nil
}

// decodeExtendedTx reconstructs an extendedWalletTx from a stored
// WalletTransaction.
func decodeExtendedTx(wt *txdb.WalletTransaction) (*extendedWalletTx, error) {
	d, err := decodeETHData(wt.RawTx)
	if err != nil {
		return nil, err
	}
	ewt := &extendedWalletTx{
		WalletTransaction:          wt,
		BlockSubmitted:             d.BlockSubmitted,
		SubmissionTime:             d.SubmissionTime,
		Nonce:                      d.Nonce,
		Receipt:                    d.Receipt,
		RawTx:                      d.RawTx,
		CallData:                   d.CallData,
		NonceReplacement:           d.NonceReplacement,
		FeeReplacement:             d.FeeReplacement,
		AssumedLost:                d.AssumedLost,
		PreviousBridgeCompletionID: d.PreviousBridgeCompletionID,
		BridgeFollowUpData:         d.BridgeFollowUpData,
		RequiresFollowUp:           d.RequiresFollowUp,
	}
	ewt.txHash = common.HexToHash(wt.ID)
	ewt.lastBroadcast = time.Unix(int64(ewt.SubmissionTime), 0)
	ewt.savedToDB = true
	return ewt, nil
}

type txDB interface {
	storeTx(wt *extendedWalletTx) error
	getTxs(tokenID *uint32, req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error)
	// getTx gets a single transaction. It is not an error if the tx is not known.
	// In that case, a nil tx is returned.
	getTx(txHash common.Hash) (*extendedWalletTx, error)
	// getPendingTxs returns any recent txs that are not confirmed, ordered
	// by nonce lowest-first.
	getPendingTxs() ([]*extendedWalletTx, error)
	getBridges(tokenID *uint32, n int, refID *common.Hash, past bool) ([]*txdb.WalletTransaction, error)
	getPendingBridges(tokenID *uint32) ([]*extendedWalletTx, error)
	getBridgeCompletions(initiationTxID string) ([]*extendedWalletTx, error)
}

// TxDB adapts the master transaction database (NetworkTxDB) to the eth wallet's
// nonce-ordered, bridge-aware transaction store. The agnostic WalletTransaction
// is stored by the master DB; the eth extension rides in its RawTx, and the
// custom indexes are built over it via the wallet index functions.
type TxDB struct {
	db txdb.NetworkTxDB

	allAssetIndex              txdb.TxIndex
	assetIndex                 txdb.TxIndex
	bridgeInitiationIndex      txdb.TxIndex
	bridgeInitiationAssetIndex txdb.TxIndex
	bridgeCompletionIndex      txdb.TxIndex

	baseChainID uint32
	log         util.Logger
}

var _ txDB = (*TxDB)(nil)

// allAssetIndexEntry is used to iterate over all transactions regardless of
// asset ID. The included data are:
//   - blockNumber: the block number of the transaction. If the transaction is pending
//     then the block number is set to ^uint64(0) in order to place pending
//     transactions at the end of the index.
//   - isRelay: 1 if the transaction is a relay tx, 0 otherwise.
//   - nonce: the nonce of the transaction, so that pending transactions and
//     transactions within the same block are ordered by nonce. Incoming
//     transfers have no nonce from our perspective, so the low 8 bytes of a
//     hash of the tx ID are used instead. Without this, two incoming
//     transfers landing in the same block would produce identical entries
//     and collide in the unique index.
func allAssetIndexEntry(wt *txdb.WalletTransaction) ([]byte, error) {
	d, err := decodeETHData(wt.RawTx)
	if err != nil {
		return nil, err
	}
	entry := make([]byte, 8+1+8)
	blockNumber := wt.BlockNumber
	if blockNumber == 0 {
		blockNumber = ^uint64(0)
	}
	isRelay := byte(0)
	if wt.IsRelay {
		isRelay = byte(1)
	}
	binary.BigEndian.PutUint64(entry[:8], blockNumber)
	entry[8] = isRelay
	if d.Nonce != nil {
		binary.BigEndian.PutUint64(entry[9:], d.Nonce.Uint64())
	} else {
		idHash := sha256.Sum256([]byte(wt.ID))
		copy(entry[9:], idHash[:8])
	}
	return entry, nil
}

// assetSpecificIndexEntry is used to iterate over all transactions for a
// specific asset ID. It is the same as allAssetIndexEntry, prepended with
// the asset ID.
func assetSpecificIndexEntry(wt *txdb.WalletTransaction, baseChainID uint32) ([]byte, error) {
	var assetID uint32 = baseChainID
	if wt.TokenID != nil {
		assetID = *wt.TokenID
	}
	allAssetIndexKey, err := allAssetIndexEntry(wt)
	if err != nil {
		return nil, err
	}
	assetKey := make([]byte, 4+len(allAssetIndexKey))
	binary.BigEndian.PutUint32(assetKey[:4], assetID)
	copy(assetKey[4:], allAssetIndexKey)
	return assetKey, nil
}

// bridgeIndexEntry generates an index entry for iterating over bridge initiation
// transactions. The index is sorted in chronological order based on the
// submission time of the bridge initiation transaction. Pending bridges appear
// at the end of the index. A pending flag byte separates pending from completed
// bridges while allowing both to be sorted by submission time.
func bridgeIndexEntry(wt *txdb.WalletTransaction) ([]byte, error) {
	d, err := decodeETHData(wt.RawTx)
	if err != nil {
		return nil, err
	}
	var pendingFlag byte = 0 // 0 for completed bridges
	if wt.BridgeCounterpartTx == nil || !wt.BridgeCounterpartTx.Complete {
		pendingFlag = 1 // 1 for pending bridges
	}

	entry := make([]byte, 9) // 1 byte for pending flag + 8 bytes for submission time
	entry[0] = pendingFlag
	binary.BigEndian.PutUint64(entry[1:9], d.SubmissionTime)
	return entry, nil
}

// bridgeAssetIndexEntry is used to iterate over bridge initiation transactions
// for a specific asset ID (base chain or token). It is the bridgeIndexEntry
// prepended with the asset ID.
func bridgeAssetIndexEntry(wt *txdb.WalletTransaction, baseChainID uint32) ([]byte, error) {
	var assetID uint32 = baseChainID
	if wt.TokenID != nil {
		assetID = *wt.TokenID
	}
	bridgeKey, err := bridgeIndexEntry(wt)
	if err != nil {
		return nil, err
	}
	assetKey := make([]byte, 4+len(bridgeKey))
	binary.BigEndian.PutUint32(assetKey[:4], assetID)
	copy(assetKey[4:], bridgeKey)
	return assetKey, nil
}

// NewTxDB creates an eth transaction store backed by the master transaction
// database handle supplied by Core.
func NewTxDB(db txdb.NetworkTxDB, log util.Logger, baseChainID uint32) (*TxDB, error) {
	if db == nil {
		return nil, errors.New("nil network tx DB")
	}
	allAssetIndex, err := db.AddUniqueTxIndex(allAssetIndexName, func(wt *txdb.WalletTransaction) ([]byte, error) {
		return allAssetIndexEntry(wt)
	})
	if err != nil {
		return nil, err
	}

	assetIndex, err := db.AddUniqueTxIndex(assetIndexName, func(wt *txdb.WalletTransaction) ([]byte, error) {
		return assetSpecificIndexEntry(wt, baseChainID)
	})
	if err != nil {
		return nil, err
	}

	bridgeInitiationIndex, err := db.AddTxIndex(bridgeInitiationIndexName, func(wt *txdb.WalletTransaction) ([]byte, error) {
		if wt.Type != txdb.InitiateBridge {
			return nil, txdb.ErrNotIndexed
		}
		return bridgeIndexEntry(wt)
	})
	if err != nil {
		return nil, err
	}

	bridgeInitiationAssetIndex, err := db.AddTxIndex(bridgeInitiationAssetIndexName, func(wt *txdb.WalletTransaction) ([]byte, error) {
		if wt.Type != txdb.InitiateBridge {
			return nil, txdb.ErrNotIndexed
		}
		return bridgeAssetIndexEntry(wt, baseChainID)
	})
	if err != nil {
		return nil, err
	}

	// bridgeCompletionIndex maps each completion tx to its initiation tx ID, so
	// completions can be looked up by initiation without a second table write.
	// Keying it on the txs table means a completion and its index entry are
	// written in the single StoreTx transaction, keeping the write atomic.
	bridgeCompletionIndex, err := db.AddTxIndex(bridgeCompletionIndexName, bridgeCompletionIndexEntry)
	if err != nil {
		return nil, err
	}

	return &TxDB{
		db:                         db,
		allAssetIndex:              allAssetIndex,
		assetIndex:                 assetIndex,
		bridgeInitiationIndex:      bridgeInitiationIndex,
		bridgeInitiationAssetIndex: bridgeInitiationAssetIndex,
		bridgeCompletionIndex:      bridgeCompletionIndex,
		baseChainID:                baseChainID,
		log:                        log,
	}, nil
}

// storeTx stores a transaction. If a transaction with the same hash or nonce
// already exists, it is replaced (the unique nonce index evicts a prior tx with
// the same nonce). A bridge completion is recorded purely through
// bridgeCompletionIndex, so the tx and its completion record are written in this
// single StoreTx transaction with no separate commit to lose on a crash.
func (db *TxDB) storeTx(wt *extendedWalletTx) error {
	if err := wt.encodeRawTx(); err != nil {
		return err
	}
	return db.db.StoreTx(wt.WalletTransaction)
}

// bridgeCompletionKey is the 32-byte index prefix for the completions of a
// given initiation tx.
func bridgeCompletionKey(initiationTxID string) []byte {
	h := common.HexToHash(initiationTxID)
	return h[:]
}

// bridgeCompletionIndexEntry indexes a bridge completion tx by its initiation
// tx ID, suffixed with the submission time so completions of the same
// initiation iterate in chronological order. Non-completion txs are skipped.
func bridgeCompletionIndexEntry(wt *txdb.WalletTransaction) ([]byte, error) {
	if wt.Type != txdb.CompleteBridge {
		return nil, txdb.ErrNotIndexed
	}
	if wt.BridgeCounterpartTx == nil || len(wt.BridgeCounterpartTx.IDs) < 1 {
		return nil, txdb.ErrNotIndexed
	}
	d, err := decodeETHData(wt.RawTx)
	if err != nil {
		return nil, err
	}
	key := bridgeCompletionKey(wt.BridgeCounterpartTx.IDs[0]) // 32 bytes
	entry := make([]byte, len(key)+8)
	copy(entry, key)
	binary.BigEndian.PutUint64(entry[len(key):], d.SubmissionTime)
	return entry, nil
}

// getTx gets a single transaction. It is not an error if the tx is not known.
// In that case, a nil tx is returned.
func (db *TxDB) getTx(txHash common.Hash) (*extendedWalletTx, error) {
	wt, err := db.db.GetTx(txHash.String())
	if err != nil {
		return nil, err
	}
	if wt == nil {
		return nil, nil
	}
	return decodeExtendedTx(wt)
}

// getTxs fetches n transactions in nonce order. See the txDB interface for the
// pagination contract.
func (db *TxDB) getTxs(assetIDPtr *uint32, req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	var n, past = req.N, req.Past
	var refID *common.Hash
	if req.RefID != nil {
		h := common.HexToHash(*req.RefID)
		if h == (common.Hash{}) {
			return nil, fmt.Errorf("invalid reference ID %q provided", *req.RefID)
		}
		refID = &h
	}

	idx := db.allAssetIndex
	var prefix []byte
	var assetID uint32
	if assetIDPtr != nil {
		assetID = *assetIDPtr
		idx = db.assetIndex
		prefix = encode.Uint32Bytes(assetID)
	}

	reverse := past || refID == nil
	var seek []byte
	if refID != nil {
		wt, err := db.getTx(*refID)
		if err != nil || wt == nil {
			return nil, asset.CoinNotFoundError
		}
		if assetIDPtr == nil {
			seek, err = allAssetIndexEntry(wt.WalletTransaction)
			if err != nil {
				return nil, err
			}
		} else {
			refTxAssetID := db.baseChainID
			if wt.TokenID != nil {
				refTxAssetID = *wt.TokenID
			}
			if refTxAssetID != assetID {
				return nil, fmt.Errorf("token ID mismatch: %d != %d", refTxAssetID, assetID)
			}
			seek, err = assetSpecificIndexEntry(wt.WalletTransaction, db.baseChainID)
			if err != nil {
				return nil, err
			}
		}
	}

	txs := txdb.PreallocTxs(n)
	var moreAvailable bool
	ignoreTypes := req.IngoreTypesLookup()
	err := idx.Iterate(prefix, func(wt *txdb.WalletTransaction) error {
		if ignoreTypes[wt.Type] {
			return nil
		}
		if n > 0 && len(txs) == n {
			moreAvailable = true
			return txdb.ErrEndIteration
		}
		txs = append(txs, wt)
		return nil
	}, iterOpts(reverse, seek)...)
	if err != nil {
		return nil, err
	}
	return &txdb.TxHistoryResponse{Txs: txs, MoreAvailable: moreAvailable}, nil
}

// getPendingTxs returns all unconfirmed transactions that have not been marked
// as lost.
func (db *TxDB) getPendingTxs() (txs []*extendedWalletTx, err error) {
	const numConfirmedTxsToCheck = 20
	var numConfirmedTxs int

	err = db.allAssetIndex.Iterate(nil, func(wt *txdb.WalletTransaction) error {
		ewt, err := decodeExtendedTx(wt)
		if err != nil {
			return err
		}
		if ewt.AssumedLost {
			return nil
		}
		if !ewt.Confirmed {
			numConfirmedTxs = 0
			txs = append(txs, ewt)
		} else {
			numConfirmedTxs++
			if numConfirmedTxs >= numConfirmedTxsToCheck {
				return txdb.ErrEndIteration
			}
		}
		return nil
	}, txdb.WithReverse())
	return
}

// getBridges fetches n bridge initiations. See the prior contract: chronological
// by submission time with pending bridges at the end.
func (db *TxDB) getBridges(assetID *uint32, n int, refID *common.Hash, past bool) ([]*txdb.WalletTransaction, error) {
	idx := db.bridgeInitiationIndex
	var prefix []byte
	if assetID != nil {
		idx = db.bridgeInitiationAssetIndex
		prefix = encode.Uint32Bytes(*assetID)
	}

	reverse := past || refID == nil
	var seek []byte
	if refID != nil {
		wt, err := db.getTx(*refID)
		if err != nil || wt == nil {
			return nil, asset.CoinNotFoundError
		}
		if wt.Type != txdb.InitiateBridge {
			return nil, fmt.Errorf("referenced transaction is not a bridge initiation")
		}
		if assetID != nil {
			refTxAssetID := db.baseChainID
			if wt.TokenID != nil {
				refTxAssetID = *wt.TokenID
			}
			if refTxAssetID != *assetID {
				return nil, fmt.Errorf("token ID mismatch: %d != %d", refTxAssetID, *assetID)
			}
			seek, err = bridgeAssetIndexEntry(wt.WalletTransaction, *assetID)
			if err != nil {
				return nil, err
			}
		} else {
			seek, err = bridgeIndexEntry(wt.WalletTransaction)
			if err != nil {
				return nil, err
			}
		}
	}

	txs := txdb.PreallocTxs(n)
	err := idx.Iterate(prefix, func(wt *txdb.WalletTransaction) error {
		txs = append(txs, wt)
		if n > 0 && len(txs) >= n {
			return txdb.ErrEndIteration
		}
		return nil
	}, iterOpts(reverse, seek)...)
	return txs, err
}

// iterOpts converts a computed direction and seek position into
// IterationOptions.
func iterOpts(reverse bool, seek []byte) []txdb.IterationOption {
	opts := make([]txdb.IterationOption, 0, 2)
	if reverse {
		opts = append(opts, txdb.WithReverse())
	}
	if len(seek) > 0 {
		opts = append(opts, txdb.WithSeek(seek))
	}
	return opts
}

// getPendingBridges returns all bridge initiation transactions that have not
// been completed and are not marked as lost.
func (db *TxDB) getPendingBridges(tokenID *uint32) (txs []*extendedWalletTx, err error) {
	idx := db.bridgeInitiationIndex
	var prefix []byte
	if tokenID != nil {
		idx = db.bridgeInitiationAssetIndex
		prefix = encode.Uint32Bytes(*tokenID)
	}

	err = idx.Iterate(prefix, func(wt *txdb.WalletTransaction) error {
		ewt, err := decodeExtendedTx(wt)
		if err != nil {
			return err
		}
		if ewt.AssumedLost {
			return nil
		}
		if ewt.BridgeCounterpartTx == nil || !ewt.BridgeCounterpartTx.Complete {
			if ewt.BridgeCounterpartTx != nil {
				ewt.BridgeCounterpartTx.Complete = false
			}
			txs = append(txs, ewt)
		}
		return nil
	}, txdb.WithReverse())
	return
}

// getBridgeCompletions retrieves all bridge completion transactions for the
// given bridge initiation transaction ID, in chronological order.
func (db *TxDB) getBridgeCompletions(initiationTxID string) ([]*extendedWalletTx, error) {
	var txs []*extendedWalletTx
	err := db.bridgeCompletionIndex.Iterate(bridgeCompletionKey(initiationTxID), func(wt *txdb.WalletTransaction) error {
		ewt, err := decodeExtendedTx(wt)
		if err != nil {
			return err
		}
		txs = append(txs, ewt)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return txs, nil
}
