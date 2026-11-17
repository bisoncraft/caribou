// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	_ "github.com/bisoncraft/caribou/wallet/utxowallet/walletdb/bdb"
)

func openDB(t *testing.T, path string) walletdb.DB {
	t.Helper()
	db, err := walletdb.Create("bdb", path, true, 10*time.Second)
	if err != nil {
		db, err = walletdb.Open("bdb", path, true, 10*time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func newTestStore(t *testing.T) (*Store, walletdb.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mn.db")
	db := openDB(t, path)
	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return s, db, path
}

func rawTx(t *testing.T, tx *bisonwire.Tx) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := tx.Serialize(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestStoreOwnerCounter(t *testing.T) {
	s, db, path := newTestStore(t)
	// Opening again must keep what is there.
	if _, err := NewStore(db); err != nil {
		t.Fatal(err)
	}
	for want := uint32(0); want < 3; want++ {
		got, err := s.ReserveOwnerIndex()
		if err != nil || got != want {
			t.Fatalf("got %d, %v, want %d", got, err, want)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The counter survives a restart: an index is never handed out twice.
	db = openDB(t, path)
	defer db.Close()
	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReserveOwnerIndex(); err != nil || got != 3 {
		t.Fatalf("after reopening: %d, %v", got, err)
	}
}

func TestStoreOwnerCounterMinNeverLowers(t *testing.T) {
	s, db, _ := newTestStore(t)
	defer db.Close()
	if err := s.Save(Update{MinNextOwner: 5}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReserveOwnerIndex(); got != 5 {
		t.Fatalf("got %d, want 5", got)
	}
	// The counter is now 6. A lower minimum must not move it back.
	if err := s.Save(Update{MinNextOwner: 2}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReserveOwnerIndex(); got != 6 {
		t.Fatalf("got %d, want 6", got)
	}
	snap, err := s.Load()
	if err != nil || snap.NextOwner != 7 {
		t.Fatalf("%+v, %v", snap, err)
	}
}

func TestStoreOwnerCounterExhausted(t *testing.T) {
	s, db, _ := newTestStore(t)
	defer db.Close()
	if err := s.Save(Update{MinNextOwner: maxMasternodeIdx}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReserveOwnerIndex(); err != nil || got != maxMasternodeIdx {
		t.Fatalf("the last index: %d, %v", got, err)
	}
	if got, err := s.ReserveOwnerIndex(); err == nil {
		t.Fatalf("handed out index %d past the last", got)
	}
	// A failed reservation must not wrap the counter.
	snap, err := s.Load()
	if err != nil || snap.NextOwner != maxMasternodeIdx+1 {
		t.Fatalf("%d, %v", snap.NextOwner, err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s, db, _ := newTestStore(t)
	defer db.Close()

	tx := specialTx(t, "proregtx")
	raw := rawTx(t, tx)
	id1, id2 := [20]byte{1}, [20]byte{2}
	cur := Cursor{Height: 910, Hash: chainhash.Hash{9, 9}}
	err := s.Save(Update{
		OwnerKeyIDs:   map[uint32][20]byte{4: id1, 7: id2},
		MinNextOwner:  8,
		Registrations: []Registration{{Height: 798, Tx: raw}},
		Cursor:        &cur,
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snap.NextOwner != 8 || snap.Cursor != cur {
		t.Fatalf("counter %d cursor %+v", snap.NextOwner, snap.Cursor)
	}
	if snap.OwnerKeyIDs[id1] != 4 || snap.OwnerKeyIDs[id2] != 7 || len(snap.OwnerKeyIDs) != 2 {
		t.Fatalf("owners %v", snap.OwnerKeyIDs)
	}
	reg, ok := snap.Registrations[tx.TxHash()]
	if !ok || len(snap.Registrations) != 1 || reg.Height != 798 || !bytes.Equal(reg.Tx, raw) {
		t.Fatalf("registrations %v", snap.Registrations)
	}
	// What comes back is what RegistrationMN reads.
	back := &bisonwire.Tx{Chain: "dash"}
	if err := back.Deserialize(bytes.NewReader(reg.Tx)); err != nil {
		t.Fatal(err)
	}
	m, err := RegistrationMN(back, reg.Height)
	if err != nil || m.ProTxHash != tx.TxHash() {
		t.Fatalf("%v", err)
	}

	// A save with no cursor leaves it, and saving the same transaction at
	// another height replaces the first, as a reorg can move it.
	if err := s.Save(Update{Registrations: []Registration{{Height: 801, Tx: raw}}}); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Load()
	if snap.Cursor != cur || len(snap.Registrations) != 1 || snap.Registrations[tx.TxHash()].Height != 801 {
		t.Fatalf("%+v", snap)
	}
}

// A registration whose owner key is cached raises the counter past that key's
// index by itself, so a restore that forgets MinNextOwner cannot reuse it.
func TestStoreRegistrationRaisesOwnerCounter(t *testing.T) {
	s, db, _ := newTestStore(t)
	defer db.Close()
	m := vectorMN(t)
	raw := rawTx(t, specialTx(t, "proregtx"))
	err := s.Save(Update{
		OwnerKeyIDs:   map[uint32][20]byte{3: m.Shares[0].KeyIDOwner, 9: {0xee}},
		Registrations: []Registration{{Height: 798, Tx: raw}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Index 3 is used by the registration, index 9 is only a probe.
	if got, _ := s.ReserveOwnerIndex(); got != 4 {
		t.Fatalf("got %d, want 4", got)
	}
	// Saving it again with a lower cached index must not lower the counter.
	err = s.Save(Update{
		OwnerKeyIDs:   map[uint32][20]byte{1: m.Shares[1].KeyIDOwner},
		Registrations: []Registration{{Height: 798, Tx: raw}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReserveOwnerIndex(); got != 5 {
		t.Fatalf("got %d, want 5", got)
	}
}

func TestStoreLoadRefusesDuplicateOwnerKeyIDs(t *testing.T) {
	s, db, _ := newTestStore(t)
	defer db.Close()
	err := s.Save(Update{OwnerKeyIDs: map[uint32][20]byte{1: {5}, 2: {5}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("loaded two indexes with one key id")
	}
}

func TestStoreSaveIsAtomic(t *testing.T) {
	s, db, _ := newTestStore(t)
	defer db.Close()
	good := rawTx(t, specialTx(t, "proregtx"))
	bad := rawTx(t, specialTx(t, "coinbase"))
	cur := Cursor{Height: 5}
	err := s.Save(Update{
		OwnerKeyIDs:   map[uint32][20]byte{1: {1}},
		MinNextOwner:  3,
		Registrations: []Registration{{Height: 798, Tx: good}, {Height: 798, Tx: bad}},
		Cursor:        &cur,
	})
	if err == nil {
		t.Fatal("saved an invalid registration")
	}
	snap, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snap.NextOwner != 0 || len(snap.OwnerKeyIDs) != 0 || len(snap.Registrations) != 0 || snap.Cursor != (Cursor{}) {
		t.Fatalf("a failed save wrote: %+v", snap)
	}
}

func TestStoreSaveRefusesBadRegistrations(t *testing.T) {
	s, db, _ := newTestStore(t)
	defer db.Close()
	good := rawTx(t, specialTx(t, "proregtx"))
	tests := map[string]Registration{
		"garbage":        {Height: 798, Tx: []byte{1, 2, 3}},
		"trailing bytes": {Height: 798, Tx: append(bytes.Clone(good), 0)},
		"height zero":    {Height: 0, Tx: good},
		"not a ProRegTx": {Height: 798, Tx: rawTx(t, specialTx(t, "prodistx"))},
		"empty":          {Height: 798},
	}
	for name, r := range tests {
		if err := s.Save(Update{Registrations: []Registration{r}}); err == nil {
			t.Errorf("%s: saved", name)
		}
	}
}

func TestStoreLoadRefusesMalformedRecords(t *testing.T) {
	s, db, _ := newTestStore(t)
	defer db.Close()
	put := func(f func(b walletdb.ReadWriteBucket) error) {
		t.Helper()
		err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
			return f(tx.ReadWriteBucket(storeBucket))
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	reset := func() {
		put(func(b walletdb.ReadWriteBucket) error {
			_ = b.Delete(nextKey)
			_ = b.Delete(cursorKey)
			_ = b.NestedReadWriteBucket(ownersBucket).Delete([]byte{0, 0, 0, 1})
			_ = b.NestedReadWriteBucket(ownersBucket).Delete([]byte{1})
			_ = b.NestedReadWriteBucket(regsBucket).Delete(make([]byte, 32))
			return nil
		})
	}
	var k4 [4]byte
	binary.BigEndian.PutUint32(k4[:], 1)
	cases := map[string]func(b walletdb.ReadWriteBucket) error{
		"counter": func(b walletdb.ReadWriteBucket) error { return b.Put(nextKey, []byte{1}) },
		"cursor":  func(b walletdb.ReadWriteBucket) error { return b.Put(cursorKey, []byte{1}) },
		"owner key": func(b walletdb.ReadWriteBucket) error {
			return b.NestedReadWriteBucket(ownersBucket).Put([]byte{1}, make([]byte, 20))
		},
		"owner value": func(b walletdb.ReadWriteBucket) error {
			return b.NestedReadWriteBucket(ownersBucket).Put(k4[:], []byte{1})
		},
		"registration": func(b walletdb.ReadWriteBucket) error {
			return b.NestedReadWriteBucket(regsBucket).Put(make([]byte, 32), []byte{1, 2})
		},
		"registration key": func(b walletdb.ReadWriteBucket) error {
			return b.NestedReadWriteBucket(regsBucket).Put([]byte{1}, []byte{1, 2, 3, 4, 5})
		},
	}
	for name, f := range cases {
		put(f)
		if _, err := s.Load(); err == nil {
			t.Errorf("%s: loaded", name)
		}
		put(func(b walletdb.ReadWriteBucket) error {
			_ = b.Delete(nextKey)
			_ = b.Delete(cursorKey)
			_ = b.NestedReadWriteBucket(ownersBucket).Delete([]byte{1})
			_ = b.NestedReadWriteBucket(ownersBucket).Delete(k4[:])
			_ = b.NestedReadWriteBucket(regsBucket).Delete(make([]byte, 32))
			_ = b.NestedReadWriteBucket(regsBucket).Delete([]byte{1})
			return nil
		})
	}
	reset()
	if _, err := s.Load(); err != nil {
		t.Fatalf("clean store: %v", err)
	}
}

// NewShareOwner works against the real store, and two registrations never get
// the same index.
func TestStoreHandsOutOwnerIndexes(t *testing.T) {
	s, db, _ := newTestStore(t)
	defer db.Close()
	d := &fakeDeriver{}
	i0, id0, err := NewShareOwner(s, d, 1)
	if err != nil {
		t.Fatal(err)
	}
	i1, id1, err := NewShareOwner(s, d, 1)
	if err != nil {
		t.Fatal(err)
	}
	if i0 != 0 || i1 != 1 || id0 == id1 {
		t.Fatalf("%d %d %x %x", i0, i1, id0, id1)
	}
}
