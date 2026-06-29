// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"fmt"
	"math/big"
	"os"
	"os/user"
	"path/filepath"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/ethereum/go-ethereum/common"
)

// Token is the definition of an ERC20 token, including all of its network and
// version variants.
type Token struct {
	*util.Token
	// NetTokens is a mapping of token addresses for each network available.
	NetTokens map[util.Network]*NetToken `json:"netAddrs"`
	// EVMFactor allows for arbitrary ERC20 decimals. For an ERC20 contract,
	// the relation
	//    math.Log10(UnitInfo.Conventional.ConversionFactor) + Token.EVMFactor = decimals
	// should hold true.
	// Since most assets will use a value of 9 here, a default value of 9 will
	// be used in AtomicToEVM and EVMToAtomic if EVMFactor is not set.
	EVMFactor *int64 `json:"evmFactor"` // default 9
}

// factor calculates the conversion factor to and from DEX atomic units to the
// units used for EVM operations.
func (t *Token) factor() *big.Int {
	var evmFactor int64 = 9
	if t.EVMFactor != nil {
		evmFactor = *t.EVMFactor
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(evmFactor), nil)
}

// AtomicToEVM converts from DEX atomic units to EVM units.
func (t *Token) AtomicToEVM(v uint64) *big.Int {
	return new(big.Int).Mul(big.NewInt(int64(v)), t.factor())
}

// EVMToAtomic converts from raw EVM units to DEX atomic units.
func (t *Token) EVMToAtomic(v *big.Int) uint64 {
	vDEX := new(big.Int).Div(v, t.factor())
	if vDEX.IsUint64() {
		return vDEX.Uint64()
	}
	return 0
}

// NetToken are the addresses associated with the token and its per-version gas
// tables.
type NetToken struct {
	// Address is the token contract address.
	Address common.Address `json:"address"`
	// Gas is the per-asset-version gas table for token wallet operations.
	Gas map[uint32]*Gases `json:"gas"`
}

// tokenV1Gases are the v1 token gas estimates, measured from usdt.eth mainnet.
// These values are used for all tokens on all networks since the v1 contract
// handles all ERC20 tokens identically.
var tokenV1Gases = Gases{
	Approve:  63_550,
	Transfer: 82_124,
}

// EVMTokens is the aggregate token registry across all EVM chains. Each
// chain's package (polygon, base, ...) calls RegisterEVMTokens from its init()
// so eth-level code can look up tokens by assetID without importing the chain
// packages (which would create import cycles).
var EVMTokens = map[uint32]*Token{}

// RegisterEVMTokens copies the provided tokens into the EVM-wide registry.
// Intended to be called from each EVM chain package's init().
func RegisterEVMTokens(tokens map[uint32]*Token) {
	for id, t := range tokens {
		EVMTokens[id] = t
	}
}

func init() {
	RegisterEVMTokens(Tokens)
}

var Tokens = map[uint32]*Token{
	usdcTokenID: {
		EVMFactor: new(int64),
		Token: &util.Token{
			NetworkID: EthBipID,
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
		NetTokens: map[util.Network]*NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"), // https://etherscan.io/address/0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48
				Gas: map[uint32]*Gases{
					0: {
						// USDC's contract is upgradable, using a proxy call, so
						// gas cost could change without notice, so we do not
						// want to set limits too low, even with live estimates.
						Approve:  78_400,
						Transfer: 85_100,
					},
					1: &tokenV1Gases,
				},
			},
			util.Testnet: {
				Address: common.HexToAddress("0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238"),
				Gas: map[uint32]*Gases{
					0: {
						Approve:  78_400, // actual ~60,190 (initial) -- https://goerli.etherscan.io/tx/0xd695fd174dede7bb798488ead7fed5ef33bcd79932b0fa35db0d17c84c97a8a1, verified on mainnet at 60,311
						Transfer: 85_100, // actual ~65,524 (initial receive, subsequent 48,424)
					},
					1: &tokenV1Gases,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				Gas: map[uint32]*Gases{
					0: {
						Approve:  32_303,
						Transfer: 66_953,
					},
					1: &tokenV1Gases,
				},
			},
		},
	},
	usdtTokenID: {
		EVMFactor: new(int64),
		Token: &util.Token{
			NetworkID: EthBipID,
			Name:      "Tether",
			UnitInfo: util.UnitInfo{
				AtomicUnit: "microUSD",
				Conventional: util.Denomination{
					Unit:             "USDT",
					ConversionFactor: 1e6,
				},
			},
		},
		NetTokens: map[util.Network]*NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0xdac17f958d2ee523a2206206994597c13d831ec7"), // https://etherscan.io/address/0xdac17f958d2ee523a2206206994597c13d831ec7
				Gas: map[uint32]*Gases{
					0: {
						// USDT's contract is upgradable, using a proxy call, so
						// gas cost could change without notice, so we do not
						// want to set limits too low, even with live estimates.
						Approve:  63_566,
						Transfer: 82_124,
					},
					1: &tokenV1Gases,
				},
			},
			util.Testnet: {
				Address: common.HexToAddress("0x7169D38820dfd117C3FA1f22a697dBA58d90BA06"),
				Gas: map[uint32]*Gases{
					0: {
						Approve:  63_609,
						Transfer: 82_196,
					},
					1: &tokenV1Gases,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				Gas: map[uint32]*Gases{
					0: {
						Approve:  32_303,
						Transfer: 66_953,
					},
					1: &tokenV1Gases,
				},
			},
		},
	},
	maticTokenID: {
		Token: &util.Token{
			NetworkID: EthBipID,
			Name:      "MATIC",
			UnitInfo: util.UnitInfo{
				AtomicUnit: "gwei",
				Conventional: util.Denomination{
					Unit:             "MATIC",
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
		NetTokens: map[util.Network]*NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0x7d1afa7b718fb893db30a3abc0cfc608aacfebb0"), // https://etherscan.io/address/0x7d1afa7b718fb893db30a3abc0cfc608aacfebb0
				Gas: map[uint32]*Gases{
					0: {
						Approve:  63_393,
						Transfer: 70_127,
					},
					1: &tokenV1Gases,
				},
			},
		},
	},

	polygonTokenID: {
		Token: &util.Token{
			NetworkID: EthBipID,
			Name:      "POL",
			UnitInfo: util.UnitInfo{
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
			},
		},
		NetTokens: map[util.Network]*NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0x455e53cbb86018ac2b8092fdcd39d8444affc3f6"),
				Gas: map[uint32]*Gases{
					1: &tokenV1Gases,
				},
			},
			util.Testnet: {
				Address: common.HexToAddress("0x44499312f493f62f2dfd3c6435ca3603ebfceeba"),
				Gas: map[uint32]*Gases{
					1: &tokenV1Gases,
				},
			},
		},
	},

	linkTokenID: {
		Token: &util.Token{
			NetworkID: EthBipID,
			Name:      "LINK",
			UnitInfo: util.UnitInfo{
				AtomicUnit: "nLINK",
				Conventional: util.Denomination{
					Unit: "LINK",
					// EVMFactor defaults to 9; log10(1e9)+9=18 decimals.
					ConversionFactor: 1e9,
				},
				FeeRateDenom: "gas",
			},
		},
		NetTokens: map[util.Network]*NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0x514910771AF9Ca656af840dff83E8264EcF986CA"),
				Gas: map[uint32]*Gases{
					1: &tokenV1Gases,
				},
			},
			util.Testnet: {
				Address: common.HexToAddress("0x779877A7B0D9E8603169DdbD7836e478b4624789"), // Sepolia, per https://docs.chain.link/resources/acquire-link
				Gas: map[uint32]*Gases{
					1: &tokenV1Gases,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				Gas: map[uint32]*Gases{
					1: &tokenV1Gases,
				},
			},
		},
	},

	paxgTokenID: {
		Token: &util.Token{
			NetworkID: EthBipID,
			Name:      "PAX Gold",
			UnitInfo: util.UnitInfo{
				AtomicUnit: "nPAXG",
				Conventional: util.Denomination{
					Unit:             "PAXG",
					ConversionFactor: 1e9,
				},
				FeeRateDenom: "gas",
			},
		},
		NetTokens: map[util.Network]*NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0x45804880De22913dAFE09f4980848ECE6EcbAf78"),
				Gas: map[uint32]*Gases{
					1: &tokenV1Gases,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				Gas: map[uint32]*Gases{
					1: &tokenV1Gases,
				},
			},
		},
	},
}

// MaybeReadSimnetAddrs attempts to read the info files generated by the eth
// simnet harness to populate the multibalance and test token addresses.
func MaybeReadSimnetAddrs() {
	MaybeReadSimnetAddrsDir("eth", MultiBalanceAddresses, Tokens[usdcTokenID].NetTokens[util.Simnet], Tokens[usdtTokenID].NetTokens[util.Simnet], Tokens[linkTokenID].NetTokens[util.Simnet], Tokens[paxgTokenID].NetTokens[util.Simnet])
}

// MaybeReadSimnetAddrsDir populates the multibalance address and the test token
// ERC20 addresses from the simnet harness's info files. The swap contracts are
// no longer deployed by the harness, so swap-contract address files are not
// read.
func MaybeReadSimnetAddrsDir(
	dir string,
	multiBalandAddresses map[util.Network]common.Address,
	usdcToken *NetToken,
	usdtToken *NetToken,
	linkToken *NetToken,
	paxgToken *NetToken,
) {
	usr, err := user.Current()
	if err != nil {
		return
	}

	harnessDir := filepath.Join(usr.HomeDir, "dextest", dir)
	fi, err := os.Stat(harnessDir)
	if err != nil {
		return
	}
	if !fi.IsDir() {
		return
	}

	multiBalandAddresses[util.Simnet] = maybeGetContractAddrFromFile(filepath.Join(harnessDir, "multibalance_address.txt"))

	if usdcToken != nil {
		usdcToken.Address = maybeGetContractAddrFromFile(filepath.Join(harnessDir, "test_usdc_contract_address.txt"))
	}

	if usdtToken != nil {
		usdtToken.Address = maybeGetContractAddrFromFile(filepath.Join(harnessDir, "test_usdt_contract_address.txt"))
	}

	if linkToken != nil {
		linkToken.Address = maybeGetContractAddrFromFile(filepath.Join(harnessDir, "test_link_contract_address.txt"))
	}

	if paxgToken != nil {
		paxgToken.Address = maybeGetContractAddrFromFile(filepath.Join(harnessDir, "test_paxg_contract_address.txt"))
	}
}

func maybeGetContractAddrFromFile(fileName string) (addr common.Address) {
	addrBytes, err := os.ReadFile(fileName)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		fmt.Printf("error reading contract address: %v \n", err)
		return
	}
	addrLen := len(addrBytes)
	if addrLen == 0 {
		fmt.Printf("no contract address found at %v \n", fileName)
		return
	}
	addrStr := string(addrBytes[:addrLen-1])
	return common.HexToAddress(addrStr)
}
