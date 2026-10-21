// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package wallet

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/chain"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// A DIP-2 special transaction: version 3, type 5, a 10-byte extra payload.
const (
	dashSpecialTxHex     = "03000500010000000000000000000000000000000000000000000000000000000000000000ffffffff0403abcdefffffffff010065cd1d000000001976a914111111111111111111111111111111111111111188ac000000000a0102030405060708090a"
	dashSpecialTxID      = "fbf8d764239596655ac584f166bd889ef532dbd850778345a30e814d0f11980c"
	dashSpecialExtraData = "0102030405060708090a"
)

// publishRecorder is a chain client that records what it is asked to broadcast
// and can reject it.
type publishRecorder struct {
	*mockChainClient
	sent []*bisonwire.Tx
	err  error
}

func (p *publishRecorder) SendRawTransaction(tx *bisonwire.Tx, _ bool) (*chainhash.Hash, error) {
	p.sent = append(p.sent, tx)
	if p.err != nil {
		return nil, p.err
	}
	h := tx.TxHash()
	return &h, nil
}

var _ chain.Backend = (*publishRecorder)(nil)

func testDashWallet(t *testing.T, client chain.Backend) *Wallet {
	t.Helper()

	dir, err := os.MkdirTemp("", "test_wallet_dash")
	if err != nil {
		t.Fatalf("Failed to create db dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	seed, err := hdkeychain.GenerateSeed(hdkeychain.MinSeedBytes)
	if err != nil {
		t.Fatalf("unable to create seed: %v", err)
	}
	loader := NewLoader(
		assets.DashParams["mainnet"], dir, true, defaultDBTimeout, 250,
		WithWalletSyncRetryInterval(10*time.Millisecond),
		WithScryptOptions(&waddrmgr.FastScryptOptions),
	)
	w, err := loader.CreateNewWallet([]byte("hello"), []byte("world"), seed, time.Now())
	if err != nil {
		t.Fatalf("unable to create wallet: %v", err)
	}
	w.chainClient = client
	return w
}

func decodeDashSpecialTx(t *testing.T) *bisonwire.Tx {
	t.Helper()
	raw, err := hex.DecodeString(dashSpecialTxHex)
	if err != nil {
		t.Fatal(err)
	}
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	return tx
}

// storedTx returns the unmined transaction with the given hash, or nil.
func storedTx(t *testing.T, w *Wallet, hash chainhash.Hash) *bisonwire.Tx {
	t.Helper()
	var found *bisonwire.Tx
	err := walletdb.View(w.db, func(dbTx walletdb.ReadTx) error {
		ns := dbTx.ReadBucket(wtxmgrNamespaceKey)
		details, err := w.TxStore.UniqueTxDetails(ns, &hash, nil)
		if err != nil {
			return err
		}
		if details != nil {
			found = &details.Tx
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestPublishBisonTxKeepsDashSpecialFields guards the publish path: the
// transaction handed to the backend and the one written to the store must both
// carry the type and payload, since the store rebroadcasts what it holds.
func TestPublishBisonTxKeepsDashSpecialFields(t *testing.T) {
	rec := &publishRecorder{mockChainClient: &mockChainClient{}}
	w := testDashWallet(t, rec)
	tx := decodeDashSpecialTx(t)

	if err := w.PublishBisonTx(tx, ""); err != nil {
		t.Fatalf("PublishBisonTx: %v", err)
	}

	if len(rec.sent) != 1 {
		t.Fatalf("backend got %d txs, want 1", len(rec.sent))
	}
	if got := rec.sent[0]; got.Type != 5 || hex.EncodeToString(got.ExtraPayload) != dashSpecialExtraData {
		t.Errorf("backend tx type=%d payload=%x, want type 5 and the payload", got.Type, got.ExtraPayload)
	}

	stored := storedTx(t, w, tx.TxHash())
	if stored == nil {
		t.Fatalf("special tx %s not in the store", dashSpecialTxID)
	}
	if stored.Type != 5 || hex.EncodeToString(stored.ExtraPayload) != dashSpecialExtraData {
		t.Errorf("stored tx type=%d payload=%x, want type 5 and the payload", stored.Type, stored.ExtraPayload)
	}
}

// TestPublishBisonTxRemovesRejectedDashSpecialTx guards cleanup: a rejected
// special tx must be removed from the unconfirmed store. Removing by a hash
// computed without the payload misses it, and the wallet would rebroadcast and
// count its outputs forever.
func TestPublishBisonTxRemovesRejectedDashSpecialTx(t *testing.T) {
	rec := &publishRecorder{
		mockChainClient: &mockChainClient{},
		err:             errors.New("bad-txns-example"),
	}
	w := testDashWallet(t, rec)
	tx := decodeDashSpecialTx(t)

	if err := w.PublishBisonTx(tx, ""); err == nil {
		t.Fatal("expected the backend rejection to be returned")
	}
	if storedTx(t, w, tx.TxHash()) != nil {
		t.Fatal("rejected special tx is still in the unconfirmed store")
	}
}

// TestResendUnminedTxsKeepsDashSpecialFields guards rebroadcast after a
// restart: the wallet resends what its store holds, so a special tx must go
// out with its type and payload. Resending it bare gets it rejected, and a
// rejected resend removes it from the store.
func TestResendUnminedTxsKeepsDashSpecialFields(t *testing.T) {
	rec := &publishRecorder{mockChainClient: &mockChainClient{}}
	w := testDashWallet(t, rec)
	tx := decodeDashSpecialTx(t)

	if err := w.PublishBisonTx(tx, ""); err != nil {
		t.Fatalf("PublishBisonTx: %v", err)
	}
	rec.sent = nil

	w.resendUnminedTxs()

	if len(rec.sent) != 1 {
		t.Fatalf("resend sent %d txs, want 1", len(rec.sent))
	}
	if got := rec.sent[0]; got.Type != 5 || hex.EncodeToString(got.ExtraPayload) != dashSpecialExtraData {
		t.Errorf("resent tx type=%d payload=%x, want type 5 and the payload", got.Type, got.ExtraPayload)
	}
	if storedTx(t, w, tx.TxHash()) == nil {
		t.Error("special tx was dropped from the store by the resend")
	}
}
