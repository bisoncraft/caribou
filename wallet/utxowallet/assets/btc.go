package assets

import (
	"math/big"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// genesisMerkleRoot is the hash of the first transaction in the genesis block
// for the main network.
var genesisMerkleRoot = chainhash.Hash([chainhash.HashSize]byte{ // Make go vet happy.
	0x3b, 0xa3, 0xed, 0xfd, 0x7a, 0x7b, 0x12, 0xb2,
	0x7a, 0xc7, 0x2c, 0x3e, 0x67, 0x76, 0x8f, 0x61,
	0x7f, 0xc8, 0x1b, 0xc3, 0x88, 0x8a, 0x51, 0x32,
	0x3a, 0x9f, 0xb8, 0xaa, 0x4b, 0x1e, 0x5e, 0x4a,
})

var BTCParams = map[string]*netparams.ChainParams{
	"mainnet": {
		Chain:       "btc",
		Name:        "mainnet",
		Net:         wire.MainNet,
		DefaultPort: "8333",
		DNSSeeds: []chaincfg.DNSSeed{
			{Host: "seed.bitcoin.sipa.be", HasFiltering: true},
			{Host: "dnsseed.bluematt.me", HasFiltering: true},
			{Host: "dnsseed.bitcoin.dashjr.org", HasFiltering: false},
			{Host: "seed.bitnodes.io", HasFiltering: false},
			{Host: "seed.bitcoin.jonasschnelli.ch", HasFiltering: true},
			{Host: "seed.btc.petertodd.net", HasFiltering: true},
			{Host: "seed.bitcoin.sprovoost.nl", HasFiltering: true},
			{Host: "seed.bitcoin.wiz.biz", HasFiltering: true},
		},

		// Chain parameters
		GenesisBlock: &wire.MsgBlock{
			Header: wire.BlockHeader{
				Version:    1,
				PrevBlock:  chainhash.Hash{},         // 0000000000000000000000000000000000000000000000000000000000000000
				MerkleRoot: genesisMerkleRoot,        // 4a5e1e4baab89f3a32518a88c31bc87f618f76673e2cc77ab2127b7afdeda33b
				Timestamp:  time.Unix(0x495fab29, 0), // 2009-01-03 18:15:05 +0000 UTC
				Bits:       0x1d00ffff,               // 486604799 [00000000ffff0000000000000000000000000000000000000000000000000000]
				Nonce:      0x7c2bac1d,               // 2083236893
			},
			Transactions: []*wire.MsgTx{{
				Version: 1,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: wire.OutPoint{
							Hash:  chainhash.Hash{},
							Index: 0xffffffff,
						},
						SignatureScript: []byte{
							0x04, 0xff, 0xff, 0x00, 0x1d, 0x01, 0x04, 0x45, /* |.......E| */
							0x54, 0x68, 0x65, 0x20, 0x54, 0x69, 0x6d, 0x65, /* |The Time| */
							0x73, 0x20, 0x30, 0x33, 0x2f, 0x4a, 0x61, 0x6e, /* |s 03/Jan| */
							0x2f, 0x32, 0x30, 0x30, 0x39, 0x20, 0x43, 0x68, /* |/2009 Ch| */
							0x61, 0x6e, 0x63, 0x65, 0x6c, 0x6c, 0x6f, 0x72, /* |ancellor| */
							0x20, 0x6f, 0x6e, 0x20, 0x62, 0x72, 0x69, 0x6e, /* | on brin| */
							0x6b, 0x20, 0x6f, 0x66, 0x20, 0x73, 0x65, 0x63, /* |k of sec|*/
							0x6f, 0x6e, 0x64, 0x20, 0x62, 0x61, 0x69, 0x6c, /* |ond bail| */
							0x6f, 0x75, 0x74, 0x20, 0x66, 0x6f, 0x72, 0x20, /* |out for |*/
							0x62, 0x61, 0x6e, 0x6b, 0x73, /* |banks| */
						},
						Sequence: 0xffffffff,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value: 0x12a05f200,
						PkScript: []byte{
							0x41, 0x04, 0x67, 0x8a, 0xfd, 0xb0, 0xfe, 0x55, /* |A.g....U| */
							0x48, 0x27, 0x19, 0x67, 0xf1, 0xa6, 0x71, 0x30, /* |H'.g..q0| */
							0xb7, 0x10, 0x5c, 0xd6, 0xa8, 0x28, 0xe0, 0x39, /* |..\..(.9| */
							0x09, 0xa6, 0x79, 0x62, 0xe0, 0xea, 0x1f, 0x61, /* |..yb...a| */
							0xde, 0xb6, 0x49, 0xf6, 0xbc, 0x3f, 0x4c, 0xef, /* |..I..?L.| */
							0x38, 0xc4, 0xf3, 0x55, 0x04, 0xe5, 0x1e, 0xc1, /* |8..U....| */
							0x12, 0xde, 0x5c, 0x38, 0x4d, 0xf7, 0xba, 0x0b, /* |..\8M...| */
							0x8d, 0x57, 0x8a, 0x4c, 0x70, 0x2b, 0x6b, 0xf1, /* |.W.Lp+k.| */
							0x1d, 0x5f, 0xac, /* |._.| */
						},
					},
				},
				LockTime: 0,
			}},
		},
		GenesisHash: hashPointer([chainhash.HashSize]byte{ // Make go vet happy.
			0x6f, 0xe2, 0x8c, 0x0a, 0xb6, 0xf1, 0xb3, 0x72,
			0xc1, 0xa6, 0xa2, 0x46, 0xae, 0x63, 0xf7, 0x4f,
			0x93, 0x1e, 0x83, 0x65, 0xe1, 0x5a, 0x08, 0x9c,
			0x68, 0xd6, 0x19, 0x00, 0x00, 0x00, 0x00, 0x00,
		}),
		PowLimit:                 new(big.Int).Sub(new(big.Int).Lsh(bigOne, 224), bigOne),
		PowLimitBits:             0x1d00ffff,
		TargetTimespan:           time.Hour * 24 * 14, // 14 days
		TargetTimePerBlock:       time.Minute * 10,    // 10 minutes
		RetargetAdjustmentFactor: 4,                   // 25% less, 400% more
		ReduceMinDifficulty:      false,
		MinDiffReductionTime:     0,

		// Checkpoints ordered from oldest to newest. The first is the launch
		// anchor: the last retarget boundary mined before
		// DefaultWalletBirthday (2026-07-01). The electrum header store
		// starts there (see electrum.NewChainConfig) and no wallet birthday
		// predates it, so it never moves. A checkpoint follows every seven
		// retarget periods (14112 blocks, about three months) and is added
		// once mined; the header store validates each one as it syncs
		// through. Hashes cross-checked via mempool.space and
		// blockstream.info.
		// The livenet TestCheckpointRecords in the spv package fetches each
		// header and filter header from P2P peers, checks the header against
		// the hash, and requires the peers to agree on the filter header.
		Checkpoints: []netparams.Checkpoint{
			{
				Height:       955584, // 2026-06-27
				Hash:         newHashFromStr("00000000000000000001e265c627e0a27ad347deb4d6b921f249eddfbf78e011"),
				Header:       newHeaderFromStr("0020a734ef046b60fe5e17086e51e194ae193f6200f5655d041e000000000000000000007460c5f7eaa219119e04ae728c430e86fc2aaa5e9609069d25f99caf34f3ae53a72e3f6a421a02171a8e62f0"),
				FilterHeader: newHashFromStr("d5343f0a410806c81549c169c31fb8e6b57ca348082aee18c5965a8045d4f102"),
			},
			{
				Height:       969696, // 2026-10-03
				Hash:         newHashFromStr("000000000000000000001e39df127cbab82a824ac43cfcdbf62e59f6a08b9f0b"),
				Header:       newHeaderFromStr("00e00020ea7409145abed3693b6aa5e269dea055406e0962804301000000000000000000a8f40cc468f3aa4518fdc368e37fc7aff01eb12b293885f847517602a96e3a93d4abc06af01e021779d3a409"),
				FilterHeader: newHashFromStr("97e95488a492738d8dd5f99c7ab0cf032823eb5876be6fcf9d73c34517c569ff"),
			},
		},
		Bech32HRPSegwit:         "bc",
		PubKeyHashAddrID:        0x00, // starts with 1
		ScriptHashAddrID:        0x05, // starts with 3
		PrivateKeyID:            0x80, // starts with 5 (uncompressed) or K (compressed)
		WitnessPubKeyHashAddrID: 0x06, // starts with p2
		WitnessScriptHashAddrID: 0x0A, // starts with 7Xh
		// BIP32 hierarchical deterministic extended key magics
		HDPrivateKeyID:   [4]byte{0x04, 0x88, 0xad, 0xe4}, // starts with xprv
		HDPublicKeyID:    [4]byte{0x04, 0x88, 0xb2, 0x1e}, // starts with xpub
		HDCoinType:       0,
		KeyPurposes:      []uint32{84},
		BIP0034Height:    227931, // 000000000000024b89b42a942fe0d9fea3bb44ab7bd1b19115dd6a759c0808b8
		BIP0065Height:    388381, // 000000000000000004c2b624ed5d7756c508d90fd0da2c7c679febfa6c4735f0
		BIP0066Height:    363725, // 00000000000000000379eaa19dce8c9b722d46ae6a57c2f1a988119488b50931
		CoinbaseMaturity: 100,
		MaxSatoshi:       btcutil.MaxSatoshi,
		ElectrumServers:  btcMainnetElectrumServers,
	},
	"testnet": {
		Chain:       "btc",
		Name:        "testnet3",
		Net:         wire.TestNet3,
		DefaultPort: "18333",
		DNSSeeds: []chaincfg.DNSSeed{
			{Host: "testnet-seed.bitcoin.jonasschnelli.ch", HasFiltering: true},
			{Host: "seed.tbtc.petertodd.net", HasFiltering: true},
			{Host: "seed.testnet.bitcoin.sprovoost.nl", HasFiltering: true},
			{Host: "testnet-seed.bluematt.me", HasFiltering: false},
		},
		GenesisBlock: &wire.MsgBlock{
			Header: wire.BlockHeader{
				Version:    1,
				PrevBlock:  chainhash.Hash{},         // 0000000000000000000000000000000000000000000000000000000000000000
				MerkleRoot: genesisMerkleRoot,        // 4a5e1e4baab89f3a32518a88c31bc87f618f76673e2cc77ab2127b7afdeda33b
				Timestamp:  time.Unix(1296688602, 0), // 2011-02-02 23:16:42 +0000 UTC
				Bits:       0x1d00ffff,               // 486604799 [00000000ffff0000000000000000000000000000000000000000000000000000]
				Nonce:      0x18aea41a,               // 414098458
			},
			Transactions: []*wire.MsgTx{{
				Version: 1,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: wire.OutPoint{
							Hash:  chainhash.Hash{},
							Index: 0xffffffff,
						},
						SignatureScript: []byte{
							0x04, 0xff, 0xff, 0x00, 0x1d, 0x01, 0x04, 0x45, /* |.......E| */
							0x54, 0x68, 0x65, 0x20, 0x54, 0x69, 0x6d, 0x65, /* |The Time| */
							0x73, 0x20, 0x30, 0x33, 0x2f, 0x4a, 0x61, 0x6e, /* |s 03/Jan| */
							0x2f, 0x32, 0x30, 0x30, 0x39, 0x20, 0x43, 0x68, /* |/2009 Ch| */
							0x61, 0x6e, 0x63, 0x65, 0x6c, 0x6c, 0x6f, 0x72, /* |ancellor| */
							0x20, 0x6f, 0x6e, 0x20, 0x62, 0x72, 0x69, 0x6e, /* | on brin| */
							0x6b, 0x20, 0x6f, 0x66, 0x20, 0x73, 0x65, 0x63, /* |k of sec|*/
							0x6f, 0x6e, 0x64, 0x20, 0x62, 0x61, 0x69, 0x6c, /* |ond bail| */
							0x6f, 0x75, 0x74, 0x20, 0x66, 0x6f, 0x72, 0x20, /* |out for |*/
							0x62, 0x61, 0x6e, 0x6b, 0x73, /* |banks| */
						},
						Sequence: 0xffffffff,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value: 0x12a05f200,
						PkScript: []byte{
							0x41, 0x04, 0x67, 0x8a, 0xfd, 0xb0, 0xfe, 0x55, /* |A.g....U| */
							0x48, 0x27, 0x19, 0x67, 0xf1, 0xa6, 0x71, 0x30, /* |H'.g..q0| */
							0xb7, 0x10, 0x5c, 0xd6, 0xa8, 0x28, 0xe0, 0x39, /* |..\..(.9| */
							0x09, 0xa6, 0x79, 0x62, 0xe0, 0xea, 0x1f, 0x61, /* |..yb...a| */
							0xde, 0xb6, 0x49, 0xf6, 0xbc, 0x3f, 0x4c, 0xef, /* |..I..?L.| */
							0x38, 0xc4, 0xf3, 0x55, 0x04, 0xe5, 0x1e, 0xc1, /* |8..U....| */
							0x12, 0xde, 0x5c, 0x38, 0x4d, 0xf7, 0xba, 0x0b, /* |..\8M...| */
							0x8d, 0x57, 0x8a, 0x4c, 0x70, 0x2b, 0x6b, 0xf1, /* |.W.Lp+k.| */
							0x1d, 0x5f, 0xac, /* |._.| */
						},
					},
				},
				LockTime: 0,
			}},
		},
		GenesisHash: hashPointer([chainhash.HashSize]byte{ // Make go vet happy.
			0x43, 0x49, 0x7f, 0xd7, 0xf8, 0x26, 0x95, 0x71,
			0x08, 0xf4, 0xa3, 0x0f, 0xd9, 0xce, 0xc3, 0xae,
			0xba, 0x79, 0x97, 0x20, 0x84, 0xe9, 0x0e, 0xad,
			0x01, 0xea, 0x33, 0x09, 0x00, 0x00, 0x00, 0x00,
		}),
		PowLimit:                 new(big.Int).Sub(new(big.Int).Lsh(bigOne, 224), bigOne),
		PowLimitBits:             0x1d00ffff,
		TargetTimespan:           time.Hour * 24 * 14, // 14 days
		TargetTimePerBlock:       time.Minute * 10,    // 10 minutes
		RetargetAdjustmentFactor: 4,                   // 25% less, 400% more
		ReduceMinDifficulty:      true,
		MinDiffReductionTime:     time.Minute * 20, // TargetTimePerBlock * 2

		// Checkpoints ordered from oldest to newest.
		// See the mainnet Checkpoints comment: the last retarget boundary
		// mined before DefaultWalletBirthday. Cross-checked via
		// mempool.space and blockstream.info.
		Checkpoints: []netparams.Checkpoint{
			{
				Height:       5013792, // 2026-06-26
				Hash:         newHashFromStr("00000000000001eb9a9e968ba73b5d2f2e559e8ec7053f5106b222554c035977"),
				Header:       newHeaderFromStr("00000020a7e352120c16ae494d54d576e975a3eae2a41edc84f33ad10b0300000000000048bfd45ad53e9c5bdcadd91557eea32ea29dd5a1a6f8cd29c22aa06608984bde95c93e6a49fc021a267df014"),
				FilterHeader: newHashFromStr("693712ac49089e3c2e0df42f42bac0647ce290a2974a0ef43813af5821973182"),
			},
		},
		Bech32HRPSegwit: "tb", // always tb for test net

		// Address encoding magics
		PubKeyHashAddrID:        0x6f,                            // starts with m or n
		ScriptHashAddrID:        0xc4,                            // starts with 2
		WitnessPubKeyHashAddrID: 0x03,                            // starts with QW
		WitnessScriptHashAddrID: 0x28,                            // starts with T7n
		PrivateKeyID:            0xef,                            // starts with 9 (uncompressed) or c (compressed)
		HDPrivateKeyID:          [4]byte{0x04, 0x35, 0x83, 0x94}, // starts with tprv
		HDPublicKeyID:           [4]byte{0x04, 0x35, 0x87, 0xcf}, // starts with tpub
		// BIP-44 says HDCoinType should be 1 for testnet, but the wallet- and
		// wtxmgr-package unit tests load wallets via this entry and reference
		// fixed Coin=0 key-scope constants (defined in the test packages)
		// directly, so the in-tree scope lookups only resolve when the wallet is
		// also rooted at Coin=0. Production BTC consumers (wallet/asset/btc) read
		// chain params from btcd's chaincfg, not from this map, and route
		// scope lookups through wallet.ScopeForPurpose which honors the real
		// HDCoinType — see DERIVATION.md. Bump this to 1 once the affected
		// tests are migrated off the Coin=0 key-scope constants.
		HDCoinType:       0,
		KeyPurposes:      []uint32{84},
		BIP0034Height:    21111,  // 0000000023b3a96d3484e5abb3755c413e7d41500f8e2a5c3f0dd01299cd8ef8
		BIP0065Height:    581885, // 00000000007f6655f22f98e72ed80d8b06dc761d5da09df0fa1dc4be4f861eb6
		BIP0066Height:    330776, // 000000002104c8c45e99a8853285a3b592602a3ccde2b832481da85e9e4ba182
		CoinbaseMaturity: 100,
		MaxSatoshi:       btcutil.MaxSatoshi,
		ElectrumServers:  btcTestnetElectrumServers,
		NeutrinoSeeds:    btcTestnetNeutrinoSeeds,
	},
	"simnet": {
		Chain:       "btc",
		Name:        "regtest",
		Net:         wire.TestNet,
		DefaultPort: "20575", // 18444 // Changed to match dcrdex testing harness
		DNSSeeds:    []chaincfg.DNSSeed{},
		GenesisBlock: &wire.MsgBlock{
			Header: wire.BlockHeader{
				Version:    1,
				PrevBlock:  chainhash.Hash{},         // 0000000000000000000000000000000000000000000000000000000000000000
				MerkleRoot: genesisMerkleRoot,        // 4a5e1e4baab89f3a32518a88c31bc87f618f76673e2cc77ab2127b7afdeda33b
				Timestamp:  time.Unix(1296688602, 0), // 2011-02-02 23:16:42 +0000 UTC
				Bits:       0x207fffff,               // 545259519 [7fffff0000000000000000000000000000000000000000000000000000000000]
				Nonce:      2,
			},
			Transactions: []*wire.MsgTx{{
				Version: 1,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: wire.OutPoint{
							Hash:  chainhash.Hash{},
							Index: 0xffffffff,
						},
						SignatureScript: []byte{
							0x04, 0xff, 0xff, 0x00, 0x1d, 0x01, 0x04, 0x45, /* |.......E| */
							0x54, 0x68, 0x65, 0x20, 0x54, 0x69, 0x6d, 0x65, /* |The Time| */
							0x73, 0x20, 0x30, 0x33, 0x2f, 0x4a, 0x61, 0x6e, /* |s 03/Jan| */
							0x2f, 0x32, 0x30, 0x30, 0x39, 0x20, 0x43, 0x68, /* |/2009 Ch| */
							0x61, 0x6e, 0x63, 0x65, 0x6c, 0x6c, 0x6f, 0x72, /* |ancellor| */
							0x20, 0x6f, 0x6e, 0x20, 0x62, 0x72, 0x69, 0x6e, /* | on brin| */
							0x6b, 0x20, 0x6f, 0x66, 0x20, 0x73, 0x65, 0x63, /* |k of sec|*/
							0x6f, 0x6e, 0x64, 0x20, 0x62, 0x61, 0x69, 0x6c, /* |ond bail| */
							0x6f, 0x75, 0x74, 0x20, 0x66, 0x6f, 0x72, 0x20, /* |out for |*/
							0x62, 0x61, 0x6e, 0x6b, 0x73, /* |banks| */
						},
						Sequence: 0xffffffff,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value: 0x12a05f200,
						PkScript: []byte{
							0x41, 0x04, 0x67, 0x8a, 0xfd, 0xb0, 0xfe, 0x55, /* |A.g....U| */
							0x48, 0x27, 0x19, 0x67, 0xf1, 0xa6, 0x71, 0x30, /* |H'.g..q0| */
							0xb7, 0x10, 0x5c, 0xd6, 0xa8, 0x28, 0xe0, 0x39, /* |..\..(.9| */
							0x09, 0xa6, 0x79, 0x62, 0xe0, 0xea, 0x1f, 0x61, /* |..yb...a| */
							0xde, 0xb6, 0x49, 0xf6, 0xbc, 0x3f, 0x4c, 0xef, /* |..I..?L.| */
							0x38, 0xc4, 0xf3, 0x55, 0x04, 0xe5, 0x1e, 0xc1, /* |8..U....| */
							0x12, 0xde, 0x5c, 0x38, 0x4d, 0xf7, 0xba, 0x0b, /* |..\8M...| */
							0x8d, 0x57, 0x8a, 0x4c, 0x70, 0x2b, 0x6b, 0xf1, /* |.W.Lp+k.| */
							0x1d, 0x5f, 0xac, /* |._.| */
						},
					},
				},
				LockTime: 0,
			}},
		},
		GenesisHash: hashPointer([chainhash.HashSize]byte{ // Make go vet happy.
			0x06, 0x22, 0x6e, 0x46, 0x11, 0x1a, 0x0b, 0x59,
			0xca, 0xaf, 0x12, 0x60, 0x43, 0xeb, 0x5b, 0xbf,
			0x28, 0xc3, 0x4f, 0x3a, 0x5e, 0x33, 0x2a, 0x1f,
			0xc7, 0xb2, 0xb7, 0x3c, 0xf1, 0x88, 0x91, 0x0f,
		}),
		PowLimit:                 new(big.Int).Sub(new(big.Int).Lsh(bigOne, 255), bigOne),
		PowLimitBits:             0x207fffff,
		PoWNoRetargeting:         true,
		TargetTimespan:           time.Hour * 24 * 14, // 14 days
		TargetTimePerBlock:       time.Minute * 10,    // 10 minutes
		RetargetAdjustmentFactor: 4,                   // 25% less, 400% more
		ReduceMinDifficulty:      true,
		MinDiffReductionTime:     time.Minute * 20, // TargetTimePerBlock * 2
		Checkpoints:              nil,
		Bech32HRPSegwit:          "bcrt",                          // always bcrt for reg test net
		PubKeyHashAddrID:         0x6f,                            // starts with m or n
		ScriptHashAddrID:         0xc4,                            // starts with 2
		PrivateKeyID:             0xef,                            // starts with 9 (uncompressed) or c (compressed)
		HDPrivateKeyID:           [4]byte{0x04, 0x35, 0x83, 0x94}, // starts with tprv
		HDPublicKeyID:            [4]byte{0x04, 0x35, 0x87, 0xcf}, // starts with tpub
		// See the testnet entry above for why this is 0 rather than the
		// BIP-44 standard 1. Same rationale: in-tree tests use fixed Coin=0
		// key-scope constants.
		HDCoinType:       0,
		KeyPurposes:      []uint32{84},
		BIP0034Height:    100000000, // Not active - Permit ver 1 blocks
		BIP0065Height:    1351,      // Used by regression tests
		BIP0066Height:    1251,      // Used by regression tests
		CoinbaseMaturity: 100,
		MaxSatoshi:       btcutil.MaxSatoshi,
		ElectrumServers:  btcSimnetElectrumServers,
		NeutrinoSeeds:    btcSimnetNeutrinoSeeds,
	},
}

// Shipped certificate pins for the BTC mainnet servers whose certificates are
// self-signed. A connection must present the key of the pinned certificate
// until that certificate expires; after that the server's key is learned on
// first use like any other self-signed mainnet server (electrum/certpin.go).
// Each was fetched with a TLS handshake on 2026-11-17 and pinned only with at
// least two years of validity left. A server that replaces its key before the
// pinned certificate expires is refused until the pin is refreshed; the
// livenet TestShippedCertPins (go test -tags livenet ./wallet/utxowallet/assets/)
// reports a changed certificate or one expiring within a year. Re-fetch with:
//
//	openssl s_client -connect host:port -servername host </dev/null | openssl x509

// btcEmzyCertPEM is the TLS leaf certificate of electrum.emzy.de:50002,
// self-signed and valid until 2031-10-07.
const btcEmzyCertPEM = `-----BEGIN CERTIFICATE-----
MIIFcjCCA1qgAwIBAgIUCeKUxFVNM7rMVEOsubzwr/DoqZ4wDQYJKoZIhvcNAQEL
BQAwKDELMAkGA1UEBhMCREUxGTAXBgNVBAMMEGVsZWN0cnVtLmVtenkuZGUwHhcN
MjYxMDA4MTkzMzU0WhcNMzExMDA3MTkzMzU0WjAoMQswCQYDVQQGEwJERTEZMBcG
A1UEAwwQZWxlY3RydW0uZW16eS5kZTCCAiIwDQYJKoZIhvcNAQEBBQADggIPADCC
AgoCggIBAOivRoQrdPR0TF4gmPGGk2Y8FKSftqLVEb6XNMCDhg/rr6Y+o5OEX5sf
5N7BAviahmrQRQcvrOYv9vxOOzskzRDPzWJZaDLGUaxxEl2MrnGb+8SM+8vj0t60
nsITjG4R/+TAifGYPm51HFPnzIvOWdNGtkui7T/h4BwhQdrjjJXlXePa0ewUcWnj
q/IjLzsTyAf8p8XEer3Y3D/ajyEH/CciF73QqS56TMmu49f6cbBC4jLk6FL01Lfh
vpejLEtFoUwR3AyP0Je70yFoBF50o4pt2GEPmy6HRJ7xnw5bHtaxWWTm47guP/nb
7VFxAHGPpUaKnkec3EmAUdMw7IML21cOSsICfZdwyFHawbXhPMUtgFZe/TeWJ+Dj
1Pp3nqWDE32vUFQ7uBdIn17eVhL9xXXno5ubQQ5dGkjU7we3N0DPPXIAjYRbTW3g
CCRUJyJBm6RzFOmP8GItk9UNMix9qSzHtj04rRZ89pat/xCU/EpRjMtAedtzik9n
CLjaM+ztjdPkulw32g/pA9qAQH1nRnP0bu8G3rwWxtFCnBDhGHS/4ufIFadvivKj
xBv30cSmqfJ/uRf9Whz9daQhwTBGoif7N/puBJtgY7d4Km1/Fx9ieQUXuLmieXvv
ZanJzzBmKIAi3bwlJTJjBDAbb/DcDhXGIZidBq/iGlL5Bd5nXCi7AgMBAAGjgZMw
gZAwHQYDVR0OBBYEFHLcuR7075ym2bkhxSlo8N0OlmklMB8GA1UdIwQYMBaAFHLc
uR7075ym2bkhxSlo8N0OlmklMBsGA1UdEQQUMBKCEGVsZWN0cnVtLmVtenkuZGUw
DAYDVR0TAQH/BAIwADAOBgNVHQ8BAf8EBAMCBaAwEwYDVR0lBAwwCgYIKwYBBQUH
AwEwDQYJKoZIhvcNAQELBQADggIBADhEjN2jetzj1IaHpQTf+z0g7/7iOTNpTRDw
oJJT0x8+J3QQ4ucH5oWp5KufbvLSTeXqU8vrfjLoYTmKuBO1Gyc/5V58NNpgRsHm
JXeNmUonTx/Q/BRoU7cOK9SKVwPMbrNQeq+D7aqLOfljh1L2R/B5jrtO7zzCV1GV
Y42klUXbfewUm6Cmb5v/arpgF0YfPOpjnqGAzU+pmAKG2d2nPY39vwVgZN5jR/NE
Ht+Yi/57IcUYCrkovz2c6dQ8ugnO6k+1S9IR5darescqnpWXUH0s1M0ef0b5Xcng
rdm+NZrnPzLiBb+bRaGGRIbKm6PX13UnNVbQw28owyELJ2t6u/XcjFfpbA5odteF
g76/w0a60YYZrgVcHSajMa+BRKHDKNl3aNzmkfO9k32FvbqMBjEmzzfN4Elf3Ap8
OXJaACvRTkAumOJBlfMp7qJXKWaM31qWMLVuaAoNjUEtF16RCFdD9fEQz5SgUwDr
TieB1uDh4kT4pM+H/ol//SsPus9HObMqKIIcgbgrZyAAUBlcA7XqX86SnBXUkCJZ
aAb4ziqs2A+2BLDg7aWMYCqUK4G9zvZ2hkgvHo4QN6HV3B/Jf5Sxp73HKKL3wuB9
SSlJE/7Mm/ppnV04ZuPanrodzgHOX4lwwsdONbmfBknvWq5t4eNZLsHit3yR44Kl
V37IuTZo
-----END CERTIFICATE-----`

// btcBitarooCertPEM is the TLS leaf certificate of electrum.bitaroo.net:50002,
// self-signed and valid until 2035-12-04.
const btcBitarooCertPEM = `-----BEGIN CERTIFICATE-----
MIIDazCCAlOgAwIBAgIUE7zxJ2kri1w1Frn3Iq2U/PN6c+IwDQYJKoZIhvcNAQEL
BQAwRTELMAkGA1UEBhMCQVUxEzARBgNVBAgMClNvbWUtU3RhdGUxITAfBgNVBAoM
GEludGVybmV0IFdpZGdpdHMgUHR5IEx0ZDAeFw0yNTEyMDYxNDE4NDVaFw0zNTEy
MDQxNDE4NDVaMEUxCzAJBgNVBAYTAkFVMRMwEQYDVQQIDApTb21lLVN0YXRlMSEw
HwYDVQQKDBhJbnRlcm5ldCBXaWRnaXRzIFB0eSBMdGQwggEiMA0GCSqGSIb3DQEB
AQUAA4IBDwAwggEKAoIBAQDi8qS/GfccDdB+lDm0NkrJ9JV4jq3NzxYMXvwVY4ug
H6jgu/TSYhhy59mWOl/PYldKZisMkbXURe1d7dbo/JA+8dQxp/smRmPDSJp618ww
1U5pck5MimpMDS4h7CC+eskwX8XZK5rs/FV4+QLXI3TxBD+ON9V4JfvRdPgeILw2
G7fxoR6sxePtwlP4cxt98B6b1ajmp3zE5GQB65qzaBGFQwk2o9WmrpoBn6EzWhGg
jbdoRoukzsrbGIE7F1VwXS7EUtRuyg3hFRDVru4SU6iaRfTobL9P5posnI9RZSdm
q/u1FGWfhxMfEGtHJ3OeQQ6x1C9g6+BaFEE48XhmgD+5AgMBAAGjUzBRMB0GA1Ud
DgQWBBTNC6HutEzs8LvsLYFwwD720JVxMzAfBgNVHSMEGDAWgBTNC6HutEzs8Lvs
LYFwwD720JVxMzAPBgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3DQEBCwUAA4IBAQDB
XX5LRzcCCtqIFjD/PZ3dEYnNo8RsFoKdJGOI0YRmko1oHoaETV94GMGjZAmMYClP
8Y4x+irMb3eVAALoGGFrF7gCHnfGK9/gGz8nPL55jQpcsdtg7Ki9+aFjqjsSJKQf
+IkqP65P3of9fl0Esmic1XG/DigLiKtDZYoggX+qOj/RV4kBNuqZCfQlHjWsME6C
b/waPadr8QTwkPoAP4Aif5w/kTUXSmKkqLq+lVrbG4CBAKsBdsCzkZdYtoVShNqs
/VeQm3cWomGWBR6OQ0Gh/qv2dULQ4BnXiZxLk+I82a9BI/lgYOQKkj9PyFLNP/NO
pYi+voNN0TLJTjZ6MqZR
-----END CERTIFICATE-----`

// btcLukeCertPEM is the TLS leaf certificate of bitcoin.lu.ke:50002,
// self-signed and valid until 2031-03-29.
const btcLukeCertPEM = `-----BEGIN CERTIFICATE-----
MIIDCTCCAfGgAwIBAgIUFbmdyrEx2XfcL4sYEa8e7srYuI0wDQYJKoZIhvcNAQEL
BQAwFDESMBAGA1UECgwJRWxlY3RydW1YMB4XDTIxMDMzMTE2MTMzNloXDTMxMDMy
OTE2MTMzNlowFDESMBAGA1UECgwJRWxlY3RydW1YMIIBIjANBgkqhkiG9w0BAQEF
AAOCAQ8AMIIBCgKCAQEAtAsmZqxtPc9zd5ITHPyexu6P3ZXF+xLEyvlYUV9iirBg
SRW2adB/CH57orjnzrlL/kdIKYjkVZ0aVbIzVDQ+uGFUUYsOozEOYINO6O2ZekPa
j3Z+mXdlrH/DW+vQVzVp20goVcXDUcGBTaUDuqrmzrgXwDgWsoa4vtesS3gXj201
xI+aUkzWcYgNg+060+PpnVYTTmRGWm5AFNh2Q3BqRlAfEHKdmumlhf+Gmq6OpJyB
JrZOX0v7HEU3QYYwwgWSvdhUNqCUcZ7+JDeLg5jDIVrQvgfqo9bV5ATLpvWiTOoW
n8zE9XhjISLwsywk7ECAx2ytGJBpwEEQyV4nIeyGPwIDAQABo1MwUTAdBgNVHQ4E
FgQU61kXeO/4CudZUUYpY1HtsFddxDQwHwYDVR0jBBgwFoAU61kXeO/4CudZUUYp
Y1HtsFddxDQwDwYDVR0TAQH/BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOCAQEAlOtZ
jP6M8sTe0CzxvtJl1+ovE6O2CQ94TgABbOfw53MLHCwi6u4ew1SgTcoRfccQcdkC
OKmuN4MmyvrbodSq1rw+z2qaUvU5ns4r2XWjA5IkdMZLD4x1IRI8PXgckVAi1EYA
HWrY5639dvixs5n87uwP9UNDQPn04b041sdo6SJQmwho9YEJzCJueDg+oWXr0/+A
SCnOmafY3in8laYutDFIPp37OtHzRkJbBnXNTb4YeE0zmwdP/FKWJFzx/jZweBw9
0C88SZmLDHSs0cFTeyGzY1pS7fAfRMdfm+VayOFt73OuOJXtWcHUuBmrBd5vBq5W
0xaV2OXs82CRYMjghA==
-----END CERTIFICATE-----`

// btcQtornadoCertPEM is the TLS leaf certificate of fortress.qtornado.com:443,
// self-signed and valid until 2029-04-06.
const btcQtornadoCertPEM = `-----BEGIN CERTIFICATE-----
MIIFBjCCAu4CCQC7D90yRDNezjANBgkqhkiG9w0BAQsFADBFMQswCQYDVQQGEwJB
VTETMBEGA1UECAwKU29tZS1TdGF0ZTEhMB8GA1UECgwYSW50ZXJuZXQgV2lkZ2l0
cyBQdHkgTHRkMB4XDTE5MDQwOTE0NDI1MVoXDTI5MDQwNjE0NDI1MVowRTELMAkG
A1UEBhMCQVUxEzARBgNVBAgMClNvbWUtU3RhdGUxITAfBgNVBAoMGEludGVybmV0
IFdpZGdpdHMgUHR5IEx0ZDCCAiIwDQYJKoZIhvcNAQEBBQADggIPADCCAgoCggIB
AMu8SPoVVN61EgMjDF/DiC3QrOZuQM9a8xS//DE0pwgVJ12PmI5jfLxYPJLXP3m8
Ep48Qn4414zz3H9Kkktl2lmdzO5wJc2C6WQk7hgVhNmW0N9EM14jM4d1HLpuBCVH
hjkxA7YrHQq7944w7Pz/elIEfDDANXyaiKcQCp2xI+SPRV/beHhVYWJemospCVBa
Vd2lGYg7Kh/L+TIlwlhwHBBsdZ382P6LOcWGJv8NPlSYsl+hzdAFYgNsxdcJ1mRU
asAlqX32mQAD0HWGifmxpc0dj0J3nM8NNRHycDIG/EHzMtxm2RYGu4E+TXGRsxAM
7S/GOxM4bY3C5LL4ZuUwe4mW7zZQk5sr+hf45vdLGWQMGLIPvQ85eXIiUyooEJzG
z5Q2jokVCl5HcNdeENnpjlysbO+HwZdoP1zTvtybey0g6xxDd/MoKx0TsT2wkyZl
GsENvSlAQ1C5Z7FoM2/TYrljHWjyU1z1EEhGHLsdY5FRA9ZebwDXbNG04DN34qW9
TIbxW9hmCYlSZMLt+Nn4hgAxqU884zKobMXr6YfM4e9jHWschwi5b5OTrMExKBjX
aOmRUQaZlgyRfczJ1wjxDL5Nj0vEUZRAjzTkcUg7Lu/bl1ZtPlZoS7IXCzhBG/va
oS4ndOt2C2a8BKmSbIW5vRnHPRXSO0lD/OfkUfV58i/XAgMBAAEwDQYJKoZIhvcN
AQELBQADggIBAIqnBIUlurrDR5nqKm1DfKx5LicyGATsJGsVBQvvHoQIGb1ZASqm
9vdiyrPWjFTXPVu/OY1/+m7PD/VDYc4O4ukRu3woRkSj0Ol39mamr4TpQj0RXzC1
araTFdEMLK36w7fxJuBg4Hdgrx5n5bM1UOBeC213Gwv+AeXJ14rt+2qNDyeySJFM
yH0RcnvutLApsJxS49pJbYxs0ojKNat8TMwVdqE+ab4bJJOn71Ykkn8sGIiI8a3W
9bagA8naGPBX7mXN+qy11jY79sBU5SZldnP/LafqJC6UkwgydowrL+opfL5szP6Z
QaPrmkHb1Pqtj+vq5R6PvahQZn3oUOGJQRbN5BSFdfvzMJ9c7Oh18Cj++GodEgoE
S0BQmjbM5T0L8XOb6XTBHVkjL2BazZemIVvSjYitmjU/QanQKiQ/+42upPnBwzt2
FNbKH4H0FJBsXgxId1Rc6qaX3ciXvlTAY6csVZfZr7gDr1oy3UxMKjHJj8gxXvC6
wVLXUQ4lawuyyocwygE03Gay0ZGw1eZP2eqQp19d93I8VFreBy5JHmYKypwVw13K
8YoTb6ll1kHGuMCTmXuk88fd3hi4g+kk/vXOvVoaQe16WMiDANLoWddp8fbGQ6MG
U9L96HJGerT2SOjDTpXO00S2+aVq4X3dbYLLjstA3eJXii1QXGwHPJKj
-----END CERTIFICATE-----`

// DER forms of the shipped BTC pins, decoded at init; a malformed constant
// panics at startup.
var (
	btcEmzyCert     = netparams.MustParseCertPEM(btcEmzyCertPEM)
	btcBitarooCert  = netparams.MustParseCertPEM(btcBitarooCertPEM)
	btcLukeCert     = netparams.MustParseCertPEM(btcLukeCertPEM)
	btcQtornadoCert = netparams.MustParseCertPEM(btcQtornadoCertPEM)
)

// btcMainnetElectrumServers are the built-in electrumx servers for BTC
// mainnet, in preference order; the network layer elects one and rotates
// past failures (electrum/serverpool.go). Operators are deliberately
// diverse. Most public electrum servers present self-signed certificates
// (the electrum ecosystem's norm); those are pinned to their shipped
// certificates above. Blockstream's uses a publicly CA-issued certificate
// verified against the system roots, since CA renewals can change the key.
// Liveness and genesis verified 2026-07-09 and again with the certificate
// fetch on 2026-11-17; blockstream was unreachable on the first date and is
// kept as a trailing candidate.
var btcMainnetElectrumServers = []*netparams.ServerAddr{
	{Proto: "ssl", Addr: "electrum.emzy.de:50002", Cert: btcEmzyCert},
	{Proto: "ssl", Addr: "electrum.bitaroo.net:50002", Cert: btcBitarooCert},
	{Proto: "ssl", Addr: "bitcoin.lu.ke:50002", Cert: btcLukeCert},
	{Proto: "ssl", Addr: "fortress.qtornado.com:443", Cert: btcQtornadoCert},
	{Proto: "ssl", Addr: "electrum.blockstream.info:700"},
}

// btcTestnetElectrumServers are the built-in electrumx servers for BTC
// testnet3; see the mainnet comment. blackie.c3-soft.com presents a
// CA-issued certificate.
var btcTestnetElectrumServers = []*netparams.ServerAddr{
	{Proto: "ssl", Addr: "testnet.aranguren.org:51002", SkipVerify: true},
	{Proto: "ssl", Addr: "blackie.c3-soft.com:57006"},
	{Proto: "ssl", Addr: "testnet.qtornado.com:51002", SkipVerify: true},
	{Proto: "ssl", Addr: "electrum.blockstream.info:993", SkipVerify: true},
}

// btcSimnetElectrumServers is the built-in electrumx server for BTC
// regtest/simnet — points at the dextest harness (BTC port 54002, ssl).
var btcSimnetElectrumServers = []*netparams.ServerAddr{
	{Proto: "ssl", Addr: "127.0.0.1:54002", SkipVerify: true},
}

// btcTestnetNeutrinoSeeds are the default neutrino peers for BTC testnet.
var btcTestnetNeutrinoSeeds = []string{"dex-test.ssgen.io:18333"}

// btcSimnetNeutrinoSeeds are the default neutrino peers for BTC simnet/regtest.
var btcSimnetNeutrinoSeeds = []string{"127.0.0.1:20575"}
