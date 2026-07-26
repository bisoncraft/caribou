// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/bisoncraft/caribou/util/fiatrates"
)

const (
	// DefaultFiatCurrency is the currency for displaying assets fiat value.
	DefaultFiatCurrency = "USD"
	// fiatRateRequestInterval is the amount of time between calls to the exchange API.
	fiatRateRequestInterval = 12 * time.Minute
	// fiatRateDataExpiry : Any data older than fiatRateDataExpiry will be discarded.
	fiatRateDataExpiry = 60 * time.Minute
	// fiatRateSourceRetryInterval is how long a rate source removed for serving
	// only expired data is left alone before it is rebuilt and retried. A
	// source reaches that state by going dark after having worked, which is an
	// outage rather than a statement about the source, so the removal has to be
	// temporary.
	fiatRateSourceRetryInterval = time.Hour
	fiatRequestTimeout          = time.Second * 5
	// chainlinkRequestTimeout bounds a full Chainlink refresh. It is much
	// larger than fiatRequestTimeout because a cold cycle may probe several
	// providers in sequence, each at up to chainlinkDialTimeout, before the
	// batch read: black-holed providers burn their full probe timeout. It
	// still has to stay well inside fiatRateRequestInterval, since
	// refreshFiatRates waits on every source.
	chainlinkRequestTimeout = time.Second * 30

	// Tokens. Used to identify fiat rate source, source name must not contain a
	// comma.
	messari       = "Messari"
	coinpaprika   = "Coinpaprika"
	dcrdataDotOrg = "dcrdata"
	chainlink     = "Chainlink"
)

var (
	dcrDataURL = "https://dcrdata.decred.org/api/exchangerate"
	// The best info I can find on Messari says
	//    Without an API key requests are rate limited to 20 requests per minute
	//    and 1000 requests per day.
	// For a
	// fiatRateRequestInterval of 12 minutes, to hit 20 requests per minute, we
	// would need to have 20 * 12 = 480 assets. To hit 1000 requests per day,
	// we would need 12 * 60 / (86,400 / 1000) = 8.33 assets. Very likely. So
	// we're in a similar position to coinpaprika here too.
	messariURL  = "https://data.messari.io/api/v1/assets/%s/metrics/market-data"
	btcBipID, _ = util.BipSymbolID("btc")
	dcrBipID, _ = util.BipSymbolID("dcr")
)

// fiatRateFetchers is the list of all supported fiat rate fetchers.
var fiatRateFetchers = map[string]rateFetcher{
	coinpaprika:   FetchCoinpaprikaRates,
	dcrdataDotOrg: FetchDcrdataRates,
	chainlink:     FetchChainlinkRates,
	// TODO: messari is temporarily disabled until it is decided to procure an API Key.
	// messari:       FetchMessariRates,
}

// chainlinkFetcher constructs the ticker-keyed Chainlink fetcher on first use.
// The fetcher caches an RPC connection and per-feed decimals internally, so it
// is built once and shared across refreshes. Construction only fails on invalid
// configuration, so a failure is permanent and is not retried.
var chainlinkFetcher = sync.OnceValues(func() (func(context.Context, []string, util.Logger) (map[string]float64, error), error) {
	return fiatrates.ChainlinkRateFetcher(fiatrates.Config{})
})

// fiatRateInfo holds the fiat rate and the last update time for an
// asset.
type fiatRateInfo struct {
	rate       float64
	lastUpdate time.Time
}

// rateFetcher can fetch fiat rates for assets from an API.
type rateFetcher func(context context.Context, logger util.Logger, assets map[uint32]*SupportedAsset) map[uint32]float64

type commonRateSource struct {
	fetchRates rateFetcher

	mtx       sync.RWMutex
	fiatRates map[uint32]*fiatRateInfo
}

// isExpired checks the last update time for all fiat rates against the
// provided expiryTime. This only returns true if all rates are expired.
func (source *commonRateSource) isExpired(expiryTime time.Duration) bool {
	now := time.Now()

	source.mtx.RLock()
	defer source.mtx.RUnlock()
	if len(source.fiatRates) == 0 {
		return false
	}
	for _, rateInfo := range source.fiatRates {
		if now.Sub(rateInfo.lastUpdate) < expiryTime {
			return false // one not expired is enough
		}
	}
	return true
}

// assetRate returns the fiat rate information for the assetID specified. The
// fiatRateInfo returned should not be modified by the caller.
func (source *commonRateSource) assetRate(assetID uint32) *fiatRateInfo {
	source.mtx.RLock()
	defer source.mtx.RUnlock()
	return source.fiatRates[assetID]
}

// refreshRates updates the last update time and the rate information for assets.
func (source *commonRateSource) refreshRates(ctx context.Context, logger util.Logger, assets map[uint32]*SupportedAsset) {
	fiatRates := source.fetchRates(ctx, logger, assets)
	now := time.Now()
	source.mtx.Lock()
	defer source.mtx.Unlock()
	for assetID, fiatRate := range fiatRates {
		if fiatRate <= 0 {
			continue
		}
		source.fiatRates[assetID] = &fiatRateInfo{
			rate:       fiatRate,
			lastUpdate: now,
		}
	}
}

// Used to initialize a fiat rate source.
func newCommonRateSource(fetcher rateFetcher) *commonRateSource {
	return &commonRateSource{
		fetchRates: fetcher,
		fiatRates:  make(map[uint32]*fiatRateInfo),
	}
}

// FetchCoinpaprikaRates retrieves and parses fiat rate data from the
// Coinpaprika API. See https://api.coinpaprika.com/#operation/getTickersById
// for sample request and response information.
func FetchCoinpaprikaRates(ctx context.Context, log util.Logger, assets map[uint32]*SupportedAsset) map[uint32]float64 {
	coinpapAssets := make([]*fiatrates.CoinpaprikaAsset, 0, len(assets) /* too small cuz tokens*/)
	for assetID, a := range assets {
		coinpapAssets = append(coinpapAssets, &fiatrates.CoinpaprikaAsset{
			AssetID: assetID,
			Name:    a.Name,
			Symbol:  a.Symbol,
		})
	}
	return fiatrates.FetchCoinpaprikaRates(ctx, coinpapAssets, log)
}

// FetchChainlinkRates retrieves USD rates from Chainlink Data Feeds. Chainlink
// is keyed on ticker rather than asset ID, so assets are collapsed to tickers
// on the way in and the resulting rates are fanned back out on the way out. A
// ticker may carry more than one asset ID, since an asset and its bridged
// representations on other chains share a ticker and therefore a price. Assets
// with no configured feed are dropped by the fetcher, which is expected: the
// feed set is much smaller than the supported asset set.
func FetchChainlinkRates(ctx context.Context, log util.Logger, assets map[uint32]*SupportedAsset) map[uint32]float64 {
	fetchRates, err := chainlinkFetcher()
	if err != nil {
		log.Errorf("Error building chainlink fiat rate fetcher: %v", err)
		return nil
	}
	return chainlinkRates(ctx, log, assets, fetchRates)
}

// chainlinkRates translates between core's asset IDs and a ticker-keyed
// Chainlink fetcher. The fetcher is a parameter so the translation can be
// exercised without an RPC connection.
func chainlinkRates(ctx context.Context, log util.Logger, assets map[uint32]*SupportedAsset,
	fetchRates func(context.Context, []string, util.Logger) (map[string]float64, error)) map[uint32]float64 {

	tickerAssets := make(map[string][]uint32, len(assets))
	for assetID, sa := range assets {
		ticker := fiatrates.ParseTicker(util.TokenSymbol(sa.Symbol))
		tickerAssets[ticker] = append(tickerAssets[ticker], assetID)
	}

	tickers := make([]string, 0, len(tickerAssets))
	for ticker := range tickerAssets {
		tickers = append(tickers, ticker)
	}

	ctx, cancel := context.WithTimeout(ctx, chainlinkRequestTimeout)
	defer cancel()

	rates, err := fetchRates(ctx, tickers, log)
	if err != nil {
		log.Errorf("Error getting fiat exchange rates from chainlink: %v", err)
		return nil
	}

	fiatRates := make(map[uint32]float64, len(rates))
	for ticker, rate := range rates {
		for _, assetID := range tickerAssets[ticker] {
			fiatRates[assetID] = rate
		}
	}
	return fiatRates
}

// FetchDcrdataRates retrieves and parses fiat rate data from dcrdata
// exchange rate API.
func FetchDcrdataRates(ctx context.Context, log util.Logger, assets map[uint32]*SupportedAsset) map[uint32]float64 {
	assetBTC := assets[btcBipID]
	assetDCR := assets[dcrBipID]
	noBTCAsset := assetBTC == nil || assetBTC.Wallet == nil
	noDCRAsset := assetDCR == nil || assetDCR.Wallet == nil
	if noBTCAsset && noDCRAsset {
		return nil
	}

	fiatRates := make(map[uint32]float64)
	res := new(struct {
		DcrPrice float64 `json:"dcrPrice"`
		BtcPrice float64 `json:"btcPrice"`
	})

	if err := getRates(ctx, dcrDataURL, res); err != nil {
		log.Error("error getting prices from dcrdata: %w", err)
		return nil
	}

	if !noBTCAsset {
		fiatRates[btcBipID] = res.BtcPrice
	}
	if !noDCRAsset {
		fiatRates[dcrBipID] = res.DcrPrice
	}

	return fiatRates
}

// FetchMessariRates retrieves and parses fiat rate data from the Messari API.
// See https://messari.io/api/docs#operation/Get%20Asset%20Market%20Data for
// sample request and response information.
// TODO: Requires an API Key, temporarily disabled.
func FetchMessariRates(ctx context.Context, log util.Logger, assets map[uint32]*SupportedAsset) map[uint32]float64 {
	fiatRates := make(map[uint32]float64)
	fetchRate := func(sa *SupportedAsset) {
		assetID := sa.ID
		if sa.Wallet == nil {
			// we don't want to fetch rate for assets with no wallet.
			return
		}

		res := new(struct {
			Data struct {
				MarketData struct {
					Price float64 `json:"price_usd"`
				} `json:"market_data"`
			} `json:"data"`
		})

		slug := util.TokenSymbol(sa.Symbol)
		reqStr := fmt.Sprintf(messariURL, slug)

		ctx, cancel := context.WithTimeout(ctx, fiatRequestTimeout)
		defer cancel()

		if err := getRates(ctx, reqStr, res); err != nil {
			log.Errorf("Error getting fiat exchange rates from messari: %v", err)
			return
		}

		fiatRates[assetID] = res.Data.MarketData.Price
	}

	for _, sa := range assets {
		fetchRate(sa)
	}
	return fiatRates
}

func getRates(ctx context.Context, uri string, thing any) error {
	return dexnet.Get(ctx, uri, thing, dexnet.WithSizeLimit(1<<22))
}
