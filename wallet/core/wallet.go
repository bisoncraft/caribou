// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/encrypt"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

var (
	errWalletNotConnected     = errors.New("wallet not connected")
	errConnectRacedDisconnect = errors.New("connect raced a concurrent disconnect")
)

// runWithTimeout runs the provided function, returning either the error from
// the function or errTimeout if the function fails to return within the
// timeout. This function is for wallet methods that may not have a context or
// timeout of their own, or we simply cannot rely on third party packages to
// respect context cancellation or deadlines.
func runWithTimeout(f func() error, timeout time.Duration) error {
	errChan := make(chan error, 1)
	go func() {
		defer close(errChan)
		errChan <- f()
	}()

	select {
	case err := <-errChan:
		return err
	case <-time.After(timeout):
		return errTimeout
	}
}

// xcWallet is a wallet. Use (*Core).loadWallet to construct a xcWallet.
type xcWallet struct {
	asset.Wallet
	log        util.Logger
	connector  *util.ConnectionMaster
	AssetID    uint32
	Symbol     string
	walletType string
	traits     asset.WalletTrait
	network    *xcWallet
	connectMtx sync.Mutex

	// monitored is nonzero while a startWalletSyncMonitor goroutine is watching
	// sync status. It is accessed atomically rather than under mtx.
	monitored uint32

	// suspended is nonzero from a suspend until the next Connect, so the sync
	// monitor can tell a background pause from an unexpected shutdown when its
	// connector context is cancelled. Atomic rather than under mtx, and read
	// from here rather than from Core.suspended: Core.Suspend holds suspendMtx
	// while connector.Disconnect cancels the context that wakes the monitor, so
	// having the monitor reach for suspendMtx would add a lock-ordering edge for
	// the sake of a log line.
	suspended uint32

	// connGen is bumped by every Disconnect call. Connect snapshots it before
	// starting the connector and self-disconnects if it changed by the time the
	// connect finishes. Disconnect cannot block on connectMtx — Logout must not
	// stall behind an in-flight Connect from any caller, not just Resume, which
	// already has its own suspendGen-scoped version of this same guard (see
	// Core.unwindStaleResume) — so a Disconnect that lands mid-connect is a
	// no-op against ConnectionMaster (documented: On() is false until the
	// connect finishes) and has nothing live to cancel. Without this, that
	// connect could still succeed afterward and leave the wallet connected past
	// the Disconnect that meant to end it.
	connGen atomic.Uint64

	mtx        sync.RWMutex
	encPass    []byte // empty means wallet not password protected
	balance    *WalletBalance
	pw         encode.PassBytes
	address    string
	peerCount  int32 // -1 means no count yet
	hookedUp   bool
	syncStatus *asset.SyncStatus
	disabled   bool

	pendingTxsMtx sync.RWMutex
	pendingTxs    map[string]*txdb.WalletTransaction

	// When wallets are being reconfigured and especially when the wallet type
	// or host is being changed, we want to suppress "walletstate" notes to
	// prevent subscribers from prematurely adopting the new WalletState before
	// the new wallet is fully validated and added to the Core wallets map.
	// WalletState notes during reconfiguration can come from the sync loop or
	// from the PeersChange callback.
	broadcasting *uint32
}

// encPW returns xcWallet's encrypted password.
func (w *xcWallet) encPW() []byte {
	w.mtx.RLock()
	defer w.mtx.RUnlock()
	return w.encPass
}

// setEncPW sets xcWallet's encrypted password.
func (w *xcWallet) setEncPW(encPW []byte) {
	w.mtx.Lock()
	w.encPass = encPW
	w.mtx.Unlock()
}

// Unlock unlocks the wallet backend and caches the decrypted wallet password so
// the wallet may be unlocked without user interaction using refreshUnlock.
func (w *xcWallet) Unlock(crypter encrypt.Crypter) error {
	if w.isDisabled() { // cannot unlock disabled wallet.
		return fmt.Errorf(walletDisabledErrStr, strings.ToUpper(unbip(w.AssetID)))
	}
	if w.network != nil {
		return w.network.Unlock(crypter)
	}
	if !w.connected() {
		return errWalletNotConnected
	}

	a, is := w.Wallet.(asset.Authenticator)
	if !is {
		return nil
	}

	if len(w.encPW()) == 0 {
		if a.Locked() {
			return fmt.Errorf("wallet reporting as locked, but no password has been set")
		}
		return nil
	}
	pw, err := crypter.Decrypt(w.encPW())
	if err != nil {
		return fmt.Errorf("%s unlockWallet decryption error: %w", unbip(w.AssetID), err)
	}

	err = a.Unlock(pw) // can be slow - no timeout and NOT in the critical section!
	if err != nil {
		return err
	}
	w.mtx.Lock()
	w.pw = pw
	w.mtx.Unlock()
	return nil
}

// refreshUnlock is used to ensure the wallet is unlocked. If the wallet backend
// reports as already unlocked, which includes a wallet with no password
// protection, no further action is taken and a nil error is returned. If the
// wallet is reporting as locked, and the wallet is not known to be password
// protected (no encPW set) or the decrypted password is not cached, a non-nil
// error is returned. If no encrypted password is set, the xcWallet is
// misconfigured and should be recreated. If the decrypted password is not
// stored, the Unlock method should be used to decrypt the password. Finally, a
// non-nil error will be returned if the cached password fails to unlock the
// wallet, in which case unlockAttempted will also be true.
func (w *xcWallet) refreshUnlock() (unlockAttempted bool, err error) {
	if w.isDisabled() { // disabled wallet cannot be unlocked.
		return false, fmt.Errorf(walletDisabledErrStr, strings.ToUpper(unbip(w.AssetID)))
	}
	if w.network != nil {
		return w.network.refreshUnlock()
	}
	if !w.connected() {
		return false, errWalletNotConnected
	}

	a, is := w.Wallet.(asset.Authenticator)
	if !is {
		return false, nil
	}

	// Check if the wallet backend is already unlocked.
	if !a.Locked() {
		return false, nil // unlocked
	}

	// Locked backend requires both encrypted and decrypted passwords. Read pw
	// under the same RLock rather than after releasing it: Lock clears it
	// (Clear zeroes the shared backing array in place, then nils the field)
	// under the write lock, so a read outside the lock can race that clear
	// and hand the backend a partially-zeroed password.
	w.mtx.RLock()
	pwUnset := len(w.encPass) == 0
	pw := w.pw
	locked := len(pw) == 0
	w.mtx.RUnlock()
	if pwUnset {
		return false, fmt.Errorf("%s wallet reporting as locked but no password"+
			" has been set", unbip(w.AssetID))
	}
	if locked {
		return false, fmt.Errorf("cannot refresh unlock on a locked %s wallet",
			unbip(w.AssetID))
	}

	return true, a.Unlock(pw)
}

// Lock the wallet. For encrypted wallets (encPW set), this clears the cached
// decrypted password and attempts to lock the wallet backend.
func (w *xcWallet) Lock(timeout time.Duration) error {
	a, is := w.Wallet.(asset.Authenticator)
	if w.isDisabled() || !is { // wallet is disabled and is locked or it's not an authenticator.
		return nil
	}
	w.mtx.RLock()
	network, hookedUp, pwUnset := w.network, w.hookedUp, len(w.encPass) == 0
	w.mtx.RUnlock()

	// The delegation and the backend lock both happen with mtx released. The
	// backend lock is an RPC that runs for up to timeout (callers pass
	// multi-second values), and network.Lock recurses into another xcWallet's
	// Lock, so holding mtx across either would stall every state read and
	// balance update on this wallet for the duration.
	if network != nil {
		return network.Lock(timeout)
	}
	if !hookedUp {
		return errWalletNotConnected
	}
	if pwUnset {
		return nil
	}
	err := runWithTimeout(a.Lock, timeout)
	if err != nil {
		return err
	}
	// Clear only after the RPC returns, and under the write lock, so a
	// concurrent refreshUnlock's RLock-guarded read of w.pw (see refreshUnlock)
	// cannot interleave with the field being nilled out here. refreshUnlock
	// copies the slice header before releasing its RLock, not the backing
	// array, so Clear zeroing that array concurrently with a still-in-flight
	// a.Unlock(pw) call remains a narrow, pre-existing exposure this does not
	// close.
	//
	// Because the backend RPC and this clear are no longer one critical
	// section, a concurrent Unlock that finishes its own backend RPC and
	// caches w.pw between this Lock's RPC and this point will have its cache
	// wiped here: backend ends up unlocked with no cached password. Concurrent
	// Lock/Unlock on one wallet is a caller-level race either way; this is a
	// real (if narrow) behavior change from when the write lock spanned the
	// backend RPC.
	w.mtx.Lock()
	w.pw.Clear()
	w.pw = nil
	w.mtx.Unlock()
	return nil
}

// clearCachedPW zeroes and drops the cached decrypted wallet password without
// touching the wallet backend. Lock is the normal way to shed this, but it
// requires a live connection (the backend lock is an RPC on most assets), so a
// wallet that was suspended for a mobile background pause cannot be locked at
// logout. The cached password is process-local, though, and evicting it is the
// whole point of the lock tier, so clear it regardless of connection state.
//
// Unlike Lock, this does not delegate to w.network for a token wallet. It does
// not need to: the only caller, Logout, walks every wallet, so the network
// wallet clears its own cached password on its own pass.
func (w *xcWallet) clearCachedPW() {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	w.pw.Clear()
	w.pw = nil
}

// unlocked will only return true if both the wallet backend is unlocked and we
// have cached the decrypted wallet password. The wallet backend may be queried
// directly, likely involving an RPC call. Use locallyUnlocked to determine if
// the wallet is automatically unlockable rather than actually unlocked.
func (w *xcWallet) unlocked() bool {
	if w.isDisabled() {
		return false
	}
	a, is := w.Wallet.(asset.Authenticator)
	if !is {
		return w.locallyUnlocked()
	}
	if w.network != nil {
		return w.network.unlocked()
	}
	if !w.connected() {
		return false
	}
	return w.locallyUnlocked() && !a.Locked()
}

// locallyUnlocked checks whether we think the wallet is unlocked, but without
// asking the wallet itself. More precisely, for encrypted wallets (encPW set)
// this is true only if the decrypted password is cached. Use this to determine
// if the wallet may be unlocked without user interaction (via refreshUnlock).
func (w *xcWallet) locallyUnlocked() bool {
	if w.isDisabled() {
		return false
	}
	if w.network != nil {
		return w.network.locallyUnlocked()
	}
	w.mtx.RLock()
	defer w.mtx.RUnlock()
	if len(w.encPass) == 0 {
		return true // unencrypted wallet
	}
	return len(w.pw) > 0 // cached password for encrypted wallet
}

// state returns the current WalletState.
func (w *xcWallet) state() *WalletState {
	winfo := w.Info()

	w.mtx.RLock()
	var peerCount uint32
	if w.peerCount > 0 { // initialized to -1 initially, means no count yet
		peerCount = uint32(w.peerCount)
	}

	// Until the first balance fetch, the wallet holds a WalletBalance shell
	// around a nil db.Balance, which would serialize as an object with every
	// balance field absent. Report no balance instead; clients handle null.
	balance := w.balance
	if balance != nil && balance.Balance == nil {
		balance = nil
	}

	state := &WalletState{
		Symbol:       unbip(w.AssetID),
		AssetID:      w.AssetID,
		Open:         len(w.encPass) == 0 || len(w.pw) > 0,
		Running:      w.connector.On(),
		Balance:      balance,
		Address:      w.address,
		Encrypted:    len(w.encPass) > 0,
		PeerCount:    peerCount,
		Synced:       w.syncStatus.Synced,
		SyncProgress: w.syncStatus.BlockProgress(),
		SyncStatus:   w.syncStatus,
		WalletType:   w.walletType,
		Class:        winfo.BlockchainClass,
		Traits:       w.traits,
		Disabled:     w.disabled,
		PendingTxs:   w.pendingTxsCopy(),
	}
	w.mtx.RUnlock()

	if w.network != nil {
		w.network.mtx.RLock()
		state.Open = len(w.network.encPass) == 0 || len(w.network.pw) > 0
		w.network.mtx.RUnlock()
	}

	// Both reads are cached in the wallet; neither touches the network.
	if establisher, ok := w.Wallet.(asset.TrustlineEstablisher); ok {
		state.Trustline = establisher.TrustlineStatus()
	}
	if depositer, ok := w.Wallet.(asset.MinimumDepositer); ok {
		state.MinDeposit = depositer.MinimumDeposit()
	}

	return state
}

// setBalance sets the wallet balance.
func (w *xcWallet) setBalance(bal *WalletBalance) {
	w.mtx.Lock()
	w.balance = bal
	w.mtx.Unlock()
}

// setDisabled sets the wallet disabled field.
func (w *xcWallet) setDisabled(status bool) {
	w.mtx.Lock()
	w.disabled = status
	w.mtx.Unlock()
}

func (w *xcWallet) isDisabled() bool {
	w.mtx.RLock()
	defer w.mtx.RUnlock()
	return w.disabled
}

func (w *xcWallet) currentDepositAddress() string {
	w.mtx.RLock()
	defer w.mtx.RUnlock()
	return w.address
}

func (w *xcWallet) refreshDepositAddress() (string, error) {
	if !w.connected() {
		return "", fmt.Errorf("cannot get address from unconnected %s wallet",
			unbip(w.AssetID))
	}

	na, is := w.Wallet.(asset.NewAddresser)
	if !is {
		return "", fmt.Errorf("wallet does not generate new addresses")
	}

	addr, err := na.NewAddress()
	if err != nil {
		return "", fmt.Errorf("%s Wallet.Address error: %w", unbip(w.AssetID), err)
	}

	w.mtx.Lock()
	w.address = addr
	w.mtx.Unlock()

	return addr, nil
}

// connected is true if the wallet has already been connected.
func (w *xcWallet) connected() bool {
	w.mtx.RLock()
	defer w.mtx.RUnlock()
	return w.hookedUp
}

// checkPeersAndSyncStatus checks that the wallet is synced, and has peers
// otherwise we might double spend if the wallet keys were used elsewhere. This
// should be checked before attempting to send funds but does not replace any
// other checks that may be required.
func (w *xcWallet) checkPeersAndSyncStatus() error {
	w.mtx.RLock()
	defer w.mtx.RUnlock()
	if w.peerCount < 1 {
		return fmt.Errorf("%s wallet has no connected peers", unbip(w.AssetID))
	}
	if !w.syncStatus.Synced {
		return fmt.Errorf("%s wallet is not synchronized", unbip(w.AssetID))
	}
	return nil
}

// Connect calls the util.Connector's Connect method, sets the xcWallet.hookedUp
// flag to true, and validates the deposit address. Use Disconnect to cleanly
// shutdown the wallet.
func (w *xcWallet) Connect() error {
	w.connectMtx.Lock()
	defer w.connectMtx.Unlock()

	// Disabled wallet cannot be connected to unless it is enabled.
	if w.isDisabled() {
		return fmt.Errorf(walletDisabledErrStr, strings.ToUpper(unbip(w.AssetID)))
	}

	if w.connected() {
		return nil
	}

	// A fresh connection ends any prior suspend, whether or not this is the
	// Resume that follows one.
	atomic.StoreUint32(&w.suspended, 0)

	gen := w.connGen.Load()

	// No parent context; use Disconnect instead. Also note that there's no
	// reconnect loop for wallet like with the server Connectors, so we use
	// ConnectOnce so that the ConnectionMaster's On method will report false.
	err := w.connector.ConnectOnce(context.Background())
	if err != nil {
		return fmt.Errorf("ConnectOnce error: %w", err)
	}

	var ready bool
	defer func() {
		// Now that we are connected, we must Disconnect if any calls fail below
		// since we are considering this wallet not "hookedUp".
		if !ready {
			w.connector.Disconnect()
		}
	}()

	ss, err := w.SyncStatus()
	if err != nil {
		return fmt.Errorf("SyncStatus error: %w", err)
	}

	w.mtx.Lock()
	defer w.mtx.Unlock()
	haveAddress := w.address != ""
	if haveAddress {
		if haveAddress, err = w.OwnsDepositAddress(w.address); err != nil {
			return fmt.Errorf("OwnsDepositAddress error: %w", err)
		}
	}
	if !haveAddress {
		if w.address, err = w.DepositAddress(); err != nil {
			return fmt.Errorf("DepositAddress error: %w", err)
		}
	}
	// A Disconnect that landed while this connect was underway found nothing
	// live to cancel and moved on assuming the wallet stayed down (see
	// connGen). Undo here via the ready-guarded defer above instead of leaving
	// the wallet connected past whatever wanted it stopped.
	if w.connGen.Load() != gen {
		return errConnectRacedDisconnect
	}
	w.hookedUp = true
	w.syncStatus = ss
	ready = true

	return nil
}

// Disconnect calls the util.Connector's Disconnect method and sets the
// xcWallet.hookedUp flag to false. Will also close the wallet if it is an
// asset.Opener and open.
func (w *xcWallet) Disconnect() {
	// Disabled wallet is already disconnected.
	if w.isDisabled() {
		return
	}
	w.connGen.Add(1)
	w.connector.Disconnect()
	w.mtx.Lock()
	w.hookedUp = false
	w.mtx.Unlock()
	opener, is := w.Wallet.(asset.Opener)
	if !is {
		return
	}

	if !opener.IsOpen() {
		return
	}

	if err := opener.Close(); err != nil {
		w.log.Errorf("Unable to close wallet: %v", err)
	}
}

// suspend drops the wallet's network connection the way Disconnect does, but
// deliberately leaves an asset.Opener wallet open so it can be reconnected later
// without the app password. It is the connection half of a mobile background
// pause: connector.Disconnect cancels the wallet's context, stopping its tickers
// and closing its sockets, while the wallet handle (and for xmr the open wallet2
// instance) stays resident. Returns whether the wallet was connected, so the
// caller knows to reconnect it on resume. xmr is the only asset.Opener, so for
// every other asset this is exactly Disconnect minus the Close it would skip
// anyway. Takes connectMtx to serialize against Connect.
func (w *xcWallet) suspend() bool {
	w.connectMtx.Lock()
	defer w.connectMtx.Unlock()

	if w.isDisabled() || !w.connected() {
		return false
	}
	// Set before the Disconnect that wakes the sync monitor, so the monitor
	// sees it.
	atomic.StoreUint32(&w.suspended, 1)
	w.connector.Disconnect()
	w.mtx.Lock()
	w.hookedUp = false
	w.mtx.Unlock()
	return true
}

// rescan will initiate a rescan of the wallet if the asset.Wallet
// implementation is a Rescanner.
func (w *xcWallet) rescan(ctx context.Context, bday /* unix time seconds*/ uint64) error {
	if !w.connected() {
		return errWalletNotConnected
	}
	rescanner, ok := w.Wallet.(asset.Rescanner)
	if !ok {
		return errors.New("wallet does not support rescanning")
	}
	return rescanner.Rescan(ctx, bday)
}

// logFilePath returns the path of the wallet's log file if the
// asset.Wallet implementation is a LogFiler.
func (w *xcWallet) logFilePath() (string, error) {
	logFiler, ok := w.Wallet.(asset.LogFiler)
	if !ok {
		return "", errors.New("wallet does not support getting log file")
	}
	return logFiler.LogFilePath(), nil
}

// TxHistory returns all the transactions a wallet has made. If refID
// is nil, then transactions starting from the most recent are returned
// (past is ignored). If past is true, the transactions prior to the
// refID are returned, otherwise the transactions after the refID are
// returned. n is the number of transactions to return. If n is <= 0,
// all the transactions will be returned.
func (w *xcWallet) TxHistory(req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	if !w.connected() {
		return nil, errWalletNotConnected
	}

	return w.Wallet.TxHistory(req)
}

// WalletTransaction returns information about a transaction that the wallet
// has made or one in which that wallet received funds.
func (w *xcWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	if !w.connected() {
		return nil, errWalletNotConnected
	}

	return w.Wallet.WalletTransaction(ctx, txID)
}

func (w *xcWallet) processWalletTransactions(txs []*txdb.WalletTransaction) {
	w.pendingTxsMtx.Lock()
	defer w.pendingTxsMtx.Unlock()
	for _, tx := range txs {
		if tx.Confirmed {
			if pendingID := tx.AdditionalData["pendingID"]; pendingID != "" {
				delete(w.pendingTxs, pendingID)
			}
			delete(w.pendingTxs, tx.ID)
		} else {
			w.pendingTxs[tx.ID] = tx
		}
	}
}

func (w *xcWallet) pendingTxsCopy() map[string]*txdb.WalletTransaction {
	w.pendingTxsMtx.RLock()
	defer w.pendingTxsMtx.RUnlock()
	return util.CopyMap(w.pendingTxs)
}

// SendTransaction broadcasts a raw transaction if the wallet is a Broadcaster.
func (w *xcWallet) SendTransaction(tx []byte) ([]byte, error) {
	broadcaster, ok := w.Wallet.(asset.Broadcaster)
	if !ok {
		return nil, errors.New("wallet is not a Broadcaster")
	}
	return broadcaster.SendTransaction(tx)
}

// usableFeeRate returns the wallet's live fee-rate estimate. It is zero when no
// estimate is available, in which case fee-dependent operations like sends must
// error rather than proceed with a made-up rate.
func (w *xcWallet) usableFeeRate() uint64 {
	return w.FeeRate()
}

// BridgeContractApprovalStatus returns the approval status of the bridge
// contract if the wallet is a Bridger.
func (w *xcWallet) BridgeContractApprovalStatus(ctx context.Context, bridgeName string) (asset.ApprovalStatus, error) {
	approver, ok := w.Wallet.(asset.Bridger)
	if !ok {
		return 0, fmt.Errorf("%s wallet is not a Bridger", unbip(w.AssetID))
	}
	return approver.BridgeContractApprovalStatus(ctx, bridgeName)
}

// ApproveBridgeContract approves the bridge contract if the wallet is a
// Bridger.
func (w *xcWallet) ApproveBridgeContract(ctx context.Context, bridgeName string, onConfirm func()) (string, error) {
	approver, ok := w.Wallet.(asset.Bridger)
	if !ok {
		return "", fmt.Errorf("%s wallet is not a Bridger", unbip(w.AssetID))
	}
	return approver.ApproveBridgeContract(ctx, bridgeName, onConfirm)
}

// UnapproveBridgeContract removes approval of the bridge contract if the
// wallet is a Bridger.
func (w *xcWallet) UnapproveBridgeContract(ctx context.Context, bridgeName string, onConfirm func()) (string, error) {
	approver, ok := w.Wallet.(asset.Bridger)
	if !ok {
		return "", fmt.Errorf("%s wallet is not a Bridger", unbip(w.AssetID))
	}
	return approver.UnapproveBridgeContract(ctx, bridgeName, onConfirm)
}

// InitiateBridge initiates the bridge on the source chain if the wallet is a
// Bridger.
func (w *xcWallet) InitiateBridge(ctx context.Context, amt uint64, dest uint32, bridgeName string) (txID string, err error) {
	bridger, ok := w.Wallet.(asset.Bridger)
	if !ok {
		return "", fmt.Errorf("%s wallet is not a Bridger", unbip(w.AssetID))
	}
	return bridger.InitiateBridge(ctx, amt, dest, bridgeName)
}

// CompleteBridge completes the bridge on the destination chain if the wallet
// is a Bridger.
func (w *xcWallet) CompleteBridge(ctx context.Context, bridgeTx *txdb.BridgeCounterpartTx, amount uint64, mintData []byte, bridgeName string) error {
	bridger, ok := w.Wallet.(asset.Bridger)
	if !ok {
		return fmt.Errorf("%s wallet is not a Bridger", unbip(w.AssetID))
	}

	return bridger.CompleteBridge(ctx, bridgeTx, amount, mintData, bridgeName)
}

// MarkBridgeComplete marks the bridge as complete on the source chain if the
// wallet is a Bridger.
func (w *xcWallet) MarkBridgeComplete(initiationTxID string, completionTxIDs []string, amtReceived, fees uint64, complete bool) {
	bridger, ok := w.Wallet.(asset.Bridger)
	if !ok {
		w.log.Errorf("MarkBridgeComplete: %s wallet is not a Bridger", unbip(w.AssetID))
		return
	}

	bridger.MarkBridgeComplete(initiationTxID, completionTxIDs, amtReceived, fees, complete)
}

// PendingBridges returns the pending bridges originating on the chain of the
// given asset ID.
func (w *xcWallet) PendingBridges() ([]*txdb.WalletTransaction, error) {
	if !w.connected() {
		return nil, errWalletNotConnected
	}

	bridger, ok := w.Wallet.(asset.Bridger)
	if !ok {
		return nil, fmt.Errorf("wallet does not support bridging")
	}

	return bridger.PendingBridges()
}

// BridgeHistory returns the bridge history originating on the chain of the
// given asset ID.
func (w *xcWallet) BridgeHistory(n int, refID *string, past bool) ([]*txdb.WalletTransaction, error) {
	if !w.connected() {
		return nil, errWalletNotConnected
	}

	bridger, ok := w.Wallet.(asset.Bridger)
	if !ok {
		return nil, fmt.Errorf("wallet does not support bridging")
	}

	return bridger.BridgeHistory(n, refID, past)
}

// OpenWithPW opens a wallet for use if it is an asset.Opener. Otherwise does
// nothing.
func (w *xcWallet) OpenWithPW(ctx context.Context, crypter encrypt.Crypter) error {
	opener, ok := w.Wallet.(asset.Opener)
	if !ok {
		return nil
	}

	if opener.IsOpen() {
		return nil
	}

	if w.isDisabled() {
		return fmt.Errorf(walletDisabledErrStr, strings.ToUpper(unbip(w.AssetID)))
	}

	if crypter == nil {
		return newError(noAuthError, "no password provided for open")
	}

	if len(w.encPW()) == 0 {
		return fmt.Errorf("no password has been set for open")
	}

	pw, err := crypter.Decrypt(w.encPW())
	if err != nil {
		return fmt.Errorf("%s open wallet decryption error: %w", unbip(w.AssetID), err)
	}

	return opener.OpenWithPW(ctx, pw)
}
