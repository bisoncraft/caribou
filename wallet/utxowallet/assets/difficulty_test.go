// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package assets

import (
	"math/big"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/wire"
)

// TestComputeNextWorkRequired pins the retarget arithmetic so it cannot drift
// from the consensus rule. btcd's implementation is not reusable standalone
// (unexported, or a *BlockChain method), so the expectations are re-derived
// from the formula across the unclamped path, both timespan clamps, and the
// PowLimit clamp.
func TestComputeNextWorkRequired(t *testing.T) {
	const expected = int64(1_209_600) // 14 days of 10-minute blocks
	const factor = int64(4)

	// A prior target well below PowLimit, canonicalized through the compact
	// round trip so the expectations use the same precision as the rule.
	bigPowLimit := new(big.Int).Lsh(big.NewInt(1), 255)
	priorBits := blockchain.BigToCompact(new(big.Int).Lsh(big.NewInt(1), 240))
	priorTarget := blockchain.CompactToBig(priorBits)

	mkParams := func(powLimit *big.Int) *netparams.ChainParams {
		return &netparams.ChainParams{
			TargetTimespan:           time.Duration(expected) * time.Second,
			RetargetAdjustmentFactor: factor,
			PowLimit:                 powLimit,
		}
	}
	mkFields := func(actualTimespan int64) (prior, first netparams.BlockDifficulty) {
		const start = 1_600_000_000
		first = netparams.BlockDifficulty{Timestamp: start}
		prior = netparams.BlockDifficulty{Bits: priorBits, Timestamp: uint32(start + actualTimespan)}
		return prior, first
	}
	// scaled returns BigToCompact(priorTarget * num / den), capped at
	// bigPowLimit.
	scaled := func(num, den int64) uint32 {
		tgt := new(big.Int).Mul(priorTarget, big.NewInt(num))
		tgt.Div(tgt, big.NewInt(den))
		if tgt.Cmp(bigPowLimit) > 0 {
			tgt = bigPowLimit
		}
		return blockchain.BigToCompact(tgt)
	}

	tests := []struct {
		name           string
		params         *netparams.ChainParams
		actualTimespan int64
		want           uint32
	}{{
		name:           "unclamped identity",
		params:         mkParams(bigPowLimit),
		actualTimespan: expected,
		want:           scaled(expected, expected),
	}, {
		name:           "clamped to max (slow blocks)",
		params:         mkParams(bigPowLimit),
		actualTimespan: expected * 100,
		want:           scaled(factor, 1),
	}, {
		name:           "clamped to min (fast blocks)",
		params:         mkParams(bigPowLimit),
		actualTimespan: 1,
		want:           scaled(1, factor),
	}, {
		name:           "negative timespan clamped to min",
		params:         mkParams(bigPowLimit),
		actualTimespan: -3600,
		want:           scaled(1, factor),
	}, {
		name:           "PowLimit clamp",
		params:         mkParams(new(big.Int).Lsh(big.NewInt(1), 241)), // 2*priorTarget
		actualTimespan: expected * 100,                                 // wants 4x
		want:           blockchain.BigToCompact(new(big.Int).Lsh(big.NewInt(1), 241)),
	}, {
		// A non-integer ratio exercises the multiply, divide and compact
		// rounding that the identity and clean multiples do not.
		name:           "partial retarget",
		params:         mkParams(bigPowLimit),
		actualTimespan: expected + 123457,
		want:           scaled(expected+123457, expected),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prior, first := mkFields(tt.actualTimespan)
			if got := computeNextWorkRequired(tt.params, prior, first); got != tt.want {
				t.Fatalf("got 0x%08x, want 0x%08x", got, tt.want)
			}
		})
	}
}

// TestComputeNextWorkRequiredLitecoinShift checks that omitting Litecoin
// Core's overflow shift never changes the encoded result. For targets from
// 2^235 to PowLimit and timespans across both clamps, the bits match Core's
// shift, multiply, divide, unshift and cap sequence.
func TestComputeNextWorkRequiredLitecoinShift(t *testing.T) {
	p := LTCParams["mainnet"]
	expected := int64(p.TargetTimespan / time.Second)
	twoTo235 := new(big.Int).Lsh(big.NewInt(1), 235)
	core := func(priorBits uint32, timespan int64) uint32 {
		timespan = max(min(timespan, expected*4), expected/4)
		tgt := blockchain.CompactToBig(priorBits)
		shift := tgt.BitLen() > p.PowLimit.BitLen()-1
		if shift {
			tgt.Rsh(tgt, 1)
		}
		tgt.Mul(tgt, big.NewInt(timespan))
		tgt.Div(tgt, big.NewInt(expected))
		if shift {
			tgt.Lsh(tgt, 1)
		}
		if tgt.Cmp(p.PowLimit) > 0 {
			tgt.Set(p.PowLimit)
		}
		return blockchain.BigToCompact(tgt)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	bitsCases := []uint32{p.PowLimitBits, 0x1e0ffff0, 0x1e080000, 0x1e0a1234, 0x1e0fffef}
	for range 200 {
		bitsCases = append(bitsCases, 0x1e080000+rng.Uint32N(0x080000))
	}
	var shifted int
	for _, bits := range bitsCases {
		tgt := blockchain.CompactToBig(bits)
		if tgt.Cmp(twoTo235) < 0 || tgt.Cmp(p.PowLimit) > 0 {
			t.Fatalf("test setup: bits 0x%08x outside [2^235, PowLimit]", bits)
		}
		if tgt.BitLen() > p.PowLimit.BitLen()-1 {
			shifted++
		}
		for range 200 {
			timespan := rng.Int64N(expected*5) - expected/2
			prior := netparams.BlockDifficulty{Bits: bits, Timestamp: uint32(2_000_000_000 + timespan)}
			first := netparams.BlockDifficulty{Timestamp: 2_000_000_000}
			if got, want := computeNextWorkRequired(p, prior, first), core(bits, timespan); got != want {
				t.Fatalf("bits 0x%08x, timespan %d: got 0x%08x, Litecoin Core 0x%08x",
					bits, timespan, got, want)
			}
		}
	}
	if shifted != len(bitsCases) {
		t.Fatalf("Core's shift applied to %d of %d cases", shifted, len(bitsCases))
	}
}

// TestLitecoinSampleHeights pins Litecoin's sample heights against values
// computed independently of the rule: the first-ever retarget measures from
// block 0 (B-2016), and every later boundary from B-2017. Timestamps advance
// 100 s per block, so each candidate sample height gives different bits.
func TestLitecoinSampleHeights(t *testing.T) {
	p := LTCParams["mainnet"]
	const interval = 2016
	const bits = 0x1b0404cb
	const base = 1_400_000_000
	expected := int64(p.TargetTimespan / time.Second)
	at := func(height int32) (netparams.BlockDifficulty, bool) {
		if height < 0 {
			return netparams.BlockDifficulty{}, false
		}
		return netparams.BlockDifficulty{Bits: bits, Timestamp: base + uint32(height)*100}, true
	}
	// scale is the retarget for a timespan inside the clamps.
	scale := func(timespan int64) uint32 {
		tgt := blockchain.CompactToBig(bits)
		tgt.Mul(tgt, big.NewInt(timespan))
		tgt.Div(tgt, big.NewInt(expected))
		return blockchain.BigToCompact(tgt)
	}
	ltc, btc := LitecoinNextBits(p), BitcoinNextBits(p)
	for _, tc := range []struct {
		name   string
		rule   func(int32, netparams.DifficultyAt) (uint32, bool)
		height int32
		want   uint32 // timespan from the sample block to B-1
	}{
		{"litecoin first retarget samples B-2016", ltc, interval, scale((interval - 1) * 100)},
		{"litecoin later retarget samples B-2017", ltc, 2 * interval, scale(interval * 100)},
		{"bitcoin later retarget samples B-2016", btc, 2 * interval, scale((interval - 1) * 100)},
	} {
		got, ok := tc.rule(tc.height, at)
		if !ok || got != tc.want {
			t.Errorf("%s: got 0x%08x (ok %v), want 0x%08x", tc.name, got, ok, tc.want)
		}
	}
	if scale((interval-1)*100) == scale(interval*100) {
		t.Fatal("test setup: the two sample heights give the same bits")
	}
}

// boundaryVector is a run of real mainnet headers around retarget boundary B:
// B-2017, B-2016, B-1, B and B+1, with their block hashes. B is a checkpoint.
// Hashes were cross-checked across two block explorers (mempool.space and
// blockstream.info for BTC, litecoinspace.org and blockcypher.com for LTC).
type boundaryVector struct {
	boundary int32
	hashes   map[int32]string
	headers  map[int32]string
}

var btcBoundaryVector = boundaryVector{
	boundary: 955584,
	hashes: map[int32]string{
		953567: "00000000000000000001b315e72169bef4867bf780527aa97c625ee9c001dc5d",
		953568: "0000000000000000000090b6c2d9f1e99b76c4c9b54d391fa9d02187425678d9",
		955583: "000000000000000000001e045d65f500623f19ae94e1516e08175efe606b04ef",
		955584: "00000000000000000001e265c627e0a27ad347deb4d6b921f249eddfbf78e011",
		955585: "0000000000000000000154d4e6da13d9729751653be6e773de97ed1c3add53b5",
	},
	headers: map[int32]string{
		953567: "00800f26dfbd8b01230b5e9b3aca93d8a720e456014883041a410100000000000000000009d6a542bf096c051bed08fac75db1319021bbc47706b3dfc461a64d8eeec8caf8f12d6a8f06021713ca8d16",
		953568: "00e0ff3f5ddc01c0e95e627ca97a5280f77b86f4be6921e715b3010000000000000000007c5ead009231dadb996a9301f5150c8b1001f919965721e37d71a4d476bd20589df42d6ac3400217895e0357",
		955583: "00000220c43add24191339285434dd150843196ff1a99ebf2a58000000000000000000008f4ae10c9cc4093a1bea417dea6f35082029f9cb4b88d39029df94bd4ee0b5832e2e3f6ac34002170cbd1d8e",
		955584: "0020a734ef046b60fe5e17086e51e194ae193f6200f5655d041e000000000000000000007460c5f7eaa219119e04ae728c430e86fc2aaa5e9609069d25f99caf34f3ae53a72e3f6a421a02171a8e62f0",
		955585: "0080842011e078bfdfed49f221b9d6b4de47d37aa2e027c665e20100000000000000000089455ae1d0a75e1260b36568c2d7e1442efca172d9a82a28a11fd62e93710d77052f3f6a421a0217beab0a8f",
	},
}

var ltcBoundaryVector = boundaryVector{
	boundary: 3132864,
	hashes: map[int32]string{
		3130847: "a8a48117a4b00f9f4cd5ef1dadccafbe919efe2153c55c808d9dfa9d2ae90f9e",
		3130848: "e780a308edfe36d44d72d887583e825e302b973d7898d37c360abaaf73dcc679",
		3132863: "619c53237b4511e230267db9e63931efe2c6c41f95a3c571dffa352f32169c06",
		3132864: "22076f9f6295e025e4b53c021b936e9f482888734536e86b6ffb7442038c14f4",
		3132865: "6f6203a5380c64b1ed54f5785d5c50e948ebd94aff2a689c31511a441ee7c9b0",
	},
	headers: map[int32]string{
		3130847: "000000207f495f57053d9c3f2b23090d82e2a9104aae0a6489cb140e49ce61cb70a610b889838b2925cb7d844aa2ec2cc0babe46001489d0461ee226a5bae4cffd6a18b9f2353c6afbe02c191216045f",
		3130848: "000000209e0fe92a9dfa9d8d805cc55321fe9e91beafccad1defd54c9f0fb0a41781a4a85f4860c2d5c423d44492ef8dcf1867ffab44c64927bb44fb7ebfc3fd64d5a80382363c6ad2252d19453e550d",
		3132863: "1400002078404a24df1df8feeb45b940422a5b35f6c0205cfae32b5a1250decd18b747f4834b2366ed21aa7faa252a197e95346df3f6422ed9a4b03d6883d68459230aca110b416ad2252d19792ada23",
		3132864: "14000020069c16322f35fadf71c5a3951fc4c6e2ef3139e6b97d2630e211457b23539c61c52a1638a6586a3b4ed2094473b1e0916e4c0ee3b96a3f0e81eb708bd03331ad650b416a7c482f198040f9c4",
		3132865: "14000020f4148c034274fb6f6be83645738828489f6e931b023cb5e425e095629f6f0722a5d4eebef92bc659fd56f9466550bef89537baae7074f89a64ab8e363c31e9b18e0b416a7c482f19d935d256",
	},
}

// TestMainnetNextBits checks the BTC and LTC mainnet rules against real
// retargets: NextBits reproduces the bits of boundary B and of B+1, B is the
// checkpoint record, and B-1 and B+1 link to it.
func TestMainnetNextBits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params *netparams.ChainParams
		vec    boundaryVector
	}{
		{"btc", BTCParams["mainnet"], btcBoundaryVector},
		{"ltc", LTCParams["mainnet"], ltcBoundaryVector},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.vec.boundary
			hdrs := make(map[int32]*wire.BlockHeader, len(tc.vec.headers))
			for height, s := range tc.vec.headers {
				hdr := newHeaderFromStr(s)
				if got, want := tc.params.HeaderHash(hdr), *newHashFromStr(tc.vec.hashes[height]); got != want {
					t.Fatalf("header %d hashes to %s, want %s", height, got, want)
				}
				hdrs[height] = hdr
			}
			var cp *netparams.Checkpoint
			for i := range tc.params.Checkpoints {
				if tc.params.Checkpoints[i].Height == b {
					cp = &tc.params.Checkpoints[i]
				}
			}
			if cp == nil || *cp.Header != *hdrs[b] {
				t.Fatalf("header %d is not the checkpoint record", b)
			}
			if hdrs[b].PrevBlock != tc.params.HeaderHash(hdrs[b-1]) ||
				hdrs[b+1].PrevBlock != tc.params.HeaderHash(hdrs[b]) {
				t.Fatal("vector headers do not link to the checkpoint")
			}
			at := func(height int32) (netparams.BlockDifficulty, bool) {
				hdr, ok := hdrs[height]
				if !ok {
					return netparams.BlockDifficulty{}, false
				}
				return netparams.HeaderDifficulty(hdr), true
			}
			for _, height := range []int32{b, b + 1} {
				got, ok := tc.params.NextBits(height, at)
				if !ok {
					t.Fatalf("NextBits(%d) reports missing inputs", height)
				}
				if got != hdrs[height].Bits {
					t.Fatalf("NextBits(%d) = 0x%08x, header has 0x%08x", height, got, hdrs[height].Bits)
				}
			}
			// The boundary is a real retarget, so the off-boundary rule
			// alone would not reproduce it.
			if hdrs[b].Bits == hdrs[b-1].Bits {
				t.Fatalf("boundary %d does not change bits", b)
			}
			// A missing sample block is reported, not guessed.
			sparse := func(height int32) (netparams.BlockDifficulty, bool) {
				if height < b-1 {
					return netparams.BlockDifficulty{}, false
				}
				return at(height)
			}
			if _, ok := tc.params.NextBits(b, sparse); ok {
				t.Fatal("NextBits at the boundary succeeded without its sample block")
			}
		})
	}
	// Litecoin samples B-2017: the Bitcoin sample height gives different
	// bits at this boundary.
	vec := ltcBoundaryVector
	p := LTCParams["mainnet"]
	at := func(height int32) (netparams.BlockDifficulty, bool) {
		s, ok := vec.headers[height]
		if !ok {
			return netparams.BlockDifficulty{}, false
		}
		return netparams.HeaderDifficulty(newHeaderFromStr(s)), true
	}
	btcRule := BitcoinNextBits(p)
	if got, _ := btcRule(vec.boundary, at); got == newHeaderFromStr(vec.headers[vec.boundary]).Bits {
		t.Fatal("Bitcoin sampling reproduces the Litecoin retarget; the vector does not pin the sample height")
	}
}

// TestNextBitsNetworks checks which networks carry a difficulty rule: every
// mainnet, and no network that allows minimum-difficulty blocks. Dash mainnet
// sets PoWNoRetargeting for the btcd params yet retargets every block; on BTC
// and LTC the flag marks regtest, which carries no rule.
func TestNextBitsNetworks(t *testing.T) {
	for chain, nets := range map[string]map[string]*netparams.ChainParams{
		"btc":  BTCParams,
		"ltc":  LTCParams,
		"dash": DashParams,
	} {
		for net, p := range nets {
			want := net == "mainnet"
			if got := p.NextBits != nil; got != want {
				t.Errorf("%s %s: NextBits set = %v, want %v", chain, net, got, want)
			}
			if p.NextBits != nil && (p.ReduceMinDifficulty || (chain != "dash" && p.PoWNoRetargeting)) {
				t.Errorf("%s %s: NextBits set on a network without enforced retargeting", chain, net)
			}
		}
	}
}
