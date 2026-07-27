// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/decred/base58"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestErc191Vector checks prehashing and recovery against the test vector
// from the verifier's own ERC-191 crate (near/intents
// crates/signatures/erc191): a known public key, message, and 65-byte
// recoverable signature with a raw (0/1) recovery byte.
func TestErc191Vector(t *testing.T) {
	pubKeyB, _ := hex.DecodeString("85a66984273f338ce4ef7b85e5430b008307e8591bb7c1b980852cf6423770b801f41e9438155eb53a5e20f748640093bb42ae3aeca035f7b7fd7a1a21f22f68")
	sigB, _ := hex.DecodeString("7800a70d05cde2c49ed546a6ce887ce6027c2c268c0285f6efef0cdfc4366b23643790f67a86468ee8301ed12cfffcb07c6530f90a9327ec057800fabd332e4701")
	msg := []byte("Hello world!")

	pub, err := crypto.SigToPub(erc191Prehash(msg), sigB)
	if err != nil {
		t.Fatalf("SigToPub: %v", err)
	}
	// crypto.FromECDSAPub returns the SEC-1 uncompressed form with the
	// leading 0x04 tag; the vector omits the tag.
	if got := crypto.FromECDSAPub(pub)[1:]; !strings.EqualFold(hex.EncodeToString(got), hex.EncodeToString(pubKeyB)) {
		t.Fatalf("recovered pubkey %x, want %x", got, pubKeyB)
	}

	// The same recovery must flow through the string-encoded path.
	identity, err := recoverIdentity(msg, "secp256k1:"+base58.Encode(sigB))
	if err != nil {
		t.Fatalf("recoverIdentity: %v", err)
	}
	wantIdentity := strings.ToLower(crypto.PubkeyToAddress(*pub).Hex())
	if identity != wantIdentity {
		t.Fatalf("recovered identity %s, want %s", identity, wantIdentity)
	}
}

// TestSignRoundTrip signs a realistic payload with a fresh key and checks
// that the recovered identity matches the key's address, that the
// signature parses as the verifier's wire form, and that the recovery
// byte is raw 0/1.
func TestSignRoundTrip(t *testing.T) {
	priv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	identity := strings.ToLower(crypto.PubkeyToAddress(priv.PublicKey).Hex())

	nonce, err := buildNonce("01ab23cd")
	if err != nil {
		t.Fatalf("buildNonce: %v", err)
	}
	payload, err := json.Marshal(&sweepPayload{
		SignerID:          identity,
		VerifyingContract: verifierContract,
		Deadline:          "2026-08-01T00:00:00Z",
		Nonce:             nonce,
		Intents: []ftWithdrawIntent{{
			Intent:     "ft_withdraw",
			Token:      "usdc.near",
			ReceiverID: "treasury-ops.near",
			Amount:     "1000000",
		}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	sig, err := signPayload(payload, priv)
	if err != nil {
		t.Fatalf("signPayload: %v", err)
	}
	if !strings.HasPrefix(sig, "secp256k1:") {
		t.Fatalf("signature missing curve prefix: %s", sig)
	}
	sigB := base58.Decode(strings.TrimPrefix(sig, "secp256k1:"))
	if len(sigB) != 65 {
		t.Fatalf("signature is %d bytes, want 65", len(sigB))
	}
	if v := sigB[64]; v != 0 && v != 1 {
		t.Fatalf("recovery byte is %d, want raw 0 or 1", v)
	}

	recovered, err := recoverIdentity(payload, sig)
	if err != nil {
		t.Fatalf("recoverIdentity: %v", err)
	}
	if recovered != identity {
		t.Fatalf("recovered %s, want %s", recovered, identity)
	}

	// A different key must not recover to the same identity.
	other, _ := crypto.GenerateKey()
	otherSig, err := signPayload(payload, other)
	if err != nil {
		t.Fatalf("signPayload (other): %v", err)
	}
	if rec, _ := recoverIdentity(payload, otherSig); rec == identity {
		t.Fatal("foreign key recovered to the treasury identity")
	}
}

// TestBuildNonce checks the salt-prefixed nonce form: 32 bytes, salt
// first, random tail, unique per call.
func TestBuildNonce(t *testing.T) {
	n1, err := buildNonce("a1b2c3d4")
	if err != nil {
		t.Fatalf("buildNonce: %v", err)
	}
	b1, err := base64.StdEncoding.DecodeString(n1)
	if err != nil {
		t.Fatalf("nonce is not base64: %v", err)
	}
	if len(b1) != 32 {
		t.Fatalf("nonce is %d bytes, want 32", len(b1))
	}
	if hex.EncodeToString(b1[:4]) != "a1b2c3d4" {
		t.Fatalf("nonce salt prefix %x, want a1b2c3d4", b1[:4])
	}
	n2, _ := buildNonce("a1b2c3d4")
	if n1 == n2 {
		t.Fatal("two nonces are identical")
	}

	for _, bad := range []string{"", "a1b2c3", "a1b2c3d4e5", "zzzzzzzz"} {
		if _, err := buildNonce(bad); err == nil {
			t.Fatalf("buildNonce accepted bad salt %q", bad)
		}
	}
}

// TestPayloadShape pins the wire-visible JSON of the signed envelope: the
// erc191 standard tag, the payload embedded as a string, and the intent's
// field names as the verifier's serde expects them.
func TestPayloadShape(t *testing.T) {
	payload, err := json.Marshal(&sweepPayload{
		SignerID:          "0x5674a958b11fe85265a58e71ac5fda5122bad7aa",
		VerifyingContract: verifierContract,
		Deadline:          "2026-08-01T00:00:00Z",
		Nonce:             "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Intents: []ftWithdrawIntent{{
			Intent:     "ft_withdraw",
			Token:      "btc.omft.near",
			ReceiverID: "btc.omft.near",
			Amount:     "125000",
			Memo:       "WITHDRAW_TO:bc1qexampleaddress",
		}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{"signer_id", "verifying_contract", "deadline", "nonce", "intents"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("payload is missing %q: %s", key, payload)
		}
	}
	intent := decoded["intents"].([]any)[0].(map[string]any)
	for _, key := range []string{"intent", "token", "receiver_id", "amount", "memo"} {
		if _, ok := intent[key]; !ok {
			t.Fatalf("intent is missing %q: %s", key, payload)
		}
	}
	if intent["intent"] != "ft_withdraw" {
		t.Fatalf("intent tag %v, want ft_withdraw", intent["intent"])
	}

	envelope, err := json.Marshal(&signedEnvelope{Signed: []multiPayload{{
		Standard:  "erc191",
		Payload:   string(payload),
		Signature: "secp256k1:3XA2eEeVDbjsHV5EQBBnFAs1AL3RJZbFvcNQjfDavGV4a1t9YFmbF63R2oPtdb2i9GUwyPGkrCDzMGYSLnWEZPh1B",
	}}})
	if err != nil {
		t.Fatalf("Marshal envelope: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(envelope, &env); err != nil {
		t.Fatalf("Unmarshal envelope: %v", err)
	}
	mp := env["signed"].([]any)[0].(map[string]any)
	if mp["standard"] != "erc191" {
		t.Fatalf("standard tag %v, want erc191", mp["standard"])
	}
	if _, isString := mp["payload"].(string); !isString {
		t.Fatal("payload must be embedded as a JSON string, not an object")
	}
}
