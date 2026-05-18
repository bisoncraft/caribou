package fiatrates

import (
	"context"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"golang.org/x/sync/errgroup"
)

const aggregatorV3ABI = `[
  {
    "inputs": [],
    "name": "latestRoundData",
    "outputs": [
      {"internalType": "uint80",  "name": "roundId",         "type": "uint80"},
      {"internalType": "int256",  "name": "answer",          "type": "int256"},
      {"internalType": "uint256", "name": "startedAt",       "type": "uint256"},
      {"internalType": "uint256", "name": "updatedAt",       "type": "uint256"},
      {"internalType": "uint80",  "name": "answeredInRound", "type": "uint80"}
    ],
    "stateMutability": "view",
    "type": "function"
  },
  {
    "inputs": [],
    "name": "decimals",
    "outputs": [{"internalType": "uint8", "name": "", "type": "uint8"}],
    "stateMutability": "view",
    "type": "function"
  }
]`

const (
	chainlinkDialTimeout = 5 * time.Second
	chainlinkInitTimeout = 5 * time.Second
	// chainlinkMaxAge is the maximum age of a Chainlink price before it is
	// rejected. Polygon BTC/USD updates every ~27s with a 1h heartbeat, so
	// 10 minutes is a tight but reasonable bound.
	chainlinkMaxAge = 10 * time.Minute
)

type chainlinkAggregatorContract interface {
	latestRoundData(ctx context.Context) (*big.Int, time.Time, error)
	decimals(ctx context.Context) (uint8, error)
}

type aggregatorBoundContract struct {
	bound *bind.BoundContract
}

func (c *aggregatorBoundContract) latestRoundData(ctx context.Context) (*big.Int, time.Time, error) {
	var out []interface{}
	if err := c.bound.Call(&bind.CallOpts{Context: ctx}, &out, "latestRoundData"); err != nil {
		return nil, time.Time{}, err
	}

	if len(out) < 4 {
		return nil, time.Time{}, fmt.Errorf("invalid latestRoundData response: expected at least 4 outputs, got %d", len(out))
	}

	converted := abi.ConvertType(out[1], new(*big.Int))
	answerPtr, ok := converted.(**big.Int)
	if !ok {
		return nil, time.Time{}, fmt.Errorf("failed to convert latestRoundData answer to *big.Int")
	}

	answer := *answerPtr
	if answer.Sign() <= 0 {
		return nil, time.Time{}, fmt.Errorf("non-positive answer: %s", answer)
	}

	convertedUpdated := abi.ConvertType(out[3], new(*big.Int))
	updatedAtPtr, ok := convertedUpdated.(**big.Int)
	if !ok {
		return nil, time.Time{}, fmt.Errorf("failed to convert latestRoundData updatedAt to *big.Int")
	}

	updatedAt := time.Unix((*updatedAtPtr).Int64(), 0)

	return answer, updatedAt, nil
}

func (c *aggregatorBoundContract) decimals(ctx context.Context) (uint8, error) {
	var out []interface{}
	if err := c.bound.Call(&bind.CallOpts{Context: ctx}, &out, "decimals"); err != nil {
		return 0, err
	}

	if len(out) < 1 {
		return 0, fmt.Errorf("invalid decimals response: expected at least 1 output, got %d", len(out))
	}

	decimalsPtr, ok := abi.ConvertType(out[0], new(uint8)).(*uint8)
	if !ok {
		return 0, fmt.Errorf("failed to convert decimals to uint8")
	}

	return *decimalsPtr, nil
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
	// Missing feeds: ADA, SOL, AVAX, XRP, BNB
	// To add: 1. Get addresses from https://docs.chain.link/data-feeds/price-feeds/addresses?network=polygon
	//         2. Verify contract has decimals() method
	//         3. Test with live RPC calls
}

var chainlinkTestnetFeeds = map[string]common.Address{
	"ETH": common.HexToAddress("0x694AA1769357215DE4FAC081bf1f309aDC325306"),
	"BTC": common.HexToAddress("0x1b44F3514812d835EB1BDB0acB33d3fA3351Ee43"),
}

// chainlinkDefaultProviders are free public Polygon RPC endpoints used as
// fallback if no providers are explicitly configured.
var chainlinkDefaultProviders = []string{
	"https://polygon.drpc.org",
	"https://polygon.publicnode.com",
	"https://polygon.api.onfinality.io/public",
	"https://polygon-public.nodies.app/",
	"https://1rpc.io/matic",
}

func parseABI(abiStr string) (*abi.ABI, error) {
	parsed, err := abi.JSON(strings.NewReader(abiStr))
	if err != nil {
		return nil, fmt.Errorf("failed to parse aggregator abi: %w", err)
	}
	return &parsed, nil
}

func validateRPCURL(urlStr string) error {
	u, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("invalid RPC URL: %w", err)
	}

	if u.Scheme != "https" {
		return fmt.Errorf("invalid RPC URL scheme: %s (must be https)", u.Scheme)
	}

	if u.Host == "" {
		return fmt.Errorf("invalid RPC URL: missing host")
	}

	return nil
}

type contractInfo struct {
	contract chainlinkAggregatorContract
	divisor  *big.Float
}

// chainlinkFetcherFromContracts creates a rate fetcher from pre-built contracts.
// Used in tests to exercise the closure logic without network I/O.
func chainlinkFetcherFromContracts(contracts map[string]*contractInfo) func(context.Context, []string, util.Logger) (map[string]float64, error) {
	return func(ctx context.Context, tickers []string, log util.Logger) (map[string]float64, error) {
		rates := make(map[string]float64)
		for _, ticker := range tickers {
			info, ok := contracts[ticker]
			if !ok {
				continue
			}
			answer, updatedAt, err := info.contract.latestRoundData(ctx)
			if err != nil {
				log.Warnf("chainlink: %s/USD: %v", ticker, err)
				continue
			}
			if updatedAt.After(time.Now()) {
				log.Warnf("chainlink: %s/USD: future timestamp %v", ticker, updatedAt)
				continue
			}
			if time.Since(updatedAt) > chainlinkMaxAge {
				log.Warnf("chainlink: %s/USD: stale price (updated %v ago)", ticker, time.Since(updatedAt).Round(time.Minute))
				continue
			}
			f, _ := new(big.Float).Quo(new(big.Float).SetInt(answer), info.divisor).Float64()
			rates[ticker] = f
		}
		return rates, nil
	}
}

func chainlinkRateFetcher(cfg Config) (func(context.Context, []string, util.Logger) (map[string]float64, error), error) {
	aggregatorABI, err := parseABI(aggregatorV3ABI)
	if err != nil {
		return nil, fmt.Errorf("chainlink: %w", err)
	}

	feeds := chainlinkPolygonFeeds
	if cfg.ChainlinkTestnet {
		feeds = chainlinkTestnetFeeds
	}

	// Get provider list: use configured providers or fall back to defaults.
	providers := chainlinkDefaultProviders
	if cfg.ChainlinkProviders != "" {
		// Split on any whitespace (spaces, tabs, newlines).
		providers = strings.Fields(cfg.ChainlinkProviders)
		for _, provider := range providers {
			if err := validateRPCURL(provider); err != nil {
				return nil, fmt.Errorf("chainlink: %w", err)
			}
		}
	}

	// Chainlink is one of several fiat rate sources; the oracle averages across
	// all active sources, so a single compromised RPC cannot unilaterally skew
	// final prices. Operators should restrict ChainlinkProviders to trusted
	// endpoints in production.
	//
	// The provider connection is established lazily on the first getRates call.
	// If any RPC call fails, the connection is reset and re-dialed from the
	// beginning of the provider list on the next call.
	var (
		mu       sync.Mutex
		ec       *ethclient.Client
		divisors = make(map[string]*big.Float) // keyed by ticker; survives reconnects
	)

	// connect dials providers in order until one succeeds. Called with mu held.
	connect := func() error {
		var lastErr error
		for _, provider := range providers {
			dialCtx, cancel := context.WithTimeout(context.Background(), chainlinkDialTimeout)
			newEC, err := ethclient.DialContext(dialCtx, provider)
			cancel()
			if err == nil {
				ec = newEC
				return nil
			}
			lastErr = err
		}
		return fmt.Errorf("dial all providers failed: %w", lastErr)
	}

	return func(ctx context.Context, tickers []string, log util.Logger) (map[string]float64, error) {
		mu.Lock()
		if ec == nil {
			if err := connect(); err != nil {
				mu.Unlock()
				return nil, fmt.Errorf("chainlink: %w", err)
			}
		}
		currentEC := ec
		mu.Unlock()

		var (
			ratesMu    sync.Mutex
			rates      = make(map[string]float64)
			connFailed atomic.Bool
		)

		g, gctx := errgroup.WithContext(ctx)
		for _, ticker := range tickers {
			addr, ok := feeds[ticker]
			if !ok {
				continue
			}
			g.Go(func() error {
				contract := &aggregatorBoundContract{
					bound: bind.NewBoundContract(addr, *aggregatorABI, currentEC, nil, nil),
				}

				mu.Lock()
				div := divisors[ticker]
				mu.Unlock()

				if div == nil {
					initCtx, cancel := context.WithTimeout(gctx, chainlinkInitTimeout)
					d, err := contract.decimals(initCtx)
					cancel()
					if err != nil {
						log.Warnf("chainlink: failed to query decimals for %s: %v", ticker, err)
						connFailed.Store(true)
						return nil
					}
					div = new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d)), nil))
					mu.Lock()
					if divisors[ticker] == nil {
						divisors[ticker] = div
					}
					mu.Unlock()
				}

				answer, updatedAt, err := contract.latestRoundData(gctx)
				if err != nil {
					log.Warnf("chainlink: %s/USD: %v", ticker, err)
					connFailed.Store(true)
					return nil
				}

				if updatedAt.After(time.Now()) {
					log.Warnf("chainlink: %s/USD: future timestamp %v", ticker, updatedAt)
					return nil
				}

				if time.Since(updatedAt) > chainlinkMaxAge {
					log.Warnf("chainlink: %s/USD: stale price (updated %v ago)", ticker, time.Since(updatedAt).Round(time.Minute))
					return nil
				}

				f, _ := new(big.Float).Quo(new(big.Float).SetInt(answer), div).Float64()
				ratesMu.Lock()
				rates[ticker] = f
				ratesMu.Unlock()
				return nil
			})
		}
		_ = g.Wait() // goroutines never return non-nil errors

		if connFailed.Load() {
			mu.Lock()
			if ec == currentEC {
				ec = nil
			}
			mu.Unlock()
		}

		return rates, nil
	}, nil
}
