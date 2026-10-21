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
// transaction. It is the single point to move to the Dash signing entrypoint
// once one exists; today the only entrypoint takes a wire.MsgTx, which has no
// room for the type or the payload.
func signDashInputs(t *testing.T, tx *bisonwire.Tx, pkScript []byte) {
	t.Helper()

	secrets := &stubSecrets{priv: fixedKey(t), compressed: true, params: &chaincfg.MainNetParams}
	err := AddAllInputScripts(&tx.MsgTx, [][]byte{pkScript},
		[]btcutil.Amount{dashTestInputValue}, secrets)
	if err != nil {
		t.Fatalf("sign: %v", err)
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
