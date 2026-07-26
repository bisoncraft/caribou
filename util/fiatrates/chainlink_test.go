package fiatrates

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
)

// TestChainlinkDialsThroughDexnet asserts that Chainlink RPC traffic is carried
// by dexnet's shared client. dexnet.SetProxy applies a SOCKS5 proxy by
// rewriting that client's transport, so a fetcher that dialed ethclient
// directly would keep reaching the providers in the clear while the rest of the
// process was proxied, disclosing the origin address on every refresh.
func TestChainlinkDialsThroughDexnet(t *testing.T) {
	var reached atomic.Bool

	old := dexnet.Client
	defer func() { dexnet.Client = old }()
	dexnet.Client = &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			reached.Store(true)
			// Fail the request. Only the transport it took is under test.
			return nil, fmt.Errorf("blocked by test transport")
		}),
	}

	getRates, err := ChainlinkRateFetcher(Config{})
	if err != nil {
		t.Fatalf("ChainlinkRateFetcher error: %v", err)
	}

	log := util.NewLogger("chainlink-test", util.LevelDebug, io.Discard)
	if _, err := getRates(context.Background(), []string{"BTC"}, log); err == nil {
		t.Fatal("expected an error from the blocked transport")
	}

	if !reached.Load() {
		t.Fatal("chainlink RPC bypassed dexnet.Client; a configured proxy would not apply")
	}
}

// rpcReqInfo carries the fields of a JSON-RPC request body that fake
// transports need: the method to dispatch on and the id to echo back.
type rpcReqInfo struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func parseRPCRequest(req *http.Request) (*rpcReqInfo, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	info := new(rpcReqInfo)
	if err := json.Unmarshal(body, info); err != nil {
		return nil, err
	}
	return info, nil
}

func rpcResponse(id json.RawMessage, result string) *http.Response {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%q}`, id, result)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestChainlinkProviderFailover asserts that a provider failing its probe is
// skipped for the next one in the list, and that the batch read is issued
// against the provider that answered. Constructing an HTTP RPC client performs
// no I/O, so without the probe the first provider would be pinned even while
// unreachable and the rest of the list would be dead weight.
func TestChainlinkProviderFailover(t *testing.T) {
	deadHost := "polygon.drpc.org"       // chainlinkDefaultProviders[0]
	liveHost := "polygon.publicnode.com" // chainlinkDefaultProviders[1]

	var deadProbes atomic.Int32
	var readHost atomic.Value // string

	old := dexnet.Client
	defer func() { dexnet.Client = old }()
	dexnet.Client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			info, err := parseRPCRequest(req)
			if err != nil {
				return nil, err
			}
			if req.URL.Host == deadHost {
				deadProbes.Add(1)
				return nil, fmt.Errorf("host down")
			}
			switch info.Method {
			case "eth_chainId":
				return rpcResponse(info.ID, "0x89"), nil // 137, Polygon mainnet
			case "eth_call":
				readHost.Store(req.URL.Host)
				// Fail the read; only its routing is under test.
				return nil, fmt.Errorf("read blocked by test transport")
			}
			return nil, fmt.Errorf("unexpected method %s", info.Method)
		}),
	}

	getRates, err := ChainlinkRateFetcher(Config{})
	if err != nil {
		t.Fatalf("ChainlinkRateFetcher error: %v", err)
	}

	log := util.NewLogger("chainlink-test", util.LevelDebug, io.Discard)
	if _, err := getRates(context.Background(), []string{"BTC"}, log); err == nil {
		t.Fatal("expected an error from the blocked batch read")
	}

	if deadProbes.Load() == 0 {
		t.Fatal("first provider was never attempted")
	}
	if got, _ := readHost.Load().(string); got != liveHost {
		t.Fatalf("batch read went to %q, want %q", got, liveHost)
	}
}

// TestChainlinkRejectsWrongChainProvider asserts that a provider answering for
// a chain other than Polygon mainnet fails its probe. The feed addresses exist
// only on Polygon mainnet, so accepting such a provider would yield empty
// results every cycle with nothing to point at the misconfiguration.
func TestChainlinkRejectsWrongChainProvider(t *testing.T) {
	old := dexnet.Client
	defer func() { dexnet.Client = old }()
	dexnet.Client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			info, err := parseRPCRequest(req)
			if err != nil {
				return nil, err
			}
			if info.Method != "eth_chainId" {
				return nil, fmt.Errorf("unexpected method %s", info.Method)
			}
			return rpcResponse(info.ID, "0x1"), nil // Ethereum mainnet
		}),
	}

	getRates, err := ChainlinkRateFetcher(Config{})
	if err != nil {
		t.Fatalf("ChainlinkRateFetcher error: %v", err)
	}

	log := util.NewLogger("chainlink-test", util.LevelDebug, io.Discard)
	_, err = getRates(context.Background(), []string{"BTC"}, log)
	if err == nil || !strings.Contains(err.Error(), "chain ID") {
		t.Fatalf("want a chain-ID rejection, got: %v", err)
	}
}

// TestIsConnErr covers the caller-vs-provider distinction: a per-call timeout
// under a live caller context is a hung provider and must fail over, while a
// dead caller context must not churn the shared connection.
func TestIsConnErr(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"generic error", context.Background(), fmt.Errorf("connection refused"), true},
		{"per-call timeout, live caller", context.Background(), context.DeadlineExceeded, true},
		{"wrapped per-call timeout", context.Background(), fmt.Errorf("post: %w", context.DeadlineExceeded), true},
		{"caller canceled", canceled, context.Canceled, false},
		{"caller ctx done, provider error", canceled, fmt.Errorf("connection refused"), false},
		{"execution revert", context.Background(), fmt.Errorf("execution reverted"), false},
	}
	for _, tt := range tests {
		if got := isConnErr(tt.ctx, tt.err); got != tt.want {
			t.Errorf("%s: isConnErr = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestChainlinkLive tests the Chainlink price feeds on Polygon mainnet end-to-end.
func TestChainlinkLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live test")
	}
	cfg := Config{}

	getRates, err := ChainlinkRateFetcher(cfg)
	if err != nil {
		t.Fatalf("failed to initialize chainlink fetcher: %v", err)
	}

	log := util.NewLogger("chainlink-test", util.LevelDebug, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tickers := []string{"BTC", "ETH", "LTC", "BCH", "DOGE", "ZEC", "LINK"}
	rates, err := getRates(ctx, tickers, log)
	if err != nil {
		t.Fatalf("failed to fetch rates: %v", err)
	}

	if len(rates) == 0 {
		t.Fatal("failed to fetch any prices")
	}

	for ticker, price := range rates {
		t.Logf("  %s: $%.2f", ticker, price)
	}

	t.Logf("Successfully fetched %d/%d prices", len(rates), len(tickers))
}

// TestChainlinkFeedsExist verifies all configured feeds have valid addresses.
func TestChainlinkFeedsExist(t *testing.T) {
	for ticker, addr := range chainlinkPolygonFeeds {
		if addr == (common.Address{}) {
			t.Errorf("%s feed has zero address", ticker)
		}
	}
	t.Logf("verified %d feeds with valid addresses", len(chainlinkPolygonFeeds))
}

// TestChainlinkMissingTicker tests handling of tickers with no configured feed.
func TestChainlinkMissingTicker(t *testing.T) {
	if testing.Short() {
		t.Skip("live test")
	}
	getRates, err := ChainlinkRateFetcher(Config{})
	if err != nil {
		t.Fatalf("failed to initialize chainlink fetcher: %v", err)
	}

	log := util.NewLogger("chainlink-test", util.LevelDebug, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Request tickers that don't have configured feeds
	tickers := []string{"UNKNOWN_COIN", "XYZ"}
	rates, err := getRates(ctx, tickers, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return empty map since no feeds exist for these tickers
	if len(rates) != 0 {
		t.Errorf("expected empty rates for unknown tickers, got %d results", len(rates))
	}
}

// TestChainlinkMixedValidInvalidTickers tests fetching a mix of valid and invalid tickers.
func TestChainlinkMixedValidInvalidTickers(t *testing.T) {
	if testing.Short() {
		t.Skip("live test")
	}
	getRates, err := ChainlinkRateFetcher(Config{})
	if err != nil {
		t.Fatalf("failed to initialize chainlink fetcher: %v", err)
	}

	log := util.NewLogger("chainlink-test", util.LevelDebug, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Mix of valid and invalid tickers
	tickers := []string{"BTC", "NONEXISTENT", "ETH", "INVALID_TICKER"}
	rates, err := getRates(ctx, tickers, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have prices for BTC and ETH only.
	if len(rates) == 0 {
		t.Fatal("no rates returned")
	}

	if _, hasBTC := rates["BTC"]; !hasBTC {
		t.Error("expected BTC price in results")
	}
	if _, hasETH := rates["ETH"]; !hasETH {
		t.Error("expected ETH price in results")
	}
	if rate, hasNonexistent := rates["NONEXISTENT"]; hasNonexistent {
		t.Errorf("should not have price for NONEXISTENT, got: %f", rate)
	}
}

// TestValidateRPCURL tests RPC URL validation directly.
func TestValidateRPCURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{
			name:    "valid https URL",
			url:     "https://polygon.drpc.org",
			wantErr: false,
		},
		{
			name:    "http rejected",
			url:     "http://polygon.drpc.org",
			wantErr: true,
		},
		{
			name:    "invalid scheme",
			url:     "ftp://invalid.com",
			wantErr: true,
		},
		{
			name:    "missing host",
			url:     "https://",
			wantErr: true,
		},
		{
			name:    "unparseable URL",
			url:     "not a valid url",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRPCURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRPCURL(%q) = %v, want error = %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

// TestChainlinkRateFetcherEmptyProviders verifies that a non-empty but
// whitespace-only ChainlinkProviders setting is rejected at construction
// rather than silently producing an empty provider list and a confusing
// dial-time error.
func TestChainlinkRateFetcherEmptyProviders(t *testing.T) {
	for _, val := range []string{" ", "\t", " \n\t "} {
		_, err := ChainlinkRateFetcher(Config{ChainlinkProviders: val})
		if err == nil {
			t.Errorf("ChainlinkProviders=%q: expected error, got nil", val)
		}
	}
}

// mockFeedAddr is an arbitrary non-zero address used as a feed-table key
// in closure tests; its value is irrelevant because the batch contract mock
// is keyed on this address.
var mockFeedAddr = common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

// mockBatchContract is a test mock of chainlinkBatchContract.
type mockBatchContract struct {
	readings map[common.Address]MultiAggregatorReading
	err      error
}

func (m *mockBatchContract) read(_ context.Context, addrs []common.Address) ([]MultiAggregatorReading, error) {
	if m.err != nil {
		return nil, m.err
	}
	out := make([]MultiAggregatorReading, len(addrs))
	for i, addr := range addrs {
		if r, ok := m.readings[addr]; ok {
			out[i] = r
		}
		// unrecognized address: out[i] stays zero-valued (Ok=false)
	}
	return out, nil
}

// newMockCoreFetch wires a single mock batch contract into chainlinkBatchFetcherCore
// under the given ticker, so closure tests exercise the production code path.
func newMockCoreFetch(ticker string, mock *mockBatchContract) func(context.Context, []string, util.Logger) (map[string]float64, error) {
	return chainlinkBatchFetcherCore(
		map[string]common.Address{ticker: mockFeedAddr},
		func(context.Context) (chainRPCClient, error) { return newMockRPCClient(), nil },
		func(_ chainRPCClient) chainlinkBatchContract { return mock },
		chainlinkMaxAge,
	)
}

// TestChainlinkClosureValidPrice tests that a fresh, valid price is returned correctly.
func TestChainlinkClosureValidPrice(t *testing.T) {
	// $50,000 with 8 decimals
	mock := &mockBatchContract{readings: map[common.Address]MultiAggregatorReading{
		mockFeedAddr: {
			Answer:    big.NewInt(5000000000000),
			Decimals:  8,
			UpdatedAt: big.NewInt(time.Now().Add(-1 * time.Minute).Unix()),
			Ok:        true,
		},
	}}
	fetch := newMockCoreFetch("BTC", mock)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)
	rates, err := fetch(context.Background(), []string{"BTC"}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	price, ok := rates["BTC"]
	if !ok {
		t.Fatal("expected BTC price in results")
	}
	if price != 50000.0 {
		t.Errorf("expected 50000.0, got %f", price)
	}
}

// TestChainlinkClosureDecimals tests price conversion across different decimal values.
func TestChainlinkClosureDecimals(t *testing.T) {
	tests := []struct {
		name     string
		answer   *big.Int
		decimals uint8
		expected float64
	}{
		{
			name:     "8 decimals (standard)",
			answer:   big.NewInt(5000000000000), // 50000 * 1e8
			decimals: 8,
			expected: 50000.0,
		},
		{
			name:     "6 decimals",
			answer:   big.NewInt(50000000000), // 50000 * 1e6
			decimals: 6,
			expected: 50000.0,
		},
		{
			name:     "0 decimals",
			answer:   big.NewInt(50000),
			decimals: 0,
			expected: 50000.0,
		},
		{
			name:     "18 decimals",
			answer:   new(big.Int).Mul(big.NewInt(50000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			decimals: 18,
			expected: 50000.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockBatchContract{readings: map[common.Address]MultiAggregatorReading{
				mockFeedAddr: {
					Answer:    tt.answer,
					Decimals:  tt.decimals,
					UpdatedAt: big.NewInt(time.Now().Add(-1 * time.Minute).Unix()),
					Ok:        true,
				},
			}}
			fetch := newMockCoreFetch("BTC", mock)

			log := util.NewLogger("test", util.LevelDebug, io.Discard)
			rates, err := fetch(context.Background(), []string{"BTC"}, log)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			price, ok := rates["BTC"]
			if !ok {
				t.Fatal("expected BTC price in results")
			}
			if price != tt.expected {
				t.Errorf("expected %f, got %f", tt.expected, price)
			}
		})
	}
}

// TestChainlinkClosureMissingTicker tests that tickers absent from feeds are silently skipped.
func TestChainlinkClosureMissingTicker(t *testing.T) {
	fetch := chainlinkBatchFetcherCore(
		map[string]common.Address{}, // no feeds configured
		func(context.Context) (chainRPCClient, error) {
			t.Fatal("dial should not happen when no requested ticker has a feed")
			return nil, nil
		},
		func(_ chainRPCClient) chainlinkBatchContract {
			t.Fatal("newBatchContract should not be called for unmatched tickers")
			return nil
		},
		chainlinkMaxAge,
	)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)
	rates, err := fetch(context.Background(), []string{"BTC", "ETH"}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rates) != 0 {
		t.Errorf("expected empty rates for unknown tickers, got %d results", len(rates))
	}
}

// TestChainlinkClosureStaleness tests that prices older than chainlinkMaxAge
// cause an error so the oracle preserves its last-known rates.
func TestChainlinkClosureStaleness(t *testing.T) {
	mock := &mockBatchContract{readings: map[common.Address]MultiAggregatorReading{
		mockFeedAddr: {
			Answer:    big.NewInt(5000000000000),
			Decimals:  8,
			UpdatedAt: big.NewInt(time.Now().Add(-(chainlinkMaxAge + time.Minute)).Unix()),
			Ok:        true,
		},
	}}
	fetch := newMockCoreFetch("BTC", mock)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)
	_, err := fetch(context.Background(), []string{"BTC"}, log)
	if err == nil {
		t.Fatal("expected error when all feeds are stale, got nil")
	}
}

// TestChainlinkClosureFutureTimestamp tests that prices beyond the skew window
// cause an error so the oracle preserves its last-known rates.
func TestChainlinkClosureFutureTimestamp(t *testing.T) {
	mock := &mockBatchContract{readings: map[common.Address]MultiAggregatorReading{
		mockFeedAddr: {
			Answer:    big.NewInt(5000000000000),
			Decimals:  8,
			UpdatedAt: big.NewInt(time.Now().Add(time.Hour).Unix()),
			Ok:        true,
		},
	}}
	fetch := newMockCoreFetch("BTC", mock)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)
	_, err := fetch(context.Background(), []string{"BTC"}, log)
	if err == nil {
		t.Fatal("expected error when all feeds have future timestamps, got nil")
	}
}

// TestChainlinkClosureContractError tests that a batch read error is returned to
// the caller so the oracle can preserve its last-known rates instead of
// overwriting them with an empty map.
func TestChainlinkClosureContractError(t *testing.T) {
	mock := &mockBatchContract{
		err: fmt.Errorf("rpc error: connection refused"),
	}
	fetch := newMockCoreFetch("BTC", mock)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)
	_, err := fetch(context.Background(), []string{"BTC"}, log)
	if err == nil {
		t.Fatal("expected error on batch read failure, got nil")
	}
}

// TestChainlinkClosureOkFalse tests that a reading with ok=false causes an
// error so the oracle preserves its last-known rates.
func TestChainlinkClosureOkFalse(t *testing.T) {
	mock := &mockBatchContract{readings: map[common.Address]MultiAggregatorReading{
		mockFeedAddr: {Ok: false},
	}}
	fetch := newMockCoreFetch("BTC", mock)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)
	_, err := fetch(context.Background(), []string{"BTC"}, log)
	if err == nil {
		t.Fatal("expected error when all feeds return ok=false, got nil")
	}
}

func TestChainlinkRateFetcherInvalidBatchAddress(t *testing.T) {
	tests := []string{
		"not-an-address",
		"0x1234",
		"0x0000000000000000000000000000000000000000",
	}

	for _, addr := range tests {
		t.Run(addr, func(t *testing.T) {
			_, err := ChainlinkRateFetcher(Config{ChainlinkBatchAddr: addr})
			if err == nil {
				t.Fatalf("expected error for ChainlinkBatchAddr=%q", addr)
			}
		})
	}
}

// abiDecodeMockClient is a chainRPCClient that returns fixed bytes from
// CallContract, used to exercise the ABI decoding path end-to-end.
type abiDecodeMockClient struct{ returnBytes []byte }

func (m *abiDecodeMockClient) CodeAt(_ context.Context, _ common.Address, _ *big.Int) ([]byte, error) {
	return nil, nil
}
func (m *abiDecodeMockClient) CallContract(_ context.Context, _ ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	return m.returnBytes, nil
}
func (m *abiDecodeMockClient) Close() {}

// TestChainlinkABITupleDecoding verifies that MultiAggregatorReading fields are
// decoded in ABI tuple position order. A field reorder in the struct produces
// wrong values here even when types still match, catching the failure in CI.
func TestChainlinkABITupleDecoding(t *testing.T) {
	// ABI-encoded return for read() → Reading[1]:
	//   answer=5000000000000, updatedAt=1000000, decimals=8, ok=true
	encoded, err := hex.DecodeString(
		"0000000000000000000000000000000000000000000000000000000000000020" + // array offset
			"0000000000000000000000000000000000000000000000000000000000000001" + // length = 1
			"0000000000000000000000000000000000000000000000000000048c27395000" + // answer
			"00000000000000000000000000000000000000000000000000000000000f4240" + // updatedAt
			"0000000000000000000000000000000000000000000000000000000000000008" + // decimals
			"0000000000000000000000000000000000000000000000000000000000000001", // ok
	)
	if err != nil {
		t.Fatal(err)
	}

	batchABI, err := parseABI(multiAggregatorABI)
	if err != nil {
		t.Fatal(err)
	}
	contract := &batchBoundContract{
		bound: bind.NewBoundContract(common.Address{}, *batchABI,
			&abiDecodeMockClient{returnBytes: encoded}, nil, nil),
	}

	readings, err := contract.read(context.Background(), []common.Address{{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(readings) != 1 {
		t.Fatalf("expected 1 reading, got %d", len(readings))
	}
	r := readings[0]
	if r.Answer == nil || r.Answer.Cmp(big.NewInt(5000000000000)) != 0 {
		t.Errorf("Answer: got %v, want 5000000000000", r.Answer)
	}
	if r.UpdatedAt == nil || r.UpdatedAt.Cmp(big.NewInt(1000000)) != 0 {
		t.Errorf("UpdatedAt: got %v, want 1000000", r.UpdatedAt)
	}
	if r.Decimals != 8 {
		t.Errorf("Decimals: got %d, want 8", r.Decimals)
	}
	if !r.Ok {
		t.Error("Ok: got false, want true")
	}
}

// mockRPCClient implements chainRPCClient for testing. CodeAt and CallContract
// are never invoked in these tests because newBatchContract returns mock
// contracts that bypass the bound-contract machinery entirely.
type mockRPCClient struct {
	once     sync.Once
	closedCh chan struct{}
}

func newMockRPCClient() *mockRPCClient {
	return &mockRPCClient{closedCh: make(chan struct{})}
}

func (m *mockRPCClient) CodeAt(_ context.Context, _ common.Address, _ *big.Int) ([]byte, error) {
	return nil, nil
}

func (m *mockRPCClient) CallContract(_ context.Context, _ ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	return nil, nil
}

func (m *mockRPCClient) Close() {
	m.once.Do(func() { close(m.closedCh) })
}

func (m *mockRPCClient) isClosed() bool {
	select {
	case <-m.closedCh:
		return true
	default:
		return false
	}
}

// TestChainlinkConnFailedReset verifies that a connection error during the
// batch read triggers a connection reset and re-dial on the next call.
func TestChainlinkConnFailedReset(t *testing.T) {
	connErr := fmt.Errorf("connection refused")

	var (
		dialCount   int
		firstClient *mockRPCClient
	)
	dialFn := func(context.Context) (chainRPCClient, error) {
		dialCount++
		c := newMockRPCClient()
		if firstClient == nil {
			firstClient = c
		}
		return c, nil
	}

	fetch := chainlinkBatchFetcherCore(
		map[string]common.Address{"BTC": mockFeedAddr},
		dialFn,
		func(_ chainRPCClient) chainlinkBatchContract {
			return &mockBatchContract{err: connErr}
		},
		chainlinkMaxAge,
	)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)

	// First call: connects, batch read fails with conn error, ec reset.
	_, err := fetch(context.Background(), []string{"BTC"}, log)
	if err == nil {
		t.Fatal("expected error on connection failure, got nil")
	}
	if !firstClient.isClosed() {
		t.Error("expected client to be closed after connection failure")
	}
	if dialCount != 1 {
		t.Errorf("expected 1 dial, got %d", dialCount)
	}

	// Second call must re-dial since conn was reset to nil.
	_, _ = fetch(context.Background(), []string{"BTC"}, log)
	if dialCount != 2 {
		t.Errorf("expected 2 dials after reconnect, got %d", dialCount)
	}
}

// TestChainlinkConcurrentRates exercises the double-check locking patterns for
// ec and divisors under concurrent load. Run with -race to detect data races.
func TestChainlinkConcurrentRates(t *testing.T) {
	fetch := chainlinkBatchFetcherCore(
		chainlinkPolygonFeeds,
		func(context.Context) (chainRPCClient, error) { return newMockRPCClient(), nil },
		func(_ chainRPCClient) chainlinkBatchContract {
			return &mockBatchContract{readings: map[common.Address]MultiAggregatorReading{
				chainlinkPolygonFeeds["BTC"]: {
					Answer:    big.NewInt(5000000000000),
					Decimals:  8,
					UpdatedAt: big.NewInt(time.Now().Add(-time.Minute).Unix()),
					Ok:        true,
				},
				chainlinkPolygonFeeds["ETH"]: {
					Answer:    big.NewInt(300000000000),
					Decimals:  8,
					UpdatedAt: big.NewInt(time.Now().Add(-time.Minute).Unix()),
					Ok:        true,
				},
			}}
		},
		chainlinkMaxAge,
	)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = fetch(context.Background(), []string{"BTC", "ETH"}, log)
		}()
	}
	wg.Wait()
}
