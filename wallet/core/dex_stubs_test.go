// dex_stubs_test.go provides shared test scaffolding: asset driver stubs and
// the poke cache used by the core tests.

package core

import (
	"sort"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/db"
)

// tDriver implements asset.Driver for testing.
type tDriver struct {
	decodedCoinID string
	winfo         *asset.WalletInfo
	openErr       error
	wallet        asset.Wallet // optional pre-opened wallet
}

func (d *tDriver) Open(_ *asset.WalletConfig, _ util.Logger, _ util.Network) (asset.Wallet, error) {
	if d.openErr != nil {
		return nil, d.openErr
	}
	return d.wallet, nil
}
func (d *tDriver) DecodeCoinID(_ []byte) (string, error) { return d.decodedCoinID, nil }
func (d *tDriver) Info() *asset.WalletInfo               { return d.winfo }

// tCreator embeds tDriver and implements asset.Creator for seeded wallets.
type tCreator struct {
	*tDriver
	existsErr error
	createErr error
}

func (c *tCreator) Exists(_, _ string, _ map[string]string, _ util.Network) (bool, error) {
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
	if len(p.cache) >= p.cap {
		p.cache[p.cursor] = n
	} else {
		p.cache = append(p.cache, n)
	}
	p.cursor = (p.cursor + 1) % p.cap
}

func (p *pokesCache) init(notes []*db.Notification) {
	if len(notes) > p.cap {
		notes = notes[:len(notes)-p.cap]
	}
	p.cache = notes
	p.cursor = len(notes) % p.cap
}

func (p *pokesCache) pokes() []*db.Notification {
	out := make([]*db.Notification, len(p.cache))
	copy(out, p.cache)
	sort.Slice(out, func(i, j int) bool {
		return out[i].TimeStamp < out[j].TimeStamp
	})
	return out
}

func init() {
	// Register a stub driver for the generic test asset IDs used in tests
	// that don't use a real registered asset.
	for _, id := range []uint32{54321} {
		asset.Register(id, &tDriver{winfo: &asset.WalletInfo{
			UnitInfo: util.UnitInfo{
				Conventional: util.Denomination{ConversionFactor: 1e8},
			},
			AvailableWallets: []*asset.WalletDefinition{{Type: "type"}},
		}}, true)
	}
}
