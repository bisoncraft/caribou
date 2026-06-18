// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
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

	w := &NearWallet{log: log, txDB: master.NetworkTxDB(BipID)}

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
