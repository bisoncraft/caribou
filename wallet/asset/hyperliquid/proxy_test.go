package hyperliquid

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/asset"
)

func tProxiedWallet(t *testing.T, api, ws, proxy *proxycanary.Listener) *HyperWallet {
	t.Helper()
	dir := t.TempDir()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	if err := (&Driver{}).Create(&asset.CreateWalletParams{
		Type: "rpc", Seed: seed, Pass: seed, DataDir: dir,
		Net: util.Simnet, Logger: util.Disabled,
	}); err != nil {
		t.Fatal(err)
	}
	w, err := newWallet(&asset.WalletConfig{
		DataDir: dir,
		Settings: map[string]string{
			"apiendpoint": "http://" + api.Addr,
			"wsendpoint":  "ws://" + ws.Addr,
		},
		TorProxy: proxy.Addr,
	}, util.Disabled, util.Simnet)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// TestRPCUsesConfiguredProxy: the REST API must be reached through the
// proxy set in the wallet config, or the user's IP is exposed to the API.
func TestRPCUsesConfiguredProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	api, ws, proxy := proxycanary.New(t), proxycanary.New(t), proxycanary.New(t)
	w := tProxiedWallet(t, api, ws, proxy)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.Connect(ctx); err == nil {
		t.Fatal("Connect succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, api)
}

// TestWebsocketUsesConfiguredProxy: the ledger websocket needs the same
// proxy as the REST client.
func TestWebsocketUsesConfiguredProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	api, ws, proxy := proxycanary.New(t), proxycanary.New(t), proxycanary.New(t)
	w := tProxiedWallet(t, api, ws, proxy)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.tipPoller(ctx)
	}()
	proxycanary.AssertViaProxy(t, proxy, ws)
	cancel()
	<-done
}

// TestWebsocketDialStopsOnCancel: a proxy that accepts the connection but
// never answers the SOCKS handshake must not keep the ledger websocket from
// stopping, or Disconnect would wait on it for as long as the proxy stalls.
func TestWebsocketDialStopsOnCancel(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	stall := proxycanary.NewStalling(t)
	api, ws := proxycanary.New(t), proxycanary.New(t)
	w := tProxiedWallet(t, api, ws, stall)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.tipPoller(ctx)
	}()
	if !stall.WaitAccepted(5 * time.Second) { // the dial is now stuck in the handshake
		t.Fatal("the websocket never dialed the proxy")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tipPoller kept running after cancel while the proxy stalled the handshake")
	}
}
