//go:build harness

package chain

import (
	"errors"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
)

// startLiveClient connects a client to the harness electrumx with its header
// store starting at startHeight (0 for genesis; otherwise a checkpoint height
// in lc.params) and waits for the initial header sync.
func startLiveClient(t *testing.T, lc liveChain, startHeight int32) *ElectrumClient {
	t.Helper()
	net, err := electrum.NewNetwork(&electrum.Config{
		Chain: &electrum.ChainConfig{
			Chain:       parseChain(t, lc.chainSymbol),
			Params:      lc.params,
			HeaderSize:  80,
			StartHeight: startHeight,
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
			c.WaitForShutdown()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("client did not shut down")
		}
	})
	select {
	case n := <-c.Notifications():
		if _, ok := n.(ClientConnected); !ok {
			t.Fatalf("first ntfn = %T, want ClientConnected", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for ClientConnected")
	}
	if !waitForCurrent(t, c, 30*time.Second) {
		t.Fatal("IsCurrent never became true after sync")
	}
	return c
}

// TestElectrumClientLiveAnchoredSync syncs a header store that starts at a
// checkpoint 50 blocks below the harness tip, as a wallet anchored at its
// birthday's checkpoint does, and checks that electrumx serves headers from a
// mid-chain start and that heights below the start are refused locally. The
// checkpoint is taken from an unanchored sync of the same chain.
func TestElectrumClientLiveAnchoredSync(t *testing.T) {
	const depth = 50
	for _, lc := range liveChains(t) {
		t.Run(lc.name, func(t *testing.T) {
			full := startLiveClient(t, lc, 0)
			_, tip, err := full.GetBestBlock()
			if err != nil {
				t.Fatalf("GetBestBlock: %v", err)
			}
			if tip <= depth {
				t.Skipf("harness tip %d is not above %d blocks", tip, depth)
			}
			anchorHeight := tip - depth
			anchorHash, err := full.GetBlockHash(int64(anchorHeight))
			if err != nil {
				t.Fatalf("GetBlockHash(%d): %v", anchorHeight, err)
			}
			anchorHdr, err := full.GetBlockHeader(anchorHash)
			if err != nil {
				t.Fatalf("GetBlockHeader: %v", err)
			}
			full.Stop()
			full.WaitForShutdown()

			// liveChains builds fresh params per call, so the test owns
			// this instance.
			lc.params.Checkpoints = []netparams.Checkpoint{{
				Height: anchorHeight, Hash: anchorHash, Header: anchorHdr,
			}}
			c := startLiveClient(t, lc, anchorHeight)

			if got := c.EarliestBlockHeight(); got != anchorHeight {
				t.Fatalf("EarliestBlockHeight = %d, want %d", got, anchorHeight)
			}
			hash, height, err := c.GetBestBlock()
			if err != nil {
				t.Fatalf("GetBestBlock: %v", err)
			}
			if height < tip {
				t.Fatalf("anchored tip %d below the unanchored tip %d", height, tip)
			}
			t.Logf("%s: synced %d headers from anchor %d to tip %d (%s)",
				lc.name, height-anchorHeight+1, anchorHeight, height, hash)

			h, err := c.GetBlockHash(int64(anchorHeight))
			if err != nil || *h != *anchorHash {
				t.Fatalf("GetBlockHash(anchor): hash=%v err=%v", h, err)
			}
			if _, err := c.GetBlockHash(int64(anchorHeight - 1)); !errors.Is(err, ErrBeforeAnchor) {
				t.Fatalf("GetBlockHash(anchor-1) err = %v, want ErrBeforeAnchor", err)
			}
		})
	}
}
