package btc

import (
	"strconv"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

type UTxO struct {
	TxHash  *chainhash.Hash
	Vout    uint32
	Address string
	Amount  uint64
}

// OutPoint is the hash and output index of a transaction output.
type OutPoint struct {
	TxHash chainhash.Hash
	Vout   uint32
}

// NewOutPoint is the constructor for a new OutPoint.
func NewOutPoint(txHash *chainhash.Hash, vout uint32) OutPoint {
	return OutPoint{
		TxHash: *txHash,
		Vout:   vout,
	}
}

// String is a string representation of the outPoint.
func (pt OutPoint) String() string {
	return pt.TxHash.String() + ":" + strconv.Itoa(int(pt.Vout))
}

// Output is information about a transaction Output. Output satisfies the
// asset.Coin interface.
type Output struct {
	Pt  OutPoint
	Val uint64
}

// NewOutput is the constructor for an output.
func NewOutput(txHash *chainhash.Hash, vout uint32, value uint64) *Output {
	return &Output{
		Pt:  NewOutPoint(txHash, vout),
		Val: value,
	}
}

// Value returns the value of the Output. Part of the asset.Coin interface.
func (op *Output) Value() uint64 {
	return op.Val
}

// ID is the Output's coin ID. Part of the asset.Coin interface. For BTC, the
// coin ID is 36 bytes = 32 bytes tx hash + 4 bytes big-endian vout.
func (op *Output) ID() util.Bytes {
	return ToCoinID(op.txHash(), op.vout())
}

// String is a string representation of the coin.
func (op *Output) String() string {
	return op.Pt.String()
}

// TxID is the ID of the transaction used to create the coin.
func (op *Output) TxID() string {
	return op.txHash().String()
}

// txHash returns the pointer of the wire.OutPoint's Hash.
func (op *Output) txHash() *chainhash.Hash {
	return &op.Pt.TxHash
}

// vout returns the wire.OutPoint's Index.
func (op *Output) vout() uint32 {
	return op.Pt.Vout
}

// WireOutPoint creates and returns a new *wire.OutPoint for the output.
func (op *Output) WireOutPoint() *wire.OutPoint {
	return wire.NewOutPoint(op.txHash(), op.vout())
}

// ConvertCoin converts the asset.Coin to an Output.
func ConvertCoin(coin asset.Coin) (*Output, error) {
	op, _ := coin.(*Output)
	if op != nil {
		return op, nil
	}
	txHash, vout, err := decodeCoinID(coin.ID())
	if err != nil {
		return nil, err
	}
	return NewOutput(txHash, vout, coin.Value()), nil
}
