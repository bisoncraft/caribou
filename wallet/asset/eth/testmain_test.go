//go:build !harness && !rpclive

package eth

import (
	"github.com/bisoncraft/bosswallet/util"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

var (
	tLogger = util.StdOutLogger("ETHTEST", util.LevelTrace)

	testAddressA = common.HexToAddress("dd93b447f7eBCA361805eBe056259853F3912E04")

	tokenGasesV1 = Tokens[usdcEthID].NetTokens[util.Simnet].Gas[1]

	signer = types.LatestSigner(params.AllEthashProtocolChanges)
)
