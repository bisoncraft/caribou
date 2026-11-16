// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package txauthor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// dashSignatureHash returns the SIGHASH_ALL digest Dash signs for input idx.
// It is Bitcoin's legacy digest with two differences that matter for special
// transactions: the version field is the packed version|type, and the extra
// payload follows the lock time. For a normal transaction the two digests are
// identical.
//
// The payload placement follows Dash Core's CTransactionSignatureSerializer,
// and TestDashSignatureHashMatchesDashd verifies it against signatures on real
// mainnet special transactions.
func dashSignatureHash(tx *bisonwire.Tx, idx int, subScript []byte) ([]byte, error) {
	if idx < 0 || idx >= len(tx.TxIn) {
		return nil, fmt.Errorf("input index %d out of range", idx)
	}

	var buf bytes.Buffer
	var u32 [4]byte
	putU32 := func(v uint32) {
		binary.LittleEndian.PutUint32(u32[:], v)
		buf.Write(u32[:])
	}

	putU32(uint32(uint16(tx.Version)) | uint32(tx.Type)<<16)

	// Only the input being signed carries a script; the rest are empty.
	if err := wire.WriteVarInt(&buf, 0, uint64(len(tx.TxIn))); err != nil {
		return nil, err
	}
	for i, in := range tx.TxIn {
		buf.Write(in.PreviousOutPoint.Hash[:])
		putU32(in.PreviousOutPoint.Index)
		script := []byte(nil)
		if i == idx {
			script = subScript
		}
		if err := wire.WriteVarBytes(&buf, 0, script); err != nil {
			return nil, err
		}
		putU32(in.Sequence)
	}

	if err := wire.WriteVarInt(&buf, 0, uint64(len(tx.TxOut))); err != nil {
		return nil, err
	}
	for _, out := range tx.TxOut {
		var v [8]byte
		binary.LittleEndian.PutUint64(v[:], uint64(out.Value))
		buf.Write(v[:])
		if err := wire.WriteVarBytes(&buf, 0, out.PkScript); err != nil {
			return nil, err
		}
	}
	putU32(tx.LockTime)

	if tx.Version >= bisonwire.DashSpecialTxVersion && tx.Type != 0 {
		if err := wire.WriteVarBytes(&buf, 0, tx.ExtraPayload); err != nil {
			return nil, err
		}
	}

	putU32(uint32(txscript.SigHashAll))

	h := chainhash.DoubleHashB(buf.Bytes())
	return h, nil
}

// AddAllDashInputScripts adds a signature script to each input of a Dash
// transaction, signing with Dash's digest so that special transactions commit
// to their type and extra payload. Use it instead of AddAllInputScripts for any
// transaction that may be special. Dash wallets only hold P2PKH outputs, so any
// other previous output script is an error.
func AddAllDashInputScripts(tx *bisonwire.Tx, prevPkScripts [][]byte,
	secrets SecretsSource) error {

	if len(tx.TxIn) != len(prevPkScripts) {
		return errors.New("tx.TxIn and prevPkScripts slices must " +
			"have equal length")
	}

	for i, in := range tx.TxIn {
		script, err := dashInputScript(tx, i, prevPkScripts[i], secrets)
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
		in.SignatureScript = script
	}
	return nil
}

// AddDashInputScripts signs only the inputs whose indexes are keys of
// prevPkScripts, each mapped to the script of the output it spends, and leaves
// every other input as it is. Use it when the transaction has inputs this
// wallet does not own: the funding inputs of the other participants of a shared
// masternode registration, which they sign themselves, and the shared
// collateral a dissolution spends, which has no signature script at all (its
// signatures are in the payload).
//
// The caller names its own inputs, and the function never guesses: a key
// lookup that fails, for instance on a locked wallet, is an error and not a
// reason to skip an input. An input that is not listed is not signed, so the
// caller knows exactly which ones remain.
//
// An error leaves tx unchanged. It is returned for no inputs, an index out of
// range, a script that is not P2PKH (an OP_TRUE collateral listed by mistake
// included), and an input that already has a signature script, so another
// participant's signature is never overwritten. A signature changes if any
// part of the transaction does, so the transaction must be final before the
// first participant signs, and one whose inputs change has to be rebuilt.
//
// Every signature is SIGHASH_ALL and commits to the version and type, all
// inputs' outpoints and sequences, all outputs, the lock time and, for a
// special transaction, the extra payload. It does not commit to the signature
// scripts of other inputs, so participants can sign in any order.
func AddDashInputScripts(tx *bisonwire.Tx, prevPkScripts map[int][]byte,
	secrets SecretsSource) error {

	if len(prevPkScripts) == 0 {
		return errors.New("no inputs to sign")
	}

	// Compute every script before assigning any, so a failure leaves tx
	// unchanged. Walking the indexes in order makes the error deterministic.
	scripts := make(map[int][]byte, len(prevPkScripts))
	for _, i := range slices.Sorted(maps.Keys(prevPkScripts)) {
		if i < 0 || i >= len(tx.TxIn) {
			return fmt.Errorf("input %d out of range", i)
		}
		if len(tx.TxIn[i].SignatureScript) != 0 {
			return fmt.Errorf("input %d: already has a signature script", i)
		}
		script, err := dashInputScript(tx, i, prevPkScripts[i], secrets)
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
		scripts[i] = script
	}
	for i, script := range scripts {
		tx.TxIn[i].SignatureScript = script
	}
	return nil
}

// dashInputScript returns the signature script for input idx of tx, which
// spends the P2PKH output pkScript. Dash wallets only hold P2PKH outputs, so
// any other script is an error.
func dashInputScript(tx *bisonwire.Tx, idx int, pkScript []byte,
	secrets SecretsSource) ([]byte, error) {

	if !txscript.IsPayToPubKeyHash(pkScript) {
		return nil, errors.New("only p2pkh inputs are supported")
	}

	_, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript,
		secrets.ChainParams())
	if err != nil {
		return nil, err
	}
	priv, compressed, err := secrets.GetKey(addrs[0])
	if err != nil {
		return nil, err
	}

	hash, err := dashSignatureHash(tx, idx, pkScript)
	if err != nil {
		return nil, err
	}
	sig := append(ecdsa.Sign(priv, hash).Serialize(), byte(txscript.SigHashAll))

	pub := priv.PubKey().SerializeCompressed()
	if !compressed {
		pub = priv.PubKey().SerializeUncompressed()
	}

	// A key for another address, or in the wrong form, would give a script
	// that looks right and fails only on the network, after the other
	// participants have signed.
	if !bytes.Equal(btcutil.Hash160(pub), addrs[0].ScriptAddress()) {
		return nil, errors.New("key does not match the previous output script")
	}
	return txscript.NewScriptBuilder().AddData(sig).AddData(pub).Script()
}
