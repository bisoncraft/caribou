// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build zec && cgo && harness

package zeclib

import (
	"context"
	"os"
	"regexp"
	"testing"

	"github.com/bisoncraft/go-bip39"
)

// harnessBetaSeed returns the seed of the beta funder of the harness, from
// the mnemonic in the harness script.
func harnessBetaSeed(t *testing.T) []byte {
	t.Helper()
	const script = "../../../../util/testing/zec/harness.sh"
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("reading the harness script: %v", err)
	}
	m := regexp.MustCompile(`(?m)^BETA_MNEMONIC="([a-z ]+)"$`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("no BETA_MNEMONIC in %s", script)
	}
	seed, err := bip39.NewSeedWithErrorChecking(string(m[1]), "")
	if err != nil {
		t.Fatalf("the beta mnemonic: %v", err)
	}
	return seed
}

// harnessReceiveAddresses returns the transparent receive addresses that the
// wallet watches for an account.
func harnessReceiveAddresses(t *testing.T, hw *harnessWallet, accountID string) []string {
	t.Helper()
	watched, err := hw.w.be.transparentReceivers(accountID)
	if err != nil {
		t.Fatalf("transparentReceivers: %v", err)
	}
	var addresses []string
	for _, a := range watched {
		if !a.change {
			addresses = append(addresses, a.address)
		}
	}
	return addresses
}

// TestHarnessRestoreBehindSpentAddresses restores the seed of the beta
// funder of the harness from the first block on. Its first transparent
// address was paid the coinbase of 150 blocks, and the funder then shielded
// all of it, so the address has a history and nothing unspent. A restored
// wallet that learns of the use watches more addresses than a new one. The
// last of them is paid here, and the next run of this test on the same
// harness finds that payment behind the emptied address and watches further
// still.
//
// The history of the address is not the only thing that shows the use on
// this chain. The wallet scans the blocks with the shielding transactions,
// finds them by its notes and fetches the transactions they spend. The tests
// with the fake server, TestWalkFindsFundsBehindSpentAddresses among them,
// cover a use that only the history shows.
func TestHarnessRestoreBehindSpentAddresses(t *testing.T) {
	const paid = 3_000_000
	hw := newHarnessWallet(t, nil)
	account, err := hw.w.CreateAccount(context.Background(), &AccountParams{
		Name: "beta funder", Seed: harnessBetaSeed(t), Birthday: 1, Restore: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	receive := func() []string { return harnessReceiveAddresses(t, hw, account.ID) }
	if n := len(receive()); n != 10 {
		t.Fatalf("a restored account watches %d receive addresses before it syncs, want 10", n)
	}

	hw.connect()
	hw.waitFor("the use of the first address", func(*Summary, SyncStatus) bool {
		return len(receive()) > 10
	})
	before, _ := hw.waitSynced(harnessHeight(t)).Balance(account.ID)
	// Every earlier run of this test left a payment ten addresses further
	// on, and each is found through the one before it. A run that was cut
	// short can have paid an address twice.
	watched := receive()
	if len(watched)%10 != 1 || before.Transparent.Total < uint64(len(watched)/10-1)*paid {
		t.Errorf("%d receive addresses are watched and the transparent balance is %d", len(watched), before.Transparent.Total)
	}

	// The last watched address is watched only because the ones before it
	// are known to be used.
	harnessSend(t, watched[len(watched)-1], paid)
	harnessMine(t, 1)
	hw.waitFor("the payment to the last watched address", func(s *Summary, _ SyncStatus) bool {
		b, _ := s.Balance(account.ID)
		return b.Transparent.Total == before.Transparent.Total+paid
	})
}

// TestHarnessRestoreIgnoresUseBelowTheBirthday restores the seed of the beta
// funder with a birthday at the chain tip, long after the funder was paid
// and shielded. The wallet asks for nothing from below the birthday, so it
// does not learn that the first transparent address was used: it watches the
// addresses of a new account and has no balance. The transactions that
// shielded the funds of that address hold notes of the account. Brought in
// from below the birthday they would count as pending for good, because the
// wallet scans no blocks there to prove them or to see them spent.
func TestHarnessRestoreIgnoresUseBelowTheBirthday(t *testing.T) {
	hw := newHarnessWallet(t, nil)
	// The funder is idle while the tests run, so the block mined here
	// holds nothing of its own.
	harnessMine(t, 1)
	tip := harnessHeight(t)
	account, err := hw.w.CreateAccount(context.Background(), &AccountParams{
		Name: "beta funder", Seed: harnessBetaSeed(t), Birthday: tip, Restore: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	hw.connect()
	balance, _ := hw.waitSynced(tip).Balance(account.ID)
	if balance.Total != 0 || balance.Ironwood != (PoolBalance{}) || balance.Transparent != (PoolBalance{}) {
		t.Errorf("balance of an account that begins above all use of its seed: %+v", balance)
	}
	if n := len(harnessReceiveAddresses(t, hw, account.ID)); n != 10 {
		t.Errorf("%d receive addresses are watched, want 10", n)
	}
	// No address counts as used, the first one included.
	watched, err := hw.w.be.transparentReceivers(account.ID)
	if err != nil {
		t.Fatalf("transparentReceivers: %v", err)
	}
	for _, a := range watched {
		if !a.unused {
			t.Errorf("address %s counts as used", a.address)
		}
	}
}
