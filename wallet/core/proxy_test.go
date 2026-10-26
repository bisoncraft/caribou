//go:build !harness && !botlive

package core

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/bisoncraft/caribou/util/testing/proxycanary"
	"github.com/bisoncraft/caribou/wallet/asset"
)

// TestIntentsEngineUsesConfiguredProxy: the proxy configured on Core must
// reach the 1Click client, which is built separately from the wallets. The
// catalog fetch on opening an asset view would otherwise expose the user's
// IP to the service.
func TestIntentsEngineUsesConfiguredProxy(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	service, proxy := proxycanary.New(t), proxycanary.New(t)

	rig := newTestRig()
	defer rig.shutdown()
	c := rig.core
	c.net = util.Simnet // mainnet ignores a base URL override
	c.cfg.IntentsBaseURL = "http://" + service.Addr
	c.cfg.TorProxy = proxy.Addr

	c.startIntentsEngine(c.ctx)
	var eng *intentsEngine
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if eng, err = c.intentsEngine(); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if eng == nil {
		t.Fatal("intents engine did not start")
	}

	if _, err := eng.client.Tokens(context.Background()); err == nil {
		t.Fatal("Tokens succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, service)
}

// tProxyDriver is a seeded driver that records the proxy Core hands each
// asset. The wallets are built behind this interface, so a Core entry point
// that forgot to pass the setting would leave a proxy-aware asset connecting
// directly with no error anywhere.
type tProxyDriver struct {
	*tSeededDriver
	created []string // CreateWalletParams.TorProxy, one per Create
	opened  []string // WalletConfig.TorProxy, one per Open
}

func (d *tProxyDriver) Create(params *asset.CreateWalletParams) error {
	d.created = append(d.created, params.TorProxy)
	return d.tSeededDriver.Create(params)
}

func (d *tProxyDriver) Open(cfg *asset.WalletConfig, log util.Logger, net util.Network) (asset.Wallet, error) {
	d.opened = append(d.opened, cfg.TorProxy)
	return d.tSeededDriver.Open(cfg, log, net)
}

// tRecoverable lets RecoverWallet run against the test wallet.
type tRecoverable struct{ *TXCWallet }

func (*tRecoverable) GetRecoveryCfg() (map[string]string, error) { return nil, nil }
func (*tRecoverable) Move(string) error                          { return nil }

// tReconfigRecorder is a live-reconfigurable wallet that keeps the config
// Core passes to Reconfigure.
type tReconfigRecorder struct {
	*TXCWallet
	got *asset.WalletConfig
}

func (r *tReconfigRecorder) Reconfigure(_ context.Context, cfg *asset.WalletConfig, _ string) (bool, error) {
	r.got = cfg
	return false, nil
}

// TestCoreHandsProxyToAssets pins every place Core builds an asset wallet:
// creating a seeded wallet, recovering one, and reconfiguring one live. Each
// must carry the proxy configured on Core.
func TestCoreHandsProxyToAssets(t *testing.T) {
	const proxy = "127.0.0.1:9050"

	rig := newTestRig()
	defer rig.shutdown()
	c := rig.core
	c.cfg.DBPath = filepath.Join(t.TempDir(), "core.db")
	c.cfg.TorProxy = proxy

	a := *tUTXOAssetA
	tAsset := &a
	tAsset.Symbol = "ppc"
	tAsset.ID, _ = util.BipSymbolID(tAsset.Symbol)

	winfo := *tWalletInfo
	winfo.AvailableWallets = []*asset.WalletDefinition{{Type: "type", Seeded: true}}

	_, raw := newTWallet(tAsset.ID)
	recoverable := &tRecoverable{TXCWallet: raw}
	drv := &tProxyDriver{tSeededDriver: &tSeededDriver{tDriver: &tDriver{
		wallet:        recoverable,
		decodedCoinID: "ppc-coin",
		winfo:         &winfo,
	}}}
	asset.Register(tAsset.ID, drv, true)

	form := &WalletForm{AssetID: tAsset.ID, Config: map[string]string{}, Type: "type"}

	assertAll := func(tag string, got []string, wantN int) {
		t.Helper()
		if len(got) != wantN {
			t.Fatalf("%s: %d calls, want %d", tag, len(got), wantN)
		}
		for i, p := range got {
			if p != proxy {
				t.Fatalf("%s call %d: proxy %q, want %q", tag, i, p, proxy)
			}
		}
	}

	if err := c.CreateWallet(tPW, nil, form); err != nil {
		t.Fatalf("CreateWallet: %v", err)
	}
	assertAll("create", drv.created, 1)
	if len(drv.opened) == 0 {
		t.Fatal("CreateWallet never opened the wallet")
	}
	assertAll("open after create", drv.opened, len(drv.opened))

	opened := len(drv.opened)
	if err := c.RecoverWallet(tAsset.ID, tPW, true); err != nil {
		t.Fatalf("RecoverWallet: %v", err)
	}
	assertAll("create on recover", drv.created, 2)
	if len(drv.opened) == opened {
		t.Fatal("RecoverWallet never reopened the wallet")
	}
	assertAll("open after recover", drv.opened, len(drv.opened))

	live, ok := c.wallet(tAsset.ID)
	if !ok {
		t.Fatal("no wallet after recover")
	}
	rec := &tReconfigRecorder{TXCWallet: raw}
	live.Wallet = rec
	if err := c.ReconfigureWallet(tPW, nil, form); err != nil {
		t.Fatalf("ReconfigureWallet: %v", err)
	}
	if rec.got == nil {
		t.Fatal("live reconfigure was not used")
	}
	if rec.got.TorProxy != proxy {
		t.Fatalf("live reconfigure proxy %q, want %q", rec.got.TorProxy, proxy)
	}
}

// TestNewProxiesSharedClient: Core.New is what applies --torproxy to
// dexnet.Client, which fee sources, fiat rates, DCR, the bridges and XRP rpc
// all use. Without it they would keep reaching their servers directly.
//
// SetProxy applies once per process and nothing else in this package calls
// it, so the test installs a fresh client for New to modify and restores the
// original afterwards.
func TestNewProxiesSharedClient(t *testing.T) {
	dexnet.ProxyLocalForTest(t)
	server, proxy := proxycanary.New(t), proxycanary.New(t)

	old := dexnet.Client
	dexnet.Client = &http.Client{Timeout: old.Timeout}
	t.Cleanup(func() { dexnet.Client = old })

	c, err := New(&Config{
		DBPath:   filepath.Join(t.TempDir(), "core.db"),
		Net:      util.Simnet,
		Logger:   tLogger,
		TorProxy: proxy.Addr,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.db.Close()
		c.txDB.Close()
	})

	if resp, err := dexnet.Client.Get("http://" + server.Addr); err == nil {
		resp.Body.Close()
		t.Fatal("request succeeded against a proxy that closes every connection")
	}
	proxycanary.AssertViaProxy(t, proxy, server)
}

// TestNewRejectsHostnameTorProxy: wallet2 parses the proxy as an IP and port,
// so a hostname would only fail later inside the XMR wallet. New refuses it up
// front instead.
func TestNewRejectsHostnameTorProxy(t *testing.T) {
	for _, proxy := range []string{"localhost:9050", "tor:9050"} {
		_, err := New(&Config{
			DBPath:   filepath.Join(t.TempDir(), "core.db"),
			Net:      util.Simnet,
			Logger:   tLogger,
			TorProxy: proxy,
		})
		if err == nil || !strings.Contains(err.Error(), "must be an IP address") {
			t.Fatalf("New(%q) error = %v, want an IP address error", proxy, err)
		}
	}
}
