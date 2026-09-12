//go:build !harness

package btc

import "testing"

func TestOutputAndElectrumCoin(t *testing.T) {
	op := NewOutput(tTxHash, 2, 42)
	if op.String() != tTxID+":2" || op.TxID() != tTxID || op.Value() != 42 || op.WireOutPoint().Index != 2 || len(op.ID()) != 36 {
		t.Fatalf("output = %+v", op)
	}
	converted, err := ConvertCoin(op)
	if err != nil || converted != op {
		t.Fatalf("ConvertCoin output = %v, %v", converted, err)
	}
	coin := &electrumCoin{txHash: *tTxHash, vout: 3, value: 7}
	converted, err = ConvertCoin(coin)
	if err != nil || converted.String() != tTxID+":3" || converted.Value() != 7 {
		t.Fatalf("ConvertCoin electrum = %v, %v", converted, err)
	}
}
