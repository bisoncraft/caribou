// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import "github.com/bisoncraft/bosswallet/util"

const (
	BipID = 501

	DefaultFee = 5_000

	MaxBlockInterval = 30

	keyFileName   = "sol-keyfile.json"
	walletTypeRPC = "rpc"
)

var (
	UnitInfo = util.UnitInfo{
		AtomicUnit: "lamports",
		Conventional: util.Denomination{
			Unit:             "SOL",
			ConversionFactor: 1e9,
		},
	}

	DefaultRPCEndpoints = map[util.Network]string{
		util.Mainnet: "https://api.mainnet-beta.solana.com",
		util.Testnet: "https://api.testnet.solana.com",
		util.Simnet:  "http://127.0.0.1:18999",
	}

	DefaultWSEndpoints = map[util.Network]string{
		util.Mainnet: "wss://api.mainnet-beta.solana.com",
		util.Testnet: "wss://api.testnet.solana.com",
		util.Simnet:  "ws://127.0.0.1:19000",
	}

	usdcTokenID, _ = util.BipSymbolID("usdc.sol")
	usdtTokenID, _ = util.BipSymbolID("usdt.sol")

	Tokens = map[uint32]*Token{
		usdcTokenID: TokenUSDC,
		usdtTokenID: TokenUSDT,
	}

	// TokenUSDC is Circle's USDC SPL token. Mainnet mint verified against
	// the solana-labs/token-list registry; devnet mint verified against
	// Circle's own developer docs (2026-07-13).
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
			// Circle's devnet USDC faucet mint, per Circle's developer docs.
			util.Testnet: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
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
