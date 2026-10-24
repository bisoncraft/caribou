// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// TestMinedTxNotificationTiming checks when and how a mined transaction is
// reported to notification subscribers.
//
// A transaction in a block that is not connected yet waits for that block's
// connection, which flushes the coalesced notification. A transaction in an
// already connected block, which a backend reports after the header it raced,
// has no later flush until the next block, so it must be delivered right away
// or it stays out of the tx history until then.
//
// Subscribers take the last attached block as the chain tip and expect blocks
// in mined order, so a late flush must end with the tip, must not report a block
// that is not connected yet, and must leave that block's entry for its own
// connection.
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
	blockMeta := func(height int32) wtxmgr.BlockMeta {
		return wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: chainhash.Hash{byte(height)}, Height: height},
			Time:  time.Now(),
		}
	}
	connect := func(height int32) {
		t.Helper()
		err := walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
			return w.connectBlock(tx, blockMeta(height))
		})
		if err != nil {
			t.Fatalf("connectBlock(%d): %v", height, err)
		}
	}
	connect(syncedHeight)

	cl := w.NtfnServer.TransactionNotifications()
	defer cl.Done()
	got := make(chan *TransactionNotifications, 8)
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
		block := blockMeta(height)
		err = walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
			return w.addRelevantTx(tx, rec, &block)
		})
		if err != nil {
			t.Fatalf("addRelevantTx: %v", err)
		}
		return rec.Hash
	}
	next := func() *TransactionNotifications {
		t.Helper()
		select {
		case n := <-got:
			return n
		case <-time.After(2 * time.Second):
			t.Fatal("no notification")
			return nil
		}
	}
	none := func(why string) {
		t.Helper()
		select {
		case n := <-got:
			t.Fatalf("notified %s: %+v", why, n)
		case <-time.After(200 * time.Millisecond):
		}
	}
	// heights returns the attached block heights and checks the tip is last.
	heights := func(n *TransactionNotifications, tip int32) []int32 {
		t.Helper()
		var hs []int32
		for _, b := range n.AttachedBlocks {
			hs = append(hs, b.Height)
		}
		if len(hs) == 0 || hs[len(hs)-1] != tip {
			t.Fatalf("attached heights %v, want the tip %d last", hs, tip)
		}
		for i := 1; i < len(hs); i++ {
			if hs[i] <= hs[i-1] {
				t.Fatalf("attached heights %v are not in mined order", hs)
			}
		}
		return hs
	}
	txHashes := func(b Block) []chainhash.Hash {
		var hs []chainhash.Hash
		for _, tx := range b.Transactions {
			hs = append(hs, *tx.Hash)
		}
		return hs
	}

	// A block above the synced height is not connected yet, so nothing is
	// delivered until its connection flushes the notification.
	pendingTx := addMined(1, syncedHeight+1)
	none("for a transaction in a block that is not connected")

	// A late transaction is delivered at once, but the unconnected block is
	// not: the notification holds the late block, then the tip.
	const lateHeight = syncedHeight - 5
	lateTx := addMined(2, lateHeight)
	n := next()
	if hs := heights(n, syncedHeight); len(hs) != 2 || hs[0] != lateHeight {
		t.Fatalf("attached heights %v, want [%d %d]", hs, lateHeight, syncedHeight)
	}
	if txs := txHashes(n.AttachedBlocks[0]); len(txs) != 1 || txs[0] != lateTx {
		t.Fatalf("late block transactions %v, want only %s", txs, lateTx)
	}
	if len(n.AttachedBlocks[1].Transactions) != 0 {
		t.Fatalf("tip entry carries transactions: %+v", n.AttachedBlocks[1])
	}
	none("a second time for the same transaction")

	// The unconnected block's entry is still waiting, and its own
	// connection delivers it.
	connect(syncedHeight + 1)
	n = next()
	if hs := heights(n, syncedHeight+1); len(hs) != 1 {
		t.Fatalf("attached heights %v, want only the connected block", hs)
	}
	if txs := txHashes(n.AttachedBlocks[0]); len(txs) != 1 || txs[0] != pendingTx {
		t.Fatalf("connected block transactions %v, want only %s", txs, pendingTx)
	}

	// A transaction in the tip block itself needs no extra tip entry.
	tipTx := addMined(3, syncedHeight+1)
	n = next()
	if hs := heights(n, syncedHeight+1); len(hs) != 1 {
		t.Fatalf("attached heights %v, want only the tip block", hs)
	}
	if txs := txHashes(n.AttachedBlocks[0]); len(txs) != 1 || txs[0] != tipTx {
		t.Fatalf("tip block transactions %v, want only %s", txs, tipTx)
	}

	// Late transactions found one by one, as a rescan delivers them, each end
	// with the tip so none moves a subscriber's chain tip backwards.
	for i, height := range []int32{3, 4, 5} {
		addMined(byte(10+i), height)
		heights(next(), syncedHeight+1)
	}
}
