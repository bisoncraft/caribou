package asset

import "testing"

// The bit order of the wallet traits is persisted and mirrored in the
// frontend, so a new trait goes after the last one.
func TestWalletTraitBits(t *testing.T) {
	if WalletTraitDestinationTagger != 1<<22 || WalletTraitMasternodeOwner != 1<<23 {
		t.Fatalf("trait bits moved: %d %d", WalletTraitDestinationTagger, WalletTraitMasternodeOwner)
	}
	if !WalletTraitMasternodeOwner.IsMasternodeOwner() || WalletTraitDestinationTagger.IsMasternodeOwner() {
		t.Fatal("IsMasternodeOwner tests the wrong bit")
	}
}
