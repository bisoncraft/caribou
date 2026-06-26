// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"

	"github.com/decred/base58"
)

const systemProgramID = "11111111111111111111111111111111"

func shortVec(v int) []byte {
	if v < 0 {
		return nil
	}
	out := make([]byte, 0, 4)
	n := uint32(v)
	for {
		elem := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			out = append(out, elem)
			return out
		}
		out = append(out, elem|0x80)
	}
}

func mustDecodePubKey(addr string) [32]byte {
	var pk [32]byte
	b := base58.Decode(addr)
	if len(b) != 32 {
		panic(fmt.Sprintf("mustDecodePubKey: invalid length %d for %q", len(b), addr))
	}
	copy(pk[:], b)
	return pk
}

func encodeTransferMessage(fromAddr, toAddr, blockhash string, lamports uint64) []byte {
	fromKey := mustDecodePubKey(fromAddr)
	toKey := mustDecodePubKey(toAddr)
	systemKey := mustDecodePubKey(systemProgramID)
	recent := mustDecodePubKey(blockhash)

	msg := make([]byte, 0, 128)
	msg = append(msg, 1, 0, 1) // 1 signer, 0 readonly signed, 1 readonly unsigned
	msg = append(msg, shortVec(3)...)
	msg = append(msg, fromKey[:]...)
	msg = append(msg, toKey[:]...)
	msg = append(msg, systemKey[:]...)
	msg = append(msg, recent[:]...)
	msg = append(msg, shortVec(1)...)
	msg = append(msg, 2) // system program account index
	msg = append(msg, shortVec(2)...)
	msg = append(msg, 0, 1)
	data := make([]byte, 12)
	binary.LittleEndian.PutUint32(data[:4], 2) // SystemInstruction::Transfer
	binary.LittleEndian.PutUint64(data[4:], lamports)
	msg = append(msg, shortVec(len(data))...)
	msg = append(msg, data...)
	return msg
}

func signTransfer(fromAddr, toAddr, blockhash string, lamports uint64, privKey ed25519.PrivateKey) (msg []byte, tx []byte, sig string) {
	msg = encodeTransferMessage(fromAddr, toAddr, blockhash, lamports)
	sigB := ed25519.Sign(privKey, msg)
	sig = base58.Encode(sigB)
	tx = make([]byte, 0, len(msg)+len(sigB)+8)
	tx = append(tx, shortVec(1)...)
	tx = append(tx, sigB...)
	tx = append(tx, msg...)
	return msg, tx, sig
}
