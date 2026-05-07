package wtxmgr

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
)

// applyMigration is a helper function that allows us to assert the state of the
// top-level bucket before and after a migration. This can be used to ensure
// the correctness of migrations.
func applyMigration(t *testing.T,
	beforeMigration, afterMigration func(walletdb.ReadWriteBucket, *Store) error,
	migration func(walletdb.ReadWriteBucket) error, shouldFail bool) {

	t.Helper()

	// We'll start by setting up our transaction store backed by a database.
	store, db, teardown, err := testStore()
	if err != nil {
		t.Fatalf("unable to create test store: %v", err)
	}
	defer teardown()

	// First, we'll run the beforeMigration closure, which contains the
	// database modifications/assertions needed before proceeding with the
	// migration.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(namespaceKey)
		if ns == nil {
			return errors.New("top-level namespace does not exist")
		}
		return beforeMigration(ns, store)
	})
	if err != nil {
		t.Fatalf("unable to run beforeMigration func: %v", err)
	}

	// Then, we'll run the migration itself and fail if it does not match
	// its expected result.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(namespaceKey)
		if ns == nil {
			return errors.New("top-level namespace does not exist")
		}
		return migration(ns)
	})
	if err != nil && !shouldFail {
		t.Fatalf("unable to perform migration: %v", err)
	} else if err == nil && shouldFail {
		t.Fatal("expected migration to fail, but did not")
	}

	// Finally, we'll run the afterMigration closure, which contains the
	// assertions needed in order to guarantee than the migration was
	// successful.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(namespaceKey)
		if ns == nil {
			return errors.New("top-level namespace does not exist")
		}
		return afterMigration(ns, store)
	})
	if err != nil {
		t.Fatalf("unable to run afterMigration func: %v", err)
	}
}

// TestMigrationDropTransactionHistory ensures that a transaction store is reset
// to a clean state after dropping its transaction history.
func TestMigrationDropTransactionHistory(t *testing.T) {
	t.Parallel()

	// checkTransactions is a helper function that will assert the correct
	// state of the transaction store based on whether the migration has
	// completed or not.
	checkTransactions := func(ns walletdb.ReadWriteBucket, s *Store,
		afterMigration bool) error {

		// We should see one confirmed unspent output before the
		// migration, and none after.
		utxos, err := s.UnspentOutputs(ns)
		if err != nil {
			return err
		}
		if len(utxos) == 0 && !afterMigration {
			return errors.New("expected to find 1 utxo, found none")
		}
		if len(utxos) > 0 && afterMigration {
			return fmt.Errorf("expected to find 0 utxos, found %d",
				len(utxos))
		}

		// We should see one unconfirmed transaction before the
		// migration, and none after.
		unconfirmedTxs, err := s.UnminedTxs(ns)
		if err != nil {
			return err
		}
		if len(unconfirmedTxs) == 0 && !afterMigration {
			return errors.New("expected to find 1 unconfirmed " +
				"transaction, found none")
		}
		if len(unconfirmedTxs) > 0 && afterMigration {
			return fmt.Errorf("expected to find 0 unconfirmed "+
				"transactions, found %d", len(unconfirmedTxs))
		}

		// We should have a non-zero balance before the migration, and
		// zero after.
		minedBalance, err := fetchMinedBalance(ns)
		if err != nil {
			return err
		}
		if minedBalance == 0 && !afterMigration {
			return errors.New("expected non-zero balance before " +
				"migration")
		}
		if minedBalance > 0 && afterMigration {
			return fmt.Errorf("expected zero balance after "+
				"migration, got %d", minedBalance)
		}

		return nil
	}

	beforeMigration := func(ns walletdb.ReadWriteBucket, s *Store) error {
		// We'll start by adding two transactions to the store: a
		// confirmed transaction and an unconfirmed transaction one.
		// The confirmed transaction will spend from a coinbase output,
		// while the unconfirmed will spend an output from the confirmed
		// transaction.
		cb := newCoinBase(1e8)
		cbRec, err := NewTxRecordFromMsgTx("btc", cb, timeNow())
		if err != nil {
			return err
		}

		b := &BlockMeta{Block: Block{Height: 100}}
		confirmedSpend := spendOutput(&cbRec.Hash, 0, 5e7, 4e7)
		confirmedSpendRec, err := NewTxRecordFromMsgTx(
			"btc", confirmedSpend, timeNow(),
		)

		if err := s.InsertTx(ns, confirmedSpendRec, b); err != nil {
			return err
		}
		err = s.AddCredit(ns, confirmedSpendRec, b, 1, true)
		if err != nil {
			return err
		}

		unconfimedSpend := spendOutput(
			&confirmedSpendRec.Hash, 0, 5e6, 5e6,
		)
		unconfirmedSpendRec, err := NewTxRecordFromMsgTx(
			"btc", unconfimedSpend, timeNow(),
		)
		if err != nil {
			return err
		}
		if err := s.InsertTx(ns, unconfirmedSpendRec, nil); err != nil {
			return err
		}

		err = s.AddCredit(ns, unconfirmedSpendRec, nil, 1, true)
		if err != nil {
			return err
		}

		// Ensure these transactions exist within the store.
		return checkTransactions(ns, s, false)
	}

	afterMigration := func(ns walletdb.ReadWriteBucket, s *Store) error {
		// Assuming the migration was successful, we should see that the
		// store no longer has the transaction history prior to the
		// migration.
		return checkTransactions(ns, s, true)
	}

	// We can now apply the migration and expect it not to fail.
	applyMigration(
		t, beforeMigration, afterMigration, dropTransactionHistory,
		false,
	)
}

// TestMigrationManagerHonorsNamespace ensures that MigrationManager.{Current,
// Set}Version operate on the namespace argument supplied by the caller, not on
// the manager's receiver bucket. The two are normally identical (the migration
// runner passes mgr.Namespace()), but the contract permits any caller to pass
// a different bucket.
func TestMigrationManagerHonorsNamespace(t *testing.T) {
	t.Parallel()

	_, db, teardown, err := testStore()
	if err != nil {
		t.Fatalf("unable to create test store: %v", err)
	}
	defer teardown()

	// Make a sibling top-level bucket that looks like a wtxmgr namespace, so
	// fetchVersion/putVersion can read and write its rootVersion key. We seed
	// it with a distinct version so we can tell which bucket got read.
	otherKey := []byte("other-ns")
	const otherVersionInit uint32 = 7
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns, err := tx.CreateTopLevelBucket(otherKey)
		if err != nil {
			return err
		}
		return putVersion(ns, otherVersionInit)
	})
	if err != nil {
		t.Fatalf("unable to create sibling namespace: %v", err)
	}

	// CurrentVersion(ns) must read from ns, not from the receiver. We root the
	// MigrationManager at namespaceKey (the wtxmgr default) but pass it the
	// sibling bucket; we should see the sibling's version, not the receiver's.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		recv := tx.ReadWriteBucket(namespaceKey)
		other := tx.ReadWriteBucket(otherKey)
		mgr := NewMigrationManager(recv)

		got, err := mgr.CurrentVersion(other)
		if err != nil {
			return fmt.Errorf("CurrentVersion: %v", err)
		}
		if got != otherVersionInit {
			return fmt.Errorf("CurrentVersion read wrong bucket: "+
				"got %d, want %d", got, otherVersionInit)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// SetVersion(ns, v) must write to ns, not to the receiver. Write a new
	// value to the sibling, then verify (a) the sibling reflects the write
	// and (b) the receiver bucket's version is unchanged.
	const newOtherVersion uint32 = 42
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		recv := tx.ReadWriteBucket(namespaceKey)
		other := tx.ReadWriteBucket(otherKey)
		mgr := NewMigrationManager(recv)
		return mgr.SetVersion(other, newOtherVersion)
	})
	if err != nil {
		t.Fatalf("SetVersion: %v", err)
	}

	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		other := tx.ReadBucket(otherKey)
		gotOther, err := fetchVersion(other)
		if err != nil {
			return fmt.Errorf("fetchVersion(other): %v", err)
		}
		if gotOther != newOtherVersion {
			return fmt.Errorf("sibling bucket version: got %d, "+
				"want %d", gotOther, newOtherVersion)
		}

		recv := tx.ReadBucket(namespaceKey)
		gotRecv, err := fetchVersion(recv)
		if err != nil {
			return fmt.Errorf("fetchVersion(recv): %v", err)
		}
		// testStore() initializes the receiver to the latest version.
		if gotRecv != getLatestVersion() {
			return fmt.Errorf("receiver bucket version was "+
				"clobbered: got %d, want %d (latest)",
				gotRecv, getLatestVersion())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
