// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package intents

import (
	"crypto/ed25519"
	"encoding/json"

	"github.com/decred/base58"
)

// SimnetSigningKey is the public half of the deterministic dev keypair the
// simnet 1Click mock server (util/testing/near-intents) signs quotes with.
// The private half is derived in that package from a fixed seed; a test
// there asserts the derivation matches this constant. Determinism, not
// secrecy, is the point — the key authenticates nothing outside a local
// simnet session.
const SimnetSigningKey = "ed25519:4oUiaE4tQJN3nemi9EmiKQ4L27EZaGHqXTqsFYX9zNEi"

// SignQuote computes the service-side signature for a quote response
// envelope — the same canonicalization VerifyQuoteSignature checks.
// rawRequest and rawQuote are the exact bytes to be emitted as the
// envelope's quoteRequest and quote members. The production service holds
// its own key; this exists for mock and test servers.
func SignQuote(priv ed25519.PrivateKey, rawRequest, rawQuote json.RawMessage, timestamp string) (string, error) {
	msg, err := signedPayload(&QuoteResponse{
		Timestamp:  timestamp,
		rawRequest: rawRequest,
		rawQuote:   rawQuote,
	})
	if err != nil {
		return "", err
	}
	return "ed25519:" + base58.Encode(ed25519.Sign(priv, []byte(msg))), nil
}
