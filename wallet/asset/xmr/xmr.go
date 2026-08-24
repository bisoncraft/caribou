// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build xmr

package xmr

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/xmr/cxmr"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

const (
	version          = 0
	BipID            = 128
	walletTypeNative = "native"

	defaultFee          = 3 // Priority level 0-4, default to low
	defaultFeeRateLimit = 30

	// Refresh interval for sync status checks during normal operation.
	syncTickerPeriod = 10 * time.Second
	// Faster polling during initial sync for progress updates.
	initialSyncTickerPeriod = 2 * time.Second
	// Refresh interval for peer count checks.
	peerCountTicker = 30 * time.Second

	// Default daemon addresses per network.
	// Mainnet uses public node; testnet/simnet use localhost.
	// TODO: Find a working public testnet node.
	defaultMainnetDaemon = "http://xmr-node.cakewallet.com:18081"
	defaultTestnetDaemon = "http://127.0.0.1:28081"
	defaultSimnetDaemon  = "http://127.0.0.1:18081"

	// Default wallet name for simnet.
	defaultSimnetWalletName = "xmrwallet"

	walletName     = "xmr"
	walletMetaFile = "xmr_meta.json"
)

// walletMeta stores persistent wallet metadata that wallet2 doesn't preserve.
// This follows Cake Wallet's approach of storing recovery info separately.
type walletMeta struct {
	// RestoreHeight is the block height to start scanning from.
	RestoreHeight uint64 `json:"restoreHeight"`
	// Birthday is the original unix timestamp used to calculate RestoreHeight.
	Birthday uint64 `json:"birthday"`
	// IsRecovery indicates this wallet needs recovery scanning.
	IsRecovery bool `json:"isRecovery"`
	// LastFee is the fee, in atoms, of the last transaction this wallet
	// broadcast, and LastFeePriority is the priority it was built at. The
	// priority is stored alongside because a fee is only meaningful at the
	// priority that produced it. See StandardSendFee.
	LastFee         uint64 `json:"lastFee,omitempty"`
	LastFeePriority int    `json:"lastFeePriority,omitempty"`
}

var (
	configOpts = []*asset.ConfigOption{
		{
			Key:         "daemonaddress",
			DisplayName: "Daemon Address",
			Description: "Address of the Monero daemon (e.g., localhost:18081)",
			Required:    true,
		},
		{
			Key:         "daemonusername",
			DisplayName: "Daemon Username",
			Description: "Username for daemon RPC authentication (optional)",
		},
		{
			Key:         "daemonpassword",
			DisplayName: "Daemon Password",
			Description: "Password for daemon RPC authentication (optional)",
			NoEcho:      true,
		},
		{
			Key:          "feepriority",
			DisplayName:  "Fee Priority",
			Description:  "Transaction fee priority (0=default, 1=low, 2=medium, 3=high, 4=highest)",
			DefaultValue: "0",
		},
	}

	nativeWalletDefinition = &asset.WalletDefinition{
		Type:        walletTypeNative,
		Seeded:      true,
		Tab:         "Native",
		Description: "Native Monero wallet using libwallet2",
		ConfigOpts:  configOpts,
	}

	// WalletInfo defines general information about a Monero wallet.
	WalletInfo = &asset.WalletInfo{
		Name:              "Monero",
		SupportedVersions: []uint32{version},
		UnitInfo:          UnitInfo,
		AvailableWallets:  []*asset.WalletDefinition{nativeWalletDefinition},
		Network: &asset.Network{
			ID:            BipID,
			NativeAssetID: BipID,
			Priority:      true,
		},
	}
)

func init() {
	asset.Register(BipID, &Driver{})
}

// Driver implements asset.Driver.
type Driver struct{}

var _ asset.Driver = (*Driver)(nil)
var _ asset.Creator = (*Driver)(nil)

// Open creates the XMR wallet.
func (d *Driver) Open(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
	return newWallet(cfg, logger, network)
}

// DecodeCoinID creates a human-readable representation of a coin ID for Monero.
// Monero coin IDs are transaction hashes.
func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	if len(coinID) != 32 {
		return "", fmt.Errorf("invalid coin ID length: expected 32, got %d", len(coinID))
	}
	return hex.EncodeToString(coinID), nil
}

// Info returns basic information about the wallet and asset.
func (d *Driver) Info() *asset.WalletInfo {
	return WalletInfo
}

// Exists checks if a wallet already exists at the specified location.
func (d *Driver) Exists(walletType, dataDir string, settings map[string]string, net util.Network) (bool, error) {
	if walletType != walletTypeNative {
		return false, fmt.Errorf("unknown wallet type %q", walletType)
	}
	walletPath := filepath.Join(dataDir, walletName)
	wm := cxmr.GetWalletManager()
	return wm.WalletExists(walletPath), nil
}

// Create creates a new Monero wallet from seed.
// The seed should be 32 bytes which is used as the Monero spend key.
func (d *Driver) Create(params *asset.CreateWalletParams) error {
	if params.Type != walletTypeNative {
		return fmt.Errorf("unknown wallet type %q", params.Type)
	}

	walletPath := filepath.Join(params.DataDir, walletName)
	netType := dexNetworkToCgo(params.Net)

	// Ensure the data directory exists
	if err := os.MkdirAll(params.DataDir, 0700); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	wm := cxmr.GetWalletManager()

	// If wallet files already exist from a previous failed attempt, remove
	// them so we can try again. The wallet is deterministic from the seed, so
	// nothing is lost.
	if wm.WalletExists(walletPath) {
		for _, f := range []string{walletPath, walletPath + ".keys", filepath.Join(params.DataDir, walletMetaFile)} {
			_ = os.Remove(f)
		}
	}

	// A seed is required. The seed must be 32 bytes (ed25519 seed size) which
	// is used as the Monero spend key directly.
	if len(params.Seed) == 0 {
		return errors.New("seed is required")
	}
	if len(params.Seed) != 32 {
		return fmt.Errorf("invalid seed size: expected 32 bytes, got %d", len(params.Seed))
	}

	// Hex-encode the seed to use as the spend key
	spendKey := hex.EncodeToString(params.Seed)

	// Hex-encode the password for wallet encryption.
	// This must match what OpenWithPW uses.
	password := hex.EncodeToString(params.Pass)

	restoreHeight := birthdayToHeight(params.Birthday, params.Net)

	if params.Logger != nil {
		params.Logger.Infof("Creating XMR wallet at %s with restore height %d", walletPath, restoreHeight)
	}

	// Create wallet from spend key. The restoreHeight passed here sets the
	// wallet's creation height but we also need to explicitly set the recovery
	// state and refresh height for proper blockchain scanning.
	wallet, err := wm.CreateDeterministicWalletFromSpendKey(
		walletPath,
		password,
		"English",
		netType,
		restoreHeight,
		spendKey,
	)
	if err != nil {
		return fmt.Errorf("failed to create wallet: %w", err)
	}

	// Mark the wallet as recovering from seed and set the refresh height.
	// This is critical for proper blockchain scanning - without this, the
	// wallet may not scan from the correct height. This follows Cake Wallet's
	// approach of explicitly setting these values after wallet creation.
	wallet.SetRecoveringFromSeed(true)
	wallet.SetRefreshFromBlockHeight(restoreHeight)

	// Store the wallet to persist the recovery state and refresh height.
	if !wallet.Store("") {
		if params.Logger != nil {
			params.Logger.Warnf("Failed to store wallet after setting recovery state: %s", wallet.ErrorString())
		}
	}

	// Log the wallet address for debugging
	if params.Logger != nil {
		addr := wallet.Address(0, 0)
		actualRestoreHeight := wallet.GetRefreshFromBlockHeight()
		params.Logger.Infof("Created XMR wallet with primary address: %s, restore height: %d",
			addr, actualRestoreHeight)
	}

	wm.CloseWallet(wallet, true)

	// Save wallet metadata separately. The wallet2 library doesn't reliably
	// persist the recovery state and refresh height, so we store them in our
	// own metadata file (like Cake Wallet does with walletInfo).
	meta := &walletMeta{
		RestoreHeight: restoreHeight,
		Birthday:      params.Birthday,
		IsRecovery:    true,
	}
	if err := saveWalletMeta(params.DataDir, meta); err != nil {
		return fmt.Errorf("failed to save wallet metadata: %w", err)
	}

	return nil
}

func dexNetworkToCgo(net util.Network) cxmr.NetworkType {
	switch net {
	case util.Mainnet:
		return cxmr.NetworkMainnet
	case util.Testnet:
		// Use Monero testnet instead of stagenet - stagenet has address
		// validation bugs in monero_c.
		// TODO: stagenet is supposedly more stable and meant for consumers
		// like us so we should switch to that whenever the library can
		// handle it. When testing validate address and send, which accepts
		// an address both fail because of the address values.
		return cxmr.NetworkTestnet
	case util.Simnet:
		// Monero regtest mode uses mainnet-style addresses (prefix 4/8).
		// We use allow_mismatched_daemon_version to handle hard fork checks.
		return cxmr.NetworkMainnet
	default:
		return cxmr.NetworkMainnet
	}
}

// ExchangeWallet implements asset.Wallet with full Monero wallet functionality.
type ExchangeWallet struct {
	log  util.Logger
	net  util.Network
	emit *asset.WalletEmitter

	wm         *cxmr.WalletManager
	wallet     *cxmr.Wallet
	walletPath string
	dataDir    string
	walletMtx  sync.RWMutex

	daemonAddr  string
	daemonUser  string
	daemonPass  string
	feePriority cxmr.Priority

	tipAtConnect  uint64
	lastPeerCount uint32
	peersChange   func(uint32, error)
	isOpen        atomic.Bool
	rescanning    atomic.Bool

	// feeRPC queries the daemon for live fee rates. See rpc.go for why the
	// daemon is asked directly rather than through monero_c.
	feeRPC   *daemonRPC
	feeCache feeEstimateCache

	// feeMtx serializes fetches on a cache miss. Concurrent fetches would
	// each open a connection to the daemon, defeating the single pooled
	// connection monerod's connection-scoped digest nonce depends on (see
	// drainAndClose), and during an outage would each stall for a full
	// feeRequestTimeout rather than sharing the one attempt per
	// feeRetryInterval that the cooldown allows.
	feeMtx sync.Mutex

	// observedFee is the last fee this wallet actually paid, in atoms. It
	// backs StandardSendFee when the daemon cannot be reached.
	observedFee atomic.Uint64

	// pendingFee is a broadcast fee awaiting a write to the meta file. commitTx
	// stores it under walletMtx and flushPendingFee performs the write after
	// that lock is released, so file I/O never stalls the readers that take
	// walletMtx.RLock. See flushPendingFee.
	pendingFee atomic.Uint64

	// metaMtx guards read-modify-write cycles on the wallet meta file, which
	// are performed from the monitor goroutine and from caller goroutines.
	metaMtx sync.Mutex

	syncStatus atomic.Value // *asset.SyncStatus

	// Monero's sweep skips outputs below the marginal fee cost of spending
	// them (ignore_fractional_outputs), but these dust outputs are still
	// included in the wallet's unlocked balance. We cache their total so
	// we can subtract it when reporting balance, ensuring users never see
	// a balance they cannot actually send. The cache avoids expensive
	// per-output CGO enumeration on every balance poll.
	dustMtx        sync.Mutex
	cachedDust     uint64
	cachedDustTime time.Time

	// ctx is the context Connect was called with. It is stored atomically
	// rather than in a plain field because FeeRate and StandardSendFee are
	// exported, error-free and reachable from a UI poll, so they can read it
	// while Connect is writing it.
	ctx    atomic.Pointer[connCtx]
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// connCtx wraps a context so it can live in an atomic.Pointer, which an
// interface value cannot.
type connCtx struct{ context.Context }

// context returns the context Connect was called with, or a background
// context if Connect has not run yet.
func (w *ExchangeWallet) context() context.Context {
	if ctx := w.ctx.Load(); ctx != nil {
		return ctx
	}
	return context.Background()
}

var _ asset.Wallet = (*ExchangeWallet)(nil)
var _ asset.Opener = (*ExchangeWallet)(nil)
var _ asset.NewAddresser = (*ExchangeWallet)(nil)
var _ asset.Withdrawer = (*ExchangeWallet)(nil)
var _ asset.TxFeeEstimator = (*ExchangeWallet)(nil)
var _ asset.Rescanner = (*ExchangeWallet)(nil)

func newWallet(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (*ExchangeWallet, error) {
	daemonAddr := cfg.Settings["daemonaddress"]
	if daemonAddr == "" {
		// Use default daemon address based on network
		switch network {
		case util.Mainnet:
			daemonAddr = defaultMainnetDaemon
		case util.Testnet:
			daemonAddr = defaultTestnetDaemon
		case util.Simnet:
			daemonAddr = defaultSimnetDaemon
		default:
			return nil, errors.New("daemon address not specified")
		}
		logger.Infof("Using default daemon address: %s", daemonAddr)
	}

	var feePriority cxmr.Priority
	if fp := cfg.Settings["feepriority"]; fp != "" {
		p, err := strconv.Atoi(fp)
		if err != nil {
			return nil, fmt.Errorf("invalid feepriority %q: %w", fp, err)
		}
		if p < 0 || p > 4 {
			return nil, fmt.Errorf("feepriority %d out of range, must be 0-4", p)
		}
		feePriority = cxmr.Priority(p)
	}

	walletPath := filepath.Join(cfg.DataDir, walletName)

	w := &ExchangeWallet{
		log:         logger,
		net:         network,
		emit:        cfg.Emit,
		wm:          cxmr.GetWalletManager(),
		walletPath:  walletPath,
		dataDir:     cfg.DataDir,
		daemonAddr:  daemonAddr,
		daemonUser:  cfg.Settings["daemonusername"],
		daemonPass:  cfg.Settings["daemonpassword"],
		feePriority: feePriority,
		peersChange: cfg.PeersChange,
	}

	// monerod scopes digest nonces to the TCP connection, so the authenticated
	// retry has to reuse the connection that was challenged. Keeping a single
	// idle connection to the daemon makes that reliable for these serialized
	// requests. See drainAndClose.
	w.feeRPC = newDaemonRPC(daemonAddr, w.daemonUser, w.daemonPass, &http.Client{
		Timeout: feeRequestTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        1,
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     90 * time.Second,
		},
	})

	w.syncStatus.Store(&asset.SyncStatus{})

	return w, nil
}

// OpenWithPW opens the wallet with the provided password. This must be called
// before Connect. Implements asset.Opener.
func (w *ExchangeWallet) OpenWithPW(ctx context.Context, pw []byte) error {
	if w.isOpen.Load() {
		return nil // Already open
	}

	netType := dexNetworkToCgo(w.net)

	// Use hex-encoded password to match the Create function
	password := hex.EncodeToString(pw)

	w.walletMtx.Lock()
	wallet, err := w.wm.OpenWallet(w.walletPath, password, netType)
	if err != nil {
		w.walletMtx.Unlock()
		return fmt.Errorf("failed to open wallet: %w", err)
	}
	w.wallet = wallet
	w.walletMtx.Unlock()

	w.isOpen.Store(true)
	return nil
}

// IsOpen returns whether the wallet is open. Implements asset.Opener.
func (w *ExchangeWallet) IsOpen() bool {
	return w.isOpen.Load()
}

// Close closes the wallet. Implements asset.Opener.
func (w *ExchangeWallet) Close() error {
	w.log.Info("Closing xmr wallet ...")
	w.walletMtx.Lock()
	defer w.walletMtx.Unlock()

	if w.wallet != nil {
		w.wm.CloseWallet(w.wallet, true)
		w.wallet = nil
	}
	w.isOpen.Store(false)
	return nil
}

// Connect establishes a connection to the Monero daemon. The wallet must be
// opened with OpenWithPW before calling Connect.
func (w *ExchangeWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	if !w.isOpen.Load() {
		return nil, errors.New("wallet not open; call OpenWithPW first")
	}

	ctx, cancel := context.WithCancel(ctx)
	w.ctx.Store(&connCtx{ctx})
	w.cancel = cancel

	w.walletMtx.RLock()
	wallet := w.wallet
	w.walletMtx.RUnlock()

	if wallet == nil {
		return nil, errors.New("wallet is nil")
	}

	// Initialize connection to daemon
	if !wallet.Init(w.daemonAddr, w.daemonUser, w.daemonPass, false, false, "") {
		return nil, fmt.Errorf("failed to initialize daemon connection: %s", wallet.ErrorString())
	}

	// For local/simnet daemons, mark as trusted and allow mismatched daemon
	// version (required for regtest where daemon starts at hard fork v16 from
	// block 0, but wallet expects mainnet hard fork schedule).
	// TODO: This is made possible with a patch on monero_c, but it looks
	// like it is in monero master so should be part of the library at some point.
	if w.net == util.Simnet {
		wallet.SetTrustedDaemon(true)
		wallet.SetAllowMismatchedDaemonVersion(true)
	}

	// Verify connection status
	connStatus := wallet.Connected()
	switch connStatus {
	case cxmr.ConnectionDisconnected:
		return nil, errors.New("failed to connect to daemon: disconnected")
	case cxmr.ConnectionWrongVersion:
		if w.net == util.Mainnet {
			return nil, errors.New("daemon version mismatch: please update your Monero daemon")
		}
		w.log.Warnf("Daemon version mismatch, continuing anyway")
	}

	// Get initial heights for sync status.
	walletHeight := wallet.BlockChainHeight()
	daemonHeight := wallet.DaemonBlockChainHeight()
	synced := wallet.Synchronized()
	w.tipAtConnect = walletHeight

	// Load wallet metadata and apply recovery settings if needed.
	// The wallet2 library doesn't reliably persist setRecoveringFromSeed and
	// setRefreshFromBlockHeight, so we store this info separately and re-apply
	// it on every connect (like Cake Wallet does).
	meta, err := loadWalletMeta(w.dataDir)
	if err != nil {
		w.log.Warnf("Failed to load wallet metadata: %v", err)
	}
	if meta != nil && meta.IsRecovery {
		// Apply recovery settings. Cake Wallet checks if getCurrentHeight() <= 1,
		// but we apply whenever IsRecovery is true and wallet isn't synced yet.
		w.log.Infof("Wallet is in recovery mode, setting refresh from block height %d", meta.RestoreHeight)
		wallet.SetRecoveringFromSeed(true)
		wallet.SetRefreshFromBlockHeight(meta.RestoreHeight)
		w.tipAtConnect = meta.RestoreHeight
	}

	// Prime the standard send fee from the last broadcast, reusing the meta
	// just loaded.
	w.seedFeeFromMeta(meta)

	refreshHeight := wallet.GetRefreshFromBlockHeight()
	w.log.Debugf("Wallet refresh from block height: %d", refreshHeight)

	// Set initial sync status before starting monitors.
	w.syncStatus.Store(&asset.SyncStatus{
		Synced:         synced,
		TargetHeight:   daemonHeight,
		Blocks:         walletHeight,
		StartingBlocks: w.tipAtConnect,
	})

	if !synced {
		blocksRemaining := satSub(daemonHeight, walletHeight)
		w.log.Infof("XMR wallet syncing: %d blocks remaining (wallet: %d, daemon: %d)",
			blocksRemaining, walletHeight, daemonHeight)
	} else {
		w.log.Infof("XMR wallet synchronized at height %d", walletHeight)
	}

	// Start background refresh thread. This handles blockchain synchronization
	// automatically without blocking. We also trigger an immediate async refresh
	// to start syncing right away.
	wallet.StartRefresh()
	wallet.RefreshAsync()

	w.wg.Add(2)
	go w.monitorBlocks()
	go w.monitorPeers()

	return &w.wg, nil
}

func (w *ExchangeWallet) monitorBlocks() {
	defer w.wg.Done()

	ctx := w.context()

	// Use faster polling during initial sync for better progress reporting.
	// The actual refresh is handled by the background thread started via
	// StartRefresh() in Connect(). We just poll the status here.
	syncTicker := time.NewTicker(initialSyncTickerPeriod)
	defer syncTicker.Stop()

	var lastHeight uint64
	synced := false

	for {
		select {
		case <-syncTicker.C:
			w.walletMtx.RLock()
			if w.wallet == nil {
				w.walletMtx.RUnlock()
				continue
			}

			// Don't call Refresh() here - it blocks and prevents shutdown.
			// The background refresh thread (started via StartRefresh) handles
			// blockchain synchronization. We just poll the current status.
			walletHeight := w.wallet.BlockChainHeight()
			daemonHeight := w.wallet.DaemonBlockChainHeight()
			walletSynced := w.wallet.Synchronized()
			w.walletMtx.RUnlock()

			// If we're rescanning, don't trust the wallet's sync status.
			// We're only synced when not rescanning AND wallet reports synced.
			rescanning := w.rescanning.Load()
			nowSynced := walletSynced && !rescanning

			// If rescanning and wallet height has caught up, clear the flag.
			if rescanning && walletHeight >= daemonHeight && daemonHeight > 0 {
				w.rescanning.Store(false)
				rescanning = false
				nowSynced = walletSynced
				w.log.Infof("Rescan complete at height %d", walletHeight)
			}

			w.syncStatus.Store(&asset.SyncStatus{
				Synced:         nowSynced,
				TargetHeight:   daemonHeight,
				Blocks:         walletHeight,
				StartingBlocks: w.tipAtConnect,
			})

			// Once synced, switch to slower polling interval.
			if nowSynced && !synced {
				synced = true
				syncTicker.Reset(syncTickerPeriod)
				w.log.Infof("XMR wallet synchronized at height %d", walletHeight)
				// Save immediately when sync completes.
				w.saveWallet()
				// Mark recovery as complete so we don't re-apply recovery
				// settings on next connect.
				w.clearRecoveryState()
			} else if !nowSynced && synced {
				// If we were synced but now rescanning, reset to fast polling.
				synced = false
				syncTicker.Reset(initialSyncTickerPeriod)
			}

			// Emit tip change if height changed.
			if walletHeight != lastHeight {
				lastHeight = walletHeight
				if w.emit != nil {
					w.emit.TipChange(walletHeight)
				}
			}

		case <-ctx.Done():
			// Pause the background refresh thread FIRST to stop any ongoing
			// rescan/refresh operations. This must happen before saveWallet()
			// because Store() can block if a refresh is in progress.
			//
			// Take the lock and nil-check as every other w.wallet access does:
			// a concurrent Close() nils the handle, which would panic here.
			w.walletMtx.RLock()
			if w.wallet != nil {
				w.wallet.PauseRefresh()
			}
			w.walletMtx.RUnlock()
			// Now save the wallet state.
			w.saveWallet()
			return
		}
	}
}

// saveWallet persists the wallet state to disk.
func (w *ExchangeWallet) saveWallet() {
	w.walletMtx.RLock()
	defer w.walletMtx.RUnlock()
	if w.wallet != nil {
		height := w.wallet.BlockChainHeight()
		if ok := w.wallet.Store(""); !ok {
			w.log.Errorf("Failed to save wallet at height %d: %s", height, w.wallet.ErrorString())
		} else {
			w.log.Tracef("Saved wallet at height %d", height)
		}
	}
}

// clearRecoveryState updates the metadata to mark recovery as complete.
func (w *ExchangeWallet) clearRecoveryState() {
	var cleared bool
	err := w.updateWalletMeta(func(meta *walletMeta) bool {
		if !meta.IsRecovery {
			return false // Nothing to clear
		}
		meta.IsRecovery = false
		cleared = true
		return true
	})
	if err != nil {
		w.log.Warnf("Failed to clear recovery state: %v", err)
	} else if cleared {
		w.log.Infof("Recovery complete, cleared recovery state")
	}
}

func (w *ExchangeWallet) monitorPeers() {
	defer w.wg.Done()

	ctx := w.context()

	ticker := time.NewTicker(peerCountTicker)
	defer ticker.Stop()

	for {
		w.checkPeers()

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (w *ExchangeWallet) checkPeers() {
	if w.peersChange == nil {
		return
	}

	w.walletMtx.RLock()
	wallet := w.wallet
	w.walletMtx.RUnlock()

	if wallet == nil {
		w.peersChange(0, nil)
		return
	}

	// Report 1 peer if we're connected to the daemon, 0 otherwise.
	// The daemon's RPC doesn't reliably report its p2p peer count to us.
	var numPeers uint32
	if wallet.Connected() == cxmr.ConnectionConnected {
		numPeers = 1
	}

	prevPeer := atomic.SwapUint32(&w.lastPeerCount, numPeers)
	if prevPeer != numPeers {
		w.peersChange(numPeers, nil)
	}
}

// Info returns basic wallet information.
func (w *ExchangeWallet) Info() *asset.WalletInfo {
	return WalletInfo
}

// Balance returns the wallet balance. Outputs whose value is below the
// dust threshold are excluded from Available, since Monero's sweep
// (create_transactions_all) skips fractional outputs that cost more in
// fees to spend than they are worth.
func (w *ExchangeWallet) Balance() (*asset.Balance, error) {
	w.walletMtx.RLock()
	defer w.walletMtx.RUnlock()

	if w.wallet == nil {
		return nil, errors.New("wallet not connected")
	}

	total := w.wallet.Balance(0)
	unlocked := w.wallet.UnlockedBalance(0)
	dust := w.dustTotal()

	return &asset.Balance{
		Available: satSub(unlocked, dust),
		Immature:  satSub(total, unlocked),
	}, nil
}

// satSub is a saturating uint64 subtraction, returning zero rather than
// underflowing when b > a. The wallet2 balance and height getters are read at
// different instants and the dust total is cached (see dustCacheDuration), so
// these operands can legitimately disagree — an unguarded subtraction would
// turn a transient skew into a ~1.8e19 balance or block count.
func satSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// dustThreshold is the minimum output value considered spendable. Outputs
// below this are ignored by Monero's sweep (ignore_fractional_outputs)
// because spending them costs more in fees than they are worth. This
// value is conservative and covers all fee priority levels.
const dustThreshold = 1_000_000_000 // 0.001 XMR

// dustTotalFromCoins computes the sum of all unlocked, unspent outputs below
// the dust threshold for account 0. This is expensive since it enumerates all
// wallet outputs via CGO.
func (w *ExchangeWallet) dustTotalFromCoins() uint64 {
	allCoins := w.wallet.AllCoins()
	var dust uint64
	for _, c := range allCoins {
		if c.Spent || !c.Unlocked || c.Frozen || c.SubaddrAcct != 0 {
			continue
		}
		if c.Amount < dustThreshold {
			dust += c.Amount
		}
	}
	return dust
}

const dustCacheDuration = 30 * time.Second

// dustTotal returns the cached dust total, refreshing it if the cache is stale.
func (w *ExchangeWallet) dustTotal() uint64 {
	w.dustMtx.Lock()
	defer w.dustMtx.Unlock()
	if time.Since(w.cachedDustTime) < dustCacheDuration {
		return w.cachedDust
	}
	w.cachedDust = w.dustTotalFromCoins()
	w.cachedDustTime = time.Now()
	return w.cachedDust
}

// DepositAddress returns a new subaddress for receiving funds.
// Each call creates a new subaddress (starting with "8" on mainnet/simnet).
// This matches the behavior of the previous RPC-based implementation.
func (w *ExchangeWallet) DepositAddress() (string, error) {
	w.walletMtx.Lock()
	defer w.walletMtx.Unlock()

	if w.wallet == nil {
		return "", errors.New("wallet not connected")
	}

	// Create a new subaddress for this deposit.
	// Subaddresses start with "8" on mainnet (vs "4" for primary address).
	numAddrs := w.wallet.NumSubaddresses(0)
	w.wallet.AddSubaddress(0, "")

	addr := w.wallet.Address(0, numAddrs)
	w.log.Debugf("Created new deposit subaddress %d: %s", numAddrs, addr)

	return addr, nil
}

// NewAddress generates a new subaddress. Same as DepositAddress.
func (w *ExchangeWallet) NewAddress() (string, error) {
	return w.DepositAddress()
}

// AddressUsed checks if an address has been used (received funds).
func (w *ExchangeWallet) AddressUsed(addr string) (bool, error) {
	// TODO: Always reporting false is a stub, not a real answer — callers
	// cannot distinguish a fresh subaddress from one that has received funds.
	//
	// wallet2 does not expose per-subaddress usage directly, so implementing
	// this means resolving addr to its subaddress index (a linear scan, as in
	// OwnsDepositAddress) and then finding an incoming transfer to that index.
	//
	// Nothing external blocks this: the shipped libmonero_wallet2_api_c.so
	// exports MONERO_TransactionInfo_subaddrIndex and
	// MONERO_CoinsInfo_subaddrIndex, neither of which cxmr binds yet (it binds
	// only the subaddrAccount half of each). Bind one in cxmr, then match it
	// against the resolved index. Both halves are O(n) cgo calls, so cache the
	// result per index.
	return false, nil
}

// OwnsDepositAddress checks if the address belongs to this wallet.
func (w *ExchangeWallet) OwnsDepositAddress(address string) (bool, error) {
	w.walletMtx.RLock()
	defer w.walletMtx.RUnlock()

	if w.wallet == nil {
		return false, errors.New("wallet not connected")
	}

	// Check if it matches any of our subaddresses
	numAddrs := w.wallet.NumSubaddresses(0)
	for i := uint64(0); i < numAddrs; i++ {
		if w.wallet.Address(0, i) == address {
			return true, nil
		}
	}
	return false, nil
}

// ValidateAddress checks if an address is valid.
func (w *ExchangeWallet) ValidateAddress(address string) bool {
	netType := dexNetworkToCgo(w.net)
	return cxmr.AddressValid(address, netType)
}

// Send sends XMR to an address.
func (w *ExchangeWallet) Send(address string, value, feeRate uint64) (asset.Coin, error) {
	// Defers run last-in-first-out, so this one runs after the unlock below.
	defer w.flushPendingFee()

	w.walletMtx.Lock()
	defer w.walletMtx.Unlock()

	if w.wallet == nil {
		return nil, errors.New("wallet not connected")
	}

	tx, err := w.wallet.CreateTransaction(address, value, w.feePriority, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to create transaction: %w", err)
	}

	return w.commitTx(tx, value)
}

// Withdraw withdraws funds, subtracting fees from the amount.
func (w *ExchangeWallet) Withdraw(address string, value, feeRate uint64) (asset.Coin, error) {
	// For Monero, Withdraw subtracts the fee from value, sending
	// (value - fee) to the recipient.

	// Defers run last-in-first-out, so this one runs after the unlock below.
	defer w.flushPendingFee()

	w.walletMtx.Lock()
	defer w.walletMtx.Unlock()

	if w.wallet == nil {
		return nil, errors.New("wallet not connected")
	}

	unlocked := w.wallet.UnlockedBalance(0)
	dust := w.dustTotalFromCoins() // Fresh computation for accuracy.
	spendable := satSub(unlocked, dust)

	// Max withdraw detection. When value >= spendable, sweep the wallet
	// using createTransactionMultDest with amount_sweep_all. This
	// calls create_transactions_all which sends the spendable balance
	// minus fee. Monero's ignore_fractional_outputs setting (enabled
	// by default) skips outputs whose value is less than the marginal
	// fee cost of including them as inputs. These dust outputs are
	// excluded from the reported balance (see dustThreshold) so the
	// user never sees a balance they cannot send.
	if value >= spendable {
		w.log.Debugf("Max withdraw (sweep): unlocked=%d, dust=%d, spendable=%d, value=%d", unlocked, dust, spendable, value)

		tx, err := w.wallet.SweepAll(address, w.feePriority, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to create sweep transaction: %w", err)
		}

		fee := tx.Fee()
		amount := tx.Amount()
		txid := tx.TxID()
		w.log.Debugf("Max withdraw pre-commit: amount=%d, fee=%d, txid=%s, txCount=%d", amount, fee, txid, tx.TxCount())

		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("failed to broadcast sweep transaction: %w", err)
		}

		// Sweep transactions via createTransactionMultDest may not
		// populate Amount() or TxID() correctly.
		if txid == "" {
			txid = tx.TxID()
		}
		if txid == "" {
			// Get txid from the most recent outgoing transaction.
			h := w.wallet.History()
			h.Refresh()
			if n := h.Count(); n > 0 {
				if ti := h.Transaction(n - 1); ti != nil {
					txid = ti.Hash()
					w.log.Debugf("Max withdraw: got txid from history: %s", txid)
				}
			}
		}
		if amount == 0 && fee > 0 {
			// spendable is satSub(unlocked, dust) and can legitimately be
			// zero when every output is below dustThreshold, in which case
			// SweepAll still succeeds and reports Amount() == 0. An unguarded
			// subtraction would report ~1.8e19 as the sent amount.
			amount = satSub(spendable, fee)
		}

		w.log.Debugf("Max withdraw committed: amount=%d, fee=%d, txid=%s", amount, fee, txid)
		coinID, _ := hex.DecodeString(txid)
		return &Coin{
			txHash: txid,
			value:  amount,
			id:     coinID,
		}, nil
	}

	// For partial withdrawals, iteratively find the right send amount.
	// The fee depends on the transaction structure (number of
	// inputs/outputs), which can change when the send amount crosses a
	// threshold that requires more inputs or outputs.
	feeEstimate, err := w.estimateFee(address, value)
	if err != nil {
		return nil, fmt.Errorf("failed to estimate fee: %w", err)
	}

	seenFees := make(map[uint64]bool)
	var lastActualFee uint64

	for i := 0; i < 8; i++ {
		if value <= feeEstimate {
			return nil, fmt.Errorf("withdrawal amount %d is less than fee %d", value, feeEstimate)
		}

		sendAmount := value - feeEstimate
		tx, err := w.wallet.CreateTransaction(address, sendAmount, w.feePriority, 0)
		if err != nil {
			if lastActualFee > 0 {
				feeEstimate = lastActualFee + 1
				lastActualFee = 0
			} else {
				feeEstimate *= 2
			}
			continue
		}

		actualFee := tx.Fee()
		lastActualFee = actualFee

		if actualFee == feeEstimate {
			return w.commitTx(tx, sendAmount)
		}

		if seenFees[actualFee] {
			return w.commitTx(tx, sendAmount)
		}

		seenFees[actualFee] = true
		feeEstimate = actualFee
	}

	return nil, fmt.Errorf("failed to create withdrawal transaction for %d after fee adjustments", value)
}

// estimateFee returns the fee wallet2 would charge to send value to address,
// by building the transaction and reading its fee. If building for the full
// value fails (e.g. because value + fee > balance), a reduced value is used.
// The fee may differ from the actual transaction's fee if a different number
// of inputs is selected; callers should verify and retry.
//
// TODO: This leaks. wallet2_api hands out a PendingTransaction the caller owns
// and must return via Wallet::disposeTransaction, but the monero_c build
// pinned in the Dockerfile exports no disposeTransaction wrapper — verified
// against the shipped libmonero_wallet2_api_c.so, whose only disposal-shaped
// exports are MONERO_WalletManager_closeWallet and MONERO_AddressBook_deleteRow.
// So every CreateTransaction and SweepAll in this file leaks its transaction:
// one or two per call here, and up to eight per withdrawal in Withdraw's
// convergence loop. Fixing it means adding a monero_c patch exposing
// disposeTransaction — the Dockerfile already carries six local patches —
// rebuilding the shared library per platform, binding it in cxmr, and
// disposing at every construction site. Until then, do not call any of these
// on a timer.
func (w *ExchangeWallet) estimateFee(address string, value uint64) (uint64, error) {
	testTx, err := w.wallet.CreateTransaction(address, value, w.feePriority, 0)
	if err == nil {
		return testTx.Fee(), nil
	}
	// The full value likely exceeds what the wallet can send (value + fee >
	// balance). Use 3/4 of the unlocked balance to get a fee estimate.
	unlocked := w.wallet.UnlockedBalance(0)
	testValue := unlocked * 3 / 4
	if testValue == 0 {
		return 0, fmt.Errorf("insufficient unlocked balance")
	}
	testTx, err = w.wallet.CreateTransaction(address, testValue, w.feePriority, 0)
	if err != nil {
		return 0, err
	}
	return testTx.Fee(), nil
}

// commitTx commits a pending transaction and returns the resulting Coin.
// sendAmount is the expected send amount, used as fallback if the library
// returns 0 for Amount().
//
// Callers hold walletMtx exclusively, so they must also arrange for
// flushPendingFee to run once they have released it.
func (w *ExchangeWallet) commitTx(tx *cxmr.PendingTransaction, sendAmount uint64) (*Coin, error) {
	// Capture values before commit — the library may clear them after.
	amount := tx.Amount()
	fee := tx.Fee()
	txid := tx.TxID()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to broadcast transaction: %w", err)
	}

	// Only now, past the broadcast: an actually-broadcast transaction is the
	// most authoritative fee this wallet can observe, but one that failed to
	// send is not a fee anyone paid. See StandardSendFee.
	w.recordFee(fee)
	w.pendingFee.Store(fee)

	// Try post-commit if pre-commit was empty.
	if txid == "" {
		txid = tx.TxID()
	}
	if txid == "" {
		h := w.wallet.History()
		h.Refresh()
		if n := h.Count(); n > 0 {
			if ti := h.Transaction(n - 1); ti != nil {
				txid = ti.Hash()
			}
		}
	}
	if amount == 0 {
		amount = sendAmount
	}

	w.log.Debugf("commitTx: amount=%d, fee=%d, txid=%s", amount, fee, txid)
	coinID, _ := hex.DecodeString(txid)
	return &Coin{
		txHash: txid,
		value:  amount,
		id:     coinID,
	}, nil
}

// FeeRate returns the daemon's current dynamic base fee, in atoms per weight
// unit, for the configured priority. It returns zero when the daemon cannot be
// reached, which the interface documents as "no estimate available".
//
// This is a real rate in the unit XMR's UnitInfo advertises (FeeRateDenom
// "B"), not the priority level. Callers still cannot steer the fee with it:
// Send, Withdraw and EstimateSendTxFee ignore their feeRate argument, because
// wallet2 prices from the priority rather than a caller-supplied rate.
func (w *ExchangeWallet) FeeRate() uint64 {
	est := w.feeEstimate()
	if est == nil {
		return 0
	}
	return est.feeForPriority(int(w.feePriority))
}

// feeEstimate returns the daemon's fee estimate, refreshing the cache when
// stale. It returns nil if the daemon has never answered.
func (w *ExchangeWallet) feeEstimate() *feeEstimate {
	if cached := w.feeCache.get(); cached != nil &&
		time.Since(time.Unix(0, cached.fetched)) < feeCacheDuration {
		return cached.est
	}

	// One fetch at a time, so the cooldown below actually bounds an outage to
	// one stalled caller per interval and the daemon sees one connection. See
	// feeMtx.
	w.feeMtx.Lock()
	defer w.feeMtx.Unlock()

	// Re-check: a fetch may have finished while this caller waited for the
	// lock, in which case it is already serving what this one would ask for.
	now := time.Now()
	cached := w.feeCache.get()
	if cached != nil && now.Sub(time.Unix(0, cached.fetched)) < feeCacheDuration {
		return cached.est
	}

	// Serving a stale rate beats stalling the caller. These lookups happen
	// behind the send form's debounced fee estimate, so retrying an
	// unreachable daemon on every call would block a caller for
	// feeRequestTimeout each time an amount changes.
	if w.feeCache.coolingDown(now, feeRetryInterval) {
		if cached != nil {
			return cached.est
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(w.context(), feeRequestTimeout)
	defer cancel()

	est, err := w.feeRPC.getFeeEstimate(ctx)

	// Stamp both outcomes at completion rather than with the now taken above.
	// A request can take up to feeRequestTimeout, and dating the result from
	// before it was made would shorten both the cache window and the cooldown
	// by however long it ran — most of it, in the timeout case the cooldown
	// exists for.
	done := time.Now().UnixNano()

	if err != nil {
		// The cooldown set here also throttles this log line.
		w.feeCache.fail(done)
		w.log.Warnf("Failed to fetch fee estimate from daemon: %v", err)
		if cached != nil {
			return cached.est
		}
		return nil
	}
	w.feeCache.set(est, done)
	return est
}

// StandardSendFee returns the fee for a standard send at the given rate.
//
// Monero's fee is base_fee * tx_weight * priority_multiplier, quantized. The
// daemon supplies the first and third (folded together into the per-priority
// rate feeRate carries), but the weight depends on how many inputs coin
// selection picks, which is unknowable before a transaction is built. So a
// typical transfer's weight is assumed — see standardTxWeight.
//
// When no rate is available the last fee this wallet actually paid is used,
// which is exact for the transaction that produced it, and failing that a
// fixed estimate.
//
// The result is rounded up to the daemon's quantization mask only when an
// estimate is on hand to supply one, so an unreachable daemon can return a fee
// a few atoms below what the mask would round it to.
//
// feeRate is treated as untrusted. It arrives from an exported interface
// method, and the rate a caller would normally pass came from a daemon that
// may be a public third-party node, so a rate past the sanity ceiling is
// refused rather than multiplied into an overflow.
func (w *ExchangeWallet) StandardSendFee(feeRate uint64) uint64 {
	est := w.feeEstimate()
	if feeRate == 0 && est != nil {
		feeRate = est.feeForPriority(int(w.feePriority))
	}
	if feeRate == 0 || feeRate > maxBaseFee {
		if feeRate > maxBaseFee {
			w.log.Warnf("Ignoring fee rate %d atoms per weight unit, above the %d sanity ceiling",
				feeRate, maxBaseFee)
		}
		if observed := w.observedFee.Load(); observed > 0 {
			return observed
		}
		return w.fallbackFee()
	}

	// maxBaseFee bounds feeRate, so this cannot overflow.
	fee := feeRate * standardTxWeight
	if est != nil {
		fee = est.quantize(fee)
	}
	return fee
}

// EstimateSendTxFee estimates the fee for sending a transaction.
func (w *ExchangeWallet) EstimateSendTxFee(address string, value, feeRate uint64, subtract, maxWithdraw bool) (uint64, bool, error) {
	w.walletMtx.Lock()
	defer w.walletMtx.Unlock()

	if w.wallet == nil {
		return 0, false, errors.New("wallet not connected")
	}

	if !cxmr.AddressValid(address, dexNetworkToCgo(w.net)) {
		return 0, false, nil
	}

	sendValue := value
	if maxWithdraw {
		sendValue = w.wallet.UnlockedBalance(0)
	}

	// For max withdraw or subtract, use estimateFee which falls back to a
	// reduced test value if sendValue + fee > balance.
	if maxWithdraw || subtract {
		fee, err := w.estimateFee(address, sendValue)
		if err != nil {
			return 0, true, nil
		}
		return fee, true, nil
	}

	tx, err := w.wallet.CreateTransaction(address, sendValue, w.feePriority, 0)
	if err != nil {
		// Return 0 fee but valid address if we can't create tx (e.g., insufficient funds)
		return 0, true, nil
	}

	fee := tx.Fee()
	w.recordFee(fee)
	return fee, true, nil
}

// SyncStatus returns the wallet sync status.
func (w *ExchangeWallet) SyncStatus() (*asset.SyncStatus, error) {
	statusI := w.syncStatus.Load()
	if statusI == nil {
		return nil, errors.New("sync status not initialized")
	}
	status, ok := statusI.(*asset.SyncStatus)
	if !ok {
		return nil, fmt.Errorf("unexpected type for syncStatus: %T", statusI)
	}
	return status, nil
}

const (
	// standardTxWeight is the assumed weight of a "standard" send: a
	// 2-input/2-output transfer with ring size 16 and bulletproofs+. For a
	// transaction of ordinary shape, weight equals serialized size in bytes.
	// Real sends vary with the number of inputs coin selection picks, so a fee
	// derived from this is an estimate; the fee actually charged is the one
	// wallet2 computes when the transaction is built.
	standardTxWeight = 1500

	// fallbackSendFee is the last resort, used only when the daemon is
	// unreachable and this wallet has never paid a fee. It is standardTxWeight
	// times the base fee observed on mainnet in July 2026 (20000 atoms per
	// weight unit at default priority), so it starts out close to real rather
	// than merely plausible. It cannot track the dynamic fee, which is the
	// whole reason the daemon is asked first.
	//
	// This is the default-priority figure. Scale it with fallbackFee rather
	// than using it directly.
	fallbackSendFee = 30_000_000 // 0.00003 XMR

	// feeCacheDuration bounds how stale a reported rate can be. The dynamic
	// base fee moves with median block weight, so it barely changes within a
	// ~2 minute block interval.
	feeCacheDuration = 60 * time.Second

	// feeRequestTimeout caps a daemon fee request. These calls sit behind
	// synchronous, error-free interface methods invoked from UI polls, so
	// they must fail fast rather than stall a caller.
	feeRequestTimeout = 10 * time.Second

	// feeRetryInterval is how long to wait after a failed fetch before trying
	// the daemon again, bounding an outage to one attempt per interval no
	// matter how often callers ask. Callers that arrive while an attempt is in
	// flight wait behind it on feeMtx, so they can still stall for up to
	// feeRequestTimeout; what the interval bounds is how often that recurs.
	feeRetryInterval = 15 * time.Second
)

// fallbackFeeMultipliers are the multipliers monerod applies to the dynamic
// base fee, for priorities Low(1) through Last(4). Confirmed against mainnet
// in July 2026, where a base fee of 20000 atoms per weight unit yields a fees
// array of [20000, 80000, 320000, 4000000].
//
// These are the daemon's numbers rather than a consensus rule, and monerod has
// changed them at past hardforks. Going stale is tolerable: they only scale
// fallbackSendFee, which is reached solely when the daemon is unreachable and
// this wallet has never paid a fee. Whenever the daemon answers, its own fees
// array is used and is authoritative.
var fallbackFeeMultipliers = [4]uint64{1, 4, 16, 200}

// fallbackFee scales fallbackSendFee, which is quoted at the default priority,
// to the priority this wallet is configured for. Without this a wallet set to
// Last would report a fee 200x below what its send actually costs, on exactly
// the cold-start path the fallback exists to serve.
func (w *ExchangeWallet) fallbackFee() uint64 {
	idx := priorityIndex(int(w.feePriority), len(fallbackFeeMultipliers))
	return fallbackSendFee * fallbackFeeMultipliers[idx]
}

// recordFee stores a fee observed from a transaction this wallet constructed.
// It is only ever called with a fee built at w.feePriority, so the in-memory
// value always matches the current priority.
func (w *ExchangeWallet) recordFee(fee uint64) {
	if fee > 0 {
		w.observedFee.Store(fee)
	}
}

// flushPendingFee writes any fee commitTx left behind to the meta file. It is
// deferred by the methods that call commitTx, registered ahead of their
// walletMtx unlock so that defer ordering runs it after the lock is released:
// persistFee reads and rewrites a file, and every Balance, TxHistory or
// NewAddress caller would otherwise wait on that write for the whole of each
// broadcast.
//
// Taking the fee rather than reading it means a broadcast that lands between
// the unlock and this call is persisted by its own flush and skipped here. The
// value is the same kind either way, and observedFee already carries the
// latest one in memory.
func (w *ExchangeWallet) flushPendingFee() {
	if fee := w.pendingFee.Swap(0); fee != 0 {
		w.persistFee(fee)
	}
}

// persistFee records a broadcast fee, and the priority it was built at, to the
// meta file so StandardSendFee can report a real number immediately on the
// next startup. Only called for actually-broadcast transactions: fee estimates
// can fire on every keystroke of a send form and are not worth the file churn.
func (w *ExchangeWallet) persistFee(fee uint64) {
	if fee == 0 {
		return
	}
	priority := int(w.feePriority)
	err := w.updateWalletMeta(func(meta *walletMeta) bool {
		if meta.LastFee == fee && meta.LastFeePriority == priority {
			return false
		}
		meta.LastFee, meta.LastFeePriority = fee, priority
		return true
	})
	if err != nil {
		w.log.Warnf("Failed to persist observed fee: %v", err)
	}
}

// updateWalletMeta applies mutate to the stored wallet meta and writes it back
// if mutate reports a change. A missing meta file is treated as an empty one.
func (w *ExchangeWallet) updateWalletMeta(mutate func(*walletMeta) bool) error {
	w.metaMtx.Lock()
	defer w.metaMtx.Unlock()

	meta, err := loadWalletMeta(w.dataDir)
	if err != nil {
		return err
	}
	if meta == nil {
		meta = new(walletMeta)
	}
	if !mutate(meta) {
		return nil
	}
	return saveWalletMeta(w.dataDir, meta)
}

// seedFeeFromMeta primes observedFee from the last broadcast fee, but only if
// it was built at the priority now configured. Nothing else can supply a
// priority-attributed fee at startup: wallet2's TransactionInfo exposes no
// priority accessor, so a fee read back from transaction history cannot be
// told apart from one built at a different priority. Reporting such a fee
// after the user changed feepriority would be confidently wrong, and since XMR
// implements no LiveReconfigurer, a priority change always arrives as a
// restart — exactly this path.
func (w *ExchangeWallet) seedFeeFromMeta(meta *walletMeta) {
	if meta == nil || meta.LastFee == 0 {
		return
	}
	if meta.LastFeePriority != int(w.feePriority) {
		w.log.Debugf("Ignoring stored fee %d atoms: built at priority %d, now configured for %d",
			meta.LastFee, meta.LastFeePriority, w.feePriority)
		return
	}
	w.observedFee.Store(meta.LastFee)
	w.log.Debugf("Seeded standard send fee from meta: %d atoms", meta.LastFee)
}

// TxHistory returns transaction history.
//
// NOTE: only req.N is honored. The refID and past fields, which asset.Wallet
// documents for pagination, are ignored, so paging through XMR history does
// not work.
func (w *ExchangeWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	// A nil request is treated as an empty result, as the other assets do,
	// rather than dereferenced.
	if req == nil {
		return &txdb.TxHistoryResponse{}, nil
	}

	w.walletMtx.RLock()
	defer w.walletMtx.RUnlock()

	if w.wallet == nil {
		return nil, errors.New("wallet not connected")
	}

	history := w.wallet.History()
	history.Refresh()

	count := history.Count()
	if req.N > 0 && req.N < count {
		count = req.N
	}

	txs := make([]*txdb.WalletTransaction, 0, count)
	for i := 0; i < count; i++ {
		ti := history.Transaction(i)
		if ti == nil {
			continue
		}

		txType := txdb.Send
		if ti.Direction() == cxmr.DirectionIn {
			txType = txdb.Receive
		}

		txs = append(txs, &txdb.WalletTransaction{
			NetworkID:   BipID,
			AssetID:     BipID,
			Type:        txType,
			ID:          ti.Hash(),
			Amount:      ti.Amount(),
			Fees:        ti.Fee(),
			BlockNumber: ti.BlockHeight(),
			Timestamp:   ti.Timestamp(),
			Confirmed:   ti.Confirmations() > 0,
		})
	}

	return &txdb.TxHistoryResponse{
		Txs: txs,
	}, nil
}

// WalletTransaction returns a single transaction.
func (w *ExchangeWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	w.walletMtx.RLock()
	defer w.walletMtx.RUnlock()

	if w.wallet == nil {
		return nil, errors.New("wallet not connected")
	}

	history := w.wallet.History()
	history.Refresh()

	for i := 0; i < history.Count(); i++ {
		ti := history.Transaction(i)
		if ti != nil && ti.Hash() == txID {
			txType := txdb.Send
			if ti.Direction() == cxmr.DirectionIn {
				txType = txdb.Receive
			}
			return &txdb.WalletTransaction{
				NetworkID:   BipID,
				AssetID:     BipID,
				Type:        txType,
				ID:          ti.Hash(),
				Amount:      ti.Amount(),
				Fees:        ti.Fee(),
				BlockNumber: ti.BlockHeight(),
				Timestamp:   ti.Timestamp(),
				Confirmed:   ti.Confirmations() > 0,
			}, nil
		}
	}

	return nil, errors.New("transaction not found")
}

// PendingTransactions returns pending (unconfirmed) transactions.
func (w *ExchangeWallet) PendingTransactions(ctx context.Context) []*txdb.WalletTransaction {
	w.walletMtx.RLock()
	defer w.walletMtx.RUnlock()

	if w.wallet == nil {
		return nil
	}

	history := w.wallet.History()
	history.Refresh()

	var pending []*txdb.WalletTransaction
	for i := 0; i < history.Count(); i++ {
		ti := history.Transaction(i)
		if ti == nil || ti.IsFailed() {
			continue
		}
		// Use Confirmations() == 0 rather than IsPending() because
		// wallet2's IsPending() does not return true for incoming pool
		// transactions.
		if ti.Confirmations() > 0 {
			continue
		}
		txType := txdb.Send
		if ti.Direction() == cxmr.DirectionIn {
			txType = txdb.Receive
		}
		pending = append(pending, &txdb.WalletTransaction{
			NetworkID: BipID,
			AssetID:   BipID,
			Type:      txType,
			ID:        ti.Hash(),
			Amount:    ti.Amount(),
			Fees:      ti.Fee(),
			Timestamp: ti.Timestamp(),
			Confirmed: false,
		})
	}

	return pending
}

// Rescan starts a rescan from the given birthday and returns as soon as it has
// been scheduled. The birthday is a unix timestamp in seconds, converted to an
// approximate block height; 0 rescans from genesis. Implements
// asset.Rescanner.
//
// NOTE: this deviates from asset.Rescanner, which documents Rescan as blocking
// until the rescan is done. A Monero rescan re-scans every block from the
// restore height and can take hours, so blocking would hang the caller for the
// entire duration. The rescan is handed to wallet2's background refresh thread
// via RescanBlockchainAsync and its progress is reported through SyncStatus:
// w.rescanning suppresses Synced until the wallet height catches back up to
// the daemon (see monitorBlocks). Callers that need completion must poll
// SyncStatus rather than wait on this return.
func (w *ExchangeWallet) Rescan(_ context.Context, bday uint64) error {
	return w.rescanFromHeight(birthdayToHeight(bday, w.net), bday)
}

// birthdayToHeight converts a unix timestamp (seconds) to an approximate
// block height for the current network. Returns 0 if bday is 0 or before
// the network genesis.
func birthdayToHeight(bday uint64, net util.Network) uint64 {
	if bday == 0 {
		return 0
	}

	// Monero has ~2 minute block times across all networks.
	const avgBlockTime uint64 = 120 // 2 minutes

	// Genesis timestamps for each network. These are approximate timestamps
	// of when each network started producing blocks.
	var genesisTime uint64
	switch net {
	case util.Mainnet:
		// Mainnet genesis: April 18, 2014
		genesisTime = 1397818193
	case util.Testnet:
		// Testnet launched alongside mainnet in April 2014.
		genesisTime = 1397818193
	case util.Simnet:
		// Simnet/regtest: local network, always scan from genesis
		return 0
	default:
		// Unknown network, use mainnet genesis as fallback
		genesisTime = 1397818193
	}

	if bday < genesisTime {
		return 0
	}

	return (bday - genesisTime) / avgBlockTime
}

// saveWalletMeta saves wallet metadata to a JSON file.
func saveWalletMeta(dataDir string, meta *walletMeta) error {
	metaPath := filepath.Join(dataDir, walletMetaFile)
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal wallet meta: %w", err)
	}
	if err := os.WriteFile(metaPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write wallet meta: %w", err)
	}
	return nil
}

// loadWalletMeta loads wallet metadata from a JSON file.
// Returns nil if the file doesn't exist.
func loadWalletMeta(dataDir string) (*walletMeta, error) {
	metaPath := filepath.Join(dataDir, walletMetaFile)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read wallet meta: %w", err)
	}
	var meta walletMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal wallet meta: %w", err)
	}
	return &meta, nil
}

// rescanFromHeight performs a wallet rescan from a specific block height.
func (w *ExchangeWallet) rescanFromHeight(height, bday uint64) error {
	w.walletMtx.Lock()
	defer w.walletMtx.Unlock()

	if w.wallet == nil {
		return errors.New("wallet not connected")
	}

	// Update our metadata file with the new restore height. Read-modify-write
	// rather than replacing the file: a rescan changes nothing about the fee,
	// so the stored LastFee/LastFeePriority must survive it.
	err := w.updateWalletMeta(func(meta *walletMeta) bool {
		meta.RestoreHeight, meta.Birthday, meta.IsRecovery = height, bday, true
		return true
	})
	if err != nil {
		w.log.Warnf("Failed to save wallet metadata: %v", err)
	}

	// Mark as recovering and set the refresh height. This is the same flow
	// used by Cake Wallet when recovering/rescanning. SetRecoveringFromSeed
	// tells the wallet2 library to scan for transactions from the specified
	// height rather than just checking for new blocks.
	w.wallet.SetRecoveringFromSeed(true)
	w.wallet.SetRefreshFromBlockHeight(height)

	// Get daemon height before triggering rescan.
	daemonHeight := w.wallet.DaemonBlockChainHeight()

	// Mark as rescanning so monitorBlocks doesn't report synced status.
	w.rescanning.Store(true)

	// RescanBlockchainAsync sets m_refreshShouldRescan=true which causes
	// doRefresh to call wallet2::rescan_blockchain(), clearing cached
	// transfers, key images, and payments before rescanning.
	w.wallet.RescanBlockchainAsync()

	// Update sync status to reflect the rescan.
	w.tipAtConnect = height
	w.syncStatus.Store(&asset.SyncStatus{
		Synced:         false,
		TargetHeight:   daemonHeight,
		Blocks:         height,
		StartingBlocks: height,
	})

	w.log.Infof("Started rescan from block height %d (target: %d)", height, daemonHeight)
	return nil
}

// PrimaryAddress returns the wallet's primary address (starting with "4").
// Note: For receiving funds, use DepositAddress() which creates subaddresses.
func (w *ExchangeWallet) PrimaryAddress() (string, error) {
	w.walletMtx.RLock()
	defer w.walletMtx.RUnlock()

	if w.wallet == nil {
		return "", errors.New("wallet not connected")
	}

	return w.wallet.Address(0, 0), nil
}

// GenerateSubaddresses pre-generates a specified number of subaddresses.
func (w *ExchangeWallet) GenerateSubaddresses(count uint64) error {
	w.walletMtx.Lock()
	defer w.walletMtx.Unlock()

	if w.wallet == nil {
		return errors.New("wallet not connected")
	}

	for i := uint64(0); i < count; i++ {
		w.wallet.AddSubaddress(0, "")
	}

	w.wallet.Store("")
	return nil
}

// RecoverWithSubaddresses generates subaddresses and rescans from block 0.
func (w *ExchangeWallet) RecoverWithSubaddresses(subaddressCount uint64) error {
	if err := w.GenerateSubaddresses(subaddressCount); err != nil {
		return fmt.Errorf("failed to generate subaddresses: %w", err)
	}
	return w.rescanFromHeight(0, 0)
}

// Coin represents a Monero transaction output.
type Coin struct {
	txHash string
	value  uint64
	id     []byte
}

func (c *Coin) ID() util.Bytes {
	return c.id
}

func (c *Coin) TxID() string {
	return c.txHash
}

func (c *Coin) String() string {
	return c.txHash
}

func (c *Coin) Value() uint64 {
	return c.value
}
