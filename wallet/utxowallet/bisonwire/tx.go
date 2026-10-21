package bisonwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

type Tx struct {
	wire.MsgTx
	Chain   string // TODO: Use bisonwire.Chain type
	IsHogEx bool
	Kern0   []byte

	// Dash DIP-2 special-transaction fields. Type is the transaction type (0 =
	// TRANSACTION_NORMAL); ExtraPayload is the opaque type-specific blob present
	// when version >= 3 && type != 0. Both are zero/nil for other chains and for
	// normal Dash transactions.
	Type         uint16
	ExtraPayload []byte
}

func (tx *Tx) Deserialize(r io.Reader) error {
	switch tx.Chain {
	case "btc":
		return tx.MsgTx.Deserialize(r)
	case "ltc":
		return deserializeLitecoinTx(r, tx)
	case "dash":
		return deserializeDashTx(r, tx)
	default:
		return fmt.Errorf("unknown chain %q specified for bisonwire tx deserialization", tx.Chain)
	}
}

// Serialize writes tx in its chain's wire format. Dash special transactions
// keep their type and extra payload; every other chain serializes as MsgTx.
func (tx *Tx) Serialize(w io.Writer) error {
	if tx.Chain == "dash" {
		return serializeDashTx(w, tx)
	}
	return tx.MsgTx.Serialize(w)
}

// SerializeSize returns the number of bytes Serialize writes.
func (tx *Tx) SerializeSize() int {
	if tx.Chain == "dash" && tx.Type != dashTxTypeNormal {
		var buf bytes.Buffer
		if err := serializeDashTx(&buf, tx); err != nil {
			return tx.MsgTx.SerializeSize()
		}
		return buf.Len()
	}
	return tx.MsgTx.SerializeSize()
}

func (tx *Tx) TxHash() chainhash.Hash {
	switch tx.Chain {
	case "btc":
		return tx.MsgTx.TxHash()
	case "ltc":
		return litcoinTxHash(tx)
	case "dash":
		return dashTxHash(tx)
	default:
		panic(fmt.Sprintf("unknown chain %q specified for bisonwire tx hash", tx.Chain))
	}
}
