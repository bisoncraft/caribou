package bisonwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// Dash transaction/block serialization (DIP-2 "special transactions").
//
// A Dash transaction is a legacy (pre-SegWit) Bitcoin transaction with two
// differences:
//
//   - The 32-bit version field is split: the low 16 bits are the version and
//     the high 16 bits are the transaction type (nVersion | (nType << 16)).
//     src/primitives/transaction.h.
//   - When version >= 3 (SPECIAL_VERSION) and type != 0 (TRANSACTION_NORMAL),
//     a type-specific extra payload — a compactSize-length-prefixed opaque blob
//     — follows the lock time. We treat it as opaque (no need to parse the
//     ProRegTx/CbTx/quorum internals to track payments).
//
// Dash has no SegWit, so there is never a witness marker/flag/data. A normal
// (type 0) transaction is byte-identical to a legacy Bitcoin transaction.
//
// The transaction id is sha256d over the full serialization, including the
// packed version and the extra payload. Only the *block-header* hash diverges
// from Bitcoin (it is X11, not sha256d) — that is handled by the netparams
// HeaderHash hook, not here.
const (
	// dashTxTypeNormal is TRANSACTION_NORMAL; such txns carry no extra payload.
	dashTxTypeNormal = 0
	// dashSpecialTxVersion is CTransaction::SPECIAL_VERSION; at or above this
	// version a non-normal type carries an extra payload.
	dashSpecialTxVersion = 3
	// maxTxPerBlock bounds the transaction count taken from a block's
	// CompactSize before we preallocate, so a malicious peer cannot declare a
	// huge count and trigger an enormous allocation. Mirrors btcd's guard
	// (wire.MaxBlockPayload / the ~10-byte minimum serialized tx).
	maxTxPerBlock = wire.MaxBlockPayload / 10
)

func deserializeDashTx(r io.Reader, tx *Tx) error {
	msgTx := &tx.MsgTx
	dec := newDecoder(r)

	// Packed version|type: low 16 bits = version, high 16 bits = type.
	verType, err := dec.readUint32()
	if err != nil {
		return err
	}
	version := int16(verType & 0xffff)
	msgTx.Version = int32(version)
	tx.Type = uint16(verType >> 16)

	// Inputs. Dash has no SegWit, so a zero count is genuinely zero inputs
	// (some special txns have none), never a witness marker.
	count, err := dec.readCompactSize()
	if err != nil {
		return err
	}
	if count > maxTxInPerMessage {
		return fmt.Errorf("too many transaction inputs to fit into "+
			"max message size [count %d, max %d]", count, maxTxInPerMessage)
	}
	msgTx.TxIn = make([]*wire.TxIn, count)
	for i := range msgTx.TxIn {
		txIn := &wire.TxIn{}
		if err = dec.readTxIn(txIn); err != nil {
			return err
		}
		msgTx.TxIn[i] = txIn
	}

	// Outputs.
	count, err = dec.readCompactSize()
	if err != nil {
		return err
	}
	if count > maxTxOutPerMessage {
		return fmt.Errorf("too many transaction outputs to fit into "+
			"max message size [count %d, max %d]", count, maxTxOutPerMessage)
	}
	msgTx.TxOut = make([]*wire.TxOut, count)
	for i := range msgTx.TxOut {
		txOut := &wire.TxOut{}
		if err = dec.readTxOut(txOut); err != nil {
			return err
		}
		msgTx.TxOut[i] = txOut
	}

	// Lock time.
	msgTx.LockTime, err = dec.readUint32()
	if err != nil {
		return err
	}

	// DIP-2 special-transaction extra payload.
	if version >= dashSpecialTxVersion && tx.Type != dashTxTypeNormal {
		tx.ExtraPayload, err = wire.ReadVarBytes(dec, pver, wire.MaxMessagePayload, "dash extra payload")
		if err != nil {
			return err
		}
	}

	return nil
}

// dashTxHash returns the Dash transaction id: sha256d over the full
// serialization, including the packed version|type field and (for special
// transactions) the trailing extra payload.
func dashTxHash(tx *Tx) chainhash.Hash {
	// Normal transactions serialize identically to a legacy Bitcoin tx, so the
	// standard MsgTx hash is correct and cheaper.
	if tx.Type == dashTxTypeNormal {
		return tx.MsgTx.TxHash()
	}

	var buf bytes.Buffer

	// Packed version|type.
	verType := uint32(uint16(tx.MsgTx.Version)) | (uint32(tx.Type) << 16)
	var vt [4]byte
	binary.LittleEndian.PutUint32(vt[:], verType)
	buf.Write(vt[:])

	// Inputs, outputs and lock time are exactly the standard (non-witness)
	// serialization. Reuse btcd's encoder and drop its leading 4-byte version,
	// which we have already written in the packed form above.
	var inner bytes.Buffer
	if err := tx.MsgTx.SerializeNoWitness(&inner); err != nil {
		// SerializeNoWitness only fails on a writer error; a bytes.Buffer does
		// not error, so this is unreachable in practice.
		return tx.MsgTx.TxHash()
	}
	buf.Write(inner.Bytes()[4:])

	// Trailing extra payload (compactSize length + bytes).
	if err := wire.WriteVarBytes(&buf, pver, tx.ExtraPayload); err != nil {
		return tx.MsgTx.TxHash()
	}

	return chainhash.DoubleHashH(buf.Bytes())
}

// deserializeDashBlock decodes a serialized Dash block: a standard 80-byte
// header, the transaction count, then the (DIP-2) transactions. There is no
// SegWit data and no extension-block trailer. The block-header identity hash is
// X11 rather than sha256d, but that is computed by the header stores via the
// netparams HeaderHash hook, not here.
func deserializeDashBlock(r io.Reader) (*Block, error) {
	hdr := &wire.BlockHeader{}
	if err := hdr.Deserialize(r); err != nil {
		return nil, fmt.Errorf("failed to deserialize block header: %w", err)
	}

	txnCount, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transaction count: %w", err)
	}

	if txnCount > maxTxPerBlock {
		return nil, fmt.Errorf("too many transactions in block: %d (max %d)", txnCount, maxTxPerBlock)
	}

	txns := make([]*Tx, 0, int(txnCount))
	for i := 0; i < cap(txns); i++ {
		tx := &Tx{Chain: string(ChainDASH)}
		if err := deserializeDashTx(r, tx); err != nil {
			return nil, fmt.Errorf("failed to deserialize transaction %d of %d in block %v: %w",
				i+1, txnCount, HeaderHash(ChainDASH, hdr), err)
		}
		txns = append(txns, tx)
	}

	return &Block{
		Chain:        string(ChainDASH),
		Header:       *hdr,
		Transactions: txns,
	}, nil
}
