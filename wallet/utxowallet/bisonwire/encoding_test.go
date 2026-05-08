package bisonwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

const testProtocolVer = wire.ProtocolVersion

// roundTrip writes msg with WriteMessageWithEncodingN, reads it back with
// ReadMessageWithEncodingN, and returns the decoded message and payload bytes.
func roundTrip(t *testing.T, msg wire.Message, chain Chain, btcnet wire.BitcoinNet) (wire.Message, []byte) {
	t.Helper()
	var buf bytes.Buffer
	written, err := WriteMessageWithEncodingN(
		&buf, msg, testProtocolVer, btcnet, wire.LatestEncoding,
	)
	if err != nil {
		t.Fatalf("WriteMessageWithEncodingN: %v", err)
	}
	if written != buf.Len() {
		t.Fatalf("WriteMessageWithEncodingN reported %d bytes written but "+
			"buffer holds %d", written, buf.Len())
	}

	read, gotMsg, payload, err := ReadMessageWithEncodingN(
		&buf, testProtocolVer, chain, btcnet, wire.LatestEncoding,
	)
	if err != nil {
		t.Fatalf("ReadMessageWithEncodingN: %v", err)
	}
	if read != written {
		t.Fatalf("read %d bytes, want %d", read, written)
	}
	if buf.Len() != 0 {
		t.Fatalf("buffer not fully consumed; %d bytes left", buf.Len())
	}
	if gotMsg.Command() != msg.Command() {
		t.Fatalf("decoded command %q, want %q", gotMsg.Command(), msg.Command())
	}
	return gotMsg, payload
}

// TestRoundTripStandardMessages verifies the wire-framing layer
// (WriteMessageWithEncodingN / ReadMessageWithEncodingN) over a representative
// set of standard wire message types, covering the BTC chain code path through
// makeEmptyMessage. These are the production hot path read/write hooks called
// by peer.peer.go on every inbound and outbound network message.
func TestRoundTripStandardMessages(t *testing.T) {
	hash1, _ := chainhash.NewHashFromStr(
		"000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f",
	)
	hash2, _ := chainhash.NewHashFromStr(
		"00000000839a8e6886ab5951d76f411475428afc90947ee320161bbf18eb6048",
	)

	// MsgInv with a couple of entries.
	inv := wire.NewMsgInv()
	_ = inv.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, hash1))
	_ = inv.AddInvVect(wire.NewInvVect(wire.InvTypeWitnessTx, hash2))

	// MsgGetHeaders with a real block locator.
	gh := wire.NewMsgGetHeaders()
	gh.ProtocolVersion = testProtocolVer
	_ = gh.AddBlockLocatorHash(hash1)
	gh.HashStop = *hash2

	// MsgHeaders carrying one header.
	hdrs := wire.NewMsgHeaders()
	_ = hdrs.AddBlockHeader(&wire.BlockHeader{
		Version:    1,
		PrevBlock:  *hash1,
		MerkleRoot: *hash2,
		Timestamp:  wire.MsgVersion{}.Timestamp,
		Bits:       0x1d00ffff,
		Nonce:      0x12345678,
	})

	tests := []struct {
		name string
		msg  wire.Message
	}{
		{"MsgPing", wire.NewMsgPing(0xdeadbeef)},
		{"MsgPong", wire.NewMsgPong(0xdeadbeef)},
		{"MsgVerAck", wire.NewMsgVerAck()},
		{"MsgGetAddr", wire.NewMsgGetAddr()},
		{"MsgSendHeaders", wire.NewMsgSendHeaders()},
		{"MsgFeeFilter", wire.NewMsgFeeFilter(1234)},
		{"MsgInv", inv},
		{"MsgGetHeaders", gh},
		{"MsgHeaders", hdrs},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundTrip(t, tt.msg, ChainBTC, wire.MainNet)
		})
	}
}

// TestRoundTripBlockBTC verifies that a wire.MsgBlock written through the
// framing layer is decoded back as a *bisonwire.Block whose contents match
// the original. bisonwire.Block is intentionally inbound-only — its BtcEncode
// is a guarded stub — so the source side of the round-trip drives writes from
// a wire.MsgBlock. Exercises the BTC branch of bisonwire.Block.BtcDecode plus
// the BlockFromMsgBlock and MsgBlock helpers.
func TestRoundTripBlockBTC(t *testing.T) {
	// Build a tiny valid-shaped block with a single coinbase tx. We don't
	// validate consensus rules here — only that serialization round-trips.
	hash1, _ := chainhash.NewHashFromStr(
		"000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f",
	)
	hash2, _ := chainhash.NewHashFromStr(
		"00000000839a8e6886ab5951d76f411475428afc90947ee320161bbf18eb6048",
	)
	coinbase := wire.NewMsgTx(1)
	coinbase.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{}, Index: 0xffffffff},
		SignatureScript:  []byte{0x04, 0xff, 0xff, 0x00, 0x1d, 0x01, 0x04},
		Sequence:         0xffffffff,
	})
	coinbase.AddTxOut(&wire.TxOut{
		Value:    50_0000_0000,
		PkScript: []byte{0x76, 0xa9, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x88, 0xac},
	})
	msgBlock := wire.NewMsgBlock(&wire.BlockHeader{
		Version:    1,
		PrevBlock:  *hash1,
		MerkleRoot: *hash2,
		Bits:       0x1d00ffff,
		Nonce:      0xdeadbeef,
	})
	_ = msgBlock.AddTransaction(coinbase)

	got, _ := roundTrip(t, msgBlock, ChainBTC, wire.MainNet)
	out, ok := got.(*Block)
	if !ok {
		t.Fatalf("decoded message is %T, want *bisonwire.Block", got)
	}
	if out.Header.BlockHash() != msgBlock.BlockHash() {
		t.Fatalf("decoded block hash %v, want %v",
			out.Header.BlockHash(), msgBlock.BlockHash())
	}
	if got, want := len(out.Transactions), len(msgBlock.Transactions); got != want {
		t.Fatalf("decoded %d transactions, want %d", got, want)
	}
	if h := out.Transactions[0].TxHash(); h != coinbase.TxHash() {
		t.Fatalf("decoded coinbase hash %v, want %v", h, coinbase.TxHash())
	}

	// MsgBlock conversion should yield a header-and-tx-hash-equal MsgBlock.
	rebuilt := out.MsgBlock()
	if rebuilt.BlockHash() != msgBlock.BlockHash() {
		t.Fatalf("MsgBlock() yields hash %v, want %v",
			rebuilt.BlockHash(), msgBlock.BlockHash())
	}

	// BlockFromMsgBlock must symmetrically rebuild a bisonwire.Block from a
	// wire.MsgBlock with the same header hash and transaction count.
	wrapped := BlockFromMsgBlock("btc", msgBlock)
	if wrapped.Header.BlockHash() != msgBlock.BlockHash() {
		t.Fatalf("BlockFromMsgBlock header hash %v, want %v",
			wrapped.Header.BlockHash(), msgBlock.BlockHash())
	}
	if len(wrapped.Transactions) != len(msgBlock.Transactions) {
		t.Fatalf("BlockFromMsgBlock %d txns, want %d",
			len(wrapped.Transactions), len(msgBlock.Transactions))
	}
}

// TestRoundTripBlockLTC verifies that an LTC block written through the
// framing layer is decoded back via the chain-aware MWEB-skipping decoder
// (bisonwire.Block.BtcDecode → deserializeLitecoinBlock). We start from the
// pre-MWEB block1821752 fixture so the wire.MsgBlock representation is a
// lossless source for the round trip.
func TestRoundTripBlockLTC(t *testing.T) {
	src, err := deserializeLitecoinBlockSafe(block1821752)
	if err != nil {
		t.Fatalf("deserializeLitecoinBlock: %v", err)
	}
	srcMsgBlock := src.MsgBlock()

	got, _ := roundTrip(t, srcMsgBlock, ChainLTC, wire.TestNet)
	out, ok := got.(*Block)
	if !ok {
		t.Fatalf("decoded message is %T, want *bisonwire.Block", got)
	}
	if out.Header.BlockHash() != src.Header.BlockHash() {
		t.Fatalf("decoded block hash %v, want %v",
			out.Header.BlockHash(), src.Header.BlockHash())
	}
	if got, want := len(out.Transactions), len(src.Transactions); got != want {
		t.Fatalf("decoded %d transactions, want %d", got, want)
	}
}

// TestReadMessageWrongMagic verifies that a payload tagged with the wrong
// network magic is rejected with a MessageError, not silently dispatched.
func TestReadMessageWrongMagic(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteMessageWithEncodingN(
		&buf, wire.NewMsgPing(1), testProtocolVer, wire.MainNet,
		wire.LatestEncoding,
	); err != nil {
		t.Fatalf("WriteMessageWithEncodingN: %v", err)
	}
	// Decode with a different network — the magic check should fail.
	_, _, _, err := ReadMessageWithEncodingN(
		&buf, testProtocolVer, ChainBTC, wire.TestNet, wire.LatestEncoding,
	)
	if err == nil {
		t.Fatal("expected error from network magic mismatch, got nil")
	}
	if _, ok := err.(*wire.MessageError); !ok {
		t.Fatalf("expected *wire.MessageError, got %T (%v)", err, err)
	}
}

// TestBlockBtcEncodeIsGuarded checks that the inbound-only Block.BtcEncode
// stub returns an error rather than silently producing zero bytes — which
// would otherwise let outbound code paths accidentally serialize a partial
// block and have peers reject it as malformed.
func TestBlockBtcEncodeIsGuarded(t *testing.T) {
	var b Block
	err := b.BtcEncode(&bytes.Buffer{}, testProtocolVer, wire.LatestEncoding)
	if err == nil {
		t.Fatal("expected BtcEncode to return an error, got nil")
	}
}

// TestReadMessageWrappers covers the ReadMessage and ReadMessageN convenience
// shims that delegate to ReadMessageWithEncodingN with wire.LatestEncoding.
func TestReadMessageWrappers(t *testing.T) {
	for _, fn := range []struct {
		name string
		read func(*bytes.Buffer) (wire.Message, error)
	}{
		{
			name: "ReadMessage",
			read: func(buf *bytes.Buffer) (wire.Message, error) {
				msg, _, err := ReadMessage(
					buf, testProtocolVer, ChainBTC, wire.MainNet,
				)
				return msg, err
			},
		},
		{
			name: "ReadMessageN",
			read: func(buf *bytes.Buffer) (wire.Message, error) {
				_, msg, _, err := ReadMessageN(
					buf, testProtocolVer, ChainBTC, wire.MainNet,
				)
				return msg, err
			},
		},
	} {
		t.Run(fn.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := WriteMessageWithEncodingN(
				&buf, wire.NewMsgPing(0xfeedface),
				testProtocolVer, wire.MainNet, wire.LatestEncoding,
			); err != nil {
				t.Fatalf("WriteMessageWithEncodingN: %v", err)
			}
			msg, err := fn.read(&buf)
			if err != nil {
				t.Fatalf("%s: %v", fn.name, err)
			}
			if msg.Command() != wire.CmdPing {
				t.Fatalf("decoded command %q, want %q", msg.Command(), wire.CmdPing)
			}
		})
	}
}

// TestTxDeserializeBTC exercises the BTC branch of bisonwire.Tx.Deserialize.
// The LTC branch is covered by ltc_test.go.
func TestTxDeserializeBTC(t *testing.T) {
	src := wire.NewMsgTx(2)
	src.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{}, Index: 0xffffffff},
		SignatureScript:  []byte{0x04, 0xff, 0xff, 0x00, 0x1d},
		Sequence:         0xffffffff,
	})
	src.AddTxOut(&wire.TxOut{
		Value:    10_0000_0000,
		PkScript: []byte{0x76, 0xa9, 0x14, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x88, 0xac},
	})

	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	got := &Tx{Chain: "btc"}
	if err := got.Deserialize(&buf); err != nil {
		t.Fatalf("Tx.Deserialize: %v", err)
	}
	if got.TxHash() != src.TxHash() {
		t.Fatalf("decoded tx hash %v, want %v", got.TxHash(), src.TxHash())
	}

	// Unknown chain should fail rather than silently no-op.
	bad := &Tx{Chain: "doge"}
	if err := bad.Deserialize(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("expected error for unknown chain, got nil")
	}
}

// TestChainFromString covers the small dispatch helper for completeness.
func TestChainFromString(t *testing.T) {
	for _, want := range KnownChains {
		got, err := ChainFromString(string(want))
		if err != nil {
			t.Fatalf("ChainFromString(%q): %v", want, err)
		}
		if got != want {
			t.Fatalf("ChainFromString(%q) = %q, want %q", want, got, want)
		}
	}
	if _, err := ChainFromString("not-a-chain"); err == nil {
		t.Fatal("expected error for unknown chain, got nil")
	}
}

// deserializeLitecoinBlockSafe wraps the package-private decoder so the test
// can be moved out of this file later if needed.
func deserializeLitecoinBlockSafe(b []byte) (*Block, error) {
	return deserializeLitecoinBlock(bytes.NewReader(b))
}
