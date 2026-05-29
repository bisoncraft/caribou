//go:build harness

package doge

// Regnet tests expect the DOGE test harness to be running. The harness miner
// must be OFF.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
)

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
	fmt.Printf("##### Fee rate fetched for %s! %d Sats/vB \n", net, feeRate)
}
