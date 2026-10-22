// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package txdb implements the master transaction database. It is owned by Core
// and provides each wallet a network-scoped NetworkTxDB handle through which the
// wallet stores and retrieves its own transactions. The asset-agnostic
// WalletTransaction is the unit of storage; a wallet's network-specific
// representation rides along inside it via WalletTransaction.RawTx.
//
// Storage is delegated to util/kvstore, which selects the underlying engine at
// build time: Badger (util/lexi) on desktop and bbolt (util/glossi) on mobile.
package txdb

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/bisoncraft/caribou/util/kvstore/engine"
)

// ErrUnknownTx is returned when a referenced transaction is not found.
const ErrUnknownTx = util.ErrorKind("unknown transaction")

// ErrMissingRefNetwork is returned by GetTxsByTicker when a request carries a
// RefID without the RefNetwork needed to resolve it to a primary key.
const ErrMissingRefNetwork = util.ErrorKind("ref network required with ref ID")

const (
	recordVersion = 0

	txsTableName = "txs"

	networkTimeIndexName  = "networkTime"
	timeIndexName         = "time"
	assetTimeIndexName    = "assetTime"
	tickerTimeIndexName   = "tickerTime"
	pendingIndexName      = "pending"
	assetPendingIndexName = "assetPending"
	finalityIndexName     = "finality"
	txTypeIndexName       = "txType"

	flagPending   byte = 1 << 0 // !Confirmed (unconfirmed; needs monitoring)
	flagConfirmed byte = 1 << 1 // finalized

	// maxPrealloc caps the capacity reserved for a history page before the
	// first record is read. TxHistoryRequest.N is decoded straight from the
	// HTTP request body, so it cannot be handed to make: a negative N panics
	// with "makeslice: cap out of range", and a large one asks for gigabytes
	// up front, which is an unrecoverable OOM rather than an error the caller
	// sees. N still bounds the page — the scan loops enforce it — this only
	// bounds the head start.
	maxPrealloc = 256
)

// PreallocTxs reserves capacity for a history page of n transactions, bounded
// by maxPrealloc. n <= 0 means unbounded per Core.TxHistory's documented
// contract, so it gets the ceiling too and grows by append. It is exported for
// the per-wallet transaction databases that keep their own history readers
// (eth), which take the same request-supplied n.
func PreallocTxs(n int) []*WalletTransaction {
	if n <= 0 || n > maxPrealloc {
		n = maxPrealloc
	}
	return make([]*WalletTransaction, 0, n)
}

// TxDB is the master transaction database. It is engine-agnostic; all
// transaction logic lives here while the byte-level storage is delegated to a
// Store.
type TxDB struct {
	store Store
	txs   StoreTable
	log   util.Logger

	// assetTicker resolves an asset ID to its ticker group (e.g. USDC.eth and
	// USDC.polygon both map to "USDC"), used to populate the tickerTime index at
	// write time. It is injected by Core, which owns the asset metadata, so the
	// txdb stays asset-agnostic. When nil, transactions are not ticker-indexed.
	// Set once at construction, before any wallet writes.
	assetTicker func(assetID uint32) string

	networkTimeIndex  StoreIndex
	timeIndex         StoreIndex
	assetTimeIndex    StoreIndex
	tickerTimeIndex   StoreIndex
	pendingIndex      StoreIndex
	assetPendingIndex StoreIndex
	finalityIndex     StoreIndex
	txTypeIndex       StoreIndex
}

// SetTickerResolver installs the asset-ID-to-ticker-group resolver used to
// populate the ticker index. It must be called before any transactions are
// stored (i.e. before wallets connect).
func (db *TxDB) SetTickerResolver(f func(assetID uint32) string) {
	db.assetTicker = f
}

// New opens the master transaction database at path using the build-selected
// storage engine.
func New(path string, log util.Logger) (*TxDB, error) {
	store, err := engine.New(path, log)
	if err != nil {
		return nil, err
	}
	db, err := newTxDB(store, log)
	if err != nil {
		if cerr := store.Close(); cerr != nil {
			log.Errorf("error closing store after newTxDB failure: %v", cerr)
		}
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
		{tickerTimeIndexName, indexTickerTime, &db.tickerTimeIndex},
		{pendingIndexName, indexPending, &db.pendingIndex},
		{assetPendingIndexName, indexAssetPending, &db.assetPendingIndex},
		{finalityIndexName, indexFinality, &db.finalityIndex},
		{txTypeIndexName, indexTxType, &db.txTypeIndex},
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

// txRecord is the value inserted into the txs table: the transaction plus the
// fixed metadata the index functions key on. The typed record (rather than
// pre-encoded bytes) is what gets passed to Set, so the index functions
// recover it with a type assertion and read fields directly instead of
// re-decoding the stored blob on every evaluation.
type txRecord struct {
	wt      *WalletTransaction
	network uint32
	assetID uint32
	effTime uint64
	flags   byte
	ticker  string // ticker group, "" when unresolved
}

func newTxRecord(wt *WalletTransaction, network uint32, effTime uint64, ticker string) *txRecord {
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
	return &txRecord{
		wt:      wt,
		network: network,
		assetID: wt.AssetID,
		effTime: effTime,
		flags:   flags,
		ticker:  ticker,
	}
}

// MarshalBinary satisfies encoding.BinaryMarshaler, encoding the stored form:
//
//	push0: JSON(WalletTransaction)   (RawTx excluded via json:"-")
//	push1: WalletTransaction.RawTx   (opaque network blob)
//
// No index metadata is stored: index entries are derived from the typed
// record at write time, seek cursors are resolved through StoreIndex.Seeker,
// and read paths recover network and asset attribution from the
// transaction's own NetworkID and AssetID fields.
func (r *txRecord) MarshalBinary() ([]byte, error) {
	js, err := json.Marshal(r.wt)
	if err != nil {
		return nil, fmt.Errorf("error marshaling transaction: %w", err)
	}
	return encode.BuildyBytes{recordVersion}.AddData(js).AddData(r.wt.RawTx), nil
}

// decodeRecord decodes a stored record into a WalletTransaction with RawTx
// restored.
func decodeRecord(b []byte) (*WalletTransaction, error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, fmt.Errorf("error decoding record: %w", err)
	}
	if ver != recordVersion {
		return nil, fmt.Errorf("unknown record version %d", ver)
	}
	if len(pushes) < 2 {
		return nil, fmt.Errorf("record has %d pushes, need 2", len(pushes))
	}
	wt := new(WalletTransaction)
	if err := json.Unmarshal(pushes[0], wt); err != nil {
		return nil, fmt.Errorf("error unmarshaling transaction: %w", err)
	}
	if len(pushes[1]) > 0 {
		wt.RawTx = append([]byte(nil), pushes[1]...)
	}
	return wt, nil
}

// --- agnostic index functions -------------------------------------------------

// txRecordV asserts a txs-table value to the *txRecord under insertion. Only
// txdb writes to the txs table, and it always Sets a *txRecord, so a mismatch
// is a programming error rather than a data condition.
func txRecordV(vI KV) (*txRecord, error) {
	rec, ok := vI.(*txRecord)
	if !ok {
		return nil, fmt.Errorf("expected *txRecord value, got %T", vI)
	}
	return rec, nil
}

// txRecordKV asserts both txs-table KVs: the primary key bytes (txKey) and the
// *txRecord under insertion.
func txRecordKV(kI, vI KV) ([]byte, *txRecord, error) {
	k, ok := kI.([]byte)
	if !ok {
		return nil, nil, fmt.Errorf("expected []byte key, got %T", kI)
	}
	rec, err := txRecordV(vI)
	return k, rec, err
}

// indexNetworkTime orders transactions within a network by effective time.
// Entry: network(4) || effTime(8) || id. The trailing id (the primary key with
// its network prefix stripped) makes every entry unique, so a RefID seek lands
// exactly even when transactions share an effective time.
func indexNetworkTime(kI, vI KV) ([]byte, error) {
	k, rec, err := txRecordKV(kI, vI)
	if err != nil {
		return nil, err
	}
	// k is the primary key, txKey(network, id) = network(4) || id.
	id := k[4:]
	e := make([]byte, 12+len(id))
	binary.BigEndian.PutUint32(e[0:], rec.network)
	binary.BigEndian.PutUint64(e[4:], rec.effTime)
	copy(e[12:], id)
	return e, nil
}

// indexTime orders all transactions, across networks, by effective time.
// Entry: effTime(8).
func indexTime(_, vI KV) ([]byte, error) {
	rec, err := txRecordV(vI)
	if err != nil {
		return nil, err
	}
	return encode.Uint64Bytes(rec.effTime), nil
}

// indexAssetTime orders transactions within an asset (token or base chain) by
// effective time. Entry: assetID(4) || effTime(8) || k. The trailing primary
// key makes every entry unique, so a RefID seek lands exactly even when
// transactions share an effective time (mirrors indexNetworkTime).
func indexAssetTime(kI, vI KV) ([]byte, error) {
	k, rec, err := txRecordKV(kI, vI)
	if err != nil {
		return nil, err
	}
	e := make([]byte, 12+len(k))
	binary.BigEndian.PutUint32(e[0:], rec.assetID)
	binary.BigEndian.PutUint64(e[4:], rec.effTime)
	copy(e[12:], k)
	return e, nil
}

// indexTickerTime orders transactions within a ticker group, across networks,
// by effective time. Entry: ticker || 0x00 || effTime(8) || id. The 0x00
// separator keeps a shorter ticker from prefix-matching a longer one (e.g. a
// "ETH" scan will not catch "ETHW"), and the trailing id makes every entry
// unique so pagination lands unambiguously. Records with an unresolved ("")
// ticker are skipped.
func indexTickerTime(kI, vI KV) ([]byte, error) {
	k, rec, err := txRecordKV(kI, vI)
	if err != nil {
		return nil, err
	}
	if rec.ticker == "" {
		return nil, ErrNotIndexed
	}
	// k is the primary key, txKey(network, id) = network(4) || id.
	id := k[4:]
	ticker := rec.ticker
	e := make([]byte, len(ticker)+1+8+len(id))
	copy(e, ticker)
	e[len(ticker)] = 0x00
	binary.BigEndian.PutUint64(e[len(ticker)+1:], rec.effTime)
	copy(e[len(ticker)+9:], id)
	return e, nil
}

// indexPending partitions transactions by pending state and network,
// time-ordered within each partition.
// Entry: pendingByte(1) || network(4) || effTime(8). The network component lets
// getPendingTxs scope its scan with a prefix instead of walking every network's
// pending transactions and discarding the non-matching ones.
func indexPending(_, vI KV) ([]byte, error) {
	rec, err := txRecordV(vI)
	if err != nil {
		return nil, err
	}
	e := make([]byte, 13)
	if rec.flags&flagPending != 0 {
		e[0] = 1
	}
	binary.BigEndian.PutUint32(e[1:], rec.network)
	binary.BigEndian.PutUint64(e[5:], rec.effTime)
	return e, nil
}

// indexAssetPending partitions transactions by pending state and asset
// (token or base chain), time-ordered within each partition. Entry:
// pendingByte(1) || assetID(4) || effTime(8). Mirrors indexPending, but
// scoped to an asset instead of a whole network, so a token wallet sharing
// its parent network's NetworkTxDB can query its own pending set without
// picking up sibling assets' records.
func indexAssetPending(_, vI KV) ([]byte, error) {
	rec, err := txRecordV(vI)
	if err != nil {
		return nil, err
	}
	e := make([]byte, 13)
	if rec.flags&flagPending != 0 {
		e[0] = 1
	}
	binary.BigEndian.PutUint32(e[1:], rec.assetID)
	binary.BigEndian.PutUint64(e[5:], rec.effTime)
	return e, nil
}

// indexFinality partitions transactions by finality (confirmed) state,
// time-ordered within each partition. Entry: confirmedByte(1) || effTime(8).
func indexFinality(_, vI KV) ([]byte, error) {
	rec, err := txRecordV(vI)
	if err != nil {
		return nil, err
	}
	e := make([]byte, 9)
	if rec.flags&flagConfirmed != 0 {
		e[0] = 1
	}
	binary.BigEndian.PutUint64(e[1:], rec.effTime)
	return e, nil
}

func indexTxType(_, vI KV) ([]byte, error) {
	rec, err := txRecordV(vI)
	if err != nil {
		return nil, err
	}
	e := make([]byte, 2)
	binary.BigEndian.PutUint16(e, uint16(rec.wt.Type))
	return e, nil
}

// --- network-scoped operations (used by networkTxDB) -------------------------

func txKey(network uint32, id string) []byte {
	return append(encode.Uint32Bytes(network), []byte(id)...)
}

// tickerFor resolves the ticker group for a transaction via the injected
// resolver, or "" when none is set. The resolver keys on the transaction's
// AssetID.
func (db *TxDB) tickerFor(wt *WalletTransaction) string {
	if db.assetTicker == nil {
		return ""
	}
	return db.assetTicker(wt.AssetID)
}

func (db *TxDB) storeTx(network uint32, wt *WalletTransaction) error {
	effTime := effectiveTime(wt, uint64(time.Now().Unix()))
	rec := newTxRecord(wt, network, effTime, db.tickerFor(wt))
	return db.txs.Set(txKey(network, wt.ID), rec, WithReplace())
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
		rec := newTxRecord(wt, network, effectiveTime(wt, now), db.tickerFor(wt))
		entries[i] = BatchEntry{K: txKey(network, wt.ID), V: rec}
	}
	return db.txs.SetBatch(entries, WithReplace())
}

func (db *TxDB) getTx(network uint32, id string) (*WalletTransaction, error) {
	raw, err := db.txs.Get(txKey(network, id))
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return decodeRecord(raw)
}

func (db *TxDB) removeTx(network uint32, id string) error {
	err := db.txs.Delete(txKey(network, id))
	if errors.Is(err, ErrKeyNotFound) {
		return nil
	}
	return err
}

// iterOpts converts the read paths' computed direction and seek position into
// IterationOptions.
func iterOpts(reverse bool, seek []byte) []IterationOption {
	opts := make([]IterationOption, 0, 2)
	if reverse {
		opts = append(opts, WithReverse())
	}
	if len(seek) > 0 {
		opts = append(opts, WithSeek(seek))
	}
	return opts
}

func (db *TxDB) getTxs(idx StoreIndex, prefix kvstore.KV, seekKey []byte, req *TxHistoryRequest) (*TxHistoryResponse, error) {
	n := req.N
	reverse := req.Past || req.RefID == nil

	txs := PreallocTxs(n)
	var moreAvailable bool
	ignore := req.IngoreTypesLookup()

	var seek []byte
	if len(seekKey) > 0 {
		var err error
		seek, err = idx.Seeker(seekKey)
		if err != nil {
			if errors.Is(err, ErrKeyNotFound) {
				return nil, ErrUnknownTx
			}
			return nil, fmt.Errorf("error getting seeker for key %v: %w", seekKey, err)
		}
	}

	err := idx.Iterate(prefix, func(it StoreIter) error {
		// Filter before the limit check: MoreAvailable must mean another kept
		// transaction exists, not merely another entry. An all-filtered tail
		// reported as more-available would have an inclusive-cursor client
		// refetching the same page forever.
		if len(ignore) > 0 {
			txTypeB, err := it.CrossIndex(txTypeIndexName)
			if err != nil {
				return err
			}
			if ignore[TransactionType(binary.BigEndian.Uint16(txTypeB))] {
				return nil
			}
		}

		if n > 0 && len(txs) == n {
			moreAvailable = true
			return ErrEndIteration
		}

		wt, err := decodeIter(it)
		if err != nil {
			return fmt.Errorf("failed to decode wallet transaction: %w", err)
		}

		txs = append(txs, wt)
		return nil
	}, iterOpts(reverse, seek)...)
	if err != nil {
		return nil, err
	}
	return &TxHistoryResponse{Txs: txs, MoreAvailable: moreAvailable}, nil
}

func (db *TxDB) getNetworkTxs(network uint32, req *TxHistoryRequest) (*TxHistoryResponse, error) {
	prefix := encode.Uint32Bytes(network)
	var seekKey []byte
	if req.RefID != nil {
		seekKey = txKey(network, *req.RefID)
	}
	return db.getTxs(db.networkTimeIndex, prefix, seekKey, req)
}

// getAssetTxs is like getTxs, but scoped to a single asset (assetID) within
// network rather than every transaction in the network. network is only
// needed to resolve a RefID to its primary-key record; the actual scan uses
// assetTimeIndex, scoped by assetID.
func (db *TxDB) getAssetTxs(network, assetID uint32, req *TxHistoryRequest) (*TxHistoryResponse, error) {
	prefix := encode.Uint32Bytes(assetID)
	var seekKey []byte
	if req.RefID != nil {
		seekKey = txKey(network, *req.RefID)
	}
	return db.getTxs(db.assetTimeIndex, prefix, seekKey, req)
}

// GetTxsByTicker returns transaction history for a ticker group across all of
// its networks, time-ordered, using the tickerTime index. The ticker must match
// the value produced by the resolver installed via SetTickerResolver.
//
// A RefID here cannot be resolved to a primary key from RefID alone: the ticker
// query spans networks and has no single network to rebuild network||id from.
// RefNetwork supplies it, so a RefID without a RefNetwork is an error
// (ErrMissingRefNetwork) rather than a scan of the whole group. The cursor
// record is looked up to seek straight to its ticker-index entry
// (ticker||0x00||effTime||id), and the RefID transaction is the first row of the
// page, matching getTxs. The common case — the most recent page, RefID nil — is
// a direct bounded scan. A RefID that names no transaction, or one outside this
// ticker's group, returns ErrUnknownTx, matching getTxs.
//
// Every stored transaction carries its own NetworkID and AssetID, so a merged
// cross-network result needs no per-read attribution.
func (db *TxDB) GetTxsByTicker(ticker string, req *TxHistoryRequest) (*TxHistoryResponse, error) {
	prefix := append([]byte(ticker), 0x00)
	var seekKey []byte
	if req.RefID != nil {
		if req.RefNetwork == nil {
			return nil, ErrMissingRefNetwork
		}
		seekKey = txKey(*req.RefNetwork, *req.RefID)
	}
	return db.getTxs(db.tickerTimeIndex, prefix, seekKey, req)
}

// getAssetPendingTxs is like getPendingTxs, but scoped to a single asset
// within a network instead of the whole network.
func (db *TxDB) getAssetPendingTxs(assetID uint32) ([]*WalletTransaction, error) {
	txs := make([]*WalletTransaction, 0)
	// The asset-pending index entry is pendingByte(1) || assetID(4) ||
	// effTime(8), so this 5-byte prefix scopes the scan to one asset's
	// pending partition.
	prefix := make([]byte, 5)
	prefix[0] = 1
	binary.BigEndian.PutUint32(prefix[1:], assetID)
	err := db.assetPendingIndex.Iterate(prefix, func(it StoreIter) error {
		wt, err := decodeIter(it)
		if err != nil {
			return err
		}
		txs = append(txs, wt)
		return nil
	})
	return txs, err
}

func (db *TxDB) getPendingTxs(network uint32) ([]*WalletTransaction, error) {
	txs := make([]*WalletTransaction, 0)
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
	})
	return txs, err
}

// decodeIter decodes the record at the iterator's current entry.
func decodeIter(it StoreIter) (wt *WalletTransaction, err error) {
	verr := it.V(func(b []byte) error {
		wt, err = decodeRecord(b)
		return err
	})
	if verr != nil {
		return nil, verr
	}
	return wt, err
}
