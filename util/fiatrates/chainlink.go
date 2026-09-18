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

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
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
	// chainlinkDialTimeout bounds the chain-ID probe made to each candidate
	// provider in turn. Constructing an HTTP RPC client performs no I/O, so
	// the probe is what establishes that a provider is reachable; a provider
	// that fails or times out is skipped for the one that follows it.
	chainlinkDialTimeout = 5 * time.Second
	// chainlinkReadTimeout bounds the single batch read that a refresh makes.
	chainlinkReadTimeout = 5 * time.Second
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

// stalenessTracker remembers, per ticker, whether the feed's last reading was
// rejected as stale. A deviation-driven feed can go hours between rounds by
// design, so an unconditional warning every cycle buries the log without
// conveying anything new; only a change of state is worth a warning.
type stalenessTracker struct {
	mtx   sync.Mutex
	stale map[string]bool
}

func newStalenessTracker() *stalenessTracker {
	return &stalenessTracker{stale: make(map[string]bool)}
}

// check reports whether a reading of the given age is too old to use, logging
// the transitions into and out of staleness at warning and info level and the
// steady state at debug level.
func (t *stalenessTracker) check(log util.Logger, ticker string, age, maxAge time.Duration) (stale bool) {
	stale = age > maxAge

	t.mtx.Lock()
	was := t.stale[ticker]
	t.stale[ticker] = stale
	t.mtx.Unlock()

	switch {
	case stale && !was:
		log.Warnf("chainlink: %s/USD: stale price (updated %v ago, limit %v)",
			ticker, age.Round(time.Minute), maxAge)
	case !stale && was:
		log.Infof("chainlink: %s/USD: price is fresh again (updated %v ago)",
			ticker, age.Round(time.Minute))
	case stale:
		log.Debugf("chainlink: %s/USD: still stale (updated %v ago, limit %v)",
			ticker, age.Round(time.Minute), maxAge)
	}
	return stale
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

func (s *sharedConn) acquire(ctx context.Context, dialFn func(context.Context) (chainRPCClient, error)) (*connRef, error) {
	s.mtx.Lock()
	if s.conn != nil {
		current := s.conn
		current.acquire()
		s.mtx.Unlock()
		return current, nil
	}
	s.mtx.Unlock()

	newEC, err := dialFn(ctx)
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

// isConnErr reports whether err indicates a failed provider, in which case the
// shared connection is reset so the next cycle re-probes the provider list. ctx
// is the caller's context, NOT the per-call one: when the caller has abandoned
// the request, the error is theirs and the connection is kept.
func isConnErr(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	// With the caller's context still live, a deadline can only be the
	// per-call timeout: the provider is hung, which fails over like any other
	// connection error.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// EIP-3 execution reverts carry error code 3 in the JSON-RPC layer;
	// use the typed interface where available to avoid string matching.
	type rpcError interface{ ErrorCode() int }
	var rpcErr rpcError
	if errors.As(err, &rpcErr) && rpcErr.ErrorCode() == 3 {
		return false
	}
	msg := err.Error()
	return !strings.Contains(msg, "execution reverted") && !strings.Contains(msg, "VM Exception")
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

// chainlinkFeed is one Polygon Data Feed: its proxy address and the published
// heartbeat, the interval within which the oracle network updates the answer
// even if the price has not moved past the feed's deviation threshold.
type chainlinkFeed struct {
	addr      common.Address
	heartbeat time.Duration
}

// chainlinkGrace and chainlinkDailyGrace are the slack allowed on top of a
// feed's heartbeat before a reading is treated as stale. Between rounds a
// deviation-triggered feed's answer is still accurate to within its threshold,
// so the heartbeat is the real correctness bound and the grace only has to
// absorb block time, propagation, and the gap between fetch cycles. A fixed
// five minutes is the right order of magnitude for a feed that updates every
// few seconds; for a feed that updates once a day it is inside the noise, so
// those get an hour.
const (
	chainlinkGrace      = 5 * time.Minute
	chainlinkDailyGrace = time.Hour
	// chainlinkGraceCutoff is the heartbeat above which the daily grace
	// applies.
	chainlinkGraceCutoff = time.Hour
)

// maxAge is the oldest reading this feed may return before it is rejected.
func (f chainlinkFeed) maxAge() time.Duration {
	if f.heartbeat <= chainlinkGraceCutoff {
		return f.heartbeat + chainlinkGrace
	}
	return f.heartbeat + chainlinkDailyGrace
}

// chainlinkPolygonFeeds maps ticker symbols to their USD Data Feed on Polygon
// mainnet. Polygon offers fresher data due to lower fees and more frequent
// updates.
//
// Addresses and heartbeats are both published at
// https://docs.chain.link/data-feeds/price-feeds/addresses?network=polygon and
// must be verified together: a feed's heartbeat is what bounds how old its
// answer may be (see chainlinkFeed.maxAge), so an address added without its
// published heartbeat cannot be checked for staleness.
//
// The feeds fall into two groups. BTC, ETH, SOL, XRP, BNB, MATIC, USDC, USDT
// and PAXG carry a 27s heartbeat and update continuously. LTC, DASH and XMR
// carry a 86400s heartbeat and are driven almost entirely by their deviation
// threshold (0.5% for LTC, 1% for DASH and XMR), so hours can pass between
// rounds; between them the answer is still within the threshold, which is why
// the age bound follows the heartbeat rather than a single global constant.
//
// The commented entries are verified feeds for assets that wallet/asset/importall
// does not currently register. Uncomment alongside the matching asset. Polygon
// carries no feed at all for HYPE or DCR, which are covered by the HTTP rate
// sources only, as is XAUT (XAU spot is a different instrument).
var chainlinkPolygonFeeds = map[string]chainlinkFeed{
	"BTC": {common.HexToAddress("0xc907E116054Ad103354f2D350FD2514433D57F6f"), 27 * time.Second},
	"ETH": {common.HexToAddress("0xF9680D99D6C9589e2a93a78A04A279e509205945"), 27 * time.Second},
	"SOL": {common.HexToAddress("0x10C8264C0935b3B9870013e057f330Ff3e9C56dC"), 27 * time.Second},
	"XRP": {common.HexToAddress("0x785ba89291f676b5386652eB12b30cF361020694"), 27 * time.Second},
	"BNB": {common.HexToAddress("0x82a6c4AF830caa6c97bb504425f6A66165C2c26e"), 27 * time.Second},
	// MATIC serves the polygon asset, which ParseTicker normalizes to the
	// MATIC ticker the rate APIs still key on. The feed kept its pre-migration
	// name but tracks POL: its published metadata gives POL as the base asset.
	"MATIC": {common.HexToAddress("0xAB594600376Ec9fD91F8e885dADF0CE036862dE0"), 27 * time.Second},
	"USDC":  {common.HexToAddress("0xfE4A8cc5b5B2366C1B58Bea3858e81843581b2F7"), 27 * time.Second},
	"USDT":  {common.HexToAddress("0x0A6513e40db6EB1b165753AD52E80663aeA50545"), 27 * time.Second},
	"PAXG":  {common.HexToAddress("0x0f6914d8e7e1214CDb3A4C6fbf729b75C69DF608"), 27 * time.Second},
	// "WBTC": {common.HexToAddress("0xDE31F8bFBD8c84b5360CFACCa3539B938dd78ae6"), 5 * time.Minute},
	// "DOGE": {common.HexToAddress("0xbaf9327b6564454F4a3364C33eFeEf032b4b4444"), 27 * time.Second},
	// "LINK": {common.HexToAddress("0xd9FFdb71EbE7496cC440152d43986Aae0AB76665"), 27 * time.Second},
	"LTC": {common.HexToAddress("0xEB99F173cf7d9a6dC4D889C2Ad7103e8383b6Efa"), 24 * time.Hour},
	// "BCH": {common.HexToAddress("0x327d9822e9932996f55b39F557AEC838313da8b7"), 24 * time.Hour},
	// "ZEC": {common.HexToAddress("0xBC08c639e579a391C4228F20d0C29d0690092DF0"), 24 * time.Hour},
	"DASH": {common.HexToAddress("0xD94427eDee70E4991b4b8DdCc848f2B58ED01C0b"), 24 * time.Hour},
	// XMR is registered only under the "xmr" build tag. The entry is harmless
	// in a build without it: a ticker no wallet asks for is never read.
	"XMR": {common.HexToAddress("0xBE6FB0AB6302B693368D0E9001fAF77ecc6571db"), 24 * time.Hour},
	// To add: 1. Get the address and heartbeat from the addresses page above
	//         2. Test with live RPC calls
}

// chainlinkPolygonBatchAddr is the deployed MultiAggregatorV0 address on Polygon mainnet.
// Verified source: https://polygonscan.com/address/0x71eCdcF7F77765F0f596c03C63c80541733fd060#code
var chainlinkPolygonBatchAddr = common.HexToAddress("0x71eCdcF7F77765F0f596c03C63c80541733fd060")

// polygonChainID is Polygon mainnet's chain ID. A provider on any other chain
// cannot host the feed addresses above, so it is rejected at dial time with a
// clear error rather than returning empty results every cycle.
var polygonChainID = big.NewInt(137)

// chainlinkDefaultProviders are free public Polygon mainnet RPC endpoints used
// as a fallback if no providers are explicitly configured. These are mainnet
// regardless of the network the wallets are running on; see
// ChainlinkRateFetcher.
//
// This list is intentionally separate from the one in wallet/asset/polygon,
// which is switched per network and includes ws:// and loopback entries that
// neither validateRPCURL nor the http-only dial accept. Sharing it would also
// make util/fiatrates depend on the Polygon wallet, which would rule out that
// package ever depending on this one.
var chainlinkDefaultProviders = []string{
	"https://polygon.drpc.org",
	"https://polygon.publicnode.com",
	"https://polygon.api.onfinality.io/public",
	"https://polygon-public.nodies.app/",
	"https://1rpc.io/matic",
}

// ChainlinkFetchTimeout bounds one full fetch through the fetcher returned by
// ChainlinkRateFetcher. A cold cycle probes providers sequentially, each
// burning up to chainlinkDialTimeout before the batch read gets its
// chainlinkReadTimeout, so the budget is derived from those values and the
// default provider count rather than hardcoded, with headroom so the final
// stage is never left with a clipped deadline. A configured provider list
// longer than the default needs a proportionally larger budget.
var ChainlinkFetchTimeout = time.Duration(len(chainlinkDefaultProviders))*chainlinkDialTimeout +
	chainlinkReadTimeout + 5*time.Second

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
// Rates always come from Polygon mainnet, whatever network the caller's wallets
// are on: a fiat rate is a real-world price, so a testnet or simnet wallet still
// wants the true figure. There is deliberately no network parameter, and
// ChainlinkProviders must name Polygon mainnet endpoints. A provider serving
// any other chain is rejected at dial time by a chain-ID check, since the feed
// addresses below exist only on Polygon mainnet.
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
	//
	// Constructing an HTTP RPC client performs no I/O and cannot fail for a
	// well-formed URL, so each candidate is probed with a chain-ID read before
	// it is accepted. Without the probe the first provider would always be
	// selected whether or not it is reachable, and the rest of the list would
	// be dead weight. The probe also pins the chain: a provider that answers
	// for the wrong network is as useless as one that is down.
	dialFn := func(ctx context.Context) (chainRPCClient, error) {
		var lastErr error
		for _, provider := range providers {
			client, err := probeProvider(ctx, provider)
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", provider, err)
				continue
			}
			return client, nil
		}
		return nil, fmt.Errorf("dial all providers failed: %w", lastErr)
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
	), nil
}

// probeProvider connects to a single RPC provider and verifies with a chain-ID
// read that it is reachable and serving Polygon mainnet. The caller owns the
// returned client and must Close it.
func probeProvider(ctx context.Context, provider string) (chainRPCClient, error) {
	probeCtx, cancel := context.WithTimeout(ctx, chainlinkDialTimeout)
	defer cancel()

	rpcClient, err := rpc.DialOptions(probeCtx, provider, rpc.WithHTTPClient(dexnet.Client))
	if err != nil {
		return nil, err
	}
	client := ethclient.NewClient(rpcClient)

	chainID, err := client.ChainID(probeCtx)
	if err != nil {
		client.Close()
		return nil, err
	}
	if chainID.Cmp(polygonChainID) != 0 {
		client.Close()
		return nil, fmt.Errorf("reports chain ID %s, want %s (Polygon mainnet)", chainID, polygonChainID)
	}
	return client, nil
}

// chainlinkBatchFetcherCore is the testable batched implementation used once
// the deployed batch contract address is configured.
func chainlinkBatchFetcherCore(
	feeds map[string]chainlinkFeed,
	dialFn func(context.Context) (chainRPCClient, error),
	newBatchContract func(client chainRPCClient) chainlinkBatchContract,
) func(context.Context, []string, util.Logger) (map[string]float64, error) {
	// Chainlink is one of several fiat rate sources; the oracle averages across
	// all active sources, so a single compromised RPC cannot unilaterally skew
	// final prices. Operators should restrict ChainlinkProviders to trusted
	// endpoints in production.
	//
	// The provider connection is established lazily on the first getRates call.
	// If the batch read fails, the connection is reset and the next call
	// re-probes the provider list in order, pinning the first endpoint that
	// answers, so a dead provider is skipped rather than re-selected. Failover
	// is between cycles, not within: a provider that dies mid-cycle costs that
	// cycle's rates entirely, since the pinned client is not swapped until the
	// next getRates invocation observes a failure. Only connection-shaped
	// failures reset it: a revert-shaped error keeps the pinned provider,
	// since a genuine revert comes from the contract and would follow the
	// call to any endpoint.
	//
	// divisors caches the 10^decimals factor keyed by "ticker:decimals". Keying
	// on both fields means a bad first read (malicious RPC returning wrong
	// decimals) cannot permanently corrupt subsequent honest reads.
	//
	// staleness remembers which feeds were stale on the previous cycle so that
	// a feed whose rounds are simply spaced further apart than a cycle does not
	// warn on every cycle. Both caches are shared by all callers of the
	// returned closure, which may run concurrently, so both carry their own
	// lock.
	var (
		conn      sharedConn
		divisors  = newDivisorCache()
		staleness = newStalenessTracker()
	)

	return func(ctx context.Context, tickers []string, log util.Logger) (map[string]float64, error) {
		// Build an ordered list of (ticker, addr) pairs for feeds that match
		// the request. The order is preserved so readings[i] maps to entries[i].
		type feedEntry struct {
			ticker string
			feed   chainlinkFeed
		}
		var entries []feedEntry
		for _, t := range tickers {
			if feed, ok := feeds[t]; ok {
				entries = append(entries, feedEntry{t, feed})
			}
		}
		if len(entries) == 0 {
			return map[string]float64{}, nil
		}

		addrs := make([]common.Address, len(entries))
		for i, e := range entries {
			addrs[i] = e.feed.addr
		}

		current, err := conn.acquire(ctx, dialFn)
		if err != nil {
			return nil, fmt.Errorf("chainlink: %w", err)
		}

		readCtx, cancel := context.WithTimeout(ctx, chainlinkReadTimeout)
		readings, err := newBatchContract(current.client).read(readCtx, addrs)
		cancel()
		if err != nil {
			log.Warnf("chainlink: batch read failed: %v", err)
			if isConnErr(ctx, err) {
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
			if staleness.check(log, ticker, time.Since(updatedAt), entries[i].feed.maxAge()) {
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
