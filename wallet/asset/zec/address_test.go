// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

func openEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	if err := e.w.OpenWithPW(context.Background(), testPW); err != nil {
		t.Fatal(err)
	}
	return e
}

// NewAddress returns the current address until it has received a payment,
// then a new one. At the transparent gap limit it keeps the current one and
// does not fail.
func TestNewAddress(t *testing.T) {
	e := openEnv(t)
	be := e.o.latest()
	be.set(func(b *fakeBackend) { b.newAddress = "ufake-new" })
	calls := func() (n int) {
		be.set(func(b *fakeBackend) { n = b.newAddrCalls })
		return n
	}

	// No payment: twelve calls, one address, no derivation.
	for i := range 12 {
		if a, err := e.w.NewAddress(); err != nil || a != testAddress {
			t.Fatalf("call %d: %q, %v", i, a, err)
		}
	}
	if n := calls(); n != 0 {
		t.Fatalf("%d addresses derived without a payment", n)
	}

	// A payment to either receiver of the current address uses it.
	for _, paid := range []struct {
		name string
		out  zeclib.TxOutput
	}{
		{"the Orchard receiver", zeclib.TxOutput{Pool: zeclib.OutputIronwood, Address: testReceiver, ToWallet: true}},
		{"the transparent receiver", zeclib.TxOutput{Pool: zeclib.OutputTransparent, Address: testTAddress, ToWallet: true}},
	} {
		e.w.setAddress(testAddress)
		be.set(func(b *fakeBackend) {
			b.history = []zeclib.TxRecord{{Seq: 1, Outputs: []zeclib.TxOutput{paid.out}}}
			b.newAddrCalls = 0
		})
		if a, err := e.w.NewAddress(); err != nil || a != "ufake-new" {
			t.Fatalf("after a payment to %s: %q, %v", paid.name, a, err)
		}
		if a, _ := e.w.DepositAddress(); a != "ufake-new" {
			t.Errorf("after a payment to %s the deposit address is %q", paid.name, a)
		}
		if n := calls(); n != 1 {
			t.Errorf("after a payment to %s: %d derivations", paid.name, n)
		}
	}

	// At the gap limit, after a use, the current address and no error.
	e.w.setAddress(testAddress)
	gap := fmt.Errorf("%w: 10 unused", zeclib.ErrAddressGapLimit)
	be.set(func(b *fakeBackend) { b.newAddrErr = gap })
	for range 2 {
		if a, err := e.w.NewAddress(); err != nil || a != testAddress {
			t.Fatalf("at the gap limit: %q, %v", a, err)
		}
	}
	if n := e.log.count("is handed out again"); n != 1 {
		t.Errorf("the reuse at the gap limit was logged %d times", n)
	}
	if n := e.log.count(testAddress); n != 0 {
		t.Errorf("the deposit address is in %d log lines", n)
	}

	// Any other failure to derive: the error, and the address unchanged.
	other := fmt.Errorf("%w: injected", zeclib.ErrServerFailure)
	be.set(func(b *fakeBackend) {
		b.newAddrErr = other
		b.newAddrCalls = 0
	})
	if a, err := e.w.NewAddress(); !errors.Is(err, zeclib.ErrServerFailure) {
		t.Fatalf("a failed derivation: %q, %v", a, err)
	}
	if a, _ := e.w.DepositAddress(); a != testAddress {
		t.Errorf("after a failed derivation the deposit address is %q", a)
	}
	if n := e.log.count("is handed out again"); n != 1 {
		t.Errorf("a failed derivation was logged as the gap limit")
	}

	// A history that cannot be read: the error, and nothing derived.
	be.set(func(b *fakeBackend) {
		b.newAddrErr = nil
		b.newAddrCalls = 0
		b.historyErr = errors.New("injected history failure")
	})
	if _, err := e.w.NewAddress(); err == nil {
		t.Fatal("a failed history read did not fail")
	}
	if n := calls(); n != 0 {
		t.Errorf("%d addresses derived after a failed history read", n)
	}
}

func TestOwnsDepositAddress(t *testing.T) {
	e := openEnv(t)
	be := e.o.latest()
	be.set(func(b *fakeBackend) { b.owns["ufake-other"] = "acct-0" })
	for _, tc := range []struct {
		address string
		want    bool
	}{
		{"nonsense", false},
		{testAddress, true},
		{testReceiver, true},
		{testTAddress, true},
		{"ufake-other", true},
		{"ufake-new", false},
	} {
		got, err := e.w.OwnsDepositAddress(tc.address)
		if err != nil || got != tc.want {
			t.Errorf("%s: %t, %v; want %t", tc.address, got, err, tc.want)
		}
	}
	// Another account's address is not this wallet's.
	be.set(func(b *fakeBackend) { b.owns["ufake-new"] = "acct-9" })
	if got, err := e.w.OwnsDepositAddress("ufake-new"); err != nil || got {
		t.Errorf("another account's address: %t, %v", got, err)
	}
}

// A unified address is the wallet's only if every receiver a sender could
// pay is the wallet's.
func TestOwnsDepositAddressByReceiver(t *testing.T) {
	e := openEnv(t)
	parsed["ufake-foreign-orchard+ours-t"] = &zeclib.ParsedAddress{Kind: zeclib.AddressUnified, Payable: true,
		Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: "ufake-foreign-orchard"},
			{Kind: zeclib.ReceiverTransparent, Address: testTAddress}}}
	parsed["ufake-ours-o+foreign-t"] = &zeclib.ParsedAddress{Kind: zeclib.AddressUnified, Payable: true,
		Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: testReceiver},
			{Kind: zeclib.ReceiverTransparent, Address: "tfake-foreign"}}}
	parsed["ufake-ours-with-sapling"] = &zeclib.ParsedAddress{Kind: zeclib.AddressUnified, Payable: true,
		Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: testReceiver},
			{Kind: zeclib.ReceiverSapling}}}
	parsed["ufake-ours-reordered"] = &zeclib.ParsedAddress{Kind: zeclib.AddressUnified, Payable: true,
		Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: testReceiver},
			{Kind: zeclib.ReceiverTransparent, Address: testTAddress}}}
	t.Cleanup(func() {
		for _, a := range []string{"ufake-foreign-orchard+ours-t", "ufake-ours-o+foreign-t", "ufake-ours-with-sapling", "ufake-ours-reordered"} {
			delete(parsed, a)
		}
	})
	// zeclib matches any one receiver; the fake says so for these.
	e.o.latest().set(func(b *fakeBackend) {
		b.owns["ufake-foreign-orchard+ours-t"] = "acct-0"
		b.owns["ufake-ours-o+foreign-t"] = "acct-0"
	})
	for _, tc := range []struct {
		address string
		want    bool
	}{
		{"ufake-foreign-orchard+ours-t", false},
		{"ufake-ours-o+foreign-t", false},
		{"ufake-ours-with-sapling", false},
		{"zfake-sapling", false},
		{"ufake-ours-reordered", true},
	} {
		got, err := e.w.OwnsDepositAddress(tc.address)
		if err != nil || got != tc.want {
			t.Errorf("%s: %t, %v; want %t", tc.address, got, err, tc.want)
		}
	}
}

// The history records a received output under the address the wallet
// generated, the full unified address; the output's pool says which receiver
// was paid.
func TestAddressUsedByPool(t *testing.T) {
	e := openEnv(t)
	be := e.o.latest()
	be.set(func(b *fakeBackend) {
		b.history = []zeclib.TxRecord{{Seq: 1, Outputs: []zeclib.TxOutput{
			{Pool: zeclib.OutputTransparent, Address: testAddress, ToWallet: true, Value: 1},
		}}}
	})
	for _, tc := range []struct {
		address string
		want    bool
	}{
		{testTAddress, true},
		{testAddress, true},
		{testReceiver, false},
	} {
		if got, err := e.w.AddressUsed(tc.address); err != nil || got != tc.want {
			t.Errorf("a transparent output under the full address, %s: %t, %v; want %t", tc.address, got, err, tc.want)
		}
	}
	be.set(func(b *fakeBackend) {
		b.history[0].Outputs[0].Pool = zeclib.OutputIronwood
	})
	for _, tc := range []struct {
		address string
		want    bool
	}{
		{testTAddress, false},
		{testReceiver, true},
	} {
		if got, err := e.w.AddressUsed(tc.address); err != nil || got != tc.want {
			t.Errorf("an Ironwood output under the full address, %s: %t, %v; want %t", tc.address, got, err, tc.want)
		}
	}
}

func TestAddressUsed(t *testing.T) {
	e := openEnv(t)
	be := e.o.latest()
	var history []zeclib.TxRecord
	for i := range 450 {
		history = append(history, zeclib.TxRecord{Seq: uint64(i + 1),
			Outputs: []zeclib.TxOutput{{Address: "tfake-elsewhere", ToWallet: false}}})
	}
	// The last page holds a payment received at the transparent receiver.
	history[449].Outputs = append(history[449].Outputs, zeclib.TxOutput{Address: testTAddress, ToWallet: true})
	be.set(func(b *fakeBackend) { b.history = history })
	used, err := e.w.AddressUsed(testAddress)
	if err != nil || !used {
		t.Fatalf("AddressUsed: %t, %v", used, err)
	}
	if be.pages != 3 {
		t.Errorf("%d pages read", be.pages)
	}
	if used, err := e.w.AddressUsed("ufake-new"); err != nil || used {
		t.Errorf("an unused address: %t, %v", used, err)
	}
	if _, err := e.w.AddressUsed("nonsense"); err == nil {
		t.Error("an invalid address")
	}
}

func TestValidateAddress(t *testing.T) {
	e := openEnv(t)
	e.w.cache.mtx.Lock()
	e.w.cache.tip = 1_000
	e.w.cache.mtx.Unlock()
	clock := time.Unix(1_800_000_000, 0)
	prev := now
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = prev })
	height := func(h uint32) *uint32 { return &h }
	secs := func(s uint64) *uint64 { return &s }
	parsed["uexp-h999"] = &zeclib.ParsedAddress{Payable: true, ExpiryHeight: height(999)}
	parsed["uexp-h1000"] = &zeclib.ParsedAddress{Payable: true, ExpiryHeight: height(1_000)}
	parsed["uexp-h0"] = &zeclib.ParsedAddress{Payable: true, ExpiryHeight: height(0)}
	parsed["uexp-past"] = &zeclib.ParsedAddress{Payable: true, ExpiryTime: secs(1_799_999_999)}
	parsed["uexp-now"] = &zeclib.ParsedAddress{Payable: true, ExpiryTime: secs(1_800_000_000)}
	parsed["uexp-far"] = &zeclib.ParsedAddress{Payable: true, ExpiryTime: secs(math.MaxUint64)}
	t.Cleanup(func() {
		for _, a := range []string{"uexp-h999", "uexp-h1000", "uexp-h0", "uexp-past", "uexp-now", "uexp-far"} {
			delete(parsed, a)
		}
	})
	for _, tc := range []struct {
		address string
		want    bool
	}{
		{testAddress, true},
		{testTAddress, true},
		{"nonsense", false},
		{"zfake-sapling", false},
		{"uexp-h999", false},
		{"uexp-h1000", true},
		// Zero is a height an address can carry, not an absent expiry.
		{"uexp-h0", false},
		{"uexp-past", false},
		{"uexp-now", true},
		// Above the largest int64.
		{"uexp-far", true},
	} {
		if got := e.w.ValidateAddress(tc.address); got != tc.want {
			t.Errorf("%s: %t, want %t", tc.address, got, tc.want)
		}
	}
}

// Two NewAddress calls that both start after the current address received a
// payment derive one address between them, and both return it.
func TestConcurrentNewAddress(t *testing.T) {
	e := openEnv(t)
	be := e.o.latest()
	be.set(func(b *fakeBackend) {
		b.newAddress = "ufake-new"
		b.history = []zeclib.TxRecord{{Seq: 1, Outputs: []zeclib.TxOutput{
			{Pool: zeclib.OutputIronwood, Address: testReceiver, ToWallet: true},
		}}}
	})
	reached, resume := pauseAt(be, "NewAddress")
	type result struct {
		addr string
		err  error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go func() {
		a, err := e.w.NewAddress()
		first <- result{a, err}
	}()
	waitClosed(t, "the first NewAddress to derive", reached)
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		a, err := e.w.NewAddress()
		second <- result{a, err}
	}()
	stillRunning(t, "the second NewAddress", secondDone)
	close(resume)
	r1, r2 := <-first, <-second
	if r1.err != nil || r2.err != nil {
		t.Fatalf("NewAddress: %v, %v", r1.err, r2.err)
	}
	if r1.addr != "ufake-new" || r2.addr != r1.addr {
		t.Errorf("the calls returned %q and %q", r1.addr, r2.addr)
	}
	var calls int
	be.set(func(b *fakeBackend) { calls = b.newAddrCalls })
	if calls != 1 {
		t.Errorf("%d derivations, want 1", calls)
	}
}
