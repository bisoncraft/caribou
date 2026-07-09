// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"math/big"

	"github.com/bisoncraft/caribou/util"
)

const (
	BipID = 397
	// DefaultFee is the default transfer fee in drops. A basic NEAR transfer
	// costs approximately 0.00045 NEAR = 45000 drops.
	DefaultFee = 45_000
	// MaxBlockInterval is the number of seconds since the last header came
	// in over which we consider the chain to be out of sync.
	MaxBlockInterval = 30
)

var (
	UnitInfo = util.UnitInfo{
		AtomicUnit: "drops",
		Conventional: util.Denomination{
			Unit:             "NEAR",
			ConversionFactor: 1e8,
		},
		Alternatives: []util.Denomination{
			{
				Unit:             "mNEAR",
				ConversionFactor: 1e5,
			},
		},
		// The NEAR fee is a flat per-transaction amount, not a per-byte/per-gas
		// rate.
		FeeRateDenom: "tx",
	}

	// YoctoPerDrop is the number of yoctoNEAR per drop.
	// 1 NEAR = 1e24 yoctoNEAR = 1e8 drops, so 1 drop = 1e16 yoctoNEAR.
	YoctoPerDrop = new(big.Int).SetUint64(1e16)

	DefaultRPCEndpoints = map[util.Network]string{
		util.Mainnet: "https://rpc.mainnet.near.org",
		util.Testnet: "https://rpc.testnet.near.org",
		util.Simnet:  "https://rpc.testnet.near.org",
	}

	usdcTokenID, _ = util.BipSymbolID("usdc.near")

	Tokens = map[uint32]*Token{
		usdcTokenID: TokenUSDC,
	}

	// TokenUSDC is Circle's *native* USDC NEP-141 token on NEAR (live since
	// September 2024), not the older Rainbow Bridge "USDC.e" wrapped token.
	// The address originally in reviews/stablecoin-support-plan.md
	// (17208628f84f5d6ad33f0da3bbbeb27ffed7bc5e.factory.bridge.near) was
	// wrong: that's a .factory.bridge.near-style bridged-token name, but
	// its underlying Ethereum contract (0x17208628...) has zero supply and
	// zero holders on Etherscan — not USDC at all, likely a mis-copied or
	// truncated address. The real bridged USDC.e contract on NEAR is
	// a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48.factory.bridge.near
	// (confirmed via nearblocks.io), matching Ethereum's actual USDC
	// contract 0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48 — but native
	// USDC is what wallets should use going forward per Circle, so that's
	// what's registered below.
	//
	// Mainnet and testnet addresses verified directly against
	// developers.circle.com/stablecoins/usdc-contract-addresses and
	// circle.com/multi-chain-usdc/near (2026-07-13), and independently
	// cross-checked as genuinely deployed, non-deleted NEAR accounts via
	// the nearblocks.io indexer API.
	TokenUSDC = &Token{
		Name:     "USDC",
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
		NetContracts: map[util.Network]string{
			util.Mainnet: "17208628f84f5d6ad33f0da3bbbeb27ffcb398eac501a31bd6ad2011e36133a1",
			util.Testnet: "3e2210e1184b45b64c8a434c0a7e7b23cc04ea7eb7a6c3c32520d03d4afcb8af",
		},
	}
)

// Token describes a NEP-141 fungible token supported alongside native NEAR.
type Token struct {
	Name         string
	Decimals     int
	UnitInfo     util.UnitInfo
	NetContracts map[util.Network]string
}

// DropsToYocto converts drops (atomic units) to yoctoNEAR.
func DropsToYocto(drops uint64) *big.Int {
	return new(big.Int).Mul(new(big.Int).SetUint64(drops), YoctoPerDrop)
}

// YoctoToDrops converts yoctoNEAR to drops (atomic units), truncating.
func YoctoToDrops(yocto *big.Int) uint64 {
	return new(big.Int).Div(yocto, YoctoPerDrop).Uint64()
}
