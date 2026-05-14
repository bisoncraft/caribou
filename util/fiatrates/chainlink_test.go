package fiatrates

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// TestChainlinkLive tests the Chainlink price feeds on Ethereum mainnet.
// This test connects to a live Ethereum RPC endpoint and queries actual price feeds.
// Use -run TestChainlinkLive to execute this test.
func TestChainlinkLive(t *testing.T) {
	// Use public RPC endpoint
	rpcURL := "https://eth.drpc.org"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ec, err := ethclient.Dial(rpcURL)
	if err != nil {
		t.Skipf("skipping live test: failed to connect to RPC: %v", err)
	}
	defer ec.Close()

	// Test multiple price feeds
	testCases := map[string]string{
		"BTC":   "0xF4030086522a5bEEa4988F8cA5B36dbC97BeE88c",
		"ETH":   "0x5f4eC3Df9cbd43714FE2740f5E3616155c5b8419",
		"LTC":   "0x6AF09DF7563C363B5763b9102712EbeD3b9e859B",
		"DOGE":  "0x2465CefD3b488BE410b941b1d4b2767088e2A028",
		"LINK":  "0x2c1d072e956AFFC0D435Cb7AC38EF18d24d9127c",
		"MATIC": "0x7bAC85A8a13A4BcD8abb3eB7d6b4d632c895a6B1",
	}

	successCount := 0
	for ticker, addrStr := range testCases {
		addr := common.HexToAddress(addrStr)
		price, err := latestUSDPrice(ctx, ec, addr)
		if err != nil {
			t.Logf("  %s: ERROR - %v", ticker, err)
			continue
		}

		if price <= 0 {
			t.Logf("  %s: INVALID - %f", ticker, price)
			continue
		}

		successCount++
		t.Logf("  %s: $%.2f", ticker, price)
	}

	if successCount == 0 {
		t.Fatal("failed to fetch any prices")
	}

	t.Logf("Successfully fetched %d/%d prices", successCount, len(testCases))
}

// TestChainlinkLatestUSDPrice tests a single price feed query.
func TestChainlinkLatestUSDPrice(t *testing.T) {
	rpcURL := "https://eth.drpc.org"

	ec, err := newEthClient(rpcURL)
	if err != nil {
		t.Skipf("skipping live test: %v", err)
	}
	defer ec.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test BTC/USD feed
	btcAddr := common.HexToAddress("0xF4030086522a5bEEa4988F8cA5B36dbC97BeE88c")
	price, err := latestUSDPrice(ctx, ec, btcAddr)
	if err != nil {
		t.Fatalf("failed to fetch BTC price: %v", err)
	}

	if price <= 0 {
		t.Fatalf("invalid BTC price: %f", price)
	}

	t.Logf("BTC/USD: $%.2f", price)

	// BTC should be between $1k and $1M (sanity check)
	if price < 1000 || price > 1000000 {
		t.Logf("warning: BTC price %f seems unusual", price)
	}
}

// TestChainlinkFeedsExist verifies all configured feeds have valid addresses.
func TestChainlinkFeedsExist(t *testing.T) {
	for ticker, addr := range chainlinkMainnetFeeds {
		if addr == (common.Address{}) {
			t.Errorf("%s feed has zero address", ticker)
		}
		if fmt.Sprintf("%s", addr) == "0x0000000000000000000000000000000000000000" {
			t.Errorf("%s feed has zero address", ticker)
		}
	}
	t.Logf("verified %d feeds with valid addresses", len(chainlinkMainnetFeeds))
}

// newEthClient creates an ethclient for the given RPC URL.
func newEthClient(rpcURL string) (*ethclient.Client, error) {
	return ethclient.Dial(rpcURL)
}
