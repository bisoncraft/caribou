// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wtxmgr

import (
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// TestInsertMinedChildBeforeParent exercises same-block parent/child
// confirmation delivered child-first: F funds the wallet, S spends F:0, both
// confirm in the same block, and S's mined insert is processed before F's.
// InsertTx must be order-independent here — when S confirms first there is no
// mined credit to debit, so the spend marker must survive S's unmined-record
// cleanup and be transferred to a proper mined debit when F's credit is
// moved. A regression leaves F:0 listed unspent forever (a stale credit that
// coin selection double-spends).
func TestInsertMinedChildBeforeParent(t *testing.T) {
	s, db, teardown, err := testStore()
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	now := time.Now()

	// F: spends some non-wallet outpoint, pays the wallet 2e8 at F:0.
	parent := spendOutput(&chainhash.Hash{1}, 0, 2e8)
	parentRec, err := NewTxRecordFromMsgTx("btc", parent, now)
	if err != nil {
		t.Fatal(err)
	}
	// S: spends F:0, pays the wallet 1e8 change at S:0.
	child := spendOutput(&parentRec.Hash, 0, 1e8)
	childRec, err := NewTxRecordFromMsgTx("btc", child, now)
	if err != nil {
		t.Fatal(err)
	}

	b100 := BlockMeta{Block: Block{Hash: chainhash.Hash{100}, Height: 100}, Time: now}

	err = walletdb.Update(db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(namespaceKey)

		// Both seen unmined first (the live mempool flow).
		if err := s.InsertTx(ns, parentRec, nil); err != nil {
			return err
		}
		if err := s.AddCredit(ns, parentRec, nil, 0, false); err != nil {
			return err
		}
		if err := s.InsertTx(ns, childRec, nil); err != nil {
			return err
		}
		if err := s.AddCredit(ns, childRec, nil, 0, true); err != nil {
			return err
		}

		// Same block, CHILD processed first.
		if err := s.InsertTx(ns, childRec, &b100); err != nil {
			return err
		}
		if err := s.AddCredit(ns, childRec, &b100, 0, true); err != nil {
			return err
		}
		if err := s.InsertTx(ns, parentRec, &b100); err != nil {
			return err
		}
		return s.AddCredit(ns, parentRec, &b100, 0, false)
	})
	if err != nil {
		t.Fatal(err)
	}

	err = walletdb.View(db, func(dbtx walletdb.ReadTx) error {
		ns := dbtx.ReadBucket(namespaceKey)

		unspent, err := s.UnspentOutputs(ns)
		if err != nil {
			return err
		}
		for _, c := range unspent {
			if c.OutPoint.Hash == parentRec.Hash {
				t.Errorf("spent parent output %v listed unspent", c.OutPoint)
			}
		}
		if len(unspent) != 1 || unspent[0].OutPoint.Hash != childRec.Hash {
			t.Errorf("unspent = %v, want only %v:0", unspent, childRec.Hash)
		}

		bal, err := s.Balance(ns, 1, 100)
		if err != nil {
			return err
		}
		if bal != 1e8 {
			t.Errorf("balance = %v, want %v", bal, 1e8)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
