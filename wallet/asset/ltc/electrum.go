// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package ltc

// Package-level note: this file implements the electrum-backed LTC wallet as a
// lean, standalone asset.Wallet. It does NOT use ExchangeWalletSPV, baseWallet,
// intermediaryWallet, or tipRedeemer. All UTXO operations are delegated to
// wallet.Wallet (the in-tree utxowallet) driven by chain.ElectrumClient. Swap /
// FindRedemption methods are intentionally absent.
//
// LTC MWEB note: Litecoin's MWEB extension block data is stripped from
// transactions when deserialized via the bisonwire MWEB-aware decoder. Electrum
// transactions arrive as raw hex from the server and pass through the same
// bisonwire path as the neutrino backend. Any MWEB-only outputs are therefore
// silently dropped. Operations on transparent outputs (balance, receive, send)
// are unaffected. MWEB-through-electrum is the highest integration risk for LTC
// and must be verified in harness tests (Track B).

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	dexbtc "github.com/bisoncraft/bosswallet/wallet/asset/btc"
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

const confTxFinality = 1

// ltcElectrumWalletDefinition is the WalletDefinition for the electrum-backed
// seeded LTC wallet. No user-configurable server/proxy knobs.
var ltcElectrumWalletDefinition = &asset.WalletDefinition{
	Type:        walletTypeElectrum,
	Tab:         "Electrum",
	Description: "Use the built-in electrum-backed light wallet.",
	ConfigOpts:  nil,
	Seeded:      true,
}

// ltcElectrumServers maps chaincfg.Params.Name to a built-in electrumx
// ServerAddr for Litecoin. The regtest/simnet entry points at the dextest
// harness (LTC port 55002, ssl); mainnet and testnet use well-known public
// servers. Keys are the actual *chaincfg.Params.Name values so they cannot
// drift out of sync with parseChainParams.
var ltcElectrumServers = map[string]*electrum.ServerAddr{
	MainNetParams.Name:       {Proto: "ssl", Addr: "electrum-ltc.bysh.me:50002"},
	TestNet4Params.Name:      {Proto: "ssl", Addr: "electrum-ltc.bysh.me:51002"},
	RegressionNetParams.Name: {Proto: "ssl", Addr: "127.0.0.1:55002"},
}

// createLTCElectrumWallet creates the on-disk btcwallet database for an LTC
// electrum wallet. The electrum network is not started here.
func createLTCElectrumWallet(privPass, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32, net *chaincfg.Params) error {
	if err := logNeutrino(walletDir); err != nil {
		return fmt.Errorf("error initializing ltcwallet logging: %w", err)
	}

	np := ltcNetParams(net)
	loader := wallet.NewLoader(np, walletDir, true, dbTimeout, 0) // recovery_window=0; see Connect
	pubPass := []byte(wallet.InsecurePubPassphrase)

	ltcw, err := loader.CreateNewWallet(pubPass, privPass, seed, bday)
	if err != nil {
		return fmt.Errorf("CreateNewWallet error: %w", err)
	}

	bail := func() {
		if err := loader.UnloadWallet(); err != nil {
			log.Errorf("Error unloading wallet after createLTCElectrumWallet error: %v", err)
		}
	}

	if extIdx > 0 || intIdx > 0 {
		if err = extendAddresses(extIdx, intIdx, ltcw); err != nil {
			bail()
			return fmt.Errorf("failed to set starting address indexes: %w", err)
		}
	}

	if err := loader.UnloadWallet(); err != nil {
		return fmt.Errorf("error unloading wallet: %w", err)
	}
	return nil
}

// ltcElectrumWallet is a lean asset.Wallet for Litecoin backed by an in-process
// electrumx connection. It does NOT embed any ExchangeWallet type.
type ltcElectrumWallet struct {
	log         util.Logger
	dir         string
	chainParams *chaincfg.Params
	netParams   *netparams.ChainParams
	emit        *asset.WalletEmitter
	peersChange func(uint32, error)

	// populated during Connect
	ltcw        *wallet.Wallet
	loader      *wallet.Loader
	chainClient *chain.ElectrumClient
	network     *electrum.Network
	txdb        txdb.NetworkTxDB

	pendingMtx sync.RWMutex
	pendingTxs map[chainhash.Hash]txdb.WalletTransaction

	receiveTxLastQuery atomic.Uint64
	syncingTxHistory   atomic.Bool
}

var _ asset.Wallet = (*ltcElectrumWallet)(nil)
var _ asset.Authenticator = (*ltcElectrumWallet)(nil)

// openLTCElectrumWallet constructs an ltcElectrumWallet without opening or
// starting anything. Called by NewWallet for walletTypeElectrum.
func openLTCElectrumWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *ltcElectrumWallet {
	return &ltcElectrumWallet{
		log:         log,
		dir:         filepath.Join(cfg.DataDir, chainParams.Name),
		chainParams: chainParams,
		netParams:   ltcNetParams(chainParams),
		emit:        cfg.Emit,
		peersChange: cfg.PeersChange,
		txdb:        cfg.NetworkTxDB,
		pendingTxs:  make(map[chainhash.Hash]txdb.WalletTransaction),
	}
}

// Info returns the WalletInfo for Litecoin.
func (w *ltcElectrumWallet) Info() *asset.WalletInfo {
	return WalletInfo
}

// Unlock unlocks the underlying wallet for spending. A nil lock channel keeps
// it unlocked until Lock is called explicitly. Satisfies asset.Authenticator.
func (w *ltcElectrumWallet) Unlock(pw []byte) error {
	if w.ltcw == nil {
		return errors.New("electrum LTC: wallet not connected")
	}
	return w.ltcw.Unlock(pw, nil)
}

// Lock locks the underlying wallet. Satisfies asset.Authenticator.
func (w *ltcElectrumWallet) Lock() error {
	if w.ltcw == nil {
		return errors.New("electrum LTC: wallet not connected")
	}
	w.ltcw.Lock()
	return nil
}

// Locked reports whether the underlying wallet is locked. Satisfies
// asset.Authenticator.
func (w *ltcElectrumWallet) Locked() bool {
	return w.ltcw == nil || w.ltcw.Locked()
}

// Connect opens the ltcwallet database, starts the electrum network and chain
// client, and launches background goroutines. Satisfies util.Connector.
func (w *ltcElectrumWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	if err := logNeutrino(w.dir); err != nil {
		return nil, fmt.Errorf("electrum LTC: error initializing logging: %w", err)
	}

	// recovery_window=0: btcwallet's block-walk recovery is the wrong model for
	// an electrum backend — one GetHistory returns an address's entire history,
	// and the block walk is pathologically slow on a deep birthday (it scans the
	// whole chain and never reaches Synced). Electrum-native gap-limit recovery
	// is a tracked follow-up; until then a restored seed discovers funds only on
	// already-derived addresses.
	w.loader = wallet.NewLoader(w.netParams, w.dir, true, dbTimeout, 0)
	exists, err := w.loader.WalletExists()
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: error checking wallet existence: %w", err)
	}
	if !exists {
		return nil, errors.New("electrum LTC: wallet not found")
	}

	ltcw, err := w.loader.OpenExistingWallet([]byte(wallet.InsecurePubPassphrase))
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: couldn't open wallet: %w", err)
	}
	w.ltcw = ltcw

	errCloser := util.NewErrorCloser()
	defer errCloser.Done(w.log)
	errCloser.Add(w.loader.UnloadWallet)

	srvAddr := w.serverAddr()
	if err = srvAddr.Validate(); err != nil {
		return nil, fmt.Errorf("electrum LTC: invalid built-in server address: %w", err)
	}

	chainCfg, err := electrum.NewChainConfig(bisonwire.ChainLTC, w.netParams)
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: chain config: %w", err)
	}

	w.network, err = electrum.NewNetwork(&electrum.Config{
		Chain:         chainCfg,
		TrustedServer: srvAddr,
		DataDir:       w.dir,
	})
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: new network: %w", err)
	}

	// The chain client owns the electrum network's lifecycle: chainClient.Start
	// starts the network, chainClient.Stop stops it, and WaitForShutdown blocks
	// on both. Do not start the network directly here — chainClient.Start would
	// then try to start it a second time and fail with "already started".
	w.chainClient, err = chain.NewElectrumClient(w.netParams, w.network)
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: new chain client: %w", err)
	}
	if err = w.chainClient.Start(); err != nil {
		return nil, fmt.Errorf("electrum LTC: couldn't start chain client: %w", err)
	}
	errCloser.Add(func() error { w.chainClient.Stop(); return nil })

	w.log.Info("Synchronizing LTC wallet with electrum network...")
	w.ltcw.Synchronize(w.chainClient)

	// The master tx database handle is supplied by Core via the wallet config;
	// its lifecycle is owned there, so there is nothing to open or close here.
	if w.txdb != nil {
		pendingTxs, err := w.txdb.GetPendingTxs()
		if err != nil {
			return nil, fmt.Errorf("electrum LTC: loading pending txs: %w", err)
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

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.blockLoop(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.peerLoop(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
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
// network. Falls back to mainnet if the network name is unrecognized.
func (w *ltcElectrumWallet) serverAddr() *electrum.ServerAddr {
	if sa, ok := ltcElectrumServers[w.chainParams.Name]; ok {
		return sa
	}
	return ltcElectrumServers[MainNetParams.Name]
}

// bestBlock returns the best block known to the chain client.
func (w *ltcElectrumWallet) bestBlock() (*chainhash.Hash, int32) {
	if w.chainClient == nil {
		return &chainhash.Hash{}, 0
	}
	h, height, err := w.chainClient.GetBestBlock()
	if err != nil {
		return &chainhash.Hash{}, 0
	}
	return h, height
}

// Balance returns available, immature, and locked balances.
func (w *ltcElectrumWallet) Balance() (*asset.Balance, error) {
	if w.ltcw == nil {
		return nil, errors.New("electrum LTC: wallet not connected")
	}
	bals, err := w.ltcw.CalculateAccountBalances(0, 1)
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: balance error: %w", err)
	}
	return &asset.Balance{
		Available: uint64(bals.Spendable),
		Immature:  uint64(bals.ImmatureReward),
		Locked:    0,
		Other:     make(map[asset.BalanceCategory]asset.CustomBalance),
	}, nil
}

// DepositAddress returns the next unused external address.
func (w *ltcElectrumWallet) DepositAddress() (string, error) {
	if w.ltcw == nil {
		return "", errors.New("electrum LTC: wallet not connected")
	}
	scope := w.ltcw.ScopeForPurpose(84)
	addr, err := w.ltcw.NewAddress(0, scope)
	if err != nil {
		return "", fmt.Errorf("electrum LTC: new address: %w", err)
	}
	return addr.EncodeAddress(), nil
}

// OwnsDepositAddress returns true if address belongs to this wallet.
func (w *ltcElectrumWallet) OwnsDepositAddress(address string) (bool, error) {
	if w.ltcw == nil {
		return false, errors.New("electrum LTC: wallet not connected")
	}
	addr, err := btcutil.DecodeAddress(address, w.chainParams)
	if err != nil {
		return false, nil
	}
	return w.ltcw.HaveAddress(addr)
}

// ValidateAddress reports whether address is a valid Litecoin address.
func (w *ltcElectrumWallet) ValidateAddress(address string) bool {
	_, err := btcutil.DecodeAddress(address, w.chainParams)
	return err == nil
}

// Send sends exactly value satoshis to address at feeRate sat/byte.
func (w *ltcElectrumWallet) Send(address string, value, feeRate uint64) (asset.Coin, error) {
	if w.ltcw == nil {
		return nil, errors.New("electrum LTC: wallet not connected")
	}

	addr, err := btcutil.DecodeAddress(address, w.chainParams)
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: invalid address %q: %w", address, err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: pay-to-addr script: %w", err)
	}

	txOut := wire.NewTxOut(int64(value), pkScript)
	scope := w.ltcw.ScopeForPurpose(84)
	satPerKB := btcutil.Amount(feeRate * 1000)

	msgTx, err := w.ltcw.SendOutputs(
		[]*wire.TxOut{txOut},
		&scope,
		0,
		1,
		satPerKB,
		wallet.CoinSelectionLargest,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: SendOutputs: %w", err)
	}

	txHash := msgTx.TxHash()
	addrCopy := address
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

	return &ltcElectrumCoin{
		txHash: txHash,
		vout:   0,
		value:  value,
	}, nil
}

// StandardSendFee returns the fee for a standard segwit send (1-in 2-out).
func (w *ltcElectrumWallet) StandardSendFee(feeRate uint64) uint64 {
	inputSize := uint64(dexbtc.RedeemP2WPKHInputSize) + uint64((dexbtc.RedeemP2PKSigScriptSize+2+3)/4)
	sz := uint64(dexbtc.MinimumTxOverhead) + inputSize + dexbtc.P2WPKHOutputSize*2
	return feeRate * sz
}

// SyncStatus returns the current sync state.
func (w *ltcElectrumWallet) SyncStatus() (*asset.SyncStatus, error) {
	if w.chainClient == nil || w.network == nil {
		return &asset.SyncStatus{}, nil
	}
	tip := w.network.Tip()
	if tip < 0 {
		return &asset.SyncStatus{}, nil
	}
	walletBlock := w.ltcw.Manager.SyncedTo()
	synced := w.network.Synced() && walletBlock.Height > 0 &&
		walletBlock.Height >= tip
	return &asset.SyncStatus{
		Synced:       synced,
		TargetHeight: uint64(tip),
		Blocks:       uint64(walletBlock.Height),
	}, nil
}

// TxHistory returns paginated transaction history from the local txdb.
func (w *ltcElectrumWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if w.txdb == nil {
		return nil, errors.New("electrum LTC: tx database not initialized")
	}
	return w.txdb.GetTxs(req)
}

// WalletTransaction looks up a single transaction.
func (w *ltcElectrumWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	// Accept a hex-encoded 36-byte coin ID (txid[32] || vout[4]) in addition to
	// a plain txid. ToCoinID produces such an encoding; attempt to decode it.
	if coinID, err := hex.DecodeString(txID); err == nil && len(coinID) == 36 {
		var h chainhash.Hash
		copy(h[:], coinID[:32])
		// Reverse byte order to get the human-readable txid.
		for i, j := 0, 31; i < j; i, j = i+1, j-1 {
			h[i], h[j] = h[j], h[i]
		}
		txID = h.String()
	}

	if w.txdb != nil {
		tx, err := w.txdb.GetTx(txID)
		if err == nil && tx != nil && tx.Confirmed {
			return tx, nil
		}
	}

	txHash, err := chainhash.NewHashFromStr(txID)
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: invalid txid %q: %w", txID, err)
	}
	details, err := wallet.UnstableAPI(w.ltcw).TxDetails(txHash)
	if err != nil {
		return nil, fmt.Errorf("electrum LTC: TxDetails: %w", err)
	}
	if details == nil {
		return nil, asset.CoinNotFoundError
	}

	return &txdb.WalletTransaction{
		Type:        txdb.Unknown,
		ID:          txID,
		BlockNumber: uint64(details.Block.Height),
		Confirmed:   details.Block.Height > 0,
	}, nil
}

// PendingTransactions returns unconfirmed transactions tracked by this wallet.
func (w *ltcElectrumWallet) PendingTransactions(_ context.Context) []*txdb.WalletTransaction {
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

// storePendingTx records a sent transaction in the txdb and the in-memory map.
func (w *ltcElectrumWallet) storePendingTx(txHash *chainhash.Hash, wt *txdb.WalletTransaction) {
	if w.txdb == nil {
		return
	}
	w.pendingMtx.Lock()
	w.pendingTxs[*txHash] = *wt
	w.pendingMtx.Unlock()
	if err := w.txdb.StoreTx(wt); err != nil {
		w.log.Errorf("electrum LTC: storePendingTx %s: %v", txHash, err)
	}
	w.emit.TransactionNote(wt, true)
}

// blockLoop watches the wallet NtfnServer and triggers tx-history syncs.
func (w *ltcElectrumWallet) blockLoop(ctx context.Context) {
	cl := w.ltcw.NtfnServer.TransactionNotifications()
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

// peerLoop monitors connectivity and calls peersChange.
func (w *ltcElectrumWallet) peerLoop(ctx context.Context) {
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

// syncTxHistory scans for new wallet transactions and updates pending ones.
func (w *ltcElectrumWallet) syncTxHistory(ctx context.Context, tip uint64) {
	if !w.syncingTxHistory.CompareAndSwap(false, true) {
		return
	}
	defer w.syncingTxHistory.Store(false)

	if w.txdb == nil || w.ltcw == nil {
		return
	}

	ss, err := w.SyncStatus()
	if err != nil || ss == nil || !ss.Synced {
		return
	}

	w.scanNewTransactions(ctx, tip)
	w.updatePendingTransactions(ctx, tip)
}

// scanNewTransactions records unknown wallet transactions in the txdb.
func (w *ltcElectrumWallet) scanNewTransactions(ctx context.Context, tip uint64) {
	const blockQueryBuffer = 3

	lastQuery := w.receiveTxLastQuery.Load()
	var fromHeight int32
	if lastQuery > blockQueryBuffer {
		fromHeight = int32(lastQuery - blockQueryBuffer)
	}

	syncHeight := w.ltcw.Manager.SyncedTo().Height
	txs, err := w.ltcw.ListSinceBlock(fromHeight, -1, syncHeight)
	if err != nil {
		w.log.Errorf("electrum LTC: ListSinceBlock(%d): %v", fromHeight, err)
		return
	}

	isInitial := lastQuery == 0

	for i := range txs {
		if ctx.Err() != nil {
			return
		}
		w.recordBtcjsonTx(&txs[i], tip, isInitial)
	}

	w.receiveTxLastQuery.Store(tip)

	if isInitial {
		w.emit.TransactionHistorySyncedNote()
	}
}

// recordBtcjsonTx processes one btcjson.ListTransactionsResult entry.
func (w *ltcElectrumWallet) recordBtcjsonTx(tx *btcjson.ListTransactionsResult, tip uint64, isInitial bool) {
	txHash, err := chainhash.NewHashFromStr(tx.TxID)
	if err != nil {
		w.log.Errorf("electrum LTC: invalid txid %q: %v", tx.TxID, err)
		return
	}

	if existing, err := w.txdb.GetTx(txHash.String()); err == nil && existing != nil {
		return
	} else if err != nil && !errors.Is(err, asset.CoinNotFoundError) {
		w.log.Errorf("electrum LTC: GetTx %s: %v", txHash, err)
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
		w.log.Errorf("electrum LTC: StoreTx %s: %v", txHash, err)
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

// updatePendingTransactions refreshes confirmation status for pending txs.
func (w *ltcElectrumWallet) updatePendingTransactions(ctx context.Context, tip uint64) {
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

		details, err := wallet.UnstableAPI(w.ltcw).TxDetails(&txHash)
		if err != nil || details == nil {
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
				w.log.Errorf("electrum LTC: updating pending tx %s: %v", txHash, err)
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

// ltcElectrumCoin implements asset.Coin for a sent UTXO.
type ltcElectrumCoin struct {
	txHash chainhash.Hash
	vout   uint32
	value  uint64
}

func (c *ltcElectrumCoin) ID() util.Bytes {
	return dexbtc.ToCoinID(&c.txHash, c.vout)
}

func (c *ltcElectrumCoin) String() string {
	return fmt.Sprintf("%s:%d", c.txHash, c.vout)
}

func (c *ltcElectrumCoin) Value() uint64 { return c.value }

func (c *ltcElectrumCoin) TxID() string { return c.txHash.String() }
