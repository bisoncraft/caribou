// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

//go:build livenet

// Live verification of the checkpoint anchors against public electrum
// servers. Unlike the harness tests (build tag "harness", local regtest),
// these dial real mainnet/testnet infrastructure, so they are opt-in:
//
//	go test -tags livenet -run TestAnchorHeaderSync -v ./wallet/utxowallet/electrum/
//
// Each case syncs headers from every checkpoint of the network in turn, used
// as the header store's StartHeight, to the live tip. That exercises the
// anchor base case (first header accepted by checkpoint hash, not linkage) at
// each start a wallet can be given, the retarget validation across real
// boundaries, every later checkpoint, and VerifyAll over the synced range.

package electrum

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btclog"
)

type anchorTestWriter struct{ t *testing.T }

func (w anchorTestWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

func anchorTestLogger(t *testing.T) {
	t.Helper()
	backend := btclog.NewBackend(anchorTestWriter{t: t})
	logger := backend.Logger("ELEC")
	logger.SetLevel(btclog.LevelTrace)
	UseLogger(logger)
	t.Cleanup(func() { DisableLog() })
}

func TestAnchorHeaderSync(t *testing.T) {
	anchorTestLogger(t)
	cases := []struct {
		name   string
		chain  bisonwire.Chain
		params *netparams.ChainParams
		server *netparams.ServerAddr
	}{
		// First entry of each shipped list: the test doubles as a
		// liveness check on the default election leaders.
		{"btc-mainnet", bisonwire.ChainBTC, assets.BTCParams["mainnet"],
			assets.BTCParams["mainnet"].ElectrumServers[0]},
		{"btc-testnet", bisonwire.ChainBTC, assets.BTCParams["testnet"],
			assets.BTCParams["testnet"].ElectrumServers[0]},
		{"ltc-mainnet", bisonwire.ChainLTC, assets.LTCParams["mainnet"],
			assets.LTCParams["mainnet"].ElectrumServers[0]},
		{"ltc-testnet", bisonwire.ChainLTC, assets.LTCParams["testnet"],
			assets.LTCParams["testnet"].ElectrumServers[0]},
		{"dash-mainnet", bisonwire.ChainDASH, assets.DashParams["mainnet"],
			assets.DashParams["mainnet"].ElectrumServers[0]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defaultCfg, err := NewChainConfig(tc.chain, tc.params)
			if err != nil {
				t.Fatalf("NewChainConfig: %v", err)
			}
			cps := tc.params.Checkpoints
			if len(cps) == 0 || defaultCfg.StartHeight != cps[0].Height {
				t.Fatalf("StartHeight %d not anchored to first checkpoint (%v)",
					defaultCfg.StartHeight, cps)
			}
			for _, anchor := range cps {
				t.Run(strconv.Itoa(int(anchor.Height)), func(t *testing.T) {
					chainCfg := *defaultCfg
					chainCfg.StartHeight = anchor.Height
					syncFromAnchor(t, &chainCfg, tc.server)
				})
			}
		})
	}
}

// syncFromAnchor syncs a fresh header store from chainCfg.StartHeight to the
// live tip and verifies every checkpoint at or above the start.
func syncFromAnchor(t *testing.T, chainCfg *ChainConfig, server *netparams.ServerAddr) {
	t.Helper()
	net, err := NewNetwork(&Config{
		Chain:         chainCfg,
		TrustedServer: server,
		DataDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := net.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer net.Stop()
	for !net.Synced() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out syncing from anchor (tip=%d)", net.Tip())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if got := net.StartHeight(); got != chainCfg.StartHeight {
		t.Fatalf("store starts at %d, want %d", got, chainCfg.StartHeight)
	}
	for _, cp := range chainCfg.Params.Checkpoints {
		if cp.Height < chainCfg.StartHeight {
			continue
		}
		hdr := net.HeaderByHeight(cp.Height)
		if hdr == nil {
			t.Fatalf("missing header at checkpoint %d", cp.Height)
		}
		if h := net.Headers().headerHash(hdr); h != *cp.Hash {
			t.Fatalf("header hash %s at %d, checkpoint %s", h, cp.Height, cp.Hash)
		}
	}
	if err := net.Headers().VerifyAll(); err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}
	t.Logf("synced %d headers from anchor %d to tip %d",
		net.Tip()-chainCfg.StartHeight+1, chainCfg.StartHeight, net.Tip())
}

// TestLiveObservers verifies phase-2 observer connections against public
// infrastructure: with the full shipped BTC mainnet list, Start elects a
// wallet server and brings up header-only observers on other candidates.
func TestLiveObservers(t *testing.T) {
	anchorTestLogger(t)
	params := assets.BTCParams["mainnet"]
	chainCfg, err := NewChainConfig(bisonwire.ChainBTC, params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: params.ElectrumServers,
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := net.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer net.Stop()
	deadline := time.Now().Add(90 * time.Second)
	for net.ObserverCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no observers connected within 90s")
		}
		time.Sleep(time.Second)
	}
	t.Logf("observers connected: %d", net.ObserverCount())

	// Discovery runs on the observer loop's first pass: the pool should
	// grow past the shipped seeds.
	shipped := len(params.ElectrumServers)
	deadline = time.Now().Add(60 * time.Second)
	for {
		net.pool.mtx.Lock()
		known := len(net.pool.entries)
		net.pool.mtx.Unlock()
		if known > shipped {
			t.Logf("pool grew to %d servers from %d shipped", known, shipped)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("discovery added nothing (%d known, %d shipped)", known, shipped)
		}
		time.Sleep(2 * time.Second)
	}
}
