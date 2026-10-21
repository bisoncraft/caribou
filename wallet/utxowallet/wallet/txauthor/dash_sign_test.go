// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package txauthor

import (
	"bytes"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const dashTestInputValue = 150_000_000_000

// dashSpecialTx returns an unsigned DIP-2 special transaction (version 3) with
// one P2PKH input and the given type and payload, plus the input's pkScript.
func dashSpecialTx(t *testing.T, txType uint16, payload []byte) (*bisonwire.Tx, []byte) {
	t.Helper()

	addr, err := btcutil.NewAddressPubKeyHash(
		btcutil.Hash160(fixedKey(t).PubKey().SerializeCompressed()),
		&chaincfg.MainNetParams)
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
		Type:         txType,
		ExtraPayload: payload,
	}
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0},
		Sequence:         0xffffffff,
	})
	tx.AddTxOut(&wire.TxOut{Value: dashTestInputValue - 1000, PkScript: pkScript})
	return tx, pkScript
}

// signDashInputs signs tx's inputs the way the wallet does for a Dash
// transaction.
func signDashInputs(t *testing.T, tx *bisonwire.Tx, pkScript []byte) {
	t.Helper()

	secrets := &stubSecrets{priv: fixedKey(t), compressed: true, params: &chaincfg.MainNetParams}
	if err := AddAllDashInputScripts(tx, [][]byte{pkScript}, secrets); err != nil {
		t.Fatalf("sign: %v", err)
	}
}

// TestDashSignatureHashMatchesBitcoinForNormalTx pins the part of the Dash
// digest that is checkable without a Dash node: for a normal transaction it
// must equal btcd's legacy digest, and the resulting signature must satisfy
// btcd's script engine. This guards the field order and hashing of the
// shared preimage; the payload placement is covered by the commitment test.
func TestDashSignatureHashMatchesBitcoinForNormalTx(t *testing.T) {
	t.Parallel()

	tx, pkScript := dashSpecialTx(t, 0, nil)

	got, err := dashSignatureHash(tx, 0, pkScript)
	if err != nil {
		t.Fatal(err)
	}
	want, err := txscript.CalcSignatureHash(pkScript, txscript.SigHashAll, &tx.MsgTx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("digest = %x, want %x", got, want)
	}

	signDashInputs(t, tx, pkScript)
	verifyInput(t, &tx.MsgTx, pkScript, dashTestInputValue)
}

func TestAddAllDashInputScriptsRejectsBadInputs(t *testing.T) {
	t.Parallel()

	secrets := &stubSecrets{priv: fixedKey(t), compressed: true, params: &chaincfg.MainNetParams}
	tx, pkScript := dashSpecialTx(t, 1, []byte{0x01})

	if err := AddAllDashInputScripts(tx, nil, secrets); err == nil {
		t.Error("expected an error for a missing previous script")
	}
	p2wpkh := append([]byte{0x00, 0x14}, make([]byte, 20)...)
	if err := AddAllDashInputScripts(tx, [][]byte{p2wpkh}, secrets); err == nil {
		t.Error("expected an error for a non-p2pkh input")
	}
	if err := AddAllDashInputScripts(tx, [][]byte{pkScript}, secrets); err != nil {
		t.Errorf("p2pkh input: %v", err)
	}
}

// TestDashInputSignatureCommitsToTypeAndPayload guards the invariant that
// makes a special transaction safe to sign: Dash's sighash covers the packed
// version|type and the extra payload, so an input signature must change when
// either does. A signature that ignores them is invalid on the network for a
// special transaction, and would let the payload be swapped after signing.
//
// This asserts only the commitment, not Dash's exact sighash algorithm. A
// known-answer test against a transaction signed by dashd belongs with the
// fix, once such a vector exists.
func TestDashInputSignatureCommitsToTypeAndPayload(t *testing.T) {
	t.Parallel()

	signed := func(txType uint16, payload []byte) []byte {
		tx, pkScript := dashSpecialTx(t, txType, payload)
		signDashInputs(t, tx, pkScript)
		return tx.TxIn[0].SignatureScript
	}

	base := signed(1, []byte{0x01, 0x02, 0x03})

	tests := []struct {
		name    string
		txType  uint16
		payload []byte
	}{
		{"different payload", 1, []byte{0x01, 0x02, 0x04}},
		{"different type", 3, []byte{0x01, 0x02, 0x03}},
		{"no payload", 1, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if bytes.Equal(base, signed(tc.txType, tc.payload)) {
				t.Fatal("input signature is identical, so it does not commit to the type and payload")
			}
		})
	}
}
