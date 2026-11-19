// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

// lockedDeriver is the deriver of a locked wallet.
type lockedDeriver struct{ n atomic.Int32 }

func (l *lockedDeriver) DeriveFromRootPath([]uint32) (*btcec.PrivateKey, error) {
	l.n.Add(1)
	return nil, errors.New("wallet is locked")
}

func (e *env) lockedManager() (*Manager, *lockedDeriver) {
	e.t.Helper()
	l := &lockedDeriver{}
	m, err := NewManager(e.chain, e.store, l, 1, regtestNet)
	if err != nil {
		e.t.Fatal(err)
	}
	return m, l
}

// A scan of a locked wallet needs no key once the ids it looks at are cached.
func TestScanWhileLockedAfterPriming(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA}, 3)
	if err := e.manager().PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	m, l := e.lockedManager()
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() != e.a.Shares[0].Amount {
		t.Fatal("A not found")
	}
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if l.n.Load() != 0 {
		t.Fatalf("derived %d keys while locked", l.n.Load())
	}
}

// Without priming the same scan needs keys, fails, and saves nothing.
func TestScanWhileLockedWithoutPriming(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA}, 3)
	m, _ := e.lockedManager()
	if err := m.Discover(t.Context()); err == nil {
		t.Fatal("scanned with no keys")
	}
	if e.snapshot().Cursor != (Cursor{}) {
		t.Fatal("a failed scan moved the cursor")
	}
}

func TestPrimeOwnerKeysCachesTheWindow(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	if err := m.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	snap := e.snapshot()
	if len(snap.OwnerKeyIDs) != ownerGap || snap.NextOwner != 0 {
		t.Fatalf("%d ids cached, counter %d", len(snap.OwnerKeyIDs), snap.NextOwner)
	}
	// Nothing is derived again.
	n := e.keys.n
	if err := m.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	if e.keys.n != n {
		t.Fatal("derived keys that were cached")
	}
	// The window follows the counter.
	e.own(nil, 5)
	if err := m.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	if got := len(e.snapshot().OwnerKeyIDs); got != 5+ownerGap {
		t.Fatalf("%d ids cached, want %d", got, 5+ownerGap)
	}
}

func TestPrimeOwnerKeysLocked(t *testing.T) {
	e := newEnv(t)
	m, _ := e.lockedManager()
	if err := m.PrimeOwnerKeys(); err == nil {
		t.Fatal("primed a locked wallet")
	}
	if len(e.snapshot().OwnerKeyIDs) != 0 {
		t.Fatal("cached ids from a failed prime")
	}
}

func TestReserveOwner(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	for want := uint32(0); want < 3; want++ {
		idx, id, err := m.ReserveOwner()
		if err != nil {
			t.Fatal(err)
		}
		if idx != want {
			t.Fatalf("index %d, want %d", idx, want)
		}
		got, ok := m.OwnerIndex(id)
		if !ok || got != idx {
			t.Fatalf("owner index of the reserved id: %d %v", got, ok)
		}
		// Every id a scan looks at is cached.
		if n := len(e.snapshot().OwnerKeyIDs); n != int(idx)+1+ownerGap {
			t.Fatalf("%d ids cached after reserving %d", n, idx)
		}
	}
	// A locked wallet cannot reserve, and the counter does not move.
	lm, _ := e.lockedManager()
	if _, _, err := lm.ReserveOwner(); err == nil {
		t.Fatal("reserved while locked")
	}
}

func TestList(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA, 5: e.idB}, 0)
	m := e.manager()
	if got := m.List(); len(got) != 0 {
		t.Fatalf("%d before discovery", len(got))
	}
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := m.List()
	if len(got) != 2 {
		t.Fatalf("%d masternodes", len(got))
	}
	// Oldest first.
	b, a := got[0], got[1]
	if b.MN.ProTxHash != e.b.ProTxHash || a.MN.ProTxHash != e.a.ProTxHash {
		t.Fatal("not in order of registration")
	}
	if b.Dissolution == nil || b.Dissolution.TxID != e.hashD || a.Dissolution != nil {
		t.Fatalf("dissolutions %v %v", b.Dissolution, a.Dissolution)
	}
	if len(a.Shares) != 1 || a.Shares[0] != 0 {
		t.Fatalf("A's own shares %v", a.Shares)
	}
	// What it returns is a copy.
	a.MN.Shares[0].Amount = 1
	a.MN.Shares[0].RewardScript[0] ^= 0xff
	if again := m.List()[1]; again.MN.Shares[0].Amount != e.a.Shares[0].Amount ||
		again.MN.Shares[0].RewardScript[0] != e.a.Shares[0].RewardScript[0] {
		t.Fatal("List returned the stored masternode")
	}
	if _, ok := m.OwnerIndex(e.idA); !ok {
		t.Fatal("A's owner id is not known")
	}
	if _, ok := m.OwnerIndex([20]byte{9}); ok {
		t.Fatal("an unknown id has an index")
	}
}
