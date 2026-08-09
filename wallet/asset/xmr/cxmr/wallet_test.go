// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build xmr

package cxmr

// These tests drive the real monero_c library, so they need
// libmonero_wallet2_api_c next to the test binary — the install name is
// @executable_path, so DYLD/LD paths do not help:
//
//	go test -tags xmr -c -o /tmp/cx/cxmr.test ./wallet/asset/xmr/cxmr/
//	cp wallet/asset/xmr/lib/darwin-arm64/libmonero_wallet2_api_c.dylib /tmp/cx/
//	(cd /tmp/cx && ./cxmr.test)
//
// They create wallets on disk but never touch the network.

import (
	"strings"
	"testing"
)

// TestStringGettersStayValidAfterFree exercises every getter that releases its
// monero_c buffer. The library hands back strdup'd memory the caller owns, so
// these must free — but freeing a buffer the library still owned would corrupt
// or crash instead, and that is what this test rules out. Values are captured
// once and re-read many times; a bad free shows up as a changed value, garbage,
// or a fault.
func TestStringGettersStayValidAfterFree(t *testing.T) {
	wm := GetWalletManager()
	if wm == nil {
		t.Fatal("GetWalletManager returned nil")
	}
	w, err := wm.CreateWallet(t.TempDir()+"/testwallet", "pw", "English", NetworkTestnet)
	if err != nil {
		t.Fatalf("CreateWallet: %v (%s)", err, wm.ErrorString())
	}
	defer wm.CloseWallet(w, false)

	addr := w.Address(0, 0)
	seed := w.Seed("")
	secSpend := w.SecretSpendKey()
	pubSpend := w.PublicSpendKey()
	secView := w.SecretViewKey()
	pubView := w.PublicViewKey()
	lang := w.GetSeedLanguage()
	filename := w.Filename()
	keysFile := w.KeysFilename()

	if len(addr) < 90 {
		t.Errorf("Address = %q, want a full testnet address", addr)
	}
	if n := len(strings.Fields(seed)); n != 25 {
		t.Errorf("Seed has %d words, want 25", n)
	}
	for name, k := range map[string]string{
		"SecretSpendKey": secSpend, "PublicSpendKey": pubSpend,
		"SecretViewKey": secView, "PublicViewKey": pubView,
	} {
		if len(k) != 64 {
			t.Errorf("%s has length %d, want 64 hex chars", name, len(k))
		}
	}
	if lang != "English" {
		t.Errorf("GetSeedLanguage = %q, want English", lang)
	}
	if !strings.HasSuffix(keysFile, ".keys") {
		t.Errorf("KeysFilename = %q, want a .keys path", keysFile)
	}
	if w.Path() == "" {
		t.Error("Path is empty")
	}
	if got := DisplayAmount(1_500_000_000_000); got != "1.500000000000" {
		t.Errorf("DisplayAmount = %q, want 1.500000000000", got)
	}

	for i := 0; i < 2000; i++ {
		if got := w.Address(0, 0); got != addr {
			t.Fatalf("Address changed on call %d: %q != %q", i, got, addr)
		}
		if got := w.SecretSpendKey(); got != secSpend {
			t.Fatalf("SecretSpendKey changed on call %d", i)
		}
		if got := w.Seed(""); got != seed {
			t.Fatalf("Seed changed on call %d", i)
		}
		if got := w.Filename(); got != filename {
			t.Fatalf("Filename changed on call %d", i)
		}
		if got := w.PublicViewKey(); got != pubView {
			t.Fatalf("PublicViewKey changed on call %d", i)
		}
	}
}

// TestSubaddressWalkStaysStable covers the access pattern behind
// OwnsDepositAddress, which reads every subaddress on every call — the site
// that leaked one buffer per subaddress per invocation, and so the one where a
// wrong free would surface fastest.
func TestSubaddressWalkStaysStable(t *testing.T) {
	wm := GetWalletManager()
	w, err := wm.CreateWallet(t.TempDir()+"/subaddrwallet", "pw", "English", NetworkTestnet)
	if err != nil {
		t.Fatalf("CreateWallet: %v (%s)", err, wm.ErrorString())
	}
	defer wm.CloseWallet(w, false)

	for i := 0; i < 200; i++ {
		w.AddSubaddress(0, "label")
	}
	n := w.NumSubaddresses(0)
	if n < 200 {
		t.Fatalf("NumSubaddresses = %d, want >= 200", n)
	}

	seen := make(map[string]bool, n)
	for round := 0; round < 50; round++ {
		for i := uint64(0); i < n; i++ {
			a := w.Address(0, i)
			if a == "" {
				t.Fatalf("empty subaddress at index %d", i)
			}
			seen[a] = true
		}
	}
	if uint64(len(seen)) != n {
		t.Errorf("saw %d distinct subaddresses across 50 rounds, want %d — values are not stable", len(seen), n)
	}
}
