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
	// incomingScanTick is how often the wallet checks for incoming ERC20
	// transfers.
	incomingScanTick = 15 * time.Second
	// maxScanBlocks caps the block range covered by a single ERC20 log-filter
	// call per tick, and is also the outer bound on how far behind tip a
	// scan will ever look: a wallet that falls further behind than this
	// (e.g. after being offline) gives up on the gap and only scans the most
	// recent maxScanBlocks blocks (see scanERC20Tick) rather than backfilling
	// it, since RPC providers cannot be relied on to serve older history.
	maxScanBlocks = 128

	// scanBackoffMin and scanBackoffMax bound the cooldown applied after the
	// ERC20 scan fails, so a provider that will never succeed at a given call
	// (e.g. eth_getLogs disabled entirely on a free public endpoint) doesn't
	// get hammered - and doesn't spam the log - every incomingScanTick
	// forever.
	scanBackoffMin = 30 * time.Second
	scanBackoffMax = 30 * time.Minute

	scanMetaTableName          = "meta"
	lastScannedERC20BlockField = "lastScannedERC20Block"
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

// failed records a scan failure and applies the exponential backoff.
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

	from := last + 1
	// RPC providers phrase "can't serve this range" in too many ways to
	// classify reliably, so rather than trying to detect an unscannable
	// range from its error, every tick unconditionally clamps to the most
	// recent maxScanBlocks blocks. A wallet that fell further behind than
	// that (offline for a while, or stuck failing) has that gap silently
	// given up on instead of retried forever against a provider that will
	// never serve it.
	oldest := uint64(1)
	if tipNum >= maxScanBlocks {
		oldest = tipNum - maxScanBlocks + 1
	}
	if from < oldest {
		w.log.Warnf("incoming transfer scan: erc20 scan cursor %d is more than %d blocks behind tip %d; "+
			"abandoning the backlog - transfers in %d-%d will not be detected", last, maxScanBlocks, tipNum, from, oldest-1)
		from = oldest
	}
	// The clamp above already guarantees to-from+1 <= maxScanBlocks: from is
	// never more than maxScanBlocks-1 below tipNum once raised to oldest, so
	// no further width cap is needed here (unlike the old narrowing design,
	// where from could still be far behind a variable-width window).
	to := tipNum

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
