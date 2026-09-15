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
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// tMinedBlockHash is a non-empty block hash, as gettransaction reports for a
// mined transaction. An empty BlockHash, as reported for a mempool
// transaction, is handled separately (see
// TestWalletTransactionNotCachedUnminedStaysPending).
var tMinedBlockHash = (&chainhash.Hash{0x01}).String()

// addrFromScript derives the encoded address for a P2PKH pkScript, the same
// way idUnknownTx does, so tests don't need to hardcode addresses.
func addrFromScript(t *testing.T, w *zecWallet, pkScript []byte) string {
	t.Helper()
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript, w.btcParams)
	if err != nil || len(addrs) != 1 {
		t.Fatalf("ExtractPkScriptAddrs error: %v, addrs: %v", err, addrs)
	}
	addr, err := EncodeAddress(addrs[0], w.addrParams)
	if err != nil {
		t.Fatalf("EncodeAddress error: %v", err)
	}
	return addr
}

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

// foreignP2PKH is a P2PKH script for an address the wallet does not own
// (tP2PKH with the last hash160 byte flipped).
var foreignP2PKH, _ = hex.DecodeString("76a9148fc02268f208a61767504fe0b48d228641ba81e488ac")

func TestWalletTransactionNotCachedFetchesFromRPC(t *testing.T) {
	w, cl, shutdown := tNewWalletWithTxDB(t)
	defer shutdown()
	defer cl.checkEmptiness(t)

	dTx := makeRawTx([]util.Bytes{tP2PKH}, []*wire.TxIn{dummyInput()})
	txB, _ := dTx.Bytes()
	// A mined transaction carries a non-empty BlockHash, so WalletTransaction
	// looks up its block header.
	cl.queueResponse("gettransaction", &GetTransactionResult{
		Bytes:     txB,
		TxID:      tTxHash.String(),
		BlockHash: tMinedBlockHash,
		Details:   []btcjson.GetTransactionDetailsResult{{Category: "receive"}},
	})
	cl.queueResponse("getblockheader", &btc.BlockHeader{Height: 5, Confirmations: 1})
	// idUnknownTx re-fetches the transaction, then identifies it. Category is
	// "receive", so it identifies as a Receive, calling OwnsDepositAddress
	// for each output address.
	cl.queueResponse("gettransaction", &GetTransactionResult{
		Bytes: txB,
		TxID:  tTxHash.String(),
	})
	// One OwnsDepositAddress lookup in txOutDirection, one in getRecipient.
	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsMine: true})
	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsMine: true})

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
	if got.Type != txdb.Receive {
		t.Fatalf("expected Type = Receive, got %v", got.Type)
	}
	if got.Amount != 1 {
		t.Fatalf("expected Amount = 1, got %d", got.Amount)
	}
	wantAddr := addrFromScript(t, w, tP2PKH)
	if got.Recipient == nil {
		t.Fatalf("expected Recipient = %s, got nil", wantAddr)
	}
	if *got.Recipient != wantAddr {
		t.Fatalf("expected Recipient = %s, got %s", wantAddr, *got.Recipient)
	}
}

func TestWalletTransactionNotCachedSendFetchesFromRPC(t *testing.T) {
	w, cl, shutdown := tNewWalletWithTxDB(t)
	defer shutdown()
	defer cl.checkEmptiness(t)

	dTx := makeRawTx([]util.Bytes{foreignP2PKH}, []*wire.TxIn{dummyInput()})
	txB, _ := dTx.Bytes()
	cl.queueResponse("gettransaction", &GetTransactionResult{
		Bytes:     txB,
		TxID:      tTxHash.String(),
		BlockHash: tMinedBlockHash,
		Details:   []btcjson.GetTransactionDetailsResult{{Category: "send"}},
	})
	cl.queueResponse("getblockheader", &btc.BlockHeader{Height: 5, Confirmations: 1})
	cl.queueResponse("gettransaction", &GetTransactionResult{
		Bytes: txB,
		TxID:  tTxHash.String(),
	})
	// allOutputsPayUs (Split check), txOutDirection, getRecipient, and
	// allOutputsPayUs (SelfSend check) each look up the single output's
	// address ownership; none of them own it.
	for i := 0; i < 4; i++ {
		cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsMine: false})
	}

	got, err := w.WalletTransaction(tCtx, tTxHash.String())
	if err != nil {
		t.Fatalf("WalletTransaction error: %v", err)
	}
	if got.Type != txdb.Send {
		t.Fatalf("expected Type = Send, got %v", got.Type)
	}
	if got.Amount != 1 {
		t.Fatalf("expected Amount = 1, got %d", got.Amount)
	}
	wantAddr := addrFromScript(t, w, foreignP2PKH)
	if got.Recipient == nil {
		t.Fatalf("expected Recipient = %s, got nil", wantAddr)
	}
	if *got.Recipient != wantAddr {
		t.Fatalf("expected Recipient = %s, got %s", wantAddr, *got.Recipient)
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

	// The caller is always told Confirmed = true, but the stored record must
	// not be, or it will be lost from pending tracking on restart.
	pending, err := w.txDB().GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs error: %v", err)
	}
	var found bool
	for _, p := range pending {
		if p.ID == tTxHash.String() {
			found = true
			if p.Confirmed {
				t.Fatalf("expected stored tx to be unconfirmed")
			}
		}
	}
	if !found {
		t.Fatalf("expected tx %s in pending txs, got %+v", tTxHash.String(), pending)
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
