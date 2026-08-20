package fiatrates

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
)

// roundTripFunc lets a plain function satisfy http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

// mockHTTP replaces dexnet.Client for the duration of a test. The handler
// receives each request and returns a value to JSON-encode as the response
// body plus an HTTP status code. The caller must call the returned restore
// function (typically via defer).
func mockHTTP(t *testing.T, handler func(*http.Request) (any, int)) (restore func()) {
	t.Helper()
	old := dexnet.Client
	dexnet.Client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, status := handler(req)
			b, err := json.Marshal(body)
			if err != nil {
				t.Errorf("mockHTTP: marshal error: %v", err)
			}
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(bytes.NewReader(b)),
				Header:     make(http.Header),
			}, nil
		}),
	}
	return func() { dexnet.Client = old }
}

var tLog = util.StdOutLogger("TEST", util.LevelTrace)

// ---- pure-function tests -----------------------------------------------

func TestParseCoinpapNameSymbol(t *testing.T) {
	tests := []struct {
		name, symbol string
		wantName     string
		wantSymbol   string
	}{
		{"usd coin", "usdc", "usd-coin", "usdc"},
		{"usd coin", "usdc.eth", "usd-coin", "usdc"},
		{"usd coin", "usdc.polygon", "usd-coin", "usdc"},
		{"polygon", "polygon", "polygon", "matic"},
		{"weth", "weth", "weth", "weth"},
		{"matic", "matic", "matic", "matic"},
		{"bitcoin", "btc", "bitcoin", "btc"},
		{"chainlink", "link", "chainlink", "link"},
		{"Ripple", "xrp", "xrp", "xrp"},
		{"USDT", "usdt.sol", "tether", "usdt"},
	}
	for _, tc := range tests {
		gotName, gotSymbol := parseCoinpapNameSymbol(tc.name, tc.symbol)
		if gotName != tc.wantName || gotSymbol != tc.wantSymbol {
			t.Errorf("parseCoinpapNameSymbol(%q, %q) = (%q, %q), want (%q, %q)",
				tc.name, tc.symbol, gotName, gotSymbol, tc.wantName, tc.wantSymbol)
		}
	}
}

func TestParseTicker(t *testing.T) {
	tests := []struct {
		ticker string
		want   string
	}{
		{"polygon", "MATIC"},
		{"POLYGON", "MATIC"},
		{"usdc.eth", "USDC"},
		{"USDC.ETH", "USDC"},
		{"usdc.polygon", "USDC"},
		{"btc", "BTC"},
		{"dcr", "DCR"},
		{"eth", "ETH"},
	}
	for _, tc := range tests {
		got := ParseTicker(tc.ticker)
		if got != tc.want {
			t.Errorf("ParseTicker(%q) = %q, want %q", tc.ticker, got, tc.want)
		}
	}
}

func TestParseTickers(t *testing.T) {
	got := parseTickers("btc", "eth", "dcr")
	want := "BTC,ETH,DCR"
	if got != want {
		t.Errorf("parseTickers = %q, want %q", got, want)
	}
}

func TestCoinpapSlug(t *testing.T) {
	tests := []struct {
		name, symbol string
		want         string
	}{
		{"bitcoin", "btc", "btc-bitcoin"},
		{"polygon", "polygon", "matic-polygon"},
		{"usd coin", "usdc", "usdc-usd-coin"},
		{"bitcoin cash", "bch", "bch-bitcoin-cash"},
		// Coinpaprika's real ID is "xrp-xrp", not "xrp-ripple" — this asset's
		// registered display name ("Ripple", wallet/asset/xrp/xrp.go) must not
		// leak into the slug, or the rate lookup silently never matches.
		{"Ripple", "xrp", "xrp-xrp"},
		// HyperEVM's native asset is HYPE, the same token hyperliquid already
		// resolves to - without this case the registered symbol/name
		// ("hyperevm"/"HyperEVM") produce "hyperevm-hyperevm", which matches
		// no real Coinpaprika ticker.
		{"Hyperliquid", "hype", "hype-hyperliquid"},
		{"HyperEVM", "hyperevm", "hype-hyperliquid"},
	}
	for _, tc := range tests {
		got := CoinpapSlug(tc.name, tc.symbol)
		if got != tc.want {
			t.Errorf("CoinpapSlug(%q, %q) = %q, want %q", tc.name, tc.symbol, got, tc.want)
		}
	}
}

func TestParseBinanceTickers(t *testing.T) {
	got := parseBinanceTickers([]string{"btc", "eth", "dcr"})
	want := `"BTCUSDT","ETHUSDT","DCRUSDT"`
	if got != want {
		t.Errorf("parseBinanceTickers = %q, want %q", got, want)
	}

	// Empty input should return empty string.
	if got := parseBinanceTickers(nil); got != "" {
		t.Errorf("parseBinanceTickers(nil) = %q, want empty", got)
	}
}

func TestAllFiatSourceDisabled(t *testing.T) {
	all := cryptoCompare + "," + kuCoin + "," + binance + "," + coinpaprika + "," + messari + "," + chainlink
	allCfg := Config{DisabledFiatSources: all}
	if !allCfg.AllFiatSourceDisabled() {
		t.Error("expected all sources to be disabled")
	}
	partialCfg := Config{DisabledFiatSources: kuCoin}
	if partialCfg.AllFiatSourceDisabled() {
		t.Error("expected not all sources to be disabled when only kuCoin is disabled")
	}
}

// ---- source method tests -----------------------------------------------

func TestSourceHasTicker(t *testing.T) {
	s := &source{}
	if s.hasTicker() {
		t.Error("new source should have no tickers")
	}
	s.rates = map[string]float64{"BTC": 1}
	if !s.hasTicker() {
		t.Error("source with rates should have tickers")
	}
}

func TestSourceIsExpired(t *testing.T) {
	s := &source{lastRefresh: time.Now()}
	if s.isExpired() {
		t.Error("recently refreshed source should not be expired")
	}
	s.lastRefresh = time.Now().Add(-2 * FiatRateDataExpiry)
	if !s.isExpired() {
		t.Error("old source should be expired")
	}
}

func TestSourceDeactivate(t *testing.T) {
	s := &source{rates: map[string]float64{"BTC": 1}}
	s.deactivate()
	if !s.disabled {
		t.Error("deactivated source should be disabled")
	}
	if s.rates != nil {
		t.Error("deactivated source should have nil rates")
	}
}

func TestSourceCheckIfSourceCanReactivate(t *testing.T) {
	// Not reactivatable: canReactivate is false.
	s := &source{disabled: true, canReactivate: false, lastRefresh: time.Now().Add(-2 * reactivateDuration)}
	if s.checkIfSourceCanReactivate() {
		t.Error("source with canReactivate=false should not reactivate")
	}

	// Not reactivatable: lastRefresh is zero.
	s = &source{disabled: true, canReactivate: true}
	if s.checkIfSourceCanReactivate() {
		t.Error("source with zero lastRefresh should not reactivate")
	}

	// Not reactivatable: not enough time has passed.
	s = &source{disabled: true, canReactivate: true, lastRefresh: time.Now()}
	if s.checkIfSourceCanReactivate() {
		t.Error("recently disabled source should not reactivate")
	}

	// Reactivatable: all conditions met.
	s = &source{disabled: true, canReactivate: true, lastRefresh: time.Now().Add(-2 * reactivateDuration)}
	if !s.checkIfSourceCanReactivate() {
		t.Error("source should reactivate after reactivateDuration")
	}
	if s.disabled {
		t.Error("reactivated source should no longer be disabled")
	}
}

// ---- HTTP-dependent tests ----------------------------------------------

// coinpaprika response shape used by FetchCoinpaprikaRates (id-based).
type coinpaprikaIDResp []struct {
	ID     string `json:"id"`
	Quotes struct {
		USD struct {
			Price float64 `json:"price"`
		} `json:"USD"`
	} `json:"quotes"`
}

// coinpaprika response shape used by the sources.go closure (symbol-based).
type coinpaprikaSymResp []struct {
	Symbol string `json:"symbol"`
	Quotes struct {
		USD struct {
			Price float64 `json:"price"`
		} `json:"USD"`
	} `json:"quotes"`
}

func TestFetchCoinpaprikaRates(t *testing.T) {
	resp := coinpaprikaIDResp{
		{ID: "btc-bitcoin"},
		{ID: "dcr-decred"},
	}
	resp[0].Quotes.USD.Price = 50000
	resp[1].Quotes.USD.Price = 15

	restore := mockHTTP(t, func(*http.Request) (any, int) {
		return resp, http.StatusOK
	})
	defer restore()

	assets := []*CoinpaprikaAsset{
		{AssetID: 0, Name: "bitcoin", Symbol: "btc"},
		{AssetID: 42, Name: "decred", Symbol: "dcr"},
	}
	rates := FetchCoinpaprikaRates(context.Background(), assets, tLog)

	if rates[0] != 50000 {
		t.Errorf("BTC rate = %v, want 50000", rates[0])
	}
	if rates[42] != 15 {
		t.Errorf("DCR rate = %v, want 15", rates[42])
	}
}

func TestFetchCoinpaprikaRatesHTTPError(t *testing.T) {
	restore := mockHTTP(t, func(*http.Request) (any, int) {
		return nil, http.StatusInternalServerError
	})
	defer restore()

	rates := FetchCoinpaprikaRates(context.Background(), []*CoinpaprikaAsset{
		{AssetID: 0, Name: "bitcoin", Symbol: "btc"},
	}, tLog)
	if len(rates) != 0 {
		t.Errorf("expected empty rates on HTTP error, got %v", rates)
	}
}

func TestFetchCoinpaprikaRatesZeroPrice(t *testing.T) {
	resp := coinpaprikaIDResp{{ID: "btc-bitcoin"}} // price is zero
	restore := mockHTTP(t, func(*http.Request) (any, int) {
		return resp, http.StatusOK
	})
	defer restore()

	rates := FetchCoinpaprikaRates(context.Background(), []*CoinpaprikaAsset{
		{AssetID: 0, Name: "bitcoin", Symbol: "btc"},
	}, tLog)
	if _, ok := rates[0]; ok {
		t.Error("zero price should be excluded from results")
	}
}

func TestSourceKuCoin(t *testing.T) {
	restore := mockHTTP(t, func(*http.Request) (any, int) {
		return map[string]any{"data": map[string]string{"BTC": "50000.5", "ETH": "bad"}}, http.StatusOK
	})
	defer restore()

	sources := fiatSources(Config{}, tLog)
	kuCoinSrc := sources[1] // kuCoin is index 1

	rates, err := kuCoinSrc.getRates(context.Background(), []string{"btc", "eth"}, tLog)
	if err != nil {
		t.Fatalf("kuCoin getRates error: %v", err)
	}
	if rates["BTC"] != 50000.5 {
		t.Errorf("BTC = %v, want 50000.5", rates["BTC"])
	}
	// "bad" parse should be skipped (logged) not fatal.
	if _, ok := rates["ETH"]; ok {
		t.Error("unparseable ETH rate should be excluded")
	}
}

func TestSourceKuCoinHTTPError(t *testing.T) {
	restore := mockHTTP(t, func(*http.Request) (any, int) {
		return nil, http.StatusServiceUnavailable
	})
	defer restore()

	sources := fiatSources(Config{}, tLog)
	_, err := sources[1].getRates(context.Background(), []string{"btc"}, tLog)
	if err == nil {
		t.Error("expected error on HTTP failure")
	}
}

func TestSourceBinance(t *testing.T) {
	restore := mockHTTP(t, func(*http.Request) (any, int) {
		return []map[string]string{
			{"symbol": "BTCUSDT", "price": "60000.0"},
			{"symbol": "ETHUSDT", "price": "3000.0"},
		}, http.StatusOK
	})
	defer restore()

	sources := fiatSources(Config{}, tLog)
	binanceSrc := sources[2] // binance is index 2

	rates, err := binanceSrc.getRates(context.Background(), []string{"btc", "eth"}, tLog)
	if err != nil {
		t.Fatalf("binance getRates error: %v", err)
	}
	if rates["BTC"] != 60000 {
		t.Errorf("BTC = %v, want 60000", rates["BTC"])
	}
	if rates["ETH"] != 3000 {
		t.Errorf("ETH = %v, want 3000", rates["ETH"])
	}
}

func TestSourceBinanceEmptyTickers(t *testing.T) {
	// parseBinanceTickers returns "" for empty input — source should return nil, nil.
	sources := fiatSources(Config{}, tLog)
	rates, err := sources[2].getRates(context.Background(), nil, tLog)
	if err != nil || rates != nil {
		t.Errorf("empty tickers: got rates=%v err=%v, want nil, nil", rates, err)
	}
}

func TestSourceCoinpaprika(t *testing.T) {
	resp := coinpaprikaSymResp{{Symbol: "BTC"}, {Symbol: "DCR"}}
	resp[0].Quotes.USD.Price = 55000
	resp[1].Quotes.USD.Price = 20

	restore := mockHTTP(t, func(*http.Request) (any, int) {
		return resp, http.StatusOK
	})
	defer restore()

	sources := fiatSources(Config{}, tLog)
	coinpapSrc := sources[3] // coinpaprika is index 3

	rates, err := coinpapSrc.getRates(context.Background(), []string{"btc", "dcr"}, tLog)
	if err != nil {
		t.Fatalf("coinpaprika getRates error: %v", err)
	}
	if rates["BTC"] != 55000 {
		t.Errorf("BTC = %v, want 55000", rates["BTC"])
	}
	if rates["DCR"] != 20 {
		t.Errorf("DCR = %v, want 20", rates["DCR"])
	}
}

func TestSourceCryptoCompare(t *testing.T) {
	restore := mockHTTP(t, func(*http.Request) (any, int) {
		return map[string]map[string]float64{"BTC": {"USD": 48000}, "ETH": {"USD": 2500}}, http.StatusOK
	})
	defer restore()

	sources := fiatSources(Config{CryptoCompareAPIKey: "testkey"}, tLog)
	ccSrc := sources[0] // cryptoCompare is index 0

	rates, err := ccSrc.getRates(context.Background(), []string{"btc", "eth"}, tLog)
	if err != nil {
		t.Fatalf("cryptoCompare getRates error: %v", err)
	}
	if rates["BTC"] != 48000 {
		t.Errorf("BTC = %v, want 48000", rates["BTC"])
	}
	if rates["ETH"] != 2500 {
		t.Errorf("ETH = %v, want 2500", rates["ETH"])
	}
}

func TestSourceCryptoCompareNoKey(t *testing.T) {
	// Without an API key the closure should return nil, nil immediately.
	sources := fiatSources(Config{}, tLog)
	rates, err := sources[0].getRates(context.Background(), []string{"btc"}, tLog)
	if err != nil || rates != nil {
		t.Errorf("no-key cryptoCompare: got rates=%v err=%v, want nil, nil", rates, err)
	}
}

// ---- Oracle tests -------------------------------------------------------

func TestCalculateAverageRate(t *testing.T) {
	o := &Oracle{
		log:       tLog,
		rates:     map[string]*FiatRateInfo{"BTC": {}, "DCR": {}},
		listeners: make(map[string]chan<- map[string]*FiatRateInfo),
		sources: []*source{
			{rates: map[string]float64{"BTC": 50000, "DCR": 20}},
			{rates: map[string]float64{"BTC": 60000, "DCR": 40}},
		},
	}
	o.calculateAverageRate()

	if got := o.rates["BTC"].Value; got != 55000 {
		t.Errorf("BTC = %v, want 55000", got)
	}
	if got := o.rates["DCR"].Value; got != 30 {
		t.Errorf("DCR = %v, want 30", got)
	}
	if o.rates["BTC"].LastUpdate.IsZero() {
		t.Error("LastUpdate not set after calculateAverageRate")
	}
}

func TestCalculateAverageRateSkipsZero(t *testing.T) {
	o := &Oracle{
		log:       tLog,
		rates:     map[string]*FiatRateInfo{"BTC": {}},
		listeners: make(map[string]chan<- map[string]*FiatRateInfo),
		sources: []*source{
			{rates: map[string]float64{"BTC": 50000}},
			{rates: map[string]float64{"BTC": 0}},
		},
	}
	o.calculateAverageRate()
	if got := o.rates["BTC"].Value; got != 50000 {
		t.Errorf("BTC = %v, want 50000 (zero source excluded)", got)
	}
}

func TestCalculateAverageRateDisabledSource(t *testing.T) {
	o := &Oracle{
		log:       tLog,
		rates:     map[string]*FiatRateInfo{"BTC": {}},
		listeners: make(map[string]chan<- map[string]*FiatRateInfo),
		sources: []*source{
			{rates: map[string]float64{"BTC": 50000}},
			{disabled: true, rates: map[string]float64{"BTC": 99999}},
		},
	}
	o.calculateAverageRate()
	if got := o.rates["BTC"].Value; got != 50000 {
		t.Errorf("BTC = %v, want 50000 (disabled source ignored)", got)
	}
}

func TestOracleRatesExpiresStale(t *testing.T) {
	stale := &FiatRateInfo{Value: 999, LastUpdate: time.Now().Add(-2 * FiatRateDataExpiry)}
	o := &Oracle{
		log:       tLog,
		rates:     map[string]*FiatRateInfo{"BTC": stale},
		listeners: make(map[string]chan<- map[string]*FiatRateInfo),
	}
	if _, ok := o.Rates()["BTC"]; ok {
		t.Error("stale rate should not appear in Rates()")
	}
}

func TestOracleTickers(t *testing.T) {
	o := &Oracle{
		rates: map[string]*FiatRateInfo{"BTC": {}, "ETH": {}},
	}
	tickers := o.tickers()
	if len(tickers) != 2 {
		t.Errorf("tickers len = %d, want 2", len(tickers))
	}
}

func TestOracleListeners(t *testing.T) {
	o := &Oracle{
		log:       tLog,
		rates:     map[string]*FiatRateInfo{"BTC": {}},
		listeners: make(map[string]chan<- map[string]*FiatRateInfo),
		sources: []*source{
			{rates: map[string]float64{"BTC": 50000}},
		},
	}

	ch := make(chan map[string]*FiatRateInfo, 1)
	o.AddFiatRateListener("test", ch)
	if len(o.listeners) != 1 {
		t.Fatalf("expected 1 listener, got %d", len(o.listeners))
	}

	// calculateAverageRate should deliver to the listener.
	o.calculateAverageRate()
	select {
	case update := <-ch:
		if update["BTC"] == nil {
			t.Error("expected BTC in listener update")
		}
	default:
		t.Error("listener did not receive rate update")
	}

	// RemoveFiatRateListener should close the channel.
	o.RemoveFiatRateListener("test")
	if len(o.listeners) != 0 {
		t.Error("listener should be removed")
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after RemoveFiatRateListener")
		}
	default:
		t.Error("channel should be closed (readable)")
	}
}

func TestOracleListenersFullChannelDrops(t *testing.T) {
	o := &Oracle{
		log:       tLog,
		rates:     map[string]*FiatRateInfo{"BTC": {}},
		listeners: make(map[string]chan<- map[string]*FiatRateInfo),
		sources: []*source{
			{rates: map[string]float64{"BTC": 50000}},
		},
	}

	// Unbuffered channel — notifyListeners must not block.
	ch := make(chan map[string]*FiatRateInfo)
	o.AddFiatRateListener("test", ch)
	o.calculateAverageRate() // must return promptly despite full channel
}

func TestNewFiatOracleValidation(t *testing.T) {
	_, err := NewFiatOracle(Config{}, "", tLog)
	if err == nil {
		t.Error("expected error for empty ticker symbols")
	}

	_, err = NewFiatOracle(Config{}, "unknown_asset_xyz", tLog)
	if err == nil {
		t.Error("expected error for unknown asset ticker")
	}

	_, err = NewFiatOracle(Config{}, "btc", tLog)
	if err != nil {
		t.Errorf("unexpected error for valid ticker: %v", err)
	}
}
