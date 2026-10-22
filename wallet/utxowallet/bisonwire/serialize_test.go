package bisonwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire"
)

// TestSerializeSizeMatchesSerialize guards the size contract that wtxmgr
// relies on: it preallocates a record from SerializeSize, so a size that
// differs from what Serialize writes would store a truncated record.
func TestSerializeSizeMatchesSerialize(t *testing.T) {
	special := &Tx{Chain: string(ChainDASH)}
	if err := special.Deserialize(bytes.NewReader(mustHex(t, dashSpecialTxHex))); err != nil {
		t.Fatal(err)
	}
	normal := &Tx{Chain: string(ChainDASH)}
	if err := normal.Deserialize(bytes.NewReader(mustHex(t, dashGenesisCoinbaseHex))); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		tx   *Tx
	}{
		{"dash special", special},
		{"dash normal", normal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.tx.Serialize(&buf); err != nil {
				t.Fatal(err)
			}
			if got := tc.tx.SerializeSize(); got != buf.Len() {
				t.Fatalf("SerializeSize = %d, Serialize wrote %d", got, buf.Len())
			}
		})
	}
}

// TestSerializeRoundTripsDash guards that Serialize is the inverse of
// Deserialize for both Dash transaction kinds.
func TestSerializeRoundTripsDash(t *testing.T) {
	for name, raw := range map[string]string{
		"special": dashSpecialTxHex,
		"normal":  dashGenesisCoinbaseHex,
	} {
		t.Run(name, func(t *testing.T) {
			tx := &Tx{Chain: string(ChainDASH)}
			if err := tx.Deserialize(bytes.NewReader(mustHex(t, raw))); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err := tx.Serialize(&buf); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), mustHex(t, raw)) {
				t.Fatalf("round trip = %x, want %s", buf.Bytes(), raw)
			}
		})
	}
}

// TestSerializeNonDashIsMsgTx guards that chains other than Dash serialize
// exactly as before this method existed, as the embedded MsgTx.
func TestSerializeNonDashIsMsgTx(t *testing.T) {
	msg := wire.NewMsgTx(2)
	msg.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 1}, Sequence: 7})
	msg.AddTxOut(&wire.TxOut{Value: 5, PkScript: []byte{0x51}})

	var want bytes.Buffer
	if err := msg.Serialize(&want); err != nil {
		t.Fatal(err)
	}

	for _, chain := range []string{"btc", "ltc"} {
		t.Run(chain, func(t *testing.T) {
			tx := &Tx{MsgTx: *msg, Chain: chain}
			var got bytes.Buffer
			if err := tx.Serialize(&got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("serialization differs from MsgTx: %x vs %x", got.Bytes(), want.Bytes())
			}
			if tx.SerializeSize() != want.Len() {
				t.Fatalf("SerializeSize = %d, want %d", tx.SerializeSize(), want.Len())
			}
		})
	}
}
