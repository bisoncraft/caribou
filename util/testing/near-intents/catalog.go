// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"fmt"
	"math/big"
	"strings"
)

// chainSpec describes one chain-native asset the mock can serve. Slugs and
// asset IDs must satisfy the wallet's catalog rules (wallet/intents/catalog.go):
// the slug must be in chainBipIDs, and the entry must classify as native —
// empty contractAddress, except NEAR which must be exactly nep141:wrap.near.
// Decimals are the Intents-side scale the wallet converts atoms to and from,
// and are also the base units drivers deal in (sats, yoctoNEAR, lamports,
// wei), so CheckDeposit/Send need no extra scaling.
type chainSpec struct {
	slug             string
	bipID            uint32
	assetID          string
	symbol           string
	decimals         int
	fallbackMicroUSD int64 // static price used until live rates arrive
}

// chainSpecs holds every chain the mock knows how to drive. Only chains
// enabled via Config.Chains are published in /v0/tokens and accepted in
// quotes.
var chainSpecs = map[string]*chainSpec{
	"btc":  {slug: "btc", bipID: 0, assetID: "nep141:btc.omft.near", symbol: "BTC", decimals: 8, fallbackMicroUSD: 60_000_000_000},
	"ltc":  {slug: "ltc", bipID: 2, assetID: "nep141:ltc.omft.near", symbol: "LTC", decimals: 8, fallbackMicroUSD: 80_000_000},
	"doge": {slug: "doge", bipID: 3, assetID: "nep141:doge.omft.near", symbol: "DOGE", decimals: 8, fallbackMicroUSD: 200_000},
	"dash": {slug: "dash", bipID: 5, assetID: "nep141:dash.omft.near", symbol: "DASH", decimals: 8, fallbackMicroUSD: 30_000_000},
	"zec":  {slug: "zec", bipID: 133, assetID: "nep141:zec.omft.near", symbol: "ZEC", decimals: 8, fallbackMicroUSD: 40_000_000},
	"bch":  {slug: "bch", bipID: 145, assetID: "nep141:bch.omft.near", symbol: "BCH", decimals: 8, fallbackMicroUSD: 300_000_000},
	"eth":  {slug: "eth", bipID: 60, assetID: "nep141:eth.omft.near", symbol: "ETH", decimals: 18, fallbackMicroUSD: 3_000_000_000},
	"sol":  {slug: "sol", bipID: 501, assetID: "nep141:sol.omft.near", symbol: "SOL", decimals: 9, fallbackMicroUSD: 150_000_000},
	"near": {slug: "near", bipID: 397, assetID: "nep141:wrap.near", symbol: "NEAR", decimals: 24, fallbackMicroUSD: 5_000_000},
	"pol":  {slug: "pol", bipID: 966, assetID: "nep245:v2_1.omni.hot.tg:137_111", symbol: "POL", decimals: 18, fallbackMicroUSD: 500_000},
}

// tenPow returns 10^n.
func tenPow(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// parseUnits parses a conventional-unit decimal string into base units.
func parseUnits(s string, decimals int) (*big.Int, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > decimals {
		frac = frac[:decimals]
	}
	frac += strings.Repeat("0", decimals-len(frac))
	if whole == "" {
		whole = "0"
	}
	w, ok := new(big.Int).SetString(whole, 10)
	if !ok {
		return nil, fmt.Errorf("bad amount %q", s)
	}
	w.Mul(w, tenPow(decimals))
	if frac != "" {
		f, ok := new(big.Int).SetString(frac, 10)
		if !ok {
			return nil, fmt.Errorf("bad amount %q", s)
		}
		w.Add(w, f)
	}
	return w, nil
}

// formatUnits renders base units as a conventional-unit decimal string,
// keeping at most places fractional digits and trimming trailing zeros.
func formatUnits(x *big.Int, decimals, places int) string {
	q, r := new(big.Int).QuoRem(x, tenPow(decimals), new(big.Int))
	if r.Sign() == 0 {
		return q.String()
	}
	frac := fmt.Sprintf("%0*s", decimals, r.String())
	if places < decimals {
		frac = frac[:places]
	}
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return q.String()
	}
	return q.String() + "." + frac
}

// usdString renders a base-unit amount priced at microUSD-per-conventional-
// unit as a 2-decimal USD string.
func usdString(amt *big.Int, decimals int, microUSD int64) string {
	// cents = amt * microUSD / 10^decimals / 10^4
	cents := new(big.Int).Mul(amt, big.NewInt(microUSD))
	cents.Quo(cents, tenPow(decimals+4))
	q, r := new(big.Int).QuoRem(cents, big.NewInt(100), new(big.Int))
	return fmt.Sprintf("%s.%02d", q.String(), r.Int64())
}
