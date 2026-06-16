// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package txdb implements the master transaction database. It is owned by Core
// and provides each wallet a network-scoped NetworkTxDB handle through which the
// wallet stores and retrieves its own transactions. The asset-agnostic
// WalletTransaction is the unit of storage; a wallet's network-specific
// representation rides along inside it via WalletTransaction.RawTx.
//
// The underlying storage engine is selected at build time (see engine_*.go):
// Badger (util/lexi) on desktop and bbolt (util/glossi) on mobile.
package txdb

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
)

// ErrUnknownTx is returned when a referenced transaction is not found.
const ErrUnknownTx = util.ErrorKind("unknown transaction")

const (
	recordVersion = 0

	txsTableName = "txs"

	networkTimeIndexName = "networkTime"
	timeIndexName        = "time"
	assetTimeIndexName   = "assetTime"
	pendingIndexName     = "pending"
	finalityIndexName    = "finality"

	// metaLen is the fixed-width index metadata stored as the third push of a
	// record: network(4) || assetID(4) || effTime(8) || flags(1).
	metaLen = 4 + 4 + 8 + 1

	flagPending   byte = 1 << 0 // !Confirmed (unconfirmed; needs monitoring)
	flagConfirmed byte = 1 << 1 // finalized
)

// TxDB is the master transaction database. It is engine-agnostic; all
// transaction logic lives here while the byte-level storage is delegated to a
// Store.
type TxDB struct {
	store Store
	txs   StoreTable
	log   util.Logger

	networkTimeIndex StoreIndex
	timeIndex        StoreIndex
	assetTimeIndex   StoreIndex
	pendingIndex     StoreIndex
	finalityIndex    StoreIndex
}

// New opens the master transaction database at path using the build-selected
// storage engine.
func New(path string, log util.Logger) (*TxDB, error) {
	store, err := newStore(path, log)
	if err != nil {
		return nil, err
	}
	db, err := newTxDB(store, log)
	if err != nil {
		store.Close()
		return nil, err
	}
	return db, nil
}

// newTxDB builds a TxDB over an already-constructed Store. It is used directly
// by tests to inject a specific engine.
func newTxDB(store Store, log util.Logger) (*TxDB, error) {
	txs, err := store.Table(txsTableName)
	if err != nil {
		return nil, fmt.Errorf("error opening txs table: %w", err)
	}
	db := &TxDB{store: store, txs: txs, log: log}

	for _, spec := range []struct {
		name string
		f    IndexFunc
		dst  *StoreIndex
	}{
		{networkTimeIndexName, indexNetworkTime, &db.networkTimeIndex},
		{timeIndexName, indexTime, &db.timeIndex},
		{assetTimeIndexName, indexAssetTime, &db.assetTimeIndex},
		{pendingIndexName, indexPending, &db.pendingIndex},
		{finalityIndexName, indexFinality, &db.finalityIndex},
	} {
		idx, err := txs.AddIndex(spec.name, spec.f)
		if err != nil {
			return nil, fmt.Errorf("error adding %s index: %w", spec.name, err)
		}
		*spec.dst = idx
	}
	return db, nil
}

// Connect ties the TxDB's shutdown to ctx: when ctx is canceled, the
// underlying storage engine is closed. The returned WaitGroup completes once
// shutdown is finished. Production callers should call Connect immediately
// after New and wait the WaitGroup as part of their shutdown sequence. Tests
// and one-shot tools may instead use Close directly.
func (db *TxDB) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	return db.store.Connect(ctx)
}

// Close closes the underlying store. Only safe when Connect has not been
// called; otherwise cancel the context passed to Connect and wait the
// returned WaitGroup.
func (db *TxDB) Close() error { return db.store.Close() }

// NetworkTxDB returns a transaction database handle scoped to a single network.
func (db *TxDB) NetworkTxDB(network uint32) NetworkTxDB {
	return &networkTxDB{db: db, network: network}
}

// effectiveTime returns the time used to order a transaction: its on-chain
// Timestamp when known, otherwise the provided fallback (store time), so that
// pending transactions still sort sensibly.
func effectiveTime(wt *WalletTransaction, fallback uint64) uint64 {
	if wt.Timestamp > 0 {
		return wt.Timestamp
	}
	return fallback
}

// encodeRecord encodes a stored transaction record:
//
//	push0: JSON(WalletTransaction)   (RawTx excluded via json:"-")
//	push1: WalletTransaction.RawTx   (opaque network blob)
//	push2: meta                      (network||assetID||effTime||flags)
func encodeRecord(wt *WalletTransaction, network uint32, effTime uint64) ([]byte, error) {
	assetID := network
	if wt.TokenID != nil {
		assetID = *wt.TokenID
	}
	var flags byte
	if !wt.Confirmed {
		// Pending means "not yet finalized, still needs monitoring", matching
		// the per-wallet tx DBs (eth/btc) whose getPendingTxs return every
		// unconfirmed tx, not just mempool ones. A mined-but-unconfirmed tx is
		// therefore still pending.
		flags |= flagPending
	}
	if wt.Confirmed {
		flags |= flagConfirmed
	}
	meta := make([]byte, metaLen)
	binary.BigEndian.PutUint32(meta[0:], network)
	binary.BigEndian.PutUint32(meta[4:], assetID)
	binary.BigEndian.PutUint64(meta[8:], effTime)
	meta[16] = flags

	js, err := json.Marshal(wt)
	if err != nil {
		return nil, fmt.Errorf("error marshaling transaction: %w", err)
	}
	return encode.BuildyBytes{recordVersion}.AddData(js).AddData(wt.RawTx).AddData(meta), nil
}

// decodeRecord decodes a stored record into a WalletTransaction (with RawTx
// restored) and returns the network the transaction belongs to.
func decodeRecord(b []byte) (*WalletTransaction, uint32, error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, 0, fmt.Errorf("error decoding record: %w", err)
	}
	if ver != recordVersion {
		return nil, 0, fmt.Errorf("unknown record version %d", ver)
	}
	if len(pushes) < 3 {
		return nil, 0, fmt.Errorf("record has %d pushes, need 3", len(pushes))
	}
	wt := new(WalletTransaction)
	if err := json.Unmarshal(pushes[0], wt); err != nil {
		return nil, 0, fmt.Errorf("error unmarshaling transaction: %w", err)
	}
	if len(pushes[1]) > 0 {
		wt.RawTx = append([]byte(nil), pushes[1]...)
	}
	meta := pushes[2]
	if len(meta) < metaLen {
		return nil, 0, fmt.Errorf("record meta too short: %d", len(meta))
	}
	return wt, binary.BigEndian.Uint32(meta[0:4]), nil
}

// recordMeta extracts the fixed-width index metadata from a record without
// unmarshaling the (heavier) JSON body, so index functions stay cheap.
func recordMeta(b []byte) (network, assetID uint32, effTime uint64, flags byte, err error) {
	_, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if len(pushes) < 3 {
		return 0, 0, 0, 0, fmt.Errorf("record has %d pushes, need 3", len(pushes))
	}
	meta := pushes[2]
	if len(meta) < metaLen {
		return 0, 0, 0, 0, fmt.Errorf("record meta too short: %d", len(meta))
	}
	network = binary.BigEndian.Uint32(meta[0:4])
	assetID = binary.BigEndian.Uint32(meta[4:8])
	effTime = binary.BigEndian.Uint64(meta[8:16])
	flags = meta[16]
	return
}

// --- agnostic index functions -------------------------------------------------

// indexNetworkTime orders transactions within a network by effective time.
// Entry: network(4) || effTime(8) || id. The trailing id (the primary key with
// its network prefix stripped) makes every entry unique, so a RefID seek lands
// exactly even when transactions share an effective time.
func indexNetworkTime(k, v []byte) ([]byte, error) {
	network, _, effTime, _, err := recordMeta(v)
	if err != nil {
		return nil, err
	}
	// k is the primary key, txKey(network, id) = network(4) || id.
	id := k[4:]
	e := make([]byte, 12+len(id))
	binary.BigEndian.PutUint32(e[0:], network)
	binary.BigEndian.PutUint64(e[4:], effTime)
	copy(e[12:], id)
	return e, nil
}

// indexTime orders all transactions, across networks, by effective time.
// Entry: effTime(8).
func indexTime(_, v []byte) ([]byte, error) {
	_, _, effTime, _, err := recordMeta(v)
	if err != nil {
		return nil, err
	}
	return encode.Uint64Bytes(effTime), nil
}

// indexAssetTime orders transactions within an asset (token or base chain) by
// effective time. Entry: assetID(4) || effTime(8).
func indexAssetTime(_, v []byte) ([]byte, error) {
	_, assetID, effTime, _, err := recordMeta(v)
	if err != nil {
		return nil, err
	}
	e := make([]byte, 12)
	binary.BigEndian.PutUint32(e[0:], assetID)
	binary.BigEndian.PutUint64(e[4:], effTime)
	return e, nil
}

// indexPending partitions transactions by pending state and network,
// time-ordered within each partition.
// Entry: pendingByte(1) || network(4) || effTime(8). The network component lets
// getPendingTxs scope its scan with a prefix instead of walking every network's
// pending transactions and discarding the non-matching ones.
func indexPending(_, v []byte) ([]byte, error) {
	network, _, effTime, flags, err := recordMeta(v)
	if err != nil {
		return nil, err
	}
	e := make([]byte, 13)
	if flags&flagPending != 0 {
		e[0] = 1
	}
	binary.BigEndian.PutUint32(e[1:], network)
	binary.BigEndian.PutUint64(e[5:], effTime)
	return e, nil
}

// indexFinality partitions transactions by finality (confirmed) state,
// time-ordered within each partition. Entry: confirmedByte(1) || effTime(8).
func indexFinality(_, v []byte) ([]byte, error) {
	_, _, effTime, flags, err := recordMeta(v)
	if err != nil {
		return nil, err
	}
	e := make([]byte, 9)
	if flags&flagConfirmed != 0 {
		e[0] = 1
	}
	binary.BigEndian.PutUint64(e[1:], effTime)
	return e, nil
}

// --- network-scoped operations (used by networkTxDB) -------------------------

func txKey(network uint32, id string) []byte {
	return append(encode.Uint32Bytes(network), []byte(id)...)
}

func (db *TxDB) storeTx(network uint32, wt *WalletTransaction) error {
	effTime := effectiveTime(wt, uint64(time.Now().Unix()))
	rec, err := encodeRecord(wt, network, effTime)
	if err != nil {
		return err
	}
	return db.txs.Set(txKey(network, wt.ID), rec, true)
}

// storeTxs writes many transactions in a single batch, collapsing what would
// be one fsync per tx on mobile (bbolt) into one. Used by bulk paths such as
// an initial history sync or a migration.
func (db *TxDB) storeTxs(network uint32, wts []*WalletTransaction) error {
	if len(wts) == 0 {
		return nil
	}
	now := uint64(time.Now().Unix())
	entries := make([]BatchEntry, len(wts))
	for i, wt := range wts {
		rec, err := encodeRecord(wt, network, effectiveTime(wt, now))
		if err != nil {
			return err
		}
		entries[i] = BatchEntry{K: txKey(network, wt.ID), V: rec}
	}
	return db.txs.SetBatch(entries, true)
}

func (db *TxDB) getTx(network uint32, id string) (*WalletTransaction, error) {
	raw, err := db.txs.Get(txKey(network, id))
	if err != nil {
		if err == ErrKeyNotFound {
			return nil, nil
		}
		return nil, err
	}
	wt, _, err := decodeRecord(raw)
	return wt, err
}

func (db *TxDB) removeTx(network uint32, id string) error {
	err := db.txs.Delete(txKey(network, id))
	if err == ErrKeyNotFound {
		return nil
	}
	return err
}

func (db *TxDB) getTxs(network uint32, req *TxHistoryRequest) (*TxHistoryResponse, error) {
	n := req.N
	prefix := encode.Uint32Bytes(network)
	reverse := req.Past || req.RefID == nil

	var seek []byte
	if req.RefID != nil {
		raw, err := db.txs.Get(txKey(network, *req.RefID))
		if err != nil {
			if err == ErrKeyNotFound {
				return nil, ErrUnknownTx
			}
			return nil, err
		}
		_, _, effTime, _, err := recordMeta(raw)
		if err != nil {
			return nil, err
		}
		// Seek to the exact networkTime index entry for this tx:
		// network(4) || effTime(8) || id. Including the id makes the cursor
		// unambiguous when several txs share an effective time (e.g. an
		// approve + initiate in the same block); a network||effTime-only seek
		// would land on the first sibling at that time and mis-page.
		id := []byte(*req.RefID)
		seek = make([]byte, 12+len(id))
		binary.BigEndian.PutUint32(seek[0:], network)
		binary.BigEndian.PutUint64(seek[4:], effTime)
		copy(seek[12:], id)
	}

	txs := make([]*WalletTransaction, 0, n)
	var moreAvailable bool
	ignore := req.IngoreTypesLookup()

	err := db.networkTimeIndex.Iterate(prefix, func(it StoreIter) error {
		wt, err := decodeIter(it)
		if err != nil {
			return err
		}
		if ignore[wt.Type] {
			return nil
		}
		if n > 0 && len(txs) == n {
			moreAvailable = true
			return ErrEndIteration
		}
		txs = append(txs, wt)
		return nil
	}, reverse, seek)
	if err != nil {
		return nil, err
	}
	return &TxHistoryResponse{Txs: txs, MoreAvailable: moreAvailable}, nil
}

func (db *TxDB) getPendingTxs(network uint32) ([]*WalletTransaction, error) {
	var txs []*WalletTransaction
	// The pending index entry is pendingByte(1) || network(4) || effTime(8), so
	// this 5-byte prefix scopes the scan to one network's pending partition.
	prefix := make([]byte, 5)
	prefix[0] = 1
	binary.BigEndian.PutUint32(prefix[1:], network)
	err := db.pendingIndex.Iterate(prefix, func(it StoreIter) error {
		wt, err := decodeIter(it)
		if err != nil {
			return err
		}
		txs = append(txs, wt)
		return nil
	}, false, nil)
	return txs, err
}

// decodeIter decodes the record at the iterator's current entry.
func decodeIter(it StoreIter) (*WalletTransaction, error) {
	wt, _, err := decodeIterWithNetwork(it)
	return wt, err
}

func decodeIterWithNetwork(it StoreIter) (wt *WalletTransaction, network uint32, err error) {
	verr := it.V(func(b []byte) error {
		wt, network, err = decodeRecord(b)
		return err
	})
	if verr != nil {
		return nil, 0, verr
	}
	return wt, network, err
}
