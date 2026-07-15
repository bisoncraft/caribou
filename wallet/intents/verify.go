// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package intents

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/decred/base58"
)

// OneClickSigningKey is the production 1Click quote-signing public key.
// Every quote response is Ed25519-signed so a client can detect
// tampering — most importantly, substitution of the deposit address by
// anything between the client and the service. The key is not published
// in the service documentation; this value matches the one pinned in the
// reference SDK (one-click-sdk-typescript, quote-signature.ts) and is
// overridable via Config.SigningKey.
const OneClickSigningKey = "ed25519:reYaWhvwu8Jzo3WUM3zhn6VrhuMEF4eADL17qtRVifc"

// The signed payload is a canonicalized flat merge of whitelisted fields
// from the echoed quoteRequest and the quote, plus the response timestamp.
// Field sets and falsy-omission behavior mirror the reference SDK: fields
// built with JavaScript's `value ? value : undefined` pattern are omitted
// when falsy (null, "", 0, false). Notably, appFees is NOT part of the
// signed payload.
var (
	// Always included from quoteRequest.
	signedRequestFields = []string{
		"dry", "swapType", "slippageTolerance", "originAsset", "depositType",
		"destinationAsset", "amount", "refundTo", "refundType", "recipient",
		"recipientType", "deadline",
	}
	// Included from quoteRequest unless falsy.
	signedRequestOptFields = []string{
		"quoteWaitingTimeMs", "referral", "virtualChainRecipient",
		"virtualChainRefundRecipient", "customRecipientMsg",
	}
	// Always included from quote (dry and committed).
	signedQuoteFields = []string{
		"amountIn", "amountInFormatted", "amountInUsd", "minAmountIn",
		"amountOut", "amountOutFormatted", "amountOutUsd", "minAmountOut",
	}
	// Included from quote unless falsy, committed quotes only.
	signedQuoteOptFields = []string{
		"depositAddress", "depositMemo", "deadline", "timeWhenInactive",
		"timeEstimate", "virtualChainRecipient", "virtualChainRefundRecipient",
		"customRecipientMsg", "refundFee", "withdrawFee",
	}
)

// isFalsyJSON reports whether a raw JSON value is JavaScript-falsy for the
// purposes of the SDK's payload construction.
func isFalsyJSON(raw json.RawMessage) bool {
	s := string(bytes.TrimSpace(raw))
	switch s {
	case "", "null", `""`, "0", "false":
		return true
	}
	return false
}

// signedPayload builds the canonical signed message for a quote response:
// the stable-stringified payload's SHA-256 digest, base58-encoded. The
// returned string's bytes are the Ed25519 message.
func signedPayload(r *QuoteResponse) (string, error) {
	var req, quote map[string]json.RawMessage
	if err := json.Unmarshal(r.rawRequest, &req); err != nil {
		return "", fmt.Errorf("error decoding raw quote request: %w", err)
	}
	if err := json.Unmarshal(r.rawQuote, &quote); err != nil {
		return "", fmt.Errorf("error decoding raw quote: %w", err)
	}

	payload := make(map[string]json.RawMessage)
	for _, k := range signedRequestFields {
		if v, ok := req[k]; ok {
			payload[k] = v
		}
	}
	for _, k := range signedRequestOptFields {
		if v, ok := req[k]; ok && !isFalsyJSON(v) {
			payload[k] = v
		}
	}
	for _, k := range signedQuoteFields {
		if v, ok := quote[k]; ok {
			payload[k] = v
		}
	}
	if isFalsyJSON(req["dry"]) { // committed quote: extended field set
		for _, k := range signedQuoteOptFields {
			if v, ok := quote[k]; ok && !isFalsyJSON(v) {
				payload[k] = v
			}
		}
	}

	ts, err := json.Marshal(r.Timestamp)
	if err != nil {
		return "", fmt.Errorf("error encoding timestamp: %w", err)
	}
	payload["timestamp"] = ts

	data, err := stableStringify(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return base58.Encode(digest[:]), nil
}

// stableStringify serializes a flat payload with lexicographically sorted
// keys and no whitespace, matching json-stable-stringify. Values are
// emitted verbatim (compacted), preserving the server's own encoding of
// numbers and strings.
func stableStringify(payload map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		if err := json.Compact(&b, payload[k]); err != nil {
			return nil, fmt.Errorf("error compacting value for %q: %w", k, err)
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// decodeEd25519Base58 decodes a base58 key or signature with an optional
// "ed25519:" prefix.
func decodeEd25519Base58(v string) []byte {
	return base58.Decode(strings.TrimPrefix(v, "ed25519:"))
}

// VerifyQuoteSignature checks a quote response's signature against the
// given signing key. A failure means the response cannot be trusted — in
// particular its deposit address — and the quote must be discarded.
func VerifyQuoteSignature(r *QuoteResponse, signingKey ed25519.PublicKey) error {
	if len(signingKey) != ed25519.PublicKeySize {
		return fmt.Errorf("bad signing key length %d", len(signingKey))
	}
	sig := decodeEd25519Base58(r.Signature)
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("bad signature length %d", len(sig))
	}
	msg, err := signedPayload(r)
	if err != nil {
		return err
	}
	if !ed25519.Verify(signingKey, []byte(msg), sig) {
		return fmt.Errorf("quote signature verification failed")
	}
	return nil
}
