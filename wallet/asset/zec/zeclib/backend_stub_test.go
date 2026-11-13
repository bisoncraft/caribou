// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !zec || !cgo

package zeclib

import (
	"errors"
	"testing"
)

// Without the Rust backend, the package compiles and says so when used.
func TestNotBuilt(t *testing.T) {
	if _, err := Open(&Config{DBPath: "wallet.sqlite", Network: Mainnet}, nil); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("Open: %v, want ErrNotBuilt", err)
	}
	w := newWallet(&Config{Network: Mainnet}, newFakeBackend(), nil)
	defer w.Close()
	if _, err := w.TransparentReceiver("u1..."); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("TransparentReceiver: %v, want ErrNotBuilt", err)
	}
}
