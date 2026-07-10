// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"math"

	"github.com/btcsuite/btcd/btcutil"
)

// toSatoshi converts a conventional-units amount to its base unit (zatoshi).
func toSatoshi(v float64) uint64 {
	return uint64(math.Round(v * 1e8))
}

// toBTC returns a float representation in conventional units for the base
// units.
func toBTC[V uint64 | int64](v V) float64 {
	return btcutil.Amount(v).ToBTC()
}
