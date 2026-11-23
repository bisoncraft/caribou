// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package assets

import (
	"math/big"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
)

// dgwInputs returns a DifficultyAt that serves entries, oldest first, as the
// blocks just below height, and nothing else.
func dgwInputs(height int32, entries []netparams.BlockDifficulty) netparams.DifficultyAt {
	first := height - int32(len(entries))
	return func(h int32) (netparams.BlockDifficulty, bool) {
		if h < first || h >= height {
			return netparams.BlockDifficulty{}, false
		}
		return entries[h-first], true
	}
}

// dgwSpan returns 24 entries with the given bits whose first and last
// timestamps are span seconds apart. Dark Gravity Wave reads no other
// timestamp.
func dgwSpan(bits uint32, span int64) []netparams.BlockDifficulty {
	const t0 = 1_800_000_000
	entries := make([]netparams.BlockDifficulty, dashDGWPastBlocks)
	for i := range entries {
		entries[i] = netparams.BlockDifficulty{Bits: bits, Timestamp: t0}
	}
	entries[len(entries)-1].Timestamp = uint32(t0 + span)
	return entries
}

// TestDashDGWCoreVector runs the mainnet case of get_next_work in Dash Core
// v24.0.2 src/test/pow_tests.cpp: the (time, bits) pairs of the 24 blocks
// ending at mainnet height 123456, copied verbatim, must yield bits 0x1b1441de
// for block 123457. The pairs match mainnet blocks 123433 through 123456, and
// block 123457 carries those bits. The devnet cases of that test exercise
// minimum-difficulty rules, which mainnet does not have.
func TestDashDGWCoreVector(t *testing.T) {
	mainnetData := [][2]uint32{
		{1408728124, 0x1b104be1}, {1408728332, 0x1b10e09e}, {1408728479, 0x1b11a33c},
		{1408728495, 0x1b121cf3}, {1408728608, 0x1b11951e}, {1408728744, 0x1b11abac},
		{1408728756, 0x1b118d9c}, {1408728950, 0x1b1123f9}, {1408729116, 0x1b1141bf},
		{1408729179, 0x1b110764}, {1408729305, 0x1b107556}, {1408729474, 0x1b104297},
		{1408729576, 0x1b1063d0}, {1408729587, 0x1b10e878}, {1408729647, 0x1b0dfaff},
		{1408729678, 0x1b0c9ab8}, {1408730179, 0x1b0c03d6}, {1408730862, 0x1b0dd168},
		{1408730914, 0x1b10b864}, {1408731242, 0x1b0fed89}, {1408731256, 0x1b113ff1},
		{1408732229, 0x1b10460b}, {1408732257, 0x1b13b83f}, {1408732489, 0x1b1418d4},
	}
	entries := make([]netparams.BlockDifficulty, len(mainnetData))
	for i, d := range mainnetData {
		entries[i] = netparams.BlockDifficulty{Timestamp: d[0], Bits: d[1]}
	}
	got, ok := DashParams["mainnet"].NextBits(123457, dgwInputs(123457, entries))
	if !ok {
		t.Fatal("NextBits(123457) reports missing inputs")
	}
	if got != 0x1b1441de {
		t.Fatalf("NextBits(123457) = 0x%08x, want 0x1b1441de", got)
	}
}

// TestDashDGWTruncation pins the iteration order and the per-step truncation
// of the past target average. The targets are small enough that every
// truncated unit survives the compact encoding: the rule gives 609, while one
// truncation at the end gives 616, iterating oldest first 585, dividing
// before multiplying 602, and the arithmetic mean 615. The 4321 s timespan
// also pins multiplying before dividing in the retarget, which would
// otherwise give 0.
func TestDashDGWTruncation(t *testing.T) {
	const height = 1_000_000
	entries := make([]netparams.BlockDifficulty, dashDGWPastBlocks)
	for i := range entries {
		entries[i] = netparams.BlockDifficulty{
			Bits:      blockchain.BigToCompact(big.NewInt(int64(i*7919%1009 + 3))),
			Timestamp: uint32(1_800_000_000 + 150*i),
		}
	}
	entries[len(entries)-1].Timestamp = 1_800_000_000 + 4321
	got, ok := DashParams["mainnet"].NextBits(height, dgwInputs(height, entries))
	if !ok {
		t.Fatal("NextBits reports missing inputs")
	}
	if target := blockchain.CompactToBig(got); target.Cmp(big.NewInt(609)) != 0 {
		t.Fatalf("NextBits target %s, want 609", target)
	}
}

// TestDashDGWClampAndCap checks the timespan clamp and the PowLimit cap. With
// 24 equal targets the average is that target, so the bits follow from the
// timespan alone.
func TestDashDGWClampAndCap(t *testing.T) {
	const height = 1_000_000
	for _, tc := range []struct {
		name string
		bits uint32
		span int64
		want uint32
	}{
		// 3600 s is the target timespan: the bits carry over.
		{"nominal", 0x1b1441de, 3600, 0x1b1441de},
		// Below 1200 s, the timespan clamps to 1200 s and the target falls
		// to a third. Unclamped, 230 s would give 0x1b014b51.
		{"fast", 0x1b1441de, 230, 0x1b06c09f},
		{"fast edge", 0x1b1441de, 1200, 0x1b06c09f},
		{"backward", 0x1b1441de, -460, 0x1b06c09f},
		// Above 10800 s, the timespan clamps to 10800 s and the target
		// triples. Unclamped, 23000 s would give 0x1c00816b.
		{"slow edge", 0x1b1441de, 10800, 0x1b3cc59a},
		{"slow", 0x1b1441de, 23000, 0x1b3cc59a},
		// Tripling the genesis target exceeds PowLimit, whose compact form
		// is 0x1e0fffff. Uncapped, the bits would be 0x1e2fffd0.
		{"cap", 0x1e0ffff0, 23000, 0x1e0fffff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DashParams["mainnet"].NextBits(height, dgwInputs(height, dgwSpan(tc.bits, tc.span)))
			if !ok {
				t.Fatal("NextBits reports missing inputs")
			}
			if got != tc.want {
				t.Fatalf("NextBits = 0x%08x, want 0x%08x", got, tc.want)
			}
		})
	}
}

// TestDashDGWInputs checks that the rule reads blocks h-24 through h-1 and
// reports false when one is missing, and that it reports false below the
// Dark Gravity Wave activation height.
func TestDashDGWInputs(t *testing.T) {
	next := DashParams["mainnet"].NextBits
	entries := dgwSpan(0x1b1441de, 3600)
	const height = 1_000_000
	if _, ok := next(height, dgwInputs(height, entries)); !ok {
		t.Fatal("NextBits reports missing inputs with all 24 blocks present")
	}
	if _, ok := next(height, dgwInputs(height, entries[1:])); ok {
		t.Fatal("NextBits succeeded without block h-24")
	}
	if _, ok := next(height, dgwInputs(height-1, entries)); ok {
		t.Fatal("NextBits succeeded without block h-1")
	}
	if _, ok := next(dashDGWHeight, dgwInputs(dashDGWHeight, entries)); !ok {
		t.Fatal("NextBits reports missing inputs at the activation height")
	}
	if _, ok := next(dashDGWHeight-1, dgwInputs(dashDGWHeight-1, entries)); ok {
		t.Fatal("NextBits succeeded below the activation height")
	}
}

// TestDashDGWOverflowBound pins the bound that lets big.Int stand in for
// Core's wrapping 256-bit arithmetic. Every target the rule reads is at most
// PowLimit (2^236 - 1 on mainnet), so avg*24 + T is at most 25*PowLimit and
// the retarget product at most 3*3600*PowLimit, both below 2^250. The
// constructor refuses a PowLimit for which they could reach 2^256.
func TestDashDGWOverflowBound(t *testing.T) {
	p := DashParams["mainnet"]
	timespan := int64(dashDGWPastBlocks * p.TargetTimePerBlock / time.Second)
	for _, factor := range []int64{dashDGWPastBlocks + 1, 3 * timespan} {
		if n := new(big.Int).Mul(p.PowLimit, big.NewInt(factor)).BitLen(); n > 250 {
			t.Fatalf("PowLimit * %d is %d bits, want at most 250", factor, n)
		}
	}
	wide := &netparams.ChainParams{
		Chain:              "dash",
		Name:               "wide",
		PowLimit:           dashRegtestPowLimit,
		TargetTimePerBlock: p.TargetTimePerBlock,
	}
	defer func() {
		if recover() == nil {
			t.Fatal("dashNextBits accepted a PowLimit whose products overflow 256 bits")
		}
	}()
	dashNextBits(wide)
}
