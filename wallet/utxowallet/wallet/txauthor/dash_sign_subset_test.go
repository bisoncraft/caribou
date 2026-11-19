// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package txauthor

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// sharedCollateralScript is the output a shared masternode's collateral pays:
// 0x04 "DSHC" OP_DROP OP_TRUE. Only a ProDisTx may spend it, with no signature
// script.
var sharedCollateralScript = []byte{0x04, 'D', 'S', 'H', 'C', txscript.OP_DROP, txscript.OP_TRUE}

// sharedFundingTx returns an unsigned version 3 transaction with the shape of
// a shared masternode transaction: inputs 0 and 2 spend P2PKH outputs of the
// test key and input 1 spends the shared collateral. It also returns the
// P2PKH script.
func sharedFundingTx(t *testing.T, txType uint16) (*bisonwire.Tx, []byte) {
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
		ExtraPayload: []byte{0x01, 0x02, 0x03},
	}
	for i := range 3 {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{byte(i + 1)}, Index: uint32(i)},
			Sequence:         0xffffffff,
		})
	}
	tx.AddTxOut(&wire.TxOut{Value: dashTestInputValue - 1000, PkScript: pkScript})
	return tx, pkScript
}

func stubKeySecrets(t *testing.T) *stubSecrets {
	t.Helper()
	return &stubSecrets{priv: fixedKey(t), compressed: true, params: &chaincfg.MainNetParams}
}

// signatureScripts returns a copy of every input's signature script.
func signatureScripts(tx *bisonwire.Tx) [][]byte {
	out := make([][]byte, len(tx.TxIn))
	for i, in := range tx.TxIn {
		out[i] = bytes.Clone(in.SignatureScript)
	}
	return out
}

// requireDashSignature checks that input idx's signature script holds a
// SIGHASH_ALL signature by the test key over Dash's digest.
func requireDashSignature(t *testing.T, tx *bisonwire.Tx, idx int, pkScript []byte) {
	t.Helper()

	pushes, err := txscript.PushedData(tx.TxIn[idx].SignatureScript)
	if err != nil || len(pushes) != 2 {
		t.Fatalf("input %d: signature script pushes = %d, err = %v", idx, len(pushes), err)
	}
	sigBytes, pubBytes := pushes[0], pushes[1]
	if sigBytes[len(sigBytes)-1] != byte(txscript.SigHashAll) {
		t.Fatalf("input %d: not SIGHASH_ALL", idx)
	}
	sig, err := ecdsa.ParseDERSignature(sigBytes[:len(sigBytes)-1])
	if err != nil {
		t.Fatalf("input %d: %v", idx, err)
	}
	pub, err := btcec.ParsePubKey(pubBytes)
	if err != nil {
		t.Fatalf("input %d: %v", idx, err)
	}
	if !pub.IsEqual(fixedKey(t).PubKey()) {
		t.Fatalf("input %d: signed with another key", idx)
	}
	hash, err := dashSignatureHash(tx, idx, pkScript)
	if err != nil {
		t.Fatal(err)
	}
	if !sig.Verify(hash, pub) {
		t.Fatalf("input %d: signature does not verify against dashSignatureHash", idx)
	}
}

// TestAddDashInputScriptsSignsOnlyListedInputs pins what the function is for:
// the caller signs its own input of a transaction that also holds another
// participant's signed input and the shared collateral. Another participant's
// signature must survive, and the collateral input, which is not listed, must
// keep its empty signature script.
func TestAddDashInputScriptsSignsOnlyListedInputs(t *testing.T) {
	t.Parallel()

	for _, txType := range []uint16{0, 1} {
		t.Run(fmt.Sprintf("type %d", txType), func(t *testing.T) {
			t.Parallel()

			tx, pkScript := sharedFundingTx(t, txType)
			other := []byte{0xde, 0xad, 0xbe, 0xef}
			tx.TxIn[0].SignatureScript = bytes.Clone(other)

			if err := AddDashInputScripts(tx, map[int][]byte{2: pkScript}, stubKeySecrets(t)); err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(tx.TxIn[0].SignatureScript, other) {
				t.Error("another participant's signature script was changed")
			}
			if len(tx.TxIn[1].SignatureScript) != 0 {
				t.Error("the collateral input must keep an empty signature script")
			}
			requireDashSignature(t, tx, 2, pkScript)

			// For a normal transaction Dash's digest is Bitcoin's, so btcd
			// is an independent oracle for the exact bytes (RFC 6979 makes
			// the signature deterministic).
			if txType == 0 {
				want, err := txscript.SignatureScript(&tx.MsgTx, 2, pkScript,
					txscript.SigHashAll, fixedKey(t), true)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(tx.TxIn[2].SignatureScript, want) {
					t.Errorf("signature script %x, btcd signs %x", tx.TxIn[2].SignatureScript, want)
				}
			}
		})
	}
}

// TestAddDashInputScriptsOrderDoesNotMatter pins that participants can sign in
// any order: a signature commits to the outpoints, outputs and payload but not
// to the other inputs' signature scripts, so one signing first does not
// invalidate the other.
func TestAddDashInputScriptsOrderDoesNotMatter(t *testing.T) {
	t.Parallel()

	signed := func(t *testing.T, steps ...map[int][]byte) [][]byte {
		t.Helper()
		tx, _ := sharedFundingTx(t, 1)
		for _, step := range steps {
			if err := AddDashInputScripts(tx, step, stubKeySecrets(t)); err != nil {
				t.Fatal(err)
			}
		}
		return signatureScripts(tx)
	}
	_, pk := sharedFundingTx(t, 1)
	first, second := map[int][]byte{0: pk}, map[int][]byte{2: pk}

	inOrder := signed(t, first, second)
	reversed := signed(t, second, first)
	together := signed(t, map[int][]byte{0: pk, 2: pk})
	for i := range inOrder {
		if !bytes.Equal(inOrder[i], reversed[i]) || !bytes.Equal(inOrder[i], together[i]) {
			t.Errorf("input %d differs with the signing order:\n in order %x\n reversed %x\n together %x",
				i, inOrder[i], reversed[i], together[i])
		}
	}
	if len(inOrder[0]) == 0 || len(inOrder[2]) == 0 || len(inOrder[1]) != 0 {
		t.Errorf("expected inputs 0 and 2 signed and 1 empty, got %x", inOrder)
	}
}

// failingSecrets is a SecretsSource whose key lookup fails, as a locked
// wallet's does.
type failingSecrets struct{ *stubSecrets }

func (failingSecrets) GetKey(btcutil.Address) (*btcec.PrivateKey, bool, error) {
	return nil, false, errors.New("wallet is locked")
}

// TestAddDashInputScriptsRejects pins that a bad request is an error and never
// a half-signed transaction: nothing may be signed if any listed input fails,
// another participant's signature is never replaced, and a failed key lookup is
// not read as "not mine".
func TestAddDashInputScriptsRejects(t *testing.T) {
	t.Parallel()

	p2wpkh := append([]byte{0x00, 0x14}, make([]byte, 20)...)
	// otherKey is a key that does not own the transaction's P2PKH output.
	otherKey := func(t *testing.T) SecretsSource {
		var b [32]byte
		for i := range b {
			b[i] = byte(0x80 + i)
		}
		priv, _ := btcec.PrivKeyFromBytes(b[:])
		return &stubSecrets{priv: priv, compressed: true, params: &chaincfg.MainNetParams}
	}
	// uncompressed returns the right key in the wrong form: the output pays
	// the hash of the compressed public key.
	uncompressed := func(t *testing.T) SecretsSource {
		return &stubSecrets{priv: fixedKey(t), compressed: false, params: &chaincfg.MainNetParams}
	}
	tests := []struct {
		name    string
		prefill map[int][]byte // signature scripts already on the transaction
		secrets func(t *testing.T) SecretsSource
		// prev is built from the transaction's P2PKH script.
		prev func(pk []byte) map[int][]byte
		// wantErr is a part of the error, so a case cannot pass for another
		// reason than the one it names.
		wantErr string
	}{
		{"no inputs", nil, nil, func([]byte) map[int][]byte { return nil }, "no inputs to sign"},
		{"negative index", nil, nil, func(pk []byte) map[int][]byte { return map[int][]byte{-1: pk} }, "out of range"},
		{"index past the last input", nil, nil, func(pk []byte) map[int][]byte { return map[int][]byte{3: pk} }, "out of range"},
		{"shared collateral listed", nil, nil, func([]byte) map[int][]byte {
			return map[int][]byte{1: sharedCollateralScript}
		}, "only p2pkh"},
		{"p2wpkh listed", nil, nil, func([]byte) map[int][]byte { return map[int][]byte{2: p2wpkh} }, "only p2pkh"},
		{"input already signed", map[int][]byte{2: {0x01}}, nil, func(pk []byte) map[int][]byte {
			return map[int][]byte{2: pk}
		}, "already has a signature script"},
		{"key lookup fails", nil, func(t *testing.T) SecretsSource {
			return failingSecrets{stubKeySecrets(t)}
		}, func(pk []byte) map[int][]byte { return map[int][]byte{2: pk} }, "wallet is locked"},
		{"key of another address", nil, otherKey,
			func(pk []byte) map[int][]byte { return map[int][]byte{2: pk} }, "key does not match"},
		{"key in the wrong form", nil, uncompressed,
			func(pk []byte) map[int][]byte { return map[int][]byte{2: pk} }, "key does not match"},
		// Input 1 is valid to sign and is walked before input 2 fails.
		{"failure after a valid input", nil, nil, func(pk []byte) map[int][]byte {
			return map[int][]byte{1: pk, 2: p2wpkh}
		}, "only p2pkh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tx, pkScript := sharedFundingTx(t, 1)
			for i, script := range tc.prefill {
				tx.TxIn[i].SignatureScript = script
			}
			var secrets SecretsSource = stubKeySecrets(t)
			if tc.secrets != nil {
				secrets = tc.secrets(t)
			}
			before, hash := signatureScripts(tx), tx.TxHash()

			err := AddDashInputScripts(tx, tc.prev(pkScript), secrets)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q, want one containing %q", err, tc.wantErr)
			}

			after := signatureScripts(tx)
			for i := range before {
				if !bytes.Equal(before[i], after[i]) {
					t.Errorf("input %d signature script changed on a failed call: %x to %x",
						i, before[i], after[i])
				}
			}
			if tx.TxHash() != hash {
				t.Error("transaction hash changed on a failed call")
			}
		})
	}
}

// TestAddAllDashInputScriptsErrorLeavesTxUnchanged pins that an error on a later
// input does not leave the earlier ones signed, as for AddDashInputScripts.
func TestAddAllDashInputScriptsErrorLeavesTxUnchanged(t *testing.T) {
	t.Parallel()

	tx, pkScript := sharedFundingTx(t, 1)
	before := signatureScripts(tx)
	// Input 1 spends a script that is not P2PKH, so signing fails after input 0.
	prev := [][]byte{pkScript, {0x51}, pkScript}

	err := AddAllDashInputScripts(tx, prev, stubKeySecrets(t))
	if err == nil || !strings.Contains(err.Error(), "input 1") {
		t.Fatalf("error %v, want one for input 1", err)
	}
	for i, got := range signatureScripts(tx) {
		if !bytes.Equal(got, before[i]) {
			t.Errorf("input %d was signed although signing failed", i)
		}
	}
}

// TestAddAllDashInputScriptsRejectsMismatchedKey pins that signing every input
// has the same protection: a key that does not own the output is an error, not
// a signature script that looks right and fails only on the network.
func TestAddAllDashInputScriptsRejectsMismatchedKey(t *testing.T) {
	t.Parallel()

	tx, pkScript := dashSpecialTx(t, 1, []byte{0x01})
	wrongForm := &stubSecrets{priv: fixedKey(t), compressed: false, params: &chaincfg.MainNetParams}

	err := AddAllDashInputScripts(tx, [][]byte{pkScript}, wrongForm)
	if err == nil || !strings.Contains(err.Error(), "key does not match") {
		t.Fatalf("error %v, want one about a key that does not match", err)
	}
	if len(tx.TxIn[0].SignatureScript) != 0 {
		t.Error("an input was signed with a key that does not match")
	}
}
