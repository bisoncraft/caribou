// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

func TestAddressReceivers(t *testing.T) {
	e := newEnv(t) // a closed wallet: the answer needs no database
	height, secs := uint32(5_000), uint64(1_900_000_000)
	entries := map[string]*zeclib.ParsedAddress{
		"u-deposit": {Kind: zeclib.AddressUnified, Payable: true, PaidPool: zeclib.PaidIronwood,
			Receivers: []zeclib.Receiver{
				{Kind: zeclib.ReceiverOrchard, Address: "u-deposit-orchard"},
				{Kind: zeclib.ReceiverTransparent, Address: "t-deposit"},
			}},
		"u-orchard": {Kind: zeclib.AddressUnified, Payable: true, PaidPool: zeclib.PaidIronwood,
			Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: "u-orchard"}}},
		"t-plain": {Kind: zeclib.AddressTransparent, Payable: true, PaidPool: zeclib.PaidTransparent},
		"u-foreign": {Kind: zeclib.AddressUnified, Payable: true, PaidPool: zeclib.PaidIronwood,
			Receivers: []zeclib.Receiver{
				{Kind: zeclib.ReceiverOrchard, Address: "u-foreign-orchard"},
				{Kind: zeclib.ReceiverSapling},
				{Kind: zeclib.ReceiverTransparent, Address: "t-foreign"},
			}},
		// Sapling and transparent: paid at the transparent receiver.
		"u-sapling-t": {Kind: zeclib.AddressUnified, Payable: true, PaidPool: zeclib.PaidTransparent,
			Receivers: []zeclib.Receiver{
				{Kind: zeclib.ReceiverSapling},
				{Kind: zeclib.ReceiverTransparent, Address: "t-of-sapling-t"},
			}},
		"u-expiring": {Kind: zeclib.AddressUnified, Payable: true, PaidPool: zeclib.PaidIronwood,
			ExpiryHeight: &height, ExpiryTime: &secs,
			Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: "u-expiring-orchard"}}},
		"u-sapling-only": {Kind: zeclib.AddressUnified, Unpayable: "a unified address without an Orchard or a transparent receiver",
			Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverSapling}}},
		"z-sapling": {Kind: zeclib.AddressSapling, Unpayable: "a Sapling address"},
		"tex-plain": {Kind: zeclib.AddressTex, Unpayable: "a TEX address"},
	}
	for a, p := range entries {
		parsed[a] = p
	}
	t.Cleanup(func() {
		for a := range entries {
			delete(parsed, a)
		}
	})

	type rcv struct {
		kind      asset.ReceiverKind
		pool      string
		address   string
		preferred bool
	}
	shielded := func(a string, pref bool) rcv { return rcv{asset.ReceiverShielded, "Ironwood", a, pref} }
	transparent := func(a string, pref bool) rcv { return rcv{asset.ReceiverTransparent, "Transparent", a, pref} }
	for _, tc := range []struct {
		address string
		want    []rcv
	}{
		{"u-deposit", []rcv{shielded("u-deposit-orchard", true), transparent("t-deposit", false)}},
		{"u-orchard", []rcv{shielded("u-orchard", true)}},
		{"t-plain", []rcv{transparent("t-plain", true)}},
		{"u-foreign", []rcv{shielded("u-foreign-orchard", true), transparent("t-foreign", false)}},
		{"u-sapling-t", []rcv{transparent("t-of-sapling-t", true)}},
		{"u-expiring", []rcv{shielded("u-expiring-orchard", true)}},
	} {
		ar, err := e.w.AddressReceivers(tc.address)
		if err != nil {
			t.Errorf("%s: %v", tc.address, err)
			continue
		}
		if len(ar.Receivers) != len(tc.want) {
			t.Errorf("%s: %d receivers, want %d", tc.address, len(ar.Receivers), len(tc.want))
			continue
		}
		for i, r := range ar.Receivers {
			if got := (rcv{r.Kind, r.Pool, r.Address, r.Preferred}); got != tc.want[i] {
				t.Errorf("%s receiver %d: %+v, want %+v", tc.address, i, got, tc.want[i])
			}
		}
		expiring := tc.address == "u-expiring"
		if (ar.ExpiryHeight != nil) != expiring || (ar.ExpiryTime != nil) != expiring {
			t.Errorf("%s: expiry %v %v", tc.address, ar.ExpiryHeight, ar.ExpiryTime)
		}
		if expiring && (*ar.ExpiryHeight != 5_000 || *ar.ExpiryTime != 1_900_000_000) {
			t.Errorf("expiry %d %d", *ar.ExpiryHeight, *ar.ExpiryTime)
		}
	}
	for _, a := range []string{"u-sapling-only", "z-sapling", "tex-plain"} {
		if ar, err := e.w.AddressReceivers(a); !errors.Is(err, errNotPayable) || ar != nil {
			t.Errorf("%s: %+v, %v", a, ar, err)
		}
	}
	if _, err := e.w.AddressReceivers("garbage"); !errors.Is(err, zeclib.ErrInvalidArgument) {
		t.Errorf("garbage: %v", err)
	}
	if e.w.IsOpen() {
		t.Error("the wallet was opened")
	}
	if !asset.DetermineWalletTraits(e.w).IsReceiverLister() {
		t.Error("the wallet does not have the trait")
	}
}
