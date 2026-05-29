//go:build !spvlive && !harness

package btc

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// TestAccelerateOrder tests the entire acceleration workflow, including
// AccelerateOrder, PreAccelerate, and AccelerationEstimate
func TestAccelerateOrder(t *testing.T) {
	runRubric(t, testAccelerateOrder)
}

func testAccelerateOrder(t *testing.T, segwit bool, walletType string) {
	w, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	wallet := &ExchangeWalletAccelerator{&ExchangeWalletFullNode{w, &authAddOn{w.node}}}

	var blockHash100 chainhash.Hash
	copy(blockHash100[:], encode.RandomBytes(32))
	node.verboseBlocks[blockHash100] = &msgBlockWithHeight{height: 100, msgBlock: &wire.MsgBlock{
		Header: wire.BlockHeader{Timestamp: time.Now()},
	}}
	node.mainchain[100] = &blockHash100

	if segwit {
		node.changeAddr = tP2WPKHAddr
		node.newAddress = tP2WPKHAddr
	} else {
		node.changeAddr = tP2PKHAddr
		node.newAddress = tP2PKHAddr
	}

	node.signFunc = func(tx *wire.MsgTx) {
		signFunc(tx, 0, wallet.segwit)
	}

	sumFees := func(fees []float64, confs []uint64) uint64 {
		var totalFees uint64
		for i, fee := range fees {
			if confs[i] == 0 {
				totalFees += toSatoshi(fee)
			}
		}
		return totalFees
	}

	sumTxSizes := func(txs []*wire.MsgTx, confirmations []uint64) uint64 {
		var totalSize uint64
		for i, tx := range txs {
			if confirmations[i] == 0 {
				totalSize += MsgTxVBytes(tx)
			}
		}

		return totalSize
	}

	loadTxsIntoNode := func(txs []*wire.MsgTx, fees []float64, confs []uint64, node *testData, withinTimeLimit bool, t *testing.T) {
		t.Helper()
		if len(txs) != len(fees) || len(txs) != len(confs) {
			t.Fatalf("len(txs) = %d, len(fees) = %d, len(confs) = %d", len(txs), len(fees), len(confs))
		}

		serializedTxs := make([][]byte, 0, len(txs))
		for _, tx := range txs {
			serializedTx, err := serializeMsgTx(tx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			serializedTxs = append(serializedTxs, serializedTx)
		}

		currentTime := time.Now().Unix()
		for i := range txs {
			var blockHash string
			if confs[i] == 1 {
				blockHash = blockHash100.String()
			}
			node.getTransactionMap[txs[i].TxHash().String()] = &GetTransactionResult{
				TxID:          txs[i].TxHash().String(),
				Bytes:         serializedTxs[i],
				BlockHash:     blockHash,
				Confirmations: confs[i]}

			if withinTimeLimit {
				node.getTransactionMap[txs[i].TxHash().String()].Time = uint64(currentTime) - minTimeBeforeAcceleration + 3
			} else {
				node.getTransactionMap[txs[i].TxHash().String()].Time = uint64(currentTime) - minTimeBeforeAcceleration - 1000
			}
		}
	}

	// getAccelerationParams returns a chain of 4 swap transactions where the change
	// output of the last transaction has a certain value. If addAcceleration true,
	// the third transaction will be an acceleration transaction instead of one
	// that initiates a swap.
	getAccelerationParams := func(changeVal int64, addChangeToUnspent, addAcceleration bool, fees []float64, node *testData) ([]util.Bytes, []util.Bytes, util.Bytes, []*wire.MsgTx) {
		txs := make([]*wire.MsgTx, 4)

		// In order to be able to test using the SPV wallet, we need to properly
		// set the size of each of the outputs. The SPV wallet will parse these
		// transactions and calculate the fee on its own instead of just returning
		// what was set in getTransactionMap.
		changeOutputAmounts := make([]int64, 4)
		changeOutputAmounts[3] = changeVal
		swapAmount := int64(2e6)
		for i := 2; i >= 0; i-- {
			changeAmount := int64(toSatoshi(fees[i+1])) + changeOutputAmounts[i+1]
			if !(i == 1 && addAcceleration) {
				changeAmount += swapAmount
			}
			changeOutputAmounts[i] = changeAmount
		}

		// The initial transaction in the chain has multiple inputs.
		fundingCoinsTotalOutput := changeOutputAmounts[0] + swapAmount + int64(toSatoshi(fees[0]))
		fundingTx := wire.MsgTx{
			TxIn: []*wire.TxIn{{
				PreviousOutPoint: wire.OutPoint{
					Hash:  *tTxHash,
					Index: 0,
				}},
				{PreviousOutPoint: wire.OutPoint{
					Hash:  *tTxHash,
					Index: 1,
				}},
			},
			TxOut: []*wire.TxOut{{
				Value: fundingCoinsTotalOutput / 2,
			}, {
				Value: fundingCoinsTotalOutput - fundingCoinsTotalOutput/2,
			}},
		}
		fudingTxHex, _ := serializeMsgTx(&fundingTx)
		node.getTransactionMap[fundingTx.TxHash().String()] = &GetTransactionResult{Bytes: fudingTxHex, BlockHash: blockHash100.String()}

		txs[0] = &wire.MsgTx{
			TxIn: []*wire.TxIn{{
				PreviousOutPoint: wire.OutPoint{
					Hash:  fundingTx.TxHash(),
					Index: 0,
				}},
				{PreviousOutPoint: wire.OutPoint{
					Hash:  fundingTx.TxHash(),
					Index: 1,
				}},
			},
			TxOut: []*wire.TxOut{{
				Value: changeOutputAmounts[0],
			}, {
				Value: swapAmount,
			}},
		}
		for i := 1; i < 4; i++ {
			txs[i] = &wire.MsgTx{
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: wire.OutPoint{
						Hash:  txs[i-1].TxHash(),
						Index: 0,
					}},
				},
				TxOut: []*wire.TxOut{{
					Value: changeOutputAmounts[i],
				}},
			}
			if !(i == 2 && addAcceleration) {
				txs[i].TxOut = append(txs[i].TxOut, &wire.TxOut{Value: swapAmount})
			}
		}

		swapCoins := make([]util.Bytes, 0, len(txs))
		accelerationCoins := make([]util.Bytes, 0, 1)
		var changeCoin util.Bytes
		for i, tx := range txs {
			hash := tx.TxHash()

			if i == 2 && addAcceleration {
				accelerationCoins = append(accelerationCoins, ToCoinID(&hash, 0))
			} else {
				ToCoinID(&hash, 0)
				swapCoins = append(swapCoins, ToCoinID(&hash, 0))
			}

			if i == len(txs)-1 {
				changeCoin = ToCoinID(&hash, 0)
				if addChangeToUnspent {
					node.listUnspent = append(node.listUnspent, &ListUnspentResult{
						TxID: hash.String(),
						Vout: 0,
					})
				}
			}
		}

		return swapCoins, accelerationCoins, changeCoin, txs
	}

	addUTXOToNode := func(confs uint32, segwit bool, amount uint64, node *testData) {
		var scriptPubKey []byte
		if segwit {
			scriptPubKey = tP2WPKH
		} else {
			scriptPubKey = tP2PKH
		}

		node.listUnspent = append(node.listUnspent, &ListUnspentResult{
			TxID:          hex.EncodeToString(encode.RandomBytes(32)),
			Address:       "1Bggq7Vu5oaoLFV1NNp5KhAzcku83qQhgi",
			Amount:        toBTC(amount),
			Confirmations: confs,
			ScriptPubKey:  scriptPubKey,
			Spendable:     true,
			Solvable:      true,
		})

		var prevChainHash chainhash.Hash
		copy(prevChainHash[:], encode.RandomBytes(32))

		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{{
				PreviousOutPoint: wire.OutPoint{
					Hash:  prevChainHash,
					Index: 0,
				}},
			},
			TxOut: []*wire.TxOut{
				{
					Value: int64(amount),
				},
			}}
		unspentTxHex, err := serializeMsgTx(tx)
		if err != nil {
			panic(fmt.Sprintf("could not serialize: %v", err))
		}
		var blockHash string
		if confs == 0 {
			blockHash = blockHash100.String()
		}

		node.getTransactionMap[node.listUnspent[len(node.listUnspent)-1].TxID] = &GetTransactionResult{
			TxID:          tx.TxHash().String(),
			Bytes:         unspentTxHex,
			BlockHash:     blockHash,
			Confirmations: uint64(confs)}
	}

	totalInputOutput := func(tx *wire.MsgTx) (uint64, uint64) {
		var in, out uint64
		for _, input := range tx.TxIn {
			inputGtr, found := node.getTransactionMap[input.PreviousOutPoint.Hash.String()]
			if !found {
				t.Fatalf("tx id not found: %v", input.PreviousOutPoint.Hash.String())
			}
			inputTx, err := msgTxFromHex(inputGtr.Bytes.String())
			if err != nil {
				t.Fatalf("failed to deserialize tx: %v", err)
			}
			in += uint64(inputTx.TxOut[input.PreviousOutPoint.Index].Value)
		}

		for _, output := range node.sentRawTx.TxOut {
			out += uint64(output.Value)
		}

		return in, out
	}

	calculateChangeTxSize := func(hasChange, segwit bool, numInputs int) uint64 {
		baseSize := MinimumTxOverhead

		var inputSize, witnessSize, outputSize int
		if segwit {
			witnessSize = RedeemP2WPKHInputWitnessWeight*numInputs + 2
			inputSize = RedeemP2WPKHInputSize * numInputs
			outputSize = P2WPKHOutputSize
		} else {
			inputSize = numInputs * RedeemP2PKHInputSize
			outputSize = P2PKHOutputSize
		}

		baseSize += inputSize
		if hasChange {
			baseSize += outputSize
		}

		txWeight := baseSize*4 + witnessSize
		txSize := (txWeight + 3) / 4
		return uint64(txSize)
	}

	var changeAmount int64 = 21350
	var newFeeRate uint64 = 50
	fees := []float64{0.00002, 0.000005, 0.00001, 0.00001}
	confs := []uint64{0, 0, 0, 0}
	_, _, _, txs := getAccelerationParams(changeAmount, false, false, fees, node)
	expectedFees := (sumTxSizes(txs, confs)+calculateChangeTxSize(false, segwit, 1))*newFeeRate - sumFees(fees, confs)
	_, _, _, txs = getAccelerationParams(changeAmount, true, false, fees, node)
	expectedFeesWithChange := (sumTxSizes(txs, confs)+calculateChangeTxSize(true, segwit, 1))*newFeeRate - sumFees(fees, confs)

	// See IsDust for the source of this dustCoverage voodoo.
	var dustCoverage uint64
	if segwit {
		dustCoverage = (P2WPKHOutputSize + 41 + (107 / 4)) * 3 * newFeeRate
	} else {
		dustCoverage = (P2PKHOutputSize + 41 + 107) * 3 * newFeeRate
	}

	type utxo struct {
		amount uint64
		confs  uint32
	}

	tests := []struct {
		name                      string
		changeNotInUnspent        bool
		addPreviousAcceleration   bool
		numUtxosUsed              int
		changeAmount              int64
		lockChange                bool
		scrambleSwapCoins         bool
		expectChange              bool
		utxos                     []utxo
		fees                      []float64
		confs                     []uint64
		requiredForRemainingSwaps uint64
		expectChangeLocked        bool
		txTimeWithinLimit         bool

		// needed to test AccelerateOrder and AccelerationEstimate
		expectAccelerateOrderErr      bool
		expectAccelerationEstimateErr bool
		newFeeRate                    uint64

		// needed to test PreAccelerate
		suggestedFeeRate       uint64
		expectPreAccelerateErr bool
		expectTooEarly         bool
	}{
		{
			name:                          "change not in utxo set",
			changeAmount:                  int64(expectedFees),
			fees:                          []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:                         []uint64{0, 0, 0, 0},
			newFeeRate:                    50,
			expectAccelerationEstimateErr: true,
			expectAccelerateOrderErr:      true,
			expectPreAccelerateErr:        true,
			changeNotInUnspent:            true,
		},
		{
			name:             "just enough without change",
			changeAmount:     int64(expectedFees),
			fees:             []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:            confs,
			newFeeRate:       newFeeRate,
			suggestedFeeRate: 30,
		},
		{
			name:                    "works with acceleration",
			changeAmount:            int64(expectedFees),
			fees:                    []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:                   confs,
			newFeeRate:              newFeeRate,
			addPreviousAcceleration: true,
		},
		{
			name:              "scramble swap coins",
			changeAmount:      int64(expectedFees),
			fees:              []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:             []uint64{0, 0, 0, 0},
			newFeeRate:        newFeeRate,
			scrambleSwapCoins: true,
		},
		{
			name:                          "not enough with just change",
			changeAmount:                  int64(expectedFees - 1),
			fees:                          []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:                         []uint64{0, 0, 0, 0},
			newFeeRate:                    newFeeRate,
			expectAccelerationEstimateErr: true,
			expectAccelerateOrderErr:      true,
		},
		{
			name:         "add greater than dust amount to change",
			changeAmount: int64(expectedFeesWithChange + dustCoverage),
			fees:         []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:        []uint64{0, 0, 0, 0},
			newFeeRate:   newFeeRate,
			expectChange: true,
		},
		{
			name:         "add dust amount to change",
			changeAmount: int64(expectedFeesWithChange + dustCoverage - 1),
			fees:         []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:        []uint64{0, 0, 0, 0},
			newFeeRate:   newFeeRate,
			expectChange: false,
		},
		{
			name:         "don't accelerate confirmed transactions",
			changeAmount: int64(expectedFeesWithChange + dustCoverage),
			fees:         []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:        []uint64{1, 1, 0, 0},
			newFeeRate:   newFeeRate,
			expectChange: true,
		},
		{
			name:                          "not enough",
			changeAmount:                  int64(expectedFees - 1),
			fees:                          []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:                         []uint64{0, 0, 0, 0},
			newFeeRate:                    newFeeRate,
			expectAccelerationEstimateErr: true,
			expectAccelerateOrderErr:      true,
		},
		{
			name:                          "not enough with 0-conf utxo in wallet",
			changeAmount:                  int64(expectedFees - 1),
			fees:                          []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:                         []uint64{0, 0, 0, 0},
			newFeeRate:                    newFeeRate,
			expectAccelerationEstimateErr: true,
			expectAccelerateOrderErr:      true,
			utxos: []utxo{{
				confs:  0,
				amount: 5e9,
			}},
		},
		{
			name:         "enough with 1-conf utxo in wallet",
			changeAmount: int64(expectedFees - 1),
			fees:         []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:        []uint64{0, 0, 0, 0},
			newFeeRate:   newFeeRate,
			numUtxosUsed: 1,
			expectChange: true,
			utxos: []utxo{{
				confs:  1,
				amount: 2e6,
			}},
		},
		{
			name:                          "not enough for remaining swaps",
			changeAmount:                  int64(expectedFees - 1),
			fees:                          fees,
			confs:                         []uint64{0, 0, 0, 0},
			newFeeRate:                    newFeeRate,
			numUtxosUsed:                  1,
			expectAccelerationEstimateErr: true,
			expectAccelerateOrderErr:      true,
			expectChange:                  true,
			requiredForRemainingSwaps:     2e6,
			utxos: []utxo{{
				confs:  1,
				amount: 2e6,
			}},
		},
		{
			name:                      "enough for remaining swaps",
			changeAmount:              int64(expectedFees - 1),
			fees:                      fees,
			confs:                     []uint64{0, 0, 0, 0},
			newFeeRate:                newFeeRate,
			numUtxosUsed:              2,
			expectChange:              true,
			expectChangeLocked:        true,
			requiredForRemainingSwaps: 2e6,
			utxos: []utxo{
				{
					confs:  1,
					amount: 2e6,
				},
				{
					confs:  1,
					amount: 1e6,
				},
			},
		},
		{
			name:                      "locked change, required for remaining >0",
			changeAmount:              int64(expectedFees - 1),
			fees:                      fees,
			confs:                     []uint64{0, 0, 0, 0},
			newFeeRate:                newFeeRate,
			numUtxosUsed:              2,
			expectChange:              true,
			expectChangeLocked:        true,
			requiredForRemainingSwaps: 2e6,
			lockChange:                true,
			utxos: []utxo{
				{
					confs:  1,
					amount: 2e6,
				},
				{
					confs:  1,
					amount: 1e6,
				},
			},
		},
		{
			name:                          "locked change, required for remaining == 0",
			changeAmount:                  int64(expectedFees - 1),
			fees:                          []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:                         []uint64{0, 0, 0, 0},
			newFeeRate:                    newFeeRate,
			numUtxosUsed:                  2,
			expectChange:                  true,
			expectAccelerationEstimateErr: true,
			expectAccelerateOrderErr:      true,
			expectPreAccelerateErr:        true,
			lockChange:                    true,
			utxos: []utxo{
				{
					confs:  1,
					amount: 2e6,
				},
				{
					confs:  1,
					amount: 1e6,
				},
			},
		},
		{
			name:              "tx time within limit",
			txTimeWithinLimit: true,
			expectTooEarly:    true,
			changeAmount:      int64(expectedFees),
			fees:              []float64{0.00002, 0.000005, 0.00001, 0.00001},
			confs:             confs,
			newFeeRate:        newFeeRate,
			suggestedFeeRate:  30,
		},
	}

	for _, test := range tests {
		node.listUnspent = []*ListUnspentResult{}
		swapCoins, accelerations, changeCoin, txs := getAccelerationParams(test.changeAmount, !test.changeNotInUnspent, test.addPreviousAcceleration, test.fees, node)
		expectedFees := (sumTxSizes(txs, test.confs)+calculateChangeTxSize(test.expectChange, segwit, 1+test.numUtxosUsed))*test.newFeeRate - sumFees(test.fees, test.confs)
		if !test.expectChange {
			expectedFees = uint64(test.changeAmount)
		}
		if test.scrambleSwapCoins {
			temp := swapCoins[0]
			swapCoins[0] = swapCoins[2]
			swapCoins[2] = swapCoins[1]
			swapCoins[1] = temp
		}
		if test.lockChange {
			changeTxID, changeVout, _ := decodeCoinID(changeCoin)
			node.listLockUnspent = []*RPCOutpoint{
				{
					TxID: changeTxID.String(),
					Vout: changeVout,
				},
			}
		}
		for _, utxo := range test.utxos {
			addUTXOToNode(utxo.confs, segwit, utxo.amount, node)
		}

		loadTxsIntoNode(txs, test.fees, test.confs, node, test.txTimeWithinLimit, t)

		testAccelerateOrder := func() {
			change, txID, err := wallet.AccelerateOrder(swapCoins, accelerations, changeCoin, test.requiredForRemainingSwaps, test.newFeeRate)
			if test.expectAccelerateOrderErr {
				if err == nil {
					t.Fatalf("%s: expected AccelerateOrder error but did not get", test.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", test.name, err)
			}

			in, out := totalInputOutput(node.sentRawTx)
			lastTxFee := in - out
			if expectedFees != lastTxFee {
				t.Fatalf("%s: expected fee to be %d but got %d", test.name, expectedFees, lastTxFee)
			}

			if node.sentRawTx.TxHash().String() != txID {
				t.Fatalf("%s: expected tx id %s, but got %s", test.name, node.sentRawTx.TxHash().String(), txID)
			}

			if test.expectChange {
				if out == 0 {
					t.Fatalf("%s: expected change but did not get", test.name)
				}
				if change == nil {
					t.Fatalf("%s: expected change, but got nil", test.name)
				}

				changeScript := node.sentRawTx.TxOut[0].PkScript

				changeAddr, err := btcutil.DecodeAddress(node.changeAddr, &chaincfg.MainNetParams)
				if err != nil {
					t.Fatalf("%s: unexpected error: %v", test.name, err)
				}
				expectedChangeScript, err := txscript.PayToAddrScript(changeAddr)
				if err != nil {
					t.Fatalf("%s: unexpected error: %v", test.name, err)
				}

				if !bytes.Equal(changeScript, expectedChangeScript) {
					t.Fatalf("%s: expected change script != actual", test.name)
				}

				changeTxHash, changeVout, err := decodeCoinID(change.ID())
				if err != nil {
					t.Fatalf("%s: unexpected error: %v", test.name, err)
				}
				if *changeTxHash != node.sentRawTx.TxHash() {
					t.Fatalf("%s: change tx hash %s != expected: %s", test.name, changeTxHash, node.sentRawTx.TxHash())
				}
				if changeVout != 0 {
					t.Fatalf("%s: change vout %v != expected: 0", test.name, changeVout)
				}

				var changeLocked bool
				for _, coin := range node.lockedCoins {
					if changeVout == coin.Vout && changeTxHash.String() == coin.TxID {
						changeLocked = true
					}
				}
				if changeLocked != test.expectChangeLocked {
					t.Fatalf("%s: expected change locked = %v, but was %v", test.name, test.expectChangeLocked, changeLocked)
				}
			} else {
				if out > 0 {
					t.Fatalf("%s: not expecting change but got %v", test.name, out)
				}
				if change != nil {
					t.Fatalf("%s: not expecting change but accelerate returned: %+v", test.name, change)
				}
			}

			if test.requiredForRemainingSwaps > out {
				t.Fatalf("%s: %d needed of remaining swaps, but output was only %d", test.name, test.requiredForRemainingSwaps, out)
			}
		}
		testAccelerateOrder()

		testAccelerationEstimate := func() {
			estimate, err := wallet.AccelerationEstimate(swapCoins, accelerations, changeCoin, test.requiredForRemainingSwaps, test.newFeeRate)
			if test.expectAccelerationEstimateErr {
				if err == nil {
					t.Fatalf("%s: expected AccelerationEstimate error but did not get", test.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", test.name, err)
			}
			if estimate != expectedFees {
				t.Fatalf("%s: estimate %v != expected fees %v", test.name, estimate, expectedFees)
			}
		}
		testAccelerationEstimate()

		testPreAccelerate := func() {
			currentRate, suggestedRange, earlyAcceleration, err := wallet.PreAccelerate(swapCoins, accelerations, changeCoin, test.requiredForRemainingSwaps, test.suggestedFeeRate)
			if test.expectPreAccelerateErr {
				if err == nil {
					t.Fatalf("%s: expected PreAccelerate error but did not get", test.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", test.name, err)
			}

			if test.expectTooEarly != (earlyAcceleration != nil) {
				t.Fatalf("%s: expected early acceleration %v, but got %v", test.name, test.expectTooEarly, earlyAcceleration)
			}

			var totalSize, totalFee uint64
			for i, tx := range txs {
				if test.confs[i] == 0 {
					totalSize += MsgTxVBytes(tx)
					totalFee += toSatoshi(test.fees[i])
				}
			}

			expectedRate := totalFee / totalSize
			if expectedRate != currentRate {
				t.Fatalf("%s: expected current rate %v != actual %v", test.name, expectedRate, currentRate)
			}

			totalFeePossible := totalFee + uint64(test.changeAmount) - test.requiredForRemainingSwaps
			var numConfirmedUTXO int
			for _, utxo := range test.utxos {
				if utxo.confs > 0 {
					totalFeePossible += utxo.amount
					numConfirmedUTXO++
				}
			}
			totalSize += calculateChangeTxSize(test.expectChange, segwit, 1+numConfirmedUTXO)
			maxRatePossible := totalFeePossible / totalSize

			expectedRangeHigh := expectedRate * 5
			if test.suggestedFeeRate > expectedRate {
				expectedRangeHigh = test.suggestedFeeRate * 5
			}
			if maxRatePossible < expectedRangeHigh {
				expectedRangeHigh = maxRatePossible
			}

			expectedRangeLowX := float64(expectedRate+1) / float64(expectedRate)
			if suggestedRange.Start.X != expectedRangeLowX {
				t.Fatalf("%s: start of range should be %v on X, got: %v", test.name, expectedRangeLowX, suggestedRange.Start.X)
			}

			if suggestedRange.Start.Y != float64(expectedRate+1) {
				t.Fatalf("%s: expected start of range on Y to be: %v, but got %v", test.name, float64(expectedRate), suggestedRange.Start.Y)
			}

			if suggestedRange.End.Y != float64(expectedRangeHigh) {
				t.Fatalf("%s: expected end of range on Y to be: %v, but got %v", test.name, float64(expectedRangeHigh), suggestedRange.End.Y)
			}

			expectedRangeHighX := float64(expectedRangeHigh) / float64(expectedRate)
			if suggestedRange.End.X != expectedRangeHighX {
				t.Fatalf("%s: expected end of range on X to be: %v, but got %v", test.name, float64(expectedRate), suggestedRange.End.X)
			}
		}
		testPreAccelerate()
	}
}

func TestTooEarlyToAccelerate(t *testing.T) {
	type tx struct {
		secondsBeforeNow uint64
		confirmations    uint64
	}
	tests := []struct {
		name           string
		accelerations  []tx
		swaps          []tx
		expectedReturn *asset.EarlyAcceleration
		expectError    bool
	}{
		{
			name: "all confirmed",
			accelerations: []tx{
				{secondsBeforeNow: minTimeBeforeAcceleration + 800,
					confirmations: 2},
				{secondsBeforeNow: minTimeBeforeAcceleration + 300,
					confirmations: 1}},
			swaps: []tx{
				{secondsBeforeNow: minTimeBeforeAcceleration + 1000,
					confirmations: 2},
				{secondsBeforeNow: minTimeBeforeAcceleration + 500,
					confirmations: 1}},
			expectError: true,
		},
		{
			name:          "no accelerations, not too early",
			accelerations: []tx{},
			swaps: []tx{
				{secondsBeforeNow: minTimeBeforeAcceleration + 1000,
					confirmations: 2},
				{secondsBeforeNow: minTimeBeforeAcceleration + 500,
					confirmations: 0},
				{secondsBeforeNow: minTimeBeforeAcceleration + 300,
					confirmations: 0},
				{secondsBeforeNow: minTimeBeforeAcceleration + 800,
					confirmations: 2}},
		},
		{
			name: "acceleration after unconfirmed swap, not too early",
			accelerations: []tx{
				{secondsBeforeNow: minTimeBeforeAcceleration + 300,
					confirmations: 0}},
			swaps: []tx{
				{secondsBeforeNow: minTimeBeforeAcceleration + 1000,
					confirmations: 2},
				{secondsBeforeNow: minTimeBeforeAcceleration + 800,
					confirmations: 2},
				{secondsBeforeNow: minTimeBeforeAcceleration + 500,
					confirmations: 0}},
		},
		{
			name: "acceleration after unconfirmed swap, too early",
			accelerations: []tx{
				{secondsBeforeNow: minTimeBeforeAcceleration - 300,
					confirmations: 0}},
			swaps: []tx{
				{secondsBeforeNow: minTimeBeforeAcceleration + 1000,
					confirmations: 2},
				{secondsBeforeNow: minTimeBeforeAcceleration + 800,
					confirmations: 2},
				{secondsBeforeNow: minTimeBeforeAcceleration + 500,
					confirmations: 0}},
			expectedReturn: &asset.EarlyAcceleration{
				TimePast:       minTimeBeforeAcceleration - 300,
				WasAccelerated: true,
			},
		},
		{
			name:          "no accelerations, too early",
			accelerations: []tx{},
			swaps: []tx{
				{secondsBeforeNow: minTimeBeforeAcceleration + 1000,
					confirmations: 2},
				{secondsBeforeNow: minTimeBeforeAcceleration + 800,
					confirmations: 2},
				{secondsBeforeNow: minTimeBeforeAcceleration - 300,
					confirmations: 0},
				{secondsBeforeNow: minTimeBeforeAcceleration - 500,
					confirmations: 0}},
			expectedReturn: &asset.EarlyAcceleration{
				TimePast:       minTimeBeforeAcceleration - 300,
				WasAccelerated: false,
			},
		},
		{
			name: "only accelerations are unconfirmed, too early",
			accelerations: []tx{
				{secondsBeforeNow: minTimeBeforeAcceleration - 300,
					confirmations: 0}},
			swaps: []tx{
				{secondsBeforeNow: minTimeBeforeAcceleration + 1000,
					confirmations: 2},
				{secondsBeforeNow: minTimeBeforeAcceleration + 800,
					confirmations: 2},
			},
			expectedReturn: &asset.EarlyAcceleration{
				TimePast:       minTimeBeforeAcceleration - 300,
				WasAccelerated: true,
			},
		},
	}

	for _, test := range tests {
		swapTxs := make([]*GetTransactionResult, 0, len(test.swaps))
		accelerationTxs := make([]*GetTransactionResult, 0, len(test.accelerations))
		now := time.Now().Unix()

		for _, swap := range test.swaps {
			var txHash chainhash.Hash
			copy(txHash[:], encode.RandomBytes(32))
			swapTxs = append(swapTxs, &GetTransactionResult{
				TxID:          txHash.String(),
				Confirmations: swap.confirmations,
				Time:          uint64(now) - swap.secondsBeforeNow,
			})
		}

		for _, acceleration := range test.accelerations {
			var txHash chainhash.Hash
			copy(txHash[:], encode.RandomBytes(32))
			accelerationTxs = append(accelerationTxs, &GetTransactionResult{
				TxID:          txHash.String(),
				Confirmations: acceleration.confirmations,
				Time:          uint64(now) - acceleration.secondsBeforeNow,
			})
		}

		earlyAcceleration, err := tooEarlyToAccelerate(swapTxs, accelerationTxs)
		if test.expectError {
			if err == nil {
				t.Fatalf("%s: expected error but did not get", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}

		if test.expectedReturn == nil && earlyAcceleration == nil {
			continue
		}
		if test.expectedReturn == nil && earlyAcceleration != nil {
			t.Fatalf("%s: expected return to be nil, but got %+v", test.name, earlyAcceleration)
		}
		if test.expectedReturn != nil && earlyAcceleration == nil {
			t.Fatalf("%s: expected return to not be nil, but got nil", test.name)
		}
		if test.expectedReturn.TimePast != earlyAcceleration.TimePast ||
			test.expectedReturn.WasAccelerated != earlyAcceleration.WasAccelerated {
			t.Fatalf("%s: expected %+v, got %+v", test.name, test.expectedReturn, earlyAcceleration)
		}
	}
}
