//go:build !mobile

package ltc

import "github.com/bisoncraft/caribou/wallet/asset"

// availableWallets is the desktop wallet menu. SPV comes first, so it is the
// default: AvailableWallets is ordered and onboarding takes the first seeded
// definition with no required config options. See wallets_mobile.go for the
// phone build, and the btc package's pair for the full rationale.
var availableWallets = []*asset.WalletDefinition{
	spvWalletDefinition,
	ltcElectrumWalletDefinition,
}

// spvWalletDefinition is the neutrino-backed (BIP-157/158) seeded wallet. It is
// defined here rather than alongside WalletInfo because availableWallets above
// is its only reference, and the mobile menu drops it; defining it in ltc.go
// would leave a dead package-level var in every mobile build.
var spvWalletDefinition = &asset.WalletDefinition{
	Type:        walletTypeSPV,
	Tab:         "Native",
	Description: "Use the built-in SPV wallet",
	Seeded:      true,
}
