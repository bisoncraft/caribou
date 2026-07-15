package fiatrates

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
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
	chainlinkInitTimeout = 5 * time.Second
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
// contract. Fields must be exported and listed in ABI tuple field order, since
// abi.ConvertType maps by position rather than by name.
type MultiAggregatorReading struct {
	Answer    *big.Int
	UpdatedAt *big.Int
	Decimals  uint8
	Ok        bool
}

type chainlinkAggregatorContract interface {
	latestRoundData(ctx context.Context) (*big.Int, time.Time, error)
	decimals(ctx context.Context) (uint8, error)
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

type divisorKey struct {
	ticker   string
	decimals uint8
}

type divisorCache struct {
	mtx  sync.Mutex
	vals map[divisorKey]*big.Float
}

func newDivisorCache() *divisorCache {
	return &divisorCache{vals: make(map[divisorKey]*big.Float)}
}

func (c *divisorCache) getOrCreate(ticker string, decimals uint8) *big.Float {
	key := divisorKey{ticker: ticker, decimals: decimals}

	c.mtx.Lock()
	defer c.mtx.Unlock()

	if div := c.vals[key]; div != nil {
		return div
	}

	div := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
	c.vals[key] = div
	return div
}

// connRef wraps a chainRPCClient with a reference count so concurrent fetch
// cycles can share a connection without one cycle closing it while another is
// mid-call. Callers must acquire before using client and call release when
// done. The caller that evicts the connection from the shared pointer calls
// release then closeWhenDone: closeWhenDone blocks until all outstanding
// acquire/release pairs complete, then closes client exactly once.
type connRef struct {
	client    chainRPCClient
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func (r *connRef) acquire() { r.wg.Add(1) }
func (r *connRef) release() { r.wg.Done() }
func (r *connRef) closeWhenDone() {
	r.closeOnce.Do(func() {
		r.wg.Wait()
		r.client.Close()
	})
}

type sharedConn struct {
	mtx  sync.Mutex
	conn *connRef
}

func (s *sharedConn) acquire(dialFn func() (chainRPCClient, error)) (*connRef, error) {
	s.mtx.Lock()
	if s.conn != nil {
		current := s.conn
		current.acquire()
		s.mtx.Unlock()
		return current, nil
	}
	s.mtx.Unlock()

	newEC, err := dialFn()
	if err != nil {
		return nil, err
	}

	s.mtx.Lock()
	if s.conn == nil {
		s.conn = &connRef{client: newEC}
	} else {
		newEC.Close()
	}
	current := s.conn
	current.acquire()
	s.mtx.Unlock()
	return current, nil
}

func (s *sharedConn) reset(current *connRef) {
	s.mtx.Lock()
	if s.conn == current {
		s.conn = nil
	}
	s.mtx.Unlock()
	current.release()
	current.closeWhenDone()
}

func isConnErr(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// EIP-3 execution reverts carry error code 3 in the JSON-RPC layer;
	// use the typed interface where available to avoid string matching.
	type rpcError interface{ ErrorCode() int }
	if rpcErr, ok := err.(rpcError); ok && rpcErr.ErrorCode() == 3 {
		return false
	}
	msg := err.Error()
	return !strings.Contains(msg, "execution reverted") && !strings.Contains(msg, "VM Exception")
}

type batchBoundContract struct {
	bound *bind.BoundContract
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

	updatedAt := *updatedAtPtr
	if !updatedAt.IsInt64() {
		return nil, time.Time{}, fmt.Errorf("latestRoundData updatedAt overflows int64: %s", updatedAt)
	}

	return answer, time.Unix(updatedAt.Int64(), 0), nil
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
	// ZEC/USD on Polygon updates less often than chainlinkMaxAge, so it is
	// intermittently rejected as stale and omitted from a cycle's results.
	// This is expected best-effort behavior, not an error: the feed is healthy,
	// the staleness guard is doing its job, and other fiat-rate sources cover
	// ZEC. Tighten or relax only by revisiting chainlinkMaxAge for all feeds.
	"ZEC":  common.HexToAddress("0xBC08c639e579a391C4228F20d0C29d0690092DF0"),
	"LINK": common.HexToAddress("0xd9FFdb71EbE7496cC440152d43986Aae0AB76665"),
	// Missing feeds: ADA, SOL, AVAX, XRP, BNB
	// To add: 1. Get addresses from https://docs.chain.link/data-feeds/price-feeds/addresses?network=polygon
	//         2. Test with live RPC calls
}

// chainlinkPolygonBatchAddr is the deployed MultiAggregatorV0 address on Polygon mainnet.
// Verified source: https://polygonscan.com/address/0x71eCdcF7F77765F0f596c03C63c80541733fd060#code
var chainlinkPolygonBatchAddr = common.HexToAddress("0x71eCdcF7F77765F0f596c03C63c80541733fd060")

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

// ChainlinkRateFetcher builds a fetcher that reads USD prices from Chainlink
// Data Feeds, keyed on the tickers produced by ParseTicker. Tickers without a
// configured feed are ignored rather than erroring, so callers may pass their
// full ticker set. A zero Config selects the built-in Polygon mainnet feeds,
// batch contract, and public RPC providers.
//
// The returned fetcher owns a ref-counted RPC connection and a feed decimals
// cache, so it is built once and reused for the life of the process rather than
// per request.
func ChainlinkRateFetcher(cfg Config) (func(context.Context, []string, util.Logger) (map[string]float64, error), error) {
	feeds := chainlinkPolygonFeeds
	batchAddr := chainlinkPolygonBatchAddr
	if cfg.ChainlinkBatchAddr != "" {
		if !common.IsHexAddress(cfg.ChainlinkBatchAddr) {
			return nil, fmt.Errorf("chainlink: invalid ChainlinkBatchAddr %q: not a valid hex address", cfg.ChainlinkBatchAddr)
		}
		batchAddr = common.HexToAddress(cfg.ChainlinkBatchAddr)
		if batchAddr == (common.Address{}) {
			return nil, fmt.Errorf("chainlink: ChainlinkBatchAddr resolved to zero address")
		}
	}

	// Get provider list: use configured providers or fall back to network defaults.
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

	// Dial through dexnet's shared client rather than ethclient's default
	// transport. dexnet.SetProxy rewrites that client's transport when a SOCKS5
	// proxy is configured, so routing every provider through it keeps Chainlink
	// on the same path as the HTTP rate sources. Dialing directly would send
	// requests to the providers in the clear even when the rest of the process
	// is proxied, revealing the origin address. Providers are restricted to
	// https, so an http.Client is always sufficient to reach them.
	dialFn := func() (chainRPCClient, error) {
		var lastErr error
		for _, provider := range providers {
			dialCtx, cancel := context.WithTimeout(context.Background(), chainlinkDialTimeout)
			rpcClient, err := rpc.DialOptions(dialCtx, provider, rpc.WithHTTPClient(dexnet.Client))
			cancel()
			if err == nil {
				return ethclient.NewClient(rpcClient), nil
			}
			lastErr = err
		}
		return nil, fmt.Errorf("dial all providers failed: %w", lastErr)
	}

	maxAge := chainlinkMaxAge

	if batchAddr == (common.Address{}) {
		aggregatorABI, err := parseABI(aggregatorV3ABI)
		if err != nil {
			return nil, fmt.Errorf("chainlink: %w", err)
		}
		return chainlinkLegacyFetcherCore(
			feeds,
			dialFn,
			func(addr common.Address, client chainRPCClient) chainlinkAggregatorContract {
				return &aggregatorBoundContract{bound: bind.NewBoundContract(addr, *aggregatorABI, client, nil, nil)}
			},
			maxAge,
		), nil
	}

	batchABI, err := parseABI(multiAggregatorABI)
	if err != nil {
		return nil, fmt.Errorf("chainlink: %w", err)
	}
	return chainlinkBatchFetcherCore(
		feeds,
		dialFn,
		func(client chainRPCClient) chainlinkBatchContract {
			return &batchBoundContract{bound: bind.NewBoundContract(batchAddr, *batchABI, client, nil, nil)}
		},
		maxAge,
	), nil
}

// chainlinkLegacyFetcherCore is the testable per-feed fallback used until the
// batch contract is deployed and configured.
func chainlinkLegacyFetcherCore(
	feeds map[string]common.Address,
	dialFn func() (chainRPCClient, error),
	newContract func(addr common.Address, client chainRPCClient) chainlinkAggregatorContract,
	maxAge time.Duration,
) func(context.Context, []string, util.Logger) (map[string]float64, error) {
	var (
		conn     sharedConn
		divisors = newDivisorCache()
	)

	return func(ctx context.Context, tickers []string, log util.Logger) (map[string]float64, error) {
		hasAny := false
		for _, t := range tickers {
			if _, ok := feeds[t]; ok {
				hasAny = true
				break
			}
		}
		if !hasAny {
			return map[string]float64{}, nil
		}

		current, err := conn.acquire(dialFn)
		if err != nil {
			return nil, fmt.Errorf("chainlink: %w", err)
		}

		var (
			ratesMtx   sync.Mutex
			rates      = make(map[string]float64)
			connFailed atomic.Bool
			wg         sync.WaitGroup
		)

		for _, ticker := range tickers {
			addr, ok := feeds[ticker]
			if !ok {
				continue
			}
			wg.Add(1)
			go func(ticker string, addr common.Address) {
				defer wg.Done()
				contract := newContract(addr, current.client)

				initCtx, cancel := context.WithTimeout(ctx, chainlinkInitTimeout)
				d, err := contract.decimals(initCtx)
				cancel()
				if err != nil {
					log.Warnf("chainlink: failed to query decimals for %s: %v", ticker, err)
					if isConnErr(err) {
						connFailed.Store(true)
					}
					return
				}
				div := divisors.getOrCreate(ticker, d)

				rateCtx, rateCancel := context.WithTimeout(ctx, chainlinkInitTimeout)
				answer, updatedAt, err := contract.latestRoundData(rateCtx)
				rateCancel()
				if err != nil {
					log.Warnf("chainlink: %s/USD: %v", ticker, err)
					if isConnErr(err) {
						connFailed.Store(true)
					}
					return
				}

				if time.Until(updatedAt) > chainlinkFutureSkew {
					log.Warnf("chainlink: %s/USD: future timestamp %v", ticker, updatedAt)
					return
				}
				if time.Since(updatedAt) > maxAge {
					log.Warnf("chainlink: %s/USD: stale price (updated %v ago)", ticker, time.Since(updatedAt).Round(time.Minute))
					return
				}

				f, _ := new(big.Float).Quo(new(big.Float).SetInt(answer), div).Float64()
				ratesMtx.Lock()
				rates[ticker] = f
				ratesMtx.Unlock()
			}(ticker, addr)
		}
		wg.Wait()

		if connFailed.Load() {
			conn.reset(current)
			if len(rates) == 0 {
				return nil, fmt.Errorf("chainlink: all feed reads failed")
			}
			return rates, nil
		}

		current.release()
		if len(rates) == 0 {
			return nil, fmt.Errorf("chainlink: no valid prices returned")
		}
		return rates, nil
	}
}

// chainlinkBatchFetcherCore is the testable batched implementation used once
// the deployed batch contract address is configured.
func chainlinkBatchFetcherCore(
	feeds map[string]common.Address,
	dialFn func() (chainRPCClient, error),
	newBatchContract func(client chainRPCClient) chainlinkBatchContract,
	maxAge time.Duration,
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
	// divisors caches the 10^decimals factor keyed by "ticker:decimals". Keying
	// on both fields means a bad first read (malicious RPC returning wrong
	// decimals) cannot permanently corrupt subsequent honest reads.
	var (
		conn     sharedConn
		divisors = newDivisorCache()
	)

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

		current, err := conn.acquire(dialFn)
		if err != nil {
			return nil, fmt.Errorf("chainlink: %w", err)
		}

		readCtx, cancel := context.WithTimeout(ctx, chainlinkInitTimeout)
		readings, err := newBatchContract(current.client).read(readCtx, addrs)
		cancel()
		if err != nil {
			log.Warnf("chainlink: batch read failed: %v", err)
			if isConnErr(err) {
				conn.reset(current)
			} else {
				current.release()
			}
			return nil, fmt.Errorf("chainlink: batch read failed: %w", err)
		}

		if len(readings) != len(entries) {
			current.release()
			return nil, fmt.Errorf("chainlink: batch read returned %d readings for %d feeds", len(readings), len(entries))
		}

		rates := make(map[string]float64, len(entries))
		for i, reading := range readings {
			ticker := entries[i].ticker

			if !reading.Ok {
				log.Warnf("chainlink: %s/USD: feed returned ok=false", ticker)
				continue
			}
			if reading.Answer == nil || reading.UpdatedAt == nil {
				log.Warnf("chainlink: %s/USD: nil fields in reading", ticker)
				continue
			}
			if reading.Answer.Sign() <= 0 {
				log.Warnf("chainlink: %s/USD: non-positive answer", ticker)
				continue
			}
			if !reading.UpdatedAt.IsInt64() {
				log.Warnf("chainlink: %s/USD: updatedAt overflows int64", ticker)
				continue
			}

			updatedAt := time.Unix(reading.UpdatedAt.Int64(), 0)
			if time.Until(updatedAt) > chainlinkFutureSkew {
				log.Warnf("chainlink: %s/USD: future timestamp %v", ticker, updatedAt)
				continue
			}
			if time.Since(updatedAt) > maxAge {
				log.Warnf("chainlink: %s/USD: stale price (updated %v ago)", ticker, time.Since(updatedAt).Round(time.Minute))
				continue
			}

			div := divisors.getOrCreate(ticker, reading.Decimals)

			f, _ := new(big.Float).Quo(new(big.Float).SetInt(reading.Answer), div).Float64()
			rates[ticker] = f
		}

		current.release()
		if len(rates) == 0 {
			return nil, fmt.Errorf("chainlink: no valid prices returned")
		}
		return rates, nil
	}
}
