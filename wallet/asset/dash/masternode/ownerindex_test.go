// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
)

type fakeStore struct {
	next      uint32
	persisted uint32 // number of indexes recorded as used
	err       error
}

func (f *fakeStore) ReserveOwnerIndex() (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	i := f.next
	f.next++
	f.persisted = f.next
	return i, nil
}

type fakeDeriver struct {
	err  error
	last []uint32
	// persistedAtDerive reports the store's persisted count when a key was
	// last derived.
	store             *fakeStore
	persistedAtDerive uint32
}

func (f *fakeDeriver) DeriveFromRootPath(path []uint32) (*btcec.PrivateKey, error) {
	f.last = path
	if f.store != nil {
		f.persistedAtDerive = f.store.persisted
	}
	if f.err != nil {
		return nil, f.err
	}
	// A key that depends on the index, so different indexes differ.
	b := make([]byte, 32)
	b[31] = byte(path[len(path)-1]) + 1
	k, _ := btcec.PrivKeyFromBytes(b)
	return k, nil
}

func TestNewShareOwner(t *testing.T) {
	s, d := &fakeStore{next: 5}, &fakeDeriver{}
	idx, id, err := NewShareOwner(s, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 5 {
		t.Fatalf("index %d", idx)
	}
	// The key id is the hash160 of the owner key at the reserved index.
	k, _ := OwnerKey(d, 5, 5)
	if want := btcutil.Hash160(k.PubKey().SerializeCompressed()); string(id[:]) != string(want) {
		t.Fatal("key id is not of the owner key at the reserved index")
	}
	idx2, id2, err := NewShareOwner(s, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if idx2 == idx || id2 == id {
		t.Fatal("a second owner reused the index or key")
	}
}

// The index must be recorded before the key is derived: a crash between the two
// then wastes an index instead of reusing it.
func TestNewShareOwnerPersistsBeforeDeriving(t *testing.T) {
	s := &fakeStore{}
	d := &fakeDeriver{err: errors.New("locked"), store: s}
	if _, _, err := NewShareOwner(s, d, 5); err == nil {
		t.Fatal("derive failure not reported")
	}
	if d.persistedAtDerive != 1 {
		t.Fatalf("index was not recorded before the key was derived: %d", d.persistedAtDerive)
	}
	// The failed index stays used, so the next owner gets a new one.
	d.err = nil
	if idx, _, err := NewShareOwner(s, d, 5); err != nil || idx != 1 {
		t.Fatalf("idx %d err %v", idx, err)
	}
}

func TestNewShareOwnerStoreError(t *testing.T) {
	boom := errors.New("disk full")
	d := &fakeDeriver{}
	_, _, err := NewShareOwner(&fakeStore{err: boom}, d, 5)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if d.last != nil {
		t.Fatal("derived a key without a reserved index")
	}
}
