package masternode

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
)

// offerOf is the offer of owner i of the fixture: its share, keys, inputs and
// change are the ones the registration was built from.
func offerOf(o owner, tx *bisonwire.Tx) *Offer {
	var pr protx.ProRegTx
	if err := protx.Parse(tx.ExtraPayload, &pr); err != nil {
		panic(err)
	}
	s := pr.Shares[o.own.ShareIndex]
	return &Offer{
		ID:    [32]byte{1, byte(o.own.ShareIndex)},
		State: OfferOpen, Amount: s.Amount, MaxFee: testFee,
		OwnerKeyID:   s.KeyIDOwner,
		RefundScript: s.RefundScript, RewardScript: s.RewardScript,
		ChangeScript: o.own.ChangeScript, Inputs: o.own.Inputs,
	}
}

func TestOfferReview(t *testing.T) {
	owners, tx := build(t)
	for i, o := range owners {
		off := offerOf(o, tx)
		rv, err := off.Review(tx, RegtestRules)
		if err != nil {
			t.Fatalf("share %d: %v", i, err)
		}
		if rv.ShareIndex != i || rv.FeePortion != testFee/3 {
			t.Fatalf("share %d: index %d, portion %d", i, rv.ShareIndex, rv.FeePortion)
		}
	}
}

// The share is the offer's: a registration whose share for the offer's key has
// another amount, refund or reward is refused, and so is one without the key,
// and a fee above the cap.
func TestOfferRefusesWhatIsNotTheOffers(t *testing.T) {
	owners, tx := build(t)
	base := offerOf(owners[1], tx)
	tests := []struct {
		name   string
		mutate func(*Offer)
		err    error
	}{
		{"amount", func(o *Offer) { o.Amount++ }, ErrOfferMismatch},
		{"refund", func(o *Offer) { o.RefundScript = scriptFor(testKey(0x77)) }, ErrOfferMismatch},
		{"reward", func(o *Offer) { o.RewardScript = scriptFor(testKey(0x77)) }, ErrOfferMismatch},
		{"owner key", func(o *Offer) { o.OwnerKeyID = idFor(testKey(0x78)) }, ErrOfferMismatch},
		{"fee cap", func(o *Offer) { o.MaxFee = testFee/3 - 1 }, ErrOfferMismatch},
		{"fee cap above the limit", func(o *Offer) { o.MaxFee = MaxFeePortion + 1 }, ErrOfferMismatch},
		{"negative portion", func(o *Offer) { o.Inputs[0].Value -= 2e8 }, ErrOfferMismatch},
		// A change script the registration does not pay to leaves the change
		// uncounted, so the offer would pay it all as fee.
		{"change script", func(o *Offer) { o.ChangeScript = scriptFor(testKey(0x79)) }, ErrOfferMismatch},
		{"state", func(o *Offer) { o.State = OfferSigned }, ErrOfferMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			off := *base
			off.Inputs = append([]FundingInput(nil), base.Inputs...)
			tt.mutate(&off)
			if _, err := off.Review(tx, RegtestRules); !errors.Is(err, tt.err) {
				t.Fatalf("got %v", err)
			}
		})
	}
	// The cap is inclusive.
	off := *base
	off.MaxFee = testFee / 3
	if _, err := off.Review(tx, RegtestRules); err != nil {
		t.Fatalf("a fee at the cap: %v", err)
	}
}

// An offer that has consented takes only the registration it consented to.
func TestOfferConsentedKeepsItsRegistration(t *testing.T) {
	owners, tx := build(t)
	off := offerOf(owners[0], tx)
	rv, err := off.Review(tx, RegtestRules)
	if err != nil {
		t.Fatal(err)
	}
	off.State, off.ConsentHash = OfferConsented, rv.ConsentHash
	if _, err := off.Review(tx, RegtestRules); err != nil {
		t.Fatal(err)
	}
	off.ConsentHash[0] ^= 1
	if _, err := off.Review(tx, RegtestRules); !errors.Is(err, ErrOfferMismatch) {
		t.Fatalf("%v", err)
	}
}

// Every byte the user is shown changes the review id. Each case rebuilds the
// registration or the offer with one thing different.
func TestReviewIDBinds(t *testing.T) {
	owners, tx := build(t)
	base := offerOf(owners[1], tx)
	baseRV, err := base.Review(tx, RegtestRules)
	if err != nil {
		t.Fatal(err)
	}
	// Changes to the registration are made through the proposal, so the
	// registration stays one the review accepts.
	reg := func(mutate func(*Proposal)) [32]byte {
		t.Helper()
		_, p := fixture(t)
		mutate(p)
		tx2, err := p.Build(RegtestRules)
		if err != nil {
			t.Fatal(err)
		}
		off := offerOf(owners[1], tx2)
		rv, err := off.Review(tx2, RegtestRules)
		if err != nil {
			t.Fatal(err)
		}
		return rv.ID
	}
	seen := map[[32]byte]string{baseRV.ID: "base"}
	check := func(name string, id [32]byte) {
		t.Helper()
		if prev, dup := seen[id]; dup {
			t.Fatalf("%s gives the id of %s", name, prev)
		}
		seen[id] = name
	}
	// The operator key is in the payload like the voting key; the fixture has
	// only one valid BLS key, so it is not varied here.
	check("voting key", reg(func(p *Proposal) { p.Terms.KeyIDVoting[0] ^= 1 }))
	check("operator reward", reg(func(p *Proposal) { p.Terms.OperatorReward = 100 }))
	check("early period", reg(func(p *Proposal) { p.Terms.EarlyPeriodBlocks++ }))
	check("early penalty", reg(func(p *Proposal) { p.Terms.EarlyPenalty++ }))
	check("other share's refund", reg(func(p *Proposal) { p.Terms.Shares[0].RefundScript = scriptFor(testKey(0x7a)) }))
	check("other share's reward", reg(func(p *Proposal) { p.Terms.Shares[2].RewardScript = scriptFor(testKey(0x7b)) }))
	check("other share's change", reg(func(p *Proposal) { p.Funding[0].ChangeScript = scriptFor(testKey(0x7c)) }))
	check("other share's input", reg(func(p *Proposal) { p.Funding[2].Inputs[0].OutPoint.Index = 9 }))
	check("fee", reg(func(p *Proposal) { p.Fee = 3003 }))

	// Offer fields.
	for name, mutate := range map[string]func(*Offer){
		"offer id":     func(o *Offer) { o.ID[5] ^= 1 },
		"offer maxfee": func(o *Offer) { o.MaxFee++ },
	} {
		off := *base
		mutate(&off)
		rv, err := off.Review(tx, RegtestRules)
		if err != nil {
			t.Fatal(err)
		}
		check(name, rv.ID)
	}
	// The own input's value is part of the offer, and so of the id.
	off := *base
	off.Inputs = append([]FundingInput(nil), base.Inputs...)
	id, err := off.ReviewID(tx, baseRV.ShareIndex, baseRV.FeePortion)
	if err != nil || id != baseRV.ID {
		t.Fatalf("recomputing the id: %v", err)
	}
	off.Inputs[0].Value++
	if id, _ := off.ReviewID(tx, baseRV.ShareIndex, baseRV.FeePortion); id == baseRV.ID {
		t.Fatal("the own input's value is not in the id")
	}
	if id, _ := base.ReviewID(tx, baseRV.ShareIndex, baseRV.FeePortion+1); id == baseRV.ID {
		t.Fatal("the fee portion is not in the id")
	}
	if id, _ := base.ReviewID(tx, 0, baseRV.FeePortion); id == baseRV.ID {
		t.Fatal("the share index is not in the id")
	}
}

// The id of a registration is the same after the other owners add their
// signatures, which is what lets a user approve a review and sign the next
// round.
func TestReviewIDStableAcrossRounds(t *testing.T) {
	owners, tx := build(t)
	off := offerOf(owners[1], tx)
	first, err := off.Review(tx, RegtestRules)
	if err != nil {
		t.Fatal(err)
	}
	var sigs []ShareSig
	for _, o := range owners {
		rv, err := Review(tx, RegtestRules, o.own)
		if err != nil {
			t.Fatal(err)
		}
		s, err := rv.SignConsent(o.ownerKey)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	rv, err := Review(tx, RegtestRules, owners[0].own)
	if err != nil {
		t.Fatal(err)
	}
	if err := rv.AddJoinSigs(sigs); err != nil {
		t.Fatal(err)
	}
	if err := rv.SignInputs(secrets{owners[0].fundKey}); err != nil {
		t.Fatal(err)
	}
	blob, err := rv.Blob()
	if err != nil {
		t.Fatal(err)
	}
	_ = blob
	raw, _ := rv.Raw()
	signed := &bisonwire.Tx{Chain: "dash"}
	if err := signed.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	second, err := off.Review(signed, RegtestRules)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatal("the id changed when signatures were added")
	}
}

// The digest a broadcast is checked against ignores input scripts and binds
// the join signatures and outputs.
func TestSignedDigest(t *testing.T) {
	_, tx := build(t)
	d0, err := SignedDigest(tx)
	if err != nil {
		t.Fatal(err)
	}
	cp, _ := cloneTx(tx)
	cp.TxIn[0].SignatureScript = []byte{1, 2, 3}
	if d, _ := SignedDigest(cp); d != d0 {
		t.Fatal("input scripts are in the digest")
	}
	cp, _ = cloneTx(tx)
	cp.TxOut[0].Value++
	if d, _ := SignedDigest(cp); d == d0 {
		t.Fatal("outputs are not in the digest")
	}
	cp, _ = cloneTx(tx)
	var pr protx.ProRegTx
	if err := protx.Parse(cp.ExtraPayload, &pr); err != nil {
		t.Fatal(err)
	}
	pr.Shares[0].JoinSig[3] = 9
	if err := setPayload(cp, &pr); err != nil {
		t.Fatal(err)
	}
	if d, _ := SignedDigest(cp); d == d0 {
		t.Fatal("join signatures are not in the digest")
	}
}

func newOfferStore(t *testing.T) *Store {
	t.Helper()
	db, err := walletdb.Create("bdb", filepath.Join(t.TempDir(), "mn.db"), true, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOfferStore(t *testing.T) {
	owners, tx := build(t)
	s := newOfferStore(t)
	off := offerOf(owners[0], tx)
	off.State = OfferBroadcast
	off.CreatedAt, off.ExpiresAt, off.OwnerIndex = 100, 200, 7
	off.ConsentHash, off.SignedDigest, off.TxID = chainhash.Hash{1}, chainhash.Hash{2}, chainhash.Hash{3}
	if _, err := s.GetOffer(off.ID); !errors.Is(err, ErrUnknownOffer) {
		t.Fatalf("%v", err)
	}
	if err := s.PutOffer(off); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetOffer(off.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, off) {
		t.Fatalf("round trip:\n%+v\n%+v", got, off)
	}
	other := offerOf(owners[1], tx)
	if err := s.PutOffer(other); err != nil {
		t.Fatal(err)
	}
	all, err := s.Offers()
	if err != nil || len(all) != 2 {
		t.Fatalf("%d offers, %v", len(all), err)
	}
	if err := s.DeleteOffer(off.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOffer(off.ID); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
	if all, _ := s.Offers(); len(all) != 1 || all[0].ID != other.ID {
		t.Fatalf("%+v", all)
	}
}

// A damaged record is an error, and a store made before offers existed gets the
// bucket.
func TestOfferStoreDamagedAndMissingBucket(t *testing.T) {
	s := newOfferStore(t)
	err := walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		ob := tx.ReadWriteBucket(storeBucket).NestedReadWriteBucket(offersBucket)
		if err := ob.Put(make([]byte, 32), []byte{9, '{', '}'}); err != nil {
			return err
		}
		return ob.Put(append([]byte{1}, make([]byte, 31)...), []byte{1, '{', '}'})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Offers(); err == nil {
		t.Fatal("damaged records were read")
	}
	if _, err := s.GetOffer([32]byte{}); err == nil {
		t.Fatal("an unknown version was read")
	}
	// Remove the bucket: a store from before offers.
	err = walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		return tx.ReadWriteBucket(storeBucket).DeleteNestedBucket(offersBucket)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Offers(); err == nil {
		t.Fatal("a missing bucket was read")
	}
	s2, err := NewStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if all, err := s2.Offers(); err != nil || len(all) != 0 {
		t.Fatalf("%v %v", all, err)
	}
}

// The fee portion is measured from the transaction, so one that does not divide
// evenly gives the first share the remainder and the others their equal part.
func TestOfferReviewUnevenFee(t *testing.T) {
	for _, fee := range []int64{3001, 3002} {
		owners, p := fixture(t)
		p.Fee = fee
		tx, err := p.Build(RegtestRules)
		if err != nil {
			t.Fatal(err)
		}
		for i, o := range owners {
			off := offerOf(o, tx)
			rv, err := off.Review(tx, RegtestRules)
			if err != nil {
				t.Fatalf("fee %d share %d: %v", fee, i, err)
			}
			if want := FeePortion(fee, i, 3); rv.FeePortion != want {
				t.Fatalf("fee %d share %d pays %d, want %d", fee, i, rv.FeePortion, want)
			}
		}
	}
}

// Outputs to the change script are summed, and an offer that lists an input
// twice is refused, as is an output that is negative.
func TestOfferChangeAndInputEdges(t *testing.T) {
	owners, tx := build(t)
	off := offerOf(owners[1], tx)
	want, err := off.Review(tx, RegtestRules)
	if err != nil {
		t.Fatal(err)
	}

	// Split the change in two outputs to the same script.
	cp, _ := cloneTx(tx)
	for i, out := range cp.TxOut {
		if string(out.PkScript) == string(off.ChangeScript) {
			half := out.Value / 2
			out.Value -= half
			cp.TxOut = append(cp.TxOut[:i+1], append([]*wire.TxOut{wire.NewTxOut(half, off.ChangeScript)}, cp.TxOut[i+1:]...)...)
			break
		}
	}
	if len(cp.TxOut) != len(tx.TxOut)+1 {
		t.Fatal("the change output was not found")
	}
	// The collateral index moved with the insert, so rebuild the payload.
	var pr protx.ProRegTx
	if err := protx.Parse(cp.ExtraPayload, &pr); err != nil {
		t.Fatal(err)
	}
	pr.Collateral.Index++
	if err := setPayload(cp, &pr); err != nil {
		t.Fatal(err)
	}
	rv, err := off.Review(cp, RegtestRules)
	if err != nil {
		t.Fatalf("two change outputs: %v", err)
	}
	if rv.FeePortion != want.FeePortion {
		t.Fatalf("portion %d with split change, %d without", rv.FeePortion, want.FeePortion)
	}

	dup := *off
	dup.Inputs = append(append([]FundingInput(nil), off.Inputs...), off.Inputs[0])
	if _, err := dup.Review(tx, RegtestRules); !errors.Is(err, ErrOfferMismatch) {
		t.Fatalf("an input listed twice: %v", err)
	}

	neg, _ := cloneTx(tx)
	neg.TxOut[0].Value = -1
	if _, err := off.Review(neg, RegtestRules); err == nil {
		t.Fatal("a negative output was accepted")
	}
}
