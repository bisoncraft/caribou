// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
)

// coin represents a Hyperliquid spot transfer.
type coin struct {
	// txID is either a 0x-prefixed 32-byte hex hash (confirmed) or a
	// nonce-based placeholder (pending, fmt "0x%016x" of unix milliseconds).
	txID  string
	value uint64 // microHYPE
}

var _ asset.Coin = (*coin)(nil)

func (c *coin) ID() util.Bytes {
	return coinIDBytes(c.txID)
}

func (c *coin) String() string { return c.txID }
func (c *coin) Value() uint64  { return c.value }
func (c *coin) TxID() string   { return c.txID }

// coinIDBytes converts a tx ID string to a 32-byte coin ID.
// For confirmed txs (0x + 64 hex chars) it decodes the hash.
// For nonce-based pending IDs it encodes the nonce as big-endian uint64,
// left-padded to 32 bytes.
func coinIDBytes(txID string) []byte {
	h := strings.TrimPrefix(txID, "0x")
	if len(h) == 64 {
		b, err := hex.DecodeString(h)
		if err == nil {
			return b
		}
	}
	// Nonce-based placeholder: parse the hex-encoded nonce from "0x%016x".
	b := make([]byte, 32)
	if len(h) == 16 {
		n, err := strconv.ParseUint(h, 16, 64)
		if err == nil {
			binary.BigEndian.PutUint64(b[24:], n)
			return b
		}
	}
	// Last resort: copy raw bytes.
	copy(b[32-min(len(txID), 32):], []byte(txID))
	return b
}

// decodeCoinID decodes a 32-byte coin ID into a hex string.
func decodeCoinID(coinID []byte) (string, error) {
	if len(coinID) != 32 {
		return "", fmt.Errorf("expected 32-byte coin ID, got %d bytes", len(coinID))
	}
	return "0x" + hex.EncodeToString(coinID), nil
}

// pendingIDFromCoinString converts a decoded 32-byte coin ID back to the
// nonce-based pending tx ID form when the coin ID was derived from a pending
// placeholder. Returns false for normal transaction hashes.
func pendingIDFromCoinString(txID string) (string, bool) {
	h := strings.TrimPrefix(txID, "0x")
	if len(h) != 64 || h[:48] != strings.Repeat("0", 48) {
		return "", false
	}
	return "0x" + h[48:], true
}
