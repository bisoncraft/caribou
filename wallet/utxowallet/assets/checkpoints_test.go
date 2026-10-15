package assets

import (
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
)

// mainnetCheckpointSpacing is the distance between consecutive mainnet
// checkpoints: seven BTC retarget periods, 26 LTC retarget periods, and a
// fixed 52500 blocks on Dash, which retargets every block. Each is about three
// months of blocks.
var mainnetCheckpointSpacing = map[string]int32{
	"btc":  14112,
	"ltc":  52416,
	"dash": 52500,
}

// TestCheckpoints validates the checkpoint records of every network: each
// header hashes to its recorded hash, heights and timestamps ascend, the first
// checkpoint sits on a retarget boundary on chains with interval retargeting,
// and mainnet checkpoints follow the quarterly spacing. The filter headers
// cannot be checked offline; the livenet TestCheckpointRecords in the spv
// package compares them against the network.
func TestCheckpoints(t *testing.T) {
	for chain, nets := range map[string]map[string]*netparams.ChainParams{
		"btc":  BTCParams,
		"ltc":  LTCParams,
		"dash": DashParams,
	} {
		for net, params := range nets {
			t.Run(chain+"."+net, func(t *testing.T) {
				cps := params.Checkpoints
				for i, cp := range cps {
					if cp.Hash == nil || cp.Header == nil || cp.FilterHeader == nil {
						t.Fatalf("checkpoint %d lacks a hash, header or filter header", cp.Height)
					}
					if h := params.HeaderHash(cp.Header); h != *cp.Hash {
						t.Fatalf("checkpoint %d: header hashes to %s, recorded hash is %s",
							cp.Height, h, cp.Hash)
					}
					if i == 0 {
						continue
					}
					prev := cps[i-1]
					if cp.Height <= prev.Height {
						t.Fatalf("checkpoint %d follows checkpoint %d", cp.Height, prev.Height)
					}
					if !cp.Header.Timestamp.After(prev.Header.Timestamp) {
						t.Fatalf("checkpoint %d is not newer than checkpoint %d", cp.Height, prev.Height)
					}
					if spacing := mainnetCheckpointSpacing[chain]; net == "mainnet" &&
						(cp.Height-cps[0].Height)%spacing != 0 {
						t.Fatalf("checkpoint %d is not a multiple of %d blocks after %d",
							cp.Height, spacing, cps[0].Height)
					}
				}
				if len(cps) == 0 || chain == "dash" {
					return
				}
				interval := int32(params.TargetTimespan / params.TargetTimePerBlock)
				if cps[0].Height%interval != 0 {
					t.Fatalf("checkpoint %d is not on a retarget boundary (every %d blocks)",
						cps[0].Height, interval)
				}
			})
		}
	}
}
