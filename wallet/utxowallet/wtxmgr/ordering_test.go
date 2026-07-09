// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wtxmgr

import (
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
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

// TestInsertMinedCrossBlockChildBeforeParent exercises cross-block
// parent/child confirmation delivered child-first with no mempool phase:
// F confirms at height 99 paying the wallet at F:0, S confirms at height 100
// spending F:0, and S's mined insert is processed before F's. Neither is ever
// seen unmined, so no unmined-input record exists and the deferred debit must
// come from the mined-spender scan in addCredit's mined branch. A regression
// leaves F:0 listed unspent forever (a stale credit inflating the balance).
func TestInsertMinedCrossBlockChildBeforeParent(t *testing.T) {
	s, db, teardown, err := testStore()
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	now := time.Now()

	// F: spends some non-wallet outpoint, pays the wallet 2e8 at F:0.
	parent := spendOutput(&chainhash.Hash{3}, 0, 2e8)
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

	b99 := BlockMeta{Block: Block{Hash: chainhash.Hash{99}, Height: 99}, Time: now}
	b100 := BlockMeta{Block: Block{Hash: chainhash.Hash{100}, Height: 100}, Time: now}

	err = walletdb.Update(db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(namespaceKey)
		// CHILD first, mined at the LATER height.
		if err := s.InsertTx(ns, childRec, &b100); err != nil {
			return err
		}
		if err := s.AddCredit(ns, childRec, &b100, 0, true); err != nil {
			return err
		}
		if err := s.InsertTx(ns, parentRec, &b99); err != nil {
			return err
		}
		return s.AddCredit(ns, parentRec, &b99, 0, false)
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

// TestInsertMinedCrossBlockChildBeforeParentMempool is the mempool-phase
// variant of the cross-block child-first ordering: both transactions are
// seen unmined first, then S's mined insert (height 100) is processed before
// F's (height 99). The spend marker preserved by deleteUnminedTx must be
// transferred to a mined debit when F's credit is moved.
func TestInsertMinedCrossBlockChildBeforeParentMempool(t *testing.T) {
	s, db, teardown, err := testStore()
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	now := time.Now()
	parent := spendOutput(&chainhash.Hash{4}, 0, 2e8)
	parentRec, err := NewTxRecordFromMsgTx("btc", parent, now)
	if err != nil {
		t.Fatal(err)
	}
	child := spendOutput(&parentRec.Hash, 0, 1e8)
	childRec, err := NewTxRecordFromMsgTx("btc", child, now)
	if err != nil {
		t.Fatal(err)
	}

	b99 := BlockMeta{Block: Block{Hash: chainhash.Hash{99}, Height: 99}, Time: now}
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

		// CHILD's mined insert processed first, at the LATER height.
		if err := s.InsertTx(ns, childRec, &b100); err != nil {
			return err
		}
		if err := s.AddCredit(ns, childRec, &b100, 0, true); err != nil {
			return err
		}
		if err := s.InsertTx(ns, parentRec, &b99); err != nil {
			return err
		}
		return s.AddCredit(ns, parentRec, &b99, 0, false)
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

// TestInsertMinedChildBeforeParentBlockOnly is the block-only variant: F and
// S are never seen unmined (no mempool phase), S is inserted first. The
// unmined-credit machinery is entirely bypassed, so the deferred debit must
// come from the same-block sibling scan in addCredit's mined branch.
func TestInsertMinedChildBeforeParentBlockOnly(t *testing.T) {
	s, db, teardown, err := testStore()
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	now := time.Now()
	parent := spendOutput(&chainhash.Hash{2}, 0, 2e8)
	parentRec, err := NewTxRecordFromMsgTx("btc", parent, now)
	if err != nil {
		t.Fatal(err)
	}
	child := spendOutput(&parentRec.Hash, 0, 1e8)
	childRec, err := NewTxRecordFromMsgTx("btc", child, now)
	if err != nil {
		t.Fatal(err)
	}
	b200 := BlockMeta{Block: Block{Hash: chainhash.Hash{200}, Height: 200}, Time: now}

	err = walletdb.Update(db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(namespaceKey)
		// CHILD first, block-only.
		if err := s.InsertTx(ns, childRec, &b200); err != nil {
			return err
		}
		if err := s.AddCredit(ns, childRec, &b200, 0, true); err != nil {
			return err
		}
		if err := s.InsertTx(ns, parentRec, &b200); err != nil {
			return err
		}
		return s.AddCredit(ns, parentRec, &b200, 0, false)
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
		if len(unspent) != 1 || unspent[0].OutPoint.Hash != childRec.Hash {
			t.Errorf("unspent = %v, want only %v:0", unspent, childRec.Hash)
		}
		bal, err := s.Balance(ns, 1, 200)
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

// TestRollbackPreservedUnminedInputNoDuplicate exercises repeated
// mine/rollback cycles of a child whose funding parent stays unmined the
// whole time. deleteUnminedTx preserves the child's unmined-input record on
// each mine (the parent's credit is still unmined, so the record is the only
// spend marker), and each rollback re-adds the same spender hash. The record
// must hold exactly one copy of the hash regardless of cycle count.
func TestRollbackPreservedUnminedInputNoDuplicate(t *testing.T) {
	s, db, teardown, err := testStore()
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	now := time.Now()
	parent := spendOutput(&chainhash.Hash{5}, 0, 2e8)
	parentRec, err := NewTxRecordFromMsgTx("btc", parent, now)
	if err != nil {
		t.Fatal(err)
	}
	child := spendOutput(&parentRec.Hash, 0, 1e8)
	childRec, err := NewTxRecordFromMsgTx("btc", child, now)
	if err != nil {
		t.Fatal(err)
	}
	b100 := BlockMeta{Block: Block{Hash: chainhash.Hash{100}, Height: 100}, Time: now}

	err = walletdb.Update(db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(namespaceKey)

		// Both unmined; the parent never confirms.
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

		// Mine and roll back the child repeatedly.
		for i := 0; i < 3; i++ {
			if err := s.InsertTx(ns, childRec, &b100); err != nil {
				return err
			}
			if err := s.AddCredit(ns, childRec, &b100, 0, true); err != nil {
				return err
			}
			if err := s.Rollback(ns, 100); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = walletdb.View(db, func(dbtx walletdb.ReadTx) error {
		ns := dbtx.ReadBucket(namespaceKey)
		opKey := canonicalOutPoint(&parentRec.Hash, 0)
		hashes := fetchUnminedInputSpendTxHashes(ns, opKey)
		if len(hashes) != 1 || hashes[0] != childRec.Hash {
			t.Errorf("unmined-input record = %v, want exactly one copy of %v",
				hashes, childRec.Hash)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAddCreditSpentOnArrival asserts that addCredit reports false for a
// credit whose outpoint is already spent by a mined transaction in the store:
// the output never enters the unspent set, so AddCredit must not fire
// NotifyUnspent for it.
func TestAddCreditSpentOnArrival(t *testing.T) {
	s, db, teardown, err := testStore()
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	now := time.Now()
	parent := spendOutput(&chainhash.Hash{6}, 0, 2e8)
	parentRec, err := NewTxRecordFromMsgTx("btc", parent, now)
	if err != nil {
		t.Fatal(err)
	}
	child := spendOutput(&parentRec.Hash, 0, 1e8)
	childRec, err := NewTxRecordFromMsgTx("btc", child, now)
	if err != nil {
		t.Fatal(err)
	}
	b100 := BlockMeta{Block: Block{Hash: chainhash.Hash{100}, Height: 100}, Time: now}

	err = walletdb.Update(db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(namespaceKey)

		// The spender confirms first, block-only.
		if err := s.InsertTx(ns, childRec, &b100); err != nil {
			return err
		}
		if err := s.AddCredit(ns, childRec, &b100, 0, true); err != nil {
			return err
		}
		if err := s.InsertTx(ns, parentRec, &b100); err != nil {
			return err
		}
		isNew, err := s.addCredit(ns, parentRec, &b100, 0, false)
		if err != nil {
			return err
		}
		if isNew {
			t.Error("addCredit returned true for a spent-on-arrival output")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
