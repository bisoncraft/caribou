//go:build rpclive

package base

import (
	"context"
	"os"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/eth"
)

var mt *eth.MRPCTest

func TestMain(m *testing.M) {
	ctx, shutdown := context.WithCancel(context.Background())
	mt = eth.NewMRPCTest(ctx, ChainConfig, NetworkCompatibilityData, "weth.base")
	doIt := func() int {
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

func TestMonitorTestnet(t *testing.T) {
	mt.TestMonitorNet(t, util.Testnet)
}

func TestMonitorMainnet(t *testing.T) {
	mt.TestMonitorNet(t, util.Mainnet)
}

func TestRPCMainnet(t *testing.T) {
	mt.TestRPC(t, util.Mainnet)
}

func TestRPCTestnet(t *testing.T) {
	mt.TestRPC(t, util.Testnet)
}

func TestFreeServers(t *testing.T) {
	// Verified working (2026-09-03)
	freeServers := []string{
		"https://base-rpc.publicnode.com",
		"https://mainnet.base.org",
		"https://base.drpc.org",
		"https://1rpc.io/base",
		"https://base.api.onfinality.io/public",
	}
	mt.TestFreeServers(t, freeServers, util.Mainnet)
}

func TestFreeTestnetServers(t *testing.T) {
	// Verified working (2026-09-03)
	freeServers := []string{
		"https://base-sepolia-rpc.publicnode.com",
		"https://sepolia.base.org",
		"https://base-sepolia.drpc.org",
		"https://84532.rpc.thirdweb.com",
		// Still answers, but dropped from the production defaults for
		// rate limiting rather than being dead.
		"https://base-sepolia.api.onfinality.io/public",
		"https://base-sepolia.gateway.tenderly.co",
	}
	mt.TestFreeServers(t, freeServers, util.Testnet)
}

func TestMainnetCompliance(t *testing.T) {
	mt.TestMainnetCompliance(t)
}

func TestTestnetFees(t *testing.T) {
	mt.FeeHistory(t, util.Testnet, 3, 90)
}

func TestFees(t *testing.T) {
	mt.FeeHistory(t, util.Mainnet, 3, 365)
}

func TestReceiptsHaveEffectiveGasPrice(t *testing.T) {
	mt.TestReceiptsHaveEffectiveGasPrice(t)
}

func TestBaseReceiptsHaveEffectiveGasPrice(t *testing.T) {
	mt.TestBaseReceiptsHaveEffectiveGasPrice(t)
}

func TestBaseBlockStats(t *testing.T) {
	mt.BaseBlockStats(t, 5, 1024, util.Mainnet)
}

func TestBaseTestnetBlockStats(t *testing.T) {
	mt.BaseBlockStats(t, 5, 1024, util.Testnet)
}
