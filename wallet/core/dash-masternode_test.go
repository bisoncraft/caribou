package core

import (
	"context"
	"errors"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
)

type tMasternodeWallet struct {
	*TXCWallet
	mns    []*asset.Masternode
	status *asset.MasternodeStatus
	hash   string
	err    error
	// offers
	offer    *asset.MasternodeOffer
	offers   []*asset.MasternodeOffer
	review   *asset.MasternodeRegReview
	signed   *asset.MasternodeRegSigned
	txid     string
	gotAmt   uint64
	gotOffer string
	gotBlob  string
	gotRev   string
}

func (w *tMasternodeWallet) CreateMasternodeOffer(_ context.Context, amount uint64) (*asset.MasternodeOffer, error) {
	w.gotAmt = amount
	return w.offer, w.err
}
func (w *tMasternodeWallet) MasternodeOffers(context.Context) ([]*asset.MasternodeOffer, error) {
	return w.offers, w.err
}
func (w *tMasternodeWallet) CancelMasternodeOffer(_ context.Context, id string) error {
	w.gotOffer = id
	return w.err
}
func (w *tMasternodeWallet) ReviewMasternodeRegistration(_ context.Context, id, blob string) (*asset.MasternodeRegReview, error) {
	w.gotOffer, w.gotBlob = id, blob
	return w.review, w.err
}
func (w *tMasternodeWallet) SignMasternodeRegistration(_ context.Context, id, blob, rev string) (*asset.MasternodeRegSigned, error) {
	w.gotOffer, w.gotBlob, w.gotRev = id, blob, rev
	return w.signed, w.err
}
func (w *tMasternodeWallet) BroadcastMasternodeRegistration(_ context.Context, id, blob string) (string, error) {
	w.gotOffer, w.gotBlob = id, blob
	return w.txid, w.err
}

func (w *tMasternodeWallet) Masternodes(context.Context) ([]*asset.Masternode, error) {
	return w.mns, w.err
}

func (w *tMasternodeWallet) MasternodeStatus(_ context.Context, h string) (*asset.MasternodeStatus, error) {
	w.hash = h
	return w.status, w.err
}

func TestMasternodeOwnerCore(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core

	// No wallet.
	if _, err := tCore.Masternodes(t.Context(), tACCTAsset.ID); err == nil {
		t.Fatal("no error without a wallet")
	}
	// A wallet that does not own masternodes.
	xcw, _ := newTWallet(tACCTAsset.ID)
	tCore.wallets[tACCTAsset.ID] = xcw
	if _, err := tCore.Masternodes(t.Context(), tACCTAsset.ID); err == nil {
		t.Fatal("no error for a wallet that is not a MasternodeOwner")
	}
	if _, err := tCore.MasternodeStatus(t.Context(), tACCTAsset.ID, "ab"); err == nil {
		t.Fatal("no error for a wallet that is not a MasternodeOwner")
	}
	// One that does.
	xcw, tw := newTWallet(tACCTAsset.ID)
	mw := &tMasternodeWallet{TXCWallet: tw, mns: []*asset.Masternode{{ProTxHash: "ab"}}, status: &asset.MasternodeStatus{State: "active"}}
	xcw.Wallet = mw
	tCore.wallets[tACCTAsset.ID] = xcw
	mns, err := tCore.Masternodes(t.Context(), tACCTAsset.ID)
	if err != nil || len(mns) != 1 || mns[0] != mw.mns[0] {
		t.Fatalf("%v %v", mns, err)
	}
	st, err := tCore.MasternodeStatus(t.Context(), tACCTAsset.ID, "ab")
	if err != nil || st != mw.status || mw.hash != "ab" {
		t.Fatalf("%v %v %q", st, err, mw.hash)
	}
	mw.err = errors.New("boom")
	if _, err := tCore.MasternodeStatus(t.Context(), tACCTAsset.ID, "ab"); !errors.Is(err, mw.err) {
		t.Fatalf("%v", err)
	}
}

func TestMasternodeOfferCore(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	ctx := t.Context()

	// Not a masternode owner, or no wallet.
	if _, err := tCore.CreateMasternodeOffer(ctx, tACCTAsset.ID, tPW, 1); err == nil {
		t.Fatal("an offer without a wallet")
	}
	xcw, _ := newTWallet(tACCTAsset.ID)
	tCore.wallets[tACCTAsset.ID] = xcw
	if _, err := tCore.CreateMasternodeOffer(ctx, tACCTAsset.ID, tPW, 1); err == nil {
		t.Fatal("an offer from a wallet that is not a MasternodeOwner")
	}
	if err := tCore.CancelMasternodeOffer(ctx, tACCTAsset.ID, "ab"); err == nil {
		t.Fatal("cancelled with a wallet that is not a MasternodeOwner")
	}

	xcw, tw := newTWallet(tACCTAsset.ID)
	mw := &tMasternodeWallet{
		TXCWallet: tw, offer: &asset.MasternodeOffer{ID: "o1"}, offers: []*asset.MasternodeOffer{{ID: "o1"}},
		review: &asset.MasternodeRegReview{ReviewID: "r1"}, signed: &asset.MasternodeRegSigned{Blob: "b2"}, txid: "tx1",
	}
	xcw.Wallet = mw
	tCore.wallets[tACCTAsset.ID] = xcw

	if o, err := tCore.CreateMasternodeOffer(ctx, tACCTAsset.ID, tPW, 5); err != nil || o != mw.offer || mw.gotAmt != 5 {
		t.Fatalf("%v %v %d", o, err, mw.gotAmt)
	}
	if l, err := tCore.MasternodeOffers(ctx, tACCTAsset.ID); err != nil || len(l) != 1 {
		t.Fatalf("%v %v", l, err)
	}
	if err := tCore.CancelMasternodeOffer(ctx, tACCTAsset.ID, "o1"); err != nil || mw.gotOffer != "o1" {
		t.Fatalf("%v %q", err, mw.gotOffer)
	}
	if r, err := tCore.ReviewMasternodeRegistration(ctx, tACCTAsset.ID, "o1", "blob"); err != nil || r != mw.review || mw.gotBlob != "blob" {
		t.Fatalf("%v %v", r, err)
	}
	if s, err := tCore.SignMasternodeRegistration(ctx, tACCTAsset.ID, tPW, "o1", "blob", "r1"); err != nil || s != mw.signed || mw.gotRev != "r1" {
		t.Fatalf("%v %v", s, err)
	}
	if id, err := tCore.BroadcastMasternodeRegistration(ctx, tACCTAsset.ID, "o1", "blob"); err != nil || id != "tx1" {
		t.Fatalf("%v %v", id, err)
	}
	mw.err = errors.New("boom")
	if _, err := tCore.BroadcastMasternodeRegistration(ctx, tACCTAsset.ID, "o1", "blob"); !errors.Is(err, mw.err) {
		t.Fatalf("%v", err)
	}
}
