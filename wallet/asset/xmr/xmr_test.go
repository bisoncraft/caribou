// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build xmr

package xmr

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
)

// tOfflineWallet creates a wallet in dataDir and opens it without connecting
// to a daemon.
func tOfflineWallet(t *testing.T, dataDir string, create bool) *ExchangeWallet {
	t.Helper()

	pass := []byte("abc")
	logger := util.NewLogger("XMRTEST", util.LevelError, os.Stdout)
	drv := &Driver{}

	if create {
		seed := make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		err := drv.Create(&asset.CreateWalletParams{
			Type:    walletTypeNative,
			Seed:    seed,
			Pass:    pass,
			DataDir: dataDir,
			Net:     util.Simnet,
			Logger:  logger,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	w, err := drv.Open(&asset.WalletConfig{
		Type:    walletTypeNative,
		DataDir: dataDir,
	}, logger, util.Simnet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	xw := w.(*ExchangeWallet)
	if err := xw.OpenWithPW(context.Background(), pass); err != nil {
		t.Fatalf("OpenWithPW: %v", err)
	}
	t.Cleanup(func() { xw.Close() })
	return xw
}

// TestDepositAddressPersisted checks that a new deposit subaddress is on disk
// as soon as DepositAddress returns. Close saves the wallet and would hide a
// missing save, so the data dir is copied while the wallet is still open, as
// a process killed at that moment would leave it, and the copy is reopened.
func TestDepositAddressPersisted(t *testing.T) {
	dataDir := t.TempDir()
	w := tOfflineWallet(t, dataDir, true)

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	crashDir := t.TempDir()
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dataDir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if err := os.WriteFile(filepath.Join(crashDir, e.Name()), b, 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	reopened := tOfflineWallet(t, crashDir, false)
	owns, err := reopened.OwnsDepositAddress(addr)
	if err != nil {
		t.Fatalf("OwnsDepositAddress: %v", err)
	}
	if !owns {
		t.Fatalf("reopened wallet does not own deposit address %s", addr)
	}

	// The reopened wallet must continue from the next index rather than
	// hand the same address out again.
	next, err := reopened.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}
	if next == addr {
		t.Fatalf("reopened wallet reissued deposit address %s", addr)
	}
}
