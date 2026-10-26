package dexnet

import (
	"net/http"
	"sync"
	"testing"

	"github.com/bisoncraft/caribou/util/testing/proxycanary"
)

// TestSetProxyRoutesSharedClient: many assets (fee sources, fiat rates, DCR,
// bridges, XRP rpc) reach the network only through the shared Client, so
// SetProxy must be what carries their traffic, never the server directly.
func TestSetProxyRoutesSharedClient(t *testing.T) {
	ProxyLocalForTest(t)
	server, proxy := proxycanary.New(t), proxycanary.New(t)

	// SetProxy applies once per process; reset it and restore the shared
	// client so other tests see the unproxied one.
	oldClient := Client
	Client = &http.Client{Timeout: oldClient.Timeout}
	setProxyOnce = sync.Once{}
	t.Cleanup(func() {
		Client = oldClient
		setProxyOnce = sync.Once{}
	})

	SetProxy(proxy.Addr)
	if resp, err := Client.Get("http://" + server.Addr); err == nil {
		resp.Body.Close()
		t.Fatal("request succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, server)
}
