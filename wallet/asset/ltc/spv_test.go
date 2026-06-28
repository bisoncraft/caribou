package ltc

import (
	"testing"
)

func TestLtcNetParamsMappings(t *testing.T) {
	// Verify ltcTableNetParams returns non-nil for all supported networks.
	if np := ltcTableNetParams(MainNetParams); np == nil {
		t.Fatal("ltcTableNetParams(mainnet) returned nil")
	}
	if np := ltcTableNetParams(TestNet4Params); np == nil {
		t.Fatal("ltcTableNetParams(testnet4) returned nil")
	}
	if np := ltcTableNetParams(RegressionNetParams); np == nil {
		t.Fatal("ltcTableNetParams(regtest) returned nil")
	}
}
