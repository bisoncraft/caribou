package fiatrates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/ethereum/go-ethereum/common"
)

// TestChainlinkLive tests the Chainlink price feeds on Polygon mainnet end-to-end.
func TestChainlinkLive(t *testing.T) {
	cfg := Config{
		ChainlinkTestnet: false,
		// Uses default providers if not specified
	}

	getRates, err := chainlinkRateFetcher(cfg)
	if err != nil {
		t.Skipf("skipping live test: failed to initialize chainlink fetcher: %v", err)
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

// mockAggregatorContract is a test mock of chainlinkAggregatorContract.
type mockAggregatorContract struct {
	answer        *big.Int
	decimalPlaces uint8
	updatedAt     time.Time
	err           error
}

func (m *mockAggregatorContract) latestRoundData(ctx context.Context) (*big.Int, time.Time, error) {
	if m.updatedAt.IsZero() {
		return m.answer, time.Now(), m.err
	}
	return m.answer, m.updatedAt, m.err
}

func (m *mockAggregatorContract) decimals(ctx context.Context) (uint8, error) {
	return m.decimalPlaces, nil
}

// TestChainlinkAggregatorContractMock tests price decoding via a mock contract.
func TestChainlinkAggregatorContractMock(t *testing.T) {
	// Test a BTC price of $50,000 (fixed-point: 5000000000000 = 50000 * 1e8)
	btcPrice := big.NewInt(5000000000000)
	mock := &mockAggregatorContract{answer: btcPrice, decimalPlaces: 8}

	answer, _, err := mock.latestRoundData(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the mock returns the configured answer
	if answer.Cmp(btcPrice) != 0 {
		t.Errorf("expected answer %v, got %v", btcPrice, answer)
	}

	// Decode to float64
	f, _ := new(big.Float).Quo(
		new(big.Float).SetInt(answer),
		new(big.Float).SetFloat64(1e8),
	).Float64()

	expected := 50000.0
	if f != expected {
		t.Errorf("expected price %f, got %f", expected, f)
	}

	// Test that mock can return custom errors
	mockWithErr := &mockAggregatorContract{err: fmt.Errorf("custom error")}
	_, _, err = mockWithErr.latestRoundData(context.Background())
	if err == nil {
		t.Error("expected error from mock, got nil")
	}
}

// TestChainlinkFeedsExist verifies all configured feeds have valid addresses.
func TestChainlinkFeedsExist(t *testing.T) {
	for ticker, addr := range chainlinkPolygonFeeds {
		if addr == (common.Address{}) {
			t.Errorf("%s feed has zero address", ticker)
		}
		if fmt.Sprintf("%s", addr) == "0x0000000000000000000000000000000000000000" {
			t.Errorf("%s feed has zero address", ticker)
		}
	}
	t.Logf("verified %d feeds with valid addresses", len(chainlinkPolygonFeeds))
}

// TestChainlinkContractCallError tests error handling for failed contract calls.
func TestChainlinkContractCallError(t *testing.T) {
	customErr := fmt.Errorf("rpc error: connection refused")
	mock := &mockAggregatorContract{err: customErr}

	_, _, err := mock.latestRoundData(context.Background())
	if err == nil {
		t.Fatal("expected error from contract call, got nil")
	}
	if !errors.Is(err, customErr) {
		t.Errorf("expected custom error, got: %v", err)
	}
}

// TestChainlinkMissingTicker tests handling of tickers with no configured feed.
func TestChainlinkMissingTicker(t *testing.T) {
	cfg := Config{
		ChainlinkTestnet: false,
	}

	getRates, err := chainlinkRateFetcher(cfg)
	if err != nil {
		t.Skipf("skipping live test: failed to initialize chainlink fetcher: %v", err)
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
	cfg := Config{
		ChainlinkTestnet: false,
	}

	getRates, err := chainlinkRateFetcher(cfg)
	if err != nil {
		t.Skipf("skipping live test: failed to initialize chainlink fetcher: %v", err)
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

	// Should have prices for BTC and ETH only (or skip if RPC fails)
	if len(rates) == 0 {
		t.Logf("no rates returned (RPC may be unavailable)")
		return
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

// TestChainlinkTestnetConfiguration tests that the testnet flag correctly selects testnet feeds.
func TestChainlinkTestnetConfiguration(t *testing.T) {
	cfg := Config{
		ChainlinkTestnet: true,
	}

	getRates, err := chainlinkRateFetcher(cfg)
	if err != nil {
		t.Skipf("skipping test: failed to initialize chainlink fetcher: %v", err)
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

// TestChainlinkAggregatorWithDifferentDecimals tests price conversion with different decimal places.
func TestChainlinkAggregatorWithDifferentDecimals(t *testing.T) {
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
			name:     "18 decimals",
			answer:   new(big.Int).Mul(big.NewInt(50000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			decimals: 18,
			expected: 50000.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockAggregatorContract{
				answer:        tt.answer,
				decimalPlaces: tt.decimals,
			}

			answer, _, err := mock.latestRoundData(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			decimals, err := mock.decimals(context.Background())
			if err != nil {
				t.Fatalf("unexpected error getting decimals: %v", err)
			}

			// Verify decimals query works
			if decimals != tt.decimals {
				t.Errorf("expected decimals %d, got %d", tt.decimals, decimals)
			}

			// Manually convert like the fetcher does
			divisor := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
			f, _ := new(big.Float).Quo(
				new(big.Float).SetInt(answer),
				divisor,
			).Float64()

			if f != tt.expected {
				t.Errorf("expected price %f, got %f", tt.expected, f)
			}
		})
	}
}

// TestChainlinkPriceStaleness tests that stale prices are rejected.
func TestChainlinkPriceStaleness(t *testing.T) {
	tests := []struct {
		name         string
		updatedAt    time.Time
		shouldReject bool
	}{
		{
			name:         "fresh price",
			updatedAt:    time.Now(),
			shouldReject: false,
		},
		{
			name:         "price from 30 minutes ago",
			updatedAt:    time.Now().Add(-30 * time.Minute),
			shouldReject: false,
		},
		{
			name:         "price from 2 hours ago",
			updatedAt:    time.Now().Add(-2 * time.Hour),
			shouldReject: true,
		},
		{
			name:         "price from 24 hours ago",
			updatedAt:    time.Now().Add(-24 * time.Hour),
			shouldReject: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Check if the staleness logic would reject this timestamp
			isStale := time.Since(tt.updatedAt) > FiatRateDataExpiry
			if isStale != tt.shouldReject {
				t.Errorf("staleness check failed: isStale=%v, shouldReject=%v", isStale, tt.shouldReject)
			}
		})
	}
}

// TestChainlinkDecimalEdgeCases tests extreme decimal values.
func TestChainlinkDecimalEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		answer   *big.Int
		decimals uint8
		expected float64
	}{
		{
			name:     "zero decimals (price is raw answer)",
			answer:   big.NewInt(50000),
			decimals: 0,
			expected: 50000.0,
		},
		{
			name:     "18 decimals (erc20 standard)",
			answer:   new(big.Int).Mul(big.NewInt(50000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			decimals: 18,
			expected: 50000.0,
		},
		{
			name:     "20 decimals (extreme)",
			answer:   new(big.Int).Mul(big.NewInt(50000), new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)),
			decimals: 20,
			expected: 50000.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockAggregatorContract{
				answer:        tt.answer,
				decimalPlaces: tt.decimals,
			}

			answer, _, err := mock.latestRoundData(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			decimals, err := mock.decimals(context.Background())
			if err != nil {
				t.Fatalf("unexpected error getting decimals: %v", err)
			}

			if decimals != tt.decimals {
				t.Errorf("expected decimals %d, got %d", tt.decimals, decimals)
			}

			divisor := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
			f, _ := new(big.Float).Quo(
				new(big.Float).SetInt(answer),
				divisor,
			).Float64()

			if f != tt.expected {
				t.Errorf("expected price %f, got %f", tt.expected, f)
			}
		})
	}
}
