// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import "github.com/bisoncraft/caribou/util"

const (
	BipID = 501

	DefaultFee = 5_000

	MaxBlockInterval = 30

	keyFileName   = "sol-keyfile.json"
	walletTypeRPC = "rpc"

	version = 0
)

var (
	UnitInfo = util.UnitInfo{
		AtomicUnit: "lamports",
		Conventional: util.Denomination{
			Unit:             "SOL",
			ConversionFactor: 1e9,
		},
		// The Solana fee is a flat per-transaction amount (base fee per
		// signature), not a per-byte/per-gas rate.
		FeeRateDenom: "tx",
	}

	// PublicNode is used over Solana's own api.*.solana.com clusters, which
	// Solana's docs describe as a lightweight-testing endpoint only (rate
	// capped at 100 req/10s per IP, no SLA) rather than a general-purpose
	// public default.
	DefaultRPCEndpoints = map[util.Network]string{
		util.Mainnet: "https://solana-rpc.publicnode.com",
		util.Testnet: "https://solana-testnet-rpc.publicnode.com",
		util.Simnet:  "http://127.0.0.1:18999",
	}

	DefaultWSEndpoints = map[util.Network]string{
		util.Mainnet: "wss://solana-rpc.publicnode.com",
		util.Testnet: "wss://solana-testnet-rpc.publicnode.com",
		util.Simnet:  "ws://127.0.0.1:19000",
	}

	usdcTokenID, _ = util.BipSymbolID("usdc.sol")
	usdtTokenID, _ = util.BipSymbolID("usdt.sol")

	Tokens = map[uint32]*Token{
		usdcTokenID: TokenUSDC,
		usdtTokenID: TokenUSDT,
	}

	// TokenUSDC is Circle's USDC SPL token. Mainnet mint verified against
	// the solana-labs/token-list registry. Circle's only public test mint is
	// on the devnet cluster, not testnet (DefaultRPCEndpoints[util.Testnet]
	// points at the testnet cluster, and util.Network has no separate Devnet
	// value), so no testnet mint is listed here — same as USDT below.
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
		NetMints: map[util.Network]string{
			// https://solscan.io/token/EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v
			util.Mainnet: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		},
	}

	// TokenUSDT is Tether's USDT SPL token. Mainnet mint verified against
	// Solana Explorer, Solscan, Phantom, and OpenSea (2026-07-13). No
	// devnet mint is known, so USDT is mainnet-only for now.
	TokenUSDT = &Token{
		Name:     "USDT",
		Decimals: 6,
		UnitInfo: util.UnitInfo{
			AtomicUnit: "microUSD",
			Conventional: util.Denomination{
				Unit:             "USDT",
				ConversionFactor: 1e6,
			},
		},
		NetMints: map[util.Network]string{
			// https://solscan.io/token/Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB
			util.Mainnet: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
		},
	}
)

// Token describes an SPL token supported alongside native SOL.
type Token struct {
	Name     string
	Decimals uint8
	UnitInfo util.UnitInfo
	NetMints map[util.Network]string
}
