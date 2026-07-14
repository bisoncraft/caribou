// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"filippo.io/edwards25519"
	"github.com/decred/base58"
)

const (
	tokenProgramID           = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	associatedTokenProgramID = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"

	// splInstructionTransferChecked is the SPL Token program instruction
	// index for TransferChecked.
	splInstructionTransferChecked = 12
	// ataInstructionCreateIdempotent is the Associated Token Account program
	// instruction index for CreateIdempotent, which succeeds as a no-op if
	// the account already exists.
	ataInstructionCreateIdempotent = 1

	// splTokenAccountSize is the on-chain size in bytes of an SPL Token
	// account, used to compute the rent-exemption reserve required to fund
	// a newly created associated token account.
	splTokenAccountSize = 165
)

// isOnCurve reports whether b is a valid compressed Edwards25519 point.
// Program-derived addresses (PDAs) are defined as the first candidate that
// does NOT decode to a valid curve point, so this is used to reject bump
// seeds that land on the curve.
func isOnCurve(b []byte) bool {
	if len(b) != 32 {
		return false
	}
	_, err := edwards25519.NewIdentityPoint().SetBytes(b)
	return err == nil
}

// findProgramAddress derives a program-derived address (PDA) for the given
// seeds and program, following Solana's off-curve bump-seed search.
func findProgramAddress(seeds [][]byte, programID [32]byte) (addr [32]byte, bump byte, err error) {
	for b := 255; b >= 0; b-- {
		h := sha256.New()
		for _, s := range seeds {
			h.Write(s)
		}
		h.Write([]byte{byte(b)})
		h.Write(programID[:])
		h.Write([]byte("ProgramDerivedAddress"))
		sum := h.Sum(nil)
		if !isOnCurve(sum) {
			copy(addr[:], sum)
			return addr, byte(b), nil
		}
	}
	return addr, 0, errors.New("unable to find a viable program address bump seed")
}

// deriveATA derives the associated token account address for owner and mint.
func deriveATA(owner, mint [32]byte) ([32]byte, error) {
	tp := mustDecodePubKey(tokenProgramID)
	atp := mustDecodePubKey(associatedTokenProgramID)
	addr, _, err := findProgramAddress([][]byte{owner[:], tp[:], mint[:]}, atp)
	return addr, err
}

// accountMeta is an account reference within an instruction or message.
type accountMeta struct {
	pubkey     [32]byte
	isSigner   bool
	isWritable bool
}

// splInstruction is a single instruction to be compiled into a message.
type splInstruction struct {
	programID [32]byte
	accounts  []accountMeta
	data      []byte
}

// transferCheckedInstruction builds a TransferChecked instruction moving
// amount (in the token's atomic units) from source to destination, signed by
// owner (the source account's authority).
func transferCheckedInstruction(source, mint, destination, owner [32]byte, amount uint64, decimals uint8) splInstruction {
	data := make([]byte, 10)
	data[0] = splInstructionTransferChecked
	binary.LittleEndian.PutUint64(data[1:9], amount)
	data[9] = decimals
	return splInstruction{
		programID: mustDecodePubKey(tokenProgramID),
		accounts: []accountMeta{
			{pubkey: source, isWritable: true},
			{pubkey: mint},
			{pubkey: destination, isWritable: true},
			{pubkey: owner, isSigner: true},
		},
		data: data,
	}
}

// createAssociatedTokenAccountIdempotentInstruction builds an instruction
// that creates the associated token account for owner/mint if it does not
// already exist, paid for by payer. It is a no-op if the account exists.
func createAssociatedTokenAccountIdempotentInstruction(payer, ata, owner, mint [32]byte) splInstruction {
	return splInstruction{
		programID: mustDecodePubKey(associatedTokenProgramID),
		accounts: []accountMeta{
			{pubkey: payer, isSigner: true, isWritable: true},
			{pubkey: ata, isWritable: true},
			{pubkey: owner},
			{pubkey: mint},
			{pubkey: mustDecodePubKey(systemProgramID)},
			{pubkey: mustDecodePubKey(tokenProgramID)},
		},
		data: []byte{ataInstructionCreateIdempotent},
	}
}

// compileSPLMessage builds a legacy Solana message from a set of
// instructions, deduplicating accounts (including program IDs) and ordering
// them into the four required groups: writable signers, readonly signers,
// writable non-signers, readonly non-signers.
func compileSPLMessage(payer [32]byte, instrs []splInstruction, blockhash [32]byte) []byte {
	type entry struct {
		pubkey     [32]byte
		isSigner   bool
		isWritable bool
	}
	flags := map[[32]byte]*entry{}
	var order [][32]byte
	upsert := func(pk [32]byte, signer, writable bool) {
		e, ok := flags[pk]
		if !ok {
			e = &entry{pubkey: pk}
			flags[pk] = e
			order = append(order, pk)
		}
		if signer {
			e.isSigner = true
		}
		if writable {
			e.isWritable = true
		}
	}
	upsert(payer, true, true)
	for _, in := range instrs {
		upsert(in.programID, false, false)
		for _, a := range in.accounts {
			upsert(a.pubkey, a.isSigner, a.isWritable)
		}
	}

	var writableSigners, readonlySigners, writableNonSigners, readonlyNonSigners []*entry
	for _, pk := range order {
		e := flags[pk]
		switch {
		case e.isSigner && e.isWritable:
			writableSigners = append(writableSigners, e)
		case e.isSigner:
			readonlySigners = append(readonlySigners, e)
		case e.isWritable:
			writableNonSigners = append(writableNonSigners, e)
		default:
			readonlyNonSigners = append(readonlyNonSigners, e)
		}
	}

	final := make([]*entry, 0, len(order))
	final = append(final, writableSigners...)
	final = append(final, readonlySigners...)
	final = append(final, writableNonSigners...)
	final = append(final, readonlyNonSigners...)

	keyIndex := make(map[[32]byte]byte, len(final))
	for i, e := range final {
		keyIndex[e.pubkey] = byte(i)
	}

	msg := make([]byte, 0, 256)
	msg = append(msg, byte(len(writableSigners)+len(readonlySigners)))
	msg = append(msg, byte(len(readonlySigners)))
	msg = append(msg, byte(len(readonlyNonSigners)))

	msg = append(msg, shortVec(len(final))...)
	for _, e := range final {
		msg = append(msg, e.pubkey[:]...)
	}
	msg = append(msg, blockhash[:]...)

	msg = append(msg, shortVec(len(instrs))...)
	for _, in := range instrs {
		msg = append(msg, keyIndex[in.programID])
		msg = append(msg, shortVec(len(in.accounts))...)
		for _, a := range in.accounts {
			msg = append(msg, keyIndex[a.pubkey])
		}
		msg = append(msg, shortVec(len(in.data))...)
		msg = append(msg, in.data...)
	}
	return msg
}

// signSPLTx signs msg and wraps it in a single-signature transaction.
func signSPLTx(msg []byte, privKey ed25519.PrivateKey) (tx []byte, sig string) {
	sigB := ed25519.Sign(privKey, msg)
	sig = base58.Encode(sigB)
	tx = make([]byte, 0, len(msg)+len(sigB)+8)
	tx = append(tx, shortVec(1)...)
	tx = append(tx, sigB...)
	tx = append(tx, msg...)
	return tx, sig
}
