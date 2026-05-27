//go:build !spvlive && !harness

package btc

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/broadcast"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func TestLockUnlock(t *testing.T) {
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
	node.unlockErr = tErr
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
		node.lockErr = tErr
		err = wallet.Lock()
		if err == nil {
			t.Fatalf("no error for walletlock rpc error")
		}
	}
}

func TestWithdraw(t *testing.T) {
	runRubric(t, func(t *testing.T, segwit bool, walletType string) {
		testSender(t, tWithdrawSender, segwit, walletType)
	})
}

func TestSend(t *testing.T) {
	runRubric(t, func(t *testing.T, segwit bool, walletType string) {
		testSender(t, tSendSender, segwit, walletType)
	})
}

func TestConfirmations(t *testing.T) {
	runRubric(t, testConfirmations)
}

func testConfirmations(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// coinID := make([]byte, 36)
	// copy(coinID[:32], tTxHash[:])

	_, _, pkScript, contract, _, _, _ := makeSwapContract(segwit, time.Hour*12)
	const tipHeight = 10
	const swapHeight = 2
	const spendHeight = 4
	const expConfs = tipHeight - swapHeight + 1

	tx := makeRawTx([]util.Bytes{pkScript}, []*wire.TxIn{dummyInput()})
	blockHash, swapBlock := node.addRawTx(swapHeight, tx)
	txHash := tx.TxHash()
	coinID := ToCoinID(&txHash, 0)
	// Simulate a spending transaction, and advance the tip so that the swap
	// has two confirmations.
	spendingTx := dummyTx()
	spendingTx.TxIn[0].PreviousOutPoint.Hash = txHash
	spendingBlockHash, _ := node.addRawTx(spendHeight, spendingTx)

	// Prime the blockchain
	for i := int64(1); i <= tipHeight; i++ {
		node.addRawTx(i, dummyTx())
	}

	matchTime := swapBlock.Header.Timestamp

	// Bad coin id
	_, _, err := wallet.SwapConfirmations(context.Background(), randBytes(35), contract, matchTime)
	if err == nil {
		t.Fatalf("no error for bad coin ID")
	}

	// Short path.
	txOutRes := &btcjson.GetTxOutResult{
		Confirmations: expConfs,
		BestBlock:     blockHash.String(),
	}
	node.txOutRes = txOutRes
	node.getCFilterScripts[*blockHash] = [][]byte{pkScript}
	confs, _, err := wallet.SwapConfirmations(context.Background(), coinID, contract, matchTime)
	if err != nil {
		t.Fatalf("error for gettransaction path: %v", err)
	}
	if confs != expConfs {
		t.Fatalf("confs not retrieved from gettxout path. expected %d, got %d", expConfs, confs)
	}

	// no tx output found
	node.txOutRes = nil
	node.getCFilterScripts[*blockHash] = nil
	node.getTransactionErr = tErr
	_, _, err = wallet.SwapConfirmations(context.Background(), coinID, contract, matchTime)
	if err == nil {
		t.Fatalf("no error for gettransaction error")
	}
	node.getCFilterScripts[*blockHash] = [][]byte{pkScript}
	node.getTransactionErr = nil
	txB, _ := serializeMsgTx(tx)

	node.getTransactionMap = map[string]*GetTransactionResult{
		"any": {
			BlockHash: blockHash.String(),
			Bytes:     txB,
		}}

	node.getCFilterScripts[*spendingBlockHash] = [][]byte{pkScript}
	node.walletTxSpent = true
	_, spent, err := wallet.SwapConfirmations(context.Background(), coinID, contract, matchTime)
	if err != nil {
		t.Fatalf("error for spent swap: %v", err)
	}
	if !spent {
		t.Fatalf("swap not spent")
	}
}

func TestSendEdges(t *testing.T) {
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

	node.getBlockchainInfoErr = tErr // rpc
	node.blockchainMtx.Lock()
	node.getBestBlockHashErr = tErr // spv BestBlock()
	delete(node.mainchain, 100)     // force spv to BestBlock() with no wallet block
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

func TestConfirmTransaction(t *testing.T) {
	segwit := true
	wallet, node, shutdown := tNewWallet(segwit, walletTypeRPC)
	defer shutdown()

	swapVal := toSatoshi(5)

	secret, _, _, contract, addr, _, lockTime := makeSwapContract(segwit, time.Hour*12)

	coin := NewOutput(tTxHash, 0, swapVal)
	ci := &asset.AuditInfo{
		Coin:       coin,
		Contract:   contract,
		Recipient:  addr.String(),
		Expiration: lockTime,
	}

	confirmTx := asset.NewRedeemConfTx(ci, secret)

	coinID := coin.ID()

	privBytes, _ := hex.DecodeString("b07209eec1a8fb6cfe5cb6ace36567406971a75c330db7101fb21bc679bc5330")
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	wif, err := btcutil.NewWIF(privKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error encoding wif: %v", err)
	}

	node.newAddress = tP2WPKHAddr
	node.privKeyForAddr = wif

	tests := []struct {
		name                 string
		confirmTx            *asset.ConfirmTx
		coinID               []byte
		wantErr              bool
		wantConfs            uint64
		txOutRes             *btcjson.GetTxOutResult
		getTransactionResult *GetTransactionResult
		txOutErr             error
		getTransactionErr    error
	}{{
		name:                 "ok and found",
		coinID:               coinID,
		confirmTx:            confirmTx,
		getTransactionResult: new(GetTransactionResult),
	}, {
		name:      "ok spent by someone but not sure who",
		coinID:    coinID,
		confirmTx: confirmTx,
		wantConfs: confTxFinality,
	}, {
		name:      "ok but sending new tx",
		coinID:    coinID,
		confirmTx: confirmTx,
		txOutRes:  new(btcjson.GetTxOutResult),
	}, {
		name:      "ok but sending new refund tx",
		coinID:    coinID,
		confirmTx: asset.NewRefundConfTx(coin.ID(), contract, secret),
		txOutRes:  newTxOutResult(nil, 1e8, 2),
	}, {
		name:      "decode coin error",
		confirmTx: confirmTx,
		wantErr:   true,
	}, {
		name:      "error finding contract output",
		coinID:    coinID,
		confirmTx: confirmTx,
		txOutErr:  errors.New(""),
		wantErr:   true,
	}, {
		name:              "error finding redeem tx",
		coinID:            coinID,
		confirmTx:         confirmTx,
		getTransactionErr: errors.New(""),
		wantErr:           true,
	}, {
		name:   "redeem error",
		coinID: coinID,
		confirmTx: func() *asset.ConfirmTx {
			ci := &asset.AuditInfo{
				Coin: coin,
				// Contract:   contract,
				Recipient:  addr.String(),
				Expiration: lockTime,
			}

			return asset.NewRedeemConfTx(ci, secret)
		}(),
		txOutRes: new(btcjson.GetTxOutResult),
		wantErr:  true,
	}, {
		name:      "refund error",
		coinID:    coinID,
		confirmTx: asset.NewRefundConfTx(coin.ID(), contract, secret),
		txOutRes:  new(btcjson.GetTxOutResult), // fee too low
		wantErr:   true,
	}}
	for _, test := range tests {
		wallet.redeemCache = broadcast.NewCache[*redeemCacheEntry]()
		wallet.refundCache = broadcast.NewCache[*refundCacheEntry]()
		node.txOutRes = test.txOutRes
		node.txOutErr = test.txOutErr
		node.getTransactionErr = test.getTransactionErr
		node.getTransactionMap[tTxID] = test.getTransactionResult

		status, err := wallet.ConfirmTransaction(test.coinID, test.confirmTx, 0)
		if test.wantErr {
			if err == nil {
				t.Fatalf("%q: expected error", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", test.name, err)
		}
		if status.Confs != test.wantConfs {
			t.Fatalf("%q: wanted %d confs but got %d", test.name, test.wantConfs, status.Confs)
		}
	}
}
