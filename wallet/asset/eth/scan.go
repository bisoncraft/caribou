// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"encoding/binary"
	"errors"
	"math/big"
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
}

func (b *scanBackoff) blocked(now time.Time) bool {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return now.Before(b.until)
}

func (b *scanBackoff) failed(now time.Time) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if b.delay == 0 {
		b.delay = scanBackoffMin
	} else if b.delay *= 2; b.delay > scanBackoffMax {
		b.delay = scanBackoffMax
	}
	b.until = now.Add(b.delay)
}

func (b *scanBackoff) succeeded() {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	b.delay = 0
	b.until = time.Time{}
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
func (w *ETHWallet) scanIncomingTransfersTick(ctx context.Context) {
	tip := w.tip()
	if tip == nil {
		return
	}
	tipNum := tip.Number.Uint64()

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
		w.nativeScanBackoff.failed(time.Now())
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

	from := last + 1
	to := tipNum
	if to-from+1 > maxScanBlocks {
		to = from + maxScanBlocks - 1
	}

	if err := w.scanERC20Receives(ctx, from, to, tokenWallets); err != nil {
		w.log.Errorf("incoming transfer scan: error scanning erc20 transfers %d-%d: %v", from, to, err)
		w.erc20ScanBackoff.failed(time.Now())
		return
	}
	w.erc20ScanBackoff.succeeded()

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
	for i := range logs {
		lg := logs[i]
		aw, found := byAddr[lg.Address]
		if !found {
			continue
		}
		occurrence := seen[lg.TxHash]
		seen[lg.TxHash] = occurrence + 1
		if err := w.storeTokenReceive(aw, &lg, receiveLogID(&lg, occurrence)); err != nil {
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
func (w *ETHWallet) storeTokenReceive(aw *assetWallet, lg *types.Log, id common.Hash) error {
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
			BlockNumber: lg.BlockNumber,
			TokenID:     &aw.assetID,
			Amount:      value,
			Recipient:   &addr,
			Confirmed:   true,
		},
	}
	if err := w.txDB.storeTx(wt); err != nil {
		return err
	}
	w.log.Infof("Detected incoming token %d transfer %s: %d atoms", aw.assetID, lg.TxHash, value)

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
// it. That balance is read pinned to block to - the end of the range about
// to be scanned - rather than the node's "latest", so the delta being
// compared always corresponds exactly to the range this call is about to
// cover; reading "latest" (which can already reflect blocks past to on a
// fast chain) can make an in-range receive look like it happened before the
// comparison baseline and get skipped as "unchanged". When the range exceeds
// maxScanBodies, only the oldest blocks in the range are checked in this
// call - the newest ones are left for a following tick - so a wallet that
// falls far behind converges over several ticks instead of permanently
// losing whatever didn't fit in one.
func (w *ETHWallet) scanNativeReceives(ctx context.Context, from, to uint64) (uint64, error) {
	bal, err := w.node.addressBalanceAt(ctx, w.addr, new(big.Int).SetUint64(to))
	if err != nil {
		return from - 1, err
	}
	unchanged := w.lastScanBalance != nil && bal.Cmp(w.lastScanBalance) == 0
	w.lastScanBalance = bal
	if unchanged {
		return to, nil
	}

	scanTo := to
	if to-from+1 > maxScanBodies {
		scanTo = from + maxScanBodies - 1
	}

	for n := from; n <= scanTo; n++ {
		block, err := w.node.blockByNumber(ctx, new(big.Int).SetUint64(n))
		if err != nil {
			return n - 1, err
		}
		for _, tx := range block.Transactions() {
			recipient := tx.To()
			if recipient == nil || *recipient != w.addr || tx.Value().Sign() <= 0 {
				continue
			}
			if err := w.storeNativeReceive(tx, block.NumberU64()); err != nil {
				w.log.Errorf("incoming transfer scan: error storing native receive %s: %v", tx.Hash(), err)
				return n - 1, err
			}
		}
	}
	return scanTo, nil
}

// storeNativeReceive builds and stores a Receive transaction for a discovered
// native-value transfer, mirroring ETHWallet.getReceivingTransaction's shape.
func (w *ETHWallet) storeNativeReceive(tx *types.Transaction, blockNumber uint64) error {
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
			BlockNumber: blockNumber,
			Amount:      w.atomize(tx.Value()),
			Recipient:   &addr,
			Confirmed:   true,
		},
	}
	if err := w.txDB.storeTx(wt); err != nil {
		return err
	}
	w.log.Infof("Detected incoming %s transfer %s: %d %s", w.ui.Conventional.Unit, tx.Hash(), wt.Amount, w.ui.AtomicUnit)

	if bal, err := w.balance(); err == nil {
		w.emit.BalanceChange(bal)
	}
	return nil
}
