//go:build !mobile

package btc

// wantWalletTypes is the wallet menu TestAvailableWallets expects on desktop,
// in order. The first entry is the default onboarding picks.
var wantWalletTypes = []string{walletTypeSPV, walletTypeElectrum}
