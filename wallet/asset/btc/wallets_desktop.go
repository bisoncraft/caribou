//go:build !mobile

package btc

import "github.com/bisoncraft/caribou/wallet/asset"

// availableWallets is the desktop wallet menu. SPV comes first, so it is the
// default: AvailableWallets is ordered and onboarding takes the first seeded
// definition with no required config options. A desktop machine has the disk
// for a full header chain and the memory for neutrino's filter and block
// caches, and gets trustless filter matching in return. See wallets_mobile.go
// for the phone build.
var availableWallets = []*asset.WalletDefinition{
	spvWalletDefinition,
	electrumWalletDefinition,
}
