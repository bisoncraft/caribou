// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	// server's chain tip and is waiting for new blocks. ChainTip is then
	// the tip the wallet is synced to: a new block shows in it once the
	// wallet has left this state to scan the block.
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
	// an attempt that succeeded, except for ErrOutputUnrecorded, which an
	// attempt that succeeded reports together with SyncSynced. The wallet
	// retries on its own whatever the error is. ErrWrongNetwork,
	// ErrReorgTooDeep and an ErrConsensusBranch that every server gives do
	// not go away without intervention. Err does not say by itself that the
	// server is lost: State does.
	Err error
	// TransparentErr is the error of the last failed attempt to update the
	// wallet's transparent addresses: to fetch their history and their
	// unspent outputs. It is nil once an attempt has succeeded. While it is
	// set, the transparent balance and the addresses the wallet watches can
	// be out of date. It does not end a sync attempt or change State: a
	// wallet whose server fails these requests keeps scanning blocks and
	// can be SyncSynced.
	TransparentErr error
}

// equal reports whether two statuses would look the same to a caller.
func (s *SyncStatus) equal(o *SyncStatus) bool {
	if s.State != o.State || s.ChainTip != o.ChainTip || s.FullyScanned != o.FullyScanned ||
		s.ScanProgress != o.ScanProgress || s.RecoveryProgress != o.RecoveryProgress ||
		s.Recovering != o.Recovering {
		return false
	}
	return sameError(s.Err, o.Err) && sameError(s.TransparentErr, o.TransparentErr)
}

// sameError reports whether two errors would look the same to a caller.
func sameError(a, b error) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || a.Error() == b.Error()
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
	// defaultStaleLag is the number of blocks by which the node of a server
	// can estimate the chain to be ahead of its own tip before the server
	// counts as behind. A node estimates from the time since its tip block,
	// at one block in 75 seconds, so 24 blocks are half an hour without a
	// new one. If blocks arrive as a Poisson process, a gap of that length
	// has a probability of about 4e-11.
	defaultStaleLag = 24
	// defaultServerCheckInterval is the time between two checks of the
	// server that a wallet is connected to.
	defaultServerCheckInterval = 10 * time.Minute

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
	// staleLag is the number of blocks behind at which a server counts as
	// stale. Zero turns the check off.
	staleLag            uint64
	serverCheckInterval time.Duration
	// rebroadcastMinDelay and rebroadcastMaxDelay bound the delay between
	// two checks of a transaction the wallet created (see rebroadcast).
	rebroadcastMinDelay time.Duration
	rebroadcastMaxDelay time.Duration
	// onChange is Config.OnChange, and onRewind Config.OnRewind.
	onChange func()
	onRewind func(height uint32)
	now      func() time.Time
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
		serverCheckInterval:  defaultServerCheckInterval,
		rebroadcastMinDelay:  defaultRebroadcastMinDelay,
		rebroadcastMaxDelay:  defaultRebroadcastMaxDelay,
		onChange:             cfg.OnChange,
		onRewind:             cfg.OnRewind,
		now:                  time.Now,
	}
	// A regtest chain grows when someone mines, and its tip can be of any
	// age.
	if cfg.Network.Type != NetworkRegtest {
		opts.staleLag = defaultStaleLag
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
	// server is the server that src is connected to.
	server endpoint
	// servers is the number of servers the wallet can use, and nextServer
	// connects to the one after a server that failed. Both are unset for a
	// wallet with a single server.
	servers    int
	nextServer func(failed endpoint) (chainSource, endpoint, error)
	// answered is set once the server in use has given the wallet a batch
	// of blocks or brought it up to the chain tip, and serverFailures
	// counts the attempts it has failed since it last did either.
	answered       bool
	serverFailures int
	// rotations counts the servers the wallet has moved on from since a
	// server last did either.
	rotations int
	// progressed is set when the sync attempt under way has given the
	// wallet a batch of blocks.
	progressed bool
	// checkedAt is when the server in use last passed checkServer.
	checkedAt time.Time
	// staleTour is set from the time the wallet leaves a server whose node
	// is behind until a server works for it or it settles on a stale one.
	// staleFrom is the server the tour began at, and staleRotations the
	// value of rotations at that time.
	staleTour      bool
	staleFrom      endpoint
	staleRotations int

	// walked holds the transparent addresses whose history has been
	// fetched since the processor was started or last failed a cycle.
	walked map[string]struct{}
	// walkQueue holds the transparent addresses whose history is still to
	// be fetched, each with the height its history is asked for from, the
	// birthday of its account. An address stays in it until that has
	// succeeded, also when it is no longer unused by then.
	walkQueue map[string]uint32
	// walkDeferred holds the addresses whose history request failed for a
	// reason of its own, by address.
	walkDeferred map[string]deferredRequest
	// transparentFailures counts the consecutive failures of the requests
	// for the transparent addresses, which are not made again before
	// transparentRetryAt.
	transparentFailures int
	transparentRetryAt  time.Time

	// fundDeferred holds the funding transactions of unspent outputs that
	// could not be stored, by transaction ID, for the accounts of every
	// account's query.
	fundDeferred map[string]deferredRequest
	// recordedUTXOs holds the unspent outputs this processor has recorded,
	// by transaction ID, index and height, so that recording one again
	// reports no change. A rewind clears it.
	recordedUTXOs map[string]struct{}
	// unrecorded holds, by account ID, why some of the account's unspent
	// outputs could not be recorded in its last query. It is reported in
	// SyncStatus.Err after a cycle that otherwise succeeded.
	unrecorded map[string]error

	// rebroadcasts holds the rebroadcast state of the transactions in the
	// wallet's rebroadcast inventory, by transaction ID, and rebroadcastAt
	// the earliest time one of them is due for a check, zero if none is.
	rebroadcasts  map[string]*rebroadcastState
	rebroadcastAt time.Time
	// inventoryReadAt is when the rebroadcast inventory was last read, and
	// checksHeldUntil the time before which no transaction is checked,
	// after a check found the server unreachable.
	inventoryReadAt time.Time
	checksHeldUntil time.Time

	// changePending is set while a write to the wallet database waits to be
	// reported through Config.OnChange, and lastChange is when it was last
	// called.
	changePending bool
	lastChange    time.Time
}

// deferredRequest is a transaction data request that is not tried again
// before retryAt.
type deferredRequest struct {
	failures int
	retryAt  time.Time
	// err is the failure that deferred it.
	err error
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
		s.TransparentErr = nil
	})

	failures := 0
	for {
		p.progressed = false
		again, err := p.cycle(ctx)
		p.flushChange()
		if ctx.Err() != nil {
			return
		}

		var wait time.Duration
		switch {
		case err != nil:
			// An attempt that brought the wallet blocks before it failed
			// is followed up as soon as a first failure is. A restore over
			// a poor connection would otherwise wait longer after every
			// lost connection, up to maxBackoff, between attempts that
			// each take it further.
			if p.progressed {
				failures = 0
			}
			failures++
			p.prepared = false
			// While the wallet was out of touch, an unused address can
			// have been paid and emptied again, which no later query for
			// unspent outputs shows. The unused addresses are asked about
			// again in the next attempt, also when a failure of those
			// requests had put them off: the next attempt can be with
			// another server.
			p.walked = nil
			p.utxoQueries = nil
			p.transparentFailures, p.transparentRetryAt = 0, time.Time{}
			wait = backoff(failures, p.opts.minBackoff, p.opts.maxBackoff)
			if failed := p.server; p.rotateAfter(err) {
				// A server that has not been tried since the last success
				// is tried at once. After a round of all of them the
				// wallet waits, longer with each round.
				wait = 0
				if p.rotations%p.servers == 0 {
					wait = backoff(p.rotations/p.servers, p.opts.minBackoff, p.opts.maxBackoff)
				}
				p.log.Warnf("Sync with %s failed, moving on to %s in %s: %v",
					failed.address, p.server.address, wait.Round(time.Millisecond), err)
			} else {
				p.log.Warnf("Sync failed, retrying in %s: %v", wait.Round(time.Millisecond), err)
			}
			p.update(func(s *SyncStatus) {
				s.Err = err
				// An error of the wallet database says nothing about
				// the connection.
				if isServerError(err) || unfit(err) {
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
			unrecorded := p.unrecordedErr()
			p.update(func(s *SyncStatus) {
				s.State = SyncSynced
				s.Err = unrecorded
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

// unfit reports whether err means that a server is of no use to the wallet
// whatever it answers next: it is on another network, follows other
// consensus rules, or its node is behind the chain.
func unfit(err error) bool {
	return errors.Is(err, ErrWrongNetwork) || errors.Is(err, ErrConsensusBranch) || errors.Is(err, ErrServerStale)
}

// countsAgainstServer reports whether err, which ended an attempt to use a
// server, is a failure of the server: the server is unfit, cannot be reached,
// failed a call, or gave a malformed reply. A call that the server rejected
// as invalid does not count. Every server rejects a request that the wallet
// got wrong in that way, and moving on would only show the request to the
// next server.
//
// A call fails when the node behind lightwalletd does, and the error is then
// not a connection error: lightwalletd answers with a status such as Internal
// or Unknown while its node restarts.
func countsAgainstServer(err error) bool {
	if unfit(err) {
		return true
	}
	return isServerError(err) && status.Code(err) != codes.InvalidArgument
}

// serverWorked records that the server in use is serving the wallet. It is
// called for every batch of blocks and not only when the wallet has reached
// the server's chain tip: a restore is one attempt that can run for hours,
// and a connection lost in the course of it is no reason to turn to another
// server.
func (p *processor) serverWorked() {
	p.answered, p.serverFailures, p.rotations = true, 0, 0
	p.staleTour = false
}

// rotateAfter moves the wallet to the next server if err counts against the
// one in use, and reports whether it did.
//
// A server that has been working gets a second attempt first. One failed
// call is not an outage, and every further server the wallet uses learns its
// transparent addresses. A server that has given the wallet nothing since
// the wallet turned to it, or that is unfit, is left at once.
func (p *processor) rotateAfter(err error) bool {
	if p.nextServer == nil || !countsAgainstServer(err) {
		return false
	}
	p.serverFailures++
	if p.answered && p.serverFailures < 2 && !unfit(err) {
		return false
	}
	src, next, dialErr := p.nextServer(p.server)
	if dialErr != nil {
		// No server could be connected to, the one in use included. It
		// stays in use.
		return false
	}
	if closeErr := p.src.close(); closeErr != nil {
		p.log.Debugf("Closing the connection to %s: %v", p.server.address, closeErr)
	}
	p.src, p.server = src, next
	p.answered, p.serverFailures = false, 0
	p.rotations++
	return true
}

// cycle brings the wallet up to the server's chain tip once. It returns
// true if it rewound the wallet and has to be run again at once.
//
// An attempt has four steps: the chain tip, the transactions the wallet
// created that are not mined, the transparent addresses, and the blocks. The
// second is after the first so that a transaction past its expiry at the new
// tip is not sent again. The third is before the fourth so that a restore,
// which can scan for hours in one attempt, learns of transparent funds first.
// A failure of the server to answer for the transparent addresses does not
// end the attempt: syncAddresses reports it on its own.
func (p *processor) cycle(ctx context.Context) (again bool, err error) {
	tip, again, err := p.syncTip(ctx)
	if err != nil || again {
		return again, err
	}
	if err := p.rebroadcast(ctx); err != nil {
		return false, err
	}
	if err := p.syncAddresses(ctx, tip); err != nil {
		return false, err
	}
	return p.syncBlocks(ctx, uint32(tip.Height))
}

// syncTip fetches the server's chain tip, rewinds the wallet if its block at
// the tip height is not the server's, and tells the wallet the tip. On the
// first attempt with a server, it checks the server and stores its subtree
// roots first. It returns true if it rewound the wallet.
func (p *processor) syncTip(ctx context.Context) (tip *walletrpc.BlockID, rewound bool, err error) {
	if !p.prepared {
		if err := p.checkServer(ctx); err != nil {
			return nil, false, err
		}
		if err := p.updateSubtreeRoots(ctx); err != nil {
			return nil, false, err
		}
		p.prepared = true
	}
	if err := p.checkServerWhenDue(ctx); err != nil {
		return nil, false, err
	}

	tip, err = p.src.latestBlock(ctx)
	if err != nil {
		return nil, false, err
	}
	if tip.Height > math.MaxUint32 {
		return nil, false, &serverError{"GetLatestBlock", fmt.Errorf("height %d is out of range", tip.Height)}
	}
	tipHeight := uint32(tip.Height)
	p.update(func(s *SyncStatus) {
		if s.State == SyncConnecting || s.State == SyncDisconnected {
			s.State = SyncSyncing
		}
		// A synced wallet keeps reporting the tip it is synced to until it
		// starts on the blocks above it or finds that there is nothing to
		// scan. SyncSynced is then never reported together with a tip the
		// wallet has yet to scan up to.
		if s.State != SyncSynced {
			s.ChainTip = tipHeight
		}
	})

	rewound, err = p.checkTip(tipHeight, tip.Hash)
	if err != nil || rewound {
		return nil, rewound, err
	}
	if err := p.be.updateChainTip(tipHeight); err != nil {
		return nil, false, err
	}
	return tip, false, nil
}

// syncAddresses fetches the history of the transparent addresses that are
// due to be asked about, and records the unspent outputs of every account's
// addresses.
//
// A failure of the server to answer these requests does not end the sync
// attempt, and a server that serves blocks is kept: the next server would
// only learn the wallet's addresses. The error is reported in
// SyncStatus.TransparentErr, and the requests are made again after a delay
// that grows with each consecutive failure. Without the delay a server that
// keeps failing them would be sent every address of the wallet at every
// poll. The error that syncAddresses returns is one that ends the attempt: a
// failed check of the server, an error of the wallet database, or a
// cancelled context.
func (p *processor) syncAddresses(ctx context.Context, tip *walletrpc.BlockID) error {
	if p.opts.now().Before(p.transparentRetryAt) {
		return nil
	}
	tipHeight := uint32(tip.Height)
	var failed error
	// Unspent outputs can move the window of watched addresses as well, so
	// the walk is repeated until it finds nothing to do.
	for round := 0; ; round++ {
		walked, walkFailed, err := p.walkAddresses(ctx, tipHeight)
		if err != nil {
			return err
		}
		failed = walkFailed
		if walked == 0 && round > 0 {
			break
		}
		utxoFailed, err := p.refreshUTXOs(ctx, tipHeight, tip.Hash)
		if err != nil {
			return err
		}
		if failed == nil {
			failed = utxoFailed
		}
		if failed != nil {
			break
		}
	}

	if failed == nil {
		p.transparentFailures, p.transparentRetryAt = 0, time.Time{}
	} else {
		p.transparentFailures++
		wait := backoff(p.transparentFailures, p.opts.minBackoff, p.opts.maxBackoff)
		p.transparentRetryAt = p.opts.now().Add(wait)
		p.log.Warnf("The requests for the transparent addresses failed and are put off for %s: %v",
			wait.Round(time.Millisecond), failed)
	}
	p.update(func(s *SyncStatus) { s.TransparentErr = failed })
	return nil
}

// syncBlocks scans the blocks the wallet wants scanned up to the server's
// chain tip, then answers the wallet's requests for transaction data. It
// returns true if it rewound the wallet.
func (p *processor) syncBlocks(ctx context.Context, tipHeight uint32) (rewound bool, err error) {
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

		p.update(func(s *SyncStatus) {
			s.State = SyncSyncing
			s.ChainTip = tipHeight
		})
		if err := p.checkServerWhenDue(ctx); err != nil {
			return false, err
		}
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
		p.serverWorked()
		p.progressed = true
		p.noteChange()
		p.log.Debugf("Scanned blocks %d to %d of range %s", summary.start, summary.end-1, next)
		if err := p.refreshSummary(); err != nil {
			return false, err
		}
		// A long scan, such as a catch-up after the wallet slept, could
		// outlast the expiry of a transaction waiting to be broadcast.
		if p.rebroadcastDue() {
			if err := p.rebroadcast(ctx); err != nil {
				return false, err
			}
		}
	}

	// The server has brought the wallet up to its chain tip. The requests
	// that follow are for single transactions and for the history of
	// addresses. A failure of those is reported and does not count against
	// the server: leaving a server that serves blocks would only tell the
	// next one the wallet's addresses and transactions.
	p.serverWorked()
	if err := p.enhance(ctx, tipHeight); err != nil {
		return false, err
	}
	p.update(func(s *SyncStatus) { s.ChainTip = tipHeight })
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

// checkConsensusBranch verifies that a server follows the consensus rules
// that the wallet has for the height of the server's chain tip. A node
// reports the consensus branch of its tip. One that has missed a network
// upgrade reports the branch from before it, and follows a chain that the
// network left at the upgrade. A wallet that has missed one finds every
// server on another branch.
//
// A server that reports no branch is not checked.
func checkConsensusBranch(info *walletrpc.LightdInfo, be backend) error {
	if info.ConsensusBranchId == "" {
		return nil
	}
	if info.BlockHeight > math.MaxUint32 {
		return &serverError{"GetLightdInfo", fmt.Errorf("height %d is out of range", info.BlockHeight)}
	}
	want, err := be.consensusBranchID(uint32(info.BlockHeight))
	if err != nil {
		return err
	}
	got, err := strconv.ParseUint(info.ConsensusBranchId, 16, 32)
	if err != nil || uint32(got) != want {
		return fmt.Errorf("%w: the server reports branch %q for its tip at height %d, for which the wallet has %08x",
			ErrConsensusBranch, info.ConsensusBranchId, info.BlockHeight, want)
	}
	return nil
}

// serverLag returns the number of blocks by which the node of a server
// estimates the chain to be ahead of its own tip.
func serverLag(info *walletrpc.LightdInfo) uint64 {
	if info.EstimatedHeight <= info.BlockHeight {
		return 0
	}
	return info.EstimatedHeight - info.BlockHeight
}

// checkStale reports a server whose node is behind the chain as
// ErrServerStale, so that the wallet turns to the next one. A node that has
// stopped following the chain still answers, and a wallet that syncs with it
// reports itself synced at a tip that is hours old.
//
// A node estimates the height of the chain from the time since its tip
// block. It cannot tell a chain that has moved on without it from a chain on
// which no block was found, and the wallet cannot either. A stale server is
// therefore passed over only while there is another to try. The wallet
// makes one tour of its servers. The tour ends at a server that works for
// it. If it ends where it began, or after as many moves as there are
// servers, no server is both further along and of use, and the wallet syncs
// with the stale one it is at. A server whose node is not behind does not
// end the tour by saying so: it has to serve the wallet. The check is made
// before any request that names an address or a transaction, so the servers
// passed over learn nothing of the wallet.
func (p *processor) checkStale(info *walletrpc.LightdInfo) error {
	if p.opts.staleLag == 0 || serverLag(info) < p.opts.staleLag {
		return nil
	}
	toured := p.staleTour && (p.server == p.staleFrom || p.rotations-p.staleRotations >= p.servers)
	if p.nextServer == nil || toured {
		p.log.Warnf("The node of %s is at height %d and estimates the chain at %d. The wallet has no server that is further along and syncs with this one.",
			p.server.address, info.BlockHeight, info.EstimatedHeight)
		p.staleTour = false
		return nil
	}
	if !p.staleTour {
		p.staleTour, p.staleFrom, p.staleRotations = true, p.server, p.rotations
	}
	return fmt.Errorf("%w: its node is at height %d and estimates the chain at %d",
		ErrServerStale, info.BlockHeight, info.EstimatedHeight)
}

// checkServerWhenDue checks the server in use again once
// serverCheckInterval has passed since it was last checked. It is called
// before every request that tells the server something of the wallet and
// before every batch of blocks, and not only at the start of a sync attempt:
// a restore is one attempt that can run for hours, and in that time a node
// can fall behind or miss a network upgrade.
func (p *processor) checkServerWhenDue(ctx context.Context) error {
	if p.opts.now().Sub(p.checkedAt) < p.opts.serverCheckInterval {
		return nil
	}
	return p.checkServer(ctx)
}

// checkServer verifies that the server follows the wallet's chain and
// consensus rules, and that its node is not behind.
func (p *processor) checkServer(ctx context.Context) error {
	info, err := checkServerChain(ctx, p.src, p.net)
	if err != nil {
		return err
	}
	if err := checkConsensusBranch(info, p.be); err != nil {
		return err
	}
	if err := p.checkStale(info); err != nil {
		return err
	}
	if !p.prepared {
		p.log.Debugf("Connected to lightwalletd %s (%s %s) on chain %q at height %d",
			p.server.address, info.Vendor, info.Version, info.ChainName, info.BlockHeight)
	}
	p.checkedAt = p.opts.now()
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
	p.noteChange()
	if p.opts.onRewind != nil {
		p.opts.onRewind(height)
	}
	p.log.Infof("Rewound the wallet to height %d", height)
	// Rewinding also forgets where the transparent outputs above the rewind
	// height were mined, so they are fetched again even at an unchanged
	// tip.
	p.utxoQueries = nil
	p.recordedUTXOs = nil

	// Rewinding shortens the wallet's scan queue to the rewind height.
	// Without telling it the tip again it would consider itself synced
	// there.
	if err := p.be.updateChainTip(tipHeight); err != nil {
		return err
	}
	return p.refreshSummary()
}

// walkAddresses fetches the mined history of transparent addresses and
// stores it, which is how the wallet learns that an address was used when
// nothing at the address is unspent any more. It returns the number of
// addresses it asked the server about.
//
// A failure of the connection or of the server as a whole ends the walk and
// is returned as failed. A request that fails for a reason of its own is put
// off, and the walk goes on. err is an error that ends the sync attempt: a
// failed check of the server, an error of the wallet database, or a
// cancelled context.
//
// The wallet watches every address up to the last used one on a chain, and
// the unused ones after it. Storing a transaction that pays an unused one
// makes it the last used one, and the wallet then watches further. The walk
// asks about the unused addresses, then about the ones that have come to be
// watched, until none of them has a history.
//
// An address is asked about once between two failed cycles, so a wallet that
// is up to date asks about the unused addresses of each chain when it is
// connected, and again after it has lost and regained the server. An address
// that has been picked is asked about until the server has answered, also
// when a payment the wallet learned of in the meantime has made it a used
// one: its history can hold outputs that the query for unspent outputs does
// not reach.
//
// The history is asked for from the account's birthday on. The wallet scans
// no blocks below the birthday, so a transaction from below it would bring
// in notes that never become spendable and that the wallet does not see
// spent. Use of an address from below the birthday is therefore not
// followed.
func (p *processor) walkAddresses(ctx context.Context, tipHeight uint32) (walked int, failed, err error) {
	accounts, err := p.be.accounts()
	if err != nil || len(accounts) == 0 {
		return 0, nil, err
	}
	for {
		for i := range accounts {
			watched, err := p.be.transparentReceivers(accounts[i].ID)
			if err != nil {
				return walked, nil, err
			}
			for _, a := range watched {
				if _, done := p.walked[a.address]; a.unused && !done {
					if p.walkQueue == nil {
						p.walkQueue = make(map[string]uint32)
					}
					p.walkQueue[a.address] = max(accounts[i].Birthday, 1)
				}
			}
		}

		progressed := false
		for _, address := range slices.Sorted(maps.Keys(p.walkQueue)) {
			if d, ok := p.walkDeferred[address]; ok && p.opts.now().Before(d.retryAt) {
				continue
			}
			// An account that begins above the server's tip has no
			// history on this server yet. The address stays in the queue.
			start := p.walkQueue[address]
			if start > tipHeight {
				continue
			}
			if err := ctx.Err(); err != nil {
				return walked, nil, err
			}
			if err := p.checkServerWhenDue(ctx); err != nil {
				return walked, nil, err
			}
			err := p.src.addressTransactions(ctx, address, start, tipHeight, func(tx *walletrpc.RawTransaction) error {
				_, err := p.be.decryptAndStoreTransaction(tx.Data, nil, minedHeight(tx))
				if err == nil {
					p.noteChange()
				}
				if err != nil && !errors.Is(err, ErrDatabase) && !errors.Is(err, ErrClosed) {
					// One transaction the wallet cannot read must not
					// hold up the rest of the walk.
					p.log.Warnf("Storing a transaction of transparent address %s: %v", address, err)
					return nil
				}
				return err
			})
			switch {
			case err == nil:
			case ctx.Err() != nil || !isServerError(err):
				return walked, nil, err
			case isConnectionError(err):
				return walked, err, nil
			default:
				// The server refused this one request. The address is
				// tried again later, and the others are not held up.
				d := p.walkDeferred[address]
				d.failures++
				wait := backoff(d.failures, p.opts.minBackoff, p.opts.maxBackoff)
				d.retryAt = p.opts.now().Add(wait)
				if p.walkDeferred == nil {
					p.walkDeferred = make(map[string]deferredRequest)
				}
				p.walkDeferred[address] = d
				p.log.Warnf("The request for the history of transparent address %s failed and is put off for %s: %v",
					address, wait.Round(time.Millisecond), err)
				continue
			}
			delete(p.walkDeferred, address)
			delete(p.walkQueue, address)
			if p.walked == nil {
				p.walked = make(map[string]struct{})
			}
			p.walked[address] = struct{}{}
			walked++
			progressed = true
		}
		if !progressed {
			return walked, nil, nil
		}
	}
}

// refreshUTXOs records the unspent transparent outputs that the server
// reports for the addresses of each account.
//
// The server reports mined outputs only, so its answer for the same
// addresses and start height changes only with the chain tip. An account is
// asked about again when one of the three has changed, and not on every
// poll: each query shows the server all of the account's addresses together.
// An account with an output that could not be recorded is asked about again
// at every refresh until it is. syncAddresses makes no refresh while the
// requests for the transparent addresses are put off after a failure.
//
// A query that the server fails, or answers with a malformed output, ends
// the refresh, and its error is returned as failed. So does a connection
// that fails while a funding transaction is fetched. A server can lack the
// call, or have a node that cannot answer it, and still serve blocks. err is
// an error that ends the sync attempt: a failed check of the server, an
// error of the wallet database, or a cancelled context.
func (p *processor) refreshUTXOs(ctx context.Context, tipHeight uint32, tipHash []byte) (failed, err error) {
	accounts, err := p.be.accounts()
	if err != nil {
		return nil, err
	}
	// waiting collects the funding transactions that wait for a retry,
	// over every account queried.
	waiting := make(map[string]struct{})
	for i := range accounts {
		id := accounts[i].ID
		watched, err := p.be.transparentReceivers(id)
		if err != nil {
			return nil, err
		}
		if len(watched) == 0 {
			continue
		}
		addresses := make([]string, len(watched))
		for i := range watched {
			addresses[i] = watched[i].address
		}
		start, err := p.be.utxoQueryHeight(id)
		if err != nil {
			return nil, err
		}
		query := fmt.Sprintf("%d %x %d %s", tipHeight, tipHash, start, strings.Join(addresses, " "))
		if p.utxoQueries[id] == query {
			continue
		}
		if err := p.checkServerWhenDue(ctx); err != nil {
			return nil, err
		}
		var replies []*walletrpc.GetAddressUtxosReply
		err = p.src.addressUTXOs(ctx, addresses, start, func(u *walletrpc.GetAddressUtxosReply) error {
			if u.ValueZat < 0 || u.Index < 0 || u.Height > math.MaxUint32 || len(u.Txid) != 32 {
				return &serverError{"GetAddressUtxosStream", errors.New("malformed output")}
			}
			replies = append(replies, u)
			return nil
		})
		switch {
		case err == nil:
		case ctx.Err() != nil || !isServerError(err):
			return nil, err
		default:
			return err, nil
		}
		// An output is recorded only once its funding transaction is
		// stored, so that the wallet knows whether it is a coinbase output.
		unstored, failed, err := p.storeFundingTransactions(ctx, replies)
		if err != nil || failed != nil {
			return failed, err
		}
		var notRecorded int
		var lastErr error
		for key, err := range unstored {
			waiting[key] = struct{}{}
			lastErr = err
		}
		for _, u := range replies {
			if _, skip := unstored[string(u.Txid)]; skip {
				notRecorded++
				continue
			}
			err := p.be.putUTXO(&utxo{
				txid:   u.Txid,
				index:  uint32(u.Index),
				script: u.Script,
				value:  uint64(u.ValueZat),
				height: uint32(u.Height),
			})
			if errors.Is(err, ErrInvalidArgument) {
				// The server's output is not one its transaction has.
				notRecorded++
				lastErr = err
				p.log.Warnf("An unspent output %s:%d reported by the server is not recorded: %v",
					displayTxID(u.Txid), u.Index, err)
				continue
			}
			if err != nil {
				return nil, err
			}
			key := fmt.Sprintf("%x:%d:%d", u.Txid, u.Index, u.Height)
			if _, ok := p.recordedUTXOs[key]; !ok {
				if p.recordedUTXOs == nil {
					p.recordedUTXOs = make(map[string]struct{})
				}
				p.recordedUTXOs[key] = struct{}{}
				p.noteChange()
			}
		}
		// The query is asked again at the next refresh until every output
		// is recorded.
		if notRecorded > 0 {
			if p.unrecorded == nil {
				p.unrecorded = make(map[string]error)
			}
			p.unrecorded[id] = wrapf(ErrOutputUnrecorded, "%d of the %d unspent outputs of account %s: %v",
				notRecorded, len(replies), id, lastErr)
			continue
		}
		delete(p.unrecorded, id)
		if p.utxoQueries == nil {
			p.utxoQueries = make(map[string]string)
		}
		p.utxoQueries[id] = query
	}
	// A transaction that no query waits for any more needs no retry. An
	// account whose query was not asked has no transaction waiting: its
	// query is kept only once every output is recorded.
	for key := range p.fundDeferred {
		if _, ok := waiting[key]; !ok {
			delete(p.fundDeferred, key)
		}
	}
	return nil, nil
}

// unrecordedErr returns an error for the unspent outputs that could not be
// recorded, nil if every output was.
func (p *processor) unrecordedErr() error {
	ids := make([]string, 0, len(p.unrecorded))
	for id := range p.unrecorded {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	return p.unrecorded[ids[0]]
}

// storeFundingTransactions fetches and stores the funding transactions of
// unspent outputs that the wallet does not hold yet, and returns those it
// could not store, by transaction ID, with the reason. A transaction the
// wallet stores from its full data is classified: a coinbase transaction is
// marked as one, and its outputs are never selected by a payment. An output
// recorded without its transaction would be taken for a regular one.
//
// A transaction that cannot be fetched or stored is tried again after a
// delay that doubles with each failure, and its outputs wait: one the server
// does not know or refuses, one the server reports other than mined at the
// height it listed the outputs at, one that is not the transaction asked for,
// and one the wallet does not keep because it does not involve the wallet. A
// transaction whose outputs are listed at two heights waits too, held or
// not, and a held one is not fetched.
//
// A failure of the connection ends the fetches and is returned as failed.
// err is an error that ends the sync attempt: a failed check of the server,
// an error of the wallet database, or a cancelled context.
func (p *processor) storeFundingTransactions(ctx context.Context, replies []*walletrpc.GetAddressUtxosReply) (unstored map[string]error, failed, err error) {
	var txids [][]byte
	// listedAt is the height the outputs of each transaction were listed
	// at, and inconsistent holds the transactions listed at two heights.
	listedAt := make(map[string]uint32)
	inconsistent := make(map[string]struct{})
	for _, u := range replies {
		key := string(u.Txid)
		h, ok := listedAt[key]
		if !ok {
			listedAt[key] = uint32(u.Height)
			txids = append(txids, u.Txid)
		} else if h != uint32(u.Height) {
			inconsistent[key] = struct{}{}
		}
	}
	if len(txids) == 0 {
		return nil, nil, nil
	}
	missing, err := p.be.missingTransactions(txids)
	if err != nil {
		return nil, nil, err
	}
	unstored = make(map[string]error)
	// A listing that puts outputs of one transaction at two heights is
	// stale, whether or not the wallet holds the transaction: none of its
	// outputs is recorded, and the query is asked again. A held transaction
	// needs no fetch, so it waits without a backoff.
	for key := range inconsistent {
		unstored[key] = errFundingInconsistent
	}
	for _, txid := range missing {
		key := string(txid)
		if d, ok := p.fundDeferred[key]; ok && p.opts.now().Before(d.retryAt) {
			unstored[key] = d.err
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		err := errFundingInconsistent
		if _, bad := inconsistent[key]; !bad {
			if err = p.checkServerWhenDue(ctx); err != nil {
				return nil, nil, err
			}
			err = p.storeFundingTransaction(ctx, txid, listedAt[key])
		}
		switch {
		case err == nil:
			delete(p.fundDeferred, key)
		case ctx.Err() != nil || errors.Is(err, ErrDatabase) || errors.Is(err, ErrClosed):
			return nil, nil, err
		case isConnectionError(err):
			return nil, err, nil
		default:
			unstored[key] = err
			d := p.fundDeferred[key]
			d.failures++
			d.err = err
			wait := backoff(d.failures, p.opts.minBackoff, p.opts.maxBackoff)
			d.retryAt = p.opts.now().Add(wait)
			if p.fundDeferred == nil {
				p.fundDeferred = make(map[string]deferredRequest)
			}
			p.fundDeferred[key] = d
			p.log.Warnf("The funding transaction %s of an unspent output could not be stored and is tried again in %s; the output waits: %v",
				displayTxID(txid), wait.Round(time.Millisecond), err)
		}
	}
	return unstored, nil, nil
}

var (
	// errFundingUnknown is the failure of a funding transaction the server
	// does not know.
	errFundingUnknown = errors.New("the server does not know the transaction")
	// errFundingInconsistent is the failure of a funding transaction whose
	// outputs the server listed at two heights.
	errFundingInconsistent = errors.New("the server listed the outputs of the transaction at different heights")
	// errFundingNotKept is the failure of a funding transaction the wallet
	// did not keep, as one that does not involve it.
	errFundingNotKept = errors.New("the transaction does not pay the wallet")
)

// storeFundingTransaction fetches one funding transaction, whose outputs the
// server listed at listedAt, and stores it.
//
// The fetch has to agree with the listing. A transaction the server reports
// in its mempool, on a fork, or mined at another height has been moved by a
// reorganization since the listing, and is not stored with a height the
// listing no longer supports: the listing is stale, and the outputs wait
// for the next query. The transaction stored is checked to be the one asked
// for, and to be held afterwards.
func (p *processor) storeFundingTransaction(ctx context.Context, txid []byte, listedAt uint32) error {
	tx, err := p.src.transaction(ctx, txid)
	if err != nil {
		return err
	}
	if tx == nil {
		return errFundingUnknown
	}
	if h := minedHeight(tx); h == nil || *h != listedAt {
		return fmt.Errorf("the outputs were listed at height %d and the server reports the transaction at height %d",
			listedAt, tx.Height)
	}
	if _, err := p.be.decryptAndStoreTransaction(tx.Data, txid, minedHeight(tx)); err != nil {
		return err
	}
	p.noteChange()
	missing, err := p.be.missingTransactions([][]byte{txid})
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return errFundingNotKept
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
			if err := p.checkServerWhenDue(ctx); err != nil {
				return err
			}
			err := p.answer(ctx, req, tipHeight)
			switch {
			case err == nil:
				delete(p.deferred, key)
				fresh = true
				p.noteChange()
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
		// The backend stores nothing unless the transaction is the one
		// asked for. Another one would be stored in its place, at a height
		// the server chose, and the request would stay open.
		_, err = p.be.decryptAndStoreTransaction(tx.Data, req.txid, minedHeight(tx))
		if errors.Is(err, ErrInvalidArgument) {
			return &serverError{"GetTransaction", err}
		}
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
		// Each stored transaction is reported as a change at once: a later
		// failure of the stream or of closing the request does not undo it.
		err := p.src.addressTransactions(ctx, req.address, req.start, last, func(tx *walletrpc.RawTransaction) error {
			if _, err := p.be.decryptAndStoreTransaction(tx.Data, nil, minedHeight(tx)); err != nil {
				return err
			}
			p.noteChange()
			return nil
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
