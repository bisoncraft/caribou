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
