package wallet

import (
	"testing"

	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
)

// TestElectrumBackendDialsThroughProxy guards against the proxy setting being
// dropped on the way to the electrum network: with a proxy configured, the
// first connection must go to the proxy and the electrum server must see
// none, since a direct dial would expose the user's IP.
func TestElectrumBackendDialsThroughProxy(t *testing.T) {
	server := proxycanary.New(t)
	proxy := proxycanary.New(t)

	np := assets.BTCParams["simnet"]
	orig := np.ElectrumServers
	np.ElectrumServers = []*netparams.ServerAddr{{Proto: "tcp", Addr: server.Addr}}
	t.Cleanup(func() { np.ElectrumServers = orig })

	b, err := newBackend(BackendElectrum, np, t.TempDir(), proxy.Addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The proxy hangs up without speaking SOCKS5, so Start fails once it
	// has tried every candidate.
	if err := b.Start(); err == nil {
		t.Fatal("Start succeeded against a proxy that closes every connection")
	}
	b.Shutdown()

	proxycanary.AssertViaProxy(t, proxy, server)
}

// TestNeutrinoBackendRefusesProxy: neutrino resolves peers through the system
// DNS resolver, so with a proxy configured it must refuse to start instead of
// leaking lookups.
func TestNeutrinoBackendRefusesProxy(t *testing.T) {
	if _, err := newBackend(BackendNeutrino, assets.BTCParams["simnet"], t.TempDir(), "127.0.0.1:9050", nil); err == nil {
		t.Fatal("neutrino backend started with --torproxy set")
	}
}
