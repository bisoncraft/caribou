// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"

	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
)

// trackedEnv is an environment whose wallet owns the key of registration A and
// has tracked it.
func trackedEnv(t *testing.T) (*env, *Manager) {
	t.Helper()
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA, 2: e.idB}, 0)
	m := e.manager()
	if err := m.Track(rawTx(t, e.txA), 0); err != nil {
		t.Fatal(err)
	}
	return e, m
}

// A registration the wallet has the transaction of is proven once it is mined,
// without the history of the shared collateral script being read.
func TestTrackRegistration(t *testing.T) {
	e, m := trackedEnv(t)
	if m.StakedAmount() != 0 {
		t.Fatal("staked before the registration is proven")
	}
	if got := e.snapshot().Tracked; len(got) != 1 || got[e.a.ProTxHash] == nil {
		t.Fatalf("tracked %v", got)
	}
	// Tracking a registration of the wallet's moves the owner counter past
	// its key, as saving one does.
	if got := e.snapshot().NextOwner; got != 2 {
		t.Fatalf("next owner %d", got)
	}

	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() != e.a.Shares[0].Amount {
		t.Fatalf("staked %d", m.StakedAmount())
	}
	snap := e.snapshot()
	reg := snap.Registrations[e.a.ProTxHash]
	if reg.Height != heightA || reg.Block != e.chain.headers[heightA].BlockHash() || len(snap.Tracked) != 0 {
		t.Fatalf("%+v tracked %d", reg, len(snap.Tracked))
	}
	if e.chain.histCalls != 0 {
		t.Fatalf("the shared script's history was read %d times", e.chain.histCalls)
	}

	// Everything survives a restart.
	if e.manager().StakedAmount() != e.a.Shares[0].Amount {
		t.Fatal("not kept")
	}
}

// A registration that is not the wallet's is not kept, so what is tracked
// stays the wallet's own.
func TestTrackRefusesOthers(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	if err := m.Track(rawTx(t, e.txA), 0); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("%v", err)
	}
	for name, raw := range map[string][]byte{
		"garbage":      {1, 2, 3},
		"another type": rawTx(t, specialTx(t, "coinbase")),
	} {
		if err := m.Track(raw, 0); err == nil {
			t.Errorf("%s: tracked", name)
		}
	}
	if snap := e.snapshot(); len(snap.Tracked) != 0 || snap.NextOwner != 0 {
		t.Fatalf("%+v", snap)
	}
}

// A transaction that is not mined yet, or whose block the chain does not hold
// yet, stays tracked, and is proven once it is.
func TestTrackWaitsForTheBlock(t *testing.T) {
	e, m := trackedEnv(t)
	id := e.a.ProTxHash.String()
	block := e.chain.blocks[id]

	delete(e.chain.blocks, id)
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() != 0 || len(e.snapshot().Tracked) != 1 {
		t.Fatal("an unmined registration was taken")
	}

	// The server names a block the chain has no header for.
	e.chain.blocks[id] = chainhash.Hash{9}.String()
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() != 0 || len(e.snapshot().Tracked) != 1 {
		t.Fatal("took a registration in a block the chain does not hold")
	}

	e.chain.blocks[id] = block
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() != e.a.Shares[0].Amount || len(e.snapshot().Tracked) != 0 {
		t.Fatal("not proven once mined")
	}
}

// The height Track is given saves asking the server which block, and a wrong
// one only costs the request.
func TestTrackHeightHint(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA}, 0)
	m := e.manager()
	if err := m.Track(rawTx(t, e.txA), heightA); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() == 0 || e.chain.verboseCalls != 0 {
		t.Fatalf("staked %d, %d verbose calls", m.StakedAmount(), e.chain.verboseCalls)
	}

	e2 := newEnv(t)
	e2.own(map[uint32][20]byte{1: e2.idA}, 0)
	m2 := e2.manager()
	if err := m2.Track(rawTx(t, e2.txA), heightB); err != nil { // not the block
		t.Fatal(err)
	}
	if err := m2.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m2.StakedAmount() == 0 || e2.chain.verboseCalls != 1 {
		t.Fatalf("a wrong hint: staked %d, %d verbose calls", m2.StakedAmount(), e2.chain.verboseCalls)
	}
}

// A server that has no verbose transaction leaves a registration tracked, and
// the error says so; the height from the wallet proves it without the server.
func TestTrackWithoutVerboseTransactions(t *testing.T) {
	e, m := trackedEnv(t)
	e.chain.verboseErr = errors.New("verbose not supported")
	if err := m.Refresh(t.Context()); err == nil || m.StakedAmount() != 0 || len(e.snapshot().Tracked) != 1 {
		t.Fatalf("%v, staked %d", err, m.StakedAmount())
	}
	if err := m.Track(rawTx(t, e.txA), heightA); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(t.Context()); err != nil || m.StakedAmount() == 0 {
		t.Fatalf("%v, staked %d", err, m.StakedAmount())
	}
}

// The server only says where to look: a block it names that does not hold the
// transaction proves nothing.
func TestTrackRefusesAWrongBlock(t *testing.T) {
	e, m := trackedEnv(t)
	e.chain.blocks[e.a.ProTxHash.String()] = e.chain.headers[heightD].BlockHash().String()
	if err := m.Refresh(t.Context()); err == nil {
		t.Fatal("proved a registration by another block")
	}
	if m.StakedAmount() != 0 || len(e.snapshot().Registrations) != 0 || len(e.snapshot().Tracked) != 1 {
		t.Fatal("kept what was not proven")
	}
}

func TestTrackTxID(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA}, 0)
	m := e.manager()
	if err := m.TrackTxID(t.Context(), e.a.ProTxHash); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(t.Context()); err != nil || m.StakedAmount() == 0 {
		t.Fatalf("%v, staked %d", err, m.StakedAmount())
	}

	// The bytes must be the transaction with that id, and a server that has
	// none cannot make the wallet track one.
	e2 := newEnv(t)
	e2.own(map[uint32][20]byte{1: e2.idA}, 0)
	m2 := e2.manager()
	e2.chain.raws[e2.a.ProTxHash.String()] = e2.rawHex(e2.txB)
	if err := m2.TrackTxID(t.Context(), e2.a.ProTxHash); err == nil {
		t.Fatal("tracked the server's other transaction")
	}
	if err := m2.TrackTxID(t.Context(), chainhash.Hash{5}); err == nil {
		t.Fatal("tracked a transaction the server does not have")
	}
	if len(e2.snapshot().Tracked) != 0 {
		t.Fatal("tracked something refused")
	}
}

func TestUntrack(t *testing.T) {
	e, m := trackedEnv(t)
	if err := m.Untrack(e.a.ProTxHash); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() != 0 || len(e.snapshot().Tracked) != 0 {
		t.Fatal("untracked, but proven")
	}
}

// A tracked transaction that is stored is dropped from tracking, so tracking
// again what is known does not wait for ever.
func TestTrackOfAKnownTransactionIsUntracked(t *testing.T) {
	e, m := trackedEnv(t)
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Track(rawTx(t, e.txA), 0); err != nil {
		t.Fatal(err)
	}
	if len(e.snapshot().Tracked) != 1 {
		t.Fatal("not tracked")
	}
	proofs := e.chain.merkleCalls[e.a.ProTxHash.String()]
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(e.snapshot().Tracked) != 0 || m.StakedAmount() != e.a.Shares[0].Amount {
		t.Fatalf("tracked %d, staked %d", len(e.snapshot().Tracked), m.StakedAmount())
	}
	if got := e.chain.merkleCalls[e.a.ProTxHash.String()]; got != proofs {
		t.Fatalf("a registration that is proven was proven again")
	}

	// The same goes for a dissolution that was found in a refund history.
	e2 := newEnv(t)
	e2.own(map[uint32][20]byte{2: e2.idB}, 0)
	m2 := e2.manager()
	if err := m2.Track(rawTx(t, e2.txB), 0); err != nil {
		t.Fatal(err)
	}
	e2.refundHistory(e2.b, 0, entry(heightD, e2.hashD))
	if err := m2.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m2.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m2.Track(rawTx(t, e2.txD), 0); err != nil {
		t.Fatal(err)
	}
	if err := m2.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(e2.snapshot().Tracked) != 0 {
		t.Fatal("a dissolution that is stored is still tracked")
	}
}

func TestTrackBelowTheHeadersIsReported(t *testing.T) {
	e, m := trackedEnv(t)
	if err := m.Track(rawTx(t, e.txA), heightA); err != nil {
		t.Fatal(err)
	}
	e.chain.start = heightA + 1
	if err := m.Refresh(t.Context()); !errors.Is(err, ErrUnprovable) {
		t.Fatalf("%v", err)
	}
	// It is reported once and not waited for.
	if len(e.snapshot().Tracked) != 0 {
		t.Fatal("still tracked")
	}
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatalf("reported again: %v", err)
	}
}

// What Refresh needs to find a dissolution is the history of the refund
// scripts of the wallet's shares, which Dash Core makes every dissolution but
// the actor's own pay.
func TestRefreshFindsADissolutionInTheRefundHistory(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{2: e.idB}, 0)
	m := e.manager()
	if err := m.Track(rawTx(t, e.txB), 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := m.StakedAmount(); got != e.b.Shares[0].Amount {
		t.Fatalf("staked %d", got)
	}

	// Its history holds entries that are not the dissolution, and they are
	// read once; the mempool, the headers' future and the time before the
	// registration are not read.
	e.refundHistory(e.b, 0,
		entry(heightB-1, e.hashD),     // before the registration
		entry(heightA, e.a.ProTxHash), // a transaction that spends nothing of B's
		electrum.HistoryEntry{Height: 0, TxHash: e.hashD.String()},
		electrum.HistoryEntry{Height: tipHeight + 1, TxHash: e.hashD.String()},
	)
	for range 3 {
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if m.StakedAmount() != e.b.Shares[0].Amount {
		t.Fatal("dissolved by what is not a dissolution")
	}
	if got := e.chain.rawCalls[e.a.ProTxHash.String()]; got != 1 {
		t.Fatalf("an irrelevant transaction was read %d times", got)
	}
	if got := e.chain.rawCalls[e.hashD.String()]; got != 0 {
		t.Fatalf("an entry that is not mined, or not yet, or earlier, was read %d times", got)
	}

	e.refundHistory(e.b, 0, entry(heightA, e.a.ProTxHash), entry(heightD, e.hashD))
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() != 0 {
		t.Fatal("not dissolved")
	}
	if _, err := m.SharedMN(t.Context(), e.b.ProTxHash, false); !errors.Is(err, ErrDissolved) {
		t.Fatalf("%v", err)
	}
	sp := e.snapshot().Dissolutions[e.b.ProTxHash]
	if sp.Height != heightD || sp.TxID != e.hashD || sp.Block != e.chain.headers[heightD].BlockHash() {
		t.Fatalf("%+v", sp)
	}
	if e.chain.histCalls != 0 {
		t.Fatalf("the shared script's history was read %d times", e.chain.histCalls)
	}

	// A masternode that is dissolved is not watched any more.
	e.chain.histTotal = 0
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.chain.histTotal != 0 {
		t.Fatalf("read %d histories of a dissolved masternode", e.chain.histTotal)
	}
}

// A share the wallet does not own is not watched: its refund script is not the
// wallet's, and may be anyone's.
func TestRefreshWatchesOnlyTheWalletsShares(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{2: e.idB}, 0)
	m := e.manager()
	if err := m.Track(rawTx(t, e.txB), 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	owned := 0
	for i := range e.b.Shares {
		if _, ok := e.snapshot().OwnerKeyIDs[e.b.Shares[i].KeyIDOwner]; ok {
			owned++
		}
	}
	e.chain.histTotal = 0
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.chain.histTotal != owned {
		t.Fatalf("read %d histories for %d owned shares", e.chain.histTotal, owned)
	}
}

// A dissolution the wallet made as the actor can leave out its own output, so
// the refund history does not show it, and Track adds it.
func TestTrackADissolution(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{2: e.idB}, 0)
	m := e.manager()
	if err := m.Track(rawTx(t, e.txB), 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Track(rawTx(t, e.txD), heightD); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SharedMN(t.Context(), e.b.ProTxHash, false); !errors.Is(err, ErrDissolved) {
		t.Fatalf("%v", err)
	}
	if snap := e.snapshot(); len(snap.Tracked) != 0 || snap.Dissolutions[e.b.ProTxHash].TxID != e.hashD {
		t.Fatalf("%+v", snap)
	}
}

// A block that was undone takes what it proved with it, and the transaction is
// proven again by the block it is in now, found by the server or the wallet.
func TestRefreshReorg(t *testing.T) {
	setup := func(t *testing.T) (*env, *Manager) {
		e := newEnv(t)
		e.own(map[uint32][20]byte{1: e.idA, 2: e.idB}, 0)
		m := e.manager()
		for _, tx := range [][]byte{rawTx(t, e.txA), rawTx(t, e.txB)} {
			if err := m.Track(tx, 0); err != nil {
				t.Fatal(err)
			}
		}
		e.refundHistory(e.b, 0, entry(heightD, e.hashD))
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := m.SharedMN(t.Context(), e.b.ProTxHash, false); !errors.Is(err, ErrDissolved) || m.StakedAmount() != e.a.Shares[0].Amount {
			t.Fatal("setup")
		}
		return e, m
	}

	// A reorg that does not reach a block leaves what it proved, whatever the
	// server says now: it cannot make the wallet forget a masternode or
	// un-dissolve one.
	t.Run("the server forgets", func(t *testing.T) {
		e, m := setup(t)
		e.chain.blocks = map[string]string{}
		e.refundHistory(e.b, 0)
		e.chain.headers[tipHeight] = synthHeader(chainhash.Hash{7}, tipHeight)
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, mm := range []*Manager{m, e.manager()} {
			if mm.StakedAmount() != e.a.Shares[0].Amount {
				t.Fatalf("staked %d", mm.StakedAmount())
			}
			if _, err := mm.SharedMN(t.Context(), e.b.ProTxHash, false); !errors.Is(err, ErrDissolved) {
				t.Fatalf("B: %v", err)
			}
		}
	})
	t.Run("registration moved", func(t *testing.T) {
		e, m := setup(t)
		id := e.a.ProTxHash.String()
		e.chain.headers[heightA] = synthHeader(chainhash.Hash{8}, heightA)
		e.chain.headers[800] = synthHeader(e.a.ProTxHash, 800)
		e.chain.proofs[id] = &electrum.MerkleProof{BlockHeight: 800}
		e.chain.blocks[id] = e.chain.headers[800].BlockHash().String()
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, mm := range []*Manager{m, e.manager()} {
			c, err := mm.SharedMN(t.Context(), e.a.ProTxHash, false)
			if err != nil {
				t.Fatal(err)
			}
			if c.RegisteredHeight != 800 {
				t.Fatalf("height %d", c.RegisteredHeight)
			}
		}
	})
	t.Run("registration's block gone", func(t *testing.T) {
		e, m := setup(t)
		id := e.a.ProTxHash.String()
		e.chain.headers[heightA] = synthHeader(chainhash.Hash{8}, heightA)
		delete(e.chain.blocks, id)
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if m.StakedAmount() != 0 {
			t.Fatal("kept a registration whose block is gone")
		}
		// It is tracked again, and the counter does not go back.
		if got := e.snapshot(); got.Tracked[e.a.ProTxHash] == nil || got.NextOwner != 3 {
			t.Fatalf("tracked %d, next owner %d", len(got.Tracked), got.NextOwner)
		}
		// A new chain has it in another block, which proves it.
		e.chain.headers[heightA] = synthHeader(e.a.ProTxHash, heightA)
		e.chain.proofs[id] = &electrum.MerkleProof{BlockHeight: heightA}
		e.chain.blocks[id] = e.chain.headers[heightA].BlockHash().String()
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if m.StakedAmount() != e.a.Shares[0].Amount {
			t.Fatal("not proven by the new block")
		}
	})
	t.Run("dissolution's block gone", func(t *testing.T) {
		e, m := setup(t)
		e.chain.headers[heightD] = synthHeader(chainhash.Hash{8}, heightD)
		e.refundHistory(e.b, 0)
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(e.snapshot().Dissolutions) != 0 {
			t.Fatal("dissolution kept")
		}
		want := e.a.Shares[0].Amount + e.b.Shares[0].Amount
		if got := m.StakedAmount(); got != want {
			t.Fatalf("staked %d, want %d", got, want)
		}

		// The transaction is mined again, in another block.
		e.chain.headers[heightD] = synthHeader(e.hashD, heightD)
		e.chain.blocks[e.hashD.String()] = e.chain.headers[heightD].BlockHash().String()
		e.refundHistory(e.b, 0, entry(heightD, e.hashD))
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if sp := e.snapshot().Dissolutions[e.b.ProTxHash]; sp.Block != e.chain.headers[heightD].BlockHash() {
			t.Fatalf("%+v", sp)
		}
	})
	t.Run("registration's block gone takes its dissolution", func(t *testing.T) {
		e, m := setup(t)
		e.chain.headers[heightB] = synthHeader(chainhash.Hash{8}, heightB)
		e.chain.blocks = map[string]string{}
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		snap := e.snapshot()
		if len(snap.Registrations) != 1 || len(snap.Dissolutions) != 0 {
			t.Fatalf("%d registrations, %d dissolutions", len(snap.Registrations), len(snap.Dissolutions))
		}
		if snap.Tracked[e.b.ProTxHash] == nil {
			t.Fatal("not tracked again")
		}
	})
}

// A spend below the headers that were proven stays proven: no reorg reaches it.
func TestRefreshKeepsProofBelowStart(t *testing.T) {
	e, m := trackedEnv(t)
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.chain.start = heightA + 1
	delete(e.chain.headers, heightA)
	e.chain.headers[tipHeight] = synthHeader(chainhash.Hash{7}, tipHeight)
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() != e.a.Shares[0].Amount {
		t.Fatal("dropped a registration below the start")
	}
}

// Without the shared script's history, Discover reports why, and Refresh is not
// affected: it never reads it.
func TestDiscoverReportsAServerThatRefuses(t *testing.T) {
	e, m := trackedEnv(t)
	e.chain.sharedErr = errors.New("history too large")
	if err := m.Discover(t.Context()); !errors.Is(err, ErrSharedHistory) {
		t.Fatalf("%v", err)
	}
	if e.snapshot().Cursor.Height != 0 {
		t.Fatal("a failed discovery saved its cursor")
	}
	if err := m.Refresh(t.Context()); err != nil || m.StakedAmount() == 0 {
		t.Fatalf("%v, staked %d", err, m.StakedAmount())
	}
}
