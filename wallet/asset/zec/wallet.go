// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// dbFileName is the zeclib database in the wallet's data directory.
const dbFileName = "wallet.sqlite"

var (
	// errClosed is the error of a call that needs the wallet database while
	// it is closed.
	errClosed = errors.New("the Zcash wallet is not open")
	// errLocked is the error of a send while the wallet is locked.
	errLocked = errors.New("the Zcash wallet is locked")
)

// registry holds the open wallet of each database path, and serializes every
// opening and closing of every instance. Core can leave an instance open
// while it builds another for the same data directory, as ReconfigureWallet
// does. Only one instance may hold the database open: two would sync, select
// inputs and write the history independently. An instance that opens a path
// another instance holds open runs that instance's close first.
//
// Lock order, for every path of the package: an instance's connMtx, then
// the registry, then an instance's beMtx (held for reading by every call
// that uses the backend), then the projection's mtx, then the leaves: the
// cache's mtx and seedMtx, which are held only briefly and never while
// another of these is taken. A path that holds a lock never takes one
// earlier in this order. In particular nothing that holds beMtx takes
// connMtx or the registry: the context of the connection, which a send
// needs under beMtx, is read from an atomic value. zeclib's own locks come
// after all of these. NewAddress's newAddrMtx is the outermost lock of its
// path: it is taken before beMtx and never while holding it, and neither
// Close nor a displacement takes it, so they never wait on it.
//
// The registry orders instances of a wallet that exists. It does not make a
// Driver.Create concurrent with an Open of the same unfinished wallet safe:
// core serializes the creation of a wallet (claimWalletCreation), and the
// package relies on it.
//
// Nothing is published to the cache or to core after the guard of the
// backend (beMtx) is released without checking that the instance is still
// open: a Close that ran in between would otherwise leave a closed instance
// holding state, or report for it.
var registry = struct {
	sync.Mutex
	open map[string]*zecWallet
}{open: make(map[string]*zecWallet)}

// zecWallet is the Zcash asset wallet.
type zecWallet struct {
	log  util.Logger
	net  util.Network
	emit *asset.WalletEmitter
	// peersChange is core's peer count callback. handleStatus calls it with
	// the guard of the backend (beMtx) held for reading, so it must neither
	// block nor call back into the wallet on the calling goroutine; core's
	// only starts a goroutine.
	peersChange func(uint32, error)
	txDB        txdb.NetworkTxDB
	dbPath      string
	seeds       seedStore
	netCfg      *netConfig

	// statusSignal and changeSignal carry the zeclib callbacks to the
	// worker. A callback only makes a non-blocking send on a channel with
	// a buffer of one: the worker reads the latest state when it runs.
	statusSignal chan struct{}
	changeSignal chan struct{}

	// beMtx guards the open database and what is read from it at opening.
	// A call that uses the backend holds the read lock for as long as it
	// does, so a close waits for it and the call never sees the backend
	// closed under it.
	beMtx   sync.RWMutex
	be      backend
	account zeclib.Account
	// closed is closed when the open backend is closed, which stops the
	// worker of a connection to it.
	closed chan struct{}

	// connMtx serializes Connect. connCtx holds the context of the running
	// connection, which calls core gives no context use; it is atomic so
	// that a call holding beMtx reads it without connMtx.
	// of the running connection, which calls core gives no context use.
	connMtx sync.Mutex
	connCtx atomic.Pointer[context.Context]

	// seedMtx guards the ZIP 32 seed held while the wallet is unlocked.
	seedMtx sync.Mutex
	seed    []byte

	cache cache
	proj  projection
	// newAddrMtx serializes NewAddress, from its check of the current
	// address through the derivation and the cache's update.
	newAddrMtx sync.Mutex
	// gapLogged is the address NewAddress last warned it hands out again at
	// the transparent gap limit, so the warning is logged once per address.
	gapLogged atomic.Pointer[string]
}

var (
	_ asset.Wallet         = (*zecWallet)(nil)
	_ asset.Opener         = (*zecWallet)(nil)
	_ asset.Authenticator  = (*zecWallet)(nil)
	_ asset.NewAddresser   = (*zecWallet)(nil)
	_ asset.WalletRestorer = (*zecWallet)(nil)
	_ asset.ReceiverLister = (*zecWallet)(nil)
)

func newWallet(cfg *asset.WalletConfig, log util.Logger, network util.Network) (*zecWallet, error) {
	if cfg.Type != walletTypeNative {
		return nil, fmt.Errorf("unknown wallet type %q", cfg.Type)
	}
	netCfg, err := configFor(network, cfg.Settings, cfg.TorProxy)
	if err != nil {
		return nil, err
	}
	dbPath, err := databasePath(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = util.Disabled
	}
	w := &zecWallet{
		log:          log,
		net:          network,
		emit:         cfg.Emit,
		peersChange:  cfg.PeersChange,
		txDB:         cfg.NetworkTxDB,
		dbPath:       dbPath,
		seeds:        newSeedStore(cfg.DataDir),
		netCfg:       netCfg,
		statusSignal: make(chan struct{}, 1),
		changeSignal: make(chan struct{}, 1),
	}
	w.cache.reset()
	return w, nil
}

// databasePath returns the path of the database in dataDir, the key of the
// registry: absolute, with symbolic links resolved, so that two names of one
// directory are one key. A directory that does not exist yet has no links
// to resolve.
func databasePath(dataDir string) (string, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return filepath.Join(dir, dbFileName), nil
}

// Info returns the wallet information.
func (w *zecWallet) Info() *asset.WalletInfo {
	wi := WalletInfo
	return &wi
}

// signal makes a non-blocking send on a signal channel.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// OpenWithPW opens the wallet database. The password is used only when the
// database needs the seed for a migration. It does nothing if the wallet is
// open.
func (w *zecWallet) OpenWithPW(ctx context.Context, pw []byte) (err error) {
	defer func() { err = w.explain(err) }()
	if w.IsOpen() {
		return nil
	}
	err = w.open(nil)
	if !errors.Is(err, zeclib.ErrSeedRequired) {
		return err
	}
	entropy, err := w.seeds.load(pw)
	if err != nil {
		return err
	}
	defer encode.ClearBytes(entropy)
	seed, err := zip32Seed(entropy)
	if err != nil {
		return err
	}
	defer encode.ClearBytes(seed)
	return w.open(seed)
}

// IsOpen reports whether the wallet database is open.
func (w *zecWallet) IsOpen() bool {
	w.beMtx.RLock()
	defer w.beMtx.RUnlock()
	return w.be != nil
}

// open opens the wallet database, closing another instance that holds it
// open. seed is nil unless a migration needs it. It does nothing if this
// instance has the database open.
func (w *zecWallet) open(seed []byte) error {
	registry.Lock()
	defer registry.Unlock()
	if w.IsOpen() {
		return nil
	}
	if other := registry.open[w.dbPath]; other != nil && other != w {
		w.log.Infof("Closing another instance of the wallet that holds %s open", w.dbPath)
		if err := other.closeLocked(); err != nil {
			w.log.Errorf("Closing the other instance: %v", err)
		}
	}

	cfg := w.netCfg.zeclibConfig(w.dbPath, w.log)
	cfg.OnSyncStatus = w.onSyncStatus()
	cfg.OnRewind = w.onRewind
	cfg.OnChange = func() { signal(w.changeSignal) }
	be, err := openBackend(cfg, seed)
	if err != nil {
		return err
	}
	account, err := theAccount(be)
	if err != nil {
		if closeErr := be.Close(); closeErr != nil {
			w.log.Errorf("Closing the wallet database: %v", closeErr)
		}
		return err
	}
	address, err := be.Address(account.ID)
	if err != nil {
		if closeErr := be.Close(); closeErr != nil {
			w.log.Errorf("Closing the wallet database: %v", closeErr)
		}
		return fmt.Errorf("reading the deposit address: %w", err)
	}

	w.beMtx.Lock()
	w.be = be
	w.account = *account
	w.closed = make(chan struct{})
	w.beMtx.Unlock()
	registry.open[w.dbPath] = w

	w.cache.reset()
	w.setAddress(address)
	w.refresh()
	return nil
}

// theAccount returns the wallet's one account: ZIP 32 account 0 of its seed.
func theAccount(be backend) (*zeclib.Account, error) {
	accounts, err := be.Accounts()
	if err != nil {
		return nil, fmt.Errorf("reading the accounts: %w", err)
	}
	if len(accounts) != 1 {
		return nil, fmt.Errorf("the wallet database holds %d accounts, want 1", len(accounts))
	}
	a := &accounts[0]
	if !a.Derived || a.HDIndex != 0 {
		return nil, fmt.Errorf("the account of the wallet database is not ZIP 32 account 0 of a seed")
	}
	return a, nil
}

// Close closes the wallet database. It waits for calls that use it to
// finish, stops the connection to it, and erases the seed held for sends.
// A later Connect or OpenWithPW opens it again.
func (w *zecWallet) Close() error {
	registry.Lock()
	defer registry.Unlock()
	return w.closeLocked()
}

// closeLocked is Close for a caller that holds the registry. It releases the
// registry's record of the path only if the record is this instance's, so
// the late Close of an instance another one displaced leaves the other's
// database open.
func (w *zecWallet) closeLocked() error {
	w.beMtx.Lock()
	be := w.be
	if be == nil {
		w.beMtx.Unlock()
		return nil
	}
	w.be = nil
	close(w.closed)
	w.beMtx.Unlock()
	if registry.open[w.dbPath] == w {
		delete(registry.open, w.dbPath)
	}
	w.eraseSeed()
	err := be.Close()
	w.cache.reset()
	return err
}

// backend returns the open backend and the account, with the read lock of
// beMtx held, and a function that releases it. It fails if the wallet is
// closed.
func (w *zecWallet) backend() (backend, *zeclib.Account, func(), error) {
	w.beMtx.RLock()
	if w.be == nil {
		w.beMtx.RUnlock()
		return nil, nil, nil, errClosed
	}
	account := w.account
	return w.be, &account, w.beMtx.RUnlock, nil
}

// Connect starts syncing. A closed database is opened first, without the
// password; one that needs the seed for a migration fails, and has to be
// opened with OpenWithPW. The WaitGroup is done when syncing and the
// wallet's worker have stopped, which happens when ctx is cancelled or the
// wallet is closed. A Connect after that connects again.
func (w *zecWallet) Connect(ctx context.Context) (_ *sync.WaitGroup, err error) {
	defer func() { err = w.explain(err) }()
	w.connMtx.Lock()
	defer w.connMtx.Unlock()
	if err := w.open(nil); err != nil {
		if errors.Is(err, zeclib.ErrSeedRequired) {
			return nil, fmt.Errorf("the Zcash wallet database needs the seed to be upgraded; "+
				"it has to be opened with the wallet password: %w", err)
		}
		return nil, err
	}
	be, _, release, err := w.backend()
	if err != nil {
		return nil, err
	}
	closed := w.closed
	syncing, err := be.Connect(ctx)
	release()
	if err != nil {
		return nil, err
	}
	w.connCtx.Store(&ctx)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		syncing.Wait()
	}()
	go func() {
		defer wg.Done()
		w.work(ctx, closed)
	}()
	return &wg, nil
}

// connContext returns the context of the running connection, for calls core
// gives no context. It is a cancelled context while the wallet is not
// connected.
func (w *zecWallet) connContext() context.Context {
	if ctx := w.connCtx.Load(); ctx != nil {
		return *ctx
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// work handles the zeclib callbacks until ctx is cancelled or the database
// is closed. It runs every handler once at the start, so the state is read
// whatever the callbacks did before it began.
func (w *zecWallet) work(ctx context.Context, closed chan struct{}) {
	w.cache.connected()
	w.handleStatus()
	w.refresh()
	w.project(ctx, projectStart)
	for {
		select {
		case <-ctx.Done():
			return
		case <-closed:
			return
		case <-w.statusSignal:
			w.handleStatus()
			w.project(ctx, projectStatus)
		case <-w.changeSignal:
			w.refresh()
			w.project(ctx, projectChange)
		}
	}
}

// Unlock decrypts the seed and holds it for sends. It opens the database if
// it is closed, and checks that the seed is the account's.
func (w *zecWallet) Unlock(pw []byte) (err error) {
	defer func() { err = w.explain(err) }()
	entropy, err := w.seeds.load(pw)
	if err != nil {
		return err
	}
	defer encode.ClearBytes(entropy)
	seed, err := zip32Seed(entropy)
	if err != nil {
		return err
	}
	if err := w.open(seed); err != nil {
		encode.ClearBytes(seed)
		return err
	}
	be, _, release, err := w.backend()
	if err != nil {
		encode.ClearBytes(seed)
		return err
	}
	// The guard is held until the seed is published, so a Close cannot
	// finish in between and leave a closed instance unlocked.
	defer release()
	relevant, err := be.IsSeedRelevant(seed)
	if err != nil || !relevant {
		encode.ClearBytes(seed)
		if err == nil {
			err = errors.New("the decrypted seed is not the seed of the wallet's account")
		}
		return err
	}
	w.seedMtx.Lock()
	old := w.seed
	w.seed = seed
	w.seedMtx.Unlock()
	encode.ClearBytes(old)
	return nil
}

// Lock erases the seed held for sends. A send that has started took a copy
// of its own and is not waited for.
func (w *zecWallet) Lock() error {
	w.eraseSeed()
	return nil
}

func (w *zecWallet) eraseSeed() {
	w.seedMtx.Lock()
	seed := w.seed
	w.seed = nil
	w.seedMtx.Unlock()
	encode.ClearBytes(seed)
}

// Locked reports whether no seed is held.
func (w *zecWallet) Locked() bool {
	w.seedMtx.Lock()
	defer w.seedMtx.Unlock()
	return w.seed == nil
}

// seedCopy returns a copy of the held seed for one send, which the caller
// erases. Lock erases the held seed and never the copy, which the bindings
// may be reading.
func (w *zecWallet) seedCopy() ([]byte, error) {
	w.seedMtx.Lock()
	defer w.seedMtx.Unlock()
	if w.seed == nil {
		return nil, errLocked
	}
	return append([]byte(nil), w.seed...), nil
}
