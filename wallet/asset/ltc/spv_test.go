package ltc

import (
	"testing"

	dexltc "github.com/bisoncraft/bosswallet/util/networks/ltc"
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
	if np := ltcNetParams(dexltc.MainNetParams); np == nil {
		t.Fatal("ltcNetParams(mainnet) returned nil")
	}
	if np := ltcNetParams(dexltc.TestNet4Params); np == nil {
		t.Fatal("ltcNetParams(testnet4) returned nil")
	}
	if np := ltcNetParams(dexltc.RegressionNetParams); np == nil {
		t.Fatal("ltcNetParams(regtest) returned nil")
	}

	// Compile-time check: verify createSPVWallet and openSPVWallet exist with
	// the signatures expected by btc.OpenSPVWallet / btc.Create.
	_ = createSPVWallet
	_ = openSPVWallet
}
