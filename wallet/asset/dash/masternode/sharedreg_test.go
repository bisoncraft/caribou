// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
)

// owner is one share owner of the test registration.
type owner struct {
	ownerKey *btcec.PrivateKey
	fundKey  *btcec.PrivateKey
	own      Own
	funding  Funding
}

func testKey(b byte) *btcec.PrivateKey {
	var k [32]byte
	for i := range k {
		k[i] = b
	}
	priv, _ := btcec.PrivKeyFromBytes(k[:])
	return priv
}

func scriptFor(key *btcec.PrivateKey) []byte {
	return append(append([]byte{0x76, 0xa9, 0x14}, btcutil.Hash160(key.PubKey().SerializeCompressed())...), 0x88, 0xac)
}

func idFor(key *btcec.PrivateKey) (id [20]byte) {
	copy(id[:], btcutil.Hash160(key.PubKey().SerializeCompressed()))
	return
}

// secrets signs for an owner's funding key, as that owner's wallet would.
type secrets struct{ key *btcec.PrivateKey }

func (s secrets) GetKey(btcutil.Address) (*btcec.PrivateKey, bool, error) { return s.key, true, nil }
func (s secrets) GetScript(btcutil.Address) ([]byte, error)               { return nil, errors.New("none") }
func (s secrets) ChainParams() *chaincfg.Params {
	p := chaincfg.RegressionNetParams
	p.PubKeyHashAddrID = 140
	p.ScriptHashAddrID = 19
	return &p
}

const testFee = 3000

// fixture is a 500/300/200 registration among three owners, each with one
// funding input worth 1 DASH more than its share.
func fixture(t *testing.T) ([]owner, *Proposal) {
	t.Helper()
	amounts := []int64{500e8, 300e8, 200e8}
	var owners []owner
	p := &Proposal{Fee: testFee}
	for i, amt := range amounts {
		b := byte(i + 1)
		o := owner{ownerKey: testKey(0x10 + b), fundKey: testKey(0x20 + b)}
		refund := scriptFor(testKey(0x30 + b))
		o.own = Own{
			ShareIndex: i, Fee: testFee,
			ChangeScript: scriptFor(testKey(0x40 + b)),
			Inputs: []FundingInput{{
				OutPoint: wire.OutPoint{Hash: chainhash.Hash{b}, Index: uint32(i)},
				PkScript: scriptFor(o.fundKey), Value: amt + 1e8,
			}},
		}
		o.funding = Funding{Inputs: o.own.Inputs, ChangeScript: o.own.ChangeScript}
		owners = append(owners, o)
		p.Terms.Shares = append(p.Terms.Shares, protx.Share{
			Amount: amt, RefundScript: refund, KeyIDOwner: idFor(o.ownerKey),
		})
		p.Funding = append(p.Funding, o.funding)
	}
	op, _ := hex.DecodeString(operatorKey)
	copy(p.Terms.PubKeyOperator[:], op)
	p.Terms.NetInfo = protx.NetInfo{Version: 1}
	p.Terms.KeyIDVoting = keyID(0x33)
	p.Terms.EarlyPeriodBlocks = testEarlyBlocks
	p.Terms.EarlyPenalty = DefaultEarlyPenalty(amounts)
	for i := range owners {
		owners[i].own.Terms = p.Terms
	}
	return owners, p
}

// testEarlyBlocks is a short early period.
const testEarlyBlocks = 10

func build(t *testing.T) ([]owner, *bisonwire.Tx) {
	t.Helper()
	owners, p := fixture(t)
	tx, err := p.Build(RegtestRules)
	if err != nil {
		t.Fatal(err)
	}
	return owners, tx
}

func serialize(t *testing.T, tx *bisonwire.Tx) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := tx.Serialize(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestBuildMatchesCore rebuilds the registration Dash Core's
// shared_register_prepare made for two shares and requires the same bytes, so
// the layout (inputs in order, then change, the collateral last, zeroed join
// signatures, the inputs hash and the collateral index) is Core's, and the
// consent digest of what we built is the one Core reported.
func TestBuildMatchesCore(t *testing.T) {
	raw, err := os.ReadFile("testdata/regprepare.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Tx, ConsentHash string }
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	txBytes, _ := hex.DecodeString(v.Tx)
	core := &bisonwire.Tx{Chain: "dash"}
	if err := core.Deserialize(bytes.NewReader(txBytes)); err != nil {
		t.Fatal(err)
	}
	var pr protx.ProRegTx
	if err := protx.Parse(core.ExtraPayload, &pr); err != nil {
		t.Fatal(err)
	}

	// Core's funding transaction has one input and one change output per
	// share, which is the shape the builder makes.
	p := &Proposal{Terms: Terms{
		Shares: pr.Shares, NetInfo: pr.NetInfo, PubKeyOperator: pr.PubKeyOperator,
		KeyIDVoting: pr.KeyIDVoting, OperatorReward: pr.OperatorReward,
		EarlyPeriodBlocks: pr.EarlyPeriodBlocks, EarlyPenalty: pr.EarlyPenalty,
	}}
	for i := range pr.Shares {
		p.Funding = append(p.Funding, Funding{
			Inputs: []FundingInput{{
				OutPoint: core.TxIn[i].PreviousOutPoint,
				PkScript: core.TxOut[i].PkScript, // any P2PKH: the builder only reads the value
				Value:    pr.Shares[i].Amount + core.TxOut[i].Value,
			}},
			ChangeScript: core.TxOut[i].PkScript,
		})
	}
	got, err := p.Build(RegtestRules)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(serialize(t, got), txBytes) {
		t.Fatalf("built\n%x\nCore\n%x", serialize(t, got), txBytes)
	}

	var gotPR protx.ProRegTx
	if err := protx.Parse(got.ExtraPayload, &gotPR); err != nil {
		t.Fatal(err)
	}
	h, err := gotPR.SharedRegConsentHash(got)
	if err != nil {
		t.Fatal(err)
	}
	if h.String() != v.ConsentHash {
		t.Fatalf("consent hash %s, Core says %s", h, v.ConsentHash)
	}
}

// TestRegistrationEndToEnd runs the whole exchange for three owners: each
// reviews and consents, the join signatures pass between them, and then each
// signs its own inputs. The result must carry a valid join signature for every
// share and a script for every input.
func TestRegistrationEndToEnd(t *testing.T) {
	owners, tx := build(t)

	var reviews []*RegReview
	var sigs []ShareSig
	for _, o := range owners {
		rv, err := Review(tx, RegtestRules, o.own)
		if err != nil {
			t.Fatalf("share %d: %v", o.own.ShareIndex, err)
		}
		if rv.FeePortion != testFee/3 {
			t.Fatalf("share %d fee portion %d", o.own.ShareIndex, rv.FeePortion)
		}
		if len(rv.OtherOutputs) != 2 {
			t.Fatalf("the other owners' two change outputs must be shown, got %d", len(rv.OtherOutputs))
		}
		s, err := rv.SignConsent(o.ownerKey)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, rv)
		sigs = append(sigs, s)
	}

	// Inputs cannot be signed before every owner has consented.
	if err := reviews[0].SignInputs(secrets{owners[0].fundKey}); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("signed inputs before the join signatures were in: %v", err)
	}

	// The blob goes round the owners, each adding the join signatures and
	// then signing its own inputs; the last has the finished transaction.
	cur := tx
	for i, o := range owners {
		rv, err := Review(cur, RegtestRules, o.own)
		if err != nil {
			t.Fatalf("share %d second review: %v", i, err)
		}
		if err := rv.AddJoinSigs(sigs); err != nil {
			t.Fatal(err)
		}
		if err := rv.SignInputs(secrets{o.fundKey}); err != nil {
			t.Fatal(err)
		}
		blob, err := rv.Blob()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(blob)
		if cur, _, err = ParseBlob(data); err != nil {
			t.Fatal(err)
		}
	}

	for i, in := range cur.TxIn {
		if len(in.SignatureScript) == 0 {
			t.Fatalf("input %d not signed", i)
		}
	}
	var pr protx.ProRegTx
	if err := protx.Parse(cur.ExtraPayload, &pr); err != nil {
		t.Fatal(err)
	}
	hash, err := pr.SharedRegConsentHash(cur)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range pr.Shares {
		if err := protx.VerifyCompact(hash, s.KeyIDOwner, s.JoinSig[:]); err != nil {
			t.Fatalf("join signature %d: %v", i, err)
		}
	}
}

// Review is the guard on consent: each case changes something the consent
// digest does not bind, or something that costs the reviewing owner, and Review
// must refuse. Owner 1 reviews.
func TestReviewRejects(t *testing.T) {
	setPR := func(t *testing.T, tx *bisonwire.Tx, f func(pr *protx.ProRegTx)) {
		t.Helper()
		var pr protx.ProRegTx
		if err := protx.Parse(tx.ExtraPayload, &pr); err != nil {
			t.Fatal(err)
		}
		f(&pr)
		if err := setPayload(tx, &pr); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name   string
		mutate func(t *testing.T, tx *bisonwire.Tx, own *Own)
		reason string
	}{
		{"collateral index moved to a change output", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.Collateral.Index = 0 })
		}, "is not the collateral"},
		{"collateral index out of range", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.Collateral.Index = 99 })
		}, "collateral index"},
		{"collateral value short", func(t *testing.T, tx *bisonwire.Tx, own *Own) { tx.TxOut[3].Value-- }, "is not the collateral"},
		{"collateral script is a key's", func(t *testing.T, tx *bisonwire.Tx, own *Own) { tx.TxOut[3].PkScript = scriptFor(testKey(9)) }, "is not the collateral"},
		{"second collateral output", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			tx.TxOut[0].PkScript = SharedCollateralScript
		}, "second collateral"},
		{"owner key id is another's", func(t *testing.T, tx *bisonwire.Tx, own *Own) { own.Terms.Shares[1].KeyIDOwner = keyID(1) }, "not the ones"},
		{"share amount differs from the owner's", func(t *testing.T, tx *bisonwire.Tx, own *Own) { own.Terms.Shares[1].Amount-- }, "not the ones"},
		{"refund script differs", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			own.Terms.Shares[1].RefundScript = scriptFor(testKey(7))
		}, "not the ones"},
		{"reward script differs", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			own.Terms.Shares[1].RewardScript = scriptFor(testKey(7))
		}, "not the ones"},
		{"share index out of range", func(t *testing.T, tx *bisonwire.Tx, own *Own) { own.ShareIndex = 3 }, "share index"},
		{"input sequence is a relative lock", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			tx.TxIn[0].Sequence = 1
			setPR(t, tx, func(pr *protx.ProRegTx) {})
		}, "sequence"},
		{"an input spent twice", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			tx.TxIn[2].PreviousOutPoint = tx.TxIn[0].PreviousOutPoint
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.InputsHash = protx.InputsHash(tx.TxIn) })
		}, "repeats an outpoint"},
		{"an empty script is not the owner's change", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			own.ChangeScript = nil
			tx.TxOut[1].PkScript = []byte{}
		}, "want exactly"},
		{"operator reward raised", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.OperatorReward = 10000 })
		}, "not the ones"},
		{"early period lengthened", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.EarlyPeriodBlocks = MaxEarlyPeriodBlocks })
		}, "not the ones"},
		{"penalty raised", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.EarlyPenalty = pr.Shares[2].Amount - 1 })
		}, "not the ones"},
		{"operator key swapped", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			// The BLS12-381 G1 generator: a valid key, but not the agreed one.
			g, _ := hex.DecodeString("97f1d3a73197d7942695638c4fa9ac0fc3688c4f9774b905a14e3a3f171bac586c55e83ff97a1aeffb3af00adb22c6bb")
			setPR(t, tx, func(pr *protx.ProRegTx) { copy(pr.PubKeyOperator[:], g) })
		}, "not the ones"},
		{"voting key swapped", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.KeyIDVoting = keyID(0x44) })
		}, "not the ones"},
		{"another share's amount moved", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.Shares[0].Amount -= 100e8; pr.Shares[2].Amount += 100e8 })
		}, "not the ones"},
		{"fee shifted onto the owner", func(t *testing.T, tx *bisonwire.Tx, own *Own) { tx.TxOut[1].Value -= 900_000 }, "want exactly"},
		{"fee total disagrees", func(t *testing.T, tx *bisonwire.Tx, own *Own) { own.Fee = 6000 }, "want exactly"},
		{"fee out of range", func(t *testing.T, tx *bisonwire.Tx, own *Own) { own.Fee = -1 }, "fee"},
		{"remainder pushes the first share over the cap", func(t *testing.T, tx *bisonwire.Tx, own *Own) { own.Fee = 3*MaxFeePortion - 1 }, "fee"},
		{"own input listed twice", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			own.Inputs = append(own.Inputs, own.Inputs[0])
		}, "listed twice"},
		{"output value wraps the sum", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			tx.TxOut[0].Value = 1<<63 - 1
		}, "value"},
		{"owner's input already signed", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			tx.TxIn[1].SignatureScript = []byte{1}
		}, "already has a signature script"},
		{"lock time set", func(t *testing.T, tx *bisonwire.Tx, own *Own) { tx.LockTime = 5 }, "lock time"},
		{"owner's input missing", func(t *testing.T, tx *bisonwire.Tx, own *Own) { own.Inputs[0].OutPoint.Index = 77 }, "is missing"},
		{"change skimmed", func(t *testing.T, tx *bisonwire.Tx, own *Own) { tx.TxOut[1].Value -= 5_000_000 }, "want exactly"},
		{"change redirected", func(t *testing.T, tx *bisonwire.Tx, own *Own) { tx.TxOut[1].PkScript = scriptFor(testKey(8)) }, "want exactly"},
		{"owner would underpay its share", func(t *testing.T, tx *bisonwire.Tx, own *Own) { own.Inputs[0].Value -= 2e8 }, "want exactly"},
		{"inputs hash wrong", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.InputsHash[0] ^= 1 })
		}, "inputs hash"},
		{"terms Core would reject", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.EarlyPenalty = pr.Shares[2].Amount })
		}, "shares-penalty"},
		{"a forged join signature", func(t *testing.T, tx *bisonwire.Tx, own *Own) {
			setPR(t, tx, func(pr *protx.ProRegTx) { pr.Shares[0].JoinSig[0] = 31; pr.Shares[0].JoinSig[1] = 1 })
		}, "join signature"},
		{"not a registration", func(t *testing.T, tx *bisonwire.Tx, own *Own) { tx.Type = 10 }, "not a provider registration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owners, tx := build(t)
			own := owners[1].own
			own.Inputs = append([]FundingInput(nil), own.Inputs...)
			own.Terms.Shares = append([]protx.Share(nil), own.Terms.Shares...)
			tt.mutate(t, tx, &own)
			_, err := Review(tx, RegtestRules, own)
			if !errors.Is(err, ErrInvalidProposal) && !errors.Is(err, ErrInvalidTerms) {
				t.Fatalf("want a rejection, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("want %q, got %v", tt.reason, err)
			}
		})
	}
}

// Review must leave the transaction it is given as it was.
func TestReviewDoesNotModify(t *testing.T) {
	owners, tx := build(t)
	before := serialize(t, tx)
	rv, err := Review(tx, RegtestRules, owners[0].own)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := rv.SignConsent(owners[0].ownerKey)
	if err := rv.AddJoinSigs([]ShareSig{s}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, serialize(t, tx)) {
		t.Fatal("the reviewed transaction was modified")
	}
}

func TestSignConsentWrongKey(t *testing.T) {
	owners, tx := build(t)
	rv, err := Review(tx, RegtestRules, owners[0].own)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rv.SignConsent(owners[1].ownerKey); err == nil {
		t.Fatal("signed consent with another share's key")
	}
	if _, err := rv.SignConsent(owners[0].fundKey); err == nil {
		t.Fatal("signed consent with a funding key")
	}
}

// A bad signature must add nothing, even when others in the same call are good.
func TestAddJoinSigsRejects(t *testing.T) {
	owners, tx := build(t)
	rv, err := Review(tx, RegtestRules, owners[0].own)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Review(tx, RegtestRules, owners[1].own)
	if err != nil {
		t.Fatal(err)
	}
	good, _ := other.SignConsent(owners[1].ownerKey)
	wrongShare := good
	wrongShare.ShareIndex = 2 // share 1's signature offered as share 2's
	tests := []struct {
		name string
		sig  ShareSig
	}{
		{"signature of another share", wrongShare},
		{"index out of range", ShareSig{ShareIndex: 5, Signature: good.Signature}},
		{"negative index", ShareSig{ShareIndex: -1, Signature: good.Signature}},
		{"not base64", ShareSig{ShareIndex: 1, Signature: "!!"}},
		{"short", ShareSig{ShareIndex: 1, Signature: base64.StdEncoding.EncodeToString([]byte{1, 2})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, _ := rv.Raw()
			if err := rv.AddJoinSigs([]ShareSig{good, tt.sig}); err == nil {
				t.Fatal("accepted")
			}
			if after, _ := rv.Raw(); !bytes.Equal(before, after) {
				t.Fatal("a rejected call still changed the transaction")
			}
		})
	}
}

func TestBuildRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(p *Proposal)
		reason string
	}{
		{"terms Core would reject", func(p *Proposal) { p.Terms.Shares[0].Amount = 1 }, "shares-amount"},
		{"funding count", func(p *Proposal) { p.Funding = p.Funding[:2] }, "fundings"},
		{"negative fee", func(p *Proposal) { p.Fee = -1 }, "fee"},
		{"remainder pushes the first share over the cap", func(p *Proposal) { p.Fee = 3*MaxFeePortion - 1 }, "fee"},
		{"fee over the cap", func(p *Proposal) { p.Fee = 3*MaxFeePortion + 1 }, "fee"},
		{"no inputs", func(p *Proposal) { p.Funding[1].Inputs = nil }, "no inputs"},
		{"non P2PKH input", func(p *Proposal) {
			p.Funding[0].Inputs = []FundingInput{{OutPoint: p.Funding[0].Inputs[0].OutPoint, PkScript: p2sh(1), Value: 600e8}}
		}, "not P2PKH"},
		{"input spent twice", func(p *Proposal) { p.Funding[1].Inputs[0].OutPoint = p.Funding[0].Inputs[0].OutPoint }, "twice"},
		{"inputs short of the share", func(p *Proposal) { p.Funding[2].Inputs[0].Value = 200e8 }, "need"},
		{"dust change", func(p *Proposal) { p.Funding[2].Inputs[0].Value = 200e8 + 1000 + 100 }, "dust"},
		{"change without a script", func(p *Proposal) { p.Funding[2].ChangeScript = nil }, "P2PKH script"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, p := fixture(t)
			// Fixture inputs are shared by reference with Own: copy.
			for i := range p.Funding {
				p.Funding[i].Inputs = append([]FundingInput(nil), p.Funding[i].Inputs...)
			}
			tt.mutate(p)
			_, err := p.Build(RegtestRules)
			if err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("want %q, got %v", tt.reason, err)
			}
		})
	}
}

// An exact funding makes no change output.
func TestBuildNoChange(t *testing.T) {
	_, p := fixture(t)
	p.Funding[2].Inputs = []FundingInput{{OutPoint: p.Funding[2].Inputs[0].OutPoint, PkScript: p.Funding[2].Inputs[0].PkScript, Value: 200e8 + testFee/3}}
	tx, err := p.Build(RegtestRules)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.TxOut) != 3 {
		t.Fatalf("%d outputs, want two change and the collateral", len(tx.TxOut))
	}
}

// The review's Terms are for display: changing them must not change what is
// signed.
func TestReviewTermsAreACopy(t *testing.T) {
	owners, tx := build(t)
	rv, err := Review(tx, RegtestRules, owners[0].own)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := rv.Raw()
	rv.Terms.Shares[2].Amount = 1
	if err := rv.AddJoinSigs(nil); err != nil {
		t.Fatal(err)
	}
	if after, _ := rv.Raw(); !bytes.Equal(before, after) {
		t.Fatal("editing the review's terms changed the transaction")
	}
}

func TestFeePortion(t *testing.T) {
	for _, tt := range []struct {
		fee int64
		n   int
	}{{3000, 3}, {3001, 3}, {3002, 2}, {0, 5}, {1, 8}} {
		var sum int64
		for i := range tt.n {
			p := FeePortion(tt.fee, i, tt.n)
			if p < 0 {
				t.Fatalf("negative portion")
			}
			sum += p
		}
		if sum != tt.fee {
			t.Fatalf("portions of %d among %d sum to %d", tt.fee, tt.n, sum)
		}
	}
}

func TestParseBlobRejects(t *testing.T) {
	for _, in := range []string{``, `{`, `{"tx":"zz"}`, `{"tx":"00"}`} {
		if _, _, err := ParseBlob([]byte(in)); !errors.Is(err, ErrInvalidProposal) {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

// Bytes after the transaction would be dropped silently, so the blob must be
// refused.
func TestParseBlobTrailingBytes(t *testing.T) {
	_, tx := build(t)
	raw := append(serialize(t, tx), 0)
	data, _ := json.Marshal(Blob{Tx: hex.EncodeToString(raw)})
	if _, _, err := ParseBlob(data); !errors.Is(err, ErrInvalidProposal) || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("want trailing bytes rejected, got %v", err)
	}
}

// The largest fee Build accepts, an even split at the cap, must also pass
// Review, so tightening either bound cannot drift them apart.
func TestReviewAcceptsMaxFee(t *testing.T) {
	owners, p := fixture(t)
	p.Fee = 3 * MaxFeePortion
	tx, err := p.Build(RegtestRules)
	if err != nil {
		t.Fatal(err)
	}
	own := owners[0].own
	own.Fee = p.Fee
	rv, err := Review(tx, RegtestRules, own)
	if err != nil {
		t.Fatal(err)
	}
	if rv.FeePortion != MaxFeePortion {
		t.Fatalf("fee portion %d, want %d", rv.FeePortion, MaxFeePortion)
	}
	own.Fee = p.Fee + 1
	if _, err := Review(tx, RegtestRules, own); err == nil {
		t.Fatal("a fee one duff over the cap was accepted")
	}
}
