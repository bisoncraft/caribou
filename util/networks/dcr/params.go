// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dcr

import "github.com/bisoncraft/meshwallet/util"

var UnitInfo = util.UnitInfo{
	AtomicUnit: "atoms",
	Conventional: util.Denomination{
		Unit:             "DCR",
		ConversionFactor: 1e8,
	},
	Alternatives: []util.Denomination{
		{
			Unit:             "mDCR",
			ConversionFactor: 1e5,
		},
		{
			Unit:             "µDCR",
			ConversionFactor: 1e2,
		},
	},
	FeeRateDenom: "B",
}
