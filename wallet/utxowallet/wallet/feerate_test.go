package wallet

import (
	"context"
	"errors"
	"testing"

	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/chain"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
)

// fakeFeeNetwork is an electrum network that answers only EstimateFee.
type fakeFeeNetwork struct {
	chain.ElectrumNetwork
	satsPerKB int64
	err       error
	calls     int
}

func (n *fakeFeeNetwork) EstimateFee(context.Context, int32) (int64, error) {
	n.calls++
	return n.satsPerKB, n.err
}

// fakeElectrumBackend is an electrum wallet backend that answers only
// Connected and Network.
type fakeElectrumBackend struct {
	walletBackend
	net       *fakeFeeNetwork
	connected bool
}

func (b *fakeElectrumBackend) Connected() bool                { return b.connected }
func (b *fakeElectrumBackend) Network() chain.ElectrumNetwork { return b.net }

// feeTestWallet returns a wallet configured with a proxy whose only electrum
// server is direct, a listener a proxied client must never reach.
func feeTestWallet(t *testing.T, backend walletBackend) (w *Wallet, proxy, direct *proxycanary.Listener) {
	t.Helper()
	direct = proxycanary.New(t)
	proxy = proxycanary.New(t)
	np := &netparams.ChainParams{
		GenesisHash:     assets.BTCParams["mainnet"].GenesisHash,
		ElectrumServers: []*netparams.ServerAddr{{Proto: "tcp", Addr: direct.Addr}},
	}
	w = &Wallet{chainParams: np, torProxy: proxy.Addr}
	if backend != nil {
		w.chainClient = backend
	}
	return w, proxy, direct
}

// TestFeeRateUsesWalletServer: with the electrum backend running, the
// estimate comes over its wallet-server connection and nothing is dialed.
func TestFeeRateUsesWalletServer(t *testing.T) {
	net := &fakeFeeNetwork{satsPerKB: 12_345}
	w, proxy, direct := feeTestWallet(t, &fakeElectrumBackend{net: net, connected: true})
	rate, err := w.FeeRate(t.Context())
	if err != nil {
		t.Fatalf("FeeRate: %v", err)
	}
	if rate != 13 || net.calls != 1 {
		t.Fatalf("rate %d after %d network calls, want 13 after 1", rate, net.calls)
	}
	if n := proxy.Accepted() + direct.Accepted(); n != 0 {
		t.Fatalf("%d connection(s) opened despite a running wallet server", n)
	}
}

// TestFeeRateFallbackDialsThroughProxy: whenever the estimate needs a
// short-lived connection (no backend, a backend not yet connected, a neutrino
// backend, or a wallet server that gave no estimate), a wallet configured with
// a proxy dials through it and never directly.
func TestFeeRateFallbackDialsThroughProxy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend walletBackend
	}{
		{"no backend", nil},
		{"electrum not connected", &fakeElectrumBackend{net: &fakeFeeNetwork{satsPerKB: 1000}}},
		{"wallet server failed", &fakeElectrumBackend{net: &fakeFeeNetwork{err: errors.New("no estimate")}, connected: true}},
		{"wallet server non-positive", &fakeElectrumBackend{net: &fakeFeeNetwork{satsPerKB: -1}, connected: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, proxy, direct := feeTestWallet(t, tc.backend)
			// The proxy hangs up without speaking SOCKS5, so the fallback
			// fails once it has tried the server.
			if _, err := w.FeeRate(t.Context()); err == nil {
				t.Fatal("FeeRate succeeded through a proxy that closes every connection")
			}
			proxycanary.AssertViaProxy(t, proxy, direct)
			if fb, ok := tc.backend.(*fakeElectrumBackend); ok && !fb.connected && fb.net.calls != 0 {
				t.Fatal("a backend that is not connected was asked for an estimate")
			}
		})
	}
}

// TestFeeRatePublishRace runs fee estimates while another goroutine builds,
// publishes and clears the backend the way Synchronize and Stop do. Under the
// race detector it fails if FeeRate reads the backend outside the published
// chain client.
func TestFeeRatePublishRace(t *testing.T) {
	w, _, _ := feeTestWallet(t, nil)
	backend := &fakeElectrumBackend{net: &fakeFeeNetwork{satsPerKB: 1000}, connected: true}
	stop := make(chan struct{})
	published := make(chan struct{})
	go func() {
		defer close(published)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			w.backend = backend // Synchronize builds the backend unlocked
			w.chainClientLock.Lock()
			if i%2 == 0 {
				w.chainClient = backend
			} else {
				w.chainClient = nil
			}
			w.chainClientLock.Unlock()
		}
	}()
	for range 50 {
		_, _ = w.FeeRate(t.Context())
	}
	close(stop)
	<-published
}
