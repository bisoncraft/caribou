// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"math/big"

	"github.com/bisoncraft/meshwallet/util"
)

const (
	BipID = 397
	// DefaultFee is the default transfer fee in drops. A basic NEAR transfer
	// costs approximately 0.00045 NEAR = 45000 drops.
	DefaultFee = 45_000
	// MaxBlockInterval is the number of seconds since the last header came
	// in over which we consider the chain to be out of sync.
	MaxBlockInterval = 30
)

var (
	UnitInfo = util.UnitInfo{
		AtomicUnit: "drops",
		Conventional: util.Denomination{
			Unit:             "NEAR",
			ConversionFactor: 1e8,
		},
		Alternatives: []util.Denomination{
			{
				Unit:             "mNEAR",
				ConversionFactor: 1e5,
			},
		},
	}

	// YoctoPerDrop is the number of yoctoNEAR per drop.
	// 1 NEAR = 1e24 yoctoNEAR = 1e8 drops, so 1 drop = 1e16 yoctoNEAR.
	YoctoPerDrop = new(big.Int).SetUint64(1e16)

	DefaultRPCEndpoints = map[util.Network]string{
		util.Mainnet: "https://rpc.mainnet.near.org",
		util.Testnet: "https://rpc.testnet.near.org",
		util.Simnet:  "https://rpc.testnet.near.org",
	}
)

// DropsToYocto converts drops (atomic units) to yoctoNEAR.
func DropsToYocto(drops uint64) *big.Int {
	return new(big.Int).Mul(new(big.Int).SetUint64(drops), YoctoPerDrop)
}

// YoctoToDrops converts yoctoNEAR to drops (atomic units), truncating.
func YoctoToDrops(yocto *big.Int) uint64 {
	return new(big.Int).Div(yocto, YoctoPerDrop).Uint64()
}
