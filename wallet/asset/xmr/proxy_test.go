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

func tProxiedWallet(t *testing.T, daemon, proxy *proxycanary.Listener) *ExchangeWallet {
	t.Helper()
	return tOpenWallet(t, t.TempDir(), true, func(cfg *asset.WalletConfig) {
		cfg.TorProxy = proxy.Addr
		cfg.Settings = map[string]string{"daemonaddress": daemon.Addr}
	})
}

// TestFeeRPCUsesProxy: the fee estimate request goes to the daemon and must
// take the proxy, or the user's IP reaches the daemon outside Tor.
func TestFeeRPCUsesProxy(t *testing.T) {
	daemon := proxycanary.New(t)
	proxy := proxycanary.New(t)
	w := tProxiedWallet(t, daemon, proxy)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.feeRPC.getFeeEstimate(ctx); err == nil {
		t.Fatal("fee estimate succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, daemon)
}

// TestDaemonConnectUsesProxy covers the native library's own connection to
// the daemon, which is configured separately from the fee RPC client.
func TestDaemonConnectUsesProxy(t *testing.T) {
	daemon := proxycanary.New(t)
	proxy := proxycanary.New(t)
	w := tProxiedWallet(t, daemon, proxy)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Connect is expected to fail or stall: the proxy never answers.
	if wg, err := w.Connect(ctx); err == nil {
		cancel()
		wg.Wait()
	}
	proxycanary.AssertViaProxy(t, proxy, daemon)
}
