// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dbtest

import (
	"bytes"
	crand "crypto/rand"
	mrand "math/rand/v2"
	"strings"
	"time"

	ordertest "github.com/bisoncraft/caribou/util/order/test"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/db"
)

var (
	nowUnix = time.Now().Unix()
	rand    = mrand.New(mrand.NewPCG(uint64(nowUnix), 0xdffe))
)

// RandomBalance creates a random balance.
func RandomBalance() *asset.Balance {
	return &asset.Balance{
		Available: rand.Uint64(),
		Immature:  rand.Uint64(),
		Locked:    rand.Uint64(),
	}
}

func RandomPrimaryCredentials() *db.PrimaryCredentials {
	return &db.PrimaryCredentials{
		EncSeed:        randBytes(10),
		EncInnerKey:    randBytes(10),
		InnerKeyParams: randBytes(10),
		OuterKeyParams: randBytes(10),
		Birthday:       time.Unix(rand.Int64(), 0),
		Version:        1,
	}
}

// RandomWallet creates a random wallet.
func RandomWallet() *db.Wallet {
	return &db.Wallet{
		AssetID: rand.Uint32(),
		Settings: map[string]string{
			ordertest.RandomAddress(): ordertest.RandomAddress(),
			ordertest.RandomAddress(): ordertest.RandomAddress(),
			ordertest.RandomAddress(): ordertest.RandomAddress(),
			ordertest.RandomAddress(): ordertest.RandomAddress(),
		},
		Balance: &db.Balance{
			Balance: *RandomBalance(),
			Stamp:   time.Unix(rand.Int64()/(1<<31), 0),
		},
		Address: ordertest.RandomAddress(),
	}
}

func randBytes(l int) []byte {
	b := make([]byte, l)
	crand.Read(b)
	return b
}

func RandomNotification(maxTime uint64) *db.Notification {
	return &db.Notification{
		NoteType:    ordertest.RandomAddress(),
		SubjectText: ordertest.RandomAddress(),
		DetailText:  ordertest.RandomAddress(),
		// Since this is for DB tests, only use severity level >= Success.
		Severeness: db.Severity(rand.IntN(3)) + db.Success,
		TimeStamp:  uint64(rand.Int64N(int64(maxTime))),
	}
}

type testKiller interface {
	Helper()
	Fatalf(string, ...any)
}

// MustCompareWallets ensures the two Wallet are identical, calling the Fatalf
// method of the testKiller if not.
func MustCompareWallets(t testKiller, w1, w2 *db.Wallet) {
	if w1.AssetID != w2.AssetID {
		t.Fatalf("AssetID mismatch. %d != %d", w1.AssetID, w2.AssetID)
	}
	if len(w1.Settings) != len(w2.Settings) {
		t.Fatalf("Settings mismatch. %d != %s", len(w1.Settings), len(w2.Settings))
	}
	w2Settings := make(map[string]string, len(w2.Settings))
	for k, v := range w2.Settings {
		w2Settings[strings.ToLower(k)] = v
	}
	for k, v1 := range w1.Settings {
		if v2, ok := w2Settings[k]; !ok {
			t.Fatalf("Settings mismatch: key '%s' not found in one wallet", k)
		} else if v1 != v2 {
			t.Fatalf("Settings mismatch: different values for key '%s'", k)
		}
	}
	MustCompareBalances(t, w1.Balance, w2.Balance)
	if w1.Address != w2.Address {
		t.Fatalf("Address mismatch. %s != %s", w1.Address, w2.Address)
	}
	if !bytes.Equal(w1.EncryptedPW, w2.EncryptedPW) {
		t.Fatalf("EncryptedPW mismatch. %x != %x", w1.EncryptedPW, w2.EncryptedPW)
	}
}

// MustCompareBalances ensures the two BalanceSet are identical, calling the
// Fatalf method of the testKiller if not.
func MustCompareBalances(t testKiller, b1, b2 *db.Balance) {
	if !b1.Stamp.Equal(b2.Stamp) {
		t.Fatalf("balance timestamp mismatch. %s != %s", b1.Stamp, b2.Stamp)
	}
	MustCompareAssetBalances(t, "zero-conf", &b1.Balance, &b2.Balance)
}

// MustCompareAssetBalances ensures the two asset.Balances are identical,
// calling the Fatalf method of the testKiller if not.
func MustCompareAssetBalances(t testKiller, host string, b1, b2 *asset.Balance) {
	if b1.Available != b2.Available {
		t.Fatalf("%s available balance mismatch. %d != %d", host, b1.Available, b2.Available)
	}
	if b1.Immature != b2.Immature {
		t.Fatalf("%s immature balance mismatch. %d != %d", host, b1.Immature, b2.Immature)
	}
	if b1.Locked != b2.Locked {
		t.Fatalf("%s locked balance mismatch. %d != %d", host, b1.Locked, b2.Locked)
	}
}

func MustCompareNotifications(t testKiller, n1, n2 *db.Notification) {
	if n1.NoteType != n2.NoteType {
		t.Fatalf("NoteType mismatch. %s != %s", n1.NoteType, n2.NoteType)
	}
	if n1.SubjectText != n2.SubjectText {
		t.Fatalf("SubjectText mismatch. %s != %s", n1.SubjectText, n2.SubjectText)
	}
	if n1.DetailText != n2.DetailText {
		t.Fatalf("DetailText mismatch. %s != %s", n1.DetailText, n2.DetailText)
	}
	if n1.Severeness != n2.Severeness {
		t.Fatalf("Severeness mismatch. %d != %d", n1.Severeness, n2.Severeness)
	}
	if n1.TimeStamp != n2.TimeStamp {
		t.Fatalf("TimeStamp mismatch. %d != %d", n1.TimeStamp, n2.TimeStamp)
	}
	if n1.ID().String() != n2.ID().String() {
		t.Fatalf("ID mismatch. %s != %s", n1.ID(), n2.ID())
	}
}
