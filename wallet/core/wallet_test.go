// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"errors"
	"testing"

	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
)

// walletOnly embeds asset.Wallet by interface, not by concrete type, so it
// promotes only the asset.Wallet method set — even if the wrapped value's
// concrete type also implements asset.TrustlineEstablisher, walletOnly does
// not, letting the test exercise xcWallet.establishTrustline's not-a-
// TrustlineEstablisher branch with the same TXCWallet test double.
type walletOnly struct {
	asset.Wallet
}

// establishTrustline must delegate to the wallet's TrustlineEstablisher
// implementation when it has one, and be a harmless no-op otherwise — most
// asset wallets (anything that isn't an XRPL issued-currency token) don't
// require this opt-in step at all.
func TestEstablishTrustlineDelegatesWhenSupported(t *testing.T) {
	xcw, tw := newTWallet(0)
	tw.trustlineErr = errors.New("boom")
	if err := xcw.establishTrustline(); err == nil || err.Error() != "boom" {
		t.Fatalf("establishTrustline() = %v, want the underlying TrustlineEstablisher error", err)
	}
	if tw.trustlineCalls != 1 {
		t.Fatalf("EstablishTrustline called %d times, want 1", tw.trustlineCalls)
	}

	tw.trustlineErr = nil
	if err := xcw.establishTrustline(); err != nil {
		t.Fatalf("establishTrustline() = %v, want nil", err)
	}
}

func TestEstablishTrustlineNoOpForNonEstablisher(t *testing.T) {
	xcw, tw := newTWallet(0)
	tw.trustlineErr = errors.New("must not be reached")
	xcw.Wallet = walletOnly{tw}
	if err := xcw.establishTrustline(); err != nil {
		t.Fatalf("establishTrustline() = %v, want nil no-op for a non-TrustlineEstablisher wallet", err)
	}
	if tw.trustlineCalls != 0 {
		t.Fatalf("EstablishTrustline called %d times, want 0", tw.trustlineCalls)
	}
}

func TestProcessWalletTransactionsClearsPendingAlias(t *testing.T) {
	w := &xcWallet{
		pendingTxs: map[string]*txdb.WalletTransaction{
			"0x0000018bcfe56800": {
				ID: "0x0000018bcfe56800",
			},
		},
	}

	confirmed := &txdb.WalletTransaction{
		ID:        "0xabc123",
		Confirmed: true,
		AdditionalData: map[string]string{
			"pendingID": "0x0000018bcfe56800",
		},
	}

	w.processWalletTransactions([]*txdb.WalletTransaction{confirmed})

	if _, found := w.pendingTxs["0x0000018bcfe56800"]; found {
		t.Fatal("pending alias was not removed")
	}
	if _, found := w.pendingTxs["0xabc123"]; found {
		t.Fatal("confirmed tx should not remain in pending map")
	}
}
