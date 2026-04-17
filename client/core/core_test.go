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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/db"
	dbtest "decred.org/dcrdex/client/db/test"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/encrypt"
	"decred.org/dcrdex/dex/wait"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
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
	tCtx          context.Context
	tUTXOAssetA   = &dex.Asset{
		ID:         42,
		Symbol:     "dcr",
		Version:    0, // match the stubbed (*TXCWallet).Info result
		MaxFeeRate: 10,
		SwapConf:   1,
	}
	tSwapSizeA uint64 = 251

	tUTXOAssetB = &dex.Asset{
		ID:         0,
		Symbol:     "btc",
		Version:    0, // match the stubbed (*TXCWallet).Info result
		MaxFeeRate: 2,
		SwapConf:   1,
	}
	tSwapSizeB uint64 = 225

	tACCTAsset = &dex.Asset{
		ID:         60,
		Symbol:     "eth",
		Version:    0, // match the stubbed (*TXCWallet).Info result
		MaxFeeRate: 20,
		SwapConf:   1,
	}
	tPW                        = []byte("dexpw")
	wPW                        = []byte("walletpw")
	tErr                       = fmt.Errorf("test error")
	tFee                uint64 = 1e8
	tFeeAsset           uint32 = 42
	tSwapFeesPaid       uint64 = 500
	tRedemptionFeesPaid uint64 = 350
	tLogger                    = dex.StdOutLogger("TCORE", dex.LevelInfo)
	tMaxFeeRate         uint64 = 10
	tWalletInfo                = &asset.WalletInfo{
		SupportedVersions: []uint32{0},
		UnitInfo: dex.UnitInfo{
			Conventional: dex.Denomination{
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
	recryptErr      error
}

func (tdb *TDB) Run(context.Context) {}

func (tdb *TDB) UpdateWallet(wallet *db.Wallet) error {
	tdb.wallet = wallet
	return tdb.updateWalletErr
}

func (tdb *TDB) SetWalletPassword(wid []byte, newPW []byte) error {
	return tdb.setWalletPwErr
}

func (tdb *TDB) UpdateBalance(wid []byte, balance *db.Balance) error {
	return nil
}

func (tdb *TDB) UpdateWalletStatus(wid []byte, disable bool) error {
	return nil
}

func (tdb *TDB) Wallets() ([]*db.Wallet, error) {
	return nil, nil
}

func (tdb *TDB) Wallet([]byte) (*db.Wallet, error) {
	return tdb.wallet, tdb.walletErr
}

func (tdb *TDB) SaveNotification(*db.Notification) error            { return nil }
func (tdb *TDB) BackupTo(dst string, overwrite, compact bool) error { return nil }
func (tdb *TDB) NotificationsN(int) ([]*db.Notification, error)     { return nil, nil }
func (tdb *TDB) SavePokes([]*db.Notification) error                 { return nil }
func (tdb *TDB) LoadPokes() ([]*db.Notification, error)             { return nil, nil }

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
func (tdb *TDB) SetSeedGenerationTime(time uint64) error {
	return nil
}
func (tdb *TDB) SeedGenerationTime() (uint64, error) {
	return 0, nil
}
func (tdb *TDB) DisabledRateSources() ([]string, error) {
	return nil, nil
}
func (tdb *TDB) SaveDisabledRateSources(disableSources []string) error {
	return nil
}
func (tdb *TDB) Recrypt(creds *db.PrimaryCredentials, oldCrypter, newCrypter encrypt.Crypter) (
	walletUpdates map[uint32][]byte, acctUpdates map[string][]byte, err error) {

	if tdb.recryptErr != nil {
		return nil, nil, tdb.recryptErr
	}

	return nil, nil, nil
}

func (tdb *TDB) Backup() error {
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

func (tdb *TDB) NextMultisigKeyIndex(assetID uint32) (uint32, error) {
	return 0, nil
}

func (tdb *TDB) StoreMultisigIndexForPubkey(assetID, idx uint32, pubkey [33]byte) error {
	return nil
}

func (tdb *TDB) MultisigIndexForPubkey(assetID uint32, pubkey [33]byte) (uint32, error) {
	return 0, nil
}

type tCoin struct {
	id []byte

	val uint64
}

func (c *tCoin) ID() dex.Bytes {
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

type tReceipt struct {
	coin       *tCoin
	contract   []byte
	expiration time.Time
}

func (r *tReceipt) Coin() asset.Coin {
	return r.coin
}

func (r *tReceipt) Contract() dex.Bytes {
	return r.contract
}

func (r *tReceipt) Expiration() time.Time {
	return r.expiration
}

func (r *tReceipt) String() string {
	return r.coin.String()
}

func (r *tReceipt) SignedRefund() dex.Bytes {
	return nil
}

type TXCWallet struct {
	swapSize            uint64
	sendFeeSuggestion   uint64
	sendCoin            *tCoin
	sendErr             error
	addrErr             error
	signCoinErr         error
	lastSwapsMtx        sync.Mutex
	lastSwaps           []*asset.Swaps
	lastRedeems         []*asset.RedeemForm
	swapReceipts        []asset.Receipt
	swapCounter         int
	swapErr             error
	auditInfo           *asset.AuditInfo
	auditInfoFunc       func(coinID, contract, txData dex.Bytes) (*asset.AuditInfo, error)
	auditErr            error
	auditChan           chan struct{}
	refundCoin          dex.Bytes
	refundErr           error
	refundFeeSuggestion uint64
	redeemCoins         []dex.Bytes
	redeemCounter       int
	redeemFeeSuggestion uint64
	redeemErr           error
	redeemErrChan       chan error
	badSecret           bool
	fundedVal           uint64
	fundedSwaps         uint64
	connectErr          error
	unlockErr           error
	balErr              error
	bal                 *asset.Balance
	fundingMtx          sync.RWMutex
	fundingCoins        asset.Coins
	fundRedeemScripts   []dex.Bytes
	returnedCoins       asset.Coins
	fundingCoinErr      error
	lockErr             error
	locked              bool
	changeCoin          *tCoin
	syncStatus          func() (bool, float32, error)
	confsMtx            sync.RWMutex
	confs               map[string]uint32
	confsErr            map[string]error
	preSwapForm         *asset.PreSwapForm
	preSwap             *asset.PreSwap
	preRedeemForm       *asset.PreRedeemForm
	preRedeem           *asset.PreRedeem
	ownsAddress         bool
	ownsAddressErr      error
	pubKeys             []dex.Bytes
	sigs                []dex.Bytes
	feeCoin             []byte
	makeRegFeeTxErr     error
	feeCoinSent         []byte
	sendTxnErr          error
	contractExpired     bool
	contractLockTime    time.Time
	accelerationParams  *struct {
		swapCoins                 []dex.Bytes
		accelerationCoins         []dex.Bytes
		changeCoin                dex.Bytes
		feeSuggestion             uint64
		newFeeRate                uint64
		requiredForRemainingSwaps uint64
	}
	newAccelerationTxID         string
	newChangeCoinID             *dex.Bytes
	preAccelerateSwapRate       uint64
	preAccelerateSuggestedRange asset.XYRange
	accelerationEstimate        uint64
	accelerateOrderErr          error
	info                        *asset.WalletInfo
	bondTxCoinID                []byte
	refundBondCoin              asset.Coin
	refundBondErr               error
	makeBondTxErr               error
	reserves                    atomic.Uint64
	findBond                    *asset.BondDetails
	findBondErr                 error
	maxSwaps, maxRedeems        int

	confirmTxResult *asset.ConfirmTxStatus
	confirmTxErr    error
	confirmTxCalled bool

	estFee    uint64
	estFeeErr error
	validAddr bool

	returnedAddr      string
	returnedContracts [][]byte
	redemptionAddr    string
}

var _ asset.Accelerator = (*TXCWallet)(nil)
var _ asset.Withdrawer = (*TXCWallet)(nil)

func newTWallet(assetID uint32) (*xcWallet, *TXCWallet) {
	w := &TXCWallet{
		changeCoin:       &tCoin{id: encode.RandomBytes(36)},
		syncStatus:       func() (synced bool, progress float32, err error) { return true, 1, nil },
		confs:            make(map[string]uint32),
		confsErr:         make(map[string]error),
		ownsAddress:      true,
		contractLockTime: time.Now().Add(time.Minute),
		lastSwaps:        make([]*asset.Swaps, 0),
		lastRedeems:      make([]*asset.RedeemForm, 0),
		info: &asset.WalletInfo{
			SupportedVersions: []uint32{0},
		},
		bondTxCoinID: encode.RandomBytes(32),
	}
	var broadcasting uint32 = 1
	xcWallet := &xcWallet{
		log:               tLogger,
		supportedVersions: w.info.SupportedVersions,
		Wallet:            w,
		Symbol:            dex.BipIDSymbol(assetID),
		connector:         dex.NewConnectionMaster(w),
		AssetID:           assetID,
		hookedUp:          true,
		dbID:              encode.Uint32Bytes(assetID),
		encPass:           []byte{0x01},
		peerCount:         1,
		syncStatus:        &asset.SyncStatus{Synced: true},
		pw:                tPW,
		traits:            asset.DetermineWalletTraits(w),
		broadcasting:      &broadcasting,
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
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		<-ctx.Done()
		wg.Done()
	}()
	return &wg, w.connectErr
}

func (w *TXCWallet) Balance() (*asset.Balance, error) {
	if w.balErr != nil {
		return nil, w.balErr
	}
	if w.bal == nil {
		w.bal = new(asset.Balance)
	}
	return w.bal, nil
}

func (w *TXCWallet) ConfirmTransaction(coinID dex.Bytes, confirmTx *asset.ConfirmTx, feeSuggestion uint64) (*asset.ConfirmTxStatus, error) {
	w.confirmTxCalled = true
	return w.confirmTxResult, w.confirmTxErr
}

func (w *TXCWallet) FundOrder(ord *asset.Order) (asset.Coins, []dex.Bytes, uint64, error) {
	w.fundedVal = ord.Value
	w.fundedSwaps = ord.MaxSwapCount
	return w.fundingCoins, w.fundRedeemScripts, 0, w.fundingCoinErr
}

func (w *TXCWallet) MaxOrder(*asset.MaxOrderForm) (*asset.SwapEstimate, error) {
	return nil, nil
}

func (w *TXCWallet) PreSwap(form *asset.PreSwapForm) (*asset.PreSwap, error) {
	w.preSwapForm = form
	return w.preSwap, nil
}

func (w *TXCWallet) PreRedeem(form *asset.PreRedeemForm) (*asset.PreRedeem, error) {
	w.preRedeemForm = form
	return w.preRedeem, nil
}
func (w *TXCWallet) RedemptionFees() (uint64, error) { return 0, nil }

func (w *TXCWallet) ReturnCoins(coins asset.Coins) error {
	w.fundingMtx.Lock()
	defer w.fundingMtx.Unlock()
	w.returnedCoins = coins
	coinInSlice := func(coin asset.Coin) bool {
		for _, c := range coins {
			if bytes.Equal(c.ID(), coin.ID()) {
				return true
			}
		}
		return false
	}

	for _, c := range w.fundingCoins {
		if coinInSlice(c) {
			continue
		}
		return errors.New("not found")
	}
	return nil
}

func (w *TXCWallet) FundingCoins([]dex.Bytes) (asset.Coins, error) {
	return w.fundingCoins, w.fundingCoinErr
}

func (w *TXCWallet) Swap(_ context.Context, swaps *asset.Swaps) ([]asset.Receipt, asset.Coin, uint64, error) {
	w.swapCounter++
	w.lastSwapsMtx.Lock()
	w.lastSwaps = append(w.lastSwaps, swaps)
	w.lastSwapsMtx.Unlock()
	if w.swapErr != nil {
		return nil, nil, 0, w.swapErr
	}
	return w.swapReceipts, w.changeCoin, tSwapFeesPaid, nil
}

func (w *TXCWallet) Redeem(_ context.Context, form *asset.RedeemForm) ([]dex.Bytes, asset.Coin, uint64, error) {
	w.redeemFeeSuggestion = form.FeeSuggestion
	defer func() {
		if w.redeemErrChan != nil {
			w.redeemErrChan <- w.redeemErr
		}
	}()
	w.lastRedeems = append(w.lastRedeems, form)
	w.redeemCounter++
	if w.redeemErr != nil {
		return nil, nil, 0, w.redeemErr
	}
	return w.redeemCoins, &tCoin{id: []byte{0x0c, 0x0d}}, tRedemptionFeesPaid, nil
}

func (w *TXCWallet) SignCoinMessage(asset.Coin, dex.Bytes) (pubkeys, sigs []dex.Bytes, err error) {
	return w.pubKeys, w.sigs, w.signCoinErr
}

func (w *TXCWallet) AuditContract(coinID, contract, txData dex.Bytes, rebroadcast bool) (*asset.AuditInfo, error) {
	defer func() {
		if w.auditChan != nil {
			w.auditChan <- struct{}{}
		}
	}()
	if w.auditInfoFunc != nil {
		return w.auditInfoFunc(coinID, contract, txData)
	}
	return w.auditInfo, w.auditErr
}

func (w *TXCWallet) LockTimeExpired(_ context.Context, lockTime time.Time) (bool, error) {
	return w.contractExpired, nil
}

func (w *TXCWallet) ContractLockTimeExpired(_ context.Context, contract dex.Bytes) (bool, time.Time, error) {
	return w.contractExpired, w.contractLockTime, nil
}

func (w *TXCWallet) FindRedemption(ctx context.Context, coinID, _ dex.Bytes) (redemptionCoin, secret dex.Bytes, err error) {
	return nil, nil, fmt.Errorf("not mocked")
}

func (w *TXCWallet) Refund(_ context.Context, refundCoin dex.Bytes, refundContract dex.Bytes, feeSuggestion uint64) (dex.Bytes, error) {
	w.refundFeeSuggestion = feeSuggestion
	return w.refundCoin, w.refundErr
}

func (w *TXCWallet) DepositAddress() (string, error) {
	return "", w.addrErr
}

func (w *TXCWallet) RedemptionAddress() (string, error) {
	return w.redemptionAddr, w.addrErr
}

func (w *TXCWallet) NewAddress() (string, error) {
	return "", w.addrErr
}

func (w *TXCWallet) AddressUsed(addr string) (bool, error) {
	return false, nil
}

func (w *TXCWallet) Unlock(pw []byte) error {
	return w.unlockErr
}

func (w *TXCWallet) Lock() error {
	return w.lockErr
}

func (w *TXCWallet) Locked() bool {
	return w.locked
}

func (w *TXCWallet) ConfirmTime(id dex.Bytes, nConfs uint32) (time.Time, error) {
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

func (w *TXCWallet) ValidateSecret(secret, secretHash []byte) bool {
	return !w.badSecret
}

func (w *TXCWallet) SyncStatus() (*asset.SyncStatus, error) {
	synced, progress, err := w.syncStatus()
	if err != nil {
		return nil, err
	}
	blocks := uint64(math.Round(float64(progress) * 100))
	return &asset.SyncStatus{Synced: synced, TargetHeight: blocks, Blocks: blocks}, nil
}

func (w *TXCWallet) setConfs(coinID dex.Bytes, confs uint32, err error) {
	id := coinID.String()
	w.confsMtx.Lock()
	w.confs[id] = confs
	w.confsErr[id] = err
	w.confsMtx.Unlock()
}

func (w *TXCWallet) tConfirmations(_ context.Context, coinID dex.Bytes) (uint32, error) {
	id := coinID.String()
	w.confsMtx.RLock()
	defer w.confsMtx.RUnlock()
	return w.confs[id], w.confsErr[id]
}

func (w *TXCWallet) SwapConfirmations(ctx context.Context, coinID dex.Bytes, contract dex.Bytes, matchTime time.Time) (uint32, bool, error) {
	confs, err := w.tConfirmations(ctx, coinID)
	return confs, false, err
}

func (w *TXCWallet) RegFeeConfirmations(ctx context.Context, coinID dex.Bytes) (uint32, error) {
	return w.tConfirmations(ctx, coinID)
}

func (w *TXCWallet) FeesForRemainingSwaps(n, feeRate uint64) uint64 {
	return n * feeRate * w.swapSize
}
func (w *TXCWallet) AccelerateOrder(swapCoins, accelerationCoins []dex.Bytes, changeCoin dex.Bytes, requiredForRemainingSwaps, newFeeRate uint64) (asset.Coin, string, error) {
	if w.accelerateOrderErr != nil {
		return nil, "", w.accelerateOrderErr
	}

	w.accelerationParams = &struct {
		swapCoins                 []dex.Bytes
		accelerationCoins         []dex.Bytes
		changeCoin                dex.Bytes
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

func (w *TXCWallet) PreAccelerate(swapCoins, accelerationCoins []dex.Bytes, changeCoin dex.Bytes, requiredForRemainingSwaps, feeSuggestion uint64) (uint64, *asset.XYRange, *asset.EarlyAcceleration, error) {
	if w.accelerateOrderErr != nil {
		return 0, nil, nil, w.accelerateOrderErr
	}

	w.accelerationParams = &struct {
		swapCoins                 []dex.Bytes
		accelerationCoins         []dex.Bytes
		changeCoin                dex.Bytes
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

func (w *TXCWallet) SingleLotSwapRefundFees(version uint32, feeRate uint64, useSafeTxSize bool) (uint64, uint64, error) {
	return 0, 0, nil
}

func (w *TXCWallet) SingleLotRedeemFees(version uint32, feeRate uint64) (uint64, error) {
	return 0, nil
}

func (w *TXCWallet) StandardSendFee(uint64) uint64 { return 1 }

func (w *TXCWallet) AccelerationEstimate(swapCoins, accelerationCoins []dex.Bytes, changeCoin dex.Bytes, requiredForRemainingSwaps, newFeeRate uint64) (uint64, error) {
	if w.accelerateOrderErr != nil {
		return 0, w.accelerateOrderErr
	}

	w.accelerationParams = &struct {
		swapCoins                 []dex.Bytes
		accelerationCoins         []dex.Bytes
		changeCoin                dex.Bytes
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

func (w *TXCWallet) ReturnRedemptionAddress(addr string) {
	w.returnedAddr = addr
}
func (w *TXCWallet) ReturnRefundContracts(contracts [][]byte) {
	w.returnedContracts = contracts
}
func (w *TXCWallet) MaxFundingFees(_ uint32, _ uint64, _ map[string]string) uint64 {
	return 0
}

func (*TXCWallet) FundMultiOrder(ord *asset.MultiOrder, maxLock uint64) (coins []asset.Coins, redeemScripts [][]dex.Bytes, fundingFees uint64, err error) {
	return nil, nil, 0, nil
}

var _ asset.Bonder = (*TXCWallet)(nil)

func (*TXCWallet) BondsFeeBuffer(feeRate uint64) uint64 {
	return 4 * 1000 * feeRate * 2
}

func (w *TXCWallet) SetBondReserves(reserves uint64) {
	w.reserves.Store(reserves)
}

func (w *TXCWallet) RefundBond(ctx context.Context, ver uint16, coinID, script []byte, amt uint64, privKey *secp256k1.PrivateKey) (asset.Coin, error) {
	return w.refundBondCoin, w.refundBondErr
}

func (w *TXCWallet) FindBond(ctx context.Context, coinID []byte, searchUntil time.Time) (bond *asset.BondDetails, err error) {
	return w.findBond, w.findBondErr
}

func (w *TXCWallet) MakeBondTx(ver uint16, amt, feeRate uint64, lockTime time.Time, privKey *secp256k1.PrivateKey, acctID []byte) (*asset.Bond, func(), error) {
	if w.makeBondTxErr != nil {
		return nil, nil, w.makeBondTxErr
	}
	return &asset.Bond{
		Version: ver,
		AssetID: tFeeAsset,
		Amount:  amt,
		CoinID:  w.bondTxCoinID,
	}, func() {}, nil
}

func (w *TXCWallet) TxHistory(*asset.TxHistoryRequest) (*asset.TxHistoryResponse, error) {
	return nil, nil
}
func (w *TXCWallet) WalletTransaction(ctx context.Context, txID string) (*asset.WalletTransaction, error) {
	return nil, nil
}

func (w *TXCWallet) PendingTransactions(ctx context.Context) []*asset.WalletTransaction {
	return nil
}

var _ asset.MaxMatchesCounter = (*TXCWallet)(nil)

func (w *TXCWallet) MaxSwaps(serverVer uint32, feeRate uint64) (int, error) {
	return w.maxSwaps, nil
}
func (w *TXCWallet) MaxRedeems(serverVer uint32) (int, error) {
	return w.maxRedeems, nil
}

type TAccountLocker struct {
	*TXCWallet
	reserveNRedemptions    uint64
	reserveNRedemptionsErr error
	reReserveRedemptionErr error
	redemptionUnlocked     uint64
	reservedRedemption     uint64

	reserveNRefunds    uint64
	reserveNRefundsErr error
	reReserveRefundErr error
	refundUnlocked     uint64
	reservedRefund     uint64
}

var _ asset.AccountLocker = (*TAccountLocker)(nil)

func newTAccountLocker(assetID uint32) (*xcWallet, *TAccountLocker) {
	xcWallet, tWallet := newTWallet(assetID)
	accountLocker := &TAccountLocker{TXCWallet: tWallet}
	xcWallet.Wallet = accountLocker
	return xcWallet, accountLocker
}

func (w *TAccountLocker) ReserveNRedemptions(n uint64, ver uint32, maxFeeRate uint64, lotSize uint64) (uint64, error) {
	return w.reserveNRedemptions, w.reserveNRedemptionsErr
}

func (w *TAccountLocker) ReReserveRedemption(v uint64) error {
	w.fundingMtx.Lock()
	defer w.fundingMtx.Unlock()
	w.reservedRedemption += v
	return w.reReserveRedemptionErr
}

func (w *TAccountLocker) UnlockRedemptionReserves(v uint64) {
	w.fundingMtx.Lock()
	defer w.fundingMtx.Unlock()
	w.redemptionUnlocked += v
}

func (w *TAccountLocker) ReserveNRefunds(n uint64, ver uint32, maxFeeRate uint64) (uint64, error) {
	return w.reserveNRefunds, w.reserveNRefundsErr
}

func (w *TAccountLocker) UnlockRefundReserves(v uint64) {
	w.fundingMtx.Lock()
	defer w.fundingMtx.Unlock()
	w.refundUnlocked += v
}

func (w *TAccountLocker) ReReserveRefund(v uint64) error {
	w.fundingMtx.Lock()
	defer w.fundingMtx.Unlock()
	w.reservedRefund += v
	return w.reReserveRefundErr
}

type TFeeRater struct {
	*TXCWallet
	feeRate uint64
}

func (w *TFeeRater) FeeRate() uint64 {
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

func tFetcher(_ context.Context, log dex.Logger, _ map[uint32]*SupportedAsset) map[uint32]float64 {
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
			ctx:           ctx,
			cfg:           &Config{},
			db:            tdb,
			log:           tLogger,
			wallets:       make(map[uint32]*xcWallet),
			blockWaiters:  make(map[string]*blockWaiter),
			newCrypter:    func([]byte) encrypt.Crypter { return crypter },
			reCrypter:     func([]byte, []byte) (encrypt.Crypter, error) { return crypter, crypter.recryptErr },
			noteChans:     make(map[uint64]chan Notification),
			tipPending:    make(map[uint32]uint64),
			tipActive:     make(map[uint32]bool),
			balPending:    make(map[uint32]*asset.Balance),
			balActive:     make(map[uint32]bool),

			fiatRateSources:  make(map[string]*commonRateSource),
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
	tILT.ID, _ = dex.BipSymbolID(tILT.Symbol)

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
	rig.crypter.(*tCrypter).encryptErr = tErr
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
	tWallet.connectErr = tErr
	ensureErr("Connect")
	tWallet.connectErr = nil

	// Unlock error.
	tWallet.unlockErr = tErr
	ensureErr("Unlock")
	tWallet.unlockErr = nil

	// Address error.
	tWallet.addrErr = tErr
	ensureErr("Address")
	tWallet.addrErr = nil

	// Balance error.
	tWallet.balErr = tErr
	ensureErr("Balance")
	tWallet.balErr = nil

	// Database error.
	rig.db.updateWalletErr = tErr
	ensureErr("db.UpdateWallet")
	rig.db.updateWalletErr = nil

	// Success
	delete(tCore.wallets, tILT.ID)
	err := tCore.CreateWallet(tPW, wPW, form)
	if err != nil {
		t.Fatalf("error when should be no error: %v", err)
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
	rig.db.setCredsErr = tErr
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

	// Successful
	coin, err := tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if coin.Value() != 1e8 {
		t.Fatalf("Expected sent value to be %v, got %v", 1e8, coin.Value())
	}

	// 0 value
	_, err = tCore.Send(tPW, tUTXOAssetA.ID, 0, address, false)
	if err == nil {
		t.Fatalf("no error for zero value send")
	}

	// no wallet
	_, err = tCore.Send(tPW, 12345, 1e8, address, false)
	if err == nil {
		t.Fatalf("no error for unknown wallet")
	}

	// connect error
	wallet.hookedUp = false
	tWallet.connectErr = tErr
	_, err = tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false)
	if err == nil {
		t.Fatalf("no error for wallet connect error")
	}
	tWallet.connectErr = nil

	// Send error
	tWallet.sendErr = tErr
	_, err = tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false)
	if err == nil {
		t.Fatalf("no error for wallet send error")
	}
	tWallet.sendErr = nil

	// Check the coin.
	tWallet.sendCoin = &tCoin{id: []byte{'a'}}
	coin, err = tCore.Send(tPW, tUTXOAssetA.ID, 3e8, address, false)
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

	// So far, the fee suggestion should have always been zero.
	if tWallet.sendFeeSuggestion != 0 {
		t.Fatalf("unexpected non-zero fee rate when no books or responses prepared")
	}

	const feeRate = 54321

	feeRater := &TFeeRater{
		TXCWallet: tWallet,
		feeRate:   feeRate,
	}

	wallet.Wallet = feeRater

	coin, err = tCore.Send(tPW, tUTXOAssetA.ID, 2e8, address, false)
	if err != nil {
		t.Fatalf("FeeRater Withdraw/send error: %v", err)
	}
	if coin.Value() != 2e8 {
		t.Fatalf("Expected sent value to be %v, got %v", 2e8, coin.Value())
	}

	if tWallet.sendFeeSuggestion != feeRate {
		t.Fatalf("unexpected fee rate from FeeRater. wanted %d, got %d", feeRate, tWallet.sendFeeSuggestion)
	}

	// wallet is not synced
	wallet.syncStatus.Synced = false
	_, err = tCore.Send(tPW, tUTXOAssetA.ID, 1e8, address, false)
	if err == nil {
		t.Fatalf("Expected error for a non-synchronized wallet")
	}
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
	rig.db.walletErr = tErr
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
	rig.crypter.(*tCrypterSmart).recryptErr = tErr
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
	if !strings.Contains(err.Error(), "unabled to decode provided seed") {
		t.Fatalf("wrong error for invalid seed length: %v", err)
	}

	// Want incorrect seed error.
	rig.crypter.(*tCrypterSmart).recryptErr = tErr
	// tCrypter is used to encode the orginal seed but we don't need it here, so
	// we need to add 8 bytes to commplete the expected seed lenght(64).
	err = tCore.ResetAppPass(tPW, seed+"blah")
	if !strings.Contains(err.Error(), "unabled to decode provided seed") {
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
	rig.crypter.(*tCrypter).recryptErr = tErr
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
	tXyzWallet.connectErr = tErr
	err = tCore.SetWalletPassword(tPW, assetID, newPW)
	if !errorHasCode(err, connectionErr) {
		t.Fatalf("wrong error for connection error: %v", err)
	}
	xyzWallet.hookedUp = true
	tXyzWallet.connectErr = nil

	// Unlock error
	tXyzWallet.unlockErr = tErr
	err = tCore.SetWalletPassword(tPW, assetID, newPW)
	if !errorHasCode(err, authErr) {
		t.Fatalf("wrong error for auth error: %v", err)
	}
	tXyzWallet.unlockErr = nil

	// SetWalletPassword db error
	rig.db.setWalletPwErr = tErr
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
		value             uint64
		subtract          bool
		wantMissingWallet bool
		wantErr           bool
	}{{
		name:     "ok",
		asset:    tUTXOAssetA.ID,
		subtract: true,
		estFee:   1e8,
		value:    1e8,
	}, {
		name:     "zero amount",
		asset:    tACCTAsset.ID,
		subtract: true,
		wantErr:  true,
	}, {
		name:     "subtract true and not withdrawer",
		asset:    tACCTAsset.ID,
		subtract: true,
		wantErr:  true,
		value:    1e8,
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

		tWallet.estFeeErr = nil
		if test.wantErr {
			tWallet.estFeeErr = tErr
		}
		estimate, _, err := tCore.EstimateSendTxFee("addr", test.asset, test.value, test.subtract, false)
		if test.wantErr {
			if err != nil {
				continue
			}
			t.Fatalf("%s: expected error", test.name)
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		if estimate != test.estFee {
			t.Fatalf("%s: expected fee %v, got %v", test.name, test.estFee, estimate)
		}
		if !test.wantErr && err != nil {
			t.Fatalf("%s: unexpected error", test.name)
		}
	}
}

type TDynamicSwapper struct {
	*TXCWallet
	tfpPaid         uint64
	tfpSecretHashes [][]byte
	tfpErr          error
}

func (dtfc *TDynamicSwapper) DynamicSwapFeesPaid(ctx context.Context, coinID, contractData dex.Bytes) (uint64, [][]byte, error) {
	return dtfc.tfpPaid, dtfc.tfpSecretHashes, dtfc.tfpErr
}
func (dtfc *TDynamicSwapper) DynamicRedemptionFeesPaid(ctx context.Context, coinID, contractData dex.Bytes) (uint64, [][]byte, error) {
	return dtfc.tfpPaid, dtfc.tfpSecretHashes, dtfc.tfpErr
}
func (dtfc *TDynamicSwapper) GasFeeLimit() uint64 {
	return 200
}

var _ asset.DynamicSwapper = (*TDynamicSwapper)(nil)

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
