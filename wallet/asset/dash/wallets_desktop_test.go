//go:build !mobile

package dash

// wantWalletTypes is the wallet menu TestAvailableWallets expects on desktop,
// in order. Electrum is first, so it is the default on desktop too.
var wantWalletTypes = []string{walletTypeElectrum, walletTypeSPV}
