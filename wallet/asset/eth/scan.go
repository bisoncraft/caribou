// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"encoding/binary"
	"errors"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/erc20"
	"github.com/bisoncraft/caribou/wallet/txdb"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	// incomingScanTick is how often the wallet checks for incoming native and
	// ERC20 transfers.
	incomingScanTick = 15 * time.Second
	// maxScanBlocks caps the block range covered by a single ERC20 log-filter
	// call, or considered by the native scan, per tick, so a wallet that
	// falls behind (e.g. after being offline) catches up gradually instead
	// of issuing one enormous RPC request.
	maxScanBlocks = 128
	// maxScanBodies caps how many block bodies are fetched per tick when
	// checking for native-value transfers. Only the most recent blocks in the
	// scan range are checked when the range exceeds this cap.
	maxScanBodies = 16

	// scanBackoffMin and scanBackoffMax bound the cooldown applied after a
	// scan sub-loop (native or ERC20) fails, so a provider that will never
	// succeed at a given call (e.g. eth_getLogs disabled entirely on a free
	// public endpoint) doesn't get hammered - and doesn't spam the log -
	// every incomingScanTick forever.
	scanBackoffMin = 30 * time.Second
	scanBackoffMax = 30 * time.Minute

	// scanSkipFailures is how many consecutive failures classified as
	// "unscannable range" (see isUnscannableRangeErr and
	// scanBackoff.unscannableFails) are required before the scanner gives up
	// on the current range and skips past it. A joined multi-provider error
	// can carry an unscannable-range marker from one provider while another
	// provider merely failed transiently, so a single such error is not proof
	// the range can never succeed; several consecutive ones are.
	scanSkipFailures = 3

	// minScanBlocks is the floor for erc20ScanBlocks when a provider reports
	// its eth_getLogs range as too wide (see isRangeTooWideErr). Kept above
	// the fastest supported chain's block rate at incomingScanTick cadence
	// (HyperEVM has been observed averaging ~1.1 blocks/s, i.e. ~17 per
	// 15s tick) so a wallet pinned at the floor can still keep pace with the
	// chain tip instead of falling further behind every tick.
	minScanBlocks = 32

	scanMetaTableName           = "meta"
	lastScannedNativeBlockField = "lastScannedBlock"
	lastScannedERC20BlockField  = "lastScannedERC20Block"
)

// scanBackoff is the cooldown applied after a scan sub-loop fails. Zero value
// is ready to use.
type scanBackoff struct {
	mtx   sync.Mutex
	until time.Time
	delay time.Duration
	// unscannableFails counts *consecutive* failures classified as
	// isUnscannableRangeErr specifically - not failures generally. A run of
	// ordinary transient failures (connection reset, timeout) does not count
	// toward it, and any such failure resets it to 0, so an unscannable-range
	// skip requires scanSkipFailures unscannable-classified failures in a
	// row, not scanSkipFailures failures of any kind with only the last one
	// classified unscannable.
	unscannableFails int
}

func (b *scanBackoff) blocked(now time.Time) bool {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return now.Before(b.until)
}

// failed records a scan failure and applies the exponential backoff.
// unscannable reports whether this specific failure was classified as
// isUnscannableRangeErr; see the unscannableFails field doc.
func (b *scanBackoff) failed(now time.Time, unscannable bool) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if b.delay == 0 {
		b.delay = scanBackoffMin
	} else if b.delay *= 2; b.delay > scanBackoffMax {
		b.delay = scanBackoffMax
	}
	b.until = now.Add(b.delay)
	if unscannable {
		b.unscannableFails++
	} else {
		b.unscannableFails = 0
	}
}

func (b *scanBackoff) succeeded() {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	b.delay = 0
	b.until = time.Time{}
	b.unscannableFails = 0
}

// unscannableFailures returns the number of consecutive isUnscannableRangeErr
// failures recorded since the last success or non-unscannable failure.
func (b *scanBackoff) unscannableFailures() int {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return b.unscannableFails
}

// unscannableRangeMarkers are substrings (matched case-insensitively) of RPC
// error messages that mean a block range can never be scanned by the
// provider that returned them, no matter how many times it is retried - the
// requested chain state has been pruned. This is necessarily a string match:
// go-ethereum's client does not expose a typed error for it, since it comes
// from third-party JSON-RPC providers with no structured error codes.
//
// "pruned" is matched bare (not "is pruned") since providers phrase this
// differently - go-ethereum/Geth-family nodes say "state ... is pruned",
// while BNB's eth_getLogs pruned-history response says "History has been
// pruned for this block" - and "pruned" alone doesn't appear in the
// transient errors this list must not match (rate limits, timeouts,
// connection resets).
//
// The list is otherwise deliberately narrow. "header not found" and bare
// "not found" are excluded even though they appear in some pruned-state
// error chains, because a provider also returns them when it is merely
// lagging the chain tip - treating those as unscannable would skip a range a
// fresher provider, or the same provider moments later, would have served.
var unscannableRangeMarkers = []string{
	"missing trie node",
	"pruned",
	"state is not available",
	"missing state for block",
	"no historical state",
}

// rangeTooWideCueMarkers are substrings (matched case-insensitively) of RPC
// error messages that, together with the word "range" appearing anywhere in
// the same message, mean the provider refuses the requested eth_getLogs
// block range because it is wider than the provider allows - a hard
// client/provider mismatch that will fail identically on every retry until
// the requested range is narrowed. Providers phrase this wildly differently
// ("eth_getLogs is limited to 0 - 50 blocks range", "requests with up to a
// 10 block range"), so rather than matching one exact phrase, this requires
// "range" plus a magnitude cue - looser than an exact-phrase list, but still
// narrow enough that a bare rate-limit message ("429 Too Many Requests",
// "limit exceeded") never contains "range" and so never matches.
//
// A joined multi-provider error (every provider failed, concatenated into
// one message - see multiRPCClient.withOne) can still pair a cue word from
// one provider's message with "range" from an unrelated one's, so the cue
// list deliberately excludes generic words like "too many" and "exceeds"
// that show up in ordinary rate-limit/quota messages having nothing to do
// with a range cap; every marker here is specific enough that it's only
// plausible in a message actually describing a size/range limit.
var rangeTooWideCueMarkers = []string{
	"limited to",
	"up to",
	"too large",
	"maximum of",
	"at most",
}

func containsAnyFold(msg string, markers []string) bool {
	msg = strings.ToLower(msg)
	for _, m := range markers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// isUnscannableRangeErr reports whether err means the requested block range
// can never be scanned by the failing provider(s) because the chain state it
// needs has been pruned.
func isUnscannableRangeErr(err error) bool {
	return err != nil && containsAnyFold(err.Error(), unscannableRangeMarkers)
}

// isRangeTooWideErr reports whether err means the failing provider rejected
// the requested eth_getLogs block range as too wide.
func isRangeTooWideErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "range") && containsAnyFold(msg, rangeTooWideCueMarkers)
}

// scanMetaKey is the meta-table key for one field scoped to a base chain.
// Every asset on the chain (native + tokens) is scanned by one loop that
// shares this cursor pair, so the key is scoped by baseChainID rather than by
// individual asset ID.
func scanMetaKey(baseChainID uint32, field string) []byte {
	k := make([]byte, 4+len(field))
	binary.BigEndian.PutUint32(k, baseChainID)
	copy(k[4:], field)
	return k
}

func (w *baseWallet) getScanCursor(field string) (uint64, error) {
	if w.metaTable == nil {
		return 0, nil
	}
	b, err := w.metaTable.Get(scanMetaKey(w.baseChainID, field))
	if errors.Is(err, txdb.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(b) < 8 {
		return 0, nil
	}
	return binary.BigEndian.Uint64(b), nil
}

func (w *baseWallet) setScanCursor(field string, n uint64) error {
	if w.metaTable == nil {
		return nil
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, n)
	return w.metaTable.Set(scanMetaKey(w.baseChainID, field), b, txdb.WithReplace())
}

func (w *baseWallet) getLastScannedBlock() (uint64, error) {
	return w.getScanCursor(lastScannedNativeBlockField)
}

func (w *baseWallet) setLastScannedBlock(n uint64) error {
	return w.setScanCursor(lastScannedNativeBlockField, n)
}

func (w *baseWallet) getLastScannedERC20Block() (uint64, error) {
	return w.getScanCursor(lastScannedERC20BlockField)
}

func (w *baseWallet) setLastScannedERC20Block(n uint64) error {
	return w.setScanCursor(lastScannedERC20BlockField, n)
}

// scanIncomingTransfers runs the incoming-transfer scan loop for native and
// ERC20 transfers on this chain until ctx is canceled. There is one loop per
// chain (started from ETHWallet.Connect), covering the native asset and every
// currently-connected token wallet.
func (w *ETHWallet) scanIncomingTransfers(ctx context.Context) {
	ticker := time.NewTicker(incomingScanTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.scanIncomingTransfersTick(ctx)
		}
	}
}

// scanIncomingTransfersTick runs a single pass of the incoming-transfer scan.
// Native and ERC20 detection each keep their own persisted cursor and
// failure backoff (see scanNativeTick/scanERC20Tick), so a failure or
// permanently-unsupported RPC method in one (e.g. a free public provider
// that doesn't serve eth_getLogs at all) can never block or slow the other.
//
// The scan never looks past tip - finalizeConfs: both store paths mark a
// detected receive Confirmed immediately, and nothing later reconciles a
// confirmed record, so scanning any closer to the tip would leave a phantom
// Receive behind if a reorg drops the block it came from.
func (w *ETHWallet) scanIncomingTransfersTick(ctx context.Context) {
	tip := w.tip()
	if tip == nil {
		return
	}
	tipNum := tip.Number.Uint64()
	if tipNum <= w.finalizeConfs {
		return
	}
	tipNum -= w.finalizeConfs

	w.scanNativeTick(ctx, tipNum)

	tokenWallets := make([]*assetWallet, 0, len(w.connectedWallets()))
	for _, aw := range w.connectedWallets() {
		if aw.tokenAddr != (common.Address{}) {
			tokenWallets = append(tokenWallets, aw)
		}
	}
	if len(tokenWallets) > 0 {
		w.scanERC20Tick(ctx, tipNum, tokenWallets)
	}
}

// scanNativeTick runs one pass of native-transfer detection against its own
// cursor and backoff.
func (w *ETHWallet) scanNativeTick(ctx context.Context, tipNum uint64) {
	if w.nativeScanBackoff.blocked(time.Now()) {
		return
	}

	last, err := w.getLastScannedBlock()
	if err != nil {
		w.log.Errorf("incoming transfer scan: error reading native scan cursor: %v", err)
		return
	}

	// First run: seed the cursor at the current tip. Pre-existing history is
	// not backfilled.
	if last == 0 {
		if err := w.setLastScannedBlock(tipNum); err != nil {
			w.log.Errorf("incoming transfer scan: error seeding native scan cursor: %v", err)
		}
		return
	}

	if tipNum <= last {
		return
	}

	from := last + 1
	to := tipNum
	if to-from+1 > maxScanBlocks {
		to = from + maxScanBlocks - 1
	}

	covered, err := w.scanNativeReceives(ctx, from, to)
	if err != nil {
		w.log.Errorf("incoming transfer scan: error scanning native transfers %d-%d: %v", from, to, err)
		unscannable := isUnscannableRangeErr(err)
		w.nativeScanBackoff.failed(time.Now(), unscannable)
		if unscannable && w.nativeScanBackoff.unscannableFailures() >= scanSkipFailures {
			// Jump straight to the current tip rather than just past this one
			// maxScanBlocks window: the provider has pruned the state this
			// wallet needs, and that's true of every block behind its
			// retention window, not just this range - inching forward one
			// window at a time would take a wallet that fell far behind
			// (as this one has) months to catch up, all while remaining
			// blind to new incoming transfers the whole time.
			w.log.Warnf("incoming transfer scan: chain state is pruned at this provider from block %d onward; "+
				"jumping to tip %d - transfers in %d-%d will not be detected: %v", from, tipNum, from, tipNum, err)
			covered = tipNum
			// The balance baseline for the skipped range is unknown, so the
			// next tick must fetch bodies rather than trust an "unchanged"
			// comparison against a stale baseline.
			w.lastScanBalance = nil
			w.nativeScanBackoff.succeeded()
		}
	} else {
		w.nativeScanBackoff.succeeded()
	}

	// Persist whatever was genuinely covered even on a partial failure -
	// covered is only ever last (no progress) or higher, never a range that
	// was skipped. Persisting the full requested "to" regardless of what was
	// actually scanned is what silently drops blocks.
	if covered > last {
		if serr := w.setLastScannedBlock(covered); serr != nil {
			w.log.Errorf("incoming transfer scan: error persisting native scan cursor: %v", serr)
		}
	}
}

// scanERC20Tick runs one pass of ERC20-transfer detection against its own
// cursor and backoff, independent of the native scan.
func (w *ETHWallet) scanERC20Tick(ctx context.Context, tipNum uint64, tokenWallets []*assetWallet) {
	if w.erc20ScanBackoff.blocked(time.Now()) {
		return
	}

	last, err := w.getLastScannedERC20Block()
	if err != nil {
		w.log.Errorf("incoming transfer scan: error reading erc20 scan cursor: %v", err)
		return
	}

	// First run: seed the cursor at the current tip. Pre-existing history is
	// not backfilled.
	if last == 0 {
		if err := w.setLastScannedERC20Block(tipNum); err != nil {
			w.log.Errorf("incoming transfer scan: error seeding erc20 scan cursor: %v", err)
		}
		return
	}

	if tipNum <= last {
		return
	}

	scanBlocks := w.erc20ScanBlocks
	if scanBlocks == 0 {
		scanBlocks = maxScanBlocks
	}

	from := last + 1
	to := tipNum
	if to-from+1 > scanBlocks {
		to = from + scanBlocks - 1
	}

	err = w.scanERC20Receives(ctx, from, to, tokenWallets)
	if err != nil {
		w.log.Errorf("incoming transfer scan: error scanning erc20 transfers %d-%d: %v", from, to, err)

		// isUnscannableRangeErr is checked first and, if it matches, the
		// range-too-wide branch below is skipped even when the joined
		// multi-provider error also carries a range-too-wide marker (BNB's
		// pruned-history and range-limit messages are frequently joined
		// together - see isRangeTooWideErr's doc comment). A pruned range can
		// never be narrowed into scannability, so it must reach the
		// unscannable-range skip below rather than get stuck narrowing
		// forever.
		unscannable := isUnscannableRangeErr(err)
		if !unscannable && isRangeTooWideErr(err) && scanBlocks > minScanBlocks {
			narrowed := max(scanBlocks/2, minScanBlocks)
			w.erc20ScanBlocks = narrowed
			w.log.Warnf("incoming transfer scan: provider caps eth_getLogs range; reducing erc20 scan window to %d blocks", narrowed)
			return
		}

		w.erc20ScanBackoff.failed(time.Now(), unscannable)
		if !unscannable || w.erc20ScanBackoff.unscannableFailures() < scanSkipFailures {
			return
		}
		// Jump straight to the current tip rather than just past this one
		// window - see the equivalent native-scan comment in scanNativeTick
		// for why inching forward window by window is impractical once a
		// wallet has fallen behind a provider's pruning horizon.
		w.log.Warnf("incoming transfer scan: chain state is pruned at this provider from block %d onward; "+
			"jumping to tip %d - transfers in %d-%d will not be detected: %v", from, tipNum, from, tipNum, err)
		to = tipNum
		w.erc20ScanBackoff.succeeded()
	} else {
		w.erc20ScanBackoff.succeeded()
	}

	if err := w.setLastScannedERC20Block(to); err != nil {
		w.log.Errorf("incoming transfer scan: error persisting erc20 scan cursor: %v", err)
	}
}

// scanERC20Receives checks for incoming ERC20 Transfer events to this wallet's
// address across every connected token contract, in a single log-filter call
// covering the given block range. Stops and returns an error on the first
// store failure rather than logging and continuing: the range is re-fetched
// and reprocessed in full next tick (see scanERC20Tick), which is safe and
// cheap for a single log-filter call, and dedup skips whatever already
// succeeded.
func (w *ETHWallet) scanERC20Receives(ctx context.Context, from, to uint64, tokenWallets []*assetWallet) error {
	addrs := make([]common.Address, len(tokenWallets))
	byAddr := make(map[common.Address]*assetWallet, len(tokenWallets))
	for i, aw := range tokenWallets {
		addrs[i] = aw.tokenAddr
		byAddr[aw.tokenAddr] = aw
	}

	transferTopic := erc20.ERC20ABI.Events["Transfer"].ID
	toTopic := common.BytesToHash(w.addr.Bytes())

	logs, err := w.node.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: addrs,
		Topics:    [][]common.Hash{{transferTopic}, nil, {toTopic}},
	})
	if err != nil {
		return err
	}

	// seen counts matching logs already processed for a given tx hash in
	// this batch, so a single tx delivering more than one token to us (an
	// aggregator payout, a multisend) gets a distinct ID per log instead of
	// the second one looking like a duplicate of the first and being
	// dropped. The first log for a tx keeps today's plain-tx-hash ID.
	seen := make(map[common.Hash]int, len(logs))
	// blockTimes caches one header lookup per distinct block that actually
	// contained a matching log, rather than one per log.
	blockTimes := make(map[common.Hash]uint64, len(logs))
	for i := range logs {
		lg := logs[i]
		aw, found := byAddr[lg.Address]
		if !found {
			continue
		}
		blockTime, cached := blockTimes[lg.BlockHash]
		if !cached {
			hdr, err := w.node.headerByHash(ctx, lg.BlockHash)
			if err != nil || hdr == nil {
				// A missing timestamp is cosmetic - it must not stall
				// detection of the transfer itself.
				w.log.Warnf("incoming transfer scan: error fetching header %s for erc20 receive timestamp: %v", lg.BlockHash, err)
				blockTime = uint64(time.Now().Unix())
			} else {
				blockTime = hdr.Time
			}
			blockTimes[lg.BlockHash] = blockTime
		}
		occurrence := seen[lg.TxHash]
		seen[lg.TxHash] = occurrence + 1
		if err := w.storeTokenReceive(aw, &lg, receiveLogID(&lg, occurrence), blockTime); err != nil {
			w.log.Errorf("incoming transfer scan: error storing erc20 receive %s: %v", lg.TxHash, err)
			return err
		}
	}
	return nil
}

// receiveLogID returns the dedup/storage ID for a discovered Transfer log.
// occurrence is how many matching logs for this same tx hash were already
// processed earlier in the same scan pass (0 for the first).
func receiveLogID(lg *types.Log, occurrence int) common.Hash {
	if occurrence == 0 {
		return lg.TxHash
	}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(occurrence))
	return crypto.Keccak256Hash(lg.TxHash.Bytes(), buf[:])
}

// storeTokenReceive builds and stores a Receive transaction for a single
// discovered Transfer log, reusing the same decode path as an on-demand
// lookup (TokenWallet.extractValueFromTransferLog) by wrapping the log in a
// synthetic receipt.
func (w *ETHWallet) storeTokenReceive(aw *assetWallet, lg *types.Log, id common.Hash, blockTime uint64) error {
	if existing, err := w.txDB.getTx(id); err != nil {
		return err
	} else if existing != nil {
		return nil
	}

	value, err := aw.tokenContractor.parseTransfer(&types.Receipt{Logs: []*types.Log{lg}})
	if err != nil {
		return err
	}

	addr := w.addr.String()
	wt := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			Type:        txdb.Receive,
			ID:          id.String(),
			NetworkID:   w.baseChainID,
			AssetID:     aw.assetID,
			BlockNumber: lg.BlockNumber,
			Timestamp:   blockTime,
			Amount:      value,
			Recipient:   &addr,
			Confirmed:   true,
		},
	}
	if err := w.txDB.storeTx(wt); err != nil {
		return err
	}
	w.log.Infof("Detected incoming token %d transfer %s: %d atoms", aw.assetID, lg.TxHash, value)

	w.emitTransactionNote(wt.WalletTransaction, true)
	if bal, err := aw.balance(); err == nil {
		aw.emit.BalanceChange(bal)
	}
	return nil
}

// scanNativeReceives checks for incoming native-value transfers to this
// wallet's address over the given block range, returning the highest block
// number actually covered - which can be less than to (down to from-1,
// meaning no progress at all) if the balance check or a body fetch failed
// partway through, or if the range was capped by maxScanBodies. Callers must
// persist the cursor at the returned value, not at the requested to, or
// blocks between the two are silently skipped forever.
//
// Block bodies are only fetched when the confirmed balance has changed since
// the last tick, since a native transfer is the only thing that can change
// it. The unchanged check reads the balance pinned to to - the requested
// range end - since an unchanged balance there proves nothing arrived
// anywhere in from..to, letting the whole range (even one wider than
// maxScanBodies) be retired without fetching a single body. Once a change is
// found, though, only from..scanTo (the range actually covered when capped
// by maxScanBodies) is scanned, so the baseline persisted for the next tick
// must be read again pinned to scanTo rather than to - pinning it to to
// would let it silently absorb a balance change from a block this call
// never looked at, so a later tick sees "unchanged" and skips the block
// containing the receive forever instead of converging onto it on a
// following tick.
func (w *ETHWallet) scanNativeReceives(ctx context.Context, from, to uint64) (uint64, error) {
	scanTo := to
	if to-from+1 > maxScanBodies {
		scanTo = from + maxScanBodies - 1
	}

	bal, err := w.node.addressBalanceAt(ctx, w.addr, new(big.Int).SetUint64(to))
	if err != nil {
		return from - 1, err
	}
	if w.lastScanBalance != nil && bal.Cmp(w.lastScanBalance) == 0 {
		return to, nil
	}

	if scanTo != to {
		bal, err = w.node.addressBalanceAt(ctx, w.addr, new(big.Int).SetUint64(scanTo))
		if err != nil {
			return from - 1, err
		}
	}

	for n := from; n <= scanTo; n++ {
		block, err := w.node.blockByNumber(ctx, new(big.Int).SetUint64(n))
		if err != nil {
			// lastScanBalance is left unset rather than advanced to bal:
			// the cursor is about to rewind to n-1, but bal describes
			// scanTo, so pinning it here would let a later tick compare
			// blocks n..scanTo against a baseline that already accounts
			// for them and skip a receive among them as "unchanged".
			w.lastScanBalance = nil
			return n - 1, err
		}
		for _, tx := range block.Transactions() {
			recipient := tx.To()
			if recipient == nil || *recipient != w.addr || tx.Value().Sign() <= 0 {
				continue
			}
			if err := w.storeNativeReceive(tx, block.NumberU64(), block.Time()); err != nil {
				w.log.Errorf("incoming transfer scan: error storing native receive %s: %v", tx.Hash(), err)
				w.lastScanBalance = nil
				return n - 1, err
			}
		}
	}
	w.lastScanBalance = bal
	return scanTo, nil
}

// storeNativeReceive builds and stores a Receive transaction for a discovered
// native-value transfer, mirroring ETHWallet.getReceivingTransaction's shape.
func (w *ETHWallet) storeNativeReceive(tx *types.Transaction, blockNumber, blockTime uint64) error {
	if existing, err := w.txDB.getTx(tx.Hash()); err != nil {
		return err
	} else if existing != nil {
		return nil
	}

	addr := w.addr.String()
	wt := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			Type:        txdb.Receive,
			ID:          tx.Hash().String(),
			NetworkID:   w.baseChainID,
			AssetID:     w.assetID,
			BlockNumber: blockNumber,
			Timestamp:   blockTime,
			Amount:      w.atomize(tx.Value()),
			Recipient:   &addr,
			Confirmed:   true,
		},
	}
	if err := w.txDB.storeTx(wt); err != nil {
		return err
	}
	w.log.Infof("Detected incoming %s transfer %s: %d %s", w.ui.Conventional.Unit, tx.Hash(), wt.Amount, w.ui.AtomicUnit)

	w.emitTransactionNote(wt.WalletTransaction, true)
	if bal, err := w.balance(); err == nil {
		w.emit.BalanceChange(bal)
	}
	return nil
}
