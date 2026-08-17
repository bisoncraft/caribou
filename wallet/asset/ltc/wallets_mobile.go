//go:build mobile

package ltc

import "github.com/bisoncraft/caribou/wallet/asset"

// availableWallets is the mobile wallet menu: electrum only, so it is both the
// default and the sole choice. LTC is the worst case for neutrino on a phone.
// Its chain is over three times as long as BTC's, so the header walk from
// genesis alone is a few hundred MB before a single filter is fetched, on top
// of the same 31.2 MB filter cache and 41 MB block cache every neutrino
// service reserves. Electrum anchors its header store at the most recent
// checkpoint instead.
//
// See the btc package's wallets_mobile.go for the privacy tradeoff this
// accepts and why the SPV code paths stay compiled.
var availableWallets = []*asset.WalletDefinition{
	ltcElectrumWalletDefinition,
}
