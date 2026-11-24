// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build live

package zeclib

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/decred/base58"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

// TestPublicServers makes every call the wallet makes against each public
// server the package lists, and checks that none of them is behind the
// others or behind its own estimate of the chain, and that all report the
// same consensus branch. It is how the lists are verified before a release:
//
//	go test -tags live -run TestPublicServers -v ./wallet/asset/zec/zeclib/
//
// A server that fails is taken out of its list in servers.go.
func TestPublicServers(t *testing.T) {
	for _, tt := range []struct {
		network Network
		servers []string
		// p2pkh is the version prefix of a transparent address.
		p2pkh []byte
	}{
		{Mainnet, publicMainnetServers, []byte{0x1c, 0xb8}},
		{Testnet, publicTestnetServers, []byte{0x1d, 0x25}},
	} {
		t.Run(tt.network.String(), func(t *testing.T) {
			// An address is needed for the calls that take one. Any valid
			// one does: this is the address of a public key hash of zeros.
			payload := append(bytes.Clone(tt.p2pkh), make([]byte, 20)...)
			first := sha256.Sum256(payload)
			second := sha256.Sum256(first[:])
			address := base58.Encode(append(payload, second[:4]...))

			var mtx sync.Mutex
			tips := make(map[string]uint32)
			branches := make(map[string][]string)
			for _, server := range tt.servers {
				t.Run(server, func(t *testing.T) {
					tip, branch := checkPublicServer(t, tt.network, server, address)
					mtx.Lock()
					tips[server] = tip
					branches[branch] = append(branches[branch], server)
					mtx.Unlock()
				})
			}
			// The wallet leaves a server whose consensus branch is not the
			// one it has for the height of the server's tip. Servers that
			// disagree here cannot all be usable.
			if len(branches) > 1 {
				t.Errorf("the servers report different consensus branches: %v", branches)
			}
			var highest uint32
			for _, tip := range tips {
				highest = max(highest, tip)
			}
			for server, tip := range tips {
				if tip+10 < highest {
					t.Errorf("%s is at height %d, %d blocks behind the highest server", server, tip, highest-tip)
				}
			}
		})
	}
}

// checkPublicServer makes the wallet's calls against one server and returns
// the height of its chain tip and the consensus branch it reports.
func checkPublicServer(t *testing.T, network Network, server, address string) (uint32, string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := dialEndpoint(&Config{Network: network}, endpoint{address: server})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()

	info, err := checkServerChain(ctx, c, network)
	if err != nil {
		t.Fatalf("GetLightdInfo: %v", err)
	}
	t.Logf("%s %s, protocol %q, node %s, height %d, estimated height %d, consensus branch %s",
		info.Vendor, info.Version, info.LightwalletProtocolVersion, info.ZcashdSubversion,
		info.BlockHeight, info.EstimatedHeight, info.ConsensusBranchId)
	// The two fields that the wallet judges a server by.
	if _, err := strconv.ParseUint(info.ConsensusBranchId, 16, 32); err != nil {
		t.Errorf("consensus branch %q: %v", info.ConsensusBranchId, err)
	}
	if info.EstimatedHeight == 0 {
		t.Error("the server reports no estimated height, so the wallet cannot tell when it is behind")
	}
	if lag := serverLag(info); lag >= defaultStaleLag {
		t.Errorf("the server's node estimates the chain %d blocks ahead of its tip", lag)
	}

	latest, err := c.latestBlock(ctx)
	if err != nil {
		t.Fatalf("GetLatestBlock: %v", err)
	}
	tip := uint32(latest.Height)
	if activation := network.ironwoodActivation(); tip < activation {
		t.Fatalf("the tip is at %d, below the NU6.3 activation at %d", tip, activation)
	}

	// The last blocks, which have to connect, and a transaction from them.
	var prev *walletrpc.CompactBlock
	var txid []byte
	blocks := 0
	err = c.blockRange(ctx, tip-49, tip, func(b *walletrpc.CompactBlock) error {
		if prev != nil && (b.Height != prev.Height+1 || !bytes.Equal(b.PrevHash, prev.Hash)) {
			t.Errorf("block %d does not follow block %d", b.Height, prev.Height)
		}
		prev = b
		blocks++
		if len(b.Vtx) > 0 {
			txid = b.Vtx[0].Txid
		}
		return nil
	})
	if err != nil || blocks != 50 {
		t.Errorf("GetBlockRange: %d blocks, %v", blocks, err)
	}
	if txid == nil {
		t.Log("no transaction in the last 50 blocks to ask for")
	} else if tx, err := c.transaction(ctx, txid); err != nil || tx == nil || len(tx.Data) == 0 {
		t.Errorf("GetTransaction: %v, %v", tx, err)
	}

	if state, err := c.treeState(ctx, tip-1); err != nil || state.Height != uint64(tip-1) || state.IronwoodTree == "" {
		t.Errorf("GetTreeState: %v, %v", state, err)
	}
	if roots, err := c.subtreeRoots(ctx, walletrpc.ShieldedProtocol_ironwood, 0); err != nil {
		t.Errorf("GetSubtreeRoots: %v", err)
	} else {
		t.Logf("%d Ironwood subtree roots", len(roots))
	}

	nop := func(*walletrpc.GetAddressUtxosReply) error { return nil }
	if err := c.addressUTXOs(ctx, []string{address}, tip-10, nop); err != nil {
		t.Errorf("GetAddressUtxosStream: %v", err)
	}
	if err := c.addressTransactions(ctx, address, tip-10, tip, func(*walletrpc.RawTransaction) error { return nil }); err != nil {
		t.Errorf("GetTaddressTxids: %v", err)
	}
	return tip, info.ConsensusBranchId
}
