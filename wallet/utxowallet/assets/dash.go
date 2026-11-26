package assets

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// Dash netparams. Two things make Dash different from a Bitcoin clone:
//
//   - Block identity. Dash's block-header hash is X11 over the serialized
//     80-byte header (CBlockHeader::GetHash → HashX11), not double-SHA256, and
//     it is also the proof-of-work hash. So both HeaderHashFunc and CheckPoW are
//     wired to the same X11 computation. Transaction ids remain sha256d (handled
//     in bisonwire).
//   - Difficulty. Dash retargets every block (Dark Gravity Wave), not on
//     Bitcoin's 2016-block schedule. Every network sets PoWNoRetargeting so
//     the btcd params derived for the neutrino path never apply Bitcoin's
//     interval retarget check (BFFastAdd skips retarget validation there in
//     any case). On mainnet the electrum header path enforces Dark Gravity
//     Wave through NextBits (dashNextBits): each header above the anchor must
//     carry the bits consensus derives from the 24 blocks below it. Testnet
//     and regtest allow minimum-difficulty blocks and carry no rule; there the
//     claimed difficulty is bounded only by PowLimit (checked by the header
//     store and by the CheckPoW hook) and the checkpoints pin the historical
//     chain.
//
// The genesis coinbase, merkle root, and per-network values are translated from
// dashpay/dash src/chainparams.cpp. The network magics are the REAL Dash
// pchMessageStart values (as little-endian uint32), required for the neutrino
// P2P path — note these differ from the collision-avoidance placeholders in
// wallet/asset/dash/params.go, which only feed the RPC clone.

func mustDecodeTx(hexStr string) *wire.MsgTx {
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		panic(err)
	}
	tx := wire.NewMsgTx(wire.TxVersion)
	if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
		panic(err)
	}
	return tx
}

// dashHeaderHash is the netparams HeaderHashFunc for Dash: the X11 block-header
// hash, delegated to the single source of truth in bisonwire.
func dashHeaderHash(hdr *wire.BlockHeader) chainhash.Hash {
	return bisonwire.HeaderHash(bisonwire.ChainDASH, hdr)
}

// dashCheckPoW returns a CheckPoW hook that validates a header's X11 hash
// against its claimed target AND bounds the claimed target itself by powLimit,
// so no caller of the hook accepts a header at a self-declared target above
// the limit. The electrum header store also range-checks every target, and on
// mainnet requires the Dark Gravity Wave bits; on testnet and regtest the
// bound is the only limit on the difficulty a server may claim. The neutrino
// path independently gets the same bound from
// blockchain.CheckBlockHeaderSanity, whose target checks run even under
// BFNoPoWCheck.
func dashCheckPoW(powLimit *big.Int) func(*wire.BlockHeader) error {
	return func(hdr *wire.BlockHeader) error {
		target := blockchain.CompactToBig(hdr.Bits)
		if target.Sign() <= 0 {
			return fmt.Errorf("block target difficulty of %064x is too low", target)
		}
		if target.Cmp(powLimit) > 0 {
			return fmt.Errorf("block target difficulty of %064x is higher than max of %064x", target, powLimit)
		}
		powHash := dashHeaderHash(hdr)
		if hashNum := blockchain.HashToBig(&powHash); hashNum.Cmp(target) > 0 {
			return fmt.Errorf("block hash of %064x is higher than expected max of %064x", hashNum, target)
		}
		return nil
	}
}

// dashDGWHeight is the first mainnet height whose bits Dark Gravity Wave sets
// (consensus.nPowDGWHeight in Dash Core's chainparams.cpp). Lower blocks
// follow Kimoto Gravity Well or Bitcoin's interval rule, neither of which is
// implemented; every header store starts far above this height.
const dashDGWHeight = 34140

// dashDGWPastBlocks is the number of blocks Dark Gravity Wave reads.
const dashDGWPastBlocks = 24

// dashNextBits returns the NextBits rule of Dash mainnet: Dark Gravity Wave
// v3, ported from DarkGravityWave in Dash Core v24.0.2 src/pow.cpp. The bits
// of the block at height h follow from blocks h-24 through h-1:
//
//   - The past target average starts at the target of block h-1 and takes in
//     each older block's target T as avg = (avg*n + T) / (n+1), where n runs
//     from 2 at block h-2 to 24 at block h-24. Every step truncates. The
//     result is not an arithmetic mean, and a closed form gives different
//     bits, so the iteration order and per-step truncation are consensus.
//   - The actual timespan runs from block h-24 to block h-1, 23 intervals,
//     against a target timespan of 24 block spacings (3600 s on mainnet).
//     Core keeps the off-by-one, which puts mainnet blocks about 2.63 minutes
//     apart on average. The actual timespan is clamped to [target/3,
//     target*3], with Core's integer division.
//   - The new target is avg * actual / target, capped at PowLimit and encoded
//     as compact bits.
//
// The rule does not read the new block's own timestamp: in Dash Core only
// the minimum-difficulty rules of testnet and devnets consult it. Heights
// below dashDGWHeight report false. The targets read come from headers that
// passed the (0, PowLimit] range check or from checkpoint entries held to the
// same range, where blockchain.CompactToBig decodes as Core's SetCompact does.
//
// Core computes in 256-bit integers that wrap on overflow; big.Int does not
// wrap. With every input target at most PowLimit the average stays at most
// PowLimit, so the largest intermediate values are 25*PowLimit (avg*24 + T)
// and 3*3600*PowLimit (the retarget product). The constructor panics unless
// both are below 2^256, where the two arithmetics agree.
func dashNextBits(p *netparams.ChainParams) func(int32, netparams.DifficultyAt) (uint32, bool) {
	if p.ReduceMinDifficulty {
		panic(fmt.Sprintf("Dark Gravity Wave rule on %s %s, which allows minimum-difficulty blocks",
			p.Chain, p.Name))
	}
	if p.PowLimit == nil || p.TargetTimePerBlock < time.Second {
		panic(fmt.Sprintf("Dark Gravity Wave rule on %s %s without difficulty parameters",
			p.Chain, p.Name))
	}
	targetTimespan := int64(dashDGWPastBlocks * p.TargetTimePerBlock / time.Second)
	maxFactor := max(dashDGWPastBlocks+1, 3*targetTimespan)
	if new(big.Int).Mul(p.PowLimit, big.NewInt(maxFactor)).BitLen() > 256 {
		panic(fmt.Sprintf("Dark Gravity Wave on %s %s: PowLimit * %d overflows 256 bits",
			p.Chain, p.Name, maxFactor))
	}
	return func(height int32, at netparams.DifficultyAt) (uint32, bool) {
		if height < dashDGWHeight {
			return 0, false
		}
		last, ok := at(height - 1)
		if !ok {
			return 0, false
		}
		avg := blockchain.CompactToBig(last.Bits)
		first := last
		for n := int64(2); n <= dashDGWPastBlocks; n++ {
			d, ok := at(height - int32(n))
			if !ok {
				return 0, false
			}
			avg.Mul(avg, big.NewInt(n))
			avg.Add(avg, blockchain.CompactToBig(d.Bits))
			avg.Quo(avg, big.NewInt(n+1))
			first = d
		}
		actual := int64(last.Timestamp) - int64(first.Timestamp)
		actual = max(min(actual, targetTimespan*3), targetTimespan/3)
		avg.Mul(avg, big.NewInt(actual))
		avg.Quo(avg, big.NewInt(targetTimespan))
		if avg.Cmp(p.PowLimit) > 0 {
			avg.Set(p.PowLimit)
		}
		return blockchain.BigToCompact(avg), true
	}
}

var (
	// dashMainPowLimit is ~uint256(0) >> 20 (mainnet/testnet).
	dashMainPowLimit = new(big.Int).Sub(new(big.Int).Lsh(bigOne, 236), bigOne)
	// dashRegtestPowLimit is ~uint256(0) >> 1 (regtest/devnet).
	dashRegtestPowLimit = new(big.Int).Sub(new(big.Int).Lsh(bigOne, 255), bigOne)

	// dashGenesisCoinbaseTx is the genesis coinbase, shared across all networks.
	// Its txid equals the genesis merkle root (verified against chainparams.cpp).
	dashGenesisCoinbaseTx = mustDecodeTx("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff6204ffff001d01044c5957697265642030392f4a616e2f3230313420546865204772616e64204578706572696d656e7420476f6573204c6976653a204f76657273746f636b2e636f6d204973204e6f7720416363657074696e6720426974636f696e73ffffffff0100f2052a010000004341040184710fa689ad5023690c80f3a49c8f13f8d45b8c857fbcbc8bc4a8e4d3eb4b10f4d4604fa08dce601aaf0f470216fe1b51850b4acf21b179c45070ac7b03a9ac00000000")
	dashGenesisMerkleRoot = dashGenesisCoinbaseTx.TxHash()
)

// dashMainnetElectrumServers are the built-in electrumx servers for Dash
// mainnet. cipig runs public Dash electrumx (spesmilo electrumx, COIN=Dash) on
// port 20061 with a CA-issued (Let's Encrypt) wildcard certificate, so they are
// verified against the system roots rather than pinned.
var dashMainnetElectrumServers = []*netparams.ServerAddr{
	{Proto: "ssl", Addr: "electrum1.cipig.net:20061"},
	{Proto: "ssl", Addr: "electrum2.cipig.net:20061"},
}

// dashSimnetElectrumServers points at the dextest harness electrumx (Dash
// regtest, self-signed → SkipVerify). Must match the port the harness serves.
var dashSimnetElectrumServers = []*netparams.ServerAddr{
	{Proto: "ssl", Addr: "127.0.0.1:55021", SkipVerify: true},
}

// dashSimnetNeutrinoSeeds is the dextest harness dashd P2P address (alpha node).
var dashSimnetNeutrinoSeeds = []string{"127.0.0.1:20775"}

// dashMainnetCheckpoints are ordered from oldest to newest. Hashes are X11
// block-header hashes, so they compare directly against HeaderHash results.
// They pin the chain during headers-sync on both the electrum and neutrino
// paths. The first is the launch anchor (block 2487500, ~2026-06-13, taken
// from dashpay/dash src/chainparams.cpp): the electrum header store starts
// there and no wallet birthday predates it, so it never moves. Dash retargets
// every block, so there is no retarget period to align to; a checkpoint
// follows every 52500 blocks (about three months) and is added once mined.
// Hashes cross-checked via insight.dash.org and blockchair.com.
// The livenet TestCheckpointRecords in the spv package fetches each header and
// filter header from P2P peers, checks the header against the hash, and
// requires the peers to agree on the filter header.
// Difficulty holds the bits and timestamps of the 23 blocks below each
// checkpoint, which Dark Gravity Wave reads for the blocks just above it
// (see dashNextBits). TestCheckpointRecords derives them from headers linked
// back to the checkpoint header; they were cross-checked against an electrumx
// server and blockchair.com.
var dashMainnetCheckpoints = []netparams.Checkpoint{
	{
		Height:       2487500, // 2026-06-13
		Hash:         newHashFromStr("00000000000000119fe42827219e0686d3f7b494ae65f823194c740c5dbab492"),
		Header:       newHeaderFromStr("000000208553a10fa4d05609e14c46e32f4f978bc375f8f6012413b02300000000000000911418a4dc044cbd5b7b1b1041a8b9e43c31e052e3d3b59ba2246be120817ed45ac22c6a3583291951845a96"),
		FilterHeader: newHashFromStr("b9842cad245a156d0f8a759f6e7c79e286855e8ed8ea2c9333b3ce7756e49d68"),
		Difficulty: []netparams.BlockDifficulty{
			{Bits: 0x1930f48b, Timestamp: 1781314489}, // 2487477
			{Bits: 0x19309075, Timestamp: 1781314517}, // 2487478
			{Bits: 0x1931323d, Timestamp: 1781314694}, // 2487479
			{Bits: 0x192d6a41, Timestamp: 1781315205}, // 2487480
			{Bits: 0x19338084, Timestamp: 1781315253}, // 2487481
			{Bits: 0x19320408, Timestamp: 1781315262}, // 2487482
			{Bits: 0x192e78e1, Timestamp: 1781315310}, // 2487483
			{Bits: 0x192ca275, Timestamp: 1781315377}, // 2487484
			{Bits: 0x192c89a1, Timestamp: 1781315630}, // 2487485
			{Bits: 0x192c7a19, Timestamp: 1781315712}, // 2487486
			{Bits: 0x192c7956, Timestamp: 1781315795}, // 2487487
			{Bits: 0x192cbcb6, Timestamp: 1781315804}, // 2487488
			{Bits: 0x192a4e71, Timestamp: 1781315827}, // 2487489
			{Bits: 0x19261c5f, Timestamp: 1781315858}, // 2487490
			{Bits: 0x1920b826, Timestamp: 1781316288}, // 2487491
			{Bits: 0x192557b9, Timestamp: 1781316422}, // 2487492
			{Bits: 0x1925bd51, Timestamp: 1781316559}, // 2487493
			{Bits: 0x192287cc, Timestamp: 1781316748}, // 2487494
			{Bits: 0x1922c81e, Timestamp: 1781316957}, // 2487495
			{Bits: 0x1924a86a, Timestamp: 1781317503}, // 2487496
			{Bits: 0x19295b4a, Timestamp: 1781317539}, // 2487497
			{Bits: 0x1928f3ca, Timestamp: 1781317589}, // 2487498
			{Bits: 0x19284a7f, Timestamp: 1781317950}, // 2487499
		},
	},
	{
		Height:       2540000, // 2026-09-16
		Hash:         newHashFromStr("00000000000000070aad634297c7ab0dcf7242b86288f80110c9ca7ab60e38c5"),
		Header:       newHeaderFromStr("00000020fb2ad6b6f861bdd345b82777cbec911bc8cf7524d151f4911f00000000000000343e1eac77001e2b6b42053bd923eceb8c8228394aac3463a30196425faab8364f07ab6a74ba1d194a49da11"),
		FilterHeader: newHashFromStr("c0d5a3ccf9e8e427a0883c39ca3a7f986325b1ea0eeecaaa795bbafc12936492"),
		Difficulty: []netparams.BlockDifficulty{
			{Bits: 0x1930f789, Timestamp: 1789591045}, // 2539977
			{Bits: 0x1936a70b, Timestamp: 1789591110}, // 2539978
			{Bits: 0x193414f2, Timestamp: 1789591159}, // 2539979
			{Bits: 0x193054e6, Timestamp: 1789591218}, // 2539980
			{Bits: 0x192fb392, Timestamp: 1789591354}, // 2539981
			{Bits: 0x192fbcf7, Timestamp: 1789591413}, // 2539982
			{Bits: 0x192dc81f, Timestamp: 1789591481}, // 2539983
			{Bits: 0x192ba462, Timestamp: 1789591602}, // 2539984
			{Bits: 0x1925e80f, Timestamp: 1789591810}, // 2539985
			{Bits: 0x19249c94, Timestamp: 1789592002}, // 2539986
			{Bits: 0x19261144, Timestamp: 1789592004}, // 2539987
			{Bits: 0x19254143, Timestamp: 1789592113}, // 2539988
			{Bits: 0x1925bff4, Timestamp: 1789592247}, // 2539989
			{Bits: 0x1927098f, Timestamp: 1789592254}, // 2539990
			{Bits: 0x1925711b, Timestamp: 1789592334}, // 2539991
			{Bits: 0x19244399, Timestamp: 1789592626}, // 2539992
			{Bits: 0x19261826, Timestamp: 1789592737}, // 2539993
			{Bits: 0x19273671, Timestamp: 1789592793}, // 2539994
			{Bits: 0x192511d1, Timestamp: 1789592825}, // 2539995
			{Bits: 0x1923ac35, Timestamp: 1789593050}, // 2539996
			{Bits: 0x19233927, Timestamp: 1789593084}, // 2539997
			{Bits: 0x1921989a, Timestamp: 1789593121}, // 2539998
			{Bits: 0x191fbd40, Timestamp: 1789593124}, // 2539999
		},
	},
}

// dashTestnetCheckpoints holds the testnet anchor, taken from dashpay/dash
// src/chainparams.cpp (block 1380000 is ~2025-12-10 per its chainTxData
// entry). See dashMainnetCheckpoints.
var dashTestnetCheckpoints = []netparams.Checkpoint{
	{
		Height:       1380000, // 2025-12-10
		Hash:         newHashFromStr("000000a98084beaf77ed26a905a7d59979009e23367a55b5d634962d7d65a1f9"),
		Header:       newHeaderFromStr("00000020bab7a42749cd042fcb7e8ee826a80ba3eaf608a184b4188c8b5d975864000000f73d143c64c09dae7168352d60fecad4907cb70452744384b0fc23c47d931785b4dd38695ace001e349d0e00"),
		FilterHeader: newHashFromStr("bdad9b9ffa75d2d1d432ea3ccc11765f7ccca08da7dfb03cf1ab2eb7f0897d18"),
	},
}

var DashParams = map[string]*netparams.ChainParams{
	"mainnet": {
		Chain:           "dash",
		Name:            "mainnet",
		Net:             0xbd6b0cbf, // pchMessageStart bf 0c 6b bd
		DefaultPort:     "9999",
		KeyPurposes:     []uint32{44}, // legacy P2PKH; Dash has no SegWit
		ElectrumServers: dashMainnetElectrumServers,
		DNSSeeds: []chaincfg.DNSSeed{
			{Host: "dnsseed.dash.org", HasFiltering: false},
		},
		GenesisBlock: &wire.MsgBlock{
			Header: wire.BlockHeader{
				Version:    1,
				PrevBlock:  chainhash.Hash{},
				MerkleRoot: dashGenesisMerkleRoot,
				Timestamp:  time.Unix(1390095618, 0),
				Bits:       0x1e0ffff0,
				Nonce:      28917698,
			},
			Transactions: []*wire.MsgTx{dashGenesisCoinbaseTx},
		},
		GenesisHash:              newHashFromStr("00000ffd590b1485b3caadc19b22e6379c733355108f107a430458cdf3407ab6"),
		PowLimit:                 dashMainPowLimit,
		PowLimitBits:             0x1e0ffff0,
		PoWNoRetargeting:         true,              // DGW retargets per-block; skip Bitcoin interval retarget
		TargetTimespan:           time.Hour * 24,    // Dash: 1 day (vestigial under DGW)
		TargetTimePerBlock:       time.Second * 150, // Dash: 2.5 minutes
		RetargetAdjustmentFactor: 4,
		ReduceMinDifficulty:      false,
		DifficultyLookback:       dashDGWPastBlocks - 1, // DGW at C+1 reads blocks C-23 through C
		Checkpoints:              dashMainnetCheckpoints,
		Bech32HRPSegwit:          "", // Dash has no SegWit
		PubKeyHashAddrID:         0x4c,
		ScriptHashAddrID:         0x10,
		PrivateKeyID:             0xcc,
		HDPrivateKeyID:           [4]byte{0x04, 0x88, 0xad, 0xe4}, // xprv
		HDPublicKeyID:            [4]byte{0x04, 0x88, 0xb2, 0x1e}, // xpub
		HDCoinType:               5,
		BIP0034Height:            951,
		BIP0065Height:            619382,
		BIP0066Height:            245817,
		CheckPoW:                 dashCheckPoW(dashMainPowLimit),
		HeaderHashFunc:           dashHeaderHash,
		CoinbaseMaturity:         100,
		MaxSatoshi:               22e6 * btcutil.SatoshiPerBitcoin,
	},
	"testnet": {
		Chain:       "dash",
		Name:        "testnet3",
		Net:         0xffcae2ce, // pchMessageStart ce e2 ca ff
		DefaultPort: "19999",
		KeyPurposes: []uint32{44}, // legacy P2PKH; Dash has no SegWit
		// No reliable public Dash testnet electrumx server; bring your own.
		DNSSeeds: []chaincfg.DNSSeed{
			{Host: "testnet-seed.dashdot.io", HasFiltering: false},
		},
		GenesisBlock: &wire.MsgBlock{
			Header: wire.BlockHeader{
				Version:    1,
				PrevBlock:  chainhash.Hash{},
				MerkleRoot: dashGenesisMerkleRoot,
				Timestamp:  time.Unix(1390666206, 0),
				Bits:       0x1e0ffff0,
				Nonce:      3861367235,
			},
			Transactions: []*wire.MsgTx{dashGenesisCoinbaseTx},
		},
		GenesisHash:              newHashFromStr("00000bafbc94add76cb75e2ec92894837288a481e5c005f6563d91623bf8bc2c"),
		PowLimit:                 dashMainPowLimit,
		PowLimitBits:             0x1e0ffff0,
		PoWNoRetargeting:         true,
		TargetTimespan:           time.Hour * 24,
		TargetTimePerBlock:       time.Second * 150,
		RetargetAdjustmentFactor: 4,
		ReduceMinDifficulty:      true,
		Checkpoints:              dashTestnetCheckpoints,
		Bech32HRPSegwit:          "",
		PubKeyHashAddrID:         0x8c,
		ScriptHashAddrID:         0x13,
		PrivateKeyID:             0xef,
		HDPrivateKeyID:           [4]byte{0x04, 0x35, 0x83, 0x94}, // tprv
		HDPublicKeyID:            [4]byte{0x04, 0x35, 0x87, 0xcf}, // tpub
		HDCoinType:               1,
		BIP0034Height:            1,
		BIP0065Height:            1,
		BIP0066Height:            1,
		CheckPoW:                 dashCheckPoW(dashMainPowLimit),
		HeaderHashFunc:           dashHeaderHash,
		CoinbaseMaturity:         100,
		MaxSatoshi:               22e6 * btcutil.SatoshiPerBitcoin,
	},
	"simnet": {
		Chain:           "dash",
		Name:            "regtest",
		Net:             0xdcb7c1fc, // pchMessageStart fc c1 b7 dc
		DefaultPort:     "19899",
		KeyPurposes:     []uint32{44}, // legacy P2PKH; Dash has no SegWit
		ElectrumServers: dashSimnetElectrumServers,
		NeutrinoSeeds:   dashSimnetNeutrinoSeeds,
		DNSSeeds:        []chaincfg.DNSSeed{},
		GenesisBlock: &wire.MsgBlock{
			Header: wire.BlockHeader{
				Version:    1,
				PrevBlock:  chainhash.Hash{},
				MerkleRoot: dashGenesisMerkleRoot,
				Timestamp:  time.Unix(1417713337, 0),
				Bits:       0x207fffff,
				Nonce:      1096447,
			},
			Transactions: []*wire.MsgTx{dashGenesisCoinbaseTx},
		},
		GenesisHash:              newHashFromStr("000008ca1832a4baf228eb1553c03d3a2c8e02399550dd6ea8d65cec3ef23d2e"),
		PowLimit:                 dashRegtestPowLimit,
		PowLimitBits:             0x207fffff,
		PoWNoRetargeting:         true,
		TargetTimespan:           time.Hour * 24,
		TargetTimePerBlock:       time.Second * 150,
		RetargetAdjustmentFactor: 4,
		ReduceMinDifficulty:      true,
		Checkpoints:              nil,
		Bech32HRPSegwit:          "",
		PubKeyHashAddrID:         0x8c,
		ScriptHashAddrID:         0x13,
		PrivateKeyID:             0xef,
		HDPrivateKeyID:           [4]byte{0x04, 0x35, 0x83, 0x94}, // tprv
		HDPublicKeyID:            [4]byte{0x04, 0x35, 0x87, 0xcf}, // tpub
		HDCoinType:               1,
		BIP0034Height:            100000000, // not active in regtest
		BIP0065Height:            1351,
		BIP0066Height:            1251,
		CheckPoW:                 dashCheckPoW(dashRegtestPowLimit),
		HeaderHashFunc:           dashHeaderHash,
		CoinbaseMaturity:         100,
		MaxSatoshi:               22e6 * btcutil.SatoshiPerBitcoin,
	},
}
