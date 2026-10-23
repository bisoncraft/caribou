// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package txsizes_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet/txauthor"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet/txsizes"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// keySecrets serves one compressed key for every address.
type keySecrets struct{ priv *btcec.PrivateKey }

func (s keySecrets) GetKey(btcutil.Address) (*btcec.PrivateKey, bool, error) {
	return s.priv, true, nil
}

func (keySecrets) GetScript(btcutil.Address) ([]byte, error) {
	return nil, fmt.Errorf("no scripts")
}

func (keySecrets) ChainParams() *chaincfg.Params { return &chaincfg.MainNetParams }

// TestEstimateSerializeSizeSpecialCoversSignedTx guards fee safety: the fee for
// a special transaction is priced from the estimate, so an estimate below the
// real signed size would produce a tx under the relay fee rate. It signs real
// transactions with the wallet's Dash signer, across ECDSA signatures whose DER
// length varies, and requires the estimate to be at least the actual size and
// no more than one worst-case signature slack per input above it.
func TestEstimateSerializeSizeSpecialCoversSignedTx(t *testing.T) {
	t.Parallel()

	for _, payloadLen := range []int{32, 252, 253, 400} {
		for inputs := 1; inputs <= 5; inputs++ {
			for _, change := range []bool{false, true} {
				name := fmt.Sprintf("payload%d/in%d/change%v", payloadLen, inputs, change)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					checkEstimate(t, payloadLen, inputs, change)
				})
			}
		}
	}
}

func checkEstimate(t *testing.T, payloadLen, inputs int, change bool) {
	t.Helper()

	// Several keys so the DER signature length is exercised.
	for k := byte(1); k <= 16; k++ {
		priv, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{k}, 32))
		addr, err := btcutil.NewAddressPubKeyHash(
			btcutil.Hash160(priv.PubKey().SerializeCompressed()), &chaincfg.MainNetParams)
		if err != nil {
			t.Fatal(err)
		}
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			t.Fatal(err)
		}

		tx := &bisonwire.Tx{
			MsgTx:        *wire.NewMsgTx(3),
			Chain:        "dash",
			Type:         1,
			ExtraPayload: bytes.Repeat([]byte{0xab}, payloadLen),
		}
		prevScripts := make([][]byte, inputs)
		for i := range inputs {
			tx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{Hash: [32]byte{byte(i + 1), k}, Index: uint32(i)},
				Sequence:         0xffffffff,
			})
			prevScripts[i] = pkScript
		}
		tx.AddTxOut(&wire.TxOut{Value: 100_000_000_000, PkScript: pkScript})
		if change {
			tx.AddTxOut(&wire.TxOut{Value: 5_000_000, PkScript: pkScript})
		}

		if err := txauthor.AddAllDashInputScripts(tx, prevScripts, keySecrets{priv}); err != nil {
			t.Fatalf("sign: %v", err)
		}

		// The estimate takes the non-change outputs and adds change itself.
		outs := tx.TxOut
		if change {
			outs = outs[:1]
		}
		got := txsizes.EstimateSerializeSizeSpecial(inputs, outs, change, payloadLen)
		actual := tx.SerializeSize()
		if got < actual {
			t.Fatalf("key %d: estimate %d is below the signed size %d", k, got, actual)
		}
		// The estimate budgets 73 bytes for a signature and its hash type.
		// btcec signs low-S, so a real one is 70-72 bytes and the slack
		// averages 1-2 bytes per input. Signing is deterministic (RFC6979)
		// with fixed keys and outpoints, so the bound cannot flake.
		if got-actual > 3*inputs {
			t.Fatalf("key %d: estimate %d overshoots the signed size %d by more than %d",
				k, got, actual, 3*inputs)
		}
	}
}

// TestEstimateSerializeSizeSpecialAddsPayload pins the formula: a special tx
// costs exactly the payload and its length prefix more than the same normal tx.
func TestEstimateSerializeSizeSpecialAddsPayload(t *testing.T) {
	t.Parallel()

	outs := []*wire.TxOut{{Value: 1, PkScript: make([]byte, txsizes.P2PKHPkScriptSize)}}
	base := txsizes.EstimateSerializeSize(2, outs, true)
	tests := []struct{ payload, extra int }{
		{0, 1}, {32, 33}, {252, 253}, {253, 256}, {65535, 65538},
	}
	for _, tc := range tests {
		got := txsizes.EstimateSerializeSizeSpecial(2, outs, true, tc.payload)
		if got-base != tc.extra {
			t.Errorf("payload %d: added %d bytes, want %d", tc.payload, got-base, tc.extra)
		}
	}
}
