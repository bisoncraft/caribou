package bolt

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/db"
	dbtest "github.com/bisoncraft/caribou/wallet/db/test"
)

var (
	tLogger = util.StdOutLogger("db_TEST", util.LevelTrace)
)

func newTestDB(t *testing.T) (*BoltDB, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "db.db")
	dbi, err := NewDB(dbPath, tLogger)
	if err != nil {
		t.Fatalf("error creating dB: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dbi.Run(ctx)
	}()
	db, ok := dbi.(*BoltDB)
	if !ok {
		t.Fatalf("DB is not a *BoltDB")
	}
	shutdown := func() {
		cancel()
		wg.Wait()
	}
	return db, shutdown
}

func TestMain(m *testing.M) {
	defer os.Stdout.Sync()
	os.Exit(m.Run())
}

func TestStorePrimaryCredentials(t *testing.T) {
	boltdb, shutdown := newTestDB(t)
	defer shutdown()

	// Trying to fetch credentials before storing should be an error.
	_, err := boltdb.PrimaryCredentials()
	if err == nil {
		t.Fatalf("no error for missing credentials: %v", err)
	}

	newCreds := func(seed, inner, innerParams, outerParams bool) *db.PrimaryCredentials {
		creds := &db.PrimaryCredentials{}
		if seed {
			creds.EncSeed = []byte("EncSeed")
		}
		if inner {
			creds.EncInnerKey = []byte("EncInnerKey")
		}
		if innerParams {
			creds.InnerKeyParams = []byte("InnerKeyParams")
		}
		if outerParams {
			creds.OuterKeyParams = []byte("OuterKeyParams")
		}
		creds.Birthday = time.Now().Truncate(time.Second)
		return creds
	}

	ensureErr := func(tag string, creds *db.PrimaryCredentials) {
		if err := boltdb.SetPrimaryCredentials(creds); err == nil {
			t.Fatalf("%s: no error", tag)
		}
	}

	ensureErr("no EncSeed", newCreds(false, true, true, true))
	ensureErr("no EncInnerKey", newCreds(true, false, true, true))
	ensureErr("no InnerKeyParams", newCreds(true, true, false, true))
	ensureErr("no OuterKeyParams", newCreds(true, true, true, false))

	// Success
	goodCreds := newCreds(true, true, true, true)
	err = boltdb.SetPrimaryCredentials(goodCreds)
	if err != nil {
		t.Fatalf("SetPrimaryCredentials error: %v", err)
	}

	// Retrieve the credentials and check for consistency.
	reCreds, err := boltdb.PrimaryCredentials()
	if err != nil {
		t.Fatalf("PrimaryCredentials error: %v", err)
	}

	if !bytes.Equal(reCreds.EncSeed, goodCreds.EncSeed) {
		t.Fatalf("EncSeed wrong, wanted %x, got %x", goodCreds.EncSeed, reCreds.EncSeed)
	}
	if !bytes.Equal(reCreds.EncInnerKey, goodCreds.EncInnerKey) {
		t.Fatalf("EncInnerKey wrong, wanted %x, got %x", goodCreds.EncInnerKey, reCreds.EncInnerKey)
	}
	if !bytes.Equal(reCreds.InnerKeyParams, goodCreds.InnerKeyParams) {
		t.Fatalf("InnerKeyParams wrong, wanted %x, got %x", goodCreds.InnerKeyParams, reCreds.InnerKeyParams)
	}
	if !bytes.Equal(reCreds.OuterKeyParams, goodCreds.OuterKeyParams) {
		t.Fatalf("OuterKeyParams wrong, wanted %x, got %x", goodCreds.OuterKeyParams, reCreds.OuterKeyParams)
	}
	if !reCreds.Birthday.Equal(goodCreds.Birthday) {
		t.Fatalf("Birthday wrong. wanted %s, got %s", goodCreds.Birthday, reCreds.Birthday)
	}
}

func TestWallets(t *testing.T) {
	boltdb, shutdown := newTestDB(t)
	defer shutdown()

	wallets, err := boltdb.Wallets()
	if err != nil {
		t.Fatalf("error listing wallets from empty DB: %v", err)
	}
	if len(wallets) != 0 {
		t.Fatalf("unexpected non-empty wallets in fresh DB")
	}
	// Create and insert 1,000 wallets.
	numToDo := 1000
	if testing.Short() {
		numToDo = 50
	}
	wallets = make([]*db.Wallet, 0, numToDo)
	walletMap := make(map[string]*db.Wallet)
	tStart := time.Now()
	nTimes(numToDo, func(int) {
		w := dbtest.RandomWallet()
		wallets = append(wallets, w)
		walletMap[w.SID()] = w
		boltdb.UpdateWallet(w)
	})
	t.Logf("%d milliseconds to insert %d Wallet", time.Since(tStart)/time.Millisecond, numToDo)

	tStart = time.Now()
	reWallets, err := boltdb.Wallets()
	if err != nil {
		t.Fatalf("wallets retrieval error: %v", err)
	}
	if len(reWallets) != numToDo {
		t.Fatalf("expected %d wallets, got %d", numToDo, len(reWallets))
	}
	for _, reW := range reWallets {
		wid := reW.SID()
		ogWallet, found := walletMap[wid]
		if !found {
			t.Fatalf("wallet %s not found after retrieval", wid)
		}
		dbtest.MustCompareWallets(t, reW, ogWallet)
	}
	// Test changing the balance
	w := reWallets[0]
	newBal := *w.Balance
	newBal.Available += 1e8
	newBal.Locked += 2e8
	newBal.Immature += 3e8
	newBal.Stamp = newBal.Stamp.Add(time.Second)
	boltdb.UpdateBalance(w.ID(), &newBal)
	reW, err := boltdb.Wallet(w.ID())
	if err != nil {
		t.Fatalf("failed to retrieve wallet for balance check")
	}
	dbtest.MustCompareBalances(t, reW.Balance, &newBal)
	if !reW.Balance.Stamp.After(w.Balance.Stamp) {
		t.Fatalf("update time can't be right: %s > %s", reW.Balance.Stamp, w.Balance.Stamp)
	}

	// Test changing the password.
	newPW := randBytes(32)
	boltdb.SetWalletPassword(w.ID(), newPW)
	reW, err = boltdb.Wallet(w.ID())
	if err != nil {
		t.Fatalf("failed to retrieve wallet for new password check")
	}
	if !bytes.Equal(newPW, reW.EncryptedPW) {
		t.Fatalf("failed to set password. wanted %x, got %x", newPW, reW.EncryptedPW)
	}
	t.Logf("%d milliseconds to read and compare %d Wallet", time.Since(tStart)/time.Millisecond, numToDo)

}

func nTimes(n int, f func(int)) {
	for i := 0; i < n; i++ {
		f(i)
	}
}

func randBytes(l int) []byte {
	b := make([]byte, l)
	crand.Read(b)
	return b
}

func TestNotifications(t *testing.T) {
	boltdb, shutdown := newTestDB(t)
	defer shutdown()

	numToDo := 1000
	numToFetch := 200
	if testing.Short() {
		numToDo = 10
		numToFetch = 2
	}
	newest := uint64(rand.Int63()) - uint64(numToFetch)

	notes := make([]*db.Notification, 0, numToDo)
	nTimes(numToDo, func(int) {
		notes = append(notes, dbtest.RandomNotification(newest))
	})

	fetches := make([]*db.Notification, numToFetch)
	for i := numToFetch - 1; i >= 0; i-- {
		newest++
		notes[i].TimeStamp = newest
		fetches[i] = notes[i]
	}

	tStart := time.Now()
	nTimes(numToDo, func(i int) {
		err := boltdb.SaveNotification(notes[i])
		if err != nil {
			t.Fatalf("SaveNotification error: %v", err)
		}
	})
	t.Logf("%d milliseconds to insert %d active Notification", time.Since(tStart)/time.Millisecond, numToDo)

	tStart = time.Now()
	fetched, err := boltdb.NotificationsN(numToFetch)
	if err != nil {
		t.Fatalf("fetch error: %v", err)
	}
	t.Logf("%d milliseconds to fetch %d sorted Notification", time.Since(tStart)/time.Millisecond, numToFetch)
	if len(fetched) != numToFetch {
		t.Fatalf("fetched wrong number of notifications. %d != %d", len(fetched), numToFetch)
	}

	for i, note := range fetched {
		dbtest.MustCompareNotifications(t, note, fetches[i])
		boltdb.AckNotification(note.ID())
	}

	fetched, err = boltdb.NotificationsN(numToFetch)
	if err != nil {
		t.Fatalf("fetch error after acks: %v", err)
	}

	for _, note := range fetched {
		if !note.Ack {
			t.Fatalf("order acknowledgement not recorded")
		}
	}
}

func TestPokes(t *testing.T) {
	boltdb, shutdown := newTestDB(t)
	defer shutdown()

	pokes := []*db.Notification{
		dbtest.RandomNotification(1), dbtest.RandomNotification(2), dbtest.RandomNotification(3),
		dbtest.RandomNotification(6), dbtest.RandomNotification(5), dbtest.RandomNotification(4),
		dbtest.RandomNotification(7),
	}

	if err := boltdb.SavePokes(pokes); err != nil {
		t.Fatalf("SavePokes error: %v", err)
	}

	rePokes, err := boltdb.LoadPokes()
	if err != nil {
		t.Fatalf("LoadPokes error: %v", err)
	}

	if len(rePokes) != len(pokes) {
		t.Fatalf("Expected to load %d pokes. Loaded %d instead.", len(pokes), len(rePokes))
	}

	for i, poke := range pokes {
		dbtest.MustCompareNotifications(t, poke, rePokes[i])
	}

	noPokes, err := boltdb.LoadPokes()
	if err != nil {
		t.Fatalf("Second LoadPokes error: %v", err)
	}
	if len(noPokes) != 0 {
		t.Fatal("Result from second LoadPokes wasn't empty")
	}
}

func TestStoreMultisigIndexForPubkey(t *testing.T) {
	boltdb, shutdown := newTestDB(t)
	defer shutdown()
	pubkey := [33]byte{}
	assetID := uint32(42)
	idx := uint32(0)
	if err := boltdb.StoreMultisigIndexForPubkey(assetID, idx, pubkey); err != nil {
		t.Fatal(err)
	}
}

func TestMultisigIndexForPubkey(t *testing.T) {
	boltdb, shutdown := newTestDB(t)
	defer shutdown()
	pubkey := [33]byte{}
	assetID := uint32(42)
	idx := uint32(0)
	if err := boltdb.StoreMultisigIndexForPubkey(assetID, idx, pubkey); err != nil {
		t.Fatal(err)
	}
	gotIdx, err := boltdb.MultisigIndexForPubkey(assetID, pubkey)
	if err != nil {
		t.Fatal(err)
	}
	if idx != gotIdx {
		t.Fatalf("wanted %d but got %d", idx, gotIdx)
	}
}

// MM epoch snapshot tests removed along with StoreMMEpochSnapshot,
// MMEpochSnapshots, and PruneMMEpochSnapshots from BoltDB.
