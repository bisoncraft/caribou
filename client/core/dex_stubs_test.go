// dex_stubs_test.go provides minimal type stubs for removed DEX types so that
// the test file compiles. All tests that use these types are skipped.

package core

import (
	"sync"

	"os"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/comms"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/client/db"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	serverdex "decred.org/dcrdex/server/dex"
	"decred.org/dcrdex/server/account"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// signMsg signs a message with a secp256k1 private key.
func signMsg(privKey *secp256k1.PrivateKey, msg []byte) []byte {
	sig := ecdsa.Sign(privKey, msg)
	return sig.Serialize()
}

// dexAccount is a stub for the removed DEX account type.
type dexAccount struct {
	host      string
	cert      []byte
	dexPubKey *secp256k1.PublicKey
	keyMtx    sync.RWMutex
	viewOnly  bool
	encKey    []byte
	privKey   *secp256k1.PrivateKey
	id        account.AccountID
	authMtx   sync.RWMutex
	isAuthed  bool
	disabled  bool
	pendingBondsConfs map[string]uint32
	pendingBonds      []*db.Bond
	bonds             []*db.Bond
	expiredBonds      []*db.Bond
	rep               account.Reputation
	targetTier        uint64
	maxBondedAmt      uint64
	penaltyComps      uint16
	bondAsset         uint32
}

func (a *dexAccount) ID() account.AccountID {
	a.keyMtx.RLock()
	defer a.keyMtx.RUnlock()
	return a.id
}
func (a *dexAccount) authed() bool {
	a.authMtx.RLock()
	defer a.authMtx.RUnlock()
	return a.isAuthed
}
func (a *dexAccount) lock() {
	a.keyMtx.Lock()
	a.privKey = nil
	a.keyMtx.Unlock()
}
func (a *dexAccount) locked() bool {
	a.keyMtx.RLock()
	defer a.keyMtx.RUnlock()
	return a.privKey == nil
}

// walletSet is a stub for the removed wallet set type.
type walletSet struct {
	fromWallet  *xcWallet
	toWallet    *xcWallet
	baseWallet  *xcWallet
	quoteWallet *xcWallet
}

// trackedTrade is a stub for the removed tracked trade type.
type trackedTrade struct {
	mtx     sync.RWMutex
	wallets walletSet
	matches map[order.MatchID]*matchTracker
}

func (t *trackedTrade) Base() uint32  { return 0 }
func (t *trackedTrade) Quote() uint32 { return 0 }

// matchTracker is a stub for the removed match tracker type.
type matchTracker struct {
	*db.MetaMatch
	exceptionMtx      sync.Mutex
	suspectSwap        bool
	suspectRedeem      bool
	tickGovernor       interface{ Stop() }
	redemptionRejected bool
	refundRejected     bool
	CounterPartyAddr   string
}

// dexConnection is a stub for the removed DEX connection type.
type dexConnection struct {
	WsConn             comms.WsConn
	log                dex.Logger
	connMaster         *dex.ConnectionMaster
	ticker             interface{}
	acct               *dexAccount
	assets             map[uint32]*dex.Asset
	assetsMtx          sync.RWMutex
	cfg                *msgjson.ConfigResult
	cfgMtx             sync.RWMutex
	notify             func(Notification)
	dispatchTradeWork  func(order.OrderID, func())
	tradeMtx           sync.RWMutex
	trades             map[order.OrderID]*trackedTrade
	cancels            map[order.OrderID]order.OrderID
	inFlightOrders     map[uint64]*InFlightOrder
	epoch              map[string]uint64
	resolvedEpoch      map[string]uint64
	apiVer             uint16
	connectionStatus   uint32
	reportingConnects  int32
	spots              map[string]*msgjson.Spot
	activeCoinIDs      map[string]order.MatchID
	activeContractsMtx sync.RWMutex
	activeSecretHashes map[string]order.MatchID
	matchCoinIDs       map[order.MatchID][]string
	matchSecretHashes  map[order.MatchID][]string
}

func (dc *dexConnection) NextID() uint64 { return 0 }
func (dc *dexConnection) marketConfig(_ string) *msgjson.Market { return nil }
func (dc *dexConnection) marketEpochDuration(_ string) uint64 { return 0 }
func (dc *dexConnection) findOrder(_ order.OrderID) (*trackedTrade, bool) { return nil, false }
func (dc *dexConnection) trackedTrades() []*trackedTrade { return nil }
func (dc *dexConnection) hasActiveOrders() bool { return false }
func (dc *dexConnection) hasActiveAssetOrders(_ uint32) bool { return false }
func (dc *dexConnection) compareServerMatches(_ *msgjson.Message) {}
func (dc *dexConnection) parseMatches(_ *msgjson.Message) ([]*matchProof, error) { return nil, nil }
func (dc *dexConnection) marketTrades(_ string) []*order.UserMatch { return nil }
func (dc *dexConnection) registerCancelLink(_, _ order.OrderID) {}
func (dc *dexConnection) releaseMatchCoinID(_ order.MatchID) {}

// matchProof is a stub used by parseMatches.
type matchProof struct{}

// newDexTicker is a stub.
func newDexTicker(_ interface{}) interface{} { return nil }

// newTrackedTrade is a stub.
func newTrackedTrade(_ *db.MetaOrder, _ order.Preimage, _ *dexConnection,
	_, _ interface{}, _ interface{}, _ interface{}, _ *walletSet,
	_ []asset.Coin, _ func(Notification), _ func(Topic, ...interface{}) (string, string),
	_ interface{}) *trackedTrade {
	return &trackedTrade{}
}

// tDriver implements asset.Driver for testing.
type tDriver struct {
	decodedCoinID string
	winfo         *asset.WalletInfo
	openErr       error
	wallet        asset.Wallet // optional pre-opened wallet
}

func (d *tDriver) Open(_ *asset.WalletConfig, _ dex.Logger, _ dex.Network) (asset.Wallet, error) {
	if d.openErr != nil {
		return nil, d.openErr
	}
	return d.wallet, nil
}
func (d *tDriver) DecodeCoinID(_ []byte) (string, error) { return d.decodedCoinID, nil }
func (d *tDriver) Info() *asset.WalletInfo                { return d.winfo }

// tCreator embeds tDriver and implements asset.Creator for seeded wallets.
type tCreator struct {
	*tDriver
	existsErr error
	createErr error
}

func (c *tCreator) Exists(_, _ string, _ map[string]string, _ dex.Network) (bool, error) {
	return false, c.existsErr
}
func (c *tCreator) Create(_ *asset.CreateWalletParams) error { return c.createErr }

// sign stamps a Signable with a secp256k1 signature.
func sign(privKey *secp256k1.PrivateKey, signable msgjson.Signable) {
	msg := signable.Serialize()
	sig := ecdsa.Sign(privKey, msg)
	signable.SetSig(sig.Serialize())
}

// newPreimage creates a random order preimage.
func newPreimage() order.Preimage {
	var pi order.Preimage
	copy(pi[:], encode.RandomBytes(order.PreimageSize))
	return pi
}

// convertMsgCancelOrder is a stub; used only in skipped tests.
func convertMsgCancelOrder(_ *msgjson.CancelOrder) *order.CancelOrder {
	return &order.CancelOrder{}
}

// orderResponse is a stub; used only in skipped tests.
func orderResponse(_ uint64, _ msgjson.Signable, _ order.Order, _, _, _ bool) *msgjson.Message {
	return nil
}

// handlePenaltyMsg is a stub; used only in skipped tests.
func handlePenaltyMsg(_ *Core, _ *dexConnection, _ *msgjson.Message) error {
	return nil
}

// handleMMEpochSnapshotMsg is a stub; used only in skipped tests.
func handleMMEpochSnapshotMsg(_ *Core, _ *dexConnection, _ *msgjson.Message) error {
	return nil
}

// parseCert parses a TLS certificate from bytes or a file path.
func parseCert(host string, certI interface{}, _ dex.Network) ([]byte, error) {
	switch c := certI.(type) {
	case []byte:
		return c, nil
	case string:
		return os.ReadFile(c)
	}
	return nil, nil
}

// deleteOrderFn returns a per-order delete function; stub for removed functionality.
func (c *Core) deleteOrderFn(ordersFileStr string) (func(*db.MetaOrder) error, func(), error) {
	if ordersFileStr == "" {
		return func(*db.MetaOrder) error { return nil }, func() {}, nil
	}
	f, err := os.OpenFile(ordersFileStr, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, nil, err
	}
	return func(*db.MetaOrder) error { return nil }, func() { f.Close() }, nil
}

// deleteMatchFn returns a per-match delete function; stub for removed functionality.
func deleteMatchFn(matchesFileStr string) (func(*db.MetaMatch, bool) error, func(), error) {
	if matchesFileStr == "" {
		return func(*db.MetaMatch, bool) error { return nil }, func() {}, nil
	}
	f, err := os.OpenFile(matchesFileStr, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, nil, err
	}
	return func(*db.MetaMatch, bool) error { return nil }, func() { f.Close() }, nil
}

// lcm returns the LCM of two values and the corresponding multipliers.
func lcm(a, b uint64) (uint64, uint64, uint64) {
	if a == 0 || b == 0 {
		return 0, 0, 0
	}
	g := a
	for tmp := b % a; tmp != 0; tmp = g % (b % g) {
		g, b = b%g, g
	}
	l := a / g * b
	return l, l / a, l / b
}

// pokesCache holds a ring buffer of poke notifications.
type pokesCache struct {
	cache  []*db.Notification
	cap    int
	cursor int
}

func newPokesCache(capacity int) *pokesCache {
	return &pokesCache{cache: make([]*db.Notification, 0, capacity), cap: capacity}
}

func (p *pokesCache) add(n *db.Notification) {
	if len(p.cache) < p.cap {
		p.cache = append(p.cache, n)
	} else {
		p.cache[p.cursor%p.cap] = n
	}
	p.cursor++
}

func (p *pokesCache) init(notes []*db.Notification) {
	p.cache = notes
	p.cursor = len(notes)
}

func (p *pokesCache) pokes() []*db.Notification { return p.cache }

func init() {
	// Register a stub driver for the generic test asset IDs used in tests
	// that don't use a real registered asset.
	for _, id := range []uint32{54321} {
		asset.Register(id, &tDriver{winfo: &asset.WalletInfo{
			SupportedVersions: []uint32{0},
			UnitInfo: dex.UnitInfo{
				Conventional: dex.Denomination{ConversionFactor: 1e8},
			},
			AvailableWallets: []*asset.WalletDefinition{{Type: "type"}},
		}}, true)
	}
}

// Suppress unused import warnings.
var (
	_ = serverdex.PreAPIVersion
	_ = account.AccountID{}
)
