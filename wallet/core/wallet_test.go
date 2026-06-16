// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"testing"

	"github.com/bisoncraft/bosswallet/wallet/txdb"
)

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
