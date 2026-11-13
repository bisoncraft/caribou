// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
)

func TestNetworks(t *testing.T) {
	regtest := Regtest(ActivationHeights{Overwinter: 3, Sapling: 7, NU6_3: 11})
	tests := []struct {
		network  Network
		name     string
		sapling  uint32
		ironwood uint32
		chains   map[string]bool
	}{
		{Mainnet, "mainnet", 419_200, 3_428_143, map[string]bool{"main": true, "test": false, "regtest": false, "": false}},
		{Testnet, "testnet", 280_000, 4_134_000, map[string]bool{"main": false, "test": true, "regtest": false}},
		{regtest, "regtest", 7, 11, map[string]bool{"main": false, "test": true, "regtest": true}},
	}
	for _, tt := range tests {
		if got := tt.network.String(); got != tt.name {
			t.Errorf("String: %q, want %q", got, tt.name)
		}
		if got := tt.network.saplingActivation(); got != tt.sapling {
			t.Errorf("%s: Sapling activation %d, want %d", tt.name, got, tt.sapling)
		}
		if got := tt.network.ironwoodActivation(); got != tt.ironwood {
			t.Errorf("%s: NU6.3 activation %d, want %d", tt.name, got, tt.ironwood)
		}
		for chain, want := range tt.chains {
			if got := tt.network.matchesChainName(chain); got != want {
				t.Errorf("%s: matchesChainName(%q) = %t", tt.name, chain, got)
			}
		}
	}
	if got := NetworkType(9).String(); got != "NetworkType(9)" {
		t.Errorf("unknown network type: %q", got)
	}

	all := AllUpgradesAt(4)
	for name, h := range map[string]uint32{
		"overwinter": all.Overwinter, "sapling": all.Sapling, "blossom": all.Blossom,
		"heartwood": all.Heartwood, "canopy": all.Canopy, "nu5": all.NU5, "nu6": all.NU6,
		"nu6.1": all.NU6_1, "nu6.2": all.NU6_2, "nu6.3": all.NU6_3, "nu7": all.NU7,
	} {
		if h != 4 {
			t.Errorf("AllUpgradesAt(4): %s at %d", name, h)
		}
	}
}

func TestRatioFraction(t *testing.T) {
	tests := []struct {
		r    Ratio
		want float64
	}{
		{Ratio{0, 0}, 1},
		{Ratio{0, 4}, 0},
		{Ratio{1, 4}, 0.25},
		{Ratio{4, 4}, 1},
		{Ratio{9, 4}, 1},
	}
	for _, tt := range tests {
		if got := tt.r.Fraction(); got != tt.want {
			t.Errorf("%+v: %v, want %v", tt.r, got, tt.want)
		}
	}
}

func TestScanRangeStrings(t *testing.T) {
	want := map[scanPriority]string{
		priorityIgnored: "ignored", priorityScanned: "scanned", priorityHistoric: "historic",
		priorityOpenAdjacent: "open-adjacent", priorityFoundNote: "found-note",
		priorityChainTip: "chain-tip", priorityVerify: "verify", scanPriority(50): "scanPriority(50)",
	}
	for p, s := range want {
		if got := p.String(); got != s {
			t.Errorf("%d: %q, want %q", uint8(p), got, s)
		}
	}
	if got := (scanRange{start: 5, end: 9, priority: priorityVerify}).String(); got != "[5, 9) verify" {
		t.Errorf("scanRange: %q", got)
	}
}

func TestTxRequestStrings(t *testing.T) {
	// A transaction ID is printed in display order, the reverse of its
	// bytes.
	id := []byte{0x01, 0x02, 0xff}
	tests := []struct {
		req  txRequest
		want string
	}{
		{txRequest{kind: txRequestStatus, txid: id}, "the status of transaction ff0201"},
		{txRequest{kind: txRequestEnhancement, txid: id}, "the data of transaction ff0201"},
		{txRequest{kind: txRequestAddress, address: "t1abc", start: 100, end: 141, bounded: true},
			"the transactions of t1abc in blocks 100 to 140"},
		{txRequest{kind: txRequestAddress, address: "t1abc", start: 100, bounded: true},
			"the transactions of t1abc in blocks 100 to 0"},
		{txRequest{kind: txRequestAddress, address: "t1abc", start: 100},
			"the transactions of t1abc from block 100 on"},
		{txRequest{kind: 99}, "transaction data of unknown kind 99"},
	}
	for _, tt := range tests {
		if got := tt.req.String(); got != tt.want {
			t.Errorf("%q, want %q", got, tt.want)
		}
	}
	if id[0] != 0x01 {
		t.Error("String changed the transaction ID")
	}
}

func TestTxRequestKeys(t *testing.T) {
	requests := []txRequest{
		{kind: txRequestStatus, txid: []byte{1}},
		{kind: txRequestEnhancement, txid: []byte{1}},
		{kind: txRequestStatus, txid: []byte{2}},
		{kind: txRequestAddress, address: "a", start: 1},
		{kind: txRequestAddress, address: "a", start: 2},
		{kind: txRequestAddress, address: "b", start: 1},
		{kind: txRequestAddress, address: "a", start: 1, end: 5, bounded: true},
		{kind: txRequestAddress, address: "a", start: 1, end: 6, bounded: true},
	}
	seen := make(map[string]int)
	for i := range requests {
		key := requests[i].key()
		if j, dup := seen[key]; dup {
			t.Errorf("requests %d and %d share the key %q", j, i, key)
		}
		seen[key] = i
	}
	// The time a request is due does not make it a different request.
	a := txRequest{kind: txRequestAddress, address: "a", start: 1}
	b := a
	b.requestAt = time.Now()
	if a.key() != b.key() {
		t.Error("the request time changes the key")
	}
}

func TestErrorTexts(t *testing.T) {
	tests := []struct {
		err  error
		want []string
	}{
		{&chainContinuityError{height: 12, reason: "hash mismatch"}, []string{"12", "hash mismatch"}},
		{&rewindError{requested: 50, safe: 90, hasSafe: true}, []string{"50", "90"}},
		{&rewindError{requested: 50}, []string{"50"}},
		{&scanError{height: 7, reason: "bad tree"}, []string{"7", "bad tree"}},
		{&serverError{op: "GetBlock(3)", err: errors.New("down")}, []string{"GetBlock(3)", "down"}},
		{wrapf(ErrDatabase, "disk %s", "full"), []string{"disk full", ErrDatabase.Error()}},
	}
	for _, tt := range tests {
		for _, want := range tt.want {
			if !strings.Contains(tt.err.Error(), want) {
				t.Errorf("%q does not mention %q", tt.err, want)
			}
		}
	}
	if !errors.Is(wrapf(ErrDatabase, "x"), ErrDatabase) {
		t.Error("wrapf does not wrap its base")
	}
	if strings.Contains((&rewindError{requested: 50}).Error(), "or above") {
		t.Error("a rewind error without a safe height names one")
	}
}

func TestWalletWithoutALogger(t *testing.T) {
	// A wallet without a logger logs to nothing.
	w := newWallet(&Config{Network: testNetwork}, newFakeBackend(), nil)
	if w.log != util.Disabled {
		t.Fatalf("the logger is %v, want the disabled logger", w.log)
	}
	w.log.Warnf("%d", 1)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSummaryBalance(t *testing.T) {
	s := &Summary{Balances: []AccountBalance{{AccountID: "a", Total: 1}, {AccountID: "b", Total: 2}}}
	if b, ok := s.Balance("b"); !ok || b.Total != 2 {
		t.Errorf("Balance(b): %+v, %t", b, ok)
	}
	if _, ok := s.Balance("c"); ok {
		t.Error("Balance(c) found")
	}
}
