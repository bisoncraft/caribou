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
