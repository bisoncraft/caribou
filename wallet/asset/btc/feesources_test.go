// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bisoncraft/bosswallet/util/dexnet"
	"github.com/bisoncraft/bosswallet/util/feeratefetcher"
)

// roundTripFunc lets tests intercept HTTP requests without standing up a
// listening server. The returned response Body must be closeable.
type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// stubDexnetTransport replaces the package-level dexnet.Client.Transport for
// the duration of the test and restores it via t.Cleanup. dexnet.Get hits the
// public internet by default; without this, fee-rate tests would either flake
// on outages or have to be gated behind a live build tag.
func stubDexnetTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	prev := dexnet.Client.Transport
	dexnet.Client.Transport = rt
	t.Cleanup(func() { dexnet.Client.Transport = prev })
}

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// TestFeeSources exercises the parsing and unit conversion of each fee-rate
// source constructor using a stubbed transport, so no request reaches the
// public internet.
func TestFeeSources(t *testing.T) {
	tests := []struct {
		name    string
		src     *feeratefetcher.SourceConfig
		status  int
		body    string
		want    uint64
		wantErr bool
		// wantPathContains, if set, asserts the request path.
		wantPathContains string
	}{{
		name:   "mempool.space fastestFee",
		src:    MempoolSpaceSource("mempool.space", "https://mempool.space", 1),
		status: http.StatusOK,
		body:   `{"fastestFee":9,"halfHourFee":5}`,
		want:   9,
	}, {
		name:   "blockcypher medium_fee_per_kb to per-byte",
		src:    BlockcypherSource("btc", 1),
		status: http.StatusOK,
		body:   `{"medium_fee_per_kb":5000,"high_fee_per_kb":10000}`,
		want:   5, // round(5000/1000)
	}, {
		name:             "bitcore coins/kB to atoms/byte",
		src:              BitcoreSource("LTC", 1),
		status:           http.StatusOK,
		body:             `{"feerate":0.00002}`,
		want:             2, // round(0.00002 * 1e5)
		wantPathContains: "/LTC/mainnet/",
	}, {
		name:   "blockchair suggested per-byte",
		src:    BlockchairSource("bitcoin", 3),
		status: http.StatusOK,
		body:   `{"data":{"suggested_transaction_fee_per_byte_sat":7}}`,
		want:   7,
	}, {
		name:   "bitcoiner.live 30-minute estimate",
		src:    BitcoinerLiveSource(1),
		status: http.StatusOK,
		body:   `{"estimates":{"30":{"sat_per_vbyte":3.4}}}`,
		want:   3, // round(3.4)
	}, {
		name:    "bitcore http error propagates",
		src:     BitcoreSource("LTC", 1),
		status:  http.StatusInternalServerError,
		body:    `boom`,
		wantErr: true,
	}, {
		name:    "bitcore zero rate is error",
		src:     BitcoreSource("LTC", 1),
		status:  http.StatusOK,
		body:    `{"feerate":0}`,
		wantErr: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubDexnetTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if tc.wantPathContains != "" && !strings.Contains(req.URL.Path, tc.wantPathContains) {
					t.Errorf("path %q missing %q", req.URL.Path, tc.wantPathContains)
				}
				return jsonResp(tc.status, tc.body), nil
			}))

			rate, _, err := tc.src.F(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got rate %d", rate)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rate != tc.want {
				t.Fatalf("got rate %d, want %d", rate, tc.want)
			}
		})
	}
}
