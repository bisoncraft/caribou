// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"errors"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// newTestTxDB returns a BadgerTxDB backed by a master tx DB, for use in wallet
// tests. The underlying storage engine is selected by build tag (Badger on
// desktop, bbolt on mobile).
func newTestTxDB(t *testing.T) *BadgerTxDB {
	t.Helper()
	log := util.StdOutLogger("TXDB", util.LevelInfo)
	master, err := txdb.New(t.TempDir()+"/tx.db", log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	db, err := NewBadgerTxDB(master.NetworkTxDB(BipID), log)
	if err != nil {
		t.Fatalf("NewBadgerTxDB: %v", err)
	}
	return db
}

func ewt(id string, blockNumber uint64) *ExtendedWalletTx {
	return &ExtendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			Type:        txdb.Send,
			ID:          id,
			Amount:      1000,
			BlockNumber: blockNumber,
			Confirmed:   blockNumber > 0,
		},
	}
}

func TestTxDBStoreGetPending(t *testing.T) {
	db := newTestTxDB(t)

	// Empty store.
	r, err := db.GetTxs(&txdb.TxHistoryRequest{Past: true})
	if err != nil {
		t.Fatalf("GetTxs: %v", err)
	}
	if len(r.Txs) != 0 {
		t.Fatalf("expected 0 txs, got %d", len(r.Txs))
	}

	if err := db.StoreTx(ewt("confirmed", 100)); err != nil {
		t.Fatalf("StoreTx confirmed: %v", err)
	}
	if err := db.StoreTx(ewt("pending", 0)); err != nil {
		t.Fatalf("StoreTx pending: %v", err)
	}

	// GetTx hit and miss.
	got, err := db.GetTx("confirmed")
	if err != nil || got == nil || got.BlockNumber != 100 {
		t.Fatalf("GetTx confirmed = %v, %v", got, err)
	}
	if _, err := db.GetTx("nope"); !errors.Is(err, asset.CoinNotFoundError) {
		t.Fatalf("GetTx missing err = %v, want CoinNotFoundError", err)
	}

	// Only the unconfirmed tx is pending.
	pending, err := db.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "pending" {
		t.Fatalf("pending = %v", pending)
	}

	// History includes both.
	r, err = db.GetTxs(&txdb.TxHistoryRequest{Past: true})
	if err != nil {
		t.Fatalf("GetTxs: %v", err)
	}
	if len(r.Txs) != 2 {
		t.Fatalf("history len = %d, want 2", len(r.Txs))
	}

	// Remove.
	if err := db.RemoveTx("confirmed"); err != nil {
		t.Fatalf("RemoveTx: %v", err)
	}
	if _, err := db.GetTx("confirmed"); !errors.Is(err, asset.CoinNotFoundError) {
		t.Fatalf("GetTx after remove = %v, want CoinNotFoundError", err)
	}
}

func TestSetAndGetLastQuery(t *testing.T) {
	db := newTestTxDB(t)

	if _, err := db.GetLastReceiveTxQuery(); !errors.Is(err, ErrNeverQueried) {
		t.Fatalf("expected ErrNeverQueried, got %v", err)
	}

	const block = uint64(12345)
	if err := db.SetLastReceiveTxQuery(block); err != nil {
		t.Fatalf("SetLastReceiveTxQuery: %v", err)
	}
	got, err := db.GetLastReceiveTxQuery()
	if err != nil || got != block {
		t.Fatalf("GetLastReceiveTxQuery = %d, %v; want %d", got, err, block)
	}
}
