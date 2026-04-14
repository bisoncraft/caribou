// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"encoding/hex"
	"fmt"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex"
)

// coin represents a NEAR transaction output.
type coin struct {
	txHash [32]byte
	value  uint64
}

var _ asset.Coin = (*coin)(nil)

func (c *coin) ID() dex.Bytes {
	return c.txHash[:]
}

func (c *coin) String() string {
	return hex.EncodeToString(c.txHash[:])
}

func (c *coin) Value() uint64 {
	return c.value
}

func (c *coin) TxID() string {
	return hex.EncodeToString(c.txHash[:])
}

// decodeCoinID decodes a coin ID byte slice into a hex string.
func decodeCoinID(coinID []byte) (string, error) {
	if len(coinID) != 32 {
		return "", fmt.Errorf("expected 32-byte coin ID, got %d bytes", len(coinID))
	}
	return hex.EncodeToString(coinID), nil
}
