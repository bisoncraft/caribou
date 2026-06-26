// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package sol

import (
	"fmt"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/decred/base58"
)

type coin struct {
	signature [64]byte
	value     uint64
}

var _ asset.Coin = (*coin)(nil)

func (c *coin) ID() util.Bytes {
	return c.signature[:]
}

func (c *coin) String() string {
	return base58.Encode(c.signature[:])
}

func (c *coin) Value() uint64 {
	return c.value
}

func (c *coin) TxID() string {
	return c.String()
}

func decodeCoinID(coinID []byte) (string, error) {
	if len(coinID) != 64 {
		return "", fmt.Errorf("expected 64-byte coin ID, got %d bytes", len(coinID))
	}
	return base58.Encode(coinID), nil
}
