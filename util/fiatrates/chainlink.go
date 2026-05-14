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

// chainlinkMainnetFeeds maps ticker symbols to their USD Data Feed addresses on Ethereum mainnet.
// Verify addresses against https://docs.chain.link/data-feeds/price-feeds/addresses?network=ethereum
// TODO: Add addresses for ADA, SOL, AVAX, XRP, BNB from official Chainlink documentation
var chainlinkMainnetFeeds = map[string]common.Address{
	"BTC":   common.HexToAddress("0xF4030086522a5bEEa4988F8cA5B36dbC97BeE88c"),
	"ETH":   common.HexToAddress("0x5f4eC3Df9cbd43714FE2740f5E3616155c5b8419"),
	"LTC":   common.HexToAddress("0x6AF09DF7563C363B5763b9102712EbeD3b9e859B"),
	"BCH":   common.HexToAddress("0x9F0F69428F923D6c95B781F89E165C9b2df9789D"),
	"DOGE":  common.HexToAddress("0x2465CefD3b488BE410b941b1d4b2767088e2A028"),
	"ZEC":   common.HexToAddress("0xd54B033D48d0475f19c5fccf7484E8A981848501"),
	"LINK":  common.HexToAddress("0x2c1d072e956AFFC0D435Cb7AC38EF18d24d9127c"),
	"MATIC": common.HexToAddress("0x7bAC85A8a13A4BcD8abb3eB7d6b4d632c895a6B1"),
	// ADA: "0x???" - need to research
	// SOL: "0x???" - need to research
	// AVAX: "0x???" - need to research
	// XRP: "0x???" - need to research
	// BNB: "0x???" - need to research
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
	feeds := chainlinkMainnetFeeds
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
