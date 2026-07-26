package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/intents"
)

func unitInfo(convFactor uint64) *util.UnitInfo {
	return &util.UnitInfo{Conventional: util.Denomination{ConversionFactor: convFactor}}
}

func TestIntentsAmountScaling(t *testing.T) {
	tests := []struct {
		name       string
		atoms      uint64
		convFactor uint64
		decimals   int
		want       string
	}{
		{"btc sats 1:1", 12345678, 1e8, 8, "12345678"},
		{"near drops to yocto", 5_000_000, 1e8, 24, "50000000000000000000000"},
		{"eth gwei to wei", 1_500_000_000, 1e9, 18, "1500000000000000000"},
		{"xrp drops 1:1", 25_000_000, 1e6, 6, "25000000"},
	}
	for _, tt := range tests {
		got, err := toIntentsAmount(tt.atoms, unitInfo(tt.convFactor), tt.decimals)
		if err != nil {
			t.Fatalf("%s: toIntentsAmount: %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: toIntentsAmount = %s, want %s", tt.name, got, tt.want)
		}
		back, err := fromIntentsAmount(got, unitInfo(tt.convFactor), tt.decimals)
		if err != nil {
			t.Fatalf("%s: fromIntentsAmount: %v", tt.name, err)
		}
		if back != tt.atoms {
			t.Errorf("%s: round trip = %d, want %d", tt.name, back, tt.atoms)
		}
	}

	// Truncation on the way back down.
	atoms, err := fromIntentsAmount("50000000000000000009999", unitInfo(1e8), 24)
	if err != nil {
		t.Fatalf("fromIntentsAmount: %v", err)
	}
	if atoms != 5_000_000 {
		t.Errorf("truncation: got %d, want 5000000", atoms)
	}

	// Overflow detection.
	if _, err := fromIntentsAmount("99999999999999999999999999999999", unitInfo(1e8), 8); err == nil {
		t.Errorf("expected overflow error")
	}

	// Non-power-of-ten conversion factor.
	if _, err := toIntentsAmount(1, unitInfo(3), 8); err == nil {
		t.Errorf("expected power-of-ten error")
	}
}

func TestIntentsSwapStates(t *testing.T) {
	for state, terminal := range map[string]bool{
		IntentsStateQuoted:              false,
		IntentsStateDepositSent:         false,
		IntentsStateExpired:             true,
		intents.StatusProcessing:        false,
		intents.StatusKnownDepositTx:    false,
		intents.StatusIncompleteDeposit: false,
		intents.StatusSuccess:           true,
		intents.StatusRefunded:          true,
		intents.StatusFailed:            true,
	} {
		if got := (&IntentsSwap{State: state}).Terminal(); got != terminal {
			t.Errorf("Terminal(%s) = %v, want %v", state, got, terminal)
		}
	}
}

func TestSwapFromStatus(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "intents", "testdata", "status-pending.json"))
	if err != nil {
		t.Fatalf("error reading fixture: %v", err)
	}
	status := new(intents.StatusResponse)
	if err := json.Unmarshal(b, status); err != nil {
		t.Fatalf("error decoding fixture: %v", err)
	}

	// Stub resolver: the fixture's destination is nep141:sol.omft.near.
	resolve := func(assetID string) (uint32, bool) {
		if assetID == "nep141:sol.omft.near" {
			return 501, true
		}
		return 0, false
	}
	swap, err := swapFromStatus(resolve, 0, "bc1qdepositaddr", "sometxid", status)
	if err != nil {
		t.Fatalf("swapFromStatus: %v", err)
	}
	if !swap.Reconstructed {
		t.Fatalf("record not flagged Reconstructed")
	}
	// PENDING_DEPOSIT maps to the local DEPOSIT_SENT (a deposit tx exists,
	// or reconstruction wouldn't have probed this address).
	if swap.State != IntentsStateDepositSent {
		t.Fatalf("state = %s, want %s", swap.State, IntentsStateDepositSent)
	}
	if swap.OriginAsset != "nep141:btc.omft.near" {
		t.Fatalf("origin asset = %s", swap.OriginAsset)
	}
	if swap.DestAssetID != 501 {
		t.Fatalf("dest asset ID = %d, want 501 (sol)", swap.DestAssetID)
	}
	if swap.DepositTxID != "sometxid" {
		t.Fatalf("deposit txid not carried")
	}
	if len(swap.QuoteEnvelope) == 0 {
		t.Fatalf("no quote envelope retained")
	}
}

// TestSwapFromStatusUnknownChain covers the DestAssetID sentinel: an
// unresolved destination must be intentsAssetUnknown, never 0 (BTC).
func TestSwapFromStatusUnknownChain(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "intents", "testdata", "status-pending.json"))
	if err != nil {
		t.Fatalf("error reading fixture: %v", err)
	}
	status := new(intents.StatusResponse)
	if err := json.Unmarshal(b, status); err != nil {
		t.Fatalf("error decoding fixture: %v", err)
	}
	resolve := func(string) (uint32, bool) { return 0, false } // nothing resolves
	swap, err := swapFromStatus(resolve, 0, "bc1qdepositaddr", "sometxid", status)
	if err != nil {
		t.Fatalf("swapFromStatus: %v", err)
	}
	if swap.DestAssetID != intentsAssetUnknown {
		t.Fatalf("unresolved dest = %d, want sentinel %d (must not be 0/BTC)", swap.DestAssetID, intentsAssetUnknown)
	}
	if got := intentsAssetLabel(swap.DestAssetID, swap.DestAsset); got != swap.DestAsset {
		t.Fatalf("unknown asset label = %q, want the Intents asset ID %q", got, swap.DestAsset)
	}
}

func TestIntentsDeadlines(t *testing.T) {
	if d := intentsDeadline(0); d != 2*time.Hour {
		t.Errorf("btc deadline %v", d)
	}
	if d := intentsDeadline(501); d != 45*time.Minute {
		t.Errorf("sol default deadline %v", d)
	}
}
