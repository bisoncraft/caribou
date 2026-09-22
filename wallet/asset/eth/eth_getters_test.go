// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestCoinAccessors(t *testing.T) {
	t.Parallel()
	txHash := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111")
	c := &coin{txHash: txHash, value: 42}

	if got := string(c.ID()); got != string(txHash[:]) {
		t.Errorf("ID() = %x, want %x", got, txHash[:])
	}
	if got := c.TxID(); got != txHash.String() {
		t.Errorf("TxID() = %q, want %q", got, txHash.String())
	}
	if got := c.String(); got != c.TxID() {
		t.Errorf("String() = %q, want TxID() %q", got, c.TxID())
	}
	if got := c.Value(); got != 42 {
		t.Errorf("Value() = %d, want 42", got)
	}
}

func TestAssetWalletInfo(t *testing.T) {
	t.Parallel()
	wi := WalletInfo
	w := &assetWallet{wi: wi}
	got := w.Info()
	if got.Name != wi.Name {
		t.Errorf("Info().Name = %q, want %q", got.Name, wi.Name)
	}
	// Info copies the value, so mutating the result must not affect the
	// wallet's own wi field.
	got.Name = "mutated"
	if w.Info().Name == "mutated" {
		t.Error("Info should isolate scalar fields from the wallet's own wi")
	}
}

func TestBaseAssetWallet(t *testing.T) {
	t.Parallel()
	const baseAssetID = 60
	aw := &assetWallet{assetID: baseAssetID}
	w := &baseWallet{
		baseChainID: baseAssetID,
		wallets:     map[uint32]*assetWallet{baseAssetID: aw},
	}

	got, err := w.baseAssetWallet()
	if err != nil {
		t.Fatal(err)
	}
	if got != aw {
		t.Error("baseAssetWallet returned a different *assetWallet than registered")
	}

	// A wallet with no base-asset entry (e.g. never finished connecting)
	// must error rather than return a nil wallet.
	empty := &baseWallet{baseChainID: baseAssetID, wallets: map[uint32]*assetWallet{}}
	if _, err := empty.baseAssetWallet(); err == nil {
		t.Error("expected an error when the base asset wallet is missing")
	}
}
