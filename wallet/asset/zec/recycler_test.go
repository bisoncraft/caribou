// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddressRecycler(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recycled.txt")

	// No file yet: should start empty without error.
	ar, err := newAddressRecycler(path, tLogger)
	if err != nil {
		t.Fatalf("newAddressRecycler error: %v", err)
	}
	if got := ar.Address(); got != "" {
		t.Fatalf("expected no address available, got %q", got)
	}

	ar.ReturnAddresses([]string{"addr1", "addr2"})
	// Returning an already-indexed address should be a no-op (just logs).
	ar.ReturnAddresses([]string{"addr1"})

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		a := ar.Address()
		if a == "" {
			t.Fatalf("expected an address on iteration %d", i)
		}
		seen[a] = true
	}
	if !seen["addr1"] || !seen["addr2"] {
		t.Fatalf("did not get back both addresses: %+v", seen)
	}
	if got := ar.Address(); got != "" {
		t.Fatalf("expected addresses exhausted, got %q", got)
	}

	ar.ReturnAddresses([]string{"addr3"})
	ar.WriteRecycledAddrsToFile()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("error reading recycle file: %v", err)
	}
	if string(b) != "addr3" {
		t.Fatalf("unexpected file contents: %q", string(b))
	}

	// Loading from an existing file should pick up the cached address.
	ar2, err := newAddressRecycler(path, tLogger)
	if err != nil {
		t.Fatalf("newAddressRecycler (reload) error: %v", err)
	}
	if got := ar2.Address(); got != "addr3" {
		t.Fatalf("expected addr3 from reload, got %q", got)
	}
}

func TestAddressRecyclerReadError(t *testing.T) {
	dir := t.TempDir()
	if _, err := newAddressRecycler(dir, tLogger); err == nil {
		t.Fatalf("expected error reading a directory as a file")
	}
}
