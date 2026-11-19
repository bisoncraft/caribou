// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package dash

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/dash/masternode"
	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wtxmgr"
)

// fakeFunding is the wallet offers draw on: it lists coins it holds the keys
// of, records leases and what is published, and makes addresses from a counter.
type fakeFunding struct {
	mu        sync.Mutex
	locked    bool
	unspent   []*btcjson.ListUnspentResult
	keys      map[[20]byte]*btcec.PrivateKey
	leases    map[wire.OutPoint]time.Duration
	lockIDs   map[wire.OutPoint]wtxmgr.LockID
	expiry    map[wire.OutPoint]time.Time
	clock     func() time.Time
	released  []wire.OutPoint
	leaseCall int
	// failLeaseAt makes the n'th (1 based) LeaseOutput fail, if not 0.
	failLeaseAt int
	published   []*bisonwire.Tx
	addrN       byte
}

func newFakeFunding() *fakeFunding {
	return &fakeFunding{
		keys: map[[20]byte]*btcec.PrivateKey{}, leases: map[wire.OutPoint]time.Duration{},
		lockIDs: map[wire.OutPoint]wtxmgr.LockID{}, expiry: map[wire.OutPoint]time.Time{}, clock: time.Now,
	}
}

func fakeKey(b byte) *btcec.PrivateKey {
	var k [32]byte
	for i := range k {
		k[i] = b
	}
	priv, _ := btcec.PrivKeyFromBytes(k[:])
	return priv
}

func (f *fakeFunding) addCoin(t *testing.T, seed byte, index uint32, duffs int64) wire.OutPoint {
	t.Helper()
	key := fakeKey(seed)
	id := btcutil.Hash160(key.PubKey().SerializeCompressed())
	f.keys[[20]byte(id)] = key
	script := append(append([]byte{0x76, 0xa9, 0x14}, id...), 0x88, 0xac)
	op := wire.OutPoint{Hash: chainhash.Hash{seed, 0xcc}, Index: index}
	f.unspent = append(f.unspent, &btcjson.ListUnspentResult{
		TxID: op.Hash.String(), Vout: index, ScriptPubKey: hex.EncodeToString(script),
		Amount: btcutil.Amount(duffs).ToBTC(), Confirmations: 6,
	})
	return op
}

func (f *fakeFunding) DeriveFromRootPath(p []uint32) (*btcec.PrivateKey, error) {
	return seqKeys{}.DeriveFromRootPath(p)
}
func (f *fakeFunding) ListUnspent(int32, int32, string) ([]*btcjson.ListUnspentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*btcjson.ListUnspentResult
	for _, u := range f.unspent {
		h, _ := chainhash.NewHashFromStr(u.TxID)
		if exp, leased := f.expiry[wire.OutPoint{Hash: *h, Index: u.Vout}]; !leased || !f.clock().Before(exp) {
			out = append(out, u)
		}
	}
	return out, nil
}
func (f *fakeFunding) LeaseOutput(id wtxmgr.LockID, op wire.OutPoint, d time.Duration) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaseCall++
	if f.failLeaseAt != 0 && f.leaseCall == f.failLeaseAt {
		return time.Time{}, errors.New("lease failed")
	}
	if exp, held := f.expiry[op]; held && f.clock().Before(exp) && f.lockIDs[op] != id {
		return time.Time{}, wtxmgr.ErrOutputAlreadyLocked
	}
	f.leases[op], f.lockIDs[op] = d, id
	f.expiry[op] = f.clock().Add(d)
	return f.expiry[op], nil
}

// ReleaseOutput is the wallet's: a lease that has lapsed or is gone releases
// to nothing, and one held by another id may not be released.
func (f *fakeFunding) ReleaseOutput(id wtxmgr.LockID, op wire.OutPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, op)
	exp, held := f.expiry[op]
	if held && f.clock().Before(exp) && f.lockIDs[op] != id {
		return wtxmgr.ErrOutputUnlockNotAllowed
	}
	delete(f.leases, op)
	delete(f.lockIDs, op)
	delete(f.expiry, op)
	return nil
}
func (f *fakeFunding) newAddr(seed byte) (btcutil.Address, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrN++
	key := fakeKey(seed + f.addrN)
	id := btcutil.Hash160(key.PubKey().SerializeCompressed())
	f.keys[[20]byte(id)] = key
	return btcutil.NewAddressPubKeyHash(id, RegressionNetParams)
}
func (f *fakeFunding) NewAddress(uint32, waddrmgr.KeyScope) (btcutil.Address, error) {
	return f.newAddr(0x60)
}
func (f *fakeFunding) NewChangeAddress(uint32, waddrmgr.KeyScope) (btcutil.Address, error) {
	return f.newAddr(0x80)
}
func (f *fakeFunding) PrimaryScope() waddrmgr.KeyScope { return waddrmgr.KeyScope{} }
func (f *fakeFunding) PrivKeyForAddress(a btcutil.Address) (*btcec.PrivateKey, error) {
	k, ok := f.keys[[20]byte(a.ScriptAddress())]
	if !ok {
		return nil, errors.New("not ours")
	}
	return k, nil
}
func (f *fakeFunding) PublishBisonTx(tx *bisonwire.Tx, _ string) error {
	f.published = append(f.published, tx)
	return nil
}
func (f *fakeFunding) Locked() bool { return f.locked }

func p2pkh(key *btcec.PrivateKey) []byte {
	return append(append([]byte{0x76, 0xa9, 0x14}, btcutil.Hash160(key.PubKey().SerializeCompressed())...), 0x88, 0xac)
}

// offerRig is a wallet with an offer, and the two other owners of a
// registration that includes it.
type offerRig struct {
	dw    *dashWallet
	fw    *fakeFunding
	chain *infoChain
	now   time.Time
	// other owners
	ownerKeys [2]*btcec.PrivateKey
	fundKeys  [2]*btcec.PrivateKey
	own       [2]masternode.Own
}

const rigFee = 3000

func newOfferRig(t *testing.T) *offerRig {
	t.Helper()
	r := &offerRig{fw: newFakeFunding(), chain: newInfoChain(t, "protx_info_shared.json"), now: time.Unix(1_800_000_000, 0)}
	r.dw = openDashSharedWallet(&asset.WalletConfig{DataDir: t.TempDir()}, RegressionNetParams, util.Disabled)
	mgr, _ := stakedManager(t, r.chain, seqKeys{})
	r.dw.mn.Store(mgr)
	r.dw.fw = r.fw
	r.dw.now = func() time.Time { return r.now }
	r.fw.clock = r.dw.now
	return r
}

// build makes the registration of the offer and two other owners, 300 and 200
// DASH, with the given fee.
func (r *offerRig) build(t *testing.T, o *asset.MasternodeOffer, fee int64) *bisonwire.Tx {
	t.Helper()
	mgr, _ := r.dw.manager()
	stored, err := mgr.Store().GetOffer(mustID(t, o.ID))
	if err != nil {
		t.Fatal(err)
	}
	p := &masternode.Proposal{Fee: fee}
	p.Terms.Shares = []protx.Share{{
		Amount: stored.Amount, KeyIDOwner: stored.OwnerKeyID,
		RefundScript: stored.RefundScript, RewardScript: stored.RewardScript,
	}}
	p.Funding = []masternode.Funding{{Inputs: stored.Inputs, ChangeScript: stored.ChangeScript}}
	amounts := []int64{stored.Amount}
	for i, amt := range []int64{300e8, 200e8} {
		b := byte(0x90 + i)
		r.ownerKeys[i], r.fundKeys[i] = fakeKey(b), fakeKey(b+8)
		id := btcutil.Hash160(r.ownerKeys[i].PubKey().SerializeCompressed())
		fi := masternode.FundingInput{
			OutPoint: wire.OutPoint{Hash: chainhash.Hash{b}, Index: uint32(i)},
			PkScript: p2pkh(r.fundKeys[i]), Value: amt + 1e8,
		}
		change := p2pkh(fakeKey(b + 16))
		p.Terms.Shares = append(p.Terms.Shares, protx.Share{
			Amount: amt, KeyIDOwner: [20]byte(id),
			RefundScript: p2pkh(fakeKey(b + 24)), RewardScript: p2pkh(fakeKey(b + 32)),
		})
		p.Funding = append(p.Funding, masternode.Funding{Inputs: []masternode.FundingInput{fi}, ChangeScript: change})
		r.own[i] = masternode.Own{Inputs: []masternode.FundingInput{fi}, ChangeScript: change, ShareIndex: i + 1, Fee: fee}
		amounts = append(amounts, amt)
	}
	op, _ := hex.DecodeString(r.chain.info.State.PubKeyOperator)
	copy(p.Terms.PubKeyOperator[:], op)
	p.Terms.NetInfo = protx.NetInfo{Version: 1}
	p.Terms.KeyIDVoting = [20]byte{0x33}
	p.Terms.EarlyPeriodBlocks = 10
	p.Terms.EarlyPenalty = masternode.DefaultEarlyPenalty(amounts)
	for i := range r.own {
		r.own[i].Terms = p.Terms
	}
	tx, err := p.Build(masternode.RegtestRules)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func mustID(t *testing.T, s string) [32]byte {
	t.Helper()
	id, err := parseOfferID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func blobOf(t *testing.T, tx *bisonwire.Tx, sigs ...masternode.ShareSig) string {
	t.Helper()
	var raw []byte
	{
		var b = new(bytes.Buffer)
		if err := tx.Serialize(b); err != nil {
			t.Fatal(err)
		}
		raw = b.Bytes()
	}
	data, err := json.Marshal(masternode.Blob{Tx: hex.EncodeToString(raw), Signatures: sigs})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// othersConsent is the other owners' consent signatures to tx.
func (r *offerRig) othersConsent(t *testing.T, tx *bisonwire.Tx) []masternode.ShareSig {
	t.Helper()
	var sigs []masternode.ShareSig
	for i := range r.own {
		rv, err := masternode.Review(tx, masternode.RegtestRules, r.own[i])
		if err != nil {
			t.Fatal(err)
		}
		s, err := rv.SignConsent(r.ownerKeys[i])
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	return sigs
}

func (r *offerRig) fund(t *testing.T) {
	t.Helper()
	r.fw.addCoin(t, 0x11, 0, 400e8)
	r.fw.addCoin(t, 0x12, 0, 150e8)
}

func TestCreateMasternodeOffer(t *testing.T) {
	ctx := t.Context()
	r := newOfferRig(t)
	if _, err := r.dw.CreateMasternodeOffer(ctx, 500e8); err == nil {
		t.Fatal("an offer without funds")
	}
	r.fund(t)

	r.fw.locked = true
	if _, err := r.dw.CreateMasternodeOffer(ctx, 500e8); err == nil {
		t.Fatal("an offer from a locked wallet")
	}
	r.fw.locked = false
	if len(r.fw.leases) != 0 {
		t.Fatal("a refused offer left leases")
	}

	// A lease that fails releases the ones before it.
	r.fw.failLeaseAt = 2
	if _, err := r.dw.CreateMasternodeOffer(ctx, 500e8); err == nil {
		t.Fatal("a failed lease did not fail the offer")
	}
	if len(r.fw.leases) != 0 {
		t.Fatalf("a failed offer left %d leases", len(r.fw.leases))
	}
	if mgr, _ := r.dw.manager(); mgr != nil {
		if offers, _ := mgr.Store().Offers(); len(offers) != 0 {
			t.Fatal("a failed offer was stored")
		}
	}
	r.fw.failLeaseAt = 0

	o, err := r.dw.CreateMasternodeOffer(ctx, 500e8)
	if err != nil {
		t.Fatal(err)
	}
	if o.State != "open" || len(o.Inputs) != 2 || o.InputsValue != 550e8 || o.Amount != 500e8 ||
		o.MaxFee != offerMaxFee || o.ExpiresAt != r.now.Add(offerLifetime).Unix() {
		t.Fatalf("%+v", o)
	}
	for _, d := range r.fw.leases {
		if d != offerLifetime {
			t.Fatalf("leased for %v", d)
		}
	}
	if len(r.fw.leases) != 2 {
		t.Fatalf("%d leases", len(r.fw.leases))
	}
	// The addresses are distinct and fresh.
	if o.RefundAddress == o.RewardAddress || o.RefundAddress == o.ChangeAddress || o.OwnerAddress == "" {
		t.Fatalf("%+v", o)
	}
	// The leased coins are no longer spendable: a second offer lacks funds.
	if _, err := r.dw.CreateMasternodeOffer(ctx, 500e8); err == nil {
		t.Fatal("a second offer reused leased coins")
	}
	// Reserved owner indexes are never reused.
	mgr, _ := r.dw.manager()
	offers, _ := mgr.Store().Offers()
	if len(offers) != 1 {
		t.Fatalf("%d offers", len(offers))
	}
}

// Offers expire with their leases: an open offer past its time is released and
// dropped, and a signed one is kept, shown as expired.
func TestMasternodeOffersExpire(t *testing.T) {
	ctx := t.Context()
	r := newOfferRig(t)
	r.fund(t)
	o, err := r.dw.CreateMasternodeOffer(ctx, 500e8)
	if err != nil {
		t.Fatal(err)
	}
	list, err := r.dw.MasternodeOffers(ctx)
	if err != nil || len(list) != 1 || list[0].State != "open" {
		t.Fatalf("%+v %v", list, err)
	}
	r.now = r.now.Add(offerLifetime)
	list, err = r.dw.MasternodeOffers(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("an expired offer was listed: %+v %v", list, err)
	}
	if len(r.fw.leases) != 0 {
		t.Fatal("an expired offer kept its leases")
	}
	// An expired offer cannot be reviewed.
	if _, err := r.dw.ReviewMasternodeRegistration(ctx, o.ID, "{}"); err == nil {
		t.Fatal("reviewed an offer that is gone")
	}
}

func TestCancelMasternodeOffer(t *testing.T) {
	ctx := t.Context()
	r := newOfferRig(t)
	r.fund(t)
	o, err := r.dw.CreateMasternodeOffer(ctx, 500e8)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.dw.CancelMasternodeOffer(ctx, "zz"); err == nil {
		t.Fatal("a malformed id")
	}
	if err := r.dw.CancelMasternodeOffer(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if len(r.fw.leases) != 0 {
		t.Fatal("a cancelled offer kept its leases")
	}
	if list, _ := r.dw.MasternodeOffers(ctx); len(list) != 0 {
		t.Fatal("a cancelled offer is listed")
	}
	if err := r.dw.CancelMasternodeOffer(ctx, o.ID); !errors.Is(err, masternode.ErrUnknownOffer) {
		t.Fatalf("%v", err)
	}
}

// The whole of a registration: review, consent, the other owners' consent,
// signing the inputs, and broadcast.
func TestMasternodeRegistrationFlow(t *testing.T) {
	ctx := t.Context()
	r := newOfferRig(t)
	r.fund(t)
	o, err := r.dw.CreateMasternodeOffer(ctx, 500e8)
	if err != nil {
		t.Fatal(err)
	}
	other := r.build(t, o, rigFee+3)
	tx := r.build(t, o, rigFee)

	rv, err := r.dw.ReviewMasternodeRegistration(ctx, o.ID, blobOf(t, tx))
	if err != nil {
		t.Fatal(err)
	}
	if rv.ShareIndex != 0 || !rv.Terms.Shares[0].Own || rv.Terms.Shares[1].Own ||
		rv.FeePortion != uint64(masternode.FeePortion(rigFee, 0, 3)) || len(rv.OtherOutputs) != 2 || len(rv.SignedShares) != 0 {
		t.Fatalf("%+v", rv)
	}

	// A wrong review id signs nothing, and changes nothing.
	if _, err := r.dw.SignMasternodeRegistration(ctx, o.ID, blobOf(t, tx), "00"); !errors.Is(err, masternode.ErrReviewChanged) {
		t.Fatalf("%v", err)
	}
	if list, _ := r.dw.MasternodeOffers(ctx); list[0].State != "open" {
		t.Fatal("a refused signature changed the offer")
	}
	// A locked wallet does not sign.
	r.fw.locked = true
	if _, err := r.dw.SignMasternodeRegistration(ctx, o.ID, blobOf(t, tx), rv.ReviewID); err == nil {
		t.Fatal("a locked wallet signed")
	}
	r.fw.locked = false

	// Round one: the consent only.
	s1, err := r.dw.SignMasternodeRegistration(ctx, o.ID, blobOf(t, tx), rv.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if s1.Complete {
		t.Fatal("complete with one consent")
	}
	if list, _ := r.dw.MasternodeOffers(ctx); list[0].State != "consented" {
		t.Fatalf("%+v", list)
	}
	var b1 masternode.Blob
	if err := json.Unmarshal([]byte(s1.Blob), &b1); err != nil || len(b1.Signatures) != 1 || b1.Signatures[0].ShareIndex != 0 {
		t.Fatalf("%+v %v", b1, err)
	}

	// A consented offer takes only that registration.
	if _, err := r.dw.ReviewMasternodeRegistration(ctx, o.ID, blobOf(t, other)); err == nil {
		t.Fatal("a consented offer took another registration")
	}

	// Round two: every consent is in. The review id is the one from before.
	others := r.othersConsent(t, tx)
	rv2, err := r.dw.ReviewMasternodeRegistration(ctx, o.ID, blobOf(t, tx, others...))
	if err != nil {
		t.Fatal(err)
	}
	if rv2.ReviewID != rv.ReviewID || len(rv2.SignedShares) != 2 {
		t.Fatalf("review id %s, then %s; signed %v", rv.ReviewID, rv2.ReviewID, rv2.SignedShares)
	}
	s2, err := r.dw.SignMasternodeRegistration(ctx, o.ID, blobOf(t, tx, append(others, b1.Signatures...)...), rv2.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Complete {
		t.Fatal("not complete with every consent")
	}
	if list, _ := r.dw.MasternodeOffers(ctx); list[0].State != "signed" {
		t.Fatalf("%+v", list)
	}
	for _, d := range r.fw.leases {
		if d != signedLifetime {
			t.Fatalf("a signed offer's lease is %v", d)
		}
	}

	// A signed offer is kept past its time, shown as expired, with its leases.
	r.now = r.now.Add(signedLifetime)
	if list, _ := r.dw.MasternodeOffers(ctx); len(list) != 1 || list[0].State != "expired" || len(r.fw.leases) != 2 {
		t.Fatalf("%+v, %d leases", list, len(r.fw.leases))
	}
	r.now = r.now.Add(-signedLifetime)

	// Broadcast needs every input signed: the others sign theirs.
	signed, _, err := masternode.ParseBlob([]byte(s2.Blob))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.dw.BroadcastMasternodeRegistration(ctx, o.ID, s2.Blob); err == nil {
		t.Fatal("broadcast with inputs that are not signed")
	}
	if len(r.fw.published) != 0 {
		t.Fatal("published an incomplete registration")
	}
	final := signed
	for i := range r.own {
		rv, err := masternode.Review(final, masternode.RegtestRules, r.own[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := rv.SignInputs(otherSecrets{r.fundKeys[i]}); err != nil {
			t.Fatal(err)
		}
		raw, _ := rv.Raw()
		final = &bisonwire.Tx{Chain: "dash"}
		if err := final.Deserialize(bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	finalBlob := blobOf(t, final)

	// Another registration is not the one the offer signed.
	if _, err := r.dw.BroadcastMasternodeRegistration(ctx, o.ID, blobOf(t, other)); err == nil {
		t.Fatal("broadcast of another registration")
	}
	txid, err := r.dw.BroadcastMasternodeRegistration(ctx, o.ID, finalBlob)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.fw.published) != 1 || r.fw.published[0].TxHash().String() != txid {
		t.Fatalf("published %d, txid %s", len(r.fw.published), txid)
	}
	list, _ := r.dw.MasternodeOffers(ctx)
	if list[0].State != "broadcast" || list[0].TxID != txid {
		t.Fatalf("%+v", list)
	}
	// A broadcast offer that is never mined is not stuck: it can be cancelled.
	if err := r.dw.CancelMasternodeOffer(ctx, o.ID); err != nil {
		t.Fatalf("cancelling a broadcast offer that did not register: %v", err)
	}
	if list, _ := r.dw.MasternodeOffers(ctx); len(list) != 0 || len(r.fw.leases) != 0 {
		t.Fatalf("%+v, %d leases", list, len(r.fw.leases))
	}
}

// A signature whose response was lost is asked for again: the signed offer
// gives the same blob for the same registration, and refuses another.
func TestSignIsRepeatable(t *testing.T) {
	ctx := t.Context()
	r := newOfferRig(t)
	r.fund(t)
	o, err := r.dw.CreateMasternodeOffer(ctx, 500e8)
	if err != nil {
		t.Fatal(err)
	}
	other := r.build(t, o, rigFee+3)
	tx := r.build(t, o, rigFee)
	rv, err := r.dw.ReviewMasternodeRegistration(ctx, o.ID, blobOf(t, tx))
	if err != nil {
		t.Fatal(err)
	}
	own, err := r.dw.SignMasternodeRegistration(ctx, o.ID, blobOf(t, tx), rv.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	var b1 masternode.Blob
	if err := json.Unmarshal([]byte(own.Blob), &b1); err != nil {
		t.Fatal(err)
	}
	blob := blobOf(t, tx, append(r.othersConsent(t, tx), b1.Signatures...)...)
	first, err := r.dw.SignMasternodeRegistration(ctx, o.ID, blob, rv.ReviewID)
	if err != nil || !first.Complete {
		t.Fatalf("%v %v", first, err)
	}
	again, err := r.dw.SignMasternodeRegistration(ctx, o.ID, blob, rv.ReviewID)
	if err != nil {
		t.Fatalf("asking again: %v", err)
	}
	if again.Blob != first.Blob {
		t.Fatal("a retry gave another blob")
	}
	// Another registration is not the one the offer signed.
	if _, err := r.dw.ReviewMasternodeRegistration(ctx, o.ID, blobOf(t, other)); err == nil {
		t.Fatal("a signed offer took another registration")
	}
}

// An offer whose lease lapsed and whose coins another offer has since leased
// is still reaped, and does not hide the other.
func TestReapSurvivesCoinsLeasedAgain(t *testing.T) {
	ctx := t.Context()
	r := newOfferRig(t)
	r.fund(t)
	a, err := r.dw.CreateMasternodeOffer(ctx, 500e8)
	if err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(offerLifetime + time.Hour)
	b, err := r.dw.CreateMasternodeOffer(ctx, 500e8)
	if err != nil {
		t.Fatalf("the coins of an expired offer are free: %v", err)
	}
	list, err := r.dw.MasternodeOffers(ctx)
	if err != nil || len(list) != 1 || list[0].ID != b.ID || list[0].State != "open" || list[0].ID == a.ID {
		t.Fatalf("%+v %v", list, err)
	}
	if len(r.fw.leases) != 2 {
		t.Fatalf("%d leases: the new offer's were released", len(r.fw.leases))
	}
}

// An offer past its time refuses to sign, even for a review that was current.
func TestSignRefusesExpiredOffer(t *testing.T) {
	ctx := t.Context()
	r := newOfferRig(t)
	r.fund(t)
	o, err := r.dw.CreateMasternodeOffer(ctx, 500e8)
	if err != nil {
		t.Fatal(err)
	}
	tx := r.build(t, o, rigFee)
	rv, err := r.dw.ReviewMasternodeRegistration(ctx, o.ID, blobOf(t, tx))
	if err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(offerLifetime + time.Second)
	if _, err := r.dw.SignMasternodeRegistration(ctx, o.ID, blobOf(t, tx), rv.ReviewID); err == nil {
		t.Fatal("signed for an expired offer")
	}
}

// A registration that does not pay the offer's share is refused, however it
// is labelled.
func TestReviewRefusesAnotherShare(t *testing.T) {
	ctx := t.Context()
	r := newOfferRig(t)
	r.fund(t)
	o, err := r.dw.CreateMasternodeOffer(ctx, 500e8)
	if err != nil {
		t.Fatal(err)
	}
	// A second offer of the same size: its registration is not the first offer's.
	r.fw.addCoin(t, 0x13, 0, 600e8)
	o2, err := r.dw.CreateMasternodeOffer(ctx, 500e8)
	if err != nil {
		t.Fatal(err)
	}
	tx := r.build(t, o2, rigFee)
	if _, err := r.dw.ReviewMasternodeRegistration(ctx, o.ID, blobOf(t, tx)); !errors.Is(err, masternode.ErrOfferMismatch) {
		t.Fatalf("%v", err)
	}
	big := `{"tx":"` + strings.Repeat("ab", maxBlobSize) + `"}`
	if _, err := r.dw.ReviewMasternodeRegistration(ctx, o.ID, big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversized blob: %v", err)
	}
	if _, err := r.dw.ReviewMasternodeRegistration(ctx, o.ID, "not json"); err == nil {
		t.Fatal("a malformed blob was read")
	}
}

func TestOfferFundingSelection(t *testing.T) {
	fw := newFakeFunding()
	var small []wire.OutPoint
	for i := byte(0); i < maxOfferInputs+1; i++ {
		small = append(small, fw.addCoin(t, 0x20+i, 0, 10e8))
	}
	// A coin that is not pay to key hash is skipped.
	fw.unspent = append(fw.unspent, &btcjson.ListUnspentResult{
		TxID: chainhash.Hash{0xee}.String(), ScriptPubKey: hex.EncodeToString(append([]byte{txscript.OP_HASH160, 20}, make([]byte, 22)...)),
		Amount: 1000,
	})
	if _, err := selectFunding(fw, 205e8); err == nil {
		t.Fatal("more than the most inputs")
	}
	got, err := selectFunding(fw, 50e8)
	if err != nil || len(got) != 5 {
		t.Fatalf("%d inputs, %v", len(got), err)
	}
	fw.addCoin(t, 0x70, 0, 600e8)
	got, err = selectFunding(fw, 500e8)
	if err != nil || len(got) != 1 || got[0].Value != 600e8 {
		t.Fatalf("largest first: %+v %v", got, err)
	}
	_ = small
}

// otherSecrets signs for another owner's funding key.
type otherSecrets struct{ key *btcec.PrivateKey }

func (s otherSecrets) GetKey(btcutil.Address) (*btcec.PrivateKey, bool, error) {
	return s.key, true, nil
}
func (s otherSecrets) GetScript(btcutil.Address) ([]byte, error) { return nil, errors.New("none") }
func (s otherSecrets) ChainParams() *chaincfg.Params             { return RegressionNetParams }

// A share is at least the minimum and leaves the minimum to the others, however
// much the wallet holds.
func TestCreateMasternodeOfferBounds(t *testing.T) {
	r := newOfferRig(t)
	r.fw.addCoin(t, 0x14, 0, 1100e8)
	for _, amt := range []uint64{masternode.MinShareAmount - 1, masternode.CollateralAmount - masternode.MinShareAmount + 1, masternode.CollateralAmount} {
		if _, err := r.dw.CreateMasternodeOffer(t.Context(), amt); err == nil {
			t.Fatalf("an offer for %d", amt)
		}
	}
	if len(r.fw.leases) != 0 {
		t.Fatal("a refused offer left leases")
	}
	if _, err := r.dw.CreateMasternodeOffer(t.Context(), masternode.MinShareAmount); err != nil {
		t.Fatalf("the smallest share: %v", err)
	}
}

// An offer whose masternode the wallet has found registered is shown as
// registered, is never reaped, and cannot be cancelled.
func TestRegisteredOffer(t *testing.T) {
	ctx := t.Context()
	r := newOfferRig(t)
	mgr, _ := r.dw.manager()
	owned := mgr.List()
	if len(owned) != 1 {
		t.Fatal("no registered masternode")
	}
	off := &masternode.Offer{
		ID: [32]byte{7}, State: masternode.OfferBroadcast, ExpiresAt: r.now.Add(-time.Hour).Unix(),
		OwnerKeyID: owned[0].MN.Shares[owned[0].Shares[0]].KeyIDOwner,
	}
	if err := mgr.Store().PutOffer(off); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(off.ID[:])
	list, err := r.dw.MasternodeOffers(ctx)
	if err != nil || len(list) != 1 || list[0].State != "registered" {
		t.Fatalf("%+v %v", list, err)
	}
	if err := r.dw.CancelMasternodeOffer(ctx, id); err == nil {
		t.Fatal("cancelled the offer of a registered masternode")
	}
}
