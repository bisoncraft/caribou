package bisonwire

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// Real Dash mainnet genesis coinbase (a normal, type-0 transaction). Its txid
// equals the genesis block's merkle root, so it is a self-checking vector. Built
// from dashpay/dash src/chainparams.cpp and cross-checked with an independent
// sha256d oracle.
const (
	dashGenesisCoinbaseHex  = "01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff6204ffff001d01044c5957697265642030392f4a616e2f3230313420546865204772616e64204578706572696d656e7420476f6573204c6976653a204f76657273746f636b2e636f6d204973204e6f7720416363657074696e6720426974636f696e73ffffffff0100f2052a010000004341040184710fa689ad5023690c80f3a49c8f13f8d45b8c857fbcbc8bc4a8e4d3eb4b10f4d4604fa08dce601aaf0f470216fe1b51850b4acf21b179c45070ac7b03a9ac00000000"
	dashGenesisCoinbaseTxID = "e0028eb9648db56b1ac77cf090b99048a8007e2bb64b68f092c03c7f56a662c7"

	// A synthetic DIP-2 special transaction: version 3, type 5 (coinbase /
	// CbTx), one input, one output, lock time 0, and a 10-byte extra payload.
	// The txid is sha256d over the full serialization (packed version|type +
	// inputs/outputs/locktime + length-prefixed extra payload), computed by an
	// independent oracle.
	dashSpecialTxHex     = "03000500010000000000000000000000000000000000000000000000000000000000000000ffffffff0403abcdefffffffff010065cd1d000000001976a914111111111111111111111111111111111111111188ac000000000a0102030405060708090a"
	dashSpecialTxID      = "fbf8d764239596655ac584f166bd889ef532dbd850778345a30e814d0f11980c"
	dashSpecialExtraData = "0102030405060708090a"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// TestDashTxNormal decodes the real genesis coinbase (type 0) and checks that
// the standard sha256d txid is reproduced.
func TestDashTxNormal(t *testing.T) {
	tx := &Tx{Chain: string(ChainDASH)}
	if err := tx.Deserialize(bytes.NewReader(mustHex(t, dashGenesisCoinbaseHex))); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if tx.Type != 0 {
		t.Fatalf("Type = %d, want 0", tx.Type)
	}
	if tx.Version != 1 {
		t.Fatalf("Version = %d, want 1", tx.Version)
	}
	if len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
		t.Fatalf("got %d in / %d out, want 1/1", len(tx.TxIn), len(tx.TxOut))
	}
	if len(tx.ExtraPayload) != 0 {
		t.Fatalf("normal tx has extra payload: %x", tx.ExtraPayload)
	}
	if got := tx.TxHash().String(); got != dashGenesisCoinbaseTxID {
		t.Fatalf("txid = %s, want %s", got, dashGenesisCoinbaseTxID)
	}
}

// TestDashTxSpecial decodes a DIP-2 special transaction and checks the
// version/type split, the extra payload, and the special-tx txid (which the
// plain MsgTx hash would get wrong).
func TestDashTxSpecial(t *testing.T) {
	raw := mustHex(t, dashSpecialTxHex)
	tx := &Tx{Chain: string(ChainDASH)}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if tx.Version != 3 {
		t.Fatalf("Version = %d, want 3", tx.Version)
	}
	if tx.Type != 5 {
		t.Fatalf("Type = %d, want 5", tx.Type)
	}
	if got := hex.EncodeToString(tx.ExtraPayload); got != dashSpecialExtraData {
		t.Fatalf("ExtraPayload = %s, want %s", got, dashSpecialExtraData)
	}
	if got := tx.TxHash().String(); got != dashSpecialTxID {
		t.Fatalf("special txid = %s, want %s", got, dashSpecialTxID)
	}
	// The naive MsgTx hash omits the packed type bits and the extra payload, so
	// it must differ — this is exactly why the dash path exists.
	if tx.MsgTx.TxHash().String() == dashSpecialTxID {
		t.Fatal("plain MsgTx hash unexpectedly matched the special txid")
	}
}

// TestDashBlock decodes a block carrying both a normal and a special
// transaction and checks the count, per-tx types, and txids.
func TestDashBlock(t *testing.T) {
	merkle, err := chainhash.NewHashFromStr(dashGenesisCoinbaseTxID)
	if err != nil {
		t.Fatal(err)
	}
	hdr := wire.BlockHeader{
		Version:    1,
		MerkleRoot: *merkle,
		Bits:       0x1e0ffff0,
		Nonce:      28917698,
	}

	var buf bytes.Buffer
	if err := hdr.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteVarInt(&buf, 0, 2); err != nil {
		t.Fatal(err)
	}
	buf.Write(mustHex(t, dashGenesisCoinbaseHex))
	buf.Write(mustHex(t, dashSpecialTxHex))

	b := &Block{Chain: string(ChainDASH)}
	if err := b.BtcDecode(&buf, 0, wire.LatestEncoding); err != nil {
		t.Fatalf("BtcDecode: %v", err)
	}
	if len(b.Transactions) != 2 {
		t.Fatalf("got %d txs, want 2", len(b.Transactions))
	}
	if b.Transactions[0].Type != 0 || b.Transactions[1].Type != 5 {
		t.Fatalf("tx types = %d,%d want 0,5", b.Transactions[0].Type, b.Transactions[1].Type)
	}
	if got := b.Transactions[0].TxHash().String(); got != dashGenesisCoinbaseTxID {
		t.Fatalf("tx0 id = %s, want %s", got, dashGenesisCoinbaseTxID)
	}
	if got := b.Transactions[1].TxHash().String(); got != dashSpecialTxID {
		t.Fatalf("tx1 id = %s, want %s", got, dashSpecialTxID)
	}
	if b.Header.MerkleRoot != *merkle {
		t.Fatalf("decoded merkle root = %s, want %s", b.Header.MerkleRoot, merkle)
	}
}
