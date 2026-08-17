//go:build !mobile

package ltc

// wantWalletTypes is the wallet menu TestDriverInfo expects on desktop, in
// order. The first entry is the default onboarding picks.
var wantWalletTypes = []string{walletTypeSPV, walletTypeElectrum}
