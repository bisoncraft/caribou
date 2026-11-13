// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

// SyncState is the state of a wallet's connection to the chain.
type SyncState uint8

const (
	// SyncStopped means the wallet is not connected: Connect has not been
	// called, or syncing has stopped.
	SyncStopped SyncState = iota
	// SyncConnecting means the wallet has been connected and has not yet
	// heard from the server.
	SyncConnecting
	// SyncSyncing means the wallet is downloading and scanning blocks.
	SyncSyncing
	// SyncSynced means the wallet has scanned every block up to the
	// server's chain tip and is waiting for new blocks.
	SyncSynced
	// SyncDisconnected means the last exchange with the server failed. The
	// wallet retries with increasing delays.
	SyncDisconnected
)

// String returns the name of the state.
func (s SyncState) String() string {
	switch s {
	case SyncStopped:
		return "stopped"
	case SyncConnecting:
		return "connecting"
	case SyncSyncing:
		return "syncing"
	case SyncSynced:
		return "synced"
	case SyncDisconnected:
		return "disconnected"
	default:
		return fmt.Sprintf("SyncState(%d)", uint8(s))
	}
}

// SyncStatus describes how far a wallet is in following the chain.
type SyncStatus struct {
	State SyncState
	// ChainTip is the height of the server's chain tip as last reported.
	// It is zero until the server has been reached.
	ChainTip uint32
	// FullyScanned is the height up to which the wallet has scanned every
	// block since its birthday. It is zero until the first block is
	// scanned.
	FullyScanned uint32
	// ScanProgress and RecoveryProgress are as in Summary.
	ScanProgress     Ratio
	RecoveryProgress Ratio
	Recovering       bool
	// Err is the error that ended the last sync attempt. It is nil after
	// an attempt that succeeded. The wallet retries on its own whatever the
	// error is. ErrWrongNetwork and ErrReorgTooDeep do not go away without
	// intervention.
	Err error
}

// equal reports whether two statuses would look the same to a caller.
func (s *SyncStatus) equal(o *SyncStatus) bool {
	if s.State != o.State || s.ChainTip != o.ChainTip || s.FullyScanned != o.FullyScanned ||
		s.ScanProgress != o.ScanProgress || s.RecoveryProgress != o.RecoveryProgress ||
		s.Recovering != o.Recovering {
		return false
	}
	if (s.Err == nil) != (o.Err == nil) {
		return false
	}
	return s.Err == nil || s.Err.Error() == o.Err.Error()
}

const (
	defaultPollInterval = 20 * time.Second
	defaultBatchSize    = 1000
	// defaultMaxBatchBytes bounds the memory held by one batch of blocks. A
	// batch is cut short when it reaches this size, whatever its block
	// count.
	defaultMaxBatchBytes = 64 << 20
	// defaultMinBackoff and defaultMaxBackoff bound the delay before a
	// retry after a failed sync attempt. The delay doubles with each
	// consecutive failure.
	defaultMinBackoff = 2 * time.Second
	defaultMaxBackoff = 10 * time.Minute
	// defaultAddressCheckInterval is the average time between two checks
	// of a transparent address that the wallet checks on a schedule.
	defaultAddressCheckInterval = 10 * time.Minute

	// rewindDistance is how far below a block that does not connect the
	// wallet rewinds. The distance grows by this much with each consecutive
	// failure to reconnect, up to maxRewindDistance.
	rewindDistance    = 10
	maxRewindDistance = 100
	// maxEnhanceRounds bounds how many times in one cycle the wallet is
	// asked for transaction data requests. Answering requests can produce
	// more of them.
	maxEnhanceRounds = 8
)

// syncOptions are the tunable parameters of a processor.
type syncOptions struct {
	pollInterval         time.Duration
	minBackoff           time.Duration
	maxBackoff           time.Duration
	batchSize            uint32
	maxBatchBytes        int
	minConfirmations     uint32
	addressCheckInterval time.Duration
	now                  func() time.Time
}

func syncOptionsFor(cfg *Config) syncOptions {
	opts := syncOptions{
		pollInterval:         cfg.PollInterval,
		minBackoff:           defaultMinBackoff,
		maxBackoff:           defaultMaxBackoff,
		batchSize:            cfg.BatchSize,
		maxBatchBytes:        defaultMaxBatchBytes,
		minConfirmations:     cfg.MinConfirmations,
		addressCheckInterval: defaultAddressCheckInterval,
		now:                  time.Now,
	}
	if opts.pollInterval <= 0 {
		opts.pollInterval = defaultPollInterval
	}
	if opts.batchSize == 0 {
		opts.batchSize = defaultBatchSize
	}
	return opts
}

// processor keeps a wallet database in sync with the chain. It is the
// counterpart of the block processor of the Zcash Android wallet SDK and
// follows the synchronization flow that zcash_client_backend documents.
//
// A processor is used by one goroutine.
type processor struct {
	log  util.Logger
	net  Network
	be   backend
	src  chainSource
	opts syncOptions
	// update applies a change to the wallet's sync status.
	update func(func(*SyncStatus))

	// prepared is set once the server has been identified and its subtree
	// roots stored. It is cleared by a failed cycle, so that both are done
	// again after a reconnect.
	prepared bool
	// chainErrors counts consecutive cycles that ended in a rewind.
	chainErrors int
	// utxoQueries holds, by account ID, what the last query for the
	// account's unspent transparent outputs covered: the chain tip, the
	// start height and the addresses. The server is asked again only when
	// one of them has changed.
	utxoQueries map[string]string
	// deferred holds the transaction data requests that failed for a
	// reason of their own, by request key.
	deferred map[string]deferredRequest
}

// deferredRequest is a transaction data request that is not tried again
// before retryAt.
type deferredRequest struct {
	failures int
	retryAt  time.Time
}

func newProcessor(log util.Logger, network Network, be backend, src chainSource, opts syncOptions, update func(func(*SyncStatus))) *processor {
	return &processor{
		log:    log,
		net:    network,
		be:     be,
		src:    src,
		opts:   opts,
		update: update,
	}
}

// run syncs the wallet until ctx is cancelled.
func (p *processor) run(ctx context.Context) {
	p.update(func(s *SyncStatus) {
		s.State = SyncConnecting
		s.Err = nil
	})

	failures := 0
	for {
		again, err := p.cycle(ctx)
		if ctx.Err() != nil {
			return
		}

		var wait time.Duration
		switch {
		case err != nil:
			failures++
			p.prepared = false
			p.utxoQueries = nil
			wait = backoff(failures, p.opts.minBackoff, p.opts.maxBackoff)
			p.log.Warnf("Sync failed, retrying in %s: %v", wait.Round(time.Millisecond), err)
			p.update(func(s *SyncStatus) {
				s.Err = err
				// An error of the wallet database says nothing about
				// the connection.
				if isServerError(err) || errors.Is(err, ErrWrongNetwork) {
					s.State = SyncDisconnected
				}
			})
		case again:
			failures = 0
			// The first rewind is retried at once. If it did not
			// reconnect the wallet to the chain, the chain may still be
			// changing, so later ones wait.
			if p.chainErrors > 1 {
				wait = backoff(p.chainErrors-1, p.opts.minBackoff, p.opts.maxBackoff)
			}
		default:
			failures = 0
			p.chainErrors = 0
			p.update(func(s *SyncStatus) {
				s.State = SyncSynced
				s.Err = nil
			})
			wait = jitter(p.opts.pollInterval)
		}

		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

// backoff returns the delay before the n-th consecutive retry: lo doubled
// n-1 times, capped at hi.
func backoff(n int, lo, hi time.Duration) time.Duration {
	d := lo
	for i := 1; i < n && d < hi; i++ {
		d *= 2
	}
	return min(d, hi)
}

// jitter returns d changed by up to a quarter in either direction.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.75 + 0.5*rand.Float64()))
}

// cycle brings the wallet up to the server's chain tip once. It returns
// true if it rewound the wallet and has to be run again at once.
func (p *processor) cycle(ctx context.Context) (again bool, err error) {
	if !p.prepared {
		if err := p.checkServer(ctx); err != nil {
			return false, err
		}
		if err := p.updateSubtreeRoots(ctx); err != nil {
			return false, err
		}
		p.prepared = true
	}

	tip, err := p.src.latestBlock(ctx)
	if err != nil {
		return false, err
	}
	if tip.Height > math.MaxUint32 {
		return false, &serverError{"GetLatestBlock", fmt.Errorf("height %d is out of range", tip.Height)}
	}
	tipHeight := uint32(tip.Height)
	p.update(func(s *SyncStatus) {
		s.ChainTip = tipHeight
		if s.State == SyncConnecting || s.State == SyncDisconnected {
			s.State = SyncSyncing
		}
	})

	rewound, err := p.checkTip(tipHeight, tip.Hash)
	if err != nil {
		return false, err
	}
	if rewound {
		return true, nil
	}
	if err := p.be.updateChainTip(tipHeight); err != nil {
		return false, err
	}
	if err := p.refreshUTXOs(ctx, tipHeight, tip.Hash); err != nil {
		return false, err
	}

	// The wallet orders the ranges it wants scanned, and scanning changes
	// that order: finding a note raises the priority of the blocks that
	// make it spendable. So the list is fetched again after every batch and
	// only its first range is acted on.
	var last scanRange
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		ranges, err := p.be.suggestScanRanges()
		if err != nil {
			return false, err
		}
		// A range above the server's tip cannot be scanned yet. The wallet
		// suggests one when it was last told of a higher tip than this
		// server has, by a server that was further along.
		i := slices.IndexFunc(ranges, func(r scanRange) bool { return r.start <= tipHeight })
		if i < 0 {
			break
		}
		next := ranges[i]
		if next == last {
			// The batch scanned from this range did not shorten it.
			return false, wrapf(ErrInternal, "scanning %s made no progress", next)
		}
		last = next

		p.update(func(s *SyncStatus) { s.State = SyncSyncing })
		summary, err := p.scanBatch(ctx, next, tipHeight)
		var discontinuity *chainContinuityError
		if errors.As(err, &discontinuity) {
			p.log.Warnf("Chain reorganization detected at height %d: %s",
				discontinuity.height, discontinuity.reason)
			// The block at that height does not connect to the wallet's
			// block below it, so the wallet's block is the stale one.
			if err := p.rewind(discontinuity.height-min(discontinuity.height, 1), tipHeight); err != nil {
				return false, err
			}
			return true, nil
		}
		if err != nil {
			return false, err
		}
		p.log.Debugf("Scanned blocks %d to %d of range %s: %d notes received, %d spent",
			summary.start, summary.end-1, next, summary.received, summary.spent)
		if err := p.refreshSummary(); err != nil {
			return false, err
		}
	}

	if err := p.enhance(ctx, tipHeight); err != nil {
		return false, err
	}
	return false, p.refreshSummary()
}

// checkServerChain verifies that a server follows the chain of network and
// returns what the server says about itself.
func checkServerChain(ctx context.Context, src chainSource, network Network) (*walletrpc.LightdInfo, error) {
	info, err := src.info(ctx)
	if err != nil {
		return nil, err
	}
	if !network.matchesChainName(info.ChainName) {
		return nil, fmt.Errorf("%w: the wallet is on %s and the server reports chain %q",
			ErrWrongNetwork, network, info.ChainName)
	}
	return info, nil
}

// checkServer verifies that the server follows the wallet's chain.
func (p *processor) checkServer(ctx context.Context) error {
	info, err := checkServerChain(ctx, p.src, p.net)
	if err != nil {
		return err
	}
	p.log.Debugf("Connected to lightwalletd %s %s on chain %q at height %d",
		info.Vendor, info.Version, info.ChainName, info.BlockHeight)
	return nil
}

// updateSubtreeRoots stores the roots of the completed subtrees of the
// Ironwood note commitment tree. With them the wallet can scan the chain out
// of order, so that recent funds become spendable before the whole history
// is scanned.
//
// The roots of the Sapling and Orchard trees are not fetched. The wallet
// holds no notes in those trees, so it never needs a witness in them.
//
// A server that does not serve the roots is treated as having none. That is
// the case for a server that predates the call, and for one that predates
// the pool. Without the roots the wallet finds the same funds by scanning
// from its birthday.
func (p *processor) updateSubtreeRoots(ctx context.Context) error {
	var roots subtreeRoots
	summary, err := p.be.summary(p.opts.minConfirmations)
	if err != nil {
		return err
	}
	if summary != nil {
		roots.start = summary.nextSubtree
	}

	roots.roots, err = p.src.subtreeRoots(ctx, walletrpc.ShieldedProtocol_ironwood, roots.start)
	switch {
	case err != nil && isUnsupported(err):
		p.log.Warnf("The server does not provide Ironwood subtree roots, continuing without them: %v", err)
		return nil
	case err != nil:
		return err
	}
	p.log.Debugf("The server has %d Ironwood subtree roots from index %d on", len(roots.roots), roots.start)
	return p.be.putSubtreeRoots(&roots)
}

// sameHash reports whether two block hashes are equal. Servers have not
// always agreed on the byte order of the hash in a block ID, so a hash also
// matches its reverse.
func sameHash(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	reversed := slices.Clone(b)
	slices.Reverse(reversed)
	return bytes.Equal(a, reversed)
}

// checkTip looks for a reorganization that left the chain no longer than
// what the wallet has scanned. The wallet does not see one of those by
// scanning, because it has nothing new to scan until the chain grows past
// its scanned height. It reports whether it rewound the wallet.
//
// A tip below the scanned height is not by itself a reorganization: a server
// that is behind reports one too. The wallet is only rewound if its block at
// the tip height is not the server's.
func (p *processor) checkTip(tipHeight uint32, tipHash []byte) (rewound bool, err error) {
	maxScanned, ok, err := p.be.maxScannedHeight()
	if err != nil {
		return false, err
	}
	if !ok || tipHeight > maxScanned || len(tipHash) != 32 {
		return false, nil
	}
	stored, err := p.be.blockHash(tipHeight)
	if err != nil {
		return false, err
	}
	if stored == nil || sameHash(stored, tipHash) {
		return false, nil
	}
	p.log.Warnf("Chain reorganization detected: the block at the tip height %d is not the one "+
		"the wallet scanned (scanned up to %d)", tipHeight, maxScanned)
	return true, p.rewind(tipHeight, tipHeight)
}

// rewind discards the wallet's view of the chain from some distance below
// staleHeight, the greatest height at which the wallet is known to hold a
// block that is no longer part of the chain.
func (p *processor) rewind(staleHeight, tipHeight uint32) error {
	p.chainErrors++
	distance := uint32(min(rewindDistance*p.chainErrors, maxRewindDistance))
	target := staleHeight - min(staleHeight, distance)

	height, err := p.be.rewindToHeight(target)
	var invalid *rewindError
	if errors.As(err, &invalid) {
		// The wallet can only rewind to a height where it kept a
		// checkpoint of its note commitment trees. If the lowest such
		// height does not remove the stale block, rewinding cannot help.
		if !invalid.hasSafe || invalid.safe >= staleHeight {
			return fmt.Errorf("%w: the wallet's block at height %d was replaced", ErrReorgTooDeep, staleHeight)
		}
		height, err = p.be.rewindToHeight(invalid.safe)
	}
	if err != nil {
		return err
	}
	p.log.Infof("Rewound the wallet to height %d", height)
	// Rewinding also forgets where the transparent outputs above the rewind
	// height were mined, so they are fetched again even at an unchanged
	// tip.
	p.utxoQueries = nil

	// Rewinding shortens the wallet's scan queue to the rewind height.
	// Without telling it the tip again it would consider itself synced
	// there.
	if err := p.be.updateChainTip(tipHeight); err != nil {
		return err
	}
	return p.refreshSummary()
}

// refreshUTXOs records the unspent transparent outputs that the server
// reports for the addresses of each account.
//
// The server reports mined outputs only, so its answer for the same
// addresses and start height changes only with the chain tip. An account is
// asked about again when one of the three has changed, and not on every
// poll: each query shows the server all of the account's addresses together.
func (p *processor) refreshUTXOs(ctx context.Context, tipHeight uint32, tipHash []byte) error {
	accounts, err := p.be.accounts()
	if err != nil {
		return err
	}
	for i := range accounts {
		id := accounts[i].ID
		addresses, err := p.be.transparentReceivers(id)
		if err != nil {
			return err
		}
		if len(addresses) == 0 {
			continue
		}
		start, err := p.be.utxoQueryHeight(id)
		if err != nil {
			return err
		}
		query := fmt.Sprintf("%d %x %d %s", tipHeight, tipHash, start, strings.Join(addresses, " "))
		if p.utxoQueries[id] == query {
			continue
		}
		err = p.src.addressUTXOs(ctx, addresses, start, func(u *walletrpc.GetAddressUtxosReply) error {
			if u.ValueZat < 0 || u.Index < 0 || u.Height > math.MaxUint32 {
				return &serverError{"GetAddressUtxosStream", errors.New("malformed output")}
			}
			return p.be.putUTXO(&utxo{
				txid:   u.Txid,
				index:  uint32(u.Index),
				script: u.Script,
				value:  uint64(u.ValueZat),
				height: uint32(u.Height),
			})
		})
		if err != nil {
			return err
		}
		if p.utxoQueries == nil {
			p.utxoQueries = make(map[string]string)
		}
		p.utxoQueries[id] = query
	}
	return nil
}

// batchEnd returns the last height of the batch that starts a scan of r.
//
// A batch does not have to end on a particular height for the wallet to keep
// the note commitment tree states it later rewinds to or anchors a spend to.
// The wallet checkpoints the state a batch starts from, every block that
// adds a note commitment, and every height of the ZIP 318 anchor grid inside
// a batch.
func (p *processor) batchEnd(r scanRange, tipHeight uint32) uint32 {
	end := min(r.end-1, tipHeight)
	if end-r.start >= p.opts.batchSize {
		end = r.start + p.opts.batchSize - 1
	}
	return end
}

// scanBatch downloads the first blocks of a scan range and has the wallet
// scan them.
func (p *processor) scanBatch(ctx context.Context, r scanRange, tipHeight uint32) (*scanSummary, error) {
	end := p.batchEnd(r, tipHeight)
	op := fmt.Sprintf("GetBlockRange(%d, %d)", r.start, end)

	blocks := make([][]byte, 0, end-r.start+1)
	var prevHash []byte
	size := 0
	err := p.src.blockRange(ctx, r.start, end, func(b *walletrpc.CompactBlock) error {
		want := uint64(r.start) + uint64(len(blocks))
		if b.Height != want {
			return &serverError{op, fmt.Errorf("got block %d, want %d", b.Height, want)}
		}
		if want > uint64(end) {
			return &serverError{op, fmt.Errorf("got block %d, past the end of the range", b.Height)}
		}
		if len(blocks) == 0 {
			prevHash = b.PrevHash
		}
		encoded, err := proto.Marshal(b)
		if err != nil {
			return wrapf(ErrInternal, "encoding block %d: %v", b.Height, err)
		}
		blocks = append(blocks, encoded)
		size += len(encoded)
		if size >= p.opts.maxBatchBytes {
			return errStopStream
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		// The server's chain is shorter than it said, which happens while
		// it follows a reorganization.
		return nil, &serverError{op, errors.New("the server returned no blocks")}
	}

	state, err := chainStateBefore(ctx, p.src, p.net, r.start, prevHash)
	if err != nil {
		return nil, err
	}
	return p.be.scanBlocks(state, blocks)
}

// minedHeight returns the height a transaction was mined at in the main
// chain, or nil. A height of zero means the transaction is in the mempool,
// and a height that does not fit a block height is the marker for a
// transaction mined on a fork.
func minedHeight(tx *walletrpc.RawTransaction) *uint32 {
	if tx.Height == 0 || tx.Height > math.MaxUint32 {
		return nil
	}
	h := uint32(tx.Height)
	return &h
}

// enhance fetches the transaction data that the wallet asks for: the status
// of transactions it cannot see being mined by scanning, full transactions
// for their memos and transparent parts, and the transparent history of its
// addresses.
//
// A request that fails for a reason of its own does not fail the cycle. The
// wallet keeps making such a request, so failing the cycle would put the
// wallet into its retry backoff for as long as the request stays open, and
// new blocks would be picked up at the backoff's pace. The request is put
// off and the others are answered. Only an error that would fail every
// request ends the cycle: a cancelled context, a failure of the connection
// or of the server as a whole, or a failure of the wallet database.
func (p *processor) enhance(ctx context.Context, tipHeight uint32) error {
	// The wallet repeats requests it still considers open, so the ones
	// handled in this cycle are remembered.
	handled := make(map[string]struct{})
	for range maxEnhanceRounds {
		requests, err := p.be.transactionDataRequests()
		if err != nil {
			return err
		}
		fresh := false
		for i := range requests {
			req := &requests[i]
			key := req.key()
			if _, done := handled[key]; done {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			handled[key] = struct{}{}
			if d, ok := p.deferred[key]; ok && p.opts.now().Before(d.retryAt) {
				continue
			}
			err := p.answer(ctx, req, tipHeight)
			switch {
			case err == nil:
				delete(p.deferred, key)
				fresh = true
			case ctx.Err() != nil || errors.Is(err, ErrDatabase) || isConnectionError(err):
				return err
			default:
				p.deferRequest(key, req, err)
			}
		}
		if !fresh {
			break
		}
	}
	// A deferred request that the wallet has stopped making is forgotten.
	for key := range p.deferred {
		if _, open := handled[key]; !open {
			delete(p.deferred, key)
		}
	}
	return nil
}

// deferRequest puts off a transaction data request that failed. The delay
// doubles with each consecutive failure of the request, like the delay
// between failed sync attempts.
func (p *processor) deferRequest(key string, req *txRequest, err error) {
	d := p.deferred[key]
	d.failures++
	wait := backoff(d.failures, p.opts.minBackoff, p.opts.maxBackoff)
	d.retryAt = p.opts.now().Add(wait)
	if p.deferred == nil {
		p.deferred = make(map[string]deferredRequest)
	}
	p.deferred[key] = d
	p.log.Warnf("The request for %s failed and is put off for %s: %v", req, wait.Round(time.Millisecond), err)
}

// answer fulfills one transaction data request.
func (p *processor) answer(ctx context.Context, req *txRequest, tipHeight uint32) error {
	switch req.kind {
	case txRequestStatus:
		tx, err := p.src.transaction(ctx, req.txid)
		if err != nil {
			return err
		}
		status := txStatus{kind: txNotRecognized}
		if tx != nil {
			status.kind = txNotInMainChain
			if h := minedHeight(tx); h != nil {
				status = txStatus{kind: txMined, height: *h}
			}
		}
		return p.be.setTransactionStatus(req.txid, status)

	case txRequestEnhancement:
		tx, err := p.src.transaction(ctx, req.txid)
		if err != nil {
			return err
		}
		if tx == nil {
			return p.be.setTransactionStatus(req.txid, txStatus{kind: txNotRecognized})
		}
		_, err = p.be.decryptAndStoreTransaction(tx.Data, minedHeight(tx))
		return err

	case txRequestAddress:
		// A request is due once the second it is scheduled for has passed.
		// The wallet keeps times in whole seconds and gives a standing
		// check a new time only when its current one lies before the
		// present second. A check made within the scheduled second would
		// leave the request as it is, and it would be made again in every
		// cycle until that second is over.
		if !req.requestAt.IsZero() && req.requestAt.Unix() >= p.opts.now().Unix() {
			return nil
		}
		// last is the last height the query covers.
		last := tipHeight
		if req.bounded {
			if req.end == 0 {
				return nil
			}
			last = req.end - 1
		}
		// A range that reaches above the server's tip is left for a later
		// cycle. Closing it now would tell the wallet that blocks were
		// checked which the server does not have yet.
		if req.start > last || last > tipHeight {
			return nil
		}
		// The server call returns mined transactions only. A request whose
		// status filter includes the mempool is therefore answered for
		// its mined part alone, and closing it below tells the wallet of a
		// mempool check that was not made. The wallet asks for the mempool
		// in its checks of ephemeral addresses, which hold funds only
		// while a payment to a TEX address (ZIP 320) is under way. Sending
		// such payments needs the mempool part answered as well.
		err := p.src.addressTransactions(ctx, req.address, req.start, last, func(tx *walletrpc.RawTransaction) error {
			_, err := p.be.decryptAndStoreTransaction(tx.Data, minedHeight(tx))
			return err
		})
		if err != nil {
			return err
		}
		if err := p.be.notifyAddressChecked(req, last); err != nil {
			return err
		}
		if !req.bounded {
			// An open-ended request is a standing check of the address.
			// The wallet repeats it once its scheduled time has passed.
			if _, _, err := p.be.scheduleNextCheck(req.address, p.opts.addressCheckInterval); err != nil {
				return err
			}
		}
		return nil

	default:
		return wrapf(ErrInternal, "unknown transaction data request kind %d", req.kind)
	}
}

// refreshSummary copies the wallet's scan state into the sync status.
func (p *processor) refreshSummary() error {
	summary, err := p.be.summary(p.opts.minConfirmations)
	if err != nil {
		return err
	}
	if summary == nil {
		return nil
	}
	p.update(func(s *SyncStatus) {
		s.FullyScanned = summary.FullyScanned
		s.ScanProgress = summary.ScanProgress
		s.RecoveryProgress = summary.RecoveryProgress
		s.Recovering = summary.Recovering
	})
	return nil
}
