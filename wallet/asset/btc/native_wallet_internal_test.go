//go:build !harness

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"math"
	"testing"
)

// TestBlockNumberFromHeight guards the unmined-sentinel overflow: wtxmgr returns
// Block.Height == -1 for an unmined tx, and a raw uint64(-1) is ~1.8e19. That
// value used to be persisted as the tx's BlockNumber and corrupt history.
func TestBlockNumberFromHeight(t *testing.T) {
	tests := []struct {
		name string
		h    int32
		want uint64
	}{
		{"unmined sentinel", -1, 0},
		{"arbitrary negative", -1000, 0},
		{"genesis", 0, 0},
		{"height 1", 1, 1},
		{"typical height", 670, 670},
		{"max int32", math.MaxInt32, uint64(math.MaxInt32)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := blockNumberFromHeight(tt.h); got != tt.want {
				t.Fatalf("blockNumberFromHeight(%d) = %d, want %d", tt.h, got, tt.want)
			}
		})
	}
}

// TestBtcToSats guards the float-truncation bug: uint64(v*1e8) truncates because
// the product is not exactly representable (0.29 → 28999999), so history amounts
// were off by a satoshi for a large class of ordinary values.
func TestBtcToSats(t *testing.T) {
	tests := []struct {
		name string
		v    float64
		want uint64
	}{
		{"truncation case", 0.29, 29000000},
		{"another truncation case", 0.07, 7000000},
		{"whole coin", 1.0, 100000000},
		{"one satoshi", 0.00000001, 1},
		{"zero", 0.0, 0},
		{"negative guards to zero", -1.0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := btcToSats(tt.v); got != tt.want {
				t.Fatalf("btcToSats(%v) = %d, want %d", tt.v, got, tt.want)
			}
		})
	}
}

// TestScanFloor exercises the pending scan-floor register that keeps
// late-notified mined transactions (blocks below the scan cursor) reachable
// by the next history scan, even when the triggering notification's scan was
// skipped by the syncingTxHistory guard.
func TestScanFloor(t *testing.T) {
	w := &NativeWallet{}

	// No floor armed: the cursor passes through.
	if got := w.consumeScanFloor(100); got != 100 {
		t.Fatalf("no-floor consume = %d, want 100", got)
	}

	// Arming keeps the minimum.
	w.armScanFloor(90)
	w.armScanFloor(95) // higher: ignored
	w.armScanFloor(85) // lower: replaces
	if got := w.consumeScanFloor(100); got != 85 {
		t.Fatalf("armed consume = %d, want 85", got)
	}
	// Consumed: the next scan is back to the cursor.
	if got := w.consumeScanFloor(100); got != 100 {
		t.Fatalf("post-consume = %d, want 100", got)
	}

	// A floor above the cursor never raises it.
	w.armScanFloor(150)
	if got := w.consumeScanFloor(100); got != 100 {
		t.Fatalf("higher-floor consume = %d, want 100", got)
	}

	// A never-scanned wallet (cursor 0) already scans from the beginning; the
	// floor must not turn that into a partial scan.
	w.armScanFloor(85)
	if got := w.consumeScanFloor(0); got != 0 {
		t.Fatalf("initial-scan consume = %d, want 0", got)
	}
}
