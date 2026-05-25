// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package ltc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/dexnet"
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

// TestExternalFeeRate exercises externalFeeRate's network dispatch and the
// parsing logic in fetchBlockCypherFees and bitcoreFeeRate using a stubbed
// transport. Replaces a previous version that hit api.blockcypher.com and
// api.bitcore.io over the public internet on every harness build.
func TestExternalFeeRate(t *testing.T) {
	const (
		blockCypherHost = "api.blockcypher.com"
		bitcoreHost     = "api.bitcore.io"
	)

	tests := []struct {
		name    string
		net     util.Network
		rt      roundTripFunc
		want    uint64
		wantErr bool
	}{{
		name: "mainnet blockcypher ok",
		net:  util.Mainnet,
		rt: func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != blockCypherHost {
				t.Errorf("mainnet hit unexpected host %q", req.URL.Host)
			}
			// low_fee_per_kb is in sat/kB; externalFeeRate returns sat/B
			// after dividing by 1000 and rounding.
			return jsonResp(http.StatusOK,
				`{"low_fee_per_kb":2500,"medium_fee_per_kb":5000,"high_fee_per_kb":10000}`), nil
		},
		want: 3, // round(2500/1000) = 3 (banker's rounding to even via math.Round)
	}, {
		name: "simnet routes to blockcypher",
		net:  util.Simnet,
		rt: func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != blockCypherHost {
				t.Errorf("simnet hit unexpected host %q", req.URL.Host)
			}
			return jsonResp(http.StatusOK, `{"low_fee_per_kb":1000}`), nil
		},
		want: 1,
	}, {
		name: "testnet bitcore ok",
		net:  util.Testnet,
		rt: func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != bitcoreHost {
				t.Errorf("testnet hit unexpected host %q", req.URL.Host)
			}
			if !strings.Contains(req.URL.Path, "/LTC/testnet/") {
				t.Errorf("testnet path missing LTC/testnet: %q", req.URL.Path)
			}
			// bitcore reports BTC/kB; bitcoreFeeRate multiplies by 1e5 to
			// get sat/B.
			return jsonResp(http.StatusOK, `{"feerate":0.00002}`), nil
		},
		want: 2,
	}, {
		name: "blockcypher zero rate is error",
		net:  util.Mainnet,
		rt: func(req *http.Request) (*http.Response, error) {
			return jsonResp(http.StatusOK, `{"low_fee_per_kb":0}`), nil
		},
		wantErr: true,
	}, {
		name: "bitcore http error propagates",
		net:  util.Testnet,
		rt: func(req *http.Request) (*http.Response, error) {
			return jsonResp(http.StatusInternalServerError, `boom`), nil
		},
		wantErr: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubDexnetTransport(t, tc.rt)
			got, err := externalFeeRate(context.Background(), tc.net)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got rate %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}
