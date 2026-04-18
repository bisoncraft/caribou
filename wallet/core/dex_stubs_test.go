// dex_stubs_test.go provides minimal type stubs so test files compile.
// The DEX trading layer has been removed; only wallet/swap infrastructure remains.

package core

import (
	"github.com/bisoncraft/meshwallet/wallet/asset"
	"github.com/bisoncraft/meshwallet/wallet/db"
	"github.com/bisoncraft/meshwallet/dex"
	"github.com/bisoncraft/meshwallet/dex/order"
)

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

// Match is a stub for the removed DEX match type.
// Referenced by helpers_test.go.
type Match struct {
	MatchID  dex.Bytes         `json:"matchID"`
	Status   order.MatchStatus `json:"status"`
	Active   bool              `json:"active"`
	Revoked  bool              `json:"revoked"`
	Rate     uint64            `json:"rate"`
	Qty      uint64            `json:"qty"`
	Side     order.MatchSide   `json:"side"`
	FeeRate  uint64            `json:"feeRate"`
	Stamp    uint64            `json:"stamp"`
	IsCancel bool              `json:"isCancel"`
}

// Order is a stub for the removed DEX order type.
// Referenced by helpers_test.go.
type Order struct {
	Host        string            `json:"host"`
	BaseID      uint32            `json:"baseID"`
	BaseSymbol  string            `json:"baseSymbol"`
	QuoteID     uint32            `json:"quoteID"`
	QuoteSymbol string            `json:"quoteSymbol"`
	MarketID    string            `json:"market"`
	Type        order.OrderType   `json:"type"`
	ID          dex.Bytes         `json:"id"`
	Stamp       uint64            `json:"stamp"`
	Status      order.OrderStatus `json:"status"`
	Qty         uint64            `json:"qty"`
	Sell        bool              `json:"sell"`
	Filled      uint64            `json:"filled"`
	Matches     []*Match          `json:"matches"`
	Cancelling  bool              `json:"cancelling"`
	Canceled    bool              `json:"canceled"`
	Rate        uint64            `json:"rate"`
	TimeInForce order.TimeInForce `json:"tif"`
}

// OrderReader is a stub for the removed order reader helper type.
// Referenced by helpers_test.go.
type OrderReader struct {
	*Order
}

// StatusString returns the order status as a string.
// Used by helpers_test.go.
func (ord *OrderReader) StatusString() string {
	isLive := false
	for _, m := range ord.Matches {
		if m.Active {
			isLive = true
			break
		}
	}
	hasFills := false
	for _, m := range ord.Matches {
		if !m.IsCancel {
			hasFills = true
			break
		}
	}
	switch ord.Status {
	case order.OrderStatusUnknown:
		return "unknown"
	case order.OrderStatusEpoch:
		return "epoch"
	case order.OrderStatusBooked:
		if ord.Cancelling {
			return "cancelling"
		}
		if isLive {
			return "booked/settling"
		}
		return "booked"
	case order.OrderStatusExecuted:
		if isLive {
			return "settling"
		}
		if !hasFills && ord.Type != order.CancelOrderType {
			return "no match"
		}
		return "executed"
	case order.OrderStatusCanceled:
		if isLive {
			return "canceled/settling"
		}
		if hasFills {
			return "canceled/partially filled"
		}
		return "canceled"
	case order.OrderStatusRevoked:
		if isLive {
			return "revoked/settling"
		}
		if hasFills {
			return "revoked/partially filled"
		}
		return "revoked"
	}
	return "unknown"
}
