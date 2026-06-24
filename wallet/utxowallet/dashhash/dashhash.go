// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package dashhash provides Dash's X11 proof-of-work / block-identity hash.
//
// Unlike Bitcoin (and Litecoin), Dash's canonical block identifier
// CBlockHeader::GetHash() is the X11 hash of the serialized 80-byte header, not
// double-SHA256 — and hashPrevBlock references it. So btcd's
// wire.BlockHeader.BlockHash() (sha256d) is wrong for Dash anywhere headers are
// linked or indexed; those sites must use HashX11 instead. For Dash the block
// identity hash and the PoW hash are the same value (this hash); transaction
// ids remain sha256d.
package dashhash

import (
	x11 "github.com/bitbandi/go-x11"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// HashX11 returns the X11 hash of b, which is typically a serialized 80-byte
// block header. The returned chainhash.Hash holds the hash in internal
// (little-endian) byte order, matching wire.BlockHeader.BlockHash(); its
// String() method yields the big-endian display form.
//
// The underlying x11.Hash is stateful and not safe for concurrent use, so a
// fresh one is allocated per call.
func HashX11(b []byte) chainhash.Hash {
	var out chainhash.Hash
	x11.New().Hash(b, out[:])
	return out
}
