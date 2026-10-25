//go:build !harness && !rpclive

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestMultiRPCClientReconfigureNoopWhenEndpointsUnchanged(t *testing.T) {
	t.Parallel()
	endpoints := []string{"https://a.example", "https://b.example"}
	origProviders := []*provider{{host: "a.example"}, {host: "b.example"}}
	m := &multiRPCClient{
		log:       tLogger,
		endpoints: endpoints,
		providers: origProviders,
	}

	// createAndCheckProviders and connectProviders both dial out; reaching
	// them here would hang/fail in a unit test. Reconfiguring with the same
	// endpoint list must return before either is called.
	if err := m.reconfigure(context.Background(), slices.Clone(endpoints), nil, "", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(m.providers, origProviders) {
		t.Fatal("providers should not have been replaced when endpoints are unchanged")
	}
}

func TestMultiRPCClientReconfigureRetriesOnPartialConnect(t *testing.T) {
	t.Parallel()
	endpoints := []string{"https://a.example", "https://b.example"}
	// Only one of the two endpoints is actually connected, simulating a
	// wallet that started with a partial connect.
	origProviders := []*provider{{host: "a.example"}}
	m := &multiRPCClient{
		log:       tLogger,
		endpoints: endpoints,
		providers: origProviders,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A canceled context makes any dial attempt fail fast. Reconfiguring
	// with the same endpoint list must not take the no-op early return when
	// the connected provider count doesn't match the endpoint count, so this
	// should reach createAndCheckProviders and return its error rather than
	// nil.
	if err := m.reconfigure(ctx, slices.Clone(endpoints), nil, t.TempDir(), false); err == nil {
		t.Fatal("expected error from attempted reconnect on partial connect, got nil")
	}
}

func TestProviderString(t *testing.T) {
	t.Parallel()
	p := &provider{host: "infura.io"}
	if p.String() != "infura.io" {
		t.Errorf("String() = %q, want infura.io", p.String())
	}
}

func TestProviderTipCache(t *testing.T) {
	t.Parallel()
	hdr := &types.Header{Number: big.NewInt(42)}

	t.Run("fresh tip is returned", func(t *testing.T) {
		t.Parallel()
		p := &provider{host: "h"}
		p.setTip(hdr, util.Disabled)
		if got := p.cachedTip(); got != hdr {
			t.Errorf("cachedTip = %v, want the set header", got)
		}
	})

	t.Run("stale header is dropped", func(t *testing.T) {
		t.Parallel()
		p := &provider{host: "h"}
		p.tip.header = hdr
		p.tip.headerStamp = time.Now().Add(-tipCacheExpiration - time.Second)
		if got := p.cachedTip(); got != nil {
			t.Errorf("cachedTip = %v, want nil for a stale header", got)
		}
	})

	t.Run("cache is no older than the active poll interval", func(t *testing.T) {
		t.Parallel()
		// Simnet polls every second, so a 2 s old header is too old for it,
		// but not for mainnet.
		for _, tt := range []struct {
			net  util.Network
			want *types.Header
		}{
			{util.Simnet, nil},
			{util.Mainnet, hdr},
		} {
			p := &provider{host: "h", net: tt.net}
			p.tip.header = hdr
			p.tip.headerStamp = time.Now().Add(-2 * time.Second)
			if got := p.cachedTip(); got != tt.want {
				t.Errorf("%s cachedTip = %v, want %v", tt.net, got, tt.want)
			}
		}
	})

	t.Run("recently failed provider returns nil", func(t *testing.T) {
		t.Parallel()
		p := &provider{host: "h"}
		p.tip.header = hdr
		p.tip.headerStamp = time.Now()
		p.setFailed()
		if got := p.cachedTip(); got != nil {
			t.Errorf("cachedTip = %v, want nil while quarantined", got)
		}
	})
}

func TestProviderFailed(t *testing.T) {
	t.Parallel()
	t.Run("fresh provider is not failed", func(t *testing.T) {
		t.Parallel()
		if (&provider{}).failed() {
			t.Error("a fresh provider should not be failed")
		}
	})

	t.Run("recently failed", func(t *testing.T) {
		t.Parallel()
		p := &provider{}
		p.setFailed()
		if !p.failed() {
			t.Error("expected failed after setFailed")
		}
	})

	t.Run("bricked by fail count", func(t *testing.T) {
		t.Parallel()
		p := &provider{}
		p.tip.failCount = brickedFailCount + 1 // old failStamp, but too many fails
		if !p.failed() {
			t.Error("expected failed when failCount exceeds brickedFailCount")
		}
	})

	t.Run("setTip clears failure", func(t *testing.T) {
		t.Parallel()
		p := &provider{host: "h"}
		p.setFailed()
		p.setTip(&types.Header{Number: big.NewInt(1)}, util.Disabled)
		if p.failed() {
			t.Error("setTip should clear the failed state")
		}
	})
}

func TestEndpointHost(t *testing.T) {
	for _, tt := range []struct {
		endpoint, want string
		wantErr        bool
	}{
		{"/home/user/.ethereum/geth.ipc", providerIPC, false},
		{"https://ethereum-rpc.publicnode.com", "ethereum-rpc.publicnode.com", false},
		{"wss://mainnet.infura.io/ws/v3/apikey", "mainnet.infura.io", false},
		{"http://127.0.0.1:8545", "127.0.0.1:8545", false},
		// The subdomain is the API key.
		{"https://apikey.rpc.rivet.cloud", providerRivetCloud, false},
		{"ftp://example.com", "", true},
		{"://", "", true},
	} {
		host, err := endpointHost(tt.endpoint)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: error = %v, want error %t", tt.endpoint, err, tt.wantErr)
			continue
		}
		if host != tt.want {
			t.Errorf("%s: host = %q, want %q", tt.endpoint, host, tt.want)
		}
	}
}

func TestFailedProviders(t *testing.T) {
	t.Parallel()
	t.Run("reports the missing endpoints by domain", func(t *testing.T) {
		t.Parallel()
		succeeded := []*provider{{endpointAddr: "https://a.infura.io/k"}}
		tried := []string{"https://a.infura.io/k", "https://b.alchemy.com/k2"}
		got := failedProviders(succeeded, tried)
		if got != "alchemy.com" {
			t.Errorf("failedProviders = %q, want alchemy.com", got)
		}
	})

	t.Run("none failed is empty", func(t *testing.T) {
		t.Parallel()
		succeeded := []*provider{{endpointAddr: "https://a.infura.io/k"}}
		tried := []string{"https://a.infura.io/k"}
		if got := failedProviders(succeeded, tried); got != "" {
			t.Errorf("failedProviders = %q, want empty", got)
		}
	})
}

func TestErrorFilter(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom sentinel")

	t.Run("substring match", func(t *testing.T) {
		t.Parallel()
		if !errorFilter(errors.New("rpc: known transaction abc"), "known transaction") {
			t.Error("expected substring match")
		}
	})

	t.Run("errors.Is match", func(t *testing.T) {
		t.Parallel()
		wrapped := fmt.Errorf("wrapped: %w", sentinel)
		if !errorFilter(wrapped, sentinel) {
			t.Error("expected errors.Is match for wrapped sentinel")
		}
	})

	t.Run("error matcher by message", func(t *testing.T) {
		t.Parallel()
		// Distinct error instance, but its message is contained in err.
		if !errorFilter(errors.New("xxx already known yyy"), errors.New("already known")) {
			t.Error("expected match on error matcher's message")
		}
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		if errorFilter(errors.New("totally unrelated"), "nope", errors.New("also no")) {
			t.Error("did not expect a match")
		}
	})
}

func TestAcceptabilityFilters(t *testing.T) {
	t.Parallel()
	t.Run("allRPCErrorsAreFails", func(t *testing.T) {
		t.Parallel()
		discard, propagate, fail := allRPCErrorsAreFails(errors.New("x"))
		if discard || propagate || !fail {
			t.Errorf("got (%v, %v, %v), want (false, false, true)", discard, propagate, fail)
		}
	})

	t.Run("allowAlreadyKnownFilter discards known txs", func(t *testing.T) {
		t.Parallel()
		discard, propagate, fail := allowAlreadyKnownFilter(errors.New("rpc: known transaction 0xabc"))
		if !discard || propagate || fail {
			t.Errorf("got (%v, %v, %v), want (true, false, false)", discard, propagate, fail)
		}
	})

	t.Run("allowAlreadyKnownFilter keeps other errors", func(t *testing.T) {
		t.Parallel()
		discard, _, _ := allowAlreadyKnownFilter(errors.New("insufficient funds"))
		if discard {
			t.Error("unrelated error should not be discarded")
		}
	})
}

func TestRPCTransactionUnmarshalJSON(t *testing.T) {
	t.Parallel()
	// Build and sign a real tx so its JSON satisfies types.Transaction's
	// unmarshaler, then augment the object with the block fields the relay
	// returns so txExtraDetail parsing is exercised too.
	priv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	to := common.HexToAddress("0xdac17f958d2ee523a2206206994597c13d831ec7")
	signer := types.LatestSignerForChainID(big.NewInt(1))
	tx := types.MustSignNewTx(priv, signer, &types.LegacyTx{
		Nonce:    7,
		GasPrice: big.NewInt(1_000_000_000),
		Gas:      21000,
		To:       &to,
		Value:    big.NewInt(12345),
	})

	txJSON, err := tx.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(txJSON, &obj); err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(priv.PublicKey)
	obj["blockNumber"] = json.RawMessage(`"0x10"`)
	obj["blockHash"] = json.RawMessage(`"0x95118897eba948a62df72765e20186081f2554d90a146403ba93da690b322e06"`)
	obj["from"] = json.RawMessage(`"` + from.Hex() + `"`)
	augmented, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}

	var rt rpcTransaction
	if err := json.Unmarshal(augmented, &rt); err != nil {
		t.Fatal(err)
	}
	if rt.tx == nil || rt.tx.Hash() != tx.Hash() {
		t.Errorf("decoded tx hash = %v, want %s", rt.tx, tx.Hash())
	}
	if rt.BlockNumber == nil || *rt.BlockNumber != "0x10" {
		t.Errorf("blockNumber = %v, want 0x10", rt.BlockNumber)
	}
	if rt.From == nil || *rt.From != from {
		t.Errorf("from = %v, want %s", rt.From, from)
	}
}

// tRPCError mimics the *rpc.jsonError a provider returns for a reverted
// simulation: a message plus the hex-encoded revert payload as error data.
type tRPCError struct {
	msg  string
	data interface{}
}

func (e *tRPCError) Error() string          { return e.msg }
func (e *tRPCError) ErrorCode() int         { return 3 }
func (e *tRPCError) ErrorData() interface{} { return e.data }

func TestDecodeRevert(t *testing.T) {
	t.Parallel()

	// abi.encodeWithSelector(Error(string), "ERC20: transfer amount exceeds balance")
	errorString := "0x08c379a0" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000026" +
		"45524332303a207472616e7366657220616d6f756e7420657863656564732062" +
		"616c616e63650000000000000000000000000000000000000000000000000000"
	// abi.encodeWithSelector(Panic(uint256), 0x11) — arithmetic overflow.
	panic11 := "0x4e487b710000000000000000000000000000000000000000000000000000000000000011"
	// Custom error InsufficientBalance() with no decodable payload.
	custom := "0xf4d678b8"

	tests := []struct {
		name string
		err  error
		want string
	}{{
		name: "nil",
	}, {
		name: "plain error, no data",
		err:  errors.New("connection refused"),
		want: "connection refused",
	}, {
		name: "bare revert with Error(string) data",
		err:  &tRPCError{msg: "execution reverted", data: errorString},
		want: "execution reverted: ERC20: transfer amount exceeds balance",
	}, {
		name: "reason already in message is not repeated",
		err:  &tRPCError{msg: "execution reverted: ERC20: transfer amount exceeds balance", data: errorString},
		want: "execution reverted: ERC20: transfer amount exceeds balance",
	}, {
		name: "wrapped by provider aggregation",
		err:  fmt.Errorf("provider a: %w", &tRPCError{msg: "execution reverted", data: errorString}),
		want: "provider a: execution reverted: ERC20: transfer amount exceeds balance",
	}, {
		name: "aggregated with a differently-shaped provider error",
		err:  fmt.Errorf("%w: %w", &tRPCError{msg: "execution reverted", data: errorString}, errors.New("i/o timeout")),
		want: "execution reverted: i/o timeout: ERC20: transfer amount exceeds balance",
	}, {
		name: "panic code",
		err:  &tRPCError{msg: "execution reverted", data: panic11},
		want: "execution reverted: arithmetic underflow or overflow",
	}, {
		name: "custom error selector",
		err:  &tRPCError{msg: "execution reverted", data: custom},
		want: "execution reverted: custom error selector 0xf4d678b8",
	}, {
		name: "non-string data",
		err:  &tRPCError{msg: "execution reverted", data: 42},
		want: "execution reverted",
	}, {
		name: "malformed hex data",
		err:  &tRPCError{msg: "execution reverted", data: "0xzz"},
		want: "execution reverted",
	}, {
		name: "empty data",
		err:  &tRPCError{msg: "execution reverted", data: "0x"},
		want: "execution reverted",
	}}

	for _, test := range tests {
		got := decodeRevert(test.err)
		if test.err == nil {
			if got != nil {
				t.Fatalf("%s: expected nil, got %v", test.name, got)
			}
			continue
		}
		if got.Error() != test.want {
			t.Errorf("%s: got %q, want %q", test.name, got.Error(), test.want)
		}
		if !errors.Is(got, test.err) {
			t.Errorf("%s: decoded error does not wrap the original", test.name)
		}
	}
}

func TestWithAny(t *testing.T) {
	t.Parallel()
	m := &multiRPCClient{log: tLogger, providers: []*provider{{host: "a"}, {host: "b"}}}
	var called []string
	err := m.withAny(context.Background(), func(_ context.Context, p *provider) error {
		called = append(called, p.host)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(called) != 1 {
		t.Fatalf("withAny should stop after the first success, called %v", called)
	}

	// All providers failing surfaces an error.
	err = m.withAny(context.Background(), func(_ context.Context, p *provider) error {
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error when every provider fails")
	}
}

func TestWithAll(t *testing.T) {
	t.Parallel()
	t.Run("runs against every provider, not just the first success", func(t *testing.T) {
		t.Parallel()
		m := &multiRPCClient{log: tLogger, providers: []*provider{{host: "a"}, {host: "b"}}}
		var called []string
		err := m.withAll(context.Background(), func(_ context.Context, p *provider) error {
			called = append(called, p.host)
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(called) != 2 {
			t.Fatalf("withAll should call every provider, called %v", called)
		}
	})

	t.Run("succeeds if at least one provider succeeds", func(t *testing.T) {
		t.Parallel()
		m := &multiRPCClient{log: tLogger, providers: []*provider{{host: "a"}, {host: "b"}}}
		err := m.withAll(context.Background(), func(_ context.Context, p *provider) error {
			if p.host == "a" {
				return errors.New("boom")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("errors when every provider fails", func(t *testing.T) {
		t.Parallel()
		m := &multiRPCClient{log: tLogger, providers: []*provider{{host: "a"}, {host: "b"}}}
		err := m.withAll(context.Background(), func(_ context.Context, p *provider) error {
			return errors.New("boom")
		})
		if err == nil {
			t.Fatal("expected error when every provider fails")
		}
	})

	t.Run("propagate short-circuits before trying the rest", func(t *testing.T) {
		t.Parallel()
		m := &multiRPCClient{log: tLogger, providers: []*provider{{host: "a"}, {host: "b"}}}
		var called []string
		propagateErr := errors.New("fatal")
		err := m.withAll(context.Background(), func(_ context.Context, p *provider) error {
			called = append(called, p.host)
			return propagateErr
		}, func(error) (discard, propagate, fail bool) { return false, true, false })
		if !errors.Is(err, propagateErr) {
			t.Fatalf("expected the propagated error, got %v", err)
		}
		if len(called) != 1 {
			t.Fatalf("withAll should stop after a propagated error, called %v", called)
		}
	})
}

func TestWithFreshest(t *testing.T) {
	t.Parallel()
	m := &multiRPCClient{log: tLogger, providers: []*provider{{host: "a"}, {host: "b"}}}
	var called []string
	err := m.withFreshest(context.Background(), func(_ context.Context, p *provider) error {
		called = append(called, p.host)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(called) != 1 {
		t.Fatalf("withFreshest should stop after the first success, called %v", called)
	}
}

func TestWithPreferred(t *testing.T) {
	t.Parallel()
	t.Run("uses the sticky last provider first", func(t *testing.T) {
		t.Parallel()
		last := &provider{host: "sticky"}
		m := &multiRPCClient{log: tLogger, providers: []*provider{{host: "a"}, last}}
		m.lastProvider.provider = last
		m.lastProvider.stamp = time.Now()

		var called []string
		err := m.withPreferred(context.Background(), func(_ context.Context, p *provider) error {
			called = append(called, p.host)
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(called) != 1 || called[0] != "sticky" {
			t.Fatalf("expected the sticky provider to be tried first, got %v", called)
		}
	})

	t.Run("stale sticky provider is not preferred", func(t *testing.T) {
		t.Parallel()
		stale := &provider{host: "stale"}
		m := &multiRPCClient{log: tLogger, providers: []*provider{{host: "a"}, stale}}
		m.lastProvider.provider = stale
		m.lastProvider.stamp = time.Now().Add(-2 * nonceProviderStickiness)

		var called []string
		err := m.withPreferred(context.Background(), func(_ context.Context, p *provider) error {
			called = append(called, p.host)
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(called) != 1 || called[0] == "stale" {
			t.Fatalf("stale sticky provider should not be preferred, got %v", called)
		}
	})
}

func TestNonceProviderList(t *testing.T) {
	t.Parallel()
	a, b := &provider{host: "a"}, &provider{host: "b"}
	m := &multiRPCClient{providers: []*provider{a, b}}

	// With no sticky provider, the list is just the freshness-sorted list.
	list := m.nonceProviderList()
	if len(list) != 2 {
		t.Fatalf("nonceProviderList returned %d providers, want 2", len(list))
	}

	// A fresh sticky provider is moved to the front, without being duplicated.
	m.lastProvider.provider = b
	m.lastProvider.stamp = time.Now()
	list = m.nonceProviderList()
	if len(list) != 2 || list[0].host != "b" {
		t.Fatalf("nonceProviderList = %v, want sticky provider b first", list)
	}
}

func TestShuffleProviders(t *testing.T) {
	t.Parallel()
	providers := []*provider{{host: "a"}, {host: "b"}, {host: "c"}}
	before := slices.Clone(providers)
	shuffleProviders(providers)
	if len(providers) != len(before) {
		t.Fatalf("shuffleProviders changed the slice length: %d != %d", len(providers), len(before))
	}
	for _, p := range before {
		if !slices.Contains(providers, p) {
			t.Fatalf("shuffleProviders lost provider %v", p)
		}
	}
}

func TestIsNotFoundError(t *testing.T) {
	t.Parallel()
	if !isNotFoundError(errors.New("transaction not found")) {
		t.Error("expected a \"not found\" error to be detected")
	}
	if isNotFoundError(errors.New("connection refused")) {
		t.Error("did not expect an unrelated error to be detected as not-found")
	}
}

// TestWithOneErrorAggregation asserts that the error withOne returns after
// every provider fails keeps each provider's error reachable through
// errors.Is/As, not only the last one. decodeRevert and the acceptability
// filters depend on that for mixed-provider setups, where providers report
// the same failure with differently-shaped errors.
func TestWithOneErrorAggregation(t *testing.T) {
	t.Parallel()
	revert := &tRPCError{msg: "execution reverted", data: "0x"}
	timeout := errors.New("i/o timeout")
	m := &multiRPCClient{log: tLogger}
	providers := []*provider{{host: "a"}, {host: "b"}}
	err := m.withOne(context.Background(), providers, func(_ context.Context, p *provider) error {
		if p.host == "a" {
			return revert
		}
		return timeout
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, revert) {
		t.Errorf("first provider's error not in chain: %v", err)
	}
	if !errors.Is(err, timeout) {
		t.Errorf("second provider's error not in chain: %v", err)
	}
	var dataErr rpc.DataError
	if !errors.As(err, &dataErr) {
		t.Errorf("rpc.DataError not reachable through aggregated error: %v", err)
	}
	if want := "execution reverted: i/o timeout"; err.Error() != want {
		t.Errorf("aggregated error = %q, want %q", err.Error(), want)
	}
}
