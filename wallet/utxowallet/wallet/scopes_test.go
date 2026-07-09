package wallet

import (
	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
)

// The wallet tests build BTC test wallets and exercise multiple address types,
// so enable all four standard key purposes on the shared BTC test params.
// Production lists a single purpose per chain; BIP-84 stays first so PrimaryScope
// remains native segwit. This mutation is confined to the wallet test binary.
func init() {
	for _, net := range []string{"mainnet", "testnet", "simnet"} {
		assets.BTCParams[net].KeyPurposes = []uint32{84, 49, 86, 44}
	}
}

// Bitcoin (coin 0) key scopes for tests. Production derives scopes from
// netparams.ChainParams.KeyPurposes; these fixed coin-0 values exist only for
// the BTC-based wallet tests.
var (
	keyScopeBIP0049Plus = waddrmgr.KeyScope{Purpose: 49, Coin: 0}
	keyScopeBIP0084     = waddrmgr.KeyScope{Purpose: 84, Coin: 0}
	keyScopeBIP0086     = waddrmgr.KeyScope{Purpose: 86, Coin: 0}
	keyScopeBIP0044     = waddrmgr.KeyScope{Purpose: 44, Coin: 0}
)
