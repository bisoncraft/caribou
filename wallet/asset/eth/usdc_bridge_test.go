// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !harness && !rpclive

package eth

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset/erc20/cctp"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestGetUsdcBridgeInfo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		assetID    uint32
		net        util.Network
		wantErr    bool
		wantDomain uint32
	}{
		{"eth mainnet", usdcEthID, util.Mainnet, false, 0},
		{"eth testnet", usdcEthID, util.Testnet, false, 0},
		{"polygon mainnet", usdcPolygonID, util.Mainnet, false, 7},
		{"polygon testnet", usdcPolygonID, util.Testnet, false, 7},
		{"base mainnet", usdcBaseID, util.Mainnet, false, 6},
		{"base testnet", usdcBaseID, util.Testnet, false, 6},
		{"eth simnet has no bridge info", usdcEthID, util.Simnet, true, 0},
		{"unknown asset id", 0xDEADBEEF, util.Mainnet, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info, err := getUsdcBridgeInfo(tc.assetID, tc.net)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if info.domainID != tc.wantDomain {
				t.Fatalf("domainID = %d, want %d", info.domainID, tc.wantDomain)
			}
			if info.usdcAssetID != tc.assetID {
				t.Fatalf("usdcAssetID = %d, want %d", info.usdcAssetID, tc.assetID)
			}
			if (info.tokenMessengerAddr == common.Address{}) {
				t.Fatal("tokenMessengerAddr is the zero address")
			}
			if (info.messageTransmitterAddr == common.Address{}) {
				t.Fatal("messageTransmitterAddr is the zero address")
			}
		})
	}
}

func TestUsdcMintInfoRoundTrip(t *testing.T) {
	t.Parallel()
	orig := &usdcMintInfo{
		attestation: encode.RandomBytes(65),
		message:     encode.RandomBytes(200),
	}
	got, err := deserializeUsdcMintInfo(orig.serialize())
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if !bytes.Equal(got.attestation, orig.attestation) {
		t.Fatalf("attestation: got %x, want %x", got.attestation, orig.attestation)
	}
	if !bytes.Equal(got.message, orig.message) {
		t.Fatalf("message: got %x, want %x", got.message, orig.message)
	}
}

func TestDeserializeUsdcMintInfoErrors(t *testing.T) {
	t.Parallel()
	// Garbage bytes that don't decode as a versioned blob.
	if _, err := deserializeUsdcMintInfo([]byte{0xff}); err == nil {
		t.Fatal("expected error for malformed blob")
	}

	// Wrong version: build a v1 blob with two pushes.
	badVersion := encode.BuildyBytes{1}.
		AddData([]byte{0xaa}).
		AddData([]byte{0xbb})
	if _, err := deserializeUsdcMintInfo(badVersion); err == nil {
		t.Fatal("expected error for non-zero version")
	}

	// Wrong push count: v0 blob with a single push.
	tooFew := encode.BuildyBytes{0}.AddData([]byte{0xaa})
	if _, err := deserializeUsdcMintInfo(tooFew); err == nil {
		t.Fatal("expected error for too few pushes")
	}

	// Wrong push count: v0 blob with three pushes.
	tooMany := encode.BuildyBytes{0}.
		AddData([]byte{0xaa}).
		AddData([]byte{0xbb}).
		AddData([]byte{0xcc})
	if _, err := deserializeUsdcMintInfo(tooMany); err == nil {
		t.Fatal("expected error for too many pushes")
	}
}

func TestUsdcBridgeSupportedDestinations(t *testing.T) {
	t.Parallel()
	b := &usdcBridge{}
	cases := []struct {
		src  uint32
		want []uint32
	}{
		{usdcEthID, []uint32{usdcPolygonID, usdcBaseID}},
		{usdcPolygonID, []uint32{usdcEthID, usdcBaseID}},
		{usdcBaseID, []uint32{usdcEthID, usdcPolygonID}},
		{0xDEAD, nil},
	}
	for _, tc := range cases {
		got := b.supportedDestinations(tc.src)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("supportedDestinations(%d) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

func TestUsdcBridgeConstants(t *testing.T) {
	t.Parallel()
	b := &usdcBridge{}

	if !b.requiresBridgeContractApproval(0) {
		t.Error("requiresBridgeContractApproval should always be true for usdc")
	}
	if !b.requiresCompletion(0) {
		t.Error("requiresCompletion should always be true for usdc")
	}
	if b.requiresFollowUpCompletion(0) {
		t.Error("requiresFollowUpCompletion should always be false for usdc")
	}

	if ok, err := b.verifyBridgeCompletion(context.Background(), nil); ok || err == nil {
		t.Errorf("verifyBridgeCompletion = (%v, %v), want (false, non-nil)", ok, err)
	}

	if g := b.initiateBridgeGas(0); g == 0 {
		t.Error("initiateBridgeGas returned 0")
	}
	if g := b.completeBridgeGas(0); g == 0 {
		t.Error("completeBridgeGas returned 0")
	}
	if g := b.followUpCompleteBridgeGas(); g != 0 {
		t.Errorf("followUpCompleteBridgeGas = %d, want 0", g)
	}

	minA, maxA, has, err := b.bridgeLimits(0, 0)
	if err != nil {
		t.Fatalf("bridgeLimits error: %v", err)
	}
	if has {
		t.Error("bridgeLimits should report no limits")
	}
	if minA == nil || maxA == nil {
		t.Fatal("bridgeLimits returned nil min/max")
	}
}

func TestUsdcBridgeFollowUpUnused(t *testing.T) {
	t.Parallel()
	b := &usdcBridge{}
	if required, data, err := b.getFollowUpCompletionData(context.Background(), "deadbeef"); err == nil || required || data != nil {
		t.Fatalf("getFollowUpCompletionData = (%v, %v, %v), want (false, nil, error)", required, data, err)
	}
	if tx, err := b.completeFollowUpBridge(nil, nil); err == nil || tx != nil {
		t.Fatalf("completeFollowUpBridge = (%v, %v), want (nil, error)", tx, err)
	}
}

// TestUsdcBridgeAssetIDGuards exercises the early-exit checks in bridge
// methods that reject calls for an asset other than the one the bridge was
// constructed for. These don't require any contract backend.
func TestUsdcBridgeAssetIDGuards(t *testing.T) {
	t.Parallel()
	b := &usdcBridge{usdcAssetID: usdcEthID}

	if _, err := b.bridgeContractAddr(context.Background(), usdcPolygonID); err == nil {
		t.Error("bridgeContractAddr should error for mismatched assetID")
	}
	if _, err := b.bridgeContractAllowance(context.Background(), usdcPolygonID); err == nil {
		t.Error("bridgeContractAllowance should error for mismatched assetID")
	}
	if _, err := b.approveBridgeContract(nil, nil, usdcPolygonID); err == nil {
		t.Error("approveBridgeContract should error for mismatched assetID")
	}
	// Source mismatch.
	if _, err := b.initiateBridge(nil, usdcPolygonID, usdcBaseID, nil); err == nil {
		t.Error("initiateBridge should error for mismatched source assetID")
	}
	// Source OK, destination not in supportedDestinations.
	if _, err := b.initiateBridge(nil, usdcEthID, 0xDEAD, nil); err == nil {
		t.Error("initiateBridge should error for unsupported destination")
	}
}

// TestCompleteBridgeRejectsBadInput verifies completeBridge rejects malformed
// mintInfo before it ever reaches the message transmitter contract call.
func TestCompleteBridgeRejectsBadInput(t *testing.T) {
	t.Parallel()
	b := &usdcBridge{}

	// Garbage bytes.
	if _, err := b.completeBridge(nil, 0, []byte{0xff}); err == nil {
		t.Error("expected error for malformed mintInfo blob")
	}

	// Well-formed blob but empty attestation.
	emptyAttest := (&usdcMintInfo{attestation: nil, message: []byte("msg")}).serialize()
	if _, err := b.completeBridge(nil, 0, emptyAttest); err == nil {
		t.Error("expected error for empty attestation")
	}

	// Well-formed blob but empty message.
	emptyMsg := (&usdcMintInfo{attestation: []byte("att"), message: nil}).serialize()
	if _, err := b.completeBridge(nil, 0, emptyMsg); err == nil {
		t.Error("expected error for empty message")
	}
}

func TestNewUsdcBridgeErrors(t *testing.T) {
	t.Parallel()
	// Chain asset ID not in baseChainToUSDCAssetID.
	if _, err := newUsdcBridge(0xDEADBEEF, util.Mainnet, nil, common.Address{}, nil); err == nil {
		t.Error("expected error for unknown chain assetID")
	}
	// Known chain asset ID but network has no bridge info entry (Simnet).
	if _, err := newUsdcBridge(ethID, util.Simnet, nil, common.Address{}, nil); err == nil {
		t.Error("expected error for unsupported network")
	}
}

// TestGetAttestation drives getAttestation against a local httptest server
// to cover the Circle API contract: complete status returns the decoded
// bytes, non-complete returns nil/nil so the caller polls again, malformed
// hex surfaces an error, and an HTTP error propagates.
// Subtests share and mutate the respBody/serverErrCode/gotRequestPath
// fixture driving the httptest server, so this test is not parallelized.
func TestGetAttestation(t *testing.T) {
	var (
		respBody       string
		serverErrCode  int
		gotRequestPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestPath = r.URL.Path
		if serverErrCode != 0 {
			http.Error(w, "boom", serverErrCode)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, respBody)
	}))
	t.Cleanup(srv.Close)

	b := &usdcBridge{attestationUrl: srv.URL + "/attestations/"}

	t.Run("pending returns nil bytes", func(t *testing.T) {
		serverErrCode = 0
		respBody = `{"attestation":"0xaabb","status":"pending_confirmations"}`
		got, err := b.getAttestation(context.Background(), []byte("msg"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("expected nil for pending status, got %x", got)
		}
		// Sanity-check that the request actually hit our path so we know the
		// keccak hash construction didn't silently target a different URL.
		if gotRequestPath == "" {
			t.Fatal("server did not receive any request")
		}
	})

	t.Run("complete with 0x prefix decodes", func(t *testing.T) {
		serverErrCode = 0
		respBody = `{"attestation":"0xaabbcc","status":"complete"}`
		got, err := b.getAttestation(context.Background(), []byte("msg"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte{0xaa, 0xbb, 0xcc}) {
			t.Fatalf("got %x, want aabbcc", got)
		}
	})

	t.Run("complete without 0x prefix decodes", func(t *testing.T) {
		serverErrCode = 0
		respBody = `{"attestation":"ddeeff","status":"complete"}`
		got, err := b.getAttestation(context.Background(), []byte("msg"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte{0xdd, 0xee, 0xff}) {
			t.Fatalf("got %x, want ddeeff", got)
		}
	})

	t.Run("malformed hex surfaces error", func(t *testing.T) {
		serverErrCode = 0
		respBody = `{"attestation":"0xzz","status":"complete"}`
		if _, err := b.getAttestation(context.Background(), []byte("msg")); err == nil {
			t.Fatal("expected error for malformed hex attestation")
		}
	})

	t.Run("HTTP 500 surfaces error", func(t *testing.T) {
		serverErrCode = http.StatusInternalServerError
		if _, err := b.getAttestation(context.Background(), []byte("msg")); err == nil {
			t.Fatal("expected error for 500 response")
		}
	})
}

// TestNewUsdcBridgeSuccess covers the construction success path - binding the
// CCTP contracts and resolving token info - for every supported chain and
// network. The error paths live in TestNewUsdcBridgeErrors.
func TestNewUsdcBridgeSuccess(t *testing.T) {
	t.Parallel()
	addr := common.HexToAddress("0x2b84C791b79Ee37De042AD2ffF1A253c3ce9bc27")
	for _, chainAssetID := range []uint32{ethID, polygonID, baseID} {
		for _, net := range []util.Network{util.Mainnet, util.Testnet} {
			b, err := newUsdcBridge(chainAssetID, net, nil, addr, &testNode{})
			if err != nil {
				t.Fatalf("chain %d net %s: %v", chainAssetID, net, err)
			}
			if b.usdcAssetID != baseChainToUSDCAssetID[chainAssetID] {
				t.Errorf("chain %d: usdcAssetID = %d, want %d", chainAssetID, b.usdcAssetID, baseChainToUSDCAssetID[chainAssetID])
			}
			if b.attestationUrl != usdcBridgeAttestationUrl[net] {
				t.Errorf("net %s: attestationUrl = %q", net, b.attestationUrl)
			}
			if (b.tokenMessengerAddr == common.Address{}) {
				t.Errorf("chain %d net %s: zero tokenMessengerAddr", chainAssetID, net)
			}
		}
	}
}

// messageSentLog builds a synthetic MessageSent event log carrying msg, so the
// receipt-parsing path can be exercised without a live node. MessageSent has a
// single non-indexed bytes field, so the message lands entirely in the log
// data and the topic is just the event signature.
func messageSentLog(t *testing.T, msg []byte) *types.Log {
	t.Helper()
	parsed, err := cctp.MessageTransmitterMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	ev := parsed.Events["MessageSent"]
	data, err := ev.Inputs.Pack(msg)
	if err != nil {
		t.Fatal(err)
	}
	return &types.Log{
		Topics: []common.Hash{ev.ID},
		Data:   data,
	}
}

func newTestUsdcBridge(t *testing.T, n *testNode) *usdcBridge {
	t.Helper()
	addr := common.HexToAddress("0x2b84C791b79Ee37De042AD2ffF1A253c3ce9bc27")
	b, err := newUsdcBridge(ethID, util.Testnet, nil, addr, n)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUsdcGetMessageSentEventLog(t *testing.T) {
	t.Parallel()
	wantMsg := []byte{0xde, 0xad, 0xbe, 0xef}
	txID := "0x" + hex.EncodeToString(make([]byte, 32))

	t.Run("extracts message from receipt", func(t *testing.T) {
		t.Parallel()
		n := &testNode{receipt: &types.Receipt{Logs: []*types.Log{messageSentLog(t, wantMsg)}}}
		got, err := newTestUsdcBridge(t, n).getMessageSentEventLog(context.Background(), txID)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, wantMsg) {
			t.Fatalf("msg = %x, want %x", got, wantMsg)
		}
	})

	t.Run("receipt error propagates", func(t *testing.T) {
		t.Parallel()
		n := &testNode{receiptErr: fmt.Errorf("receipt unavailable")}
		if _, err := newTestUsdcBridge(t, n).getMessageSentEventLog(context.Background(), txID); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("no MessageSent log in receipt", func(t *testing.T) {
		t.Parallel()
		// A receipt whose only log isn't a MessageSent event.
		n := &testNode{receipt: &types.Receipt{Logs: []*types.Log{{
			Topics: []common.Hash{common.HexToHash("0x01")},
		}}}}
		if _, err := newTestUsdcBridge(t, n).getMessageSentEventLog(context.Background(), txID); err == nil {
			t.Fatal("expected error for receipt with no MessageSent event")
		}
	})
}

// TestUsdcGetCompletionData covers the full completion flow: pull the
// MessageSent message out of the burn receipt, fetch the attestation from
// Circle, and serialize the two into mint info the destination chain can use.
func TestUsdcGetCompletionData(t *testing.T) {
	t.Parallel()
	wantMsg := []byte{0x11, 0x22, 0x33}
	txID := "0x" + hex.EncodeToString(make([]byte, 32))

	receiptNode := func() *testNode {
		return &testNode{receipt: &types.Receipt{Logs: []*types.Log{messageSentLog(t, wantMsg)}}}
	}

	t.Run("complete attestation yields mint info", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, `{"attestation":"0xaabb","status":"complete"}`)
		}))
		t.Cleanup(srv.Close)

		b := newTestUsdcBridge(t, receiptNode())
		b.attestationUrl = srv.URL + "/attestations/"

		data, err := b.getCompletionData(context.Background(), usdcEthID, txID)
		if err != nil {
			t.Fatal(err)
		}
		mintInfo, err := deserializeUsdcMintInfo(data)
		if err != nil {
			t.Fatalf("deserialize completion data: %v", err)
		}
		if !bytes.Equal(mintInfo.message, wantMsg) {
			t.Errorf("message = %x, want %x", mintInfo.message, wantMsg)
		}
		if !bytes.Equal(mintInfo.attestation, []byte{0xaa, 0xbb}) {
			t.Errorf("attestation = %x, want aabb", mintInfo.attestation)
		}
	})

	t.Run("pending attestation yields nil", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, `{"status":"pending_confirmations"}`)
		}))
		t.Cleanup(srv.Close)

		b := newTestUsdcBridge(t, receiptNode())
		b.attestationUrl = srv.URL + "/attestations/"

		data, err := b.getCompletionData(context.Background(), usdcEthID, txID)
		if err != nil {
			t.Fatal(err)
		}
		if data != nil {
			t.Errorf("expected nil completion data while attestation pending, got %x", data)
		}
	})

	t.Run("receipt error propagates", func(t *testing.T) {
		t.Parallel()
		n := &testNode{receiptErr: fmt.Errorf("receipt unavailable")}
		if _, err := newTestUsdcBridge(t, n).getCompletionData(context.Background(), usdcEthID, txID); err == nil {
			t.Fatal("expected error")
		}
	})
}
