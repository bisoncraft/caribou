// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/dexnet"
)

// Across mainnet ETH chain ID; mirrors the value in across_protocol.go so the
// tests fail loudly if either side drifts.
const testAcrossEthChainID uint64 = 1

// resetAcrossRoutesCache clears the package-level cache populated by
// getAcrossAvailableRoutes. Tests share process state and any prior call would
// otherwise short-circuit the stubbed transport.
func resetAcrossRoutesCache(t *testing.T) {
	t.Helper()
	acrossAvailableRoutesMutex.Lock()
	defer acrossAvailableRoutesMutex.Unlock()
	prev := acrossAvailableRoutesCache
	acrossAvailableRoutesCache = make(map[util.Network][]acrossAvailableRoute)
	t.Cleanup(func() {
		acrossAvailableRoutesMutex.Lock()
		defer acrossAvailableRoutesMutex.Unlock()
		acrossAvailableRoutesCache = prev
	})
}

// roundTripFn is an http.RoundTripper backed by a closure.
type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubDexnetClient swaps dexnet.Client.Transport for the duration of the test
// so calls through dexnet.Get / dexnet.Do never hit the public internet.
func stubDexnetClient(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	prev := dexnet.Client.Transport
	dexnet.Client.Transport = rt
	t.Cleanup(func() { dexnet.Client.Transport = prev })
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// acrossHandler maps URL paths to canned response bodies. Any unmatched path
// surfaces a test error so accidental drift in the URLs gets caught.
type acrossHandler struct {
	t         *testing.T
	responses map[string]func(*http.Request) (int, string)
}

func newAcrossHandler(t *testing.T) *acrossHandler {
	return &acrossHandler{
		t:         t,
		responses: make(map[string]func(*http.Request) (int, string)),
	}
}

func (h *acrossHandler) on(pathPrefix string, fn func(*http.Request) (int, string)) {
	h.responses[pathPrefix] = fn
}

func (h *acrossHandler) transport() http.RoundTripper {
	return roundTripFn(func(r *http.Request) (*http.Response, error) {
		for prefix, fn := range h.responses {
			if strings.HasPrefix(r.URL.Path, prefix) {
				status, body := fn(r)
				return jsonResponse(status, body), nil
			}
		}
		h.t.Errorf("unexpected across request: %s %s", r.Method, r.URL)
		return jsonResponse(http.StatusNotFound, `{"message":"unstubbed"}`), nil
	})
}

func TestAcrossBaseURL(t *testing.T) {
	if got := acrossBaseURL(util.Mainnet); got != "https://app.across.to" {
		t.Errorf("mainnet base = %q", got)
	}
	if got := acrossBaseURL(util.Testnet); got != "https://testnet.across.to" {
		t.Errorf("testnet base = %q", got)
	}
	// Anything that isn't mainnet falls through to the testnet URL.
	if got := acrossBaseURL(util.Simnet); got != "https://testnet.across.to" {
		t.Errorf("simnet base = %q", got)
	}
}

func TestGetSpokePoolAddr(t *testing.T) {
	if _, err := getSpokePoolAddr(util.Mainnet, testAcrossEthChainID); err != nil {
		t.Fatalf("mainnet eth: %v", err)
	}
	if _, err := getSpokePoolAddr(util.Testnet, 11155111); err != nil {
		t.Fatalf("testnet eth: %v", err)
	}
	if _, err := getSpokePoolAddr(util.Mainnet, 0xDEADBEEF); err == nil {
		t.Error("expected error for unknown chain ID")
	}
	if _, err := getSpokePoolAddr(util.Simnet, testAcrossEthChainID); err == nil {
		t.Error("expected error for unknown network (simnet has no spoke pools)")
	}
}

func TestGetAcrossAvailableRoutes(t *testing.T) {
	t.Run("ok and cached", func(t *testing.T) {
		resetAcrossRoutesCache(t)

		calls := 0
		h := newAcrossHandler(t)
		h.on("/api/available-routes", func(r *http.Request) (int, string) {
			calls++
			return http.StatusOK, `[{"originChainId":1,"destinationChainId":8453,"originToken":"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2","destinationToken":"0x4200000000000000000000000000000000000006","originTokenSymbol":"WETH","destinationTokenSymbol":"WETH","isNative":true}]`
		})
		stubDexnetClient(t, h.transport())

		routes, err := getAcrossAvailableRoutes(context.Background(), util.Mainnet)
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) != 1 || routes[0].OriginChainID != 1 {
			t.Fatalf("unexpected routes: %+v", routes)
		}

		// Second call must hit the cache, not the stub.
		if _, err := getAcrossAvailableRoutes(context.Background(), util.Mainnet); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Errorf("expected 1 HTTP call, got %d", calls)
		}
	})

	t.Run("http error surfaces", func(t *testing.T) {
		resetAcrossRoutesCache(t)
		h := newAcrossHandler(t)
		h.on("/api/available-routes", func(r *http.Request) (int, string) {
			return http.StatusBadGateway, `bad gateway`
		})
		stubDexnetClient(t, h.transport())

		if _, err := getAcrossAvailableRoutes(context.Background(), util.Mainnet); err == nil {
			t.Fatal("expected error on 502")
		}
	})
}

// fakeLogger is a minimal util.Logger that records error/warn calls so tests
// can assert on logged token-mismatch warnings without pulling in a full
// logger.
type fakeLogger struct {
	util.Logger
	errs int
}

func (l *fakeLogger) Errorf(string, ...any) { l.errs++ }

func TestAcrossSupportedDestinations(t *testing.T) {
	// Setup: a route from ETH mainnet WETH to BASE mainnet WETH. Both are in
	// chainIDToNativeTokenAddr so assetIDToAcrossAsset returns the right
	// native address and acrossSupportedDestinations accepts the route.
	resetAcrossRoutesCache(t)
	h := newAcrossHandler(t)
	h.on("/api/available-routes", func(r *http.Request) (int, string) {
		return http.StatusOK, `[
            {"originChainId":1,"destinationChainId":8453,"originToken":"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2","destinationToken":"0x4200000000000000000000000000000000000006","originTokenSymbol":"WETH","destinationTokenSymbol":"WETH","isNative":true}
        ]`
	})
	stubDexnetClient(t, h.transport())

	log := &fakeLogger{}
	dests, err := acrossSupportedDestinations(context.Background(), ethID, util.Mainnet, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(dests) != 1 || dests[0] != baseID {
		t.Fatalf("dests = %v, want [%d]", dests, baseID)
	}
	if log.errs != 0 {
		t.Errorf("unexpected error logs: %d", log.errs)
	}

	// Unknown source asset → error before touching routes.
	if _, err := acrossSupportedDestinations(context.Background(), 0xDEADBEEF, util.Mainnet, log); err == nil {
		t.Error("expected error for unknown source asset")
	}
}

func TestNewAcrossBridge(t *testing.T) {
	// Unknown chain asset ID rejected before any backend work.
	if _, err := newAcrossBridge(context.Background(), nil, nil, 0xDEADBEEF, util.Mainnet, common.Address{}, nil); err == nil {
		t.Error("expected error for unknown chain asset ID")
	}
	// Known chain asset ID but unknown network (Simnet has no spoke pool).
	if _, err := newAcrossBridge(context.Background(), nil, nil, ethID, util.Simnet, common.Address{}, nil); err == nil {
		t.Error("expected error for unsupported network")
	}
}

func TestAcrossBridgeContractAddr(t *testing.T) {
	b := &acrossBridge{chainAssetID: ethID, spokePoolAddr: common.HexToAddress("0xabc")}

	// Native asset has no contract approval requirement.
	if _, err := b.bridgeContractAddr(context.Background(), ethID); err == nil {
		t.Error("expected error for native source asset")
	}
	// Token asset returns the spoke pool address.
	addr, err := b.bridgeContractAddr(context.Background(), usdcEthID)
	if err != nil {
		t.Fatal(err)
	}
	if addr != b.spokePoolAddr {
		t.Errorf("addr = %s, want %s", addr.Hex(), b.spokePoolAddr.Hex())
	}
}

func TestAcrossBridgeFlags(t *testing.T) {
	b := &acrossBridge{chainAssetID: ethID}

	if b.requiresBridgeContractApproval(ethID) {
		t.Error("native asset should not require approval")
	}
	if !b.requiresBridgeContractApproval(usdcEthID) {
		t.Error("token asset should require approval")
	}
	if b.requiresCompletion(0) {
		t.Error("requiresCompletion should be false for across")
	}
	if b.requiresFollowUpCompletion(0) {
		t.Error("requiresFollowUpCompletion should be false for across")
	}

	if g := b.initiateBridgeGas(0); g == 0 {
		t.Error("initiateBridgeGas returned 0")
	}
	if g := b.completeBridgeGas(0); g != 0 {
		t.Errorf("completeBridgeGas = %d, want 0", g)
	}
	if g := b.followUpCompleteBridgeGas(); g != 0 {
		t.Errorf("followUpCompleteBridgeGas = %d, want 0", g)
	}
}

func TestAcrossBridgeTrivialErrors(t *testing.T) {
	b := &acrossBridge{}
	if _, err := b.completeBridge(nil, 0, nil); err == nil {
		t.Error("completeBridge should always error for across")
	}
	if _, err := b.completeFollowUpBridge(nil, nil); err == nil {
		t.Error("completeFollowUpBridge should always error for across")
	}
	required, data, err := b.getFollowUpCompletionData(context.Background(), "deadbeef")
	if err != nil || required || data != nil {
		t.Fatalf("getFollowUpCompletionData = (%v, %v, %v), want (false, nil, nil)", required, data, err)
	}
}

func TestAcrossBridgeVerifyCompletion(t *testing.T) {
	makeCompletionData := func(chainID, depositID uint64) []byte {
		out := make([]byte, 64)
		new(big.Int).SetUint64(chainID).FillBytes(out[:32])
		new(big.Int).SetUint64(depositID).FillBytes(out[32:])
		return out
	}

	t.Run("rejects wrong length", func(t *testing.T) {
		b := &acrossBridge{net: util.Mainnet}
		if _, err := b.verifyBridgeCompletion(context.Background(), []byte{0xaa}); err == nil {
			t.Error("expected error for short data")
		}
	})

	t.Run("testnet shortcircuits to true", func(t *testing.T) {
		// Per the Across docs comment in the source: testnet event indexing is
		// incomplete, so we report completion as true without hitting the API.
		b := &acrossBridge{net: util.Testnet}
		ok, err := b.verifyBridgeCompletion(context.Background(), makeCompletionData(1, 42))
		if err != nil || !ok {
			t.Fatalf("verifyBridgeCompletion = (%v, %v), want (true, nil)", ok, err)
		}
	})

	t.Run("mainnet filled is true", func(t *testing.T) {
		h := newAcrossHandler(t)
		h.on("/api/deposit/status", func(r *http.Request) (int, string) {
			if got := r.URL.Query().Get("depositId"); got != "42" {
				t.Errorf("depositId = %q, want 42", got)
			}
			return http.StatusOK, `{"status":"filled"}`
		})
		stubDexnetClient(t, h.transport())

		b := &acrossBridge{net: util.Mainnet, apiBaseURL: "https://app.across.to"}
		ok, err := b.verifyBridgeCompletion(context.Background(), makeCompletionData(1, 42))
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("expected ok=true for filled status")
		}
	})

	t.Run("mainnet pending is false", func(t *testing.T) {
		h := newAcrossHandler(t)
		h.on("/api/deposit/status", func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"pending"}`
		})
		stubDexnetClient(t, h.transport())

		b := &acrossBridge{net: util.Mainnet, apiBaseURL: "https://app.across.to"}
		ok, err := b.verifyBridgeCompletion(context.Background(), makeCompletionData(1, 1))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("expected ok=false for pending status")
		}
	})

	t.Run("mainnet http error", func(t *testing.T) {
		h := newAcrossHandler(t)
		h.on("/api/deposit/status", func(r *http.Request) (int, string) {
			return http.StatusInternalServerError, `boom`
		})
		stubDexnetClient(t, h.transport())

		b := &acrossBridge{net: util.Mainnet, apiBaseURL: "https://app.across.to"}
		if _, err := b.verifyBridgeCompletion(context.Background(), makeCompletionData(1, 1)); err == nil {
			t.Error("expected error on 500")
		}
	})
}

func TestAcrossBridgeLimits(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		h := newAcrossHandler(t)
		h.on("/api/limits", func(r *http.Request) (int, string) {
			q := r.URL.Query()
			if got := q.Get("originChainId"); got != "1" {
				t.Errorf("originChainId = %q, want 1", got)
			}
			if got := q.Get("destinationChainId"); got != "8453" {
				t.Errorf("destinationChainId = %q, want 8453", got)
			}
			return http.StatusOK, `{"minDeposit":"1000","maxDeposit":"5000000"}`
		})
		stubDexnetClient(t, h.transport())

		b := &acrossBridge{
			net:        util.Mainnet,
			chainID:    testAcrossEthChainID,
			apiBaseURL: "https://app.across.to",
		}
		minD, maxD, has, err := b.bridgeLimits(ethID, baseID)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Error("expected has=true")
		}
		if minD.Int64() != 1000 || maxD.Int64() != 5000000 {
			t.Errorf("min/max = %s/%s, want 1000/5000000", minD, maxD)
		}
	})

	t.Run("unknown source asset", func(t *testing.T) {
		b := &acrossBridge{net: util.Mainnet}
		if _, _, _, err := b.bridgeLimits(0xDEADBEEF, baseID); err == nil {
			t.Error("expected error for unknown source asset")
		}
	})

	t.Run("unknown destination asset", func(t *testing.T) {
		b := &acrossBridge{net: util.Mainnet}
		if _, _, _, err := b.bridgeLimits(ethID, 0xDEADBEEF); err == nil {
			t.Error("expected error for unknown destination asset")
		}
	})

	t.Run("bad min", func(t *testing.T) {
		h := newAcrossHandler(t)
		h.on("/api/limits", func(r *http.Request) (int, string) {
			return http.StatusOK, `{"minDeposit":"notanumber","maxDeposit":"5000000"}`
		})
		stubDexnetClient(t, h.transport())
		b := &acrossBridge{net: util.Mainnet, chainID: testAcrossEthChainID, apiBaseURL: "https://app.across.to"}
		if _, _, _, err := b.bridgeLimits(ethID, baseID); err == nil {
			t.Error("expected error for malformed minDeposit")
		}
	})

	t.Run("http error", func(t *testing.T) {
		h := newAcrossHandler(t)
		h.on("/api/limits", func(r *http.Request) (int, string) {
			return http.StatusServiceUnavailable, `down`
		})
		stubDexnetClient(t, h.transport())
		b := &acrossBridge{net: util.Mainnet, chainID: testAcrossEthChainID, apiBaseURL: "https://app.across.to"}
		if _, _, _, err := b.bridgeLimits(ethID, baseID); err == nil {
			t.Error("expected error on 503")
		}
	})
}

// TestAcrossInitiateBridgeErrors exercises every error path in initiateBridge
// that fires before the SpokePool.DepositV3 contract call, so we never need
// to spin up a contract backend.
func TestAcrossInitiateBridgeErrors(t *testing.T) {
	type stubs struct {
		routesStatus int
		routesBody   string
		feesStatus   int
		feesBody     string
	}

	setup := func(t *testing.T, s stubs) *acrossBridge {
		resetAcrossRoutesCache(t)
		h := newAcrossHandler(t)
		h.on("/api/available-routes", func(r *http.Request) (int, string) {
			return s.routesStatus, s.routesBody
		})
		h.on("/api/suggested-fees", func(r *http.Request) (int, string) {
			return s.feesStatus, s.feesBody
		})
		stubDexnetClient(t, h.transport())
		return &acrossBridge{
			net:          util.Mainnet,
			chainAssetID: ethID,
			chainID:      testAcrossEthChainID,
			apiBaseURL:   "https://app.across.to",
			log:          &fakeLogger{},
		}
	}

	// Reusable ETH→BASE route response that lets initiateBridge get past the
	// supported-destinations check.
	const routeETHtoBase = `[
        {"originChainId":1,"destinationChainId":8453,"originToken":"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2","destinationToken":"0x4200000000000000000000000000000000000006","originTokenSymbol":"WETH","destinationTokenSymbol":"WETH","isNative":true}
    ]`
	amount := big.NewInt(1_000_000)

	t.Run("unsupported destination", func(t *testing.T) {
		// Empty routes → no destination is supported.
		b := setup(t, stubs{
			routesStatus: http.StatusOK,
			routesBody:   `[]`,
		})
		txOpts := &bind.TransactOpts{Context: context.Background()}
		if _, err := b.initiateBridge(txOpts, ethID, baseID, amount); err == nil {
			t.Error("expected error for unsupported destination")
		}
	})

	t.Run("amount too low", func(t *testing.T) {
		b := setup(t, stubs{
			routesStatus: http.StatusOK,
			routesBody:   routeETHtoBase,
			feesStatus:   http.StatusOK,
			feesBody:     `{"isAmountTooLow":true}`,
		})
		txOpts := &bind.TransactOpts{Context: context.Background()}
		if _, err := b.initiateBridge(txOpts, ethID, baseID, amount); err == nil {
			t.Error("expected error for amount too low")
		}
	})

	t.Run("invalid relay fee total", func(t *testing.T) {
		b := setup(t, stubs{
			routesStatus: http.StatusOK,
			routesBody:   routeETHtoBase,
			feesStatus:   http.StatusOK,
			feesBody:     `{"totalRelayFee":{"total":"notanumber"},"timestamp":"1","fillDeadline":"2"}`,
		})
		txOpts := &bind.TransactOpts{Context: context.Background()}
		if _, err := b.initiateBridge(txOpts, ethID, baseID, amount); err == nil {
			t.Error("expected error for invalid relay fee total")
		}
	})

	t.Run("invalid quote timestamp", func(t *testing.T) {
		b := setup(t, stubs{
			routesStatus: http.StatusOK,
			routesBody:   routeETHtoBase,
			feesStatus:   http.StatusOK,
			feesBody:     `{"totalRelayFee":{"total":"100"},"timestamp":"notanumber","fillDeadline":"2"}`,
		})
		txOpts := &bind.TransactOpts{Context: context.Background()}
		if _, err := b.initiateBridge(txOpts, ethID, baseID, amount); err == nil {
			t.Error("expected error for invalid timestamp")
		}
	})

	t.Run("invalid fill deadline", func(t *testing.T) {
		b := setup(t, stubs{
			routesStatus: http.StatusOK,
			routesBody:   routeETHtoBase,
			feesStatus:   http.StatusOK,
			feesBody:     `{"totalRelayFee":{"total":"100"},"timestamp":"1","fillDeadline":"notanumber"}`,
		})
		txOpts := &bind.TransactOpts{Context: context.Background()}
		if _, err := b.initiateBridge(txOpts, ethID, baseID, amount); err == nil {
			t.Error("expected error for invalid fill deadline")
		}
	})

	t.Run("fees http error propagates", func(t *testing.T) {
		b := setup(t, stubs{
			routesStatus: http.StatusOK,
			routesBody:   routeETHtoBase,
			feesStatus:   http.StatusServiceUnavailable,
			feesBody:     `{"message":"upstream down"}`,
		})
		txOpts := &bind.TransactOpts{Context: context.Background()}
		if _, err := b.initiateBridge(txOpts, ethID, baseID, amount); err == nil {
			t.Error("expected error on fees 503")
		}
	})
}
