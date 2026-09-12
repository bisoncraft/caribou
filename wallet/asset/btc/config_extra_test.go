//go:build !harness

package btc

import (
	"testing"

	"github.com/bisoncraft/caribou/util"
)

func TestRPCConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  RPCConfig
		want string
	}{
		{"defaults", RPCConfig{RPCUser: "u", RPCPass: "p"}, "127.0.0.1:8332"},
		{"bind port wins", RPCConfig{RPCUser: "u", RPCPass: "p", RPCBind: "host:9999", RPCPort: 1}, "host:9999"},
		{"connect host", RPCConfig{RPCUser: "u", RPCPass: "p", RPCConnect: "node", RPCPort: 1234}, "node:1234"},
		{"public provider", RPCConfig{RPCBind: "https://provider.example/path"}, "provider.example/path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := test.cfg
			if err := CheckRPCConfig(&cfg, "btc", util.Mainnet, RPCPorts); err != nil {
				t.Fatal(err)
			}
			if cfg.RPCBind != test.want {
				t.Fatalf("RPCBind = %q, want %q", cfg.RPCBind, test.want)
			}
		})
	}
	for _, cfg := range []*RPCConfig{{}, {RPCUser: "u"}} {
		if err := CheckRPCConfig(cfg, "btc", util.Mainnet, RPCPorts); err == nil {
			t.Fatal("expected missing credential error")
		}
	}
	if err := CheckRPCConfig(&RPCConfig{}, "btc", util.Network(99), RPCPorts); err == nil {
		t.Fatal("expected unknown network error")
	}
}
