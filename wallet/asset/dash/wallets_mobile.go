//go:build mobile

package dash

import "github.com/bisoncraft/caribou/wallet/asset"

// availableWallets is the mobile wallet menu: electrum only. Electrum was
// already the Dash default on every build, so this withholds a choice rather
// than changing one. The case against offering SPV on a phone is stronger here
// than for btc or ltc: on top of neutrino's header walk from genesis and its
// 31.2 MB filter and 41 MB block caches, Dash Core does not implement BIP-157
// and mainnet defines no NeutrinoSeeds, so the peers a phone would find need
// not serve compact filters at all.
//
// See the btc package's wallets_mobile.go for the privacy tradeoff this
// accepts and why the SPV code paths stay compiled.
var availableWallets = []*asset.WalletDefinition{
	dashElectrumWalletDefinition,
}
