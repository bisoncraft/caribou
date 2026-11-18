// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"

	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
)

func regUpdate(t *testing.T, e *env) Update {
	t.Helper()
	return Update{Registrations: []Registration{{Height: heightA, Tx: rawTx(t, e.txA)}}}
}

func TestStoreDrop(t *testing.T) {
	e := newEnv(t)
	up := regUpdate(t, e)
	up.Registrations[0].Block = chainhash.Hash{4}
	up.OwnerKeyIDs = map[uint32][20]byte{3: e.idA}
	want := Spend{Height: 900, Block: chainhash.Hash{6}, TxID: chainhash.Hash{5}}
	up.Dissolutions = map[chainhash.Hash]Spend{e.a.ProTxHash: want}
	if err := e.store.Save(up); err != nil {
		t.Fatal(err)
	}
	s := e.snapshot()
	if len(s.Registrations) != 1 || s.Dissolutions[e.a.ProTxHash] != want || s.NextOwner != 4 ||
		s.Registrations[e.a.ProTxHash].Block != (chainhash.Hash{4}) {
		t.Fatalf("%+v", s)
	}
	// Dropping a dissolution leaves the registration.
	if err := e.store.Save(Update{DropDissolutions: []chainhash.Hash{e.a.ProTxHash}}); err != nil {
		t.Fatal(err)
	}
	s = e.snapshot()
	if len(s.Registrations) != 1 || len(s.Dissolutions) != 0 {
		t.Fatalf("%+v", s)
	}
	up.Dissolutions = map[chainhash.Hash]Spend{e.a.ProTxHash: want}
	if err := e.store.Save(up); err != nil {
		t.Fatal(err)
	}
	// Dropping a registration takes its dissolution with it.
	if err := e.store.Save(Update{Drop: []chainhash.Hash{e.a.ProTxHash}}); err != nil {
		t.Fatal(err)
	}
	s = e.snapshot()
	if len(s.Registrations) != 0 || len(s.Dissolutions) != 0 {
		t.Fatalf("drop left %+v", s)
	}
	// The owner key ids and counter only grow.
	if s.NextOwner != 4 || s.OwnerKeyIDs[e.idA] != 3 {
		t.Fatalf("drop touched the owners: %+v", s)
	}
	// What an update drops and puts is put.
	up = regUpdate(t, e)
	up.Drop = []chainhash.Hash{e.a.ProTxHash}
	if err := e.store.Save(up); err != nil {
		t.Fatal(err)
	}
	if len(e.snapshot().Registrations) != 1 {
		t.Fatal("an update that drops and puts a registration lost it")
	}
}

func TestStoreDissolutionNeedsRegistration(t *testing.T) {
	e := newEnv(t)
	sp := map[chainhash.Hash]Spend{e.a.ProTxHash: {Height: 900}}
	if err := e.store.Save(Update{Dissolutions: sp}); err == nil {
		t.Fatal("saved a dissolution with no registration")
	}
	if len(e.snapshot().Dissolutions) != 0 {
		t.Fatal("kept it")
	}
	// A dissolution of a stored registration is fine, in a later update.
	if err := e.store.Save(regUpdate(t, e)); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Save(Update{Dissolutions: sp}); err != nil {
		t.Fatal(err)
	}
	// A failed update changes nothing, including its cursor.
	bad := Update{Dissolutions: map[chainhash.Hash]Spend{{1}: {}}, Cursor: &Cursor{Height: 5}}
	if err := e.store.Save(bad); err == nil {
		t.Fatal("saved")
	}
	if e.snapshot().Cursor.Height != 0 {
		t.Fatal("failed update moved the cursor")
	}
}

func TestStoreLoadRefusesMalformedDissolutions(t *testing.T) {
	for name, put := range map[string]func(b walletdb.ReadWriteBucket, e *env) error{
		"no registration": func(b walletdb.ReadWriteBucket, e *env) error {
			return b.NestedReadWriteBucket(dissolvedBucket).Put(make([]byte, 32), make([]byte, 68))
		},
		"short value": func(b walletdb.ReadWriteBucket, e *env) error {
			return b.NestedReadWriteBucket(dissolvedBucket).Put(e.a.ProTxHash[:], make([]byte, 67))
		},
		"short key": func(b walletdb.ReadWriteBucket, e *env) error {
			return b.NestedReadWriteBucket(dissolvedBucket).Put([]byte{1}, make([]byte, 68))
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if err := e.store.Save(regUpdate(t, e)); err != nil {
				t.Fatal(err)
			}
			err := walletdb.Update(e.db, func(tx walletdb.ReadWriteTx) error {
				return put(tx.ReadWriteBucket(storeBucket), e)
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.store.Load(); err == nil {
				t.Fatal("loaded")
			}
			if _, err := NewManager(e.chain, e.store, e.keys, 1, regtestNet); err == nil {
				t.Fatal("manager started on a bad store")
			}
		})
	}
}

// A store made before the dissolved bucket existed gets it when it is opened.
func TestNewStoreAddsMissingBuckets(t *testing.T) {
	s, db, _ := newTestStore(t)
	defer db.Close()
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		return tx.ReadWriteBucket(storeBucket).DeleteNestedBucket(dissolvedBucket)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("loaded a store with a missing bucket")
	}
	if s, err = NewStore(db); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err != nil {
		t.Fatal(err)
	}
	// Opening again changes nothing.
	if _, err := NewStore(db); err != nil {
		t.Fatal(err)
	}
}
