//go:build !harness && !rpclive

package eth

import (
	"context"
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

// TestScanERC20FailureDoesNotAdvanceCursorOrBlockNative is the regression
// test for the cascading-failure bug: an ERC20 scan error must not advance
// the ERC20 cursor, and — critically — must not prevent the independent
// native scan from running and progressing on its own cursor. Before the
// native/ERC20 cursors and backoffs were split, a permanently-failing ERC20
// scan (e.g. a free public RPC provider that doesn't serve eth_getLogs at
// all) silently blocked native-transfer detection forever too.
func TestScanERC20FailureDoesNotAdvanceCursorOrBlockNative(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	// Seed only the ERC20 cursor so its tick doesn't take the first-run
	// seeding path; the native cursor is deliberately left unseeded so its
	// own first run is what proves it wasn't blocked by the ERC20 failure.
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

	// Cursors seed at tip - finalizeConfs, not the raw tip: scanning any
	// closer to the tip would let a reorg leave a phantom Receive behind.
	safeTip := uint64(60) - eth.finalizeConfs
	nativeLast, err := eth.getLastScannedBlock()
	if err != nil {
		t.Fatalf("getLastScannedBlock: %v", err)
	}
	if nativeLast != safeTip {
		t.Fatalf("expected native cursor to seed at tip-finalizeConfs %d despite the erc20 failure, got %d", safeTip, nativeLast)
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

	// Cursors seed at tip - finalizeConfs, not the raw tip.
	safeTip := uint64(1000) - eth.finalizeConfs
	last, err := eth.getLastScannedBlock()
	if err != nil {
		t.Fatalf("getLastScannedBlock: %v", err)
	}
	if last != safeTip {
		t.Fatalf("expected native cursor to seed at tip-finalizeConfs %d, got %d", safeTip, last)
	}
	erc20Last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if erc20Last != safeTip {
		t.Fatalf("expected erc20 cursor to seed at tip-finalizeConfs %d, got %d", safeTip, erc20Last)
	}
	node := eth.node.(*tMempoolNode)
	if node.filterLogsCalls != 0 || node.blockByNumCalls != 0 {
		t.Fatalf("expected no scanning on first run, got %d filter calls and %d block calls",
			node.filterLogsCalls, node.blockByNumCalls)
	}
}

func TestScanNativeReceive(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	toAddr := eth.addr
	// newTransaction (used elsewhere in this test suite) always sends to a
	// random address, so build a tx targeting our own address directly.
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		Nonce:   1,
		Value:   big.NewInt(1000),
		Gas:     50_000,
		To:      &toAddr,
		ChainID: eth.node.chainConfig().ChainID,
	}), signer, eth.node.(*tMempoolNode).privKey)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}

	header := &types.Header{Number: big.NewInt(5)}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: []*types.Transaction{tx}})
	eth.node.(*tMempoolNode).blocks = map[uint64]*types.Block{5: block}
	eth.node.(*tMempoolNode).bal = big.NewInt(9999)

	if covered, err := eth.scanNativeReceives(context.Background(), 5, 5); err != nil {
		t.Fatalf("scanNativeReceives: %v", err)
	} else if covered != 5 {
		t.Fatalf("expected covered = 5, got %d", covered)
	}

	db := eth.txDB.(*tTxDB)
	if !db.storeTxCalled {
		t.Fatalf("expected a tx to be stored")
	}
	if db.storedTx.ID != tx.Hash().String() {
		t.Fatalf("wrong ID: %s", db.storedTx.ID)
	}
	if db.storedTx.AssetID != BipID {
		t.Fatalf("expected the base chain's AssetID for a native receive, got %d", db.storedTx.AssetID)
	}
}

func TestScanNativeBalanceUnchangedSkipsBodies(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	node := eth.node.(*tMempoolNode)
	node.bal = big.NewInt(1000) // constant, so addressBalanceAt returns 1000 for any block via the fallback
	node.blocks = map[uint64]*types.Block{
		1: types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)}),
	}

	// First call: no prior balance recorded, so this always proceeds and
	// fetches the block body even though there happens to be no transfer.
	if _, err := eth.scanNativeReceives(context.Background(), 1, 1); err != nil {
		t.Fatalf("scanNativeReceives (first): %v", err)
	}
	if node.blockByNumCalls != 1 {
		t.Fatalf("expected 1 block fetch on first call, got %d", node.blockByNumCalls)
	}

	if covered, err := eth.scanNativeReceives(context.Background(), 2, 2); err != nil {
		t.Fatalf("scanNativeReceives (second): %v", err)
	} else if covered != 2 {
		t.Fatalf("expected covered = 2 (balance-unchanged range is fully covered), got %d", covered)
	}
	if node.blockByNumCalls != 1 {
		t.Fatalf("expected no additional block fetch when balance is unchanged, got %d total", node.blockByNumCalls)
	}
}

func TestScanNativeRangeCap(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	node := eth.node.(*tMempoolNode)
	node.bal = big.NewInt(1000)
	eth.currentTip = &types.Header{Number: big.NewInt(1000)}

	// The truncated range is head-biased (oldest blocks of [from,to] first,
	// leaving the newest for a following tick) so that a wallet catching up
	// makes steady, non-skipping progress across ticks - see scan.go's
	// scanNativeReceives doc comment.
	node.blocks = make(map[uint64]*types.Block)
	for n := uint64(1); n <= maxScanBodies; n++ {
		node.blocks[n] = types.NewBlockWithHeader(&types.Header{Number: new(big.Int).SetUint64(n)})
	}

	covered, err := eth.scanNativeReceives(context.Background(), 1, 1000)
	if err != nil {
		t.Fatalf("scanNativeReceives: %v", err)
	}
	if covered != maxScanBodies {
		t.Fatalf("expected covered = %d (range capped, oldest-first), got %d", maxScanBodies, covered)
	}
	if node.blockByNumCalls != maxScanBodies {
		t.Fatalf("expected %d block fetches (range capped), got %d", maxScanBodies, node.blockByNumCalls)
	}
}

// TestScanNativeBalancePinnedToRangeEnd is the regression test for a real
// bug: the balance-diff prefilter used to read the node's "latest" balance,
// which can already reflect blocks past the range about to be scanned on a
// fast chain - making an in-range receive look like it happened before the
// comparison baseline and get silently skipped. The balance must be read
// pinned to the range's own end (to), not "latest".
func TestScanNativeBalancePinnedToRangeEnd(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	toAddr := eth.addr
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		Nonce: 1, Value: big.NewInt(500), Gas: 50_000, To: &toAddr,
		ChainID: eth.node.chainConfig().ChainID,
	}), signer, eth.node.(*tMempoolNode).privKey)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(5)}).
		WithBody(types.Body{Transactions: []*types.Transaction{tx}})

	node := eth.node.(*tMempoolNode)
	node.blocks = map[uint64]*types.Block{5: block}
	// "latest" (the fallback used when balAt has no entry for the queried
	// block) is way ahead, as if the node's tip has already moved past the
	// range being scanned - a live possibility on a fast chain. If the scan
	// used "latest" instead of the balance at block 5, it would look
	// unchanged relative to a later observation and skip fetching block 5's
	// body entirely, missing the receive.
	node.bal = big.NewInt(999_999)
	node.balAt = map[uint64]*big.Int{5: big.NewInt(500)}
	eth.lastScanBalance = big.NewInt(0)

	covered, err := eth.scanNativeReceives(context.Background(), 5, 5)
	if err != nil {
		t.Fatalf("scanNativeReceives: %v", err)
	}
	if covered != 5 {
		t.Fatalf("expected covered = 5, got %d", covered)
	}
	if node.blockByNumCalls != 1 {
		t.Fatalf("expected the block to be fetched (balance pinned to block 5 differs from lastScanBalance), got %d fetches", node.blockByNumCalls)
	}
	db := eth.txDB.(*tTxDB)
	if !db.storeTxCalled {
		t.Fatal("expected the receive to be stored")
	}
}

// TestScanNativeTickWithholdsCursorOnFailure is the regression test for a
// real bug: scanNativeTick used to persist the cursor at the full requested
// range end regardless of what scanNativeReceives actually covered, so a
// failure partway through a range silently and permanently dropped the
// unscanned tail.
func TestScanNativeTickWithholdsCursorOnFailure(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedBlock(10); err != nil {
		t.Fatalf("setLastScannedBlock: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(20)}

	node := eth.node.(*tMempoolNode)
	node.bal = big.NewInt(1000)
	eth.lastScanBalance = big.NewInt(0) // force the "changed" path so blocks are actually fetched
	// Blocks 11..13 exist; 14 does not, so the fetch loop fails there.
	node.blocks = map[uint64]*types.Block{
		11: types.NewBlockWithHeader(&types.Header{Number: big.NewInt(11)}),
		12: types.NewBlockWithHeader(&types.Header{Number: big.NewInt(12)}),
		13: types.NewBlockWithHeader(&types.Header{Number: big.NewInt(13)}),
	}

	eth.scanNativeTick(context.Background(), 20)

	last, err := eth.getLastScannedBlock()
	if err != nil {
		t.Fatalf("getLastScannedBlock: %v", err)
	}
	if last != 13 {
		t.Fatalf("expected cursor to persist at 13 (the last fully-scanned block), got %d", last)
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

// TestScanNativeBaselinePinnedToScanTo is the regression test for a real bug:
// the balance-diff baseline used to be read at the requested range end (to),
// but when the range is capped by maxScanBodies only blocks up to scanTo are
// actually looked at. Pinning the baseline to to let it silently absorb a
// balance change from a block this call never scanned.
func TestScanNativeBaselinePinnedToScanTo(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	node := eth.node.(*tMempoolNode)
	node.blocks = make(map[uint64]*types.Block)
	for n := uint64(1); n <= maxScanBodies; n++ {
		node.blocks[n] = types.NewBlockWithHeader(&types.Header{Number: new(big.Int).SetUint64(n)})
	}
	node.balAt = map[uint64]*big.Int{
		maxScanBodies: big.NewInt(1000), // scanTo: the block actually covered
		1000:          big.NewInt(1500), // to: the requested (uncovered) range end
	}

	covered, err := eth.scanNativeReceives(context.Background(), 1, 1000)
	if err != nil {
		t.Fatalf("scanNativeReceives: %v", err)
	}
	if covered != maxScanBodies {
		t.Fatalf("expected covered = %d, got %d", maxScanBodies, covered)
	}
	if eth.lastScanBalance == nil || eth.lastScanBalance.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("expected baseline pinned to the balance at scanTo (1000), got %v", eth.lastScanBalance)
	}
}

// TestScanNativeUnchangedCoversFullRequestedRange is the regression test for
// a real bug introduced while fixing the baseline-pinning issue above: the
// unchanged-balance fast path was accidentally capped to scanTo (at most
// maxScanBodies blocks) instead of covering the full requested range - an
// unchanged balance at to proves nothing arrived anywhere in from..to, so
// the whole range should be retired in one tick without fetching any
// bodies, even when it's wider than maxScanBodies.
func TestScanNativeUnchangedCoversFullRequestedRange(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	node := eth.node.(*tMempoolNode)
	node.bal = big.NewInt(1000) // constant, so addressBalanceAt returns 1000 for any block
	eth.lastScanBalance = big.NewInt(1000)

	const to = 1000 // far wider than maxScanBodies
	covered, err := eth.scanNativeReceives(context.Background(), 1, to)
	if err != nil {
		t.Fatalf("scanNativeReceives: %v", err)
	}
	if covered != to {
		t.Fatalf("expected the unchanged range to be fully retired at %d, got %d", to, covered)
	}
	if node.blockByNumCalls != 0 {
		t.Fatalf("expected no block fetches on the unchanged path, got %d", node.blockByNumCalls)
	}
}

// TestScanNativeConvergesAcrossTicks is the regression test for the
// permanent-loss bug: with the baseline pinned to the uncovered range end, a
// receive in the truncated tail would look "unchanged" on the following tick
// (since the tip balance had already been absorbed into the first tick's
// baseline) and be skipped forever instead of being picked up once the
// cursor reaches it.
func TestScanNativeConvergesAcrossTicks(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	toAddr := eth.addr
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		Nonce: 1, Value: big.NewInt(500), Gas: 50_000, To: &toAddr,
		ChainID: eth.node.chainConfig().ChainID,
	}), signer, eth.node.(*tMempoolNode).privKey)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}

	node := eth.node.(*tMempoolNode)
	node.blocks = make(map[uint64]*types.Block)
	for n := uint64(1); n <= 2*maxScanBodies; n++ {
		node.blocks[n] = types.NewBlockWithHeader(&types.Header{Number: new(big.Int).SetUint64(n)})
	}
	// The receive lands in the second tick's range (17-32), not the first
	// (1-16).
	const receiveBlock = 20
	node.blocks[receiveBlock] = types.NewBlockWithHeader(&types.Header{Number: big.NewInt(receiveBlock)}).
		WithBody(types.Body{Transactions: []*types.Transaction{tx}})
	node.balAt = map[uint64]*big.Int{
		maxScanBodies:     big.NewInt(1000), // scanTo of tick 1: balance before the receive
		2 * maxScanBodies: big.NewInt(1500), // scanTo of tick 2: balance after the receive
	}
	node.bal = big.NewInt(1500) // fallback used by w.balance() after storing a receive

	// Tick 1: covers 1-16, no receive there.
	covered, err := eth.scanNativeReceives(context.Background(), 1, 1000)
	if err != nil {
		t.Fatalf("scanNativeReceives (tick 1): %v", err)
	}
	if covered != maxScanBodies {
		t.Fatalf("expected tick 1 to cover %d, got %d", maxScanBodies, covered)
	}

	// Tick 2: covers 17-32, where the receive is.
	covered, err = eth.scanNativeReceives(context.Background(), covered+1, 1000)
	if err != nil {
		t.Fatalf("scanNativeReceives (tick 2): %v", err)
	}
	if covered != 2*maxScanBodies {
		t.Fatalf("expected tick 2 to cover %d, got %d", 2*maxScanBodies, covered)
	}

	db := eth.txDB.(*tTxDB)
	if !db.storeTxCalled {
		t.Fatal("expected the receive in the truncated tail to be found on the following tick")
	}
	if db.storedTx.ID != tx.Hash().String() {
		t.Fatalf("wrong ID: %s", db.storedTx.ID)
	}
}

// TestScanNativeBodyFetchFailureClearsBaseline is the regression test for a
// real bug: on a body-fetch failure partway through a range, the baseline had
// already been advanced to the balance at scanTo even though the cursor
// rewinds to before it - so a later tick could compare against a baseline
// that already accounts for blocks it hasn't actually scanned yet.
func TestScanNativeBodyFetchFailureClearsBaseline(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	node := eth.node.(*tMempoolNode)
	node.bal = big.NewInt(1000)
	eth.lastScanBalance = big.NewInt(0) // force the "changed" path
	// Blocks 11..13 exist; 14 does not, so the fetch loop fails there.
	node.blocks = map[uint64]*types.Block{
		11: types.NewBlockWithHeader(&types.Header{Number: big.NewInt(11)}),
		12: types.NewBlockWithHeader(&types.Header{Number: big.NewInt(12)}),
		13: types.NewBlockWithHeader(&types.Header{Number: big.NewInt(13)}),
	}

	covered, err := eth.scanNativeReceives(context.Background(), 11, 20)
	if err == nil {
		t.Fatal("expected an error from the missing block")
	}
	if covered != 13 {
		t.Fatalf("expected covered = 13, got %d", covered)
	}
	if eth.lastScanBalance != nil {
		t.Fatalf("expected the baseline to be cleared on a body-fetch failure, got %v", eth.lastScanBalance)
	}
}

// TestScanNativeReceiveSetsTimestampAndNote covers two real bugs together:
// a detected receive was stored with Timestamp 0 (a sentinel the frontend
// treats as "pending, sort to top"), and no transaction note was emitted, so
// the receive didn't show in the activity list until reload.
func TestScanNativeReceiveSetsTimestampAndNote(t *testing.T) {
	t.Parallel()
	eth, _, cancel := newScanTestWallets(t)
	defer cancel()

	notes := make(chan asset.WalletNotification, 4)
	eth.emit = asset.NewWalletEmitter(notes, BipID, tLogger)
	eth.wallets[BipID] = eth.assetWallet

	toAddr := eth.addr
	const blockTime = 1_700_000_000
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		Nonce: 1, Value: big.NewInt(1000), Gas: 50_000, To: &toAddr,
		ChainID: eth.node.chainConfig().ChainID,
	}), signer, eth.node.(*tMempoolNode).privKey)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}
	header := &types.Header{Number: big.NewInt(5), Time: blockTime}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: []*types.Transaction{tx}})
	node := eth.node.(*tMempoolNode)
	node.blocks = map[uint64]*types.Block{5: block}
	node.bal = big.NewInt(9999)

	if _, err := eth.scanNativeReceives(context.Background(), 5, 5); err != nil {
		t.Fatalf("scanNativeReceives: %v", err)
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

// TestScanERC20ReceiveSetsTimestampAndNote is the token-path counterpart of
// TestScanNativeReceiveSetsTimestampAndNote.
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
