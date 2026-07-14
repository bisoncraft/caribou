// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"bytes"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

const (
	fieldHdrLimitAmount = 0x63 // Amount field 3 — LimitAmount (TrustSet)

	// trustSetTransactionType is the XRPL TransactionType code for TrustSet.
	trustSetTransactionType = 20

	// tfSetNoRipple prevents rippling through a trustline. Set by default on
	// trustlines this wallet creates, per XRPL community best practice.
	tfSetNoRipple = 0x00020000

	iouMinExponent  = -96
	iouMaxExponent  = 80
	iouMantissaMin  = 1_000_000_000_000_000  // 10^15
	iouMantissaMax  = 10_000_000_000_000_000 // 10^16
	iouMantissaMask = (uint64(1) << 54) - 1
)

// encodeCurrencyCode encodes an XRPL currency code into the 20-byte currency
// field. Two forms are accepted:
//   - A 3-character ISO-style code (e.g. "USD"), placed at the standard
//     byte offset [12:15]. XRPL identifies an issued currency by the
//     (currency, issuer) pair, so different issuers may share the same
//     generic 3-letter code (e.g. multiple gateways issuing "USD").
//   - A 40-character hex string, decoded directly into the 20 bytes. This
//     is how XRPL represents non-standard currency codes (tickers that
//     don't fit the 3-letter ISO convention, e.g. Circle's "USDC" on
//     XRPL, which left-justifies the ASCII bytes at offset 0 rather than
//     using the ISO offset).
func encodeCurrencyCode(code string) ([20]byte, error) {
	var out [20]byte
	switch len(code) {
	case 3:
		copy(out[12:15], []byte(code))
		return out, nil
	case 40:
		b, err := hex.DecodeString(code)
		if err != nil || len(b) != 20 {
			return out, fmt.Errorf("invalid 40-character hex currency code %q", code)
		}
		copy(out[:], b)
		return out, nil
	default:
		return out, fmt.Errorf("currency code must be exactly 3 characters (ISO) or 40 hex characters (non-standard), got %q (%d chars)", code, len(code))
	}
}

// normalizeIOUValue converts an atomic amount (value * 10^-decimals) into
// XRPL's normalized (mantissa, exponent) representation, with mantissa in
// [10^15, 10^16) for any nonzero value. Returns an error instead of the
// amount if the atomic magnitude exceeds 10^16 and shrinking it into that
// range would drop nonzero digits.
func normalizeIOUValue(atomic uint64, decimals int) (mantissa uint64, exponent int, err error) {
	if atomic == 0 {
		return 0, 0, nil
	}
	mantissa = atomic
	exponent = -decimals
	for mantissa < iouMantissaMin {
		mantissa *= 10
		exponent--
	}
	for mantissa >= iouMantissaMax {
		if mantissa%10 != 0 {
			return 0, 0, fmt.Errorf("atomic value %d exceeds IOU precision (15 significant digits)", atomic)
		}
		mantissa /= 10
		exponent++
	}
	return mantissa, exponent, nil
}

// writeIOUAmount encodes an issued-currency Amount field: an 8-byte
// normalized value (with the "not XRP" bit set), followed by a 20-byte
// currency code and a 20-byte issuer AccountID. Only nonnegative amounts are
// supported, matching this wallet's send/receive-only use of IOUs.
func writeIOUAmount(b *bytes.Buffer, atomic uint64, decimals int, currency, issuer [20]byte) error {
	var serial uint64 = 1 << 63 // "not XRP" bit
	if atomic != 0 {
		mantissa, exponent, err := normalizeIOUValue(atomic, decimals)
		if err != nil {
			return err
		}
		if exponent < iouMinExponent || exponent > iouMaxExponent {
			return fmt.Errorf("IOU exponent %d out of range [%d, %d]", exponent, iouMinExponent, iouMaxExponent)
		}
		serial |= 1 << 62 // positive
		serial |= uint64(exponent+97) << 54
		serial |= mantissa & iouMantissaMask
	}
	buf := [8]byte{}
	binary.BigEndian.PutUint64(buf[:], serial)
	b.Write(buf[:])
	b.Write(currency[:])
	b.Write(issuer[:])
	return nil
}

// xrpTrustSet holds the fields needed to build a signed TrustSet transaction.
type xrpTrustSet struct {
	account       [20]byte
	currency      [20]byte
	issuer        [20]byte
	limitAtomic   uint64
	limitDecimals int
	flags         uint32
	fee           uint64
	sequence      uint32
	lastLedgerSeq uint32
	signingPubKey []byte
	txnSig        []byte
}

// encodeTrustSet serializes a TrustSet transaction in XRP binary format.
// Field order follows XRPL's canonical (type code, field code) ordering,
// matching the style of encodePayment.
func encodeTrustSet(t *xrpTrustSet, forSigning bool) ([]byte, error) {
	var b bytes.Buffer

	b.WriteByte(fieldHdrTransactionType)
	writeUInt16(&b, trustSetTransactionType)

	b.WriteByte(fieldHdrFlags)
	writeUInt32(&b, t.flags)

	b.WriteByte(fieldHdrSequence)
	writeUInt32(&b, t.sequence)

	b.WriteByte(fieldHdrLastLedgerSeq0)
	b.WriteByte(fieldHdrLastLedgerSeq1)
	writeUInt32(&b, t.lastLedgerSeq)

	b.WriteByte(fieldHdrLimitAmount)
	if err := writeIOUAmount(&b, t.limitAtomic, t.limitDecimals, t.currency, t.issuer); err != nil {
		return nil, err
	}

	b.WriteByte(fieldHdrFee)
	writeXRPAmount(&b, t.fee)

	b.WriteByte(fieldHdrSigningPubKey)
	writeVarBlob(&b, t.signingPubKey)

	if !forSigning && len(t.txnSig) > 0 {
		b.WriteByte(fieldHdrTxnSignature)
		writeVarBlob(&b, t.txnSig)
	}

	b.WriteByte(fieldHdrAccount)
	writeVarBlob(&b, t.account[:])

	return b.Bytes(), nil
}

// signTrustSet computes the signing hash and returns the DER-encoded ECDSA sig.
func signTrustSet(t *xrpTrustSet, privKey *btcec.PrivateKey) ([]byte, error) {
	txBytes, err := encodeTrustSet(t, true)
	if err != nil {
		return nil, err
	}
	h := sha512.New()
	h.Write(txSigningPrefix)
	h.Write(txBytes)
	msgHash := h.Sum(nil)[:32]
	sig := ecdsa.Sign(privKey, msgHash)
	return sig.Serialize(), nil
}

// xrpIOUPayment holds the fields needed to build a signed issued-currency
// Payment transaction.
type xrpIOUPayment struct {
	account        [20]byte
	destination    [20]byte
	currency       [20]byte
	issuer         [20]byte
	amountAtomic   uint64
	amountDecimals int
	fee            uint64
	sequence       uint32
	lastLedgerSeq  uint32
	destinationTag *uint32
	signingPubKey  []byte
	txnSig         []byte
}

// encodeIOUPayment serializes an issued-currency Payment transaction. Field
// order mirrors encodePayment, substituting an IOU Amount for the native
// drops Amount.
func encodeIOUPayment(p *xrpIOUPayment, forSigning bool) ([]byte, error) {
	var b bytes.Buffer

	b.WriteByte(fieldHdrTransactionType)
	writeUInt16(&b, 0) // Payment = 0

	b.WriteByte(fieldHdrFlags)
	writeUInt32(&b, 0)

	b.WriteByte(fieldHdrSequence)
	writeUInt32(&b, p.sequence)

	if p.destinationTag != nil {
		b.WriteByte(fieldHdrDestTag)
		writeUInt32(&b, *p.destinationTag)
	}

	b.WriteByte(fieldHdrLastLedgerSeq0)
	b.WriteByte(fieldHdrLastLedgerSeq1)
	writeUInt32(&b, p.lastLedgerSeq)

	b.WriteByte(fieldHdrAmount)
	if err := writeIOUAmount(&b, p.amountAtomic, p.amountDecimals, p.currency, p.issuer); err != nil {
		return nil, err
	}

	b.WriteByte(fieldHdrFee)
	writeXRPAmount(&b, p.fee)

	b.WriteByte(fieldHdrSigningPubKey)
	writeVarBlob(&b, p.signingPubKey)

	if !forSigning && len(p.txnSig) > 0 {
		b.WriteByte(fieldHdrTxnSignature)
		writeVarBlob(&b, p.txnSig)
	}

	b.WriteByte(fieldHdrAccount)
	writeVarBlob(&b, p.account[:])

	b.WriteByte(fieldHdrDestination)
	writeVarBlob(&b, p.destination[:])

	return b.Bytes(), nil
}

func signIOUPayment(p *xrpIOUPayment, privKey *btcec.PrivateKey) ([]byte, error) {
	txBytes, err := encodeIOUPayment(p, true)
	if err != nil {
		return nil, err
	}
	h := sha512.New()
	h.Write(txSigningPrefix)
	h.Write(txBytes)
	msgHash := h.Sum(nil)[:32]
	sig := ecdsa.Sign(privKey, msgHash)
	return sig.Serialize(), nil
}
