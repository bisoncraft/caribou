//go:build harness

package zcl

// Regnet tests expect the ZEC test harness to be running. The harness miner
// must be OFF.

import (
	"testing"

	"github.com/bisoncraft/meshwallet/wallet/asset/btc/livetest"
	"github.com/bisoncraft/meshwallet/dex"
)

var (
	tLotSize uint64 = 1e6
	tZCL            = &dex.Asset{
		ID:         BipID,
		Symbol:     "zcl",
		MaxFeeRate: 100,
		SwapConf:   1,
	}
)

func TestWallet(t *testing.T) {
	livetest.Run(t, &livetest.Config{
		NewWallet: NewWallet,
		LotSize:   tLotSize,
		Asset:     tZCL,
		FirstWallet: &livetest.WalletName{
			Node:     "alpha",
			Filename: "alpha.conf",
		},
		SecondWallet: &livetest.WalletName{
			Node:     "beta",
			Filename: "beta.conf",
		},
	})
}
