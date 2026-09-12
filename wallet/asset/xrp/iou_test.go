// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"bytes"
	"testing"
)

func TestEncodeCurrencyCode(t *testing.T) {
	b, err := encodeCurrencyCode("USD")
	if err != nil {
		t.Fatalf("encodeCurrencyCode error: %v", err)
	}
	want := [20]byte{}
	copy(want[12:15], []byte("USD"))
	if b != want {
		t.Fatalf("encodeCurrencyCode(%q) = %x, want %x", "USD", b, want)
	}
	if _, err := encodeCurrencyCode("USDC"); err == nil {
		t.Fatal("expected error for a bare 4-character currency code")
	}
	if _, err := encodeCurrencyCode(""); err == nil {
		t.Fatal("expected error for an empty currency code")
	}
	if _, err := encodeCurrencyCode("XRP"); err == nil {
		t.Fatal("expected error for native XRP currency code")
	}
}

func TestEncodeCurrencyCodeNonStandardHex(t *testing.T) {
	// Circle's XRPL USDC currency code: hex encoding of ASCII "USDC"
	// left-justified at byte 0, per developers.circle.com.
	const hexCode = "5553444300000000000000000000000000000000"
	b, err := encodeCurrencyCode(hexCode)
	if err != nil {
		t.Fatalf("encodeCurrencyCode error: %v", err)
	}
	if string(b[:4]) != "USDC" {
		t.Fatalf("decoded currency bytes = %q, want ASCII \"USDC\" at byte 0", b[:4])
	}
	for _, x := range b[4:] {
		if x != 0 {
			t.Fatalf("expected zero padding after byte 4, got %x", b)
		}
	}
	invalidHex := ""
	for range 40 {
		invalidHex += "z"
	}
	if _, err := encodeCurrencyCode(invalidHex); err == nil {
		t.Fatal("expected error for a 40-character string that isn't valid hex")
	}
	if _, err := encodeCurrencyCode("0000000000000000000000005852500000000000"); err == nil {
		t.Fatal("expected error for XRP's 40-character representation")
	}
	if _, err := encodeCurrencyCode("0000000000000000000000000000000000000000"); err == nil {
		t.Fatal("expected error for XRP's native all-zero representation")
	}
}

func TestNormalizeIOUValue(t *testing.T) {
	cases := []struct {
		atomic       uint64
		decimals     int
		wantMantissa uint64
		wantExponent int
	}{
		{0, 6, 0, 0},
		{1_000_000, 6, 1_000_000_000_000_000, -15}, // 1.000000 USDC
		{1, 6, 1_000_000_000_000_000, -21},         // 0.000001 USDC
		{123_456_789, 6, 1_234_567_890_000_000, -13},
	}
	for _, c := range cases {
		m, e, err := normalizeIOUValue(c.atomic, c.decimals)
		if err != nil {
			t.Fatalf("normalizeIOUValue(%d, %d) error: %v", c.atomic, c.decimals, err)
		}
		if m != c.wantMantissa || e != c.wantExponent {
			t.Fatalf("normalizeIOUValue(%d, %d) = (%d, %d), want (%d, %d)",
				c.atomic, c.decimals, m, e, c.wantMantissa, c.wantExponent)
		}
		if c.atomic != 0 && (m < iouMantissaMin || m >= iouMantissaMax) {
			t.Fatalf("mantissa %d out of normalized range for atomic=%d", m, c.atomic)
		}
	}
}

func TestNormalizeIOUValuePrecisionLoss(t *testing.T) {
	// 15_000_000_000_000_001 needs 17 significant digits to represent
	// exactly; shrinking it to fit the 16-digit mantissa ceiling would drop
	// the trailing 1, so this must error rather than silently truncate.
	if _, _, err := normalizeIOUValue(15_000_000_000_000_001, 0); err == nil {
		t.Fatal("expected error for atomic value exceeding IOU precision")
	}
	// 20_000_000_000_000_000 shrinks to 2_000_000_000_000_000 (still exact,
	// no nonzero digits dropped), so this must succeed.
	m, e, err := normalizeIOUValue(20_000_000_000_000_000, 0)
	if err != nil {
		t.Fatalf("normalizeIOUValue error: %v", err)
	}
	if m != 2_000_000_000_000_000 || e != 1 {
		t.Fatalf("normalizeIOUValue = (%d, %d), want (2000000000000000, 1)", m, e)
	}
}

func TestWriteIOUAmountZero(t *testing.T) {
	var b bytes.Buffer
	currency, _ := encodeCurrencyCode("USD")
	var issuer [20]byte
	issuer[0] = 0xAA
	if err := writeIOUAmount(&b, 0, 6, currency, issuer); err != nil {
		t.Fatalf("writeIOUAmount error: %v", err)
	}
	out := b.Bytes()
	if len(out) != 48 {
		t.Fatalf("IOU amount length = %d, want 48", len(out))
	}
	// Zero must encode as exactly the "not XRP" bit and nothing else in the
	// 8-byte value prefix.
	want := [8]byte{0x80, 0, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(out[:8], want[:]) {
		t.Fatalf("zero IOU value bytes = %x, want %x", out[:8], want)
	}
	if !bytes.Equal(out[8:28], currency[:]) {
		t.Fatal("currency bytes mismatch")
	}
	if !bytes.Equal(out[28:48], issuer[:]) {
		t.Fatal("issuer bytes mismatch")
	}
}

func TestWriteIOUAmountNonzeroSetsNotXRPAndPositiveBits(t *testing.T) {
	var b bytes.Buffer
	currency, _ := encodeCurrencyCode("USD")
	var issuer [20]byte
	if err := writeIOUAmount(&b, 1_000_000, 6, currency, issuer); err != nil {
		t.Fatalf("writeIOUAmount error: %v", err)
	}
	out := b.Bytes()
	if out[0]&0x80 == 0 {
		t.Fatal("expected 'not XRP' bit set")
	}
	if out[0]&0x40 == 0 {
		t.Fatal("expected positive bit set for a positive amount")
	}
}

func TestEncodeIOUPaymentAndTrustSetDeterministic(t *testing.T) {
	currency, _ := encodeCurrencyCode("USD")
	var acct, dest, issuer [20]byte
	acct[0] = 1
	dest[0] = 2
	issuer[0] = 3

	p := &xrpIOUPayment{
		account:        acct,
		destination:    dest,
		currency:       currency,
		issuer:         issuer,
		amountAtomic:   1_000_000,
		amountDecimals: 6,
		fee:            12,
		sequence:       5,
		lastLedgerSeq:  1000,
		signingPubKey:  bytes.Repeat([]byte{0x02}, 33),
	}
	b1, err := encodeIOUPayment(p, true)
	if err != nil {
		t.Fatalf("encodeIOUPayment error: %v", err)
	}
	b2, err := encodeIOUPayment(p, true)
	if err != nil {
		t.Fatalf("encodeIOUPayment error (2nd call): %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("encodeIOUPayment is not deterministic")
	}

	ts := &xrpTrustSet{
		account:       acct,
		currency:      currency,
		issuer:        issuer,
		limitAtomic:   1_000_000_000_000,
		limitDecimals: 6,
		flags:         tfSetNoRipple,
		fee:           12,
		sequence:      5,
		lastLedgerSeq: 1000,
		signingPubKey: bytes.Repeat([]byte{0x02}, 33),
	}
	t1, err := encodeTrustSet(ts, true)
	if err != nil {
		t.Fatalf("encodeTrustSet error: %v", err)
	}
	if len(t1) == 0 {
		t.Fatal("empty TrustSet encoding")
	}
	// TransactionType = 20 (TrustSet) must appear right after the field header.
	if t1[0] != fieldHdrTransactionType || t1[1] != 0 || t1[2] != trustSetTransactionType {
		t.Fatalf("unexpected TrustSet header bytes: %x", t1[:3])
	}
}

func TestParseDecimalToAtomic(t *testing.T) {
	cases := []struct {
		value    string
		decimals int
		want     uint64
	}{
		{"1", 6, 1_000_000},
		{"1.5", 6, 1_500_000},
		{"0.000001", 6, 1},
		{"0", 6, 0},
		{"123.456789", 6, 123_456_789},
		{"123.4567891", 6, 123_456_789}, // sub-atomic precision truncated
	}
	for _, c := range cases {
		got, err := parseDecimalToAtomic(c.value, c.decimals)
		if err != nil {
			t.Fatalf("parseDecimalToAtomic(%q, %d) error: %v", c.value, c.decimals, err)
		}
		if got != c.want {
			t.Fatalf("parseDecimalToAtomic(%q, %d) = %d, want %d", c.value, c.decimals, got, c.want)
		}
	}
	if _, err := parseDecimalToAtomic("-1", 6); err == nil {
		t.Fatal("expected error for negative value")
	}
	if _, err := parseDecimalToAtomic("not-a-number", 6); err == nil {
		t.Fatal("expected error for invalid value")
	}
}
