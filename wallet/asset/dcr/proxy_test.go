//go:build !harness && !vspd

package dcr

import (
	"context"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
)

// TestSPVWalletRefusesProxy: the SPV syncer dials peers directly, so with a
// proxy configured the wallet must fail to open instead of leaking the
// user's IP to the peer network.
func TestSPVWalletRefusesProxy(t *testing.T) {
	_, err := NewWallet(&asset.WalletConfig{
		Type:     walletTypeSPV,
		DataDir:  t.TempDir(),
		Settings: map[string]string{},
		TorProxy: "127.0.0.1:9050",
	}, util.Disabled, util.Simnet)
	if err == nil || !strings.Contains(err.Error(), "--torproxy") {
		t.Fatalf("expected the SPV wallet to refuse --torproxy, got %v", err)
	}
}

// TestPoliteiaDisabledWithProxy: the upstream Politeia client cannot be
// proxied, so it must not start when a proxy is configured.
func TestPoliteiaDisabledWithProxy(t *testing.T) {
	w := &ExchangeWallet{
		log:       util.Disabled,
		network:   util.Mainnet,
		torProxy:  "127.0.0.1:9050",
		walletDir: t.TempDir(),
	}
	w.startPoliteia(context.Background())
	if w.politeia != nil {
		t.Fatal("Politeia started with --torproxy set")
	}
}
