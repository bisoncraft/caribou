// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// TestMinedTxNotificationTiming checks when a mined transaction is reported to
// notification subscribers. A transaction in a block that is not connected yet
// waits for that block's connection, which flushes the coalesced notification.
// A transaction in an already connected block, which a backend reports after
// the header it raced, has no later flush until the next block, so it must be
// delivered right away or it stays unreported (and out of the tx history) until
// the next block arrives.
func TestMinedTxNotificationTiming(t *testing.T) {
	w, cleanup := testWallet(t)
	defer cleanup()

	addr, err := w.NewAddress(0, w.ScopeForPurpose(84))
	if err != nil {
		t.Fatalf("NewAddress: %v", err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript: %v", err)
	}

	const syncedHeight = 10
	err = walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return w.Manager.SetSyncedTo(ns, &waddrmgr.BlockStamp{
			Height:    syncedHeight,
			Hash:      chainhash.Hash{syncedHeight},
			Timestamp: time.Now(),
		})
	})
	if err != nil {
		t.Fatalf("SetSyncedTo: %v", err)
	}

	cl := w.NtfnServer.TransactionNotifications()
	defer cl.Done()
	got := make(chan *TransactionNotifications, 4)
	go func() {
		for n := range cl.C {
			got <- n
		}
	}()

	addMined := func(seed byte, height int32) chainhash.Hash {
		t.Helper()
		msgTx := wire.NewMsgTx(2)
		msgTx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{seed}, Index: 0}})
		msgTx.AddTxOut(&wire.TxOut{Value: 100_000, PkScript: pkScript})
		rec, err := wtxmgr.NewTxRecordFromMsgTx("btc", msgTx, time.Now())
		if err != nil {
			t.Fatalf("NewTxRecordFromMsgTx: %v", err)
		}
		block := &wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: chainhash.Hash{byte(height)}, Height: height},
			Time:  time.Now(),
		}
		err = walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
			return w.addRelevantTx(tx, rec, block)
		})
		if err != nil {
			t.Fatalf("addRelevantTx: %v", err)
		}
		return rec.Hash
	}

	// A block above the synced height is not connected yet, so nothing is
	// delivered until its connection flushes the notification.
	addMined(1, syncedHeight+1)
	select {
	case n := <-got:
		t.Fatalf("notified for a transaction in a block that is not connected: %+v", n)
	case <-time.After(200 * time.Millisecond):
	}

	// The coalesced notification is still pending, so a transaction in an
	// already connected block would flush it along with itself. Reset it the
	// way a block connection would.
	w.NtfnServer.currentTxNtfn = nil

	// A block at or below the synced height is already connected, so the
	// transaction is delivered at once.
	const lateHeight = syncedHeight - 5
	hash := addMined(2, lateHeight)
	select {
	case n := <-got:
		if len(n.AttachedBlocks) != 1 || n.AttachedBlocks[0].Height != lateHeight {
			t.Fatalf("attached blocks = %+v, want one at height %d", n.AttachedBlocks, lateHeight)
		}
		txs := n.AttachedBlocks[0].Transactions
		if len(txs) != 1 || *txs[0].Hash != hash {
			t.Fatalf("transactions = %+v, want only %s", txs, hash)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification for a transaction in an already connected block")
	}
}
