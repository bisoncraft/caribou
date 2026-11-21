// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
)

// An Offer is what this wallet commits to before it joins a shared
// registration that someone else builds: how much it stakes, the keys and
// scripts of its share, the inputs it funds it with, and the most it will pay
// towards the fee. The registration it is asked to sign is checked against the
// offer, so the share in the registration is the one the offer describes and
// not the one the builder wrote.

// OfferState is where an offer is in its life.
type OfferState string

const (
	// OfferOpen is an offer that has signed nothing.
	OfferOpen OfferState = "open"
	// OfferConsented has signed its consent to one registration, ConsentHash.
	OfferConsented OfferState = "consented"
	// OfferSigned has signed its inputs, so the registration can be broadcast
	// by anyone who holds it. Its ConsentHash is the registration it signed.
	OfferSigned OfferState = "signed"
	// OfferBroadcast has had its registration broadcast.
	OfferBroadcast OfferState = "broadcast"
)

var (
	// ErrOfferMismatch is returned for a registration that does not match the
	// offer.
	ErrOfferMismatch = errors.New("registration does not match the offer")
	// ErrUnknownOffer is returned for an offer that is not stored.
	ErrUnknownOffer = errors.New("unknown offer")
	// ErrReviewChanged is returned when what was reviewed is not what is
	// being signed.
	ErrReviewChanged = errors.New("the registration changed since it was reviewed")
)

// Offer is a share owner's commitment to a shared registration.
type Offer struct {
	// ID is random, and is the lock id of the funding leases.
	ID                   [32]byte
	CreatedAt, ExpiresAt int64
	State                OfferState
	// Amount is the share, in duffs.
	Amount int64
	// MaxFee is the most the offer pays towards the fee.
	MaxFee     int64
	OwnerIndex uint32
	OwnerKeyID [protx.KeyIDSize]byte
	// RefundScript and RewardScript are the share's refund and reward scripts.
	RefundScript, RewardScript []byte
	// ChangeScript receives what the inputs hold beyond the share and the fee.
	ChangeScript []byte
	Inputs       []FundingInput
	// ConsentHash is set when the offer consents, SignedDigest when it signs
	// its inputs, and TxID when the registration is broadcast.
	ConsentHash  chainhash.Hash
	SignedDigest chainhash.Hash
	TxID         chainhash.Hash
}

// OfferReview is a registration reviewed against an offer.
type OfferReview struct {
	*RegReview
	// ShareIndex is the offer's place in the share table.
	ShareIndex int
	// ID binds this review to the offer and to every byte of the
	// registration the user saw. Signing needs it.
	ID [32]byte
}

// Own returns what the offer expects of tx. The share is the offer's: it must
// appear in the registration exactly once, found by its owner key, with the
// offer's amount, refund script and reward script. What the inputs hold beyond
// the share and the change is the fee portion, measured here and held to
// MaxFee, since the owner is never told the total.
func (o *Offer) Own(tx *bisonwire.Tx) (Own, error) {
	var pr protx.ProRegTx
	if err := protx.Parse(tx.ExtraPayload, &pr); err != nil {
		return Own{}, fmt.Errorf("%w: payload: %w", ErrInvalidProposal, err)
	}
	if o.MaxFee < 0 || o.MaxFee > MaxFeePortion {
		return Own{}, fmt.Errorf("%w: fee cap %d", ErrOfferMismatch, o.MaxFee)
	}
	idx := -1
	for i := range pr.Shares {
		if pr.Shares[i].KeyIDOwner != o.OwnerKeyID {
			continue
		}
		if idx >= 0 {
			return Own{}, fmt.Errorf("%w: the owner key is in two shares", ErrOfferMismatch)
		}
		idx = i
	}
	if idx < 0 {
		return Own{}, fmt.Errorf("%w: no share has the offer's owner key", ErrOfferMismatch)
	}
	share := &pr.Shares[idx]
	if share.Amount != o.Amount || !bytes.Equal(share.RefundScript, o.RefundScript) ||
		!bytes.Equal(share.RewardScript, o.RewardScript) {
		return Own{}, fmt.Errorf("%w: share %d is not the offer's", ErrOfferMismatch, idx)
	}
	ci := int(pr.Collateral.Index)
	var in, change int64
	seenIn := make(map[wire.OutPoint]struct{}, len(o.Inputs))
	for _, fi := range o.Inputs {
		if _, dup := seenIn[fi.OutPoint]; dup {
			return Own{}, fmt.Errorf("%w: input %v is listed twice", ErrOfferMismatch, fi.OutPoint)
		}
		seenIn[fi.OutPoint] = struct{}{}
		in += fi.Value
	}
	for i, out := range tx.TxOut {
		if i != ci && len(o.ChangeScript) != 0 && bytes.Equal(out.PkScript, o.ChangeScript) {
			change += out.Value
		}
	}
	portion := in - change - o.Amount
	if portion < 0 || portion > o.MaxFee {
		return Own{}, fmt.Errorf("%w: the offer would pay %d beyond its share, at most %d", ErrOfferMismatch, portion, o.MaxFee)
	}
	terms := Terms{
		Shares:            slices.Clone(pr.Shares),
		NetInfo:           pr.NetInfo,
		PubKeyOperator:    pr.PubKeyOperator,
		KeyIDVoting:       pr.KeyIDVoting,
		OperatorReward:    pr.OperatorReward,
		EarlyPeriodBlocks: pr.EarlyPeriodBlocks,
		EarlyPenalty:      pr.EarlyPenalty,
	}
	// The share is the offer's, whatever the registration says.
	terms.Shares[idx].Amount = o.Amount
	terms.Shares[idx].KeyIDOwner = o.OwnerKeyID
	terms.Shares[idx].RefundScript = slices.Clone(o.RefundScript)
	terms.Shares[idx].RewardScript = slices.Clone(o.RewardScript)
	// Fee is not what the owners agreed to: Review splits it evenly, so this
	// total gives the offer exactly its own portion.
	return Own{
		ShareIndex:   idx,
		Terms:        terms,
		Fee:          portion * int64(len(pr.Shares)),
		Inputs:       slices.Clone(o.Inputs),
		ChangeScript: slices.Clone(o.ChangeScript),
	}, nil
}

// Review checks tx against the offer and returns it for signing. An offer that
// has consented or signed accepts only the registration it consented to, which
// lets a signature that was lost on its way back be asked for again.
func (o *Offer) Review(tx *bisonwire.Tx, rules NetRules) (*OfferReview, error) {
	if o.State != OfferOpen && o.State != OfferConsented && o.State != OfferSigned {
		return nil, fmt.Errorf("%w: the offer is %s", ErrOfferMismatch, o.State)
	}
	// A signed offer's own inputs carry its signatures in the blob it gave out
	// and in the final one. Review refuses an own input that is already signed,
	// so look at the registration without them: signing it again gives the same
	// signatures, which the caller checks against what it stored.
	if o.State == OfferSigned {
		var err error
		if tx, err = o.withoutOwnScripts(tx); err != nil {
			return nil, err
		}
	}
	own, err := o.Own(tx)
	if err != nil {
		return nil, err
	}
	rv, err := Review(tx, rules, own)
	if err != nil {
		return nil, err
	}
	if (o.State == OfferConsented || o.State == OfferSigned) && rv.ConsentHash != o.ConsentHash {
		return nil, fmt.Errorf("%w: the offer consented to another registration", ErrOfferMismatch)
	}
	id, err := o.ReviewID(tx, own.ShareIndex, rv.FeePortion)
	if err != nil {
		return nil, err
	}
	return &OfferReview{RegReview: rv, ShareIndex: own.ShareIndex, ID: id}, nil
}

// withoutOwnScripts is a copy of tx whose inputs that the offer funds have no
// signature script.
func (o *Offer) withoutOwnScripts(tx *bisonwire.Tx) (*bisonwire.Tx, error) {
	cp, err := cloneTx(tx)
	if err != nil {
		return nil, err
	}
	for _, in := range cp.TxIn {
		for _, fi := range o.Inputs {
			if in.PreviousOutPoint == fi.OutPoint {
				in.SignatureScript = nil
			}
		}
	}
	return cp, nil
}

// CanonicalTx is the serialization of tx without its input signature scripts,
// and without the join signatures in its payload if stripJoinSigs. It is what
// stays the same as the owners add their signatures.
func CanonicalTx(tx *bisonwire.Tx, stripJoinSigs bool) ([]byte, error) {
	cp, err := cloneTx(tx)
	if err != nil {
		return nil, err
	}
	for _, in := range cp.TxIn {
		in.SignatureScript = nil
	}
	if stripJoinSigs {
		var pr protx.ProRegTx
		if err := protx.Parse(cp.ExtraPayload, &pr); err != nil {
			return nil, fmt.Errorf("%w: payload: %w", ErrInvalidProposal, err)
		}
		for i := range pr.Shares {
			pr.Shares[i].JoinSig = [protx.CompactSigSize]byte{}
		}
		if err := setPayload(cp, &pr); err != nil {
			return nil, err
		}
	}
	var b bytes.Buffer
	if err := cp.Serialize(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// SignedDigest is what a broadcast checks the registration against: the
// transaction without its input scripts, with the join signatures and every
// output.
func SignedDigest(tx *bisonwire.Tx) (chainhash.Hash, error) {
	b, err := CanonicalTx(tx, false)
	if err != nil {
		return chainhash.Hash{}, err
	}
	return chainhash.DoubleHashH(b), nil
}

const reviewIDDomain = "caribou/dash-shared-reg-review/v1"

func putBytes(h interface{ Write([]byte) (int, error) }, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	h.Write(n[:])
	h.Write(b)
}

func putInt(h interface{ Write([]byte) (int, error) }, v int64) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(v))
	h.Write(n[:])
}

// ReviewID is a hash of the offer fields the review depends on, the share
// index, the fee portion and the registration without its signatures. A user
// who approves an ID approves exactly that, and nothing is stored: signing
// recomputes it from the registration it is given. Join signatures and input
// scripts are left out, so the ID is the same before and after the other
// owners sign.
func (o *Offer) ReviewID(tx *bisonwire.Tx, shareIndex int, portion int64) ([32]byte, error) {
	canon, err := CanonicalTx(tx, true)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	putBytes(h, []byte(reviewIDDomain))
	h.Write(o.ID[:])
	putInt(h, o.Amount)
	putInt(h, o.MaxFee)
	h.Write(o.OwnerKeyID[:])
	putBytes(h, o.RefundScript)
	putBytes(h, o.RewardScript)
	putBytes(h, o.ChangeScript)
	putInt(h, int64(len(o.Inputs)))
	for _, fi := range o.Inputs {
		h.Write(fi.OutPoint.Hash[:])
		putInt(h, int64(fi.OutPoint.Index))
		putInt(h, fi.Value)
		putBytes(h, fi.PkScript)
	}
	putInt(h, int64(shareIndex))
	putInt(h, portion)
	putBytes(h, canon)
	var id [32]byte
	copy(id[:], h.Sum(nil))
	return id, nil
}
