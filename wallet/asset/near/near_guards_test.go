// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// TestBalanceNotConnected mirrors xrp's TestBalanceNotConnected and
// hyperliquid's TestBalanceBeforeConnect: Balance must return an error
// rather than panic when called before Connect has set w.rpc.
func TestBalanceNotConnected(t *testing.T) {
	w := &NearWallet{}
	if _, err := w.Balance(); err == nil {
		t.Fatal("expected error when rpc is nil")
	}
}

// TestSendNotConnected verifies Send returns an error rather than panicking
// when called before Connect.
func TestSendNotConnected(t *testing.T) {
	w := &NearWallet{}
	if _, err := w.Send("alice.near", 1, 0); err == nil {
		t.Fatal("expected error when rpc is nil")
	}
}

// TestWalletTransactionNotConnected verifies WalletTransaction returns an
// error rather than panicking when the local DB misses and rpc is nil.
func TestWalletTransactionNotConnected(t *testing.T) {
	w := &NearWallet{}
	if _, err := w.WalletTransaction(context.Background(), "sometx"); err == nil {
		t.Fatal("expected error when rpc is nil")
	}
}

// TestOwnsDepositAddressCaseSensitive guards against EqualFold accepting an
// uppercase variant of an account ID that ValidateAddress itself rejects.
func TestOwnsDepositAddressCaseSensitive(t *testing.T) {
	w := &NearWallet{accountID: "98793cd91a3f870fb126f66285808c7e094afcfc4eda8a970f6648cdf0dbd6de"}
	owns, err := w.OwnsDepositAddress("98793CD91A3F870FB126F66285808C7E094AFCFC4EDA8A970F6648CDF0DBD6DE")
	if err != nil {
		t.Fatalf("OwnsDepositAddress error: %v", err)
	}
	if owns {
		t.Fatal("OwnsDepositAddress accepted an uppercase variant of the account ID")
	}
	owns, err = w.OwnsDepositAddress(w.accountID)
	if err != nil {
		t.Fatalf("OwnsDepositAddress error: %v", err)
	}
	if !owns {
		t.Fatal("OwnsDepositAddress rejected the wallet's own account ID")
	}
}

// TestScanIncomingTxsNetDecrease guards against the watermark bug: a net
// balance decrease over the poll window (an outgoing send that outweighs an
// incoming receive landing in the same window) must not skip block scanning
// -- the incoming transfer must still be found and stored, and the
// watermark must still advance across the range that was actually scanned.
func TestScanIncomingTxsNetDecrease(t *testing.T) {
	const (
		accountID  = "sender.near"
		senderID   = "other.near"
		lastHeight = uint64(100)
		newTip     = uint64(101)
	)
	m := newMockNearRPC(t)
	m.handle("query", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		blockID, _ := p["block_id"].(float64)
		amount := "2000000000000000000000000" // 2 NEAR
		if uint64(blockID) == newTip {
			amount = "1000000000000000000000000" // 1 NEAR: net decrease despite a receive
		}
		return map[string]any{"amount": amount, "locked": "0"}, nil
	})
	m.handle("block", func(params json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"header": map[string]any{"height": newTip, "hash": "11111111111111111111111111111111111111111", "timestamp": uint64(0)},
			"chunks": []map[string]any{{"chunk_hash": "chunkhash1", "shard_id": 0}},
		}, nil
	})
	m.handle("chunk", func(params json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"transactions": []map[string]any{{
				"hash":        "incomingtxhash",
				"signer_id":   senderID,
				"receiver_id": accountID,
				"actions":     []map[string]any{{"Transfer": map[string]any{"deposit": "500000000000000000000000"}}},
			}},
		}, nil
	})

	log := util.NewLogger("T", util.LevelInfo, os.Stdout)
	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	w := &NearWallet{
		accountID:         accountID,
		rpc:               m.client(),
		txDB:              master.NetworkTxDB(BipID),
		lastScannedHeight: lastHeight,
		log:               log,
	}

	w.scanIncomingTxs(context.Background(), newTip)

	if w.lastScannedHeight != newTip {
		t.Fatalf("lastScannedHeight = %d, want %d", w.lastScannedHeight, newTip)
	}
	got, err := w.txDB.GetTx("incomingtxhash")
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if got == nil {
		t.Fatal("incoming transfer was not recorded despite a net balance decrease over the window")
	}
	if got.Type != txdb.Receive {
		t.Fatalf("recorded tx type = %v, want Receive", got.Type)
	}
}
