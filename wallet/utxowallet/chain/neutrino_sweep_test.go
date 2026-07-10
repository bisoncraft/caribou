// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"container/list"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/netparams"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv/headerfs"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/gcs"
	"github.com/btcsuite/btcd/btcutil/gcs/builder"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// sweepChainService serves one block (with a real cfilter) at a target height
// for the sweep, and stubs the rest via the shared mock.
type sweepChainService struct {
	*mockChainService
	tip        int32
	hashAt     map[int32]chainhash.Hash
	filterAt   map[chainhash.Hash]*gcs.Filter
	blockByHsh map[chainhash.Hash]*wire.MsgBlock
}

func (s *sweepChainService) BestBlock() (*headerfs.BlockStamp, error) {
	return &headerfs.BlockStamp{Height: s.tip}, nil
}
func (s *sweepChainService) GetBlockHash(h int64) (*chainhash.Hash, error) {
	hash := s.hashAt[int32(h)]
	return &hash, nil
}
func (s *sweepChainService) GetCFilter(h chainhash.Hash, _ wire.FilterType,
	_ ...spv.QueryOption) (*gcs.Filter, error) {
	return s.filterAt[h], nil
}
func (s *sweepChainService) GetBlock(h chainhash.Hash,
	_ ...spv.QueryOption) (*bisonwire.BlockWithHeight, error) {
	blk := s.blockByHsh[h]
	return &bisonwire.BlockWithHeight{
		Block:  bisonwire.BlockFromMsgBlock("btc", blk),
		Height: uint32(s.tip),
	}, nil
}

func TestNeutrinoSweep(t *testing.T) {
	old := sweepBlockDelay
	sweepBlockDelay = 0
	t.Cleanup(func() { sweepBlockDelay = old })

	// Cold address to sweep for, and its script.
	var h20 [20]byte
	h20[0] = 0xab
	addr, err := btcutil.NewAddressWitnessPubKeyHash(h20[:], &chaincfg.RegressionNetParams)
	if err != nil {
		t.Fatal(err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatal(err)
	}

	// A block at height 100 with a tx paying the cold address, and a real
	// basic cfilter over it so MatchAny hits.
	pay := wire.NewMsgTx(2)
	pay.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 0}, nil, nil))
	pay.AddTxOut(wire.NewTxOut(1e8, script))
	block := &wire.MsgBlock{
		Header:       wire.BlockHeader{Version: 4, Timestamp: time.Unix(1_700_000_000, 0)},
		Transactions: []*wire.MsgTx{pay},
	}
	hash := block.BlockHash()
	filter, err := builder.BuildBasicFilter(block, nil)
	if err != nil {
		t.Fatal(err)
	}

	cs := &sweepChainService{
		mockChainService: &mockChainService{},
		tip:              100,
		hashAt:           map[int32]chainhash.Hash{100: hash},
		filterAt:         map[chainhash.Hash]*gcs.Filter{hash: filter},
		blockByHsh:       map[chainhash.Hash]*wire.MsgBlock{hash: block},
	}

	client := &NeutrinoClient{
		CS:                  cs,
		chainParams:         &netparams.ChainParams{Chain: "btc"},
		enqueueNotification: make(chan interface{}, 4),
		quit:                make(chan struct{}),
		scanning:            true, // so re-promotion routes through rescan.Update
		rescan:              &mockRescanner{updateArgs: list.New()},
	}

	hits, failed, err := client.Sweep([]btcutil.Address{addr}, 99)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if failed != nil {
		t.Fatalf("failed = %v, want nil (all-or-nothing)", failed)
	}
	if len(hits) != 1 || hits[0].String() != addr.String() {
		t.Fatalf("hits = %v, want [%s]", hits, addr)
	}

	// The paying tx was emitted as a FilteredBlockConnected.
	select {
	case n := <-client.enqueueNotification:
		fb, ok := n.(FilteredBlockConnected)
		if !ok {
			t.Fatalf("notification type %T, want FilteredBlockConnected", n)
		}
		if fb.Block.Height != 100 || len(fb.RelevantTxs) != 1 {
			t.Fatalf("emitted block = %+v", fb)
		}
	default:
		t.Fatal("no FilteredBlockConnected emitted for the swept payment")
	}

	// The hit was re-promoted via a rescan Update(AddAddrs).
	if got := client.rescan.(*mockRescanner).updateArgs.Len(); got != 1 {
		t.Fatalf("rescan updates = %d, want 1 (re-promotion)", got)
	}
}

func TestNeutrinoUnwatchNotScanning(t *testing.T) {
	client := newMockNeutrinoClient()
	// scanning is false: nothing is watched, so Unwatch is a no-op and must
	// not touch the (nil) rescan.
	if err := client.Unwatch([]btcutil.Address{}); err != nil {
		t.Fatalf("Unwatch(not scanning) = %v, want nil", err)
	}
}
