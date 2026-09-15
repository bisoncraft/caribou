//go:build !harness && !botlive

package core

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/encrypt"
	"github.com/bisoncraft/caribou/util/wait"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/db"
	dbtest "github.com/bisoncraft/caribou/wallet/db/test"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

func init() {
	asset.Register(tUTXOAssetA.ID, &tDriver{
		decodedCoinID: tUTXOAssetA.Symbol + "-coin",
		winfo:         tWalletInfo,
	}, true)
	asset.Register(tUTXOAssetB.ID, &tCreator{
		tDriver: &tDriver{
			decodedCoinID: tUTXOAssetB.Symbol + "-coin",
			winfo:         tWalletInfo,
		},
	}, true)
	asset.Register(tACCTAsset.ID, &tCreator{
		tDriver: &tDriver{
			decodedCoinID: tACCTAsset.Symbol + "-coin",
			winfo:         tWalletInfo,
		},
	}, true)
}

var (
	tCtx        context.Context
	tUTXOAssetA = &util.Asset{
		ID:         42,
		Symbol:     "dcr",
		Version:    0, // match the stubbed (*TXCWallet).Info result
		MaxFeeRate: 10,
		SwapConf:   1,
	}

	tUTXOAssetB = &util.Asset{
		ID:         0,
		Symbol:     "btc",
		Version:    0, // match the stubbed (*TXCWallet).Info result
		MaxFeeRate: 2,
		SwapConf:   1,
	}

	tACCTAsset = &util.Asset{
		ID:         60,
		Symbol:     "eth",
		Version:    0, // match the stubbed (*TXCWallet).Info result
		MaxFeeRate: 20,
		SwapConf:   1,
	}
	tPW         = []byte("dexpw")
	wPW         = []byte("walletpw")
	errTest     = fmt.Errorf("test error")
	tLogger     = util.StdOutLogger("TCORE", util.LevelInfo)
	tWalletInfo = &asset.WalletInfo{
		BlockchainClass: asset.BlockchainClassUTXO,
		UnitInfo: util.UnitInfo{
			Conventional: util.Denomination{
				ConversionFactor: 1e8,
			},
		},
		AvailableWallets: []*asset.WalletDefinition{{
			Type: "type",
		}},
	}
)

type TDB struct {
	updateWalletErr error
	wallet          *db.Wallet
	walletErr       error
	setWalletPwErr  error
	creds           *db.PrimaryCredentials
	setCredsErr     error
	// savedDisabledRateSources records the last SaveDisabledRateSources
	// argument. nil means the method was never called.
	savedDisabledRateSources []string

	// intentsSwaps is a live in-memory implementation of the IntentsSwap
	// methods, keyed by deposit address, so the swap engine's
	// read-modify-write paths can run against the stub.
	intentsMtx   sync.Mutex
	intentsSwaps map[string]*db.IntentsSwap
}

func (tdb *TDB) Connect(context.Context) (*sync.WaitGroup, error) {
	return &sync.WaitGroup{}, nil
}

func (tdb *TDB) Close() error { return nil }

func (tdb *TDB) UpdateWallet(wallet *db.Wallet) error {
	tdb.wallet = wallet
	return tdb.updateWalletErr
}

func (tdb *TDB) SetWalletPassword(assetID uint32, newPW []byte) error {
	return tdb.setWalletPwErr
}

func (tdb *TDB) SetWalletAddress(assetID uint32, addr string) error {
	return nil
}

func (tdb *TDB) UpdateBalance(assetID uint32, balance *db.Balance) error {
	return nil
}

func (tdb *TDB) UpdateWalletStatus(assetID uint32, disable bool) error {
	return nil
}

func (tdb *TDB) Wallets() ([]*db.Wallet, error) {
	return nil, nil
}

func (tdb *TDB) Wallet(uint32) (*db.Wallet, error) {
	return tdb.wallet, tdb.walletErr
}

func (tdb *TDB) SaveNotification(*db.Notification) error        { return nil }
func (tdb *TDB) NotificationsN(int) ([]*db.Notification, error) { return nil, nil }
func (tdb *TDB) SavePokes([]*db.Notification) error             { return nil }
func (tdb *TDB) LoadPokes() ([]*db.Notification, error)         { return nil, nil }

func (tdb *TDB) SetPrimaryCredentials(creds *db.PrimaryCredentials) error {
	if tdb.setCredsErr != nil {
		return tdb.setCredsErr
	}
	tdb.creds = creds
	return nil
}

func (tdb *TDB) PrimaryCredentials() (*db.PrimaryCredentials, error) {
	return tdb.creds, nil
}
func (tdb *TDB) DisabledRateSources() ([]string, error) {
	return nil, nil
}
func (tdb *TDB) SaveDisabledRateSources(disableSources []string) error {
	tdb.savedDisabledRateSources = disableSources
	return nil
}
func (tdb *TDB) AckNotification(id []byte) error { return nil }

func (tdb *TDB) SetLanguage(lang string) error {
	return nil
}
func (tdb *TDB) Language() (string, error) {
	return "en-US", nil
}
func (tdb *TDB) SetCompanionToken(token string) error {
	return nil
}
func (tdb *TDB) CompanionToken() (string, error) {
	return "", nil
}
func (tdb *TDB) SetAPIToken(token string) error {
	return nil
}
func (tdb *TDB) APIToken() (string, error) {
	return "", nil
}
func (tdb *TDB) SetSendRequiresPassword(require bool) error {
	return nil
}
func (tdb *TDB) SendRequiresPassword() (bool, error) {
	return false, nil
}

func (tdb *TDB) NextMultisigKeyIndex(assetID uint32) (uint32, error) {
	return 0, nil
}

func (tdb *TDB) StoreMultisigIndexForPubkey(assetID, idx uint32, pubkey [33]byte) error {
	return nil
}

func (tdb *TDB) MultisigIndexForPubkey(assetID uint32, pubkey [33]byte) (uint32, error) {
	return 0, nil
}

func (tdb *TDB) StoreIntentsSwap(swap *db.IntentsSwap) error {
	tdb.intentsMtx.Lock()
	defer tdb.intentsMtx.Unlock()
	if tdb.intentsSwaps == nil {
		tdb.intentsSwaps = make(map[string]*db.IntentsSwap)
	}
	cp := *swap
	tdb.intentsSwaps[swap.DepositAddress] = &cp
	return nil
}

func (tdb *TDB) IntentsSwap(depositAddress string) (*db.IntentsSwap, error) {
	tdb.intentsMtx.Lock()
	defer tdb.intentsMtx.Unlock()
	if swap, found := tdb.intentsSwaps[depositAddress]; found {
		cp := *swap
		return &cp, nil
	}
	return nil, db.ErrIntentsSwapNotFound
}

func (tdb *TDB) IntentsSwaps() ([]*db.IntentsSwap, error) {
	tdb.intentsMtx.Lock()
	defer tdb.intentsMtx.Unlock()
	swaps := make([]*db.IntentsSwap, 0, len(tdb.intentsSwaps))
	for _, swap := range tdb.intentsSwaps {
		cp := *swap
		swaps = append(swaps, &cp)
	}
	return swaps, nil
}

func (tdb *TDB) ActiveIntentsSwaps() ([]*db.IntentsSwap, error) {
	tdb.intentsMtx.Lock()
	defer tdb.intentsMtx.Unlock()
	var swaps []*db.IntentsSwap
	for _, swap := range tdb.intentsSwaps {
		if swap.Terminal() {
			continue
		}
		cp := *swap
		swaps = append(swaps, &cp)
	}
	return swaps, nil
}

type tCoin struct {
	id []byte

	val uint64
}

func (c *tCoin) ID() util.Bytes {
	return c.id
}

func (c *tCoin) TxID() string {
	return ""
}

func (c *tCoin) String() string {
	return hex.EncodeToString(c.id)
}

func (c *tCoin) Value() uint64 {
	return c.val
}

type TXCWallet struct {
	swapSize           uint64
	sendFeeSuggestion  uint64
	feeRate            uint64
	sendCoin           *tCoin
	sendErr            error
	addrErr            error
	connectErr         error
	unlockErr          error
	balErr             error
	bal                *asset.Balance
	lockErr            error
	locked             bool
	changeCoin         *tCoin
	syncStatus         func() (bool, float32, error)
	ownsAddress        bool
	ownsAddressErr     error
	feeCoinSent        []byte
	sendTxnErr         error
	accelerationParams *struct {
		swapCoins                 []util.Bytes
		accelerationCoins         []util.Bytes
		changeCoin                util.Bytes
		feeSuggestion             uint64
		newFeeRate                uint64
		requiredForRemainingSwaps uint64
	}
	newAccelerationTxID         string
	newChangeCoinID             *util.Bytes
	preAccelerateSwapRate       uint64
	preAccelerateSuggestedRange asset.XYRange
	accelerationEstimate        uint64
	accelerateOrderErr          error
	info                        *asset.WalletInfo

	estFee    uint64
	estFeeErr error
	validAddr bool

	trustlineErr   error
	trustlineCalls int

	// connectHook, when non-nil, is called at the top of Connect. It lets a
	// test park a connection mid-flight. Set it before the Connect it should
	// catch, so the goroutine that runs Connect sees it published.
	connectHook func()

	// balanceHook, when non-nil, is called at the top of Balance. Unlike
	// connectHook it parks a reconnect after xcWallet.Connect has released
	// connectMtx, which is where connectAndUpdateWallet's own comments put the
	// stall a lifecycle call can interleave with: a Suspend serializes against a
	// wallet inside Connect, but not against one refreshing its balance.
	balanceHook func()

	// lockHook and unlockHook, when non-nil, are called at the top of Lock and
	// Unlock respectively. They let a test park a backend auth RPC mid-flight
	// to assert that no caller-side lock is held across it. Set them before the
	// call they should catch, so the goroutine making the call sees them
	// published.
	lockHook   func()
	unlockHook func()
}

var _ asset.Accelerator = (*TXCWallet)(nil)
var _ asset.Withdrawer = (*TXCWallet)(nil)
var _ asset.TrustlineEstablisher = (*TXCWallet)(nil)

func (w *TXCWallet) EstablishTrustline() error {
	w.trustlineCalls++
	return w.trustlineErr
}

func newTWallet(assetID uint32) (*xcWallet, *TXCWallet) {
	w := &TXCWallet{
		changeCoin:  &tCoin{id: encode.RandomBytes(36)},
		syncStatus:  func() (synced bool, progress float32, err error) { return true, 1, nil },
		ownsAddress: true,
		info: &asset.WalletInfo{
			BlockchainClass: asset.BlockchainClassUTXO,
		},
	}
	var broadcasting uint32 = 1
	xcWallet := &xcWallet{
		log:          tLogger,
		Wallet:       w,
		Symbol:       util.BipIDSymbol(assetID),
		connector:    util.NewConnectionMaster(w),
		AssetID:      assetID,
		walletType:   "type",
		hookedUp:     true,
		encPass:      []byte{0x01},
		peerCount:    1,
		syncStatus:   &asset.SyncStatus{Synced: true},
		pw:           tPW,
		traits:       asset.DetermineWalletTraits(w),
		broadcasting: &broadcasting,
	}

	return xcWallet, w
}

func (w *TXCWallet) Info() *asset.WalletInfo {
	return w.info
}

func (w *TXCWallet) OwnsDepositAddress(address string) (bool, error) {
	return w.ownsAddress, w.ownsAddressErr
}

func (w *TXCWallet) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	if w.connectHook != nil {
		w.connectHook()
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		<-ctx.Done()
		wg.Done()
	}()
	return &wg, w.connectErr
}

func (w *TXCWallet) Balance() (*asset.Balance, error) {
	if w.balanceHook != nil {
		w.balanceHook()
	}
	if w.balErr != nil {
		return nil, w.balErr
	}
	if w.bal == nil {
		w.bal = new(asset.Balance)
	}
	return w.bal, nil
}

func (w *TXCWallet) DepositAddress() (string, error) {
	return "", w.addrErr
}

func (w *TXCWallet) NewAddress() (string, error) {
	return "", w.addrErr
}

func (w *TXCWallet) AddressUsed(addr string) (bool, error) {
	return false, nil
}

func (w *TXCWallet) Unlock(pw []byte) error {
	if w.unlockHook != nil {
		w.unlockHook()
	}
	return w.unlockErr
}

func (w *TXCWallet) Lock() error {
	if w.lockHook != nil {
		w.lockHook()
	}
	return w.lockErr
}

func (w *TXCWallet) Locked() bool {
	return w.locked
}

func (w *TXCWallet) ConfirmTime(id util.Bytes, nConfs uint32) (time.Time, error) {
	return time.Time{}, nil
}

func (w *TXCWallet) Send(address string, value, feeSuggestion uint64) (asset.Coin, error) {
	w.sendFeeSuggestion = feeSuggestion
	w.sendCoin.val = value
	return w.sendCoin, w.sendErr
}

func (w *TXCWallet) SendTransaction(rawTx []byte) ([]byte, error) {
	return w.feeCoinSent, w.sendTxnErr
}

func (w *TXCWallet) Withdraw(address string, value, feeSuggestion uint64) (asset.Coin, error) {
	w.sendFeeSuggestion = feeSuggestion
	return w.sendCoin, w.sendErr
}

func (w *TXCWallet) ValidateAddress(address string) bool {
	return w.validAddr
}

func (w *TXCWallet) EstimateSendTxFee(address string, value, feeRate uint64, subtract, maxWithdraw bool) (fee uint64, isValidAddress bool, err error) {
	return w.estFee, true, w.estFeeErr
}

func (w *TXCWallet) SyncStatus() (*asset.SyncStatus, error) {
	synced, progress, err := w.syncStatus()
	if err != nil {
		return nil, err
	}
	blocks := uint64(math.Round(float64(progress) * 100))
	return &asset.SyncStatus{Synced: synced, TargetHeight: blocks, Blocks: blocks}, nil
}

func (w *TXCWallet) FeesForRemainingSwaps(n, feeRate uint64) uint64 {
	return n * feeRate * w.swapSize
}
func (w *TXCWallet) AccelerateOrder(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, newFeeRate uint64) (asset.Coin, string, error) {
	if w.accelerateOrderErr != nil {
		return nil, "", w.accelerateOrderErr
	}

	w.accelerationParams = &struct {
		swapCoins                 []util.Bytes
		accelerationCoins         []util.Bytes
		changeCoin                util.Bytes
		feeSuggestion             uint64
		newFeeRate                uint64
		requiredForRemainingSwaps uint64
	}{
		swapCoins:                 swapCoins,
		accelerationCoins:         accelerationCoins,
		changeCoin:                changeCoin,
		requiredForRemainingSwaps: requiredForRemainingSwaps,
		newFeeRate:                newFeeRate,
	}
	if w.newChangeCoinID != nil {
		return &tCoin{id: *w.newChangeCoinID}, w.newAccelerationTxID, nil
	}

	return nil, w.newAccelerationTxID, nil
}

func (w *TXCWallet) PreAccelerate(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, feeSuggestion uint64) (uint64, *asset.XYRange, *asset.EarlyAcceleration, error) {
	if w.accelerateOrderErr != nil {
		return 0, nil, nil, w.accelerateOrderErr
	}

	w.accelerationParams = &struct {
		swapCoins                 []util.Bytes
		accelerationCoins         []util.Bytes
		changeCoin                util.Bytes
		feeSuggestion             uint64
		newFeeRate                uint64
		requiredForRemainingSwaps uint64
	}{
		swapCoins:                 swapCoins,
		accelerationCoins:         accelerationCoins,
		changeCoin:                changeCoin,
		requiredForRemainingSwaps: requiredForRemainingSwaps,
		feeSuggestion:             feeSuggestion,
	}

	return w.preAccelerateSwapRate, &w.preAccelerateSuggestedRange, nil, nil
}

func (w *TXCWallet) StandardSendFee(uint64) uint64 { return 1 }

func (w *TXCWallet) AccelerationEstimate(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes, requiredForRemainingSwaps, newFeeRate uint64) (uint64, error) {
	if w.accelerateOrderErr != nil {
		return 0, w.accelerateOrderErr
	}

	w.accelerationParams = &struct {
		swapCoins                 []util.Bytes
		accelerationCoins         []util.Bytes
		changeCoin                util.Bytes
		feeSuggestion             uint64
		newFeeRate                uint64
		requiredForRemainingSwaps uint64
	}{
		swapCoins:                 swapCoins,
		accelerationCoins:         accelerationCoins,
		changeCoin:                changeCoin,
		requiredForRemainingSwaps: requiredForRemainingSwaps,
		newFeeRate:                newFeeRate,
	}

	return w.accelerationEstimate, nil
}

func (w *TXCWallet) TxHistory(*txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	return nil, nil
}
func (w *TXCWallet) WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error) {
	return nil, nil
}

func (w *TXCWallet) PendingTransactions(ctx context.Context) []*txdb.WalletTransaction {
	return nil
}

func (w *TXCWallet) FeeRate() uint64 {
	return w.feeRate
}

type TLiveReconfigurer struct {
	*TXCWallet
	restart     bool
	reconfigErr error
}

func (r *TLiveReconfigurer) Reconfigure(ctx context.Context, cfg *asset.WalletConfig, currentAddress string) (restartRequired bool, err error) {
	return r.restart, r.reconfigErr
}

type tCrypterSmart struct {
	params     []byte
	encryptErr error
	decryptErr error
	recryptErr error
}

func newTCrypterSmart() *tCrypterSmart {
	return &tCrypterSmart{
		params: encode.RandomBytes(5),
	}
}

// Encrypt appends 8 random bytes to given []byte to mock.
func (c *tCrypterSmart) Encrypt(b []byte) ([]byte, error) {
	randSuffix := make([]byte, 8)
	crand.Read(randSuffix)
	b = append(b, randSuffix...)
	return b, c.encryptErr
}

// Decrypt deletes the last 8 bytes from given []byte.
func (c *tCrypterSmart) Decrypt(b []byte) ([]byte, error) {
	return b[:len(b)-8], c.decryptErr
}

func (c *tCrypterSmart) Serialize() []byte { return c.params }

func (c *tCrypterSmart) Close() {}

type tCrypter struct {
	encryptErr error
	decryptErr error
	recryptErr error
}

func (c *tCrypter) Encrypt(b []byte) ([]byte, error) {
	return b, c.encryptErr
}

func (c *tCrypter) Decrypt(b []byte) ([]byte, error) {
	return b, c.decryptErr
}

func (c *tCrypter) Serialize() []byte { return nil }

func (c *tCrypter) Close() {}

func tFetcher(_ context.Context, log util.Logger, _ map[uint32]*SupportedAsset) map[uint32]float64 {
	return map[uint32]float64{
		tUTXOAssetA.ID: 45,
		tUTXOAssetB.ID: 32000,
	}
}

type testRig struct {
	shutdown func()
	core     *Core
	db       *TDB
	queue    *wait.TickerQueue
	crypter  encrypt.Crypter
}

func newTestRig() *testRig {
	tdb := &TDB{
		wallet: &db.Wallet{},
	}

	// Set the global waiter expiration, and start the waiter.
	queue := wait.NewTickerQueue(time.Millisecond * 5)
	ctx, cancel := context.WithCancel(tCtx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		queue.Run(ctx)
	}()

	crypter := &tCrypter{}

	shutdown := func() {
		cancel()
		wg.Wait()
	}

	rig := &testRig{
		shutdown: shutdown,
		core: &Core{
			ctx:        ctx,
			cfg:        &Config{},
			db:         tdb,
			log:        tLogger,
			wallets:    make(map[uint32]*xcWallet),
			newCrypter: func([]byte) encrypt.Crypter { return crypter },
			reCrypter:  func([]byte, []byte) (encrypt.Crypter, error) { return crypter, crypter.recryptErr },
			noteChans:  make(map[uint64]chan Notification),
			tipPending: make(map[uint32]uint64),
			tipActive:  make(map[uint32]bool),
			balPending: make(map[uint32]*asset.Balance),
			balActive:  make(map[uint32]bool),

			fiatRateSources:         make(map[string]*commonRateSource),
			autoDisabledRateSources: make(map[string]time.Time),

			notes:            make(chan asset.WalletNotification, 128),
			requestedActions: make(map[string]*asset.ActionRequiredNote),
		},
		db:      tdb,
		queue:   queue,
		crypter: crypter,
	}

	rig.core.intl.Store(&locale{
		m:       originLocale,
		printer: message.NewPrinter(language.AmericanEnglish),
	})

	rig.core.InitializeClient(tPW, nil)

	return rig
}

func TestMain(m *testing.M) {
	var shutdown context.CancelFunc
	tCtx, shutdown = context.WithCancel(context.Background())

	doIt := func() int {
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

func TestCreateWallet(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	// Create a new asset.
	a := *tUTXOAssetA
	tILT := &a
	tILT.Symbol = "ilt"
	tILT.ID, _ = util.BipSymbolID(tILT.Symbol)

	// Create registration form.
	form := &WalletForm{
		AssetID: tILT.ID,
		Config: map[string]string{
			"rpclisten": "localhost",
		},
		Type: "type",
	}

	ensureErr := func(tag string) {
		t.Helper()
		err := tCore.CreateWallet(tPW, wPW, form)
		if err == nil {
			t.Fatalf("no %s error", tag)
		}
	}

	// Try to add an existing wallet.
	wallet, tWallet := newTWallet(tILT.ID)
	tCore.wallets[tILT.ID] = wallet
	ensureErr("existing wallet")
	delete(tCore.wallets, tILT.ID)

	// Failure to retrieve encryption key params.
	creds := tCore.credentials
	tCore.credentials = nil
	ensureErr("db.Get")
	tCore.credentials = creds

	// Crypter error.
	rig.crypter.(*tCrypter).encryptErr = errTest
	ensureErr("Encrypt")
	rig.crypter.(*tCrypter).encryptErr = nil

	// Try an unknown wallet (not yet asset.Register'ed).
	ensureErr("unregistered asset")

	// Register the asset.
	asset.Register(tILT.ID, &tDriver{
		wallet:        wallet.Wallet,
		decodedCoinID: "ilt-coin",
		winfo:         tWalletInfo,
	}, true)

	// Connection error.
	tWallet.connectErr = errTest
	ensureErr("Connect")
	tWallet.connectErr = nil

	// Unlock error.
	tWallet.unlockErr = errTest
	ensureErr("Unlock")
	tWallet.unlockErr = nil

	// Address error.
	tWallet.addrErr = errTest
	ensureErr("Address")
	tWallet.addrErr = nil

	// Balance error. A failed creation must also not broadcast wallet state:
	// the unlock step (which notifies on state change) runs before the wallet
	// is registered, and a leaked note would tell clients about a wallet that
	// core never keeps.
	noteFeed := tCore.NotificationFeed()
	tWallet.balErr = errTest
	ensureErr("Balance")
	tWallet.balErr = nil
drain:
	for {
		select {
		case n := <-noteFeed.C:
			if n.Type() == NoteTypeWalletState {
				t.Fatalf("failed wallet creation leaked a walletstate note: %v", n)
			}
		default:
			break drain
		}
	}

	// Database error.
	rig.db.updateWalletErr = errTest
	ensureErr("db.UpdateWallet")
	rig.db.updateWalletErr = nil

	// Success
	delete(tCore.wallets, tILT.ID)
	err := tCore.CreateWallet(tPW, wPW, form)
	if err != nil {
		t.Fatalf("error when should be no error: %v", err)
	}
}

// TestSplitDBWalletsByTokenStatusOrderIndependent guards against the
// initialize() panic-turned-error "cannot load X wallet before Y wallet":
// db.Wallets() reads a Go map internally, so its returned order is
// randomized and can put a token wallet ahead of its network wallet.
// splitDBWalletsByTokenStatus is what lets initialize() load all network
// wallets before any token wallet regardless of that input order.
func TestSplitDBWalletsByTokenStatusOrderIndependent(t *testing.T) {
	const tokenAssetID = 60001 // arbitrary; registered only for this test
	asset.RegisterToken(tokenAssetID, &util.Token{
		NetworkID: tACCTAsset.ID,
		Name:      "Test Token",
	}, &asset.WalletDefinition{Type: "type"}, nil, true)

	// Token wallet listed first, reproducing the case that broke initialize().
	dbWallets := []*db.Wallet{
		{AssetID: tokenAssetID, Type: "type"},
		{AssetID: tACCTAsset.ID, Type: "type"},
	}
	networkWallets, tokenWallets := splitDBWalletsByTokenStatus(dbWallets)
	if len(networkWallets) != 1 || networkWallets[0].AssetID != tACCTAsset.ID {
		t.Fatalf("networkWallets = %+v, want just the %d network wallet", networkWallets, tACCTAsset.ID)
	}
	if len(tokenWallets) != 1 || tokenWallets[0].AssetID != tokenAssetID {
		t.Fatalf("tokenWallets = %+v, want just the %d token wallet", tokenWallets, tokenAssetID)
	}
}

func TestInitializeClient(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	clearCreds := func() {
		tCore.credentials = nil
		rig.db.creds = nil
	}

	clearCreds()

	_, err := tCore.InitializeClient(tPW, nil)
	if err != nil {
		t.Fatalf("InitializeClient error: %v", err)
	}

	clearCreds()

	// Empty password.
	emptyPass := []byte("")
	_, err = tCore.InitializeClient(emptyPass, nil)
	if err == nil {
		t.Fatalf("no error for empty password")
	}

	// Store error. Use a non-empty password to pass empty password check.
	rig.db.setCredsErr = errTest
	_, err = tCore.InitializeClient(tPW, nil)
	if err == nil {
		t.Fatalf("no error for StoreEncryptedKey error")
	}
	rig.db.setCredsErr = nil

	// Success again
	_, err = tCore.InitializeClient(tPW, nil)
	if err != nil {
		t.Fatalf("final InitializeClient error: %v", err)
	}
}

func TestSend(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	wallet, tWallet := newTWallet(tUTXOAssetA.ID)
	tCore.wallets[tUTXOAssetA.ID] = wallet
	tWallet.sendCoin = &tCoin{id: encode.RandomBytes(36)}
	address := "addr"

	// The caller-approved fee rate is passed through to the wallet unchanged.
	const feeRate = 54321

	// Successful
	coin, err := tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false, feeRate, nil)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if coin.Value() != 1e8 {
		t.Fatalf("Expected sent value to be %v, got %v", 1e8, coin.Value())
	}
	if tWallet.sendFeeSuggestion != feeRate {
		t.Fatalf("expected caller fee rate %d passed to wallet, got %d", feeRate, tWallet.sendFeeSuggestion)
	}

	// A rate more than a factor of 2 from the wallet's live estimate is
	// rejected, in both directions.
	tWallet.feeRate = feeRate / 3
	if _, err = tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false, feeRate, nil); err == nil {
		t.Fatalf("no error for a fee rate more than 2x the live estimate")
	}
	tWallet.feeRate = feeRate * 3
	if _, err = tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false, feeRate, nil); err == nil {
		t.Fatalf("no error for a fee rate less than half the live estimate")
	}
	tWallet.feeRate = 0

	// A rate beyond the asset's absolute ceiling is rejected even when no
	// live estimate is available to band-check against.
	tWallet.info.MaxFeeRate = feeRate - 1
	if _, err = tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false, feeRate, nil); err == nil {
		t.Fatalf("no error for a fee rate beyond the asset's sanity ceiling")
	}
	tWallet.info.MaxFeeRate = 0

	// A failed fee pre-check estimate fails the send.
	tWallet.estFeeErr = errTest
	if _, err = tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false, feeRate, nil); err == nil {
		t.Fatalf("no error when the fee pre-check estimator errors")
	}
	tWallet.estFeeErr = nil

	// 0 value
	_, err = tCore.Send(tPW, tUTXOAssetA.ID, 0, address, false, feeRate, nil)
	if err == nil {
		t.Fatalf("no error for zero value send")
	}

	// no wallet
	_, err = tCore.Send(tPW, 12345, 1e8, address, false, feeRate, nil)
	if err == nil {
		t.Fatalf("no error for unknown wallet")
	}

	// connect error
	wallet.hookedUp = false
	tWallet.connectErr = errTest
	_, err = tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false, feeRate, nil)
	if err == nil {
		t.Fatalf("no error for wallet connect error")
	}
	tWallet.connectErr = nil

	// Send error
	tWallet.sendErr = errTest
	_, err = tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false, feeRate, nil)
	if err == nil {
		t.Fatalf("no error for wallet send error")
	}
	tWallet.sendErr = nil

	// Check the coin.
	tWallet.sendCoin = &tCoin{id: []byte{'a'}}
	coin, err = tCore.Send(tPW, tUTXOAssetA.ID, 3e8, address, false, feeRate, nil)
	if err != nil {
		t.Fatalf("coin check error: %v", err)
	}
	coinID := coin.ID()
	if len(coinID) != 1 || coinID[0] != 'a' {
		t.Fatalf("coin ID not propagated")
	}
	if coin.Value() != 3e8 {
		t.Fatalf("Expected sent value to be %v, got %v", 3e8, coin.Value())
	}

	// wallet is not synced
	wallet.syncStatus.Synced = false
	_, err = tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false, feeRate, nil)
	if err == nil {
		t.Fatalf("Expected error for a non-synchronized wallet")
	}
}

// tTaggedWallet wraps TXCWallet to also implement asset.DestinationTagger,
// for testing Core.Send's destination-tag dispatch without teaching the
// shared TXCWallet mock about a feature only one real asset (XRP) has.
type tTaggedWallet struct {
	*TXCWallet
	sentTag *uint32
}

func (w *tTaggedWallet) SendTagged(_ string, value, _ uint64, destTag uint32) (asset.Coin, error) {
	w.sentTag = &destTag
	w.sendCoin.val = value
	if w.sendErr != nil {
		return nil, w.sendErr
	}
	return w.sendCoin, nil
}

func newTaggedTestWallet(assetID uint32) (*xcWallet, *tTaggedWallet) {
	xcw, tw := newTWallet(assetID)
	// Account-based classification, matching real XRP: skips Core.Send's
	// UTXO fee-rate sanity check, which this test isn't exercising.
	tw.info = &asset.WalletInfo{BlockchainClass: asset.BlockchainClassAccount}
	tagged := &tTaggedWallet{TXCWallet: tw}
	xcw.Wallet = tagged
	return xcw, tagged
}

func TestSendDestinationTag(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	address := "addr"
	tag := uint32(12345)

	t.Run("tagger wallet dispatches to SendTagged", func(t *testing.T) {
		wallet, tagged := newTaggedTestWallet(tACCTAsset.ID)
		tCore.wallets[tACCTAsset.ID] = wallet
		tagged.sendCoin = &tCoin{id: encode.RandomBytes(36)}

		coin, err := tCore.Send(tPW, tACCTAsset.ID, 1e8, address, false, 0, &tag)
		if err != nil {
			t.Fatalf("Send error: %v", err)
		}
		if coin.Value() != 1e8 {
			t.Fatalf("Value = %d, want 1e8", coin.Value())
		}
		if tagged.sentTag == nil || *tagged.sentTag != tag {
			t.Fatalf("sentTag = %v, want %d", tagged.sentTag, tag)
		}
	})

	t.Run("non-tagger wallet rejects a destination tag", func(t *testing.T) {
		wallet, tWallet := newTWallet(tUTXOAssetA.ID)
		tCore.wallets[tUTXOAssetA.ID] = wallet
		tWallet.sendCoin = &tCoin{id: encode.RandomBytes(36)}

		if _, err := tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false, 54321, &tag); err == nil {
			t.Fatal("expected an error for a destination tag on a non-DestinationTagger wallet")
		}
	})

	t.Run("destination tag combined with subtract is rejected", func(t *testing.T) {
		wallet, tagged := newTaggedTestWallet(tACCTAsset.ID)
		tCore.wallets[tACCTAsset.ID] = wallet
		tagged.sendCoin = &tCoin{id: encode.RandomBytes(36)}

		if _, err := tCore.Send(tPW, tACCTAsset.ID, 1e8, address, true, 0, &tag); err == nil {
			t.Fatal("expected an error combining a destination tag with subtract")
		}
		// Must not have silently dropped the tag and gone through Withdraw.
		if tagged.sentTag != nil {
			t.Fatal("SendTagged should not have been called")
		}
	})
}

func TestAddrHost(t *testing.T) {
	tests := []struct {
		name, addr, want string
		wantErr          bool
	}{{
		name: "scheme, host, and port",
		addr: "https://localhost:5758",
		want: "localhost:5758",
	}, {
		name: "scheme, ipv6 host, and port",
		addr: "https://[::1]:5758",
		want: "[::1]:5758",
	}, {
		name: "host and port",
		addr: "localhost:5758",
		want: "localhost:5758",
	}, {
		name: "just port",
		addr: ":5758",
		want: "localhost:5758",
	}, {
		name: "ip host and port",
		addr: "127.0.0.1:5758",
		want: "127.0.0.1:5758",
	}, {
		name: "just host",
		addr: "thatonedex.com",
		want: "thatonedex.com:7232",
	}, {
		name: "scheme and host",
		addr: "https://thatonedex.com",
		want: "thatonedex.com:7232",
	}, {
		name: "scheme, host, and path",
		addr: "https://thatonedex.com/any/path",
		want: "thatonedex.com:7232",
	}, {
		name: "ipv6 host",
		addr: "[1:2::]",
		want: "[1:2::]:7232",
	}, {
		name: "ipv6 host and port",
		addr: "[1:2::]:5758",
		want: "[1:2::]:5758",
	}, {
		name: "empty address",
		want: "localhost:7232",
	}, {
		name:    "invalid host",
		addr:    "https://\n:1234",
		wantErr: true,
	}, {
		name:    "invalid port",
		addr:    ":asdf",
		wantErr: true,
	}}
	for _, test := range tests {
		res, err := addrHost(test.addr)
		if res != test.want {
			t.Fatalf("wanted %s but got %s for test '%s'", test.want, res, test.name)
		}
		if test.wantErr {
			if err == nil {
				t.Fatalf("wanted error for test %s, but got none", test.name)
			}
			continue
		} else if err != nil {
			t.Fatalf("addrHost error for test %s: %v", test.name, err)
		}
		// Parsing results a second time should produce the same results.
		res, _ = addrHost(res)
		if res != test.want {
			t.Fatalf("wanted %s but got %s for test '%s'", test.want, res, test.name)
		}
	}
}

func TestAssetBalance(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	wallet, tWallet := newTWallet(tUTXOAssetA.ID)
	tCore.wallets[tUTXOAssetA.ID] = wallet
	bal := &asset.Balance{
		Available: 4e7,
		Immature:  6e7,
		Locked:    2e8,
	}
	tWallet.bal = bal
	walletBal, err := tCore.AssetBalance(tUTXOAssetA.ID)
	if err != nil {
		t.Fatalf("error retrieving asset balance: %v", err)
	}
	dbtest.MustCompareAssetBalances(t, "zero-conf", bal, &walletBal.Balance.Balance)
	if walletBal.ContractLocked != 0 {
		t.Fatalf("contractlocked balance %d > expected value 0", walletBal.ContractLocked)
	}
}

func TestAssetCounter(t *testing.T) {
	assets := make(assetMap)
	assets.count(1)
	if len(assets) != 1 {
		t.Fatalf("count not added")
	}

	newCounts := assetMap{
		1: struct{}{},
		2: struct{}{},
	}
	assets.merge(newCounts)
	if len(assets) != 2 {
		t.Fatalf("counts not absorbed properly")
	}
}

func TestWalletSettings(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	rig.db.wallet = &db.Wallet{
		Settings: map[string]string{
			"abc": "123",
		},
	}
	var assetID uint32 = 54321

	// wallet not found
	_, err := tCore.WalletSettings(assetID)
	if !errorHasCode(err, missingWalletErr) {
		t.Fatalf("wrong error for missing wallet: %v", err)
	}

	tCore.wallets[assetID] = &xcWallet{}

	// db error
	rig.db.walletErr = errTest
	_, err = tCore.WalletSettings(assetID)
	if !errorHasCode(err, dbErr) {
		t.Fatalf("wrong error when expected db error: %v", err)
	}
	rig.db.walletErr = nil

	// success
	returnedSettings, err := tCore.WalletSettings(assetID)
	if err != nil {
		t.Fatalf("WalletSettings error: %v", err)
	}

	if len(returnedSettings) != 1 || returnedSettings["abc"] != "123" {
		t.Fatalf("returned wallet settings are not correct: %v", returnedSettings)
	}
}

func TestChangeAppPass(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	// Use the smarter crypter.
	smartCrypter := newTCrypterSmart()
	rig.crypter = smartCrypter
	rig.core.newCrypter = func([]byte) encrypt.Crypter { return newTCrypterSmart() }
	rig.core.reCrypter = func([]byte, []byte) (encrypt.Crypter, error) { return rig.crypter, smartCrypter.recryptErr }

	tCore := rig.core
	newTPW := []byte("apppass")

	// App Password error
	rig.crypter.(*tCrypterSmart).recryptErr = errTest
	err := tCore.ChangeAppPass(tPW, newTPW)
	if !errorHasCode(err, authErr) {
		t.Fatalf("wrong error for password error: %v", err)
	}
	rig.crypter.(*tCrypterSmart).recryptErr = nil

	oldCreds := tCore.credentials

	rig.db.creds = nil
	err = tCore.ChangeAppPass(tPW, newTPW)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(oldCreds.OuterKeyParams, tCore.credentials.OuterKeyParams) {
		t.Fatalf("credentials not updated in Core")
	}

	if rig.db.creds == nil || !bytes.Equal(tCore.credentials.OuterKeyParams, rig.db.creds.OuterKeyParams) {
		t.Fatalf("credentials not updated in DB")
	}
}

func TestResetAppPass(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	crypter := newTCrypterSmart()
	rig.crypter = crypter
	rig.core.newCrypter = func([]byte) encrypt.Crypter { return crypter }
	rig.core.reCrypter = func([]byte, []byte) (encrypt.Crypter, error) { return rig.crypter, crypter.recryptErr }

	rig.core.credentials = nil
	rig.core.InitializeClient(tPW, nil)

	tCore := rig.core
	seed, err := tCore.ExportSeed(tPW)
	if err != nil {
		t.Fatalf("seed export failed: %v", err)
	}

	// Invalid seed error
	invalidSeed := seed[:24]
	err = tCore.ResetAppPass(tPW, invalidSeed)
	if !strings.Contains(err.Error(), "error decoding seed") {
		t.Fatalf("wrong error for invalid seed length: %v", err)
	}

	// Want incorrect seed error. Appending to the final word makes it one the
	// mnemonic word list does not contain.
	rig.crypter.(*tCrypterSmart).recryptErr = errTest
	err = tCore.ResetAppPass(tPW, seed+"blah")
	if !strings.Contains(err.Error(), "error decoding seed") {
		t.Fatalf("wrong error for incorrect seed: %v", err)
	}

	// ok, no crypter error.
	rig.crypter.(*tCrypterSmart).recryptErr = nil
	err = tCore.ResetAppPass(tPW, seed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetWalletPassword(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	rig.db.wallet = &db.Wallet{
		EncryptedPW: []byte("abc"),
	}
	newPW := []byte("def")
	var assetID uint32 = 54321

	// Nil password error
	err := tCore.SetWalletPassword(tPW, assetID, nil)
	if !errorHasCode(err, passwordErr) {
		t.Fatalf("wrong error for nil password error: %v", err)
	}

	// Auth error
	rig.crypter.(*tCrypter).recryptErr = errTest
	err = tCore.SetWalletPassword(tPW, assetID, newPW)
	if !errorHasCode(err, authErr) {
		t.Fatalf("wrong error for auth error: %v", err)
	}
	rig.crypter.(*tCrypter).recryptErr = nil

	// Missing wallet error
	err = tCore.SetWalletPassword(tPW, assetID, newPW)
	if !errorHasCode(err, missingWalletErr) {
		t.Fatalf("wrong error for missing wallet: %v", err)
	}

	xyzWallet, tXyzWallet := newTWallet(assetID)
	tCore.wallets[assetID] = xyzWallet

	// Connection error
	xyzWallet.hookedUp = false
	tXyzWallet.connectErr = errTest
	err = tCore.SetWalletPassword(tPW, assetID, newPW)
	if !errorHasCode(err, connectionErr) {
		t.Fatalf("wrong error for connection error: %v", err)
	}
	xyzWallet.hookedUp = true
	tXyzWallet.connectErr = nil

	// Unlock error
	tXyzWallet.unlockErr = errTest
	err = tCore.SetWalletPassword(tPW, assetID, newPW)
	if !errorHasCode(err, authErr) {
		t.Fatalf("wrong error for auth error: %v", err)
	}
	tXyzWallet.unlockErr = nil

	// SetWalletPassword db error
	rig.db.setWalletPwErr = errTest
	err = tCore.SetWalletPassword(tPW, assetID, newPW)
	if !errorHasCode(err, dbErr) {
		t.Fatalf("wrong error for missing wallet: %v", err)
	}
	rig.db.setWalletPwErr = nil

	// Success
	err = tCore.SetWalletPassword(tPW, assetID, newPW)
	if err != nil {
		t.Fatalf("SetWalletPassword error: %v", err)
	}

	// Check that the xcWallet was updated.
	decNewPW, _ := rig.crypter.Decrypt(xyzWallet.encPW())
	if !bytes.Equal(decNewPW, newPW) {
		t.Fatalf("xcWallet encPW field not updated")
	}
}

// SetWalletPassword must hold walletMtx only for the wallet lookup. Everything
// after it — the connect, the backend Unlock/Lock RPCs, the DB write — is
// network- and disk-bound, and holding the process-wide write lock across that
// stalls every other asset's c.wallet lookup on an unreachable node.
func TestSetWalletPasswordDoesNotBlockWalletLookups(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	rig.db.wallet = &db.Wallet{EncryptedPW: []byte("abc")}

	var assetID uint32 = 54321
	xyzWallet, tXyzWallet := newTWallet(assetID)
	tCore.wallets[assetID] = xyzWallet
	otherWallet, _ := newTWallet(tUTXOAssetA.ID)
	tCore.wallets[tUTXOAssetA.ID] = otherWallet

	// Park the backend Unlock that setWalletPassword makes to verify the new
	// password, simulating an unreachable node.
	entered := make(chan struct{})
	release := make(chan struct{})
	tXyzWallet.unlockHook = func() {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}

	setDone := make(chan error, 1)
	go func() { setDone <- tCore.SetWalletPassword(tPW, assetID, []byte("def")) }()
	<-entered

	lookupDone := make(chan struct{})
	go func() {
		defer close(lookupDone)
		tCore.wallet(tUTXOAssetA.ID)
		tCore.wallet(assetID)
	}()

	select {
	case <-lookupDone:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("c.wallet lookup blocked behind SetWalletPassword's backend RPC")
	}

	close(release)
	if err := <-setDone; err != nil {
		t.Fatalf("SetWalletPassword error: %v", err)
	}
}

// ReconfigureWallet's restart path (a non-live-reconfigurable wallet, or one
// whose Reconfigure reports restart required) loads a brand new xcWallet and
// only installs it into c.wallets after the whole reload succeeds. Passing
// newWalletPW != nil calls setWalletPassword on that not-yet-installed
// wallet, which must not be rejected by the "still the live wallet" guard
// that only holds for the SetWalletPassword entry point.
func TestReconfigureWalletRestartPathSetsNewPassword(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	const assetID = 28 // vtc; unused by other tests in this file

	oldXCWallet, _ := newTWallet(assetID)
	tCore.wallets[assetID] = oldXCWallet

	newRawWallet := &TXCWallet{
		info:        tWalletInfo,
		syncStatus:  func() (bool, float32, error) { return true, 1, nil },
		ownsAddress: true,
	}
	asset.Register(assetID, &tDriver{
		wallet:        newRawWallet,
		decodedCoinID: "vtc-coin",
		winfo:         tWalletInfo,
	}, true)

	form := &WalletForm{
		AssetID: assetID,
		Config:  map[string]string{},
		Type:    "type",
	}
	newPW := []byte("newpw")

	if err := tCore.ReconfigureWallet(tPW, newPW, form); err != nil {
		t.Fatalf("ReconfigureWallet error: %v", err)
	}

	liveWallet, found := tCore.wallet(assetID)
	if !found {
		t.Fatalf("no wallet installed for asset %d after reconfigure", assetID)
	}
	if liveWallet.Wallet != asset.Wallet(newRawWallet) {
		t.Fatalf("reconfigured wallet was not swapped into c.wallets")
	}
	decPW, err := rig.crypter.Decrypt(liveWallet.encPW())
	if err != nil {
		t.Fatalf("Decrypt error: %v", err)
	}
	if !bytes.Equal(decPW, newPW) {
		t.Fatalf("new password not persisted: got %q, want %q", decPW, newPW)
	}
}

// beginTask must make the shuttingDown check and the wg.Add atomic. An
// unsynchronized c.ctx check before an Add races Run's Wait: the counter can
// legitimately reach zero and Wait return between the check and the Add, which
// panics with "sync: WaitGroup misuse". Run under -race with -count>1.
func TestBeginTaskRacesShutdown(t *testing.T) {
	for range 50 {
		c := &Core{}

		// Mimic Run's persistent notification goroutine: it holds a count for
		// the life of the run, publishes shuttingDown when its context is
		// cancelled, and only then drops that count.
		c.wg.Add(1)
		shutdown := make(chan struct{})
		go func() {
			defer c.wg.Done()
			<-shutdown
			c.taskMtx.Lock()
			c.shuttingDown = true
			c.taskMtx.Unlock()
		}()

		var starters sync.WaitGroup
		for range 32 {
			starters.Add(1)
			go func() {
				defer starters.Done()
				if !c.beginTask() {
					return
				}
				defer c.endTask()
			}()
		}

		go close(shutdown)
		c.wg.Wait() // races the beginTask calls above
		starters.Wait()

		// Once shutdown has been published, registration must be refused: Run
		// has already passed Wait and nothing would observe the new count.
		if c.beginTask() {
			c.endTask()
			t.Fatal("beginTask registered work after shutdown was published")
		}
	}
}

func TestWalletSyncing(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	noteFeed := tCore.NotificationFeed()
	dcrWallet, tDcrWallet := newTWallet(tUTXOAssetA.ID)
	dcrWallet.syncStatus.Synced = false
	dcrWallet.syncStatus.Blocks = 0
	dcrWallet.hookedUp = false
	// Connect with tCore.connectWallet below.

	tStart := time.Now()
	testDuration := 100 * time.Millisecond
	syncTickerPeriod = 10 * time.Millisecond

	tDcrWallet.syncStatus = func() (bool, float32, error) {
		progress := float32(float64(time.Since(tStart)) / float64(testDuration))
		if progress >= 1 {
			return true, 1, nil
		}
		return false, progress, nil
	}

	_, err := tCore.connectWallet(dcrWallet)
	if err != nil {
		t.Fatalf("connectWallet error: %v", err)
	}

	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	var progressNotes int
out:
	for {
		select {
		case note := <-noteFeed.C:
			syncNote, ok := note.(*WalletSyncNote)
			if !ok {
				continue
			}
			progressNotes++
			if syncNote.SyncStatus.Synced {
				break out
			}
		case <-timeout.C:
			t.Fatalf("timed out waiting for synced wallet note. Received %d progress notes", progressNotes)
		}
	}
	// By the time we've got 10th note it should signal that the wallet has been
	// synced (due to how we've set up testDuration and syncTickerPeriod values).
	if progressNotes > 10 {
		t.Fatalf("expected 10 progress notes at most, got %d", progressNotes)
	}
}

func TestCoreAssetSeedAndPass(t *testing.T) {
	// This test ensures the derived wallet seed and password are deterministic
	// and depend on both asset ID and app seed.

	// NOTE: the blake256 hash of an empty slice is:
	// []byte{0x71, 0x6f, 0x6e, 0x86, 0x3f, 0x74, 0x4b, 0x9a, 0xc2, 0x2c, 0x97, 0xec, 0x7b, 0x76, 0xea, 0x5f,
	//        0x59, 0x8, 0xbc, 0x5b, 0x2f, 0x67, 0xc6, 0x15, 0x10, 0xbf, 0xc4, 0x75, 0x13, 0x84, 0xea, 0x7a}
	// The above was very briefly the password for all seeded wallets, not released.

	tests := []struct {
		name     string
		appSeed  []byte
		assetID  uint32
		wantSeed []byte
		wantPass []byte
	}{
		{
			name:    "base",
			appSeed: []byte{1, 2, 3},
			assetID: 2,
			wantSeed: []byte{
				0xac, 0x61, 0xb1, 0xbc, 0x77, 0xd0, 0xa6, 0xd5, 0xd2, 0xb5, 0xc9, 0x77, 0x91, 0xd6, 0x4a, 0xaf,
				0x4a, 0xa3, 0x47, 0xb7, 0xb, 0x85, 0xe, 0x82, 0x1c, 0x79, 0xab, 0xc0, 0x86, 0x50, 0xee, 0xda},
			wantPass: []byte{
				0xd8, 0xf0, 0x27, 0x4d, 0xbc, 0x56, 0xb0, 0x74, 0x1e, 0x20, 0x3b, 0x98, 0xe9, 0xaa, 0x5c, 0xba,
				0x13, 0xfd, 0x60, 0x3b, 0x83, 0x76, 0x2e, 0x4b, 0x5d, 0x6d, 0x19, 0x57, 0x89, 0xe2, 0x8b, 0xc7},
		},
		{
			name:    "change app seed",
			appSeed: []byte{2, 2, 3},
			assetID: 2,
			wantSeed: []byte{
				0xf, 0xc9, 0xf, 0xa8, 0xb3, 0xe9, 0x31, 0x2a, 0xba, 0xf1, 0xda, 0x70, 0x41, 0x81, 0x49, 0xed,
				0xad, 0x47, 0x9, 0xcd, 0xe2, 0x17, 0x14, 0xd, 0x63, 0x49, 0x8a, 0xd8, 0xff, 0x1f, 0x3e, 0x8b},
			wantPass: []byte{
				0x78, 0x21, 0x72, 0x59, 0xbe, 0x39, 0xea, 0x54, 0x10, 0x46, 0x7d, 0x7e, 0xa, 0x95, 0xc4, 0xa0,
				0xd8, 0x73, 0xce, 0x1, 0xb2, 0x49, 0x98, 0x6c, 0x68, 0xc5, 0x69, 0x69, 0xa7, 0x13, 0xc1, 0xce},
		},
		{
			name:    "change asset ID",
			appSeed: []byte{1, 2, 3},
			assetID: 0,
			wantSeed: []byte{
				0xe1, 0xad, 0x62, 0xe4, 0x60, 0xfd, 0x75, 0x91, 0x3d, 0x41, 0x2e, 0x8e, 0xc5, 0x72, 0xd4, 0xa2,
				0x39, 0x2d, 0x32, 0x86, 0xf0, 0x6b, 0xf7, 0xdf, 0x48, 0xcc, 0x57, 0xb1, 0x4b, 0x7b, 0xc6, 0xce},
			wantPass: []byte{
				0x52, 0xba, 0x59, 0x21, 0xd3, 0xc5, 0x6b, 0x2, 0x2c, 0x12, 0xc1, 0x98, 0xdc, 0x84, 0xed, 0x68,
				0x6, 0x35, 0xa6, 0x25, 0xd0, 0xc4, 0x49, 0x5a, 0x13, 0xc3, 0x12, 0xfb, 0xeb, 0xb3, 0x61, 0x88},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed, pass := AssetSeedAndPass(tt.assetID, tt.appSeed)
			if !bytes.Equal(pass, tt.wantPass) {
				t.Errorf("pass not as expected, got %#v", pass)
			}
			if !bytes.Equal(seed, tt.wantSeed) {
				t.Errorf("seed not as expected, got %#v", seed)
			}
		})
	}
}

// TestAssetSeedAndPassSeedAssetIDOverride guards the mechanism that lets two
// wallets control one address: a chain that shares another asset's keys (e.g.
// HyperEVM sharing HyperCore's) declares WalletInfo.SeedAssetID, and
// AssetSeedAndPass must derive the identical seed/pass for both asset IDs. It
// must also win over the existing EVM-class asset ID 60 remap, since a
// SeedAssetID chain can itself be EVM-class (as HyperEVM is) but must not
// fall back to the generic EVM seed once it declares a specific one.
func TestAssetSeedAndPassSeedAssetIDOverride(t *testing.T) {
	// Unused elsewhere in this file.
	const (
		sourceAssetID = 99001
		aliasAssetID  = 99002
	)
	asset.Register(sourceAssetID, &tDriver{
		winfo: &asset.WalletInfo{
			BlockchainClass: asset.BlockchainClassAccount,
			UnitInfo:        util.UnitInfo{Conventional: util.Denomination{ConversionFactor: 1e8}},
		},
	}, true)
	asset.Register(aliasAssetID, &tDriver{
		winfo: &asset.WalletInfo{
			BlockchainClass: asset.BlockchainClassEVM,
			SeedAssetID:     sourceAssetID,
			UnitInfo:        util.UnitInfo{Conventional: util.Denomination{ConversionFactor: 1e9}},
		},
	}, true)

	appSeed := []byte{4, 5, 6}
	sourceSeed, sourcePass := AssetSeedAndPass(sourceAssetID, appSeed)
	aliasSeed, aliasPass := AssetSeedAndPass(aliasAssetID, appSeed)

	if !bytes.Equal(sourceSeed, aliasSeed) {
		t.Fatalf("aliased asset derived a different seed than its source: %#v != %#v", aliasSeed, sourceSeed)
	}
	if !bytes.Equal(sourcePass, aliasPass) {
		t.Fatalf("aliased asset derived a different pass than its source: %#v != %#v", aliasPass, sourcePass)
	}

	// Confirm the override actually took effect rather than both asset IDs
	// coincidentally landing on the generic EVM-class seed (60).
	genericEVMSeed, _ := AssetSeedAndPass(60, appSeed)
	if bytes.Equal(genericEVMSeed, aliasSeed) {
		t.Fatal("SeedAssetID did not override the generic EVM-class seed derivation")
	}
}

func TestToggleRateSourceStatus(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	tests := []struct {
		name, source  string
		wantErr, init bool
	}{{
		name:    "Invalid rate source",
		source:  "binance",
		wantErr: true,
	}, {
		name:    "ok valid source",
		source:  coinpaprika,
		wantErr: false,
	}, {
		name:    "ok already disabled/not initialized || enabled",
		source:  coinpaprika,
		wantErr: false,
	}}

	// Test disabling fiat rate source.
	for _, test := range tests {
		err := tCore.ToggleRateSourceStatus(test.source, true)
		if test.wantErr != (err != nil) {
			t.Fatalf("%s: wantErr = %t, err = %v", test.name, test.wantErr, err)
		}
	}

	// Test enabling fiat rate source.
	for _, test := range tests {
		if test.init {
			tCore.fiatRateSources[test.source] = newCommonRateSource(tFetcher)
		}
		err := tCore.ToggleRateSourceStatus(test.source, false)
		if test.wantErr != (err != nil) {
			t.Fatalf("%s: wantErr = %t, err = %v", test.name, test.wantErr, err)
		}
	}
}

func TestFiatRateSources(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	supportedFetchers := len(fiatRateFetchers)
	rateSources := tCore.FiatRateSources()
	if len(rateSources) != supportedFetchers {
		t.Fatalf("Expected %d number of fiat rate source/fetchers", supportedFetchers)
	}
}

func TestFiatConversions(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	// No fiat rate source initialized
	fiatRates := tCore.fiatConversions()
	if len(fiatRates) != 0 {
		t.Fatal("Unexpected asset rate values.")
	}

	// Initialize fiat rate sources.
	for token := range fiatRateFetchers {
		tCore.fiatRateSources[token] = newCommonRateSource(tFetcher)
	}

	// Fetch fiat rates.
	tCore.wg.Add(1)
	go func() {
		defer tCore.wg.Done()
		tCore.refreshFiatRates(tCtx)
	}()
	tCore.wg.Wait()

	// Expects assets fiat rate values.
	fiatRates = tCore.fiatConversions()
	if len(fiatRates) != 2 {
		t.Fatal("Expected assets fiat rate for two assets")
	}

	// fiat rates for assets can expire, and fiat rate fetchers can be
	// removed if expired.
	for token, source := range tCore.fiatRateSources {
		source.fiatRates[tUTXOAssetA.ID].lastUpdate = time.Now().Add(-time.Minute)
		source.fiatRates[tUTXOAssetB.ID].lastUpdate = time.Now().Add(-time.Minute)
		if source.isExpired(55 * time.Second) {
			delete(tCore.fiatRateSources, token)
		}
	}

	fiatRates = tCore.fiatConversions()
	if len(fiatRates) != 0 {
		t.Fatal("Unexpected assets fiat rate values, expected to ignore expired fiat rates.")
	}

	if len(tCore.fiatRateSources) != 0 {
		t.Fatal("Expected fiat conversion to be disabled, all rate source data has expired.")
	}
}

func TestValidateAddress(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	wallet, tWallet := newTWallet(tUTXOAssetA.ID)
	tCore.wallets[tUTXOAssetA.ID] = wallet

	tests := []struct {
		name              string
		addr              string
		wantValidAddr     bool
		wantMissingWallet bool
		wantErr           bool
	}{{
		name:          "valid address",
		addr:          "randomvalidaddress",
		wantValidAddr: true,
	}, {
		name: "invalid address",
		addr: "",
	}, {
		name:              "wallet not found",
		addr:              "randomaddr",
		wantMissingWallet: true,
		wantErr:           true,
	}}
	for _, test := range tests {
		tWallet.validAddr = test.wantValidAddr
		if test.wantMissingWallet {
			tCore.wallets = make(map[uint32]*xcWallet)
		}
		valid, err := tCore.ValidateAddress(test.addr, tUTXOAssetA.ID)
		if test.wantErr {
			if err != nil {
				continue
			}
			t.Fatalf("%s: expected error", test.name)
		}
		if test.wantValidAddr != valid {
			t.Fatalf("Got wrong response for address validation, got %v expected %v", valid, test.wantValidAddr)
		}
	}
}

func TestEstimateSendTxFee(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	tests := []struct {
		name              string
		asset             uint32
		estFee            uint64
		feeRate           uint64
		value             uint64
		subtract          bool
		wantMissingWallet bool
		estFeeErr         error
		wantErr           bool
	}{{
		name:     "ok",
		asset:    tUTXOAssetA.ID,
		subtract: true,
		estFee:   1e8,
		feeRate:  100,
		value:    1e8,
	}, {
		name:      "zero amount",
		asset:     tACCTAsset.ID,
		subtract:  true,
		estFeeErr: errTest,
		wantErr:   true,
	}, {
		name:      "estimator error",
		asset:     tACCTAsset.ID,
		subtract:  true,
		feeRate:   100,
		estFeeErr: errTest,
		wantErr:   true,
		value:     1e8,
	}, {
		// A UTXO wallet prices at exactly the rate the estimate reports, so
		// no live rate must be an error, not an estimate at some other rate.
		name:    "no UTXO fee rate available",
		asset:   tUTXOAssetA.ID,
		value:   1e8,
		wantErr: true,
	}, {
		name:              "wallet not found",
		asset:             tUTXOAssetA.ID,
		wantErr:           true,
		wantMissingWallet: true,
		value:             1e8,
	}}

	for _, test := range tests {
		wallet, tWallet := newTWallet(test.asset)
		tCore.wallets[test.asset] = wallet
		if test.wantMissingWallet {
			delete(tCore.wallets, test.asset)
		}

		tWallet.estFee = test.estFee
		tWallet.feeRate = test.feeRate
		tWallet.estFeeErr = test.estFeeErr

		estimate, err := tCore.EstimateSendTxFee("addr", test.asset, test.value, test.subtract, false)
		if test.wantErr {
			if err != nil {
				continue
			}
			t.Fatalf("%s: expected error", test.name)
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		if estimate.Fee != test.estFee {
			t.Fatalf("%s: expected fee %v, got %v", test.name, test.estFee, estimate.Fee)
		}
		if estimate.FeeRate != test.feeRate {
			t.Fatalf("%s: expected fee rate %v, got %v", test.name, test.feeRate, estimate.FeeRate)
		}
	}
}

func TestPokesCacheInit(t *testing.T) {
	tPokes := []*db.Notification{
		{DetailText: "poke 1"},
		{DetailText: "poke 2"},
		{DetailText: "poke 3"},
		{DetailText: "poke 4"},
		{DetailText: "poke 5"},
	}
	{
		pokesCapacity := 6
		c := newPokesCache(pokesCapacity)
		c.init(tPokes)

		// Check if the cache is initialized correctly
		if len(c.cache) != 5 {
			t.Errorf("Expected cache length %d, got %d", len(tPokes), len(c.cache))
		}

		if c.cursor != 5 {
			t.Errorf("Expected cursor %d, got %d", len(tPokes)%pokesCapacity, c.cursor)
		}

		// Check if the cache contains the correct pokes
		for i, poke := range tPokes {
			if c.cache[i] != poke {
				t.Errorf("Expected poke %v at index %d, got %v", poke, i, c.cache[i])
			}
		}
	}
	{
		pokesCapacity := 4
		c := newPokesCache(pokesCapacity)
		c.init(tPokes)

		// Check if the cache is initialized correctly
		if len(c.cache) != 1 {
			t.Errorf("Expected cache length %d, got %d", 1, len(c.cache))
		}

		if c.cursor != 1 {
			t.Errorf("Expected cursor %d, got %d", 1, c.cursor)
		}

		// Check if the cache contains the correct pokes
		for i, poke := range tPokes[:len(tPokes)-pokesCapacity] {
			if c.cache[i] != poke {
				t.Errorf("Expected poke %v at index %d, got %v", poke, i, c.cache[i])
			}
		}
	}
}

func TestPokesAdd(t *testing.T) {
	tPokes := []*db.Notification{
		{DetailText: "poke 1"},
		{DetailText: "poke 2"},
		{DetailText: "poke 3"},
		{DetailText: "poke 4"},
		{DetailText: "poke 5"},
	}
	tNewPoke := &db.Notification{
		DetailText: "poke 6",
	}
	{
		pokesCapacity := 6
		c := newPokesCache(pokesCapacity)
		c.init(tPokes)
		c.add(tNewPoke)

		// Check if the cache is updated correctly
		if len(c.cache) != 6 {
			t.Errorf("Expected cache length %d, got %d", len(tPokes), len(c.cache))
		}

		if c.cursor != 0 {
			t.Errorf("Expected cursor %d, got %d", 0, c.cursor)
		}

		// Check if the cache contains the correct pokes
		tAllPokes := append(tPokes, tNewPoke)
		for i, poke := range tAllPokes {
			if c.cache[i] != poke {
				t.Errorf("Expected poke %v at index %d, got %v", poke, i, c.cache[i])
			}
		}
	}
	{
		pokesCapacity := 5
		c := newPokesCache(pokesCapacity)
		c.init(tPokes)
		c.add(tNewPoke)

		// Check if the cache is updated correctly
		if len(c.cache) != pokesCapacity {
			t.Errorf("Expected cache length %d, got %d", pokesCapacity, len(c.cache))
		}

		if c.cursor != 1 {
			t.Errorf("Expected cursor %d, got %d", 1, c.cursor)
		}

		// Check if the cache contains the correct pokes
		tAllPokes := make([]*db.Notification, 0)
		tAllPokes = append(tAllPokes, tNewPoke)
		tAllPokes = append(tAllPokes, tPokes[1:]...)
		for i, poke := range tAllPokes {
			if c.cache[i] != poke {
				t.Errorf("Expected poke %v at index %d, got %v", poke, i, c.cache[i])
			}
		}
	}
}

func TestPokesCachePokes(t *testing.T) {
	tPokes := []*db.Notification{
		{TimeStamp: 1, DetailText: "poke 1"},
		{TimeStamp: 2, DetailText: "poke 2"},
		{TimeStamp: 3, DetailText: "poke 3"},
		{TimeStamp: 4, DetailText: "poke 4"},
		{TimeStamp: 5, DetailText: "poke 5"},
	}
	{
		pokesCapacity := 6
		c := newPokesCache(pokesCapacity)
		c.init(tPokes)
		pokes := c.pokes()

		// Check if the result length is correct
		if len(pokes) != len(tPokes) {
			t.Errorf("Expected pokes length %d, got %d", len(tPokes), len(pokes))
		}

		// Check if the result contains the correct pokes
		for i, poke := range tPokes {
			if pokes[i] != poke {
				t.Errorf("Expected poke %v at index %d, got %v", poke, i, pokes[i])
			}
		}
	}
	{
		pokesCapacity := 5
		tNewPoke := &db.Notification{
			TimeStamp:  6,
			DetailText: "poke 6",
		}
		c := newPokesCache(pokesCapacity)
		c.init(tPokes)
		c.add(tNewPoke)
		pokes := c.pokes()

		// Check if the result length is correct
		if len(pokes) != pokesCapacity {
			t.Errorf("Expected cache length %d, got %d", 1, len(pokes))
		}

		tAllPokes := append(tPokes[1:], tNewPoke)
		// Check if the result contains the correct pokes
		for i, poke := range tAllPokes {
			if pokes[i] != poke {
				t.Errorf("Expected poke %v at index %d, got %v", poke, i, pokes[i])
			}
		}
	}
}

// newSuspendRig builds a logged-in rig with a connected wallet per assetID,
// ready for a Suspend/Resume round trip. The caller defers rig.shutdown.
func newSuspendRig(t *testing.T, assetIDs ...uint32) (*testRig, []*xcWallet, []*TXCWallet) {
	t.Helper()
	rig := newTestRig()
	tCore := rig.core

	xcws := make([]*xcWallet, 0, len(assetIDs))
	tws := make([]*TXCWallet, 0, len(assetIDs))
	for i, assetID := range assetIDs {
		w, tw := newTWallet(assetID)
		w.hookedUp = false
		// Don't share tPW's backing array: Logout clears the cached password in
		// place, which would zero the package-level tPW for every other test.
		w.pw = encode.PassBytes(fmt.Sprintf("suspendpw%d", i))
		tCore.wallets[assetID] = w
		// Connect the wallet the way login would, so the connector is live.
		if err := w.Connect(); err != nil {
			t.Fatalf("initial Connect of %s: %v", unbip(assetID), err)
		}
		if !w.connected() {
			t.Fatalf("%s wallet should be connected after Connect", unbip(assetID))
		}
		xcws = append(xcws, w)
		tws = append(tws, tw)
	}

	tCore.loginMtx.Lock()
	tCore.loggedIn = true
	tCore.loginMtx.Unlock()
	// Logout zeroes this unconditionally, so a nil would panic.
	xPriv, err := deriveMultisigXPriv(encode.RandomBytes(32))
	if err != nil {
		t.Fatalf("deriveMultisigXPriv: %v", err)
	}
	tCore.multisigXPriv = xPriv

	return rig, xcws, tws
}

// resumeOrder reorders a newSuspendRig's wallets to match the order the Resume
// after a Suspend will walk them in. Suspend builds its set from xcWallets, and
// that is a map iteration, so which wallet a Resume reaches first varies run to
// run. A test that parks "the first reconnect" has to ask which one that is
// rather than assume it is the first one it asked the rig for. Call it after
// Suspend; assetIDs are the ones newSuspendRig was given, in the same order.
func resumeOrder(t *testing.T, tCore *Core, assetIDs []uint32, xcws []*xcWallet, tws []*TXCWallet) ([]*xcWallet, []*TXCWallet) {
	t.Helper()

	tCore.suspendMtx.Lock()
	order := append([]uint32(nil), tCore.suspendedWallets...)
	tCore.suspendMtx.Unlock()
	if len(order) != len(assetIDs) {
		t.Fatalf("expected %d suspended wallets, got %d", len(assetIDs), len(order))
	}

	idx := make(map[uint32]int, len(assetIDs))
	for i, assetID := range assetIDs {
		idx[assetID] = i
	}
	oxcws := make([]*xcWallet, 0, len(order))
	otws := make([]*TXCWallet, 0, len(order))
	for _, assetID := range order {
		i, found := idx[assetID]
		if !found {
			t.Fatalf("suspended set holds an unexpected asset %d", assetID)
		}
		oxcws = append(oxcws, xcws[i])
		otws = append(otws, tws[i])
	}
	return oxcws, otws
}

// TestSuspendResume verifies the mobile pause/resume round-trip: Suspend
// disconnects every wallet without logging out, Resume reconnects them with no
// password, and a real Logout while suspended still evicts key material and
// leaves Resume with nothing to do.
func TestSuspendResume(t *testing.T) {
	rig, wallets, _ := newSuspendRig(t, 42 /* dcr */)
	defer rig.shutdown()
	tCore, w := rig.core, wallets[0]

	// Suspend disconnects without logging out and records the connected set.
	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if w.connected() {
		t.Fatal("wallet should be disconnected after Suspend")
	}
	tCore.suspendMtx.Lock()
	suspendedOK := tCore.suspended && len(tCore.suspendedWallets) == 1 &&
		tCore.suspendedWallets[0] == w.AssetID
	tCore.suspendMtx.Unlock()
	if !suspendedOK {
		t.Fatal("suspend state not recorded correctly")
	}

	// Suspend is idempotent.
	if err := tCore.Suspend(); err != nil {
		t.Fatalf("second Suspend: %v", err)
	}

	// Resume reconnects with no crypter, proving password-free resume.
	if err := tCore.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !w.connected() {
		t.Fatal("wallet should be reconnected after Resume")
	}
	tCore.suspendMtx.Lock()
	cleared := !tCore.suspended && tCore.suspendedWallets == nil
	tCore.suspendMtx.Unlock()
	if !cleared {
		t.Fatal("suspend state should be cleared after Resume")
	}

	// Resume is a no-op when not suspended.
	if err := tCore.Resume(); err != nil {
		t.Fatalf("no-op Resume: %v", err)
	}

	// Suspend, then a real Logout. This is the mobile eviction path: every
	// eviction alarm is preceded by a background pause, so Logout always sees
	// disconnected wallets. It must still shed the cached password even though
	// the wallet cannot be locked over a dropped connection.
	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend before logout: %v", err)
	}
	if !w.locallyUnlocked() {
		t.Fatal("wallet password should still be cached after Suspend")
	}
	if err := tCore.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if w.locallyUnlocked() {
		t.Fatal("cached wallet password still resident after Suspend+Logout")
	}
	if w.connected() {
		t.Fatal("wallet should be disconnected after Logout")
	}

	// Logout cleared the pause state itself, so Resume is a no-op and the
	// shell's Unlock is what reconnects.
	tCore.suspendMtx.Lock()
	stillSuspended := tCore.suspended
	tCore.suspendMtx.Unlock()
	if stillSuspended {
		t.Fatal("suspend state should be cleared by Logout")
	}
	if err := tCore.Resume(); err != nil {
		t.Fatalf("Resume after logout should be a no-op: %v", err)
	}
	if w.connected() {
		t.Fatal("Resume must not reconnect a wallet after logout")
	}
}

// TestResumeInterleavedWithLogout parks a Resume inside its reconnect and runs a
// Logout underneath it. Two things must hold. Logout must not block: Resume no
// longer holds suspendMtx across the reconnects, which are network-bound and
// unbounded. And the wallet must not survive the logout: ConnectionMaster's
// Disconnect is a no-op while a connect is in flight (it returns early on !On,
// and the pre-suspend done channel is still closed), so Logout's own disconnect
// loop cannot stop it and Resume has to undo the connection itself.
func TestResumeInterleavedWithLogout(t *testing.T) {
	rig, wallets, twallets := newSuspendRig(t, 42)
	defer rig.shutdown()
	tCore, w, tw := rig.core, wallets[0], twallets[0]

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// Park the reconnect so Resume is stuck inside connectAndUpdateWallet.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tw.connectHook = func() {
		once.Do(func() { close(entered) })
		<-release
	}

	resumed := make(chan error, 1)
	go func() { resumed <- tCore.Resume() }()
	<-entered

	logoutDone := make(chan error, 1)
	go func() { logoutDone <- tCore.Logout() }()
	select {
	case err := <-logoutDone:
		if err != nil {
			t.Fatalf("Logout: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Logout blocked behind an in-flight Resume")
	}

	close(release)
	if err := <-resumed; err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if w.connected() {
		t.Fatal("wallet left connected by a Resume that raced a Logout")
	}
	if w.locallyUnlocked() {
		t.Fatal("cached wallet password resident after Logout")
	}
}

// TestResumeUnwindsWholeClaimedSet covers a Resume that brings up more than one
// wallet inside a single reconnect. connectAndUpdateWallet does exactly that for
// a token: it connects the token's network wallet first. Undoing only the wallet
// whose reconnect just returned would leave that network wallet connected and
// polling past the very Logout meant to quiet it, since Logout's own disconnect
// loop already ran past it while it was still down.
//
// Rather than register a token asset for the whole test binary, the hook brings
// the second wallet up from inside the first's Connect, which is the same
// side-effecting shape from Resume's point of view. It fires on release so the
// second wallet comes up after Logout's loop has passed it, which is the
// ordering that strands it.
func TestResumeUnwindsWholeClaimedSet(t *testing.T) {
	rig, wallets, twallets := newSuspendRig(t, 42, 0)
	defer rig.shutdown()
	tCore := rig.core

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	wallets, twallets = resumeOrder(t, tCore, []uint32{42, 0}, wallets, twallets)
	first, second := wallets[0], wallets[1]

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	twallets[0].connectHook = func() {
		once.Do(func() { close(entered) })
		<-release
		if err := second.Connect(); err != nil {
			t.Errorf("side-effect Connect: %v", err)
		}
	}

	resumed := make(chan error, 1)
	go func() { resumed <- tCore.Resume() }()
	<-entered

	if err := tCore.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	close(release)
	if err := <-resumed; err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if first.connected() {
		t.Fatal("resumed wallet left connected by a Resume that raced a Logout")
	}
	if second.connected() {
		t.Fatal("wallet connected as a side effect of the reconnect survived the Logout")
	}
}

// TestResumeInterleavedWithNewerResume covers a stale Resume returning after a
// newer one already finished. A bumped generation says only that something
// moved, not what: Resume clears the suspended flag without bumping, so
// "generation moved and not suspended" describes a completed foreground exactly
// as well as it describes a logout. Reading it as a logout tears down wallets
// the user just watched come back, and nothing restores them - the teardown
// records nothing, the next Suspend skips a wallet that is already down, the
// next Resume returns early on a clear flag, and connectWallets runs only on
// Login. The wallets stay dark for the rest of the session.
func TestResumeInterleavedWithNewerResume(t *testing.T) {
	rig, wallets, twallets := newSuspendRig(t, 42, 0)
	defer rig.shutdown()
	tCore := rig.core

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	wallets, twallets = resumeOrder(t, tCore, []uint32{42, 0}, wallets, twallets)

	// Publish the balances before any goroutine runs, so the parked reconnect
	// and the foreground one are not both lazily filling the same field.
	for _, tw := range twallets {
		tw.bal = new(asset.Balance)
	}

	// Park the stale Resume in the second wallet's balance refresh rather than
	// in its Connect: Connect holds connectMtx, which xcWallet.suspend takes, so
	// the Suspend below would simply serialize behind it instead of interleaving.
	// By then the first wallet is back up, which is what gives that Suspend
	// something to record and the foreground Resume something to reconnect.
	entered, release := make(chan struct{}), make(chan struct{})
	var parked int32
	twallets[1].balanceHook = func() {
		if !atomic.CompareAndSwapInt32(&parked, 0, 1) {
			return
		}
		close(entered)
		<-release
	}

	stale := make(chan error, 1)
	go func() { stale <- tCore.Resume() }()
	<-entered

	// A full background/foreground cycle underneath it, both halves completing.
	if err := tCore.Suspend(); err != nil {
		t.Fatalf("second Suspend: %v", err)
	}
	if err := tCore.Resume(); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	for _, w := range wallets {
		if !w.connected() {
			t.Fatalf("%s wallet not reconnected by the second Resume", unbip(w.AssetID))
		}
	}

	close(release)
	if err := <-stale; err != nil {
		t.Fatalf("stale Resume: %v", err)
	}

	for _, w := range wallets {
		if !w.connected() {
			t.Fatalf("%s wallet torn down by a stale Resume after a newer one brought it up", unbip(w.AssetID))
		}
	}
	tCore.suspendMtx.Lock()
	suspended, tracked := tCore.suspended, len(tCore.suspendedWallets)
	tCore.suspendMtx.Unlock()
	if suspended || tracked != 0 {
		t.Fatalf("pause state latched (suspended %v, %d tracked) after a foreground that succeeded",
			suspended, tracked)
	}
}

// TestStaleResumeRearmsUnreachedWallets covers the tail of the claimed set that
// bowing out to a newer session would otherwise drop. Leaving the wallets to it
// is right only for the ones it can see: a wallet the stale Resume never reached
// was still down when the interleaving Suspend swept, so that Suspend recorded
// nothing for it and the newer Resume never claimed it. Nobody owns it after
// that - it sits disconnected with the app in the foreground, no polling and no
// sync monitor, until the next Login - which is the same stranding the staleness
// check exists to prevent, just reached from the other side.
//
// Three wallets, parked in the second, so the third is the one no one claims.
func TestStaleResumeRearmsUnreachedWallets(t *testing.T) {
	assetIDs := []uint32{42, 0, 60}
	rig, wallets, twallets := newSuspendRig(t, assetIDs...)
	defer rig.shutdown()
	tCore := rig.core

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	wallets, twallets = resumeOrder(t, tCore, assetIDs, wallets, twallets)

	for _, tw := range twallets {
		tw.bal = new(asset.Balance)
	}

	// Park in the middle wallet's balance refresh, as in
	// TestResumeInterleavedWithNewerResume, and for the same reason: Connect
	// holds connectMtx, so a Suspend would serialize behind it rather than
	// interleave. The wallet behind the parked one is never touched.
	entered, release := make(chan struct{}), make(chan struct{})
	var parked int32
	twallets[1].balanceHook = func() {
		if !atomic.CompareAndSwapInt32(&parked, 0, 1) {
			return
		}
		close(entered)
		<-release
	}

	stale := make(chan error, 1)
	go func() { stale <- tCore.Resume() }()
	<-entered

	unreached := wallets[2]
	if unreached.connected() {
		t.Fatalf("%s wallet should still be down; the stale Resume has not reached it",
			unbip(unreached.AssetID))
	}

	// A full background/foreground cycle underneath the parked Resume. The
	// Suspend can only record the two wallets that are up, so the third is
	// absent from the set this Resume claims.
	if err := tCore.Suspend(); err != nil {
		t.Fatalf("second Suspend: %v", err)
	}
	tCore.suspendMtx.Lock()
	claimedByNewer := append([]uint32(nil), tCore.suspendedWallets...)
	tCore.suspendMtx.Unlock()
	for _, assetID := range claimedByNewer {
		if assetID == unreached.AssetID {
			t.Fatal("test no longer exercises the gap: the interleaving Suspend saw the unreached wallet")
		}
	}
	if err := tCore.Resume(); err != nil {
		t.Fatalf("second Resume: %v", err)
	}

	close(release)
	if err := <-stale; err != nil {
		t.Fatalf("stale Resume: %v", err)
	}

	// The wallets the newer session brought up are its business, and untouched.
	for _, w := range wallets[:2] {
		if !w.connected() {
			t.Fatalf("%s wallet torn down by a stale Resume after a newer one brought it up",
				unbip(w.AssetID))
		}
	}

	// The one it could not have claimed is handed back, so a foreground retries
	// it. Recorded and latched both: a set with the flag clear is unreachable,
	// since Resume returns early on it.
	tCore.suspendMtx.Lock()
	suspended := tCore.suspended
	rearmed := append([]uint32(nil), tCore.suspendedWallets...)
	tCore.suspendMtx.Unlock()
	if len(rearmed) != 1 || rearmed[0] != unreached.AssetID {
		t.Fatalf("expected the unreached %s wallet re-armed, got %v",
			unbip(unreached.AssetID), rearmed)
	}
	if !suspended {
		t.Fatal("wallets re-armed with the pause flag clear; the next Resume would return early")
	}

	if err := tCore.Resume(); err != nil {
		t.Fatalf("retry Resume: %v", err)
	}
	for _, w := range wallets {
		if !w.connected() {
			t.Fatalf("%s wallet still down after the retry foreground", unbip(w.AssetID))
		}
	}
}

// TestResumeInterleavedWithRelogin is the same root cause reached through the
// lock tier instead of the pause tier. App.Lock and App.Unlock take App.mtx,
// which Pause and Resume deliberately do not, so an autolock and the biometric
// unlock behind it can both complete under an in-flight Resume. Logout bumps the
// generation and Login does not restore it, so the stale Resume finds a moved
// generation, a clear suspended flag, and a live session that owns the wallet it
// is about to disconnect - closing an asset.Opener's handle in the process,
// which no later Resume can reopen without the app password.
func TestResumeInterleavedWithRelogin(t *testing.T) {
	rig, wallets, twallets := newSuspendRig(t, 42)
	defer rig.shutdown()
	tCore, w, tw := rig.core, wallets[0], twallets[0]
	tw.bal = new(asset.Balance)

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	var parked int32
	tw.balanceHook = func() {
		if !atomic.CompareAndSwapInt32(&parked, 0, 1) {
			return
		}
		close(entered)
		<-release
	}

	stale := make(chan error, 1)
	go func() { stale <- tCore.Resume() }()
	<-entered

	if err := tCore.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	// The unlock that follows. Login sets loggedIn under loginMtx and releases
	// it before connectWallets brings the wallets back up.
	tCore.loginMtx.Lock()
	tCore.loggedIn = true
	tCore.loginMtx.Unlock()
	tCore.connectWallets(rig.crypter)
	if !w.connected() {
		t.Fatal("login did not reconnect the wallet")
	}

	close(release)
	if err := <-stale; err != nil {
		t.Fatalf("stale Resume: %v", err)
	}

	if !w.connected() {
		t.Fatal("wallet disconnected by a stale Resume after a fresh login brought it up")
	}
}

// TestResumeInterleavedWithLoginConnect covers the interleaving
// TestResumeInterleavedWithRelogin cannot: that one releases the stale Resume
// after connectWallets has run to completion, so every wallet is already up by
// the time the unwind inspects them. The window that matters is earlier. Login
// publishes loggedIn and releases loginMtx before it starts dialing, so a stale
// Resume can return while the whole set is still down - down because the login
// has not reached it yet, not because a session change stranded it. Reading
// that as unreached re-arms the set and latches the pause under the login, and
// connectWallets' end-of-connect sweep then tears down everything it just
// connected, leaving a foregrounded app with no wallets and no Resume coming.
func TestResumeInterleavedWithLoginConnect(t *testing.T) {
	rig, wallets, twallets := newSuspendRig(t, 42, 0)
	defer rig.shutdown()
	tCore := rig.core
	for _, tw := range twallets {
		tw.bal = new(asset.Balance)
	}

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// Park the stale Resume in its first reconnect. balanceHook runs after
	// xcWallet.Connect has released connectMtx, so the login's own dial of the
	// same wallet is not blocked behind it.
	entered, release := make(chan struct{}), make(chan struct{})
	var parked int32
	for _, tw := range twallets {
		tw.balanceHook = func() {
			if !atomic.CompareAndSwapInt32(&parked, 0, 1) {
				return
			}
			close(entered)
			<-release
		}
	}

	stale := make(chan error, 1)
	go func() { stale <- tCore.Resume() }()
	<-entered

	if err := tCore.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	// Park the login inside its connect, and release the stale Resume there:
	// loggedIn is published, the wallets are still down, and the unwind is
	// deciding what to do about them. connectHook runs at the top of Connect,
	// so the first wallet the login dials is still disconnected when the stale
	// Resume unwinds.
	loginEntered, loginRelease := make(chan struct{}), make(chan struct{})
	var loginParked int32
	for _, tw := range twallets {
		tw.connectHook = func() {
			if !atomic.CompareAndSwapInt32(&loginParked, 0, 1) {
				return
			}
			close(loginEntered)
			<-loginRelease
		}
	}

	login := make(chan error, 1)
	go func() { login <- tCore.Login(tPW) }()
	<-loginEntered

	close(release)
	if err := <-stale; err != nil {
		t.Fatalf("stale Resume: %v", err)
	}

	close(loginRelease)
	if err := <-login; err != nil {
		t.Fatalf("Login: %v", err)
	}

	for _, w := range wallets {
		if !w.connected() {
			t.Fatalf("%s wallet left disconnected after a fresh login", unbip(w.AssetID))
		}
	}
	// The flag is the mechanism: latched here, the next Suspend is a no-op on
	// an already-suspended core and the wallets that are up never get swept.
	tCore.suspendMtx.Lock()
	suspended, tracked := tCore.suspended, len(tCore.suspendedWallets)
	tCore.suspendMtx.Unlock()
	if suspended {
		t.Fatal("stale Resume latched the pause under a fresh login")
	}
	if tracked != 0 {
		t.Fatalf("stale Resume handed back %d wallets the login was bringing up", tracked)
	}
}

// TestSuspendBeforeLoginLatches covers the pause window that opens before
// TestSuspendDuringLoginConnect's. Login runs the app pass KDF and the seed
// decrypt before it sets loggedIn, and both are slow, so a pause can land on a
// core that has no session yet but is moments away from connecting every wallet.
// Returning early on a logged-out core would latch nothing, leaving those
// wallets polling through a background the app believes is quiet, with the next
// Resume a no-op on a clear flag.
func TestSuspendBeforeLoginLatches(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	assetIDs := []uint32{42 /* dcr */, 0 /* btc */}
	xcws := make([]*xcWallet, 0, len(assetIDs))
	for i, assetID := range assetIDs {
		w, _ := newTWallet(assetID)
		// Not connected yet: connectWallets is what brings these up.
		w.hookedUp = false
		w.pw = encode.PassBytes(fmt.Sprintf("suspendpw%d", i))
		tCore.wallets[assetID] = w
		xcws = append(xcws, w)
	}

	// loggedIn is still false: the pause landed while Login was deriving the
	// encryption key.
	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	tCore.suspendMtx.Lock()
	latched := tCore.suspended
	tCore.suspendMtx.Unlock()
	if !latched {
		t.Fatal("a pause that landed before login set loggedIn did not latch")
	}

	tCore.loginMtx.Lock()
	tCore.loggedIn = true
	tCore.loginMtx.Unlock()
	tCore.connectWallets(rig.crypter)

	for _, w := range xcws {
		if w.connected() {
			t.Fatalf("%s wallet left polling by a pause that landed before login", unbip(w.AssetID))
		}
	}
	// Recorded, not merely disconnected: a wallet torn down but left out of the
	// set is stranded exactly the way an unrecorded one that stayed up is.
	tCore.suspendMtx.Lock()
	tracked := len(tCore.suspendedWallets)
	tCore.suspendMtx.Unlock()
	if tracked != len(assetIDs) {
		t.Fatalf("expected %d wallets recorded for the next Resume, got %d", len(assetIDs), tracked)
	}

	if err := tCore.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for _, w := range xcws {
		if !w.connected() {
			t.Fatalf("%s wallet not reconnected by Resume", unbip(w.AssetID))
		}
	}
}

// TestResumeWithoutSession covers the foreground that follows a pause latched
// before there was a session - the app backgrounded on the login screen. There
// is nothing to reconnect, but the latch must still be cleared, and quietly: an
// error here is a spurious toast on an ordinary foreground, and a latch left
// standing would make the next login's sweep suspend every wallet it connects.
func TestResumeWithoutSession(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := tCore.Resume(); err != nil {
		t.Fatalf("Resume with no session: %v", err)
	}

	tCore.suspendMtx.Lock()
	latched := tCore.suspended
	tCore.suspendMtx.Unlock()
	if latched {
		t.Fatal("pause left latched by a foreground with no session")
	}
}

// TestResumeStopsAfterFailedReconnect covers the staleness re-check on the error
// path. The unwind is what lets App.Stop drain in-flight lifecycle calls before
// it cancels the core, and a reconnect fails by burning a dial timeout, so
// checking only after a successful one is precisely backwards: Resume would keep
// going for one more timeout per wallet left in the set, with a termination
// waiting behind it.
func TestResumeStopsAfterFailedReconnect(t *testing.T) {
	rig, wallets, twallets := newSuspendRig(t, 42, 0)
	defer rig.shutdown()
	tCore := rig.core

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	wallets, twallets = resumeOrder(t, tCore, []uint32{42, 0}, wallets, twallets)

	// Park the first reconnect, then fail it: xcWallet.Connect calls SyncStatus
	// after ConnectOnce and unwinds the connection when it errors, so
	// connectAndUpdateWallet returns an error the way a dead backend would.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	twallets[0].connectHook = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	twallets[0].syncStatus = func() (bool, float32, error) {
		return false, 0, errors.New("backend is gone")
	}

	var secondAttempts int32
	twallets[1].connectHook = func() { atomic.AddInt32(&secondAttempts, 1) }

	resumed := make(chan error, 1)
	go func() { resumed <- tCore.Resume() }()
	<-entered

	if err := tCore.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	close(release)
	if err := <-resumed; err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if n := atomic.LoadInt32(&secondAttempts); n != 0 {
		t.Fatalf("Resume tried %d more reconnects after the session changed", n)
	}
	if wallets[1].connected() {
		t.Fatal("wallet reconnected after the session changed")
	}
}

// TestResumeRetriesFailedReconnect covers recovery from a reconnect that fails.
// A foreground is the likeliest place for one — the radio is still coming back
// up — and nothing else would ever bring the wallet back: connectWallets runs
// only on Login, a disconnected wallet is invisible to the next Suspend's
// teardown loop, and the sync monitor exits with the very context the reconnect
// was meant to replace. So Resume re-arms the pause with the wallets it could
// not reach, and the next foreground retries them.
func TestResumeRetriesFailedReconnect(t *testing.T) {
	rig, wallets, twallets := newSuspendRig(t, 42, 0)
	defer rig.shutdown()
	tCore := rig.core

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	wallets, twallets = resumeOrder(t, tCore, []uint32{42, 0}, wallets, twallets)
	failing, healthy := wallets[0], wallets[1]

	// Fail the first wallet's reconnect once. xcWallet.Connect calls SyncStatus
	// after ConnectOnce and unwinds the connection when it errors, so
	// connectAndUpdateWallet returns an error the way a dead backend would.
	failNext := int32(1)
	twallets[0].syncStatus = func() (bool, float32, error) {
		if atomic.CompareAndSwapInt32(&failNext, 1, 0) {
			return false, 0, errors.New("radio is still coming up")
		}
		return true, 1, nil
	}

	if err := tCore.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if failing.connected() {
		t.Fatal("wallet should be disconnected after a failed reconnect")
	}
	if !healthy.connected() {
		t.Fatal("the other wallet should have resumed")
	}

	tCore.suspendMtx.Lock()
	rearmed := tCore.suspended && len(tCore.suspendedWallets) == 1 &&
		tCore.suspendedWallets[0] == failing.AssetID
	tCore.suspendMtx.Unlock()
	if !rearmed {
		t.Fatal("the failed wallet was not re-armed for the next Resume")
	}

	// Re-arming leaves the core marked suspended in the foreground, so the next
	// background must still quiet the wallet that did come back up rather than
	// short-circuiting on the flag.
	if err := tCore.Suspend(); err != nil {
		t.Fatalf("second Suspend: %v", err)
	}
	if healthy.connected() {
		t.Fatal("a Suspend that landed while already suspended left a wallet polling")
	}

	if err := tCore.Resume(); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if !failing.connected() {
		t.Fatal("the failed wallet was never retried")
	}
	if !healthy.connected() {
		t.Fatal("wallet not reconnected by the second Resume")
	}

	tCore.suspendMtx.Lock()
	clean := !tCore.suspended && len(tCore.suspendedWallets) == 0
	tCore.suspendMtx.Unlock()
	if !clean {
		t.Fatal("suspend state should be clear once every wallet is back")
	}
}

// TestSuspendDuringLoginConnect covers a pause that lands inside login's connect
// phase. Login sets loggedIn and releases loginMtx before connectWallets brings
// the wallets up in parallel, and the WebView login path calls core.Login
// without App.mtx, so a Pause can observe a logged-in session and walk a wallet
// set that is still coming up. suspend() reports false for a wallet that is not
// connected yet, so that walk records nothing while still latching the pause:
// the app would go on polling every wallet through a background it believes is
// quiet.
//
// The parked wallet is an Opener, and it parks in OpenWithPW rather than in
// Connect. A wallet already inside xcWallet.Connect holds connectMtx, which
// xcWallet.suspend takes, so Suspend serializes against that one and handles it
// correctly. The gap is the wallet that has not reached Connect yet — which for
// xmr, the one real Opener, is the whole open phase, the slowest part of login.
func TestSuspendDuringLoginConnect(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	assetIDs := []uint32{42 /* dcr */, 0 /* btc */}
	xcws := make([]*xcWallet, 0, len(assetIDs))
	for i, assetID := range assetIDs {
		w, tw := newTWallet(assetID)
		// Not connected yet: connectWallets is what brings these up.
		w.hookedUp = false
		w.pw = encode.PassBytes(fmt.Sprintf("suspendpw%d", i))
		tCore.wallets[assetID] = w
		xcws = append(xcws, w)
		if i > 0 {
			continue
		}
		opener := &tOpenerWallet{TXCWallet: tw}
		w.Wallet = opener
		w.traits = asset.DetermineWalletTraits(opener)
		w.connector = util.NewConnectionMaster(opener)
	}

	// Login has already flipped this and dropped loginMtx by the time it calls
	// connectWallets.
	tCore.loginMtx.Lock()
	tCore.loggedIn = true
	tCore.loginMtx.Unlock()

	// Park the Opener's open so the Suspend runs before it reaches Connect.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	xcws[0].Wallet.(*tOpenerWallet).openHook = func() {
		once.Do(func() { close(entered) })
		<-release
	}

	connected := make(chan struct{})
	go func() {
		defer close(connected)
		tCore.connectWallets(rig.crypter)
	}()
	<-entered

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	close(release)
	<-connected

	for _, w := range xcws {
		if w.connected() {
			t.Fatalf("%s wallet left polling by a pause that landed during login", unbip(w.AssetID))
		}
	}

	tCore.suspendMtx.Lock()
	suspended, tracked := tCore.suspended, len(tCore.suspendedWallets)
	tCore.suspendMtx.Unlock()
	if !suspended {
		t.Fatal("core should be suspended")
	}
	// Recorded, not merely disconnected: a wallet torn down but left out of the
	// set is stranded exactly the way an unrecorded one that stayed up is.
	if tracked != len(assetIDs) {
		t.Fatalf("expected %d wallets recorded for the next Resume, got %d", len(assetIDs), tracked)
	}

	if err := tCore.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for _, w := range xcws {
		if !w.connected() {
			t.Fatalf("%s wallet not reconnected by Resume", unbip(w.AssetID))
		}
	}
}

// tOpenerWallet is a TXCWallet that also satisfies asset.Opener, standing in for
// xmr — the only asset whose wallet handle is deliberately left open across a
// suspend so resume needs no password.
type tOpenerWallet struct {
	*TXCWallet

	// openHook, when non-nil, is called at the top of OpenWithPW. Unlike
	// connectHook it runs before xcWallet.Connect takes connectMtx, so a test
	// can park a wallet in the part of login's connect phase that a Suspend
	// walks straight past rather than serializing against.
	openHook func()

	// closeHook, when non-nil, is called at the top of Close.
	closeHook func()

	openMtx sync.Mutex
	open    bool
}

func (w *tOpenerWallet) OpenWithPW(context.Context, []byte) error {
	if w.openHook != nil {
		w.openHook()
	}
	w.openMtx.Lock()
	defer w.openMtx.Unlock()
	w.open = true
	return nil
}

func (w *tOpenerWallet) IsOpen() bool {
	w.openMtx.Lock()
	defer w.openMtx.Unlock()
	return w.open
}

func (w *tOpenerWallet) Close() error {
	if w.closeHook != nil {
		w.closeHook()
	}
	w.openMtx.Lock()
	defer w.openMtx.Unlock()
	w.open = false
	return nil
}

// TestLogoutAfterSuspendClosesOpener covers the other half of the eviction: a
// suspended asset.Opener stays open by design, and xcWallet.Disconnect is the
// only caller of Close, so Logout must disconnect unconditionally rather than
// skipping the wallets a prior Suspend already dropped.
func TestLogoutAfterSuspendClosesOpener(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	var assetID uint32 = 42
	xcw, tw := newTWallet(assetID)
	opener := &tOpenerWallet{TXCWallet: tw}
	xcw.Wallet = opener
	xcw.traits = asset.DetermineWalletTraits(opener)
	xcw.connector = util.NewConnectionMaster(opener)
	xcw.hookedUp = false
	xcw.pw = encode.PassBytes("openerpw")
	tCore.wallets[assetID] = xcw

	if err := opener.OpenWithPW(tCore.ctx, nil); err != nil {
		t.Fatalf("OpenWithPW: %v", err)
	}
	if err := xcw.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tCore.loginMtx.Lock()
	tCore.loggedIn = true
	tCore.loginMtx.Unlock()
	xPriv, err := deriveMultisigXPriv(encode.RandomBytes(32))
	if err != nil {
		t.Fatalf("deriveMultisigXPriv: %v", err)
	}
	tCore.multisigXPriv = xPriv

	if err := tCore.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if !opener.IsOpen() {
		t.Fatal("Suspend must leave an Opener wallet open for a password-free resume")
	}

	if err := tCore.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if opener.IsOpen() {
		t.Fatal("Opener wallet still open after Suspend+Logout")
	}
}

// tSeededDriver is a tDriver that also implements asset.Creator, writing a
// wallet marker into the asset data directory the way the on-disk creation of
// a real seeded wallet does. Exists reports on that marker.
type tSeededDriver struct {
	*tDriver
	createErr error
	creates   int
}

func (d *tSeededDriver) marker(dataDir string) string {
	return filepath.Join(dataDir, "wallet.db")
}

func (d *tSeededDriver) Exists(_, dataDir string, _ map[string]string, _ util.Network) (bool, error) {
	_, err := os.Stat(d.marker(dataDir))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (d *tSeededDriver) Create(params *asset.CreateWalletParams) error {
	d.creates++
	// A creation that fails partway still leaves the directory behind, like a
	// real driver would.
	if err := os.MkdirAll(params.DataDir, 0o700); err != nil {
		return err
	}
	if d.createErr != nil {
		return d.createErr
	}
	return os.WriteFile(d.marker(params.DataDir), []byte("wallet"), 0o600)
}

// TestCreateSeededWalletDataCleanup checks that a failed seeded-wallet creation
// leaves the asset data directory as it found it, whichever step fails. Data
// written by the failed call is removed, so that a retry does not fail with an
// already-exists error. Data that was already on disk, as an interrupted
// creation leaves it, is used rather than recreated and survives a failure.
func TestCreateSeededWalletDataCleanup(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	tCore.cfg.DBPath = filepath.Join(t.TempDir(), "core.db")

	a := *tUTXOAssetA
	tSeeded := &a
	tSeeded.Symbol = "vtc"
	tSeeded.ID, _ = util.BipSymbolID(tSeeded.Symbol)

	winfo := *tWalletInfo
	winfo.AvailableWallets = []*asset.WalletDefinition{{
		Type:   "type",
		Seeded: true,
	}}

	// An asset.Opener stands in for the wallet, so the test can check that a
	// wallet the creation opened is closed again before its data is discarded.
	_, tWallet := newTWallet(tSeeded.ID)
	opener := &tOpenerWallet{TXCWallet: tWallet}
	drv := &tSeededDriver{tDriver: &tDriver{
		wallet:        opener,
		decodedCoinID: "vtc-coin",
		winfo:         &winfo,
	}}
	asset.Register(tSeeded.ID, drv, true)

	form := &WalletForm{
		AssetID: tSeeded.ID,
		Config:  map[string]string{},
		Type:    "type",
	}
	dataDir := tCore.assetDataDirectory(tSeeded.ID)
	marker := drv.marker(dataDir)

	opener.closeHook = func() {
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("wallet closed after its data was discarded (stat err = %v)", err)
		}
	}

	ensureErr := func(tag string) {
		t.Helper()
		if err := tCore.CreateWallet(tPW, nil, form); err == nil {
			t.Fatalf("no %s error", tag)
		}
	}
	ensureNoData := func(tag string) {
		t.Helper()
		if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
			t.Fatalf("%s: asset data directory survived a failed creation (stat err = %v)", tag, err)
		}
	}
	ensureData := func(tag string) {
		t.Helper()
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("%s: wallet data missing: %v", tag, err)
		}
	}
	ensureCreates := func(tag string, n int) {
		t.Helper()
		if drv.creates != n {
			t.Fatalf("%s: %d on-disk creations, want %d", tag, drv.creates, n)
		}
	}
	ensureClosed := func(tag string) {
		t.Helper()
		if opener.IsOpen() {
			t.Fatalf("%s: wallet left open by a failed creation", tag)
		}
	}

	// On-disk creation fails partway.
	drv.createErr = errTest
	ensureErr("Create")
	drv.createErr = nil
	ensureNoData("Create")
	ensureCreates("Create", 1)

	// Connection failure after the wallet is created on disk and opened.
	tWallet.connectErr = errTest
	ensureErr("Connect")
	tWallet.connectErr = nil
	ensureClosed("Connect")
	ensureNoData("Connect")
	ensureCreates("Connect", 2)

	// Balance failure, past the connect step.
	tWallet.balErr = errTest
	ensureErr("Balance")
	tWallet.balErr = nil
	ensureClosed("Balance")
	ensureNoData("Balance")
	ensureCreates("Balance", 3)

	// Storing the wallet fails.
	rig.db.updateWalletErr = errTest
	ensureErr("db.UpdateWallet")
	rig.db.updateWalletErr = nil
	ensureClosed("db.UpdateWallet")
	ensureNoData("db.UpdateWallet")
	ensureCreates("db.UpdateWallet", 4)

	// Success keeps the data.
	if err := tCore.CreateWallet(tPW, nil, form); err != nil {
		t.Fatalf("error creating wallet: %v", err)
	}
	ensureCreates("success", 5)
	ensureData("success")

	// Wallet data left on disk with no configured wallet, as an interrupted
	// creation leaves it, is used rather than recreated.
	delete(tCore.wallets, tSeeded.ID)
	if err := tCore.CreateWallet(tPW, nil, form); err != nil {
		t.Fatalf("error creating wallet from existing data: %v", err)
	}
	ensureCreates("existing data", 5)

	// Existing data is not the failed creation's to discard.
	delete(tCore.wallets, tSeeded.ID)
	tWallet.connectErr = errTest
	ensureErr("Connect with existing data")
	tWallet.connectErr = nil
	ensureClosed("Connect with existing data")
	ensureData("Connect with existing data")
	ensureCreates("Connect with existing data", 5)

	// A wallet that failed to load at startup is absent from the wallets map
	// but keeps both its db record and its data. A creation attempted for it
	// fails to load the wallet again and must leave the data alone.
	delete(tCore.wallets, tSeeded.ID)
	rig.db.wallet = &db.Wallet{AssetID: tSeeded.ID, Type: "type", Settings: map[string]string{}}
	drv.openErr = errTest
	ensureErr("load")
	drv.openErr = nil
	rig.db.wallet = nil
	ensureData("load")
	ensureCreates("load", 5)
}
