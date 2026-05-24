package fiatrates

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// TestChainlinkLive tests the Chainlink price feeds on Polygon mainnet end-to-end.
func TestChainlinkLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live test")
	}
	cfg := Config{
		ChainlinkTestnet: false,
		// Uses default providers if not specified
	}

	getRates, err := chainlinkRateFetcher(cfg)
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
	cfg := Config{
		ChainlinkTestnet: false,
	}

	getRates, err := chainlinkRateFetcher(cfg)
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
	cfg := Config{
		ChainlinkTestnet: false,
	}

	getRates, err := chainlinkRateFetcher(cfg)
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
		_, err := chainlinkRateFetcher(Config{ChainlinkProviders: val})
		if err == nil {
			t.Errorf("ChainlinkProviders=%q: expected error, got nil", val)
		}
	}
}

// TestChainlinkTestnetConfiguration tests that the testnet flag correctly selects testnet feeds.
func TestChainlinkTestnetConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("live test")
	}
	cfg := Config{
		ChainlinkTestnet: true,
	}

	getRates, err := chainlinkRateFetcher(cfg)
	if err != nil {
		t.Fatalf("failed to initialize chainlink fetcher: %v", err)
	}

	log := util.NewLogger("chainlink-test", util.LevelDebug, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Testnet feeds only have ETH and BTC
	tickers := []string{"ETH", "BTC"}
	rates, err := getRates(ctx, tickers, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should successfully get rates from testnet
	if len(rates) > 0 {
		t.Logf("Successfully fetched %d/%d testnet prices", len(rates), len(tickers))
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

// newMockCoreFetch wires a single mock batch contract into chainlinkFetcherCore
// under the given ticker, so closure tests exercise the production code path.
func newMockCoreFetch(ticker string, mock *mockBatchContract) func(context.Context, []string, util.Logger) (map[string]float64, error) {
	return chainlinkFetcherCore(
		map[string]common.Address{ticker: mockFeedAddr},
		func() (chainRPCClient, error) { return newMockRPCClient(), nil },
		func(_ chainRPCClient) chainlinkBatchContract { return mock },
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
	fetch := chainlinkFetcherCore(
		map[string]common.Address{}, // no feeds configured
		func() (chainRPCClient, error) {
			t.Fatal("dial should not happen when no requested ticker has a feed")
			return nil, nil
		},
		func(_ chainRPCClient) chainlinkBatchContract {
			t.Fatal("newBatchContract should not be called for unmatched tickers")
			return nil
		},
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

// TestChainlinkClosureStaleness tests that prices older than chainlinkMaxAge are rejected.
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
	rates, err := fetch(context.Background(), []string{"BTC"}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := rates["BTC"]; ok {
		t.Error("expected stale BTC price to be rejected, but it was returned")
	}
}

// TestChainlinkClosureFutureTimestamp tests that prices beyond the skew window are rejected.
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
	rates, err := fetch(context.Background(), []string{"BTC"}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := rates["BTC"]; ok {
		t.Error("expected future-timestamp BTC price to be rejected, but it was returned")
	}
}

// TestChainlinkClosureContractError tests that a batch read error causes the ticker to be skipped.
func TestChainlinkClosureContractError(t *testing.T) {
	mock := &mockBatchContract{
		err: fmt.Errorf("rpc error: connection refused"),
	}
	fetch := newMockCoreFetch("BTC", mock)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)
	rates, err := fetch(context.Background(), []string{"BTC"}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := rates["BTC"]; ok {
		t.Error("expected erroring batch contract to produce no rates, but BTC was returned")
	}
}

// TestChainlinkClosureOkFalse tests that a reading with ok=false is logged and skipped.
func TestChainlinkClosureOkFalse(t *testing.T) {
	mock := &mockBatchContract{readings: map[common.Address]MultiAggregatorReading{
		mockFeedAddr: {Ok: false},
	}}
	fetch := newMockCoreFetch("BTC", mock)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)
	rates, err := fetch(context.Background(), []string{"BTC"}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := rates["BTC"]; ok {
		t.Error("expected ok=false reading to be skipped, but BTC was returned")
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
	dialFn := func() (chainRPCClient, error) {
		dialCount++
		c := newMockRPCClient()
		if firstClient == nil {
			firstClient = c
		}
		return c, nil
	}

	fetch := chainlinkFetcherCore(
		map[string]common.Address{"BTC": mockFeedAddr},
		dialFn,
		func(_ chainRPCClient) chainlinkBatchContract {
			return &mockBatchContract{err: connErr}
		},
	)

	log := util.NewLogger("test", util.LevelDebug, io.Discard)

	// First call: connects, batch read fails with conn error, ec reset.
	rates, err := fetch(context.Background(), []string{"BTC"}, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 0 {
		t.Errorf("expected no rates on connection failure, got %v", rates)
	}
	if !firstClient.isClosed() {
		t.Error("expected client to be closed after connection failure")
	}
	if dialCount != 1 {
		t.Errorf("expected 1 dial, got %d", dialCount)
	}

	// Second call must re-dial since ec was reset to nil.
	_, _ = fetch(context.Background(), []string{"BTC"}, log)
	if dialCount != 2 {
		t.Errorf("expected 2 dials after reconnect, got %d", dialCount)
	}
}

// TestChainlinkConcurrentRates exercises the double-check locking patterns for
// ec and divisors under concurrent load. Run with -race to detect data races.
func TestChainlinkConcurrentRates(t *testing.T) {
	fetch := chainlinkFetcherCore(
		chainlinkPolygonFeeds,
		func() (chainRPCClient, error) { return newMockRPCClient(), nil },
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
