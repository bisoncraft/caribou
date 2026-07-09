// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

//go:build harness

// Live smoke tests against locally running electrumx servers. Spin up the
// harnesses first:
//
//	util/testing/btc/harness.sh
//	util/testing/btc/electrumx.sh
//	util/testing/ltc/harness.sh
//	util/testing/ltc/electrumx.sh
//
// Then:
//
//	go test -tags harness -run TestLive ./wallet/utxowallet/electrum/

package electrum

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	"golang.org/x/crypto/scrypt"
)

type liveCase struct {
	name   string
	chain  bisonwire.Chain
	addr   string
	params *netparams.ChainParams
}

func liveCases(t *testing.T) []liveCase {
	t.Helper()

	btcParams := regtestParams(t, &chaincfg.RegressionNetParams)
	ltcParams := ltcRegtestParams(t)

	return []liveCase{
		{
			name:   "btc",
			chain:  bisonwire.ChainBTC,
			addr:   "localhost:54002",
			params: btcParams,
		},
		{
			name:   "ltc",
			chain:  bisonwire.ChainLTC,
			addr:   "localhost:55002",
			params: ltcParams,
		},
	}
}

// regtestParams wraps a *chaincfg.Params in a *netparams.ChainParams with
// only the fields the electrum package actually reads (Chain, GenesisHash,
// Net). A genesis-height checkpoint is configured so the live sync
// exercises the checkpoint validator on a known-good hash.
func regtestParams(t *testing.T, p *chaincfg.Params) *netparams.ChainParams {
	t.Helper()
	return &netparams.ChainParams{
		Chain:        "btc",
		Net:          p.Net,
		GenesisBlock: p.GenesisBlock,
		GenesisHash:  p.GenesisHash,
		// Pin genesis as a checkpoint so the validator path runs at
		// least once during the live sync. BTC regtest's full chain
		// is too volatile for higher checkpoints.
		Checkpoints: []chaincfg.Checkpoint{{Height: 0, Hash: p.GenesisHash}},
		// Regtest doesn't retarget; record that so the retarget
		// validator skips. Without this, every 2016-block boundary
		// would expect a retarget that never happens on regtest.
		PoWNoRetargeting: true,
	}
}

// ltcRegtestParams constructs a netparams.ChainParams matching what the
// ltc harness reports. We can't import ltcsuite from this package without
// pulling in heavy dependencies, so we hard-code the regtest genesis hash.
func ltcRegtestParams(t *testing.T) *netparams.ChainParams {
	t.Helper()
	// Litecoin regtest genesis. Confirmed against the live electrumx
	// server via server.features:
	//   530827f38f93b43ed12af0b3ad25a288dc02ed74d6d7857862df51fc56c416f9
	const ltcRegtestGenesis = "530827f38f93b43ed12af0b3ad25a288dc02ed74d6d7857862df51fc56c416f9"
	h, err := chainhash.NewHashFromStr(ltcRegtestGenesis)
	if err != nil {
		t.Fatalf("decode ltc genesis: %v", err)
	}
	return &netparams.ChainParams{
		Chain:       "ltc",
		GenesisHash: h,
		PowLimit:    new(big.Int), // unused in this test
		// Litecoin PoW is scrypt(header), not SHA256d. The header
		// validation path in Headers.checkPoW delegates to this hook
		// when present, falling back to SHA256d when nil — without
		// it, regtest headers' scrypt hashes wouldn't satisfy the
		// 0x207fffff target check against their SHA256d. See
		// wallet/utxowallet/assets/ltc.go for the production
		// equivalent.
		CheckPoW: checkPoWScryptForTest,
		// Genesis-pinned checkpoint and regtest no-retarget flag for
		// the same reasons as the BTC params above.
		Checkpoints:      []chaincfg.Checkpoint{{Height: 0, Hash: h}},
		PoWNoRetargeting: true,
	}
}

// checkPoWScryptForTest is the Litecoin PoW check used by the live tests.
// It mirrors wallet/utxowallet/assets/ltc.go's checkPoWScrypt but is
// duplicated here because the electrum package can't import the asset
// package without inviting a heavy dependency cycle.
func checkPoWScryptForTest(hdr *wire.BlockHeader) error {
	var buf bytes.Buffer
	buf.Grow(wire.MaxBlockHeaderPayload)
	if err := hdr.Serialize(&buf); err != nil {
		return err
	}
	sh, err := scrypt.Key(buf.Bytes(), buf.Bytes(), 1024, 1, 1, 32)
	if err != nil {
		return err
	}
	var powHash chainhash.Hash
	copy(powHash[:], sh)
	target := blockchain.CompactToBig(hdr.Bits)
	if blockchain.HashToBig(&powHash).Cmp(target) > 0 {
		return fmt.Errorf("scrypt %x exceeds target for bits 0x%08x", powHash, hdr.Bits)
	}
	return nil
}

// useTestLogger installs a verbose btclog backend so failures include
// package-level diagnostic output.
func useTestLogger(t *testing.T) {
	t.Helper()
	backend := btclog.NewBackend(testWriter{t: t})
	logger := backend.Logger("ELEC")
	logger.SetLevel(btclog.LevelTrace)
	UseLogger(logger)
	t.Cleanup(func() { DisableLog() })
}

// testWriter routes btclog output to testing.T.Log so it shows up on failure.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

func TestLiveNodeConnect(t *testing.T) {
	useTestLogger(t)
	for _, tc := range liveCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			node, err := NewNode(ctx, &ServerAddr{Proto: "ssl", Addr: tc.addr}, "",
				tc.params.GenesisHash.String())
			if err != nil {
				t.Fatalf("NewNode: %v", err)
			}
			defer node.Close()
			tip, err := node.SubscribeHeaders(ctx)
			if err != nil {
				t.Fatalf("SubscribeHeaders: %v", err)
			}
			if tip.Height <= 0 {
				t.Fatalf("expected positive tip height, got %d", tip.Height)
			}
			if len(tip.Hex) != 160 {
				t.Fatalf("unexpected header hex length %d", len(tip.Hex))
			}
			t.Logf("%s tip = %d (server %s, proto %s)",
				tc.name, tip.Height, node.softwareVersion, node.protocolVersion)
		})
	}
}

func TestLiveNetworkSyncHeaders(t *testing.T) {
	useTestLogger(t)
	for _, tc := range liveCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			chainCfg, err := NewChainConfig(tc.chain, tc.params)
			if err != nil {
				t.Fatalf("NewChainConfig: %v", err)
			}
			net, err := NewNetwork(&Config{
				Chain:         chainCfg,
				TrustedServer: &ServerAddr{Proto: "ssl", Addr: tc.addr},
				DataDir:       t.TempDir(),
			})
			if err != nil {
				t.Fatalf("NewNetwork: %v", err)
			}
			// With the per-header PoW window in place, only the
			// trailing recentPoWWindow headers are scrypt-checked
			// for LTC; older headers fall through to retarget +
			// checkpoint validation. 30s is plenty.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := net.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer net.Stop()

			// Start now returns promptly; the initial header sync runs in
			// the background. Poll until Synced() is true or the context
			// deadline fires.
			for !net.Synced() {
				select {
				case <-ctx.Done():
					t.Fatalf("timed out waiting for header sync (tip=%d)", net.Tip())
				case <-time.After(200 * time.Millisecond):
				}
			}

			tip := net.Tip()
			if tip <= 0 {
				t.Fatalf("expected positive tip after sync, got %d", tip)
			}
			// Spot-check that genesis decodes correctly and chains.
			h0 := net.HeaderByHeight(0)
			if h0 == nil {
				t.Fatal("missing header at height 0")
			}
			if h0.PrevBlock != (chainhash.Hash{}) {
				t.Fatalf("genesis prev block should be zero, got %s", h0.PrevBlock)
			}
			// And the chain validates end-to-end.
			if err := net.Headers().VerifyAll(); err != nil {
				t.Fatalf("VerifyAll: %v", err)
			}
			t.Logf("%s synced %d headers; tip hash %s", tc.name, tip+1, net.Headers().TipHash())
		})
	}
}

func TestLiveGetTransaction(t *testing.T) {
	useTestLogger(t)
	for _, tc := range liveCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			node, err := NewNode(ctx, &ServerAddr{Proto: "ssl", Addr: tc.addr}, "",
				tc.params.GenesisHash.String())
			if err != nil {
				t.Fatalf("NewNode: %v", err)
			}
			defer node.Close()
			// The genesis header is always retrievable.
			hexHdr, err := node.BlockHeader(ctx, 0)
			if err != nil {
				t.Fatalf("BlockHeader(0): %v", err)
			}
			if len(hexHdr) != 160 {
				t.Fatalf("unexpected genesis header length %d", len(hexHdr))
			}
			// Sanity: decode the header and check it BlockHash()es to
			// the chain's known genesis.
			raw, err := hex.DecodeString(hexHdr)
			if err != nil {
				t.Fatalf("decode header hex: %v", err)
			}
			hdr := new(wire.BlockHeader)
			if err := hdr.Deserialize(bytes.NewReader(raw)); err != nil {
				t.Fatalf("deserialize header: %v", err)
			}
			got := hdr.BlockHash()
			if got != *tc.params.GenesisHash {
				t.Fatalf("%s genesis hash mismatch: got %s, want %s",
					tc.name, got, tc.params.GenesisHash)
			}
		})
	}
}

// TestLiveFailover verifies the server-pool election: with a dead candidate
// listed first, Start rotates to the live harness server, and the dead
// candidate is demoted so the next election tries the healthy one first.
func TestLiveFailover(t *testing.T) {
	useTestLogger(t)
	tc := liveCases(t)[0] // btc harness

	serverHealth.mtx.Lock()
	serverHealth.m = make(map[string]*serverRecord)
	serverHealth.mtx.Unlock()

	dead := &ServerAddr{Proto: "ssl", Addr: "127.0.0.1:1", SkipVerify: true}
	live := &ServerAddr{Proto: "ssl", Addr: tc.addr, SkipVerify: true}

	chainCfg, err := NewChainConfig(tc.chain, tc.params)
	if err != nil {
		t.Fatalf("NewChainConfig: %v", err)
	}
	net, err := NewNetwork(&Config{
		Chain:   chainCfg,
		Servers: []*ServerAddr{dead, live},
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := net.Start(ctx); err != nil {
		t.Fatalf("Start did not fail over past the dead candidate: %v", err)
	}
	defer net.Stop()

	net.mu.RLock()
	got := net.node.Addr().Addr
	net.mu.RUnlock()
	if got != live.Addr {
		t.Fatalf("elected %s, want %s", got, live.Addr)
	}

	// The dead candidate is demoted: a fresh election orders the live
	// server first.
	ordered := orderCandidates([]*ServerAddr{dead, live})
	if ordered[0].Addr != live.Addr {
		t.Fatalf("dead candidate not demoted: order %s first", ordered[0].Addr)
	}
	t.Logf("failover elected %s; dead candidate demoted", got)
}
