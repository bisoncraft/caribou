package zec

import (
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset/btc"
)

// TestRPCConnConfigProxy is a canary for the rpcclient dial, which the
// outbound guard test cannot see: a remote zcashd must be reached through
// the proxy, a local one directly.
func TestRPCConnConfigProxy(t *testing.T) {
	const proxy = "127.0.0.1:9050"
	remote := rpcConnConfig(&btc.RPCConfig{RPCBind: "zcashd.example.com:8232"}, proxy)
	if !strings.HasPrefix(remote.Proxy, "socks5://") || !strings.HasSuffix(remote.Proxy, "@"+proxy) {
		t.Fatalf("remote zcashd not proxied with isolation credentials: %q", remote.Proxy)
	}
	if local := rpcConnConfig(&btc.RPCConfig{RPCBind: "127.0.0.1:8232"}, proxy); local.Proxy != "" {
		t.Fatalf("local zcashd proxied: %q", local.Proxy)
	}
	if none := rpcConnConfig(&btc.RPCConfig{RPCBind: "zcashd.example.com:8232"}, ""); none.Proxy != "" {
		t.Fatalf("proxy set without --torproxy: %q", none.Proxy)
	}
}
