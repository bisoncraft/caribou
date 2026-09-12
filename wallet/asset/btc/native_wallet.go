// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

// Package-level note: this file implements NativeWallet, a standalone
// asset.Wallet shared by the btc and ltc packages and used for both the electrum
// and SPV wallet types — they differ only in the chain backend it builds. All
// UTXO operations are delegated to wallet.Wallet (the in-tree utxowallet) which
// owns and manages its chain backend. Swap / FindRedemption methods are
// intentionally absent.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/feeratefetcher"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet/txauthor"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	_ "github.com/bisoncraft/caribou/wallet/utxowallet/walletdb/bdb" // bdb init() registers the walletdb driver
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// dbTimeout is the bbolt open timeout for the wallet and neutrino databases.
const dbTimeout = 20 * time.Second

// defaultAcctNum is the wallet's default account number.
const defaultAcctNum = 0

// wAddrMgrBkt is the waddrmgr namespace bucket key in the wallet database.
var wAddrMgrBkt = []byte("waddrmgr")

// extendAddresses pre-derives extIdx external and intIdx internal addresses
// on the wallet's primary key scope so recovery scans cover previously-used
// addresses. The primary scope is chain-dependent (BIP84 for BTC/LTC, BIP44
// for non-segwit chains like Dash), so it must not be hardcoded — extending a
// non-primary scope would leave the branches actually used for derivation at
// index 0.
func extendAddresses(extIdx, intIdx uint32, btcw *wallet.Wallet) error {
	scopedKeyManager, err := btcw.Manager.FetchScopedKeyManager(btcw.PrimaryScope())
	if err != nil {
		return err
	}

	return walletdb.Update(btcw.Database(), func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(wAddrMgrBkt)
		if extIdx > 0 {
			if err := scopedKeyManager.ExtendExternalAddresses(ns, defaultAcctNum, extIdx); err != nil {
				return err
			}
		}
		if intIdx > 0 {
			return scopedKeyManager.ExtendInternalAddresses(ns, defaultAcctNum, intIdx)
		}
		return nil
	})
}

// ChainConfig carries the per-chain values that parameterize the shared
// NativeWallet, so the btc and ltc asset packages can share one asset.Wallet
// implementation across both the electrum and SPV wallet types. (The
// implementation lives here in asset/btc for now; it will move to a neutral home
// later, together with the other shared wallet.Wallet stack. asset/ltc imports it.)
// ElectrumRecoveryWindow is the default address-recovery window (gap limit)
// for electrum-backed wallets. It drives the address-probe discovery scan
// (chain.AddressProber) that runs during syncWithChain: on a fresh-seed
// restore the scan discovers used addresses branch-wide; on an ordinary
// reconnect it probes only this many addresses beyond each branch's pool top,
// so the per-connect cost is bounded regardless of wallet age. 20 is the
// standard BIP44/electrum gap limit.
const ElectrumRecoveryWindow = 20

type ChainConfig struct {
	Chain       bisonwire.Chain
	ChainParams *chaincfg.Params
	NetParams   *netparams.ChainParams
	Info        *asset.WalletInfo
	// BackendKind selects the chain backend the wallet.Wallet builds and owns.
	// Electrum wallets use BackendElectrum (default); SPV wallets use
	// BackendNeutrino. The wallet constructs the backend from its chainParams and
	// dataDir on the first Synchronize() call.
	BackendKind wallet.BackendKind
	// RecoveryWindow is the default address-recovery window. Electrum sets
	// ElectrumRecoveryWindow, driving the address-probe discovery scan; neutrino
	// sets it >0 so btcwallet's recovery drives the compact-filter scan. The
	// "recoverywindow" wallet setting, when present, overrides this ("0"
	// disables recovery).
	RecoveryWindow uint32
	// FeeRateSources are the external fee-rate APIs the wallet composites into
	// its FeeRate estimate, consulted before the electrum-server fallback. The
	// lean native wallet has no local fee-estimation RPC, so with an empty
	// list FeeRate depends on the electrum fallback alone.
	FeeRateSources []*feeratefetcher.SourceConfig
}

// btcElectrumChainConfig returns the ChainConfig for the BTC electrum wallet.
func btcElectrumChainConfig(chainParams *chaincfg.Params) ChainConfig {
	return ChainConfig{
		Chain:          bisonwire.ChainBTC,
		ChainParams:    chainParams,
		NetParams:      btcTableNetParams(chainParams),
		Info:           WalletInfo,
		BackendKind:    wallet.BackendElectrum,
		FeeRateSources: BitcoinFeeSources(),
		RecoveryWindow: ElectrumRecoveryWindow,
	}
}

// btcTableNetParams looks up the *netparams.ChainParams for chainParams from the
// assets table, which carries ElectrumServers and NeutrinoSeeds. It does NOT
// call chaincfg.Register; registration is only done when explicitly required
// (e.g. assets.NetParams) and must not happen on every wallet open.
func btcTableNetParams(chainParams *chaincfg.Params) *netparams.ChainParams {
	switch chainParams.Name {
	case "testnet3":
		return assets.BTCParams["testnet"]
	case "regtest":
		return assets.BTCParams["simnet"]
	default:
		return assets.BTCParams["mainnet"]
	}
}

// electrumWalletDefinition is the WalletDefinition for the electrum-backed
// seeded BTC wallet. No user-configurable server/proxy knobs; the server is
// chosen automatically from the built-in per-network defaults.
var electrumWalletDefinition = &asset.WalletDefinition{
	Type:        walletTypeElectrum,
	Tab:         "Electrum",
	Description: "Use the built-in electrum-backed light wallet.",
	ConfigOpts:  nil,
	Seeded:      true,
}

// CreateElectrumWallet creates the on-disk btcwallet database for a NativeWallet
// of the chain described by cc — the same database serves the electrum and SPV
// wallet types. No chain backend is started here. Shared by the btc and ltc
// asset packages. opts are forwarded to the underlying loader; production
// callers should leave them empty (default scrypt params), tests may pass
// wallet.WithScryptOptions(&waddrmgr.FastScryptOptions) to avoid paying the
// full KDF cost per wallet.
func CreateElectrumWallet(cc ChainConfig, privPass, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32, opts ...wallet.LoaderOption) error {

	loader := wallet.NewLoader(cc.NetParams, walletDir, true, dbTimeout, 0, opts...) // recovery_window=0; see Connect
	pubPass := []byte(wallet.InsecurePubPassphrase)

	btcw, err := loader.CreateNewWallet(pubPass, privPass, seed, bday)
	if err != nil {
		return fmt.Errorf("CreateNewWallet error: %w", err)
	}

	bail := func() {
		if err := loader.UnloadWallet(); err != nil {
			log.Errorf("Error unloading wallet after createElectrumWallet error: %v", err)
		}
	}

	if extIdx > 0 || intIdx > 0 {
		if err = extendAddresses(extIdx, intIdx, btcw); err != nil {
			bail()
			return fmt.Errorf("failed to set starting address indexes: %w", err)
		}
	}

	if err := loader.UnloadWallet(); err != nil {
		return fmt.Errorf("error unloading wallet: %w", err)
	}
	return nil
}

// createElectrumWallet creates the on-disk btcwallet database for a BTC
// NativeWallet (electrum or SPV).
func createElectrumWallet(privPass, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32, net *chaincfg.Params) error {
	return CreateElectrumWallet(btcElectrumChainConfig(net), privPass, seed, bday, walletDir, log, extIdx, intIdx)
}

// NativeWallet is an asset.Wallet backed by an in-process chain backend — an
// electrumx connection (electrum wallets) or a neutrino chain service (SPV
// wallets). It does NOT embed any ExchangeWallet type. The chain backend is owned
// by the underlying wallet.Wallet, which builds and starts it on Synchronize().
type NativeWallet struct {
	cfg         ChainConfig
	log         util.Logger
	dir         string
	chainParams *chaincfg.Params
	netParams   *netparams.ChainParams
	emit        *asset.WalletEmitter
	peersChange func(uint32, error)

	// networkID and assetID identify this wallet's chain in the master
	// transaction database records. This wallet type serves the clone assets
	// (ltc, dash) too, so records must carry the configured chain's identity
	// from ChainConfig.Info, never the btc package's own BipID.
	networkID uint32
	assetID   uint32

	// ctx is the wallet's lifetime context, set in Connect. It is used for
	// on-demand fee-rate fetches so they cancel with the wallet. It is
	// atomic because FeeRate is reachable from request goroutines that have
	// no happens-before edge to an in-flight Connect.
	ctx atomic.Value // context.Context

	// feeFetcher composites external fee-rate sources for FeeRate. The
	// mainnet sources are configured on every network for BTC and LTC; it is
	// nil only for a chain configured without sources.
	feeFetcher *feeratefetcher.Fetcher

	// recoveryWindow is the address-recovery window: the address-probe gap
	// limit for electrum (default ElectrumRecoveryWindow), the block-walk
	// recovery window for neutrino. 0 disables recovery. See
	// wallet.syncWithChain.
	recoveryWindow uint32

	// watchDepths carries the "externalquietdepth" and "sweepinterval"
	// settings (0 = default) into the loader; test knobs for the watch
	// policy's cold-address lifecycle.
	watchDepths [2]int32

	// populated during Connect
	btcw   *wallet.Wallet
	loader *wallet.Loader
	// rescanMtx serializes Rescan, which stops and restarts the wallet and the
	// chain backend underneath the running notification goroutines.
	rescanMtx sync.Mutex
	// txdb is the master transaction database handle, scoped to this wallet's
	// network and supplied by Core via the wallet config. Core owns its lifecycle.
	txdb txdb.NetworkTxDB

	// pendingTxs caches unconfirmed transactions.
	pendingMtx sync.RWMutex
	pendingTxs map[chainhash.Hash]txdb.WalletTransaction

	// receiveTxLastQuery is the block height at which we last scanned for
	// incoming transactions; 0 means never.
	receiveTxLastQuery atomic.Uint64
	// scanFloor is a pending downward override for the next history scan's
	// floor; 0 means none. Both backends normally deliver relevant
	// transactions while they are still unmined, but delivery is not
	// guaranteed to outrun mining: a reconnect, a lagging or restarted
	// server, or a subscription racing a block (routine on simnet, where the
	// harness electrumx pushes lazily and blocks mine within seconds) makes
	// the transaction arrive as an already-mined import, which the
	// coalescing NotificationServer reports under a block below
	// receiveTxLastQuery. The register holds the lowest such height until a
	// scan actually consumes it, so a trigger skipped by the
	// syncingTxHistory guard cannot lose the regression.
	scanFloor        atomic.Uint64
	syncingTxHistory atomic.Bool
	// updatingPending guards updatePendingTransactions, which is reachable both
	// from syncTxHistory and from pendingTxLoop; without it the two could
	// process the same pending tx concurrently and emit duplicate notes.
	updatingPending atomic.Bool
}

var _ asset.Wallet = (*NativeWallet)(nil)
var _ asset.Authenticator = (*NativeWallet)(nil)
var _ asset.PeerManager = (*NativeWallet)(nil)

// Peers returns the wallet's network peers when the backend manages peers
// (neutrino/SPV); the electrum backend has no peer concept. Satisfies
// asset.PeerManager.
func (w *NativeWallet) Peers() ([]*asset.WalletPeer, error) {
	if w.btcw == nil {
		return nil, nil
	}
	walletPeers, err := w.btcw.Peers()
	if err != nil {
		return nil, err
	}
	return walletPeersToAsset(walletPeers), nil
}

// AddPeer adds a persistent peer, when the backend manages peers.
func (w *NativeWallet) AddPeer(addr string) error {
	if w.btcw == nil {
		return errors.New("native: wallet not connected")
	}
	return w.btcw.AddPeer(addr)
}

// RemovePeer removes a peer added with AddPeer, when the backend manages peers.
func (w *NativeWallet) RemovePeer(addr string) error {
	if w.btcw == nil {
		return errors.New("native: wallet not connected")
	}
	return w.btcw.RemovePeer(addr)
}

// walletPeersToAsset maps []wallet.Peer to []*asset.WalletPeer at the
// asset-package boundary.
func walletPeersToAsset(peers []wallet.Peer) []*asset.WalletPeer {
	if len(peers) == 0 {
		return nil
	}
	out := make([]*asset.WalletPeer, len(peers))
	for i, p := range peers {
		out[i] = &asset.WalletPeer{
			Addr:      p.Addr,
			Connected: p.Connected,
			Source:    walletPeerSourceToAsset(p.Source),
		}
	}
	return out
}

// walletPeerSourceToAsset maps wallet.PeerSource → asset.PeerSource.
func walletPeerSourceToAsset(s wallet.PeerSource) asset.PeerSource {
	switch s {
	case wallet.PeerDefault:
		return asset.WalletDefault
	case wallet.PeerUserAdded:
		return asset.UserAdded
	default:
		return asset.Discovered
	}
}

var _ asset.Rescanner = (*NativeWallet)(nil)

// Rescan drops the wallet's transaction history and re-scans the chain from the
// seed birthday, rediscovering all funds. It satisfies asset.Rescanner; the
// caller should poll SyncStatus to follow progress. The bday argument is ignored
// — the wallet rescans from the birthday stored with its seed.
//
// The mechanics mirror the previous SPV wallet's RescanAsync: stop the wallet
// (which stops the chain client — for neutrino only the client, leaving the
// synced chain service running; for electrum the network), drop the transaction
// history and reset the synced-to height, then restart and re-synchronize so the
// next sync recovers from the birthday. rescanMtx serializes this against itself.
func (w *NativeWallet) Rescan(_ context.Context, _ uint64) error {
	w.rescanMtx.Lock()
	defer w.rescanMtx.Unlock()

	if w.btcw == nil {
		return errors.New("native: cannot rescan, wallet not connected")
	}
	if !w.btcw.CanRescan() {
		// The electrum backend can't restart its network in place, and electrum
		// rediscovers funds by re-querying address history on every sync anyway,
		// so an explicit rescan isn't offered.
		return errors.New("this wallet's backend does not support rescanning")
	}

	w.log.Info("Stopping wallet for full rescan...")
	w.btcw.Stop() // also stops the chain client (neutrino: client only)
	w.btcw.WaitForShutdown()

	if err := w.forceRescan(); err != nil {
		// Press on and restart so the wallet isn't left stopped.
		w.log.Errorf("rescan: %v", err)
	}

	w.log.Info("Restarting wallet to perform rescan...")
	w.btcw.Start()
	if err := w.btcw.Synchronize(); err != nil {
		return fmt.Errorf("native: restarting chain backend for rescan: %w", err)
	}
	w.receiveTxLastQuery.Store(0)
	return nil
}

// forceRescan drops the wallet's transaction history and resets its synced-to
// height to nil, so the next synchronization recovers from the seed birthday.
// The wallet must be stopped.
func (w *NativeWallet) forceRescan() error {
	wdb := w.btcw.Database()
	if err := wallet.DropTransactionHistory(wdb, false); err != nil {
		return fmt.Errorf("dropping transaction history: %w", err)
	}
	return walletdb.Update(wdb, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(wAddrMgrBkt)
		return w.btcw.Manager.SetSyncedTo(ns, nil)
	})
}

// NewNativeWallet constructs a NativeWallet for the chain and backend described
// by cc, without opening or starting anything. Shared by the btc and ltc asset
// packages for both the electrum and SPV wallet types.
func NewNativeWallet(cc ChainConfig, cfg *asset.WalletConfig, log util.Logger) *NativeWallet {
	recoveryWindow := cc.RecoveryWindow
	if s := cfg.Settings["recoverywindow"]; s != "" {
		if n, err := strconv.ParseUint(s, 10, 32); err == nil {
			recoveryWindow = uint32(n)
		} else {
			log.Errorf("native: ignoring invalid recoverywindow setting %q: %v", s, err)
		}
	}
	var feeFetcher *feeratefetcher.Fetcher
	if len(cc.FeeRateSources) > 0 {
		feeFetcher = feeratefetcher.New(cc.FeeRateSources, log.SubLogger("FEE"))
	}
	var watchDepths [2]int32
	for i, key := range []string{"externalquietdepth", "sweepinterval"} {
		if s := cfg.Settings[key]; s != "" {
			if n, err := strconv.ParseInt(s, 10, 32); err == nil {
				watchDepths[i] = int32(n)
			} else {
				log.Errorf("native: ignoring invalid %s setting %q: %v", key, s, err)
			}
		}
	}
	return &NativeWallet{
		cfg:            cc,
		log:            log,
		dir:            filepath.Join(cfg.DataDir, cc.ChainParams.Name),
		chainParams:    cc.ChainParams,
		netParams:      cc.NetParams,
		emit:           cfg.Emit,
		peersChange:    cfg.PeersChange,
		networkID:      cc.Info.Network.ID,
		assetID:        cc.Info.Network.NativeAssetID,
		feeFetcher:     feeFetcher,
		recoveryWindow: recoveryWindow,
		watchDepths:    watchDepths,
		txdb:           cfg.NetworkTxDB,
		pendingTxs:     make(map[chainhash.Hash]txdb.WalletTransaction),
	}
}

// openElectrumWallet constructs a BTC NativeWallet. Called by NewWallet for
// walletTypeElectrum.
func openElectrumWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *NativeWallet {
	return NewNativeWallet(btcElectrumChainConfig(chainParams), cfg, log)
}

// openSPVWallet constructs a NativeWallet driven by the neutrino
// (BIP-157/158) backend. Called by NewWallet for walletTypeSPV.
func openSPVWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *NativeWallet {
	return NewNativeWallet(btcSPVChainConfig(chainParams), cfg, log)
}

// Info returns the WalletInfo for the wallet's chain.
func (w *NativeWallet) Info() *asset.WalletInfo {
	return w.cfg.Info
}

// Unlock unlocks the underlying wallet for spending. A nil lock channel keeps
// it unlocked until Lock is called explicitly. Satisfies asset.Authenticator.
func (w *NativeWallet) Unlock(pw []byte) error {
	if w.btcw == nil {
		return errors.New("native: wallet not connected")
	}
	return w.btcw.Unlock(pw, nil)
}

// Lock locks the underlying wallet. Satisfies asset.Authenticator.
func (w *NativeWallet) Lock() error {
	if w.btcw == nil {
		return errors.New("native: wallet not connected")
	}
	w.btcw.Lock()
	return nil
}

// Locked reports whether the underlying wallet is locked. Satisfies
// asset.Authenticator.
func (w *NativeWallet) Locked() bool {
	return w.btcw == nil || w.btcw.Locked()
}

// Connect opens the btcwallet database, wires the chain backend (which is built
// and started inside wallet.Wallet via Synchronize), and launches background
// goroutines for tx-history sync and peer monitoring. Satisfies util.Connector.
func (w *NativeWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	w.ctx.Store(ctx)

	// recoveryWindow drives on-open address recovery, and the two backends use
	// it differently. Recovery only runs when the window is > 0 (syncWithChain).
	// For neutrino it gates the block-walk that scans compact filters for the
	// wallet's addresses (default neutrinoRecoveryWindow). For electrum it
	// drives the AddressProber path (per-address GetHistory probing, not an
	// O(chain length) block walk; default ElectrumRecoveryWindow): a fresh-seed
	// restore's empty pool gets a branch-wide discovery scan, while an ordinary
	// reconnect probes only the window beyond each branch's pool top. The
	// "recoverywindow" setting overrides either default ("0" disables).
	loaderOpts := []wallet.LoaderOption{wallet.WithBackendKind(w.cfg.BackendKind)}
	// Undocumented test knobs: shrink the watch policy's depths so harness
	// tests can exercise the cold-address lifecycle in a few blocks.
	if w.watchDepths[0] > 0 || w.watchDepths[1] > 0 {
		loaderOpts = append(loaderOpts,
			wallet.WithWatchDepths(w.watchDepths[0], w.watchDepths[1]))
	}
	w.loader = wallet.NewLoader(
		w.netParams, w.dir, true, dbTimeout, w.recoveryWindow,
		loaderOpts...,
	)
	exists, err := w.loader.WalletExists()
	if err != nil {
		return nil, fmt.Errorf("native: error checking wallet existence: %w", err)
	}
	if !exists {
		return nil, errors.New("native: wallet not found")
	}

	btcw, err := w.loader.OpenExistingWallet([]byte(wallet.InsecurePubPassphrase))
	if err != nil {
		return nil, fmt.Errorf("native: couldn't open wallet: %w", err)
	}
	w.btcw = btcw

	errCloser := util.NewErrorCloser()
	defer errCloser.Done(w.log)
	errCloser.Add(w.loader.UnloadWallet)

	// Synchronize builds the chain backend (if not built), starts it, and wires
	// it as the wallet's chain client. The wallet owns the backend lifecycle.
	w.log.Info("Synchronizing wallet with the chain backend...")
	if err = w.btcw.Synchronize(); err != nil {
		return nil, fmt.Errorf("native: synchronize: %w", err)
	}
	errCloser.Add(func() error {
		w.btcw.Shutdown()
		return nil
	})

	// Restore unconfirmed transactions from previous sessions.
	if w.txdb != nil {
		pendingTxs, err := w.txdb.GetPendingTxs()
		if err != nil {
			return nil, fmt.Errorf("native: loading pending txs: %w", err)
		}
		w.pendingMtx.Lock()
		for _, tx := range pendingTxs {
			h, err := chainhash.NewHashFromStr(tx.ID)
			if err != nil {
				w.log.Errorf("Invalid txid %q in pending txs: %v", tx.ID, err)
				continue
			}
			w.pendingTxs[*h] = *tx
		}
		w.pendingMtx.Unlock()
	}

	errCloser.Success()

	var wg sync.WaitGroup

	// Tie wallet and backend teardown to the Connect context. Unload the wallet
	// first (it stops btcwallet and closes its db), then fully shut down the
	// backend — mirroring the previous SPV wallet's Stop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		if err := w.loader.UnloadWallet(); err != nil {
			w.log.Errorf("native: error unloading wallet on shutdown: %v", err)
		}
		w.btcw.Shutdown()
		w.btcw.WaitForShutdown()
	}()

	// Block-notification goroutine — watches the wallet's NtfnServer and
	// triggers tx-history syncs on each new block.
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.blockLoop(ctx)
	}()

	// Peer monitoring goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.peerLoop(ctx)
	}()

	// Pending-tx reconciliation goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.pendingTxLoop(ctx)
	}()

	// Initial tx-history sync.
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Wait briefly so the chain client has a chance to connect before we
		// check ChainSynced.
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		_, tip := w.bestBlock()
		w.syncTxHistory(ctx, uint64(tip))
	}()

	return &wg, nil
}

// bestBlock returns the hash and height of the best synced block according to
// the chain client, or zero/genesis values if not yet synced.
func (w *NativeWallet) bestBlock() (*chainhash.Hash, int32) {
	if w.btcw == nil {
		return &chainhash.Hash{}, 0
	}
	cc := w.btcw.ChainClient()
	if cc == nil {
		return &chainhash.Hash{}, 0
	}
	h, height, err := cc.GetBestBlock()
	if err != nil {
		return &chainhash.Hash{}, 0
	}
	return h, height
}

// Balance returns the wallet's available, immature, and locked balances.
func (w *NativeWallet) Balance() (*asset.Balance, error) {
	if w.btcw == nil {
		return nil, errors.New("native: wallet not connected")
	}
	// minconf 0: an unconfirmed (0-conf) output is spendable immediately — a
	// standard mempool tx is usable right away — so it counts toward Available.
	// Send() uses the same minconf for coin selection so what we report as
	// available is exactly what we can actually spend.
	bals, err := w.btcw.CalculateAccountBalances(0, 0)
	if err != nil {
		return nil, fmt.Errorf("native: balance error: %w", err)
	}
	return &asset.Balance{
		Available: uint64(bals.Spendable),
		Immature:  uint64(bals.ImmatureReward),
		Locked:    0,
		Other:     make(map[asset.BalanceCategory]asset.CustomBalance),
	}, nil
}

// DepositAddress returns the next unused external address.
func (w *NativeWallet) DepositAddress() (string, error) {
	if w.btcw == nil {
		return "", errors.New("native: wallet not connected")
	}
	scope := w.btcw.PrimaryScope()
	addr, err := w.btcw.NewAddress(0, scope)
	if err != nil {
		return "", fmt.Errorf("native: new address: %w", err)
	}
	return addr.EncodeAddress(), nil
}

// OwnsDepositAddress returns true if the address belongs to this wallet.
func (w *NativeWallet) OwnsDepositAddress(address string) (bool, error) {
	if w.btcw == nil {
		return false, errors.New("native: wallet not connected")
	}
	addr, err := btcutil.DecodeAddress(address, w.chainParams)
	if err != nil {
		return false, nil
	}
	return w.btcw.HaveAddress(addr)
}

// ValidateAddress reports whether address is a valid Bitcoin address for the
// active network.
func (w *NativeWallet) ValidateAddress(address string) bool {
	_, err := btcutil.DecodeAddress(address, w.chainParams)
	return err == nil
}

// Send sends exactly value satoshis to address, paying feeRate sat/byte.
// Coin selection is handled by wallet.SendOutputs internally.
func (w *NativeWallet) Send(address string, value, feeRate uint64) (asset.Coin, error) {
	if w.btcw == nil {
		return nil, errors.New("native: wallet not connected")
	}
	// The caller-approved rate is used exactly as given (no fallback), so a
	// zero rate is a hard error rather than a zero-fee transaction.
	if feeRate == 0 {
		return nil, errors.New("native: no fee rate specified")
	}

	addr, err := btcutil.DecodeAddress(address, w.chainParams)
	if err != nil {
		return nil, fmt.Errorf("native: invalid address %q: %w", address, err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, fmt.Errorf("native: pay-to-addr script: %w", err)
	}

	txOut := wire.NewTxOut(int64(value), pkScript)
	scope := w.btcw.PrimaryScope()
	satPerKB := btcutil.Amount(feeRate * 1000)

	msgTx, err := w.btcw.SendOutputs(
		[]*wire.TxOut{txOut},
		&scope,
		0,        // account
		0,        // minconf: spend unconfirmed (0-conf) outputs too, matching Balance
		satPerKB, // fee rate as sat/kB
		wallet.CoinSelectionLargest,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("native: SendOutputs: %w", err)
	}

	txHash := msgTx.TxHash()
	addrCopy := address

	// SendOutputs builds the payment and change outputs in no guaranteed order,
	// so locate the payment output by its script and value rather than assuming
	// index 0 — otherwise the returned coin can point at the change output.
	vout := uint32(0)
	for i, out := range msgTx.TxOut {
		if out.Value == int64(value) && bytes.Equal(out.PkScript, pkScript) {
			vout = uint32(i)
			break
		}
	}

	// Record in tx history. Fee will be updated by the next syncTxHistory pass.
	wt := &txdb.WalletTransaction{
		NetworkID: w.networkID,
		AssetID:   w.assetID,
		Type:      txdb.Send,
		ID:        txHash.String(),
		Amount:    value,
		Fees:      0,
		Recipient: &addrCopy,
		Timestamp: uint64(time.Now().Unix()),
		Confirms:  &txdb.Confirms{Target: confTxFinality},
	}
	w.storePendingTx(&txHash, wt)

	return &electrumCoin{
		txHash: txHash,
		vout:   vout,
		value:  value,
	}, nil
}

// StandardSendFee returns the fee for a standard send (1-in 2-out), sized for
// the wallet's address type: legacy P2PKH for non-segwit chains (Dash), native
// segwit P2WPKH otherwise (BTC/LTC).
func (w *NativeWallet) StandardSendFee(feeRate uint64) uint64 {
	var sz uint64
	if w.netParams.KeyPurposes[0] == 44 {
		// legacy: 1 P2PKH input + 2 P2PKH outputs
		sz = uint64(MinimumTxOverhead) + uint64(RedeemP2PKHInputSize) + P2PKHOutputSize*2
	} else {
		// segwit: 1 P2WPKH input + 2 P2WPKH outputs
		inputSize := uint64(RedeemP2WPKHInputSize) + uint64((RedeemP2PKSigScriptSize+2+3)/4)
		sz = uint64(MinimumTxOverhead) + inputSize + P2WPKHOutputSize*2
	}
	return feeRate * sz
}

// FeeRate satisfies asset.Wallet, returning a fee-rate estimate in sats/vByte.
// The lean native wallet has no local estimatesmartfee RPC, so it composites the
// configured external sources first, then falls back to an electrum server
// estimate via the underlying wallet. It returns zero only when neither can
// provide a rate, in which case fee-dependent operations like sends error
// rather than proceed at a made-up rate.
func (w *NativeWallet) FeeRate() uint64 {
	ctx, _ := w.ctx.Load().(context.Context)
	if ctx == nil {
		return 0 // not connected
	}
	if w.feeFetcher != nil {
		if r := w.feeFetcher.Rate(ctx); r != 0 {
			return r
		}
	}
	// Fall back to an electrum fee estimate from the wallet's configured servers,
	// available regardless of the chain backend (including neutrino). It is an
	// estimatesmartfee-derived value of variable quality, so it is used only when
	// the external sources above yield nothing.
	if w.btcw != nil {
		if r, err := w.btcw.FeeRate(ctx); err == nil {
			return r
		}
	}
	return 0
}

var _ asset.TxFeeEstimator = (*NativeWallet)(nil)

// EstimateSendTxFee estimates the network fee, in atoms, for sending value to
// address at feeRate (atoms/byte). It satisfies asset.TxFeeEstimator. The
// estimate runs the same coin-selection and tx-construction path Send uses, but
// without signing or broadcasting, so it matches what a subsequent Send at this
// rate would pay. The caller supplies the rate (core sources it from FeeRate),
// and the estimate prices at exactly that rate — a zero rate is an error, never
// a cue to substitute a different rate than the one the caller will report and
// validate against. With subtract or maxWithdraw the fee comes out of value
// rather than on top of it; maxWithdraw additionally means value is the whole
// balance, so the estimate is for the changeless transaction that spends every
// eligible output.
func (w *NativeWallet) EstimateSendTxFee(address string, value, feeRate uint64, subtract, maxWithdraw bool) (fee uint64, isValidAddress bool, err error) {
	if w.btcw == nil {
		return 0, false, errors.New("native: wallet not connected")
	}
	if value == 0 {
		return 0, false, errors.New("native: cannot estimate fee for zero value")
	}
	if feeRate == 0 {
		return 0, false, errors.New("native: no fee rate specified")
	}

	// A dummy P2WPKH script keeps the size estimate realistic while the user is
	// still typing an incomplete address.
	pkScript := append([]byte{txscript.OP_0, txscript.OP_DATA_20}, make([]byte, 20)...)
	if addr, decErr := btcutil.DecodeAddress(address, w.chainParams); decErr == nil {
		if s, scriptErr := txscript.PayToAddrScript(addr); scriptErr == nil {
			pkScript = s
			isValidAddress = true
		}
	}

	txOut := wire.NewTxOut(int64(value), pkScript)
	scope := w.btcw.PrimaryScope()
	satPerKB := btcutil.Amount(feeRate * 1000)

	sendAllFee := func() (uint64, error) {
		f, err := w.btcw.EstimateSendAllFee(&scope, 0, 0, satPerKB, []*wire.TxOut{txOut})
		if err != nil {
			return 0, fmt.Errorf("native: fee estimation: %w", err)
		}
		if uint64(f) >= value {
			return 0, fmt.Errorf("native: fee %d consumes the send value %d", f, value)
		}
		return uint64(f), nil
	}

	// A max withdraw sends the whole balance minus the fee: every eligible
	// output is consumed and there is no change.
	if maxWithdraw {
		fee, err = sendAllFee()
		return fee, isValidAddress, err
	}

	// CreateSimpleTx's final argument builds the transaction without signing it
	// and rolls back the wallet db, so this is safe to call on a locked wallet
	// (e.g. while the send form is being filled in).
	authoredTx, err := w.btcw.CreateSimpleTx(&scope, 0, []*wire.TxOut{txOut}, 0, satPerKB, wallet.CoinSelectionLargest, true)
	if err != nil {
		// A subtract send takes the fee out of value, so it needs inputs
		// covering only value, not value + fee. When value is at or near the
		// whole balance, the regular funding attempt above comes up one fee
		// short — that send is the changeless spend-all shape, so estimate it
		// as such. For any other subtract value the regular estimate below is
		// the right one: the transaction differs only in the output being
		// reduced by the fee, which doesn't change its size.
		var insufficient txauthor.InputSourceError
		if subtract && errors.As(err, &insufficient) {
			fee, err = sendAllFee()
			return fee, isValidAddress, err
		}
		return 0, isValidAddress, fmt.Errorf("native: fee estimation: %w", err)
	}

	var out uint64
	for _, o := range authoredTx.Tx.TxOut {
		out += uint64(o.Value)
	}
	return uint64(authoredTx.TotalInput) - out, isValidAddress, nil
}

// SyncStatus returns the current chain-sync state.
func (w *NativeWallet) SyncStatus() (*asset.SyncStatus, error) {
	if w.btcw == nil {
		return &asset.SyncStatus{}, nil
	}

	// localTip is our synced header tip; targetTip is the height the backend is
	// syncing toward (the electrum server's reported best, the neutrino sync
	// target). targetTip is always >= localTip, so progress climbs from near-0%
	// to 100% as headers download rather than pinning at ~100%.
	localTip, targetTip, backendSynced := w.btcw.SyncHeight()
	if localTip < 0 {
		return &asset.SyncStatus{}, nil
	}

	targetHeight := targetTip
	if targetHeight <= 0 {
		targetHeight = localTip
	}

	walletBlock := w.btcw.Manager.SyncedTo()
	synced := backendSynced && walletBlock.Height > 0 &&
		walletBlock.Height >= localTip

	return &asset.SyncStatus{
		Synced:       synced,
		TargetHeight: uint64(targetHeight),
		Blocks:       uint64(walletBlock.Height),
	}, nil
}

// TxHistory returns paginated transaction history from the local txdb.
func (w *NativeWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if w.txdb == nil {
		return nil, errors.New("native: tx database not initialized")
	}
	return w.txdb.GetTxs(req)
}

// WalletTransaction looks up a single transaction. If it is confirmed it is
// served from the local txdb; otherwise the chain client is queried.
func (w *NativeWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	// Accept either a plain txid or a hex-encoded coin ID.
	coinID, err := hex.DecodeString(txID)
	if err == nil {
		txHash, _, err2 := decodeCoinID(coinID)
		if err2 == nil {
			txID = txHash.String()
		}
	}

	if w.txdb != nil {
		tx, err := w.txdb.GetTx(txID)
		if err == nil && tx != nil && tx.Confirmed {
			return tx, nil
		}
	}
	if w.btcw == nil {
		return nil, errors.New("native: wallet not connected")
	}

	// Fall through to wallet database lookup.
	txHash, err := chainhash.NewHashFromStr(txID)
	if err != nil {
		return nil, fmt.Errorf("native: invalid txid %q: %w", txID, err)
	}
	details, err := wallet.UnstableAPI(w.btcw).TxDetails(txHash)
	if err != nil {
		return nil, fmt.Errorf("native: TxDetails: %w", err)
	}
	if details == nil {
		return nil, asset.CoinNotFoundError
	}

	wt := &txdb.WalletTransaction{
		NetworkID:   w.networkID,
		AssetID:     w.assetID,
		Type:        txdb.Unknown,
		ID:          txID,
		BlockNumber: blockNumberFromHeight(details.Block.Height),
		Confirmed:   details.Block.Height > 0,
	}
	return wt, nil
}

// PendingTransactions returns unconfirmed transactions tracked by this wallet.
func (w *NativeWallet) PendingTransactions(_ context.Context) []*txdb.WalletTransaction {
	w.pendingMtx.RLock()
	defer w.pendingMtx.RUnlock()
	txs := make([]*txdb.WalletTransaction, 0, len(w.pendingTxs))
	for _, tx := range w.pendingTxs {
		if tx.Confirmed {
			continue
		}
		cp := tx
		txs = append(txs, &cp)
	}
	return txs
}

// blockNumberFromHeight converts a wtxmgr block height to the txdb BlockNumber
// representation. wtxmgr uses -1 as the "unmined" sentinel, so a naive
// uint64(details.Block.Height) becomes ~1.8e19 for any unmined tx; persisting
// that as the tx's block height corrupts history sorting/grouping. Any
// non-positive height (unmined, or the genesis sentinel) maps to 0, matching the
// "Height > 0" convention used to decide Confirmed elsewhere.
func blockNumberFromHeight(h int32) uint64 {
	if h <= 0 {
		return 0
	}
	return uint64(h)
}

// btcToSats converts a BTC-denominated float (as returned by btcjson results)
// to satoshis, rounding to the nearest satoshi. A raw uint64(v*1e8) truncates
// (e.g. 0.29 → 28999999 instead of 29000000) because the product is not exactly
// representable; btcutil.NewAmount rounds. Negative/NaN inputs map to 0.
func btcToSats(v float64) uint64 {
	amt, err := btcutil.NewAmount(v)
	if err != nil || amt < 0 {
		return 0
	}
	return uint64(amt)
}

// storePendingTx records a sent transaction in the txdb and the in-memory
// pending map.
func (w *NativeWallet) storePendingTx(txHash *chainhash.Hash, wt *txdb.WalletTransaction) {
	if w.txdb == nil {
		return
	}
	w.pendingMtx.Lock()
	w.pendingTxs[*txHash] = *wt
	w.pendingMtx.Unlock()
	if err := w.txdb.StoreTx(wt); err != nil {
		w.log.Errorf("native: storePendingTx %s: %v", txHash, err)
	}
	w.emit.TransactionNote(wt, true)
}

// blockLoop watches the wallet's NtfnServer for transaction and block events,
// relays them to core, and triggers tx-history syncs.
//
// btcwallet's wtxmgr learns about received funds (via the chain backend's
// address watching — electrum scripthash subscriptions or neutrino filter scans)
// and confirmations (via block connects) on its own, but nothing reaches core
// unless we emit it here. TipChange drives core's per-block balance
// re-poll and updates the displayed sync height; BalanceChange surfaces balance
// movement that isn't tied to a new block (e.g. an outgoing send that spends
// inputs while still unconfirmed). Without these, a balance change the wallet
// already sees never propagates to the UI, even on a manual refresh — core only
// re-reads its cached balance, which these notifications are what update.
func (w *NativeWallet) blockLoop(ctx context.Context) {
	cl := w.btcw.NtfnServer.TransactionNotifications()
	defer cl.Done()
	for {
		select {
		case note := <-cl.C:
			// CRITICAL: NtfnServer delivers on an UNBUFFERED channel and
			// broadcasts with a blocking send while holding its lock and a live
			// db transaction (see notifyUnminedTransaction / notifyAttachedBlock
			// in utxowallet/wallet/notifications.go). This loop must receive and
			// move on without blocking. Blocking here back-pressures btcwallet's
			// notification dispatch, which stops it draining the chain client,
			// which stalls delivery of new transactions and blocks — i.e.
			// delayed or missing tx detection. So all real work (balance reads,
			// tx-history scans) runs in goroutines; emit calls are themselves
			// non-blocking (they drop on a full channel).
			switch {
			case len(note.AttachedBlocks) > 0:
				last := note.AttachedBlocks[len(note.AttachedBlocks)-1]
				// The batch can carry transactions in blocks well below the
				// scan cursor (see scanFloor); arm the floor with the lowest
				// tx-bearing height so the next scan reaches them.
				for i := range note.AttachedBlocks {
					blk := &note.AttachedBlocks[i]
					if len(blk.Transactions) > 0 && blk.Height > 0 {
						w.armScanFloor(uint64(blk.Height))
					}
				}
				// New block(s): advance the tip so core re-polls the balance
				// and updates the sync height, then refresh tx history.
				w.emit.TipChange(uint64(last.Height))
				go w.syncTxHistory(ctx, uint64(last.Height))
			case len(note.UnminedTransactions) > 0:
				// A new unconfirmed tx: an incoming receive still in the
				// mempool, or an outgoing send. Surface it and push the balance.
				go w.handleUnminedTxChange(ctx)
			}
		case <-ctx.Done():
			return
		}
	}
}

// handleUnminedTxChange records newly-seen unconfirmed transactions and pushes
// an updated balance. It records and emits the unconfirmed tx so it surfaces in
// the UI immediately instead of waiting for a block: scanNewTransactions'
// ListSinceBlock includes mempool txs (end height -1) and recordBtcjsonTx skips
// any already recorded, so an outgoing send that storePendingTx already emitted
// is not duplicated while an incoming receive (which has no other emit path
// until mined) now surfaces. The balance push lets an outgoing spend reflect
// before it confirms.
//
// This MUST run off blockLoop (see the comment there): it does blocking db work
// and the NtfnServer channel that drives blockLoop is unbuffered.
func (w *NativeWallet) handleUnminedTxChange(ctx context.Context) {
	w.syncTxHistory(ctx, uint64(w.btcw.Manager.SyncedTo().Height))
	if bal, err := w.Balance(); err != nil {
		w.log.Errorf("native: balance after unmined tx change: %v", err)
	} else {
		w.emit.BalanceChange(bal)
	}
}

// pendingTxLoop periodically reconciles unconfirmed transactions while any are
// tracked, flipping them to confirmed (and emitting the updated TransactionNote)
// as they reach finality. btcwallet learns of a confirmation from the chain
// backend but coalesces the resulting notification and only flushes it on a
// subsequent block connect, which on mainnet can be ~10 minutes away; polling
// here surfaces the confirmed status within the interval instead. Spendable
// balance does not depend on this — 0-conf outputs already count as available
// (see Balance) — so this is purely about timely confirmation status. The loop
// is idle when there are no pending txs.
// pendingTxLoopInterval is a var, not a const, so tests can shorten it and
// observe a tick without a multi-second real-time wait.
var pendingTxLoopInterval = 5 * time.Second

func (w *NativeWallet) pendingTxLoop(ctx context.Context) {
	ticker := time.NewTicker(pendingTxLoopInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pendingMtx.RLock()
			n := len(w.pendingTxs)
			w.pendingMtx.RUnlock()
			if n == 0 {
				continue
			}
			w.updatePendingTransactions(ctx, uint64(w.btcw.Manager.SyncedTo().Height))
		}
	}
}

// peerLoop periodically checks network connectivity and calls peersChange.
func (w *NativeWallet) peerLoop(ctx context.Context) {
	const interval = 5 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastCount uint32
	check := func() {
		if w.btcw == nil {
			return
		}
		var count uint32
		if w.btcw.Connected() {
			count = 1
		}
		if count != lastCount {
			lastCount = count
			w.peersChange(count, nil)
		}
	}

	for {
		check()
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// syncTxHistory scans for new wallet transactions since the last query and
// updates pending transactions.
func (w *NativeWallet) syncTxHistory(ctx context.Context, tip uint64) {
	if !w.syncingTxHistory.CompareAndSwap(false, true) {
		return
	}
	defer w.syncingTxHistory.Store(false)

	if w.txdb == nil || w.btcw == nil {
		return
	}

	ss, err := w.SyncStatus()
	if err != nil || ss == nil || !ss.Synced {
		return
	}

	w.scanNewTransactions(ctx, tip)
	w.updatePendingTransactions(ctx, tip)
}

// armScanFloor lowers the pending scan-floor override to h if it is below the
// currently armed value (0 = none armed). See the scanFloor field docs.
func (w *NativeWallet) armScanFloor(h uint64) {
	for {
		cur := w.scanFloor.Load()
		if cur != 0 && cur <= h {
			return
		}
		if w.scanFloor.CompareAndSwap(cur, h) {
			return
		}
	}
}

// peekScanFloor returns the height a scan that would otherwise start from
// lastQuery should start from, along with the armed override it observed (0 for
// none). An override never raises the floor, and a never-scanned wallet
// (lastQuery 0) already scans from the beginning.
//
// The override is read, not consumed: a scan that does not complete must leave
// it armed for the next one. See clearScanFloor.
func (w *NativeWallet) peekScanFloor(lastQuery uint64) (from, floor uint64) {
	floor = w.scanFloor.Load()
	if floor != 0 && lastQuery != 0 && floor < lastQuery {
		return floor, floor
	}
	return lastQuery, floor
}

// clearScanFloor retires the override that peekScanFloor observed, once the scan
// it was observed for has actually covered those blocks. Only that exact value
// is retired: armScanFloor only ever lowers the register, so a value that
// changed under us is a lower floor armed while the scan was running, naming
// blocks this scan started above and did not reach. Swapping it away would drop
// the late-notified mined transaction it was armed for from tx history for the
// life of the process, which is the whole thing the register exists to prevent.
func (w *NativeWallet) clearScanFloor(floor uint64) {
	if floor == 0 {
		return
	}
	w.scanFloor.CompareAndSwap(floor, 0)
}

// scanNewTransactions queries the wallet for transactions since the last scan
// and records any unknown ones in the txdb.
func (w *NativeWallet) scanNewTransactions(ctx context.Context, tip uint64) {
	const blockQueryBuffer = 3

	lastQuery, floor := w.peekScanFloor(w.receiveTxLastQuery.Load())
	var fromHeight int32
	if lastQuery > blockQueryBuffer {
		fromHeight = int32(lastQuery - blockQueryBuffer)
	}

	syncHeight := w.btcw.Manager.SyncedTo().Height
	txs, err := w.btcw.ListSinceBlock(fromHeight, -1, syncHeight)
	if err != nil {
		w.log.Errorf("native: ListSinceBlock(%d): %v", fromHeight, err)
		return
	}

	isInitial := lastQuery == 0

	for i := range txs {
		if ctx.Err() != nil {
			return
		}
		w.recordBtcjsonTx(ctx, &txs[i], tip, isInitial)
	}

	// Retire the floor only here, next to the cursor advance, because the two
	// say the same thing: the blocks up to tip have been scanned. Every return
	// above leaves the cursor where it was, and a floor consumed on one of those
	// paths would be a lowered start height that no scan ever used - the blocks
	// below the cursor would never be reached again.
	w.clearScanFloor(floor)
	w.receiveTxLastQuery.Store(tip)

	if isInitial {
		w.emit.TransactionHistorySyncedNote()
	}
}

// recordBtcjsonTx processes one btcjson.ListTransactionsResult and stores it in
// the txdb if not already present.
func (w *NativeWallet) recordBtcjsonTx(_ context.Context, tx *btcjson.ListTransactionsResult, tip uint64, isInitial bool) {
	txHash, err := chainhash.NewHashFromStr(tx.TxID)
	if err != nil {
		w.log.Errorf("native: invalid txid %q: %v", tx.TxID, err)
		return
	}

	// Skip if already recorded.
	if existing, err := w.txdb.GetTx(txHash.String()); err == nil && existing != nil {
		return
	} else if err != nil && !errors.Is(err, asset.CoinNotFoundError) {
		w.log.Errorf("native: GetTx %s: %v", txHash, err)
		return
	}

	txType := txdb.Receive
	if tx.Category == "send" {
		txType = txdb.Send
	}

	amount := tx.Amount
	if amount < 0 {
		amount = -amount
	}
	amtSats := btcToSats(amount)

	var feeSats uint64
	if tx.Fee != nil && *tx.Fee < 0 {
		feeSats = btcToSats(-*tx.Fee)
	}

	var blockNumber uint64
	var ts uint64
	if tx.BlockHeight != nil && *tx.BlockHeight > 0 {
		blockNumber = uint64(*tx.BlockHeight)
		ts = uint64(tx.BlockTime)
	} else {
		ts = uint64(time.Now().Unix())
	}

	const blockQueryBuffer = 3
	// Within blockQueryBuffer of the tip: treat as not-yet-final (it will be
	// confirmed on a later pass). The tip < blockQueryBuffer guard avoids a
	// uint64 underflow of tip-blockQueryBuffer on a very short chain
	// (regtest/simnet or early sync), where every positive height is within the
	// buffer anyway.
	if blockNumber > 0 && (tip < blockQueryBuffer || blockNumber >= tip-blockQueryBuffer) {
		blockNumber = 0
	}

	addrCopy := tx.Address
	wt := &txdb.WalletTransaction{
		NetworkID:   w.networkID,
		AssetID:     w.assetID,
		Type:        txType,
		ID:          txHash.String(),
		Amount:      amtSats,
		Fees:        feeSats,
		BlockNumber: blockNumber,
		Timestamp:   ts,
		Confirmed:   blockNumber > 0,
		Recipient:   &addrCopy,
	}
	if blockNumber == 0 {
		wt.Confirms = &txdb.Confirms{Target: confTxFinality}
	}

	if err = w.txdb.StoreTx(wt); err != nil {
		w.log.Errorf("native: StoreTx %s: %v", txHash, err)
		return
	}
	if wt.BlockNumber == 0 {
		w.pendingMtx.Lock()
		w.pendingTxs[*txHash] = *wt
		w.pendingMtx.Unlock()
	}
	if !isInitial {
		w.emit.TransactionNote(wt, false)
	}
}

// updatePendingTransactions refreshes confirmation status for all tracked
// pending transactions. It is reachable from both syncTxHistory and
// pendingTxLoop, so a CAS guard keeps concurrent invocations from processing the
// same tx twice (which would emit duplicate notes and double-write the txdb); a
// skipped run loses nothing — the next trigger reconciles.
func (w *NativeWallet) updatePendingTransactions(ctx context.Context, tip uint64) {
	if !w.updatingPending.CompareAndSwap(false, true) {
		return
	}
	defer w.updatingPending.Store(false)

	w.pendingMtx.RLock()
	snapshot := make(map[chainhash.Hash]txdb.WalletTransaction, len(w.pendingTxs))
	for k, v := range w.pendingTxs {
		snapshot[k] = v
	}
	w.pendingMtx.RUnlock()

	for txHash, tx := range snapshot {
		if ctx.Err() != nil {
			return
		}

		details, err := wallet.UnstableAPI(w.btcw).TxDetails(&txHash)
		if err != nil || details == nil {
			// Transaction not known to wallet yet; leave pending.
			continue
		}

		blockHeight := blockNumberFromHeight(details.Block.Height)
		var confs uint64
		if blockHeight > 0 && tip >= blockHeight {
			confs = tip - blockHeight + 1
		}

		var updated bool
		if tx.BlockNumber != blockHeight {
			tx.BlockNumber = blockHeight
			if details.Block.Time != (time.Time{}) {
				tx.Timestamp = uint64(details.Block.Time.Unix())
			}
			updated = true
		}

		if confs >= confTxFinality {
			if !tx.Confirmed {
				tx.Confirmed = true
				tx.Confirms = nil
				updated = true
			}
		} else {
			newConfs := &txdb.Confirms{Current: uint32(confs), Target: confTxFinality}
			if tx.Confirms == nil || tx.Confirms.Current != newConfs.Current {
				tx.Confirms = newConfs
				tx.Confirmed = false
				updated = true
			}
		}

		if updated {
			if err := w.txdb.StoreTx(&tx); err != nil {
				w.log.Errorf("native: updating pending tx %s: %v", txHash, err)
				continue
			}
			w.pendingMtx.Lock()
			if tx.Confirmed {
				delete(w.pendingTxs, txHash)
			} else {
				w.pendingTxs[txHash] = tx
			}
			w.pendingMtx.Unlock()
			w.emit.TransactionNote(&tx, false)
		}
	}
}

// electrumCoin implements asset.Coin for a sent UTXO.
type electrumCoin struct {
	txHash chainhash.Hash
	vout   uint32
	value  uint64
}

func (c *electrumCoin) ID() util.Bytes {
	return ToCoinID(&c.txHash, c.vout)
}

func (c *electrumCoin) String() string {
	return fmt.Sprintf("%s:%d", c.txHash, c.vout)
}

func (c *electrumCoin) Value() uint64 { return c.value }

func (c *electrumCoin) TxID() string { return c.txHash.String() }
