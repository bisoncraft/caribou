package bisonwire

import (
	"github.com/btcsuite/btcd/btcutil/gcs"
	"github.com/btcsuite/btcd/btcutil/gcs/builder"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// BuildBasicFilter constructs a BIP-158 basic filter for block, keyed by the
// chain's canonical block-header hash (X11 for Dash, double-SHA256 otherwise).
//
// btcutil's builder.BuildBasicFilter always keys the filter by double-SHA256
// (block.BlockHash()), which produces the wrong filter — hence the wrong filter
// header and SipHash matching key — for chains whose block identity differs.
// The element selection here mirrors btcutil's (every non-empty, non-OP_RETURN
// output script); only the key differs.
//
// Like the callers' prior builder.BuildBasicFilter(block, nil) usage, this adds
// no prevout scripts, so it is only correct for blocks whose inputs contribute
// nothing to the filter — i.e. the genesis block, whose sole input is the
// coinbase null input. Do not use it to build filters for arbitrary blocks.
func BuildBasicFilter(chain Chain, block *wire.MsgBlock) (*gcs.Filter, error) {
	keyHash := HeaderHash(chain, &block.Header)
	b := builder.WithKeyHash(&keyHash)
	if _, err := b.Key(); err != nil {
		return nil, err
	}
	for _, tx := range block.Transactions {
		for _, txOut := range tx.TxOut {
			if len(txOut.PkScript) == 0 || txOut.PkScript[0] == txscript.OP_RETURN {
				continue
			}
			b.AddEntry(txOut.PkScript)
		}
	}
	return b.Build()
}
