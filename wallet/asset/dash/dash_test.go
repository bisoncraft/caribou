// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dash

import (
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/btc"
)

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
	exists, err := d.Exists(walletTypeElectrum, t.TempDir(), nil, util.Regtest)
	if err != nil {
		t.Fatalf("Exists on empty data dir errored: %v", err)
	}
	if exists {
		t.Fatal("Exists reported true for an empty data dir")
	}
	// walletTypeSPV shares the same on-disk database.
	exists, err = d.Exists(walletTypeSPV, t.TempDir(), nil, util.Regtest)
	if err != nil {
		t.Fatalf("Exists on empty data dir errored: %v", err)
	}
	if exists {
		t.Fatal("Exists reported true for an empty data dir")
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
	if w == nil {
		t.Fatal("newWallet(electrum) returned a nil wallet")
	}

	spvCfg := *cfg
	spvCfg.Type = walletTypeSPV
	w, err = newWallet(&spvCfg, util.Disabled, util.Regtest)
	if err != nil {
		t.Fatalf("newWallet(SPV) errored: %v", err)
	}
	if w == nil {
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
	if w == nil {
		t.Fatal("Open returned a nil wallet")
	}

	if _, err := d.Open(cfg, util.Disabled, util.Network(99)); err == nil {
		t.Fatal("Open accepted unknown network")
	}
}
