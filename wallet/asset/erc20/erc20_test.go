// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package erc20

import "testing"

func TestParseABI(t *testing.T) {
	if ERC20ABI.Methods["transfer"].RawName != "transfer" {
		t.Fatal("transfer method missing from ERC20 ABI")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("parseABI did not panic for invalid ABI")
		}
	}()
	parseABI("not JSON")
}
