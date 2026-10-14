package dexnet

import "testing"

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
		{"foo.localhost:8232", proxy, ""},
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
