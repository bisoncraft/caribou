//go:build !harness && !vspd

package dcr

import (
	"context"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/testing/sharedclient"
)

// TestDCRCallsUseSharedClient: the fee oracle, VSP info and dcrdata lookups
// must go through the shared client so --torproxy applies to them.
func TestDCRCallsUseSharedClient(t *testing.T) {
	ctx := context.Background()
	tests := map[string]func(){
		"fee oracle": func() { fetchFeeFromOracle(ctx, util.Mainnet, 2) },
		"vsp info":   func() { vspInfo(ctx, "https://vsp.example") },
		"dcrdata":    func() { getDcrdataTxs(ctx, "Dsexample", util.Mainnet) },
	}
	for name, call := range tests {
		reqs := sharedclient.Capture(t)
		call()
		sharedclient.AssertReached(t, reqs, name)
	}
}
