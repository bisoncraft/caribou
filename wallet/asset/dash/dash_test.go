// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dash

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
)

// testSeed is a valid-length (256-bit) seed for tests that actually create a
// wallet; CreateWalletParams.Seed must be 128-512 bits.
var testSeed = []byte("01234567890123456789012345678901")

func TestParseChainParams(t *testing.T) {
	tests := []struct {
		net     util.Network
		want    string
		wantErr bool
	}{
		{util.Mainnet, MainNetParams.Name, false},
		{util.Testnet, TestNetParams.Name, false},
		{util.Regtest, RegressionNetParams.Name, false},
		{util.Network(99), "", true},
	}
	for _, tt := range tests {
		params, err := parseChainParams(tt.net)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseChainParams(%v) expected error, got none", tt.net)
			}
			if params != nil {
				t.Errorf("parseChainParams(%v) = %v, want nil params on error", tt.net, params)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseChainParams(%v) unexpected error: %v", tt.net, err)
			continue
		}
		if params.Name != tt.want {
			t.Errorf("parseChainParams(%v) = %s, want %s", tt.net, params.Name, tt.want)
		}
	}
}

func TestDriverDecodeCoinID(t *testing.T) {
	d := &Driver{}
	coinID := make([]byte, 36)
	got, err := d.DecodeCoinID(coinID)
	want, wantErr := (&btc.Driver{}).DecodeCoinID(coinID)
	if (err != nil) != (wantErr != nil) {
		t.Fatalf("DecodeCoinID error = %v, want %v", err, wantErr)
	}
	if got != want {
		t.Fatalf("DecodeCoinID = %s, want %s", got, want)
	}

	if _, err := d.DecodeCoinID(nil); err == nil {
		t.Fatal("DecodeCoinID accepted malformed coin ID")
	}
}

func TestDriverMinLotSize(t *testing.T) {
	d := &Driver{}
	const maxFeeRate = 100
	if got, want := d.MinLotSize(maxFeeRate), btc.MinLotSize(maxFeeRate, false); got != want {
		t.Fatalf("MinLotSize(%d) = %d, want %d", maxFeeRate, got, want)
	}
}

func TestDriverExists(t *testing.T) {
	d := &Driver{}

	if _, err := d.Exists("unknown", t.TempDir(), nil, util.Mainnet); err == nil {
		t.Fatal("Exists accepted unsupported wallet type")
	}
	if _, err := d.Exists(walletTypeElectrum, t.TempDir(), nil, util.Network(99)); err == nil {
		t.Fatal("Exists accepted unknown network")
	}
	dataDir := t.TempDir()
	exists, err := d.Exists(walletTypeElectrum, dataDir, nil, util.Regtest)
	if err != nil {
		t.Fatalf("Exists on empty data dir errored: %v", err)
	}
	if exists {
		t.Fatal("Exists reported true for an empty data dir")
	}
	// walletTypeSPV shares the same on-disk database, so it must agree with
	// walletTypeElectrum on the same, still-empty data dir.
	exists, err = d.Exists(walletTypeSPV, dataDir, nil, util.Regtest)
	if err != nil {
		t.Fatalf("Exists on empty data dir errored: %v", err)
	}
	if exists {
		t.Fatal("Exists reported true for an empty data dir")
	}

	baseParams := &asset.CreateWalletParams{
		Type:    walletTypeElectrum,
		Seed:    testSeed,
		Pass:    []byte("pass"),
		DataDir: dataDir,
		Net:     util.Regtest,
		Logger:  util.Disabled,
	}
	if err := d.Create(baseParams); err != nil {
		t.Fatalf("Create errored: %v", err)
	}
	// Pin the directory layout itself, not just that Create and Exists agree
	// with each other on it.
	walletDir := filepath.Join(dataDir, RegressionNetParams.Name)
	if _, err := os.Stat(walletDir); err != nil {
		t.Fatalf("wallet directory %s not created: %v", walletDir, err)
	}
	exists, err = d.Exists(walletTypeElectrum, dataDir, nil, util.Regtest)
	if err != nil {
		t.Fatalf("Exists after create errored: %v", err)
	}
	if !exists {
		t.Fatal("Exists reported false after Create")
	}
	// walletTypeSPV shares the same on-disk database, so it must now also
	// report the wallet created as walletTypeElectrum as existing.
	exists, err = d.Exists(walletTypeSPV, dataDir, nil, util.Regtest)
	if err != nil {
		t.Fatalf("Exists(SPV) after electrum create errored: %v", err)
	}
	if !exists {
		t.Fatal("Exists(SPV) reported false after electrum Create, want the shared database to be found")
	}
}

func TestDriverCreate(t *testing.T) {
	d := &Driver{}
	baseParams := &asset.CreateWalletParams{
		Type:    walletTypeElectrum,
		Seed:    []byte("seed"),
		DataDir: t.TempDir(),
		Net:     util.Regtest,
		Logger:  util.Disabled,
	}

	bad := *baseParams
	bad.Type = "unknown"
	if err := d.Create(&bad); err == nil {
		t.Fatal("Create accepted unsupported wallet type")
	}

	bad = *baseParams
	bad.Seed = nil
	if err := d.Create(&bad); err == nil {
		t.Fatal("Create accepted empty seed")
	}

	bad = *baseParams
	bad.DataDir = ""
	if err := d.Create(&bad); err == nil {
		t.Fatal("Create accepted empty data directory")
	}

	bad = *baseParams
	bad.Net = util.Network(99)
	if err := d.Create(&bad); err == nil {
		t.Fatal("Create accepted unknown network")
	}
}

func TestNewWallet(t *testing.T) {
	cfg := &asset.WalletConfig{
		Type:    walletTypeElectrum,
		DataDir: t.TempDir(),
	}

	if _, err := newWallet(cfg, util.Disabled, util.Network(99)); err == nil {
		t.Fatal("newWallet accepted unknown network")
	}

	bad := *cfg
	bad.Type = "unknown"
	if _, err := newWallet(&bad, util.Disabled, util.Regtest); err == nil {
		t.Fatal("newWallet accepted unknown wallet type")
	}

	w, err := newWallet(cfg, util.Disabled, util.Regtest)
	if err != nil {
		t.Fatalf("newWallet(electrum) errored: %v", err)
	}
	if nw, ok := w.(*btc.NativeWallet); !ok || nw == nil {
		t.Fatal("newWallet(electrum) returned a nil wallet")
	}

	spvCfg := *cfg
	spvCfg.Type = walletTypeSPV
	w, err = newWallet(&spvCfg, util.Disabled, util.Regtest)
	if err != nil {
		t.Fatalf("newWallet(SPV) errored: %v", err)
	}
	if nw, ok := w.(*btc.NativeWallet); !ok || nw == nil {
		t.Fatal("newWallet(SPV) returned a nil wallet")
	}
}

func TestDriverOpen(t *testing.T) {
	d := &Driver{}
	cfg := &asset.WalletConfig{
		Type:    walletTypeElectrum,
		DataDir: t.TempDir(),
	}
	w, err := d.Open(cfg, util.Disabled, util.Regtest)
	if err != nil {
		t.Fatalf("Open errored: %v", err)
	}
	if nw, ok := w.(*btc.NativeWallet); !ok || nw == nil {
		t.Fatal("Open returned a nil wallet")
	}

	if _, err := d.Open(cfg, util.Disabled, util.Network(99)); err == nil {
		t.Fatal("Open accepted unknown network")
	}
}
