//go:build !harness && !rpclive

package eth

import (
	"context"
	"errors"
	"fmt"
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

func TestIsUnscannableRangeErr(t *testing.T) {
	t.Parallel()
	pruned := errors.New("missing trie node: header not found: state at block #117666854 is pruned")
	prunedOtherPhrasing := errors.New("state is not available for the requested block")
	transient := errors.New("429 Too Many Requests")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"pruned, clause order 1", errors.New("missing trie node: header not found: state at block #117666854 is pruned"), true},
		{"pruned, clause order 2", errors.New("not supported: header not found: state at block #117666854 is pruned: missing trie node"), true},
		{"pruned, clause order 3", errors.New("state at block #117666854 is pruned: not supported: missing trie node"), true},
		{"bnb erc20 pruned-history phrasing", errors.New("History has been pruned for this block. To remove restrictions, order a dedicated full node here: https://www.allnodes.com/bnb/host: limit exceeded: limit exceeded"), true},
		{
			"joined multi-provider error where every provider is pruned",
			fmt.Errorf("%w: %w", pruned, prunedOtherPhrasing),
			true, // every leaf error is pruned, so the joined error is too
		},
		{
			"joined multi-provider error mixing pruned and transient",
			fmt.Errorf("%w: %w", pruned, transient),
			// The transient leaf is ignored, not treated as proof the whole
			// failure is recoverable - a provider that fails the same way on
			// every call (e.g. a free endpoint that 429s forever) would
			// otherwise always contribute a transient-looking leaf and wedge
			// the scan permanently. The pruned leaf is actionable on its own.
			true,
		},
		{
			"joined multi-provider error mixing pruned and range-too-wide",
			fmt.Errorf("%w: %w", pruned, errors.New("eth_getLogs is limited to 0 - 50 blocks range")),
			// The range-capped provider may still serve a narrower request,
			// so this is not unscannable - it's isRangeTooWideErr instead
			// (see TestIsRangeTooWideErr).
			false,
		},
		{"bare header not found (lagging provider, not pruned)", errors.New("header not found"), false},
		{"bare rate limit", errors.New("429 Too Many Requests"), false},
		{"bare limit exceeded", errors.New("limit exceeded"), false},
		{"connection reset", errors.New("read tcp: connection reset by peer"), false},
		{"context deadline", context.DeadlineExceeded, false},
		{"nil", nil, false},
		{
			"joined multi-provider error where no leaf is actionable",
			fmt.Errorf("%w: %w", transient, errors.New("read tcp: connection reset by peer")),
			// No leaf is pruned or too-wide, so the joined failure stays
			// transient - pins the default arm of classifyRangeErr, which no
			// other case here reaches (every other transient case is a
			// single, non-joined leaf).
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isUnscannableRangeErr(tt.err); got != tt.want {
				t.Errorf("isUnscannableRangeErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsRangeTooWideErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bnb provider message", errors.New("eth_getLogs is limited to 0 - 50 blocks range: limit exceeded"), true},
		{"eth provider message, different phrasing", errors.New(`400 Bad Request: {"jsonrpc":"2.0","id":31,"error":{"code":-32600,"message":"You can make eth_getLogs requests with up to a 10 block range. Based on your parameters, this block range should work: [0x18c746a, 0x18c7473]"}}`), true},
		{"bare limit exceeded (rate limit, not a range cap)", errors.New("limit exceeded"), false},
		{"bare rate limit", errors.New("429 Too Many Requests"), false},
		{"too many requests, no range mentioned", errors.New("too many requests, please slow down"), false},
		{"connection reset", errors.New("read tcp: connection reset by peer"), false},
		{"nil", nil, false},
		{
			"joined multi-provider error mixing pruned and range-too-wide",
			fmt.Errorf("%w: %w", errors.New("missing trie node: state at block #20 is pruned"), errors.New("eth_getLogs is limited to 0 - 50 blocks range")),
			// Narrowing might unblock the range-capped provider and can't
			// hurt the pruned one, so this is a range-too-wide failure - the
			// direct regression pin for the F1 classifier bug (this used to
			// return false, wedging the scan on neither classification).
			true,
		},
		{
			"joined range-too-wide mixed with transient",
			fmt.Errorf("%w: %w", errors.New("eth_getLogs is limited to 0 - 50 blocks range"), errors.New("429 Too Many Requests")),
			// The transient leaf is ignored; the too-wide leaf is actionable
			// on its own, for the same reason as the pruned+transient case in
			// TestIsUnscannableRangeErr.
			true,
		},
		{
			"single message carrying both pruned and range-cap phrasing",
			errors.New("History has been pruned for this block: eth_getLogs is limited to 0 - 50 blocks range"),
			// A single (non-joined) provider message is one leaf: pruned is
			// checked before too-wide, so this stays classified pruned, not
			// too-wide - see TestScanERC20JoinedPrunedAndRangeTooWideSkipsNotNarrows.
			false,
		},
		{
			"joined multi-provider error where no leaf is actionable",
			fmt.Errorf("%w: %w", errors.New("429 Too Many Requests"), errors.New("read tcp: connection reset by peer")),
			// No leaf is pruned or too-wide, so the joined failure is not a
			// range-too-wide failure either - pins the default arm of
			// classifyRangeErr from the too-wide side.
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isRangeTooWideErr(tt.err); got != tt.want {
				t.Errorf("isRangeTooWideErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestScanERC20TickSkipsPrunedRange is the regression test for the
// permanent-stall bug: a range that fails forever with a pruned-state error
// must eventually be skipped so the cursor advances and future ticks make
// progress, instead of retrying the identical unscannable range forever.
func TestScanERC20TickSkipsPrunedRange(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(10); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(20)}

	node := eth.node.(*tMempoolNode)
	node.filterLogsErr = errors.New("missing trie node: state at block #20 is pruned")

	for range scanSkipFailures - 1 {
		eth.erc20ScanBackoff.until = time.Time{} // force each tick past the backoff
		eth.scanERC20Tick(context.Background(), 20, []*assetWallet{tokenAw})
	}
	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 10 {
		t.Fatalf("expected cursor to stay at 10 before the skip threshold, got %d", last)
	}

	eth.erc20ScanBackoff.until = time.Time{}
	eth.scanERC20Tick(context.Background(), 20, []*assetWallet{tokenAw})
	last, err = eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 20 {
		t.Fatalf("expected cursor to skip past the unscannable range to 20, got %d", last)
	}
}

// TestScanERC20MixedPrunedAndTransientEventuallySkips is the regression test
// for the automated PR review's finding: a joined error requiring every leaf
// to agree before it's actionable lets a single steadily-failing provider (a
// free endpoint that 429s on every call, not just once) wedge the scan
// forever, because that leaf is present in every retry and forces the whole
// failure transient. A pruned leaf must count toward the skip even alongside
// a transient one, since narrowing can't help the pruned leaf either way and
// nothing about the transient leaf's presence makes the pruned leaf less
// permanent.
func TestScanERC20MixedPrunedAndTransientEventuallySkips(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(10); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(20)}

	node := eth.node.(*tMempoolNode)
	pruned := errors.New("missing trie node: state at block #20 is pruned")
	transient := errors.New("429 Too Many Requests")
	node.filterLogsErr = fmt.Errorf("%w: %w", pruned, transient)

	for range scanSkipFailures {
		eth.erc20ScanBackoff.until = time.Time{}
		eth.scanERC20Tick(context.Background(), 20, []*assetWallet{tokenAw})
	}

	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 20 {
		t.Fatalf("expected the pruned leaf to count toward the skip despite the steadily-failing transient leaf, cursor should reach tip 20, got %d", last)
	}
}

// TestScanERC20MixedTooWideAndTransientNarrows is the counterpart of
// TestScanERC20MixedPrunedAndTransientEventuallySkips for the range-too-wide
// side of the same finding: a range-capped provider joined with a leaf from a
// provider that fails every call with a rate limit must still narrow the
// window - the too-wide leaf is actionable regardless of what the other leaf
// says, and previously a persistently-failing transient leaf would have
// masked it forever.
func TestScanERC20MixedTooWideAndTransientNarrows(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(1); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(1_000_000)}

	node := eth.node.(*tMempoolNode)
	tooWide := errors.New("eth_getLogs is limited to 0 - 50 blocks range: limit exceeded")
	transient := errors.New("429 Too Many Requests")
	node.filterLogsErr = fmt.Errorf("%w: %w", tooWide, transient)

	eth.scanERC20Tick(context.Background(), 1_000_000, []*assetWallet{tokenAw})
	if eth.erc20ScanBlocks != maxScanBlocks/2 {
		t.Fatalf("expected erc20ScanBlocks to halve to %d despite the steadily-failing transient leaf, got %d", maxScanBlocks/2, eth.erc20ScanBlocks)
	}
}

// TestScanERC20TickSkipJumpsToTip is the regression test for the
// impractically-slow-recovery bug: once a range is confirmed unscannable
// because the provider has pruned that state, every block behind the
// provider's retention window is equally unreachable, not just the one
// maxScanBlocks-wide window requested. Skipping only to the end of that
// window (rather than to the current tip) would force a wallet that fell far
// behind to crawl forward maxScanBlocks at a time - months to catch up on a
// large backlog - while remaining blind to new incoming transfers the whole
// time. The skip must jump straight to the tip.
func TestScanERC20TickSkipJumpsToTip(t *testing.T) {
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
	node.filterLogsErr = errors.New("missing trie node: state at block #21 is pruned")

	for range scanSkipFailures {
		eth.erc20ScanBackoff.until = time.Time{}
		eth.scanERC20Tick(context.Background(), farTip, []*assetWallet{tokenAw})
	}

	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != farTip {
		t.Fatalf("expected the skip to jump straight to tip %d, got %d", farTip, last)
	}
}

// TestScanERC20TickShrinksOnRangeLimit is the regression test for the
// eth_getLogs range-mismatch bug: a provider that rejects the requested
// block range as too wide must cause the scanner to narrow its window and
// keep making progress, rather than failing the same oversized request
// forever.
func TestScanERC20TickShrinksOnRangeLimit(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(0); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	// Seed past the first-run path directly via the field, since 0 is the
	// sentinel for "unseeded".
	if err := eth.setLastScannedERC20Block(1); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(1000)}

	node := eth.node.(*tMempoolNode)
	node.filterLogsErr = errors.New("eth_getLogs is limited to 0 - 50 blocks range: limit exceeded")

	eth.scanERC20Tick(context.Background(), 1000, []*assetWallet{tokenAw})
	if eth.erc20ScanBlocks != maxScanBlocks/2 {
		t.Fatalf("expected erc20ScanBlocks to halve to %d, got %d", maxScanBlocks/2, eth.erc20ScanBlocks)
	}
	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 1 {
		t.Fatalf("expected cursor to stay put on a range-too-wide error, got %d", last)
	}

	// Once narrowed within what the provider accepts, scanning resumes.
	node.filterLogsErr = nil
	eth.scanERC20Tick(context.Background(), 1000, []*assetWallet{tokenAw})
	last, err = eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 1+maxScanBlocks/2 {
		t.Fatalf("expected cursor to advance by the narrowed window, got %d", last)
	}
}

// TestScanERC20NarrowingClearsStaleBackoff is the regression test for the
// automated PR review's finding: a range-too-wide narrowing must fully reset
// the backoff (delay, until, and unscannableFails), not just the counter, so
// an unrelated failure that happens after narrowing (once the window has
// found a width the provider accepts, or the wallet moves on to a different
// failure mode) doesn't inherit an escalated delay left over from failures
// that predate the narrowing.
func TestScanERC20NarrowingClearsStaleBackoff(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(1); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(1000)}

	node := eth.node.(*tMempoolNode)

	// A couple of ordinary transient failures escalate the exponential
	// backoff delay well past scanBackoffMin.
	node.filterLogsErr = context.DeadlineExceeded
	eth.scanERC20Tick(context.Background(), 1000, []*assetWallet{tokenAw})
	eth.erc20ScanBackoff.until = time.Time{}
	eth.scanERC20Tick(context.Background(), 1000, []*assetWallet{tokenAw})
	if eth.erc20ScanBackoff.delay <= scanBackoffMin {
		t.Fatalf("expected the transient failures to escalate the backoff delay past %s, got %s", scanBackoffMin, eth.erc20ScanBackoff.delay)
	}

	// A range-too-wide failure narrows the window - this must clear the
	// escalated delay, not just the unscannableFails counter.
	eth.erc20ScanBackoff.until = time.Time{}
	node.filterLogsErr = errors.New("eth_getLogs is limited to 0 - 50 blocks range: limit exceeded")
	eth.scanERC20Tick(context.Background(), 1000, []*assetWallet{tokenAw})
	if eth.erc20ScanBackoff.delay != 0 {
		t.Fatalf("expected narrowing to clear the stale backoff delay, got %s", eth.erc20ScanBackoff.delay)
	}
	if eth.erc20ScanBackoff.blocked(time.Now()) {
		t.Fatal("expected narrowing to clear the backoff entirely so the next tick isn't blocked")
	}
}

// TestScanERC20WidensAfterConsecutiveSuccesses is the regression test for the
// never-widens-back bug: a window narrowed by a single stale or spurious
// isRangeTooWideErr match used to stay pinned below maxScanBlocks for the
// rest of the process lifetime, issuing roughly 4x more eth_getLogs calls
// than needed forever after. erc20WidenAfterSuccesses consecutive successful
// scans at the narrowed width must widen it - doubling toward maxScanBlocks,
// not jumping straight back to it (see
// TestScanERC20WidenDoublesRatherThanResetting for why an instant jump back
// to maxScanBlocks re-wedges a provider genuinely capped below it).
func TestScanERC20WidensAfterConsecutiveSuccesses(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(1); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(1_000_000)}
	eth.erc20ScanBlocks = minScanBlocks // already narrowed

	for i := 0; i < erc20WidenAfterSuccesses-1; i++ {
		eth.scanERC20Tick(context.Background(), 1_000_000, []*assetWallet{tokenAw})
	}
	if eth.erc20ScanBlocks != minScanBlocks {
		t.Fatalf("expected erc20ScanBlocks to stay narrowed before the widen threshold, got %d", eth.erc20ScanBlocks)
	}

	// One more consecutive success crosses erc20WidenAfterSuccesses.
	eth.scanERC20Tick(context.Background(), 1_000_000, []*assetWallet{tokenAw})
	if eth.erc20ScanBlocks != minScanBlocks*2 {
		t.Fatalf("expected erc20ScanBlocks to double to %d after %d consecutive successes, got %d",
			minScanBlocks*2, erc20WidenAfterSuccesses, eth.erc20ScanBlocks)
	}
	if eth.erc20ScanSuccesses != 0 {
		t.Fatalf("expected the success counter to reset after widening, got %d", eth.erc20ScanSuccesses)
	}
}

// TestScanERC20WidenDoublesRatherThanResetting is the regression test for the
// automated PR review's finding: jumping erc20ScanBlocks straight from a
// narrowed width back to maxScanBlocks after erc20WidenAfterSuccesses
// successes, instead of doubling it, leaves a provider genuinely capped
// below maxScanBlocks unable to ever settle - it fails at maxScanBlocks and
// every halving step back down to its cap, every widen cycle, spending a
// large fraction of ticks making no progress. Doubling toward maxScanBlocks
// instead lets the window approach (and, once it overshoots, narrow back
// toward) the provider's real cap without repeating the full halving
// sequence each time.
func TestScanERC20WidenDoublesRatherThanResetting(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(1); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(1_000_000)}
	const narrowedWindow = 32
	eth.erc20ScanBlocks = narrowedWindow // already narrowed, well below maxScanBlocks

	for range erc20WidenAfterSuccesses {
		eth.scanERC20Tick(context.Background(), 1_000_000, []*assetWallet{tokenAw})
	}
	if eth.erc20ScanBlocks != narrowedWindow*2 {
		t.Fatalf("expected erc20ScanBlocks to double to %d rather than reset to maxScanBlocks, got %d",
			narrowedWindow*2, eth.erc20ScanBlocks)
	}

	// A later widen that would overshoot maxScanBlocks clamps to it (stored
	// as the 0 sentinel) instead of exceeding it.
	eth.erc20ScanBlocks = maxScanBlocks - maxScanBlocks/4 // 96: doubling would overshoot 128
	for range erc20WidenAfterSuccesses {
		eth.scanERC20Tick(context.Background(), 1_000_000, []*assetWallet{tokenAw})
	}
	if eth.erc20ScanBlocks != 0 {
		t.Fatalf("expected erc20ScanBlocks to clamp to the maxScanBlocks sentinel (0) instead of overshooting, got %d",
			eth.erc20ScanBlocks)
	}
}

// TestScanERC20WidenStreakResetsOnFailure is the counterpart of
// TestScanERC20WidensAfterConsecutiveSuccesses: a failure partway through the
// streak must reset it, so the widen threshold requires truly consecutive
// successes, not erc20WidenAfterSuccesses successes total with failures
// interspersed.
func TestScanERC20WidenStreakResetsOnFailure(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(1); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(1_000_000)}
	eth.erc20ScanBlocks = minScanBlocks // already narrowed

	node := eth.node.(*tMempoolNode)
	for i := 0; i < erc20WidenAfterSuccesses-1; i++ {
		eth.scanERC20Tick(context.Background(), 1_000_000, []*assetWallet{tokenAw})
	}
	if eth.erc20ScanSuccesses != erc20WidenAfterSuccesses-1 {
		t.Fatalf("expected %d consecutive successes recorded, got %d", erc20WidenAfterSuccesses-1, eth.erc20ScanSuccesses)
	}

	// An ordinary transient failure interrupts the streak.
	node.filterLogsErr = context.DeadlineExceeded
	eth.erc20ScanBackoff.until = time.Time{}
	eth.scanERC20Tick(context.Background(), 1_000_000, []*assetWallet{tokenAw})
	if eth.erc20ScanSuccesses != 0 {
		t.Fatalf("expected the success streak to reset on a failure, got %d", eth.erc20ScanSuccesses)
	}
	if eth.erc20ScanBlocks != minScanBlocks {
		t.Fatalf("expected erc20ScanBlocks to remain narrowed, got %d", eth.erc20ScanBlocks)
	}
}

// TestScanERC20RangeTooWideStopsNarrowingAtFloor is the regression test for
// the no-terminal-condition bug: once erc20ScanBlocks has bottomed out at
// minScanBlocks (the absolute 1-block floor), a persistent range-too-wide
// error must stop retrying every tick unconditionally (which would re-create
// the exact permanent-hammering bug this feature exists to fix) and instead
// engage the ordinary exponential backoff. It must never reach the
// skip-to-tip path, unlike a pruned failure - see
// TestScanERC20TickNeverSkipsRangeTooWideEvenAtFloor for that distinction
// held across scanSkipFailures consecutive failures.
func TestScanERC20RangeTooWideStopsNarrowingAtFloor(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(1); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(1_000_000)}
	eth.erc20ScanBlocks = minScanBlocks // already at the floor

	node := eth.node.(*tMempoolNode)
	node.filterLogsErr = errors.New("eth_getLogs is limited to 0 - 8 blocks range: limit exceeded")

	eth.scanERC20Tick(context.Background(), 1_000_000, []*assetWallet{tokenAw})
	if eth.erc20ScanBlocks != minScanBlocks {
		t.Fatalf("expected erc20ScanBlocks to stay at the floor %d, got %d", minScanBlocks, eth.erc20ScanBlocks)
	}
	if !eth.erc20ScanBackoff.blocked(time.Now()) {
		t.Fatal("expected the backoff to engage once narrowing can no longer help, instead of retrying every tick forever")
	}
}

// TestScanERC20TickNeverSkipsRangeTooWideEvenAtFloor is the regression test
// for finding F2: a range-too-wide failure must never reach the skip-to-tip
// path, even once narrowing has bottomed out at the absolute 1-block floor
// and even after scanSkipFailures consecutive failures - unlike a pruned
// range, a range a provider merely caps too low is never provably
// unscannable (narrowing further, in principle down to one block, can always
// help), so treating it like pruned state would silently and permanently
// discard a range a narrower request could still have served. It must keep
// retrying under the ordinary exponential backoff instead.
func TestScanERC20TickNeverSkipsRangeTooWideEvenAtFloor(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(10); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(20)}
	eth.erc20ScanBlocks = minScanBlocks // already at the absolute floor

	node := eth.node.(*tMempoolNode)
	node.filterLogsErr = errors.New("eth_getLogs is limited to 0 - 8 blocks range: limit exceeded")

	for range scanSkipFailures + 5 {
		eth.erc20ScanBackoff.until = time.Time{}
		eth.scanERC20Tick(context.Background(), 20, []*assetWallet{tokenAw})
	}

	if eth.erc20ScanBlocks != minScanBlocks {
		t.Fatalf("expected erc20ScanBlocks to stay at the floor %d, not narrow further, got %d", minScanBlocks, eth.erc20ScanBlocks)
	}
	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 10 {
		t.Fatalf("expected a persistent floor-width range-too-wide error to never skip to tip, cursor should stay at 10, got %d", last)
	}
}

// TestScanERC20JoinedPrunedAndRangeTooWideSkipsNotNarrows is the regression
// test for the classifier-precedence bug: multiRPCClient joins every
// provider's error into one message once all providers fail, so a pruned
// provider and a range-capped provider failing in the same call produces one
// error matching both isUnscannableRangeErr and isRangeTooWideErr. That must
// be treated as unscannable (and eventually skipped to tip), not endlessly
// narrowed - narrowing the window can never make already-pruned history
// scannable, so treating it as a range problem would silently re-wedge the
// scanner this fix exists to unwedge.
func TestScanERC20JoinedPrunedAndRangeTooWideSkipsNotNarrows(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(10); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(20)}

	node := eth.node.(*tMempoolNode)
	// Shaped like multiRPCClient.withOne's joined output: one provider's
	// pruned-history message concatenated with another's range-cap message.
	node.filterLogsErr = errors.New("History has been pruned for this block: eth_getLogs is limited to 0 - 50 blocks range")

	for range scanSkipFailures {
		eth.erc20ScanBackoff.until = time.Time{}
		eth.scanERC20Tick(context.Background(), 20, []*assetWallet{tokenAw})
	}

	if eth.erc20ScanBlocks != 0 {
		t.Fatalf("expected the joined error to skip rather than narrow, but erc20ScanBlocks changed to %d", eth.erc20ScanBlocks)
	}
	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 20 {
		t.Fatalf("expected the joined pruned+range-too-wide error to skip to tip 20, got %d", last)
	}
}

// TestScanERC20JoinedPrunedAndTooWideLeavesNeverSkips is the tick-level
// regression test for the F1 classifier bug, combined with the F2 design fix:
// multiRPCClient.withOne joins every provider's error into one value via
// fmt.Errorf("%w: %w", ...), producing a real Unwrap() []error tree with one
// leaf per provider. The old all-leaves-must-match-one-marker-list
// classifiers matched neither list for a pruned leaf plus a separate
// range-capped leaf, so the failure fell through as transient and the scan
// wedged permanently - unlike TestScanERC20JoinedPrunedAndRangeTooWideSkipsNotNarrows
// above, which uses a single flat message carrying both phrasings (one leaf,
// not a joined tree). classifyRangeErr now resolves a joined pruned+tooWide
// tree to rangeErrTooWide (any tooWide leaf wins - narrowing might still
// unblock that provider and can't hurt the pruned one), so per the F2 design
// it must narrow but never skip to tip, even once bottomed out at the
// absolute floor and even after scanSkipFailures consecutive failures.
func TestScanERC20JoinedPrunedAndTooWideLeavesNeverSkips(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(1); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	eth.currentTip = &types.Header{Number: big.NewInt(1_000_000)}

	node := eth.node.(*tMempoolNode)
	node.filterLogsErr = fmt.Errorf("%w: %w",
		errors.New("missing trie node: state at block #20 is pruned"),
		errors.New("eth_getLogs is limited to 0 - 50 blocks range"))

	// Above the floor: narrows rather than skipping on the first failure.
	eth.scanERC20Tick(context.Background(), 1_000_000, []*assetWallet{tokenAw})
	if eth.erc20ScanBlocks != maxScanBlocks/2 {
		t.Fatalf("expected erc20ScanBlocks to halve to %d, got %d", maxScanBlocks/2, eth.erc20ScanBlocks)
	}
	last, err := eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 1 {
		t.Fatalf("expected cursor to stay put while narrowing, got %d", last)
	}

	// At the floor: still classifies tooWide (not pruned), so even
	// scanSkipFailures consecutive failures must never skip to tip.
	eth.erc20ScanBlocks = minScanBlocks
	for range scanSkipFailures + 5 {
		eth.erc20ScanBackoff.until = time.Time{}
		eth.scanERC20Tick(context.Background(), 1_000_000, []*assetWallet{tokenAw})
	}
	last, err = eth.getLastScannedERC20Block()
	if err != nil {
		t.Fatalf("getLastScannedERC20Block: %v", err)
	}
	if last != 1 {
		t.Fatalf("expected the joined pruned+too-wide error to never skip to tip, cursor should stay at 1, got %d", last)
	}
	if eth.erc20ScanBlocks != minScanBlocks {
		t.Fatalf("expected erc20ScanBlocks to stay at the floor, got %d", eth.erc20ScanBlocks)
	}
}

// TestScanERC20WidenStreakIgnoresPartialWindowScans is the regression test
// for finding F3: a tick that requests far fewer blocks than the configured
// window because the cursor is near tip proves nothing about whether the
// provider would accept a full-width request, so it must not count toward
// the widen-back streak. TestScanERC20WidensAfterConsecutiveSuccesses cannot
// catch this bug because it runs from cursor 1 to a tip fixed far in the
// future, so every tick there already uses the full narrowed window.
func TestScanERC20WidenStreakIgnoresPartialWindowScans(t *testing.T) {
	t.Parallel()
	eth, tokenAw, cancel := newScanTestWallets(t)
	defer cancel()

	eth.metaTable = newTestMetaTable(t)
	if err := eth.setLastScannedERC20Block(1); err != nil {
		t.Fatalf("setLastScannedERC20Block: %v", err)
	}
	const narrowedWindow = 32
	eth.erc20ScanBlocks = narrowedWindow // already narrowed

	for i := 0; i < erc20WidenAfterSuccesses+1; i++ {
		// Advance the tip by 3 blocks per tick, well under narrowedWindow, so
		// every tick requests a small partial window instead of the full
		// configured one.
		tip := uint64(1 + 3*(i+1))
		eth.currentTip = &types.Header{Number: new(big.Int).SetUint64(tip)}
		eth.scanERC20Tick(context.Background(), tip, []*assetWallet{tokenAw})
	}

	if eth.erc20ScanBlocks != narrowedWindow {
		t.Fatalf("expected erc20ScanBlocks to stay narrowed since no tick used the full window, got %d", eth.erc20ScanBlocks)
	}
	if eth.erc20ScanSuccesses != 0 {
		t.Fatalf("expected the widen streak to never advance on partial-window scans, got %d", eth.erc20ScanSuccesses)
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
