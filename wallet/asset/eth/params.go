// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	"github.com/bisoncraft/caribou/util"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
)

const (
	// GweiFactor is the amount of wei in one gwei.
	GweiFactor = 1e9
	// MaxBlockInterval is the number of seconds since the last header came
	// in over which we consider the chain to be out of sync.
	MaxBlockInterval = 180
	EthBipID         = 60
	MinGasTipCap     = 2 //gwei
)

// These are the chain IDs of the various Ethereum network supported.
const (
	MainnetChainID = 1
	TestnetChainID = 11155111 // Sepolia
	SimnetChainID  = 1337     // see dex/testing/eth/harness.sh
)

var (
	// ChainIDs is a map of the network name to it's chain ID.
	ChainIDs = map[util.Network]int64{
		util.Mainnet: MainnetChainID,
		util.Testnet: TestnetChainID,
		util.Simnet:  SimnetChainID,
	}

	UnitInfo = util.UnitInfo{
		AtomicUnit: "gwei",
		Conventional: util.Denomination{
			Unit:             "ETH",
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

	MultiBalanceAddresses = map[util.Network]common.Address{
		util.Mainnet: common.HexToAddress("0x73bc803A2604b2c58B8680c3CE1b14489842EF16"), // tx 0xaf6cb861578c0ded0750397d7e044a7dd86c94aa47211d02188e146a2424dda4
		util.Testnet: common.HexToAddress("0x8Bd6F6dBe69588D94953EE289Fd3E1db3e8dB43D"), // tx 0x46a416344927a8d1f33865374e9b9e824249980da8d34f6c3214a1ee036ca5fe
	}
)

// LoadGenesisFile loads a Genesis config from a json file.
func LoadGenesisFile(genesisFile string) (*core.Genesis, error) {
	fid, err := os.Open(genesisFile)
	if err != nil {
		return nil, err
	}
	defer fid.Close()

	var genesis core.Genesis
	err = json.NewDecoder(fid).Decode(&genesis)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal simnet genesis: %v", err)
	}
	return &genesis, nil
}

var (
	// add before diving by gweiFactorBig to take the ceiling.
	gweiCeilAddend = big.NewInt(GweiFactor - 1)
	gweiFactorBig  = big.NewInt(GweiFactor)
)

// GweiToWei converts uint64 Gwei to *big.Int Wei.
func GweiToWei(v uint64) *big.Int {
	return new(big.Int).Mul(big.NewInt(int64(v)), gweiFactorBig)
}

// WeiToGwei converts *big.Int Wei to uint64 Gwei. If v is determined to be
// unsuitable for a uint64, zero is returned. For values that are not even
// multiples of 1 gwei, this function returns the floor.
func WeiToGwei(v *big.Int) uint64 {
	vGwei := new(big.Int).Div(v, gweiFactorBig)
	if vGwei.IsUint64() {
		return vGwei.Uint64()
	}
	return 0
}

// WeiToGweiCeil converts *big.Int Wei to uint64 Gwei. If v is determined to be
// unsuitable for a uint64, zero is returned. For values that are not even
// multiples of 1 gwei, this function returns the ceiling. In general,
// WeiToWeiCeil should be used with gwei-unit fee rates are generated or
// validated and WeiToGwei should be used with balances and values.
func WeiToGweiCeil(v *big.Int) uint64 {
	vGwei := new(big.Int).Div(new(big.Int).Add(v, gweiCeilAddend), big.NewInt(GweiFactor))
	if vGwei.IsUint64() {
		return vGwei.Uint64()
	}
	return 0
}

// WeiToGweiSafe converts a *big.Int in wei (1e18 unit) to gwei (1e9 unit) as
// a uint64. Errors if the amount of gwei is too big to fit fully into a uint64.
// For values that are not even multiples of 1 gwei, this function returns the
// ceiling. As such, WeiToGweiSafe is more suitable for validating or generating
// fee rates. If balance or value validation is the goal, use truncation e.g.
// WeiToGwei.
func WeiToGweiSafe(wei *big.Int) (uint64, error) {
	if wei.Cmp(new(big.Int)) == -1 {
		return 0, fmt.Errorf("wei must be non-negative")
	}
	gwei := new(big.Int).Div(new(big.Int).Add(wei, gweiCeilAddend), gweiFactorBig)
	if !gwei.IsUint64() {
		return 0, fmt.Errorf("%v gwei is too big for a uint64", gwei)
	}
	return gwei.Uint64(), nil
}

var (
	usdcTokenID, _    = util.BipSymbolID("usdc.eth")
	usdtTokenID, _    = util.BipSymbolID("usdt.eth")
	polygonTokenID, _ = util.BipSymbolID("polygon.eth")
	maticTokenID, _   = util.BipSymbolID("matic.eth") // old matic 0x7d1afa7b718fb893db30a3abc0cfc608aacfebb0
	linkTokenID, _    = util.BipSymbolID("link.eth")
	paxgTokenID, _    = util.BipSymbolID("paxg.eth")
	xautTokenID, _    = util.BipSymbolID("xaut.eth")
)

// Gases lists typical gas usage for token wallet operations. These values are
// for fee estimation only (pre-send fee display, balance sufficiency checks).
// Transactions that are actually broadcast derive their gas limit from a live
// eth_estimateGas call via bufferedGasLimit, never from this table.
type Gases struct {
	// Approve is the amount of gas typically needed to approve a contract for
	// transferring tokens. The first approval for an address uses more gas than
	// subsequent approvals for the same address.
	Approve uint64 `json:"approve"`
	// Transfer is the amount of gas typically needed to transfer tokens. The
	// first transfer to an address uses more gas than subsequent transfers to
	// the same address.
	Transfer uint64 `json:"transfer"`
}
