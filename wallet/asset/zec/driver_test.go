// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"os"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
)

func TestDriverOpen(t *testing.T) {
	dataDir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(dataDir)

	cfg := &asset.WalletConfig{
		Emit:    asset.NewWalletEmitter(make(chan asset.WalletNotification), BipID, tLogger),
		DataDir: dataDir,
		Settings: map[string]string{
			"rpcuser":     "a",
			"rpcpassword": "b",
		},
	}
	d := &Driver{}
	w, err := d.Open(cfg, tLogger, util.Simnet)
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	if w == nil {
		t.Fatalf("nil wallet returned")
	}
}

func TestDriverDecodeCoinID(t *testing.T) {
	d := &Driver{}

	// Shielded case: 32-byte tx hash.
	shielded := make([]byte, 32)
	s, err := d.DecodeCoinID(shielded)
	if err != nil {
		t.Fatalf("DecodeCoinID (shielded) error: %v", err)
	}
	if s != tTxHash.String() {
		// Not necessarily equal, just confirm it decoded to a hash string.
		if len(s) != 64 {
			t.Fatalf("unexpected decoded shielded coin id: %s", s)
		}
	}

	// Transparent case: 36-byte outpoint (delegates to btc.Driver).
	transparent := encodeCoinID(tTxHash, 0)
	s, err = d.DecodeCoinID(transparent)
	if err != nil {
		t.Fatalf("DecodeCoinID (transparent) error: %v", err)
	}
	if s == "" {
		t.Fatalf("expected non-empty decoded transparent coin id")
	}
}

func TestDriverInfo(t *testing.T) {
	d := &Driver{}
	if d.Info() != WalletInfo {
		t.Fatalf("Info() did not return the package WalletInfo")
	}
}

func TestDriverMinLotSize(t *testing.T) {
	d := &Driver{}
	if got := d.MinLotSize(1000); got != MinLotSize(1000) {
		t.Fatalf("MinLotSize mismatch: %d != %d", got, MinLotSize(1000))
	}
}
