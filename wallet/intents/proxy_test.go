package intents

import (
	"context"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/testing/proxycanary"
)

// TestClientUsesProxy: 1Click requests must go through the configured proxy.
// Without it the user's IP reaches the service, for the catalog fetch on
// opening an asset view as much as for a swap.
func TestClientUsesProxy(t *testing.T) {
	service, proxy := proxycanary.New(t), proxycanary.New(t)

	c, err := NewClient(&Config{BaseURL: "http://" + service.Addr, Proxy: proxy.Addr}, util.Disabled)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Tokens(context.Background()); err == nil {
		t.Fatal("Tokens succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, service)
}
