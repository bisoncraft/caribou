package ltc

import (
	"testing"
)

func TestLtcNetParamsMappings(t *testing.T) {
	// Verify ltcNetParams returns non-nil for all supported networks.
	for _, p := range []*struct {
		name string
	}{
		{"mainnet"},
		{"testnet4"},
		{"regtest"},
	} {
		_ = p
	}
	if np := ltcNetParams(MainNetParams); np == nil {
		t.Fatal("ltcNetParams(mainnet) returned nil")
	}
	if np := ltcNetParams(TestNet4Params); np == nil {
		t.Fatal("ltcNetParams(testnet4) returned nil")
	}
	if np := ltcNetParams(RegressionNetParams); np == nil {
		t.Fatal("ltcNetParams(regtest) returned nil")
	}

	// Compile-time check: verify createSPVWallet exists with the signature
	// expected by btc.Create.
	_ = createSPVWallet
}
