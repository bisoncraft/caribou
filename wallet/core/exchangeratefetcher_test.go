// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
)

// expireRateSource installs a source whose only rate is old enough that
// isExpired reports it, which is the state removeExpiredRateSources acts on.
func expireRateSource(tCore *Core, token string) {
	source := newCommonRateSource(tFetcher)
	source.fiatRates[tUTXOAssetA.ID] = &fiatRateInfo{
		rate:       45,
		lastUpdate: time.Now().Add(-2 * fiatRateDataExpiry),
	}
	tCore.fiatRateSources[token] = source
}

// TestExpiredRateSourceIsRetriedNotPersisted covers the recovery path for a
// source that goes dark: it is removed, but neither written to the database nor
// left removed for good.
func TestExpiredRateSourceIsRetriedNotPersisted(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	expireRateSource(tCore, coinpaprika)
	rig.db.savedDisabledRateSources = nil

	tCore.removeExpiredRateSources()

	if tCore.fiatRateSources[coinpaprika] != nil {
		t.Fatal("expired source was not removed")
	}
	if _, auto := tCore.autoDisabledRateSources[coinpaprika]; !auto {
		t.Fatal("expired source was not recorded for retry")
	}
	if rig.db.savedDisabledRateSources != nil {
		t.Fatalf("expiry persisted a disable: %v", rig.db.savedDisabledRateSources)
	}

	// Still inside the retry interval.
	tCore.retryAutoDisabledRateSources()
	if tCore.fiatRateSources[coinpaprika] != nil {
		t.Fatal("source retried before the retry interval elapsed")
	}

	// Past the retry interval.
	tCore.autoDisabledRateSources[coinpaprika] = time.Now().Add(-fiatRateSourceRetryInterval - time.Second)
	tCore.retryAutoDisabledRateSources()
	if tCore.fiatRateSources[coinpaprika] == nil {
		t.Fatal("source was not retried after the retry interval elapsed")
	}
	if len(tCore.autoDisabledRateSources) != 0 {
		t.Fatalf("retried source left an entry behind: %v", tCore.autoDisabledRateSources)
	}
}

// TestDisableBeatsExpiryRetry covers a user disabling a source while it is
// awaiting a retry. The explicit choice has to win, or the retry resurrects a
// source the user turned off.
func TestDisableBeatsExpiryRetry(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	expireRateSource(tCore, coinpaprika)
	tCore.removeExpiredRateSources()

	if err := tCore.ToggleRateSourceStatus(coinpaprika, true); err != nil {
		t.Fatalf("disable error: %v", err)
	}

	if _, auto := tCore.autoDisabledRateSources[coinpaprika]; auto {
		t.Fatal("disable did not take the source back from the retry")
	}
	if !slicesContain(rig.db.savedDisabledRateSources, coinpaprika) {
		t.Fatalf("disable was not persisted, saved: %v", rig.db.savedDisabledRateSources)
	}

	tCore.retryAutoDisabledRateSources()
	if tCore.fiatRateSources[coinpaprika] != nil {
		t.Fatal("retry resurrected a source the user disabled")
	}
}

// TestEnableBeatsExpiryRetry covers a user re-enabling a source while it is
// awaiting a retry.
func TestEnableBeatsExpiryRetry(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	expireRateSource(tCore, coinpaprika)
	tCore.removeExpiredRateSources()

	if err := tCore.ToggleRateSourceStatus(coinpaprika, false); err != nil {
		t.Fatalf("enable error: %v", err)
	}

	if tCore.fiatRateSources[coinpaprika] == nil {
		t.Fatal("source was not enabled")
	}
	if _, auto := tCore.autoDisabledRateSources[coinpaprika]; auto {
		t.Fatal("enable did not take the source back from the retry")
	}
	if slicesContain(rig.db.savedDisabledRateSources, coinpaprika) {
		t.Fatalf("enabled source was persisted as disabled: %v", rig.db.savedDisabledRateSources)
	}
}

// TestExpiredSourceNotPersistedByUnrelatedToggle is the regression that makes
// the retry meaningful: saveDisabledRateSources derives its list from the
// absence of a source, so without an explicit skip a source awaiting retry
// would be written to the database as disabled the next time any other source
// was toggled, making a transient outage permanent.
func TestExpiredSourceNotPersistedByUnrelatedToggle(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	expireRateSource(tCore, chainlink)
	tCore.fiatRateSources[coinpaprika] = newCommonRateSource(tFetcher)
	tCore.removeExpiredRateSources()

	// Toggle an unrelated source, forcing a save.
	if err := tCore.ToggleRateSourceStatus(coinpaprika, true); err != nil {
		t.Fatalf("disable error: %v", err)
	}

	if slicesContain(rig.db.savedDisabledRateSources, chainlink) {
		t.Fatalf("source awaiting retry was persisted as disabled: %v", rig.db.savedDisabledRateSources)
	}
	if !slicesContain(rig.db.savedDisabledRateSources, coinpaprika) {
		t.Fatalf("user-disabled source was not persisted: %v", rig.db.savedDisabledRateSources)
	}
}

func slicesContain(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Asset IDs and symbols mirror util/bip-id.go. usdcEthID and usdcPolygonID both
// carry the USDC ticker, which is the real case for a ticker held by more than
// one asset. baseID's symbol is "base" rather than an eth-suffixed token
// symbol, so it resolves to a ticker with no Chainlink feed.
const (
	btcID         uint32 = 0
	dcrID         uint32 = 42
	ethID         uint32 = 60
	baseID        uint32 = 8453
	usdcEthID     uint32 = 60001
	usdcPolygonID uint32 = 966001
)

func tChainlinkAssets() map[uint32]*SupportedAsset {
	return map[uint32]*SupportedAsset{
		btcID:         {ID: btcID, Symbol: "btc", Name: "Bitcoin"},
		dcrID:         {ID: dcrID, Symbol: "dcr", Name: "Decred"},
		ethID:         {ID: ethID, Symbol: "eth", Name: "Ethereum"},
		baseID:        {ID: baseID, Symbol: "base", Name: "Base"},
		usdcEthID:     {ID: usdcEthID, Symbol: "usdc.eth", Name: "USDC"},
		usdcPolygonID: {ID: usdcPolygonID, Symbol: "usdc.polygon", Name: "USDC"},
	}
}

// TestChainlinkRatesLive exercises the whole fetcher against the live Polygon
// feeds over the public RPC providers. Skipped in CI, which runs -short.
func TestChainlinkRatesLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live test")
	}

	assets := tChainlinkAssets()
	rates := FetchChainlinkRates(context.Background(), tLogger, assets)
	if len(rates) == 0 {
		t.Fatal("no rates returned")
	}
	for assetID, rate := range rates {
		if rate <= 0 {
			t.Errorf("asset %d: non-positive rate %f", assetID, rate)
		}
		t.Logf("asset %d (%s): %f", assetID, assets[assetID].Symbol, rate)
	}
	// These feeds carry a 27s heartbeat and so are always well inside their
	// staleness bound. A feed whose provider is briefly unreachable simply
	// drops out of a cycle, but that would have to hit the whole batch, which
	// the len check above already covers.
	//
	// usdc.eth and usdc.polygon both resolve to the USDC ticker, so one feed
	// prices both, which is what makes them worth asserting here.
	for _, assetID := range []uint32{btcID, ethID, usdcEthID, usdcPolygonID} {
		if _, has := rates[assetID]; !has {
			t.Errorf("no rate for asset %d", assetID)
		}
	}
	// USDC has a feed shared by all its deployments' asset IDs.
	if rates[usdcEthID] != rates[usdcPolygonID] {
		t.Errorf("usdc.eth rate %f != usdc.polygon rate %f", rates[usdcEthID], rates[usdcPolygonID])
	}
	// dcr and base have no Chainlink feed.
	for _, assetID := range []uint32{dcrID, baseID} {
		if _, has := rates[assetID]; has {
			t.Errorf("got a rate for asset %d, which has no feed", assetID)
		}
	}
}

func TestChainlinkRates(t *testing.T) {
	// Every asset's ticker, deduplicated: usdc.eth and usdc.polygon collapse to
	// a single USDC entry. The full set is passed to the fetcher, which drops
	// the tickers it has no feed for.
	wantTickers := []string{"BASE", "BTC", "DCR", "ETH", "USDC"}

	tests := []struct {
		name     string
		rates    map[string]float64
		fetchErr error
		want     map[uint32]float64
	}{{
		name:  "a ticker fans out to every asset that shares it",
		rates: map[string]float64{"BTC": 100_000, "USDC": 1.0001},
		want: map[uint32]float64{
			btcID:         100_000,
			usdcEthID:     1.0001,
			usdcPolygonID: 1.0001,
		},
	}, {
		name:  "assets with no feed are simply absent",
		rates: map[string]float64{"BTC": 100_000, "ETH": 4_000},
		want:  map[uint32]float64{btcID: 100_000, ethID: 4_000},
	}, {
		name:  "no rates yields no conversions",
		rates: map[string]float64{},
		want:  map[uint32]float64{},
	}, {
		name:     "a fetch error is swallowed",
		fetchErr: errors.New("dial all providers failed"),
		want:     nil,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotTickers []string
			fetchRates := func(_ context.Context, tickers []string, _ util.Logger) (map[string]float64, error) {
				gotTickers = tickers
				return tt.rates, tt.fetchErr
			}

			got := chainlinkRates(context.Background(), tLogger, tChainlinkAssets(), fetchRates)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got rates %v, want %v", got, tt.want)
			}

			sort.Strings(gotTickers)
			if !reflect.DeepEqual(gotTickers, wantTickers) {
				t.Errorf("got tickers %v, want %v", gotTickers, wantTickers)
			}
		})
	}
}

// TestInitFiatRateSourcesIsSynchronized covers the startup window: Run
// populates the rate sources after closing c.ready, so the app server is
// already answering requests — GET /api/user calls FiatRateSources — while the
// map is being written. Unsynchronized, that pair is a fatal concurrent map
// read and write, which no Recoverer can catch, so this must stay race-clean.
func TestInitFiatRateSourcesIsSynchronized(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			tCore.FiatRateSources()
		}
	}()
	tCore.initFiatRateSources([]string{coinpaprika})
	wg.Wait()

	// Disabled sources stay out; everything else is enabled.
	for token := range fiatRateFetchers {
		got := tCore.fiatRateSources[token] != nil
		want := token != coinpaprika
		if got != want {
			t.Fatalf("source %s enabled = %v, want %v", token, got, want)
		}
	}
}
