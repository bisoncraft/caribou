package assets

import (
	"bytes"
	"fmt"
	"math/big"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"golang.org/x/crypto/scrypt"
)

var (
	ltcMainPowLimit, _   = new(big.Int).SetString("0x0fffff000000000000000000000000000000000000000000000000000000", 0)
	ltcGenesisCoinbaseTx = wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: 0xffffffff,
				},
				SignatureScript: []byte{
					0x04, 0xff, 0xff, 0x00, 0x1d, 0x01, 0x04, 0x40, 0x4e, 0x59, 0x20, 0x54, 0x69, 0x6d, 0x65, 0x73, // |.......@NY Times|
					0x20, 0x30, 0x35, 0x2f, 0x4f, 0x63, 0x74, 0x2f, 0x32, 0x30, 0x31, 0x31, 0x20, 0x53, 0x74, 0x65, // | 05/Oct/2011 Ste|
					0x76, 0x65, 0x20, 0x4a, 0x6f, 0x62, 0x73, 0x2c, 0x20, 0x41, 0x70, 0x70, 0x6c, 0x65, 0xe2, 0x80, // |ve Jobs, Apple..|
					0x99, 0x73, 0x20, 0x56, 0x69, 0x73, 0x69, 0x6f, 0x6e, 0x61, 0x72, 0x79, 0x2c, 0x20, 0x44, 0x69, // |.s Visionary, Di|
					0x65, 0x73, 0x20, 0x61, 0x74, 0x20, 0x35, 0x36, // |es at 56|

				},
				Sequence: 0xffffffff,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value: 0x12a05f200,
				PkScript: []byte{
					0x41, 0x4, 0x1, 0x84, 0x71, 0xf, 0xa6, 0x89,
					0xad, 0x50, 0x23, 0x69, 0xc, 0x80, 0xf3, 0xa4,
					0x9c, 0x8f, 0x13, 0xf8, 0xd4, 0x5b, 0x8c, 0x85,
					0x7f, 0xbc, 0xbc, 0x8b, 0xc4, 0xa8, 0xe4, 0xd3,
					0xeb, 0x4b, 0x10, 0xf4, 0xd4, 0x60, 0x4f, 0xa0,
					0x8d, 0xce, 0x60, 0x1a, 0xaf, 0xf, 0x47, 0x2,
					0x16, 0xfe, 0x1b, 0x51, 0x85, 0xb, 0x4a, 0xcf,
					0x21, 0xb1, 0x79, 0xc4, 0x50, 0x70, 0xac, 0x7b,
					0x3, 0xa9, 0xac,
				},
			},
		},
		LockTime: 0,
	}
	ltcGenesisMerkleRoot = chainhash.Hash([chainhash.HashSize]byte{ // Make go vet happy.
		0xd9, 0xce, 0xd4, 0xed, 0x11, 0x30, 0xf7, 0xb7,
		0xfa, 0xad, 0x9b, 0xe2, 0x53, 0x23, 0xff, 0xaf,
		0xa3, 0x32, 0x32, 0xa1, 0x7c, 0x3e, 0xdf, 0x6c,
		0xfd, 0x97, 0xbe, 0xe6, 0xba, 0xfb, 0xdd, 0x97,
	})
)

var LTCParams = map[string]*netparams.ChainParams{
	"mainnet": {
		Chain:       "ltc",
		Name:        "mainnet",
		Net:         0xdbb6c0fb,
		DefaultPort: "9333",
		DNSSeeds: []chaincfg.DNSSeed{
			{Host: "seed-a.litecoin.loshan.co.uk", HasFiltering: true},
			{Host: "dnsseed.thrasher.io", HasFiltering: true},
			{Host: "dnsseed.litecointools.com", HasFiltering: false},
			{Host: "dnsseed.litecoinpool.org", HasFiltering: false},
		},
		GenesisBlock: &wire.MsgBlock{
			Header: wire.BlockHeader{
				Version:    1,
				PrevBlock:  chainhash.Hash{}, // 0000000000000000000000000000000000000000000000000000000000000000
				MerkleRoot: ltcGenesisMerkleRoot,
				Timestamp:  time.Unix(1317972665, 0),
				Bits:       0x1e0ffff0,
				Nonce:      2084524493,
			},
			Transactions: []*wire.MsgTx{&ltcGenesisCoinbaseTx},
		},
		GenesisHash: hashPointer([chainhash.HashSize]byte{ // Make go vet happy.
			0xe2, 0xbf, 0x04, 0x7e, 0x7e, 0x5a, 0x19, 0x1a,
			0xa4, 0xef, 0x34, 0xd3, 0x14, 0x97, 0x9d, 0xc9,
			0x98, 0x6e, 0x0f, 0x19, 0x25, 0x1e, 0xda, 0xba,
			0x59, 0x40, 0xfd, 0x1f, 0xe3, 0x65, 0xa7, 0x12,
		}),
		PowLimit:                 ltcMainPowLimit,
		PowLimitBits:             504365055,
		TargetTimespan:           (time.Hour * 24 * 3) + (time.Hour * 12), // 3.5 days
		TargetTimePerBlock:       (time.Minute * 2) + (time.Second * 30),  // 2.5 minutes
		RetargetAdjustmentFactor: 4,                                       // 25% less, 400% more
		ReduceMinDifficulty:      false,
		MinDiffReductionTime:     0,
		// The first retarget above a checkpoint at boundary B samples the
		// timestamp of block B-1 (see LitecoinNextBits).
		DifficultyLookback: 1,
		// Checkpoints ordered from oldest to newest. The first is the launch
		// anchor: the last retarget boundary mined before
		// DefaultWalletBirthday (2026-07-01). The electrum header store
		// starts there (see electrum.NewChainConfig) and no wallet birthday
		// predates it, so it never moves. A checkpoint follows every 26
		// retarget periods (52416 blocks, about three months) and is added
		// once mined; the header store validates each one as it syncs
		// through. Hashes cross-checked via two of litecoinspace.org,
		// blockchair.com and blockcypher.com.
		// The livenet TestCheckpointRecords in the spv package fetches each
		// header and filter header from P2P peers, checks the header against
		// the hash, and requires the peers to agree on the filter header.
		// Difficulty holds the bits and timestamp of the block below each
		// checkpoint, derived by that test from headers linked back to the
		// checkpoint header.
		Checkpoints: []netparams.Checkpoint{
			{
				Height:       3132864, // 2026-06-28
				Hash:         newHashFromStr("22076f9f6295e025e4b53c021b936e9f482888734536e86b6ffb7442038c14f4"),
				Header:       newHeaderFromStr("14000020069c16322f35fadf71c5a3951fc4c6e2ef3139e6b97d2630e211457b23539c61c52a1638a6586a3b4ed2094473b1e0916e4c0ee3b96a3f0e81eb708bd03331ad650b416a7c482f198040f9c4"),
				FilterHeader: newHashFromStr("bc2cce0376dad660ca4855dc9b7c204d0def5766c3d08d71b6748dff31717e93"),
				Difficulty:   []netparams.BlockDifficulty{{Bits: 0x192d25d2, Timestamp: 1782647569}},
			},
			{
				Height:       3185280, // 2026-09-27
				Hash:         newHashFromStr("be08d745b1dcf064fb8d725662070a3c3461a9b02f0889a106dbe709bda8547e"),
				Header:       newHeaderFromStr("00000020ace651653dbc6ac7747f0fec1cee48d49300f2edcf4ce0209367029575dfe18f5bd989d655e967a6147ed828a9ac7876ebeff58791eb731e1bb629421c9c455ed515b96adda82e19819dfbbb"),
				FilterHeader: newHashFromStr("a15fd93e9ecb079386fc9a998f418cb4331e80f9e97d9de828b13f085107edd3"),
				Difficulty:   []netparams.BlockDifficulty{{Bits: 0x192e0c95, Timestamp: 1790514548}},
			},
		},
		Bech32HRPSegwit:         "ltc",                           // always ltc for main net
		PubKeyHashAddrID:        0x30,                            // starts with L
		ScriptHashAddrID:        0x32,                            // starts with M
		PrivateKeyID:            0xB0,                            // starts with 6 (uncompressed) or T (compressed)
		WitnessPubKeyHashAddrID: 0x06,                            // starts with p2
		WitnessScriptHashAddrID: 0x0A,                            // starts with 7Xh
		HDPrivateKeyID:          [4]byte{0x04, 0x88, 0xad, 0xe4}, // starts with xprv
		HDPublicKeyID:           [4]byte{0x04, 0x88, 0xb2, 0x1e}, // starts with xpub
		HDCoinType:              2,
		KeyPurposes:             []uint32{84},
		BIP0034Height:           710000,
		BIP0065Height:           918684,
		BIP0066Height:           811879,
		CheckPoW:                checkPoWScrypt,
		CoinbaseMaturity:        100,
		MaxSatoshi:              84e6 * btcutil.SatoshiPerBitcoin,
		ElectrumServers:         ltcMainnetElectrumServers,
	},
	"testnet": {
		Chain:       "ltc",
		Name:        "testnet4",
		Net:         0xf1c8d2fd,
		DefaultPort: "19335",
		DNSSeeds: []chaincfg.DNSSeed{
			{Host: "testnet-seed.litecointools.com", HasFiltering: false},
			{Host: "seed-b.litecoin.loshan.co.uk", HasFiltering: false /*true*/},
			{Host: "dnsseed-testnet.thrasher.io", HasFiltering: false /*true*/},
		},
		GenesisBlock: &wire.MsgBlock{
			Header: wire.BlockHeader{
				Version:    1,
				PrevBlock:  chainhash.Hash{}, // 0000000000000000000000000000000000000000000000000000000000000000
				MerkleRoot: ltcGenesisMerkleRoot,
				Timestamp:  time.Unix(1486949366, 0),
				Bits:       0x1e0ffff0,
				Nonce:      293345,
			},
			Transactions: []*wire.MsgTx{&ltcGenesisCoinbaseTx},
		},
		GenesisHash: hashPointer([chainhash.HashSize]byte{ // Make go vet happy.
			0xa0, 0x29, 0x3e, 0x4e, 0xeb, 0x3d, 0xa6, 0xe6,
			0xf5, 0x6f, 0x81, 0xed, 0x59, 0x5f, 0x57, 0x88,
			0xd, 0x1a, 0x21, 0x56, 0x9e, 0x13, 0xee, 0xfd,
			0xd9, 0x51, 0x28, 0x4b, 0x5a, 0x62, 0x66, 0x49,
		}),
		PowLimit:                 ltcMainPowLimit,
		PowLimitBits:             504365055,
		BIP0034Height:            76,
		BIP0065Height:            76,
		BIP0066Height:            76,
		TargetTimespan:           (time.Hour * 24 * 3) + (time.Hour * 12), // 3.5 days
		TargetTimePerBlock:       (time.Minute * 2) + (time.Second * 30),  // 2.5 minutes
		RetargetAdjustmentFactor: 4,
		ReduceMinDifficulty:      true,
		MinDiffReductionTime:     time.Minute * 5, // TargetTimePerBlock * 2
		// See the mainnet Checkpoints comment: the last retarget boundary
		// mined before DefaultWalletBirthday. Cross-checked via
		// litecoinspace.org and the bysh.me electrum server.
		Checkpoints: []netparams.Checkpoint{
			{
				Height:       4775904, // 2026-06-23
				Hash:         newHashFromStr("34e2595bb4c0e4017924e9f618a4482d3f8fd121fee1794b61e7b17684d18502"),
				Header:       newHeaderFromStr("0000002061be1827e925a2fb8a7842e1314b5759ba876fdcb16a3c92fea4d1afa0debe51cf3749f50e7df5b80f6cc4612a2f5482b45a3a59d612f31597fbd8bfbe0eb9e925ac3a6ade10051d46d62744"),
				FilterHeader: newHashFromStr("8b6455af90ddc339ed8c96bf0ab2c2a87c6095b9b82d5f734e03880ea35d5699"),
			},
		},
		Bech32HRPSegwit:         "tltc",                          // always tltc for test net
		PubKeyHashAddrID:        0x6f,                            // starts with m or n
		ScriptHashAddrID:        0x3a,                            // starts with Q
		WitnessPubKeyHashAddrID: 0x52,                            // starts with QW
		WitnessScriptHashAddrID: 0x31,                            // starts with T7n
		PrivateKeyID:            0xef,                            // starts with 9 (uncompressed) or c (compressed)
		HDPrivateKeyID:          [4]byte{0x04, 0x35, 0x83, 0x94}, // starts with tprv
		HDPublicKeyID:           [4]byte{0x04, 0x35, 0x87, 0xcf}, // starts with tpub
		HDCoinType:              1,
		KeyPurposes:             []uint32{84},
		CheckPoW:                checkPoWScrypt,
		CoinbaseMaturity:        100,
		MaxSatoshi:              84e6 * btcutil.SatoshiPerBitcoin,
		ElectrumServers:         ltcTestnetElectrumServers,
		NeutrinoSeeds:           ltcTestnetNeutrinoSeeds,
	},
	"simnet": {
		Chain:       "ltc",
		Name:        "regtest",
		Net:         0xdab5bffa,
		DefaultPort: "19444",
		DNSSeeds:    []chaincfg.DNSSeed{},

		// Chain parameters
		GenesisBlock: &wire.MsgBlock{
			Header: wire.BlockHeader{
				Version:    1,
				PrevBlock:  chainhash.Hash{},
				MerkleRoot: ltcGenesisMerkleRoot,
				Timestamp:  time.Unix(1296688602, 0), // 2011-02-02 23:16:42 +0000 UTC
				Bits:       0x207fffff,               // 545259519 [7fffff0000000000000000000000000000000000000000000000000000000000]
				Nonce:      0,
			},
			Transactions: []*wire.MsgTx{&ltcGenesisCoinbaseTx},
		},
		GenesisHash: hashPointer([chainhash.HashSize]byte{ // Make go vet happy.
			0xf9, 0x16, 0xc4, 0x56, 0xfc, 0x51, 0xdf, 0x62,
			0x78, 0x85, 0xd7, 0xd6, 0x74, 0xed, 0x02, 0xdc,
			0x88, 0xa2, 0x25, 0xad, 0xb3, 0xf0, 0x2a, 0xd1,
			0x3e, 0xb4, 0x93, 0x8f, 0xf3, 0x27, 0x08, 0x53,
		}),
		PowLimit:                 new(big.Int).Sub(new(big.Int).Lsh(bigOne, 255), bigOne),
		PowLimitBits:             0x207fffff,
		PoWNoRetargeting:         true,
		BIP0034Height:            100000000,                               // Not active - Permit ver 1 blocks
		BIP0065Height:            1351,                                    // Used by regression tests
		BIP0066Height:            1251,                                    // Used by regression tests
		TargetTimespan:           (time.Hour * 24 * 3) + (time.Hour * 12), // 3.5 days
		TargetTimePerBlock:       (time.Minute * 2) + (time.Second * 30),  // 2.5 minutes
		RetargetAdjustmentFactor: 4,                                       // 25% less, 400% more
		ReduceMinDifficulty:      true,
		MinDiffReductionTime:     time.Minute * 20, // TargetTimePerBlock * 2

		// Checkpoints ordered from oldest to newest.
		Checkpoints:      nil,
		Bech32HRPSegwit:  "rltc",                          // always rltc for reg test net
		PubKeyHashAddrID: 0x6f,                            // starts with m or n
		ScriptHashAddrID: 0x3a,                            // starts with Q
		PrivateKeyID:     0xef,                            // starts with 9 (uncompressed) or c (compressed)
		HDPrivateKeyID:   [4]byte{0x04, 0x35, 0x83, 0x94}, // starts with tprv
		HDPublicKeyID:    [4]byte{0x04, 0x35, 0x87, 0xcf}, // starts with tpub
		HDCoinType:       1,
		KeyPurposes:      []uint32{84},
		CheckPoW:         checkPoWScrypt,
		CoinbaseMaturity: 100,
		MaxSatoshi:       84e6 * btcutil.SatoshiPerBitcoin,
		ElectrumServers:  ltcSimnetElectrumServers,
		NeutrinoSeeds:    ltcSimnetNeutrinoSeeds,
	},
}

func checkPoWScrypt(hdr *wire.BlockHeader) error {
	var powHash chainhash.Hash

	buf := bytes.NewBuffer(make([]byte, 0, wire.MaxBlockHeaderPayload))
	hdr.Serialize(buf)

	scryptHash, _ := scrypt.Key(buf.Bytes(), buf.Bytes(), 1024, 1, 1, 32)
	copy(powHash[:], scryptHash)

	target := blockchain.CompactToBig(hdr.Bits)

	hashNum := blockchain.HashToBig(&powHash)
	if hashNum.Cmp(target) > 0 {
		return fmt.Errorf("block hash of %064x is higher than "+
			"expected max of %064x", hashNum, target)
	}
	return nil
}

// ltcMainnetCertPEM is the pinned TLS leaf certificate served by the built-in
// mainnet LTC electrumx server (electrum-ltc.bysh.me:50002). It is self-signed
// and long-lived (valid until 2036), so it cannot be verified against a public
// CA and is pinned instead. If the server rotates its certificate this must be
// refreshed; re-fetch with:
//
//	openssl s_client -connect electrum-ltc.bysh.me:50002 </dev/null | openssl x509
const ltcMainnetCertPEM = `-----BEGIN CERTIFICATE-----
MIIDcjCCAloCCQDghQZwjGMwajANBgkqhkiG9w0BAQsFADB7MQswCQYDVQQGEwJO
TDEWMBQGA1UECAwNTm9vcmQtSG9sbGFuZDESMBAGA1UEBwwJQW1zdGVyZGFtMSEw
HwYDVQQKDBhJbnRlcm5ldCBXaWRnaXRzIFB0eSBMdGQxHTAbBgNVBAMMFGVsZWN0
cnVtLWx0Yy5ieXNoLm1lMB4XDTI2MDUwNDA4MTQ0NVoXDTM2MDUwMzA4MTQ0NVow
ezELMAkGA1UEBhMCTkwxFjAUBgNVBAgMDU5vb3JkLUhvbGxhbmQxEjAQBgNVBAcM
CUFtc3RlcmRhbTEhMB8GA1UECgwYSW50ZXJuZXQgV2lkZ2l0cyBQdHkgTHRkMR0w
GwYDVQQDDBRlbGVjdHJ1bS1sdGMuYnlzaC5tZTCCASIwDQYJKoZIhvcNAQEBBQAD
ggEPADCCAQoCggEBAPqzSqn/9DPbD5LFp50+QaJWE+i2P/fSmbHzWW6fWSRPNFkM
UEm/fvxtNNwqZxIMXBNsO79KBNazIbQGZ1BszF98zlHRu0bu6NPlB0HbCjO8nAl/
3JNnJscEnXvDc2qc6A2azHYO4Lo1v5GH0XPL19PUaOVAFLUq9cCNLfKeAxdIdbOV
IZa/3fU3Xp+hAXrNba4irIZYVfu4102uRt2rvbODY8LyAMaQil3h2ao3ODBxL+qB
avQUdYnVgD6GmV3kaWM93Stt/lXuSddh1o6b43GA6z/jS7+3zenu03RCIdZNwOdL
kY5u+bKSsRy551ZMtTB++FFoDzaizZEdJtvVIaECAwEAATANBgkqhkiG9w0BAQsF
AAOCAQEAUcyVcngv5eaUvYL2Cw7zdGfM/aNkTmDSdZGkRhQ9oofWCtDXuzoN5ggK
tIcrAVr+itI684aHKqFgEmuKYGq9+L8T+v1x51vYUgOdqlZi4Z3EVqXqPuZGZZmS
ZXIruwgA+fVVqtKiH/RQSyuBnFSb0h3//Jf5QC31I/WxwMPDoHOpwX2J7qv++erO
v39IxLT/zL+e8s2QgE7qTn2Fof6QG6Gg7Dg/ipuWIn7iRagaz5yd/RVdoGyGgvkG
C3ReuQrfaqq2jkZp7LJL2zi3L98bqdKfp4kLkqw8+pFVPQy4E6VdYwh0H7tNYYAv
JF0LTH0Kr986wDhHDvdCIaV6DR4s7w==
-----END CERTIFICATE-----`

// ltcMainnetCert is the DER form of ltcMainnetCertPEM, pinned for the mainnet
// LTC electrumx server. Decoded at init; a malformed constant panics at startup.
var ltcMainnetCert = netparams.MustParseCertPEM(ltcMainnetCertPEM)

// Shipped certificate pins for further self-signed LTC mainnet servers,
// fetched and checked as described for the BTC pins (assets/btc.go), on
// 2026-11-17. ltc.aftrek.org's certificate expires 2028-03-27, under two
// years from the fetch, so that server is left to trust on first use.

// ltcBackupElectrumCertPEM is the TLS leaf certificate of backup.electrum-ltc.org:443,
// self-signed and valid until 2037-12-19.
const ltcBackupElectrumCertPEM = `-----BEGIN CERTIFICATE-----
MIIDoTCCAomgAwIBAgIJAIln3N4Wi0HIMA0GCSqGSIb3DQEBCwUAMGcxCzAJBgNV
BAYTAk5MMRMwEQYDVQQIDApTb21lLVN0YXRlMSEwHwYDVQQKDBhJbnRlcm5ldCBX
aWRnaXRzIFB0eSBMdGQxIDAeBgNVBAMMF2JhY2t1cC5lbGVjdHJ1bS1sdGMub3Jn
MB4XDTE3MTIxOTExMzgwMloXDTM3MTIxOTExMzgwMlowZzELMAkGA1UEBhMCTkwx
EzARBgNVBAgMClNvbWUtU3RhdGUxITAfBgNVBAoMGEludGVybmV0IFdpZGdpdHMg
UHR5IEx0ZDEgMB4GA1UEAwwXYmFja3VwLmVsZWN0cnVtLWx0Yy5vcmcwggEiMA0G
CSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCoNlT9UYBNlMz98jpMlgA0+D/3VVbW
mrnFDibeQqFRVpcOAUZWW+rrqXK8zqS9AjC7FCaxLfpNI9N4KFVL8OtDu+Qx8QsG
OqGtFSOW9QvAH2MXCFGxlwMC+U6qURtg1er8pwQLLuex2CglKP3eKBW/X/jPhrdo
InnhwHipDHyMQD5vugN2Ebl1yvaROviQ7mj9CRGmrIO97nK0a7sZsAE94N5CSYlf
6zWuvlHWenNINCZIPH1meiaiDqPSnOB1tZteufH7dC+LklZL/UcyKKi1bxRUHcTi
TG+piQk+LCBJ9qulwEUJs+bDScbZT6vOn46TqLY9oCLgr32xO77ZN7kRAgMBAAGj
UDBOMB0GA1UdDgQWBBQrc0ViACsbzdMYUGnH/8fOzU7+ozAfBgNVHSMEGDAWgBQr
c0ViACsbzdMYUGnH/8fOzU7+ozAMBgNVHRMEBTADAQH/MA0GCSqGSIb3DQEBCwUA
A4IBAQBcWlX5CwamLYcUGx2Iw563fv0tGwK0+O9plz9Z47TuA4ztYQMEpzru4ymZ
cxLvY8puDeckZdO3esO06SScIM3tPTp6IwqvsDwfRxaSHqDofj5Eaa8qdFfBYvo3
A1zDeCg7u0FGFJcsyYF1YtcNGYrrZWBLuUg5X6Zc7iQVacsHLhtzYCmGicHeoz4T
dGQxBCnESfsc94YsMy0UNDa1XnsuCFE+VvbHdl1sxua01HTh0tqPxwpLD8RUyfOC
k2Uzp9IFi4+78fNwYHiYWv7Af9QzP0nbdOZ1m/VB5X51bzNuEEV7aWX5zIyhT1Cr
o1P3eogvzteEEJzA/aqFFOsfwTS0
-----END CERTIFICATE-----`

// ltcXuriousCertPEM is the TLS leaf certificate of electrum.ltc.xurious.com:50002,
// self-signed and valid until 2036-05-03.
const ltcXuriousCertPEM = `-----BEGIN CERTIFICATE-----
MIIDeTCCAmECFB2oKN64YSNngBxiNlHvoT0rXgA6MA0GCSqGSIb3DQEBCwUAMHkx
CzAJBgNVBAYTAlVTMRIwEAYDVQQIDAlXaXNjb25zaW4xEDAOBgNVBAcMB01hZGlz
b24xITAfBgNVBAoMGEludGVybmV0IFdpZGdpdHMgUHR5IEx0ZDEhMB8GA1UEAwwY
ZWxlY3RydW0ubHRjLnh1cmlvdXMuY29tMB4XDTI2MDUwNDA4MTA0N1oXDTM2MDUw
MzA4MTA0N1oweTELMAkGA1UEBhMCVVMxEjAQBgNVBAgMCVdpc2NvbnNpbjEQMA4G
A1UEBwwHTWFkaXNvbjEhMB8GA1UECgwYSW50ZXJuZXQgV2lkZ2l0cyBQdHkgTHRk
MSEwHwYDVQQDDBhlbGVjdHJ1bS5sdGMueHVyaW91cy5jb20wggEiMA0GCSqGSIb3
DQEBAQUAA4IBDwAwggEKAoIBAQCX9wIBrEk/ATsqOpnP4WcAqgBP1Rp5NSLpCR75
WWBeENa/C8KO0gyKwQdvIVbFhn5w1QurhO5eb+vBnoeGY+4DrqAwO1PxNuWxQMxI
XsgcV3aN2WL9d1AAutoSZznaKCI379OgJGVInkqqNSZRxtqeW4TGT1r65sJYY9Ps
w+/g9ewf/sWQONOAskOqeELVi7D5/qOzs9vEmlMoSq2p0tZFfdFfyG+p/2nmqy9B
UW9fb4qf/5vNFBK0P4nbXXx3h+e0mzV5YbEVkxsgtVyiZP1JDI8DP+ZEnukjP9q0
VRaSJNvAbkGSYqA1XjjNnugqnX3C21CyWn4ba0a8t24sE2ezAgMBAAEwDQYJKoZI
hvcNAQELBQADggEBACiLrWtH49eBG1WweWOZl88wS6aahAsu1F4dU4Prex0pzKSM
3q5T4G/M4f8XtJJV9nCIAvBsUPffYm2mxnzAbz0LQ41p6S4Vvli0I+wG9lenO+iI
9j5oLUmeL1kbQarF1ULJeSHJRWr3p63gHOkxvnyKcBfbK4/M+IdcLxcOo0jI8xlf
Q/phQk593QedwpkHdArR7Q6XtJMAwz1rMk8VZ/iZni5wdaQk36KYoJ1EHBaiaX5i
qA76H6aDGkq5K0HjZ48jYmP+TKzoBqKTZ8m0UYtm1oq/TEPiADPCZjsUonTCkMJ5
72GhUWxzWKr1jz3zvd2yRc8BJadNAfa6XIe8kI4=
-----END CERTIFICATE-----`

// ltcFemCertPEM is the TLS leaf certificate of l.fem.sh:50002, self-signed
// and valid until 2030-05-04.
const ltcFemCertPEM = `-----BEGIN CERTIFICATE-----
MIIDETCCAfkCFEd+bJx3g8jCpUmUep9YYl5My+vEMA0GCSqGSIb3DQEBCwUAMEUx
CzAJBgNVBAYTAkFVMRMwEQYDVQQIDApTb21lLVN0YXRlMSEwHwYDVQQKDBhJbnRl
cm5ldCBXaWRnaXRzIFB0eSBMdGQwHhcNMjUwNTA1MTIzNDExWhcNMzAwNTA0MTIz
NDExWjBFMQswCQYDVQQGEwJBVTETMBEGA1UECAwKU29tZS1TdGF0ZTEhMB8GA1UE
CgwYSW50ZXJuZXQgV2lkZ2l0cyBQdHkgTHRkMIIBIjANBgkqhkiG9w0BAQEFAAOC
AQ8AMIIBCgKCAQEAxdnFUfBKX+s9M3pCksohbQxsbIpF5izw+R738OesAGXpT7qF
hW+wE2JejnkDmXRxe7ttQccOeaSi1RPwRJ4mP6X/Sp8DU+LaujjgrO9R1YcJwH5H
grPktaQ+L7f+2vWnt010Vd6Un/S7KqlIB5z5ibpzS8+Oa3CsjKjnax6OMDUppUOm
v10I/17hly1c54kSb+88THzz+S5WwarEZw5gq0LZB5r0tKBg7PseDKRYoNWd0as1
5lNlOE1hDLMd2wzn4RDgd/mLTa/olp9IoCpgnXUKSWHNash9JPDJoy2eDD+KRNEJ
pMCuSJb2O1HjC40LIazyy9iUnk6QPbkWMxPkawIDAQABMA0GCSqGSIb3DQEBCwUA
A4IBAQBn3Ca6fiy5okzxyFnwZZHkw2Bjr6SqvAJpIAERSIZZTzHMbbz9oIKfOWaO
1r1+x87ck73H4LcOby9jFfiSBmPBvOXkD3roHA5svw+fQgy8ujcpryP0fPrU7kfv
GULJzXyB261kMqlZ2voE2V2gJAnqVF+pMQOiAxVHdD0MECHKqsNz4Zz6KnPKCsJy
ilwSlsGA6w9UyDbjY2/cHJjZFt9Q7/0WmRjr8+Fs9Sa7sFgoAWWAEzskIeEJDo9I
KpdlzyeQKKmVctkRtog7NPN3v8cY2ydQM8Fr+WDZbexeRxqyURekyd51dHK26j6o
GDB1+VaQx6DTc17MrdcpAxot23qy
-----END CERTIFICATE-----`

// DER forms of the shipped LTC pins, decoded at init.
var (
	ltcBackupElectrumCert = netparams.MustParseCertPEM(ltcBackupElectrumCertPEM)
	ltcXuriousCert        = netparams.MustParseCertPEM(ltcXuriousCertPEM)
	ltcFemCert            = netparams.MustParseCertPEM(ltcFemCertPEM)
)

// ltcMainnetElectrumServers are the built-in electrum servers for LTC
// mainnet, in preference order (see the BTC list comment for the election
// and certificate conventions). Entries with neither Cert nor SkipVerify
// present CA-issued certificates; ltc.aftrek.org's key is learned on first
// use.
//
// Verified 2026-11-17 in three probe passes between 21:59 and 22:15 UTC: TLS
// reachable, protocol 1.4 or later, LTC genesis, tip agreeing with the other
// servers, and blockchain.block.headers serving 2016 linked headers from each
// checkpoint, starting at the checkpoint's hash. ltc.rentonisk.com refused
// connections in every pass and is kept last among the CA servers.
//
// The order spreads the leading entries across inferred operator groups and
// AS networks; the groups rest on shared operator signals, not proof of
// ownership. electrum-ltc.bysh.me and backup.electrum-ltc.org share a
// donation address, banner and hosting network (LeaseWeb Amsterdam).
// electrum.ltc.xurious.com is grouped with them on timing alone: its
// self-signed certificate and bysh.me's were issued four minutes apart on
// 2026-05-04 with the same ten-year term. Hetzner hosts jhoenicke, cipig and
// aftrek. A group of about 27 servers advertised through peer discovery
// under one donation address is left out, so that one group does not gain
// weight in the election.
var ltcMainnetElectrumServers = []*netparams.ServerAddr{
	{Proto: "ssl", Addr: "electrum-ltc.bysh.me:50002", Cert: ltcMainnetCert},
	{Proto: "ssl", Addr: "electrum.jhoenicke.de:50091"},
	{Proto: "ssl", Addr: "electrum.petrkr.net:60002"},
	{Proto: "ssl", Addr: "fallacy.fiatfaucet.com:50002"},
	{Proto: "ssl", Addr: "cryptod.bruijn.nu:50002"},
	{Proto: "ssl", Addr: "l.fem.sh:50002", Cert: ltcFemCert},
	{Proto: "ssl", Addr: "ltc.electrum1.cipig.net:20063"},
	{Proto: "ssl", Addr: "backup.electrum-ltc.org:443", Cert: ltcBackupElectrumCert}, // grouped with bysh.me
	{Proto: "ssl", Addr: "ltc.rentonisk.com:50002"},
	{Proto: "ssl", Addr: "electrum.ltc.xurious.com:50002", Cert: ltcXuriousCert}, // grouped with bysh.me
	{Proto: "ssl", Addr: "ltc.aftrek.org:50002", SkipVerify: true},
}

// ltcTestnetElectrumServers is the built-in electrumx server for LTC testnet4.
var ltcTestnetElectrumServers = []*netparams.ServerAddr{
	{Proto: "ssl", Addr: "electrum-ltc.bysh.me:51002", SkipVerify: true},
	{Proto: "ssl", Addr: "electrum.ltc.xurious.com:51002", SkipVerify: true},
}

// ltcSimnetElectrumServers is the built-in electrumx server for LTC
// regtest/simnet — points at the dextest harness (LTC port 55002, ssl).
var ltcSimnetElectrumServers = []*netparams.ServerAddr{
	{Proto: "ssl", Addr: "127.0.0.1:55002", SkipVerify: true},
}

// ltcTestnetNeutrinoSeeds are the default neutrino peers for LTC testnet.
// Snapshot of valid LTC testnet4 peers. 10 May 2024.
var ltcTestnetNeutrinoSeeds = append([]string{"127.0.0.1:19335"},
	"13.200.66.216:19335",
	"208.91.111.150:18333",
	"92.244.111.167:19335",
	"164.92.171.95:8333",
	"204.16.244.114:18333",
	"34.227.13.195:19335",
	"18.192.56.149:18333",
)

// ltcSimnetNeutrinoSeeds are the default neutrino peers for LTC simnet/regtest.
var ltcSimnetNeutrinoSeeds = []string{"127.0.0.1:20585"}
