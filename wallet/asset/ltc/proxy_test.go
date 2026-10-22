//go:build !harness

package ltc

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
)

// TestElectrumWalletUsesConfiguredProxy: the proxy on the wallet config must
// reach the electrum dial, or the server sees the user's address directly.
func TestElectrumWalletUsesConfiguredProxy(t *testing.T) {
	server, proxy := proxycanary.New(t), proxycanary.New(t)

	np := assets.LTCParams["simnet"]
	orig := np.ElectrumServers
	np.ElectrumServers = []*netparams.ServerAddr{{Proto: "tcp", Addr: server.Addr}}
	t.Cleanup(func() { np.ElectrumServers = orig })

	dir := t.TempDir()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	logger := util.Disabled
	if err := (&Driver{}).Create(&asset.CreateWalletParams{
		Type: walletTypeElectrum, Seed: seed, Pass: []byte("abc"),
		DataDir: dir, Net: util.Simnet, Logger: logger,
	}); err != nil {
		t.Fatal(err)
	}
	w, err := NewWallet(&asset.WalletConfig{
		Type: walletTypeElectrum, DataDir: dir, TorProxy: proxy.Addr,
	}, logger, util.Simnet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Connect(context.Background()); err == nil {
		t.Fatal("Connect succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, server)
}
