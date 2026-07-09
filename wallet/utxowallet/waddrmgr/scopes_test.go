package waddrmgr

import "github.com/bisoncraft/caribou/wallet/utxowallet/assets"

// The address manager's multi-scope machinery (BIP-44/49/84/86) is exercised by
// the package tests independently of what any asset configures in production,
// where each chain lists a single key purpose. Enable all four standard
// purposes on the shared BTC test params so Create registers every scope the
// tests fetch. BIP-84 stays first so it remains the primary scope. This mutation
// is confined to the waddrmgr test binary.
func init() {
	assets.BTCParams["mainnet"].KeyPurposes = []uint32{84, 49, 86, 44}
}

// Bitcoin (coin 0) key scopes and schema map, used only by tests. Production
// derives scopes from netparams.ChainParams.KeyPurposes, so these fixed coin-0
// values are no longer part of the package's exported surface.
var (
	KeyScopeBIP0049Plus = KeyScope{Purpose: 49, Coin: 0}
	KeyScopeBIP0084     = KeyScope{Purpose: 84, Coin: 0}
	KeyScopeBIP0086     = KeyScope{Purpose: 86, Coin: 0}
	KeyScopeBIP0044     = KeyScope{Purpose: 44, Coin: 0}

	DefaultKeyScopes = []KeyScope{
		KeyScopeBIP0049Plus,
		KeyScopeBIP0084,
		KeyScopeBIP0086,
		KeyScopeBIP0044,
	}

	ScopeAddrMap = ScopeAddrMapForCoin(0)
)
