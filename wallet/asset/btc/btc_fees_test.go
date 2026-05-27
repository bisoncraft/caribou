//go:build !spvlive && !harness

package btc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func TestEstimateSendTxFee(t *testing.T) {
	runRubric(t, testEstimateSendTxFee)
}

func testEstimateSendTxFee(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	addr := btcAddr(segwit)
	sendAddr := addr.String()
	node.changeAddr = btcAddr(segwit).String()
	pkScript, _ := txscript.PayToAddrScript(addr)

	unspentVal := 100 // BTC
	unspents := []*ListUnspentResult{{
		Address:       addr.String(),
		Amount:        float64(unspentVal),
		Confirmations: 1,
		Vout:          1,
		ScriptPubKey:  pkScript,
		SafePtr:       boolPtr(true),
		Spendable:     true,
	}}
	node.listUnspent = unspents
	var bals GetBalancesResult
	node.getBalances = &bals
	bals.Mine.Trusted = float64(unspentVal)
	unspentSats := toSatoshi(unspents[0].Amount)

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxOut(wire.NewTxOut(int64(unspentSats), pkScript))

	// single ouptput size.
	opSize := P2PKHOutputSize
	if segwit {
		opSize = P2WPKHOutputSize
	}

	// bSize is the size for a single input.
	witnessWeight := 4
	bSize := TxInOverhead +
		wire.VarIntSerializeSize(uint64(RedeemP2PKHSigScriptSize)) +
		RedeemP2PKHSigScriptSize + (witnessWeight-1)/witnessWeight
	if segwit {
		bSize = TxInOverhead + 1 + (RedeemP2WPKHInputWitnessWeight+
			(witnessWeight-1))/witnessWeight
	}

	txSize := bSize + tx.SerializeSize()
	minEstFee := optimalFeeRate * uint64(txSize)

	// This should return fee rate estimate for one output.
	node.txFee = minEstFee

	estimate, _, err := wallet.EstimateSendTxFee(sendAddr, unspentSats, optimalFeeRate, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if estimate != minEstFee {
		t.Fatalf("expected estimate to be %v, got %v)", minEstFee, estimate)
	}

	// This should return fee rate estimate for two output.
	minEstFeeWithEstChangeFee := uint64(txSize+opSize) * optimalFeeRate
	node.txFee = minEstFeeWithEstChangeFee
	estimate, _, err = wallet.EstimateSendTxFee(sendAddr, unspentSats/2, optimalFeeRate, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if estimate != minEstFeeWithEstChangeFee {
		t.Fatalf("expected estimate to be %v, got %v)", minEstFeeWithEstChangeFee, estimate)
	}

	dust := 0.00000016
	node.listUnspent[0].Amount += dust
	// This should return fee rate estimate for one output with dust added to fee.
	minFeeWithDust := minEstFee + toSatoshi(dust)
	node.txFee = minFeeWithDust
	estimate, _, err = wallet.EstimateSendTxFee(sendAddr, unspentSats, optimalFeeRate, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if estimate != minFeeWithDust {
		t.Fatalf("expected estimate to be %v, got %v)", minFeeWithDust, estimate)
	}

	// Invalid address
	_, valid, _ := wallet.EstimateSendTxFee("invalidsendAddr", unspentSats, optimalFeeRate, true, false)
	if valid {
		t.Fatal("Expected false for invalid send address")
	}

	// Successful estimation without an address
	_, _, err = wallet.EstimateSendTxFee("", unspentSats, optimalFeeRate, true, false)
	if err != nil {
		t.Fatalf("Error for estimation without an address: %v", err)
	}

	// Zero send amount
	_, _, err = wallet.EstimateSendTxFee(sendAddr, 0, optimalFeeRate, true, false)
	if err == nil {
		t.Fatal("Expected an error for zero send amount")
	}

	// Output value is dust.
	_, _, err = wallet.EstimateSendTxFee(sendAddr, 500, optimalFeeRate, true, false)
	if err == nil {
		t.Fatal("Expected an error for dust output")
	}
}

func TestGetTxFee(t *testing.T) {
	runRubric(t, testGetTxFee)
}

func testGetTxFee(t *testing.T, segwit bool, walletType string) {
	w, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	inputTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{{}},
		TxOut: []*wire.TxOut{
			{
				Value: 2e6,
			},
			{
				Value: 3e6,
			},
		},
	}
	txBytes, err := serializeMsgTx(inputTx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	node.getTransactionMap = map[string]*GetTransactionResult{
		"any": {
			Bytes: txBytes,
		},
	}

	tests := []struct {
		name        string
		tx          *wire.MsgTx
		expectErr   bool
		expectedFee uint64
		getTxErr    bool
	}{
		{
			name: "ok",
			tx: &wire.MsgTx{
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: wire.OutPoint{
						Hash:  inputTx.TxHash(),
						Index: 0,
					}},
					{PreviousOutPoint: wire.OutPoint{
						Hash:  inputTx.TxHash(),
						Index: 1,
					}},
				},
				TxOut: []*wire.TxOut{{
					Value: 4e6,
				}},
			},
			expectedFee: 1e6,
		},
		{
			name: "get transaction error",
			tx: &wire.MsgTx{
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: wire.OutPoint{
						Hash:  inputTx.TxHash(),
						Index: 0,
					}},
					{PreviousOutPoint: wire.OutPoint{
						Hash:  inputTx.TxHash(),
						Index: 1,
					}},
				},
				TxOut: []*wire.TxOut{{
					Value: 4e6,
				}},
			},
			getTxErr:  true,
			expectErr: true,
		},
		{
			name: "invalid prev output index error",
			tx: &wire.MsgTx{
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: wire.OutPoint{
						Hash:  inputTx.TxHash(),
						Index: 0,
					}},
					{PreviousOutPoint: wire.OutPoint{
						Hash:  inputTx.TxHash(),
						Index: 2,
					}},
				},
				TxOut: []*wire.TxOut{{
					Value: 4e6,
				}},
			},
			expectErr: true,
		},
		{
			name: "tx out > in error",
			tx: &wire.MsgTx{
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: wire.OutPoint{
						Hash:  inputTx.TxHash(),
						Index: 0,
					}},
					{PreviousOutPoint: wire.OutPoint{
						Hash:  inputTx.TxHash(),
						Index: 2,
					}},
				},
				TxOut: []*wire.TxOut{{
					Value: 8e6,
				}},
			},
			expectErr: true,
		},
	}

	for _, test := range tests {
		node.getTransactionErr = nil
		if test.getTxErr {
			node.getTransactionErr = errors.New("")
		}

		fee, err := w.getTxFee(test.tx)
		if test.expectErr {
			if err == nil {
				t.Fatalf("%s: expected error but did not get", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", test.name, err)
		}
		if fee != test.expectedFee {
			t.Fatalf("%s: expected fee %d, but got %d", test.name, test.expectedFee, fee)
		}
	}
}

func TestFeeRateCache(t *testing.T) {
	const okRate = 1
	var n int
	feeRateOK := func(context.Context, util.Network) (uint64, error) {
		n++
		return okRate, nil
	}
	err1 := errors.New("test error 1")
	feeRateBad := func(context.Context, util.Network) (uint64, error) {
		n++
		return 0, err1
	}
	c := &feeRateCache{f: feeRateOK}
	r, err := c.rate(context.Background(), util.Mainnet)
	if err != nil {
		t.Fatalf("rate error: %v", err)
	}
	if r != okRate {
		t.Fatalf("expected rate %d, got %d", okRate, r)
	}
	if n != 1 {
		t.Fatal("counter not incremented")
	}

	// Again should hit cache
	r, err = c.rate(context.Background(), util.Mainnet)
	if err != nil {
		t.Fatalf("cached rate error: %v", err)
	}
	if r != okRate {
		t.Fatalf("expected cached rate %d, got %d", okRate, r)
	}
	if n != 1 {
		t.Fatal("counter incremented")
	}

	// Expire cache
	c.fetchStamp = time.Now().Add(-time.Minute * 5) // const defaultShelfLife
	r, err = c.rate(context.Background(), util.Mainnet)
	if err != nil {
		t.Fatalf("fresh rate error: %v", err)
	}
	if r != okRate {
		t.Fatalf("expected fresh rate %d, got %d", okRate, r)
	}
	if n != 2 {
		t.Fatal("counter not incremented for fresh rate")
	}

	c.fetchStamp = time.Time{}
	c.f = feeRateBad
	_, err = c.rate(context.Background(), util.Mainnet)
	if err == nil {
		t.Fatal("error not propagated")
	}
	if n != 3 {
		t.Fatal("counter not incremented for error")
	}

	// Again should hit error cache.
	c.f = feeRateOK
	_, err = c.rate(context.Background(), util.Mainnet)
	if err == nil {
		t.Fatal("error not cached")
	}
	if n != 3 {
		t.Fatal("counter incremented for cached error")
	}

	// expire error
	c.errorStamp = time.Now().Add(-time.Minute) // const errorDelay
	r, err = c.rate(context.Background(), util.Mainnet)
	if err != nil {
		t.Fatalf("recovered rate error: %v", err)
	}
	if r != okRate {
		t.Fatalf("expected recovered rate %d, got %d", okRate, r)
	}
	if n != 4 {
		t.Fatal("counter not incremented for recovered rate")
	}
}
