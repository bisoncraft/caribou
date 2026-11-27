// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build zec && cgo

package zec

import (
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

// The unified address of account 0 of the alpha funder of the simnet
// harness, with Orchard, Sapling and transparent receivers, and its
// transparent receiver.
const (
	alphaUnified     = "uregtest13pvma5dl78lka0cl83u4jmm78emspd6rm9e56xgm3ttk4pxcwcr4guhdc5u8mg7z3fyr9zjec593khe5m4c3a825yx22lcjwm4anp2xvu4ej5frpc5dm25294c8w3x0pnnxjde38tz4fpgaglrrhx0k7z5259slahnxng23mhn8lhhff8dkeznyhynrfwlu3e8mzedknqathcdlv7hd"
	alphaTransparent = "tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm"
	alphaSapling     = "zregtestsapling1uwt3xqagtnemsucergl4f5lcjj0qsh4hu2avqs6gp3fq62yke38dcyxg5ddewwx5pnjrkfmrr6w"

	// alphaExpiring is alpha's Orchard and transparent receivers with an
	// expiry height of 500 and an expiry time of 9,000, and alphaTOnly its
	// transparent receiver alone with the same expiry (a revision 2 unified
	// address may have no shielded receiver). Both were made with zcash_keys
	// 0.17.0's UnifiedAddress::from_receivers, as zeclib's Rust test
	// the_orchard_receiver_keeps_the_expiry makes the first.
	alphaExpiring = "turegtest1xqun4m3yq9w604an43k36h6x6fh5kz3xmvxn8z89gh8pyfmqlj28v7fu2ex39fx9rvzaaamhl8lp5srlz5a9cfh82zp0hvfv5y7us5hfphxvaweu8avseg9ttwxmcxqt35s5uq83x2xqzapcapv3k6r530naalqy3m648"
	alphaTOnly    = "turegtest18v58pzh5zu82zy8hyp7ac30qrjhd2dl4yqyejd0zcfkmz7letvhp7gupx9tttc3anrru3cylqzp6udgm4uu0puc3jz2hp"
)

// On real addresses each receiver parses back to an address of one receiver
// of its kind. The Ironwood receiver is zeclib's Orchard-only form, which
// zeclib's history uses for the recipient of a shielded receive (the Rust
// test a_received_shielded_output_is_reported_at_its_orchard_receiver ties
// the two byte for byte).
func TestAddressReceiversOnRealAddresses(t *testing.T) {
	w, err := newWallet(&asset.WalletConfig{Type: walletTypeNative, DataDir: t.TempDir()}, nil, util.Simnet)
	if err != nil {
		t.Fatal(err)
	}
	ar, err := w.AddressReceivers(alphaUnified)
	if err != nil {
		t.Fatal(err)
	}
	if len(ar.Receivers) != 2 {
		t.Fatalf("%d receivers: %+v", len(ar.Receivers), ar.Receivers)
	}
	orchard, transparent := ar.Receivers[0], ar.Receivers[1]
	if orchard.Kind != asset.ReceiverShielded || !orchard.Preferred || transparent.Kind != asset.ReceiverTransparent ||
		transparent.Preferred || transparent.Address != alphaTransparent {
		t.Errorf("receivers: %+v %+v", orchard, transparent)
	}
	p, err := zeclib.ParseAddress(harnessNetworkFFI, orchard.Address)
	if err != nil || p.Kind != zeclib.AddressUnified || len(p.Receivers) != 1 || p.Receivers[0].Kind != zeclib.ReceiverOrchard {
		t.Errorf("the Ironwood receiver %s: %+v, %v", orchard.Address, p, err)
	}
	full, err := zeclib.ParseAddress(harnessNetworkFFI, alphaUnified)
	if err != nil || full.Receivers[0].Address != orchard.Address {
		t.Errorf("not zeclib's Orchard form: %v", err)
	}
	ar, err = w.AddressReceivers(alphaTransparent)
	if err != nil || len(ar.Receivers) != 1 || ar.Receivers[0].Address != alphaTransparent || !ar.Receivers[0].Preferred {
		t.Errorf("a transparent address: %+v, %v", ar, err)
	}

	// An expiring address carries its expiry, and each entry is the unified
	// address reduced to its receiver and the expiry.
	ar, err = w.AddressReceivers(alphaExpiring)
	if err != nil || len(ar.Receivers) != 2 {
		t.Fatalf("an expiring address: %+v, %v", ar, err)
	}
	if ar.ExpiryHeight == nil || *ar.ExpiryHeight != 500 || ar.ExpiryTime == nil || *ar.ExpiryTime != 9_000 {
		t.Errorf("the expiry: %v %v", ar.ExpiryHeight, ar.ExpiryTime)
	}
	// Each entry parses back to a unified address of one receiver of its
	// kind that carries both expiry fields, and the Ironwood entry is the
	// one this wallet pays.
	for i, want := range []struct {
		kind      asset.ReceiverKind
		receiver  zeclib.ReceiverKind
		preferred bool
	}{
		{asset.ReceiverShielded, zeclib.ReceiverOrchard, true},
		{asset.ReceiverTransparent, zeclib.ReceiverTransparent, false},
	} {
		entry := ar.Receivers[i]
		p, err := zeclib.ParseAddress(harnessNetworkFFI, entry.Address)
		if err != nil || entry.Kind != want.kind || entry.Preferred != want.preferred ||
			p.Kind != zeclib.AddressUnified || len(p.Receivers) != 1 || p.Receivers[0].Kind != want.receiver ||
			p.ExpiryHeight == nil || *p.ExpiryHeight != 500 || p.ExpiryTime == nil || *p.ExpiryTime != 9_000 {
			t.Errorf("entry %d of an expiring address, %+v: %+v, %v", i, entry, p, err)
		}
	}
	// A unified address with a transparent receiver alone is paid there.
	ar, err = w.AddressReceivers(alphaTOnly)
	if err != nil || len(ar.Receivers) != 1 || ar.Receivers[0].Kind != asset.ReceiverTransparent ||
		!ar.Receivers[0].Preferred {
		t.Errorf("a transparent-only unified address: %+v, %v", ar, err)
	}

	if _, err := w.AddressReceivers(alphaSapling); err == nil {
		t.Error("a Sapling address")
	}
	if _, err := w.AddressReceivers("tmGarbage"); err == nil {
		t.Error("garbage")
	}
}

var harnessNetworkFFI = zeclib.Regtest(zeclib.AllUpgradesAt(1))
