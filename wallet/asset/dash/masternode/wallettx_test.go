// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"errors"
	"testing"
)

var errTooLarge = errors.New("history too large")

func TestIsProviderTx(t *testing.T) {
	e := newEnv(t)
	tests := map[string]struct {
		raw  []byte
		want bool
	}{
		"registration":   {rawTx(t, e.txA), true},
		"dissolution":    {rawTx(t, e.txD), true},
		"another type":   {rawTx(t, specialTx(t, "coinbase")), false},
		"too short":      {[]byte{3, 0, 1}, false},
		"empty":          {nil, false},
		"version 2 type": {[]byte{2, 0, 1, 0}, false},
	}
	for name, tc := range tests {
		if got := IsProviderTx(tc.raw); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestDiscovered(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	if done, err := m.Discovered(); err != nil || done {
		t.Fatalf("%v %v", done, err)
	}
	e.own(map[uint32][20]byte{1: e.idA}, 0)
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if done, err := m.Discovered(); err != nil || !done {
		t.Fatalf("%v %v", done, err)
	}

	// A discovery the server refuses has not read the history.
	e2 := newEnv(t)
	e2.chain.sharedErr = errTooLarge
	m2 := e2.manager()
	_ = m2.Discover(t.Context())
	if done, err := m2.Discovered(); err != nil || done {
		t.Fatalf("a refused discovery is done: %v %v", done, err)
	}
}

// Only what the wallet gave is the wallet's to drop: not what was put back by
// a reorg or added by id.
func TestTrackedIsWhatTheWalletGave(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA}, 0)
	m := e.manager()
	if err := m.TrackWallet(rawTx(t, e.txA), 0); err != nil {
		t.Fatal(err)
	}
	ids, err := m.Tracked()
	if err != nil || len(ids) != 1 || ids[0] != e.a.ProTxHash {
		t.Fatalf("%v %v", ids, err)
	}
	// Tracked again by the manager (as after a reorg), it is not the wallet's.
	if err := e.store.Save(Update{Track: [][]byte{rawTx(t, e.txA)}}); err != nil {
		t.Fatal(err)
	}
	if ids, err := m.Tracked(); err != nil || len(ids) != 0 {
		t.Fatalf("%v %v", ids, err)
	}
}

// A transaction added with Track is not the wallet's to drop: a dissolution
// the wallet's actor made may pay nothing to it, so the wallet never has it.
func TestTrackIsNotTheWalletsToDrop(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA}, 0)
	m := e.manager()
	if err := m.Track(rawTx(t, e.txA), 0); err != nil {
		t.Fatal(err)
	}
	if ids, err := m.Tracked(); err != nil || len(ids) != 0 {
		t.Fatalf("%v %v", ids, err)
	}
}
