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
