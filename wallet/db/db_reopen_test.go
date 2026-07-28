// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db_test

import (
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/wallet/db"
	dbtest "github.com/bisoncraft/caribou/wallet/db/test"
)

// TestReopen covers the restart path the conformance suite cannot: write,
// close the database, reopen it at the same path, and read everything back.
// The engine under test is selected at build time, same as the suite drivers.
func TestReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coredb")
	d, err := db.New(path, tLogger)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}

	w1, w2 := dbtest.RandomWallet(), dbtest.RandomWallet()
	// w2 takes the shape of a token wallet: no encrypted password, and no
	// address until the first connect.
	w2.EncryptedPW = nil
	w2.Address = ""
	for _, w := range []*db.Wallet{w1, w2} {
		if err := d.UpdateWallet(w); err != nil {
			t.Fatalf("UpdateWallet(%d): %v", w.AssetID, err)
		}
	}
	creds := dbtest.RandomPrimaryCredentials()
	if err := d.SetPrimaryCredentials(creds); err != nil {
		t.Fatalf("SetPrimaryCredentials: %v", err)
	}
	note := dbtest.RandomNotification(1000)
	if err := d.SaveNotification(note); err != nil {
		t.Fatalf("SaveNotification: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	d, err = db.New(path, tLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close after reopen: %v", err)
		}
	}()

	ws, err := d.Wallets()
	if err != nil {
		t.Fatalf("Wallets after reopen: %v", err)
	}
	if len(ws) != 2 {
		t.Fatalf("expected 2 wallets after reopen, got %d", len(ws))
	}
	for _, reW := range ws {
		og := w1
		if reW.AssetID == w2.AssetID {
			og = w2
		}
		dbtest.MustCompareWallets(t, reW, og)
	}

	reCreds, err := d.PrimaryCredentials()
	if err != nil {
		t.Fatalf("PrimaryCredentials after reopen: %v", err)
	}
	if !reCreds.Birthday.Equal(creds.Birthday) {
		t.Fatalf("credentials Birthday wrong after reopen: %s != %s",
			reCreds.Birthday, creds.Birthday)
	}

	notes, err := d.NotificationsN(10)
	if err != nil {
		t.Fatalf("NotificationsN after reopen: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("expected 1 notification after reopen, got %d", len(notes))
	}
	dbtest.MustCompareNotifications(t, notes[0], note)
}
