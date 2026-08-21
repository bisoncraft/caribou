//go:build !harness && !rpclive

package eth

import (
	"github.com/bisoncraft/caribou/util"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

var (
	tLogger = util.StdOutLogger("ETHTEST", util.LevelTrace)

	testAddressA = common.HexToAddress("dd93b447f7eBCA361805eBe056259853F3912E04")

	tUSDCGasEstimate = Tokens[usdcEthID].NetTokens[util.Simnet].GasEstimate

	// tTransferEstimate is the live transfer gas estimate returned by the test
	// token contractor.
	tTransferEstimate uint64 = 50_000

	signer = types.LatestSigner(params.AllEthashProtocolChanges)
)
