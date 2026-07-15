package intents

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadFixture decodes a captured live API response from testdata. The
// fixtures are real signed responses from the production service
// (2026-07-14), making them end-to-end vectors for the canonicalization
// and signature scheme.
func loadFixture(t *testing.T, name string, thing any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("error reading fixture %s: %v", name, err)
	}
	if err := json.Unmarshal(b, thing); err != nil {
		t.Fatalf("error decoding fixture %s: %v", name, err)
	}
}

func pinnedKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	key := decodeEd25519Base58(OneClickSigningKey)
	if len(key) != ed25519.PublicKeySize {
		t.Fatalf("bad pinned key")
	}
	return key
}

func TestVerifyLiveDryQuote(t *testing.T) {
	resp := new(QuoteResponse)
	loadFixture(t, "quote-dry.json", resp)
	if err := VerifyQuoteSignature(resp, pinnedKey(t)); err != nil {
		t.Fatalf("live dry quote failed verification: %v", err)
	}
}

func TestVerifyLiveCommittedQuote(t *testing.T) {
	resp := new(QuoteResponse)
	loadFixture(t, "quote-committed.json", resp)
	if err := VerifyQuoteSignature(resp, pinnedKey(t)); err != nil {
		t.Fatalf("live committed quote failed verification: %v", err)
	}
	if resp.Quote.DepositAddress == "" {
		t.Fatalf("committed fixture has no deposit address")
	}
}

func TestStatusEmbeddedQuoteNotVerifiable(t *testing.T) {
	status := new(StatusResponse)
	loadFixture(t, "status-pending.json", status)
	if status.Status != StatusPendingDeposit {
		t.Fatalf("unexpected fixture status %s", status.Status)
	}
	if status.QuoteResponse == nil {
		t.Fatalf("no embedded quote response")
	}
	// The status endpoint re-serializes the stored request rather than
	// echoing the exact bytes the signature was computed over (observed
	// live: originAsset comes back un-normalized). Consequently
	// status-embedded quote responses do NOT re-verify, by construction.
	// Quotes are verified once, from the /quote response, and persisted;
	// nothing in the status flow may rely on re-verification. This test
	// documents the constraint: if it starts failing (i.e. verification
	// starts succeeding), the constraint can be revisited.
	if err := VerifyQuoteSignature(status.QuoteResponse, pinnedKey(t)); err == nil {
		t.Fatalf("status-embedded quote unexpectedly verified; re-verification constraint can be revisited")
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	resp := new(QuoteResponse)
	loadFixture(t, "quote-committed.json", resp)

	// Substitute the deposit address in the raw quote — the attack this
	// verification exists to catch.
	tampered := strings.Replace(string(resp.rawQuote),
		resp.Quote.DepositAddress, "bc1qattackeraddressxxxxxxxxxxxxxxxxxxxxxx", 1)
	if tampered == string(resp.rawQuote) {
		t.Fatalf("tampering had no effect")
	}
	resp.rawQuote = json.RawMessage(tampered)
	if err := VerifyQuoteSignature(resp, pinnedKey(t)); err == nil {
		t.Fatalf("tampered deposit address passed verification")
	}

	// Tampered timestamp must also fail.
	resp = new(QuoteResponse)
	loadFixture(t, "quote-committed.json", resp)
	resp.Timestamp = "2027-01-01T00:00:00.000Z"
	if err := VerifyQuoteSignature(resp, pinnedKey(t)); err == nil {
		t.Fatalf("tampered timestamp passed verification")
	}

	// Wrong key must fail.
	resp = new(QuoteResponse)
	loadFixture(t, "quote-committed.json", resp)
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := VerifyQuoteSignature(resp, pub); err == nil {
		t.Fatalf("wrong key passed verification")
	}
}

func TestIsFalsyJSON(t *testing.T) {
	for raw, falsy := range map[string]bool{
		`null`: true, `""`: true, `0`: true, `false`: true, ``: true,
		`"0"`: false, `1`: false, `true`: false, `"x"`: false, `802`: false,
	} {
		if got := isFalsyJSON(json.RawMessage(raw)); got != falsy {
			t.Errorf("isFalsyJSON(%s) = %v, want %v", raw, got, falsy)
		}
	}
}
