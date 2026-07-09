//go:build !harness && !rpclive

package eth

import (
	"fmt"
	"math/big"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/davecgh/go-spew/spew"
	"github.com/ethereum/go-ethereum/common"
)

// newTestTxDBAtPath opens a master tx DB at path and returns an eth TxDB over
// it plus the master (so callers can close/reopen for persistence tests). The
// underlying engine is selected by build tag (Badger on desktop, bbolt on
// mobile).
func newTestTxDBAtPath(t *testing.T, path string) (*TxDB, *txdb.TxDB) {
	t.Helper()
	log := util.StdOutLogger("TXDB", util.LevelTrace)
	master, err := txdb.New(path, log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	db, err := NewTxDB(master.NetworkTxDB(BipID), log, BipID)
	if err != nil {
		t.Fatalf("NewTxDB: %v", err)
	}
	return db, master
}

// newTestTxDB returns an eth TxDB backed by a master tx DB that is closed on
// test cleanup.
func newTestTxDB(t *testing.T) *TxDB {
	t.Helper()
	db, master := newTestTxDBAtPath(t, t.TempDir()+"/tx.db")
	t.Cleanup(func() { master.Close() })
	return db
}

func TestTxDB(t *testing.T) {
	t.Parallel()
	dbPath := t.TempDir() + "/tx.db"

	// Grab these for the tx generation utilities
	_, eth, node, shutdown := tassetWallet(BipID)
	shutdown()

	txHistoryStore, master := newTestTxDBAtPath(t, dbPath)

	r, err := txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{Past: true})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	if len(r.Txs) != 0 {
		t.Fatalf("expected 0 txs but got %d", len(r.Txs))
	}

	newTx := func(nonce uint64) *extendedWalletTx {
		return eth.extendedTx(&genTxResult{
			tx:     node.newTransaction(nonce, big.NewInt(1)),
			txType: txdb.Send,
			amt:    1,
		})
	}

	wt1 := newTx(1)
	wt1.Confirmed = true
	wt1.TokenID = &usdcEthID
	wt2 := newTx(2)
	wt3 := newTx(3)
	wt4 := newTx(4)

	err = txHistoryStore.storeTx(wt1)
	if err != nil {
		t.Fatalf("error storing tx: %v", err)
	}

	r, err = txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{Past: true})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	expectedTxs := []*txdb.WalletTransaction{wt1.WalletTransaction}
	if !reflect.DeepEqual(expectedTxs, r.Txs) {
		t.Fatalf("expected txs %+v but got %+v", expectedTxs, r.Txs)
	}

	err = txHistoryStore.storeTx(wt2)
	if err != nil {
		t.Fatalf("error storing tx: %v", err)
	}
	r, err = txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{Past: true})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	expectedTxs = []*txdb.WalletTransaction{wt2.WalletTransaction, wt1.WalletTransaction}
	if !reflect.DeepEqual(expectedTxs, r.Txs) {
		t.Fatalf("expected txs %s but got %s", spew.Sdump(expectedTxs), spew.Sdump(r.Txs))
	}

	err = txHistoryStore.storeTx(wt3)
	if err != nil {
		t.Fatalf("error storing tx: %v", err)
	}
	r, err = txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{N: 2, Past: true})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	expectedTxs = []*txdb.WalletTransaction{wt3.WalletTransaction, wt2.WalletTransaction}
	if !reflect.DeepEqual(expectedTxs, r.Txs) {
		t.Fatalf("expected txs %+v but got %+v", expectedTxs, r.Txs)
	}

	s := wt2.txHash.String()
	r, err = txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{RefID: &s, Past: true})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	expectedTxs = []*txdb.WalletTransaction{wt2.WalletTransaction, wt1.WalletTransaction}
	if !reflect.DeepEqual(expectedTxs, r.Txs) {
		t.Fatalf("expected txs %+v but got %+v", expectedTxs, r.Txs)
	}

	s = wt2.txHash.String()
	r, err = txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{RefID: &s})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	expectedTxs = []*txdb.WalletTransaction{wt2.WalletTransaction, wt3.WalletTransaction}
	if !reflect.DeepEqual(expectedTxs, r.Txs) {
		t.Fatalf("expected txs %+v but got %+v", expectedTxs, r.Txs)
	}

	allTxs := []*txdb.WalletTransaction{wt4.WalletTransaction, wt3.WalletTransaction, wt2.WalletTransaction, wt1.WalletTransaction}

	// Update same tx with new fee
	wt4.Fees = 300
	err = txHistoryStore.storeTx(wt4)
	if err != nil {
		t.Fatalf("error storing tx: %v", err)
	}
	r, err = txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	if !reflect.DeepEqual(allTxs, r.Txs) {
		t.Fatalf("expected txs %s but got %s", spew.Sdump(allTxs), spew.Sdump(r.Txs))
	}
	master.Close()

	txHistoryStore, master = newTestTxDBAtPath(t, dbPath)
	defer master.Close()

	r, err = txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	if !reflect.DeepEqual(allTxs, r.Txs) {
		t.Fatalf("expected txs %+v but got %+v", expectedTxs, r.Txs)
	}

	unconfirmedTxs, err := txHistoryStore.getPendingTxs()
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	expectedUnconfirmedTxs := []*extendedWalletTx{wt4, wt3, wt2}
	compareTxs := func(txs0, txs1 []*extendedWalletTx) bool {
		if len(txs0) != len(txs1) {
			return false
		}
		for i, tx0 := range txs0 {
			tx1 := txs1[i]
			n0, n1 := tx0.Nonce, tx1.Nonce
			tx0.Nonce, tx1.Nonce = nil, nil
			eq := reflect.DeepEqual(tx0.WalletTransaction, tx1.WalletTransaction)
			tx0.Nonce, tx1.Nonce = n0, n1
			if !eq {
				return false
			}
		}
		return true
	}
	if !compareTxs(expectedUnconfirmedTxs, unconfirmedTxs) {
		t.Fatalf("expected txs:\n%s\n\nbut got:\n%s", spew.Sdump(expectedUnconfirmedTxs), spew.Sdump(unconfirmedTxs))
	}

	r, err = txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	if !reflect.DeepEqual(allTxs, r.Txs) {
		t.Fatalf("expected txs %+v but got %+v", expectedTxs, r.Txs)
	}

	r, err = txHistoryStore.getTxs(&usdcEthID, &txdb.TxHistoryRequest{})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	expectedTxs = []*txdb.WalletTransaction{wt1.WalletTransaction}
	if !reflect.DeepEqual(expectedTxs, r.Txs) {
		t.Fatalf("expected txs %+v but got %+v", expectedTxs, r.Txs)
	}
	r, err = txHistoryStore.getTxs(&usdcEthID, &txdb.TxHistoryRequest{IgnoreTypes: []txdb.TransactionType{txdb.Send}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Txs) != 0 {
		t.Fatal("expected no tx with type filter on, but got some")
	}
}

func TestTxDB_relay(t *testing.T) {
	t.Parallel()
	// Grab these for the tx generation utilities
	_, eth, node, shutdown := tassetWallet(BipID)
	shutdown()

	txHistoryStore := newTestTxDB(t)

	newTx := func(nonce uint64, blockNumber uint64, isRelay bool) *extendedWalletTx {
		tx := eth.extendedTx(&genTxResult{
			tx:     node.newTransaction(nonce, big.NewInt(1)),
			txType: txdb.Send,
			amt:    1,
		})
		tx.BlockNumber = blockNumber
		tx.IsRelay = isRelay
		tx.ID = fmt.Sprintf("%d", rand.Intn(1000000))
		if blockNumber > 0 {
			tx.Confirmed = true
		}
		return tx
	}

	wt1 := newTx(1, 100, false)
	wt2 := newTx(2, 101, false)
	wt2Relay := newTx(1, 101, true)
	wt3 := newTx(3, 102, false)
	wt4 := newTx(4, 103, false)

	for _, wt := range []*extendedWalletTx{wt1, wt2, wt2Relay, wt3, wt4} {
		err := txHistoryStore.storeTx(wt)
		if err != nil {
			t.Fatalf("error storing tx: %v", err)
		}
	}

	expectedTxs := []*txdb.WalletTransaction{wt4.WalletTransaction, wt3.WalletTransaction, wt2Relay.WalletTransaction, wt2.WalletTransaction, wt1.WalletTransaction}
	r, err := txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{
		N:           0,
		RefID:       nil,
		Past:        true,
		IgnoreTypes: nil,
	})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	if !reflect.DeepEqual(expectedTxs, r.Txs) {
		t.Fatalf("expected txs %+v but got %+v", expectedTxs, r.Txs)
	}
}

func TestTxDBReplaceNonce(t *testing.T) {
	t.Parallel()
	_, eth, node, shutdown := tassetWallet(BipID)
	shutdown()

	txHistoryStore := newTestTxDB(t)

	newTx := func(nonce uint64) *extendedWalletTx {
		return eth.extendedTx(&genTxResult{
			tx:     node.newTransaction(nonce, big.NewInt(1)),
			txType: txdb.Send,
			amt:    1,
		})
	}

	wt1 := newTx(1)
	wt2 := newTx(1)

	err := txHistoryStore.storeTx(wt1)
	if err != nil {
		t.Fatalf("error storing tx: %v", err)
	}

	err = txHistoryStore.storeTx(wt2)
	if err != nil {
		t.Fatalf("error storing tx: %v", err)
	}

	tx, err := txHistoryStore.getTx(wt1.txHash)
	if err != nil {
		t.Fatalf("error retrieving tx: %v", err)
	}
	if tx != nil {
		t.Fatalf("expected nil tx but got %+v", tx)
	}

	r, err := txHistoryStore.getTxs(nil, &txdb.TxHistoryRequest{})
	if err != nil {
		t.Fatalf("error retrieving txs: %v", err)
	}
	if len(r.Txs) != 1 {
		t.Fatalf("expected 1 tx but got %d", len(r.Txs))
	}
	if r.Txs[0].ID != wt2.ID {
		t.Fatalf("expected tx %s but got %s", wt2.ID, r.Txs[0].ID)
	}
}

// TestTxDBCorruptRawTxDoesNotEvict ensures that a transaction whose eth RawTx
// extension is undecodable fails to store rather than silently producing a
// nonce-0 unique-index key that evicts an unrelated transaction.
func TestTxDBCorruptRawTxDoesNotEvict(t *testing.T) {
	t.Parallel()
	_, eth, node, shutdown := tassetWallet(BipID)
	shutdown()

	txHistoryStore := newTestTxDB(t)

	// A normal tx with nonce 0.
	good := eth.extendedTx(&genTxResult{
		tx:     node.newTransaction(0, big.NewInt(1)),
		txType: txdb.Send,
		amt:    1,
	})
	if err := txHistoryStore.storeTx(good); err != nil {
		t.Fatalf("error storing good tx: %v", err)
	}

	// A tx whose eth RawTx extension is undecodable. Computing its unique
	// nonce-index entry decodes RawTx; a decode failure must fail the store
	// instead of yielding a nonce-0 key that evicts `good` (which is also
	// nonce 0) via the unique-index replace path.
	bad := &txdb.WalletTransaction{
		Type:  txdb.Send,
		ID:    common.HexToHash("0xdead").String(),
		RawTx: []byte("{ not valid json"),
	}
	if err := txHistoryStore.db.StoreTx(bad); err == nil {
		t.Fatal("expected error storing tx with corrupt eth RawTx, got nil")
	}

	// The good tx must still be present (not evicted by the failed store).
	got, err := txHistoryStore.getTx(good.txHash)
	if err != nil {
		t.Fatalf("error retrieving good tx: %v", err)
	}
	if got == nil {
		t.Fatal("good tx was evicted by a failed corrupt-RawTx store")
	}
}

func TestTxDB_getUnknownTx(t *testing.T) {
	t.Parallel()
	txHistoryStore := newTestTxDB(t)

	tx, err := txHistoryStore.getTx(common.Hash{0x01})
	if err != nil {
		t.Fatalf("error retrieving tx: %v", err)
	}
	if tx != nil {
		t.Fatalf("expected nil tx but got %+v", tx)
	}
}

func TestTxDB_GetBridges(t *testing.T) {
	t.Parallel()
	db := newTestTxDB(t)

	now := time.Now()
	txs := []*extendedWalletTx{
		// Completed bridge, older submission time
		{
			WalletTransaction: &txdb.WalletTransaction{
				ID:   "0x1111111111111111111111111111111111111111111111111111111111111111",
				Type: txdb.InitiateBridge,
				BridgeCounterpartTx: &txdb.BridgeCounterpartTx{
					IDs:      []string{"0x1111111111111111111111111111111111111111111111111111111111111112"},
					Complete: true,
				},
			},
			Nonce:          big.NewInt(1),
			SubmissionTime: uint64(now.Add(-24 * time.Hour).Unix()),
		},
		// Completed bridge, newer submission time
		{
			WalletTransaction: &txdb.WalletTransaction{
				ID:   "0x2222222222222222222222222222222222222222222222222222222222222222",
				Type: txdb.InitiateBridge,
				BridgeCounterpartTx: &txdb.BridgeCounterpartTx{
					IDs:      []string{"0x2222222222222222222222222222222222222222222222222222222222222223"},
					Complete: true,
				},
			},
			Nonce:          big.NewInt(2),
			SubmissionTime: uint64(now.Add(-20 * time.Hour).Unix()),
		},
		// Pending bridge, older
		{
			WalletTransaction: &txdb.WalletTransaction{
				ID:   "0x3333333333333333333333333333333333333333333333333333333333333333",
				Type: txdb.InitiateBridge,
				BridgeCounterpartTx: &txdb.BridgeCounterpartTx{
					Complete: false,
				},
			},
			Nonce:          big.NewInt(3),
			SubmissionTime: uint64(now.Add(-18 * time.Hour).Unix()),
		},
		// Pending bridge, newer
		{
			WalletTransaction: &txdb.WalletTransaction{
				ID:   "0x4444444444444444444444444444444444444444444444444444444444444444",
				Type: txdb.InitiateBridge,
				BridgeCounterpartTx: &txdb.BridgeCounterpartTx{
					Complete: false,
				},
			},
			Nonce:          big.NewInt(4),
			SubmissionTime: uint64(now.Add(-10 * time.Hour).Unix()),
		},
		// Non-bridge transaction
		{
			WalletTransaction: &txdb.WalletTransaction{
				ID:   "0x5555555555555555555555555555555555555555555555555555555555555555",
				Type: txdb.Send,
			},
			Nonce:          big.NewInt(5),
			SubmissionTime: uint64(now.Add(-5 * time.Hour).Unix()),
		},
	}

	// Store all transactions
	for _, tx := range txs {
		if err := db.storeTx(tx); err != nil {
			t.Fatalf("error storing tx: %v", err)
		}
	}

	t.Run("getBridges all", func(t *testing.T) {
		t.Parallel()
		result, err := db.getBridges(nil, 0, nil, false)
		if err != nil {
			t.Fatalf("getBridges failed: %v", err)
		}
		if len(result) != 4 {
			t.Fatalf("expected 4 bridge initiations, got %d", len(result))
		}
		// Expected order: pending bridges first (sorted by submission time, newest first),
		// then completed bridges (sorted by submission time, newest first)
		// So: txs[3] (pending, newer), txs[2] (pending, older), txs[1] (completed, newer), txs[0] (completed, older)
		if result[0].ID != txs[3].ID || result[1].ID != txs[2].ID ||
			result[2].ID != txs[1].ID || result[3].ID != txs[0].ID {
			t.Fatalf("getBridges returned incorrect order of transactions")
		}
	})

	t.Run("getBridges limited", func(t *testing.T) {
		t.Parallel()
		result, err := db.getBridges(nil, 2, nil, false)
		if err != nil {
			t.Fatalf("getBridges failed: %v", err)
		}
		if len(result) != 2 {
			t.Fatalf("expected 2 bridge initiations, got %d", len(result))
		}

		// Expected order: first two pending bridges (newest first)
		if result[0].ID != txs[3].ID || result[1].ID != txs[2].ID {
			t.Fatalf("getBridges returned incorrect transactions")
		}
	})

	t.Run("getBridges with refID and past=false", func(t *testing.T) {
		t.Parallel()
		refID := common.HexToHash(txs[0].ID)
		result, err := db.getBridges(nil, 2, &refID, false)
		if err != nil {
			t.Fatalf("getBridges failed: %v", err)
		}
		if len(result) != 2 {
			t.Fatalf("expected 2 bridge initiations, got %d", len(result))
		}

		// Forward iteration from txs[0]: should get txs[0], then txs[1] (next in chronological order)
		if result[0].ID != txs[0].ID || result[1].ID != txs[1].ID {
			t.Fatalf("getBridges returned incorrect transactions")
		}
	})

	t.Run("getBridges with refID and past=true", func(t *testing.T) {
		t.Parallel()
		refID := common.HexToHash(txs[0].ID)
		result, err := db.getBridges(nil, 2, &refID, true)
		if err != nil {
			t.Fatalf("getBridges failed: %v", err)
		}
		if len(result) != 1 {
			t.Fatalf("expected 1 bridge transaction, got %d", len(result))
		}

		// Only txs[0] should be returned since it's the oldest completed bridge
		if result[0].ID != txs[0].ID {
			t.Fatalf("getBridges returned incorrect transactions")
		}
	})

	t.Run("getBridges with refID and past=true from newer bridge", func(t *testing.T) {
		t.Parallel()
		// Use txs[1] as reference to test past=true with older transactions available
		refID := common.HexToHash(txs[1].ID)
		result, err := db.getBridges(nil, 2, &refID, true)
		if err != nil {
			t.Fatalf("getBridges failed: %v", err)
		}
		if len(result) != 2 {
			t.Fatalf("expected 2 bridge transactions, got %d", len(result))
		}

		// Should return txs[1] (reference) then txs[0] (older) in reverse chronological order
		if result[0].ID != txs[1].ID || result[1].ID != txs[0].ID {
			t.Fatalf("getBridges returned incorrect transactions")
		}
	})

	t.Run("getBridges with invalid refID", func(t *testing.T) {
		t.Parallel()
		invalidID := common.HexToHash("0xdeadbeef")
		_, err := db.getBridges(nil, 2, &invalidID, false)
		if err == nil || err != asset.CoinNotFoundError {
			t.Fatalf("expected CoinNotFoundError, got %v", err)
		}
	})

	t.Run("getBridges with non-bridge refID", func(t *testing.T) {
		t.Parallel()
		nonBridgeID := common.HexToHash(txs[4].ID)
		_, err := db.getBridges(nil, 2, &nonBridgeID, false)
		if err == nil {
			t.Fatalf("expected error for non-bridge refID, got nil")
		}
	})

	t.Run("getPendingBridges", func(t *testing.T) {
		t.Parallel()
		result, err := db.getPendingBridges(nil)
		if err != nil {
			t.Fatalf("getPendingBridges failed: %v", err)
		}
		if len(result) != 2 {
			t.Fatalf("expected 2 pending bridge transactions, got %d", len(result))
		}
		// Should be in reverse chronological order (newest first)
		if result[0].ID != txs[3].ID || result[1].ID != txs[2].ID {
			t.Fatalf("getPendingBridges returned incorrect order")
		}
	})
}

func TestTxDB_GetBridgeCompletions(t *testing.T) {
	t.Parallel()
	db := newTestTxDB(t)

	burnTxID := "0x1111111111111111111111111111111111111111111111111111111111111111"

	// Create first mint transaction that references the burn transaction
	mintTx1 := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			ID:   "0x2222222222222222222222222222222222222222222222222222222222222222",
			Type: txdb.CompleteBridge,
			BridgeCounterpartTx: &txdb.BridgeCounterpartTx{
				IDs: []string{burnTxID},
			},
		},
		Nonce:          big.NewInt(2),
		SubmissionTime: uint64(time.Now().Add(-20 * time.Hour).Unix()),
	}

	// Create second mint transaction that references the same burn ID
	mintTx2 := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			ID:   "0x3333333333333333333333333333333333333333333333333333333333333333",
			Type: txdb.CompleteBridge,
			BridgeCounterpartTx: &txdb.BridgeCounterpartTx{
				IDs: []string{burnTxID},
			},
		},
		Nonce:          big.NewInt(3),
		SubmissionTime: uint64(time.Now().Add(-18 * time.Hour).Unix()),
	}

	// Store first completion and verify single result
	if err := db.storeTx(mintTx1); err != nil {
		t.Fatalf("error storing first mint tx: %v", err)
	}

	result, err := db.getBridgeCompletions(burnTxID)
	if err != nil {
		t.Fatalf("getBridgeCompletions failed: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 completion transaction, got %d", len(result))
	}
	if result[0].ID != mintTx1.ID {
		t.Fatalf("expected first mint tx %s, got %s", mintTx1.ID, result[0].ID)
	}

	// Store second completion and verify both results in correct order
	if err := db.storeTx(mintTx2); err != nil {
		t.Fatalf("error storing second mint tx: %v", err)
	}

	result, err = db.getBridgeCompletions(burnTxID)
	if err != nil {
		t.Fatalf("getBridgeCompletions failed: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 completion transactions, got %d", len(result))
	}
	// Should be in order they were stored
	if result[0].ID != mintTx1.ID {
		t.Fatalf("expected first result to be %s, got %s", mintTx1.ID, result[0].ID)
	}
	if result[1].ID != mintTx2.ID {
		t.Fatalf("expected second result to be %s, got %s", mintTx2.ID, result[1].ID)
	}

	// Store duplicate and verify no change
	if err := db.storeTx(mintTx1); err != nil {
		t.Fatalf("error storing duplicate mint tx: %v", err)
	}

	result, err = db.getBridgeCompletions(burnTxID)
	if err != nil {
		t.Fatalf("getBridgeCompletions failed after duplicate: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 completion transactions after duplicate, got %d", len(result))
	}
	// Should still be in same order
	if result[0].ID != mintTx1.ID {
		t.Fatalf("expected first result to be %s after duplicate, got %s", mintTx1.ID, result[0].ID)
	}
	if result[1].ID != mintTx2.ID {
		t.Fatalf("expected second result to be %s after duplicate, got %s", mintTx2.ID, result[1].ID)
	}
}
