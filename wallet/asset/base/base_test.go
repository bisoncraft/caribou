// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package base

import (
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/ethereum/go-ethereum/common"
)

// TestTokensRegistryConsistency asserts structural invariants of the static
// Tokens map. OpenTokenWallet refuses to open a token wallet whose NetToken
// has no GasEstimate, so an omission here is a wallet-open regression.
func TestTokensRegistryConsistency(t *testing.T) {
	t.Parallel()
	for assetID, tok := range Tokens {
		if tok.Token == nil {
			t.Errorf("assetID %d: nil embedded util.Token", assetID)
			continue
		}
		if tok.Name == "" {
			t.Errorf("assetID %d: empty token name", assetID)
		}
		if len(tok.NetTokens) == 0 {
			t.Errorf("assetID %d (%s): no NetTokens", assetID, tok.Name)
		}
		for net, nt := range tok.NetTokens {
			if g := nt.GasEstimate; g == nil || g.Approve == 0 || g.Transfer == 0 {
				t.Errorf("%s/%s: missing gas estimate", tok.Name, net)
			}
			// Simnet token addresses are filled in at runtime from the harness;
			// mainnet and testnet must be present in the static map.
			if net != util.Simnet && (nt.Address == common.Address{}) {
				t.Errorf("%s/%s: zero token address", tok.Name, net)
			}
		}
	}
}
