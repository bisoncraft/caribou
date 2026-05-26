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
		if cid.IsRelay {
			t.Error("IsRelay should be false for a plain tx hash")
		}
		if !bytes.Equal(cid.TxHash[:], raw) {
			t.Errorf("TxHash = %x, want %x", cid.TxHash, raw)
		}
		if (cid.RelayTaskHash != common.Hash{}) {
			t.Errorf("RelayTaskHash = %x, want zero", cid.RelayTaskHash)
		}
	})

	t.Run("relay redemption", func(t *testing.T) {
		taskHash := bytes.Repeat([]byte{0x11}, common.HashLength)
		txHash := bytes.Repeat([]byte{0x22}, common.HashLength)
		raw := append(append([]byte{}, taskHash...), txHash...)

		cid, err := DecodeCoinID(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !cid.IsRelay {
			t.Error("IsRelay should be true for a relay coin ID")
		}
		if !bytes.Equal(cid.RelayTaskHash[:], taskHash) {
			t.Errorf("RelayTaskHash = %x, want %x", cid.RelayTaskHash, taskHash)
		}
		if !bytes.Equal(cid.TxHash[:], txHash) {
			t.Errorf("TxHash = %x, want %x", cid.TxHash, txHash)
		}
	})

	t.Run("wrong length", func(t *testing.T) {
		for _, n := range []int{0, 1, common.HashLength - 1, common.HashLength + 1, RelayCoinIDLength - 1, RelayCoinIDLength + 1} {
			if _, err := DecodeCoinID(make([]byte, n)); err == nil {
				t.Errorf("expected error for coin ID of length %d", n)
			}
		}
	})
}
