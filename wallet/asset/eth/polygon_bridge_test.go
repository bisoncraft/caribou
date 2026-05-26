// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !harness && !rpclive

package eth

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/dexnet"
)

// stateSyncedEventHash is the StateSynced event topic that
// getPolygonBridgeCompletionData scans receipt logs for.
const stateSyncedEventHash = "0x103fed9db65eac19c4d870f49ab7520fe03b99f1838e5996caf47e9e43308392"

var errTestPolygon = errors.New("test error")

type polyRoundTripper func(*http.Request) (*http.Response, error)

func (f polyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubProofGen swaps dexnet.Client.Transport for the duration of the test.
// getPolygonWithdrawalCompletionData builds its request against a hardcoded
// proof-generator URL, so the transport - not the URL - is what we intercept.
// The requested URL is recorded into *gotURL when non-nil.
func stubProofGen(t *testing.T, status int, body string, gotURL *string) {
	t.Helper()
	prev := dexnet.Client.Transport
	dexnet.Client.Transport = polyRoundTripper(func(r *http.Request) (*http.Response, error) {
		if gotURL != nil {
			*gotURL = r.URL.String()
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})
	t.Cleanup(func() { dexnet.Client.Transport = prev })
}

func TestPolygonBridgeSupportedDestinations(t *testing.T) {
	// polygonBridgePolygon dispatches on the polygonID column of the map,
	// polygonBridgeEth on the ethID column.
	polB := &polygonBridgePolygon{net: util.Mainnet}
	ethB := &polygonBridgeEth{net: util.Mainnet}

	cases := []struct {
		name string
		got  []uint32
		want []uint32
	}{
		{"polygon weth->eth", polB.supportedDestinations(wethPolygonID), []uint32{ethID}},
		{"polygon pol->polEth", polB.supportedDestinations(polygonID), []uint32{polygonEthID}},
		{"polygon unknown", polB.supportedDestinations(0xDEAD), nil},
		{"eth eth->weth", ethB.supportedDestinations(ethID), []uint32{wethPolygonID}},
		{"eth matic->pol", ethB.supportedDestinations(maticEthID), []uint32{polygonID}},
		{"eth unknown", ethB.supportedDestinations(0xDEAD), nil},
	}
	for _, tc := range cases {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestNewPolygonBridgePolygon(t *testing.T) {
	// Only binds the StateReceiver contract; no network call, so a nil backend
	// is fine.
	b, err := newPolygonBridgePolygon(nil, util.Mainnet, util.Disabled)
	if err != nil {
		t.Fatal(err)
	}
	if b.stateReceiver == nil {
		t.Error("stateReceiver not bound")
	}
}

func TestPolygonBridgePolygonTrivial(t *testing.T) {
	b, err := newPolygonBridgePolygon(nil, util.Mainnet, util.Disabled)
	if err != nil {
		t.Fatal(err)
	}

	// Withdrawals from polygon POS have no bridge contract to approve.
	if _, err := b.approveBridgeContract(nil, nil, polygonID); err == nil {
		t.Error("approveBridgeContract should error")
	}
	if _, err := b.bridgeContractAllowance(context.Background(), polygonID); err == nil {
		t.Error("bridgeContractAllowance should error")
	}
	if _, err := b.bridgeContractAddr(context.Background(), polygonID); err == nil {
		t.Error("bridgeContractAddr should error")
	}
	if b.requiresBridgeContractApproval(polygonID) {
		t.Error("requiresBridgeContractApproval should be false")
	}
	if b.requiresCompletion(0) {
		t.Error("requiresCompletion should be false")
	}
	if b.requiresFollowUpCompletion(0) {
		t.Error("requiresFollowUpCompletion should be false")
	}
	if required, data, err := b.getFollowUpCompletionData(context.Background(), "x"); required || data != nil || err == nil {
		t.Errorf("getFollowUpCompletionData = (%v, %v, %v), want (false, nil, err)", required, data, err)
	}
	if _, err := b.completeFollowUpBridge(nil, nil); err == nil {
		t.Error("completeFollowUpBridge should error")
	}
	if g := b.followUpCompleteBridgeGas(); g != 0 {
		t.Errorf("followUpCompleteBridgeGas = %d, want 0", g)
	}
	if _, err := b.completeBridge(nil, 0, nil); err == nil {
		t.Error("completeBridge should error (no completion tx for eth->pol)")
	}
	if g := b.initiateBridgeGas(0); g != 60_000 {
		t.Errorf("initiateBridgeGas = %d, want 60000", g)
	}
	if g := b.completeBridgeGas(0); g != 0 {
		t.Errorf("completeBridgeGas = %d, want 0", g)
	}
	min, max, has, err := b.bridgeLimits(polygonID, ethID)
	if err != nil || has || min.Sign() != 0 || max.Sign() != 0 {
		t.Errorf("bridgeLimits = (%s, %s, %v, %v), want (0, 0, false, nil)", min, max, has, err)
	}
}

func TestPolygonBridgePolygonInitiateBridgeGuard(t *testing.T) {
	b := &polygonBridgePolygon{net: util.Mainnet}
	// pol is bridged to polEth; asking for any other destination is rejected
	// before any contract interaction.
	if _, err := b.initiateBridge(nil, polygonID, ethID, big.NewInt(1)); err == nil {
		t.Error("expected error for unsupported source/destination pair")
	}
}

func TestNewPolygonBridgeEthUnsupportedNetwork(t *testing.T) {
	// Simnet has no bridge addresses, so construction fails before any
	// contract call.
	if _, err := newPolygonBridgeEth(context.Background(), nil, util.Simnet, common.Address{}, &testNode{}, util.Disabled); err == nil {
		t.Error("expected error for unsupported network")
	}
}

func TestPolygonBridgeEthBridgeContractAddr(t *testing.T) {
	b := &polygonBridgeEth{net: util.Mainnet}
	depositManager := polygonBridgeAddrs[util.Mainnet].depositManagerAddr

	t.Run("unsupported asset", func(t *testing.T) {
		if _, err := b.bridgeContractAddr(context.Background(), 0xDEAD); err == nil {
			t.Error("expected error for unsupported asset")
		}
	})

	t.Run("eth needs no approval", func(t *testing.T) {
		if _, err := b.bridgeContractAddr(context.Background(), ethID); err == nil {
			t.Error("expected error - eth requires no bridge contract approval")
		}
	})

	t.Run("matic uses deposit manager", func(t *testing.T) {
		got, err := b.bridgeContractAddr(context.Background(), maticEthID)
		if err != nil {
			t.Fatal(err)
		}
		if got != depositManager {
			t.Errorf("addr = %s, want %s", got, depositManager)
		}
	})

	t.Run("pol uses deposit manager", func(t *testing.T) {
		got, err := b.bridgeContractAddr(context.Background(), polygonEthID)
		if err != nil {
			t.Fatal(err)
		}
		if got != depositManager {
			t.Errorf("addr = %s, want %s", got, depositManager)
		}
	})
}

func TestPolygonBridgeEthFlags(t *testing.T) {
	b := &polygonBridgeEth{net: util.Mainnet}

	if b.requiresBridgeContractApproval(ethID) {
		t.Error("eth should not require approval")
	}
	if !b.requiresBridgeContractApproval(maticEthID) {
		t.Error("non-eth assets should require approval")
	}
	if !b.requiresCompletion(0) {
		t.Error("requiresCompletion should be true for eth side")
	}
	if !b.requiresFollowUpCompletion(polygonEthID) {
		t.Error("pol withdrawals require follow-up completion")
	}
	if b.requiresFollowUpCompletion(ethID) {
		t.Error("eth should not require follow-up completion")
	}

	if g := b.initiateBridgeGas(ethID); g != 130_000 {
		t.Errorf("initiateBridgeGas(eth) = %d, want 130000", g)
	}
	if g := b.initiateBridgeGas(maticEthID); g != 600_000 {
		t.Errorf("initiateBridgeGas(matic) = %d, want 600000", g)
	}
	if g := b.initiateBridgeGas(0xDEAD); g != 160_000 {
		t.Errorf("initiateBridgeGas(other) = %d, want 160000", g)
	}
	if g := b.completeBridgeGas(0); g != 600_000 {
		t.Errorf("completeBridgeGas = %d, want 600000", g)
	}
	if g := b.followUpCompleteBridgeGas(); g != 600_000 {
		t.Errorf("followUpCompleteBridgeGas = %d, want 600000", g)
	}

	min, max, has, err := b.bridgeLimits(ethID, wethPolygonID)
	if err != nil || has || min.Sign() != 0 || max.Sign() != 0 {
		t.Errorf("bridgeLimits = (%s, %s, %v, %v), want (0, 0, false, nil)", min, max, has, err)
	}
}

func TestPolygonBridgeEthInitiateBridgeGuard(t *testing.T) {
	b := &polygonBridgeEth{net: util.Mainnet}
	// eth bridges to wethPolygon; any other destination is rejected up front.
	if _, err := b.initiateBridge(nil, ethID, polygonEthID, big.NewInt(1)); err == nil {
		t.Error("expected error for unsupported source/destination pair")
	}
}

func TestPolygonBridgeEthJSONErrors(t *testing.T) {
	b := &polygonBridgeEth{net: util.Mainnet}

	// Both follow-up methods unmarshal their data argument before touching any
	// contract, so malformed JSON is rejected offline.
	if _, err := b.verifyBridgeCompletion(context.Background(), []byte("not json")); err == nil {
		t.Error("verifyBridgeCompletion should error on malformed data")
	}
	if _, err := b.completeFollowUpBridge(nil, []byte("not json")); err == nil {
		t.Error("completeFollowUpBridge should error on malformed data")
	}
}

func TestGetPolygonBridgeCompletionData(t *testing.T) {
	txID := "0x" + hex.EncodeToString(make([]byte, 32))
	stateID := common.HexToHash("0x2a") // 42

	t.Run("extracts state id from log", func(t *testing.T) {
		n := &testNode{receipt: &types.Receipt{Logs: []*types.Log{{
			Topics: []common.Hash{common.HexToHash(stateSyncedEventHash), stateID},
		}}}}
		got, err := getPolygonBridgeCompletionData(context.Background(), n, txID)
		if err != nil {
			t.Fatal(err)
		}
		if new(big.Int).SetBytes(got).Int64() != 42 {
			t.Errorf("stateID = %x, want 42", got)
		}
	})

	t.Run("receipt error propagates", func(t *testing.T) {
		n := &testNode{receiptErr: errTestPolygon}
		if _, err := getPolygonBridgeCompletionData(context.Background(), n, txID); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("matching log without state id topic", func(t *testing.T) {
		n := &testNode{receipt: &types.Receipt{Logs: []*types.Log{{
			Topics: []common.Hash{common.HexToHash(stateSyncedEventHash)},
		}}}}
		if _, err := getPolygonBridgeCompletionData(context.Background(), n, txID); err == nil {
			t.Error("expected error for log missing the state-id topic")
		}
	})

	t.Run("no matching log", func(t *testing.T) {
		n := &testNode{receipt: &types.Receipt{Logs: []*types.Log{{
			Topics: []common.Hash{common.HexToHash("0x01")},
		}}}}
		if _, err := getPolygonBridgeCompletionData(context.Background(), n, txID); err == nil {
			t.Error("expected error when no StateSynced log present")
		}
	})
}

func TestGetPolygonWithdrawalCompletionData(t *testing.T) {
	txID := "0xabc"

	t.Run("url targets matic + transfer sig for tokens", func(t *testing.T) {
		var gotURL string
		stubProofGen(t, http.StatusOK, `{"error":false,"result":"0xaabb"}`, &gotURL)
		got, err := getPolygonWithdrawalCompletionData(context.Background(), txID, true, util.Mainnet)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(gotURL, "/matic/") {
			t.Errorf("url %q missing /matic/", gotURL)
		}
		if !strings.Contains(gotURL, transferEventSignature) {
			t.Errorf("url %q missing transfer event signature", gotURL)
		}
		if hex.EncodeToString(got) != "aabb" {
			t.Errorf("result = %x, want aabb", got)
		}
	})

	t.Run("url targets amoy + withdraw sig for native", func(t *testing.T) {
		var gotURL string
		stubProofGen(t, http.StatusOK, `{"error":false,"result":"0x01"}`, &gotURL)
		if _, err := getPolygonWithdrawalCompletionData(context.Background(), txID, false, util.Testnet); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(gotURL, "/amoy/") {
			t.Errorf("url %q missing /amoy/", gotURL)
		}
		if !strings.Contains(gotURL, withdrawEventSignature) {
			t.Errorf("url %q missing withdraw event signature", gotURL)
		}
	})

	t.Run("not checkpointed yet returns nil", func(t *testing.T) {
		stubProofGen(t, http.StatusNotFound, `{"message":"Burn transaction has not been checkpointed yet"}`, nil)
		got, err := getPolygonWithdrawalCompletionData(context.Background(), txID, true, util.Mainnet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil while uncheckpointed, got %x", got)
		}
	})

	t.Run("error flag surfaces error", func(t *testing.T) {
		stubProofGen(t, http.StatusOK, `{"error":true,"message":"boom"}`, nil)
		if _, err := getPolygonWithdrawalCompletionData(context.Background(), txID, true, util.Mainnet); err == nil {
			t.Error("expected error for error:true response")
		}
	})

	t.Run("other http error surfaces error", func(t *testing.T) {
		stubProofGen(t, http.StatusInternalServerError, `{"message":"server down"}`, nil)
		if _, err := getPolygonWithdrawalCompletionData(context.Background(), txID, true, util.Mainnet); err == nil {
			t.Error("expected error for 500 response")
		}
	})
}

func TestPolygonBridgePolygonGetCompletionData(t *testing.T) {
	// getCompletionData delegates to the withdrawal proof generator, selecting
	// the withdraw signature for native pol (isToken == false).
	var gotURL string
	stubProofGen(t, http.StatusOK, `{"error":false,"result":"0xcc"}`, &gotURL)
	b := &polygonBridgePolygon{net: util.Mainnet}
	got, err := b.getCompletionData(context.Background(), polygonID, "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotURL, withdrawEventSignature) {
		t.Errorf("native pol withdrawal should use withdraw signature, url = %q", gotURL)
	}
	if hex.EncodeToString(got) != "cc" {
		t.Errorf("result = %x, want cc", got)
	}
}

func TestPolygonBridgeEthGetFollowUpCompletionDataErrors(t *testing.T) {
	txID := "0x" + hex.EncodeToString(make([]byte, 32))
	b := &polygonBridgeEth{net: util.Mainnet}

	t.Run("receipt error propagates", func(t *testing.T) {
		b.node = &testNode{receiptErr: errTestPolygon}
		if _, _, err := b.getFollowUpCompletionData(context.Background(), txID); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("no exit log", func(t *testing.T) {
		b.node = &testNode{receipt: &types.Receipt{Logs: []*types.Log{{
			Topics: []common.Hash{common.HexToHash("0x01")},
		}}}}
		if _, _, err := b.getFollowUpCompletionData(context.Background(), txID); err == nil {
			t.Error("expected error when no exit log present")
		}
	})
}
