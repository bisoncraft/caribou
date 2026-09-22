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
	// incomingScanTick is how often the wallet checks for incoming ERC20
	// transfers.
	incomingScanTick = 15 * time.Second
	// maxScanBlocks caps the block range covered by a single ERC20 log-filter
	// call per tick, so a wallet that falls behind (e.g. after being offline)
	// catches up gradually instead of issuing one enormous RPC request.
	maxScanBlocks = 128

	// scanBackoffMin and scanBackoffMax bound the cooldown applied after the
	// ERC20 scan fails, so a provider that will never succeed at a given call
	// (e.g. eth_getLogs disabled entirely on a free public endpoint) doesn't
	// get hammered - and doesn't spam the log - every incomingScanTick
	// forever.
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

	// minScanBlocks is the absolute floor for erc20ScanBlocks: a block range
	// request can never be narrower than one block. When a provider reports
	// its eth_getLogs range as too wide (see isRangeTooWideErr), the window
	// keeps narrowing toward this floor every tick rather than giving up,
	// because narrowing can only help a range-capped provider and never
	// hurts - unlike a pruned range (see isUnscannableRangeErr), which can
	// never be narrowed into scannability at any width and is the only
	// failure mode that reaches the skip-to-tip path in scanERC20Tick. A
	// provider that still rejects even a 1-block range as too wide isn't
	// expected in practice; if it happened, the scan would retry it forever
	// under the ordinary exponential backoff rather than silently discarding
	// a range a narrower request might still have served.
	minScanBlocks = 1

	// erc20WidenAfterSuccesses is how many consecutive successful ERC20 scans
	// are required before erc20ScanBlocks is doubled back toward
	// maxScanBlocks, so a window narrowed by one stale or spurious
	// isRangeTooWideErr match doesn't stay pinned below maxScanBlocks for the
	// rest of the process lifetime. Doubling rather than jumping straight
	// back to maxScanBlocks matters for a provider genuinely capped below
	// maxScanBlocks: jumping back would fail at the full window and every
	// halving step down to the capped width again each cycle, spending a
	// large fraction of ticks making no progress.
	erc20WidenAfterSuccesses = 10

	scanMetaTableName          = "meta"
	lastScannedERC20BlockField = "lastScannedERC20Block"
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

// allErrors flattens err into its leaf errors, unwrapping both single-error
// (Unwrap() error) and multi-error (Unwrap() []error) chains - the shape
// multiRPCClient.withOne produces via fmt.Errorf("%w: %w", ...) when every
// provider in a call fails, one leaf per distinct provider error message.
func allErrors(err error) []error {
	if err == nil {
		return nil
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range u.Unwrap() {
			out = append(out, allErrors(e)...)
		}
		return out
	}
	if u, ok := err.(interface{ Unwrap() error }); ok {
		if next := u.Unwrap(); next != nil {
			return allErrors(next)
		}
	}
	return []error{err}
}

// rangeErrClass classifies the outcome of a failed scan attempt for the
// purposes of deciding whether to narrow the window, back off and retry
// unchanged, or give up on the range and skip to tip.
type rangeErrClass int

const (
	// rangeErrTransient means no provider in a joined multi-provider failure
	// (see allErrors) reported a pruned or range-too-wide condition - nothing
	// can be concluded about the range itself, so the caller should apply the
	// ordinary backoff and retry unchanged. A provider that fails the same
	// way on every call (e.g. eth_getLogs disabled, or a free endpoint that
	// 429s every time) always contributes a leaf to a joined error, so a
	// class decided by "every leaf must agree" would let that one steadily
	// failing leaf force transient forever; the classification below is
	// decided from the actionable leaves alone instead.
	rangeErrTransient rangeErrClass = iota
	// rangeErrPruned means at least one provider reported pruned state and
	// none reported range-too-wide - the range can never be served by that
	// provider, at any window width, and no other leaf suggests narrowing
	// would help.
	rangeErrPruned
	// rangeErrTooWide means at least one provider reported a range-too-wide
	// condition. Narrowing the window can only help (a too-wide provider may
	// then succeed) and never hurts a pruned one, so it takes precedence over
	// rangeErrPruned and should always be attempted before concluding a range
	// is unscannable.
	rangeErrTooWide
)

// classifyLeaf classifies a single (non-joined) provider error message.
// Pruned is checked before too-wide so that a single provider message
// carrying both phrasings (e.g. BNB's pruned-history and range-limit
// phrasing) still classifies pruned, not too-wide.
func classifyLeaf(msg string) rangeErrClass {
	switch {
	case containsAnyFold(msg, unscannableRangeMarkers):
		return rangeErrPruned
	case strings.Contains(strings.ToLower(msg), "range") && containsAnyFold(msg, rangeTooWideCueMarkers):
		return rangeErrTooWide
	default:
		return rangeErrTransient
	}
}

// classifyRangeErr classifies err, which may be a joined multi-provider
// error (see allErrors / multiRPCClient.withOne), into how the scanner
// should react. The decision is made from the actionable leaves alone: a
// leaf that is merely transient (a rate limit, a timeout) is ignored rather
// than forcing the whole failure transient, since a provider that fails the
// same way on every call would otherwise wedge the scan forever (it always
// contributes a transient-looking leaf to the joined error). Among the
// remaining leaves, any tooWide leaf wins over pruned ones, since narrowing
// can only help the capped provider and cannot hurt the pruned one. Only
// when no leaf is actionable at all is the failure treated as transient.
func classifyRangeErr(err error) rangeErrClass {
	leaves := allErrors(err)
	if len(leaves) == 0 {
		return rangeErrTransient
	}
	sawTooWide := false
	sawPruned := false
	for _, e := range leaves {
		switch classifyLeaf(e.Error()) {
		case rangeErrTooWide:
			sawTooWide = true
		case rangeErrPruned:
			sawPruned = true
		}
	}
	switch {
	case sawTooWide:
		return rangeErrTooWide
	case sawPruned:
		return rangeErrPruned
	default:
		return rangeErrTransient
	}
}

// isUnscannableRangeErr reports whether err means the requested block range
// can never be scanned by the failing provider(s) because the chain state it
// needs has been pruned.
func isUnscannableRangeErr(err error) bool {
	return classifyRangeErr(err) == rangeErrPruned
}

// isRangeTooWideErr reports whether err means the failing provider(s)
// rejected the requested eth_getLogs block range as too wide (narrowing the
// window can help), possibly alongside another provider reporting pruned
// state (narrowing can't help that one, but trying costs nothing).
func isRangeTooWideErr(err error) bool {
	return classifyRangeErr(err) == rangeErrTooWide
}

// scanMetaKey is the meta-table key for one field scoped to a base chain.
// Every token wallet on the chain is scanned by one loop that shares this
// cursor, so the key is scoped by baseChainID rather than by individual
// asset ID.
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

func (w *baseWallet) getLastScannedERC20Block() (uint64, error) {
	return w.getScanCursor(lastScannedERC20BlockField)
}

func (w *baseWallet) setLastScannedERC20Block(n uint64) error {
	return w.setScanCursor(lastScannedERC20BlockField, n)
}

// scanIncomingTransfers runs the incoming-transfer scan loop for ERC20
// transfers on this chain until ctx is canceled. There is one loop per chain
// (started from ETHWallet.Connect), covering every currently-connected token
// wallet.
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

// scanIncomingTransfersTick runs a single pass of the incoming ERC20 transfer
// scan.
//
// The scan never looks past tip - finalizeConfs: storeTokenReceive marks a
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

// scanERC20Tick runs one pass of ERC20-transfer detection against its own
// cursor and backoff.
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
	// reqBlocks is the width actually requested this tick, which is only
	// equal to scanBlocks when the wallet is at least a full window behind
	// tip - it's always <= scanBlocks.
	reqBlocks := to - from + 1

	err = w.scanERC20Receives(ctx, from, to, tokenWallets)
	if err != nil {
		w.log.Errorf("incoming transfer scan: error scanning erc20 transfers %d-%d: %v", from, to, err)
		w.erc20ScanSuccesses = 0

		// classifyRangeErr resolves the whole (possibly joined
		// multi-provider) failure to one class from the actionable leaves
		// alone: too-wide as soon as any leaf is a range cap (narrowing might
		// unblock that provider and cannot hurt a pruned one), else pruned if
		// any leaf is pruned, else transient only when no leaf is actionable.
		// A merely-transient leaf never forces the whole failure transient -
		// a provider that fails the same way on every call (e.g. eth_getLogs
		// disabled) would otherwise always contribute one and wedge the scan
		// forever. A single message carrying both phrasings (BNB's
		// pruned-history and range-limit phrasing) still classifies pruned,
		// so an already-pruned range reaches the skip path below instead of
		// narrowing forever.
		class := classifyRangeErr(err)
		pruned := class == rangeErrPruned
		tooWide := class == rangeErrTooWide
		if tooWide && reqBlocks > minScanBlocks {
			narrowed := max(scanBlocks/2, minScanBlocks)
			w.erc20ScanBlocks = narrowed
			// A range-too-wide narrowing isn't a scan failure from the
			// backoff's perspective - it's meant to retry promptly at the
			// new window width, not wait out the exponential backoff or
			// leave a stale delay/unscannableFails behind for a later,
			// unrelated failure to inherit. succeeded() gives it the same
			// clean-slate reset a successful scan would.
			w.erc20ScanBackoff.succeeded()
			w.log.Warnf("incoming transfer scan: provider caps eth_getLogs range; reducing erc20 scan window to %d blocks", narrowed)
			return
		}

		// Only a pruned failure ever reaches the skip-to-tip path below - a
		// range-too-wide failure never does, even once narrowing has
		// bottomed out at minScanBlocks (the narrowing branch above already
		// returned before reaching here in that case; falling through this
		// far while tooWide means the window is already at the absolute
		// floor). Narrowing can only help a range-capped provider and never
		// hurts, so it keeps retrying under the ordinary exponential backoff
		// instead of silently discarding a range a narrower request might
		// still have served.
		w.erc20ScanBackoff.failed(time.Now(), pruned)
		if !pruned || w.erc20ScanBackoff.unscannableFailures() < scanSkipFailures {
			return
		}
		// Jump straight to the current tip rather than just past this one
		// window: every block behind the provider's retention window is
		// equally unreachable, not just this one - inching forward window by
		// window would take a wallet that fell far behind months to catch
		// up, all while remaining blind to new incoming transfers the whole
		// time.
		w.log.Warnf("incoming transfer scan: block range unscannable at this provider (chain state has been "+
			"pruned); jumping to tip %d - transfers in %d-%d will not be detected: %v", tipNum, from, tipNum, err)
		to = tipNum
		w.erc20ScanBackoff.succeeded()
	} else {
		w.erc20ScanBackoff.succeeded()
		// A tick that requested fewer than the full configured window (a
		// near-tip catch-up scan) proves nothing about whether the provider
		// would accept a full-width request, so it must not count toward the
		// widen-back streak - it's neutral: neither incremented nor reset.
		if w.erc20ScanBlocks > 0 && reqBlocks == scanBlocks {
			w.erc20ScanSuccesses++
			if w.erc20ScanSuccesses >= erc20WidenAfterSuccesses {
				widened := min(scanBlocks*2, maxScanBlocks)
				w.log.Debugf("incoming transfer scan: erc20 scan window recovered after %d consecutive successes; "+
					"widening to %d blocks", erc20WidenAfterSuccesses, widened)
				if widened >= maxScanBlocks {
					w.erc20ScanBlocks = 0
				} else {
					w.erc20ScanBlocks = widened
				}
				w.erc20ScanSuccesses = 0
			}
		}
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
