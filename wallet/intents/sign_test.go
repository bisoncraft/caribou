package intents

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"
)

// TestSignQuoteRoundTrip asserts that SignQuote produces a signature
// VerifyQuoteSignature accepts, for both dry and committed payload shapes.
func TestSignQuoteRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	const rawQuote = `{"amountIn":"10000000","amountInFormatted":"0.1","amountInUsd":"6000",` +
		`"minAmountIn":"10000000","amountOut":"83000000000","amountOutFormatted":"83",` +
		`"amountOutUsd":"5990","minAmountOut":"82000000000","refundFee":"1900",` +
		`"withdrawFee":"129432","timeEstimate":802,"deadline":"2026-07-18T12:00:00.000Z",` +
		`"timeWhenInactive":"2026-07-18T12:00:00.000Z","depositAddress":"bc1qserverdepositaddr"}`

	for _, dry := range []bool{true, false} {
		req := testQuoteRequest()
		req.Dry = dry
		rawRequest, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		const ts = "2026-07-14T23:40:38.373Z"
		sig, err := SignQuote(priv, rawRequest, []byte(rawQuote), ts)
		if err != nil {
			t.Fatalf("SignQuote (dry=%v): %v", dry, err)
		}

		resp := &QuoteResponse{
			Timestamp:  ts,
			Signature:  sig,
			rawRequest: rawRequest,
			rawQuote:   []byte(rawQuote),
		}
		if err := VerifyQuoteSignature(resp, pub); err != nil {
			t.Fatalf("VerifyQuoteSignature (dry=%v): %v", dry, err)
		}

		// A different key must not verify.
		otherPub, _, _ := ed25519.GenerateKey(nil)
		if err := VerifyQuoteSignature(resp, otherPub); err == nil {
			t.Fatalf("signature verified under wrong key (dry=%v)", dry)
		}
	}
}
