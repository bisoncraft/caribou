package fiatrates

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// TestChainlinkLive tests the Chainlink price feeds on Polygon mainnet.
// This test connects to a live Polygon RPC endpoint and queries actual price feeds.
// Use -run TestChainlinkLive to execute this test.
func TestChainlinkLive(t *testing.T) {
	t.Skipf("skipping live test: requires an authenticated rpc endpoint")

	// Use public RPC endpoint
	rpcURL := "https://rpc.ankr.com/polygon/<your-auth-token>",

	}

	var ec *ethclient.Client
	var err error

	for _, rpcURL := range rpcURLs {
		ec, err = ethclient.Dial(rpcURL)
		if err == nil {
			break
		}
	}

	if ec == nil {
		t.Fatalf("skipping live test: failed to connect to any RPC endpoint")
	}
	defer ec.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Test multiple price feeds
	testCases := map[string]string{
		"BTC":  "0xc907E116054Ad103354f2D350FD2514433D57F6f",
		"ETH":  "0xF9680D99D6C9589e2a93a78A04A279e509205945",
		"LTC":  "0xEB99F173cf7d9a6dC4D889C2Ad7103e8383b6Efa",
		"BCH":  "0x327d9822e9932996f55b39F557AEC838313da8b7",
		"DOGE": "0xbaf9327b6564454F4a3364C33eFeEf032b4b4444",
		"ZEC":  "0xBC08c639e579a391C4228F20d0C29d0690092DF0",
		"LINK": "0xd9FFdb71EbE7496cC440152d43986Aae0AB76665",
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
	// Try multiple public Polygon RPC endpoints
	rpcURLs := []string{
		"https://rpc.ankr.com/polygon/cf24355cdd40a03578f2fbaff682439c63877f8e61f206358abb908ee384d917",
		"https://polygon-rpc.com",
		"https://gateway.tenderly.co/public/polygon-mainnet",
	}

	var ec *ethclient.Client
	var err error

	for _, rpcURL := range rpcURLs {
		ec, err = newEthClient(rpcURL)
		if err == nil {
			break
		}
	}

	if ec == nil {
		t.Skipf("skipping live test: failed to connect to any RPC endpoint")
	}
	defer ec.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test BTC/USD feed on Polygon
	btcAddr := common.HexToAddress("0xc907E116054Ad103354f2D350FD2514433D57F6f")
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

// newEthClient creates an ethclient for the given RPC URL.
func newEthClient(rpcURL string) (*ethclient.Client, error) {
	return ethclient.Dial(rpcURL)
}
