// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

func newSendTestWallet(t *testing.T, m *mockNearRPC) *NearWallet {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x09}, 64)))
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return &NearWallet{
		ctx:       context.Background(),
		pubKey:    pub,
		privKey:   priv,
		accountID: "sender.near",
		rpc:       m.client(),
		log:       util.Disabled,
	}
}

// TestSendSerializesReserveAndBroadcast guards against the reordering bug:
// two concurrent sends must not interleave their reserveNonce/broadcast
// calls, since NEAR permanently invalidates a lower nonce that lands after
// a higher one and has no queue to reorder them like xrp does. The delay
// sits in the "reserve" leg (the query handler): without sendMtx, both
// goroutines' reserveNonce calls run concurrently and both entries land as
// "reserve" before either "broadcast" is recorded, failing deterministically.
// With sendMtx, the second goroutine can't start reserveNonce until the
// first has finished its whole reserve-sign-broadcast section.
func TestSendSerializesReserveAndBroadcast(t *testing.T) {
	m := newMockNearRPC(t)
	var mu sync.Mutex
	var order []string

	m.handle("query", func(_ json.RawMessage) (any, *rpcError) {
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		order = append(order, "reserve")
		mu.Unlock()
		return map[string]any{"nonce": uint64(5)}, nil
	})
	m.handle("broadcast_tx_async", func(_ json.RawMessage) (any, *rpcError) {
		mu.Lock()
		order = append(order, "broadcast")
		mu.Unlock()
		return "somehash", nil
	})

	w := newSendTestWallet(t, m)

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			if _, err := w.Send("receiver.near", 1, 0); err != nil {
				t.Errorf("Send error: %v", err)
			}
		}()
	}
	wg.Wait()

	if len(order) != 4 {
		t.Fatalf("order = %v, want 4 entries", order)
	}
	for i := 0; i < len(order); i += 2 {
		if order[i] != "reserve" || order[i+1] != "broadcast" {
			t.Fatalf("reserve/broadcast interleaved across concurrent sends: %v", order)
		}
	}
}

// TestSendDeterministicRejectionMarksTerminal guards against a pre-stored
// send tx staying Pending forever when the node deterministically rejects
// the broadcast (as opposed to a transport error, whose outcome is
// genuinely unknown).
func TestSendDeterministicRejectionMarksTerminal(t *testing.T) {
	m := newMockNearRPC(t)
	m.handle("query", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{"nonce": uint64(5)}, nil
	})
	m.handle("broadcast_tx_async", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{
			Code:    -32000,
			Message: "Server error",
			Cause:   &rpcErrorCause{Name: "INVALID_TRANSACTION"},
		}
	})

	log := util.NewLogger("T", util.LevelInfo, os.Stdout)
	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	w := newSendTestWallet(t, m)
	w.log = log
	w.txDB = master.NetworkTxDB(BipID)

	_, err = w.Send("receiver.near", 1, 0)
	if err == nil {
		t.Fatal("expected error from a deterministically rejected broadcast")
	}

	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("rejected tx was left pending: %+v", pending)
	}
}

// TestSendNodeLevelTransientErrorLeavesTxPending guards against
// broadcastSend misclassifying a non-deterministic node-level error (e.g.
// TIMEOUT_ERROR or INTERNAL_ERROR) as an outright rejection: only
// INVALID_TRANSACTION means the node parsed and rejected the tx outright.
// Anything else must leave the pre-stored tx pending for checkPendingTxs to
// resolve, since the node may have already accepted it.
func TestSendNodeLevelTransientErrorLeavesTxPending(t *testing.T) {
	m := newMockNearRPC(t)
	m.handle("query", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{"nonce": uint64(5)}, nil
	})
	m.handle("broadcast_tx_async", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{
			Code:    -32000,
			Message: "Server error",
			Cause:   &rpcErrorCause{Name: "TIMEOUT_ERROR"},
		}
	})

	log := util.NewLogger("T", util.LevelInfo, os.Stdout)
	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	w := newSendTestWallet(t, m)
	w.log = log
	w.txDB = master.NetworkTxDB(BipID)

	_, err = w.Send("receiver.near", 1, 0)
	if err == nil {
		t.Fatal("expected error from a failed broadcast")
	}

	pending, err := w.txDB.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1 (a node-level error whose outcome is unknown must stay pending)", len(pending))
	}
}

// TestCheckPendingTxsExpiry guards against a pre-stored tx that never landed
// staying Pending forever: once its reference block is older than the
// network's transaction_validity_period, it can never be included and must
// be resolved as terminal.
func TestCheckPendingTxsExpiry(t *testing.T) {
	m := newMockNearRPC(t)
	m.handle("tx", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{
			Code:    -32000,
			Message: "Server error",
			Cause:   &rpcErrorCause{Name: "UNKNOWN_TRANSACTION"},
		}
	})

	log := util.NewLogger("T", util.LevelInfo, os.Stdout)
	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	w := newSendTestWallet(t, m)
	w.log = log
	w.txDB = master.NetworkTxDB(BipID)
	w.tip = 1_000_000

	recipient := "receiver.near"
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        "expiredtxhash",
		Amount:    1,
		Recipient: &recipient,
	}
	setSentAtHeight(wtx, w.tip-TxValidityBlocks-1) // older than the validity window
	if err := w.txDB.StoreTx(wtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	w.checkPendingTxs(context.Background())

	got, err := w.txDB.GetTx("expiredtxhash")
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if got == nil {
		t.Fatal("expired tx not found")
	}
	if !got.Confirmed || !got.Rejected {
		t.Fatalf("expired tx not marked terminal: Confirmed=%v Rejected=%v", got.Confirmed, got.Rejected)
	}
}

// TestCheckPendingTxsNotYetExpired guards the flip side: a pending tx whose
// reference block is still within the validity window must stay Pending,
// not be prematurely marked Rejected.
func TestCheckPendingTxsNotYetExpired(t *testing.T) {
	m := newMockNearRPC(t)
	m.handle("tx", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{
			Code:    -32000,
			Message: "Server error",
			Cause:   &rpcErrorCause{Name: "UNKNOWN_TRANSACTION"},
		}
	})

	log := util.NewLogger("T", util.LevelInfo, os.Stdout)
	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	w := newSendTestWallet(t, m)
	w.log = log
	w.txDB = master.NetworkTxDB(BipID)
	w.tip = 1_000_000

	recipient := "receiver.near"
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        "freshtxhash",
		Amount:    1,
		Recipient: &recipient,
	}
	setSentAtHeight(wtx, w.tip-10) // well within the validity window
	if err := w.txDB.StoreTx(wtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	w.checkPendingTxs(context.Background())

	got, err := w.txDB.GetTx("freshtxhash")
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if got.Confirmed || got.Rejected {
		t.Fatalf("tx within validity window marked terminal prematurely: Confirmed=%v Rejected=%v", got.Confirmed, got.Rejected)
	}
}

// TestCheckPendingTxsExpiresOnAnyErrorClass guards against gating expiry on
// a specific txStatus error class (e.g. only UNKNOWN_TRANSACTION): under
// wait_until:EXECUTED_OPTIMISTIC an unknown tx can surface as a
// TIMEOUT_ERROR instead, so the height-based expiry check must fire
// regardless of which error txStatus returns, not just the "not found" one.
func TestCheckPendingTxsExpiresOnAnyErrorClass(t *testing.T) {
	m := newMockNearRPC(t)
	m.handle("tx", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{
			Code:    -32000,
			Message: "Server error",
			Cause:   &rpcErrorCause{Name: "TIMEOUT_ERROR"},
		}
	})

	log := util.NewLogger("T", util.LevelInfo, os.Stdout)
	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	w := newSendTestWallet(t, m)
	w.log = log
	w.txDB = master.NetworkTxDB(BipID)
	w.tip = 1_000_000

	recipient := "receiver.near"
	wtx := &txdb.WalletTransaction{
		Type:      txdb.Send,
		ID:        "expiredtxhash2",
		Amount:    1,
		Recipient: &recipient,
	}
	setSentAtHeight(wtx, w.tip-TxValidityBlocks-1)
	if err := w.txDB.StoreTx(wtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	w.checkPendingTxs(context.Background())

	got, err := w.txDB.GetTx("expiredtxhash2")
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if !got.Confirmed || !got.Rejected {
		t.Fatalf("expired tx not marked terminal on a TIMEOUT_ERROR response: Confirmed=%v Rejected=%v", got.Confirmed, got.Rejected)
	}
}

// TestScanIncomingTxsSkipsUnknownBlock guards against a single skipped
// block height (a producer missing its slot -- routine on NEAR) aborting
// the whole scan window and losing every receive after it.
func TestScanIncomingTxsSkipsUnknownBlock(t *testing.T) {
	const (
		accountID  = "sender.near"
		senderID   = "other.near"
		lastHeight = uint64(100)
		newTip     = uint64(102)
		skipped    = uint64(101)
	)
	m := newMockNearRPC(t)
	m.handle("query", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		blockID, _ := p["block_id"].(float64)
		amount := "1000000000000000000000000"
		if uint64(blockID) == newTip {
			amount = "2000000000000000000000000" // balance increased
		}
		return map[string]any{"amount": amount, "locked": "0"}, nil
	})
	m.handle("block", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		blockID, _ := p["block_id"].(float64)
		if uint64(blockID) == skipped {
			return nil, &rpcError{Code: -32000, Message: "Server error", Cause: &rpcErrorCause{Name: "UNKNOWN_BLOCK"}}
		}
		return map[string]any{
			"header": map[string]any{"height": uint64(blockID), "hash": "11111111111111111111111111111111111111111", "timestamp": uint64(0)},
			"chunks": []map[string]any{{"chunk_hash": "chunkhash1", "shard_id": 0}},
		}, nil
	})
	m.handle("chunk", func(params json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"transactions": []map[string]any{{
				"hash":        "incomingtxhash",
				"signer_id":   senderID,
				"receiver_id": accountID,
				"actions":     []map[string]any{{"Transfer": map[string]any{"deposit": "1000000000000000000000000"}}},
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
		t.Fatalf("lastScannedHeight = %d, want %d (a skipped height must not abort the scan)", w.lastScannedHeight, newTip)
	}
	got, err := w.txDB.GetTx("incomingtxhash")
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if got == nil {
		t.Fatal("incoming transfer at newTip was not recorded; the skipped block aborted the scan")
	}
}

// TestScanIncomingTxsHoldsWatermarkOnTransientError guards against a
// transient block-fetch error advancing the watermark past unscanned
// blocks: the watermark must hold at the last height actually scanned so
// the range is retried, not marked done.
func TestScanIncomingTxsHoldsWatermarkOnTransientError(t *testing.T) {
	const (
		accountID  = "sender.near"
		lastHeight = uint64(100)
		newTip     = uint64(102)
		failing    = uint64(101)
	)
	m := newMockNearRPC(t)
	m.handle("query", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		blockID, _ := p["block_id"].(float64)
		amount := "1000000000000000000000000"
		if uint64(blockID) == newTip {
			amount = "2000000000000000000000000"
		}
		return map[string]any{"amount": amount, "locked": "0"}, nil
	})
	m.handle("block", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		blockID, _ := p["block_id"].(float64)
		if uint64(blockID) == failing {
			return nil, &rpcError{Code: -32000, Message: "internal error"}
		}
		return map[string]any{
			"header": map[string]any{"height": uint64(blockID), "hash": "11111111111111111111111111111111111111111", "timestamp": uint64(0)},
			"chunks": []map[string]any{},
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

	if w.lastScannedHeight != failing-1 {
		t.Fatalf("lastScannedHeight = %d, want %d (watermark must hold, not jump to newTip)", w.lastScannedHeight, failing-1)
	}
}

// TestScanIncomingTxsTransientErrorAfterSkippedBlockHoldsRealWatermark
// guards against parking the watermark on a skipped (UNKNOWN_BLOCK) height
// when a later height then fails transiently: watermark-1 would be that
// nonexistent skipped height, and since every future poll's
// balance-comparison query (viewAccountAt at lastScannedHeight) fails the
// same way for a height with no block, the scan would stall forever. The
// watermark must fall back to the last height a block was actually fetched
// for.
func TestScanIncomingTxsTransientErrorAfterSkippedBlockHoldsRealWatermark(t *testing.T) {
	const (
		accountID  = "sender.near"
		lastHeight = uint64(100)
		newTip     = uint64(103)
		skipped    = uint64(101)
		failing    = uint64(102)
	)
	m := newMockNearRPC(t)
	m.handle("query", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		blockID, _ := p["block_id"].(float64)
		amount := "1000000000000000000000000"
		if uint64(blockID) == newTip {
			amount = "2000000000000000000000000"
		}
		return map[string]any{"amount": amount, "locked": "0"}, nil
	})
	m.handle("block", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		blockID, _ := p["block_id"].(float64)
		switch uint64(blockID) {
		case skipped:
			return nil, &rpcError{Code: -32000, Message: "Server error", Cause: &rpcErrorCause{Name: "UNKNOWN_BLOCK"}}
		case failing:
			return nil, &rpcError{Code: -32000, Message: "internal error"}
		}
		return map[string]any{
			"header": map[string]any{"height": uint64(blockID), "hash": "11111111111111111111111111111111111111111", "timestamp": uint64(0)},
			"chunks": []map[string]any{},
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

	if w.lastScannedHeight != lastHeight {
		t.Fatalf("lastScannedHeight = %d, want %d (must fall back to the last real block fetched, not the skipped height)", w.lastScannedHeight, lastHeight)
	}
}

// TestParseYoctoNEARFailureHoldsWatermark guards against the dead-code bug:
// a malformed account balance must hold the watermark, not silently be
// treated as a zero balance that lets the watermark advance over unscanned
// blocks.
func TestParseYoctoNEARFailureHoldsWatermark(t *testing.T) {
	const (
		accountID  = "sender.near"
		lastHeight = uint64(100)
		newTip     = uint64(101)
	)
	m := newMockNearRPC(t)
	m.handle("query", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		blockID, _ := p["block_id"].(float64)
		amount := "1000000000000000000000000"
		if uint64(blockID) == newTip {
			amount = "not-a-number" // malformed balance at the tip
		}
		return map[string]any{"amount": amount, "locked": "0"}, nil
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

	if w.lastScannedHeight != lastHeight {
		t.Fatalf("lastScannedHeight = %d, want %d (a parse failure must hold the watermark)", w.lastScannedHeight, lastHeight)
	}
}

// TestTokenWalletBalanceNotConnected guards against a nil-rpc panic when a
// token wallet is reachable (via OpenTokenWallet) before the parent
// wallet's Connect has run.
func TestTokenWalletBalanceNotConnected(t *testing.T) {
	tw := &TokenWallet{parent: &NearWallet{}}
	if _, err := tw.Balance(); err == nil {
		t.Fatal("expected error when parent rpc is nil")
	}
}

// TestTokenWalletSendNotConnected mirrors TestTokenWalletBalanceNotConnected
// for Send. Asserts the specific "not connected" message rather than any
// error, since without the nil-rpc guard control flow still reaches the
// (also non-nil) "wallet is locked" error from the zero-value privKey --
// which would pass a bare err != nil check without the guard being present.
func TestTokenWalletSendNotConnected(t *testing.T) {
	tw := &TokenWallet{parent: &NearWallet{}}
	_, err := tw.Send("receiver.near", 1, 0)
	if err == nil {
		t.Fatal("expected error when parent rpc is nil")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("err = %q, want a wallet-not-connected error", err)
	}
}
