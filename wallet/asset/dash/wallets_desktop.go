//go:build !mobile

package dash

import "github.com/bisoncraft/caribou/wallet/asset"

// availableWallets is the desktop wallet menu. Unlike btc and ltc, Dash is
// ordered electrum-first here too, so electrum is the desktop default as well;
// Dash Core does not implement BIP-157, so its neutrino path is the weaker of
// the two even on a desktop. SPV stays on the menu for anyone who wants it.
// See wallets_mobile.go for the phone build.
var availableWallets = []*asset.WalletDefinition{
	dashElectrumWalletDefinition,
	spvWalletDefinition,
}

// spvWalletDefinition is the WalletDefinition for the neutrino-backed
// (BIP-157/158) seeded Dash wallet. Shares the same lean wallet and database as
// the electrum wallet, driven by the neutrino backend instead. It is defined
// here rather than alongside WalletInfo because availableWallets above is its
// only reference, and the mobile menu drops it; defining it in dash.go would
// leave a dead package-level var in every mobile build.
var spvWalletDefinition = &asset.WalletDefinition{
	Type:        walletTypeSPV,
	Tab:         "Native",
	Description: "Use the built-in SPV wallet",
	ConfigOpts:  nil,
	Seeded:      true,
}
