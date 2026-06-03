// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestDecodeCoinID(t *testing.T) {
	t.Run("tx hash", func(t *testing.T) {
		raw := bytes.Repeat([]byte{0xab}, common.HashLength)
		cid, err := DecodeCoinID(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(cid.TxHash[:], raw) {
			t.Errorf("TxHash = %x, want %x", cid.TxHash, raw)
		}
	})

	t.Run("wrong length", func(t *testing.T) {
		for _, n := range []int{0, 1, common.HashLength - 1, common.HashLength + 1, common.HashLength * 2} {
			if _, err := DecodeCoinID(make([]byte, n)); err == nil {
				t.Errorf("expected error for coin ID of length %d", n)
			}
		}
	})
}
