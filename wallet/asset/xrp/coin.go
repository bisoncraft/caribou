// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
)

// coin represents a sent XRP transaction.
type coin struct {
	txHash [32]byte
	value  uint64
}

var _ asset.Coin = (*coin)(nil)

func (c *coin) ID() util.Bytes {
	return c.txHash[:]
}

func (c *coin) String() string {
	return strings.ToUpper(hex.EncodeToString(c.txHash[:]))
}

func (c *coin) Value() uint64 {
	return c.value
}

func (c *coin) TxID() string {
	return c.String()
}

// decodeCoinID decodes a 32-byte coin ID into an uppercase hex XRP tx hash.
func decodeCoinID(coinID []byte) (string, error) {
	if len(coinID) != 32 {
		return "", fmt.Errorf("expected 32-byte coin ID, got %d bytes", len(coinID))
	}
	return strings.ToUpper(hex.EncodeToString(coinID)), nil
}
