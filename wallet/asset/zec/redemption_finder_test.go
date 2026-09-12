// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func notImplErr(name string) error { return fmt.Errorf("%s not implemented for this test", name) }

// newTestRedemptionFinder returns a redemptionFinder whose dependencies all
// fail unless overridden by the caller.
func newTestRedemptionFinder() *redemptionFinder {
	return newRedemptionFinder(
		tLogger,
		func(*chainhash.Hash) (*getTransactionResult, error) { return nil, notImplErr("getWalletTransaction") },
		func(*chainhash.Hash) (int32, error) { return 0, notImplErr("getBlockHeight") },
		func(chainhash.Hash) (*wire.MsgBlock, error) { return nil, notImplErr("getBlock") },
		func(*chainhash.Hash) (*btc.BlockHeader, bool, error) { return nil, false, notImplErr("getBlockHeader") },
		func(tx *wire.MsgTx) *chainhash.Hash { h := tx.TxHash(); return &h },
		func(b []byte) (*wire.MsgTx, error) {
			tx := wire.NewMsgTx(wire.TxVersion)
			if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
				return nil, err
			}
			return tx, nil
		},
		func() (int32, error) { return 0, notImplErr("getBestBlockHeight") },
		func(ctx context.Context, reqs map[btc.OutPoint]*findRedemptionReq, blockHash chainhash.Hash) map[btc.OutPoint]*findRedemptionResult {
			return nil
		},
		func(int64) (*chainhash.Hash, error) { return nil, notImplErr("getBlockHash") },
		func(ctx context.Context, reqs map[btc.OutPoint]*findRedemptionReq) map[btc.OutPoint]*findRedemptionResult {
			return nil
		},
	)
}

func encodeCoinID(txHash *chainhash.Hash, vout uint32) util.Bytes {
	b := make([]byte, 36)
	copy(b, txHash[:])
	binary.BigEndian.PutUint32(b[32:], vout)
	return b
}

func newFindRedemptionReq(outPt btc.OutPoint) *findRedemptionReq {
	return &findRedemptionReq{
		outPt:      outPt,
		resultChan: make(chan *findRedemptionResult, 1),
		pkScript:   []byte{0x01},
	}
}

func waitForResult(t *testing.T, ch chan *findRedemptionResult) *findRedemptionResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for result")
		return nil
	}
}

func TestQueueFindRedemptionRequest(t *testing.T) {
	rf := newTestRedemptionFinder()
	req := newFindRedemptionReq(btc.NewOutPoint(tTxHash, 0))
	if err := rf.queueFindRedemptionRequest(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := rf.queueFindRedemptionRequest(req); err == nil {
		t.Fatalf("expected error for duplicate request")
	}
}

func TestCheckRedemptionBlockDetails(t *testing.T) {
	rf := newTestRedemptionFinder()

	outPt := btc.NewOutPoint(tTxHash, 0)
	pkScript := []byte{0xaa, 0xbb}
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxOut(wire.NewTxOut(1, pkScript))
	blk := &wire.MsgBlock{Transactions: []*wire.MsgTx{tx}}

	rf.getBlockHeight = func(*chainhash.Hash) (int32, error) { return 100, nil }
	rf.hashTx = func(tx *wire.MsgTx) *chainhash.Hash { return tTxHash }
	rf.getBlock = func(chainhash.Hash) (*wire.MsgBlock, error) { return blk, nil }

	height, err := rf.checkRedemptionBlockDetails(outPt, tTxHash, pkScript)
	if err != nil {
		t.Fatalf("checkRedemptionBlockDetails error: %v", err)
	}
	if height != 100 {
		t.Fatalf("wrong height: %d", height)
	}

	// getBlockHeight error.
	rf.getBlockHeight = func(*chainhash.Hash) (int32, error) { return 0, errTest }
	if _, err := rf.checkRedemptionBlockDetails(outPt, tTxHash, pkScript); err == nil {
		t.Fatalf("expected getBlockHeight error")
	}
	rf.getBlockHeight = func(*chainhash.Hash) (int32, error) { return 100, nil }

	// getBlock error.
	rf.getBlock = func(chainhash.Hash) (*wire.MsgBlock, error) { return nil, errTest }
	if _, err := rf.checkRedemptionBlockDetails(outPt, tTxHash, pkScript); err == nil {
		t.Fatalf("expected getBlock error")
	}
	rf.getBlock = func(chainhash.Hash) (*wire.MsgBlock, error) { return blk, nil }

	// vout out of range.
	if _, err := rf.checkRedemptionBlockDetails(btc.NewOutPoint(tTxHash, 5), tTxHash, pkScript); err == nil {
		t.Fatalf("expected error for out-of-range vout")
	}

	// pkScript mismatch.
	if _, err := rf.checkRedemptionBlockDetails(outPt, tTxHash, []byte{0xff}); err == nil {
		t.Fatalf("expected error for pkScript mismatch")
	}

	// Not found on first attempt, but found after one retry.
	var calls int
	rf.hashTx = func(*wire.MsgTx) *chainhash.Hash {
		calls++
		if calls == 1 {
			// First call is the miss-scan of an empty block.
			var zero chainhash.Hash
			return &zero
		}
		return tTxHash
	}
	rf.getBlock = func(chainhash.Hash) (*wire.MsgBlock, error) { return blk, nil }
	height, err = rf.checkRedemptionBlockDetails(outPt, tTxHash, pkScript)
	if err != nil {
		t.Fatalf("checkRedemptionBlockDetails (retry) error: %v", err)
	}
	if height != 100 {
		t.Fatalf("wrong height after retry: %d", height)
	}
}

func TestTryRedemptionRequestsMempoolOnly(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt) // no blockHash: mempool-only
	rf.redemptions[outPt] = req

	rf.getBestBlockHeight = func() (int32, error) { return 0, nil }
	rf.findRedemptionsInMempool = func(ctx context.Context, reqs map[btc.OutPoint]*findRedemptionReq) map[btc.OutPoint]*findRedemptionResult {
		if _, ok := reqs[outPt]; !ok {
			t.Errorf("expected outpoint in mempool reqs")
		}
		return map[btc.OutPoint]*findRedemptionResult{
			outPt: {redemptionCoinID: encodeCoinID(tTxHash, 0)},
		}
	}

	rf.tryRedemptionRequests(tCtx, nil, []*findRedemptionReq{req})
	res := waitForResult(t, req.resultChan)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
}

func TestTryRedemptionRequestsBestHeightError(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	req.blockHash = tTxHash
	rf.redemptions[outPt] = req

	rf.getBestBlockHeight = func() (int32, error) { return 0, errTest }

	rf.tryRedemptionRequests(tCtx, nil, []*findRedemptionReq{req})
	res := waitForResult(t, req.resultChan)
	if res.err == nil {
		t.Fatalf("expected error result")
	}
}

func TestTryRedemptionRequestsFoundInBlock(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	req.blockHash = tTxHash
	req.blockHeight = 10
	rf.redemptions[outPt] = req

	rf.getBestBlockHeight = func() (int32, error) { return 10, nil }
	rf.searchBlockForRedemptions = func(ctx context.Context, reqs map[btc.OutPoint]*findRedemptionReq, blockHash chainhash.Hash) map[btc.OutPoint]*findRedemptionResult {
		return map[btc.OutPoint]*findRedemptionResult{
			outPt: {redemptionCoinID: encodeCoinID(tTxHash, 1)},
		}
	}

	rf.tryRedemptionRequests(tCtx, nil, []*findRedemptionReq{req})
	res := waitForResult(t, req.resultChan)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
}

func TestTryRedemptionRequestsStartBlockError(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	req.blockHash = tTxHash
	rf.redemptions[outPt] = req

	rf.getBestBlockHeight = func() (int32, error) { return 10, nil }
	rf.getBlockHeight = func(*chainhash.Hash) (int32, error) { return 0, errTest }

	rf.tryRedemptionRequests(tCtx, tTxHash, []*findRedemptionReq{req})
	res := waitForResult(t, req.resultChan)
	if res.err == nil {
		t.Fatalf("expected error result for startBlock getBlockHeight failure")
	}
}

func TestTryRedemptionRequestsBlockHashFetchError(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	req.blockHash = tTxHash
	req.blockHeight = 5
	rf.redemptions[outPt] = req

	rf.getBestBlockHeight = func() (int32, error) { return 10, nil }
	rf.searchBlockForRedemptions = func(ctx context.Context, reqs map[btc.OutPoint]*findRedemptionReq, blockHash chainhash.Hash) map[btc.OutPoint]*findRedemptionResult {
		return nil // nothing found at this height
	}
	rf.getBlockHash = func(int64) (*chainhash.Hash, error) { return nil, errTest }

	// No result should be sent (search is expected to resume later via block
	// monitor loop), so just ensure this returns promptly without blocking or
	// panicking.
	done := make(chan struct{})
	go func() {
		rf.tryRedemptionRequests(tCtx, nil, []*findRedemptionReq{req})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("tryRedemptionRequests did not return")
	}
}

func TestPrepareRedemptionRequestsForBlockCheck(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt) // no blockHash yet
	rf.redemptions[outPt] = req

	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) {
		return nil, errTest // trySetRedemptionRequestBlock will just log and return
	}

	reqs := rf.prepareRedemptionRequestsForBlockCheck()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
}

func TestTrySetRedemptionRequestBlock(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	rf.redemptions[outPt] = req

	pkScript := []byte{0xaa, 0xbb}
	req.pkScript = pkScript
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxOut(wire.NewTxOut(1, pkScript))
	blk := &wire.MsgBlock{Transactions: []*wire.MsgTx{tx}}

	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) {
		return &getTransactionResult{BlockHash: tTxHash.String()}, nil
	}
	rf.getBlockHeight = func(*chainhash.Hash) (int32, error) { return 50, nil }
	rf.hashTx = func(*wire.MsgTx) *chainhash.Hash { return tTxHash }
	rf.getBlock = func(chainhash.Hash) (*wire.MsgBlock, error) { return blk, nil }

	rf.trySetRedemptionRequestBlock(req)
	updated := rf.redemptions[outPt]
	if updated.blockHash == nil || updated.blockHeight != 50 {
		t.Fatalf("request block details not set: %+v", updated)
	}

	// getWalletTransaction error: no change, no panic.
	req2 := newFindRedemptionReq(btc.NewOutPoint(tTxHash, 1))
	rf.redemptions[req2.outPt] = req2
	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) { return nil, errTest }
	rf.trySetRedemptionRequestBlock(req2)
	if rf.redemptions[req2.outPt].blockHash != nil {
		t.Fatalf("expected no block hash set on error")
	}

	// No block hash yet on the tx: no-op.
	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) {
		return &getTransactionResult{}, nil
	}
	rf.trySetRedemptionRequestBlock(req2)
	if rf.redemptions[req2.outPt].blockHash != nil {
		t.Fatalf("expected no block hash set when tx has none")
	}

	// Bad block hash string: no-op.
	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) {
		return &getTransactionResult{BlockHash: "not-a-hash"}, nil
	}
	rf.trySetRedemptionRequestBlock(req2)
	if rf.redemptions[req2.outPt].blockHash != nil {
		t.Fatalf("expected no block hash set for bad hash string")
	}
}

func TestCancelRedemptionSearches(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	rf.redemptions[outPt] = req

	rf.CancelRedemptionSearches()

	res := waitForResult(t, req.resultChan)
	if res.err == nil {
		t.Fatalf("expected cancellation error")
	}
	if len(rf.redemptions) != 0 {
		t.Fatalf("expected redemptions map to be cleared")
	}
}

func TestReportNewTipSingleBlock(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	req.blockHash = tTxHash
	rf.redemptions[outPt] = req

	prevTip := &blockVector{Hash: *tTxHash, Height: 10}
	newTip := &blockVector{Hash: *tTxHash, Height: 11}

	rf.getBlockHeader = func(*chainhash.Hash) (*btc.BlockHeader, bool, error) {
		return &btc.BlockHeader{Height: 10}, true, nil
	}
	rf.getBestBlockHeight = func() (int32, error) { return 11, nil }
	rf.getBlockHeight = func(*chainhash.Hash) (int32, error) { return 11, nil }
	rf.searchBlockForRedemptions = func(ctx context.Context, reqs map[btc.OutPoint]*findRedemptionReq, blockHash chainhash.Hash) map[btc.OutPoint]*findRedemptionResult {
		return map[btc.OutPoint]*findRedemptionResult{
			outPt: {redemptionCoinID: encodeCoinID(tTxHash, 0)},
		}
	}

	rf.ReportNewTip(tCtx, prevTip, newTip)
	res := waitForResult(t, req.resultChan)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
}

func TestReportNewTipGetBlockHeaderError(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	rf.redemptions[outPt] = req
	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) { return nil, errTest }

	prevTip := &blockVector{Hash: *tTxHash, Height: 10}
	newTip := &blockVector{Hash: *tTxHash, Height: 11}

	rf.getBlockHeader = func(*chainhash.Hash) (*btc.BlockHeader, bool, error) {
		return nil, false, errTest
	}

	rf.ReportNewTip(tCtx, prevTip, newTip)
	res := waitForResult(t, req.resultChan)
	if res.err == nil {
		t.Fatalf("expected fatal error result")
	}
}

func TestReportNewTipHeightGap(t *testing.T) {
	rf := newTestRedemptionFinder()
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	req.blockHash = tTxHash
	rf.redemptions[outPt] = req

	prevTip := &blockVector{Hash: *tTxHash, Height: 10}
	newTip := &blockVector{Hash: *tTxHash, Height: 15}

	rf.getBlockHeader = func(*chainhash.Hash) (*btc.BlockHeader, bool, error) {
		return &btc.BlockHeader{Height: 10}, true, nil
	}
	rf.getBlockHash = func(h int64) (*chainhash.Hash, error) {
		if h != 11 {
			t.Errorf("expected height 11, got %d", h)
		}
		return tTxHash, nil
	}
	rf.getBestBlockHeight = func() (int32, error) { return 15, nil }
	rf.getBlockHeight = func(*chainhash.Hash) (int32, error) { return 11, nil }
	rf.searchBlockForRedemptions = func(ctx context.Context, reqs map[btc.OutPoint]*findRedemptionReq, blockHash chainhash.Hash) map[btc.OutPoint]*findRedemptionResult {
		return map[btc.OutPoint]*findRedemptionResult{
			outPt: {redemptionCoinID: encodeCoinID(tTxHash, 0)},
		}
	}

	rf.ReportNewTip(tCtx, prevTip, newTip)
	res := waitForResult(t, req.resultChan)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
}

func TestReportNewTipNoRequests(t *testing.T) {
	rf := newTestRedemptionFinder()

	prevTip := &blockVector{Hash: *tTxHash, Height: 10}
	newTip := &blockVector{Hash: *tTxHash, Height: 11}

	rf.getBlockHeader = func(*chainhash.Hash) (*btc.BlockHeader, bool, error) {
		return &btc.BlockHeader{Height: 10}, true, nil
	}

	// Should return without starting a search goroutine or panicking.
	rf.ReportNewTip(tCtx, prevTip, newTip)
}

func TestFindRedemptionsInTxWithHasher(t *testing.T) {
	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	req.contractHash = []byte{0x01, 0x02, 0x03}
	reqs := map[btc.OutPoint]*findRedemptionReq{outPt: req}

	hashTx := func(*wire.MsgTx) *chainhash.Hash { return tTxHash }

	// No matching previous outpoint.
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{0x09}, 3), nil, nil))
	discovered := findRedemptionsInTxWithHasher(tCtx, false, reqs, tx, MainNetParams, hashTx)
	if len(discovered) != 0 {
		t.Fatalf("expected no discoveries for non-matching input")
	}

	// Matching previous outpoint but invalid redemption script: fails the
	// request instead of adding to discovered.
	tx2 := wire.NewMsgTx(wire.TxVersion)
	tx2.AddTxIn(wire.NewTxIn(wire.NewOutPoint(tTxHash, 0), []byte{0x00}, nil))
	req2 := newFindRedemptionReq(outPt)
	req2.contractHash = []byte{0x01, 0x02, 0x03}
	reqs2 := map[btc.OutPoint]*findRedemptionReq{outPt: req2}
	discovered = findRedemptionsInTxWithHasher(tCtx, false, reqs2, tx2, MainNetParams, hashTx)
	if len(discovered) != 0 {
		t.Fatalf("expected no successful discoveries for invalid redemption script")
	}
	res := waitForResult(t, req2.resultChan)
	if res.err == nil {
		t.Fatalf("expected req to be failed for invalid redemption script")
	}

	// Cancelled context returns immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	discovered = findRedemptionsInTxWithHasher(ctx, false, reqs, tx, MainNetParams, hashTx)
	if len(discovered) != 0 {
		t.Fatalf("expected no discoveries for cancelled context")
	}
}

func TestIsTxNotFoundErrHelper(t *testing.T) {
	if isTxNotFoundErr(errTest) {
		t.Fatalf("plain error should not be a tx-not-found error")
	}
}

func TestChainOKHelper(t *testing.T) {
	tests := []struct {
		net  util.Network
		str  string
		want bool
	}{
		{util.Mainnet, "main", true},
		{util.Mainnet, "test", false},
		{util.Testnet, "test", true},
		{util.Regtest, "regtest", true},
	}
	for _, tt := range tests {
		if got := chainOK(tt.net, tt.str); got != tt.want {
			t.Errorf("chainOK(%v, %q) = %v, want %v", tt.net, tt.str, got, tt.want)
		}
	}
}

func TestFindRedemptionsInMempoolAndSearchBlock(t *testing.T) {
	txWithMatch := wire.NewMsgTx(wire.TxVersion)
	txWithMatch.AddTxIn(wire.NewTxIn(wire.NewOutPoint(tTxHash, 0), []byte{0x00}, nil))

	outPt := btc.NewOutPoint(tTxHash, 0)
	req := newFindRedemptionReq(outPt)
	req.contractHash = []byte{0x01}
	reqs := map[btc.OutPoint]*findRedemptionReq{outPt: req}
	hashTx := func(*wire.MsgTx) *chainhash.Hash { return tTxHash }

	discovered := searchBlockForRedemptions(tCtx, reqs, &wire.MsgBlock{Transactions: []*wire.MsgTx{txWithMatch}}, false, hashTx, MainNetParams)
	if len(discovered) != 0 {
		t.Fatalf("expected no successful discoveries (invalid script), got %+v", discovered)
	}
	waitForResult(t, req.resultChan)

	// findRedemptionsInMempool: mempool retrieval error aborts cleanly.
	req2 := newFindRedemptionReq(outPt)
	reqs2 := map[btc.OutPoint]*findRedemptionReq{outPt: req2}
	discovered = findRedemptionsInMempool(tCtx, tLogger, reqs2, func() ([]*chainhash.Hash, error) {
		return nil, errTest
	}, nil, false, hashTx, MainNetParams)
	if len(discovered) != 0 {
		t.Fatalf("expected no discoveries when mempool retrieval fails")
	}

	// findRedemptionsInMempool: getTx error aborts.
	discovered = findRedemptionsInMempool(tCtx, tLogger, reqs2, func() ([]*chainhash.Hash, error) {
		return []*chainhash.Hash{tTxHash}, nil
	}, func(*chainhash.Hash) (*wire.MsgTx, error) {
		return nil, errTest
	}, false, hashTx, MainNetParams)
	if len(discovered) != 0 {
		t.Fatalf("expected no discoveries when getTx fails")
	}

	// findRedemptionsInMempool: happy path with no matches.
	req3 := newFindRedemptionReq(outPt)
	reqs3 := map[btc.OutPoint]*findRedemptionReq{outPt: req3}
	nonMatch := wire.NewMsgTx(wire.TxVersion)
	nonMatch.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{0x09}, 0), nil, nil))
	discovered = findRedemptionsInMempool(tCtx, tLogger, reqs3, func() ([]*chainhash.Hash, error) {
		return []*chainhash.Hash{tTxHash}, nil
	}, func(*chainhash.Hash) (*wire.MsgTx, error) {
		return nonMatch, nil
	}, false, hashTx, MainNetParams)
	if len(discovered) != 0 {
		t.Fatalf("expected no discoveries for non-matching mempool tx")
	}
}

func TestFindRedemptionDecodeError(t *testing.T) {
	rf := newTestRedemptionFinder()
	if _, _, err := rf.FindRedemption(tCtx, []byte{0x01}); err == nil {
		t.Fatalf("expected decode error for short coin ID")
	}
}

func TestFindRedemptionGetWalletTxError(t *testing.T) {
	rf := newTestRedemptionFinder()
	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) { return nil, errTest }
	coinID := encodeCoinID(tTxHash, 0)
	if _, _, err := rf.FindRedemption(tCtx, coinID); err == nil {
		t.Fatalf("expected error from getWalletTransaction failure")
	}
}

func TestFindRedemptionTxOutError(t *testing.T) {
	rf := newTestRedemptionFinder()
	dTx := dummyTx()
	txB, _ := dTx.Bytes()
	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) {
		return &getTransactionResult{Bytes: txB}, nil
	}
	// vout 5 is out of range for dummyTx's single output.
	coinID := encodeCoinID(tTxHash, 5)
	if _, _, err := rf.FindRedemption(tCtx, coinID); err == nil {
		t.Fatalf("expected error for out-of-range vout")
	}
}

func TestFindRedemptionCheckBlockDetailsError(t *testing.T) {
	rf := newTestRedemptionFinder()
	dTx := dummyTx()
	txB, _ := dTx.Bytes()
	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) {
		return &getTransactionResult{Bytes: txB, BlockHash: tTxHash.String()}, nil
	}
	rf.getBlockHeight = func(*chainhash.Hash) (int32, error) { return 0, errTest }
	coinID := encodeCoinID(tTxHash, 0)
	if _, _, err := rf.FindRedemption(tCtx, coinID); err == nil {
		t.Fatalf("expected error from checkRedemptionBlockDetails failure")
	}
}

func TestFindRedemptionDuplicate(t *testing.T) {
	rf := newTestRedemptionFinder()
	dTx := dummyTx()
	txB, _ := dTx.Bytes()
	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) {
		return &getTransactionResult{Bytes: txB}, nil
	}
	outPt := btc.NewOutPoint(tTxHash, 0)
	rf.redemptions[outPt] = newFindRedemptionReq(outPt)

	coinID := encodeCoinID(tTxHash, 0)
	if _, _, err := rf.FindRedemption(tCtx, coinID); err == nil {
		t.Fatalf("expected error for duplicate find redemption request")
	}
}

func TestFindRedemptionSuccess(t *testing.T) {
	rf := newTestRedemptionFinder()
	dTx := dummyTx()
	txB, _ := dTx.Bytes()
	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) {
		return &getTransactionResult{Bytes: txB}, nil // no BlockHash: mempool-only search
	}
	rf.getBestBlockHeight = func() (int32, error) { return 0, nil }
	rf.findRedemptionsInMempool = func(ctx context.Context, reqs map[btc.OutPoint]*findRedemptionReq) map[btc.OutPoint]*findRedemptionResult {
		out := make(map[btc.OutPoint]*findRedemptionResult, len(reqs))
		for outPt := range reqs {
			out[outPt] = &findRedemptionResult{redemptionCoinID: encodeCoinID(tTxHash, 1)}
		}
		return out
	}

	coinID := encodeCoinID(tTxHash, 0)
	redemptionCoin, secret, err := rf.FindRedemption(tCtx, coinID)
	if err != nil {
		t.Fatalf("FindRedemption error: %v", err)
	}
	if len(redemptionCoin) == 0 {
		t.Fatalf("expected a redemption coin ID")
	}
	if secret != nil {
		t.Fatalf("expected nil secret, got %x", secret)
	}

	// Ensure the request was removed from the queue after completion.
	if len(rf.redemptions) != 0 {
		t.Fatalf("expected redemptions map to be empty after completion")
	}
}

func TestFindRedemptionContextCancelled(t *testing.T) {
	rf := newTestRedemptionFinder()
	dTx := dummyTx()
	txB, _ := dTx.Bytes()
	rf.getWalletTransaction = func(*chainhash.Hash) (*getTransactionResult, error) {
		return &getTransactionResult{Bytes: txB}, nil
	}
	// Let the background goroutine fail quickly rather than block; the test
	// only cares that FindRedemption itself returns via ctx.Done().
	rf.getBestBlockHeight = func() (int32, error) { return 0, errTest }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	coinID := encodeCoinID(tTxHash, 0)
	if _, _, err := rf.FindRedemption(ctx, coinID); err == nil {
		t.Fatalf("expected error for cancelled context")
	}
}

func TestFindRedemptionReqPkScript(t *testing.T) {
	req := newFindRedemptionReq(btc.NewOutPoint(tTxHash, 0))
	req.pkScript = []byte{0x01, 0x02}
	if string(req.PkScript()) != string([]byte{0x01, 0x02}) {
		t.Fatalf("PkScript() returned unexpected bytes: %x", req.PkScript())
	}
}
