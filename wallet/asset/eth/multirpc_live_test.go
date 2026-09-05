//go:build rpclive

package eth

import (
	"context"
	"os"
	"testing"

	"github.com/bisoncraft/caribou/util"
)

const (
	alphaHTTPPort = "38556"
	alphaWSPort   = "38557"
)

var mt *MRPCTest

func TestMain(m *testing.M) {
	ctx, shutdown := context.WithCancel(context.Background())
	mt = NewMRPCTest(ctx, ChainConfig, NetworkCompatibilityData, "eth")
	doIt := func() int {
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

func TestHTTP(t *testing.T) {
	mt.TestHTTP(t, alphaHTTPPort)
}

func TestWS(t *testing.T) {
	mt.TestWS(t, alphaWSPort)
}

func TestWSTxLogs(t *testing.T) {
	mt.TestWSTxLogs(t, alphaWSPort)
}

func TestSimnetMultiRPCClient(t *testing.T) {
	mt.TestSimnetMultiRPCClient(t, alphaWSPort, alphaHTTPPort)
}

func TestMonitorTestnet(t *testing.T) {
	mt.TestMonitorNet(t, util.Testnet)
}

func TestMonitorMainnet(t *testing.T) {
	mt.TestMonitorNet(t, util.Mainnet)
}

func TestRPC(t *testing.T) {
	mt.TestRPC(t, util.Mainnet)
}

var freeServers = []string{
	// Verified working (26-12-2025)
	"https://eth.api.onfinality.io/public",   // OnFinality
	"https://eth-mainnet.public.blastapi.io", // Blast API
	"https://ethereum-rpc.publicnode.com",    // PublicNode

	// Verified working (01-09-2026)
	"https://eth.drpc.org", // dRPC

	// Failing 26-12-2025
	// "https://ethereum.publicnode.com",      // "TransactionReceipt" error: not found
	// "https://nodes.mewapi.io/rpc/eth",      // connect error: failed to connect to even a single provider among: mewapi.io
	// "https://eth-mainnet-public.unifra.io", // connect error: failed to connect to even a single provider among: unifra.io
	// "https://cloudflare-eth.com/",                                          // "SuggestGasTipCap" error: Method not found
	// "https://eth.llamarpc.com", 			// FAILED : "HeaderByHash" error: context deadline exceeded
}

func TestFreeServers(t *testing.T) {
	mt.TestFreeServers(t, freeServers, util.Mainnet)
}

var freeTestnetServers = []string{
	// Verified working (2026-09-03)
	"https://ethereum-sepolia-rpc.publicnode.com", // PublicNode
	"https://1rpc.io/sepolia",                     // 1RPC
	"https://gateway.tenderly.co/public/sepolia",  // Tenderly

	// Failing 03-27-2024
	// "https://relay-sepolia.flashbots.net", // connect error: failed to connect to even a single provider among: flashbots.net

	// Goerli
	// // Passing 03-26-2024
	// "https://goerli.blockpi.network/v1/rpc/public",
	// // Failing 03-26-2024
	// "https://rpc.ankr.com/eth_goerli",
	// "https://rpc.goerli.eth.gateway.fm",
	// "https://rpc.goerli.mudit.blog",
	// "https://endpoints.omniatech.io/v1/eth/goerli/public",
	// "https://eth-goerli.api.onfinality.io/public",
	// "https://relay-goerli.flashbots.net",
}

func TestFreeTestnetServers(t *testing.T) {
	mt.TestFreeServers(t, freeTestnetServers, util.Testnet)
}

func TestMainnetCompliance(t *testing.T) {
	mt.TestMainnetCompliance(t)
}

func TestReceiptsHaveEffectiveGasPrice(t *testing.T) {
	mt.TestReceiptsHaveEffectiveGasPrice(t)
}

func TestBlockStats(t *testing.T) {
	mt.BlockStats(t, 5, 1024, util.Mainnet)
}

func TestTestnetBlockStats(t *testing.T) {
	mt.BlockStats(t, 5, 1024, util.Testnet)
}
