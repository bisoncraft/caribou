// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
)

// offersBucket holds the offers by id. A value is a version byte and the JSON
// of an offerRecord: unlike the fixed size records of the other buckets an
// offer is variable and nested.
var offersBucket = []byte("offers")

const offerRecordVersion = 1

type offerInput struct {
	Hash     string `json:"hash"`
	Index    uint32 `json:"index"`
	PkScript string `json:"pkScript"`
	Value    int64  `json:"value"`
}

type offerRecord struct {
	CreatedAt    int64        `json:"createdAt"`
	ExpiresAt    int64        `json:"expiresAt"`
	State        OfferState   `json:"state"`
	Amount       int64        `json:"amount"`
	MaxFee       int64        `json:"maxFee"`
	OwnerIndex   uint32       `json:"ownerIndex"`
	OwnerKeyID   string       `json:"ownerKeyID"`
	RefundScript string       `json:"refundScript"`
	RewardScript string       `json:"rewardScript"`
	ChangeScript string       `json:"changeScript"`
	Inputs       []offerInput `json:"inputs"`
	ConsentHash  string       `json:"consentHash"`
	SignedDigest string       `json:"signedDigest"`
	TxID         string       `json:"txid"`
}

func encodeOffer(o *Offer) ([]byte, error) {
	r := offerRecord{
		CreatedAt: o.CreatedAt, ExpiresAt: o.ExpiresAt, State: o.State,
		Amount: o.Amount, MaxFee: o.MaxFee, OwnerIndex: o.OwnerIndex,
		OwnerKeyID:   hex.EncodeToString(o.OwnerKeyID[:]),
		RefundScript: hex.EncodeToString(o.RefundScript),
		RewardScript: hex.EncodeToString(o.RewardScript),
		ChangeScript: hex.EncodeToString(o.ChangeScript),
		ConsentHash:  o.ConsentHash.String(),
		SignedDigest: o.SignedDigest.String(),
		TxID:         o.TxID.String(),
	}
	for _, fi := range o.Inputs {
		r.Inputs = append(r.Inputs, offerInput{
			Hash: fi.OutPoint.Hash.String(), Index: fi.OutPoint.Index,
			PkScript: hex.EncodeToString(fi.PkScript), Value: fi.Value,
		})
	}
	b, err := json.Marshal(&r)
	if err != nil {
		return nil, err
	}
	return append([]byte{offerRecordVersion}, b...), nil
}

func decodeOffer(id, v []byte) (*Offer, error) {
	if len(id) != 32 {
		return nil, errors.New("malformed offer key")
	}
	if len(v) < 2 || v[0] != offerRecordVersion {
		return nil, errors.New("unknown offer record version")
	}
	var r offerRecord
	if err := json.Unmarshal(v[1:], &r); err != nil {
		return nil, fmt.Errorf("malformed offer record: %w", err)
	}
	o := &Offer{
		CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, State: r.State,
		Amount: r.Amount, MaxFee: r.MaxFee, OwnerIndex: r.OwnerIndex,
	}
	copy(o.ID[:], id)
	switch r.State {
	case OfferOpen, OfferConsented, OfferSigned, OfferBroadcast:
	default:
		return nil, fmt.Errorf("offer state %q", r.State)
	}
	var err error
	dec := func(s string) []byte {
		b, e := hex.DecodeString(s)
		if e != nil && err == nil {
			err = e
		}
		return b
	}
	if k := dec(r.OwnerKeyID); len(k) == protx.KeyIDSize {
		copy(o.OwnerKeyID[:], k)
	} else if err == nil {
		err = errors.New("owner key id size")
	}
	o.RefundScript, o.RewardScript, o.ChangeScript = dec(r.RefundScript), dec(r.RewardScript), dec(r.ChangeScript)
	for _, in := range r.Inputs {
		h, e := chainhash.NewHashFromStr(in.Hash)
		if e != nil && err == nil {
			err = e
		}
		if h == nil {
			h = &chainhash.Hash{}
		}
		o.Inputs = append(o.Inputs, FundingInput{
			OutPoint: wire.OutPoint{Hash: *h, Index: in.Index},
			PkScript: dec(in.PkScript), Value: in.Value,
		})
	}
	for _, p := range []struct {
		dst *chainhash.Hash
		s   string
	}{{&o.ConsentHash, r.ConsentHash}, {&o.SignedDigest, r.SignedDigest}, {&o.TxID, r.TxID}} {
		if e := chainhash.Decode(p.dst, p.s); e != nil && err == nil {
			err = e
		}
	}
	if err != nil {
		return nil, fmt.Errorf("malformed offer record: %w", err)
	}
	return o, nil
}

func offers(tx walletdb.ReadTx) (walletdb.ReadBucket, error) {
	b := tx.ReadBucket(storeBucket)
	if b == nil {
		return nil, errors.New("masternode store is missing")
	}
	ob := b.NestedReadBucket(offersBucket)
	if ob == nil {
		return nil, errors.New("masternode store is missing a bucket")
	}
	return ob, nil
}

// PutOffer stores o, replacing the offer with its id.
func (s *Store) PutOffer(o *Offer) error {
	v, err := encodeOffer(o)
	if err != nil {
		return err
	}
	return walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		b := tx.ReadWriteBucket(storeBucket)
		if b == nil {
			return errors.New("masternode store is missing")
		}
		ob := b.NestedReadWriteBucket(offersBucket)
		if ob == nil {
			return errors.New("masternode store is missing a bucket")
		}
		return ob.Put(o.ID[:], v)
	})
}

// GetOffer returns the offer with id, or ErrUnknownOffer.
func (s *Store) GetOffer(id [32]byte) (*Offer, error) {
	var o *Offer
	err := walletdb.View(s.db, func(tx walletdb.ReadTx) error {
		ob, err := offers(tx)
		if err != nil {
			return err
		}
		v := ob.Get(id[:])
		if v == nil {
			return ErrUnknownOffer
		}
		o, err = decodeOffer(id[:], v)
		return err
	})
	return o, err
}

// Offers returns every stored offer.
func (s *Store) Offers() ([]*Offer, error) {
	var out []*Offer
	err := walletdb.View(s.db, func(tx walletdb.ReadTx) error {
		ob, err := offers(tx)
		if err != nil {
			return err
		}
		return ob.ForEach(func(k, v []byte) error {
			o, err := decodeOffer(k, v)
			if err != nil {
				return fmt.Errorf("offer %x: %w", k, err)
			}
			out = append(out, o)
			return nil
		})
	})
	return out, err
}

// DeleteOffer removes the offer with id. Deleting an unknown offer is not an
// error.
func (s *Store) DeleteOffer(id [32]byte) error {
	return walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		b := tx.ReadWriteBucket(storeBucket)
		if b == nil {
			return errors.New("masternode store is missing")
		}
		ob := b.NestedReadWriteBucket(offersBucket)
		if ob == nil {
			return errors.New("masternode store is missing a bucket")
		}
		return ob.Delete(id[:])
	})
}
