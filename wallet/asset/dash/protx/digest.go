// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package protx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
)

// The digests below are what the share owners of a shared masternode sign, as
// Dash Core v24.0.0 computes them (CProRegTx::MakeSharedRegConsentHash,
// CProDisTx::MakeSignHash and the payload hashes of types 11 and 12). Each is a
// double SHA-256 with no message prefix, and is signed as the raw 32 bytes: a
// chainhash.Hash holds them in that order, while Dash and String print them
// reversed. They were checked against real signatures, see the tests.
//
// None of them needs the value or script of an input being spent, so the
// transaction is the only input besides the payload.
//
// The digests are computed from the decoded fields. For the payload hashes of
// types 11 and 12 that is the same as hashing the bytes as received: every
// field is fixed size or a canonical CompactSize, so decoding and encoding again
// gives the same bytes, and the tests pin it. The consent and dissolution
// digests are not hashes of the payload, so there is no received form to
// compare with: they are right when our encoding of the decoded fields is the
// one Dash Core computes, which the known-answer tests show for real
// registrations. Dash Core normalises a netInfo it decodes (an invalid entry
// is cleared and purposes must be ascending) while this package keeps it as
// written, so the consent digest matches Dash's for any netInfo Dash accepts,
// and a payload Dash rejects would not confirm anyway.

const (
	txTypeProReg = 1
	txTypeProDis = 10

	consentMagic  = "DashSharedMNReg"
	dissolveMagic = "DashSharedMNDissolve"
)

// ErrBadSignature is returned by VerifyCompact for a signature it does not
// accept for the key.
var ErrBadSignature = errors.New("bad compact signature")

// ErrWrongTxType is returned by a digest for a transaction that is not of the
// type the payload belongs to. Dash hashes the transaction's own type, so a
// digest for another type would be valid for a transaction that cannot carry it.
var ErrWrongTxType = errors.New("wrong transaction type")

// halfOrder is half the secp256k1 group order. VerifyCompact only accepts a
// signature whose S is at most this, so a third party cannot malleate it.
var halfOrder = new(big.Int).Rsh(btcec.S256().N, 1)

// SharedRegConsentHash returns the digest every share owner signs to consent to
// the registration, which is the join signature in the payload. tx is the
// registration transaction.
//
// It binds the funding inputs (outpoints and sequences), all outputs, the
// whole share table (every share's amount, refund and reward script and owner
// key id, in order) with the penalty terms, and the registrar configuration.
// The sequences are bound because BIP68 gives them meaning, so one rewritten
// after consent could impose a long relative lock on the registration.
//
// It does not bind the collateral index, the owner key id, the payouts, the
// join signatures or the input signature scripts. The value of an input is
// fixed by its outpoint but is not written into the digest. Binding is not
// checking: a signer must itself check that its own share entry (its amount, key
// and addresses), the collateral index and its own inputs are what it intends,
// since the digest only proves that everyone signed the same thing.
//
// It is for a shared registration at version 3, so it fails for another
// version and for a registration with no shares.
func (p *ProRegTx) SharedRegConsentHash(tx *bisonwire.Tx) (chainhash.Hash, error) {
	if p.Version != VersionExtAddr {
		return chainhash.Hash{}, ErrUnsupportedVersion
	}
	if tx.Type != txTypeProReg {
		return chainhash.Hash{}, fmt.Errorf("%w: %d, want %d", ErrWrongTxType, tx.Type, txTypeProReg)
	}
	if len(p.Shares) == 0 {
		return chainhash.Hash{}, errors.New("not a shared registration: no shares")
	}

	var buf bytes.Buffer
	if err := writeString(&buf, consentMagic); err != nil {
		return chainhash.Hash{}, err
	}
	if err := writeU16s(&buf, p.Version, uint16(tx.Version), tx.Type); err != nil {
		return chainhash.Hash{}, err
	}
	if err := writeU32(&buf, tx.LockTime); err != nil {
		return chainhash.Hash{}, err
	}
	inputs := InputsHash(tx.TxIn)
	buf.Write(inputs[:])
	for _, in := range tx.TxIn {
		if err := writeU32(&buf, in.Sequence); err != nil {
			return chainhash.Hash{}, err
		}
	}
	outputs, err := outputsHash(&tx.MsgTx)
	if err != nil {
		return chainhash.Hash{}, err
	}
	buf.Write(outputs[:])

	if err := writeU16s(&buf, p.MNType, p.Mode); err != nil {
		return chainhash.Hash{}, err
	}
	if err := p.NetInfo.serialize(&buf); err != nil {
		return chainhash.Hash{}, err
	}
	if p.MNType == TypeEvo {
		buf.Write(p.PlatformNodeID[:])
	}
	// The voting key comes before the operator key, the reverse of the
	// payload.
	buf.Write(p.KeyIDVoting[:])
	buf.Write(p.PubKeyOperator[:])
	if err := writeU16s(&buf, p.OperatorReward); err != nil {
		return chainhash.Hash{}, err
	}
	if err := writeCount8(&buf, "shares", len(p.Shares)); err != nil {
		return chainhash.Hash{}, err
	}
	for i := range p.Shares {
		if err := p.Shares[i].serialize(&buf); err != nil {
			return chainhash.Hash{}, err
		}
	}
	if err := writeU32(&buf, p.EarlyPeriodBlocks); err != nil {
		return chainhash.Hash{}, err
	}
	if err := writeU64(&buf, uint64(p.EarlyPenalty)); err != nil {
		return chainhash.Hash{}, err
	}
	return chainhash.DoubleHashH(buf.Bytes()), nil
}

// SignHash returns the digest the signatures of a dissolution cover. tx is the
// dissolution transaction, and must be of that type. The number of signatures
// in the payload is part of the digest: one is a unilateral dissolution by the
// actor share (penalised during the early period) and one per share is a
// unanimous one, so a signature cannot be reused to reinterpret one kind as
// the other. Set Sigs to the signatures the transaction will carry, or to as
// many placeholders, before signing. The digest covers the transaction's
// outpoints, sequences and outputs directly, which is why the payload has no
// inputs hash.
//
// A unilateral signature is by the actor share's owner, and signature i of a
// unanimous one by share i's.
//
// It does not check that the signature count is 1 or the number of shares, or
// that ActorIndex is a share of the masternode, which Dash Core enforces when
// it validates the transaction, so the caller must.
func (p *ProDisTx) SignHash(tx *bisonwire.Tx) (chainhash.Hash, error) {
	if p.Version != SharedVersion {
		return chainhash.Hash{}, ErrUnsupportedVersion
	}
	if tx.Type != txTypeProDis {
		return chainhash.Hash{}, fmt.Errorf("%w: %d, want %d", ErrWrongTxType, tx.Type, txTypeProDis)
	}

	var buf bytes.Buffer
	if err := writeString(&buf, dissolveMagic); err != nil {
		return chainhash.Hash{}, err
	}
	if err := writeU16s(&buf, p.Version, uint16(tx.Version), tx.Type); err != nil {
		return chainhash.Hash{}, err
	}
	if err := writeU32(&buf, tx.LockTime); err != nil {
		return chainhash.Hash{}, err
	}
	for _, in := range tx.TxIn {
		buf.Write(in.PreviousOutPoint.Hash[:])
		if err := writeU32(&buf, in.PreviousOutPoint.Index); err != nil {
			return chainhash.Hash{}, err
		}
	}
	for _, in := range tx.TxIn {
		if err := writeU32(&buf, in.Sequence); err != nil {
			return chainhash.Hash{}, err
		}
	}
	for _, out := range tx.TxOut {
		if err := wire.WriteTxOut(&buf, 0, 0, out); err != nil {
			return chainhash.Hash{}, err
		}
	}
	buf.Write(p.ProTxHash[:])
	if err := writeU16s(&buf, p.ActorIndex); err != nil {
		return chainhash.Hash{}, err
	}
	if err := writeCount8(&buf, "signatures", len(p.Sigs)); err != nil {
		return chainhash.Hash{}, err
	}
	return chainhash.DoubleHashH(buf.Bytes()), nil
}

// PayloadHash returns the digest the owner of the share signs: the payload
// without its signature. Its inputs hash binds the transaction's inputs.
func (p *ProUpShareTx) PayloadHash() (chainhash.Hash, error) {
	return payloadHash(func(w io.Writer) error { return p.serialize(w, false) })
}

// PayloadHash returns the digest every share owner signs: the payload without
// its signatures. Signature i is by share i. Its inputs hash binds the
// transaction's inputs.
func (p *ProUpSharedRegTx) PayloadHash() (chainhash.Hash, error) {
	return payloadHash(p.serializeBody)
}

// VerifyCompact checks a share owner's compact signature: 65 bytes, a header
// between 27 and 34 and a recovered public key whose hash160, in the form the
// header says, is keyID (the header is checked by the recovery).
// It also requires S to be at most half the group order, as Dash Core's
// CHashSigner::VerifyHashCanonical does for shared payloads, so a high-S
// signature would not confirm. It also keeps a signature from being changed by
// a third party without the key.
func VerifyCompact(hash chainhash.Hash, keyID [KeyIDSize]byte, sig []byte) error {
	if len(sig) != CompactSigSize {
		return fmt.Errorf("%w: %d bytes", ErrBadSignature, len(sig))
	}
	if new(big.Int).SetBytes(sig[33:]).Cmp(halfOrder) > 0 {
		return fmt.Errorf("%w: S is not low", ErrBadSignature)
	}

	pub, compressed, err := ecdsa.RecoverCompact(sig, hash[:])
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadSignature, err)
	}
	serialized := pub.SerializeUncompressed()
	if compressed {
		serialized = pub.SerializeCompressed()
	}
	if !bytes.Equal(btcutil.Hash160(serialized), keyID[:]) {
		return fmt.Errorf("%w: signed by another key", ErrBadSignature)
	}
	return nil
}

// SignCompact signs hash with key as a share owner does: a compact signature
// for a compressed key, with a low S, which VerifyCompact accepts.
func SignCompact(key *btcec.PrivateKey, hash chainhash.Hash) [CompactSigSize]byte {
	var sig [CompactSigSize]byte
	copy(sig[:], ecdsa.SignCompact(key, hash[:], true))
	return sig
}

// outputsHash is the double SHA-256 of the transaction's outputs written one
// after another, with no count.
func outputsHash(tx *wire.MsgTx) (chainhash.Hash, error) {
	var buf bytes.Buffer
	for _, out := range tx.TxOut {
		if err := wire.WriteTxOut(&buf, 0, 0, out); err != nil {
			return chainhash.Hash{}, err
		}
	}
	return chainhash.DoubleHashH(buf.Bytes()), nil
}

// writeString writes a string as Dash Core's serializer does: a CompactSize
// length and the bytes.
func writeString(w io.Writer, s string) error {
	if err := wire.WriteVarInt(w, 0, uint64(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}
