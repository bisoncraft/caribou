// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package txauthor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// dashSpecialTxVersion is the lowest transaction version that carries an extra
// payload when its type is not normal.
const dashSpecialTxVersion = 3

// dashSignatureHash returns the SIGHASH_ALL digest Dash signs for input idx.
// It is Bitcoin's legacy digest with two differences that matter for special
// transactions: the version field is the packed version|type, and the extra
// payload follows the lock time. For a normal transaction the two digests are
// identical.
//
// The payload placement follows Dash Core's CTransactionSignatureSerializer as
// recalled; it has not been checked against a transaction signed by dashd.
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

	if tx.Version >= dashSpecialTxVersion && tx.Type != 0 {
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
		pkScript := prevPkScripts[i]
		if !txscript.IsPayToPubKeyHash(pkScript) {
			return fmt.Errorf("input %d: only p2pkh inputs are supported", i)
		}

		_, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript,
			secrets.ChainParams())
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
		priv, compressed, err := secrets.GetKey(addrs[0])
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}

		hash, err := dashSignatureHash(tx, i, pkScript)
		if err != nil {
			return err
		}
		sig := append(ecdsa.Sign(priv, hash).Serialize(), byte(txscript.SigHashAll))

		pub := priv.PubKey().SerializeCompressed()
		if !compressed {
			pub = priv.PubKey().SerializeUncompressed()
		}
		in.SignatureScript, err = txscript.NewScriptBuilder().
			AddData(sig).AddData(pub).Script()
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
	}
	return nil
}
