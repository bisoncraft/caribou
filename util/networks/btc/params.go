// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import "github.com/bisoncraft/meshwallet/util"

var UnitInfo = util.UnitInfo{
	AtomicUnit: "Sats",
	Conventional: util.Denomination{
		Unit:             "BTC",
		ConversionFactor: 1e8,
	},
	Alternatives: []util.Denomination{
		{
			Unit:             "mBTC",
			ConversionFactor: 1e5,
		},
		{
			Unit:             "µBTC",
			ConversionFactor: 1e2,
		},
	},
	FeeRateDenom: "vB",
}
