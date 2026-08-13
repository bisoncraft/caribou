// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ratesPollInterval is how often the mock refreshes prices from the
// caribou app server.
const ratesPollInterval = 2 * time.Minute

// ratesHTTPClient tolerates the app server's self-signed certificate when
// RatesURL is https.
var ratesHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

// pollRates periodically fetches USD rates from the caribou app server's
// unauthenticated GET /api/fiatrates endpoint and updates the price table
// for enabled assets. fiatRates is keyed by BIP-44 asset ID (chain and
// token IDs alike). Assets missing from the response, and any fetch
// failure, leave the previous (or fallback) prices in place.
func (s *Server) pollRates(ctx context.Context) {
	bipToIntentsID := make(map[uint32]string, len(s.assets))
	for id, a := range s.assets {
		bipToIntentsID[a.walletBipID] = id
	}

	fetch := func() {
		rates, err := fetchFiatRates(ctx, s.cfg.RatesURL)
		if err != nil {
			s.log.Printf("fiat rate fetch from %s failed (using previous prices): %v", s.cfg.RatesURL, err)
			return
		}
		updated := 0
		s.priceMtx.Lock()
		for bipID, usd := range rates {
			id, ok := bipToIntentsID[bipID]
			if !ok || usd <= 0 {
				continue
			}
			s.prices[id] = int64(usd * 1e6)
			updated++
		}
		s.priceMtx.Unlock()
		if updated > 0 {
			s.log.Printf("updated %d prices from %s", updated, s.cfg.RatesURL)
		}
	}

	fetch()
	ticker := time.NewTicker(ratesPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetch()
		}
	}
}

func fetchFiatRates(ctx context.Context, baseURL string) (map[uint32]float64, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/api/fiatrates"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := ratesHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d from %s", resp.StatusCode, url)
	}
	var body struct {
		FiatRates map[uint32]float64 `json:"fiatRates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("error decoding /api/fiatrates response: %w", err)
	}
	return body.FiatRates, nil
}
