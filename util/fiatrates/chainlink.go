package fiatrates

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const aggregatorV3ABI = `[{"inputs":[],"name":"latestRoundData","outputs":[{"internalType":"uint80","name":"roundId","type":"uint80"},{"internalType":"int256","name":"answer","type":"int256"},{"internalType":"uint256","name":"startedAt","type":"uint256"},{"internalType":"uint256","name":"updatedAt","type":"uint256"},{"internalType":"uint80","name":"answeredInRound","type":"uint80"}],"stateMutability":"view","type":"function"}]`

var aggregatorABI = parseABIOrPanic(aggregatorV3ABI)

// chainlinkPolygonFeeds maps ticker symbols to their USD Data Feed addresses on Polygon mainnet.
// Verify addresses against https://docs.chain.link/data-feeds/price-feeds/addresses?network=polygon
// Polygon offers fresher data due to lower fees and more frequent updates.
var chainlinkPolygonFeeds = map[string]common.Address{
	"BTC":  common.HexToAddress("0xc907E116054Ad103354f2D350FD2514433D57F6f"),
	"ETH":  common.HexToAddress("0xF9680D99D6C9589e2a93a78A04A279e509205945"),
	"LTC":  common.HexToAddress("0xEB99F173cf7d9a6dC4D889C2Ad7103e8383b6Efa"),
	"BCH":  common.HexToAddress("0x327d9822e9932996f55b39F557AEC838313da8b7"),
	"DOGE": common.HexToAddress("0xbaf9327b6564454F4a3364C33eFeEf032b4b4444"),
	"ZEC":  common.HexToAddress("0xBC08c639e579a391C4228F20d0C29d0690092DF0"),
	"LINK": common.HexToAddress("0xd9FFdb71EbE7496cC440152d43986Aae0AB76665"),
	// TODO: Add addresses for ADA, SOL, AVAX, XRP, BNB from official Chainlink documentation
}

var chainlinkTestnetFeeds = map[string]common.Address{
	"ETH": common.HexToAddress("0x694AA1769357215DE4FAC081bf1f309aDC325306"),
	"BTC": common.HexToAddress("0x1b44F3514812d835EB1BDB0acB33d3fA3351Ee43"),
}

func parseABIOrPanic(abiStr string) *abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(abiStr))
	if err != nil {
		panic(fmt.Sprintf("failed to parse aggregator abi: %v", err))
	}
	return &parsed
}

func chainlinkRateFetcher(cfg Config) func(context.Context, []string, util.Logger) (map[string]float64, error) {
	feeds := chainlinkPolygonFeeds
	if cfg.ChainlinkTestnet {
		feeds = chainlinkTestnetFeeds
	}

	var (
		once  sync.Once
		ec    *ethclient.Client
		ecErr error
	)

	return func(ctx context.Context, tickers []string, log util.Logger) (map[string]float64, error) {
		once.Do(func() {
			ec, ecErr = ethclient.DialContext(context.Background(), cfg.ChainlinkRPCURL)
		})
		if ecErr != nil {
			return nil, fmt.Errorf("chainlink: dial: %w", ecErr)
		}

		rates := make(map[string]float64)
		for _, ticker := range tickers {
			addr, ok := feeds[ticker]
			if !ok {
				continue
			}
			price, err := latestUSDPrice(ctx, ec, addr)
			if err != nil {
				log.Warnf("chainlink: %s/USD: %v", ticker, err)
				continue
			}
			rates[ticker] = price
		}
		return rates, nil
	}
}

func latestUSDPrice(ctx context.Context, client *ethclient.Client, addr common.Address) (float64, error) {
	contract := bind.NewBoundContract(addr, *aggregatorABI, client, nil, nil)
	var out []interface{}
	if err := contract.Call(&bind.CallOpts{Context: ctx}, &out, "latestRoundData"); err != nil {
		return 0, err
	}

	answer := *abi.ConvertType(out[1], new(*big.Int)).(**big.Int)
	if answer.Sign() <= 0 {
		return 0, fmt.Errorf("non-positive answer: %s", answer)
	}

	f, _ := new(big.Float).Quo(
		new(big.Float).SetInt(answer),
		new(big.Float).SetFloat64(1e8),
	).Float64()
	return f, nil
}
