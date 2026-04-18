// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"encoding/binary"
	"math/big"
)

// NEAR action type indices (Borsh enum variants).
const (
	actionCreateAccount  = 0
	actionDeployContract = 1
	actionFunctionCall   = 2
	actionTransfer       = 3
	actionStake          = 4
	actionAddKey         = 5
	actionDeleteKey      = 6
	actionDeleteAccount  = 7
	// actionDelegate    = 8 // NEP-366: for future Intents support
)

// NEAR key type prefixes.
const (
	keyTypeED25519 = 0
)

// nearTransaction is the NEAR transaction structure for Borsh serialization.
type nearTransaction struct {
	signerID   string
	publicKey  [32]byte // ed25519 public key
	nonce      uint64
	receiverID string
	blockHash  [32]byte
	actions    []action
}

type action struct {
	variant uint8
	data    []byte // pre-serialized action data
}

// transferAction creates a Transfer action with the given yoctoNEAR amount.
func transferAction(yoctoNEAR *big.Int) action {
	return action{
		variant: actionTransfer,
		data:    marshalU128LE(yoctoNEAR),
	}
}

// serializeTransaction Borsh-encodes a NEAR transaction.
func serializeTransaction(tx *nearTransaction) []byte {
	var b []byte
	b = appendString(b, tx.signerID)
	b = appendKeyED25519(b, tx.publicKey[:])
	b = appendU64LE(b, tx.nonce)
	b = appendString(b, tx.receiverID)
	b = append(b, tx.blockHash[:]...)
	b = appendU32LE(b, uint32(len(tx.actions)))
	for _, a := range tx.actions {
		b = append(b, a.variant)
		b = append(b, a.data...)
	}
	return b
}

// serializeSignedTransaction Borsh-encodes a signed NEAR transaction.
// txBytes is the serialized transaction, sig is the 64-byte ed25519 signature.
func serializeSignedTransaction(txBytes, sig []byte) []byte {
	b := make([]byte, 0, len(txBytes)+1+64)
	b = append(b, txBytes...)
	b = append(b, keyTypeED25519)
	b = append(b, sig...)
	return b
}

func appendU32LE(b []byte, v uint32) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, v)
	return append(b, buf...)
}

func appendU64LE(b []byte, v uint64) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, v)
	return append(b, buf...)
}

// appendString writes a Borsh string: 4-byte LE length prefix + UTF-8 bytes.
func appendString(b []byte, s string) []byte {
	b = appendU32LE(b, uint32(len(s)))
	return append(b, s...)
}

// appendKeyED25519 writes a NEAR public key: 1-byte type prefix + 32 bytes.
func appendKeyED25519(b []byte, key []byte) []byte {
	b = append(b, keyTypeED25519)
	return append(b, key...)
}

// marshalU128LE encodes a big.Int as a 16-byte little-endian u128.
func marshalU128LE(v *big.Int) []byte {
	buf := make([]byte, 16)
	b := v.Bytes() // big-endian
	// Reverse into little-endian.
	for i, j := 0, len(b)-1; i <= j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	copy(buf, b)
	return buf
}
