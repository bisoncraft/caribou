// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/bisoncraft/caribou/wallet/txdb"
)

// newScanTestWallet builds a wallet wired to f with a real txDB (scanChunk
// hits the DB directly, unlike the send paths) and the test USDC token
// contract registered so token-receive detection can be exercised.
func newScanTestWallet(t *testing.T, f *fakeRPCServer) *NearWallet {
	t.Helper()
	w := newTestWallet(t, f)

	master, err := txdb.New(filepath.Join(t.TempDir(), "tx.db"), testLogger)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	w.txDB = master.NetworkTxDB(BipID)
	w.tokenContracts = map[string]uint32{"usdc.token.near": usdcTokenID}
	return w
}

// blockWithChunks registers a "block" and "chunk" fixture for the given
// height: getBlockByHeight(height) returns a block naming one chunk, whose
// getChunk response carries txs.
func blockWithChunks(f *fakeRPCServer, blocks map[uint64][]chunkTransaction) {
	f.handle("block", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			BlockID uint64 `json:"block_id"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Message: "bad block_id"}
		}
		if _, ok := blocks[req.BlockID]; !ok {
			return nil, &rpcError{Name: "HANDLER_ERROR", Message: "unknown block"}
		}
		return map[string]any{
			"header": map[string]any{
				"height":    req.BlockID,
				"hash":      "11111111111111111111111111111111",
				"timestamp": 1_700_000_000_000_000_000,
			},
			"chunks": []any{
				map[string]any{"chunk_hash": chunkHashFor(req.BlockID), "shard_id": 0},
			},
		}, nil
	})
	f.handle("chunk", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			ChunkID string `json:"chunk_id"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Message: "bad chunk_id"}
		}
		height, ok := heightForChunkHash(blocks, req.ChunkID)
		if !ok {
			return nil, &rpcError{Name: "HANDLER_ERROR", Message: "unknown chunk"}
		}
		return map[string]any{"transactions": blocks[height]}, nil
	})
}

func chunkHashFor(height uint64) string {
	return "chunk-" + strconv.FormatUint(height, 10)
}

func heightForChunkHash(blocks map[uint64][]chunkTransaction, hash string) (uint64, bool) {
	for h := range blocks {
		if chunkHashFor(h) == hash {
			return h, true
		}
	}
	return 0, false
}

func ftTransferAction(t *testing.T, method, receiverID, amount string) txAction {
	t.Helper()
	args, err := json.Marshal(map[string]string{"receiver_id": receiverID, "amount": amount})
	if err != nil {
		t.Fatalf("marshal ft args: %v", err)
	}
	return txAction{FunctionCall: &txFunctionCall{
		MethodName: method,
		Args:       base64.StdEncoding.EncodeToString(args),
	}}
}

func transferAction2(deposit string) txAction {
	return txAction{Transfer: &txTransfer{Deposit: deposit}}
}

// TestScanIncomingNativeReceive verifies a native receive is stored with
// Recipient set to this wallet's own account and the sender recorded in
// AdditionalData, not the other way around (F4).
func TestScanIncomingNativeReceive(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 9

	blockWithChunks(f, map[uint64][]chunkTransaction{
		10: {{
			Hash:       "recvhash1",
			SignerID:   "alice.near",
			ReceiverID: w.accountID,
			Actions:    []txAction{transferAction2(DropsToYocto(500).String())},
		}},
	})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 10)

	if w.lastScannedHeight != 10 {
		t.Fatalf("lastScannedHeight = %d, want 10", w.lastScannedHeight)
	}
	tx, err := w.txDB.GetTx("recvhash1")
	if err != nil || tx == nil {
		t.Fatalf("GetTx: %v, %v", tx, err)
	}
	if tx.Recipient == nil || *tx.Recipient != w.accountID {
		t.Fatalf("Recipient = %v, want own account %s", tx.Recipient, w.accountID)
	}
	if tx.AdditionalData["sender"] != "alice.near" {
		t.Fatalf("AdditionalData[sender] = %q, want alice.near", tx.AdditionalData["sender"])
	}
	if tx.Amount != 500 {
		t.Fatalf("Amount = %d, want 500", tx.Amount)
	}
}

// TestScanIncomingTokenReceive verifies an incoming ft_transfer to a
// registered NEP-141 contract is recorded with TokenID set, and that a
// failed on-chain receipt is not credited (F1).
func TestScanIncomingTokenReceive(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 9

	blockWithChunks(f, map[uint64][]chunkTransaction{
		10: {
			{
				Hash:       "ftok-ok",
				SignerID:   "bob.near",
				ReceiverID: "usdc.token.near",
				Actions:    []txAction{ftTransferAction(t, "ft_transfer", w.accountID, "2500000")},
			},
			{
				Hash:       "ftok-fail",
				SignerID:   "carol.near",
				ReceiverID: "usdc.token.near",
				Actions:    []txAction{ftTransferAction(t, "ft_transfer", w.accountID, "999")},
			},
		},
	})
	f.handle("tx", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			TxHash string `json:"tx_hash"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad tx params: %v", err)
		}
		if req.TxHash == "ftok-fail" {
			return map[string]any{"status": map[string]any{"Failure": map[string]any{}}}, nil
		}
		return map[string]any{"status": map[string]any{"SuccessValue": ""}}, nil
	})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 10)

	ok, err := w.txDB.GetTx("ftok-ok")
	if err != nil || ok == nil {
		t.Fatalf("GetTx(ftok-ok): %v, %v", ok, err)
	}
	if ok.TokenID == nil || *ok.TokenID != usdcTokenID {
		t.Fatalf("TokenID = %v, want %d", ok.TokenID, usdcTokenID)
	}
	if ok.Amount != 2_500_000 {
		t.Fatalf("Amount = %d, want 2500000", ok.Amount)
	}
	if ok.Recipient == nil || *ok.Recipient != w.accountID {
		t.Fatalf("Recipient = %v, want own account", ok.Recipient)
	}
	if ok.AdditionalData["sender"] != "bob.near" {
		t.Fatalf("sender = %q, want bob.near", ok.AdditionalData["sender"])
	}

	failed, err := w.txDB.GetTx("ftok-fail")
	if err != nil {
		t.Fatalf("GetTx(ftok-fail): %v", err)
	}
	if failed != nil {
		t.Fatalf("failed ft_transfer was recorded as a receive: %+v", failed)
	}
}

// TestScanIncomingNoBalanceGate verifies scanIncomingTxs no longer queries
// account balance to decide whether to scan (the F2 net-negative-window
// bug): a receive is detected in a window purely from block/chunk data,
// with no "query" RPC call made at all.
func TestScanIncomingNoBalanceGate(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 9

	blockWithChunks(f, map[uint64][]chunkTransaction{
		10: {{
			Hash:       "recvhash2",
			SignerID:   "dave.near",
			ReceiverID: w.accountID,
			Actions:    []txAction{transferAction2(DropsToYocto(10).String())},
		}},
	})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 10)

	if n := f.callCount("query"); n != 0 {
		t.Fatalf("scanIncomingTxs made %d balance queries, want 0", n)
	}
	if tx, err := w.txDB.GetTx("recvhash2"); err != nil || tx == nil {
		t.Fatalf("receive not recorded despite no balance gate: %v, %v", tx, err)
	}
}

// TestScanIncomingWatermarkStopsOnError verifies a mid-scan RPC error does
// not advance lastScannedHeight past the last block actually scanned (F2,
// path 3): the failed block and everything after it must be retried on a
// later call, not silently skipped forever.
func TestScanIncomingWatermarkStopsOnError(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	blocks := map[uint64][]chunkTransaction{
		1: {},
		2: {},
		// height 3 deliberately missing: getBlockByHeight fails for it.
		4: {},
		5: {{
			Hash:       "late-recv",
			SignerID:   "erin.near",
			ReceiverID: w.accountID,
			Actions:    []txAction{transferAction2(DropsToYocto(1).String())},
		}},
	}
	blockWithChunks(f, blocks)

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 5)

	if w.lastScannedHeight != 2 {
		t.Fatalf("lastScannedHeight = %d, want 2 (stopped before the failed block)", w.lastScannedHeight)
	}

	// The transient failure clears (e.g. the node catches up) and a later
	// call picks up where the error left off, reaching the receive that
	// would otherwise have been lost forever.
	blocks[3] = []chunkTransaction{}
	w.scanIncomingTxs(context.Background(), mustClient(t, w), 5)
	if w.lastScannedHeight != 5 {
		t.Fatalf("lastScannedHeight after retry = %d, want 5", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("late-recv"); err != nil || tx == nil {
		t.Fatalf("receive past the earlier error was not eventually recorded: %v, %v", tx, err)
	}
}

// TestScanIncomingWatermarkDrainsBacklog verifies a backlog larger than
// maxScanBlocks is drained gradually over successive calls, rather than the
// unscanned tail being permanently skipped by jumping the watermark to the
// tip (F2, path 2).
func TestScanIncomingWatermarkDrainsBacklog(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	const tip = 25
	blocks := make(map[uint64][]chunkTransaction, tip)
	for h := uint64(1); h <= tip; h++ {
		blocks[h] = nil
	}
	// Bury a receive deep in the backlog (block 3) to prove it survives the
	// clamp instead of being skipped when startHeight is advanced past it.
	blocks[3] = []chunkTransaction{{
		Hash:       "buried-recv",
		SignerID:   "frank.near",
		ReceiverID: w.accountID,
		Actions:    []txAction{transferAction2(DropsToYocto(7).String())},
	}}
	blockWithChunks(f, blocks)

	rpc := mustClient(t, w)
	w.scanIncomingTxs(context.Background(), rpc, tip)
	if w.lastScannedHeight != maxScanBlocks {
		t.Fatalf("after first call lastScannedHeight = %d, want %d", w.lastScannedHeight, maxScanBlocks)
	}
	if tx, err := w.txDB.GetTx("buried-recv"); err != nil || tx == nil {
		t.Fatalf("buried receive not recorded on first pass: %v, %v", tx, err)
	}

	w.scanIncomingTxs(context.Background(), rpc, tip)
	if w.lastScannedHeight != 2*maxScanBlocks {
		t.Fatalf("after second call lastScannedHeight = %d, want %d", w.lastScannedHeight, 2*maxScanBlocks)
	}

	w.scanIncomingTxs(context.Background(), rpc, tip)
	if w.lastScannedHeight != tip {
		t.Fatalf("after third call lastScannedHeight = %d, want %d (fully caught up)", w.lastScannedHeight, tip)
	}
}

func mustClient(t *testing.T, w *NearWallet) *rpcClient {
	t.Helper()
	rpc, err := w.client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return rpc
}
