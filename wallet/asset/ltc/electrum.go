// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package ltc

// This file wires the electrum-backed LTC wallet. The actual asset.Wallet
// implementation is shared with BTC — btc.ElectrumWallet, parameterized by a
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
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/electrum"
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

// ltcElectrumServers maps chaincfg.Params.Name to a built-in electrumx
// ServerAddr for Litecoin. The regtest/simnet entry points at the dextest
// harness (LTC port 55002, ssl); mainnet and testnet use well-known public
// servers. Keys are the actual *chaincfg.Params.Name values so they cannot
// drift out of sync with parseChainParams.
var ltcElectrumServers = map[string]*electrum.ServerAddr{
	MainNetParams.Name:       {Proto: "ssl", Addr: "electrum-ltc.bysh.me:50002"},
	TestNet4Params.Name:      {Proto: "ssl", Addr: "electrum-ltc.bysh.me:51002"},
	RegressionNetParams.Name: {Proto: "ssl", Addr: "127.0.0.1:55002"},
}

// ltcChainConfig is the LTC parameterization of the shared electrum wallet.
func ltcChainConfig(chainParams *chaincfg.Params) btc.ChainConfig {
	return btc.ChainConfig{
		Chain:       bisonwire.ChainLTC,
		Servers:     ltcElectrumServers,
		ChainParams: chainParams,
		NetParams:   ltcNetParams(chainParams),
		Info:        WalletInfo,
		LogWallet:   logNeutrino,
	}
}

// createLTCElectrumWallet creates an LTC electrum wallet's on-disk database via
// the shared implementation. The electrum network is not started here.
func createLTCElectrumWallet(privPass, seed []byte, bday time.Time, walletDir string, log util.Logger, extIdx, intIdx uint32, net *chaincfg.Params) error {
	return btc.CreateElectrumWallet(ltcChainConfig(net), privPass, seed, bday, walletDir, log, extIdx, intIdx)
}

// openLTCElectrumWallet constructs the shared electrum wallet with LTC's config.
// Called by NewWallet for walletTypeElectrum.
func openLTCElectrumWallet(cfg *asset.WalletConfig, chainParams *chaincfg.Params, log util.Logger) *btc.ElectrumWallet {
	return btc.NewElectrumWallet(ltcChainConfig(chainParams), cfg, log)
}
