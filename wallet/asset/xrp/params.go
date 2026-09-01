// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import "github.com/bisoncraft/caribou/util"

const (
	BipID = 144

	// BaseReserveDrops is the minimum XRP balance required to activate an
	// account. Reduced on 2024-12-02 from 10 XRP to 1 XRP; query
	// server_state for the authoritative network value.
	BaseReserveDrops = 1_000_000

	// OwnerReserveDrops is the per-owned-object reserve. Reduced on
	// 2024-12-02 from 2 XRP to 0.2 XRP per object.
	OwnerReserveDrops = 200_000

	// DefaultFeeDrops is used when the network fee cannot be queried.
	DefaultFeeDrops = 12

	// MaxBlockInterval is the maximum seconds since the last validated ledger
	// before the wallet is considered out of sync.
	MaxBlockInterval = 30

	keyFileName   = "xrp-keyfile.json"
	walletTypeRPC = "rpc"
)

var (
	UnitInfo = util.UnitInfo{
		AtomicUnit: "drops",
		Conventional: util.Denomination{
			Unit:             "XRP",
			ConversionFactor: 1e6,
		},
		// The XRP fee is a flat per-transaction amount (the load-scaled open
		// ledger fee), not a per-byte/per-gas rate.
		FeeRateDenom: "tx",
	}

	// xrplcluster.com is used over Ripple's own s1.ripple.com on mainnet:
	// xrpl.org's docs describe s1.ripple.com as "not for sustained or
	// business use," while xrplcluster.com is the community-run,
	// load-balanced full-history cluster recommended for public/production
	// use. The testnet endpoint is unaffected — s.altnet.rippletest.net is
	// the official XRPL Foundation testnet server and has no such caveat.
	DefaultRPCEndpoints = map[util.Network]string{
		util.Mainnet: "https://xrplcluster.com",
		util.Testnet: "https://s.altnet.rippletest.net:51234",
		util.Simnet:  "http://localhost:15005",
	}

	DefaultWSEndpoints = map[util.Network]string{
		util.Mainnet: "wss://xrplcluster.com",
		util.Testnet: "wss://s.altnet.rippletest.net:51233/",
		util.Simnet:  "ws://localhost:16006",
	}

	usdcTokenID, _ = util.BipSymbolID("usdc.xrp")

	Tokens = map[uint32]*Token{
		usdcTokenID: TokenUSDC,
	}

	// TokenUSDC is Circle's USDC issued currency on the XRP Ledger. Circle
	// uses a non-standard currency code — the hex encoding of ASCII "USDC"
	// left-justified at byte 0 — rather than the generic 3-letter "USD"
	// code most gateways use. Currency code and mainnet issuer verified
	// directly against developers.circle.com/stablecoins/usdc-contract-addresses
	// (2026-07-13). No devnet/testnet issuer is known, so USDC is
	// mainnet-only for now.
	//
	// No canonical Tether-issued USDT trustline on XRPL mainnet exists —
	// confirmed via web search (2026-07-13): Tether does not list XRPL among
	// the networks it issues USDT on. USDT is not registered here (see
	// reviews/stablecoin-support-plan.md Step 3).
	TokenUSDC = &Token{
		Name: "USDC",
		// Hex encoding of ASCII "USDC" left-justified at byte 0, per
		// Circle's non-standard XRPL currency code.
		Currency: "5553444300000000000000000000000000000000",
		Decimals: 6,
		UnitInfo: util.UnitInfo{
			AtomicUnit: "µUSD",
			Conventional: util.Denomination{
				Unit:             "USDC",
				ConversionFactor: 1e6,
			},
			Alternatives: []util.Denomination{
				{
					Unit:             "cents",
					ConversionFactor: 1e2,
				},
			},
		},
		NetIssuers: map[util.Network]string{
			// https://developers.circle.com/stablecoins/usdc-contract-addresses
			util.Mainnet: "rGm7WCVp9gb4jZHWTEtGUr4dd74z2XuWhE",
		},
	}
)

// Token describes an issued currency supported alongside native XRP.
type Token struct {
	Name       string
	Currency   string
	Decimals   int
	UnitInfo   util.UnitInfo
	NetIssuers map[util.Network]string
}
