// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
)

// The chain here is a fake that serves the real vectors: the registration
// 8e7019ff (A) with its real header and merkle proof at 798, the registration
// db7fab62 (B) and the transaction 410de96f (D) that dissolves B. B and D have
// no real proofs, so their blocks are synthetic headers whose merkle root is
// the transaction id, which a proof with an empty branch proves.

const tipHeight = 910

type fakeChain struct {
	mu      sync.Mutex
	tip     int32
	start   int32
	headers map[int32]*wire.BlockHeader
	raws    map[string]string
	proofs  map[string]*electrum.MerkleProof
	history []electrum.HistoryEntry
	info    *electrum.ProTxInfo
	infoErr error

	rawCalls    map[string]int
	merkleCalls map[string]int
	histCalls   int
	infoCalls   int
}

func (f *fakeChain) Tip() int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tip
}

func (f *fakeChain) StartHeight() int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.start
}

func (f *fakeChain) HeaderByHeight(h int32) *wire.BlockHeader {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers[h]
}

func (f *fakeChain) GetHistory(_ context.Context, sh string) ([]electrum.HistoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.histCalls++
	if sh != sharedCollateralScripthash {
		return nil, errors.New("wrong scripthash")
	}
	return slices.Clone(f.history), nil
}

func (f *fakeChain) GetRawTransaction(_ context.Context, txid string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawCalls[txid]++
	r, ok := f.raws[txid]
	if !ok {
		return "", errors.New("no such transaction")
	}
	return r, nil
}

func (f *fakeChain) GetMerkle(_ context.Context, txid string, _ int32) (*electrum.MerkleProof, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merkleCalls[txid]++
	p, ok := f.proofs[txid]
	if !ok {
		return nil, errors.New("no proof")
	}
	c := *p
	c.Merkle = slices.Clone(p.Merkle)
	return &c, nil
}

func (f *fakeChain) ProTxInfo(context.Context, chainhash.Hash) (*electrum.ProTxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCalls++
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	c := *f.info
	c.State.Shares = slices.Clone(f.info.State.Shares)
	return &c, nil
}

func synthHeader(root chainhash.Hash, h int32) *wire.BlockHeader {
	return &wire.BlockHeader{
		Version:    1,
		MerkleRoot: root,
		Timestamp:  time.Unix(1_700_000_000+int64(h), 0),
		Bits:       0x207fffff,
		Nonce:      uint32(h),
	}
}

type countingDeriver struct {
	fakeDeriver
	mu sync.Mutex
	n  int
}

func (c *countingDeriver) DeriveFromRootPath(p []uint32) (*btcec.PrivateKey, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.fakeDeriver.DeriveFromRootPath(p)
}

type env struct {
	t     *testing.T
	chain *fakeChain
	store *Store
	db    walletdb.DB
	keys  *countingDeriver
	a, b  *SharedMN
	idA   [20]byte
	idB   [20]byte
	txA   *bisonwire.Tx
	txB   *bisonwire.Tx
	txD   *bisonwire.Tx
	hashD chainhash.Hash
}

const (
	heightA = 798
	heightB = 781
	heightD = 790
)

func newEnv(t *testing.T) *env {
	t.Helper()
	st, db, _ := newTestStore(t)
	e := &env{t: t, store: st, db: db, keys: &countingDeriver{}}
	e.txA = specialTx(t, "proregtx")
	e.txB = dissolvedRegTx(t)
	e.txD = specialTx(t, "prodistx")
	var err error
	if e.a, err = RegistrationMN(e.txA, heightA); err != nil {
		t.Fatal(err)
	}
	if e.b, err = RegistrationMN(e.txB, heightB); err != nil {
		t.Fatal(err)
	}
	if !e.b.SpendsCollateral(e.txD) {
		t.Fatal("vector D does not spend B")
	}
	e.hashD = e.txD.TxHash()
	e.idA = e.a.Shares[0].KeyIDOwner
	e.idB = e.b.Shares[0].KeyIDOwner

	f := &fakeChain{
		tip:         tipHeight,
		headers:     map[int32]*wire.BlockHeader{},
		raws:        map[string]string{},
		proofs:      map[string]*electrum.MerkleProof{},
		rawCalls:    map[string]int{},
		merkleCalls: map[string]int{},
		info:        loadInfo(t, "protx_info_shared.json"),
	}
	e.chain = f
	f.headers[tipHeight] = synthHeader(chainhash.Hash{}, tipHeight)

	var real struct {
		BlockHeight int32    `json:"block_height"`
		Merkle      []string `json:"merkle"`
		Pos         uint32   `json:"pos"`
		Header      string   `json:"header"`
	}
	readJSON(t, "merkle_proregtx.json", &real)
	hb, err := hex.DecodeString(real.Header)
	if err != nil {
		t.Fatal(err)
	}
	var hdr wire.BlockHeader
	if err := hdr.Deserialize(bytes.NewReader(hb)); err != nil {
		t.Fatal(err)
	}
	f.headers[heightA] = &hdr
	idA := e.a.ProTxHash.String()
	f.proofs[idA] = &electrum.MerkleProof{BlockHeight: real.BlockHeight, Merkle: real.Merkle, Pos: real.Pos}
	f.raws[idA] = e.rawHex(e.txA)
	f.history = append(f.history, electrum.HistoryEntry{Height: heightA, TxHash: idA})

	e.addSynth(e.txB, heightB)
	e.addSynth(e.txD, heightD)
	return e
}

func (e *env) rawHex(tx *bisonwire.Tx) string {
	return hex.EncodeToString(rawTx(e.t, tx))
}

// addSynth serves tx as mined at height in a synthetic block, and lists it in
// the history.
func (e *env) addSynth(tx *bisonwire.Tx, height int32) {
	id := tx.TxHash()
	s := id.String()
	e.chain.headers[height] = synthHeader(id, height)
	e.chain.proofs[s] = &electrum.MerkleProof{BlockHeight: height}
	e.chain.raws[s] = e.rawHex(tx)
	e.chain.history = append(e.chain.history, electrum.HistoryEntry{Height: height, TxHash: s})
}

// setHistory replaces the history with entries of the given transactions.
func (e *env) setHistory(es ...electrum.HistoryEntry) {
	e.chain.history = es
}

// own caches the key id at index idx, which is what an earlier derivation or
// a registration built by this wallet leaves.
func (e *env) own(ids map[uint32][20]byte, minNext uint32) {
	e.t.Helper()
	if err := e.store.Save(Update{OwnerKeyIDs: ids, MinNextOwner: minNext}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) manager() *Manager {
	e.t.Helper()
	m, err := NewManager(e.chain, e.store, e.keys, 1, regtestNet)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) snapshot() Snapshot {
	e.t.Helper()
	s, err := e.store.Load()
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func entry(h int32, id chainhash.Hash) electrum.HistoryEntry {
	return electrum.HistoryEntry{Height: h, TxHash: id.String()}
}

func TestScripthashConstant(t *testing.T) {
	sum := sha256.Sum256(SharedCollateralScript)
	slices.Reverse(sum[:])
	if got := hex.EncodeToString(sum[:]); got != sharedCollateralScripthash {
		t.Fatalf("scripthash %s", got)
	}
}

func TestDiscover(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA, 5: e.idB}, 0)
	e.chain.history = append(e.chain.history, electrum.HistoryEntry{Height: 0, TxHash: "ff" + e.chain.history[0].TxHash[2:]})
	m := e.manager()
	if m.StakedAmount() != 0 {
		t.Fatal("staked before discovery")
	}
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A is ours and live. B is ours, and D dissolved it.
	if got := m.StakedAmount(); got != e.a.Shares[0].Amount {
		t.Fatalf("staked %d, want %d", got, e.a.Shares[0].Amount)
	}
	if _, err := m.SharedMN(t.Context(), e.a.ProTxHash, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SharedMN(t.Context(), e.b.ProTxHash, false); !errors.Is(err, ErrDissolved) {
		t.Fatalf("B: %v", err)
	}
	if _, err := m.SharedMN(t.Context(), chainhash.Hash{1}, false); !errors.Is(err, ErrUnknownMN) {
		t.Fatalf("unknown: %v", err)
	}
	snap := e.snapshot()
	if snap.Cursor.Height != tipHeight || snap.Cursor.Hash != e.chain.headers[tipHeight].BlockHash() {
		t.Fatalf("cursor %+v", snap.Cursor)
	}
	// Registering B at index 5 used the indexes below it.
	if snap.NextOwner != 6 {
		t.Fatalf("next owner %d", snap.NextOwner)
	}
	if sp := snap.Dissolutions[e.b.ProTxHash]; sp.Height != heightD || sp.TxID != e.hashD {
		t.Fatalf("dissolution %+v", sp)
	}
	// Only the mined entries were read, and only ours were proven.
	if len(e.chain.rawCalls) != 3 {
		t.Fatalf("raw calls %v", e.chain.rawCalls)
	}
	if e.chain.merkleCalls[e.a.ProTxHash.String()] != 1 || e.chain.merkleCalls[e.hashD.String()] != 1 {
		t.Fatalf("merkle calls %v", e.chain.merkleCalls)
	}

	// A second scan of an unchanged chain fetches and proves nothing again.
	before := len(e.chain.merkleCalls)
	rawBefore := e.chain.rawCalls[e.hashD.String()]
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.chain.rawCalls[e.hashD.String()] != rawBefore || e.chain.merkleCalls[e.hashD.String()] != 1 || len(e.chain.merkleCalls) != before {
		t.Fatalf("a second scan read again: %v %v", e.chain.rawCalls, e.chain.merkleCalls)
	}

	// Everything survives a restart, with no network.
	m2 := e.manager()
	if m2.StakedAmount() != e.a.Shares[0].Amount {
		t.Fatalf("restored staked %d", m2.StakedAmount())
	}
	if _, err := m2.SharedMN(t.Context(), e.b.ProTxHash, false); !errors.Is(err, ErrDissolved) {
		t.Fatalf("restored B: %v", err)
	}
}

func TestDiscoverGap(t *testing.T) {
	cases := []struct {
		name     string
		idx      uint32
		minNext  uint32
		found    bool
		derived  int
		wantNext uint32
	}{
		// Misses 0 to 18, then a hit at 19 moves the limit to 40.
		{"last index in the gap", 19, 0, true, 39, 20},
		{"first index past the gap", 20, 0, false, 20, 0},
		// The counter is 30, so indexes below 50 are looked at.
		{"gap starts at the counter", 45, 30, true, 65, 46},
		{"past the counter's gap", 50, 30, false, 50, 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.setHistory(entry(heightA, e.a.ProTxHash))
			e.own(map[uint32][20]byte{c.idx: e.idA}, c.minNext)
			m := e.manager()
			if err := m.Discover(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := m.StakedAmount() > 0; got != c.found {
				t.Fatalf("found %v", got)
			}
			if e.keys.n != c.derived {
				t.Fatalf("derived %d keys, want %d", e.keys.n, c.derived)
			}
			if got := e.snapshot().NextOwner; got != c.wantNext {
				t.Fatalf("next owner %d, want %d", got, c.wantNext)
			}
		})
	}
}

// With no registration in the history there is nothing to match, so no key is
// derived: a locked wallet can still scan.
func TestDiscoverNoRegistrationsDerivesNothing(t *testing.T) {
	e := newEnv(t)
	e.setHistory()
	m := e.manager()
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.keys.n != 0 {
		t.Fatalf("derived %d keys", e.keys.n)
	}
}

func TestDiscoverRejectsUnprovenData(t *testing.T) {
	idA := func(e *env) string { return e.a.ProTxHash.String() }
	cases := map[string]func(e *env){
		"wrong sibling": func(e *env) {
			p := e.chain.proofs[idA(e)]
			p.Merkle = []string{e.hashD.String()}
		},
		"wrong proof height": func(e *env) { e.chain.proofs[idA(e)].BlockHeight = heightA + 1 },
		"no header":          func(e *env) { delete(e.chain.headers, heightA) },
		"history height is not the proof's": func(e *env) {
			e.chain.headers[heightA+1] = synthHeader(e.a.ProTxHash, heightA+1)
			e.setHistory(entry(heightA+1, e.a.ProTxHash))
		},
		"proof missing":  func(e *env) { delete(e.chain.proofs, idA(e)) },
		"spend unproven": func(e *env) { e.chain.proofs[e.hashD.String()].Merkle = []string{idA(e)} },
		"spend in a block that is not the header's": func(e *env) {
			e.chain.headers[heightD] = synthHeader(chainhash.Hash{9}, heightD)
		},
		"wrong transaction served": func(e *env) { e.chain.raws[idA(e)] = e.rawHex(e.txB) },
		"transaction garbage":      func(e *env) { e.chain.raws[idA(e)] = "zz" },
		"trailing bytes":           func(e *env) { e.chain.raws[idA(e)] += "00" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.own(map[uint32][20]byte{1: e.idA, 2: e.idB}, 0)
			mutate(e)
			m := e.manager()
			if err := m.Discover(t.Context()); err == nil {
				t.Fatal("discovered from unproven data")
			}
			snap := e.snapshot()
			if len(snap.Registrations) != 0 || len(snap.Dissolutions) != 0 || snap.Cursor != (Cursor{}) {
				t.Fatalf("saved %+v", snap)
			}
			if m.StakedAmount() != 0 {
				t.Fatal("staked from unproven data")
			}
		})
	}
}

func TestProveRefuses64ByteTransaction(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	if _, err := m.prove(t.Context(), e.a.ProTxHash, make([]byte, 64), heightA); err == nil {
		t.Fatal("proved a 64 byte transaction")
	}
	block, err := m.prove(t.Context(), e.a.ProTxHash, rawTx(t, e.txA), heightA)
	if err != nil {
		t.Fatal(err)
	}
	if block != e.chain.headers[heightA].BlockHash() {
		t.Fatal("not the block's fingerprint")
	}
}

// A registration whose owners are not ours is not proven, and a spend that
// does not spend one of ours is not either.
func TestDiscoverIgnoresOthers(t *testing.T) {
	e := newEnv(t)
	e.setHistory(entry(heightB, e.b.ProTxHash), entry(heightD, e.hashD))
	m := e.manager()
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(e.chain.merkleCalls) != 0 {
		t.Fatalf("proved %v", e.chain.merkleCalls)
	}
	if len(e.snapshot().Registrations) != 0 || m.StakedAmount() != 0 {
		t.Fatal("found a masternode that is not ours")
	}
}

func TestRefreshIsIncremental(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA, 2: e.idB}, 0)
	e.setHistory(entry(heightA, e.a.ProTxHash))
	m := e.manager()
	if err := m.Refresh(t.Context()); err != nil { // no cursor: a full scan
		t.Fatal(err)
	}
	if m.StakedAmount() != e.a.Shares[0].Amount {
		t.Fatal("A not found")
	}

	// B is mined above the cursor's re-read window, then D dissolves it.
	e.addSynth(e.txB, 905)
	e.chain.history = slices.DeleteFunc(e.chain.history, func(h electrum.HistoryEntry) bool { return h.Height != 905 && h.TxHash != e.a.ProTxHash.String() })
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := m.StakedAmount(); got != e.a.Shares[0].Amount+e.b.Shares[0].Amount {
		t.Fatalf("staked %d", got)
	}
	if e.chain.rawCalls[e.a.ProTxHash.String()] != 1 {
		t.Fatalf("A was read %d times", e.chain.rawCalls[e.a.ProTxHash.String()])
	}
	e.addSynth(e.txD, 908)
	e.chain.history = slices.DeleteFunc(e.chain.history, func(h electrum.HistoryEntry) bool { return h.Height == heightD })
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := m.StakedAmount(); got != e.a.Shares[0].Amount {
		t.Fatalf("staked after dissolution %d", got)
	}
	if e.chain.rawCalls[e.b.ProTxHash.String()] != 1 {
		t.Fatalf("B was read %d times", e.chain.rawCalls[e.b.ProTxHash.String()])
	}
	if e.chain.rawCalls[e.hashD.String()] != 1 {
		t.Fatalf("D was read %d times", e.chain.rawCalls[e.hashD.String()])
	}
}

// An incremental scan re-reads a few blocks below the cursor, because the
// history of a server that has just caught up can lag its headers.
func TestRefreshRescanDepth(t *testing.T) {
	for _, c := range []struct {
		height int32
		found  bool
	}{
		{tipHeight - rescanDepth + 1, true},
		{tipHeight - rescanDepth, false},
	} {
		e := newEnv(t)
		e.own(map[uint32][20]byte{1: e.idA, 2: e.idB}, 0)
		e.setHistory(entry(heightA, e.a.ProTxHash))
		m := e.manager()
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		e.addSynth(e.txB, c.height)
		e.chain.history = slices.DeleteFunc(e.chain.history, func(h electrum.HistoryEntry) bool { return h.Height == heightB || h.Height == heightD })
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := m.StakedAmount() > e.a.Shares[0].Amount; got != c.found {
			t.Fatalf("height %d: found %v", c.height, got)
		}
		// A full scan finds it either way.
		if err := m.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		if m.StakedAmount() <= e.a.Shares[0].Amount {
			t.Fatalf("height %d: discover missed it", c.height)
		}
	}
}

// Without a reorg an incremental scan trusts what it has: a registration the
// server stops listing stays.
func TestRefreshWithoutReorgKeeps(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA}, 0)
	e.setHistory(entry(heightA, e.a.ProTxHash))
	m := e.manager()
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.setHistory()
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() == 0 {
		t.Fatal("dropped a registration with no reorg")
	}
}

func TestRefreshReorg(t *testing.T) {
	setup := func(t *testing.T) (*env, *Manager) {
		e := newEnv(t)
		e.own(map[uint32][20]byte{1: e.idA, 2: e.idB}, 0)
		m := e.manager()
		if err := m.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		if m.StakedAmount() != e.a.Shares[0].Amount {
			t.Fatal("setup")
		}
		// The block at the cursor changes.
		e.chain.headers[tipHeight] = synthHeader(chainhash.Hash{7}, tipHeight)
		return e, m
	}
	// A reorg that does not reach a block leaves what it proved, whatever the
	// server's history says now: the server must not be able to make the wallet
	// forget a masternode or un-dissolve one.
	t.Run("history omits what the reorg did not touch", func(t *testing.T) {
		e, m := setup(t)
		e.setHistory()
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, mm := range []*Manager{m, e.manager()} {
			if got := mm.StakedAmount(); got != e.a.Shares[0].Amount {
				t.Fatalf("staked %d", got)
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
		e.setHistory(entry(800, e.a.ProTxHash), entry(heightB, e.b.ProTxHash), entry(heightD, e.hashD))
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
		e.chain.headers[heightA] = synthHeader(chainhash.Hash{8}, heightA)
		e.setHistory(entry(heightB, e.b.ProTxHash), entry(heightD, e.hashD))
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, mm := range []*Manager{m, e.manager()} {
			if _, err := mm.SharedMN(t.Context(), e.a.ProTxHash, false); !errors.Is(err, ErrUnknownMN) {
				t.Fatalf("A: %v", err)
			}
			if mm.StakedAmount() != 0 {
				t.Fatal("staked")
			}
		}
		// The counter does not go back.
		if e.snapshot().NextOwner != 3 {
			t.Fatalf("next owner %d", e.snapshot().NextOwner)
		}
	})
	t.Run("dissolution's block gone", func(t *testing.T) {
		e, m := setup(t)
		e.chain.headers[heightD] = synthHeader(chainhash.Hash{8}, heightD)
		e.setHistory(entry(heightA, e.a.ProTxHash), entry(heightB, e.b.ProTxHash))
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		want := e.a.Shares[0].Amount + e.b.Shares[0].Amount
		for _, mm := range []*Manager{m, e.manager()} {
			if got := mm.StakedAmount(); got != want {
				t.Fatalf("staked %d, want %d", got, want)
			}
		}
		if len(e.snapshot().Dissolutions) != 0 {
			t.Fatal("dissolution kept")
		}
	})
	t.Run("registration's block gone takes its dissolution", func(t *testing.T) {
		e, m := setup(t)
		e.chain.headers[heightB] = synthHeader(chainhash.Hash{8}, heightB)
		e.setHistory(entry(heightA, e.a.ProTxHash), entry(heightD, e.hashD))
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		snap := e.snapshot()
		if len(snap.Registrations) != 1 || len(snap.Dissolutions) != 0 {
			t.Fatalf("%d registrations, %d dissolutions", len(snap.Registrations), len(snap.Dissolutions))
		}
	})
	t.Run("blocks above the new tip are gone", func(t *testing.T) {
		e, m := setup(t)
		delete(e.chain.headers, tipHeight)
		delete(e.chain.headers, heightA)
		e.chain.tip = 795
		e.chain.headers[795] = synthHeader(chainhash.Hash{}, 795)
		e.setHistory(entry(heightB, e.b.ProTxHash), entry(heightD, e.hashD))
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if m.StakedAmount() != 0 {
			t.Fatal("kept A above the tip")
		}
		if c := e.snapshot().Cursor; c.Height != 795 {
			t.Fatalf("cursor %+v", c)
		}
	})
}

// A spend in the mempool is not mined, so it does not dissolve.
func TestMempoolSpendDoesNotDissolve(t *testing.T) {
	for _, h := range []int32{0, -1} {
		e := newEnv(t)
		e.own(map[uint32][20]byte{1: e.idB}, 0)
		e.setHistory(entry(heightB, e.b.ProTxHash), entry(h, e.hashD))
		m := e.manager()
		if err := m.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := m.SharedMN(t.Context(), e.b.ProTxHash, false); err != nil {
			t.Fatalf("height %d: %v", h, err)
		}
		if m.StakedAmount() != e.b.Shares[0].Amount {
			t.Fatal("staked")
		}
	}
}

// A transaction that the server serves under another id is refused, even
// where its proof is of the id.
func TestDiscoverRejectsWrongTransactionForSpend(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idB}, 0)
	e.chain.raws[e.hashD.String()] = e.rawHex(e.txA)
	m := e.manager()
	if err := m.Discover(t.Context()); err == nil {
		t.Fatal("accepted a transaction served under another id")
	}
}

// A transaction of the wallet's below the chain's headers cannot be proven. It
// is skipped and said so, and the rest of the scan is kept.
func TestDiscoverSkipsUnprovableBelowStart(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA, 2: e.idB}, 0)
	e.chain.start = 790
	delete(e.chain.headers, heightB)
	delete(e.chain.headers, heightD)
	m := e.manager()
	err := m.Discover(t.Context())
	if !errors.Is(err, ErrUnprovable) {
		t.Fatalf("%v", err)
	}
	// A is proven and kept; B, below the start, is not.
	if m.StakedAmount() != e.a.Shares[0].Amount {
		t.Fatalf("staked %d", m.StakedAmount())
	}
	if _, err := m.SharedMN(t.Context(), e.b.ProTxHash, false); !errors.Is(err, ErrUnknownMN) {
		t.Fatalf("B: %v", err)
	}
}

// A spend below the chain's headers of a registration the wallet already holds
// is skipped too.
func TestDiscoverSkipsUnprovableSpend(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idB}, 0)
	e.setHistory(entry(heightB, e.b.ProTxHash))
	m := e.manager()
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.addSynth(e.txD, heightD)
	e.chain.start = 795
	delete(e.chain.headers, heightB)
	delete(e.chain.headers, heightD)
	e.chain.headers[tipHeight] = synthHeader(chainhash.Hash{7}, tipHeight)
	if err := m.Refresh(t.Context()); !errors.Is(err, ErrUnprovable) {
		t.Fatalf("%v", err)
	}
	if _, err := m.SharedMN(t.Context(), e.b.ProTxHash, false); err != nil {
		t.Fatalf("B: %v", err)
	}
	if len(e.snapshot().Dissolutions) != 0 {
		t.Fatal("saved a spend it could not prove")
	}
}

// What was proven below the chain's headers stays: no reorg reaches it.
func TestRefreshKeepsProofBelowStart(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA}, 0)
	m := e.manager()
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.chain.start = heightA + 1
	delete(e.chain.headers, heightA)
	e.setHistory()
	e.chain.headers[tipHeight] = synthHeader(chainhash.Hash{7}, tipHeight)
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() != e.a.Shares[0].Amount {
		t.Fatal("dropped a registration below the start")
	}
}

func TestEntriesAboveTipAreLeft(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA}, 0)
	e.chain.history[0].Height = tipHeight + 1
	m := e.manager()
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.StakedAmount() != 0 {
		t.Fatal("found a registration above the tip")
	}
}

func TestSharedMN(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA}, 0)
	m := e.manager()
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	t.Run("copy", func(t *testing.T) {
		c, err := m.SharedMN(ctx, e.a.ProTxHash, false)
		if err != nil {
			t.Fatal(err)
		}
		c.Shares[0].RewardScript[0] ^= 0xff
		c.Shares[0].RefundScript[0] ^= 0xff
		c.Shares[0].Amount = 1
		c2, err := m.SharedMN(ctx, e.a.ProTxHash, false)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(c2.Shares[0].RewardScript, e.a.Shares[0].RewardScript) ||
			!bytes.Equal(c2.Shares[0].RefundScript, e.a.Shares[0].RefundScript) || c2.Shares[0].Amount != e.a.Shares[0].Amount {
			t.Fatal("the stored masternode was changed through a copy")
		}
	})

	t.Run("fresh takes the info on a copy", func(t *testing.T) {
		e.chain.info = loadInfo(t, "protx_info_shared.json")
		other := hex.EncodeToString(append(append([]byte{0x76, 0xa9, 0x14}, bytes.Repeat([]byte{7}, 20)...), 0x88, 0xac))
		e.chain.info.State.Shares[0].RewardScript = other
		c, err := m.SharedMN(ctx, e.a.ProTxHash, true)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(c.Shares[0].RewardScript) != other {
			t.Fatal("info not applied")
		}
		c2, err := m.SharedMN(ctx, e.a.ProTxHash, false)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(c2.Shares[0].RewardScript) == other {
			t.Fatal("server data reached the stored masternode")
		}
	})

	t.Run("stale", func(t *testing.T) {
		e.chain.info = loadInfo(t, "protx_info_shared.json")
		e.chain.tip = tipHeight + 1
		defer func() { e.chain.tip = tipHeight }()
		if _, err := m.SharedMN(ctx, e.a.ProTxHash, true); !errors.Is(err, ErrStale) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("not found is not dissolved", func(t *testing.T) {
		e.chain.infoErr = electrum.ErrProTxNotFound
		defer func() { e.chain.infoErr = nil }()
		_, err := m.SharedMN(ctx, e.a.ProTxHash, true)
		if !errors.Is(err, electrum.ErrProTxNotFound) || errors.Is(err, ErrDissolved) {
			t.Fatalf("%v", err)
		}
		if _, err := m.SharedMN(ctx, e.a.ProTxHash, false); err != nil {
			t.Fatalf("not found dissolved it: %v", err)
		}
	})

	t.Run("mismatch refreshes once", func(t *testing.T) {
		e.chain.info = loadInfo(t, "protx_info_shared.json")
		e.chain.info.State.EarlyPenalty++
		e.chain.infoCalls, e.chain.histCalls = 0, 0
		_, err := m.SharedMN(ctx, e.a.ProTxHash, true)
		if !errors.Is(err, ErrInfoMismatch) {
			t.Fatalf("%v", err)
		}
		if e.chain.infoCalls != 2 || e.chain.histCalls != 1 {
			t.Fatalf("%d info calls, %d history calls", e.chain.infoCalls, e.chain.histCalls)
		}
	})

	t.Run("mismatch after a reorg resolves", func(t *testing.T) {
		e.chain.info = loadInfo(t, "protx_info_shared.json")
		e.chain.info.State.RegisteredHeight = 800
		e.chain.info.Confirmations = tipHeight - 800 + 1
		// The registration moves to 800, and the cursor's block changes.
		id := e.a.ProTxHash.String()
		e.chain.headers[heightA] = synthHeader(chainhash.Hash{8}, heightA)
		e.chain.headers[800] = synthHeader(e.a.ProTxHash, 800)
		e.chain.proofs[id] = &electrum.MerkleProof{BlockHeight: 800}
		e.setHistory(entry(800, e.a.ProTxHash))
		e.chain.headers[tipHeight] = synthHeader(chainhash.Hash{3}, tipHeight)
		c, err := m.SharedMN(ctx, e.a.ProTxHash, true)
		if err != nil {
			t.Fatal(err)
		}
		if c.RegisteredHeight != 800 {
			t.Fatalf("height %d", c.RegisteredHeight)
		}
	})
}

func TestManagerConcurrent(t *testing.T) {
	e := newEnv(t)
	e.own(map[uint32][20]byte{1: e.idA, 2: e.idB}, 0)
	m := e.manager()
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := m.Refresh(t.Context()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = m.StakedAmount()
				if _, err := m.SharedMN(t.Context(), e.a.ProTxHash, false); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
