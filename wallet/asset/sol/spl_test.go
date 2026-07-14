// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/decred/base58"
)

func TestIsOnCurve(t *testing.T) {
	// A real ed25519 public key must decode to a valid curve point.
	pub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x07}, 32)).Public().(ed25519.PublicKey)
	if !isOnCurve(pub) {
		t.Fatal("expected a real ed25519 public key to be on-curve")
	}
	if isOnCurve(make([]byte, 31)) {
		t.Fatal("expected wrong-length input to be rejected")
	}
}

func TestFindProgramAddressDeterministic(t *testing.T) {
	programID := mustDecodePubKey(associatedTokenProgramID)
	seeds := [][]byte{bytes.Repeat([]byte{0xAA}, 32), bytes.Repeat([]byte{0xBB}, 32)}
	addr1, bump1, err := findProgramAddress(seeds, programID)
	if err != nil {
		t.Fatalf("findProgramAddress error: %v", err)
	}
	addr2, bump2, err := findProgramAddress(seeds, programID)
	if err != nil {
		t.Fatalf("findProgramAddress error (2nd call): %v", err)
	}
	if addr1 != addr2 || bump1 != bump2 {
		t.Fatal("findProgramAddress is not deterministic")
	}
	// A PDA must, by construction, be off-curve.
	if isOnCurve(addr1[:]) {
		t.Fatal("derived PDA landed on-curve; off-curve search failed")
	}
}

func TestDeriveATA(t *testing.T) {
	owner := mustDecodePubKey(base58.Encode(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x01}, 32)).Public().(ed25519.PublicKey)))
	mint := mustDecodePubKey(TokenUSDC.NetMints[util.Mainnet])

	ata1, err := deriveATA(owner, mint)
	if err != nil {
		t.Fatalf("deriveATA error: %v", err)
	}
	ata2, err := deriveATA(owner, mint)
	if err != nil {
		t.Fatalf("deriveATA error (2nd call): %v", err)
	}
	if ata1 != ata2 {
		t.Fatal("deriveATA is not deterministic")
	}
	if isOnCurve(ata1[:]) {
		t.Fatal("derived ATA landed on-curve")
	}

	otherOwner := mustDecodePubKey(base58.Encode(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x02}, 32)).Public().(ed25519.PublicKey)))
	ata3, err := deriveATA(otherOwner, mint)
	if err != nil {
		t.Fatalf("deriveATA error (other owner): %v", err)
	}
	if ata3 == ata1 {
		t.Fatal("expected different owners to derive different ATAs")
	}
}

func TestTransferCheckedInstruction(t *testing.T) {
	var source, mint, destination, owner [32]byte
	source[0] = 1
	mint[0] = 2
	destination[0] = 3
	owner[0] = 4
	inst := transferCheckedInstruction(source, mint, destination, owner, 1_000_000, 6)
	if inst.data[0] != splInstructionTransferChecked {
		t.Fatalf("unexpected instruction discriminator %d", inst.data[0])
	}
	if len(inst.accounts) != 4 {
		t.Fatalf("expected 4 accounts, got %d", len(inst.accounts))
	}
	if !inst.accounts[3].isSigner {
		t.Fatal("expected owner account to be marked as signer")
	}
}

func TestCompileSPLMessageAccountOrdering(t *testing.T) {
	var payer, ata, owner, mint, dest [32]byte
	payer[0] = 1
	ata[0] = 2
	owner[0] = 3
	mint[0] = 4
	dest[0] = 5
	var blockhash [32]byte
	blockhash[0] = 9

	instrs := []splInstruction{
		createAssociatedTokenAccountIdempotentInstruction(payer, dest, owner, mint),
		transferCheckedInstruction(ata, mint, dest, payer, 500, 6),
	}
	msg := compileSPLMessage(payer, instrs, blockhash)
	if len(msg) == 0 {
		t.Fatal("empty message")
	}
	numRequiredSignatures := msg[0]
	if numRequiredSignatures < 1 {
		t.Fatal("expected at least one required signature (payer)")
	}
	// First account key after the 3-byte header + shortvec length byte must
	// be the payer, since it is both a writable signer and the first one
	// referenced.
	firstKeyOffset := 4
	if !bytes.Equal(msg[firstKeyOffset:firstKeyOffset+32], payer[:]) {
		t.Fatal("expected payer to be the first account key")
	}
}
