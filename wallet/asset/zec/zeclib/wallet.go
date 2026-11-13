// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

// Config configures a Wallet.
type Config struct {
	// DBPath is the file of the wallet database. Open creates it if it does
	// not exist. Its directory must exist.
	DBPath string
	// Network is the chain the wallet follows.
	Network Network

	// Server is the host:port of a lightwalletd server.
	Server string
	// Plaintext disables TLS, for a server on the local machine.
	Plaintext bool
	// TLSConfig replaces the default TLS configuration, which verifies the
	// server against the system roots.
	TLSConfig *tls.Config
	// Dialer, if set, opens the connection to the server. It is how the
	// connection is sent through a proxy. It receives the Server address
	// with the host name unresolved.
	Dialer func(ctx context.Context, address string) (net.Conn, error)

	// Logger receives log output. Nothing is logged if it is nil.
	Logger util.Logger
	// OnSyncStatus, if set, is called with the new status whenever the sync
	// status changes. Calls are made one at a time from the goroutine that
	// syncs the wallet, so it must not block. In particular it must not
	// call Close or wait on the WaitGroup of Connect, both of which wait
	// for that goroutine. Cancelling the context of Connect and reading
	// from the wallet are safe.
	OnSyncStatus func(SyncStatus)

	// PollInterval is how often a synced wallet asks the server for new
	// blocks. Each wait is randomized by a quarter either way, so that the
	// timing of requests does not identify the wallet. The default is 20
	// seconds.
	PollInterval time.Duration
	// BatchSize is the greatest number of blocks downloaded and scanned in
	// one step. Balances and progress are refreshed between steps, and a
	// read of the wallet waits for the step in progress. The default is
	// 1000.
	BatchSize uint32
	// MinConfirmations is the number of confirmations a received output
	// needs to count as spendable in a Summary. Zero selects the ZIP 315
	// policy: 3 for outputs of the wallet's own transactions and 10 for
	// everything else.
	MinConfirmations uint32
}

// AccountParams describes an account to create.
type AccountParams struct {
	// Name labels the account.
	Name string
	// Seed is the ZIP 32 seed the account's keys are derived from, 32 to
	// 252 bytes. The account takes the next unused ZIP 32 account index for
	// the seed. Only viewing keys are stored: the wallet keeps neither the
	// seed nor a spending key.
	//
	// The seed is copied into the Rust backend, which erases its copy after
	// use. The copy made in transit by the bindings is freed without being
	// erased.
	Seed []byte
	// Birthday is the height of the first block that can hold funds of the
	// account. The wallet does not look at earlier blocks. Zero selects a
	// height shortly below the current chain tip, for an account that has
	// never been used. A birthday below the activation height of NU6.3 is
	// raised to it, because nothing is in the Ironwood pool before then.
	Birthday uint32
	// Restore marks an account that may already hold funds, such as one
	// restored from a backup. It makes the wallet report the scan of the
	// account's past as recovery progress.
	Restore bool
	// KeySource optionally records where the account's spending keys are
	// held.
	KeySource string
}

// newAccountDepth is how far below the chain tip the birthday of a new
// account is placed when none is given. The wallet's note commitment trees
// start from the tree state at the birthday, so that state has to be final.
// The depth is the deepest reorganization a node follows.
const newAccountDepth = 100

// Wallet is a Zcash light wallet. It holds accounts, keeps them in sync with
// the chain through a lightwalletd server, and reports their addresses and
// balances.
//
// Ironwood is the only shielded pool a Wallet supports. Its accounts hold
// funds in the Ironwood pool and in transparent outputs, begin at or after
// the activation of NU6.3, and have no address with a Sapling receiver.
//
// A Wallet is safe for concurrent use.
type Wallet struct {
	cfg  Config
	log  util.Logger
	be   backend
	dial func() (chainSource, error)
	// tune, if set, adjusts the sync options. Tests use it to shorten the
	// delays.
	tune func(*syncOptions)

	// mtx guards the lifecycle fields below.
	mtx    sync.Mutex
	closed bool
	// closeDone is closed when the Close call that closes the wallet has
	// finished. It is nil until then.
	closeDone chan struct{}
	// cancel stops the sync goroutine. It is nil while the wallet is not
	// connected.
	cancel context.CancelFunc
	// syncing is the WaitGroup of the latest Connect, or nil if there has
	// been none. It is kept after syncing has stopped, so that Close can
	// wait for a sync goroutine that is on its way out.
	syncing *sync.WaitGroup

	statusMtx sync.RWMutex
	status    SyncStatus
	// notifyMtx serializes OnSyncStatus calls.
	notifyMtx sync.Mutex
}

// Open opens the wallet database named in cfg, creating it if it does not
// exist, and brings it up to date with the current schema.
//
// seed is normally nil. It is needed only when a schema migration requires
// it, in which case Open without it returns ErrSeedRequired.
//
// Open does not contact the server. Close releases the database.
func Open(cfg *Config, seed []byte) (*Wallet, error) {
	if cfg == nil {
		return nil, wrapf(ErrInvalidArgument, "no configuration")
	}
	be, err := openBackend(cfg.DBPath, cfg.Network, seed)
	if err != nil {
		return nil, err
	}
	return newWallet(cfg, be, nil), nil
}

// newWallet builds a Wallet around a backend. dial, if not nil, replaces the
// gRPC connection to the configured server.
func newWallet(cfg *Config, be backend, dial func() (chainSource, error)) *Wallet {
	w := &Wallet{
		cfg:  *cfg,
		log:  cfg.Logger,
		be:   be,
		dial: dial,
	}
	if w.log == nil {
		w.log = util.Disabled
	}
	if w.dial == nil {
		w.dial = func() (chainSource, error) { return dialLightwalletd(&w.cfg) }
	}
	return w
}

// Network returns the chain the wallet follows.
func (w *Wallet) Network() Network { return w.cfg.Network }

// Close stops syncing if the wallet is connected, waits for it to stop, and
// releases the wallet database. When it returns, the WaitGroup of Connect is
// done and OnSyncStatus is not called again. Every later call on the wallet
// returns ErrClosed.
//
// Close may be called more than once and from several goroutines. Only the
// first call closes the wallet and returns its error. The others return nil,
// and not before the first one has finished.
func (w *Wallet) Close() error {
	w.mtx.Lock()
	if w.closed {
		done := w.closeDone
		w.mtx.Unlock()
		<-done
		return nil
	}
	w.closed = true
	done := make(chan struct{})
	w.closeDone = done
	cancel, syncing := w.cancel, w.syncing
	w.mtx.Unlock()
	defer close(done)

	if cancel != nil {
		cancel()
	}
	// Waited for even when the wallet is no longer connected: the sync
	// goroutine reports its last status after it has stopped syncing, and
	// the database must not be released while it can still touch the
	// wallet.
	if syncing != nil {
		syncing.Wait()
	}
	return w.be.close()
}

// Connect starts keeping the wallet in sync with the chain and returns
// immediately. Syncing continues until ctx is cancelled or the wallet is
// closed. The returned WaitGroup is done when syncing has stopped completely:
// no goroutine of the wallet is running and the connection to the server is
// closed.
//
// Connect does not wait for the server and does not fail if the server
// cannot be reached. The wallet keeps retrying, and SyncStatus reports the
// state of the connection. The error is non-nil only if the wallet is
// already connected, is closed, or the server address is malformed.
//
// A wallet that has stopped can be connected again once the WaitGroup of
// the previous Connect is done. Before that, Connect returns ErrConnected,
// even if SyncStatus already reports SyncStopped.
func (w *Wallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	if w.closed {
		return nil, ErrClosed
	}
	if w.cancel != nil {
		return nil, ErrConnected
	}

	src, err := w.dial()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	wg := new(sync.WaitGroup)
	w.cancel, w.syncing = cancel, wg

	opts := syncOptionsFor(&w.cfg)
	if w.tune != nil {
		w.tune(&opts)
	}
	p := newProcessor(w.log, w.cfg.Network, w.be, src, opts, w.updateStatus)

	wg.Go(func() {
		p.run(ctx)
		cancel()
		if err := src.close(); err != nil {
			w.log.Debugf("Closing the lightwalletd connection: %v", err)
		}

		// The last status is reported while the wallet still counts as
		// connected. A new Connect is refused until then, so the status
		// of a new sync goroutine cannot be overwritten by this one.
		w.updateStatus(func(s *SyncStatus) {
			s.State = SyncStopped
			s.Err = nil
		})
		w.mtx.Lock()
		w.cancel = nil
		w.mtx.Unlock()
	})
	return wg, nil
}

// SyncStatus returns the current sync status.
func (w *Wallet) SyncStatus() SyncStatus {
	w.statusMtx.RLock()
	defer w.statusMtx.RUnlock()
	return w.status
}

// updateStatus applies f to the sync status and reports the result to
// OnSyncStatus if it changed anything.
func (w *Wallet) updateStatus(f func(*SyncStatus)) {
	// Held across the callback so that updates are delivered in order.
	w.notifyMtx.Lock()
	defer w.notifyMtx.Unlock()

	w.statusMtx.Lock()
	prev := w.status
	f(&w.status)
	next := w.status
	w.statusMtx.Unlock()

	if w.cfg.OnSyncStatus != nil && !next.equal(&prev) {
		w.cfg.OnSyncStatus(next)
	}
}

// Accounts lists the accounts of the wallet, in ZIP 32 account index order.
func (w *Wallet) Accounts() ([]Account, error) {
	return w.be.accounts()
}

// CreateAccount derives a new account from a seed and adds it to the wallet.
//
// It asks the server for the tree state of the block before the account's
// birthday, and for the chain tip when the birthday is not given, is below
// the NU6.3 activation height, or the account is being restored. It returns
// ErrWrongNetwork if the server is on another chain than the wallet, and
// ErrInvalidArgument if the chain tip is more than one block below the NU6.3
// activation height, since no account can begin before NU6.3.
//
// The server learns the birthday from the request. CreateAccountWithTreeState
// avoids that by taking the tree state from the caller.
func (w *Wallet) CreateAccount(ctx context.Context, p *AccountParams) (*Account, error) {
	if p == nil {
		return nil, wrapf(ErrInvalidArgument, "no account parameters")
	}
	src, err := w.dial()
	if err != nil {
		return nil, err
	}
	defer src.close()

	// The birthday state of an account cannot be replaced later, and the
	// wallet cannot rewind below it. A state from a server on another
	// chain would leave the account unusable.
	if _, err := checkServerChain(ctx, src, w.cfg.Network); err != nil {
		return nil, err
	}

	activation := w.cfg.Network.ironwoodActivation()
	birthday := p.Birthday
	var recoverUntil *uint32
	// The tip is needed for the default birthday, for the recovery range of
	// a restored account, and to tell whether the chain has reached NU6.3
	// when the birthday is below its activation.
	if birthday == 0 || birthday < activation || p.Restore {
		tip, err := latestHeight(ctx, src)
		if err != nil {
			return nil, err
		}
		if p.Restore {
			recoverUntil = &tip
		}
		if birthday == 0 {
			birthday = tip - min(tip, newAccountDepth)
		}
		// The wallet holds shielded funds in the Ironwood pool only, and
		// there is nothing in that pool before NU6.3. An account begins
		// at the activation at the earliest, which takes the state of
		// the block before it.
		if birthday < activation {
			if tip+1 < activation {
				return nil, wrapf(ErrInvalidArgument, "no account can begin yet: NU6.3 activates at height %d "+
					"and the chain tip is %d", activation, tip)
			}
			birthday = activation
		}
	}

	state, err := chainStateBefore(ctx, src, w.cfg.Network, birthday, nil)
	if err != nil {
		return nil, err
	}
	return w.be.createAccount(p.Name, p.Seed, state, recoverUntil, p.KeySource)
}

// CreateAccountWithTreeState is CreateAccount without the server. treeState
// is the lightwalletd tree state of the block before the account's birthday,
// for example from a checkpoint shipped with the application. p.Birthday is
// ignored: the birthday is the height after the tree state. For an account
// being restored, recoverUntil is the height of the chain tip at the time of
// the restore. It is zero otherwise, and p.Restore is ignored.
//
// A tree state that names its network has to name the wallet's, or the
// error is ErrWrongNetwork. The birthday cannot be below the activation
// height of NU6.3, before which there is nothing in the Ironwood pool.
func (w *Wallet) CreateAccountWithTreeState(p *AccountParams, treeState *walletrpc.TreeState, recoverUntil uint32) (*Account, error) {
	if p == nil || treeState == nil {
		return nil, wrapf(ErrInvalidArgument, "no account parameters or tree state")
	}
	if treeState.Network != "" && !w.cfg.Network.matchesChainName(treeState.Network) {
		return nil, fmt.Errorf("%w: the wallet is on %s and the tree state is for chain %q",
			ErrWrongNetwork, w.cfg.Network, treeState.Network)
	}
	if activation := w.cfg.Network.ironwoodActivation(); treeState.Height+1 < uint64(activation) {
		return nil, wrapf(ErrInvalidArgument, "the account birthday %d is before NU6.3 activates at %d",
			treeState.Height+1, activation)
	}
	state, err := proto.Marshal(treeState)
	if err != nil {
		return nil, wrapf(ErrInvalidArgument, "tree state: %v", err)
	}
	var until *uint32
	if recoverUntil != 0 {
		until = &recoverUntil
	}
	return w.be.createAccount(p.Name, p.Seed, state, until, p.KeySource)
}

// IsSeedRelevant reports whether seed is the seed of at least one account
// that was derived in this wallet.
func (w *Wallet) IsSeedRelevant(seed []byte) (bool, error) {
	return w.be.isSeedRelevant(seed)
}

// Address returns the current unified address of the account, which has an
// Ironwood and a transparent receiver. It is the default address of a new
// account, and later the newest address that NewAddress generated with
// ReceiversAll.
func (w *Wallet) Address(accountID string) (string, error) {
	return w.be.currentAddress(accountID)
}

// NewAddress generates and returns a new unified address for the account.
//
// With ReceiversAll the address has a transparent receiver, which the wallet
// takes from a limited window of unused transparent addresses. It returns
// ErrAddressGapLimit when every address in the window has been handed out.
// ReceiversShielded has no such limit.
func (w *Wallet) NewAddress(accountID string, receivers ReceiverSet) (string, error) {
	return w.be.nextAvailableAddress(accountID, receivers)
}

// TransparentAddresses lists the transparent addresses of the account that
// can receive funds, including its change addresses, sorted.
func (w *Wallet) TransparentAddresses(accountID string) ([]string, error) {
	addrs, err := w.be.transparentReceivers(accountID)
	if err != nil {
		return nil, err
	}
	slices.Sort(addrs)
	return addrs, nil
}

// TransparentReceiver returns the transparent receiver of a unified address
// as a transparent address, or the empty string if the unified address has
// none.
func (w *Wallet) TransparentReceiver(unifiedAddress string) (string, error) {
	return transparentReceiver(w.cfg.Network, unifiedAddress)
}

// Summary returns the balances and scan state of the wallet. It returns nil
// while the wallet lacks the data for one, which is until it has an account
// and has scanned its first block.
//
// The summary is read from the wallet database, so it waits for a batch of
// blocks that is being scanned.
func (w *Wallet) Summary() (*Summary, error) {
	return w.be.summary(w.cfg.MinConfirmations)
}

// latestHeight returns the height of the server's chain tip.
func latestHeight(ctx context.Context, src chainSource) (uint32, error) {
	tip, err := src.latestBlock(ctx)
	if err != nil {
		return 0, err
	}
	if tip.Height > uint64(^uint32(0)) {
		return 0, &serverError{"GetLatestBlock", fmt.Errorf("height %d is out of range", tip.Height)}
	}
	return uint32(tip.Height), nil
}

// chainStateBefore returns the encoded tree state of the block before
// height, which is what the wallet needs in order to scan from height on.
//
// Servers do not serve tree states below Sapling activation. The trees are
// empty there, so the state of the block before the activation height is
// built locally. It needs that block's hash, which is the previous-block
// hash of the block at height: prevHash if the caller has it, and otherwise
// fetched.
func chainStateBefore(ctx context.Context, src chainSource, network Network, height uint32, prevHash []byte) ([]byte, error) {
	if height == 0 {
		return nil, wrapf(ErrInvalidArgument, "there is no block before height 0")
	}
	var state *walletrpc.TreeState
	if height <= network.saplingActivation() {
		if prevHash == nil {
			block, err := src.block(ctx, height)
			if err != nil {
				return nil, err
			}
			prevHash = block.PrevHash
		}
		if len(prevHash) != 32 {
			return nil, &serverError{fmt.Sprintf("block %d", height),
				fmt.Errorf("previous block hash is %d bytes", len(prevHash))}
		}
		// A tree state carries the hash as hex in display order, which is
		// the reverse of the order in a compact block.
		display := slices.Clone(prevHash)
		slices.Reverse(display)
		state = &walletrpc.TreeState{
			Height: uint64(height - 1),
			Hash:   hex.EncodeToString(display),
		}
	} else {
		var err error
		state, err = src.treeState(ctx, height-1)
		if err != nil {
			return nil, err
		}
		if state.Height != uint64(height-1) {
			return nil, &serverError{fmt.Sprintf("GetTreeState(%d)", height-1),
				fmt.Errorf("the reply is for height %d", state.Height)}
		}
	}
	encoded, err := proto.Marshal(state)
	if err != nil {
		return nil, wrapf(ErrInternal, "encoding a tree state: %v", err)
	}
	return encoded, nil
}
