// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

// assertDialsViaProxy dials a remote name with the configuration's Dialer
// and fails unless the connection reached the proxy. The canary hangs up on
// every connection, so the dial itself fails.
func assertDialsViaProxy(t *testing.T, cfg *zeclib.Config, proxy *proxycanary.Listener) {
	t.Helper()
	if cfg.Dialer == nil {
		t.Fatal("no Dialer with a proxy configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := cfg.Dialer(ctx, "lightwalletd.example.com:443"); err == nil {
		conn.Close()
	}
	if !proxy.WaitAccepted(2 * time.Second) {
		t.Fatal("the dial did not go through the proxy")
	}
}

// Every connection of the built-in servers goes through the proxy, the
// database Create opens and the one the wallet opens.
func TestBuiltInServersUseTheProxy(t *testing.T) {
	o := installOpener(t)
	proxy := proxycanary.New(t)
	dir := t.TempDir()
	p := createParams(dir, util.Mainnet, 0)
	p.TorProxy = proxy.Addr
	if err := (&Driver{}).Create(p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg := o.latest().cfg
	if cfg.Server != "" || cfg.Plaintext {
		t.Errorf("Create's configuration: server %q, plaintext %t", cfg.Server, cfg.Plaintext)
	}
	assertDialsViaProxy(t, cfg, proxy)

	proxy = proxycanary.New(t)
	w, err := newWallet(&asset.WalletConfig{Type: walletTypeNative, DataDir: dir, TorProxy: proxy.Addr}, nil, util.Mainnet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	if err := w.OpenWithPW(context.Background(), testPW); err != nil {
		t.Fatal(err)
	}
	assertDialsViaProxy(t, o.latest().cfg, proxy)
}

func TestCustomServer(t *testing.T) {
	const proxy = "127.0.0.1:9050"
	for _, tc := range []struct {
		server    string
		plaintext bool
		proxied   bool
	}{
		{"127.0.0.1:9067", true, false},
		{"localhost:9067", true, false},
		{"[::1]:9067", true, false},
		// A server on the local network is not proxied, and uses TLS.
		{"192.168.1.20:9067", false, false},
		{"lwd.example.com:443", false, true},
	} {
		c, err := configFor(util.Mainnet, map[string]string{settingServer: tc.server}, proxy)
		if err != nil {
			t.Fatalf("%s: %v", tc.server, err)
		}
		cfg := c.zeclibConfig("db", nil)
		if cfg.Server != tc.server || cfg.Plaintext != tc.plaintext || (cfg.Dialer != nil) != tc.proxied {
			t.Errorf("%s: server %q, plaintext %t, proxied %t", tc.server, cfg.Server, cfg.Plaintext, cfg.Dialer != nil)
		}
	}
	for _, bad := range []string{"no-port", ":9067"} {
		if _, err := configFor(util.Mainnet, map[string]string{settingServer: bad}, ""); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	// Simnet uses the harness, in plaintext, unless the setting says
	// otherwise.
	c, err := configFor(util.Simnet, nil, "")
	if err != nil || c.server != simnetServer || !c.plaintext {
		t.Errorf("simnet: %+v, %v", c, err)
	}
	// No proxy, no Dialer.
	c, _ = configFor(util.Mainnet, nil, "")
	if cfg := c.zeclibConfig("db", nil); cfg.Dialer != nil {
		t.Error("a Dialer without a proxy")
	}
}
