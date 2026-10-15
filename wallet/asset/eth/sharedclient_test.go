//go:build !harness && !rpclive

package eth

import (
	"context"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/testing/sharedclient"
)

// TestBridgeCallsUseSharedClient: the Across and Circle API requests must go
// through the shared client so --torproxy applies to them.
func TestBridgeCallsUseSharedClient(t *testing.T) {
	ctx := context.Background()
	tests := map[string]func(){
		"across routes": func() { getAcrossAvailableRoutes(ctx, util.Mainnet) },
		"usdc attestation": func() {
			(&usdcBridge{attestationUrl: "https://circle.example/attestations/"}).getAttestation(ctx, []byte("msg"))
		},
	}
	for name, call := range tests {
		reqs := sharedclient.Capture(t)
		call()
		sharedclient.AssertReached(t, reqs, name)
	}
}
