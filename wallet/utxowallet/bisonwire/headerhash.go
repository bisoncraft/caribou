package bisonwire

import (
	"bytes"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/dashhash"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// HeaderHash returns the canonical block-header hash (block identity) for the
// given chain. Most chains identify blocks by double-SHA256, which is exactly
// wire.BlockHeader.BlockHash(); Dash identifies blocks by the X11 hash of the
// serialized 80-byte header (also its proof-of-work hash). This is the single
// source of truth for chain-aware header hashing: the netparams HeaderHash hook
// and the spv/electrum header stores all resolve to it, so block linkage,
// indexing, and GCS filter keys stay consistent with the network. Transaction
// ids are unaffected (always sha256d).
func HeaderHash(chain Chain, hdr *wire.BlockHeader) chainhash.Hash {
	switch chain {
	case ChainDASH:
		var buf bytes.Buffer
		// wire.BlockHeader.Serialize cannot fail writing to a bytes.Buffer.
		_ = hdr.Serialize(&buf)
		return dashhash.HashX11(buf.Bytes())
	default:
		return hdr.BlockHash()
	}
}

// HeaderHash returns this block's canonical header hash for its chain.
func (b *Block) HeaderHash() chainhash.Hash {
	return HeaderHash(Chain(b.Chain), &b.Header)
}
