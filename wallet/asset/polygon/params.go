// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package polygon

import (
	"github.com/bisoncraft/caribou/util"
	dexeth "github.com/bisoncraft/caribou/wallet/asset/eth"
	"github.com/ethereum/go-ethereum/common"
)

const (
	PolygonBipID = 966
)

var (
	UnitInfo = util.UnitInfo{
		AtomicUnit: "gwei",
		Conventional: util.Denomination{
			Unit:             "POL",
			ConversionFactor: 1e9,
		},
		Alternatives: []util.Denomination{
			{
				Unit:             "Szabos",
				ConversionFactor: 1e6,
			},
			{
				Unit:             "Finneys",
				ConversionFactor: 1e3,
			},
		},
		FeeRateDenom: "gas",
	}

	// v0Gases and v1Gases are the base-chain POL gas tables. The native asset
	// does not use token Transfer/Approve, so these are empty placeholders that
	// preserve the supported version keys.
	v0Gases = &dexeth.Gases{}

	v1Gases = &dexeth.Gases{}

	// tokenV1Gases are the v1 token gas estimates, measured from usdt.polygon
	// mainnet. These values are used for all tokens on all networks since the
	// v1 contract handles all ERC20 tokens identically.
	tokenV1Gases = dexeth.Gases{
		Approve:  67_597,
		Transfer: 77_183,
	}

	VersionedGases = map[uint32]*dexeth.Gases{
		0: v0Gases,
		1: v1Gases,
	}

	MultiBalanceAddresses = map[util.Network]common.Address{
		util.Mainnet: common.HexToAddress("0x23d8203d8E3c839F359bcC85BFB71cf0d707EDF0"), // tx: 0xc593222106c700b153977fdf290f8d9656610cd2dd88522724e85b3f7fd600cf
		util.Testnet: common.HexToAddress("0xa958d5B8a3a29E3f5f41742Fbb939A0dd93EB418"), // tx 0x692cf15b145cb45c0098bedf8a55d067b1ac994973bb62000c046b8453d8b624
	}

	usdcTokenID, _ = util.BipSymbolID("usdc.polygon")
	usdtTokenID, _ = util.BipSymbolID("usdt.polygon")
	wethTokenID, _ = util.BipSymbolID("weth.polygon")
	wbtcTokenID, _ = util.BipSymbolID("wbtc.polygon")

	Tokens = map[uint32]*dexeth.Token{
		usdcTokenID: TokenUSDC,
		usdtTokenID: TokenUSDT,
		wethTokenID: TokenWETH,
		wbtcTokenID: TokenWBTC,
	}

	TokenUSDC = &dexeth.Token{
		EVMFactor: new(int64),
		Token: &util.Token{
			NetworkID: PolygonBipID,
			Name:      "USDC",
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
				FeeRateDenom: "gas",
			},
		},
		NetTokens: map[util.Network]*dexeth.NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0x3c499c542cef5e3811e1192ce70d8cc03d5c3359"), // https://polygonscan.com/address/0x3c499c542cef5e3811e1192ce70d8cc03d5c3359
				Gas: map[uint32]*dexeth.Gases{
					0: {
						Approve:  78_215,
						Transfer: 85_150,
					},
					1: &tokenV1Gases,
				},
			},
			util.Testnet: {
				Address: common.HexToAddress("0x41E94Eb019C0762f9Bfcf9Fb1E58725BfB0e7582"), // https://amoy.polygonscan.com/address/0x41E94Eb019C0762f9Bfcf9Fb1E58725BfB0e7582#readContract
				Gas: map[uint32]*dexeth.Gases{
					0: {
						Approve:  76_224,
						Transfer: 82_816,
					},
					1: &tokenV1Gases,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				Gas: map[uint32]*dexeth.Gases{
					0: {
						Approve:  58_180,
						Transfer: 64_539,
					},
					1: &tokenV1Gases,
				},
			},
		},
	}

	TokenUSDT = &dexeth.Token{
		EVMFactor: new(int64),
		Token: &util.Token{
			NetworkID: PolygonBipID,
			Name:      "Tether",
			UnitInfo: util.UnitInfo{
				AtomicUnit: "microUSD",
				Conventional: util.Denomination{
					Unit:             "USDT",
					ConversionFactor: 1e6,
				},
			},
		},
		NetTokens: map[util.Network]*dexeth.NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0xc2132D05D31c914a87C6611C10748AEb04B58e8F"), // https://polygonscan.com/address/0xc2132D05D31c914a87C6611C10748AEb04B58e8F
				Gas: map[uint32]*dexeth.Gases{
					0: {
						Approve:  67_693,
						Transfer: 74_451,
					},
					// Mainnet v1 token evidence:
					//   approve:   0x952edd274572f22db7e7631da7cf57430a1ae1736b83350f5b7e11d600863879
					//   transfer:  0xf9bc549ac8d20b0eda780edfc6fc8223a527553483431c20855c39ce354b152f
					// Approvals: [51998 51998]
					// Transfers: [59372]
					1: &tokenV1Gases,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				Gas: map[uint32]*dexeth.Gases{
					0: {
						Approve:  58_180,
						Transfer: 64_539,
					},
					1: &tokenV1Gases,
				},
			},
		},
	}

	// TokenWETH is for Wrapped ETH.
	TokenWETH = &dexeth.Token{
		Token: &util.Token{
			NetworkID: PolygonBipID,
			Name:      "Wrapped Ether",
			UnitInfo: util.UnitInfo{
				AtomicUnit: "gwei",
				Conventional: util.Denomination{
					Unit:             "WETH",
					ConversionFactor: 1e9,
				},
				Alternatives: []util.Denomination{
					{
						Unit:             "Szabos",
						ConversionFactor: 1e6,
					},
					{
						Unit:             "Finneys",
						ConversionFactor: 1e3,
					},
				},
				FeeRateDenom: "gas",
			},
		},
		NetTokens: map[util.Network]*dexeth.NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0x7ceb23fd6bc0add59e62ac25578270cff1b9f619"), // https://polygonscan.com/token/0x7ceb23fd6bc0add59e62ac25578270cff1b9f619
				Gas: map[uint32]*dexeth.Gases{
					0: {
						Approve:  56_054,
						Transfer: 67_483,
					},
					1: &tokenV1Gases,
				},
			},
			util.Testnet: {
				Address: common.HexToAddress("0x52ef3d68bab452a294342dc3e5f464d7f610f72e"),
				Gas: map[uint32]*dexeth.Gases{
					1: &tokenV1Gases,
				},
			},
		},
	}

	// TokenWBTC is for Wrapped BTC.
	TokenWBTC = &dexeth.Token{
		EVMFactor: new(int64),
		Token: &util.Token{
			NetworkID: PolygonBipID,
			Name:      "Wrapped Bitcoin",
			UnitInfo: util.UnitInfo{
				AtomicUnit: "Sats",
				Conventional: util.Denomination{
					Unit:             "WBTC",
					ConversionFactor: 1e8,
				},
				Alternatives: []util.Denomination{
					{
						Unit:             "mWBTC",
						ConversionFactor: 1e5,
					},
					{
						Unit:             "µWBTC",
						ConversionFactor: 1e2,
					},
				},
				FeeRateDenom: "gas",
			},
		},
		NetTokens: map[util.Network]*dexeth.NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0x1bfd67037b42cf73acf2047067bd4f2c47d9bfd6"), // https://polygonscan.com/token/0x1bfd67037b42cf73acf2047067bd4f2c47d9bfd6
				Gas: map[uint32]*dexeth.Gases{
					0: {
						Approve:  67_693,
						Transfer: 74_451,
					},
					1: &tokenV1Gases,
				},
			},
		},
	}
)

// MaybeReadSimnetAddrs attempts to read the info files generated by the eth
// simnet harness to populate token addresses in Tokens.
func MaybeReadSimnetAddrs() {
	dexeth.MaybeReadSimnetAddrsDir("polygon", MultiBalanceAddresses, Tokens[usdcTokenID].NetTokens[util.Simnet], Tokens[usdtTokenID].NetTokens[util.Simnet], nil, nil, nil)
}
