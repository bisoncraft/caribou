// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

//go:build harness

// A live check against the Dash regtest harness and its ElectrumX (see
// wallet/utxowallet/electrum/protx_live_test.go for the setup). It reads every
// shared masternode through the one collateral scripthash, as the wallet's
// discovery will, and checks that this package accepts what Dash Core made.
//
//	go test -tags harness -run TestLiveChainState ./wallet/asset/dash/masternode/

package masternode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
)

func TestLiveChainState(t *testing.T) {
	node, err := electrum.NewNode(t.Context(), &electrum.ServerAddr{Proto: "ssl", Addr: "127.0.0.1:55021", SkipVerify: true}, "", "", bisonwire.ChainDASH)
	if err != nil {
		t.Skipf("the Dash harness electrumx is not reachable: %v", err)
	}
	defer node.Close()

	sum := sha256.Sum256(SharedCollateralScript)
	for i, j := 0, len(sum)-1; i < j; i, j = i+1, j-1 {
		sum[i], sum[j] = sum[j], sum[i]
	}
	hist, err := node.GetHistory(t.Context(), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 {
		t.Skip("no shared masternode is registered on the harness")
	}

	var mns []*SharedMN
	var spends []*bisonwire.Tx
	for _, h := range hist {
		if h.Height <= 0 {
			continue
		}
		rawHex, err := node.GetRawTransaction(t.Context(), h.TxHash)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := hex.DecodeString(rawHex)
		tx := &bisonwire.Tx{Chain: "dash"}
		if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		if tx.Type != txTypeProReg {
			spends = append(spends, tx)
			continue
		}
		m, err := RegistrationMN(tx, h.Height)
		if err != nil {
			t.Fatalf("%s: %v", h.TxHash, err)
		}
		mns = append(mns, m)
	}

	var active, banned, dissolved int
	for _, m := range mns {
		gone := false
		for _, tx := range spends {
			gone = gone || m.SpendsCollateral(tx)
		}
		hash, _ := chainhash.NewHashFromStr(m.ProTxHash.String())
		info, err := node.ProTxInfo(t.Context(), *hash)
		if errors.Is(err, electrum.ErrProTxNotFound) {
			info, err = nil, nil
			if !gone {
				t.Fatalf("%s: the server has no such masternode but its collateral is unspent", m.ProTxHash)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		if info != nil {
			if err := m.ApplyInfo(info, regtestNet); err != nil {
				t.Fatalf("%s: %v", m.ProTxHash, err)
			}
		}
		switch DeriveState(info, gone) {
		case StateActive:
			active++
		case StateBanned:
			banned++
		case StateDissolved:
			dissolved++
		default:
			t.Fatalf("%s: unknown state", m.ProTxHash)
		}
	}
	t.Logf("%d registrations: %d active, %d banned, %d dissolved", len(mns), active, banned, dissolved)
	if dissolved == 0 || active+banned == 0 {
		t.Fatal("expected both live and dissolved masternodes on the harness")
	}
}
