// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dash

// This file wires the electrum-backed Dash wallet. The asset.Wallet
// implementation is the shared btc.NativeWallet, parameterized by a
// btc.ChainConfig; Dash only supplies its chain-specific config (params, wallet
// info) and driver registration. The Dash-specific block/tx decoding (DIP-2
// special transactions) and the X11 block-header hash are handled in the
// utxowallet layers (bisonwire, netparams HeaderHash hook), so the wallet code
// itself is chain-agnostic.
//
// Dash has no SegWit, so the wallet derives legacy BIP-44 P2PKH addresses; this
// is driven by netparams.KeyPurposes ({44}), set on the assets table entry.

import (
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/btc"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/assets"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/chaincfg"
)

// dashElectrumWalletDefinition is the WalletDefinition for the electrum-backed
// seeded Dash wallet. No user-configurable server/proxy knobs.
var dashElectrumWalletDefinition = &asset.WalletDefinition{
	Type:        walletTypeElectrum,
	Tab:         "Electrum",
	Description: "Use the built-in electrum-backed light wallet.",
	ConfigOpts:  nil,
	Seeded:      true,
}

// dashElectrumChainConfig is the Dash parameterization of the shared electrum
// wallet.
func dashElectrumChainConfig(chainParams *chaincfg.Params) btc.ChainConfig {
	return btc.ChainConfig{
		Chain:       bisonwire.ChainDASH,
		ChainParams: chainParams,
		NetParams:   dashTableNetParams(chainParams),
		Info:        WalletInfo,
		// Enable the gap-limit address-discovery scan on every connect, matching
		// BTC and LTC. Without it, a wallet restored from seed (or one that
		// received funds while offline) never rediscovers its addresses, so its
		// balance is not recovered. The neutrino config overrides this window via
		// SPVChainConfig.
		RecoveryWindow: btc.ElectrumRecoveryWindow,
	}
}

// dashTableNetParams looks up the *netparams.ChainParams for chainParams from
// the assets table, which carries the genesis, X11 hooks, electrum servers and
// neutrino seeds. It does NOT call chaincfg.Register.
func dashTableNetParams(chainParams *chaincfg.Params) *netparams.ChainParams {
	switch chainParams.Name {
	case TestNetParams.Name:
		return assets.DashParams["testnet"]
	case RegressionNetParams.Name:
		return assets.DashParams["simnet"]
	default:
		return assets.DashParams["mainnet"]
	}
}

// createDashElectrumWallet creates a Dash electrum wallet's on-disk database via
// the shared implementation. The electrum network is not started here.
func createDashElectrumWallet(privPass, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32, net *chaincfg.Params) error {
	return btc.CreateElectrumWallet(dashElectrumChainConfig(net), privPass, seed, bday, walletDir, log, extIdx, intIdx)
}

// openDashElectrumWallet constructs the shared electrum wallet with Dash's
// config. Called by newWallet for walletTypeElectrum.
func openDashElectrumWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *btc.NativeWallet {
	return btc.NewNativeWallet(dashElectrumChainConfig(chainParams), cfg, log)
}

// dashSPVChainConfig is the Dash parameterization of the shared NativeWallet
// driven by the neutrino (BIP-157/158) backend rather than electrum: same lean
// wallet, neutrino seeds sourced from the assets table via the NetParams field.
// Dash header validation is X11 throughout the neutrino stack, and the peer
// service mask drops NODE_WITNESS for Dash.
func dashSPVChainConfig(chainParams *chaincfg.Params) btc.ChainConfig {
	return btc.SPVChainConfig(dashElectrumChainConfig(chainParams))
}

// openDashSPVWallet constructs the shared NativeWallet driven by the neutrino
// backend with Dash's config. Called by newWallet for walletTypeSPV.
func openDashSPVWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *btc.NativeWallet {
	return btc.NewNativeWallet(dashSPVChainConfig(chainParams), cfg, log)
}
