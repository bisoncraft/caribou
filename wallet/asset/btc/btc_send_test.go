//go:build !harness

package btc

import (
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func TestLockUnlock(t *testing.T) {
	t.Parallel()
	runRubric(t, testLockUnlock)
}

func testLockUnlock(t *testing.T, segwit bool, walletType string) {
	w, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	pw := []byte("pass")
	wallet := &ExchangeWalletFullNode{w, &authAddOn{w.node}}

	// just checking that the errors come through.
	err := wallet.Unlock(pw)
	if err != nil {
		t.Fatalf("unlock error: %v", err)
	}
	node.unlockErr = errTest
	err = wallet.Unlock(pw)
	if err == nil {
		t.Fatalf("no error for walletpassphrase error")
	}

	// Locking can't error on SPV.
	if walletType == walletTypeRPC {
		err = wallet.Lock()
		if err != nil {
			t.Fatalf("lock error: %v", err)
		}
		node.lockErr = errTest
		err = wallet.Lock()
		if err == nil {
			t.Fatalf("no error for walletlock rpc error")
		}
	}
}

func TestWithdraw(t *testing.T) {
	t.Parallel()
	runRubric(t, func(t *testing.T, segwit bool, walletType string) {
		testSender(t, tWithdrawSender, segwit, walletType)
	})
}

func TestSend(t *testing.T) {
	t.Parallel()
	runRubric(t, func(t *testing.T, segwit bool, walletType string) {
		testSender(t, tSendSender, segwit, walletType)
	})
}

func TestSendEdges(t *testing.T) {
	t.Parallel()
	runRubric(t, testSendEdges)
}

func testSendEdges(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	const feeRate uint64 = 3

	const swapVal = 2e8 // leaving untyped. NewTxOut wants int64

	var addr, contractAddr btcutil.Address
	var dexReqFees, dustCoverage uint64

	addr = btcAddr(segwit)

	if segwit {
		contractAddr, _ = btcutil.NewAddressWitnessScriptHash(randBytes(32), &chaincfg.MainNetParams)
		// See IsDust for the source of this dustCoverage voodoo.
		dustCoverage = (P2WPKHOutputSize + 41 + (107 / 4)) * feeRate * 3
		dexReqFees = InitTxSizeSegwit * feeRate
	} else {
		contractAddr, _ = btcutil.NewAddressScriptHash(randBytes(20), &chaincfg.MainNetParams)
		dustCoverage = (P2PKHOutputSize + 41 + 107) * feeRate * 3
		dexReqFees = InitTxSize * feeRate
	}

	pkScript, _ := txscript.PayToAddrScript(contractAddr)

	newBaseTx := func() *wire.MsgTx {
		baseTx := wire.NewMsgTx(wire.TxVersion)
		baseTx.AddTxIn(wire.NewTxIn(new(wire.OutPoint), nil, nil))
		baseTx.AddTxOut(wire.NewTxOut(swapVal, pkScript))
		return baseTx
	}

	node.signFunc = func(tx *wire.MsgTx) {
		signFunc(tx, 0, wallet.segwit)
	}

	tests := []struct {
		name      string
		funding   uint64
		expChange bool
	}{
		{
			name:    "not enough for change output",
			funding: swapVal + dexReqFees - 1,
		},
		{
			// Still dust here, but a different path.
			name:    "exactly enough for change output",
			funding: swapVal + dexReqFees,
		},
		{
			name:    "more than enough for change output but still dust",
			funding: swapVal + dexReqFees + 1,
		},
		{
			name:    "1 atom short to not be dust",
			funding: swapVal + dexReqFees + dustCoverage - 1,
		},
		{
			name:      "exactly enough to not be dust",
			funding:   swapVal + dexReqFees + dustCoverage,
			expChange: true,
		},
	}

	for _, tt := range tests {
		tx, err := wallet.sendWithReturn(newBaseTx(), addr, tt.funding, swapVal, feeRate)
		if err != nil {
			t.Fatalf("sendWithReturn error: %v", err)
		}

		if len(tx.TxOut) == 1 && tt.expChange {
			t.Fatalf("%s: no change added", tt.name)
		} else if len(tx.TxOut) == 2 && !tt.expChange {
			t.Fatalf("%s: change output added for dust. Output value = %d", tt.name, tx.TxOut[1].Value)
		}
	}
}

func TestSyncStatus(t *testing.T) {
	t.Parallel()
	runRubric(t, testSyncStatus)
}

func testSyncStatus(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// full node
	node.getBlockchainInfo = &GetBlockchainInfoResult{
		Headers: 100,
		Blocks:  99, // full node allowed to be synced when 1 block behind
	}

	// spv
	blkHash, msgBlock := node.addRawTx(100, dummyTx())
	node.birthdayTime = msgBlock.Header.Timestamp.Add(-time.Minute) // SPV, wallet birthday is passed
	node.blockchainMtx.Lock()
	node.mainchain[100] = blkHash // SPV, actually has to reach target
	node.blockchainMtx.Unlock()

	ss, err := wallet.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus error (synced expected): %v", err)
	}
	if !ss.Synced {
		t.Fatalf("synced = false")
	}
	if ss.BlockProgress() < 1 {
		t.Fatalf("progress not complete when loading last block")
	}

	node.getBlockchainInfoErr = errTest // rpc
	node.blockchainMtx.Lock()
	node.getBestBlockHashErr = errTest // spv BestBlock()
	delete(node.mainchain, 100)        // force spv to BestBlock() with no wallet block
	node.blockchainMtx.Unlock()
	_, err = wallet.SyncStatus()
	if err == nil {
		t.Fatalf("SyncStatus error not propagated")
	}
	node.getBlockchainInfoErr = nil
	node.blockchainMtx.Lock()
	node.getBestBlockHashErr = nil
	node.blockchainMtx.Unlock()

	wallet.tipAtConnect = 100
	node.getBlockchainInfo = &GetBlockchainInfoResult{
		Headers: 200,
		Blocks:  150,
	}
	node.addRawTx(150, makeRawTx([]util.Bytes{randBytes(1)}, []*wire.TxIn{dummyInput()})) // spv needs this for BestBlock
	ss, err = wallet.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus error (half-synced): %v", err)
	}
	if ss.Synced {
		t.Fatalf("synced = true for 50 blocks to go")
	}
	if ss.BlockProgress() > 0.500001 || ss.BlockProgress() < 0.4999999 {
		t.Fatalf("progress out of range. Expected 0.5, got %.2f", ss.BlockProgress())
	}
}
