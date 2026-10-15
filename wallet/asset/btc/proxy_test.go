//go:build !harness

package btc

import (
	"crypto/rand"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
)

// TestElectrumWalletUsesConfiguredProxy covers the whole chain from the proxy
// on the wallet config to the electrum dial: the config, the native wallet,
// the loader and the backend. LTC and DASH build the same native wallet from
// their own configs. A drop at any step would leave the server contacted
// directly with nothing failing.
func TestElectrumWalletUsesConfiguredProxy(t *testing.T) {
	server, proxy := proxycanary.New(t), proxycanary.New(t)

	np := assets.BTCParams["simnet"]
	orig := np.ElectrumServers
	np.ElectrumServers = []*netparams.ServerAddr{{Proto: "tcp", Addr: server.Addr}}
	t.Cleanup(func() { np.ElectrumServers = orig })

	dir := t.TempDir()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	if err := (&Driver{}).Create(&asset.CreateWalletParams{
		Type: walletTypeElectrum, Seed: seed, Pass: []byte("abc"),
		DataDir: dir, Net: util.Simnet, Logger: tLogger,
	}); err != nil {
		t.Fatal(err)
	}
	w, err := NewWallet(&asset.WalletConfig{
		Type: walletTypeElectrum, DataDir: dir, TorProxy: proxy.Addr,
	}, tLogger, util.Simnet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Connect(tCtx); err == nil {
		t.Fatal("Connect succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, server)
}
