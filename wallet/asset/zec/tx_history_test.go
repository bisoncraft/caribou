// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// tNewWalletWithTxDB is like tNewWallet, but wires up a real, temp-dir-backed
// NetworkTxDB so the transaction history paths can be exercised.
func tNewWalletWithTxDB(t *testing.T) (*zecWallet, *tRPCClient, func()) {
	t.Helper()
	dataDir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatalf("couldn't create data dir: %v", err)
	}

	db, err := txdb.New(filepath.Join(dataDir, "tx.db"), tLogger)
	if err != nil {
		t.Fatalf("txdb.New error: %v", err)
	}

	cl := newRPCClient()
	walletCfg := &asset.WalletConfig{
		Emit: asset.NewWalletEmitter(cl.tipChanged, BipID, tLogger),
		PeersChange: func(num uint32, err error) {
		},
		DataDir:     dataDir,
		NetworkTxDB: db.NetworkTxDB(BipID),
		Settings: map[string]string{
			"rpcuser":     "a",
			"rpcpassword": "b",
		},
	}
	walletCtx, cancel := context.WithCancel(tCtx)

	wi, err := NewWallet(walletCfg, tLogger, util.Simnet)
	if err != nil {
		t.Fatalf("NewWallet error: %v", err)
	}

	w := wi.(*zecWallet)
	w.node = cl
	bestHash, _ := cl.bestBlock()
	w.currentTip = &blockVector{
		Height: cl.getBestBlockHeight(),
		Hash:   *bestHash,
	}

	if _, err := w.startTxHistoryDB(walletCtx); err != nil {
		t.Fatalf("startTxHistoryDB error: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.watchBlocks(walletCtx)
	}()
	shutdown := func() {
		cancel()
		wg.Wait()
		db.Close()
		os.RemoveAll(dataDir)
	}
	return w, cl, shutdown
}

func TestStartTxHistoryDBWithRealDB(t *testing.T) {
	w, _, shutdown := tNewWalletWithTxDB(t)
	defer shutdown()

	if w.txDB() == nil {
		t.Fatalf("expected txDB to be initialized")
	}
}

func TestAddTxToHistoryAndWalletTransactionCached(t *testing.T) {
	w, _, shutdown := tNewWalletWithTxDB(t)
	defer shutdown()

	wt := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        tTxHash.String(),
		Amount:    100,
		NetworkID: BipID,
		AssetID:   BipID,
	}
	w.addTxToHistory(wt, tTxHash)

	// A pending (unconfirmed) tx should show up in PendingTransactions.
	pending := w.PendingTransactions(tCtx)
	if len(pending) != 1 || pending[0].ID != tTxHash.String() {
		t.Fatalf("unexpected pending transactions: %+v", pending)
	}

	// WalletTransaction should find it in the DB without needing an RPC call.
	got, err := w.WalletTransaction(tCtx, tTxHash.String())
	if err != nil {
		t.Fatalf("WalletTransaction error: %v", err)
	}
	if got.ID != tTxHash.String() || !got.Confirmed {
		t.Fatalf("unexpected wallet transaction: %+v", got)
	}

	// TxHistory should return the same transaction.
	hist, err := w.TxHistory(&txdb.TxHistoryRequest{N: 10})
	if err != nil {
		t.Fatalf("TxHistory error: %v", err)
	}
	if len(hist.Txs) != 1 || hist.Txs[0].ID != tTxHash.String() {
		t.Fatalf("unexpected tx history: %+v", hist)
	}
}

func TestWalletTransactionByCoinIDHex(t *testing.T) {
	w, _, shutdown := tNewWalletWithTxDB(t)
	defer shutdown()

	wt := &txdb.WalletTransaction{
		Type:      txdb.Receive,
		ID:        tTxHash.String(),
		NetworkID: BipID,
		AssetID:   BipID,
	}
	w.addTxToHistory(wt, tTxHash)

	// hex-encoded 36-byte outpoint coin ID decodes to the tx hash string.
	coinID := encodeCoinID(tTxHash, 0)
	got, err := w.WalletTransaction(tCtx, hex.EncodeToString(coinID))
	if err != nil {
		t.Fatalf("WalletTransaction (coin id) error: %v", err)
	}
	if got.ID != tTxHash.String() {
		t.Fatalf("unexpected tx: %+v", got)
	}
}

func TestWalletTransactionNotCachedFetchesFromRPC(t *testing.T) {
	w, cl, shutdown := tNewWalletWithTxDB(t)
	defer shutdown()
	defer cl.checkEmptiness(t)

	dTx := dummyTx()
	txB, _ := dTx.Bytes()
	cl.queueResponse("gettransaction", &GetTransactionResult{
		Bytes:     txB,
		TxID:      tTxHash.String(),
		BlockHash: tTxHash.String(),
	})
	cl.queueResponse("getblockheader", &btc.BlockHeader{Height: 5, Time: 10, Confirmations: 1})
	// idUnknownTx re-fetches the transaction, then identifies it. Category is
	// empty (not "send"), so it identifies as a Receive, calling
	// OwnsDepositAddress for each output address.
	cl.queueResponse("gettransaction", &GetTransactionResult{
		Bytes: txB,
		TxID:  tTxHash.String(),
	})

	got, err := w.WalletTransaction(tCtx, tTxHash.String())
	if err != nil {
		t.Fatalf("WalletTransaction error: %v", err)
	}
	if got.ID != tTxHash.String() {
		t.Fatalf("unexpected tx id: %s", got.ID)
	}
	if !got.Confirmed {
		t.Fatalf("expected Confirmed = true")
	}
	if got.BlockNumber != 5 || got.Timestamp != 10 {
		t.Fatalf("unexpected block details: %+v", got)
	}
}

func TestWalletTransactionNotCachedWithoutBlock(t *testing.T) {
	w, cl, shutdown := tNewWalletWithTxDB(t)
	defer shutdown()
	defer cl.checkEmptiness(t)

	dTx := dummyTx()
	txB, _ := dTx.Bytes()
	cl.queueResponse("gettransaction", &GetTransactionResult{
		Bytes:     txB,
		TxID:      tTxHash.String(),
		BlockHash: "",
	})
	// idUnknownTx re-fetches the transaction to identify it.
	cl.queueResponse("gettransaction", &GetTransactionResult{
		Bytes: txB,
		TxID:  tTxHash.String(),
	})

	got, err := w.WalletTransaction(tCtx, tTxHash.String())
	if err != nil {
		t.Fatalf("WalletTransaction error: %v", err)
	}
	if got.ID != tTxHash.String() || !got.Confirmed {
		t.Fatalf("unexpected wallet transaction: %+v", got)
	}
	if got.BlockNumber != 0 || got.Timestamp != 0 {
		t.Fatalf("unexpected block details: %+v", got)
	}
}

func TestWalletTransactionBadTxID(t *testing.T) {
	w, _, shutdown := tNewWalletWithTxDB(t)
	defer shutdown()

	if _, err := w.WalletTransaction(tCtx, "not-a-txid"); err == nil {
		t.Fatalf("expected error for bad txid")
	}
}

func TestPendingTransactionsEmpty(t *testing.T) {
	w, _, shutdown := tNewWalletWithTxDB(t)
	defer shutdown()

	if got := w.PendingTransactions(tCtx); len(got) != 0 {
		t.Fatalf("expected no pending transactions, got %+v", got)
	}
}

func TestTxHistoryNoDB(t *testing.T) {
	w, _, shutdown := tNewWallet()
	defer shutdown()

	if _, err := w.TxHistory(&txdb.TxHistoryRequest{N: 10}); err == nil {
		t.Fatalf("expected error when tx history DB is not initialized")
	}
}

func TestWalletTransactionNoDB(t *testing.T) {
	w, _, shutdown := tNewWallet()
	defer shutdown()

	if _, err := w.WalletTransaction(tCtx, tTxHash.String()); err == nil {
		t.Fatalf("expected error when tx history DB is not initialized")
	}
}
