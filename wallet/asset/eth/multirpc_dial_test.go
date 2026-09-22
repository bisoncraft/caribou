//go:build !harness && !rpclive

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// newFakeEthHTTPServer starts fakeEthNode behind a real HTTP listener (a
// loopback httptest.Server), so multirpc.go's dial machinery (connectProviders,
// connect, reconfigure, ...) can dial it by URL exactly as it would a live
// provider. connectProviders always tries a websocket upgrade first
// (forceTryWS); that attempt fails against a plain HTTP server and falls back
// to HTTP, which is what this exercises.
func newFakeEthHTTPServer(t *testing.T, node *fakeEthNode) string {
	t.Helper()
	srv := rpc.NewServer()
	if err := srv.RegisterName("eth", node); err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(func() {
		httpSrv.Close()
		srv.Stop()
	})
	return httpSrv.URL
}

func TestConnectProvidersOverHTTP(t *testing.T) {
	t.Parallel()

	t.Run("single healthy endpoint", func(t *testing.T) {
		t.Parallel()
		chainID := big.NewInt(1337)
		url := newFakeEthHTTPServer(t, newFakeEthNode(chainID))

		providers, err := connectProviders(t.Context(), []string{url}, tLogger, chainID, util.Simnet, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(providers) != 1 {
			t.Fatalf("got %d providers, want 1", len(providers))
		}
		defer providers[0].shutdown()
		if providers[0].endpointAddr != url {
			t.Errorf("endpointAddr = %q, want %q", providers[0].endpointAddr, url)
		}
		if providers[0].cachedTip() == nil {
			t.Error("a freshly connected provider should have a cached tip")
		}
	})

	t.Run("chain ID mismatch is rejected, not fatal alone", func(t *testing.T) {
		t.Parallel()
		url := newFakeEthHTTPServer(t, newFakeEthNode(big.NewInt(999)))
		// connectProviders expects chainID 1337; the node reports 999.
		_, err := connectProviders(t.Context(), []string{url}, tLogger, big.NewInt(1337), util.Simnet, "", nil)
		if err == nil {
			t.Fatal("expected an error when the only endpoint reports the wrong chain ID")
		}
	})

	t.Run("unreachable endpoint alone errors", func(t *testing.T) {
		t.Parallel()
		_, err := connectProviders(t.Context(), []string{"http://127.0.0.1:1"}, tLogger, big.NewInt(1337), util.Simnet, "", nil)
		if err == nil {
			t.Fatal("expected an error when no endpoint is reachable")
		}
	})

	t.Run("partial connect succeeds", func(t *testing.T) {
		t.Parallel()
		chainID := big.NewInt(1337)
		goodURL := newFakeEthHTTPServer(t, newFakeEthNode(chainID))

		providers, err := connectProviders(t.Context(),
			[]string{goodURL, "http://127.0.0.1:1"}, tLogger, chainID, util.Simnet, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(providers) != 1 {
			t.Fatalf("got %d providers, want 1 (only the reachable one)", len(providers))
		}
		providers[0].shutdown()
	})
}

func TestMultiRPCClientConnect(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		chainID := big.NewInt(1337)
		url := newFakeEthHTTPServer(t, newFakeEthNode(chainID))

		m := &multiRPCClient{
			log:       tLogger,
			endpoints: []string{url},
			chainID:   chainID,
			net:       util.Simnet,
			newHeadCh: make(chan *types.Header, 1),
		}
		if err := m.connect(t.Context()); err != nil {
			t.Fatal(err)
		}
		providers := m.providerList()
		if len(providers) != 1 {
			t.Fatalf("got %d providers, want 1", len(providers))
		}
		defer providers[0].shutdown()
	})

	t.Run("no reachable endpoints errors", func(t *testing.T) {
		t.Parallel()
		m := &multiRPCClient{
			log:       tLogger,
			endpoints: []string{"http://127.0.0.1:1"},
			chainID:   big.NewInt(1337),
			net:       util.Simnet,
			newHeadCh: make(chan *types.Header, 1),
		}
		if err := m.connect(t.Context()); err == nil {
			t.Fatal("expected an error when no endpoint connects")
		}
	})
}

func TestNewMultiRPCClient(t *testing.T) {
	t.Parallel()

	t.Run("keystore with one account", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		walletDir := getWalletDir(dir, util.Simnet)
		ks := keystore.NewKeyStore(filepath.Join(walletDir, "keystore"), keystore.LightScryptN, keystore.LightScryptP)
		if _, err := ks.NewAccount(string(testKeyStorePW)); err != nil {
			t.Fatal(err)
		}

		cfg := &params.ChainConfig{ChainID: big.NewInt(1337)}
		m, err := newMultiRPCClient(dir, []string{"http://127.0.0.1:1"}, tLogger, cfg, 5, util.Simnet, "")
		if err != nil {
			t.Fatal(err)
		}
		if m.chainID.Cmp(cfg.ChainID) != 0 {
			t.Errorf("chainID = %s, want %s", m.chainID, cfg.ChainID)
		}
		if m.finalizeConfs != 5 {
			t.Errorf("finalizeConfs = %d, want 5", m.finalizeConfs)
		}
		if m.creds == nil {
			t.Fatal("creds is nil")
		}
		if m.receipts.cache == nil {
			t.Fatal("receipts cache map was not initialized")
		}
	})

	t.Run("no keystore errors", func(t *testing.T) {
		t.Parallel()
		cfg := &params.ChainConfig{ChainID: big.NewInt(1337)}
		if _, err := newMultiRPCClient(t.TempDir(), nil, tLogger, cfg, 5, util.Simnet, ""); err == nil {
			t.Fatal("expected an error with no keystore present")
		}
	})
}

func TestMultiRPCClientReconfigureFull(t *testing.T) {
	t.Parallel()

	t.Run("endpoints changed, domain already marked compliant, skips the compliance suite", func(t *testing.T) {
		t.Parallel()
		chainID := big.NewInt(1337)
		url := newFakeEthHTTPServer(t, newFakeEthNode(chainID))
		d, err := domain(url)
		if err != nil {
			t.Fatal(err)
		}

		walletDir := t.TempDir()
		// createAndCheckProviders skips the full compliance suite for a
		// domain already recorded as compliant, so this endpoint doesn't
		// need to answer the compatibility checks (HeaderByHash,
		// TransactionReceipt, ChainID, getRPCTransaction, ...).
		cp, err := json.Marshal(map[string]bool{d: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(walletDir, "compliant-providers.json"), cp, 0644); err != nil {
			t.Fatal(err)
		}

		m := &multiRPCClient{
			log:       tLogger,
			chainID:   chainID,
			net:       util.Simnet,
			newHeadCh: make(chan *types.Header, 1),
		}
		if err := m.reconfigure(t.Context(), []string{url}, &CompatibilityData{}, walletDir, true); err != nil {
			t.Fatal(err)
		}
		providers := m.providerList()
		if len(providers) != 1 {
			t.Fatalf("got %d providers, want 1", len(providers))
		}
		defer providers[0].shutdown()
		if len(m.endpoints) != 1 || m.endpoints[0] != url {
			t.Errorf("endpoints = %v, want [%s]", m.endpoints, url)
		}
	})

	t.Run("unchanged endpoints is a no-op, no dialing", func(t *testing.T) {
		t.Parallel()
		endpoints := []string{"http://127.0.0.1:1"} // would fail to dial if attempted
		origProviders := []*provider{{host: "a"}}
		m := &multiRPCClient{
			log:       tLogger,
			endpoints: endpoints,
			providers: origProviders,
		}
		if err := m.reconfigure(t.Context(), endpoints, nil, t.TempDir(), true); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown endpoint runs the full compliance suite and passes", func(t *testing.T) {
		t.Parallel()
		chainID := big.NewInt(1337)
		node := newFakeEthNode(chainID)

		// A funded, real signed tx so getRPCTransaction's signature check
		// (and the tx's JSON round-trip) succeeds.
		priv, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		to := common.HexToAddress("0x1111111111111111111111111111111111111111")
		signer := types.LatestSignerForChainID(chainID)
		tx := types.MustSignNewTx(priv, signer, &types.LegacyTx{
			Nonce: 0, GasPrice: big.NewInt(1_000_000_000), Gas: 21000, To: &to, Value: big.NewInt(1),
		})
		node.seedTxAndReceipt(tx)

		url := newFakeEthHTTPServer(t, node)
		compat := &CompatibilityData{
			Addr:   common.HexToAddress("0x2222222222222222222222222222222222222222"),
			TxHash: tx.Hash(),
			// TokenAddr left zero: newCompatibilityTests skips CodeAt and
			// CallContract(balanceOf) for a zero token address, which this
			// fake node can't otherwise answer (no real ERC20 bytecode).
		}

		walletDir := t.TempDir()
		m := &multiRPCClient{
			log:       tLogger,
			chainID:   chainID,
			net:       util.Simnet,
			newHeadCh: make(chan *types.Header, 1),
		}
		if err := m.reconfigure(t.Context(), []string{url}, compat, walletDir, true); err != nil {
			t.Fatal(err)
		}
		providers := m.providerList()
		if len(providers) != 1 {
			t.Fatalf("got %d providers, want 1", len(providers))
		}
		defer providers[0].shutdown()

		// createAndCheckProviders should have persisted the now-compliant
		// domain so a later reconfigure can skip the suite.
		b, err := os.ReadFile(filepath.Join(walletDir, "compliant-providers.json"))
		if err != nil {
			t.Fatalf("compliant-providers.json was not written: %v", err)
		}
		var cp map[string]bool
		if err := json.Unmarshal(b, &cp); err != nil {
			t.Fatal(err)
		}
		d, err := domain(url)
		if err != nil {
			t.Fatal(err)
		}
		if !cp[d] {
			t.Errorf("compliant-providers.json = %v, want %q marked true", cp, d)
		}
	})

	t.Run("unknown endpoint fails compliance, allProvidersMustConnect errors", func(t *testing.T) {
		t.Parallel()
		chainID := big.NewInt(1337)
		node := newFakeEthNode(chainID)
		url := newFakeEthHTTPServer(t, node)
		// compat.TxHash has no matching tx or receipt on this node, so
		// TransactionReceipt (and getRPCTransaction) fail the suite.
		compat := &CompatibilityData{TxHash: common.HexToHash("0xdead")}

		m := &multiRPCClient{
			log:       tLogger,
			chainID:   chainID,
			net:       util.Simnet,
			newHeadCh: make(chan *types.Header, 1),
		}
		// reconfigure's defaultProviders arg is negated before it reaches
		// checkProvidersCompliance as allProvidersMustConnect, so false is
		// what makes a single failing test fatal here.
		if err := m.reconfigure(t.Context(), []string{url}, compat, t.TempDir(), false); err == nil {
			t.Fatal("expected an error when the only endpoint fails compliance and allProvidersMustConnect is set")
		}
	})
}

// A short sanity check that the fake node's HTTP round trip doesn't hang past
// a small timeout, so a broken fixture fails fast instead of stalling CI.
func TestFakeEthHTTPServerRespondsPromptly(t *testing.T) {
	t.Parallel()
	url := newFakeEthHTTPServer(t, newFakeEthNode(big.NewInt(1337)))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	providers, err := connectProviders(ctx, []string{url}, tLogger, big.NewInt(1337), util.Simnet, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	providers[0].shutdown()
}
