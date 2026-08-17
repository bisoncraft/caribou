//go:build mobile

package btc

// wantWalletTypes is the wallet menu TestAvailableWallets expects on mobile:
// electrum alone, with no SPV entry to select.
var wantWalletTypes = []string{walletTypeElectrum}
