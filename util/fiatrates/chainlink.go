package fiatrates

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const multiAggregatorABI = `[
  {
    "inputs": [{"internalType": "address[]", "name": "aggs", "type": "address[]"}],
    "name": "read",
    "outputs": [{
      "components": [
        {"internalType": "int256",  "name": "answer",    "type": "int256"},
        {"internalType": "uint256", "name": "updatedAt", "type": "uint256"},
        {"internalType": "uint8",   "name": "decimals",  "type": "uint8"},
        {"internalType": "bool",    "name": "ok",        "type": "bool"}
      ],
      "internalType": "struct MultiAggregatorV0.Reading[]",
      "name": "out",
      "type": "tuple[]"
    }],
    "stateMutability": "view",
    "type": "function"
  }
]`

const (
	chainlinkDialTimeout = 5 * time.Second
	// chainlinkMaxAge is the maximum age of a Chainlink price before it is
	// rejected. Polygon BTC/USD updates every ~27s with a 1h heartbeat, so
	// 10 minutes is a tight but reasonable bound.
	chainlinkMaxAge = 10 * time.Minute
	// chainlinkFutureSkew tolerates small forward drift between the Chainlink
	// block timestamp and the local wall clock before treating a price as
	// invalid. Validator clock skew on Polygon is small but not zero.
	chainlinkFutureSkew = 30 * time.Second
)

// MultiAggregatorReading mirrors MultiAggregatorV0.Reading from the on-chain
// contract. Fields must be exported so abi.ConvertType can map ABI tuple field
// names to struct fields by reflection (e.g. "updatedAt" → "UpdatedAt").
type MultiAggregatorReading struct {
	Answer    *big.Int
	UpdatedAt *big.Int
	Decimals  uint8
	Ok        bool
}

type chainlinkBatchContract interface {
	read(ctx context.Context, addrs []common.Address) ([]MultiAggregatorReading, error)
}

// chainRPCClient is the subset of *ethclient.Client used by the rate fetcher.
// Close() matches *ethclient.Client which returns no error.
type chainRPCClient interface {
	bind.ContractCaller
	Close()
}

type batchBoundContract struct {
	bound *bind.BoundContract
}

func (c *batchBoundContract) read(ctx context.Context, addrs []common.Address) ([]MultiAggregatorReading, error) {
	var out []interface{}
	if err := c.bound.Call(&bind.CallOpts{Context: ctx}, &out, "read", addrs); err != nil {
		return nil, err
	}
	if len(out) < 1 {
		return nil, fmt.Errorf("invalid read response: expected 1 output, got %d", len(out))
	}
	converted := abi.ConvertType(out[0], new([]MultiAggregatorReading))
	readingsPtr, ok := converted.(*[]MultiAggregatorReading)
	if !ok {
		return nil, fmt.Errorf("failed to convert read output to []MultiAggregatorReading")
	}
	return *readingsPtr, nil
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
	//         2. Test with live RPC calls
}

// chainlinkTestnetFeeds are Polygon Amoy feed addresses.
// TODO: replace placeholder addresses with real Polygon Amoy Chainlink feed addresses
// from https://docs.chain.link/data-feeds/price-feeds/addresses?network=polygon&page=1&testnetPage=1
var chainlinkTestnetFeeds = map[string]common.Address{
	"ETH": common.HexToAddress("0x694AA1769357215DE4FAC081bf1f309aDC325306"),
	"BTC": common.HexToAddress("0x1b44F3514812d835EB1BDB0acB33d3fA3351Ee43"),
}

// chainlinkPolygonBatchAddr is the deployed MultiAggregatorV0 address on Polygon mainnet.
// TODO: fill in after deployment.
var chainlinkPolygonBatchAddr = common.Address{}

// chainlinkAmoyBatchAddr is the deployed MultiAggregatorV0 address on Polygon Amoy testnet.
// TODO: fill in after deployment.
var chainlinkAmoyBatchAddr = common.Address{}

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

func chainlinkRateFetcher(cfg Config) (func(context.Context, []string, util.Logger) (map[string]float64, error), error) {
	batchABI, err := parseABI(multiAggregatorABI)
	if err != nil {
		return nil, fmt.Errorf("chainlink: %w", err)
	}

	feeds := chainlinkPolygonFeeds
	batchAddr := chainlinkPolygonBatchAddr
	if cfg.ChainlinkTestnet {
		feeds = chainlinkTestnetFeeds
		batchAddr = chainlinkAmoyBatchAddr
	}

	// Get provider list: use configured providers or fall back to defaults.
	providers := chainlinkDefaultProviders
	if cfg.ChainlinkProviders != "" {
		// Split on any whitespace (spaces, tabs, newlines).
		providers = strings.Fields(cfg.ChainlinkProviders)
		if len(providers) == 0 {
			return nil, fmt.Errorf("chainlink: ChainlinkProviders is set but contains no URLs; unset it to use defaults")
		}
		for _, provider := range providers {
			if err := validateRPCURL(provider); err != nil {
				return nil, fmt.Errorf("chainlink: invalid provider %q: %w", provider, err)
			}
		}
	}

	dialFn := func() (chainRPCClient, error) {
		var lastErr error
		for _, provider := range providers {
			dialCtx, cancel := context.WithTimeout(context.Background(), chainlinkDialTimeout)
			newEC, err := ethclient.DialContext(dialCtx, provider)
			cancel()
			if err == nil {
				return newEC, nil
			}
			lastErr = err
		}
		return nil, fmt.Errorf("dial all providers failed: %w", lastErr)
	}

	return chainlinkFetcherCore(feeds, dialFn, func(client chainRPCClient) chainlinkBatchContract {
		return &batchBoundContract{bound: bind.NewBoundContract(batchAddr, *batchABI, client, nil, nil)}
	}), nil
}

// chainlinkFetcherCore is the testable core of chainlinkRateFetcher with
// injectable dial and batch contract factory functions.
func chainlinkFetcherCore(
	feeds map[string]common.Address,
	dialFn func() (chainRPCClient, error),
	newBatchContract func(client chainRPCClient) chainlinkBatchContract,
) func(context.Context, []string, util.Logger) (map[string]float64, error) {
	// Chainlink is one of several fiat rate sources; the oracle averages across
	// all active sources, so a single compromised RPC cannot unilaterally skew
	// final prices. Operators should restrict ChainlinkProviders to trusted
	// endpoints in production.
	//
	// The provider connection is established lazily on the first getRates call.
	// If the batch read call fails, the connection is reset and re-dialed from
	// the beginning of the provider list on the next call. Failover is between
	// cycles, not within: a provider that dies mid-cycle costs that cycle's
	// rates entirely, since the pinned client is not swapped until the next
	// getRates invocation observes a failure.
	//
	// divisors caches the 10^decimals factor per feed and intentionally
	// survives reconnects: Chainlink Data Feed decimals are immutable per
	// aggregator address (decimal changes are deployed as a new feed at a new
	// address), so a cached divisor cannot go stale relative to its address.
	var (
		mtx      sync.Mutex
		ec       chainRPCClient
		divisors = make(map[string]*big.Float)
	)

	// isConnErr returns true for errors that indicate a provider failure
	// rather than a cancelled or timed-out context.
	isConnErr := func(err error) bool {
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}

	return func(ctx context.Context, tickers []string, log util.Logger) (map[string]float64, error) {
		// Build an ordered list of (ticker, addr) pairs for feeds that match
		// the request. The order is preserved so readings[i] maps to entries[i].
		type feedEntry struct {
			ticker string
			addr   common.Address
		}
		var entries []feedEntry
		for _, t := range tickers {
			if addr, ok := feeds[t]; ok {
				entries = append(entries, feedEntry{t, addr})
			}
		}
		if len(entries) == 0 {
			return map[string]float64{}, nil
		}

		addrs := make([]common.Address, len(entries))
		for i, e := range entries {
			addrs[i] = e.addr
		}

		mtx.Lock()
		needConnect := ec == nil
		mtx.Unlock()

		if needConnect {
			newEC, err := dialFn()
			if err != nil {
				return nil, fmt.Errorf("chainlink: %w", err)
			}
			mtx.Lock()
			if ec == nil {
				ec = newEC
			} else {
				newEC.Close() // another caller connected first
			}
			mtx.Unlock()
		}

		mtx.Lock()
		currentEC := ec
		mtx.Unlock()

		readings, err := newBatchContract(currentEC).read(ctx, addrs)
		if err != nil {
			log.Warnf("chainlink: batch read failed: %v", err)
			if isConnErr(err) {
				var doClose bool
				mtx.Lock()
				if ec == currentEC {
					ec = nil
					doClose = true
				}
				mtx.Unlock()
				if doClose {
					currentEC.Close()
				}
			}
			return map[string]float64{}, nil
		}

		if len(readings) != len(entries) {
			return nil, fmt.Errorf("chainlink: batch read returned %d readings for %d feeds", len(readings), len(entries))
		}

		rates := make(map[string]float64, len(entries))
		for i, reading := range readings {
			ticker := entries[i].ticker

			if !reading.Ok {
				log.Warnf("chainlink: %s/USD: feed returned ok=false", ticker)
				continue
			}
			if reading.Answer.Sign() <= 0 {
				log.Warnf("chainlink: %s/USD: non-positive answer", ticker)
				continue
			}

			updatedAt := time.Unix(reading.UpdatedAt.Int64(), 0)
			if time.Until(updatedAt) > chainlinkFutureSkew {
				log.Warnf("chainlink: %s/USD: future timestamp %v", ticker, updatedAt)
				continue
			}
			if time.Since(updatedAt) > chainlinkMaxAge {
				log.Warnf("chainlink: %s/USD: stale price (updated %v ago)", ticker, time.Since(updatedAt).Round(time.Minute))
				continue
			}

			mtx.Lock()
			div := divisors[ticker]
			mtx.Unlock()
			if div == nil {
				div = new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(reading.Decimals)), nil))
				mtx.Lock()
				if divisors[ticker] == nil {
					divisors[ticker] = div
				} else {
					div = divisors[ticker]
				}
				mtx.Unlock()
			}

			f, _ := new(big.Float).Quo(new(big.Float).SetInt(reading.Answer), div).Float64()
			rates[ticker] = f
		}

		return rates, nil
	}
}
