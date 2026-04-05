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
	mrand "math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/comms"
	"decred.org/dcrdex/client/db"
	dbtest "decred.org/dcrdex/client/db/test"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/encrypt"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	ordertest "decred.org/dcrdex/dex/order/test"
	"decred.org/dcrdex/dex/wait"
	"decred.org/dcrdex/server/account"
	serverdex "decred.org/dcrdex/server/dex"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var rand = mrand.New(mrand.NewPCG(0xbadc0de, 0xdeadbeef))

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
	tCtx           context.Context
	dcrBtcLotSize  uint64 = 1e7
	dcrBtcRateStep uint64 = 10
	tUTXOAssetA           = &dex.Asset{
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
	tDexPriv            *secp256k1.PrivateKey
	tDexKey             *secp256k1.PublicKey
	tPW                        = []byte("dexpw")
	wPW                        = []byte("walletpw")
	tDexHost                   = "somedex.tld:7232"
	tDcrBtcMktName             = "dcr_btc"
	tBtcEthMktName             = "btc_eth"
	tErr                       = fmt.Errorf("test error")
	tFee                uint64 = 1e8
	tFeeAsset           uint32 = 42
	tUnparseableHost           = string([]byte{0x7f})
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
	dcrBondAsset = &msgjson.BondAsset{ID: 42, Amt: tFee, Confs: 1}
)

type tMsg = *msgjson.Message
type msgFunc = func(*msgjson.Message)

func uncovertAssetInfo(ai *dex.Asset) *msgjson.Asset {
	return &msgjson.Asset{
		Symbol:     ai.Symbol,
		ID:         ai.ID,
		Version:    ai.Version,
		MaxFeeRate: ai.MaxFeeRate,
		SwapConf:   uint16(ai.SwapConf),
	}
}

func makeAcker(serializer func(msg *msgjson.Message) msgjson.Signable) func(msg *msgjson.Message, f msgFunc) error {
	return func(msg *msgjson.Message, f msgFunc) error {
		signable := serializer(msg)
		sigMsg := signable.Serialize()
		sig := signMsg(tDexPriv, sigMsg)
		ack := &msgjson.Acknowledgement{
			Sig: sig,
		}
		resp, _ := msgjson.NewResponse(msg.ID, ack, nil)
		f(resp)
		return nil
	}
}

var (
	invalidAcker = func(msg *msgjson.Message, f msgFunc) error {
		resp, _ := msgjson.NewResponse(msg.ID, msg, nil)
		f(resp)
		return nil
	}
	initAcker = makeAcker(func(msg *msgjson.Message) msgjson.Signable {
		init := new(msgjson.Init)
		msg.Unmarshal(init)
		return init
	})
	redeemAcker = makeAcker(func(msg *msgjson.Message) msgjson.Signable {
		redeem := new(msgjson.Redeem)
		msg.Unmarshal(redeem)
		return redeem
	})
)

type TWebsocket struct {
	mtx            sync.RWMutex
	id             uint64
	sendErr        error
	sendMsgErrChan chan *msgjson.Error
	reqErr         error
	connectErr     error
	msgs           <-chan *msgjson.Message
	// handlers simulates a peer (server) response for request, and handles the
	// response with the msgFunc.
	handlers       map[string][]func(*msgjson.Message, msgFunc) error
	submittedBond  *msgjson.PostBond
	liveBondExpiry uint64
}

func newTWebsocket() *TWebsocket {
	return &TWebsocket{
		msgs:     make(<-chan *msgjson.Message),
		handlers: make(map[string][]func(*msgjson.Message, msgFunc) error),
	}
}

func tNewAccount(crypter *tCrypter) *dexAccount {
	privKey, _ := secp256k1.GeneratePrivateKey()
	encKey, err := crypter.Encrypt(privKey.Serialize())
	if err != nil {
		panic(err)
	}
	return &dexAccount{
		host:      tDexHost,
		encKey:    encKey,
		dexPubKey: tDexKey,
		privKey:   privKey,
		id:        account.NewID(privKey.PubKey().SerializeCompressed()),
		// feeAssetID is 0 (btc)
		// tier, bonds, etc. set on auth
		pendingBondsConfs: make(map[string]uint32),
		rep:               account.Reputation{BondedTier: 1}, // not suspended by default
	}
}

func testDexConnection(ctx context.Context, crypter *tCrypter) (*dexConnection, *TWebsocket, *dexAccount) {
	conn := newTWebsocket()
	connMaster := dex.NewConnectionMaster(conn)
	connMaster.Connect(ctx)
	acct := tNewAccount(crypter)
	return &dexConnection{
		WsConn:     conn,
		log:        tLogger,
		connMaster: connMaster,
		ticker:     newDexTicker(time.Millisecond * 1000 / 3),
		acct:       acct,
		assets: map[uint32]*dex.Asset{
			tUTXOAssetA.ID: tUTXOAssetA,
			tUTXOAssetB.ID: tUTXOAssetB,
			tACCTAsset.ID:  tACCTAsset,
		},
		cfg: &msgjson.ConfigResult{
			APIVersion:       serverdex.PerMatchAddrVersion,
			DEXPubKey:        acct.dexPubKey.SerializeCompressed(),
			CancelMax:        0.8,
			BroadcastTimeout: 1000, // 1000 ms for faster expiration, but ticker fires fast
			Assets: []*msgjson.Asset{
				uncovertAssetInfo(tUTXOAssetA),
				uncovertAssetInfo(tUTXOAssetB),
				uncovertAssetInfo(tACCTAsset),
			},
			Markets: []*msgjson.Market{
				{
					Name:            tDcrBtcMktName,
					Base:            tUTXOAssetA.ID,
					Quote:           tUTXOAssetB.ID,
					LotSize:         dcrBtcLotSize,
					ParcelSize:      1,
					RateStep:        dcrBtcRateStep,
					EpochLen:        60000,
					MarketBuyBuffer: 1.1,
					MarketStatus: msgjson.MarketStatus{
						StartEpoch: 12, // since the stone age
						FinalEpoch: 0,  // no scheduled suspend
						// Persist:   nil,
					},
				},
				{
					Name:            tBtcEthMktName,
					Base:            tUTXOAssetB.ID,
					Quote:           tACCTAsset.ID,
					LotSize:         dcrBtcLotSize,
					RateStep:        dcrBtcRateStep,
					EpochLen:        60000,
					MarketBuyBuffer: 1.1,
					MarketStatus: msgjson.MarketStatus{
						StartEpoch: 12,
						FinalEpoch: 0,
					},
				},
			},
			BondExpiry: 86400, // >0 make client treat as API v1
			BondAssets: map[string]*msgjson.BondAsset{
				"dcr": dcrBondAsset,
			},
			BinSizes: []string{"1h", "24h"},
		},
		notify:             func(Notification) {},
		dispatchTradeWork:  func(_ order.OrderID, fn func()) { fn() },
		trades:             make(map[order.OrderID]*trackedTrade),
		cancels:            make(map[order.OrderID]order.OrderID),
		inFlightOrders:     make(map[uint64]*InFlightOrder),
		epoch:              map[string]uint64{tDcrBtcMktName: 0},
		resolvedEpoch:      map[string]uint64{tDcrBtcMktName: 0},
		apiVer:             serverdex.PreAPIVersion,
		connectionStatus:   uint32(comms.Connected),
		reportingConnects:  1,
		spots:              make(map[string]*msgjson.Spot),
		activeCoinIDs:      make(map[string]order.MatchID),
		activeSecretHashes: make(map[string]order.MatchID),
		matchCoinIDs:       make(map[order.MatchID][]string),
		matchSecretHashes:  make(map[order.MatchID][]string),
	}, conn, acct
}

func (conn *TWebsocket) queueResponse(route string, handler func(*msgjson.Message, msgFunc) error) {
	conn.mtx.Lock()
	defer conn.mtx.Unlock()
	handlers := conn.handlers[route]
	if handlers == nil {
		handlers = make([]func(*msgjson.Message, msgFunc) error, 0, 1)
	}
	conn.handlers[route] = append(handlers, handler) // NOTE: handler is called by RequestWithTimeout
}

func (conn *TWebsocket) NextID() uint64 {
	conn.mtx.Lock()
	defer conn.mtx.Unlock()
	conn.id++
	return conn.id
}
func (conn *TWebsocket) Send(msg *msgjson.Message) error {
	if conn.sendMsgErrChan != nil {
		resp, err := msg.Response()
		if err != nil {
			return err
		}
		if resp.Error != nil {
			conn.sendMsgErrChan <- resp.Error
			return nil // the response was sent successfully
		}
	}

	return conn.sendErr
}

func (conn *TWebsocket) SendRaw([]byte) error {
	return conn.sendErr
}
func (conn *TWebsocket) Request(msg *msgjson.Message, f msgFunc) error {
	return conn.RequestWithTimeout(msg, f, 0, func() {})
}
func (conn *TWebsocket) RequestRaw(msgID uint64, rawMsg []byte, respHandler func(*msgjson.Message)) error {
	return nil
}
func (conn *TWebsocket) RequestWithTimeout(msg *msgjson.Message, f func(*msgjson.Message), _ time.Duration, _ func()) error {
	if conn.reqErr != nil {
		return conn.reqErr
	}
	conn.mtx.Lock()
	defer conn.mtx.Unlock()
	handlers := conn.handlers[msg.Route]
	if len(handlers) > 0 {
		handler := handlers[0]
		conn.handlers[msg.Route] = handlers[1:]
		return handler(msg, f)
	}
	return fmt.Errorf("no handler for route %q", msg.Route)
}
func (conn *TWebsocket) MessageSource() <-chan *msgjson.Message { return conn.msgs } // use when Core.listen is running
func (conn *TWebsocket) IsDown() bool {
	return false
}
func (conn *TWebsocket) Connect(context.Context) (*sync.WaitGroup, error) {
	// NOTE: tCore's wsConstructor just returns a reused conn, so we can't close
	// conn.msgs on ctx cancel. See the wsConstructor definition in newTestRig.
	// Consider reworking the tests (TODO).
	return &sync.WaitGroup{}, conn.connectErr
}

func (conn *TWebsocket) UpdateURL(string) {}

type TDB struct {
	updateWalletErr  error
	acct             *db.AccountInfo
	acctErr          error
	createAccountErr error
	// updateMatchHook is called during UpdateMatch if non-nil.
	updateMatchHook  func(m *db.MetaMatch)
	addBondErr       error
	updateOrderErr   error
	activeDEXOrders  []*db.MetaOrder
	allOrders        []*db.MetaOrder
	matchesForOID    []*db.MetaMatch
	matchesByOrderID map[order.OrderID][]*db.MetaMatch
	matchesForOIDErr error
	updateMatchChan  chan order.MatchStatus
	// For async-safe match update tracking
	matchUpdatesMtx          sync.Mutex
	matchUpdates             []order.MatchStatus
	matchUpdateCond          *sync.Cond
	activeMatchOIDs          []order.OrderID
	activeMatchOIDSErr       error
	lastStatusID             order.OrderID
	lastStatus               order.OrderStatus
	wallet                   *db.Wallet
	walletErr                error
	setWalletPwErr           error
	orderOrders              map[order.OrderID]*db.MetaOrder
	orderErr                 error
	linkedFromID             order.OrderID
	linkedToID               order.OrderID
	existValues              map[string]bool
	accountProofErr          error
	verifyCreateAccount      bool
	verifyUpdateAccountInfo  bool
	disabledHost             *string
	disableAccountErr        error
	creds                    *db.PrimaryCredentials
	setCredsErr              error
	legacyKeyErr             error
	recryptErr               error
	deleteInactiveOrdersErr  error
	archivedOrders           int
	deleteInactiveMatchesErr error
	archivedMatches          int
	updateAccountInfoErr     error
}

func (tdb *TDB) Run(context.Context) {}

func (tdb *TDB) ListAccounts() ([]string, error) {
	return nil, nil
}

func (tdb *TDB) Accounts() ([]*db.AccountInfo, error) {
	return []*db.AccountInfo{}, nil
}

func (tdb *TDB) Account(url string) (*db.AccountInfo, error) {
	return tdb.acct, tdb.acctErr
}

func (tdb *TDB) CreateAccount(ai *db.AccountInfo) error {
	tdb.verifyCreateAccount = true
	tdb.acct = ai
	return tdb.createAccountErr
}

func (tdb *TDB) NextBondKeyIndex(assetID uint32) (uint32, error) {
	return 0, nil
}

func (tdb *TDB) AddBond(host string, bond *db.Bond) error {
	return tdb.addBondErr
}

func (tdb *TDB) ConfirmBond(host string, assetID uint32, bondCoinID []byte) error {
	return nil
}
func (tdb *TDB) BondRefunded(host string, assetID uint32, bondCoinID []byte) error {
	return nil
}

func (tdb *TDB) ToggleAccountStatus(host string, disable bool) error {
	if disable {
		tdb.disabledHost = &host
	} else {
		tdb.disabledHost = nil
	}
	return tdb.disableAccountErr
}

func (tdb *TDB) UpdateAccountInfo(ai *db.AccountInfo) error {
	tdb.verifyUpdateAccountInfo = true
	tdb.acct = ai
	return tdb.updateAccountInfoErr
}

func (tdb *TDB) UpdateOrder(m *db.MetaOrder) error {
	return tdb.updateOrderErr
}

func (tdb *TDB) ActiveDEXOrders(dex string) ([]*db.MetaOrder, error) {
	return tdb.activeDEXOrders, nil
}

func (tdb *TDB) ActiveOrders() ([]*db.MetaOrder, error) {
	return nil, nil
}

func (tdb *TDB) AccountOrders(dex string, n int, since uint64) ([]*db.MetaOrder, error) {
	return nil, nil
}

func (tdb *TDB) Order(oid order.OrderID) (*db.MetaOrder, error) {
	if tdb.orderErr != nil {
		return nil, tdb.orderErr
	}
	return tdb.orderOrders[oid], nil
}

func (tdb *TDB) Orders(filter *db.OrderFilter) ([]*db.MetaOrder, error) {
	if tdb.allOrders == nil {
		return nil, nil
	}

	// Filter orders based on status
	var filtered []*db.MetaOrder
	for _, ord := range tdb.allOrders {
		// If no status filter, include all
		if len(filter.Statuses) == 0 {
			filtered = append(filtered, ord)
			continue
		}

		// Check if order status matches any of the filter statuses
		if slices.Contains(filter.Statuses, ord.MetaData.Status) {
			filtered = append(filtered, ord)
			continue
		}

		// Handle IncludePartial: include canceled/revoked orders with partial fills
		// when filtering for "executed" status
		if filter.IncludePartial && slices.Contains(filter.Statuses, order.OrderStatusExecuted) {
			if ord.MetaData.Status == order.OrderStatusCanceled ||
				ord.MetaData.Status == order.OrderStatusRevoked {
				// Check if order has trade matches (non-cancel matches indicating partial fill)
				oid := ord.Order.ID()
				if tdb.matchesByOrderID != nil {
					if matches := tdb.matchesByOrderID[oid]; len(matches) > 0 {
						// Check for non-cancel trade matches
						// A trade match has:
						// - Non-empty Address (not a cancel match for maker)
						// - If Status == MatchComplete, must have InitSig (not a cancel match for taker)
						for _, m := range matches {
							// Cancel match for maker has empty Address
							if m.UserMatch.Address == "" {
								continue
							}
							// Cancel match for taker has no InitSig and status MatchComplete
							if m.UserMatch.Status == order.MatchComplete {
								if m.MetaData == nil || len(m.MetaData.Proof.Auth.InitSig) == 0 {
									continue
								}
							}
							// Found a trade match - include this order
							filtered = append(filtered, ord)
							break
						}
					}
				}
			}
		}
	}

	return filtered, nil
}

func (tdb *TDB) MarketOrders(dex string, base, quote uint32, n int, since uint64) ([]*db.MetaOrder, error) {
	return nil, nil
}

func (tdb *TDB) UpdateOrderMetaData(order.OrderID, *db.OrderMetaData) error {
	return nil
}

func (tdb *TDB) UpdateOrderStatus(oid order.OrderID, status order.OrderStatus) error {
	tdb.lastStatusID = oid
	tdb.lastStatus = status
	return nil
}

func (tdb *TDB) LinkOrder(oid, linkedID order.OrderID) error {
	tdb.linkedFromID = oid
	tdb.linkedToID = linkedID
	return nil
}

func (tdb *TDB) UpdateMatch(m *db.MetaMatch) error {
	if tdb.updateMatchHook != nil {
		tdb.updateMatchHook(m)
	}
	// Non-blocking channel send for backward compatibility with tests
	// that still use the channel pattern.
	if tdb.updateMatchChan != nil {
		select {
		case tdb.updateMatchChan <- m.Status:
		default:
			// Channel full or no reader - don't block
		}
	}
	// Also track updates in a thread-safe slice for async-aware tests.
	tdb.matchUpdatesMtx.Lock()
	tdb.matchUpdates = append(tdb.matchUpdates, m.Status)
	if tdb.matchUpdateCond != nil {
		tdb.matchUpdateCond.Broadcast()
	}
	tdb.matchUpdatesMtx.Unlock()
	return nil
}

// resetMatchUpdates clears the tracked match updates.
func (tdb *TDB) resetMatchUpdates() {
	tdb.matchUpdatesMtx.Lock()
	tdb.matchUpdates = nil
	tdb.matchUpdatesMtx.Unlock()
}

// waitForMatchUpdate waits for at least n match updates within the timeout.
// Returns the updates received.
func (tdb *TDB) waitForMatchUpdate(n int, timeout time.Duration) []order.MatchStatus {
	deadline := time.Now().Add(timeout)
	tdb.matchUpdatesMtx.Lock()
	defer tdb.matchUpdatesMtx.Unlock()

	if tdb.matchUpdateCond == nil {
		tdb.matchUpdateCond = sync.NewCond(&tdb.matchUpdatesMtx)
	}

	// Wake up when the deadline expires so we don't block forever.
	timer := time.AfterFunc(timeout, func() {
		tdb.matchUpdateCond.Broadcast()
	})
	defer timer.Stop()

	for len(tdb.matchUpdates) < n && time.Now().Before(deadline) {
		tdb.matchUpdateCond.Wait()
	}
	result := make([]order.MatchStatus, len(tdb.matchUpdates))
	copy(result, tdb.matchUpdates)
	return result
}

func (tdb *TDB) ActiveMatches() ([]*db.MetaMatch, error) {
	return nil, nil
}

func (tdb *TDB) MatchesForOrder(oid order.OrderID, excludeCancels bool) ([]*db.MetaMatch, error) {
	// Use matchesByOrderID map if available for more accurate testing
	if tdb.matchesByOrderID != nil {
		return tdb.matchesByOrderID[oid], tdb.matchesForOIDErr
	}
	return tdb.matchesForOID, tdb.matchesForOIDErr
}

func (tdb *TDB) DEXOrdersWithActiveMatches(dex string) ([]order.OrderID, error) {
	return tdb.activeMatchOIDs, tdb.activeMatchOIDSErr
}

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

func (tdb *TDB) DeleteInactiveOrders(ctx context.Context, olderThan *time.Time, perBatchFn func(ords *db.MetaOrder) error) (int, error) {
	return tdb.archivedOrders, tdb.deleteInactiveOrdersErr
}

func (tdb *TDB) DeleteInactiveMatches(ctx context.Context, olderThan *time.Time, perBatchFn func(mtchs *db.MetaMatch, isSell bool) error) (int, error) {
	return tdb.archivedMatches, tdb.deleteInactiveMatchesErr
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
func (tdb *TDB) StoreMMEpochSnapshot(host string, snap *msgjson.MMEpochSnapshot) error {
	return nil
}
func (tdb *TDB) MMEpochSnapshots(host string, base, quote uint32, startEpoch, endEpoch uint64) ([]*msgjson.MMEpochSnapshot, error) {
	return nil, nil
}
func (tdb *TDB) PruneMMEpochSnapshots(host string, base, quote uint32, minEpochIdx uint64) (int, error) {
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
		AssetID: dcrBondAsset.ID,
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

var tAssetID uint32

func randomAsset() *msgjson.Asset {
	tAssetID++
	return &msgjson.Asset{
		Symbol:  "BT" + strconv.Itoa(int(tAssetID)),
		ID:      tAssetID,
		Version: tAssetID * 2,
	}
}

func randomMsgMarket() (baseAsset, quoteAsset *msgjson.Asset) {
	return randomAsset(), randomAsset()
}

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
	ws       *TWebsocket
	dc       *dexConnection
	acct     *dexAccount
	crypter  encrypt.Crypter
}

func newTestRig() *testRig {
	tdb := &TDB{
		orderOrders:  make(map[order.OrderID]*db.MetaOrder),
		wallet:       &db.Wallet{},
		existValues:  map[string]bool{},
		legacyKeyErr: tErr,
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
	dc, conn, acct := testDexConnection(ctx, crypter) // crypter makes acct.encKey consistent with privKey

	ai := &db.AccountInfo{
		Host:      "somedex.com",
		Cert:      acct.cert,
		DEXPubKey: acct.dexPubKey,
		EncKeyV2:  acct.encKey,
	}
	tdb.acct = ai

	shutdown := func() {
		cancel()
		wg.Wait()
		dc.connMaster.Wait()
	}

	rig := &testRig{
		shutdown: shutdown,
		core: &Core{
			ctx:           ctx,
			cfg:           &Config{},
			db:            tdb,
			log:           tLogger,
			lockTimeTaker: dex.LockTimeTaker(dex.Testnet),
			lockTimeMaker: dex.LockTimeMaker(dex.Testnet),
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
		ws:      conn,
		dc:      dc,
		acct:    acct,
		crypter: crypter,
	}

	rig.core.intl.Store(&locale{
		m:       originLocale,
		printer: message.NewPrinter(language.AmericanEnglish),
	})

	rig.core.InitializeClient(tPW, nil)

	// tCrypter doesn't actually use random bytes supplied by InitializeClient,
	// (the crypter is known ahead of time) but if that changes, we would need
	// to encrypt the acct.privKey here, after InitializeClient generates a new
	// random inner key/crypter: rig.resetAcctEncKey(tPW)

	return rig
}

// Encrypt acct.privKey -> acct.encKey if InitializeClient generates a new
// random inner key/crypter that is different from the one used on construction.
// Important if Core's crypters actually use their initialization data (random
// bytes for inner crypter and the pw for outer).
func (rig *testRig) resetAcctEncKey(pw []byte) error {
	innerCrypter, err := rig.core.encryptionKey(pw)
	if err != nil {
		return fmt.Errorf("encryptionKey error: %w", err)
	}
	encKey, err := innerCrypter.Encrypt(rig.acct.privKey.Serialize())
	if err != nil {
		return fmt.Errorf("crypter.Encrypt error: %w", err)
	}
	rig.acct.encKey = encKey
	return nil
}

func (rig *testRig) queueConfig() {
	rig.ws.queueResponse(msgjson.ConfigRoute, func(msg *msgjson.Message, f msgFunc) error {
		resp, _ := msgjson.NewResponse(msg.ID, rig.dc.cfg, nil)
		f(resp)
		return nil
	})
}

func (rig *testRig) queuePrevalidateBond() {
	rig.ws.queueResponse(msgjson.PreValidateBondRoute, func(msg *msgjson.Message, f msgFunc) error {
		preEval := new(msgjson.PreValidateBond)
		msg.Unmarshal(preEval)

		preEvalResult := &msgjson.PreValidateBondResult{
			AccountID: rig.dc.acct.id[:],
			AssetID:   preEval.AssetID,
			Amount:    dcrBondAsset.Amt,
			// Expiry: ,
		}
		sign(tDexPriv, preEvalResult)
		resp, _ := msgjson.NewResponse(msg.ID, preEvalResult, nil)
		f(resp)
		return nil
	})
}

func (rig *testRig) queuePostBond(postBondResult *msgjson.PostBondResult) {
	rig.ws.queueResponse(msgjson.PostBondRoute, func(msg *msgjson.Message, f msgFunc) error {
		bond := new(msgjson.PostBond)
		msg.Unmarshal(bond)
		rig.ws.submittedBond = bond
		postBondResult.BondID = bond.CoinID
		sign(tDexPriv, postBondResult)
		resp, _ := msgjson.NewResponse(msg.ID, postBondResult, nil)
		f(resp)
		return nil
	})
}

func (rig *testRig) queueConnect(rpcErr *msgjson.Error, matches []*msgjson.Match, orders []*msgjson.OrderStatus, suspended ...bool) {
	rig.ws.queueResponse(msgjson.ConnectRoute, func(msg *msgjson.Message, f msgFunc) error {
		if rpcErr != nil {
			resp, _ := msgjson.NewResponse(msg.ID, nil, rpcErr)
			f(resp)
			return nil
		}

		connect := new(msgjson.Connect)
		msg.Unmarshal(connect)
		sign(tDexPriv, connect)

		activeBonds := make([]*msgjson.Bond, 0, 1)
		if b := rig.ws.submittedBond; b != nil {
			activeBonds = append(activeBonds, &msgjson.Bond{
				Version: b.Version,
				Amount:  dcrBondAsset.Amt,
				Expiry:  rig.ws.liveBondExpiry,
				CoinID:  b.CoinID,
				AssetID: b.AssetID,
			})
		}

		result := &msgjson.ConnectResult{
			Sig:                 connect.Sig,
			ActiveMatches:       matches,
			ActiveOrderStatuses: orders,
			ActiveBonds:         activeBonds,
			Score:               10,
			Reputation:          &account.Reputation{BondedTier: 1},
		}
		if len(suspended) > 0 && suspended[0] {
			result.Reputation.Penalties = 1
		}
		resp, _ := msgjson.NewResponse(msg.ID, result, nil)
		f(resp)
		return nil
	})
}

func (rig *testRig) queueCancel(rpcErr *msgjson.Error) {
	rig.ws.queueResponse(msgjson.CancelRoute, func(msg *msgjson.Message, f msgFunc) error {
		var resp *msgjson.Message
		if rpcErr == nil {
			// Need to stamp and sign the message with the server's key.
			msgOrder := new(msgjson.CancelOrder)
			err := msg.Unmarshal(msgOrder)
			if err != nil {
				rpcErr = msgjson.NewError(msgjson.RPCParseError, "unable to unmarshal request")
			} else {
				co := convertMsgCancelOrder(msgOrder)
				resp = orderResponse(msg.ID, msgOrder, co, false, false, false)
			}
		}
		if rpcErr != nil {
			resp, _ = msgjson.NewResponse(msg.ID, nil, rpcErr)
		}
		f(resp)
		return nil
	})
}

func TestMain(m *testing.M) {
	var shutdown context.CancelFunc
	tCtx, shutdown = context.WithCancel(context.Background())
	tDexPriv, _ = secp256k1.GeneratePrivateKey()
	tDexKey = tDexPriv.PubKey()

	doIt := func() int {
		// Not counted as coverage, must test Archiver constructor explicitly.
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

func TestMarkets(t *testing.T) {
	t.Skip("removed DEX trading layer")
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

// TODO: TestGetDEXConfig

func TestGetFee(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestPostBond(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestCredentialsUpgrade(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestLogin(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestAccountNotFoundError(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestInitializeDEXConnectionsSuccess(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestConnectDEX(t *testing.T) { t.Skip("DEX connectivity removed in Phase 2") }

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

func trade(t *testing.T, async bool) {
}

func TestTrade(t *testing.T) { t.Skip("DEX connectivity removed in Phase 2") }

func TestTradeAsync(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestRefundReserves(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestRedemptionReserves(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestCancel(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestHandlePreimageRequest(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestHandleRevokeOrderMsg(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestHandleRevokeMatchMsg(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestTradeTracking(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestReconcileTrades(t *testing.T) {
	t.Skip("removed: DEX trading functionality")
}

func makeTradeTracker(rig *testRig, walletSet *walletSet, force order.TimeInForce, status order.OrderStatus) *trackedTrade {
	qty := 4 * dcrBtcLotSize
	lo, dbOrder, preImg, _ := makeLimitOrder(rig.dc, true, qty, dcrBtcRateStep)
	lo.Force = force
	dbOrder.MetaData.Status = status

	return newTrackedTrade(dbOrder, preImg, rig.dc,
		rig.core.lockTimeTaker, rig.core.lockTimeMaker,
		rig.db, rig.queue, walletSet, nil, rig.core.notify,
		rig.core.formatDetails, &rig.core.wg)
}

func TestRefunds(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestNotifications(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestResolveActiveTrades(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestReReserveFunding(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestCompareServerMatches(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestHandleEpochOrderMsg(t *testing.T) { t.Skip("order book functionality removed") }

func makeMatchProof(preimages []order.Preimage, commitments []order.Commitment) (msgjson.Bytes, msgjson.Bytes, error) {
	if len(preimages) != len(commitments) {
		return nil, nil, fmt.Errorf("expected equal number of preimages and commitments")
	}

	sbuff := make([]byte, 0, len(preimages)*order.PreimageSize)
	cbuff := make([]byte, 0, len(commitments)*order.CommitmentSize)
	for i := 0; i < len(preimages); i++ {
		sbuff = append(sbuff, preimages[i][:]...)
		cbuff = append(cbuff, commitments[i][:]...)
	}
	seed := blake256.Sum256(sbuff)
	csum := blake256.Sum256(cbuff)
	return seed[:], csum[:], nil
}

func TestHandleMatchProofMsg(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func Test_marketTrades(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestLogout(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestSetEpoch(t *testing.T) { t.Skip("order book functionality removed") }

func makeLimitOrder(dc *dexConnection, sell bool, qty, rate uint64) (*order.LimitOrder, *db.MetaOrder, order.Preimage, string) {
	return makeLimitOrderWithTiF(dc, sell, qty, rate, order.ImmediateTiF)
}

func makeLimitOrderWithTiF(dc *dexConnection, sell bool, qty, rate uint64, force order.TimeInForce) (*order.LimitOrder, *db.MetaOrder, order.Preimage, string) {
	preImg := newPreimage()
	addr := ordertest.RandomAddress()
	lo := &order.LimitOrder{
		P: order.Prefix{
			AccountID:  dc.acct.ID(),
			BaseAsset:  tUTXOAssetA.ID,
			QuoteAsset: tUTXOAssetB.ID,
			OrderType:  order.LimitOrderType,
			ClientTime: time.Now(),
			ServerTime: time.Now().Add(time.Millisecond),
			Commit:     preImg.Commit(),
		},
		T: order.Trade{
			// Coins needed?
			Sell:     sell,
			Quantity: qty,
			Address:  addr,
		},
		Rate:  rate,
		Force: force,
	}
	fromAsset, toAsset := tUTXOAssetB, tUTXOAssetA
	if sell {
		fromAsset, toAsset = tUTXOAssetA, tUTXOAssetB
	}
	dbOrder := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusEpoch,
			Host:   dc.acct.host,
			Proof: db.OrderProof{
				Preimage: preImg[:],
			},
			MaxFeeRate:   tMaxFeeRate,
			EpochDur:     dc.marketEpochDuration(tDcrBtcMktName),
			FromSwapConf: fromAsset.SwapConf,
			ToSwapConf:   toAsset.SwapConf,
		},
		Order: lo,
	}
	return lo, dbOrder, preImg, addr
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

func TestHandleTradeSuspensionMsg(t *testing.T) { t.Skip("order book functionality removed") }

func orderNoteFeed(tCore *Core) (orderNotes chan *OrderNote, done func()) {
	orderNotes = make(chan *OrderNote, 16)

	ntfnFeed := tCore.NotificationFeed()
	feedDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case n := <-ntfnFeed.C:
				if ordNote, ok := n.(*OrderNote); ok {
					orderNotes <- ordNote
				}
			case <-tCtx.Done():
				return
			case <-feedDone:
				return
			}
		}
	}()

	done = func() {
		close(feedDone) // close first on return
		wg.Wait()
	}
	return orderNotes, done
}

func verifyRevokeNotification(ch chan *OrderNote, expectedTopic Topic, t *testing.T) {
	t.Helper()
	select {
	case actualOrderNote := <-ch:
		if expectedTopic != actualOrderNote.TopicID {
			t.Fatalf("SubjectText mismatch. %s != %s", actualOrderNote.TopicID,
				expectedTopic)
		}
		return
	case <-tCtx.Done():
		return
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for OrderNote notification")
		return
	}
}

func TestHandleTradeResumptionMsg(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestHandleNomatch(t *testing.T) {
	t.Skip("removed DEX trading layer")
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

func TestReconfigureWallet(t *testing.T) {
	t.Skip("removed DEX trading layer")
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

func TestHandlePenaltyMsg(t *testing.T) {
	t.Skip("removed DEX trading layer")
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	dc := rig.dc
	penalty := &msgjson.Penalty{
		Rule:    account.Rule(1),
		Time:    uint64(1598929305),
		Details: "You may no longer trade. Leave your client running to finish pending trades.",
	}
	diffKey, _ := secp256k1.GeneratePrivateKey()
	noMatch, err := msgjson.NewNotification(msgjson.NoMatchRoute, "fake")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		key     *secp256k1.PrivateKey
		payload any
		wantErr bool
	}{{
		name:    "ok",
		key:     tDexPriv,
		payload: penalty,
	}, {
		name:    "bad note",
		key:     tDexPriv,
		payload: noMatch,
		wantErr: true,
	}, {
		name:    "wrong sig",
		key:     diffKey,
		payload: penalty,
		wantErr: true,
	}}
	for _, test := range tests {
		var err error
		var note *msgjson.Message
		switch v := test.payload.(type) {
		case *msgjson.Penalty:
			penaltyNote := &msgjson.PenaltyNote{
				Penalty: v,
			}
			sign(test.key, penaltyNote)
			note, err = msgjson.NewNotification(msgjson.PenaltyRoute, penaltyNote)
			if err != nil {
				t.Fatalf("error creating penalty notification: %v", err)
			}
		case *msgjson.Message:
			note = v
		default:
			t.Fatalf("unknown payload type: %T", v)
		}

		err = handlePenaltyMsg(tCore, dc, note)
		if test.wantErr {
			if err == nil {
				t.Fatalf("expected error for test %s", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}
	}
}

func TestHandleMMEpochSnapshotMsg(t *testing.T) {
	t.Skip("removed DEX trading layer")
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	dc := rig.dc

	acctID := make([]byte, 32)
	copy(acctID, dc.acct.id[:])

	snap := &msgjson.MMEpochSnapshot{
		MarketID:  tDcrBtcMktName,
		Base:      tUTXOAssetA.ID,
		Quote:     tUTXOAssetB.ID,
		EpochIdx:  1000,
		EpochDur:  60000,
		AccountID: acctID,
		BuyOrders: []msgjson.SnapOrder{
			{Rate: 1e8, Qty: 2e8},
		},
		SellOrders: []msgjson.SnapOrder{
			{Rate: 3e8, Qty: 4e8},
		},
		BestBuy:  5e8,
		BestSell: 6e8,
	}

	diffKey, _ := secp256k1.GeneratePrivateKey()
	badPayload, err := msgjson.NewNotification(msgjson.NoMatchRoute, "fake")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		key     *secp256k1.PrivateKey
		payload any
		wantErr bool
	}{{
		name:    "ok",
		key:     tDexPriv,
		payload: snap,
	}, {
		name:    "bad payload",
		key:     tDexPriv,
		payload: badPayload,
		wantErr: true,
	}, {
		name:    "wrong sig",
		key:     diffKey,
		payload: snap,
		wantErr: true,
	}}
	for _, test := range tests {
		var err error
		var note *msgjson.Message
		switch v := test.payload.(type) {
		case *msgjson.MMEpochSnapshot:
			snapCopy := *v
			sign(test.key, &snapCopy)
			note, err = msgjson.NewNotification(msgjson.MMEpochSnapshotRoute, &snapCopy)
			if err != nil {
				t.Fatalf("%s: error creating notification: %v", test.name, err)
			}
		case *msgjson.Message:
			note = v
		default:
			t.Fatalf("unknown payload type: %T", v)
		}

		err = handleMMEpochSnapshotMsg(tCore, dc, note)
		if test.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}
	}
}

func TestPreimageSync(t *testing.T) {
	t.Skip("removed: DEX trading functionality")
}

func TestAccelerateOrder(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestMatchStatusResolution(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestConfirmTransaction(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestMaxSwapsRedeemsInTx(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestSuspectTrades(t *testing.T) {
	t.Skip("removed DEX trading layer")
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

func TestParseCert(t *testing.T) {
	byteCert := []byte{0x0a, 0x0b}
	cert, err := parseCert("anyhost", []byte{0x0a, 0x0b}, dex.Mainnet)
	if err != nil {
		t.Fatalf("byte cert error: %v", err)
	}
	if !bytes.Equal(cert, byteCert) {
		t.Fatalf("byte cert note returned unmodified. expected %x, got %x", byteCert, cert)
	}
	byteCert = []byte{0x05, 0x06}
	certFile, _ := os.CreateTemp("", "dumbcert")
	defer os.Remove(certFile.Name())
	certFile.Write(byteCert)
	certFile.Close()
	cert, err = parseCert("anyhost", certFile.Name(), dex.Mainnet)
	if err != nil {
		t.Fatalf("file cert error: %v", err)
	}
	if !bytes.Equal(cert, byteCert) {
		t.Fatalf("byte cert note returned unmodified. expected %x, got %x", byteCert, cert)
	}
	_, err = parseCert("bison.exchange:17232", []byte(nil), dex.Testnet)
	if err != nil {
		t.Fatalf("CertStore cert error: %v", err)
	}
}

func TestPreOrder(t *testing.T) { t.Skip("order book functionality removed") }

func TestRefreshServerConfig(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestCredentialHandling(t *testing.T) {
	t.Skip("removed DEX trading layer")
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

var randU32 = func() uint32 { return uint32(rand.Int32()) }

func randOrderForMarket(base, quote uint32) order.Order {
	switch rand.IntN(3) {
	case 0:
		o, _ := ordertest.RandomCancelOrder()
		o.BaseAsset = base
		o.QuoteAsset = quote
		return o
	case 1:
		o, _ := ordertest.RandomMarketOrder()
		o.BaseAsset = base
		o.QuoteAsset = quote
		return o
	default:
		o, _ := ordertest.RandomLimitOrder()
		o.BaseAsset = base
		o.QuoteAsset = quote
		return o
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	crand.Read(b)
	return b
}

func TestDeleteOrderFn(t *testing.T) {
	t.Skip("removed DEX trading layer")
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	randomOdrs := func() []*db.MetaOrder {
		acct1 := dbtest.RandomAccountInfo()
		acct2 := dbtest.RandomAccountInfo()
		base1, quote1 := tUTXOAssetA.ID, tUTXOAssetB.ID
		base2, quote2 := tACCTAsset.ID, tUTXOAssetA.ID
		n := rand.IntN(9) + 1
		orders := make([]*db.MetaOrder, n)
		for i := 0; i < n; i++ {
			acct := acct1
			base, quote := base1, quote1
			if i%2 == 1 {
				acct = acct2
				base, quote = base2, quote2
			}
			ord := randOrderForMarket(base, quote)
			orders[i] = &db.MetaOrder{
				MetaData: &db.OrderMetaData{
					Status:             order.OrderStatus(rand.IntN(5) + 1),
					Host:               acct.Host,
					Proof:              db.OrderProof{DEXSig: randBytes(73)},
					SwapFeesPaid:       rand.Uint64(),
					RedemptionFeesPaid: rand.Uint64(),
					MaxFeeRate:         rand.Uint64(),
				},
				Order: ord,
			}
		}
		return orders
	}

	ordersFile, err := os.CreateTemp("", "delete_archives_test_orders")
	if err != nil {
		t.Fatal(err)
	}
	ordersFileName := ordersFile.Name()
	ordersFile.Close()
	os.Remove(ordersFileName)

	tests := []struct {
		name, ordersFileStr string
		wantErr             bool
	}{{
		name:          "ok orders and file save",
		ordersFileStr: ordersFileName,
	}, {
		name:          "bad file (already closed)",
		ordersFileStr: ordersFileName,
		wantErr:       true,
	}}

	for _, test := range tests {
		perOrdFn, cleanupFn, err := tCore.deleteOrderFn(test.ordersFileStr)
		if test.wantErr {
			if err != nil {
				continue
			}
			t.Fatalf("%q: expected error", test.name)
		}
		if err != nil {
			t.Fatalf("%q: unexpected failure: %v", test.name, err)
		}
		for _, o := range randomOdrs() {
			err = perOrdFn(o)
			if err != nil {
				t.Fatalf("%q: unexpected failure: %v", test.name, err)
			}
		}
		cleanupFn()
	}

	b, err := os.ReadFile(ordersFileName)
	if err != nil {
		t.Fatalf("unable to read file: %s", ordersFileName)
	}
	fmt.Println(string(b))
	os.Remove(ordersFileName)
}

func TestDeleteMatchFn(t *testing.T) {
	t.Skip("removed DEX trading layer")
	randomMtchs := func() []*db.MetaMatch {
		base, quote := tUTXOAssetA.ID, tUTXOAssetB.ID
		acct := dbtest.RandomAccountInfo()
		n := rand.IntN(9) + 1
		metaMatches := make([]*db.MetaMatch, 0, n)
		for i := 0; i < n; i++ {
			m := &db.MetaMatch{
				MetaData: &db.MatchMetaData{
					Proof: *dbtest.RandomMatchProof(0.5),
					DEX:   acct.Host,
					Base:  base,
					Quote: quote,
					Stamp: rand.Uint64(),
				},
				UserMatch: ordertest.RandomUserMatch(),
			}
			if i%2 == 1 {
				m.Status = order.MatchStatus(rand.IntN(4))
			} else {
				m.Status = order.MatchComplete              // inactive
				m.MetaData.Proof.Auth.RedeemSig = []byte{0} // redeemSig required for MatchComplete to be considered inactive
			}
			metaMatches = append(metaMatches, m)
		}
		return metaMatches
	}

	matchesFile, err := os.CreateTemp("", "delete_archives_test_matches")
	if err != nil {
		t.Fatal(err)
	}
	matchesFileName := matchesFile.Name()
	matchesFile.Close()
	os.Remove(matchesFileName)

	tests := []struct {
		name, matchesFileStr string
		wantErr              bool
	}{{
		name:           "ok matches and file save",
		matchesFileStr: matchesFileName,
	}, {
		name:           "bad file (already closed)",
		matchesFileStr: matchesFileName,
		wantErr:        true,
	}}

	for _, test := range tests {
		perMatchFn, cleanupFn, err := deleteMatchFn(test.matchesFileStr)
		if test.wantErr {
			if err != nil {
				continue
			}
			t.Fatalf("%q: expected error", test.name)
		}
		if err != nil {
			t.Fatalf("%q: unexpected failure: %v", test.name, err)
		}
		for _, m := range randomMtchs() {
			err = perMatchFn(m, true)
			if err != nil {
				t.Fatalf("%q: unexpected failure: %v", test.name, err)
			}
		}
		cleanupFn()
	}

	b, err := os.ReadFile(matchesFileName)
	if err != nil {
		t.Fatalf("unable to read file: %s", matchesFileName)
	}
	fmt.Println(string(b))
	os.Remove(matchesFileName)
}

func TestDeleteArchivedRecords(t *testing.T) {
	t.Skip("removed DEX trading layer")
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	tdb := tCore.db.(*TDB)

	tempFile := func(suffix string) (path string) {
		matchesFile, err := os.CreateTemp("", suffix+"delete_archives_test_matches")
		if err != nil {
			t.Fatal(err)
		}
		matchesFileName := matchesFile.Name()
		matchesFile.Close()
		os.Remove(matchesFileName)
		return matchesFileName
	}

	tests := []struct {
		name                                              string
		olderThan                                         *time.Time
		matchesFileStr, ordersFileStr                     string
		archivedMatches, archivedOrders                   int
		deleteInactiveOrdersErr, deleteInactiveMatchesErr error
		wantErr                                           bool
	}{{
		name:            "ok no order or file save",
		archivedMatches: 12,
		archivedOrders:  24,
	}, {
		name:            "ok orders and file save",
		ordersFileStr:   tempFile("abc"),
		matchesFileStr:  tempFile("123"),
		archivedMatches: 34,
		archivedOrders:  67,
	}, {
		name:                    "orders save error",
		ordersFileStr:           tempFile("abc"),
		deleteInactiveOrdersErr: errors.New(""),
		wantErr:                 true,
	}, {
		name:                     "matches save error",
		matchesFileStr:           tempFile("123"),
		deleteInactiveMatchesErr: errors.New(""),
		wantErr:                  true,
	}}

	for _, test := range tests {
		tdb.archivedMatches = test.archivedMatches
		tdb.archivedOrders = test.archivedOrders
		tdb.deleteInactiveOrdersErr = test.deleteInactiveOrdersErr
		tdb.deleteInactiveMatchesErr = test.deleteInactiveMatchesErr
		nRecordsDeleted, err := tCore.DeleteArchivedRecords(test.olderThan, test.matchesFileStr, test.ordersFileStr)
		if test.wantErr {
			if err != nil {
				continue
			}
			t.Fatalf("%q: expected error", test.name)
		}
		if err != nil {
			t.Fatalf("%q: unexpected failure: %v", test.name, err)
		}
		expectedRecords := test.archivedMatches + test.archivedOrders
		if nRecordsDeleted != expectedRecords {
			t.Fatalf("%s: Expected %d deleted records, got %d", test.name, expectedRecords, nRecordsDeleted)
		}
	}
}

func TestLCM(t *testing.T) {
	t.Skip("removed DEX trading layer")
	tests := []struct {
		name                                  string
		a, b, wantDenom, wantMultA, wantMultB uint64
	}{{
		name:      "ok 5 and 10",
		a:         5,
		b:         10,
		wantDenom: 10,
		wantMultA: 2,
		wantMultB: 1,
	}, {
		name:      "ok 3 and 7",
		a:         3,
		b:         7,
		wantDenom: 21,
		wantMultA: 7,
		wantMultB: 3,
	}, {
		name:      "ok 6 and 34",
		a:         34,
		b:         6,
		wantDenom: 102,
		wantMultA: 3,
		wantMultB: 17,
	}}

	for _, test := range tests {
		denom, multA, multB := lcm(test.a, test.b)
		if denom != test.wantDenom || multA != test.wantMultA || multB != test.wantMultB {
			t.Fatalf("%q: expected %d %d %d but got %d %d %d", test.name,
				test.wantDenom, test.wantMultA, test.wantMultB, denom, multA, multB)
		}
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

// TDynamicAccountLocker combines TAccountLocker with DynamicSwapper interface
// for testing gas fee limit validation in prepareTradeRequest.
type TDynamicAccountLocker struct {
	*TAccountLocker
	gasFeeLimit     uint64
	tfpPaid         uint64
	tfpSecretHashes [][]byte
	tfpErr          error
}

func newTDynamicAccountLocker(assetID uint32) (*xcWallet, *TDynamicAccountLocker) {
	xcWallet, accountLocker := newTAccountLocker(assetID)
	dynamicAccountLocker := &TDynamicAccountLocker{
		TAccountLocker: accountLocker,
		gasFeeLimit:    200, // default higher than tACCTAsset.MaxFeeRate (20)
	}
	xcWallet.Wallet = dynamicAccountLocker
	return xcWallet, dynamicAccountLocker
}

func (w *TDynamicAccountLocker) DynamicSwapFeesPaid(ctx context.Context, coinID, contractData dex.Bytes) (uint64, [][]byte, error) {
	return w.tfpPaid, w.tfpSecretHashes, w.tfpErr
}

func (w *TDynamicAccountLocker) DynamicRedemptionFeesPaid(ctx context.Context, coinID, contractData dex.Bytes) (uint64, [][]byte, error) {
	return w.tfpPaid, w.tfpSecretHashes, w.tfpErr
}

func (w *TDynamicAccountLocker) GasFeeLimit() uint64 {
	return w.gasFeeLimit
}

var _ asset.DynamicSwapper = (*TDynamicAccountLocker)(nil)
var _ asset.AccountLocker = (*TDynamicAccountLocker)(nil)

func TestUpdateFeesPaid(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestDynamicSwapperGasFeeLimit(t *testing.T) { t.Skip("order book functionality removed") }

func TestUpdateBondOptions(t *testing.T) { t.Skip("bond functionality removed") }

func TestRotateBonds(t *testing.T) { t.Skip("bond functionality removed") }

func TestFindBondKeyIdx(t *testing.T) { t.Skip("bond functionality removed") }

func TestFindBond(t *testing.T) { t.Skip("bond functionality removed") }

func TestNetworkFeeRate(t *testing.T) { t.Skip("order book functionality removed") }

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

func TestTradingLimits(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestTakeAction(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestCore_Orders_SmartFilterExecuted(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	// Create test orders with different statuses
	lo1, _, _, _ := makeLimitOrder(rig.dc, true, 1000, 100)
	lo2, _, _, _ := makeLimitOrder(rig.dc, true, 1000, 100)
	lo3, _, _, _ := makeLimitOrder(rig.dc, true, 1000, 100)
	lo4, _, _, _ := makeLimitOrder(rig.dc, true, 1000, 100)

	// Set order statuses
	lo1.Force = order.StandingTiF
	lo2.Force = order.StandingTiF
	lo3.Force = order.StandingTiF
	lo4.Force = order.StandingTiF

	// Create MetaOrders
	metaOrder1 := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusExecuted,
		},
		Order: lo1,
	}

	metaOrder2 := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusCanceled,
		},
		Order: lo2,
	}

	metaOrder3 := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusCanceled,
		},
		Order: lo3,
	}

	metaOrder4 := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusBooked,
		},
		Order: lo4,
	}

	// Set up TDB with orders
	tCore.db.(*TDB).allOrders = []*db.MetaOrder{metaOrder1, metaOrder2, metaOrder3, metaOrder4}

	// Set up orderOrders map for coreOrderFromMetaOrder
	if tCore.db.(*TDB).orderOrders == nil {
		tCore.db.(*TDB).orderOrders = make(map[order.OrderID]*db.MetaOrder)
	}
	tCore.db.(*TDB).orderOrders[lo1.ID()] = metaOrder1
	tCore.db.(*TDB).orderOrders[lo2.ID()] = metaOrder2
	tCore.db.(*TDB).orderOrders[lo3.ID()] = metaOrder3
	tCore.db.(*TDB).orderOrders[lo4.ID()] = metaOrder4

	// Set up matches - lo1 has fills (executed), lo2 has fills (canceled), lo3 has no fills
	mid1 := ordertest.RandomMatchID()
	mid2 := ordertest.RandomMatchID()
	tCore.db.(*TDB).matchesByOrderID = map[order.OrderID][]*db.MetaMatch{
		lo1.ID(): {
			{
				MetaData: &db.MatchMetaData{},
				UserMatch: &order.UserMatch{
					OrderID:  lo1.ID(),
					MatchID:  mid1,
					Quantity: 800,
					Rate:     100,
					Status:   order.MakerSwapCast,
					Side:     order.Maker,
					Address:  "some-address", // Must be non-empty to not be a cancel match
				},
			},
		},
		lo2.ID(): {
			{
				MetaData: &db.MatchMetaData{},
				UserMatch: &order.UserMatch{
					OrderID:  lo2.ID(),
					MatchID:  mid2,
					Quantity: 500,
					Rate:     100,
					Status:   order.MakerSwapCast,
					Side:     order.Maker,
					Address:  "some-address", // Must be non-empty to not be a cancel match
				},
			},
		},
		// lo3 has no matches (empty or not in map)
	}

	// Test: Filter for "executed" orders with IncludePartial to include
	// canceled orders that have partial fills
	filter := &OrderFilter{
		Statuses:       []order.OrderStatus{order.OrderStatusExecuted},
		IncludePartial: true,
	}

	results, err := tCore.Orders(filter)
	if err != nil {
		t.Fatalf("Orders error: %v", err)
	}

	// Should return 2 orders:
	// 1. Executed order (lo1)
	// 2. Canceled with fills (lo2) - partially filled
	if len(results) != 2 {
		t.Fatalf("Expected 2 orders, got %d", len(results))
	}

	// Verify the correct orders are returned
	foundExecuted := false
	foundPartiallyCanceled := false
	for _, ord := range results {
		if ord.ID.String() == lo1.ID().String() {
			foundExecuted = true
			if ord.Status != order.OrderStatusExecuted {
				t.Errorf("Expected executed status, got %v", ord.Status)
			}
		}
		if ord.ID.String() == lo2.ID().String() {
			foundPartiallyCanceled = true
			if ord.Status != order.OrderStatusCanceled {
				t.Errorf("Expected canceled status, got %v", ord.Status)
			}
		}
	}

	if !foundExecuted {
		t.Error("Did not find executed order in results")
	}
	if !foundPartiallyCanceled {
		t.Error("Did not find partially canceled order in results")
	}
}

// TestCore_Orders_CanceledFilterStillWorks tests that explicitly filtering
// for "canceled" returns ALL canceled orders (with and without fills).
func TestCore_Orders_CanceledFilterStillWorks(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	// Create canceled orders with and without fills
	lo1, _, _, _ := makeLimitOrder(rig.dc, true, 1000, 100)
	lo2, _, _, _ := makeLimitOrder(rig.dc, true, 1000, 100)

	lo1.Force = order.StandingTiF
	lo2.Force = order.StandingTiF

	metaOrder1 := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusCanceled,
		},
		Order: lo1,
	}

	metaOrder2 := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusCanceled,
		},
		Order: lo2,
	}

	tCore.db.(*TDB).allOrders = []*db.MetaOrder{metaOrder1, metaOrder2}

	if tCore.db.(*TDB).orderOrders == nil {
		tCore.db.(*TDB).orderOrders = make(map[order.OrderID]*db.MetaOrder)
	}
	tCore.db.(*TDB).orderOrders[lo1.ID()] = metaOrder1
	tCore.db.(*TDB).orderOrders[lo2.ID()] = metaOrder2

	// Test: Filter for "canceled" orders only
	filter := &OrderFilter{
		Statuses: []order.OrderStatus{order.OrderStatusCanceled},
	}

	results, err := tCore.Orders(filter)
	if err != nil {
		t.Fatalf("Orders error: %v", err)
	}

	// Should return ALL canceled orders (both with and without fills)
	if len(results) != 2 {
		t.Fatalf("Expected 2 canceled orders, got %d", len(results))
	}

	for _, ord := range results {
		if ord.Status != order.OrderStatusCanceled {
			t.Errorf("Expected canceled status, got %v", ord.Status)
		}
	}
}

// TestCore_Orders_ExecutedAndCanceledFilter tests that when user explicitly
// selects BOTH "executed" AND "canceled" filters, ALL canceled orders are
// returned (including those with 0% filled), because the user explicitly
// selected "canceled" status.
func TestCore_Orders_ExecutedAndCanceledFilter(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	// Create test orders
	lo1, _, _, _ := makeLimitOrder(rig.dc, true, 1000, 100) // Executed
	lo2, _, _, _ := makeLimitOrder(rig.dc, true, 1000, 100) // Canceled with fills
	lo3, _, _, _ := makeLimitOrder(rig.dc, true, 1000, 100) // Canceled without fills
	lo4, _, _, _ := makeLimitOrder(rig.dc, true, 1000, 100) // Booked (should not be returned)

	lo1.Force = order.StandingTiF
	lo2.Force = order.StandingTiF
	lo3.Force = order.StandingTiF
	lo4.Force = order.StandingTiF

	metaOrder1 := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusExecuted,
		},
		Order: lo1,
	}

	metaOrder2 := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusCanceled,
		},
		Order: lo2,
	}

	metaOrder3 := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusCanceled,
		},
		Order: lo3,
	}

	metaOrder4 := &db.MetaOrder{
		MetaData: &db.OrderMetaData{
			Status: order.OrderStatusBooked,
		},
		Order: lo4,
	}

	tCore.db.(*TDB).allOrders = []*db.MetaOrder{metaOrder1, metaOrder2, metaOrder3, metaOrder4}

	if tCore.db.(*TDB).orderOrders == nil {
		tCore.db.(*TDB).orderOrders = make(map[order.OrderID]*db.MetaOrder)
	}
	tCore.db.(*TDB).orderOrders[lo1.ID()] = metaOrder1
	tCore.db.(*TDB).orderOrders[lo2.ID()] = metaOrder2
	tCore.db.(*TDB).orderOrders[lo3.ID()] = metaOrder3
	tCore.db.(*TDB).orderOrders[lo4.ID()] = metaOrder4

	// Set up matches - lo1 has fills (executed), lo2 has fills (canceled), lo3 has no fills (canceled)
	mid1 := ordertest.RandomMatchID()
	mid2 := ordertest.RandomMatchID()
	tCore.db.(*TDB).matchesByOrderID = map[order.OrderID][]*db.MetaMatch{
		lo1.ID(): {
			{
				MetaData: &db.MatchMetaData{},
				UserMatch: &order.UserMatch{
					OrderID:  lo1.ID(),
					MatchID:  mid1,
					Quantity: 800,
					Rate:     100,
					Status:   order.MakerSwapCast,
					Side:     order.Maker,
					Address:  "some-address", // Non-empty to not be a cancel match
				},
			},
		},
		lo2.ID(): {
			{
				MetaData: &db.MatchMetaData{},
				UserMatch: &order.UserMatch{
					OrderID:  lo2.ID(),
					MatchID:  mid2,
					Quantity: 500,
					Rate:     100,
					Status:   order.MakerSwapCast,
					Side:     order.Maker,
					Address:  "some-address", // Non-empty to not be a cancel match
				},
			},
		},
		// lo3 has no matches
	}

	// Test: Filter for BOTH "executed" AND "canceled"
	filter := &OrderFilter{
		Statuses: []order.OrderStatus{order.OrderStatusExecuted, order.OrderStatusCanceled},
	}

	results, err := tCore.Orders(filter)
	if err != nil {
		t.Fatalf("Orders error: %v", err)
	}

	// Should return 3 orders:
	// 1. Executed order (lo1)
	// 2. Canceled with fills (lo2)
	// 3. Canceled without fills (lo3) - Should NOT be filtered because user explicitly selected "canceled"
	if len(results) != 3 {
		t.Fatalf("Expected 3 orders (executed + all canceled), got %d", len(results))
	}

	// Verify the correct orders are returned
	foundExecuted := false
	foundCanceledWithFills := false
	foundCanceledNoFills := false
	for _, ord := range results {
		if ord.ID.String() == lo1.ID().String() {
			foundExecuted = true
		}
		if ord.ID.String() == lo2.ID().String() {
			foundCanceledWithFills = true
		}
		if ord.ID.String() == lo3.ID().String() {
			foundCanceledNoFills = true
		}
	}

	if !foundExecuted {
		t.Error("Did not find executed order in results")
	}
	if !foundCanceledWithFills {
		t.Error("Did not find canceled order with fills in results")
	}
	if !foundCanceledNoFills {
		t.Error("Did not find canceled order without fills in results - should NOT be filtered when user explicitly selects 'canceled'")
	}
}

func TestHandleCounterPartyAddressMsg(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestAuditContractCrossMatchDedup(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestReleaseMatchCoinID(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	dc := rig.dc

	mid1 := ordertest.RandomMatchID()
	mid2 := ordertest.RandomMatchID()

	// Register some CoinIDs and secret hashes.
	dc.activeContractsMtx.Lock()
	dc.activeCoinIDs["coinA"] = mid1
	dc.activeCoinIDs["coinB"] = mid1
	dc.activeCoinIDs["coinC"] = mid2
	dc.activeSecretHashes["hashA"] = mid1
	dc.activeSecretHashes["hashC"] = mid2
	dc.matchCoinIDs[mid1] = []string{"coinA", "coinB"}
	dc.matchCoinIDs[mid2] = []string{"coinC"}
	dc.matchSecretHashes[mid1] = []string{"hashA"}
	dc.matchSecretHashes[mid2] = []string{"hashC"}
	dc.activeContractsMtx.Unlock()

	// Release match1.
	dc.releaseMatchCoinID(mid1)

	dc.activeContractsMtx.Lock()
	// match1 entries should be gone.
	if _, exists := dc.activeCoinIDs["coinA"]; exists {
		t.Fatal("coinA not cleaned up")
	}
	if _, exists := dc.activeCoinIDs["coinB"]; exists {
		t.Fatal("coinB not cleaned up")
	}
	if _, exists := dc.activeSecretHashes["hashA"]; exists {
		t.Fatal("hashA not cleaned up")
	}
	if _, exists := dc.matchCoinIDs[mid1]; exists {
		t.Fatal("matchCoinIDs[mid1] not cleaned up")
	}
	if _, exists := dc.matchSecretHashes[mid1]; exists {
		t.Fatal("matchSecretHashes[mid1] not cleaned up")
	}
	// match2 entries should still exist.
	if _, exists := dc.activeCoinIDs["coinC"]; !exists {
		t.Fatal("coinC incorrectly removed")
	}
	if _, exists := dc.activeSecretHashes["hashC"]; !exists {
		t.Fatal("hashC incorrectly removed")
	}
	dc.activeContractsMtx.Unlock()
}

func TestIsSwappableGatedOnCounterPartyAddr(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestParseMatchesPerMatchAddr(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

func TestTradePerMatchAddr(t *testing.T) {
	t.Skip("removed DEX trading layer")
}

