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

// TestHarnessRestoreBehindSpentAddresses restores the seed of the beta
// funder of the harness. Its first transparent address was paid the coinbase
// of 150 blocks, and the funder then shielded all of it, so the address has
// a history and nothing unspent. A restored wallet that learns of the use
// watches more addresses than a new one. The last of them is paid here, and
// the next run of this test on the same harness finds that payment behind
// the emptied address and watches further still.
//
// The account begins a few blocks below the tip, long after the funder
// shielded. The wallet never scans the blocks that hold the shielding
// transactions, so it does not come across them through its notes: the
// history of the address is the only thing that shows the use.
func TestHarnessRestoreBehindSpentAddresses(t *testing.T) {
	const paid = 3_000_000
	hw := newHarnessWallet(t, nil)
	tip := harnessHeight(t)
	account, err := hw.w.CreateAccount(context.Background(), &AccountParams{
		Name: "beta funder", Seed: harnessBetaSeed(t), Birthday: tip - 5, Restore: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	receive := func() []string {
		watched, err := hw.w.be.transparentReceivers(account.ID)
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
	if n := len(receive()); n != 10 {
		t.Fatalf("a restored account watches %d receive addresses before it syncs, want 10", n)
	}

	hw.connect()
	hw.waitFor("the use of the first address", func(*Summary, SyncStatus) bool {
		return len(receive()) > 10
	})
	before, _ := hw.waitSynced(harnessHeight(t)).Balance(account.ID)
	// The history of the first address holds the transaction that shielded
	// its funds, and with it the notes the funder received then. The wallet
	// counts them as pending. They are from below the birthday, where it
	// scans no blocks, so they do not become spendable and it does not see
	// them spent.
	if before.Ironwood.Spendable != 0 || before.Ironwood.ValuePending == 0 {
		t.Errorf("balance of the restored funder: %+v", before)
	}
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
