package intents

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/decred/base58"
)

var testLogger = util.StdOutLogger("INTENTSTEST", util.LevelError)

// signQuoteResponse signs an envelope the way the service does, with a
// test key. The canonicalization itself is validated separately against
// live production fixtures.
func signQuoteResponse(t *testing.T, priv ed25519.PrivateKey, rawRequest, rawQuote []byte, timestamp string) string {
	t.Helper()
	resp := &QuoteResponse{
		Timestamp:  timestamp,
		rawRequest: rawRequest,
		rawQuote:   rawQuote,
	}
	msg, err := signedPayload(resp)
	if err != nil {
		t.Fatalf("signedPayload: %v", err)
	}
	return "ed25519:" + base58.Encode(ed25519.Sign(priv, []byte(msg)))
}

type quoteMangler func(echo map[string]json.RawMessage)

// newQuoteServer serves /v0/quote, echoing the received request (after
// optional mangling) and signing the response with the test key.
func newQuoteServer(t *testing.T, priv ed25519.PrivateKey, mangle quoteMangler) *httptest.Server {
	t.Helper()
	const rawQuote = `{"amountIn":"10000000","amountInFormatted":"0.1","amountInUsd":"6000",` +
		`"minAmountIn":"10000000","amountOut":"83000000000","amountOutFormatted":"83",` +
		`"amountOutUsd":"5990","minAmountOut":"82000000000","refundFee":"1900",` +
		`"withdrawFee":"129432","timeEstimate":802,"deadline":"2026-07-18T12:00:00.000Z",` +
		`"timeWhenInactive":"2026-07-18T12:00:00.000Z","depositAddress":"bc1qserverdepositaddr"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/quote" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("error reading request: %v", err)
		}
		var echo map[string]json.RawMessage
		if err := json.Unmarshal(body, &echo); err != nil {
			t.Errorf("error decoding request: %v", err)
		}
		if mangle != nil {
			mangle(echo)
		}
		echoRaw, err := json.Marshal(echo)
		if err != nil {
			t.Errorf("error re-encoding echo: %v", err)
		}

		const ts = "2026-07-14T23:40:38.373Z"
		sig := signQuoteResponse(t, priv, echoRaw, []byte(rawQuote), ts)
		resp := map[string]any{
			"timestamp":     ts,
			"signature":     sig,
			"correlationId": "test",
			"quoteRequest":  json.RawMessage(echoRaw),
			"quote":         json.RawMessage(rawQuote),
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("error encoding response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testQuoteRequest() *QuoteRequest {
	return &QuoteRequest{
		SwapType:          SwapTypeExactInput,
		SlippageTolerance: 100,
		OriginAsset:       "nep141:btc.omft.near",
		DepositType:       LocationOriginChain,
		DestinationAsset:  "nep141:sol.omft.near",
		Amount:            "10000000",
		RefundTo:          "bc1qrefund",
		RefundType:        LocationOriginChain,
		Recipient:         "solrecipient",
		RecipientType:     LocationDestinationChain,
		Deadline:          "2026-07-15T12:00:00.000Z",
	}
}

func newTestClient(t *testing.T, baseURL string, pub ed25519.PublicKey, appFees []AppFee) *Client {
	t.Helper()
	c, err := NewClient(&Config{
		BaseURL:    baseURL,
		SigningKey: base58.Encode(pub),
		AppFees:    appFees,
	}, testLogger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestQuoteAppFeeInjectionAndVerification(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	var sawAppFees []AppFee
	srv := newQuoteServer(t, priv, func(echo map[string]json.RawMessage) {
		if raw, ok := echo["appFees"]; ok {
			if err := json.Unmarshal(raw, &sawAppFees); err != nil {
				t.Errorf("bad appFees: %v", err)
			}
		}
	})

	fees := []AppFee{{Recipient: "0xtreasury", Fee: 10}}
	c := newTestClient(t, srv.URL, pub, fees)

	resp, err := c.Quote(context.Background(), testQuoteRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if resp.Quote.DepositAddress != "bc1qserverdepositaddr" {
		t.Fatalf("wrong deposit address %q", resp.Quote.DepositAddress)
	}
	if len(sawAppFees) != 1 || sawAppFees[0] != fees[0] {
		t.Fatalf("appFees not injected: %+v", sawAppFees)
	}
}

func TestQuoteRejectsBadSignature(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	_, otherPriv, _ := ed25519.GenerateKey(nil)

	srv := newQuoteServer(t, otherPriv, nil) // signed with the wrong key
	c := newTestClient(t, srv.URL, pub, nil)

	if _, err := c.Quote(context.Background(), testQuoteRequest()); err == nil {
		t.Fatalf("quote with bad signature accepted")
	}
}

func TestQuoteRejectsEchoMismatch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	// The server tampers with the recipient, then signs the tampered echo:
	// the signature is valid, so only the echo cross-check catches it.
	srv := newQuoteServer(t, priv, func(echo map[string]json.RawMessage) {
		echo["recipient"] = json.RawMessage(`"attacker"`)
	})
	c := newTestClient(t, srv.URL, pub, nil)

	_, err := c.Quote(context.Background(), testQuoteRequest())
	if err == nil || !strings.Contains(err.Error(), "echoed request differs") {
		t.Fatalf("expected echo mismatch rejection, got %v", err)
	}
}

func TestStatusNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Deposit address x not found","error":"Not Found","statusCode":404}`))
	}))
	t.Cleanup(srv.Close)

	pub, _, _ := ed25519.GenerateKey(nil)
	c := newTestClient(t, srv.URL, pub, nil)

	if _, err := c.Status(context.Background(), "bc1qunknown"); !errors.Is(err, ErrSwapNotFound) {
		t.Fatalf("expected ErrSwapNotFound, got %v", err)
	}
}

func TestStatusDecodesFixture(t *testing.T) {
	var raw json.RawMessage
	loadFixture(t, "status-pending.json", &raw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)

	pub, _, _ := ed25519.GenerateKey(nil)
	c := newTestClient(t, srv.URL, pub, nil)

	status, err := c.Status(context.Background(), "bc1qwhatever")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Status != StatusPendingDeposit {
		t.Fatalf("wrong status %s", status.Status)
	}
	if status.SwapDetails.RefundFee != "1900" {
		t.Fatalf("wrong refund fee %q", status.SwapDetails.RefundFee)
	}
	if status.QuoteResponse == nil || status.QuoteResponse.Quote.DepositAddress == "" {
		t.Fatalf("embedded quote response not decoded")
	}
}

func TestSubmitDeposit(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/deposit/submit" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("bad body: %v", err)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	pub, _, _ := ed25519.GenerateKey(nil)
	c := newTestClient(t, srv.URL, pub, nil)

	if err := c.SubmitDeposit(context.Background(), "bc1qdeposit", "txhash123"); err != nil {
		t.Fatalf("SubmitDeposit: %v", err)
	}
	if got["depositAddress"] != "bc1qdeposit" || got["txHash"] != "txhash123" {
		t.Fatalf("wrong submit body: %v", got)
	}
}
