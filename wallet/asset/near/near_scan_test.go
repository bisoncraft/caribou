// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
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
// getChunk response carries txs. A height with no entry in blocks is a
// generic (transient-looking) RPC error, not NEAR's authoritative
// UNKNOWN_BLOCK — see blockWithChunksAndSkips for that case.
func blockWithChunks(f *fakeRPCServer, blocks map[uint64][]chunkTransaction) {
	blockWithChunksAndSkips(f, blocks, nil)
}

// blockWithChunksAndSkips is blockWithChunks, plus a set of heights that
// return NEAR's authoritative UNKNOWN_BLOCK error (a routine gap from a
// missed block-producer slot) instead of a generic error.
func blockWithChunksAndSkips(f *fakeRPCServer, blocks map[uint64][]chunkTransaction, skipped map[uint64]bool) {
	f.handle("block", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			BlockID uint64 `json:"block_id"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Message: "bad block_id"}
		}
		if _, ok := blocks[req.BlockID]; !ok {
			if skipped[req.BlockID] {
				return nil, &rpcError{
					Name:  "HANDLER_ERROR",
					Cause: &rpcErrorCause{Name: "UNKNOWN_BLOCK"},
				}
			}
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
// registered NEP-141 contract is recorded under the token's AssetID, and that a
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
	if ok.AssetID != usdcTokenID {
		t.Fatalf("AssetID = %v, want %d", ok.AssetID, usdcTokenID)
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

// TestScanIncomingWatermarkStopsOnError verifies a mid-scan *generic* RPC
// error (a transient failure, distinct from NEAR's authoritative
// UNKNOWN_BLOCK — see TestScanIncomingSkipsUnknownBlockHeight for that
// case) does not advance lastScannedHeight past the last block actually
// scanned (F2, path 3): the failed block and everything after it must be
// retried on a later call, not silently skipped forever.
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

// TestScanIncomingSkipsUnknownBlockHeight verifies that a NEAR
// UNKNOWN_BLOCK response — a routine gap from a missed block-producer
// slot, not a transient failure — advances past the missing height
// immediately, on the first call, rather than stalling the scan there
// forever (F1).
func TestScanIncomingSkipsUnknownBlockHeight(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	blockWithChunksAndSkips(f, map[uint64][]chunkTransaction{
		1: {},
		2: {},
		// height 3 is a genuine gap: no block was ever produced there.
		4: {},
		5: {{
			Hash:       "recv-after-gap",
			SignerID:   "erin.near",
			ReceiverID: w.accountID,
			Actions:    []txAction{transferAction2(DropsToYocto(1).String())},
		}},
	}, map[uint64]bool{3: true})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 5)

	if w.lastScannedHeight != 5 {
		t.Fatalf("lastScannedHeight = %d, want 5 (gap skipped on the first pass)", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("recv-after-gap"); err != nil || tx == nil {
		t.Fatalf("receive past the gap was not recorded on the first call: %v, %v", tx, err)
	}
}

// TestScanIncomingChunkFailureNotCheckpointed verifies that a chunk which
// fails to fetch stops the watermark from advancing past its block, and
// that the block (and the receive in it) is retried, not lost, on a later
// call (F2).
func TestScanIncomingChunkFailureNotCheckpointed(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	blocks := map[uint64][]chunkTransaction{
		1: {},
		2: {{
			Hash:       "chunk-fail-recv",
			SignerID:   "gina.near",
			ReceiverID: w.accountID,
			Actions:    []txAction{transferAction2(DropsToYocto(3).String())},
		}},
		3: {},
	}
	blockWithChunks(f, blocks)

	var chunkFail bool
	f.handle("chunk", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			ChunkID string `json:"chunk_id"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad chunk_id: %v", err)
		}
		if chunkFail && req.ChunkID == chunkHashFor(2) {
			return nil, &rpcError{Name: "HANDLER_ERROR", Message: "boom"}
		}
		height, ok := heightForChunkHash(blocks, req.ChunkID)
		if !ok {
			return nil, &rpcError{Name: "HANDLER_ERROR", Message: "unknown chunk"}
		}
		return map[string]any{"transactions": blocks[height]}, nil
	})

	chunkFail = true
	w.scanIncomingTxs(context.Background(), mustClient(t, w), 3)
	if w.lastScannedHeight != 1 {
		t.Fatalf("lastScannedHeight = %d, want 1 (stopped before the failed chunk's block)", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("chunk-fail-recv"); err != nil || tx != nil {
		t.Fatalf("receive recorded despite the chunk fetch failing: %v, %v", tx, err)
	}

	chunkFail = false
	w.scanIncomingTxs(context.Background(), mustClient(t, w), 3)
	if w.lastScannedHeight != 3 {
		t.Fatalf("lastScannedHeight after retry = %d, want 3", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("chunk-fail-recv"); err != nil || tx == nil {
		t.Fatalf("receive not recorded after the chunk fetch recovered: %v, %v", tx, err)
	}
}

// TestScanIncomingTrailingUnknownBlockNotConfirmed verifies an UNKNOWN_BLOCK
// gap at the end of a scan window is not folded into the watermark until a
// later height proves the node is past it: a lagging or load-balanced
// endpoint answering UNKNOWN_BLOCK for a height it simply has not received
// yet must not have that height silently marked scanned (review finding 1).
func TestScanIncomingTrailingUnknownBlockNotConfirmed(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	blockWithChunksAndSkips(f, map[uint64][]chunkTransaction{
		1: {},
		2: {},
		// height 3 is the trailing (last requested) height and is
		// unresolved: could be a genuine gap or a lagging endpoint.
	}, map[uint64]bool{3: true})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 3)
	if w.lastScannedHeight != 2 {
		t.Fatalf("lastScannedHeight = %d, want 2 (trailing gap must not be confirmed yet)", w.lastScannedHeight)
	}

	// The node catches up and now has height 3; the gap resolves as an
	// ordinary successfully-scanned height on the next poll.
	blockWithChunksAndSkips(f, map[uint64][]chunkTransaction{
		1: {}, 2: {}, 3: {},
	}, nil)
	w.scanIncomingTxs(context.Background(), mustClient(t, w), 3)
	if w.lastScannedHeight != 3 {
		t.Fatalf("lastScannedHeight after catch-up = %d, want 3", w.lastScannedHeight)
	}
}

// TestScanIncomingAllGapWindowStallBound verifies a scan window that is
// entirely unresolved UNKNOWN_BLOCK gaps — none of them ever confirmed by a
// later successful height in the same window — still counts toward the
// stall backstop and eventually force-advances, rather than repeating the
// same unadvancing window forever. Before this fix, the gap branch's
// `continue` bypassed the stalled/blockOK bookkeeping entirely, so
// stallCount never incremented and the backstop never engaged for an
// all-gap window (goaudit finding F1).
func TestScanIncomingAllGapWindowStallBound(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	skipped := make(map[uint64]bool, maxScanBlocks+5)
	for h := uint64(1); h <= maxScanBlocks+5; h++ {
		skipped[h] = true
	}
	blockWithChunksAndSkips(f, nil, skipped)

	const tip = maxScanBlocks + 5
	for i := 0; i < maxStallRetries-1; i++ {
		w.scanIncomingTxs(context.Background(), mustClient(t, w), tip)
		if w.lastScannedHeight != 0 {
			t.Fatalf("call %d: lastScannedHeight = %d, want 0 (still an unresolved all-gap window)", i, w.lastScannedHeight)
		}
	}

	w.scanIncomingTxs(context.Background(), mustClient(t, w), tip)
	if w.lastScannedHeight != 1 {
		t.Fatalf("lastScannedHeight after %d all-gap polls = %d, want 1 (forced past the first gap)", maxStallRetries, w.lastScannedHeight)
	}
}

// TestScanIncomingStallBoundForcesAdvance verifies a height that keeps
// failing the same way (not classified as a definitive, ruled-out error)
// does not wedge the watermark forever: after maxStallRetries consecutive
// stalled polls it is forced past (review finding 2).
func TestScanIncomingStallBoundForcesAdvance(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	blocks := map[uint64][]chunkTransaction{
		1: {},
		2: {},
	}
	blockWithChunks(f, blocks)

	f.handle("chunk", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			ChunkID string `json:"chunk_id"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad chunk_id: %v", err)
		}
		if req.ChunkID == chunkHashFor(2) {
			return nil, &rpcError{Name: "HANDLER_ERROR", Message: "boom"}
		}
		height, ok := heightForChunkHash(blocks, req.ChunkID)
		if !ok {
			return nil, &rpcError{Name: "HANDLER_ERROR", Message: "unknown chunk"}
		}
		return map[string]any{"transactions": blocks[height]}, nil
	})

	for i := 0; i < maxStallRetries-1; i++ {
		w.scanIncomingTxs(context.Background(), mustClient(t, w), 2)
		if w.lastScannedHeight != 1 {
			t.Fatalf("call %d: lastScannedHeight = %d, want 1 (still stalled)", i, w.lastScannedHeight)
		}
	}

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 2)
	if w.lastScannedHeight != 2 {
		t.Fatalf("lastScannedHeight after %d stalled polls = %d, want 2 (forced past)", maxStallRetries, w.lastScannedHeight)
	}
}

// TestScanIncomingUnknownChunkNotRuledOutImmediately verifies a chunk fetch
// that comes back UNKNOWN_CHUNK is retried, not immediately treated as
// definitive: rpcClient fails over per-call, so the block that named this
// chunk may have come from a different, better-synced endpoint than the one
// that just answered UNKNOWN_CHUNK for it. Ruling it out on the first
// answer would reintroduce the same silent-loss bug class the UNKNOWN_BLOCK
// fix closed. It must still resolve (and credit the receive) once a later
// poll gets a real answer (goaudit finding F3).
func TestScanIncomingUnknownChunkNotRuledOutImmediately(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	blocks := map[uint64][]chunkTransaction{
		1: {{
			Hash:       "would-be-lost",
			SignerID:   "iris.near",
			ReceiverID: w.accountID,
			Actions:    []txAction{transferAction2(DropsToYocto(2).String())},
		}},
		2: {},
	}
	blockWithChunks(f, blocks)

	var chunkFail bool
	f.handle("chunk", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			ChunkID string `json:"chunk_id"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad chunk_id: %v", err)
		}
		if chunkFail && req.ChunkID == chunkHashFor(1) {
			return nil, &rpcError{Name: "HANDLER_ERROR", Cause: &rpcErrorCause{Name: "UNKNOWN_CHUNK"}}
		}
		height, ok := heightForChunkHash(blocks, req.ChunkID)
		if !ok {
			return nil, &rpcError{Name: "HANDLER_ERROR", Message: "unknown chunk"}
		}
		return map[string]any{"transactions": blocks[height]}, nil
	})

	chunkFail = true
	w.scanIncomingTxs(context.Background(), mustClient(t, w), 2)
	if w.lastScannedHeight != 0 {
		t.Fatalf("lastScannedHeight = %d, want 0 (UNKNOWN_CHUNK must not be ruled out on the first answer)", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("would-be-lost"); err != nil || tx != nil {
		t.Fatalf("receive recorded despite the chunk fetch failing: %v, %v", tx, err)
	}

	chunkFail = false
	w.scanIncomingTxs(context.Background(), mustClient(t, w), 2)
	if w.lastScannedHeight != 2 {
		t.Fatalf("lastScannedHeight after retry = %d, want 2", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("would-be-lost"); err != nil || tx == nil {
		t.Fatalf("receive not recorded once the chunk resolved: %v, %v", tx, err)
	}
}

// TestScanIncomingUnknownTransactionNotRuledOutImmediately verifies a token
// transfer whose status query comes back UNKNOWN_TRANSACTION is retried,
// not immediately treated as definitive: unlike checkPendingTxs (which has
// a broadcast reference height to judge a tx truly dead against), a
// transaction just read out of a scanned chunk answering UNKNOWN_TRANSACTION
// more likely reflects that endpoint's own shard-tracking lag. It must
// still resolve once a later poll gets a real answer (goaudit finding F3).
func TestScanIncomingUnknownTransactionNotRuledOutImmediately(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	blockWithChunks(f, map[uint64][]chunkTransaction{
		1: {{
			Hash:       "ftok-unknown",
			SignerID:   "hank.near",
			ReceiverID: "usdc.token.near",
			Actions:    []txAction{ftTransferAction(t, "ft_transfer", w.accountID, "1000000")},
		}},
	})

	var txFail bool
	f.handle("tx", func(params json.RawMessage) (any, *rpcError) {
		if txFail {
			return nil, &rpcError{Name: "HANDLER_ERROR", Cause: &rpcErrorCause{Name: "UNKNOWN_TRANSACTION"}}
		}
		return map[string]any{"status": map[string]any{"SuccessValue": ""}}, nil
	})

	txFail = true
	w.scanIncomingTxs(context.Background(), mustClient(t, w), 1)
	if w.lastScannedHeight != 0 {
		t.Fatalf("lastScannedHeight = %d, want 0 (UNKNOWN_TRANSACTION must not be ruled out on the first answer)", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("ftok-unknown"); err != nil || tx != nil {
		t.Fatalf("receive recorded despite the status query failing: %v, %v", tx, err)
	}

	txFail = false
	w.scanIncomingTxs(context.Background(), mustClient(t, w), 1)
	if w.lastScannedHeight != 1 {
		t.Fatalf("lastScannedHeight after retry = %d, want 1", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("ftok-unknown"); err != nil || tx == nil {
		t.Fatalf("receive not recorded once the status query resolved: %v, %v", tx, err)
	}
}

// TestScanIncomingTokenReceivePending verifies a token transfer whose
// receipt has not yet reached a final outcome is retried on a later scan
// rather than being treated as failed (F3).
func TestScanIncomingTokenReceivePending(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	blockWithChunks(f, map[uint64][]chunkTransaction{
		1: {{
			Hash:       "ftok-pending",
			SignerID:   "hank.near",
			ReceiverID: "usdc.token.near",
			Actions:    []txAction{ftTransferAction(t, "ft_transfer", w.accountID, "1000000")},
		}},
	})

	var final bool
	f.handle("tx", func(params json.RawMessage) (any, *rpcError) {
		if !final {
			// The real wait_until:NONE response for a not-yet-finalized
			// outcome carries no "status" key at all.
			return map[string]any{"final_execution_status": "NONE"}, nil
		}
		return map[string]any{"status": map[string]any{"SuccessValue": ""}}, nil
	})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 1)
	if w.lastScannedHeight != 0 {
		t.Fatalf("lastScannedHeight = %d, want 0 (pending receipt must not be checkpointed)", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("ftok-pending"); err != nil || tx != nil {
		t.Fatalf("pending token tx recorded early: %v, %v", tx, err)
	}

	final = true
	w.scanIncomingTxs(context.Background(), mustClient(t, w), 1)
	if w.lastScannedHeight != 1 {
		t.Fatalf("lastScannedHeight after resolution = %d, want 1", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("ftok-pending"); err != nil || tx == nil {
		t.Fatalf("token receive not recorded once its receipt finalized: %v, %v", tx, err)
	}
}

// TestScanIncomingIgnoresFtTransferCall verifies an ft_transfer_call
// naming this account is never credited, since this wallet's account is
// never a contract and so the call is always refunded on-chain even
// though the transaction itself reports success (F6).
func TestScanIncomingIgnoresFtTransferCall(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	blockWithChunks(f, map[uint64][]chunkTransaction{
		1: {{
			Hash:       "ftok-call",
			SignerID:   "ivan.near",
			ReceiverID: "usdc.token.near",
			Actions:    []txAction{ftTransferAction(t, "ft_transfer_call", w.accountID, "5000000")},
		}},
	})
	f.handle("tx", func(json.RawMessage) (any, *rpcError) {
		t.Fatal("txStatus should not be queried for an ignored ft_transfer_call")
		return nil, nil
	})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 1)

	if w.lastScannedHeight != 1 {
		t.Fatalf("lastScannedHeight = %d, want 1", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("ftok-call"); err != nil || tx != nil {
		t.Fatalf("ft_transfer_call was credited: %v, %v", tx, err)
	}
}

// TestScanIncomingMultiActionNativeVerified verifies a multi-action
// transaction's native Transfer deposit is only credited once its
// receipt's outcome is confirmed, since a failing sibling action would
// have refunded the whole receipt (F7).
func TestScanIncomingMultiActionNativeVerified(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	blockWithChunks(f, map[uint64][]chunkTransaction{
		1: {{
			Hash:       "multi-action-fail",
			SignerID:   "judy.near",
			ReceiverID: w.accountID,
			Actions: []txAction{
				transferAction2(DropsToYocto(2).String()),
				{FunctionCall: &txFunctionCall{MethodName: "does_not_exist"}},
			},
		}},
	})
	f.handle("tx", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"status": map[string]any{"Failure": map[string]any{}}}, nil
	})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 1)

	if w.lastScannedHeight != 1 {
		t.Fatalf("lastScannedHeight = %d, want 1 (definitively failed, not retried)", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("multi-action-fail"); err != nil || tx != nil {
		t.Fatalf("multi-action transfer credited despite the receipt failing: %v, %v", tx, err)
	}
}

// TestScanIncomingTokenAmountOverflow verifies a NEP-141 amount exceeding
// uint64 is logged and not recorded, but does not stall the scan (F8).
func TestScanIncomingTokenAmountOverflow(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	huge := new(big.Int).Lsh(big.NewInt(1), 65).String() // exceeds uint64
	blockWithChunks(f, map[uint64][]chunkTransaction{
		1: {{
			Hash:       "ftok-overflow",
			SignerID:   "karl.near",
			ReceiverID: "usdc.token.near",
			Actions:    []txAction{ftTransferAction(t, "ft_transfer", w.accountID, huge)},
		}},
	})
	f.handle("tx", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"status": map[string]any{"SuccessValue": ""}}, nil
	})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 1)

	if w.lastScannedHeight != 1 {
		t.Fatalf("lastScannedHeight = %d, want 1 (an unrepresentable amount must not stall the scan)", w.lastScannedHeight)
	}
	if tx, err := w.txDB.GetTx("ftok-overflow"); err != nil || tx != nil {
		t.Fatalf("overflowing amount was recorded: %v, %v", tx, err)
	}
}

// TestScanIncomingTokenReceiveEmitsToTokenWallet verifies a detected
// token receive is announced through the open token wallet's own
// emitter, not the parent native wallet's, matching the emitter the
// token send path already uses (F5).
func TestScanIncomingTokenReceiveEmitsToTokenWallet(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	parentCh := make(chan asset.WalletNotification, 4)
	tokenCh := make(chan asset.WalletNotification, 4)
	w.emit = asset.NewWalletEmitter(parentCh, BipID, testLogger)
	tw := &TokenWallet{parent: w, assetID: usdcTokenID, emit: asset.NewWalletEmitter(tokenCh, usdcTokenID, testLogger)}
	w.tokenWallets = map[uint32]*TokenWallet{usdcTokenID: tw}

	blockWithChunks(f, map[uint64][]chunkTransaction{
		1: {{
			Hash:       "ftok-emit",
			SignerID:   "liam.near",
			ReceiverID: "usdc.token.near",
			Actions:    []txAction{ftTransferAction(t, "ft_transfer", w.accountID, "4200000")},
		}},
	})
	f.handle("tx", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"status": map[string]any{"SuccessValue": ""}}, nil
	})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 1)

	select {
	case n := <-tokenCh:
		note, ok := n.(*asset.TransactionNote)
		if !ok || note.Transaction.ID != "ftok-emit" {
			t.Fatalf("unexpected token wallet notification: %+v", n)
		}
	default:
		t.Fatal("token wallet emitter received no notification")
	}

	select {
	case n := <-parentCh:
		t.Fatalf("parent wallet emitter unexpectedly received a token notification: %+v", n)
	default:
	}
}

// TestScanIncomingSkipsRepeatedChunkHash verifies a shard that reuses its
// previous chunk hash (no new chunk produced for it) is not re-fetched,
// since its contents cannot have changed.
func TestScanIncomingSkipsRepeatedChunkHash(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newScanTestWallet(t, f)
	w.lastScannedHeight = 0

	f.handle("block", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			BlockID uint64 `json:"block_id"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Message: "bad block_id"}
		}
		if req.BlockID > 2 {
			return nil, &rpcError{Name: "HANDLER_ERROR", Message: "unknown block"}
		}
		return map[string]any{
			"header": map[string]any{
				"height":    req.BlockID,
				"hash":      "11111111111111111111111111111111",
				"timestamp": 1_700_000_000_000_000_000,
			},
			// Same chunk hash both blocks: shard 0 produced no new chunk
			// for block 2.
			"chunks": []any{
				map[string]any{"chunk_hash": "same-chunk", "shard_id": 0},
			},
		}, nil
	})
	f.handle("chunk", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"transactions": []chunkTransaction{}}, nil
	})

	w.scanIncomingTxs(context.Background(), mustClient(t, w), 2)

	if n := f.callCount("chunk"); n != 1 {
		t.Fatalf("chunk fetched %d times for a repeated hash, want 1", n)
	}
	if w.lastScannedHeight != 2 {
		t.Fatalf("lastScannedHeight = %d, want 2", w.lastScannedHeight)
	}
}
