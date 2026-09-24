//go:build !harness && !rpclive

package eth

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// newTestMetaTable returns a real, badger-backed meta table so scan cursor
// persistence is exercised for real rather than through a hand-rolled fake.
func newTestMetaTable(t *testing.T) txdb.StoreTable {
	t.Helper()
	log := util.StdOutLogger("SCANMETA", util.LevelTrace)
	master, err := txdb.New(t.TempDir()+"/scan.db", log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	mt, err := master.NetworkTxDB(BipID).Table(scanMetaTableName)
	if err != nil {
		t.Fatalf("error opening meta table: %v", err)
	}
	return mt
}

// newScanTestWallets returns an ETHWallet fixture and a connected token
// wallet's assetWallet, wired into the ETHWallet's wallets map the way
// connectedWallets() expects.
func newScanTestWallets(t *testing.T) (eth *ETHWallet, tokenAw *assetWallet, cancel context.CancelFunc) {
	t.Helper()
	w, aw, _, cancelEth := tassetWallet(BipID)
	eth = w.(*ETHWallet)
	aw.connected.Store(true)

	_, tokenAssetWallet, tokenNode, cancelTok := tassetWallet(usdcEthID)
	tokenAssetWallet.connected.Store(true)
	tokenAssetWallet.tokenAddr = common.HexToAddress("0x0000000000000000000000000000000000dEaD")
	tokenAssetWallet.tokenContractor = tokenNode.tokenContractor
	// balance() is called (best-effort) after a receive is stored; give the
	// contractor a real value so that path doesn't panic on a nil balance.
	tokenNode.tokenContractor.bal = big.NewInt(0)

	eth.walletsMtx.Lock()
	eth.wallets[usdcEthID] = tokenAssetWallet
	eth.walletsMtx.Unlock()

	return eth, tokenAssetWallet, func() {
		cancelEth()
		cancelTok()
	}
}

func TestScanERC20Receive(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	txHash := common.HexToHash("0x01")
	eth.node.(*tMempoolNode).filterLogsResult = []types.Log{{
		Address:     tokenAw.tokenAddr,
		TxHash:      txHash,
		BlockNumber: 100,
	}}
	tokenAw.tokenContractor.(*tTokenContractor).parseTransferAmt = 500

	if err := eth.scanERC20Receives(context.Background(), 90, 100, []*assetWallet{tokenAw}); err != nil {
		t.Fatalf("scanERC20Receives: %v", err)
	}

	db := eth.txDB.(*tTxDB)
	if !db.storeTxCalled {
		t.Fatalf("expected a tx to be stored")
	}
	wt := db.storedTx
	if wt.ID != txHash.String() {
		t.Fatalf("wrong ID: %s", wt.ID)
	}
	if wt.AssetID != usdcEthID {
		t.Fatalf("wrong AssetID: %+v", wt.AssetID)
	}
	if wt.Amount != 500 {
		t.Fatalf("wrong amount: %d", wt.Amount)
	}
	if wt.Type != txdb.Receive {
		t.Fatalf("wrong type: %v", wt.Type)
	}
}

func TestScanERC20LogNotForUs(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	// A log from an address that isn't any connected token contract.
	eth.node.(*tMempoolNode).filterLogsResult = []types.Log{{
		Address:     common.HexToAddress("0xdeadbeef"),
		TxHash:      common.HexToHash("0x02"),
		BlockNumber: 100,
	}}

	if err := eth.scanERC20Receives(context.Background(), 90, 100, []*assetWallet{tokenAw}); err != nil {
		t.Fatalf("scanERC20Receives: %v", err)
	}

	db := eth.txDB.(*tTxDB)
	if db.storeTxCalled {
		t.Fatalf("did not expect a tx to be stored")
	}
}

func TestScanERC20Dedup(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	txHash := common.HexToHash("0x03")
	eth.node.(*tMempoolNode).filterLogsResult = []types.Log{{
		Address:     tokenAw.tokenAddr,
		TxHash:      txHash,
		BlockNumber: 100,
	}}

	db := eth.txDB.(*tTxDB)
	db.txs = map[common.Hash]*extendedWalletTx{
		txHash: {WalletTransaction: &txdb.WalletTransaction{ID: txHash.String()}},
	}

	if err := eth.scanERC20Receives(context.Background(), 90, 100, []*assetWallet{tokenAw}); err != nil {
		t.Fatalf("scanERC20Receives: %v", err)
	}
	if db.storeTxCalled {
		t.Fatalf("did not expect a duplicate tx to be stored")
	}
}

// TestScanERC20FailureDoesNotAdvanceCursor is the regression test for the
// cascading-failure bug: an ERC20 scan error must not advance the ERC20
// cursor.
func TestScanERC20FailureDoesNotAdvanceCursor(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(50); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(60)}
	eth.node.(*tMempoolNode).filterLogsErr = context.DeadlineExceeded
	_ = tokenAw

	eth.scanIncomingTransfersTick(context.Background())

	erc20Last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if erc20Last != 50 {
		t.Fatalf("expected erc20 cursor to stay at 50 after a scan error, got %d", erc20Last)
	}
}

// TestScanERC20BackoffPreventsHammering guards against spamming a provider
// (and the log) every incomingScanTick when the ERC20 scan is persistently
// failing - e.g. a free public RPC provider that doesn't serve eth_getLogs
// at all, which will never succeed no matter how soon it's retried.
func TestScanERC20BackoffPreventsHammering(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()
	_ = tokenAw

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(50); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(60)}
	node := eth.node.(*tMempoolNode)
	node.filterLogsErr = context.DeadlineExceeded

	eth.scanIncomingTransfersTick(context.Background())
	if node.filterLogsCalls != 1 {
		t.Fatalf("expected 1 filterLogs call after the first failing tick, got %d", node.filterLogsCalls)
	}

	// A second tick immediately after must not retry: the backoff engaged.
	eth.scanIncomingTransfersTick(context.Background())
	if node.filterLogsCalls != 1 {
		t.Fatalf("expected the backoff to skip a second immediate retry, got %d total filterLogs calls", node.filterLogsCalls)
	}

	// Once the backoff expires and the provider recovers, scanning resumes.
	eth.erc20ScanBackoff.until = time.Time{}
	node.filterLogsErr = nil
	node.filterLogsResult = nil
	eth.scanIncomingTransfersTick(context.Background())
	if node.filterLogsCalls != 2 {
		t.Fatalf("expected scanning to resume after the backoff expired, got %d total filterLogs calls", node.filterLogsCalls)
	}
	// The scan only ever advances to tip - finalizeConfs, not the raw tip.
	safeTip := uint64(60) - eth.finalizeConfs
	erc20Last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if erc20Last != safeTip {
		t.Fatalf("expected erc20 cursor to advance to tip-finalizeConfs %d after recovering, got %d", safeTip, erc20Last)
	}
}

func TestScanFirstRunSeedsCursorAtTip(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	eth.currentTip = &types.Header{Number: big.NewInt(1000)}

	eth.scanIncomingTransfersTick(context.Background())

	// Cursor seeds at tip - finalizeConfs, not the raw tip.
	safeTip := uint64(1000) - eth.finalizeConfs
	erc20Last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if erc20Last != safeTip {
		t.Fatalf("expected erc20 cursor to seed at tip-finalizeConfs %d, got %d", safeTip, erc20Last)
	}
	node := eth.node.(*tMempoolNode)
	if node.filterLogsCalls != 0 {
		t.Fatalf("expected no scanning on first run, got %d filter calls", node.filterLogsCalls)
	}
}

// TestScanTokenReceiveStoreFailureWithholdsCursor is the regression test for
// a real bug: a per-item txDB store failure was only logged, and the
// enclosing scan still reported success - clearing the backoff and
// persisting the cursor past the range - permanently losing that receive
// even though the failure was likely transient.
func TestScanTokenReceiveStoreFailureWithholdsCursor(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(90); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(100)}

	txHash := common.HexToHash("0x01")
	node := eth.node.(*tMempoolNode)
	node.filterLogsResult = []types.Log{{
		Address:     tokenAw.tokenAddr,
		TxHash:      txHash,
		BlockNumber: 95,
	}}
	tokenAw.tokenContractor.(*tTokenContractor).parseTransferAmt = 500

	db := eth.txDB.(*tTxDB)
	db.storeTxErr = context.DeadlineExceeded

	eth.scanIncomingTransfersTick(context.Background())

	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 90 {
		t.Fatalf("expected erc20 cursor to stay at 90 after a store failure, got %d", last)
	}
	if !eth.erc20ScanBackoff.blocked(time.Now()) {
		t.Fatal("expected the erc20 backoff to engage after a store failure")
	}
}

// TestScanERC20MultiTokenSameTxNotDropped is the regression test for a real
// bug: dedup was keyed on tx hash alone, so a single transaction delivering
// two different tokens to our address (an aggregator payout, a multisend)
// recorded only the first - the second looked like a duplicate of the first
// and was silently skipped.
func TestScanERC20MultiTokenSameTxNotDropped(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	otherToken := common.HexToAddress("0xbeef")
	otherAssetID := uint32(60999)
	otherAw := &assetWallet{
		baseWallet:      eth.baseWallet,
		assetID:         otherAssetID,
		tokenAddr:       otherToken,
		tokenContractor: &tTokenContractor{parseTransferAmt: 700, bal: big.NewInt(0)},
		log:             tokenAw.log,
		emit:            tokenAw.emit,
		atomize:         tokenAw.atomize,
		evmify:          tokenAw.evmify,
	}
	otherAw.connected.Store(true)
	eth.walletsMtx.Lock()
	eth.wallets[otherAssetID] = otherAw
	eth.walletsMtx.Unlock()

	txHash := common.HexToHash("0x77")
	eth.node.(*tMempoolNode).filterLogsResult = []types.Log{
		{Address: tokenAw.tokenAddr, TxHash: txHash, BlockNumber: 100, Index: 3},
		{Address: otherToken, TxHash: txHash, BlockNumber: 100, Index: 5},
	}
	tokenAw.tokenContractor.(*tTokenContractor).parseTransferAmt = 500

	if err := eth.scanERC20Receives(context.Background(), 90, 100, []*assetWallet{tokenAw, otherAw}); err != nil {
		t.Fatalf("scanERC20Receives: %v", err)
	}

	db := eth.txDB.(*tTxDB)
	if len(db.storedTxs) != 2 {
		t.Fatalf("expected 2 receives stored (one per token), got %d", len(db.storedTxs))
	}
	amounts := map[uint64]bool{}
	for _, wt := range db.storedTxs {
		amounts[wt.Amount] = true
	}
	if !amounts[500] || !amounts[700] {
		t.Fatalf("expected both amounts 500 and 700 stored, got %+v", db.storedTxs)
	}
}

// TestScanERC20TickCatchesUpNormallyWithinWindow is the baseline case: a
// wallet that fell behind by less than maxScanBlocks must advance its cursor
// from right where it left off, requesting only up to tip.
func TestScanERC20TickCatchesUpNormallyWithinWindow(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(10); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	const tip = 10 + maxScanBlocks/2
	eth.currentTip = &types.Header{Number: big.NewInt(tip)}

	node := eth.node.(*tMempoolNode)
	eth.scanERC20Tick(context.Background(), tip, []*assetWallet{tokenAw})

	if node.filterLogsQuery.FromBlock.Uint64() != 11 {
		t.Fatalf("expected the scan to resume right after the cursor at 11, got %d", node.filterLogsQuery.FromBlock.Uint64())
	}
	if node.filterLogsQuery.ToBlock.Uint64() != tip {
		t.Fatalf("expected the scan to request up to tip %d, got %d", tip, node.filterLogsQuery.ToBlock.Uint64())
	}
	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != tip {
		t.Fatalf("expected the cursor to advance to tip %d, got %d", tip, last)
	}
}

// TestScanERC20TickYoungChainWithinWindow covers a tip below maxScanBlocks
// (e.g. simnet, or a chain shortly after genesis): the oldest-scannable-block
// floor stays pinned at the block-1 sentinel rather than computing
// tipNum-maxScanBlocks+1 (which would go negative), so the cursor still
// resumes normally instead of clamping.
func TestScanERC20TickYoungChainWithinWindow(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(1); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	const tip = 50 // well below maxScanBlocks
	eth.currentTip = &types.Header{Number: big.NewInt(tip)}

	node := eth.node.(*tMempoolNode)
	eth.scanERC20Tick(context.Background(), tip, []*assetWallet{tokenAw})

	if node.filterLogsQuery.FromBlock.Uint64() != 2 {
		t.Fatalf("expected the scan to resume right after the cursor at 2, got %d", node.filterLogsQuery.FromBlock.Uint64())
	}
	if node.filterLogsQuery.ToBlock.Uint64() != tip {
		t.Fatalf("expected the scan to request up to tip %d, got %d", tip, node.filterLogsQuery.ToBlock.Uint64())
	}
}

// TestScanERC20TickClampsToRecentWindow is the regression test for the
// unscannable-backlog wedge Buck reported against real providers (PR #173):
// providers phrase "this range can no longer be served" in too many ways to
// classify reliably, so rather than trying to detect that from the error, a
// wallet that fell further behind than maxScanBlocks must have that gap
// silently given up on every tick - clamping to the most recent
// maxScanBlocks blocks - instead of retrying the same unreachable range
// forever.
func TestScanERC20TickClampsToRecentWindow(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(10); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	const farTip = 10 + 100*maxScanBlocks
	eth.currentTip = &types.Header{Number: big.NewInt(farTip)}

	node := eth.node.(*tMempoolNode)
	eth.scanERC20Tick(context.Background(), farTip, []*assetWallet{tokenAw})

	wantFrom := uint64(farTip - maxScanBlocks + 1)
	if node.filterLogsQuery.FromBlock.Uint64() != wantFrom {
		t.Fatalf("expected the scan to clamp its from-block to %d, got %d", wantFrom, node.filterLogsQuery.FromBlock.Uint64())
	}
	if node.filterLogsQuery.ToBlock.Uint64() != farTip {
		t.Fatalf("expected the scan to request up to tip %d, got %d", farTip, node.filterLogsQuery.ToBlock.Uint64())
	}
	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != farTip {
		t.Fatalf("expected the cursor to advance straight to tip %d, got %d", farTip, last)
	}
}

// TestScanERC20TickAtExactWindowBoundaryDoesNotClamp pins the clamp
// threshold from the non-clamping side: a cursor exactly maxScanBlocks
// behind tip must still scan the full window normally, not trigger the
// abandon-the-backlog path.
func TestScanERC20TickAtExactWindowBoundaryDoesNotClamp(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	const tip = 1000
	last := uint64(tip - maxScanBlocks) // exactly maxScanBlocks behind
	if err := eth.setLastScannedERC20Block(last); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(tip)}

	node := eth.node.(*tMempoolNode)
	eth.scanERC20Tick(context.Background(), tip, []*assetWallet{tokenAw})

	wantFrom := last + 1
	if node.filterLogsQuery.FromBlock.Uint64() != wantFrom {
		t.Fatalf("expected no clamp at exactly maxScanBlocks behind, from-block %d, got %d", wantFrom, node.filterLogsQuery.FromBlock.Uint64())
	}
	if node.filterLogsQuery.ToBlock.Uint64() != tip {
		t.Fatalf("expected the scan to request up to tip %d, got %d", tip, node.filterLogsQuery.ToBlock.Uint64())
	}
}

// TestScanERC20TickOneBlockBeyondWindowClamps is the counterpart of
// TestScanERC20TickAtExactWindowBoundaryDoesNotClamp from the clamping side:
// a cursor just one block further behind must abandon exactly that one
// block, not the whole window nor none of it - pinning the clamp threshold
// against an off-by-one in either direction.
func TestScanERC20TickOneBlockBeyondWindowClamps(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	const tip = 1000
	last := uint64(tip - maxScanBlocks - 1) // one block past the boundary
	if err := eth.setLastScannedERC20Block(last); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(tip)}

	node := eth.node.(*tMempoolNode)
	eth.scanERC20Tick(context.Background(), tip, []*assetWallet{tokenAw})

	wantFrom := uint64(tip - maxScanBlocks + 1)
	if node.filterLogsQuery.FromBlock.Uint64() != wantFrom {
		t.Fatalf("expected the clamp to abandon exactly one block, from-block %d, got %d", wantFrom, node.filterLogsQuery.FromBlock.Uint64())
	}
	if node.filterLogsQuery.ToBlock.Uint64() != tip {
		t.Fatalf("expected the scan to request up to tip %d, got %d", tip, node.filterLogsQuery.ToBlock.Uint64())
	}
}

// TestScanERC20TickOnErrorDoesNotAdvanceOrSkip is the counterpart to
// TestScanERC20FailureDoesNotAdvanceCursor for a wallet that has fallen far
// behind: a scan error must not silently advance the cursor - only a
// successful scan does that. On error the cursor stays put and the ordinary
// backoff applies, so the next tick retries.
func TestScanERC20TickOnErrorDoesNotAdvanceOrSkip(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(10); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	const farTip = 10 + 200*maxScanBlocks
	eth.currentTip = &types.Header{Number: big.NewInt(farTip)}

	node := eth.node.(*tMempoolNode)
	node.filterLogsErr = errors.New("archive requests require a personal token")

	eth.scanERC20Tick(context.Background(), farTip, []*assetWallet{tokenAw})

	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 10 {
		t.Fatalf("expected the cursor to stay at 10 after a scan error, got %d", last)
	}
	if !eth.erc20ScanBackoff.blocked(time.Now()) {
		t.Fatal("expected the erc20 backoff to engage after the error")
	}
}

// TestScanERC20ReceiveSetsTimestampAndNote covers two real bugs together: a
// detected receive was stored with Timestamp 0 (a sentinel the frontend
// treats as "pending, sort to top"), and no transaction note was emitted, so
// the receive didn't show in the activity list until reload.
func TestScanERC20ReceiveSetsTimestampAndNote(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	notes := make(chan asset.WalletNotification, 4)
	eth.emit = asset.NewWalletEmitter(notes, BipID, tLogger)
	eth.wallets[BipID] = eth.assetWallet
	tokenAw.emit = asset.NewWalletEmitter(notes, tokenAw.assetID, tLogger)

	blockHash := common.HexToHash("0xaa")
	const blockTime = 1_700_000_000
	node := eth.node.(*tMempoolNode)
	node.filterLogsResult = []types.Log{{
		Address:     tokenAw.tokenAddr,
		TxHash:      common.HexToHash("0x01"),
		BlockNumber: 100,
		BlockHash:   blockHash,
	}}
	node.hdrByHash[blockHash] = &types.Header{Time: blockTime}
	tokenAw.tokenContractor.(*tTokenContractor).parseTransferAmt = 500

	if err := eth.scanERC20Receives(context.Background(), 90, 100, []*assetWallet{tokenAw}); err != nil {
		t.Fatalf("scanERC20Receives: %v", err)
	}

	db := eth.txDB.(*tTxDB)
	if !db.storeTxCalled {
		t.Fatal("expected a tx to be stored")
	}
	if db.storedTx.Timestamp != blockTime {
		t.Fatalf("expected Timestamp %d, got %d", uint64(blockTime), db.storedTx.Timestamp)
	}

	select {
	case n := <-notes:
		if _, ok := n.(*asset.TransactionNote); !ok {
			t.Fatalf("expected a TransactionNote, got %T", n)
		}
	default:
		t.Fatal("expected a transaction note to be emitted")
	}
}
