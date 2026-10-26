//go:build !harness && !rpclive

package eth

import (
	"github.com/bisoncraft/caribou/util/dexnet"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/params"
)

// TestMultiRPCClientUsesProxy: both the websocket attempt and the HTTP
// fallback must dial the proxy, or the user's IP is exposed to the provider.
func TestMultiRPCClientUsesProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	endpoint, proxy := proxycanary.New(t), proxycanary.New(t)

	dir := t.TempDir()
	ks := keystore.NewKeyStore(filepath.Join(getWalletDir(dir, util.Simnet), "keystore"),
		keystore.LightScryptN, keystore.LightScryptP)
	if _, err := ks.NewAccount(string(testKeyStorePW)); err != nil {
		t.Fatal(err)
	}
	cfg := &params.ChainConfig{ChainID: big.NewInt(1337)}
	m, err := newMultiRPCClient(dir, []string{"http://" + endpoint.Addr}, tLogger, cfg, 5, util.Simnet, proxy.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.connect(t.Context()); err == nil {
		t.Fatal("connect succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, endpoint)
}

// TestWalletUsesConfiguredProxy covers the hop from the proxy on the wallet
// config to the RPC client the wallet builds on Connect. The client is
// proxy-aware on its own, so a wallet that dropped the setting would still
// pass the client test while connecting to the provider directly.
func TestWalletUsesConfiguredProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	endpoint, proxy := proxycanary.New(t), proxycanary.New(t)

	// Testnet, because simnet reads its compatibility data from harness files
	// that only exist on a machine running the dev harness.
	dir := t.TempDir()
	ks := keystore.NewKeyStore(filepath.Join(getWalletDir(dir, util.Testnet), "keystore"),
		keystore.LightScryptN, keystore.LightScryptP)
	if _, err := ks.NewAccount(string(testKeyStorePW)); err != nil {
		t.Fatal(err)
	}
	w, err := newWallet(&asset.WalletConfig{
		Type:     walletTypeRPC,
		DataDir:  dir,
		Settings: map[string]string{providersKey: "http://" + endpoint.Addr},
		TorProxy: proxy.Addr,
	}, tLogger, util.Testnet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Connect(t.Context()); err == nil {
		t.Fatal("Connect succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, endpoint)
}

// TestMultiRPCClientWebsocketEndpointUsesProxy: a ws:// provider whose
// proxied websocket attempt fails falls back to a second dial. That fallback
// must stay on the proxy too, since go-ethereum picks the transport from the
// URL scheme and a websocket URL ignores an http client.
func TestMultiRPCClientWebsocketEndpointUsesProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	endpoint, proxy := proxycanary.New(t), proxycanary.New(t)

	dir := t.TempDir()
	ks := keystore.NewKeyStore(filepath.Join(getWalletDir(dir, util.Testnet), "keystore"),
		keystore.LightScryptN, keystore.LightScryptP)
	if _, err := ks.NewAccount(string(testKeyStorePW)); err != nil {
		t.Fatal(err)
	}
	cfg := &params.ChainConfig{ChainID: big.NewInt(1337)}
	m, err := newMultiRPCClient(dir, []string{"ws://" + endpoint.Addr}, tLogger, cfg, 5, util.Testnet, proxy.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.connect(t.Context()); err == nil {
		t.Fatal("connect succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, endpoint)
}
