// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	"github.com/btcsuite/btcd/chaincfg"
)

// neutrinoRecoveryWindow is the address gap limit btcwallet recovers across when
// opening a neutrino-backed wallet. Neutrino is filter-scan, so this recovery is
// what drives the compact-filter scan that discovers the wallet's funds. Matches
// the window the previous SPV wallet used.
const neutrinoRecoveryWindow = 250

// SPVChainConfig returns base configured to drive the shared NativeWallet with
// the neutrino (BIP-157/158) backend — i.e. as an SPV wallet rather than an
// electrum one. btc and ltc build their SPV wallet by passing their electrum
// ChainConfig here.
func SPVChainConfig(base ChainConfig) ChainConfig {
	base.BackendKind = wallet.BackendNeutrino
	base.RecoveryWindow = neutrinoRecoveryWindow
	return base
}

// btcSPVChainConfig returns the ChainConfig for a BTC SPV wallet.
func btcSPVChainConfig(chainParams *chaincfg.Params) ChainConfig {
	base := btcElectrumChainConfig(chainParams)
	return SPVChainConfig(base)
}
