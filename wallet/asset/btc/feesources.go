// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/bisoncraft/caribou/util/feeratefetcher"
)

// The constructors below build feeratefetcher.SourceConfig values for the
// free, public fee-rate APIs used by the UTXO wallets. Each returns a rate in
// the asset's base atoms-per-byte unit (e.g. sats/vByte for BTC). Callers
// assign a Rank to place the source in a priority group; a lower Rank is a
// higher priority.

// MempoolSpaceSource builds a fee source backed by a mempool.space-style REST
// API. This covers mempool.space (BTC) and its forks such as litecoinspace.org
// (LTC). baseURI is the mainnet API root without a trailing slash, e.g.
// "https://mempool.space".
func MempoolSpaceSource(name, baseURI string, rank uint) *feeratefetcher.SourceConfig {
	uri := baseURI + "/api/v1/fees/recommended"
	return &feeratefetcher.SourceConfig{
		Name:   name,
		Rank:   rank,
		Period: time.Minute * 2, // Rate limit may be around 1 per 10 seconds.
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			var res struct {
				FastestFee uint64 `json:"fastestFee"`
			}
			var code int
			if err := dexnet.Get(ctx, uri, &res, dexnet.WithStatusFunc(func(c int) { code = c })); err != nil {
				if code == http.StatusTooManyRequests {
					return 0, time.Minute * 30, errors.New("exceeded request limit")
				}
				return 0, time.Minute * 2, err
			}
			return res.FastestFee, 0, nil
		},
	}
}

// BlockcypherSource builds a fee source backed by the api.blockcypher.com chain
// endpoint. coin is the lower-case symbol in the API path (btc, ltc, doge,
// dash). The endpoint reports mainnet only.
func BlockcypherSource(coin string, rank uint) *feeratefetcher.SourceConfig {
	uri := fmt.Sprintf("https://api.blockcypher.com/v1/%s/main", coin)
	return &feeratefetcher.SourceConfig{
		Name:   "blockcypher.com",
		Rank:   rank,
		Period: time.Minute * 3, // 100 requests/hr => a request every 0.6 minutes.
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			var res struct {
				MediumPerKB uint64 `json:"medium_fee_per_kb"`
			}
			var code int
			if err := dexnet.Get(ctx, uri, &res, dexnet.WithStatusFunc(func(c int) { code = c })); err != nil {
				if code == http.StatusTooManyRequests {
					return 0, time.Minute * 30, errors.New("exceeded request limit")
				}
				return 0, time.Minute * 10, err
			}
			return uint64(math.Round(float64(res.MediumPerKB) / 1e3)), 0, nil
		},
	}
}

// BitcoreSource builds a fee source backed by the api.bitcore.io API. ticker is
// the upper-case symbol in the API path (BTC, LTC, DOGE, BCH). The reported
// feerate is in whole coins per kB. Like every source in this file, the
// mainnet endpoint serves all networks.
func BitcoreSource(ticker string, rank uint) *feeratefetcher.SourceConfig {
	uri := fmt.Sprintf("https://api.bitcore.io/api/%s/mainnet/fee/1", ticker)
	return &feeratefetcher.SourceConfig{
		Name:   "bitcore.io",
		Rank:   rank,
		Period: time.Minute * 3,
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			var res struct {
				FeeRate float64 `json:"feerate"`
			}
			if err := dexnet.Get(ctx, uri, &res); err != nil {
				return 0, time.Minute * 10, err
			}
			if res.FeeRate <= 0 {
				return 0, time.Minute * 10, errors.New("zero or negative fee rate")
			}
			// coins/kB * 1e8 atoms/coin / 1e3 B/kB = coins/kB * 1e5.
			return uint64(math.Round(res.FeeRate * 1e5)), 0, nil
		},
	}
}

// BlockchairSource builds a fee source backed by the api.blockchair.com stats
// endpoint. chain is the chain name in the API path (bitcoin, litecoin,
// dogecoin, dash, bitcoin-cash, zcash). It occasionally reports zero, so it is
// best used as a low-priority source.
func BlockchairSource(chain string, rank uint) *feeratefetcher.SourceConfig {
	uri := fmt.Sprintf("https://api.blockchair.com/%s/stats", chain)
	return &feeratefetcher.SourceConfig{
		Name:   "blockchair.com",
		Rank:   rank,
		Period: time.Minute * 3, // 1440 requests/day => a request every minute.
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			var res struct {
				Data struct {
					SatsPerByte uint64 `json:"suggested_transaction_fee_per_byte_sat"`
				} `json:"data"`
			}
			var code int
			if err := dexnet.Get(ctx, uri, &res, dexnet.WithStatusFunc(func(c int) { code = c })); err != nil {
				switch code {
				case http.StatusTooManyRequests, http.StatusPaymentRequired:
					return 0, time.Minute * 30, errors.New("exceeded request limit")
				case http.StatusServiceUnavailable, 430, 434:
					return 0, time.Hour * 24, errors.New("banned from api")
				}
				return 0, time.Minute * 10, err
			}
			return res.Data.SatsPerByte, 0, nil
		},
	}
}

// BitcoinerLiveSource builds a fee source backed by the bitcoiner.live API.
// This source is BTC mainnet only.
func BitcoinerLiveSource(rank uint) *feeratefetcher.SourceConfig {
	return &feeratefetcher.SourceConfig{
		Name:   "bitcoiner.live",
		Rank:   rank,
		Period: time.Minute * 5, // Data is refreshed every 5 minutes.
		F: func(ctx context.Context) (uint64, time.Duration, error) {
			const uri = "https://bitcoiner.live/api/fees/estimates/latest"
			var res struct {
				Estimates map[string]struct {
					SatsPerVB float64 `json:"sat_per_vbyte"`
				} `json:"estimates"`
			}
			if err := dexnet.Get(ctx, uri, &res); err != nil {
				return 0, time.Minute * 10, err
			}
			// Using the 30-minute estimate. 60, 120 and higher are also offered.
			r, found := res.Estimates["30"]
			if !found {
				return 0, time.Minute * 10, errors.New("no 30-minute estimate returned")
			}
			return uint64(math.Round(r.SatsPerVB)), 0, nil
		},
	}
}

// BitcoinFeeSources returns the ranked fee-rate sources for BTC. The mainnet
// sources are used on every network: on test networks a mainnet fee rate is a
// reasonable proxy and avoids maintaining separate, less-reliable test
// endpoints. bitcoiner.live reports a 30-minute-target rate, so it is ranked
// below the next-block/medium sources rather than averaged with them.
func BitcoinFeeSources() []*feeratefetcher.SourceConfig {
	return []*feeratefetcher.SourceConfig{
		MempoolSpaceSource("mempool.space", "https://mempool.space", 1),
		BlockcypherSource("btc", 1),
		BitcoinerLiveSource(2),
		BlockchairSource("bitcoin", 3),
	}
}
