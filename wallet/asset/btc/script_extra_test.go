//go:build !harness

package btc

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire"
)

func TestTransactionSizingAndPubKeyHash(t *testing.T) {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1, tP2PKH))
	if MsgTxVBytes(tx) == 0 {
		t.Fatal("zero virtual transaction size")
	}
	nfo := &SpendInfo{SigScriptSize: 10, WitnessSize: 11}
	if nfo.VBytes() <= TxInOverhead {
		t.Fatalf("input vbytes = %d", nfo.VBytes())
	}
	if got := ExtractPubKeyHash(tP2PKH); !bytes.Equal(got, tP2PKH[3:23]) {
		t.Fatalf("pubkey hash = %x", got)
	}
	if ExtractPubKeyHash(nil) != nil {
		t.Fatal("invalid script produced pubkey hash")
	}
}
