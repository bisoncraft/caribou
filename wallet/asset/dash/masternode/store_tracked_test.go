// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"

	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
)

// The wallet's own registrations and dissolutions wait in the store until a
// scan proves them, and leave it once they are proven.
func TestStoreTracked(t *testing.T) {
	e := newEnv(t)
	rawA, rawD := rawTx(t, e.txA), rawTx(t, e.txD)
	if err := e.store.Save(Update{Track: [][]byte{rawA, rawD}}); err != nil {
		t.Fatal(err)
	}
	snap := e.snapshot()
	if len(snap.Tracked) != 2 || !bytes.Equal(snap.Tracked[e.txA.TxHash()], rawA) || !bytes.Equal(snap.Tracked[e.hashD], rawD) {
		t.Fatalf("tracked %v", snap.Tracked)
	}
	// Tracking again is not an error, and nothing else is touched.
	if err := e.store.Save(Update{Track: [][]byte{rawA}}); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); len(got.Tracked) != 2 || got.NextOwner != 0 || len(got.Registrations) != 0 {
		t.Fatalf("%+v", got)
	}

	if err := e.store.Save(Update{Untrack: []chainhash.Hash{e.hashD}}); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot().Tracked; len(got) != 1 || got[e.txA.TxHash()] == nil {
		t.Fatalf("untrack left %v", got)
	}
}

// A registration that is stored is proven, so it is no longer tracked, and
// neither is a dissolution that is stored.
func TestStoreProvenLeavesTracked(t *testing.T) {
	e := newEnv(t)
	rawA, rawB, rawD := rawTx(t, e.txA), rawTx(t, e.txB), rawTx(t, e.txD)
	if err := e.store.Save(Update{Track: [][]byte{rawA, rawB, rawD}}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Save(Update{Registrations: []Registration{{Height: heightA, Tx: rawA}}}); err != nil {
		t.Fatal(err)
	}
	snap := e.snapshot()
	if _, still := snap.Tracked[e.a.ProTxHash]; still || len(snap.Tracked) != 2 {
		t.Fatalf("a stored registration is still tracked: %v", snap.Tracked)
	}

	up := Update{
		Registrations: []Registration{{Height: heightB, Tx: rawB}},
		Dissolutions:  map[chainhash.Hash]Spend{e.b.ProTxHash: {Height: heightD, TxID: e.hashD}},
	}
	if err := e.store.Save(up); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot().Tracked; len(got) != 0 {
		t.Fatalf("proven transactions are still tracked: %v", got)
	}
}

// A registration a reorg dropped is put back to be proven again, in the same
// update: Track is applied after Drop.
func TestStoreDropThenTrack(t *testing.T) {
	e := newEnv(t)
	rawA := rawTx(t, e.txA)
	if err := e.store.Save(Update{Registrations: []Registration{{Height: heightA, Tx: rawA}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Save(Update{Drop: []chainhash.Hash{e.a.ProTxHash}, Track: [][]byte{rawA}}); err != nil {
		t.Fatal(err)
	}
	snap := e.snapshot()
	if len(snap.Registrations) != 0 || snap.Tracked[e.a.ProTxHash] == nil {
		t.Fatalf("%+v", snap)
	}
}

// Only a shared registration or a dissolution can be tracked, and one bad
// transaction fails the whole update.
func TestStoreTrackRefuses(t *testing.T) {
	e := newEnv(t)
	good := rawTx(t, e.txA)
	notShared := rawTx(t, specialTx(t, "coinbase"))
	tests := map[string][]byte{
		"garbage":        {1, 2, 3},
		"empty":          nil,
		"trailing bytes": append(bytes.Clone(good), 0),
		"another type":   notShared,
	}
	for name, raw := range tests {
		err := e.store.Save(Update{Track: [][]byte{good, raw}, MinNextOwner: 5})
		if err == nil {
			t.Errorf("%s: tracked", name)
		}
	}
	if snap := e.snapshot(); len(snap.Tracked) != 0 || snap.NextOwner != 0 {
		t.Fatalf("a refused update wrote: %+v", snap)
	}
}

func TestStoreLoadRefusesMalformedTracked(t *testing.T) {
	e := newEnv(t)
	for name, kv := range map[string][2][]byte{
		"key":   {{1}, {1, 2, 3}},
		"empty": {make([]byte, chainhash.HashSize), nil},
	} {
		err := walletdb.Update(e.db, func(tx walletdb.ReadWriteTx) error {
			return tx.ReadWriteBucket(storeBucket).NestedReadWriteBucket(trackedBucket).Put(kv[0], kv[1])
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.Load(); err == nil {
			t.Errorf("%s: loaded", name)
		}
		err = walletdb.Update(e.db, func(tx walletdb.ReadWriteTx) error {
			return tx.ReadWriteBucket(storeBucket).NestedReadWriteBucket(trackedBucket).Delete(kv[0])
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.store.Load(); err != nil {
		t.Fatalf("clean store: %v", err)
	}
}
