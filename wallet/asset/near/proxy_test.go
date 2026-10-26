package near

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

// TestWalletUsesConfiguredProxy: RPC requests must go through the proxy set
// in the wallet config, or the user's IP is exposed to the RPC provider.
func TestWalletUsesConfiguredProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	endpoint, proxy := proxycanary.New(t), proxycanary.New(t)

	dir := t.TempDir()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{"rpcprovider": "http://" + endpoint.Addr}
	if err := (&Driver{}).Create(&asset.CreateWalletParams{
		Type: walletTypeRPC, Seed: seed, Pass: seed, Settings: settings,
		DataDir: dir, Net: util.Simnet, Logger: util.Disabled,
	}); err != nil {
		t.Fatal(err)
	}
	w, err := (&Driver{}).Open(&asset.WalletConfig{
		Type: walletTypeRPC, Settings: settings, DataDir: dir,
		TorProxy: proxy.Addr,
	}, util.Disabled, util.Simnet)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.Connect(ctx); err == nil {
		t.Fatal("Connect succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, endpoint)
}
