// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package ltc

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

func TestDriverInfo(t *testing.T) {
	drv := &Driver{}
	info := drv.Info()
	if info.Name != "Litecoin" {
		t.Fatalf("Info.Name = %q, want %q", info.Name, "Litecoin")
	}
	if info.BlockchainClass != asset.BlockchainClassUTXO {
		t.Fatalf("Info.BlockchainClass = %q, want %q", info.BlockchainClass, asset.BlockchainClassUTXO)
	}
	if info.Network == nil || info.Network.ID != BipID {
		t.Fatalf("Info.Network.ID = %v, want %d", info.Network, BipID)
	}
	// SPV and RPC must both be advertised as available wallet types so that
	// users can either run litecoind or use the built-in wallet.
	wantTypes := map[string]bool{walletTypeSPV: false, walletTypeRPC: false}
	for _, wd := range info.AvailableWallets {
		if _, ok := wantTypes[wd.Type]; ok {
			wantTypes[wd.Type] = true
		}
	}
	for typ, found := range wantTypes {
		if !found {
			t.Fatalf("AvailableWallets missing entry for %q", typ)
		}
	}
}

func TestDriverDecodeCoinID(t *testing.T) {
	drv := &Driver{}

	// Build a valid 36-byte coin ID: 32-byte txid + 4-byte big-endian vout.
	const vout uint32 = 7
	txHash := chainhash.Hash{}
	copy(txHash[:], encode.RandomBytes(chainhash.HashSize))
	coinID := make([]byte, 36)
	copy(coinID[:32], txHash[:])
	binary.BigEndian.PutUint32(coinID[32:], vout)

	got, err := drv.DecodeCoinID(coinID)
	if err != nil {
		t.Fatalf("DecodeCoinID error: %v", err)
	}
	if !strings.HasSuffix(got, ":7") {
		t.Fatalf("DecodeCoinID = %q, want suffix %q", got, ":7")
	}
	if !strings.HasPrefix(got, txHash.String()) {
		t.Fatalf("DecodeCoinID = %q, want prefix %q", got, txHash.String())
	}

	// Wrong length must error rather than silently truncate.
	if _, err := drv.DecodeCoinID(encode.RandomBytes(20)); err == nil {
		t.Fatal("expected error for short coin ID")
	}
}

func TestDriverMinLotSize(t *testing.T) {
	drv := &Driver{}
	// LTC enables segwit, so MinLotSize must equal MinLotSize(rate, true)
	// from the shared BTC package; using zero rate would be degenerate, so
	// just confirm a typical fee rate returns a non-zero result and that
	// the result is monotonic in the fee rate.
	small := drv.MinLotSize(1)
	large := drv.MinLotSize(100)
	if small == 0 {
		t.Fatal("MinLotSize(1) returned 0")
	}
	if large <= small {
		t.Fatalf("MinLotSize not monotonic: MinLotSize(1)=%d MinLotSize(100)=%d", small, large)
	}
}

func TestDriverExists(t *testing.T) {
	drv := &Driver{}
	tmp := t.TempDir()

	if _, err := drv.Exists("not-spv", tmp, nil, util.Regtest); err == nil {
		t.Fatal("expected error for unknown wallet type")
	}
	if _, err := drv.Exists(walletTypeSPV, tmp, nil, util.Network(99)); err == nil {
		t.Fatal("expected error for unknown network")
	}
	exists, err := drv.Exists(walletTypeSPV, tmp, nil, util.Regtest)
	if err != nil {
		t.Fatalf("Exists(SPV, tempdir) error: %v", err)
	}
	if exists {
		t.Fatal("a fresh tempdir should not report an existing wallet")
	}
}

func TestDriverCreateValidation(t *testing.T) {
	drv := &Driver{}
	tmp := t.TempDir()
	seed := encode.RandomBytes(32)

	cases := []struct {
		name   string
		params *asset.CreateWalletParams
	}{
		{
			"wrong wallet type",
			&asset.CreateWalletParams{Type: walletTypeRPC, Seed: seed, DataDir: tmp, Net: util.Regtest},
		},
		{
			"empty seed",
			&asset.CreateWalletParams{Type: walletTypeSPV, Seed: nil, DataDir: tmp, Net: util.Regtest},
		},
		{
			"empty data dir",
			&asset.CreateWalletParams{Type: walletTypeSPV, Seed: seed, DataDir: "", Net: util.Regtest},
		},
		{
			"unknown network",
			&asset.CreateWalletParams{Type: walletTypeSPV, Seed: seed, DataDir: tmp, Net: util.Network(99)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := drv.Create(tc.params); err == nil {
				t.Fatalf("Create(%s) returned nil error; want validation failure", tc.name)
			}
		})
	}
}

func TestNewWalletDispatch(t *testing.T) {
	cfg := &asset.WalletConfig{Type: walletTypeRPC, Settings: map[string]string{}}

	if _, err := NewWallet(cfg, nil, util.Network(99)); err == nil {
		t.Fatal("expected error for unknown network")
	}

	// A network that resolves to chain params but a walletType that isn't
	// RPC/SPV/Electrum and isn't a registered custom constructor must fall
	// through to the dispatch error rather than silently doing nothing.
	bogus := &asset.WalletConfig{Type: "totally-bogus", Settings: map[string]string{}}
	if _, err := NewWallet(bogus, nil, util.Regtest); err == nil {
		t.Fatal("expected error for unknown wallet type")
	}
}

func TestParseChainParams(t *testing.T) {
	cases := []struct {
		net      util.Network
		wantName string
	}{
		{util.Mainnet, "mainnet"},
		{util.Testnet, "testnet4"},
		{util.Regtest, "regtest"},
	}
	for _, tc := range cases {
		p, err := parseChainParams(tc.net)
		if err != nil {
			t.Fatalf("parseChainParams(%s) error: %v", tc.net, err)
		}
		if p == nil {
			t.Fatalf("parseChainParams(%s) returned nil", tc.net)
		}
		if p.Name != tc.wantName {
			t.Fatalf("parseChainParams(%s).Name = %q, want %q", tc.net, p.Name, tc.wantName)
		}
	}
	if _, err := parseChainParams(util.Network(99)); err == nil {
		t.Fatal("expected error for unknown network")
	}
}

// TestLtcAssetRegistered confirms the LTC driver is registered with the asset
// registry at the BIP-44 ID so it can be looked up by callers that don't
// import the package directly.
func TestLtcAssetRegistered(t *testing.T) {
	ra := asset.Asset(BipID)
	if ra == nil {
		t.Fatalf("asset.Asset(%d) returned nil; ltc driver not registered", BipID)
	}
	if ra.Info == nil {
		t.Fatal("registered asset has nil Info")
	}
	if ra.Info.Name != "Litecoin" {
		t.Fatalf("registered Info.Name = %q, want %q", ra.Info.Name, "Litecoin")
	}
}
