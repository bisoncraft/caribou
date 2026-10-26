// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build xmr

package xmr

import (
	"context"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/asset"
)

// tDaemonHost is a non-local daemon address. Loopback and private daemons are
// deliberately kept off the proxy (see dexnet.ProxyFor), so the proxied path
// can only be exercised with a hostname. It is never reached: the proxy
// canary hangs up on every connection.
const tDaemonHost = "xmr-node.example.com:18081"

func tProxiedWallet(t *testing.T, proxy *proxycanary.Listener) *ExchangeWallet {
	t.Helper()
	return tOpenWallet(t, t.TempDir(), true, func(cfg *asset.WalletConfig) {
		cfg.TorProxy = proxy.Addr
		cfg.Settings = map[string]string{"daemonaddress": tDaemonHost}
	})
}

// assertProxied fails the test unless the proxy received a connection. A
// hostname cannot be pointed at a direct canary, so a leak is not observable
// here, only the proxy being used.
func assertProxied(t *testing.T, proxy *proxycanary.Listener) {
	t.Helper()
	if !proxy.WaitAccepted(2 * time.Second) {
		t.Fatal("never connected to the configured proxy")
	}
}

// TestFeeRPCUsesProxy: the fee estimate request goes to the daemon and must
// take the proxy, or the user's IP reaches the daemon outside Tor.
func TestFeeRPCUsesProxy(t *testing.T) {
	proxy := proxycanary.New(t)
	w := tProxiedWallet(t, proxy)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.feeRPC.getFeeEstimate(ctx); err == nil {
		t.Fatal("fee estimate succeeded against a proxy that closes every connection")
	}
	assertProxied(t, proxy)
}

// TestDaemonConnectUsesProxy covers the native library's own connection to
// the daemon, which is configured separately from the fee RPC client.
func TestDaemonConnectUsesProxy(t *testing.T) {
	proxy := proxycanary.New(t)
	w := tProxiedWallet(t, proxy)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Connect is expected to fail or stall: the proxy never answers.
	if wg, err := w.Connect(ctx); err == nil {
		cancel()
		wg.Wait()
	}
	assertProxied(t, proxy)
}
