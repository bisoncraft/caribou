// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

//go:build harness

// An end to end check of the Manager against the Dash regtest harness and its
// ElectrumX: a masternode registered with owner keys derived from a random
// per run master (Core refuses a repeated owner key, so a fixed one would fail
// on the second run) is found by Discover among the others on the harness, its
// info is applied, and its dissolution is seen by Refresh.
//
// Prerequisites are those of TestDissolutionAcceptedByDashCore, plus the
// harness ElectrumX on 127.0.0.1:55021.
//
//	go test -tags harness -run TestLiveManager ./wallet/asset/dash/masternode/

package masternode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
)

// rootDeriver derives from a master key along an explicit path.
type rootDeriver struct{ master *hdkeychain.ExtendedKey }

func (d rootDeriver) DeriveFromRootPath(path []uint32) (*btcec.PrivateKey, error) {
	k := d.master
	for _, p := range path {
		var err error
		if k, err = k.Derive(p); err != nil {
			return nil, err
		}
	}
	return k.ECPrivKey()
}

// nodeChain is a Chain over one server connection. It takes its headers from
// the server, which the wallet's Network checks and this test does not.
type nodeChain struct {
	t    *testing.T
	node *electrum.Node
}

func (c nodeChain) Tip() int32 {
	tip, err := c.node.SubscribeHeaders(c.t.Context())
	if err != nil {
		c.t.Fatal(err)
	}
	return tip.Height
}

func (c nodeChain) StartHeight() int32 { return 0 }

func (c nodeChain) HeaderByHeight(h int32) *wire.BlockHeader {
	s, err := c.node.BlockHeader(c.t.Context(), h)
	if err != nil {
		return nil
	}
	raw, _ := hex.DecodeString(s)
	var hdr wire.BlockHeader
	if hdr.Deserialize(bytes.NewReader(raw)) != nil {
		return nil
	}
	return &hdr
}

func (c nodeChain) GetHistory(ctx context.Context, sh string) ([]electrum.HistoryEntry, error) {
	return c.node.GetHistory(ctx, sh)
}

func (c nodeChain) GetRawTransaction(ctx context.Context, id string) (string, error) {
	return c.node.GetRawTransaction(ctx, id)
}

func (c nodeChain) GetMerkle(ctx context.Context, id string, h int32) (*electrum.MerkleProof, error) {
	return c.node.GetMerkle(ctx, id, h)
}

func (c nodeChain) ProTxInfo(ctx context.Context, h chainhash.Hash) (*electrum.ProTxInfo, error) {
	return c.node.ProTxInfo(ctx, h)
}

func TestLiveManager(t *testing.T) {
	node, err := electrum.NewNode(t.Context(), &electrum.ServerAddr{Proto: "ssl", Addr: "127.0.0.1:55021", SkipVerify: true}, "", "", bisonwire.ChainDASH)
	if err != nil {
		t.Skipf("the Dash harness electrumx is not reachable: %v", err)
	}
	defer node.Close()
	chain := nodeChain{t: t, node: node}

	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	master, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	keys := rootDeriver{master}

	// Reserve four owner indexes as a wallet registering would, and register
	// with the keys at 0 and 3: the others are the gap discovery must cross.
	regStore, regDB, _ := newTestStore(t)
	defer regDB.Close()
	for i := 0; i < 4; i++ {
		if _, _, err := NewShareOwner(regStore, keys, 1); err != nil {
			t.Fatal(err)
		}
	}
	k0, err := OwnerKey(keys, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	k3, err := OwnerKey(keys, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	mn, ownerKeys := registerHarnessMN(t, k0, k3)

	// Wait for the server to have the registration.
	waitFor := func(what string, f func() bool) {
		t.Helper()
		for i := 0; i < 50; i++ {
			if f() {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	listed := func(id chainhash.Hash) func() bool {
		return func() bool {
			h, err := node.GetHistory(t.Context(), sharedCollateralScripthash)
			if err != nil {
				return false
			}
			for _, e := range h {
				if e.TxHash == id.String() && e.Height > 0 && chain.Tip() >= e.Height {
					return true
				}
			}
			return false
		}
	}
	waitFor("the registration", listed(mn.ProTxHash))

	// A wallet restored from the seed has no cache and no counter.
	st, db, _ := newTestStore(t)
	defer db.Close()
	m, err := NewManager(chain, st, keys, 1, regtestNet)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Registrations) != 1 || snap.NextOwner < 4 {
		t.Fatalf("found %d registrations, next owner %d", len(snap.Registrations), snap.NextOwner)
	}
	if _, ok := snap.Registrations[mn.ProTxHash]; !ok {
		t.Fatal("did not find the masternode")
	}
	if got := m.StakedAmount(); got != 1000e8 {
		t.Fatalf("staked %d", got)
	}

	var fresh *SharedMN
	for i := 0; i < 20; i++ {
		fresh, err = m.SharedMN(t.Context(), mn.ProTxHash, true)
		if !errors.Is(err, ErrStale) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if fresh.KeyIDVoting != mn.KeyIDVoting || fresh.PubKeyOperator != mn.PubKeyOperator {
		t.Fatal("the server's keys are not the registered ones")
	}

	// A unanimous dissolution of the masternode, mined.
	const fee = 100_000
	d, err := NewDissolution(mn, DissolveParams{Actor: 0, Unanimous: true, Fee: fee})
	if err != nil {
		t.Fatal(err)
	}
	var sigs []ShareSig
	for i := range ownerKeys {
		s, err := d.Sign(ownerKeys[i], i)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	if err := d.AddSigs(sigs); err != nil {
		t.Fatal(err)
	}
	raw, err := d.Raw()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rpc(t, "sendrawtransaction", hex.EncodeToString(raw)); err != nil {
		t.Fatal(err)
	}
	mine(t, 1)
	waitFor("the dissolution", func() bool {
		if err := m.Refresh(t.Context()); err != nil {
			return false
		}
		_, err := m.SharedMN(t.Context(), mn.ProTxHash, false)
		return errors.Is(err, ErrDissolved)
	})
	if got := m.StakedAmount(); got != 0 {
		t.Fatalf("staked %d after the dissolution", got)
	}
}
