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

// ltcMainnetCertPEM is the pinned TLS leaf certificate served by the built-in
// mainnet LTC electrumx server (electrum-ltc.bysh.me:50002). It is self-signed
// and long-lived (valid until 2036), so it cannot be verified against a public
// CA and is pinned instead. If the server rotates its certificate this must be
// refreshed; re-fetch with:
//
//	openssl s_client -connect electrum-ltc.bysh.me:50002 </dev/null | openssl x509
const ltcMainnetCertPEM = `-----BEGIN CERTIFICATE-----
MIIDcjCCAloCCQDghQZwjGMwajANBgkqhkiG9w0BAQsFADB7MQswCQYDVQQGEwJO
TDEWMBQGA1UECAwNTm9vcmQtSG9sbGFuZDESMBAGA1UEBwwJQW1zdGVyZGFtMSEw
HwYDVQQKDBhJbnRlcm5ldCBXaWRnaXRzIFB0eSBMdGQxHTAbBgNVBAMMFGVsZWN0
cnVtLWx0Yy5ieXNoLm1lMB4XDTI2MDUwNDA4MTQ0NVoXDTM2MDUwMzA4MTQ0NVow
ezELMAkGA1UEBhMCTkwxFjAUBgNVBAgMDU5vb3JkLUhvbGxhbmQxEjAQBgNVBAcM
CUFtc3RlcmRhbTEhMB8GA1UECgwYSW50ZXJuZXQgV2lkZ2l0cyBQdHkgTHRkMR0w
GwYDVQQDDBRlbGVjdHJ1bS1sdGMuYnlzaC5tZTCCASIwDQYJKoZIhvcNAQEBBQAD
ggEPADCCAQoCggEBAPqzSqn/9DPbD5LFp50+QaJWE+i2P/fSmbHzWW6fWSRPNFkM
UEm/fvxtNNwqZxIMXBNsO79KBNazIbQGZ1BszF98zlHRu0bu6NPlB0HbCjO8nAl/
3JNnJscEnXvDc2qc6A2azHYO4Lo1v5GH0XPL19PUaOVAFLUq9cCNLfKeAxdIdbOV
IZa/3fU3Xp+hAXrNba4irIZYVfu4102uRt2rvbODY8LyAMaQil3h2ao3ODBxL+qB
avQUdYnVgD6GmV3kaWM93Stt/lXuSddh1o6b43GA6z/jS7+3zenu03RCIdZNwOdL
kY5u+bKSsRy551ZMtTB++FFoDzaizZEdJtvVIaECAwEAATANBgkqhkiG9w0BAQsF
AAOCAQEAUcyVcngv5eaUvYL2Cw7zdGfM/aNkTmDSdZGkRhQ9oofWCtDXuzoN5ggK
tIcrAVr+itI684aHKqFgEmuKYGq9+L8T+v1x51vYUgOdqlZi4Z3EVqXqPuZGZZmS
ZXIruwgA+fVVqtKiH/RQSyuBnFSb0h3//Jf5QC31I/WxwMPDoHOpwX2J7qv++erO
v39IxLT/zL+e8s2QgE7qTn2Fof6QG6Gg7Dg/ipuWIn7iRagaz5yd/RVdoGyGgvkG
C3ReuQrfaqq2jkZp7LJL2zi3L98bqdKfp4kLkqw8+pFVPQy4E6VdYwh0H7tNYYAv
JF0LTH0Kr986wDhHDvdCIaV6DR4s7w==
-----END CERTIFICATE-----`

// ltcMainnetCert is the DER form of ltcMainnetCertPEM, pinned for the mainnet
// server. Decoded at init; a malformed constant panics at startup.
var ltcMainnetCert = electrum.MustParseCertPEM(ltcMainnetCertPEM)

// ltcElectrumServers maps chaincfg.Params.Name to a built-in electrumx
// ServerAddr for Litecoin. The regtest/simnet entry points at the dextest
// harness (LTC port 55002, ssl, self-signed → SkipVerify); testnet is a public
// self-signed server (SkipVerify); mainnet pins its self-signed certificate.
// Keys are the actual *chaincfg.Params.Name values so they cannot drift out of
// sync with parseChainParams.
var ltcElectrumServers = map[string]*electrum.ServerAddr{
	MainNetParams.Name:       {Proto: "ssl", Addr: "electrum-ltc.bysh.me:50002", Cert: ltcMainnetCert},
	TestNet4Params.Name:      {Proto: "ssl", Addr: "electrum-ltc.bysh.me:51002", SkipVerify: true},
	RegressionNetParams.Name: {Proto: "ssl", Addr: "127.0.0.1:55002", SkipVerify: true},
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
