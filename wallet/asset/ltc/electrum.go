// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package ltc

// This file wires the electrum-backed LTC wallet. The actual asset.Wallet
// implementation is shared with BTC — btc.NativeWallet, parameterized by a
// btc.ChainConfig. LTC only supplies its chain-specific config (servers, params,
// wallet info, logger) plus driver registration; everything else (Connect,
// Balance, Send, SyncStatus, tx-history sync, recovery, …) is the shared code,
// so LTC inherits changes — including electrum gap-limit recovery — for free.
//
// The shared implementation lives in wallet/asset/btc for now; it will move to a
// neutral home later, together with the rest of the shared wallet.Wallet stack
// (which is why asset/ltc still imports asset/btc).
//
// LTC MWEB note: Litecoin's MWEB extension-block data is stripped from
// transactions by the bisonwire MWEB-aware decoder — the same path the neutrino
// backend uses. Electrum transactions arrive as raw hex and pass through it, so
// any MWEB-only outputs are silently dropped; transparent-output operations
// (balance, receive, send) are unaffected. This is the highest LTC integration
// risk and is covered by the harness tests.

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

// ltcElectrumWalletDefinition is the WalletDefinition for the electrum-backed
// seeded LTC wallet. No user-configurable server/proxy knobs.
var ltcElectrumWalletDefinition = &asset.WalletDefinition{
	Type:        walletTypeElectrum,
	Tab:         "Electrum",
	Description: "Use the built-in electrum-backed light wallet.",
	ConfigOpts:  nil,
	Seeded:      true,
}

// ltcElectrumChainConfig is the LTC parameterization of the shared electrum wallet.
func ltcElectrumChainConfig(chainParams *chaincfg.Params) btc.ChainConfig {
	return btc.ChainConfig{
		Chain:       bisonwire.ChainLTC,
		ChainParams: chainParams,
		NetParams:   ltcTableNetParams(chainParams),
		Info:        WalletInfo,
	}
}

// ltcTableNetParams looks up the *netparams.ChainParams for chainParams from
// the assets table, which carries ElectrumServers and NeutrinoSeeds. It does
// NOT call chaincfg.Register; registration is only done when explicitly needed.
func ltcTableNetParams(chainParams *chaincfg.Params) *netparams.ChainParams {
	switch chainParams.Name {
	case TestNet4Params.Name:
		return assets.LTCParams["testnet"]
	case RegressionNetParams.Name:
		return assets.LTCParams["simnet"]
	default:
		return assets.LTCParams["mainnet"]
	}
}

// createLTCElectrumWallet creates an LTC electrum wallet's on-disk database via
// the shared implementation. The electrum network is not started here.
func createLTCElectrumWallet(privPass, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32, net *chaincfg.Params) error {
	return btc.CreateElectrumWallet(ltcElectrumChainConfig(net), privPass, seed, bday, walletDir, log, extIdx, intIdx)
}

// openLTCElectrumWallet constructs the shared electrum wallet with LTC's config.
// Called by NewWallet for walletTypeElectrum.
func openLTCElectrumWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *btc.NativeWallet {
	return btc.NewNativeWallet(ltcElectrumChainConfig(chainParams), cfg, log)
}

// ltcSPVChainConfig is the LTC parameterization of the shared NativeWallet driven
// by the neutrino (BIP-157/158) backend rather than electrum: same wallet, LTC's
// seeds. Neutrino seeds are sourced from the assets table via the NetParams field.
func ltcSPVChainConfig(chainParams *chaincfg.Params) btc.ChainConfig {
	base := ltcElectrumChainConfig(chainParams)
	return btc.SPVChainConfig(base)
}

// openLTCSPVWallet constructs the shared NativeWallet driven by the neutrino
// backend with LTC's config. Called by NewWallet for walletTypeSPV.
func openLTCSPVWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *btc.NativeWallet {
	return btc.NewNativeWallet(ltcSPVChainConfig(chainParams), cfg, log)
}
