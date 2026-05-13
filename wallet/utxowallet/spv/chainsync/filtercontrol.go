package chainsync

import (
	"fmt"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// ErrCheckpointMismatch is returned if given filter headers don't pass our
// control check.
var ErrCheckpointMismatch = fmt.Errorf("checkpoint doesn't match")

// filterHeaderCheckpoints holds a mapping from heights to filter headers for
// various heights. We use them to check whether peers are serving us the
// expected filter headers.
var filterHeaderCheckpoints = map[wire.BitcoinNet]map[uint32]*chainhash.Hash{
	// Mainnet filter header checkpoints.
	chaincfg.MainNetParams.Net: {
		100000: hashFromStr("f28cbc1ab369eb01b7b5fe8bf59763abb73a31471fe404a26a06be4153aa7fa5"),
		200000: hashFromStr("e5031471732f4fbfe7a25f6a03acc1413300d5c56ae8e06b95046b8e4c0f32b3"),
		300000: hashFromStr("1bd50220fcdde929ca3143c91d2dd9a9bfedb38c452ba98dbb51e719bff8aa5b"),
		400000: hashFromStr("5d973ab1f1c569c70deec1c1a8fb2e317a260f1656edb3b262c65f78ef192e3a"),
		500000: hashFromStr("5d16ca293c9bdc0a9bc279b63f99fb661be38b095a59a44200a807caaa631a3c"),
		600000: hashFromStr("bde0854d0b2f4386a860462547140e0c6817f5b4b2ab515ef70e204e377598f8"),
		660000: hashFromStr("08312375fabc082b17fa8ee88443feb350c19a34bb7483f94f7478fa4ad33032"),
	},

	// Testnet filter header checkpoints.
	chaincfg.TestNet3Params.Net: {
		100000:  hashFromStr("97c0633f14625627fcd133250ad8cc525937e776b5f3fd272b06d02c58b65a1c"),
		200000:  hashFromStr("51aa817e5abe3acdcf103616b1a5736caf84bc3773a7286e9081108ecc38cc87"),
		400000:  hashFromStr("4aab9b3d4312cd85cfcd48a08b36c4402bfdc1e8395dcf4236c3029dfa837c48"),
		600000:  hashFromStr("713d9c9198e2dba0739e85aab6875cb951c36297b95a2d51131aa6919753b55d"),
		800000:  hashFromStr("0dafdff27269a70293c120b14b1f5e9a72a5e8688098cfc6140b9d64f8325b99"),
		1000000: hashFromStr("c2043fa2f6eb5f8f8d2c5584f743187f36302ed86b62c302e31155f378da9c5f"),
		1400000: hashFromStr("f9ae1750483d4c8ce82512616b1ded932886af46decb8d3e575907930542d9b3"),
		1500000: hashFromStr("dc0cfa13daf09df9b8dbe7532f75ebdb4255860b295016b2ca4b789394bc5090"),
		1800000: hashFromStr("67083b2d5dfc9ca1415bffa14e43a5bbe595e2e8b7ffbcc7a4ea78fa069a9c8d"),
		1900000: hashFromStr("96a31467f9edcaa3297770bc6cdf66926d5d17dfad70cb0cac285bfe9075c494"),
	},

	// Litecoin mainnet filter header checkpoints (magic 0xdbb6c0fb).
	// Sourced from dcrlabs/ltcwallet and ltcsuite/neutrino, both independent
	// references agree on these values.
	wire.BitcoinNet(0xdbb6c0fb): {
		50000:   hashFromStr("40ef276cc92c0edf323ee0be4cda6d72007846470329a150547947d52064841c"),
		100000:  hashFromStr("16481d67008768cbbfde5a5a4440cb53cc7c6bf6fa7d619aafbcfaa4a708830e"),
		150000:  hashFromStr("60f41c9a1e84590c14783413df2b4d865d4b09fee90ee50b48999a3877e8bc5f"),
		250000:  hashFromStr("60f3a67c5f95e4b34048b9c6df43580f587da749d3adab6cc7293c255155de08"),
		300000:  hashFromStr("c3ce44651b1cce4b7b2fcd07861948363759f95c9a7e03e1ac7e4f79b9cc7763"),
		350000:  hashFromStr("43d979781b215b365cc94be7e5205aaf24607d64088a81e8c87e686051da720b"),
		500000:  hashFromStr("f8aaf8bb9ec1b9e2dae9c9e37e12d0be9712572bfbd1de15a4fda4b39539136c"),
		550000:  hashFromStr("6657d5df5779ee936841b3979a950467666e47d6dae74b77b8249fac1d226c6d"),
		600000:  hashFromStr("491a699439130c8898d488f4eb88528604ac6721dc2775b8567ef62847840174"),
		650000:  hashFromStr("8e0d153a67219128c5be6c1eafecb358d7c32fb78f7188dcce067a3340b57f54"),
		700000:  hashFromStr("9c458879fc6ec919c9040400618699e64b7ff175cf420788b3c68dadfdf8582a"),
		801000:  hashFromStr("c4fd4b38ecc56f1cb35037c2ff8395b48c477368d0edeec16bc7dd065314685e"),
		901000:  hashFromStr("62752d9fd50e1876a731087f6fcc28abad5f25d51d7eb04351f81bf80096f31f"),
		1001000: hashFromStr("2e73db026373af029f37201515870f607b9ad9f541811e65208e228f3719ba95"),
		1101000: hashFromStr("2f157ca9fd36cb5cf7bd5a881e40c112c82a35dab578dd8b57747211a68fe296"),
		1201000: hashFromStr("9bfb167a5b3e41567315fe1ba8ecbb964cc7fac11b37c9e874a3beb79f6fca5d"),
		1301000: hashFromStr("0074cd8e4c41407320ca0b3d640265114ec184ae29970a9ab04d6d7e6c67cc43"),
		1403000: hashFromStr("b99e248cea04bdcf8cc988ea5ebc91b6e7a190c6896a9df718f1b702cf636b28"),
		1505000: hashFromStr("ffdf8b4637b533a5848563d8541a116c3cf1584bef07a05fd11ed4ba0b61e10c"),
		1603000: hashFromStr("c9c115714f37e5e325effc9a386a5659844a5abfa6c25695fd591c4a61933b13"),
		1701000: hashFromStr("f6bee45920a347c2b245a5c9796c20268c27bc7143c7c62ac0ac2cc9b5dcdace"),
		1781000: hashFromStr("6d2505ff9d8ecaa573db0bb3aa34fc48986de30a39d29c3137333d753ae2b868"),
		1865000: hashFromStr("ed28ca668aafad16b94dea78e2ed738dcfb64eedc2f49ccab670071c26d210ee"),
		1999000: hashFromStr("01569255d7f6b3f1297b1b59538d443ac8f5a3e0dd8dcf48a4ca564f2e017ce1"),
		2101000: hashFromStr("c52a617fe976ff7a1ddb282cf0d86f966ffe6f83c1d8f56fce115a1228ef59de"),
		2215000: hashFromStr("da27b1459b1105d9e95f8709215a9e990d1a5f827b3f0603e8759a4b9915e3b6"),
		2284583: hashFromStr("f46e91177160603dd6eb081e23cb49c9e247d55c9c538c0ff4cd8a6bbd702fe9"),
	},

	// Litecoin testnet4 filter header checkpoints (magic 0xf1c8d2fd).
	// Sourced from dcrlabs/ltcwallet (independent from ltcsuite/neutrino which
	// only includes entries up to height 1351000).
	wire.BitcoinNet(0xf1c8d2fd): {
		10000:   hashFromStr("3b98491e48c3f89e6aba3466ab2f1ba802faef6e5ea92c7b0bcc20682ba44465"),
		101000:  hashFromStr("8b54ceec6cde1f1b20e6ac2b28dcf3d72c51799760e625b156fca8b7c5c0a558"),
		303000:  hashFromStr("3ff217c2f372bc3bb7273e085906183f8afac1858922c710e05d2f961357b21f"),
		505000:  hashFromStr("1b7ebeabbcc37731932b09a68f36b4a3a54aa6067805a3ea6c057d866e8549d1"),
		707000:  hashFromStr("ec15c1a28981cb19096b5b9aea2f2389fc41e755a1b0228f27f2976d50851e21"),
		909000:  hashFromStr("cd51428ecfbe42bcd3920c42eaa1ad14d5b2eecff7d8a57d70df9e0b247f5b4a"),
		1111000: hashFromStr("52a74833414ded5b7bdc7e274b0107569adcdb09cdd9f75bf25ad8793aac437c"),
		1313000: hashFromStr("c81c0db37c5bcf27ba973215770a8a17630cb85db5035f898c94d1964b9b856a"),
		1351000: hashFromStr("fd0d8cd62d0ce53ae003c5bd32db4943f330e4a1cc9ea09be6a34325085016e4"),
		2408827: hashFromStr("8bd21a03b2e651a83bc94ba64da84ce5e4aac403e3cadc4ea1f89fff110cab12"),
	},
}

// ControlCFHeader controls the given filter header against our list of
// checkpoints. It returns ErrCheckpointMismatch if we have a checkpoint at the
// given height, and it doesn't match.
func ControlCFHeader(params *netparams.ChainParams, fType wire.FilterType,
	height uint32, filterHeader *chainhash.Hash) error {

	if fType != wire.GCSFilterRegular {
		return fmt.Errorf("unsupported filter type %v", fType)
	}

	control, ok := filterHeaderCheckpoints[params.Net]
	if !ok {
		return nil
	}

	hash, ok := control[height]
	if !ok {
		return nil
	}

	if *filterHeader != *hash {
		return ErrCheckpointMismatch
	}

	return nil
}

// hashFromStr makes a chainhash.Hash from a valid hex string. If the string is
// invalid, a nil pointer will be returned.
func hashFromStr(hexStr string) *chainhash.Hash {
	hash, _ := chainhash.NewHashFromStr(hexStr)
	return hash
}
