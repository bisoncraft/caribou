// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
)

const (
	// GweiFactor is the amount of wei in one gwei.
	GweiFactor = 1e9
	// MaxBlockInterval is the number of seconds since the last header came
	// in over which we consider the chain to be out of sync.
	MaxBlockInterval = 180
	EthBipID         = 60
	MinGasTipCap     = 2 //gwei
)

// These are the chain IDs of the various Ethereum network supported.
const (
	MainnetChainID = 1
	TestnetChainID = 11155111 // Sepolia
	SimnetChainID  = 1337     // see dex/testing/eth/harness.sh
)

var (
	// ChainIDs is a map of the network name to it's chain ID.
	ChainIDs = map[util.Network]int64{
		util.Mainnet: MainnetChainID,
		util.Testnet: TestnetChainID,
		util.Simnet:  SimnetChainID,
	}

	UnitInfo = util.UnitInfo{
		AtomicUnit: "gwei",
		Conventional: util.Denomination{
			Unit:             "ETH",
			ConversionFactor: 1e9,
		},
		Alternatives: []util.Denomination{
			{
				Unit:             "Szabos",
				ConversionFactor: 1e6,
			},
			{
				Unit:             "Finneys",
				ConversionFactor: 1e3,
			},
		},
		FeeRateDenom: "gas",
	}

	VersionedGases = map[uint32]*Gases{
		0: v0Gases,
		1: v1Gases,
	}

	MultiBalanceAddresses = map[util.Network]common.Address{
		util.Mainnet: common.HexToAddress("0x73bc803A2604b2c58B8680c3CE1b14489842EF16"), // tx 0xaf6cb861578c0ded0750397d7e044a7dd86c94aa47211d02188e146a2424dda4
		util.Testnet: common.HexToAddress("0x8Bd6F6dBe69588D94953EE289Fd3E1db3e8dB43D"), // tx 0x46a416344927a8d1f33865374e9b9e824249980da8d34f6c3214a1ee036ca5fe
	}
)

// v0Gases and v1Gases are the base-chain ETH gas tables. The native ETH asset
// does not use token Transfer/Approve, so these are empty placeholders that
// preserve the supported version keys.
var v0Gases = &Gases{}

var v1Gases = &Gases{}

// LoadGenesisFile loads a Genesis config from a json file.
func LoadGenesisFile(genesisFile string) (*core.Genesis, error) {
	fid, err := os.Open(genesisFile)
	if err != nil {
		return nil, err
	}
	defer fid.Close()

	var genesis core.Genesis
	err = json.NewDecoder(fid).Decode(&genesis)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal simnet genesis: %v", err)
	}
	return &genesis, nil
}

// EncodeContractData packs the contract version and the locator into a byte
// slice for communicating a swap's identity.
func EncodeContractData(contractVersion uint32, locator []byte) []byte {
	b := make([]byte, len(locator)+4)
	binary.BigEndian.PutUint32(b[:4], contractVersion)
	copy(b[4:], locator[:])
	return b
}

func DecodeContractDataV0(data []byte) (secretHash [32]byte, err error) {
	contractVer, secretHashB, err := DecodeContractData(data)
	if err != nil {
		return secretHash, err
	}
	if contractVer != 0 {
		return secretHash, errors.New("not contract version 0")
	}
	copy(secretHash[:], secretHashB)
	return
}

// DecodeContractData unpacks the contract version and the locator.
func DecodeContractData(data []byte) (contractVersion uint32, locator []byte, err error) {
	if len(data) < 4 {
		err = errors.New("invalid short encoding")
		return
	}
	locator = data[4:]
	contractVersion = binary.BigEndian.Uint32(data[:4])
	switch contractVersion {
	case 0:
		if len(locator) != SecretHashSize {
			err = fmt.Errorf("v0 locator is too small. expected %d, got %d", SecretHashSize, len(locator))
			return
		}
	case 1:
		if len(locator) != LocatorV1Length {
			err = fmt.Errorf("v1 locator is too small. expected %d, got %d", LocatorV1Length, len(locator))
			return
		}
	default:
		err = fmt.Errorf("unkown contract version %d", contractVersion)
	}
	return
}

var (
	// add before diving by gweiFactorBig to take the ceiling.
	gweiCeilAddend = big.NewInt(GweiFactor - 1)
	gweiFactorBig  = big.NewInt(GweiFactor)
)

// GweiToWei converts uint64 Gwei to *big.Int Wei.
func GweiToWei(v uint64) *big.Int {
	return new(big.Int).Mul(big.NewInt(int64(v)), gweiFactorBig)
}

// WeiToGwei converts *big.Int Wei to uint64 Gwei. If v is determined to be
// unsuitable for a uint64, zero is returned. For values that are not even
// multiples of 1 gwei, this function returns the floor.
func WeiToGwei(v *big.Int) uint64 {
	vGwei := new(big.Int).Div(v, gweiFactorBig)
	if vGwei.IsUint64() {
		return vGwei.Uint64()
	}
	return 0
}

// WeiToGweiCeil converts *big.Int Wei to uint64 Gwei. If v is determined to be
// unsuitable for a uint64, zero is returned. For values that are not even
// multiples of 1 gwei, this function returns the ceiling. In general,
// WeiToWeiCeil should be used with gwei-unit fee rates are generated or
// validated and WeiToGwei should be used with balances and values.
func WeiToGweiCeil(v *big.Int) uint64 {
	vGwei := new(big.Int).Div(new(big.Int).Add(v, gweiCeilAddend), big.NewInt(GweiFactor))
	if vGwei.IsUint64() {
		return vGwei.Uint64()
	}
	return 0
}

// WeiToGweiSafe converts a *big.Int in wei (1e18 unit) to gwei (1e9 unit) as
// a uint64. Errors if the amount of gwei is too big to fit fully into a uint64.
// For values that are not even multiples of 1 gwei, this function returns the
// ceiling. As such, WeiToGweiSafe is more suitable for validating or generating
// fee rates. If balance or value validation is the goal, use truncation e.g.
// WeiToGwei.
func WeiToGweiSafe(wei *big.Int) (uint64, error) {
	if wei.Cmp(new(big.Int)) == -1 {
		return 0, fmt.Errorf("wei must be non-negative")
	}
	gwei := new(big.Int).Div(new(big.Int).Add(wei, gweiCeilAddend), gweiFactorBig)
	if !gwei.IsUint64() {
		return 0, fmt.Errorf("%v gwei is too big for a uint64", gwei)
	}
	return gwei.Uint64(), nil
}

// SwapStep is the state of a swap and corresponds to values in the Solidity
// swap contract.
type SwapStep uint8

// Swap states represent the status of a swap. The default state of a swap is
// SSNone. A swap in status SSNone does not exist. SSInitiated indicates that a
// party has initiated the swap and funds have been sent to the contract.
// SSRedeemed indicates a successful swap where the participant was able to
// redeem with the secret hash. SSRefunded indicates a failed swap, where the
// initiating party refunded their coins after the locktime passed. A swap no
// longer changes states after reaching SSRedeemed or SSRefunded.
const (
	// SSNone indicates that the swap is not initiated. This is the default
	// state of a swap.
	SSNone SwapStep = iota
	// SSInitiated indicates that the swap has been initiated.
	SSInitiated
	// SSRedeemed indicates that the swap was initiated and then redeemed.
	// This is one of two possible end states of a swap.
	SSRedeemed
	// SSRefunded indicates that the swap was initiated and then refunded.
	// This is one of two possible end states of a swap.
	SSRefunded
)

// String satisfies the Stringer interface.
func (ss SwapStep) String() string {
	switch ss {
	case SSNone:
		return "none"
	case SSInitiated:
		return "initiated"
	case SSRedeemed:
		return "redeemed"
	case SSRefunded:
		return "refunded"
	}
	return "unknown"
}

// SwapVector is immutable contract data.
type SwapVector struct {
	From       common.Address
	To         common.Address
	Value      *big.Int
	SecretHash [32]byte
	LockTime   uint64 // seconds
}

// Locator encodes a version 1 locator for the SwapVector.
func (v *SwapVector) Locator() []byte {
	locator := make([]byte, LocatorV1Length)
	copy(locator[0:20], v.From[:])
	copy(locator[20:40], v.To[:])
	v.Value.FillBytes(locator[40:72])
	copy(locator[72:104], v.SecretHash[:])
	binary.BigEndian.PutUint64(locator[104:112], v.LockTime)
	return locator
}

func (v *SwapVector) String() string {
	return fmt.Sprintf("{ from = %s, to = %s, value = %d, secret hash = %s, locktime = %s }",
		v.From, v.To, v.Value, hex.EncodeToString(v.SecretHash[:]), time.UnixMilli(int64(v.LockTime)))
}

func CompareVectors(v1, v2 *SwapVector) bool {
	// Check vector equivalence.
	return v1.Value.Cmp(v2.Value) == 0 && v1.To == v2.To && v1.From == v2.From &&
		v1.LockTime == v2.LockTime && v1.SecretHash == v2.SecretHash
}

// SwapStatus is the contract data that specifies the current contract state.
type SwapStatus struct {
	BlockHeight uint64
	Secret      [32]byte
	Step        SwapStep
}

// SwapState is the current state of an in-process swap, as stored on-chain by
// the v0 contract.
type SwapState struct {
	BlockHeight uint64
	LockTime    time.Time
	Secret      [32]byte
	Initiator   common.Address
	Participant common.Address
	Value       *big.Int
	State       SwapStep
}

// Initiation is the data used to initiate a swap.
type Initiation struct {
	LockTime    time.Time
	SecretHash  [32]byte
	Participant common.Address
	Value       *big.Int
}

// Redemption is the data used to redeem a swap.
type Redemption struct {
	Secret     [32]byte
	SecretHash [32]byte
}

var (
	usdcTokenID, _    = util.BipSymbolID("usdc.eth")
	usdtTokenID, _    = util.BipSymbolID("usdt.eth")
	polygonTokenID, _ = util.BipSymbolID("polygon.eth")
	maticTokenID, _   = util.BipSymbolID("matic.eth") // old matic 0x7d1afa7b718fb893db30a3abc0cfc608aacfebb0
	linkTokenID, _    = util.BipSymbolID("link.eth")
)

// Gases lists the expected gas required for token wallet operations.
type Gases struct {
	// Approve is the amount of gas needed to approve a contract for
	// transferring tokens. The first approval for an address uses more gas than
	// subsequent approvals for the same address.
	Approve uint64 `json:"approve"`
	// Transfer is the amount of gas needed to transfer tokens. The first
	// transfer to an address uses more gas than subsequent transfers to the
	// same address.
	Transfer uint64 `json:"transfer"`
}

func ParseV0Locator(locator []byte) (secretHash [32]byte, err error) {
	if len(locator) == SecretHashSize {
		copy(secretHash[:], locator)
	} else {
		err = fmt.Errorf("wrong v0 locator length. wanted %d, got %d", SecretHashSize, len(locator))
	}
	return
}

// LocatorV1Length = from 20 + to 20 + value 32 + secretHash 32 +
// lockTime 8 = 112 bytes
const LocatorV1Length = 112

func ParseV1Locator(locator []byte) (v *SwapVector, err error) {
	// from 20 + to 20 + value 8 + secretHash 32 + lockTime 8
	if len(locator) == LocatorV1Length {
		v = &SwapVector{
			From:     common.BytesToAddress(locator[:20]),
			To:       common.BytesToAddress(locator[20:40]),
			Value:    new(big.Int).SetBytes(locator[40:72]),
			LockTime: binary.BigEndian.Uint64(locator[104:112]),
		}
		copy(v.SecretHash[:], locator[72:104])
	} else {
		err = fmt.Errorf("wrong v1 locator length. wanted %d, got %d", LocatorV1Length, len(locator))
	}
	return
}

// ProtocolVersion assists in mapping the util.Asset.Version to a contract
// version.
type ProtocolVersion uint32

const (
	ProtocolVersionZero ProtocolVersion = iota
	ProtocolVersionV1Contracts
)

func (v ProtocolVersion) ContractVersion() uint32 {
	switch v {
	case ProtocolVersionZero:
		return 0
	case ProtocolVersionV1Contracts:
		return 1
	default:
		return ContractVersionUnknown
	}
}

var (
	// ContractVersionERC20 is passed as the contract version when calling
	// ERC20 contract methods.
	ContractVersionERC20   = ^uint32(0)
	ContractVersionNewest  = ContractVersionERC20 // same thing
	ContractVersionUnknown = ContractVersionERC20 - 1
)
