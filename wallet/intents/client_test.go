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
	sig, err := SignQuote(priv, rawRequest, rawQuote, timestamp)
	if err != nil {
		t.Fatalf("SignQuote: %v", err)
	}
	return sig
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

	fees := []AppFee{{Recipient: "0xfeeaccount", Fee: 10}}
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

	// The server tampers with one signed request field at a time, then
	// signs the tampered echo: the signature is valid over what the
	// service received, so only the echo cross-check catches the change.
	// Every compared field gets a case; the location-type flips
	// (recipientType/refundType to INTENTS) are the redirection attack the
	// check exists to stop. The optional signed fields (not modeled on
	// QuoteRequest) cover injection: the raw comparison must see fields
	// encoding/json would silently drop.
	tests := []struct {
		field   string
		mangled json.RawMessage
	}{
		{"recipient", json.RawMessage(`"attacker"`)},
		{"refundTo", json.RawMessage(`"attacker"`)},
		{"amount", json.RawMessage(`"20000000"`)},
		{"swapType", json.RawMessage(`"EXACT_OUTPUT"`)},
		{"dry", json.RawMessage(`true`)},
		{"recipientType", json.RawMessage(`"INTENTS"`)},
		{"refundType", json.RawMessage(`"INTENTS"`)},
		{"depositType", json.RawMessage(`"INTENTS"`)},
		{"slippageTolerance", json.RawMessage(`10000`)},
		{"deadline", json.RawMessage(`"2026-07-15T11:00:00.000Z"`)},
		{"virtualChainRecipient", json.RawMessage(`"0xattacker"`)},
		{"virtualChainRefundRecipient", json.RawMessage(`"0xattacker"`)},
		{"customRecipientMsg", json.RawMessage(`"attacker-memo"`)},
		{"referral", json.RawMessage(`"attacker"`)},
		{"quoteWaitingTimeMs", json.RawMessage(`3000`)},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			srv := newQuoteServer(t, priv, func(echo map[string]json.RawMessage) {
				echo[tt.field] = tt.mangled
			})
			c := newTestClient(t, srv.URL, pub, nil)

			_, err := c.Quote(context.Background(), testQuoteRequest())
			if err == nil || !strings.Contains(err.Error(), "differs from submitted request") {
				t.Fatalf("expected echo mismatch rejection for %s, got %v", tt.field, err)
			}
		})
	}
}

// TestQuoteAcceptsBenignEchoDrift covers the two ways a legitimate echo
// differs from the submitted body: explicit nulls (or other falsy values)
// for optional fields the client omitted — the production service echoes
// "virtualChainRecipient":null — and asset IDs normalized between the
// service's ID schemes, which callers verify against the catalog instead.
func TestQuoteAcceptsBenignEchoDrift(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	srv := newQuoteServer(t, priv, func(echo map[string]json.RawMessage) {
		echo["virtualChainRecipient"] = json.RawMessage(`null`)
		echo["virtualChainRefundRecipient"] = json.RawMessage(`null`)
		echo["customRecipientMsg"] = json.RawMessage(`null`)
		echo["referral"] = json.RawMessage(`""`)
		echo["quoteWaitingTimeMs"] = json.RawMessage(`0`)
		echo["originAsset"] = json.RawMessage(`"1cs_v1:btc:native"`)
		echo["destinationAsset"] = json.RawMessage(`"1cs_v1:sol:native"`)
	})
	c := newTestClient(t, srv.URL, pub, nil)

	if _, err := c.Quote(context.Background(), testQuoteRequest()); err != nil {
		t.Fatalf("benign echo drift rejected: %v", err)
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

// TestQuoteAppFeeMatrix covers every combination of a client-configured
// fee and a request's fee intent, on dry and committed quotes: an unset
// request takes the client's fees, a request's own fees replace them,
// NoAppFee sends none, and the directive itself never reaches the wire.
func TestQuoteAppFeeMatrix(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	clientFees := []AppFee{{Recipient: "0xcaribou", Fee: DefaultAppFeeBps}}
	customFees := []AppFee{{Recipient: "0xother", Fee: 5}}

	var sent map[string]json.RawMessage
	srv := newQuoteServer(t, priv, func(echo map[string]json.RawMessage) {
		sent = make(map[string]json.RawMessage, len(echo))
		for k, v := range echo {
			sent[k] = v
		}
	})

	tests := []struct {
		name       string
		clientFees []AppFee
		reqFees    []AppFee
		noAppFee   bool
		want       []AppFee // nil: no appFees member sent
	}{
		{"client fee, request unset", clientFees, nil, false, clientFees},
		{"client fee, request custom", clientFees, customFees, false, customFees},
		{"client fee, request none", clientFees, nil, true, nil},
		{"no client fee, request unset", nil, nil, false, nil},
		{"no client fee, request custom", nil, customFees, false, customFees},
		{"no client fee, request none", nil, nil, true, nil},
	}
	for _, tt := range tests {
		for _, dry := range []bool{true, false} {
			c := newTestClient(t, srv.URL, pub, tt.clientFees)
			req := testQuoteRequest()
			req.Dry = dry
			req.AppFees = tt.reqFees
			req.NoAppFee = tt.noAppFee
			sent = nil
			if _, err := c.Quote(context.Background(), req); err != nil {
				t.Fatalf("%s (dry %t): Quote: %v", tt.name, dry, err)
			}
			for k := range sent {
				if strings.EqualFold(k, "noAppFee") {
					t.Fatalf("%s (dry %t): directive %q sent", tt.name, dry, k)
				}
			}
			raw, ok := sent["appFees"]
			if tt.want == nil {
				if ok {
					t.Fatalf("%s (dry %t): appFees sent: %s", tt.name, dry, raw)
				}
				continue
			}
			var got []AppFee
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%s (dry %t): bad appFees %s: %v", tt.name, dry, raw, err)
			}
			if len(got) != len(tt.want) || got[0] != tt.want[0] {
				t.Fatalf("%s (dry %t): appFees = %+v, want %+v", tt.name, dry, got, tt.want)
			}
			if tt.reqFees == nil && req.AppFees != nil {
				t.Fatalf("%s (dry %t): caller's request mutated", tt.name, dry)
			}
		}
	}

	// A request that both supplies fees and disables them is contradictory
	// and is refused before anything is sent.
	c := newTestClient(t, srv.URL, pub, clientFees)
	req := testQuoteRequest()
	req.AppFees = customFees
	req.NoAppFee = true
	sent = nil
	if _, err := c.Quote(context.Background(), req); err == nil {
		t.Fatalf("contradictory fee request accepted")
	}
	if sent != nil {
		t.Fatalf("contradictory fee request reached the service")
	}
}

// TestQuoteCustomRecipientMsg checks that a recipient message is sent
// verbatim, covered by the echo cross-check whether altered or dropped,
// and covered by the signature.
func TestQuoteCustomRecipientMsg(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	const msg = `{"w":[{"EVM":"242c9fc3162321d326892cfe044fc0d9ffc3e9b0"}],"b":{"r":"BurrowSupply"}}`

	var sentMsg json.RawMessage
	srv := newQuoteServer(t, priv, func(echo map[string]json.RawMessage) {
		sentMsg = echo["customRecipientMsg"]
	})
	c := newTestClient(t, srv.URL, pub, nil)
	req := testQuoteRequest()
	req.CustomRecipientMsg = msg
	resp, err := c.Quote(context.Background(), req)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	var got string
	if err := json.Unmarshal(sentMsg, &got); err != nil || got != msg {
		t.Fatalf("message not sent verbatim: %s", sentMsg)
	}
	if resp.Request.CustomRecipientMsg != msg {
		t.Fatalf("echoed message not decoded: %q", resp.Request.CustomRecipientMsg)
	}

	// The signature covers the message: altering the echoed value after
	// signing breaks verification.
	tampered := strings.Replace(string(resp.rawRequest), "BurrowSupply", "BurrowCollateral", 1)
	if tampered == string(resp.rawRequest) {
		t.Fatalf("tampering had no effect")
	}
	resp.rawRequest = json.RawMessage(tampered)
	if err := VerifyQuoteSignature(resp, pub); err == nil {
		t.Fatalf("altered recipient message passed signature verification")
	}

	// A service that signs a different or missing message is caught by
	// the echo cross-check.
	for name, mangled := range map[string]json.RawMessage{
		"altered": json.RawMessage(`"{\"w\":[],\"b\":{\"r\":\"BurrowCollateral\"}}"`),
		"dropped": json.RawMessage(`null`),
		"empty":   json.RawMessage(`""`),
	} {
		srv := newQuoteServer(t, priv, func(echo map[string]json.RawMessage) {
			echo["customRecipientMsg"] = mangled
		})
		c := newTestClient(t, srv.URL, pub, nil)
		req := testQuoteRequest()
		req.CustomRecipientMsg = msg
		if _, err := c.Quote(context.Background(), req); err == nil || !strings.Contains(err.Error(), "differs from submitted request") {
			t.Fatalf("%s: expected echo mismatch rejection, got %v", name, err)
		}
	}
}
