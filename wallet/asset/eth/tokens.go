// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"fmt"
	"math/big"
	"os"
	"os/user"
	"path/filepath"

	"github.com/bisoncraft/caribou/util"
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

// NetToken is a token's contract address and gas estimates on one network.
type NetToken struct {
	// Address is the token contract address.
	Address common.Address `json:"address"`
	// GasEstimate is the token's typical gas usage for wallet operations on
	// this network. It is used for fee estimation only; broadcast transactions
	// use a live eth_estimateGas result instead. See Gases.
	GasEstimate *Gases `json:"gasEstimate"`
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
				// USDC's contract is upgradable, using a proxy call, so gas cost could
				// change without notice, so estimates are kept well above measured usage.
				GasEstimate: &Gases{
					Approve:  78_400,
					Transfer: 85_100,
				},
			},
			util.Testnet: {
				Address: common.HexToAddress("0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238"),
				// Approve actual ~60,190 (initial) -- https://goerli.etherscan.io/tx/0xd695fd174dede7bb798488ead7fed5ef33bcd79932b0fa35db0d17c84c97a8a1, verified on mainnet at 60,311.
				// Transfer actual ~65,524 (initial receive, subsequent 48,424).
				GasEstimate: &Gases{
					Approve:  78_400,
					Transfer: 85_100,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				GasEstimate: &Gases{
					Approve:  32_303,
					Transfer: 66_953,
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
				// Average of 2 approvals: 48,897. Average of 1 transfer: 63,173.
				// Mainnet evidence:
				//   approve:   0x4e0b729387bb694a490df2445d5105bd519a36df8acbbbe84da82f4f2b2a0467
				//   transfer:  0xbee8582a7a1d78103fa71222dd6e6df43e82bfc21b8c4c7d9fac1dd1950d05d8
				// Approvals: [48885 48885]
				// Transfers: [63173]
				// The "usdt.eth measurements x1.3" figure (63,550 / 82,124) used by
				// unmeasured tokens below derives from this 48,885 / 63,173 pair.
				// USDT's contract is upgradable, using a proxy call, so gas cost could
				// change without notice.
				GasEstimate: &Gases{
					Approve:  63_566,
					Transfer: 82_124,
				},
			},
			util.Testnet: {
				Address: common.HexToAddress("0x7169D38820dfd117C3FA1f22a697dBA58d90BA06"),
				// Average of 2 approvals: 48,930. Average of 1 transfer: 63,228.
				GasEstimate: &Gases{
					Approve:  63_609,
					Transfer: 82_196,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				GasEstimate: &Gases{
					Approve:  32_303,
					Transfer: 66_953,
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
				// Not measured for this token; usdt.eth measurements x1.3.
				GasEstimate: &Gases{
					Approve:  63_550,
					Transfer: 82_124,
				},
			},
			util.Testnet: {
				Address: common.HexToAddress("0x779877A7B0D9E8603169DdbD7836e478b4624789"), // Sepolia, per https://docs.chain.link/resources/acquire-link
				// Not measured for this token; usdt.eth measurements x1.3.
				GasEstimate: &Gases{
					Approve:  63_550,
					Transfer: 82_124,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				// Not measured for this token; usdt.eth measurements x1.3.
				GasEstimate: &Gases{
					Approve:  63_550,
					Transfer: 82_124,
				},
			},
		},
	},

	// PAXG has a Paxos-controlled feeRate and can deduct a transfer fee from
	// the sent amount, so transfer(to, amount) may credit the recipient less
	// than amount. feeRate is currently 0, so sent == received in practice,
	// but if Paxos ever enables it, balance/tx accounting here (which assumes
	// standard ERC-20 semantics) could drift.
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
				// Not measured for this token; usdt.eth measurements x1.3.
				GasEstimate: &Gases{
					Approve:  63_550,
					Transfer: 82_124,
				},
			},
			util.Testnet: {
				Address: common.HexToAddress("0xe0d215bc63580325794d858c9c49f61e77704501"), // Sepolia, per https://docs.paxos.com/guides/stablecoin/paxg/testnet
				// Not measured for this token; usdt.eth measurements x1.3.
				GasEstimate: &Gases{
					Approve:  63_550,
					Transfer: 82_124,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				// Not measured for this token; usdt.eth measurements x1.3.
				GasEstimate: &Gases{
					Approve:  63_550,
					Transfer: 82_124,
				},
			},
		},
	},

	xautTokenID: {
		EVMFactor: new(int64),
		Token: &util.Token{
			NetworkID: EthBipID,
			Name:      "Tether Gold",
			UnitInfo: util.UnitInfo{
				AtomicUnit: "µXAUT",
				Conventional: util.Denomination{
					Unit:             "XAUT",
					ConversionFactor: 1e6,
				},
				FeeRateDenom: "gas",
			},
		},
		NetTokens: map[util.Network]*NetToken{
			util.Mainnet: {
				Address: common.HexToAddress("0x68749665FF8D2d112Fa859AA293F07A622782F38"), // https://etherscan.io/token/0x68749665ff8d2d112fa859aa293f07a622782f38
				// Not measured for this token; usdt.eth measurements x1.3.
				GasEstimate: &Gases{
					Approve:  63_550,
					Transfer: 82_124,
				},
			},
			util.Simnet: {
				Address: common.Address{},
				// Not measured for this token; usdt.eth measurements x1.3.
				GasEstimate: &Gases{
					Approve:  63_550,
					Transfer: 82_124,
				},
			},
		},
	},
}

// MaybeReadSimnetAddrs attempts to read the info files generated by the eth
// simnet harness to populate the multibalance and test token addresses.
func MaybeReadSimnetAddrs() {
	MaybeReadSimnetAddrsDir("eth", MultiBalanceAddresses, Tokens[usdcTokenID].NetTokens[util.Simnet], Tokens[usdtTokenID].NetTokens[util.Simnet], Tokens[linkTokenID].NetTokens[util.Simnet], Tokens[paxgTokenID].NetTokens[util.Simnet], Tokens[xautTokenID].NetTokens[util.Simnet])
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
	xautToken *NetToken,
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

	if xautToken != nil {
		xautToken.Address = maybeGetContractAddrFromFile(filepath.Join(harnessDir, "test_xaut_contract_address.txt"))
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
