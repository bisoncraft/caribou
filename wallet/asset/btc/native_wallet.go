// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

// Package-level note: this file implements NativeWallet, a standalone
// asset.Wallet shared by the btc and ltc packages and used for both the electrum
// and SPV wallet types — they differ only in the chain backend it builds. It
// does NOT use ExchangeWalletSPV, baseWallet, intermediaryWallet, or tipRedeemer.
// All UTXO operations are delegated to wallet.Wallet (the in-tree utxowallet)
// driven by a chainBackend (chain.ElectrumClient or chain.NeutrinoClient). Swap /
// FindRedemption methods are intentionally absent.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/chain"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/electrum"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// chainBackend is the chain.Backend a wallet drives, plus the two
// backend-specific signals the wallet needs for status reporting: sync progress
// and connectivity. Both chain.ElectrumClient and chain.NeutrinoClient satisfy
// it, so the wallet stays backend-agnostic.
type chainBackend interface {
	chain.Backend
	SyncHeight() (localTip, targetTip int32, synced bool)
	Connected() bool
	// Shutdown fully tears the backend down. chain.Backend.Stop may stop only a
	// client while keeping a service running (neutrino does this so the wallet
	// can restart the client across a rescan without re-downloading the chain);
	// Shutdown always releases everything and is what the wallet calls on
	// disconnect.
	Shutdown()
	// CanRescan reports whether the wallet can perform an in-place rescan with
	// this backend — i.e. its client can be stopped and restarted (keeping any
	// synced service). Neutrino can; the electrum backend cannot restart its
	// network, and never offered a rescan anyway.
	CanRescan() bool
}

// peerManagedBackend is an optional capability of a chainBackend: backends with
// a peer concept (neutrino) implement it so the wallet can satisfy
// asset.PeerManager; the electrum backend does not.
type peerManagedBackend interface {
	Peers() ([]*asset.WalletPeer, error)
	AddPeer(addr string) error
	RemovePeer(addr string) error
}

// ChainConfig carries the per-chain values that parameterize the shared
// NativeWallet, so the btc and ltc asset packages can share one asset.Wallet
// implementation across both the electrum and SPV wallet types. (The
// implementation lives here in asset/btc for now; it will move to a neutral home
// later, together with the other shared wallet.Wallet stack. asset/ltc imports
// it.)
type ChainConfig struct {
	Chain       bisonwire.Chain
	Servers     map[string]*electrum.ServerAddr // per-net electrumx endpoints
	ChainParams *chaincfg.Params
	NetParams   *netparams.ChainParams
	Info        *asset.WalletInfo
	LogWallet   func(walletDir string) error // per-asset logger setup (logNeutrino)
	// NewBackend stands up the chain backend for this wallet — an electrum
	// network + client for electrum wallets, a neutrino chain service + client
	// for SPV wallets. It returns an unstarted backend; the wallet owns
	// Start/Stop/Shutdown/WaitForShutdown. Defaults to the electrum backend when
	// nil (see NewNativeWallet).
	NewBackend func(dir string, log util.Logger) (chainBackend, error)
	// RecoveryWindow is the default address-recovery window the wallet opens
	// with. Electrum leaves it 0 (it queries address history directly and only
	// recovers on an explicit restore); neutrino sets it >0 so btcwallet's
	// recovery drives the compact-filter scan that discovers funds. The
	// "recoverywindow" wallet setting, when present, overrides this.
	RecoveryWindow uint32
}

// electrumBackendFactory returns a NewBackend that stands up an electrumx
// network + ElectrumClient for cc's chain. It is the default backend.
func electrumBackendFactory(cc ChainConfig) func(dir string, log util.Logger) (chainBackend, error) {
	return func(dir string, log util.Logger) (chainBackend, error) {
		srvAddr := cc.Servers[cc.ChainParams.Name]
		if srvAddr == nil {
			return nil, fmt.Errorf("electrum: no built-in server for network %q", cc.ChainParams.Name)
		}
		if err := srvAddr.Validate(); err != nil {
			return nil, fmt.Errorf("electrum: invalid built-in server address: %w", err)
		}
		chainCfg, err := electrum.NewChainConfig(cc.Chain, cc.NetParams)
		if err != nil {
			return nil, fmt.Errorf("electrum: chain config: %w", err)
		}
		network, err := electrum.NewNetwork(&electrum.Config{
			Chain:         chainCfg,
			TrustedServer: srvAddr,
			DataDir:       dir,
		})
		if err != nil {
			return nil, fmt.Errorf("electrum: new network: %w", err)
		}
		return chain.NewElectrumClient(cc.NetParams, network)
	}
}

// btcElectrumChainConfig returns the ChainConfig for the BTC electrum wallet.
func btcElectrumChainConfig(chainParams *chaincfg.Params) ChainConfig {
	return ChainConfig{
		Chain:       bisonwire.ChainBTC,
		Servers:     btcElectrumServers,
		ChainParams: chainParams,
		NetParams:   btcNetParams(chainParams),
		Info:        WalletInfo,
		LogWallet:   logNeutrino,
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

// btcElectrumServers maps chaincfg.Params.Name to a built-in electrumx
// ServerAddr. The regtest/simnet entry points at the dextest harness address
// (BTC port 54002, ssl); mainnet and testnet use well-known public servers.
// Keys are the actual chaincfg.Params.Name values so they cannot drift out of
// sync with parseChainParams.
//
// The server selection is intentionally hard-coded — no user configuration.
var btcElectrumServers = map[string]*electrum.ServerAddr{
	// Mainnet uses Blockstream's server, which presents a publicly CA-issued
	// (Let's Encrypt) certificate, so it is verified against the system roots
	// rather than pinned: a pinned leaf would break on every ~90-day renewal.
	chaincfg.MainNetParams.Name:       {Proto: "ssl", Addr: "electrum.blockstream.info:700"},
	chaincfg.TestNet3Params.Name:      {Proto: "ssl", Addr: "electrum.blockstream.info:993", SkipVerify: true},
	chaincfg.RegressionNetParams.Name: {Proto: "ssl", Addr: "127.0.0.1:54002", SkipVerify: true},
}

// CreateElectrumWallet creates the on-disk btcwallet database for a NativeWallet
// of the chain described by cc — the same database serves the electrum and SPV
// wallet types. No chain backend is started here. Shared by the btc and ltc
// asset packages.
func CreateElectrumWallet(cc ChainConfig, privPass, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32) error {
	if err := cc.LogWallet(walletDir); err != nil {
		return fmt.Errorf("error initializing wallet logging: %w", err)
	}

	loader := wallet.NewLoader(cc.NetParams, walletDir, true, dbTimeout, 0) // recovery_window=0; see Connect
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
// wallets). It does NOT embed any ExchangeWallet type.
type NativeWallet struct {
	cfg         ChainConfig
	log         util.Logger
	dir         string
	chainParams *chaincfg.Params
	netParams   *netparams.ChainParams
	emit        *asset.WalletEmitter
	peersChange func(uint32, error)

	// recoveryWindow is the address-probe gap limit. 0 (the default) disables
	// recovery; a restore sets it >0 (via the "recoverywindow" setting) to scan
	// for funds at gap-limit addresses. See wallet.syncWithChain.
	recoveryWindow uint32

	// populated during Connect
	btcw        *wallet.Wallet
	loader      *wallet.Loader
	chainClient chainBackend
	// rescanMtx serializes Rescan, which stops and restarts the wallet and the
	// chain client underneath the running notification goroutines.
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
	syncingTxHistory   atomic.Bool
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
	if pm, ok := w.chainClient.(peerManagedBackend); ok {
		return pm.Peers()
	}
	return nil, nil
}

// AddPeer adds a persistent peer, when the backend manages peers.
func (w *NativeWallet) AddPeer(addr string) error {
	if pm, ok := w.chainClient.(peerManagedBackend); ok {
		return pm.AddPeer(addr)
	}
	return errors.New("this wallet's backend does not support peer management")
}

// RemovePeer removes a peer added with AddPeer, when the backend manages peers.
func (w *NativeWallet) RemovePeer(addr string) error {
	if pm, ok := w.chainClient.(peerManagedBackend); ok {
		return pm.RemovePeer(addr)
	}
	return errors.New("this wallet's backend does not support peer management")
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

	if w.btcw == nil || w.chainClient == nil {
		return errors.New("native: cannot rescan, wallet not connected")
	}
	if !w.chainClient.CanRescan() {
		// The electrum backend can't restart its network in place, and electrum
		// rediscovers funds by re-querying address history on every sync anyway,
		// so an explicit rescan isn't offered.
		return errors.New("this wallet's backend does not support rescanning")
	}

	w.log.Info("Stopping wallet for full rescan...")
	w.btcw.Stop() // also stops the chain client
	w.btcw.WaitForShutdown()
	w.chainClient.WaitForShutdown()

	if err := w.forceRescan(); err != nil {
		// Press on and restart so the wallet isn't left stopped.
		w.log.Errorf("rescan: %v", err)
	}

	w.log.Info("Restarting wallet to perform rescan...")
	w.btcw.Start()
	if err := w.chainClient.Start(); err != nil {
		return fmt.Errorf("native: restarting chain backend for rescan: %w", err)
	}
	w.btcw.Synchronize(w.chainClient)
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
	if cc.NewBackend == nil {
		cc.NewBackend = electrumBackendFactory(cc)
	}
	recoveryWindow := cc.RecoveryWindow
	if s := cfg.Settings["recoverywindow"]; s != "" {
		if n, err := strconv.ParseUint(s, 10, 32); err == nil {
			recoveryWindow = uint32(n)
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
		recoveryWindow: recoveryWindow,
		txdb:           cfg.NetworkTxDB,
		pendingTxs:     make(map[chainhash.Hash]txdb.WalletTransaction),
	}
}

// openElectrumWallet constructs a BTC NativeWallet. Called by NewWallet for
// walletTypeElectrum.
func openElectrumWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *NativeWallet {
	return NewNativeWallet(btcElectrumChainConfig(chainParams), cfg, log)
}

// openSPVLeanWallet constructs a NativeWallet driven by the neutrino
// (BIP-157/158) backend. Called by NewWallet for walletTypeSPV.
func openSPVLeanWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *NativeWallet {
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

// Connect opens the btcwallet database, stands up and starts the chain backend
// (electrum or neutrino), and launches background goroutines for tx-history sync
// and peer monitoring. Satisfies util.Connector.
func (w *NativeWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	if err := w.cfg.LogWallet(w.dir); err != nil {
		return nil, fmt.Errorf("native: error initializing logging: %w", err)
	}

	// recoveryWindow drives address recovery on open, and the two backends use it
	// differently. The electrum client probes each derived address's history
	// directly (the AddressProber path in syncWithChain); for electrum btcwallet's
	// block-walk recovery is the wrong model — one GetHistory returns an address's
	// whole history, while a block walk is O(chain length). The neutrino client
	// instead relies on that block-walk: it is what scans the compact filters for
	// the wallet's addresses, so SPV wallets default the window >0 (see
	// RecoveryWindow). 0 (the electrum default) runs no recovery; the
	// "recoverywindow" setting overrides either default.
	w.loader = wallet.NewLoader(w.netParams, w.dir, true, dbTimeout, w.recoveryWindow)
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

	// Stand up the chain backend (electrum network + client for electrum wallets,
	// a neutrino chain service + client for SPV wallets). The backend owns its own
	// resources — its Start launches them and Shutdown/WaitForShutdown tear them
	// down.
	w.chainClient, err = w.cfg.NewBackend(w.dir, w.log)
	if err != nil {
		return nil, fmt.Errorf("native: new chain backend: %w", err)
	}
	if err = w.chainClient.Start(); err != nil {
		return nil, fmt.Errorf("native: couldn't start chain backend: %w", err)
	}
	errCloser.Add(func() error { w.chainClient.Shutdown(); return nil })

	w.log.Info("Synchronizing BTC wallet with the chain backend...")
	w.btcw.Synchronize(w.chainClient)

	// Restore unconfirmed transactions from previous sessions. The master tx
	// database handle is supplied by Core via the wallet config and its lifecycle
	// is owned there, so there is nothing to open or close here.
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

	// Tie chain-backend teardown to the Connect context. Nothing else in the
	// success path shuts the backend down, so without this its goroutines and
	// resources (the electrum network, or the neutrino service and db) would leak
	// on shutdown.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		w.chainClient.Shutdown()
		w.chainClient.WaitForShutdown()
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
	if w.chainClient == nil {
		return &chainhash.Hash{}, 0
	}
	h, height, err := w.chainClient.GetBestBlock()
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
	scope := w.btcw.ScopeForPurpose(84)
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

	addr, err := btcutil.DecodeAddress(address, w.chainParams)
	if err != nil {
		return nil, fmt.Errorf("native: invalid address %q: %w", address, err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, fmt.Errorf("native: pay-to-addr script: %w", err)
	}

	txOut := wire.NewTxOut(int64(value), pkScript)
	scope := w.btcw.ScopeForPurpose(84)
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

	// Record in tx history. Fee will be updated by the next syncTxHistory pass.
	wt := &txdb.WalletTransaction{
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
		vout:   0,
		value:  value,
	}, nil
}

// StandardSendFee returns the fee for a standard segwit send (1-in 2-out).
func (w *NativeWallet) StandardSendFee(feeRate uint64) uint64 {
	// segwit: 1 P2WPKH input + 2 P2WPKH outputs
	inputSize := uint64(RedeemP2WPKHInputSize) + uint64((RedeemP2PKSigScriptSize+2+3)/4)
	sz := uint64(MinimumTxOverhead) + inputSize + P2WPKHOutputSize*2
	return feeRate * sz
}

// SyncStatus returns the current chain-sync state.
func (w *NativeWallet) SyncStatus() (*asset.SyncStatus, error) {
	if w.chainClient == nil {
		return &asset.SyncStatus{}, nil
	}

	// localTip is our synced header tip; targetTip is the height the backend is
	// syncing toward (the electrum server's reported best, the neutrino sync
	// target). targetTip is always >= localTip, so progress climbs from near-0%
	// to 100% as headers download rather than pinning at ~100%.
	localTip, targetTip, backendSynced := w.chainClient.SyncHeight()
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
		Type:        txdb.Unknown,
		ID:          txID,
		BlockNumber: uint64(details.Block.Height),
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
func (w *NativeWallet) pendingTxLoop(ctx context.Context) {
	const interval = 5 * time.Second
	ticker := time.NewTicker(interval)
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
		if w.chainClient == nil {
			return
		}
		var count uint32
		if w.chainClient.Connected() {
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

// scanNewTransactions queries the wallet for transactions since the last scan
// and records any unknown ones in the txdb.
func (w *NativeWallet) scanNewTransactions(ctx context.Context, tip uint64) {
	const blockQueryBuffer = 3

	lastQuery := w.receiveTxLastQuery.Load()
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
	amtSats := uint64(amount * 1e8)

	var feeSats uint64
	if tx.Fee != nil && *tx.Fee < 0 {
		feeSats = uint64(-*tx.Fee * 1e8)
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
	if blockNumber > 0 && blockNumber >= tip-blockQueryBuffer {
		// Too close to the tip; will be confirmed on next pass.
		blockNumber = 0
	}

	addrCopy := tx.Address
	wt := &txdb.WalletTransaction{
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

		blockHeight := uint64(details.Block.Height)
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
