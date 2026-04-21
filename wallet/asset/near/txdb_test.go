package near

import (
	"fmt"
	"testing"

	"github.com/bisoncraft/meshwallet/wallet/asset"
	"github.com/bisoncraft/meshwallet/util"
)

func newTestTxDB(t *testing.T) *nearTxDB {
	t.Helper()
	dir := t.TempDir()
	log := util.StdOutLogger("TXDB", util.LevelTrace)
	db, err := newTxDB(dir, log)
	if err != nil {
		t.Fatalf("newTxDB error: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
	})
	return db
}

func makeDBTx(id string, blockNumber uint64, txType asset.TransactionType) *dbTx {
	return &dbTx{
		WalletTransaction: &asset.WalletTransaction{
			Type:        txType,
			ID:          id,
			Amount:      1000,
			Fees:        100,
			BlockNumber: blockNumber,
		},
		SubmissionTime: 1000,
	}
}

func TestTxDBStoreAndGet(t *testing.T) {
	db := newTestTxDB(t)

	tx := makeDBTx("tx1", 100, asset.Send)

	// Store.
	if err := db.storeTx(tx); err != nil {
		t.Fatalf("storeTx error: %v", err)
	}

	// Retrieve.
	got, err := db.getTx("tx1")
	if err != nil {
		t.Fatalf("getTx error: %v", err)
	}
	if got == nil {
		t.Fatal("getTx returned nil")
	}
	if got.ID != "tx1" {
		t.Fatalf("expected ID tx1, got %s", got.ID)
	}
	if got.BlockNumber != 100 {
		t.Fatalf("expected BlockNumber 100, got %d", got.BlockNumber)
	}
	if got.Amount != 1000 {
		t.Fatalf("expected Amount 1000, got %d", got.Amount)
	}

	// Not found.
	got, err = db.getTx("nonexistent")
	if err != nil {
		t.Fatalf("getTx error for missing tx: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for missing tx")
	}
}

func TestTxDBReplace(t *testing.T) {
	db := newTestTxDB(t)

	tx := makeDBTx("tx1", 0, asset.Send)
	if err := db.storeTx(tx); err != nil {
		t.Fatalf("storeTx error: %v", err)
	}

	// Update with confirmation.
	tx.BlockNumber = 200
	tx.Fees = 150
	if err := db.storeTx(tx); err != nil {
		t.Fatalf("storeTx (replace) error: %v", err)
	}

	got, err := db.getTx("tx1")
	if err != nil {
		t.Fatalf("getTx error: %v", err)
	}
	if got.BlockNumber != 200 {
		t.Fatalf("expected updated BlockNumber 200, got %d", got.BlockNumber)
	}
	if got.Fees != 150 {
		t.Fatalf("expected updated Fees 150, got %d", got.Fees)
	}
}

func TestTxDBGetTxsEmpty(t *testing.T) {
	db := newTestTxDB(t)

	resp, err := db.getTxs(&asset.TxHistoryRequest{N: 10, Past: true})
	if err != nil {
		t.Fatalf("getTxs error: %v", err)
	}
	if len(resp.Txs) != 0 {
		t.Fatalf("expected 0 txs, got %d", len(resp.Txs))
	}
	if resp.MoreAvailable {
		t.Fatal("expected MoreAvailable = false")
	}
}

func TestTxDBGetTxsOrdering(t *testing.T) {
	db := newTestTxDB(t)

	// Insert confirmed transactions with different block numbers.
	for i := uint64(1); i <= 5; i++ {
		tx := makeDBTx(fmt.Sprintf("tx%d", i), i*100, asset.Send)
		if err := db.storeTx(tx); err != nil {
			t.Fatalf("storeTx error: %v", err)
		}
	}

	// Default (no RefID): should be reverse order (most recent first).
	resp, err := db.getTxs(&asset.TxHistoryRequest{N: 10, Past: true})
	if err != nil {
		t.Fatalf("getTxs error: %v", err)
	}
	if len(resp.Txs) != 5 {
		t.Fatalf("expected 5 txs, got %d", len(resp.Txs))
	}
	// Highest block number first.
	if resp.Txs[0].ID != "tx5" {
		t.Fatalf("expected tx5 first, got %s", resp.Txs[0].ID)
	}
	if resp.Txs[4].ID != "tx1" {
		t.Fatalf("expected tx1 last, got %s", resp.Txs[4].ID)
	}
}

func TestTxDBGetTxsPagination(t *testing.T) {
	db := newTestTxDB(t)

	for i := uint64(1); i <= 10; i++ {
		tx := makeDBTx(fmt.Sprintf("tx%d", i), i*100, asset.Send)
		if err := db.storeTx(tx); err != nil {
			t.Fatalf("storeTx error: %v", err)
		}
	}

	// Fetch first 3 (most recent).
	resp, err := db.getTxs(&asset.TxHistoryRequest{N: 3, Past: true})
	if err != nil {
		t.Fatalf("getTxs error: %v", err)
	}
	if len(resp.Txs) != 3 {
		t.Fatalf("expected 3 txs, got %d", len(resp.Txs))
	}
	if !resp.MoreAvailable {
		t.Fatal("expected MoreAvailable = true")
	}
	if resp.Txs[0].ID != "tx10" {
		t.Fatalf("expected tx10 first, got %s", resp.Txs[0].ID)
	}
	if resp.Txs[2].ID != "tx8" {
		t.Fatalf("expected tx8 last in page, got %s", resp.Txs[2].ID)
	}

	// Paginate backwards from tx8.
	refID := "tx8"
	resp, err = db.getTxs(&asset.TxHistoryRequest{N: 3, RefID: &refID, Past: true})
	if err != nil {
		t.Fatalf("getTxs pagination error: %v", err)
	}
	if len(resp.Txs) != 3 {
		t.Fatalf("expected 3 txs in second page, got %d", len(resp.Txs))
	}
	// tx8 is the anchor and should be included.
	if resp.Txs[0].ID != "tx8" {
		t.Fatalf("expected tx8 first in second page, got %s", resp.Txs[0].ID)
	}
}

func TestTxDBPendingExcludedFromHistory(t *testing.T) {
	db := newTestTxDB(t)

	// Insert a pending tx (BlockNumber == 0).
	pending := makeDBTx("pending1", 0, asset.Send)
	if err := db.storeTx(pending); err != nil {
		t.Fatalf("storeTx error: %v", err)
	}

	// Insert a confirmed tx.
	confirmed := makeDBTx("confirmed1", 100, asset.Send)
	if err := db.storeTx(confirmed); err != nil {
		t.Fatalf("storeTx error: %v", err)
	}

	// History should only contain confirmed.
	resp, err := db.getTxs(&asset.TxHistoryRequest{N: 10, Past: true})
	if err != nil {
		t.Fatalf("getTxs error: %v", err)
	}
	if len(resp.Txs) != 1 {
		t.Fatalf("expected 1 confirmed tx, got %d", len(resp.Txs))
	}
	if resp.Txs[0].ID != "confirmed1" {
		t.Fatalf("expected confirmed1, got %s", resp.Txs[0].ID)
	}
}

func TestTxDBGetPendingTxs(t *testing.T) {
	db := newTestTxDB(t)

	// Insert mix of pending and confirmed.
	pending1 := makeDBTx("p1", 0, asset.Send)
	pending2 := makeDBTx("p2", 0, asset.Send)
	confirmed1 := makeDBTx("c1", 100, asset.Send)

	for _, tx := range []*dbTx{pending1, pending2, confirmed1} {
		if err := db.storeTx(tx); err != nil {
			t.Fatalf("storeTx error: %v", err)
		}
	}

	pending, err := db.getPendingTxs()
	if err != nil {
		t.Fatalf("getPendingTxs error: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending txs, got %d", len(pending))
	}

	ids := map[string]bool{pending[0].ID: true, pending[1].ID: true}
	if !ids["p1"] || !ids["p2"] {
		t.Fatalf("expected p1 and p2, got %v", ids)
	}
}

func TestTxDBPendingToConfirmed(t *testing.T) {
	db := newTestTxDB(t)

	// Store as pending.
	tx := makeDBTx("tx1", 0, asset.Send)
	if err := db.storeTx(tx); err != nil {
		t.Fatalf("storeTx error: %v", err)
	}

	// Should be in pending list.
	pending, err := db.getPendingTxs()
	if err != nil {
		t.Fatalf("getPendingTxs error: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}

	// Should NOT be in history (block index).
	resp, err := db.getTxs(&asset.TxHistoryRequest{N: 10, Past: true})
	if err != nil {
		t.Fatalf("getTxs error: %v", err)
	}
	if len(resp.Txs) != 0 {
		t.Fatalf("expected 0 confirmed txs, got %d", len(resp.Txs))
	}

	// Update to confirmed.
	tx.BlockNumber = 500
	if err := db.storeTx(tx); err != nil {
		t.Fatalf("storeTx (confirm) error: %v", err)
	}

	// Should no longer be pending.
	pending, err = db.getPendingTxs()
	if err != nil {
		t.Fatalf("getPendingTxs error: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after confirm, got %d", len(pending))
	}

	// Should now be in history.
	resp, err = db.getTxs(&asset.TxHistoryRequest{N: 10, Past: true})
	if err != nil {
		t.Fatalf("getTxs error: %v", err)
	}
	if len(resp.Txs) != 1 {
		t.Fatalf("expected 1 confirmed tx, got %d", len(resp.Txs))
	}
	if resp.Txs[0].ID != "tx1" {
		t.Fatalf("expected tx1, got %s", resp.Txs[0].ID)
	}
}

func TestTxDBIgnoreTypes(t *testing.T) {
	db := newTestTxDB(t)

	send := makeDBTx("send1", 100, asset.Send)
	recv := makeDBTx("recv1", 200, asset.Receive)

	if err := db.storeTx(send); err != nil {
		t.Fatalf("storeTx error: %v", err)
	}
	if err := db.storeTx(recv); err != nil {
		t.Fatalf("storeTx error: %v", err)
	}

	// Ignore sends.
	resp, err := db.getTxs(&asset.TxHistoryRequest{
		N:           10,
		Past:        true,
		IgnoreTypes: []asset.TransactionType{asset.Send},
	})
	if err != nil {
		t.Fatalf("getTxs error: %v", err)
	}
	if len(resp.Txs) != 1 {
		t.Fatalf("expected 1 tx after filtering, got %d", len(resp.Txs))
	}
	if resp.Txs[0].ID != "recv1" {
		t.Fatalf("expected recv1, got %s", resp.Txs[0].ID)
	}
}

func TestTxDBRefIDNotFound(t *testing.T) {
	db := newTestTxDB(t)

	refID := "nonexistent"
	_, err := db.getTxs(&asset.TxHistoryRequest{N: 10, RefID: &refID, Past: true})
	if err != asset.CoinNotFoundError {
		t.Fatalf("expected CoinNotFoundError, got %v", err)
	}
}

func TestTxDBRefIDPending(t *testing.T) {
	db := newTestTxDB(t)

	// Store a pending tx.
	tx := makeDBTx("pending", 0, asset.Send)
	if err := db.storeTx(tx); err != nil {
		t.Fatalf("storeTx error: %v", err)
	}

	// Using a pending tx as RefID should error.
	refID := "pending"
	_, err := db.getTxs(&asset.TxHistoryRequest{N: 10, RefID: &refID, Past: true})
	if err != asset.CoinNotFoundError {
		t.Fatalf("expected CoinNotFoundError for pending RefID, got %v", err)
	}
}
