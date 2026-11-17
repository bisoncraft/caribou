package assets

import (
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
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
// header hashes to its recorded hash, heights and timestamps ascend, every
// checkpoint sits on a retarget boundary on chains with interval retargeting,
// mainnet checkpoints follow the quarterly spacing, and each record carries
// DifficultyLookback difficulty entries with bits inside PowLimit. The filter
// headers and difficulty entries cannot be checked against the chain offline;
// the livenet TestCheckpointRecords in the spv package derives and compares
// them.
//
// The boundary requirement is what lets LTC's lookback be 1: a checkpoint at
// boundary B is followed by an off-boundary run that reads only parents, and
// the first retarget above it, at B+2016, samples B-1.
func TestCheckpoints(t *testing.T) {
	for chain, nets := range map[string]map[string]*netparams.ChainParams{
		"btc":  BTCParams,
		"ltc":  LTCParams,
		"dash": DashParams,
	} {
		for net, params := range nets {
			t.Run(chain+"."+net, func(t *testing.T) {
				cps := params.Checkpoints
				interval := int32(params.TargetTimespan / params.TargetTimePerBlock)
				for i, cp := range cps {
					if cp.Hash == nil || cp.Header == nil || cp.FilterHeader == nil {
						t.Fatalf("checkpoint %d lacks a hash, header or filter header", cp.Height)
					}
					if h := params.HeaderHash(cp.Header); h != *cp.Hash {
						t.Fatalf("checkpoint %d: header hashes to %s, recorded hash is %s",
							cp.Height, h, cp.Hash)
					}
					if chain != "dash" && cp.Height%interval != 0 {
						t.Fatalf("checkpoint %d is not on a retarget boundary (every %d blocks)",
							cp.Height, interval)
					}
					if len(cp.Difficulty) != params.DifficultyLookback {
						t.Fatalf("checkpoint %d carries %d difficulty entries, want %d",
							cp.Height, len(cp.Difficulty), params.DifficultyLookback)
					}
					for j, d := range cp.Difficulty {
						if tgt := blockchain.CompactToBig(d.Bits); tgt.Sign() <= 0 || tgt.Cmp(params.PowLimit) > 0 {
							t.Fatalf("checkpoint %d: difficulty entry %d has bits 0x%08x outside (0, PowLimit]",
								cp.Height, j, d.Bits)
						}
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
			})
		}
	}
}
