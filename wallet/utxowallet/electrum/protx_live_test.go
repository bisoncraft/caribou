// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

//go:build harness

// A live check against the Dash regtest harness and its ElectrumX, which
// needs V24 active and at least one shared masternode registered (the
// masternode package's harness tests register them):
//
//	util/testing/dash/harness.sh
//	util/testing/btc/electrumx.sh   (for Dash)
//
//	go test -tags harness -run TestLiveDashProTx ./wallet/utxowallet/electrum/

package electrum

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
)

// sharedCollateralScripthash is the electrum scripthash of the script every
// shared masternode's collateral pays to: a push of "DSHC", OP_DROP, OP_TRUE.
func sharedCollateralScripthash() string {
	sum := sha256.Sum256([]byte{0x04, 'D', 'S', 'H', 'C', 0x75, 0x51})
	for i, j := 0, len(sum)-1; i < j; i, j = i+1, j-1 {
		sum[i], sum[j] = sum[j], sum[i]
	}
	return hex.EncodeToString(sum[:])
}

// TestLiveDashProTx finds the shared masternodes through the one scripthash they
// all pay to, which needs no protx method, then checks what the wallet needs of
// each: the raw registration with its txid verified, its inclusion in a block
// whose header the server gave, and the server's state for it.
func TestLiveDashProTx(t *testing.T) {
	node, err := NewNode(t.Context(), &ServerAddr{Proto: "ssl", Addr: "127.0.0.1:55021", SkipVerify: true}, "", "", bisonwire.ChainDASH)
	if err != nil {
		t.Skipf("the Dash harness electrumx is not reachable: %v", err)
	}
	defer node.Close()

	hist, err := node.GetHistory(t.Context(), sharedCollateralScripthash())
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 {
		t.Skip("no shared masternode is registered on the harness")
	}

	var found, notFound int
	for _, h := range hist {
		if h.Height <= 0 {
			continue
		}
		// Fails before the fix: the registration has a payload.
		rawHex, err := node.GetRawTransaction(t.Context(), h.TxHash)
		if err != nil {
			t.Fatalf("%s: %v", h.TxHash, err)
		}
		raw, _ := hex.DecodeString(rawHex)
		tx := &bisonwire.Tx{Chain: "dash"}
		if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}

		// The proof folds to the header's root.
		proof, err := node.GetMerkle(t.Context(), h.TxHash, h.Height)
		if err != nil {
			t.Fatal(err)
		}
		hdrHex, err := node.BlockHeader(t.Context(), h.Height)
		if err != nil {
			t.Fatal(err)
		}
		hdrRaw, _ := hex.DecodeString(hdrHex)
		var hdr wire.BlockHeader
		if err := hdr.Deserialize(bytes.NewReader(hdrRaw)); err != nil {
			t.Fatal(err)
		}
		txid, _ := chainhash.NewHashFromStr(h.TxHash)
		root, err := MerkleRoot(*txid, proof)
		if err != nil || root != hdr.MerkleRoot {
			t.Fatalf("%s: merkle root %s, header %s: %v", h.TxHash, root, hdr.MerkleRoot, err)
		}

		if tx.Type != 1 {
			continue // a dissolution
		}
		info, err := node.ProTxInfo(t.Context(), *txid)
		switch {
		case errors.Is(err, ErrProTxNotFound):
			notFound++ // dissolved
		case err != nil:
			t.Fatalf("protx.info %s: %v", h.TxHash, err)
		default:
			found++
			if info.State.RegisteredHeight != h.Height || len(info.State.Shares) < 2 || info.CollateralHash != h.TxHash {
				t.Fatalf("%s: server reports %+v", h.TxHash, info)
			}
		}
	}
	t.Logf("%d history entries: %d registered masternodes, %d dissolved", len(hist), found, notFound)
	if found == 0 {
		t.Fatal("no registered masternode is known to the server")
	}
}
