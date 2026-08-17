// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dash

import (
	"slices"
	"testing"
)

// TestAvailableWallets pins the wallet menu exactly rather than checking mere
// membership. AvailableWallets is ordered and its first entry is the default
// onboarding picks, so the order is load-bearing, and mobile drops SPV
// entirely. The expectation is build-tagged alongside the list itself. See
// wallets_desktop.go and wallets_mobile.go.
func TestAvailableWallets(t *testing.T) {
	gotTypes := make([]string, 0, len(WalletInfo.AvailableWallets))
	for _, wd := range WalletInfo.AvailableWallets {
		gotTypes = append(gotTypes, wd.Type)
	}
	if !slices.Equal(gotTypes, wantWalletTypes) {
		t.Fatalf("AvailableWallets types = %v, want %v", gotTypes, wantWalletTypes)
	}
}
