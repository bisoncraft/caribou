// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// TestTxDBDelegation verifies the wallet's transaction methods read through the
// master transaction DB handle (NetworkTxDB) supplied by Core, now that NEAR no
// longer keeps its own database.
func TestTxDBDelegation(t *testing.T) {
	log := util.NewLogger("T", util.LevelInfo, os.Stdout)
	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	w := &NearWallet{ctx: context.Background(), log: log, txDB: master.NetworkTxDB(BipID)}

	confirmed := &txdb.WalletTransaction{
		Type: txdb.Send, ID: "conf", Amount: 1,
		BlockNumber: 100, Timestamp: 1000, Confirmed: true,
	}
	pending := &txdb.WalletTransaction{Type: txdb.Send, ID: "pend", Amount: 2}
	if err := w.txDB.StoreTx(confirmed); err != nil {
		t.Fatalf("StoreTx confirmed: %v", err)
	}
	if err := w.txDB.StoreTx(pending); err != nil {
		t.Fatalf("StoreTx pending: %v", err)
	}

	// PendingTransactions returns only the unconfirmed tx.
	if got := w.PendingTransactions(context.Background()); len(got) != 1 || got[0].ID != "pend" {
		t.Fatalf("PendingTransactions = %v, want [pend]", txIDs(got))
	}

	// TxHistory returns everything stored for this network.
	resp, err := w.TxHistory(&txdb.TxHistoryRequest{})
	if err != nil {
		t.Fatalf("TxHistory: %v", err)
	}
	if len(resp.Txs) != 2 {
		t.Fatalf("TxHistory len = %d, want 2", len(resp.Txs))
	}

	// WalletTransaction is served from the local DB without touching the RPC
	// client (which is nil here).
	got, err := w.WalletTransaction(context.Background(), "conf")
	if err != nil || got == nil || got.ID != "conf" {
		t.Fatalf("WalletTransaction = %v, %v", got, err)
	}
}

func txIDs(txs []*txdb.WalletTransaction) []string {
	out := make([]string, len(txs))
	for i, tx := range txs {
		out[i] = tx.ID
	}
	return out
}

// TestPendingTxExpiry verifies that a pending transaction is marked rejected
// only when its death is proven: the node definitively does not know it
// (UNKNOWN_TRANSACTION) and the chain has advanced past the validity period
// of the transaction's reference block. Transport-level errors and missing
// reference heights must never expire a record.
func TestPendingTxExpiry(t *testing.T) {
	log := util.NewLogger("T", util.LevelInfo, os.Stdout)
	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	f := newFakeRPCServer(t)
	w := newTestWallet(t, f)
	w.txDB = master.NetworkTxDB(BipID)
	rpc, err := w.client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	const refHeight = 1000
	withRef := func(h uint64) map[string]string {
		return map[string]string{refHeightKey: strconv.FormatUint(h, 10)}
	}
	txs := []*txdb.WalletTransaction{
		// Dead: unknown to the node, reference block long lapsed.
		{Type: txdb.Send, ID: "dead", Amount: 1, AdditionalData: withRef(refHeight)},
		// Late but alive: unknown to the node, still inside its window.
		{Type: txdb.Send, ID: "late", Amount: 1, AdditionalData: withRef(refHeight + 5000)},
		// Unreadable: the node errors, but not with UNKNOWN_TRANSACTION.
		{Type: txdb.Send, ID: "err", Amount: 1, AdditionalData: withRef(refHeight)},
		// Legacy: no reference height recorded; death cannot be proven.
		{Type: txdb.Send, ID: "noref", Amount: 1},
	}
	for _, tx := range txs {
		if err := w.txDB.StoreTx(tx); err != nil {
			t.Fatalf("StoreTx %s: %v", tx.ID, err)
		}
	}

	unknownErr := &rpcError{
		Name:    "HANDLER_ERROR",
		Cause:   &rpcErrorCause{Name: "UNKNOWN_TRANSACTION"},
		Code:    -32000,
		Message: "Server error",
		Data:    json.RawMessage(`"Transaction doesn't exist"`),
	}
	f.handle("tx", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			TxHash string `json:"tx_hash"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad tx params: %v", err)
		}
		if req.TxHash == "err" {
			return nil, &rpcError{
				Name:    "HANDLER_ERROR",
				Cause:   &rpcErrorCause{Name: "INTERNAL_ERROR"},
				Code:    -32000,
				Message: "Server error",
			}
		}
		return nil, unknownErr
	})

	pendingIDs := func() map[string]bool {
		t.Helper()
		pending, err := w.txDB.GetPendingTxs()
		if err != nil {
			t.Fatalf("GetPendingTxs: %v", err)
		}
		ids := make(map[string]bool, len(pending))
		for _, tx := range pending {
			ids[tx.ID] = true
		}
		return ids
	}

	// Inside the window (boundary inclusive): nothing expires.
	w.checkPendingTxs(context.Background(), rpc, refHeight+transactionValidityPeriod+pendingExpiryMargin)
	if ids := pendingIDs(); len(ids) != 4 {
		t.Fatalf("pending after boundary check = %v, want all 4", ids)
	}

	// One block past the boundary: only the provably dead record expires.
	w.checkPendingTxs(context.Background(), rpc, refHeight+transactionValidityPeriod+pendingExpiryMargin+1)
	ids := pendingIDs()
	if ids["dead"] || !ids["late"] || !ids["err"] || !ids["noref"] {
		t.Fatalf("pending after expiry check = %v, want all but dead", ids)
	}

	// The dead record is terminal: confirmed and rejected.
	deadTx, err := w.WalletTransaction(context.Background(), "dead")
	if err != nil {
		t.Fatalf("WalletTransaction(dead): %v", err)
	}
	if !deadTx.Confirmed || !deadTx.Rejected {
		t.Fatalf("dead tx Confirmed=%v Rejected=%v, want true/true", deadTx.Confirmed, deadTx.Rejected)
	}

	// Far in the future, the late record's window lapses too.
	w.checkPendingTxs(context.Background(), rpc, refHeight+5000+transactionValidityPeriod+pendingExpiryMargin+1)
	if ids := pendingIDs(); ids["late"] || !ids["err"] || !ids["noref"] {
		t.Fatalf("pending after second expiry = %v, want only err and noref", ids)
	}
}
