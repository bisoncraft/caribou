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
//     Bitcoin's 2016-block schedule, so the Bitcoin-style interval retarget
//     validation in the electrum header path is inapplicable. We set
//     PoWNoRetargeting on every network to skip it (the same posture the
//     neutrino path already takes, where BFFastAdd skips retarget validation).
//     The per-header check therefore carries the difficulty validation: the
//     CheckPoW hook bounds the claimed target by PowLimit in addition to
//     checking the X11 hash against it, and the static Checkpoints (from
//     dashpay/dash chainparams.cpp) pin the historical chain that headers-sync
//     must reproduce.
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
// against its claimed target AND bounds the claimed target itself by powLimit.
// Bounding Bits matters more for Dash than for BTC/LTC: with PoWNoRetargeting
// set (DGW retargets per-block, so interval retarget validation is
// inapplicable), nothing else on the electrum header path constrains the
// difficulty a server may claim, so an unbounded hook would accept headers at
// any self-declared target. The neutrino path independently gets the same
// bound from blockchain.CheckBlockHeaderSanity, whose target checks run even
// under BFNoPoWCheck.
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
// With interval retarget validation disabled (PoWNoRetargeting), they pin the
// chain during headers-sync on both the electrum and neutrino paths. The first
// is the launch anchor (block 2487500, ~2026-06-13, taken from dashpay/dash
// src/chainparams.cpp): the electrum header store starts there and no wallet
// birthday predates it, so it never moves. Dash retargets every block, so
// there is no retarget period to align to; a checkpoint follows every 52500
// blocks (about three months) and is added once mined. Hashes cross-checked
// via insight.dash.org and blockchair.com.
var dashMainnetCheckpoints = []chaincfg.Checkpoint{
	{Height: 2487500, Hash: newHashFromStr("00000000000000119fe42827219e0686d3f7b494ae65f823194c740c5dbab492")},
	{Height: 2540000, Hash: newHashFromStr("00000000000000070aad634297c7ab0dcf7242b86288f80110c9ca7ab60e38c5")}, // 2026-09-16
}

// dashTestnetCheckpoints holds the testnet anchor, taken from dashpay/dash
// src/chainparams.cpp (block 1380000 is ~2025-12-10 per its chainTxData
// entry). See dashMainnetCheckpoints.
var dashTestnetCheckpoints = []chaincfg.Checkpoint{
	{Height: 1380000, Hash: newHashFromStr("000000a98084beaf77ed26a905a7d59979009e23367a55b5d634962d7d65a1f9")},
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
