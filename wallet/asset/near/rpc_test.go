// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestRPCContextCancel verifies that a context deadline actually aborts an
// in-flight RPC call rather than being ignored.
func TestRPCContextCancel(t *testing.T) {
	m := newMockNearRPC(t)
	m.handle("block", func(_ json.RawMessage) (any, *rpcError) {
		time.Sleep(200 * time.Millisecond) // slow response
		return map[string]any{"header": map[string]any{"height": uint64(1), "hash": "11111111111111111111111111111111111111111", "timestamp": uint64(0)}}, nil
	})
	c := m.client()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.getBlock(ctx, "final"); err == nil {
		t.Fatal("expected error from cancelled context")
	}
}
