//go:build !spvlive && !harness

package btc

import (
	"bytes"
	"encoding/hex"
	"sort"
	"strings"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/calc"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func TestFundMultiOrder(t *testing.T) {
	runRubric(t, testFundMultiOrder)
}

func testFundMultiOrder(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	maxFeeRate := uint64(200)
	feeSuggestion := uint64(100)

	txIDs := make([]string, 0, 5)
	txHashes := make([]*chainhash.Hash, 0, 5)

	addresses_legacy := []string{
		"n235HrCqx9EcS7teHcJEAthoBF5gvtrAoy",
		"mfjtHyu163DW5ZJXRHkY6kMLtHyWHTH6Qx",
		"mi5QmGr9KwVLM2WVNB9hwSS7KhUx8uNQqU",
		"mhMda8yTy52Avowe34ufbq3D59VoG7jUsy",
		"mhgZ41MUC6EBevkwnhvetkLbBmb61F7Lyr",
	}
	scriptPubKeys_legacy := []util.Bytes{
		decodeString("76a914e114d5bb20cdbd75f3726f27c10423eb1332576288ac"),
		decodeString("76a91402721143370117f4b37146f6688862892f272a7b88ac"),
		decodeString("76a9141c13a8666a373c29a8a8a270b56816bb43a395e888ac"),
		decodeString("76a914142ce1063712235182613794d6759f62dea8205a88ac"),
		decodeString("76a91417c10273e4236592fd91f3aec1571810bbb0db6888ac"),
	}

	addresses_segwit := []string{
		"bcrt1qy7agjj62epx0ydnqskgwlcfwu52xjtpj36hr0d",
		"bcrt1qhyflz52jwha67dvg92q92k887emxu6jj0ytrrd",
		"bcrt1qus3827tpu7uhreq3f3x5wmfmpgmkd0y7zhjdrs",
		"bcrt1q4fjzhnum2krkurhg55xtadzyf76f8waqj26d0e",
		"bcrt1qqnl9fhnms6gpmlmrwnsq36h4rqxwd3m4plkcw4",
	}
	scriptPubKeys_segwit := []util.Bytes{
		decodeString("001427ba894b4ac84cf236608590efe12ee514692c32"),
		decodeString("0014b913f1515275fbaf35882a805558e7f6766e6a52"),
		decodeString("0014e422757961e7b971e4114c4d476d3b0a3766bc9e"),
		decodeString("0014aa642bcf9b55876e0ee8a50cbeb4444fb493bba0"),
		decodeString("001404fe54de7b86901dff6374e008eaf5180ce6c775"),
	}
	for i := 0; i < 5; i++ {
		txIDs = append(txIDs, hex.EncodeToString(encode.RandomBytes(32)))
		h, _ := chainhash.NewHashFromStr(txIDs[i])
		txHashes = append(txHashes, h)
	}

	addresses := func(i int) string {
		if segwit {
			return addresses_segwit[i]
		}
		return addresses_legacy[i]
	}

	scriptPubKeys := func(i int) util.Bytes {
		if segwit {
			return scriptPubKeys_segwit[i]
		}
		return scriptPubKeys_legacy[i]
	}

	addrStr := tP2PKHAddr
	if segwit {
		addrStr = tP2WPKHAddr
	}
	node.newAddress = addrStr
	node.changeAddr = addrStr
	node.signFunc = func(tx *wire.MsgTx) {
		signFunc(tx, 0, wallet.segwit)
	}

	expectedSplitFee := func(numInputs, numOutputs uint64) uint64 {
		var inputSize, outputSize uint64
		if segwit {
			inputSize = RedeemP2WPKHInputTotalSize
			outputSize = P2WPKHOutputSize
		} else {
			inputSize = RedeemP2PKHInputSize
			outputSize = P2PKHOutputSize
		}

		return (MinimumTxOverhead + numInputs*inputSize + numOutputs*outputSize) * feeSuggestion
	}

	requiredForOrder := func(value, maxSwapCount uint64) int64 {
		var inputSize uint64
		if segwit {
			inputSize = RedeemP2WPKHInputTotalSize
		} else {
			inputSize = RedeemP2PKHInputSize
		}
		return int64(calc.RequiredOrderFunds(value, inputSize, maxSwapCount,
			wallet.initTxSizeBase, wallet.initTxSize, maxFeeRate))
	}

	type test struct {
		name         string
		multiOrder   *asset.MultiOrder
		allOrNothing bool
		maxLock      uint64
		utxos        []*ListUnspentResult
		bondReserves uint64
		balance      uint64

		// if expectedCoins is nil, all the coins are from
		// the split output. If any of the coins are nil,
		// than that output is from the split output.
		expectedCoins         []asset.Coins
		expectedRedeemScripts [][]util.Bytes
		expectSendRawTx       bool
		expectedSplitFee      uint64
		expectedInputs        []*wire.TxIn
		expectedOutputs       []*wire.TxOut
		expectedChange        uint64
		expectedLockedCoins   []*RPCOutpoint
		expectErr             bool
	}

	tests := []*test{
		{ // "split not allowed, utxos like split previously done"
			name: "split not allowed, utxos like split previously done",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        1e6,
						MaxSwapCount: 1,
					},
					{
						Value:        2e6,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					"swapsplit": "false",
				},
			},
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        19e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[1],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(1),
					Address:       addresses(1),
					Amount:        35e5 / 1e8,
					Vout:          0,
				},
			},
			balance: 35e5,
			expectedCoins: []asset.Coins{
				{NewOutput(txHashes[0], 0, 19e5)},
				{NewOutput(txHashes[1], 0, 35e5)},
			},
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
				{nil},
			},
		},
		{ // "split not allowed, require multiple utxos per order"
			name: "split not allowed, require multiple utxos per order",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        1e6,
						MaxSwapCount: 1,
					},
					{
						Value:        2e6,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					"swapsplit": "false",
				},
			},
			allOrNothing: true,
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        6e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[1],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(1),
					Address:       addresses(1),
					Amount:        5e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[2],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(2),
					Address:       addresses(2),
					Amount:        22e5 / 1e8,
					Vout:          0,
				},
			},
			balance: 33e5,
			expectedCoins: []asset.Coins{
				{NewOutput(txHashes[0], 0, 6e5), NewOutput(txHashes[1], 0, 5e5)},
				{NewOutput(txHashes[2], 0, 22e5)},
			},
			expectedRedeemScripts: [][]util.Bytes{
				{nil, nil},
				{nil},
			},
			expectedLockedCoins: []*RPCOutpoint{
				{txHashes[0].String(), 0},
				{txHashes[1].String(), 0},
				{txHashes[2].String(), 0},
			},
		},
		{ // "split not allowed, can only fund first order and respect maxLock"
			name: "split not allowed, can only fund first order and respect maxLock",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        1e6,
						MaxSwapCount: 1,
					},
					{
						Value:        2e6,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					"swapsplit": "false",
				},
			},
			maxLock: 32e5,
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[2],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(2),
					Address:       addresses(2),
					Amount:        1e6 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        11e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[1],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(1),
					Address:       addresses(1),
					Amount:        25e5 / 1e8,
					Vout:          0,
				},
			},
			balance: 46e5,
			expectedCoins: []asset.Coins{
				{NewOutput(txHashes[0], 0, 11e5)},
			},
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
			},
			expectedLockedCoins: []*RPCOutpoint{
				{txHashes[0].String(), 0},
			},
		},
		{ // "split not allowed, can only fund first order and respect bond reserves"
			name: "no split allowed, can only fund first order and respect bond reserves",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        1e6,
						MaxSwapCount: 1,
					},
					{
						Value:        2e6,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "false",
				},
			},
			maxLock:      46e5,
			bondReserves: 12e5,
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[2],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(2),
					Address:       addresses(2),
					Amount:        1e6 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        11e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[1],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(1),
					Address:       addresses(1),
					Amount:        25e5 / 1e8,
					Vout:          0,
				},
			},
			balance: 46e5,
			expectedCoins: []asset.Coins{
				{NewOutput(txHashes[0], 0, 11e5)},
			},
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
			},
			expectedLockedCoins: []*RPCOutpoint{
				{txHashes[0].String(), 0},
			},
		},
		{ // "split not allowed, need to fund in increasing order"
			name: "no split, need to fund in increasing order",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        2e6,
						MaxSwapCount: 2,
					},
					{
						Value:        11e5,
						MaxSwapCount: 1,
					},
					{
						Value:        9e5,
						MaxSwapCount: 1,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "false",
				},
			},
			maxLock: 50e5,
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        11e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[1],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(1),
					Address:       addresses(1),
					Amount:        13e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[2],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(2),
					Address:       addresses(2),
					Amount:        26e5 / 1e8,
					Vout:          0,
				},
			},
			balance: 50e5,
			expectedCoins: []asset.Coins{
				{NewOutput(txHashes[2], 0, 26e5)},
				{NewOutput(txHashes[1], 0, 13e5)},
				{NewOutput(txHashes[0], 0, 11e5)},
			},
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
				{nil},
				{nil},
			},
			expectedLockedCoins: []*RPCOutpoint{
				{txHashes[0].String(), 0},
				{txHashes[1].String(), 0},
				{txHashes[2].String(), 0},
			},
		},
		{ // "split allowed, no split required"
			name: "split allowed, no split required",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        1e6,
						MaxSwapCount: 1,
					},
					{
						Value:        2e6,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "true",
				},
			},
			allOrNothing: false,
			maxLock:      43e5,
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[2],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(2),
					Address:       addresses(2),
					Amount:        1e6 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        11e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[1],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(1),
					Address:       addresses(1),
					Amount:        22e5 / 1e8,
					Vout:          0,
				},
			},
			balance: 43e5,
			expectedCoins: []asset.Coins{
				{NewOutput(txHashes[0], 0, 11e5)},
				{NewOutput(txHashes[1], 0, 22e5)},
			},
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
				{nil},
			},
			expectedLockedCoins: []*RPCOutpoint{
				{txHashes[0].String(), 0},
				{txHashes[1].String(), 0},
			},
		},
		{ // "split allowed, can fund both with split"
			name: "split allowed, can fund both with split",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "true",
				},
			},
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        1e6 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[1],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(1),
					Address:       addresses(1),
					Amount:        (2*float64(requiredForOrder(15e5, 2)) + float64(expectedSplitFee(2, 2)) - 1e6) / 1e8,
					Vout:          0,
				},
			},
			maxLock:         2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(2, 2),
			balance:         2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(2, 2),
			expectSendRawTx: true,
			expectedInputs: []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Hash:  *txHashes[1],
						Index: 0,
					},
				},
				{
					PreviousOutPoint: wire.OutPoint{
						Hash:  *txHashes[0],
						Index: 0,
					},
				},
			},
			expectedOutputs: []*wire.TxOut{
				wire.NewTxOut(requiredForOrder(15e5, 2), []byte{}),
				wire.NewTxOut(requiredForOrder(15e5, 2), []byte{}),
			},
			expectedSplitFee: expectedSplitFee(2, 2),
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
				{nil},
			},
		},
		{ // "split allowed, cannot fund both with split"
			name: "split allowed, cannot fund both with split",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "true",
				},
			},
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        1e6 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[1],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(1),
					Address:       addresses(1),
					Amount:        (2*float64(requiredForOrder(15e5, 2)) + float64(expectedSplitFee(2, 2)) - 1e6) / 1e8,
					Vout:          0,
				},
			},
			maxLock:   2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(2, 2) - 1,
			balance:   2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(2, 2) - 1,
			expectErr: true,
		},
		{ // "can fund both with split and respect maxLock"
			name: "can fund both with split and respect maxLock",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "true",
				},
			},
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        float64(50e5) / 1e8,
					Vout:          0,
				},
			},
			balance:         50e5,
			maxLock:         2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(1, 2),
			expectSendRawTx: true,
			expectedInputs: []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Hash:  *txHashes[0],
						Index: 0,
					},
				},
			},
			expectedOutputs: []*wire.TxOut{
				wire.NewTxOut(requiredForOrder(15e5, 2), []byte{}),
				wire.NewTxOut(requiredForOrder(15e5, 2), []byte{}),
			},
			expectedChange:   50e5 - (2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(1, 3)),
			expectedSplitFee: expectedSplitFee(1, 3),
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
				{nil},
			},
		},
		{ // "cannot fund both with split and respect maxLock"
			name: "cannot fund both with split and respect maxLock",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "true",
				},
			},
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        float64(50e5) / 1e8,
					Vout:          0,
				},
			},
			balance:   50e5,
			maxLock:   2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(1, 2) - 1,
			expectErr: true,
		},
		{ // "split allowed, can fund both with split with bond reserves"
			name: "split allowed, can fund both with split with bond reserves",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "true",
				},
			},
			bondReserves: 2e6,
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        (2*float64(requiredForOrder(15e5, 2)) + 2e6 + float64(expectedSplitFee(1, 3))) / 1e8,
					Vout:          0,
				},
			},
			balance:         2e6 + 2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(1, 3),
			maxLock:         2e6 + 2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(1, 3),
			expectSendRawTx: true,
			expectedInputs: []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Hash:  *txHashes[0],
						Index: 0,
					},
				},
			},
			expectedOutputs: []*wire.TxOut{
				wire.NewTxOut(requiredForOrder(15e5, 2), []byte{}),
				wire.NewTxOut(requiredForOrder(15e5, 2), []byte{}),
			},
			expectedChange:   2e6,
			expectedSplitFee: expectedSplitFee(1, 3),
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
				{nil},
			},
		},
		{ // "split allowed, cannot fund both with split and keep and bond reserves"
			name: "split allowed, cannot fund both with split and keep and bond reserves",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "true",
				},
			},
			bondReserves: 2e6,
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        ((2*float64(requiredForOrder(15e5, 2)) + 2e6 + float64(expectedSplitFee(1, 3))) / 1e8) - 1/1e8,
					Vout:          0,
				},
			},
			balance:   2e6 + 2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(1, 3) - 1,
			maxLock:   2e6 + 2*uint64(requiredForOrder(15e5, 2)) + expectedSplitFee(1, 3) - 1,
			expectErr: true,
		},
		{ // "split with buffer"
			name: "split with buffer",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey:       "true",
					multiSplitBufferKey: "10",
				},
			},
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        (2*float64(requiredForOrder(15e5, 2)*110/100) + float64(expectedSplitFee(1, 2))) / 1e8,
					Vout:          0,
				},
			},
			balance:         2*uint64(requiredForOrder(15e5, 2)*110/100) + expectedSplitFee(1, 2),
			maxLock:         2*uint64(requiredForOrder(15e5, 2)*110/100) + expectedSplitFee(1, 2),
			expectSendRawTx: true,
			expectedInputs: []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Hash:  *txHashes[0],
						Index: 0,
					},
				},
			},
			expectedOutputs: []*wire.TxOut{
				wire.NewTxOut(requiredForOrder(15e5, 2)*110/100, []byte{}),
				wire.NewTxOut(requiredForOrder(15e5, 2)*110/100, []byte{}),
			},
			expectedSplitFee: expectedSplitFee(1, 2),
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
				{nil},
			},
		},
		{ // "split, maxLock too low to fund buffer"
			name: "split, maxLock too low to fund buffer",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
					{
						Value:        15e5,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey:       "true",
					multiSplitBufferKey: "10",
				},
			},
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        (2*float64(requiredForOrder(15e5, 2)*110/100) + float64(expectedSplitFee(1, 2))) / 1e8,
					Vout:          0,
				},
			},
			balance:   2*uint64(requiredForOrder(15e5, 2)*110/100) + expectedSplitFee(1, 2),
			maxLock:   2*uint64(requiredForOrder(15e5, 2)*110/100) + expectedSplitFee(1, 2) - 1,
			expectErr: true,
		},
		{ // "only one order needs a split, rest can be funded without"
			name: "only one order needs a split, rest can be funded without",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        1e6,
						MaxSwapCount: 2,
					},
					{
						Value:        1e6,
						MaxSwapCount: 2,
					},
					{
						Value:        1e6,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "true",
				},
			},
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        12e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[1],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(1),
					Address:       addresses(1),
					Amount:        12e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[2],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(2),
					Address:       addresses(2),
					Amount:        120e5 / 1e8,
					Vout:          0,
				},
			},
			maxLock:         50e5,
			balance:         144e5,
			expectSendRawTx: true,
			expectedInputs: []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Hash:  *txHashes[2],
						Index: 0,
					},
				},
			},
			expectedOutputs: []*wire.TxOut{
				wire.NewTxOut(requiredForOrder(1e6, 2), []byte{}),
				wire.NewTxOut(120e5-requiredForOrder(1e6, 2)-int64(expectedSplitFee(1, 2)), []byte{}),
			},
			expectedSplitFee: expectedSplitFee(1, 2),
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
				{nil},
				{nil},
			},
			expectedCoins: []asset.Coins{
				{NewOutput(txHashes[0], 0, 12e5)},
				{NewOutput(txHashes[1], 0, 12e5)},
				nil,
			},
		},
		{ // "only one order needs a split due to bond reserves, rest funded without"
			name: "only one order needs a split, rest can be funded without",
			multiOrder: &asset.MultiOrder{
				Values: []*asset.MultiOrderValue{
					{
						Value:        1e6,
						MaxSwapCount: 2,
					},
					{
						Value:        1e6,
						MaxSwapCount: 2,
					},
					{
						Value:        1e6,
						MaxSwapCount: 2,
					},
				},
				MaxFeeRate:    maxFeeRate,
				FeeSuggestion: feeSuggestion,
				Options: map[string]string{
					multiSplitKey: "true",
				},
			},
			utxos: []*ListUnspentResult{
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[0],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(0),
					Address:       addresses(0),
					Amount:        12e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[1],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(1),
					Address:       addresses(1),
					Amount:        12e5 / 1e8,
					Vout:          0,
				},
				{
					Confirmations: 1,
					Spendable:     true,
					TxID:          txIDs[2],
					RedeemScript:  nil,
					ScriptPubKey:  scriptPubKeys(2),
					Address:       addresses(2),
					Amount:        120e5 / 1e8,
					Vout:          0,
				},
			},
			maxLock:         0,
			bondReserves:    1e6,
			balance:         144e5,
			expectSendRawTx: true,
			expectedInputs: []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Hash:  *txHashes[2],
						Index: 0,
					},
				},
			},
			expectedOutputs: []*wire.TxOut{
				wire.NewTxOut(requiredForOrder(1e6, 2), []byte{}),
				wire.NewTxOut(120e5-requiredForOrder(1e6, 2)-int64(expectedSplitFee(1, 2)), []byte{}),
			},
			expectedSplitFee: expectedSplitFee(1, 2),
			expectedRedeemScripts: [][]util.Bytes{
				{nil},
				{nil},
				{nil},
			},
			expectedCoins: []asset.Coins{
				{NewOutput(txHashes[0], 0, 12e5)},
				{NewOutput(txHashes[1], 0, 12e5)},
				nil,
			},
		},
	}

	for _, test := range tests {
		node.listUnspent = test.utxos
		node.sentRawTx = nil
		node.lockedCoins = nil
		node.getBalances = &GetBalancesResult{
			Mine: Balances{
				Trusted: toBTC(test.balance),
			},
		}
		wallet.cm.lockedOutputs = make(map[OutPoint]*UTxO)
		wallet.bondReserves.Store(test.bondReserves)

		allCoins, _, splitFee, err := wallet.FundMultiOrder(test.multiOrder, test.maxLock)
		if test.expectErr {
			if err == nil {
				t.Fatalf("%s: no error returned", test.name)
			}
			if strings.Contains(err.Error(), "insufficient funds") {
				t.Fatalf("%s: unexpected insufficient funds error", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		if !test.expectSendRawTx { // no split
			if node.sentRawTx != nil {
				t.Fatalf("%s: unexpected transaction sent", test.name)
			}
			if len(allCoins) != len(test.expectedCoins) {
				t.Fatalf("%s: expected %d coins, got %d", test.name, len(test.expectedCoins), len(allCoins))
			}
			for i := range allCoins {
				if len(allCoins[i]) != len(test.expectedCoins[i]) {
					t.Fatalf("%s: expected %d coins in set %d, got %d", test.name, len(test.expectedCoins[i]), i, len(allCoins[i]))
				}
				actual := allCoins[i]
				expected := test.expectedCoins[i]
				sort.Slice(actual, func(i, j int) bool {
					return bytes.Compare(actual[i].ID(), actual[j].ID()) < 0
				})
				sort.Slice(expected, func(i, j int) bool {
					return bytes.Compare(expected[i].ID(), expected[j].ID()) < 0
				})
				for j := range actual {
					if !bytes.Equal(actual[j].ID(), expected[j].ID()) {
						t.Fatalf("%s: unexpected coin in set %d. expected %s, got %s", test.name, i, expected[j].ID(), actual[j].ID())
					}
					if actual[j].Value() != expected[j].Value() {
						t.Fatalf("%s: unexpected coin value in set %d. expected %d, got %d", test.name, i, expected[j].Value(), actual[j].Value())
					}
				}
			}
		} else { // expectSplit
			if node.sentRawTx == nil {
				t.Fatalf("%s: SendRawTransaction not called", test.name)
			}
			if len(node.sentRawTx.TxIn) != len(test.expectedInputs) {
				t.Fatalf("%s: expected %d inputs, got %d", test.name, len(test.expectedInputs), len(node.sentRawTx.TxIn))
			}
			for i, actualIn := range node.sentRawTx.TxIn {
				expectedIn := test.expectedInputs[i]
				if !bytes.Equal(actualIn.PreviousOutPoint.Hash[:], expectedIn.PreviousOutPoint.Hash[:]) {
					t.Fatalf("%s: unexpected input %d hash. expected %s, got %s", test.name, i, expectedIn.PreviousOutPoint.Hash, actualIn.PreviousOutPoint.Hash)
				}
				if actualIn.PreviousOutPoint.Index != expectedIn.PreviousOutPoint.Index {
					t.Fatalf("%s: unexpected input %d index. expected %d, got %d", test.name, i, expectedIn.PreviousOutPoint.Index, actualIn.PreviousOutPoint.Index)
				}
			}
			expectedNumOutputs := len(test.expectedOutputs)
			if test.expectedChange > 0 {
				expectedNumOutputs++
			}
			if len(node.sentRawTx.TxOut) != expectedNumOutputs {
				t.Fatalf("%s: expected %d outputs, got %d", test.name, expectedNumOutputs, len(node.sentRawTx.TxOut))
			}

			for i, expectedOut := range test.expectedOutputs {
				actualOut := node.sentRawTx.TxOut[i]
				if actualOut.Value != expectedOut.Value {
					t.Fatalf("%s: unexpected output %d value. expected %d, got %d", test.name, i, expectedOut.Value, actualOut.Value)
				}
			}
			if test.expectedChange > 0 {
				actualOut := node.sentRawTx.TxOut[len(node.sentRawTx.TxOut)-1]
				if uint64(actualOut.Value) != test.expectedChange {
					t.Fatalf("%s: unexpected change value. expected %d, got %d", test.name, test.expectedChange, actualOut.Value)
				}
			}

			if len(test.multiOrder.Values) != len(allCoins) {
				t.Fatalf("%s: expected %d coins, got %d", test.name, len(test.multiOrder.Values), len(allCoins))
			}
			splitTxID := node.sentRawTx.TxHash()

			// This means all coins are split outputs
			if test.expectedCoins == nil {
				for i, actualCoin := range allCoins {
					actualOut := actualCoin[0].(*Output)
					expectedOut := node.sentRawTx.TxOut[i]
					if uint64(expectedOut.Value) != actualOut.Val {
						t.Fatalf("%s: unexpected output %d value. expected %d, got %d", test.name, i, expectedOut.Value, actualOut.Val)
					}
					if !bytes.Equal(actualOut.Pt.TxHash[:], splitTxID[:]) {
						t.Fatalf("%s: unexpected output %d txid. expected %s, got %s", test.name, i, splitTxID, actualOut.Pt.TxHash)
					}
				}
			} else {
				var splitTxOutputIndex int
				for i := range allCoins {
					actual := allCoins[i]
					expected := test.expectedCoins[i]

					// This means the coins are the split outputs
					if expected == nil {
						actualOut := actual[0].(*Output)
						expectedOut := node.sentRawTx.TxOut[splitTxOutputIndex]
						if uint64(expectedOut.Value) != actualOut.Val {
							t.Fatalf("%s: unexpected output %d value. expected %d, got %d", test.name, i, expectedOut.Value, actualOut.Val)
						}
						if !bytes.Equal(actualOut.Pt.TxHash[:], splitTxID[:]) {
							t.Fatalf("%s: unexpected output %d txid. expected %s, got %s", test.name, i, splitTxID, actualOut.Pt.TxHash)
						}
						splitTxOutputIndex++
						continue
					}

					if len(actual) != len(expected) {
						t.Fatalf("%s: expected %d coins in set %d, got %d", test.name, len(test.expectedCoins[i]), i, len(allCoins[i]))
					}
					sort.Slice(actual, func(i, j int) bool {
						return bytes.Compare(actual[i].ID(), actual[j].ID()) < 0
					})
					sort.Slice(expected, func(i, j int) bool {
						return bytes.Compare(expected[i].ID(), expected[j].ID()) < 0
					})
					for j := range actual {
						if !bytes.Equal(actual[j].ID(), expected[j].ID()) {
							t.Fatalf("%s: unexpected coin in set %d. expected %s, got %s", test.name, i, expected[j].ID(), actual[j].ID())
						}
						if actual[j].Value() != expected[j].Value() {
							t.Fatalf("%s: unexpected coin value in set %d. expected %d, got %d", test.name, i, expected[j].Value(), actual[j].Value())
						}
					}
				}
			}

			// Each split output should be locked
			if len(node.lockedCoins) != len(allCoins)+len(test.expectedInputs) {
				t.Fatalf("%s: expected %d locked coins, got %d", test.name, len(allCoins)+len(test.expectedInputs), len(node.lockedCoins))
			}

		}

		// Check that the right coins are locked and in the fundingCoins map
		var totalNumCoins int
		for _, coins := range allCoins {
			totalNumCoins += len(coins)
		}
		if totalNumCoins != len(wallet.cm.lockedOutputs) {
			t.Fatalf("%s: expected %d funding coins in wallet, got %d", test.name, totalNumCoins, len(wallet.cm.lockedOutputs))
		}
		totalNumCoins += len(test.expectedInputs)
		if totalNumCoins != len(node.lockedCoins) {
			t.Fatalf("%s: expected %d locked coins, got %d", test.name, totalNumCoins, len(node.lockedCoins))
		}
		lockedCoins := make(map[RPCOutpoint]any)
		for _, coin := range node.lockedCoins {
			lockedCoins[*coin] = true
		}
		checkLockedCoin := func(txHash chainhash.Hash, vout uint32) {
			if _, ok := lockedCoins[RPCOutpoint{TxID: txHash.String(), Vout: vout}]; !ok {
				t.Fatalf("%s: expected locked coin %s:%d not found", test.name, txHash, vout)
			}
		}
		checkFundingCoin := func(txHash chainhash.Hash, vout uint32) {
			if _, ok := wallet.cm.lockedOutputs[OutPoint{TxHash: txHash, Vout: vout}]; !ok {
				t.Fatalf("%s: expected locked coin %s:%d not found in wallet", test.name, txHash, vout)
			}
		}
		for _, coins := range allCoins {
			for _, coin := range coins {
				// decode coin to output
				out := coin.(*Output)
				checkLockedCoin(out.Pt.TxHash, out.Pt.Vout)
				checkFundingCoin(out.Pt.TxHash, out.Pt.Vout)
			}
		}
		for _, expectedIn := range test.expectedInputs {
			checkLockedCoin(expectedIn.PreviousOutPoint.Hash, expectedIn.PreviousOutPoint.Index)
		}

		if test.expectedSplitFee != splitFee {
			t.Fatalf("%s: unexpected split fee. expected %d, got %d", test.name, test.expectedSplitFee, splitFee)
		}
	}
}

func TestMaxFundingFees(t *testing.T) {
	runRubric(t, testMaxFundingFees)
}

func testMaxFundingFees(t *testing.T, segwit bool, walletType string) {
	wallet, _, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	maxFeeRate := uint64(100)

	useSplitOptions := map[string]string{
		multiSplitKey: "true",
	}
	noSplitOptions := map[string]string{
		multiSplitKey: "false",
	}

	var inputSize, outputSize uint64
	if segwit {
		inputSize = RedeemP2WPKHInputTotalSize
		outputSize = P2WPKHOutputSize
	} else {
		inputSize = RedeemP2PKHInputSize
		outputSize = P2PKHOutputSize
	}

	const maxSwaps = 3
	const numInputs = 12
	maxFundingFees := wallet.MaxFundingFees(maxSwaps, maxFeeRate, useSplitOptions)
	expectedFees := maxFeeRate * (inputSize*numInputs + outputSize*(maxSwaps+1) + MinimumTxOverhead)
	if maxFundingFees != expectedFees {
		t.Fatalf("unexpected max funding fees. expected %d, got %d", expectedFees, maxFundingFees)
	}

	maxFundingFees = wallet.MaxFundingFees(maxSwaps, maxFeeRate, noSplitOptions)
	if maxFundingFees != 0 {
		t.Fatalf("unexpected max funding fees. expected 0, got %d", maxFundingFees)
	}
}

func TestAvailableFund(t *testing.T) {
	runRubric(t, testAvailableFund)
}

func testAvailableFund(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// With an empty list returned, there should be no error, but the value zero
	// should be returned.
	unspents := make([]*ListUnspentResult, 0)
	node.listUnspent = unspents // only needed for Fund, not Balance
	node.listLockUnspent = []*RPCOutpoint{}
	var bals GetBalancesResult
	node.getBalances = &bals
	bal, err := wallet.Balance()
	if err != nil {
		t.Fatalf("error for zero utxos: %v", err)
	}
	if bal.Available != 0 {
		t.Fatalf("expected available = 0, got %d", bal.Available)
	}
	if bal.Immature != 0 {
		t.Fatalf("expected unconf = 0, got %d", bal.Immature)
	}

	node.getBalancesErr = tErr
	_, err = wallet.Balance()
	if err == nil {
		t.Fatalf("no wallet error for rpc error")
	}
	node.getBalancesErr = nil
	var littleLots uint64 = 12
	littleOrder := tLotSize * littleLots
	littleFunds := calc.RequiredOrderFunds(littleOrder, RedeemP2PKHInputSize, littleLots, tSwapSizeBase, tSwapSize, tBTC.MaxFeeRate)
	littleUTXO := &ListUnspentResult{
		TxID:          tTxID,
		Address:       "1Bggq7Vu5oaoLFV1NNp5KhAzcku83qQhgi",
		Amount:        float64(littleFunds) / 1e8,
		Confirmations: 0,
		ScriptPubKey:  tP2PKH,
		Spendable:     true,
		Solvable:      true,
		SafePtr:       boolPtr(true),
	}
	unspents = append(unspents, littleUTXO)
	node.listUnspent = unspents
	bals.Mine.Trusted = float64(littleFunds) / 1e8
	node.getBalances = &bals
	lockedVal := uint64(1e6)
	node.listLockUnspent = []*RPCOutpoint{
		{
			TxID: tTxID,
			Vout: 1,
		},
	}

	msgTx := makeRawTx([]util.Bytes{{0x01}, {0x02}}, []*wire.TxIn{dummyInput()})
	msgTx.TxOut[1].Value = int64(lockedVal)
	txBuf := bytes.NewBuffer(make([]byte, 0, MsgTxVBytes(msgTx)))
	msgTx.Serialize(txBuf)
	const blockHeight = 5
	blockHash, _ := node.addRawTx(blockHeight, msgTx)

	node.getTransactionMap = map[string]*GetTransactionResult{
		"any": {
			BlockHash:  blockHash.String(),
			BlockIndex: blockHeight,
			Bytes:      txBuf.Bytes(),
		}}

	bal, err = wallet.Balance()
	if err != nil {
		t.Fatalf("error for 1 utxo: %v", err)
	}
	if bal.Available != littleFunds-lockedVal {
		t.Fatalf("expected available = %d for confirmed utxos, got %d", littleOrder-lockedVal, bal.Available)
	}
	if bal.Immature != 0 {
		t.Fatalf("expected immature = 0, got %d", bal.Immature)
	}
	if bal.Locked != lockedVal {
		t.Fatalf("expected locked = %d, got %d", lockedVal, bal.Locked)
	}

	var lottaLots uint64 = 100
	lottaOrder := tLotSize * lottaLots
	// Add funding for an extra input to accommodate the later combined tests.
	lottaFunds := calc.RequiredOrderFunds(lottaOrder, 2*RedeemP2PKHInputSize, lottaLots, tSwapSizeBase, tSwapSize, tBTC.MaxFeeRate)
	lottaUTXO := &ListUnspentResult{
		TxID:          tTxID,
		Address:       "1Bggq7Vu5oaoLFV1NNp5KhAzcku83qQhgi",
		Amount:        float64(lottaFunds) / 1e8,
		Confirmations: 1,
		Vout:          1,
		ScriptPubKey:  tP2PKH,
		Spendable:     true,
		Solvable:      true,
		SafePtr:       boolPtr(true),
	}
	unspents = append(unspents, lottaUTXO)
	littleUTXO.Confirmations = 1
	node.listUnspent = unspents
	bals.Mine.Trusted += float64(lottaFunds) / 1e8
	node.getBalances = &bals
	bal, err = wallet.Balance()
	if err != nil {
		t.Fatalf("error for 2 utxos: %v", err)
	}
	if bal.Available != littleFunds+lottaFunds-lockedVal {
		t.Fatalf("expected available = %d for 2 outputs, got %d", littleFunds+lottaFunds-lockedVal, bal.Available)
	}
	if bal.Immature != 0 {
		t.Fatalf("expected immature = 0 for 2 outputs, got %d", bal.Immature)
	}

	ord := &asset.Order{
		AssetVersion:  version,
		Value:         0,
		MaxSwapCount:  1,
		MaxFeeRate:    tBTC.MaxFeeRate,
		FeeSuggestion: feeSuggestion,
	}

	setOrderValue := func(v uint64) {
		ord.Value = v
		ord.MaxSwapCount = v / tLotSize
	}

	// Zero value
	_, _, _, err = wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no funding error for zero value")
	}

	// Nothing to spend
	node.listUnspent = []*ListUnspentResult{}
	setOrderValue(littleOrder)
	_, _, _, err = wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no error for zero utxos")
	}
	node.listUnspent = unspents

	// RPC error
	node.listUnspentErr = tErr
	_, _, _, err = wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no funding error for rpc error")
	}
	node.listUnspentErr = nil

	// Negative response when locking outputs.
	// There is no way to error locking outpoints in spv
	if walletType != walletTypeSPV {
		node.lockUnspentErr = tErr
		_, _, _, err = wallet.FundOrder(ord)
		if err == nil {
			t.Fatalf("no error for lockunspent result = false: %v", err)
		}
		node.lockUnspentErr = nil
	}

	// Fund a little bit, with unsafe littleUTXO.
	littleUTXO.SafePtr = boolPtr(false)
	littleUTXO.Confirmations = 0
	node.listUnspent = unspents
	spendables, _, _, err := wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error funding small amount: %v", err)
	}
	if len(spendables) != 1 {
		t.Fatalf("expected 1 spendable, got %d", len(spendables))
	}
	v := spendables[0].Value()
	if v != lottaFunds { // has to pick the larger output
		t.Fatalf("expected spendable of value %d, got %d", lottaFunds, v)
	}

	// Return/unlock the reserved coins to avoid warning in subsequent tests
	// about fundingCoins map containing the coins already. i.e.
	// "Known order-funding coin %v returned by listunspent"

	_ = wallet.ReturnCoins(spendables)

	// Now with safe confirmed littleUTXO.
	littleUTXO.SafePtr = boolPtr(true)
	littleUTXO.Confirmations = 2
	node.listUnspent = unspents
	spendables, _, _, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error funding small amount: %v", err)
	}
	if len(spendables) != 1 {
		t.Fatalf("expected 1 spendable, got %d", len(spendables))
	}
	v = spendables[0].Value()
	if v != littleFunds {
		t.Fatalf("expected spendable of value %d, got %d", littleFunds, v)
	}
	_ = wallet.ReturnCoins(spendables)

	// Adding a fee bump should now require the larger UTXO.
	ord.Options = map[string]string{swapFeeBumpKey: "1.5"}
	spendables, _, _, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error funding bumped fees: %v", err)
	}
	if len(spendables) != 1 {
		t.Fatalf("expected 1 spendable, got %d", len(spendables))
	}
	v = spendables[0].Value()
	if v != lottaFunds { // picks the bigger output because it is confirmed
		t.Fatalf("expected bumped fee utxo of value %d, got %d", littleFunds, v)
	}
	ord.Options = nil
	littleUTXO.Confirmations = 0
	_ = wallet.ReturnCoins(spendables)

	// Make lottaOrder unconfirmed like littleOrder, favoring little now.
	lottaUTXO.Confirmations = 0
	node.listUnspent = unspents
	spendables, _, _, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error funding small amount: %v", err)
	}
	if len(spendables) != 1 {
		t.Fatalf("expected 1 spendable, got %d", len(spendables))
	}
	v = spendables[0].Value()
	if v != littleFunds { // now picks the smaller output
		t.Fatalf("expected spendable of value %d, got %d", littleFunds, v)
	}
	_ = wallet.ReturnCoins(spendables)

	// Fund a lotta bit, covered by just the lottaBit UTXO.
	setOrderValue(lottaOrder)
	spendables, _, fees, err := wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error funding large amount: %v", err)
	}
	if len(spendables) != 1 {
		t.Fatalf("expected 1 spendable, got %d", len(spendables))
	}
	if fees != 0 {
		t.Fatalf("expected no fees, got %d", fees)
	}
	v = spendables[0].Value()
	if v != lottaFunds {
		t.Fatalf("expected spendable of value %d, got %d", lottaFunds, v)
	}
	_ = wallet.ReturnCoins(spendables)

	// require both spendables
	extraLottaOrder := littleOrder + lottaOrder
	extraLottaLots := littleLots + lottaLots
	setOrderValue(extraLottaOrder)
	spendables, _, fees, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error funding large amount: %v", err)
	}
	if len(spendables) != 2 {
		t.Fatalf("expected 2 spendable, got %d", len(spendables))
	}
	if fees != 0 {
		t.Fatalf("expected no fees, got %d", fees)
	}
	v = spendables[0].Value()
	if v != lottaFunds {
		t.Fatalf("expected spendable of value %d, got %d", lottaFunds, v)
	}
	_ = wallet.ReturnCoins(spendables)

	// Not enough to cover transaction fees.
	tweak := float64(littleFunds+lottaFunds-calc.RequiredOrderFunds(extraLottaOrder, 2*RedeemP2PKHInputSize, extraLottaLots, tSwapSizeBase, tSwapSize, tBTC.MaxFeeRate)+1) / 1e8
	lottaUTXO.Amount -= tweak
	node.listUnspent = unspents
	_, _, _, err = wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no error when not enough to cover tx fees")
	}
	lottaUTXO.Amount += tweak
	node.listUnspent = unspents

	// Prepare for a split transaction.
	baggageFees := tBTC.MaxFeeRate * splitTxBaggage
	if segwit {
		node.changeAddr = tP2WPKHAddr
		node.newAddress = tP2WPKHAddr
	} else {
		node.changeAddr = tP2PKHAddr
		node.newAddress = tP2PKHAddr
	}
	node.walletCfg.useSplitTx = true
	// No error when no split performed cuz math.
	coins, _, fees, err := wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error for no-split split: %v", err)
	}
	if fees != 0 {
		t.Fatalf("no-split split returned non-zero fees: %d", fees)
	}
	// Should be both coins.
	if len(coins) != 2 {
		t.Fatalf("no-split split didn't return both coins")
	}
	_ = wallet.ReturnCoins(coins)

	// No split because not standing order.
	ord.Immediate = true
	coins, _, fees, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error for no-split split: %v", err)
	}
	ord.Immediate = false
	if len(coins) != 2 {
		t.Fatalf("no-split split didn't return both coins")
	}
	if fees != 0 {
		t.Fatalf("no-split split returned non-zero fees: %d", fees)
	}
	_ = wallet.ReturnCoins(coins)

	var inputSize, outputSize uint64
	if wallet.segwit {
		inputSize = RedeemP2WPKHInputTotalSize
		outputSize = P2WPKHOutputSize
	} else {
		inputSize = RedeemP2PKHInputSize
		outputSize = P2PKHOutputSize
	}
	expectedTxSize := MinimumTxOverhead + 2*inputSize + 2*outputSize
	if wallet.segwit {
		expectedTxSize -= 1 // double counted wittness flag
	}
	expectedFees := expectedTxSize * feeSuggestion

	// With a little more locked, the split should be performed.
	node.signFunc = func(tx *wire.MsgTx) {
		signFunc(tx, 0, wallet.segwit)
	}
	lottaUTXO.Amount += float64(baggageFees) / 1e8
	node.listUnspent = unspents
	coins, _, fees, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error for split tx: %v", err)
	}
	// Should be just one coin.
	if len(coins) != 1 {
		t.Fatalf("split failed - coin count != 1")
	}
	if node.sentRawTx == nil {
		t.Fatalf("split failed - no tx sent")
	}
	if fees != expectedFees {
		t.Fatalf("split returned unexpected fees. wanted %d, got %d", expectedFees, fees)
	}
	_ = wallet.ReturnCoins(coins)

	// The split should also be added if we set the option at order time.
	node.walletCfg.useSplitTx = false
	ord.Options = map[string]string{splitKey: "true"}
	coins, _, fees, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error for forced split tx: %v", err)
	}
	// Should be just one coin still.
	if len(coins) != 1 {
		t.Fatalf("forced split failed - coin count != 1")
	}
	if fees != expectedFees {
		t.Fatalf("split returned unexpected fees. wanted %d, got %d", expectedFees, fees)
	}
	_ = wallet.ReturnCoins(coins)

	// // Hit some error paths.

	// Split transaction requires valid fee suggestion.
	// TODO:
	// 1.0: Error when no suggestion.
	// ord.FeeSuggestion = 0
	// _, _, err = wallet.FundOrder(ord)
	// if err == nil {
	// 	t.Fatalf("no error for no fee suggestions on split tx")
	// }
	ord.FeeSuggestion = tBTC.MaxFeeRate + 1
	_, _, _, err = wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no error for no fee suggestions on split tx")
	}
	// Check success again.
	ord.FeeSuggestion = tBTC.MaxFeeRate
	coins, _, _, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error fixing split tx: %v", err)
	}
	if fees != expectedFees {
		t.Fatalf("expected fees of %d, got %d", expectedFees, fees)
	}
	_ = wallet.ReturnCoins(coins)

	// GetRawChangeAddress error
	node.changeAddrErr = tErr
	_, _, _, err = wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no error for split tx change addr error")
	}
	node.changeAddrErr = nil

	// SendRawTx error
	node.sendErr = tErr
	_, _, _, err = wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no error for split tx send error")
	}
	node.sendErr = nil

	// Success again.
	spendables, _, _, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error for split tx recovery run")
	}
	_ = wallet.ReturnCoins(spendables)
}

func TestReturnCoins(t *testing.T) {
	wallet, node, shutdown := tNewWallet(true, walletTypeRPC)
	defer shutdown()

	// Test it with the local output type.
	coins := asset.Coins{
		NewOutput(tTxHash, 0, 1),
	}
	err := wallet.ReturnCoins(coins)
	if err != nil {
		t.Fatalf("error with output type coins: %v", err)
	}

	// Should error for no coins.
	err = wallet.ReturnCoins(asset.Coins{})
	if err == nil {
		t.Fatalf("no error for zero coins")
	}

	// nil unlocks all
	wallet.cm.lockedOutputs[OutPoint{*tTxHash, 0}] = &UTxO{}
	err = wallet.ReturnCoins(nil)
	if err != nil {
		t.Fatalf("error for nil coins: %v", err)
	}
	if len(wallet.cm.lockedOutputs) != 0 {
		t.Errorf("all funding coins not unlocked")
	}

	// Have the RPC return negative response.
	node.lockUnspentErr = tErr
	err = wallet.ReturnCoins(coins)
	if err == nil {
		t.Fatalf("no error for RPC failure")
	}
	node.lockUnspentErr = nil

	// ReturnCoins should accept any type that implements asset.Coin.
	err = wallet.ReturnCoins(asset.Coins{&tCoin{}, &tCoin{}})
	if err != nil {
		t.Fatalf("error with custom coin type: %v", err)
	}
}

func TestFundingCoins(t *testing.T) {
	// runRubric(t, testFundingCoins)
	testFundingCoins(t, false, walletTypeRPC)
}

func testFundingCoins(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	const vout0 = 1
	const txBlockHeight = 3
	tx0 := makeRawTx([]util.Bytes{{0x01}, tP2PKH}, []*wire.TxIn{dummyInput()})
	txHash0 := tx0.TxHash()
	_, _ = node.addRawTx(txBlockHeight, tx0)
	coinID0 := ToCoinID(&txHash0, vout0)
	// Make spendable (confs > 0)
	node.addRawTx(txBlockHeight+1, dummyTx())

	p2pkhUnspent0 := &ListUnspentResult{
		TxID:         txHash0.String(),
		Vout:         vout0,
		ScriptPubKey: tP2PKH,
		Spendable:    true,
		Solvable:     true,
		SafePtr:      boolPtr(true),
		Amount:       1,
	}
	unspents := []*ListUnspentResult{p2pkhUnspent0}

	// Add a second funding coin to make sure more than one iteration of the
	// utxo loops is required.
	const vout1 = 0
	tx1 := makeRawTx([]util.Bytes{tP2PKH, {0x02}}, []*wire.TxIn{dummyInput()})
	txHash1 := tx1.TxHash()
	_, _ = node.addRawTx(txBlockHeight, tx1)
	coinID1 := ToCoinID(&txHash1, vout1)
	// Make spendable (confs > 0)
	node.addRawTx(txBlockHeight+1, dummyTx())

	p2pkhUnspent1 := &ListUnspentResult{
		TxID:         txHash1.String(),
		Vout:         vout1,
		ScriptPubKey: tP2PKH,
		Spendable:    true,
		Solvable:     true,
		SafePtr:      boolPtr(true),
		Amount:       1,
	}
	unspents = append(unspents, p2pkhUnspent1)

	node.listLockUnspent = []*RPCOutpoint{}
	node.listUnspent = unspents
	coinIDs := []util.Bytes{coinID0, coinID1}

	ensureGood := func() {
		t.Helper()
		coins, err := wallet.FundingCoins(coinIDs)
		if err != nil {
			t.Fatalf("FundingCoins error: %v", err)
		}
		if len(coins) != 2 {
			t.Fatalf("expected 2 coins, got %d", len(coins))
		}
	}
	ensureGood()

	ensureErr := func(tag string) {
		t.Helper()
		// Clear the cache.
		wallet.cm.lockedOutputs = make(map[OutPoint]*UTxO)
		_, err := wallet.FundingCoins(coinIDs)
		if err == nil {
			t.Fatalf("%s: no error", tag)
		}
	}

	// No coins
	node.listUnspent = []*ListUnspentResult{}
	ensureErr("no coins")
	node.listUnspent = unspents

	// RPC error
	node.listUnspentErr = tErr
	ensureErr("rpc coins")
	node.listUnspentErr = nil

	// Bad coin ID.
	ogIDs := coinIDs
	coinIDs = []util.Bytes{randBytes(35)}
	ensureErr("bad coin ID")
	coinIDs = ogIDs

	// Coins locked but not in wallet.fundingCoins.
	irrelevantTx := dummyTx()
	node.addRawTx(txBlockHeight+1, irrelevantTx)
	node.listLockUnspent = []*RPCOutpoint{
		{TxID: p2pkhUnspent0.TxID, Vout: p2pkhUnspent0.Vout},
		{TxID: p2pkhUnspent1.TxID, Vout: p2pkhUnspent1.Vout},
	}
	node.listUnspent = []*ListUnspentResult{}

	txRaw0, _ := serializeMsgTx(tx0)
	getTxRes0 := &GetTransactionResult{
		Bytes: txRaw0,
	}
	txRaw1, _ := serializeMsgTx(tx1)
	getTxRes1 := &GetTransactionResult{
		Bytes: txRaw1,
	}

	node.getTransactionMap = map[string]*GetTransactionResult{
		txHash0.String(): getTxRes0,
		txHash1.String(): getTxRes1,
	}

	ensureGood()
}

func TestFundEdges(t *testing.T) {
	wallet, node, shutdown := tNewWallet(false, walletTypeRPC)
	defer shutdown()
	swapVal := uint64(1e7)
	lots := swapVal / tLotSize

	// Base Fees
	// fee_rate: 34 satoshi / vbyte (MaxFeeRate)
	// swap_size: 225 bytes (InitTxSize)
	// p2pkh input: 149 bytes (RedeemP2PKHInputSize)

	// NOTE: Shouldn't swap_size_base be 73 bytes?

	// swap_size_base: 76 bytes (225 - 149 p2pkh input) (InitTxSizeBase)
	// lot_size: 1e6
	// swap_value: 1e7
	// lots = swap_value / lot_size = 10
	//   total_bytes = first_swap_size + chained_swap_sizes
	//   chained_swap_sizes = (lots - 1) * swap_size
	//   first_swap_size = swap_size_base + backing_bytes
	//   total_bytes = swap_size_base + backing_bytes + (lots - 1) * swap_size
	//   base_tx_bytes = total_bytes - backing_bytes
	// base_tx_bytes = (lots - 1) * swap_size + swap_size_base = 9 * 225 + 76 = 2101
	// base_fees = base_tx_bytes * fee_rate = 2101 * 34 = 71434
	// backing_bytes: 1x P2PKH inputs = P2PKHInputSize = 149 bytes
	// backing_fees: 149 * fee_rate(34 atoms/byte) = 5066 atoms
	// total_bytes  = base_tx_bytes + backing_bytes = 2101 + 149 = 2250
	// total_fees: base_fees + backing_fees = 71434 + 5066 = 76500 atoms
	//          OR total_bytes * fee_rate = 2250 * 34 = 76500
	// base_best_case_bytes = swap_size_base + backing_bytes
	//                      = 76 + 149 = 225
	const swapSize = 225
	const totalBytes = 2250
	const bestCaseBytes = 225
	backingFees := uint64(totalBytes) * tBTC.MaxFeeRate // total_bytes * fee_rate
	p2pkhUnspent := &ListUnspentResult{
		TxID:          tTxID,
		Address:       tP2PKHAddr,
		Amount:        float64(swapVal+backingFees-1) / 1e8,
		Confirmations: 5,
		ScriptPubKey:  tP2PKH,
		Spendable:     true,
		Solvable:      true,
		SafePtr:       boolPtr(true),
	}
	unspents := []*ListUnspentResult{p2pkhUnspent}
	node.listUnspent = unspents
	ord := &asset.Order{
		AssetVersion:  version,
		Value:         swapVal,
		MaxSwapCount:  lots,
		MaxFeeRate:    tBTC.MaxFeeRate,
		FeeSuggestion: feeSuggestion,
	}

	var feeReduction uint64 = swapSize * tBTC.MaxFeeRate
	estFeeReduction := swapSize * feeSuggestion
	splitFees := splitTxBaggage * tBTC.MaxFeeRate
	checkMaxOrder(t, wallet, lots-1, swapVal-tLotSize, backingFees+splitFees-feeReduction,
		(totalBytes+splitTxBaggage)*feeSuggestion-estFeeReduction,
		(bestCaseBytes+splitTxBaggage)*feeSuggestion)

	_, _, _, err := wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no error when not enough funds in single p2pkh utxo")
	}
	// Now add the needed satoshi and try again.
	p2pkhUnspent.Amount = float64(swapVal+backingFees) / 1e8
	node.listUnspent = unspents

	checkMaxOrder(t, wallet, lots, swapVal, backingFees, totalBytes*feeSuggestion,
		bestCaseBytes*feeSuggestion)

	spendables, _, _, err := wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error when should be enough funding in single p2pkh utxo: %v", err)
	}
	_ = wallet.ReturnCoins(spendables)

	// For a split transaction, we would need to cover the splitTxBaggage as
	// well.
	node.walletCfg.useSplitTx = true
	node.changeAddr = tP2WPKHAddr
	node.newAddress = tP2WPKHAddr
	node.signFunc = func(tx *wire.MsgTx) {
		signFunc(tx, 0, wallet.segwit)
	}
	backingFees = uint64(totalBytes+splitTxBaggage) * tBTC.MaxFeeRate
	// 1 too few atoms
	v := swapVal + backingFees - 1
	p2pkhUnspent.Amount = float64(v) / 1e8
	node.listUnspent = unspents

	coins, _, _, err := wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error when skipping split tx due to baggage: %v", err)
	}
	if coins[0].Value() != v {
		t.Fatalf("split performed when baggage wasn't covered")
	}
	_ = wallet.ReturnCoins(spendables)

	// Just enough.
	v = swapVal + backingFees
	p2pkhUnspent.Amount = float64(v) / 1e8
	node.listUnspent = unspents

	checkMaxOrder(t, wallet, lots, swapVal, backingFees,
		(totalBytes+splitTxBaggage)*feeSuggestion,
		(bestCaseBytes+splitTxBaggage)*feeSuggestion)

	coins, _, _, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error funding split tx: %v", err)
	}
	if coins[0].Value() == v {
		t.Fatalf("split performed when baggage wasn't covered")
	}
	_ = wallet.ReturnCoins(spendables)
	node.walletCfg.useSplitTx = false

	// P2SH(P2PKH) p2sh pkScript = 23 bytes, p2pkh pkScript (redeemscript) = 25 bytes
	// sigScript = signature(1 + 73) + pubkey(1 + 33) + redeemscript(1 + 25) = 134
	// P2SH input size = overhead(40) + sigScriptData(1 + 134) = 40 + 135 = 175 bytes
	// backing fees: 175 bytes * fee_rate(34) = 5950 satoshi
	// Use 1 P2SH AND 1 P2PKH from the previous test.
	// total: 71434 + 5950 + 5066 = 82450 satoshi
	p2shRedeem, _ := hex.DecodeString("76a914db1755408acd315baa75c18ebbe0e8eaddf64a9788ac") // 25, p2pkh redeem script
	scriptAddr := "37XDx4CwPVEg5mC3awSPGCKA5Fe5FdsAS2"
	p2shScriptPubKey, _ := hex.DecodeString("a9143ff6a24a50135f69be9ffed744443da08408fc1a87") // 23, p2sh pkScript
	backingFees = 82450
	halfSwap := swapVal / 2
	p2shUnspent := &ListUnspentResult{
		TxID:          tTxID,
		Address:       scriptAddr,
		Amount:        float64(halfSwap) / 1e8,
		Confirmations: 10,
		ScriptPubKey:  p2shScriptPubKey,
		RedeemScript:  p2shRedeem,
		Spendable:     true,
		Solvable:      true,
		SafePtr:       boolPtr(true),
	}
	p2pkhUnspent.Amount = float64(halfSwap+backingFees-1) / 1e8
	unspents = []*ListUnspentResult{p2pkhUnspent, p2shUnspent}
	node.listUnspent = unspents
	_, _, _, err = wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no error when not enough funds in two utxos")
	}
	p2pkhUnspent.Amount = float64(halfSwap+backingFees) / 1e8
	node.listUnspent = unspents
	_, _, _, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error when should be enough funding in two utxos: %v", err)
	}
	_ = wallet.ReturnCoins(spendables)

	// P2WPKH witness: RedeemP2WPKHInputWitnessWeight = 109
	// P2WPKH input size = overhead(40) + no sigScript(1+0) + witness(ceil(109/4)) = 69 vbytes
	// backing fees: 69 * fee_rate(34) = 2346 satoshi
	// total: base_fees(71434) + 2346 = 73780 satoshi
	backingFees = 73780
	p2wpkhAddr := tP2WPKHAddr
	p2wpkhPkScript, _ := hex.DecodeString("0014054a40a6aa7c1f6bd15f286debf4f33cef0e21a1")
	p2wpkhUnspent := &ListUnspentResult{
		TxID:          tTxID,
		Address:       p2wpkhAddr,
		Amount:        float64(swapVal+backingFees-1) / 1e8,
		Confirmations: 3,
		ScriptPubKey:  p2wpkhPkScript,
		Spendable:     true,
		Solvable:      true,
		SafePtr:       boolPtr(true),
	}
	unspents = []*ListUnspentResult{p2wpkhUnspent}
	node.listUnspent = unspents
	_, _, _, err = wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no error when not enough funds in single p2wpkh utxo")
	}
	p2wpkhUnspent.Amount = float64(swapVal+backingFees) / 1e8
	node.listUnspent = unspents
	_, _, _, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error when should be enough funding in single p2wpkh utxo: %v", err)
	}
	_ = wallet.ReturnCoins(spendables)

	// P2WSH(P2WPKH)
	//  p2wpkh redeem script length, btc.P2WPKHPkScriptSize: 22
	// witness: version(1) + signature(1 + 73) + pubkey(1 + 33) + redeemscript(1 + 22) = 132
	// input size: overhead(40) + no sigScript(1+0) + witness(132)/4 = 74 vbyte
	// backing fees: 74 * 34 = 2516 satoshi
	// total: base_fees(71434) + 2516 = 73950 satoshi
	backingFees = 73950
	p2wpkhRedeemScript, _ := hex.DecodeString("0014b71554f9a66ef4fa4dbeddb9fa491f5a1d938ebc") //22
	p2wshAddr := "bc1q9heng7q275grmy483cueqrr00dvyxpd8w6kes3nzptm7087d6lvsvffpqf"
	p2wshPkScript, _ := hex.DecodeString("00202df334780af5103d92a78e39900c6f7b584305a776ad9846620af7e79fcdd7d9") //34
	p2wpshUnspent := &ListUnspentResult{
		TxID:          tTxID,
		Address:       p2wshAddr,
		Amount:        float64(swapVal+backingFees-1) / 1e8,
		Confirmations: 7,
		ScriptPubKey:  p2wshPkScript,
		RedeemScript:  p2wpkhRedeemScript,
		Spendable:     true,
		Solvable:      true,
		SafePtr:       boolPtr(true),
	}
	unspents = []*ListUnspentResult{p2wpshUnspent}
	node.listUnspent = unspents
	_, _, _, err = wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no error when not enough funds in single p2wsh utxo")
	}
	p2wpshUnspent.Amount = float64(swapVal+backingFees) / 1e8
	node.listUnspent = unspents
	_, _, _, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error when should be enough funding in single p2wsh utxo: %v", err)
	}
	_ = wallet.ReturnCoins(spendables)
}

func TestFundEdgesSegwit(t *testing.T) {
	wallet, node, shutdown := tNewWallet(true, walletTypeRPC)
	defer shutdown()
	swapVal := uint64(1e7)
	lots := swapVal / tLotSize

	// Base Fees
	// fee_rate: 34 satoshi / vbyte (MaxFeeRate)

	// swap_size: 153 bytes (InitTxSizeSegwit)
	// p2wpkh input, incl. marker and flag: 69 bytes (RedeemP2WPKHInputSize + ((RedeemP2WPKHInputWitnessWeight + 2 + 3) / 4))
	// swap_size_base: 84 bytes (153 - 69 p2pkh input) (InitTxSizeBaseSegwit)

	// lot_size: 1e6
	// swap_value: 1e7
	// lots = swap_value / lot_size = 10
	//   total_bytes = first_swap_size + chained_swap_sizes
	//   chained_swap_sizes = (lots - 1) * swap_size
	//   first_swap_size = swap_size_base + backing_bytes
	//   total_bytes  = swap_size_base + backing_bytes + (lots - 1) * swap_size
	//   base_tx_bytes = total_bytes - backing_bytes
	// base_tx_bytes = (lots - 1) * swap_size + swap_size_base = 9 * 153 + 84 = 1461
	// base_fees = base_tx_bytes * fee_rate = 1461 * 34 = 49674
	// backing_bytes: 1x P2WPKH-spending input = p2wpkh input = 69 bytes
	// backing_fees: 69 * fee_rate(34 atoms/byte) = 2346 atoms
	// total_bytes  = base_tx_bytes + backing_bytes = 1461 + 69 = 1530
	// total_fees: base_fees + backing_fees = 49674 + 2346 = 52020 atoms
	//          OR total_bytes * fee_rate = 1530 * 34 = 52020
	// base_best_case_bytes = swap_size_base + backing_bytes
	//                      = 84 + 69 = 153
	const swapSize = 153
	const totalBytes = 1530
	const bestCaseBytes = 153
	backingFees := uint64(totalBytes) * tBTC.MaxFeeRate // total_bytes * fee_rate
	p2wpkhUnspent := &ListUnspentResult{
		TxID:          tTxID,
		Address:       tP2WPKHAddr,
		Amount:        float64(swapVal+backingFees-1) / 1e8,
		Confirmations: 5,
		ScriptPubKey:  tP2WPKH,
		Spendable:     true,
		Solvable:      true,
		SafePtr:       boolPtr(true),
	}
	unspents := []*ListUnspentResult{p2wpkhUnspent}
	node.listUnspent = unspents
	ord := &asset.Order{
		AssetVersion:  version,
		Value:         swapVal,
		MaxSwapCount:  lots,
		MaxFeeRate:    tBTC.MaxFeeRate,
		FeeSuggestion: feeSuggestion,
	}

	var feeReduction uint64 = swapSize * tBTC.MaxFeeRate
	estFeeReduction := swapSize * feeSuggestion
	splitFees := splitTxBaggageSegwit * tBTC.MaxFeeRate
	checkMaxOrder(t, wallet, lots-1, swapVal-tLotSize, backingFees+splitFees-feeReduction,
		(totalBytes+splitTxBaggageSegwit)*feeSuggestion-estFeeReduction,
		(bestCaseBytes+splitTxBaggageSegwit)*feeSuggestion)

	_, _, _, err := wallet.FundOrder(ord)
	if err == nil {
		t.Fatalf("no error when not enough funds in single p2wpkh utxo")
	}
	// Now add the needed satoshi and try again.
	p2wpkhUnspent.Amount = float64(swapVal+backingFees) / 1e8
	node.listUnspent = unspents

	checkMaxOrder(t, wallet, lots, swapVal, backingFees, totalBytes*feeSuggestion,
		bestCaseBytes*feeSuggestion)

	spendables, _, _, err := wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error when should be enough funding in single p2wpkh utxo: %v", err)
	}
	_ = wallet.ReturnCoins(spendables)

	// For a split transaction, we would need to cover the splitTxBaggage as
	// well.
	node.walletCfg.useSplitTx = true
	node.changeAddr = tP2WPKHAddr
	node.newAddress = tP2WPKHAddr
	node.signFunc = func(tx *wire.MsgTx) {
		signFunc(tx, 0, wallet.segwit)
	}
	backingFees = uint64(totalBytes+splitTxBaggageSegwit) * tBTC.MaxFeeRate
	v := swapVal + backingFees - 1
	p2wpkhUnspent.Amount = float64(v) / 1e8
	node.listUnspent = unspents
	coins, _, _, err := wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error when skipping split tx because not enough to cover baggage: %v", err)
	}
	if coins[0].Value() != v {
		t.Fatalf("split performed when baggage wasn't covered")
	}
	_ = wallet.ReturnCoins(spendables)
	// Now get the split.
	v = swapVal + backingFees
	p2wpkhUnspent.Amount = float64(v) / 1e8
	node.listUnspent = unspents

	checkMaxOrder(t, wallet, lots, swapVal, backingFees,
		(totalBytes+splitTxBaggageSegwit)*feeSuggestion,
		(bestCaseBytes+splitTxBaggageSegwit)*feeSuggestion)

	coins, _, _, err = wallet.FundOrder(ord)
	if err != nil {
		t.Fatalf("error funding split tx: %v", err)
	}
	_ = wallet.ReturnCoins(spendables)
	if coins[0].Value() == v {
		t.Fatalf("split performed when baggage wasn't covered")
	}
	node.walletCfg.useSplitTx = false
}

func TestPreSwap(t *testing.T) {
	runRubric(t, testPreSwap)
}

func testPreSwap(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// See math from TestFundEdges. 10 lots with max fee rate of 34 sats/vbyte.

	swapVal := uint64(1e7)
	lots := swapVal / tLotSize // 10 lots

	// var swapSize = 225
	var totalBytes uint64 = 2250
	var bestCaseBytes uint64 = 225
	pkScript := tP2PKH
	if segwit {
		// swapSize = 153
		totalBytes = 1530
		bestCaseBytes = 153
		pkScript = tP2WPKH
	}

	backingFees := totalBytes * tBTC.MaxFeeRate // total_bytes * fee_rate

	minReq := swapVal + backingFees

	unspent := &ListUnspentResult{
		TxID:          tTxID,
		Address:       tP2PKHAddr,
		Confirmations: 5,
		ScriptPubKey:  pkScript,
		Spendable:     true,
		Solvable:      true,
		SafePtr:       boolPtr(true),
	}
	unspents := []*ListUnspentResult{unspent}

	setFunds := func(v uint64) {
		unspent.Amount = float64(v) / 1e8
		node.listUnspent = unspents
	}

	form := &asset.PreSwapForm{
		AssetVersion:  version,
		LotSize:       tLotSize,
		Lots:          lots,
		MaxFeeRate:    tBTC.MaxFeeRate,
		Immediate:     false,
		FeeSuggestion: feeSuggestion,
		// Redeem fields unneeded
	}

	setFunds(minReq)

	// Initial success.
	preSwap, err := wallet.PreSwap(form)
	if err != nil {
		t.Fatalf("PreSwap error: %v", err)
	}

	maxFees := totalBytes * tBTC.MaxFeeRate
	estHighFees := totalBytes * feeSuggestion
	estLowFees := bestCaseBytes * feeSuggestion
	checkSwapEstimate(t, preSwap.Estimate, lots, swapVal, maxFees, estHighFees, estLowFees)

	// Too little funding is an error.
	setFunds(minReq - 1)
	_, err = wallet.PreSwap(form)
	if err == nil {
		t.Fatalf("no PreSwap error for not enough funds")
	}
	setFunds(minReq)

	// Success again.
	_, err = wallet.PreSwap(form)
	if err != nil {
		t.Fatalf("PreSwap error: %v", err)
	}
}

func TestPreRedeem(t *testing.T) {
	runRubric(t, testPreRedeem)
}

func testPreRedeem(t *testing.T, segwit bool, walletType string) {
	wallet, _, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	preRedeem, err := wallet.PreRedeem(&asset.PreRedeemForm{
		AssetVersion: version,
		Lots:         5,
	})
	// Shouldn't actually be any path to error.
	if err != nil {
		t.Fatalf("PreRedeem non-segwit error: %v", err)
	}

	// Just a couple of sanity checks.
	if preRedeem.Estimate.RealisticBestCase >= preRedeem.Estimate.RealisticWorstCase {
		t.Fatalf("best case > worst case")
	}
}
