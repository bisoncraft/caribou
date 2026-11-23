// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package assets

import (
	"fmt"
	"math/big"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
)

func init() {
	btc := BTCParams["mainnet"]
	btc.NextBits = BitcoinNextBits(btc)
	ltc := LTCParams["mainnet"]
	ltc.NextBits = LitecoinNextBits(ltc)
	dash := DashParams["mainnet"]
	dash.NextBits = dashNextBits(dash)
}

// BitcoinNextBits returns the NextBits rule of a chain that retargets every
// TargetTimespan/TargetTimePerBlock blocks the way Bitcoin does: a block off a
// retarget boundary carries its parent's bits, and a block at boundary B
// scales the parent's target by the time between blocks B-interval and B-1.
// The rule omits minimum-difficulty blocks, so p must not set
// ReduceMinDifficulty or PoWNoRetargeting.
func BitcoinNextBits(p *netparams.ChainParams) func(int32, netparams.DifficultyAt) (uint32, bool) {
	return intervalNextBits(p, false)
}

// LitecoinNextBits is BitcoinNextBits with Litecoin's sample height: the
// timespan at boundary B starts at block B-interval-1 instead of B-interval,
// except at the first retarget, which has no block below B-interval. The
// overlap is Litecoin's fix for a majority miner setting difficulty at will
// through the timestamps at period edges.
func LitecoinNextBits(p *netparams.ChainParams) func(int32, netparams.DifficultyAt) (uint32, bool) {
	return intervalNextBits(p, true)
}

func intervalNextBits(p *netparams.ChainParams, litecoin bool) func(int32, netparams.DifficultyAt) (uint32, bool) {
	if p.ReduceMinDifficulty || p.PoWNoRetargeting {
		panic(fmt.Sprintf("interval difficulty rule on %s %s, which allows minimum-difficulty "+
			"blocks or does not retarget", p.Chain, p.Name))
	}
	if p.TargetTimePerBlock <= 0 || p.TargetTimespan < p.TargetTimePerBlock || p.PowLimit == nil {
		panic(fmt.Sprintf("interval difficulty rule on %s %s without retarget parameters",
			p.Chain, p.Name))
	}
	interval := int32(p.TargetTimespan / p.TargetTimePerBlock)
	genesisBits := p.PowLimitBits
	if p.GenesisBlock != nil {
		genesisBits = p.GenesisBlock.Header.Bits
	}
	return func(height int32, at netparams.DifficultyAt) (uint32, bool) {
		if height <= 0 {
			return genesisBits, height == 0
		}
		parent, ok := at(height - 1)
		if !ok {
			return 0, false
		}
		if height%interval != 0 {
			return parent.Bits, true
		}
		first := height - interval
		if litecoin && height != interval {
			first--
		}
		start, ok := at(first)
		if !ok {
			return 0, false
		}
		return computeNextWorkRequired(p, parent, start), true
	}
}

// computeNextWorkRequired implements the Bitcoin and Litecoin retarget:
// new_target = prev_target * actual_timespan / expected_timespan, with the
// timespan clamped to [expected/factor, expected*factor] and the target capped
// at PowLimit. prior is the last block of the period and first is the block
// the timespan is measured from.
//
// It mirrors btcd's blockchain.calcNextRequiredDifficulty, which cannot be
// called directly: the standalone form is unexported, and the exported
// CalcNextRequiredDifficulty is a method on *blockchain.BlockChain that needs
// a full block index. The duplication is consensus-sensitive, so
// TestComputeNextWorkRequired pins the arithmetic and the mainnet vector tests
// reproduce real retargets.
//
// Litecoin Core shifts the target right by one bit before multiplying when the
// target is at least 2^235, to keep its 256-bit intermediate from overflowing,
// and shifts the quotient back afterward. big.Int needs no shift, and the
// results encode identically. A target decoded from compact bits at or above
// 2^235 is a multiple of 2^212 (a 23-bit mantissa times a power of 256), so
// the pre-shift is exact. With y = (target/2) * timespan / expected, Core's
// result 2*floor(y) and the exact floor(2y) then differ by at most the lowest
// bit, with Core's even. The clamped
// timespan keeps both at or above 2^233, whose compact encoding drops at least
// the low 216 bits, so the two encode the same. When the product exceeds
// PowLimit, Core's is at least PowLimit, and both cap to it.
// TestComputeNextWorkRequiredLitecoinShift pins the equivalence.
func computeNextWorkRequired(p *netparams.ChainParams, prior, first netparams.BlockDifficulty) uint32 {
	expectedTimespan := int64(p.TargetTimespan / time.Second)
	factor := p.RetargetAdjustmentFactor
	if factor == 0 {
		factor = 4
	}
	minTimespan := expectedTimespan / factor
	maxTimespan := expectedTimespan * factor

	actualTimespan := int64(prior.Timestamp) - int64(first.Timestamp)
	actualTimespan = max(min(actualTimespan, maxTimespan), minTimespan)

	newTarget := blockchain.CompactToBig(prior.Bits)
	newTarget.Mul(newTarget, big.NewInt(actualTimespan))
	newTarget.Div(newTarget, big.NewInt(expectedTimespan))
	if newTarget.Cmp(p.PowLimit) > 0 {
		newTarget.Set(p.PowLimit)
	}
	return blockchain.BigToCompact(newTarget)
}
