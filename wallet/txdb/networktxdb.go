// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package txdb

import "fmt"

// NetworkTxDB is the per-network transaction database handed to a wallet. A
// wallet uses it to manage its own transactions inside the shared master
// database. It offers a fixed asset-agnostic surface (which Core can serve
// transaction history from without any wallet involvement) plus a generic
// table/index facility for the wallet's auxiliary, network-private data.
type NetworkTxDB interface {
	// StoreTx inserts or updates a transaction, keyed by wt.ID. The wallet's
	// network-specific representation should be encoded into wt.RawTx.
	StoreTx(wt *WalletTransaction) error
	// StoreTxs inserts or updates many transactions in a single underlying
	// write. It is intended for bulk paths - an initial history sync or a
	// migration - where storing transactions one at a time would incur a
	// per-transaction fsync on mobile (bbolt).
	StoreTxs(wts []*WalletTransaction) error
	// GetTx returns a single transaction, or (nil, nil) if not found.
	GetTx(txID string) (*WalletTransaction, error)
	// GetTxs returns transaction history for this network, time-ordered.
	GetTxs(req *TxHistoryRequest) (*TxHistoryResponse, error)
	// GetPendingTxs returns this network's unconfirmed transactions: every tx
	// that is not yet finalized (Confirmed), not just those still in the
	// mempool. This matches the per-wallet tx DBs that wallets migrate from.
	GetPendingTxs() ([]*WalletTransaction, error)
	// RemoveTx deletes a transaction. It is not an error if it does not exist.
	RemoveTx(txID string) error

	// Table returns a network-private, indexable table for auxiliary wallet
	// data (e.g. eth bridge completions, btc secret nonces / last-query block).
	Table(name string) (StoreTable, error)
	// AddTxIndex adds a wallet-defined secondary index over this network's
	// transactions. The index function receives the full WalletTransaction
	// (including RawTx) so it may order by network-specific fields. The
	// returned TxIndex iterates the network's transactions in index order.
	AddTxIndex(name string, f func(*WalletTransaction) ([]byte, error)) (TxIndex, error)
	// AddUniqueTxIndex is like AddTxIndex but the index is unique: storing a
	// transaction whose index entry collides with an existing, different
	// transaction replaces (deletes) that existing transaction. Wallets use
	// this to key replacement on a network value rather than the primary key
	// (e.g. an Ethereum account nonce). Index functions must emit fixed-length
	// entries.
	AddUniqueTxIndex(name string, f func(*WalletTransaction) ([]byte, error)) (TxIndex, error)
}

// TxIndex is a wallet-defined secondary index over a network's transactions,
// returned by AddTxIndex/AddUniqueTxIndex.
type TxIndex interface {
	// Iterate visits the network's transactions in index order. prefix and seek
	// are in index-entry space (the bytes returned by the index function).
	Iterate(prefix []byte, f func(*WalletTransaction) error, reverse bool, seek []byte) error
}

// networkTxDB is the concrete NetworkTxDB, a thin view over TxDB scoped to one
// network.
type networkTxDB struct {
	db      *TxDB
	network uint32
}

var _ NetworkTxDB = (*networkTxDB)(nil)

func (n *networkTxDB) StoreTx(wt *WalletTransaction) error {
	return n.db.storeTx(n.network, wt)
}

func (n *networkTxDB) StoreTxs(wts []*WalletTransaction) error {
	return n.db.storeTxs(n.network, wts)
}

func (n *networkTxDB) GetTx(txID string) (*WalletTransaction, error) {
	return n.db.getTx(n.network, txID)
}

func (n *networkTxDB) GetTxs(req *TxHistoryRequest) (*TxHistoryResponse, error) {
	return n.db.getTxs(n.network, req)
}

func (n *networkTxDB) GetPendingTxs() ([]*WalletTransaction, error) {
	return n.db.getPendingTxs(n.network)
}

func (n *networkTxDB) RemoveTx(txID string) error {
	return n.db.removeTx(n.network, txID)
}

// auxTableName namespaces an auxiliary table per network so two networks'
// tables of the same name do not collide.
func (n *networkTxDB) auxTableName(name string) string {
	return fmt.Sprintf("net%d_%s", n.network, name)
}

func (n *networkTxDB) Table(name string) (StoreTable, error) {
	return n.db.store.Table(n.auxTableName(name))
}

// txIndexFunc wraps a wallet-supplied index function so the index lives on the
// shared txs table but is scoped to this network: records belonging to other
// networks are skipped.
func (n *networkTxDB) txIndexFunc(f func(*WalletTransaction) ([]byte, error)) IndexFunc {
	return func(_, v []byte) ([]byte, error) {
		wt, recNet, err := decodeRecord(v)
		if err != nil {
			return nil, err
		}
		if recNet != n.network {
			return nil, ErrNotIndexed
		}
		return f(wt)
	}
}

func (n *networkTxDB) AddTxIndex(name string, f func(*WalletTransaction) ([]byte, error)) (TxIndex, error) {
	idx, err := n.db.txs.AddIndex(n.auxTableName(name), n.txIndexFunc(f))
	if err != nil {
		return nil, err
	}
	return &txIndex{idx: idx}, nil
}

func (n *networkTxDB) AddUniqueTxIndex(name string, f func(*WalletTransaction) ([]byte, error)) (TxIndex, error) {
	idx, err := n.db.txs.AddUniqueIndex(n.auxTableName(name), n.txIndexFunc(f))
	if err != nil {
		return nil, err
	}
	return &txIndex{idx: idx}, nil
}

// txIndex wraps a StoreIndex so iteration decodes the stored record into a
// WalletTransaction, which the engine-internal record format would otherwise
// hide from wallets.
type txIndex struct {
	idx StoreIndex
}

func (i *txIndex) Iterate(prefix []byte, f func(*WalletTransaction) error, reverse bool, seek []byte) error {
	return i.idx.Iterate(prefix, func(it StoreIter) error {
		wt, err := decodeIter(it)
		if err != nil {
			return err
		}
		return f(wt)
	}, reverse, seek)
}
