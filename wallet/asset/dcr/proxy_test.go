//go:build !harness && !vspd

package dcr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
)

// TestSPVWalletRefusesProxy: the SPV syncer dials peers directly, so with a
// proxy configured the wallet must refuse to connect instead of leaking the
// user's IP to the peer network. It refuses at Connect, not when opened, so
// the wallet stays loaded and its settings reachable.
func TestSPVWalletRefusesProxy(t *testing.T) {
	w := &ExchangeWallet{
		log:        util.Disabled,
		walletType: walletTypeSPV,
		torProxy:   "127.0.0.1:9050",
	}
	_, err := w.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--torproxy") {
		t.Fatalf("expected the SPV wallet to refuse --torproxy, got %v", err)
	}
}

// TestCreateSPVRefusesProxy: creation must fail before any wallet files are
// written, or they would be left behind by a wallet that can never connect.
func TestCreateSPVRefusesProxy(t *testing.T) {
	dir := t.TempDir()
	err := (&Driver{}).Create(&asset.CreateWalletParams{
		Type:     walletTypeSPV,
		Seed:     make([]byte, 32),
		DataDir:  dir,
		Net:      util.Simnet,
		TorProxy: "127.0.0.1:9050",
	})
	if err == nil || !strings.Contains(err.Error(), "--torproxy") {
		t.Fatalf("expected creation to refuse --torproxy, got %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("creation left %d entries behind", len(entries))
	}
}

// TestRPCWalletProxy is a canary for the rpcclient dial, which the outbound
// guard test cannot see: a remote dcrwallet must be reached through the
// proxy with isolation credentials, a local one directly.
func TestRPCWalletProxy(t *testing.T) {
	const proxy = "127.0.0.1:9050"
	cert := filepath.Join(t.TempDir(), "rpc.cert")
	if err := os.WriteFile(cert, []byte("cert"), 0600); err != nil {
		t.Fatal(err)
	}
	newWallet := func(listen, torProxy string) *rpcWallet {
		t.Helper()
		w, err := newRPCWallet(map[string]string{
			"rpclisten": listen,
			"rpccert":   cert,
			"username":  "user",
			"password":  "pass",
		}, util.Disabled, util.Simnet, torProxy)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}

	remote := newWallet("dcrwallet.example.com:9110", proxy)
	if remote.rpcCfg.Proxy != proxy || remote.rpcCfg.ProxyUser == "" || remote.rpcCfg.ProxyPass == "" {
		t.Fatalf("remote dcrwallet not proxied with isolation credentials: %+v", remote.rpcCfg)
	}
	if local := newWallet("127.0.0.1:9110", proxy); local.rpcCfg.Proxy != "" {
		t.Fatalf("local dcrwallet proxied: %q", local.rpcCfg.Proxy)
	}
	if none := newWallet("dcrwallet.example.com:9110", ""); none.rpcCfg.Proxy != "" {
		t.Fatalf("proxy set without --torproxy: %q", none.rpcCfg.Proxy)
	}
}

// TestPoliteiaDisabledWithProxy: the upstream Politeia client cannot be
// proxied, so it must not start when a proxy is configured.
func TestPoliteiaDisabledWithProxy(t *testing.T) {
	w := &ExchangeWallet{
		log:       util.Disabled,
		network:   util.Mainnet,
		torProxy:  "127.0.0.1:9050",
		walletDir: t.TempDir(),
	}
	w.startPoliteia(context.Background())
	if w.politeia != nil {
		t.Fatal("Politeia started with --torproxy set")
	}
}
