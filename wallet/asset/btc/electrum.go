// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

// Package-level note: this file implements the electrum-backed BTC wallet as a
// lean, standalone asset.Wallet. It does NOT use ExchangeWalletSPV, baseWallet,
// intermediaryWallet, or tipRedeemer. All UTXO operations are delegated to
// wallet.Wallet (the in-tree utxowallet) driven by chain.ElectrumClient. Swap /
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
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

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
	chaincfg.MainNetParams.Name:       {Proto: "ssl", Addr: "electrum.blockstream.info:700"},
	chaincfg.TestNet3Params.Name:      {Proto: "ssl", Addr: "electrum.blockstream.info:993"},
	chaincfg.RegressionNetParams.Name: {Proto: "ssl", Addr: "127.0.0.1:54002"},
}

// createElectrumWallet creates the on-disk btcwallet database for an electrum
// wallet. The electrum network itself is not started here.
func createElectrumWallet(privPass, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32, net *chaincfg.Params) error {
	if err := logNeutrino(walletDir); err != nil {
		return fmt.Errorf("error initializing btcwallet logging: %w", err)
	}

	loader := wallet.NewLoader(btcNetParams(net), walletDir, true, dbTimeout, 0) // recovery_window=0; see Connect
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

// electrumWallet is a lean asset.Wallet backed by an in-process electrumx
// connection. It does NOT embed any ExchangeWallet type.
type electrumWallet struct {
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
	chainClient *chain.ElectrumClient
	network     *electrum.Network
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
}

var _ asset.Wallet = (*electrumWallet)(nil)
var _ asset.Authenticator = (*electrumWallet)(nil)

// openElectrumWallet constructs an electrumWallet without opening or starting
// anything. Called by NewWallet for walletTypeElectrum.
func openElectrumWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *electrumWallet {
	var recoveryWindow uint32
	if s := cfg.Settings["recoverywindow"]; s != "" {
		if n, err := strconv.ParseUint(s, 10, 32); err == nil {
			recoveryWindow = uint32(n)
		}
	}
	return &electrumWallet{
		log:            log,
		dir:            filepath.Join(cfg.DataDir, chainParams.Name),
		chainParams:    chainParams,
		netParams:      btcNetParams(chainParams),
		emit:           cfg.Emit,
		peersChange:    cfg.PeersChange,
		recoveryWindow: recoveryWindow,
		txdb:           cfg.NetworkTxDB,
		pendingTxs:     make(map[chainhash.Hash]txdb.WalletTransaction),
	}
}

// Info returns the WalletInfo for Bitcoin.
func (w *electrumWallet) Info() *asset.WalletInfo {
	return WalletInfo
}

// Unlock unlocks the underlying wallet for spending. A nil lock channel keeps
// it unlocked until Lock is called explicitly. Satisfies asset.Authenticator.
func (w *electrumWallet) Unlock(pw []byte) error {
	if w.btcw == nil {
		return errors.New("electrum BTC: wallet not connected")
	}
	return w.btcw.Unlock(pw, nil)
}

// Lock locks the underlying wallet. Satisfies asset.Authenticator.
func (w *electrumWallet) Lock() error {
	if w.btcw == nil {
		return errors.New("electrum BTC: wallet not connected")
	}
	w.btcw.Lock()
	return nil
}

// Locked reports whether the underlying wallet is locked. Satisfies
// asset.Authenticator.
func (w *electrumWallet) Locked() bool {
	return w.btcw == nil || w.btcw.Locked()
}

// Connect opens the btcwallet database, starts the electrum network and chain
// client, and launches background goroutines for tx-history sync and peer
// monitoring. Satisfies util.Connector.
func (w *electrumWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	if err := logNeutrino(w.dir); err != nil {
		return nil, fmt.Errorf("electrum BTC: error initializing logging: %w", err)
	}

	// recoveryWindow gates electrum-native gap-limit recovery: 0 (the default)
	// runs no recovery; a restore sets it >0 so syncWithChain probes derived
	// addresses for funds via the AddressProber path, instead of btcwallet's
	// block-walk recovery (the wrong model for electrum — one GetHistory returns
	// an address's whole history, and the block walk is O(chain length)).
	w.loader = wallet.NewLoader(w.netParams, w.dir, true, dbTimeout, w.recoveryWindow)
	exists, err := w.loader.WalletExists()
	if err != nil {
		return nil, fmt.Errorf("electrum BTC: error checking wallet existence: %w", err)
	}
	if !exists {
		return nil, errors.New("electrum BTC: wallet not found")
	}

	btcw, err := w.loader.OpenExistingWallet([]byte(wallet.InsecurePubPassphrase))
	if err != nil {
		return nil, fmt.Errorf("electrum BTC: couldn't open wallet: %w", err)
	}
	w.btcw = btcw

	errCloser := util.NewErrorCloser()
	defer errCloser.Done(w.log)
	errCloser.Add(w.loader.UnloadWallet)

	srvAddr := w.serverAddr()
	if err = srvAddr.Validate(); err != nil {
		return nil, fmt.Errorf("electrum BTC: invalid built-in server address: %w", err)
	}

	chainCfg, err := electrum.NewChainConfig(bisonwire.ChainBTC, w.netParams)
	if err != nil {
		return nil, fmt.Errorf("electrum BTC: chain config: %w", err)
	}

	w.network, err = electrum.NewNetwork(&electrum.Config{
		Chain:         chainCfg,
		TrustedServer: srvAddr,
		DataDir:       w.dir,
	})
	if err != nil {
		return nil, fmt.Errorf("electrum BTC: new network: %w", err)
	}

	// The chain client owns the electrum network's lifecycle: chainClient.Start
	// starts the network, chainClient.Stop stops it, and WaitForShutdown blocks
	// on both. Do not start the network directly here — chainClient.Start would
	// then try to start it a second time and fail with "already started".
	w.chainClient, err = chain.NewElectrumClient(w.netParams, w.network)
	if err != nil {
		return nil, fmt.Errorf("electrum BTC: new chain client: %w", err)
	}
	if err = w.chainClient.Start(); err != nil {
		return nil, fmt.Errorf("electrum BTC: couldn't start chain client: %w", err)
	}
	errCloser.Add(func() error { w.chainClient.Stop(); return nil })

	w.log.Info("Synchronizing BTC wallet with electrum network...")
	w.btcw.Synchronize(w.chainClient)

	// Restore unconfirmed transactions from previous sessions. The master tx
	// database handle is supplied by Core via the wallet config and its lifecycle
	// is owned there, so there is nothing to open or close here.
	if w.txdb != nil {
		pendingTxs, err := w.txdb.GetPendingTxs()
		if err != nil {
			return nil, fmt.Errorf("electrum BTC: loading pending txs: %w", err)
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

	// Tie chain-client teardown (and thus the electrum network) to the Connect
	// context. Nothing else in the success path stops the chain client, so
	// without this its goroutines and the network would leak on shutdown.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		w.chainClient.Stop()
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

// serverAddr returns the built-in electrumx server address for the active
// network.  Falls back to mainnet if the network name is unrecognized.
func (w *electrumWallet) serverAddr() *electrum.ServerAddr {
	if sa, ok := btcElectrumServers[w.chainParams.Name]; ok {
		return sa
	}
	return btcElectrumServers[chaincfg.MainNetParams.Name]
}

// bestBlock returns the hash and height of the best synced block according to
// the chain client, or zero/genesis values if not yet synced.
func (w *electrumWallet) bestBlock() (*chainhash.Hash, int32) {
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
func (w *electrumWallet) Balance() (*asset.Balance, error) {
	if w.btcw == nil {
		return nil, errors.New("electrum BTC: wallet not connected")
	}
	bals, err := w.btcw.CalculateAccountBalances(0, 1)
	if err != nil {
		return nil, fmt.Errorf("electrum BTC: balance error: %w", err)
	}
	return &asset.Balance{
		Available: uint64(bals.Spendable),
		Immature:  uint64(bals.ImmatureReward),
		Locked:    0,
		Other:     make(map[asset.BalanceCategory]asset.CustomBalance),
	}, nil
}

// DepositAddress returns the next unused external address.
func (w *electrumWallet) DepositAddress() (string, error) {
	if w.btcw == nil {
		return "", errors.New("electrum BTC: wallet not connected")
	}
	scope := w.btcw.ScopeForPurpose(84)
	addr, err := w.btcw.NewAddress(0, scope)
	if err != nil {
		return "", fmt.Errorf("electrum BTC: new address: %w", err)
	}
	return addr.EncodeAddress(), nil
}

// OwnsDepositAddress returns true if the address belongs to this wallet.
func (w *electrumWallet) OwnsDepositAddress(address string) (bool, error) {
	if w.btcw == nil {
		return false, errors.New("electrum BTC: wallet not connected")
	}
	addr, err := btcutil.DecodeAddress(address, w.chainParams)
	if err != nil {
		return false, nil
	}
	return w.btcw.HaveAddress(addr)
}

// ValidateAddress reports whether address is a valid Bitcoin address for the
// active network.
func (w *electrumWallet) ValidateAddress(address string) bool {
	_, err := btcutil.DecodeAddress(address, w.chainParams)
	return err == nil
}

// Send sends exactly value satoshis to address, paying feeRate sat/byte.
// Coin selection is handled by wallet.SendOutputs internally.
func (w *electrumWallet) Send(address string, value, feeRate uint64) (asset.Coin, error) {
	if w.btcw == nil {
		return nil, errors.New("electrum BTC: wallet not connected")
	}

	addr, err := btcutil.DecodeAddress(address, w.chainParams)
	if err != nil {
		return nil, fmt.Errorf("electrum BTC: invalid address %q: %w", address, err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, fmt.Errorf("electrum BTC: pay-to-addr script: %w", err)
	}

	txOut := wire.NewTxOut(int64(value), pkScript)
	scope := w.btcw.ScopeForPurpose(84)
	satPerKB := btcutil.Amount(feeRate * 1000)

	msgTx, err := w.btcw.SendOutputs(
		[]*wire.TxOut{txOut},
		&scope,
		0,        // account
		1,        // minconf
		satPerKB, // fee rate as sat/kB
		wallet.CoinSelectionLargest,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("electrum BTC: SendOutputs: %w", err)
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
func (w *electrumWallet) StandardSendFee(feeRate uint64) uint64 {
	// segwit: 1 P2WPKH input + 2 P2WPKH outputs
	inputSize := uint64(RedeemP2WPKHInputSize) + uint64((RedeemP2PKSigScriptSize+2+3)/4)
	sz := uint64(MinimumTxOverhead) + inputSize + P2WPKHOutputSize*2
	return feeRate * sz
}

// SyncStatus returns the current chain-sync state.
func (w *electrumWallet) SyncStatus() (*asset.SyncStatus, error) {
	if w.chainClient == nil || w.network == nil {
		return &asset.SyncStatus{}, nil
	}

	tip := w.network.Tip()
	if tip < 0 {
		return &asset.SyncStatus{}, nil
	}

	walletBlock := w.btcw.Manager.SyncedTo()
	synced := w.network.Synced() && walletBlock.Height > 0 &&
		walletBlock.Height >= tip

	return &asset.SyncStatus{
		Synced:       synced,
		TargetHeight: uint64(tip),
		Blocks:       uint64(walletBlock.Height),
	}, nil
}

// TxHistory returns paginated transaction history from the local txdb.
func (w *electrumWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if w.txdb == nil {
		return nil, errors.New("electrum BTC: tx database not initialized")
	}
	return w.txdb.GetTxs(req)
}

// WalletTransaction looks up a single transaction. If it is confirmed it is
// served from the local txdb; otherwise the chain client is queried.
func (w *electrumWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
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
		return nil, fmt.Errorf("electrum BTC: invalid txid %q: %w", txID, err)
	}
	details, err := wallet.UnstableAPI(w.btcw).TxDetails(txHash)
	if err != nil {
		return nil, fmt.Errorf("electrum BTC: TxDetails: %w", err)
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
func (w *electrumWallet) PendingTransactions(_ context.Context) []*txdb.WalletTransaction {
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
func (w *electrumWallet) storePendingTx(txHash *chainhash.Hash, wt *txdb.WalletTransaction) {
	if w.txdb == nil {
		return
	}
	w.pendingMtx.Lock()
	w.pendingTxs[*txHash] = *wt
	w.pendingMtx.Unlock()
	if err := w.txdb.StoreTx(wt); err != nil {
		w.log.Errorf("electrum BTC: storePendingTx %s: %v", txHash, err)
	}
	w.emit.TransactionNote(wt, true)
}

// blockLoop watches the wallet's NtfnServer for block events and triggers
// tx-history syncs.
func (w *electrumWallet) blockLoop(ctx context.Context) {
	cl := w.btcw.NtfnServer.TransactionNotifications()
	defer cl.Done()
	for {
		select {
		case note := <-cl.C:
			if len(note.AttachedBlocks) == 0 {
				continue
			}
			last := note.AttachedBlocks[len(note.AttachedBlocks)-1]
			go w.syncTxHistory(ctx, uint64(last.Height))
		case <-ctx.Done():
			return
		}
	}
}

// peerLoop periodically checks network connectivity and calls peersChange.
func (w *electrumWallet) peerLoop(ctx context.Context) {
	const interval = 5 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastCount uint32
	check := func() {
		if w.network == nil {
			return
		}
		var count uint32
		select {
		case <-w.network.Done():
			count = 0
		default:
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
func (w *electrumWallet) syncTxHistory(ctx context.Context, tip uint64) {
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
func (w *electrumWallet) scanNewTransactions(ctx context.Context, tip uint64) {
	const blockQueryBuffer = 3

	lastQuery := w.receiveTxLastQuery.Load()
	var fromHeight int32
	if lastQuery > blockQueryBuffer {
		fromHeight = int32(lastQuery - blockQueryBuffer)
	}

	syncHeight := w.btcw.Manager.SyncedTo().Height
	txs, err := w.btcw.ListSinceBlock(fromHeight, -1, syncHeight)
	if err != nil {
		w.log.Errorf("electrum BTC: ListSinceBlock(%d): %v", fromHeight, err)
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
func (w *electrumWallet) recordBtcjsonTx(_ context.Context, tx *btcjson.ListTransactionsResult, tip uint64, isInitial bool) {
	txHash, err := chainhash.NewHashFromStr(tx.TxID)
	if err != nil {
		w.log.Errorf("electrum BTC: invalid txid %q: %v", tx.TxID, err)
		return
	}

	// Skip if already recorded.
	if existing, err := w.txdb.GetTx(txHash.String()); err == nil && existing != nil {
		return
	} else if err != nil && !errors.Is(err, asset.CoinNotFoundError) {
		w.log.Errorf("electrum BTC: GetTx %s: %v", txHash, err)
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
		w.log.Errorf("electrum BTC: StoreTx %s: %v", txHash, err)
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
// pending transactions.
func (w *electrumWallet) updatePendingTransactions(ctx context.Context, tip uint64) {
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
				w.log.Errorf("electrum BTC: updating pending tx %s: %v", txHash, err)
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
