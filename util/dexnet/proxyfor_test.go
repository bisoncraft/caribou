package dexnet

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util/testing/proxycanary"
)

// TestProxyFor: Tor cannot reach loopback or private addresses, so a local
// node must be dialed directly while a remote one goes through the proxy.
func TestProxyFor(t *testing.T) {
	const proxy = "127.0.0.1:9050"
	tests := []struct {
		addr, proxy, want string
	}{
		{"127.0.0.1:18081", proxy, ""},
		{"http://127.0.0.1:28081", proxy, ""},
		{"localhost:9110", proxy, ""},
		{"[::1]:9110", proxy, ""},
		{"192.168.1.5:9110", proxy, ""},
		{"10.0.0.2", proxy, ""},
		{"[::1]", proxy, ""},
		{"http://[::1]", proxy, ""},
		{"0.0.0.0:8232", proxy, ""},
		{"[::]:8232", proxy, ""},
		{"169.254.1.1:8232", proxy, ""},
		{"[fe80::1]:8232", proxy, ""},
		{"100.101.102.103:8232", proxy, ""},
		{"LOCALHOST:8232", proxy, ""},
		{"[fe80::1%en0]:8232", proxy, ""},
		{"http://[fe80::1%25en0]:8232", proxy, ""},
		{"foo.localhost:8232", proxy, proxy},
		{"mynode.lan:8232", proxy, proxy},
		{"100.128.0.1:8232", proxy, proxy},
		{"node.example.com:18081", proxy, proxy},
		{"https://node.example.com:18081", proxy, proxy},
		{"8.8.8.8:53", proxy, proxy},
		{"node.example.com:18081", "", ""},
	}
	for _, tt := range tests {
		if got := ProxyFor(tt.addr, tt.proxy); got != tt.want {
			t.Errorf("ProxyFor(%q, %q) = %q, want %q", tt.addr, tt.proxy, got, tt.want)
		}
	}
}

// TestProxyDialContextLocalTargets: Tor cannot reach local addresses, so a
// custom provider on this machine or LAN must be dialed direct while remote
// hosts still take the proxy.
func TestProxyDialContextLocalTargets(t *testing.T) {
	local := proxycanary.New(t)
	proxy := proxycanary.New(t)
	dial := ProxyDialContext(proxy.Addr)

	if conn, err := dial(context.Background(), "tcp", local.Addr); err != nil {
		t.Fatalf("local target was not dialed direct: %v", err)
	} else {
		conn.Close()
	}
	if !local.WaitAccepted(2 * time.Second) {
		t.Fatal("local target never received the direct connection")
	}
	if proxy.Accepted() != 0 {
		t.Fatal("local target went through the proxy")
	}

	dial(context.Background(), "tcp", "node.example.com:443") // fails: canary hangs up
	if !proxy.WaitAccepted(2 * time.Second) {
		t.Fatal("remote target never reached the proxy")
	}
}

// TestProxiedClientRefusesLocalRedirect: a remote server reached over Tor must
// not be able to point the client at a local service, which would be dialed
// direct.
func TestProxiedClientRefusesLocalRedirect(t *testing.T) {
	req := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	remote := []*http.Request{req("https://node.example.com/")}
	if err := refuseLocalRedirect(req("http://127.0.0.1:8332/"), remote); err == nil {
		t.Fatal("redirect from a remote host to a local address was allowed")
	}
	if err := refuseLocalRedirect(req("https://other.example.com/"), remote); err != nil {
		t.Fatalf("remote to remote redirect refused: %v", err)
	}
	// A request that was local to begin with may redirect locally.
	local := []*http.Request{req("http://192.168.1.5:8332/")}
	if err := refuseLocalRedirect(req("http://127.0.0.1:8332/"), local); err != nil {
		t.Fatalf("local to local redirect refused: %v", err)
	}
}
