package xrp

import (
	"context"
	"crypto/rand"
	"github.com/bisoncraft/caribou/util/dexnet"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/asset"
)

// TestSubscriptionUsesProxy: the ledger websocket must dial the configured
// proxy, never the endpoint, or the user's IP is exposed to the provider.
func TestSubscriptionUsesProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	endpoint := proxycanary.New(t)
	proxy := proxycanary.New(t)

	w := &xrpWallet{torProxy: proxy.Addr}
	if _, err := w.runSubscription(context.Background(), "ws://"+endpoint.Addr); err == nil {
		t.Fatal("subscription succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, endpoint)
}

// TestWalletKeepsConfiguredProxy: the websocket dial reads the wallet's own
// proxy, so the wallet built from a config must carry the config's proxy or
// the ledger subscription connects directly.
func TestWalletKeepsConfiguredProxy(t *testing.T) {
	dir := t.TempDir()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	if err := (&Driver{}).Create(&asset.CreateWalletParams{
		Type: walletTypeRPC, Seed: seed, Pass: seed, DataDir: dir,
		Net: util.Simnet, Logger: util.Disabled,
	}); err != nil {
		t.Fatal(err)
	}
	w, err := newWallet(&asset.WalletConfig{
		Type: walletTypeRPC, DataDir: dir, TorProxy: "127.0.0.1:9050",
	}, util.Disabled, util.Simnet)
	if err != nil {
		t.Fatal(err)
	}
	if w.torProxy != "127.0.0.1:9050" {
		t.Fatalf("wallet proxy %q, want the configured one", w.torProxy)
	}
}
