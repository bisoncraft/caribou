package sol

import (
	"context"
	"crypto/rand"
	"github.com/bisoncraft/caribou/util/dexnet"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/asset"
)

// TestRPCClientUsesProxy: with --torproxy set, the endpoint must be reached
// only through the proxy, or the user's IP is exposed to the RPC provider.
func TestRPCClientUsesProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	endpoint := proxycanary.New(t)
	proxy := proxycanary.New(t)

	var slot uint64
	err := newRPCClient("http://"+endpoint.Addr, proxy.Addr).call(context.Background(), "getSlot", nil, &slot)
	if err == nil {
		t.Fatal("call succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, endpoint)
}

// TestWSClientUsesProxy is the websocket counterpart: the subscription
// connection must also dial the proxy, never the endpoint.
func TestWSClientUsesProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	endpoint := proxycanary.New(t)
	proxy := proxycanary.New(t)

	c := newWSClient("ws://"+endpoint.Addr, proxy.Addr, util.Disabled)
	if err := c.session(context.Background(), func() {}); err == nil {
		t.Fatal("session succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, endpoint)
}

// TestWalletUsesConfiguredProxy covers the hop from the proxy on the wallet
// config to the rpc client the wallet builds on Connect. The client is
// proxy-aware on its own, so a wallet that dropped the setting would still
// pass the client test while contacting the provider directly.
func TestWalletUsesConfiguredProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	endpoint, proxy := proxycanary.New(t), proxycanary.New(t)

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
	w, err := (&Driver{}).Open(&asset.WalletConfig{
		Type:     walletTypeRPC,
		DataDir:  dir,
		Settings: map[string]string{"rpcprovider": "http://" + endpoint.Addr},
		TorProxy: proxy.Addr,
	}, util.Disabled, util.Simnet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Connect(context.Background()); err == nil {
		t.Fatal("Connect succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, endpoint)
}
