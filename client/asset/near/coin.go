// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"fmt"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex"
	"github.com/decred/base58"
)

// coin represents a NEAR transaction.
type coin struct {
	txHash [32]byte // raw 32-byte NEAR tx hash
	value  uint64
}

var _ asset.Coin = (*coin)(nil)

func (c *coin) ID() dex.Bytes {
	return c.txHash[:]
}

func (c *coin) String() string {
	return base58.Encode(c.txHash[:])
}

func (c *coin) Value() uint64 {
	return c.value
}

// TxID returns the NEAR transaction hash as a base58 string, matching the
// format used by NEAR explorers and RPC responses.
func (c *coin) TxID() string {
	return base58.Encode(c.txHash[:])
}

// decodeCoinID decodes a 32-byte coin ID into a base58 NEAR tx hash string.
func decodeCoinID(coinID []byte) (string, error) {
	if len(coinID) != 32 {
		return "", fmt.Errorf("expected 32-byte coin ID, got %d bytes", len(coinID))
	}
	return base58.Encode(coinID), nil
}
