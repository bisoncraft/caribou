// Copyright 2026 The Boss Wallet developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package txauthor

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// stubSecrets is the minimal SecretsSource needed by AddAllInputScripts: it
// hands back a single key whenever asked, ignores ScriptDB lookups, and
// returns whatever chaincfg the test asked for.
type stubSecrets struct {
	priv       *btcec.PrivateKey
	compressed bool
	params     *chaincfg.Params
}

func (s *stubSecrets) GetKey(_ btcutil.Address) (*btcec.PrivateKey, bool, error) {
	return s.priv, s.compressed, nil
}

func (s *stubSecrets) GetScript(_ btcutil.Address) ([]byte, error) {
	return nil, errors.New("no scripts: this stub never holds redeem scripts")
}

func (s *stubSecrets) ChainParams() *chaincfg.Params {
	return s.params
}

// fixedKey is a determined-by-test private key. We construct one once and
// reuse it across the address-type variants below; using a fixed scalar keeps
// the signatures' sighashes deterministic.
func fixedKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	var b [32]byte
	for i := range b {
		b[i] = byte(i + 1) // 0x01 0x02 ... 0x20 — non-zero, on-curve
	}
	priv, _ := btcec.PrivKeyFromBytes(b[:])
	return priv
}

// buildSpendingTx constructs a single-input, single-output transaction that
// spends a synthetic prev-output worth `inputValue` satoshis with the given
// pkScript, and pays `inputValue/2` to a throw-away P2WPKH output. The
// returned tx has no signature data; caller is expected to fill it in.
func buildSpendingTx(t *testing.T, inputValue int64) (*wire.MsgTx, *wire.TxOut) {
	t.Helper()

	tx := wire.NewMsgTx(2)
	prevOut := wire.OutPoint{Index: 0}
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: prevOut, Sequence: 0xffffffff})

	// Pay-to-anyone destination — a 22-byte P2WPKH placeholder.
	dest := make([]byte, 22)
	dest[0] = 0x00
	dest[1] = 0x14
	tx.AddTxOut(&wire.TxOut{Value: inputValue / 2, PkScript: dest})

	return tx, &wire.TxOut{Value: inputValue}
}

// verifyInput executes the txscript engine against the freshly-signed input
// to confirm the witness/sigScript actually unlocks the prev-output.
func verifyInput(t *testing.T, tx *wire.MsgTx, prevPkScript []byte, prevValue int64) {
	t.Helper()
	prev := txscript.NewCannedPrevOutputFetcher(prevPkScript, prevValue)
	hashCache := txscript.NewTxSigHashes(tx, prev)

	flags := txscript.StandardVerifyFlags
	engine, err := txscript.NewEngine(
		prevPkScript, tx, 0, flags,
		nil, hashCache, prevValue, prev,
	)
	if err != nil {
		t.Fatalf("txscript.NewEngine: %v", err)
	}
	if err := engine.Execute(); err != nil {
		t.Fatalf("script verification failed: %v", err)
	}
}

// TestAddAllInputScriptsP2WPKH covers spendWitnessKeyHash: the BIP-141
// pay-to-witness-pubkey-hash signing path used for native segwit v0 inputs.
func TestAddAllInputScriptsP2WPKH(t *testing.T) {
	params := &chaincfg.MainNetParams
	priv := fixedKey(t)
	pub := priv.PubKey()

	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(pub.SerializeCompressed()), params,
	)
	if err != nil {
		t.Fatalf("NewAddressWitnessPubKeyHash: %v", err)
	}
	prevPkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript: %v", err)
	}

	const inputValue int64 = 1_0000_0000 // 1 BTC
	tx, _ := buildSpendingTx(t, inputValue)

	secrets := &stubSecrets{priv: priv, compressed: true, params: params}
	if err := AddAllInputScripts(
		tx, [][]byte{prevPkScript},
		[]btcutil.Amount{btcutil.Amount(inputValue)},
		secrets,
	); err != nil {
		t.Fatalf("AddAllInputScripts: %v", err)
	}
	if len(tx.TxIn[0].Witness) == 0 {
		t.Fatal("AddAllInputScripts left an empty witness for P2WPKH input")
	}
	if len(tx.TxIn[0].SignatureScript) != 0 {
		t.Fatal("P2WPKH input should not have a sigScript")
	}

	verifyInput(t, tx, prevPkScript, inputValue)
}

// TestAddAllInputScriptsP2TR covers spendTaprootKey: the BIP-341 key-path
// taproot signing.
func TestAddAllInputScriptsP2TR(t *testing.T) {
	params := &chaincfg.MainNetParams
	priv := fixedKey(t)
	pub := priv.PubKey()

	// BIP-86 derives the output key directly from the internal key with no
	// script-tree commitment: tweak with the empty script-merkle-root.
	tapKey := txscript.ComputeTaprootKeyNoScript(pub)
	addr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(tapKey), params,
	)
	if err != nil {
		t.Fatalf("NewAddressTaproot: %v", err)
	}
	prevPkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript: %v", err)
	}

	const inputValue int64 = 2_0000_0000 // 2 BTC
	tx, _ := buildSpendingTx(t, inputValue)

	// txscript.TaprootWitnessSignature applies the BIP-86 tweak before
	// signing, so the SecretsSource holds the *internal* key.
	secrets := &stubSecrets{priv: priv, compressed: true, params: params}
	if err := AddAllInputScripts(
		tx, [][]byte{prevPkScript},
		[]btcutil.Amount{btcutil.Amount(inputValue)},
		secrets,
	); err != nil {
		t.Fatalf("AddAllInputScripts: %v", err)
	}
	// BIP-341 key-path spends should produce exactly one 64-byte schnorr
	// signature in the witness stack.
	if got := len(tx.TxIn[0].Witness); got != 1 {
		t.Fatalf("taproot witness has %d elements, want 1", got)
	}
	if got := len(tx.TxIn[0].Witness[0]); got != 64 {
		t.Fatalf("schnorr signature is %d bytes, want 64", got)
	}

	verifyInput(t, tx, prevPkScript, inputValue)
}

// TestAddAllInputScriptsP2SHP2WPKH covers spendNestedWitnessPubKeyHash: the
// BIP-49 pay-to-script-hash-wrapping-pay-to-witness-pubkey-hash scheme.
func TestAddAllInputScriptsP2SHP2WPKH(t *testing.T) {
	params := &chaincfg.MainNetParams
	priv := fixedKey(t)
	pub := priv.PubKey()

	// First the inner P2WPKH program, then wrap it in P2SH.
	witAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(pub.SerializeCompressed()), params,
	)
	if err != nil {
		t.Fatalf("NewAddressWitnessPubKeyHash: %v", err)
	}
	witnessProgram, err := txscript.PayToAddrScript(witAddr)
	if err != nil {
		t.Fatalf("PayToAddrScript(P2WPKH): %v", err)
	}
	p2shAddr, err := btcutil.NewAddressScriptHash(witnessProgram, params)
	if err != nil {
		t.Fatalf("NewAddressScriptHash: %v", err)
	}
	prevPkScript, err := txscript.PayToAddrScript(p2shAddr)
	if err != nil {
		t.Fatalf("PayToAddrScript(P2SH): %v", err)
	}

	const inputValue int64 = 3_0000_0000 // 3 BTC
	tx, _ := buildSpendingTx(t, inputValue)

	secrets := &stubSecrets{priv: priv, compressed: true, params: params}
	if err := AddAllInputScripts(
		tx, [][]byte{prevPkScript},
		[]btcutil.Amount{btcutil.Amount(inputValue)},
		secrets,
	); err != nil {
		t.Fatalf("AddAllInputScripts: %v", err)
	}
	if len(tx.TxIn[0].Witness) == 0 {
		t.Fatal("nested P2WPKH input must have a witness stack")
	}
	if len(tx.TxIn[0].SignatureScript) == 0 {
		t.Fatal("nested P2WPKH input must have a sigScript wrapping the witness program")
	}

	verifyInput(t, tx, prevPkScript, inputValue)
}

// TestAddAllInputScriptsLengthMismatch pins the obvious error path:
// AddAllInputScripts must reject a prevPkScripts slice of the wrong length
// rather than silently signing the prefix or panicking.
func TestAddAllInputScriptsLengthMismatch(t *testing.T) {
	params := &chaincfg.MainNetParams
	priv := fixedKey(t)
	tx, _ := buildSpendingTx(t, 1_0000_0000) // single input

	secrets := &stubSecrets{priv: priv, compressed: true, params: params}
	err := AddAllInputScripts(
		tx, [][]byte{}, []btcutil.Amount{}, secrets,
	)
	if err == nil {
		t.Fatal("expected length-mismatch error, got nil")
	}
}
