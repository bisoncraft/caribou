//go:build mobile

package ltc

// wantWalletTypes is the wallet menu TestDriverInfo expects on mobile: electrum
// alone, with no SPV entry to select.
var wantWalletTypes = []string{walletTypeElectrum}
