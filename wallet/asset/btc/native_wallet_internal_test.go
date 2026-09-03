//go:build !harness

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"math"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/btcsuite/btcd/chaincfg"
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
	if got, floor := w.peekScanFloor(100); got != 100 || floor != 0 {
		t.Fatalf("no-floor peek = (%d, %d), want (100, 0)", got, floor)
	}

	// Arming keeps the minimum.
	w.armScanFloor(90)
	w.armScanFloor(95) // higher: ignored
	w.armScanFloor(85) // lower: replaces
	got, floor := w.peekScanFloor(100)
	if got != 85 || floor != 85 {
		t.Fatalf("armed peek = (%d, %d), want (85, 85)", got, floor)
	}
	// Peeking does not consume: the scan has not run yet.
	if got, _ := w.peekScanFloor(100); got != 85 {
		t.Fatalf("second peek = %d, want 85; peek must not consume", got)
	}
	// Retired once the scan that used it completed.
	w.clearScanFloor(floor)
	if got, _ := w.peekScanFloor(100); got != 100 {
		t.Fatalf("post-clear peek = %d, want 100", got)
	}

	// A floor above the cursor never raises it, but a scan from the cursor
	// covers those blocks, so it is still retired.
	w.armScanFloor(150)
	got, floor = w.peekScanFloor(100)
	if got != 100 || floor != 150 {
		t.Fatalf("higher-floor peek = (%d, %d), want (100, 150)", got, floor)
	}
	w.clearScanFloor(floor)
	if armed := w.scanFloor.Load(); armed != 0 {
		t.Fatalf("floor still armed at %d after a scan that covered it", armed)
	}

	// A never-scanned wallet (cursor 0) already scans from the beginning; the
	// floor must not turn that into a partial scan.
	w.armScanFloor(85)
	if got, _ := w.peekScanFloor(0); got != 0 {
		t.Fatalf("initial-scan peek = %d, want 0", got)
	}
}

// TestScanFloorSurvivesFailedScan covers the paths where the scan the floor was
// peeked for never reaches the blocks it names. scanNewTransactions used to
// consume the register up front, so a ListSinceBlock error - or a context
// cancellation partway through - returned with the cursor still at its higher
// value and the override gone. Nothing rescans below the cursor after that, so
// the late-notified mined transaction the floor was armed for is dropped from tx
// history for the life of the process.
//
// Driven through the register rather than through scanNewTransactions: btcw is a
// concrete *wallet.Wallet, so ListSinceBlock cannot be made to fail without an
// interface seam that exists for nothing else.
func TestScanFloorSurvivesFailedScan(t *testing.T) {
	w := &NativeWallet{}

	// A late-notified tx in block 85 arms the floor under a cursor at 100.
	w.armScanFloor(85)
	from, floor := w.peekScanFloor(100)
	if from != 85 {
		t.Fatalf("scan start = %d, want 85", from)
	}

	// The scan fails, so clearScanFloor is never reached. The next one must
	// still reach block 85.
	if from, _ := w.peekScanFloor(100); from != 85 {
		t.Fatalf("scan start after a failed scan = %d, want 85", from)
	}

	// The retry succeeds and retires the floor.
	w.clearScanFloor(floor)
	if from, _ := w.peekScanFloor(100); from != 100 {
		t.Fatalf("scan start after a completed scan = %d, want 100", from)
	}

	// A floor armed while a scan is running names blocks below where that scan
	// started, so completing it must not retire the newer, lower value.
	w.armScanFloor(85)
	_, floor = w.peekScanFloor(100)
	w.armScanFloor(70) // a second late notification, mid-scan
	w.clearScanFloor(floor)
	if from, _ := w.peekScanFloor(100); from != 70 {
		t.Fatalf("scan start = %d, want 70; a floor armed mid-scan was retired by it", from)
	}
}

// TestNativeWalletChainIdentity guards the chain identity stamped on master
// transaction database records. This wallet type serves the clone assets
// (ltc, dash) too, so the IDs must come from ChainConfig.Info — records
// stamped with the btc package's own BipID (0) are rejected by every
// clone's network-scoped database.
func TestNativeWalletChainIdentity(t *testing.T) {
	const cloneID = 2 // ltc
	cc := ChainConfig{
		ChainParams: &chaincfg.MainNetParams,
		Info: &asset.WalletInfo{Network: &asset.Network{
			ID:            cloneID,
			NativeAssetID: cloneID,
		}},
	}
	log := util.StdOutLogger("TEST", util.LevelInfo)
	w := NewNativeWallet(cc, &asset.WalletConfig{}, log)
	if w.networkID != cloneID || w.assetID != cloneID {
		t.Fatalf("wallet identity = network %d, asset %d; want %d, %d",
			w.networkID, w.assetID, cloneID, cloneID)
	}
}
