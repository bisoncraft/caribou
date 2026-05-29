//go:build harness

package firo

// Regnet tests expect the Firo test harness to be running. The harness miner
// must be OFF.
//
// Sim harness info:
// The harness has four nodes: alpha, beta, gamma and delta with one wallet
// per node. All wallets have confirmed UTXOs. The alpha wallet has only
// coinbase outputs.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
)

// Tests only mainnet, testnet is expected to return -1 normally
func TestFetchExternalFee(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	rate, err := externalFeeRate(ctx, util.Mainnet)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("External fee rate fetched:: %d sat/B\n", rate)
}
func TestExternalFeeRate(t *testing.T) {
	fetchRateWithTimeout(t, util.Mainnet)
	fetchRateWithTimeout(t, util.Testnet)
}

func fetchRateWithTimeout(t *testing.T, net util.Network) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	feeRate, err := externalFeeRate(ctx, net)
	if err != nil {
		t.Fatalf("error fetching %s fees: %v", net, err)
	}
	fmt.Printf("##### Fee rate fetched for %s! %d Sats/B \n", net, feeRate)
}
