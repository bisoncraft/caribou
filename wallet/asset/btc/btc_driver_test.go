//go:build !harness

package btc

import (
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
)

func TestDriverCoinIDAndNetwork(t *testing.T) {
	d := new(Driver)
	coinID := ToCoinID(tTxHash, 7)
	got, err := d.DecodeCoinID(coinID)
	if err != nil || got != tTxID+":7" {
		t.Fatalf("DecodeCoinID = %q, %v", got, err)
	}
	if _, err := d.DecodeCoinID(nil); err == nil {
		t.Fatal("expected malformed coin ID error")
	}
	for _, net := range []util.Network{util.Mainnet, util.Testnet, util.Regtest} {
		if _, err := parseChainParams(net); err != nil {
			t.Fatalf("parseChainParams(%v): %v", net, err)
		}
	}
	if _, err := parseChainParams(util.Network(99)); err == nil {
		t.Fatal("expected unknown network error")
	}
}

func TestDriverValidation(t *testing.T) {
	d := new(Driver)
	if _, err := d.Exists("unknown", t.TempDir(), nil, util.Mainnet); err == nil {
		t.Fatal("expected unknown wallet type error")
	}
	if err := d.Create(&asset.CreateWalletParams{Type: "unknown"}); err == nil {
		t.Fatal("expected unsupported wallet type error")
	}
	if err := d.Create(&asset.CreateWalletParams{Type: walletTypeElectrum}); err == nil {
		t.Fatal("expected missing seed error")
	}
	if err := d.Create(&asset.CreateWalletParams{Type: walletTypeElectrum, Seed: []byte("seed")}); err == nil {
		t.Fatal("expected missing data dir error")
	}
	if _, err := NewWallet(&asset.WalletConfig{Type: "unknown"}, tLogger, util.Mainnet); err == nil {
		t.Fatal("expected unknown wallet type error")
	}
	if _, err := NewWallet(&asset.WalletConfig{Type: walletTypeElectrum}, tLogger, util.Network(99)); err == nil {
		t.Fatal("expected unknown network error")
	}
	if d.Info() != WalletInfo || d.MinLotSize(1) == 0 {
		t.Fatal("unexpected driver metadata")
	}
	if p := SystemConfigPath("bitcoin"); !strings.HasSuffix(p, "bitcoin.conf") {
		t.Fatalf("SystemConfigPath = %q", p)
	}
}

func TestDriverCreateAndExists(t *testing.T) {
	d := new(Driver)
	seed := make([]byte, hdkeychain.RecommendedSeedLen)
	for i := range seed {
		seed[i] = byte(i)
	}
	params := &asset.CreateWalletParams{
		Type: walletTypeElectrum, Seed: seed, Pass: []byte("pass"), DataDir: t.TempDir(),
		Net: util.Regtest, Birthday: uint64(time.Now().Unix()), Logger: tLogger,
	}
	if err := d.Create(params); err != nil {
		t.Fatal(err)
	}
	exists, err := d.Exists(walletTypeElectrum, params.DataDir, nil, util.Regtest)
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
}

func TestDriverOpen(t *testing.T) {
	d := new(Driver)
	for _, walletType := range []string{walletTypeElectrum, walletTypeSPV} {
		w, err := d.Open(&asset.WalletConfig{Type: walletType, DataDir: t.TempDir()}, tLogger, util.Mainnet)
		if err != nil {
			t.Fatalf("Open(%s): %v", walletType, err)
		}
		if _, ok := w.(*NativeWallet); !ok {
			t.Fatalf("Open(%s) returned %T", walletType, w)
		}
	}
}
