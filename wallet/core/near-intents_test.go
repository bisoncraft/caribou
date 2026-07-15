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

func TestIntentsChainOf(t *testing.T) {
	tests := []struct {
		assetID string
		want    uint32
		ok      bool
	}{
		{"nep141:btc.omft.near", 0, true},
		{"nep141:zec.omft.near", 133, true},
		{"nep141:sol.omft.near", 501, true},
		{"nep141:sol-b9c68f94ec8fd160137af8cdfe5e61cd68e2afba.omft.near", 501, true},
		{"nep141:eth-0xdac17f958d2ee523a2206206994597c13d831ec7.omft.near", 60, true},
		{"nep141:wrap.near", 397, true},
		{"nep141:17208628f84f5d6ad33f0da3bbbeb27ffcb398eac501a31bd6ad2011e36133a1", 397, true},
		{"nep245:v2_1.omni.hot.tg:137_11111111111111111111", 0, false},
		{"1cs_v1:btc:native:coin", 0, false},
	}
	for _, tt := range tests {
		got, ok := intentsChainOf(tt.assetID)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("intentsChainOf(%s) = %d, %v; want %d, %v", tt.assetID, got, ok, tt.want, tt.ok)
		}
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
		if got := intentsSwapTerminal(state); got != terminal {
			t.Errorf("intentsSwapTerminal(%s) = %v, want %v", state, got, terminal)
		}
	}
}

func TestIntentsSwapRecordRoundTrip(t *testing.T) {
	swap := &IntentsSwap{
		DepositAddress: "bc1qdeposit",
		State:          IntentsStateDepositSent,
		OriginAssetID:  0,
		DestAssetID:    501,
		OriginAsset:    "nep141:btc.omft.near",
		DestAsset:      "nep141:sol.omft.near",
		AtomsIn:        10_000_000,
		AmountIn:       "10000000",
		AmountOut:      "83000000000",
		AtomsOut:       83_000_000_000,
		Deadline:       time.Now().Round(time.Second),
		AppFees:        []intents.AppFee{{Recipient: "x", Fee: 10}},
		QuoteEnvelope:  json.RawMessage(`{"signature":"sig"}`),
		CreatedAt:      time.Now().Round(time.Second),
	}
	b, err := swap.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	back := new(IntentsSwap)
	if err := back.UnmarshalBinary(b); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if back.DepositAddress != swap.DepositAddress || back.AtomsIn != swap.AtomsIn ||
		back.State != swap.State || string(back.QuoteEnvelope) != string(swap.QuoteEnvelope) {
		t.Fatalf("round trip mismatch: %+v vs %+v", back, swap)
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

	swap, err := swapFromStatus(0, "bc1qdepositaddr", "sometxid", status)
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

func TestIntentsDeadlines(t *testing.T) {
	if d := intentsDeadline(0); d != 2*time.Hour {
		t.Errorf("btc deadline %v", d)
	}
	if d := intentsDeadline(501); d != 45*time.Minute {
		t.Errorf("sol default deadline %v", d)
	}
}
