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

// spvWalletDefinition is the neutrino-backed (BIP-157/158) seeded wallet. It is
// defined here rather than alongside WalletInfo because availableWallets above
// is its only reference, and the mobile menu drops it; defining it in btc.go
// would leave a dead package-level var in every mobile build.
var spvWalletDefinition = &asset.WalletDefinition{
	Type:        walletTypeSPV,
	Tab:         "Native",
	Description: "Use the built-in SPV wallet",
	Seeded:      true,
}
