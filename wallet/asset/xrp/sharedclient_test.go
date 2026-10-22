package xrp

import (
	"context"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/testing/sharedclient"
)

// TestRPCUsesSharedClient: rippled rpc requests must go through the shared
// client so --torproxy applies to them.
func TestRPCUsesSharedClient(t *testing.T) {
	reqs := sharedclient.Capture(t)
	newRPCClient("http://rippled.example", util.Disabled).call(context.Background(), "server_info", nil)
	sharedclient.AssertReached(t, reqs, "xrp rpc")
}
