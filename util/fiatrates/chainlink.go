package fiatrates

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const aggregatorV3ABI = `[{"inputs":[],"name":"latestRoundData","outputs":[{"internalType":"uint80","name":"roundId","type":"uint80"},{"internalType":"int256","name":"answer","type":"int256"},{"internalType":"uint256","name":"startedAt","type":"uint256"},{"internalType":"uint256","name":"updatedAt","type":"uint256"},{"internalType":"uint80","name":"answeredInRound","type":"uint80"}],"stateMutability":"view","type":"function"}]`

var aggregatorABI = parseABIOrPanic(aggregatorV3ABI)

type chainlinkAggregatorContract interface {
	latestRoundData(ctx context.Context) (*big.Int, error)
}

type aggregatorBoundContract struct {
	bound *bind.BoundContract
}

func (c *aggregatorBoundContract) latestRoundData(ctx context.Context) (*big.Int, error) {
	var out []interface{}
	if err := c.bound.Call(&bind.CallOpts{Context: ctx}, &out, "latestRoundData"); err != nil {
		return nil, err
	}

	answer := *abi.ConvertType(out[1], new(*big.Int)).(**big.Int)
	if answer.Sign() <= 0 {
		return nil, fmt.Errorf("non-positive answer: %s", answer)
	}

	return answer, nil
}

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

// chainlinkDefaultProviders are free public Polygon RPC endpoints used as fallback
// if no providers are explicitly configured. Verified working as of 2025-05-16.
var chainlinkDefaultProviders = []string{
	"https://polygon.drpc.org",             // dRPC - fast, reliable
	"https://polygon.publicnode.com",       // Allnodes - verified working
	"https://polygon.api.onfinality.io/public", // OnFinality - verified working
	"https://polygon-public.nodies.app/",   // Nodies - verified working
	"https://1rpc.io/matic",                // 1RPC - verified working
}

func parseABIOrPanic(abiStr string) *abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(abiStr))
	if err != nil {
		panic(fmt.Sprintf("failed to parse aggregator abi: %v", err))
	}
	return &parsed
}

func chainlinkRateFetcher(cfg Config) (func(context.Context, []string, util.Logger) (map[string]float64, error), error) {
	feeds := chainlinkPolygonFeeds
	if cfg.ChainlinkTestnet {
		feeds = chainlinkTestnetFeeds
	}

	// Get provider list: use configured providers or fall back to defaults
	providers := chainlinkDefaultProviders
	if cfg.ChainlinkProviders != "" {
		providers = strings.Fields(cfg.ChainlinkProviders)
	}

	var ec *ethclient.Client
	var lastErr error
	for _, provider := range providers {
		var err error
		ec, err = ethclient.DialContext(context.Background(), provider)
		if err == nil {
			break
		}
		lastErr = err
	}

	if ec == nil {
		return nil, fmt.Errorf("chainlink: dial all providers failed: %w", lastErr)
	}

	contracts := make(map[string]chainlinkAggregatorContract, len(feeds))
	for ticker, addr := range feeds {
		contracts[ticker] = &aggregatorBoundContract{
			bound: bind.NewBoundContract(addr, *aggregatorABI, ec, nil, nil),
		}
	}

	return func(ctx context.Context, tickers []string, log util.Logger) (map[string]float64, error) {
		rates := make(map[string]float64)
		for _, ticker := range tickers {
			contract, ok := contracts[ticker]
			if !ok {
				continue
			}
			answer, err := contract.latestRoundData(ctx)
			if err != nil {
				log.Warnf("chainlink: %s/USD: %v", ticker, err)
				continue
			}
			f, _ := new(big.Float).Quo(
				new(big.Float).SetInt(answer),
				new(big.Float).SetFloat64(1e8),
			).Float64()
			rates[ticker] = f
		}
		return rates, nil
	}, nil
}
