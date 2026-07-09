// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

//go:build harness

// Live integration test for chain.ElectrumClient against the running BTC
// and LTC electrumx harnesses (see wallet/utxowallet/electrum/live_test.go
// for run order).

package chain

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"golang.org/x/crypto/scrypt"
)

// checkLTCScrypt is the Litecoin PoW check used by the live chain tests.
// See wallet/utxowallet/assets/ltc.go for the production-side equivalent.
// Duplicated here to avoid a heavy asset-package import from a test file.
func checkLTCScrypt(hdr *wire.BlockHeader) error {
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

type liveChain struct {
	name        string
	addr        string
	chainSymbol string
	params      *netparams.ChainParams
}

func liveChains(t *testing.T) []liveChain {
	t.Helper()

	btcParams := &netparams.ChainParams{
		Chain:        "btc",
		Net:          chaincfg.RegressionNetParams.Net,
		GenesisBlock: chaincfg.RegressionNetParams.GenesisBlock,
		GenesisHash:  chaincfg.RegressionNetParams.GenesisHash,
		// Pin genesis as a checkpoint so the validator path runs at
		// least once during the live sync, and flag regtest's
		// no-retarget rule so the retarget validator skips the
		// 2016-block boundary checks.
		Checkpoints: []chaincfg.Checkpoint{
			{Height: 0, Hash: chaincfg.RegressionNetParams.GenesisHash},
		},
		PoWNoRetargeting: true,
	}

	ltcGenesis, err := chainhash.NewHashFromStr(
		"530827f38f93b43ed12af0b3ad25a288dc02ed74d6d7857862df51fc56c416f9")
	if err != nil {
		t.Fatalf("ltc genesis: %v", err)
	}
	ltcParams := &netparams.ChainParams{
		Chain:       "ltc",
		GenesisHash: ltcGenesis,
		// Litecoin PoW is scrypt(header), not SHA256d. Headers.checkPoW
		// delegates to this hook when present and falls back to
		// SHA256d when nil; without it the live LTC sync would reject
		// every header because its scrypt hash doesn't satisfy bits
		// 0x207fffff under the SHA256d interpretation. Mirrors
		// wallet/utxowallet/assets/ltc.go's checkPoWScrypt.
		CheckPoW: checkLTCScrypt,
		// Genesis-pinned checkpoint + regtest no-retarget flag for
		// the same reasons as btcParams above.
		Checkpoints:      []chaincfg.Checkpoint{{Height: 0, Hash: ltcGenesis}},
		PoWNoRetargeting: true,
	}

	return []liveChain{
		{name: "btc", addr: "localhost:54002", chainSymbol: "btc", params: btcParams},
		{name: "ltc", addr: "localhost:55002", chainSymbol: "ltc", params: ltcParams},
	}
}

// TestElectrumClientLiveSync drives an ElectrumClient against a running
// electrumx server end-to-end: Start (which syncs headers), GetBestBlock,
// and the standard ClientConnected → BlockConnected notification sequence
// when the wallet's view advances.
func TestElectrumClientLiveSync(t *testing.T) {
	for _, lc := range liveChains(t) {
		t.Run(lc.name, func(t *testing.T) {
			net, err := electrum.NewNetwork(&electrum.Config{
				Chain: &electrum.ChainConfig{
					Chain:       parseChain(t, lc.chainSymbol),
					Params:      lc.params,
					HeaderSize:  80,
					StartHeight: 0,
				},
				TrustedServer: &electrum.ServerAddr{Proto: "ssl", Addr: lc.addr, SkipVerify: true},
				DataDir:       t.TempDir(),
			})
			if err != nil {
				t.Fatalf("NewNetwork: %v", err)
			}

			c, err := NewElectrumClient(lc.params, net)
			if err != nil {
				t.Fatalf("NewElectrumClient: %v", err)
			}
			if err := c.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() {
				c.Stop()
				done := make(chan struct{})
				go func() {
					// Generous shutdown window: a -race run
					// after the long LTC scrypt-PoW sync can
					// take several seconds to fully drain.
					c.WaitForShutdown()
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					t.Errorf("client did not shut down")
				}
			})

			// First notification should be ClientConnected.
			select {
			case n := <-c.Notifications():
				if _, ok := n.(ClientConnected); !ok {
					t.Fatalf("first ntfn = %T, want ClientConnected", n)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timeout waiting for ClientConnected")
			}

			// Start now returns once connected and subscribed; the
			// header sync runs in the background, so poll until the
			// client reports current rather than asserting it
			// immediately.
			if !waitForCurrent(t, c, 30*time.Second) {
				t.Fatal("IsCurrent never became true after sync")
			}
			hash, height, err := c.GetBestBlock()
			if err != nil {
				t.Fatalf("GetBestBlock: %v", err)
			}
			if height <= 0 {
				t.Fatalf("height = %d", height)
			}
			t.Logf("%s tip = %d (%s)", lc.name, height, hash)

			// Header round-trip: GetBlockHash(height) must equal
			// GetBestBlock's hash; GetBlockHeader must return a
			// header whose BlockHash matches.
			h, err := c.GetBlockHash(int64(height))
			if err != nil || *h != *hash {
				t.Fatalf("GetBlockHash round-trip: hash=%v err=%v", h, err)
			}
			hdr, err := c.GetBlockHeader(hash)
			if err != nil {
				t.Fatalf("GetBlockHeader: %v", err)
			}
			if hdr.BlockHash() != *hash {
				t.Fatalf("header.BlockHash() mismatch")
			}

			// BlockStamp should be readable and consistent with the
			// current best block.
			bs, err := c.BlockStamp()
			if err != nil {
				t.Fatalf("BlockStamp: %v", err)
			}
			if bs.Height != height || bs.Hash != *hash {
				t.Fatalf("BlockStamp = (%d, %s), want (%d, %s)",
					bs.Height, bs.Hash, height, hash)
			}
		})
	}
}

// TestElectrumClientLiveRescanEmptyWallet drives Rescan with no addresses;
// the client should walk from genesis to tip emitting BlockConnected for
// every block, then RescanFinished.
func TestElectrumClientLiveRescanEmptyWallet(t *testing.T) {
	for _, lc := range liveChains(t) {
		t.Run(lc.name, func(t *testing.T) {
			net, err := electrum.NewNetwork(&electrum.Config{
				Chain: &electrum.ChainConfig{
					Chain:       parseChain(t, lc.chainSymbol),
					Params:      lc.params,
					HeaderSize:  80,
					StartHeight: 0,
				},
				TrustedServer: &electrum.ServerAddr{Proto: "ssl", Addr: lc.addr, SkipVerify: true},
				DataDir:       t.TempDir(),
			})
			if err != nil {
				t.Fatalf("NewNetwork: %v", err)
			}

			c, err := NewElectrumClient(lc.params, net)
			if err != nil {
				t.Fatalf("NewElectrumClient: %v", err)
			}
			if err := c.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() {
				c.Stop()
				c.WaitForShutdown()
			})

			// Consume the initial ClientConnected before starting
			// the rescan so the notification stream is empty.
			<-c.Notifications()

			// Start returns before the background header sync
			// completes; a rescan launched before headers exist
			// would walk an empty chain. Wait until current.
			if !waitForCurrent(t, c, 30*time.Second) {
				t.Fatal("IsCurrent never became true before rescan")
			}

			start := time.Now()
			var rescanErr error
			rescanDone := make(chan struct{})
			go func() {
				defer close(rescanDone)
				rescanErr = c.Rescan(lc.params.GenesisHash, nil, nil)
			}()

			// Drain notifications. We expect every block in the
			// chain to surface, followed by exactly one
			// RescanFinished. For the BTC harness (~9.7k blocks)
			// this is a few thousand BlockConnecteds; for LTC it's
			// a few hundred. Cap the wait generously.
			var (
				blockCount int
				finished   bool
				mu         sync.Mutex
			)
			deadline := time.After(60 * time.Second)
		loop:
			for {
				select {
				case n := <-c.Notifications():
					switch v := n.(type) {
					case BlockConnected:
						mu.Lock()
						blockCount++
						mu.Unlock()
					case *RescanFinished:
						_ = v
						finished = true
						break loop
					}
				case <-deadline:
					t.Fatalf("timeout: blocks=%d finished=%v", blockCount, finished)
				}
			}
			if !finished {
				t.Fatal("never saw RescanFinished")
			}

			select {
			case <-rescanDone:
			case <-time.After(5 * time.Second):
				t.Fatal("Rescan call did not return after finished notification")
			}
			if rescanErr != nil {
				t.Fatalf("Rescan: %v", rescanErr)
			}
			t.Logf("%s rescan finished: %d BlockConnected notifications in %s",
				lc.name, blockCount, time.Since(start))
		})
	}
}

func parseChain(t *testing.T, s string) bisonwire.Chain {
	t.Helper()
	c, err := bisonwire.ChainFromString(s)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	return c
}

// waitForCurrent polls c.IsCurrent until it reports true or the timeout
// elapses, returning whether the client became current. Start now returns
// before the background header sync finishes, so callers that need a usable
// tip must wait on this rather than assuming Start implies synced.
func waitForCurrent(t *testing.T, c *ElectrumClient, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if c.IsCurrent() {
			return true
		}
		select {
		case <-deadline:
			return c.IsCurrent()
		case <-tick.C:
		}
	}
}
