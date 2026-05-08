// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package ltc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	dexltc "github.com/bisoncraft/bosswallet/util/networks/ltc"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/btc"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/assets"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/chain"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv/headerfs"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet/txauthor"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	_ "github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb/bdb"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/gcs"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	"github.com/jrick/logrotate/rotator"
)

const (
	DefaultM       uint64 = 784931 // From ltcutil. Used for gcs filters.
	logDirName            = "logs"
	neutrinoDBName        = "neutrino.db"
	defaultAcctNum        = 0
	dbTimeout             = 20 * time.Second
)

var (
	waddrmgrNamespace = []byte("waddrmgr")

	// Snapshot of valid peers. 10 May 2024
	testnet4Seeds = []string{
		"13.200.66.216:19335",
		"208.91.111.150:18333",
		"92.244.111.167:19335",
		"164.92.171.95:8333",
		"204.16.244.114:18333",
		"34.227.13.195:19335",
		"18.192.56.149:18333",
	}
)

// ltcSPVWallet is an implementation of btc.BTCWallet that runs a native
// Litecoin SPV wallet using the unified utxowallet library.
type ltcSPVWallet struct {
	// This section is populated in openSPVWallet.
	dir            string
	btcParams      *chaincfg.Params
	netChainParams *netparams.ChainParams
	log            util.Logger

	// This section is populated in Start.
	*wallet.Wallet
	chainClient *chain.NeutrinoClient
	cl          *spv.ChainService
	loader      *wallet.Loader
	neutrinoDB  walletdb.DB

	// rescanStarting is set while reloading the wallet and dropping
	// transactions from the wallet db.
	rescanStarting uint32 // atomic

	peerManager *btc.SPVPeerManager
}

var _ btc.BTCWallet = (*ltcSPVWallet)(nil)

// ltcNetParams returns the *netparams.ChainParams for a given *chaincfg.Params
// by looking up the LTC network in the assets table.
func ltcNetParams(btcParams *chaincfg.Params) *netparams.ChainParams {
	switch btcParams.Name {
	case dexltc.MainNetParams.Name:
		return assets.LTCParams["mainnet"]
	case dexltc.TestNet4Params.Name:
		return assets.LTCParams["testnet"]
	case dexltc.RegressionNetParams.Name:
		return assets.LTCParams["simnet"]
	default:
		// Fallback: return mainnet params (should not happen in practice).
		return assets.LTCParams["mainnet"]
	}
}

// openSPVWallet creates an ltcSPVWallet, but does not Start.
func openSPVWallet(dir string, cfg *btc.WalletConfig, btcParams *chaincfg.Params, log util.Logger) btc.BTCWallet {
	return &ltcSPVWallet{
		dir:            dir,
		btcParams:      btcParams,
		netChainParams: ltcNetParams(btcParams),
		log:            log,
	}
}

// createSPVWallet creates a new SPV wallet.
func createSPVWallet(privPass []byte, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32, net *chaincfg.Params) error {
	if err := logNeutrino(walletDir); err != nil {
		return fmt.Errorf("error initializing ltcwallet+neutrino logging: %w", err)
	}

	np := ltcNetParams(net)
	loader := wallet.NewLoader(np, walletDir, true, dbTimeout, 250)

	pubPass := []byte(wallet.InsecurePubPassphrase)

	ltcw, err := loader.CreateNewWallet(pubPass, privPass, seed, bday)
	if err != nil {
		return fmt.Errorf("CreateNewWallet error: %w", err)
	}

	errCloser := util.NewErrorCloser()
	defer errCloser.Done(log)
	errCloser.Add(loader.UnloadWallet)

	if extIdx > 0 || intIdx > 0 {
		err = extendAddresses(extIdx, intIdx, ltcw)
		if err != nil {
			return fmt.Errorf("failed to set starting address indexes: %w", err)
		}
	}

	// The chain service DB
	neutrinoDBPath := filepath.Join(walletDir, neutrinoDBName)
	db, err := walletdb.Create("bdb", neutrinoDBPath, true, dbTimeout)
	if err != nil {
		return fmt.Errorf("unable to create neutrino db at %q: %w", neutrinoDBPath, err)
	}
	if err = db.Close(); err != nil {
		return fmt.Errorf("error closing newly created wallet database: %w", err)
	}

	if err := loader.UnloadWallet(); err != nil {
		return fmt.Errorf("error unloading wallet: %w", err)
	}

	errCloser.Success()
	return nil
}

// Start initializes the *wallet.Wallet and its supporting players and starts
// syncing.
func (w *ltcSPVWallet) Start() (btc.SPVService, error) {
	if err := logNeutrino(w.dir); err != nil {
		return nil, fmt.Errorf("error initializing ltcwallet+neutrino logging: %v", err)
	}

	w.loader = wallet.NewLoader(w.netChainParams, w.dir, true, dbTimeout, 250)

	exists, err := w.loader.WalletExists()
	if err != nil {
		return nil, fmt.Errorf("error verifying wallet existence: %v", err)
	}
	if !exists {
		return nil, errors.New("wallet not found")
	}

	w.log.Debug("Starting native LTC wallet...")
	w.Wallet, err = w.loader.OpenExistingWallet([]byte(wallet.InsecurePubPassphrase))
	if err != nil {
		return nil, fmt.Errorf("couldn't load wallet: %w", err)
	}

	errCloser := util.NewErrorCloser()
	defer errCloser.Done(w.log)
	errCloser.Add(w.loader.UnloadWallet)

	neutrinoDBPath := filepath.Join(w.dir, neutrinoDBName)
	w.neutrinoDB, err = walletdb.Create("bdb", neutrinoDBPath, true, dbTimeout)
	if err != nil {
		return nil, fmt.Errorf("unable to create wallet db at %q: %v", neutrinoDBPath, err)
	}
	errCloser.Add(w.neutrinoDB.Close)

	w.log.Debug("Starting neutrino chain service...")
	w.cl, err = spv.NewChainService(spv.Config{
		DataDir:       w.dir,
		Database:      w.neutrinoDB,
		Chain:         "ltc",
		ChainParams:   w.netChainParams,
		PersistToDisk: true, // keep cfilter headers on disk for efficient rescanning
		// WARNING: PublishTransaction currently uses the entire duration
		// because of an external bug, but even if resolved, a typical
		// inv/getdata round trip is ~4 seconds, so we set this so neutrino does
		// not cancel queries too readily.
		BroadcastTimeout: 6 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("couldn't create Neutrino ChainService: %v", err)
	}
	errCloser.Add(w.cl.Stop)

	w.chainClient = chain.NewNeutrinoClient(w.netChainParams, w.cl)

	var defaultPeers []string
	switch w.btcParams.Name {
	case dexltc.TestNet4Params.Name:
		defaultPeers = append([]string{"127.0.0.1:19335"}, testnet4Seeds...)
	case dexltc.RegressionNetParams.Name:
		defaultPeers = []string{"127.0.0.1:20585"}
	}
	peerManager := btc.NewSPVPeerManager(&ltcChainService{w.cl}, defaultPeers, w.dir, w.log, w.netChainParams.DefaultPort)
	w.peerManager = peerManager

	if err = w.chainClient.Start(); err != nil { // lazily starts connmgr
		return nil, fmt.Errorf("couldn't start Neutrino client: %v", err)
	}

	w.log.Info("Synchronizing wallet with network...")
	w.Wallet.Synchronize(w.chainClient)

	errCloser.Success()

	w.peerManager.ConnectToInitialWalletPeers()

	return &ltcChainService{w.cl}, nil
}

func (w *ltcSPVWallet) Birthday() time.Time {
	return w.Manager.Birthday()
}

func (w *ltcSPVWallet) updateDBBirthday(bday time.Time) error {
	ltcw, isLoaded := w.loader.LoadedWallet()
	if !isLoaded {
		return fmt.Errorf("wallet not loaded")
	}
	return walletdb.Update(ltcw.Database(), func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(waddrmgrNamespace)
		return ltcw.Manager.SetBirthday(ns, bday)
	})
}

// PublishTransaction publishes the transaction to the network.
func (w *ltcSPVWallet) PublishTransaction(tx *wire.MsgTx, label string) error {
	return w.Wallet.PublishTransaction(tx, label)
}

// CalculateAccountBalances returns account balance information.
func (w *ltcSPVWallet) CalculateAccountBalances(account uint32, confirms int32) (wallet.Balances, error) {
	return w.Wallet.CalculateAccountBalances(account, confirms)
}

// ListSinceBlock returns transaction information since the given block.
func (w *ltcSPVWallet) ListSinceBlock(start, end, syncHeight int32) ([]btcjson.ListTransactionsResult, error) {
	return w.Wallet.ListSinceBlock(start, end, syncHeight)
}

// GetTransactions returns transaction information within the given range.
func (w *ltcSPVWallet) GetTransactions(startBlock, endBlock int32, accountName string, cancel <-chan struct{}) (*wallet.GetTransactionsResult, error) {
	startID := wallet.NewBlockIdentifierFromHeight(startBlock)
	endID := wallet.NewBlockIdentifierFromHeight(endBlock)
	return w.Wallet.GetTransactions(startID, endID, accountName, cancel)
}

// ListUnspent returns unspent outputs.
func (w *ltcSPVWallet) ListUnspent(minconf, maxconf int32, acctName string) ([]*btcjson.ListUnspentResult, error) {
	return w.Wallet.ListUnspent(minconf, maxconf, acctName)
}

// FetchInputInfo returns information about a transaction input.
func (w *ltcSPVWallet) FetchInputInfo(prevOut *wire.OutPoint) (*wire.MsgTx, *wire.TxOut, *psbt.Bip32Derivation, int64, error) {
	return w.Wallet.FetchInputInfo(prevOut)
}

// ResetLockedOutpoints clears all locked outpoints.
func (w *ltcSPVWallet) ResetLockedOutpoints() {
	w.Wallet.ResetLockedOutpoints()
}

// LockOutpoint locks an outpoint.
func (w *ltcSPVWallet) LockOutpoint(op wire.OutPoint) {
	w.Wallet.LockOutpoint(op)
}

// UnlockOutpoint unlocks an outpoint.
func (w *ltcSPVWallet) UnlockOutpoint(op wire.OutPoint) {
	w.Wallet.UnlockOutpoint(op)
}

// LockedOutpoints returns all locked outpoints.
func (w *ltcSPVWallet) LockedOutpoints() []btcjson.TransactionInput {
	return w.Wallet.LockedOutpoints()
}

// NewChangeAddress returns a new change address.
func (w *ltcSPVWallet) NewChangeAddress(account uint32, _ waddrmgr.KeyScope) (btcutil.Address, error) {
	return w.Wallet.NewChangeAddress(account, w.Wallet.ScopeForPurpose(84))
}

// NewAddress returns a new external address.
func (w *ltcSPVWallet) NewAddress(account uint32, _ waddrmgr.KeyScope) (btcutil.Address, error) {
	return w.Wallet.NewAddress(account, w.Wallet.ScopeForPurpose(84))
}

// PrivKeyForAddress returns the private key for the given address.
func (w *ltcSPVWallet) PrivKeyForAddress(a btcutil.Address) (*btcec.PrivateKey, error) {
	return w.Wallet.PrivKeyForAddress(a)
}

// SendOutputs sends outputs from the wallet.
func (w *ltcSPVWallet) SendOutputs(outputs []*wire.TxOut, _ *waddrmgr.KeyScope, account uint32, minconf int32,
	satPerKb btcutil.Amount, coinSelStrategy wallet.CoinSelectionStrategy, label string) (*wire.MsgTx, error) {
	scope := w.Wallet.ScopeForPurpose(84)
	return w.Wallet.SendOutputs(outputs, &scope, account, minconf, satPerKb, coinSelStrategy, label)
}

// HaveAddress checks whether an address belongs to the wallet.
func (w *ltcSPVWallet) HaveAddress(a btcutil.Address) (bool, error) {
	return w.Wallet.HaveAddress(a)
}

// AccountProperties returns properties of the specified account.
func (w *ltcSPVWallet) AccountProperties(_ waddrmgr.KeyScope, acct uint32) (*waddrmgr.AccountProperties, error) {
	return w.Wallet.AccountProperties(w.Wallet.ScopeForPurpose(84), acct)
}

// Stop stops the wallet and database threads.
func (w *ltcSPVWallet) Stop() {
	w.log.Info("Unloading wallet")
	if err := w.loader.UnloadWallet(); err != nil {
		w.log.Errorf("UnloadWallet error: %v", err)
	}
	if w.chainClient != nil {
		w.log.Trace("Stopping neutrino client chain interface")
		w.chainClient.Stop()
		w.chainClient.WaitForShutdown()
	}
	w.log.Trace("Stopping neutrino chain sync service")
	if err := w.cl.Stop(); err != nil {
		w.log.Errorf("error stopping neutrino chain service: %v", err)
	}
	w.log.Trace("Stopping neutrino DB.")
	if err := w.neutrinoDB.Close(); err != nil {
		w.log.Errorf("wallet db close error: %v", err)
	}

	w.log.Info("SPV wallet closed")
}

// RescanAsync initiates a full wallet recovery.
func (w *ltcSPVWallet) RescanAsync() error {
	if !atomic.CompareAndSwapUint32(&w.rescanStarting, 0, 1) {
		w.log.Error("rescan already in progress")
	}
	defer atomic.StoreUint32(&w.rescanStarting, 0)
	w.log.Info("Stopping wallet and chain client...")
	w.Wallet.Stop() // stops Wallet and chainClient (not chainService)
	w.Wallet.WaitForShutdown()
	w.chainClient.WaitForShutdown()

	w.ForceRescan()

	w.log.Info("Starting wallet...")
	w.Wallet.Start()

	if err := w.chainClient.Start(); err != nil {
		return fmt.Errorf("couldn't start Neutrino client: %v", err)
	}

	w.log.Info("Synchronizing wallet with network...")
	w.Wallet.Synchronize(w.chainClient)
	return nil
}

// ForceRescan forces a full rescan with active address discovery.
func (w *ltcSPVWallet) ForceRescan() {
	wdb := w.Wallet.Database()

	w.log.Info("Dropping transaction history to perform full rescan...")
	err := wallet.DropTransactionHistory(wdb, false)
	if err != nil {
		w.log.Errorf("Failed to drop wallet transaction history: %v", err)
		// Continue to attempt restarting the wallet anyway.
	}

	err = walletdb.Update(wdb, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(waddrmgrNamespace)
		return w.Wallet.Manager.SetSyncedTo(ns, nil) // never synced, forcing recover from birthday
	})
	if err != nil {
		w.log.Errorf("Failed to reset wallet manager sync height: %v", err)
	}
}

// WalletTransaction pulls the transaction from the database.
func (w *ltcSPVWallet) WalletTransaction(txHash *chainhash.Hash) (*wtxmgr.TxDetails, error) {
	details, err := wallet.UnstableAPI(w.Wallet).TxDetails(txHash)
	if err != nil {
		return nil, err
	}
	if details == nil {
		return nil, btc.WalletTransactionNotFound
	}

	return details, nil
}

// SyncedTo returns the wallet's synced-to block.
func (w *ltcSPVWallet) SyncedTo() waddrmgr.BlockStamp {
	return w.Wallet.Manager.SyncedTo()
}

// SignTx signs the transaction inputs.
func (w *ltcSPVWallet) SignTx(tx *wire.MsgTx) error {
	var prevPkScripts [][]byte
	var inputValues []btcutil.Amount
	for _, txIn := range tx.TxIn {
		_, txOut, _, _, err := w.Wallet.FetchInputInfo(&txIn.PreviousOutPoint)
		if err != nil {
			return err
		}
		inputValues = append(inputValues, btcutil.Amount(txOut.Value))
		prevPkScripts = append(prevPkScripts, txOut.PkScript)
		// Zero the previous witness and signature script or else
		// AddAllInputScripts does some weird stuff.
		txIn.SignatureScript = nil
		txIn.Witness = nil
	}
	return txauthor.AddAllInputScripts(tx, prevPkScripts, inputValues, &secretSource{w, w.btcParams})
}

// BlockNotifications returns a channel on which to receive notifications of
// newly processed blocks.
func (w *ltcSPVWallet) BlockNotifications(ctx context.Context) <-chan *btc.BlockNotification {
	cl := w.NtfnServer.TransactionNotifications()
	ch := make(chan *btc.BlockNotification, 1)
	go func() {
		defer cl.Done()
		for {
			select {
			case note := <-cl.C:
				if len(note.AttachedBlocks) > 0 {
					lastBlock := note.AttachedBlocks[len(note.AttachedBlocks)-1]
					select {
					case ch <- &btc.BlockNotification{
						Hash:   *lastBlock.Hash,
						Height: lastBlock.Height,
					}:
					default:
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// Peers returns the wallet's SPV peers.
func (w *ltcSPVWallet) Peers() ([]*asset.WalletPeer, error) {
	return w.peerManager.Peers()
}

// AddPeer adds a peer to the wallet.
func (w *ltcSPVWallet) AddPeer(addr string) error {
	return w.peerManager.AddPeer(addr)
}

// RemovePeer removes a peer from the wallet.
func (w *ltcSPVWallet) RemovePeer(addr string) error {
	return w.peerManager.RemovePeer(addr)
}

// TotalReceivedForAddr returns the total amount received for an address.
func (w *ltcSPVWallet) TotalReceivedForAddr(addr btcutil.Address, minConf int32) (btcutil.Amount, error) {
	return w.Wallet.TotalReceivedForAddr(addr, minConf)
}

// secretSource is used to locate keys and redemption scripts while signing a
// transaction. secretSource satisfies the txauthor.SecretsSource interface.
type secretSource struct {
	w           *ltcSPVWallet
	chainParams *chaincfg.Params
}

// ChainParams returns the chain parameters.
func (s *secretSource) ChainParams() *chaincfg.Params {
	return s.chainParams
}

// GetKey fetches a private key for the specified address.
func (s *secretSource) GetKey(addr btcutil.Address) (*btcec.PrivateKey, bool, error) {
	ma, err := s.w.Wallet.AddressInfo(addr)
	if err != nil {
		return nil, false, err
	}

	mpka, ok := ma.(waddrmgr.ManagedPubKeyAddress)
	if !ok {
		e := fmt.Errorf("managed address type for %v is `%T` but "+
			"want waddrmgr.ManagedPubKeyAddress", addr, ma)
		return nil, false, e
	}

	privKey, err := mpka.PrivKey()
	if err != nil {
		return nil, false, err
	}
	return privKey, ma.Compressed(), nil
}

// GetScript fetches the redemption script for the specified p2sh/p2wsh address.
func (s *secretSource) GetScript(addr btcutil.Address) ([]byte, error) {
	ma, err := s.w.Wallet.AddressInfo(addr)
	if err != nil {
		return nil, err
	}

	msa, ok := ma.(waddrmgr.ManagedScriptAddress)
	if !ok {
		e := fmt.Errorf("managed address type for %v is `%T` but "+
			"want waddrmgr.ManagedScriptAddress", addr, ma)
		return nil, e
	}
	return msa.Script()
}

// ltcChainService wraps *spv.ChainService in order to translate the
// spv.ServerPeer to the btc.SPVPeer interface type, and to convert GetBlock's
// return type from *bisonwire.BlockWithHeight to *btcutil.Block.
type ltcChainService struct {
	*spv.ChainService
}

var _ btc.SPVService = (*ltcChainService)(nil)

func (s *ltcChainService) BestBlock() (*headerfs.BlockStamp, error) {
	return s.ChainService.BestBlock()
}

func (s *ltcChainService) Peers() []btc.SPVPeer {
	rawPeers := s.ChainService.Peers()
	peers := make([]btc.SPVPeer, 0, len(rawPeers))
	for _, p := range rawPeers {
		peers = append(peers, p)
	}
	return peers
}

func (s *ltcChainService) AddPeer(addr string) error {
	return s.ChainService.ConnectNode(addr, true)
}

func (s *ltcChainService) RemovePeer(addr string) error {
	return s.ChainService.RemoveNodeByAddr(addr)
}

// GetBlock wraps spv.ChainService.GetBlock, converting the returned
// *bisonwire.BlockWithHeight to a *btcutil.Block as expected by btc.SPVService.
func (s *ltcChainService) GetBlock(blockHash chainhash.Hash, options ...spv.QueryOption) (*btcutil.Block, error) {
	bwh, err := s.ChainService.GetBlock(blockHash, options...)
	if err != nil {
		return nil, err
	}
	msgBlock := bwh.Block.MsgBlock()
	return btcutil.NewBlock(msgBlock), nil
}

// GetCFilter satisfies the btc.SPVService interface.
func (s *ltcChainService) GetCFilter(blockHash chainhash.Hash, filterType wire.FilterType, options ...spv.QueryOption) (*gcs.Filter, error) {
	return s.ChainService.GetCFilter(blockHash, filterType, options...)
}

func extendAddresses(extIdx, intIdx uint32, ltcw *wallet.Wallet) error {
	scopedKeyManager, err := ltcw.Manager.FetchScopedKeyManager(ltcw.ScopeForPurpose(84))
	if err != nil {
		return err
	}

	return walletdb.Update(ltcw.Database(), func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(waddrmgrNamespace)
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

var (
	loggingInited uint32
	logFileName   = "neutrino.log"
)

// logNeutrino initializes logging in the neutrino + wallet packages.
func logNeutrino(walletDir string) error {
	if !atomic.CompareAndSwapUint32(&loggingInited, 0, 1) {
		return nil
	}

	logDir := filepath.Join(walletDir, logDirName)
	logSpinner, err := logRotator(logDir)
	if err != nil {
		return fmt.Errorf("error initializing log rotator: %w", err)
	}

	backendLog := btclog.NewBackend(logSpinner)
	logger := func(name string, lvl btclog.Level) btclog.Logger {
		l := backendLog.Logger(name)
		l.SetLevel(lvl)
		return l
	}

	spv.UseLogger(logger("NTRNO", btclog.LevelDebug))
	wallet.UseLogger(logger("LTCW", btclog.LevelInfo))
	wtxmgr.UseLogger(logger("TXMGR", btclog.LevelInfo))
	chain.UseLogger(logger("CHAIN", btclog.LevelInfo))

	return nil
}

// logRotator initializes a rotating file logger.
func logRotator(dir string) (*rotator.Rotator, error) {
	const maxLogRolls = 8
	if err := os.MkdirAll(dir, 0744); err != nil {
		return nil, fmt.Errorf("error creating log directory: %w", err)
	}

	logFilename := filepath.Join(dir, logFileName)
	return rotator.New(logFilename, 32*1024, false, maxLogRolls)
}

// walletExists checks if a wallet exists at the given path.
func walletExists(dir string, btcParams *chaincfg.Params) (bool, error) {
	loader := wallet.NewLoader(ltcNetParams(btcParams), dir, true, dbTimeout, 250)
	return loader.WalletExists()
}

// bisonwire type needed for TxRecord construction in tests.
var _ = bisonwire.Tx{}
