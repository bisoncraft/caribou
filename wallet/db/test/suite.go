// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dbtest

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/db"
)

// RunSuite runs the backend-agnostic conformance tests for the db.DB
// interface against the implementation constructed by newDB. newDB must
// return a DB that is ready for use, registering any teardown with t.Cleanup.
// Every subtest constructs its own fresh DB, so assertions about empty-store
// behavior hold in each.
//
// The assertions here define the db.DB contract: anything a backend must
// honor for wallet/core to function belongs in this suite, while behavior
// particular to a storage engine stays in that backend's own tests.
func RunSuite(t *testing.T, newDB func(t *testing.T) db.DB) {
	run := func(name string, f func(*testing.T, db.DB)) {
		t.Run(name, func(t *testing.T) {
			f(t, newDB(t))
		})
	}
	run("PrimaryCredentials", testPrimaryCredentials)
	run("Wallets", testWallets)
	run("WalletStatus", testWalletStatus)
	run("Notifications", testNotifications)
	run("Pokes", testPokes)
	run("DisabledRateSources", testDisabledRateSources)
	run("Language", testLanguage)
	run("SendRequiresPassword", testSendRequiresPassword)
	run("Tokens", testTokens)
	run("Multisig", testMultisig)
	run("ConnectLifecycle", testConnectLifecycle)
}

func testConnectLifecycle(t *testing.T, d db.DB) {
	// Connect ties the store's shutdown to the context: cancellation closes
	// the underlying engine, and the returned WaitGroup completes only once
	// the close has finished. A write after that must return an error rather
	// than panic. The teardown Close registered by the suite's factory runs
	// after this test, so it also verifies that both engines tolerate Close
	// on an already-closed store.
	ctx, cancel := context.WithCancel(t.Context())
	wg, err := d.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	w := RandomWallet()
	if err := d.UpdateWallet(w); err != nil {
		t.Fatalf("UpdateWallet while connected: %v", err)
	}
	cancel()
	wg.Wait()
	if err := d.UpdateWallet(w); err == nil {
		t.Fatalf("no error writing after context-canceled shutdown")
	}
}

func testPrimaryCredentials(t *testing.T, d db.DB) {
	// Trying to fetch credentials before storing must return
	// db.ErrNoCredentials; wallet/core distinguishes it from real errors
	// during initialization.
	_, err := d.PrimaryCredentials()
	if !errors.Is(err, db.ErrNoCredentials) {
		t.Fatalf("expected ErrNoCredentials for missing credentials, got %v", err)
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
		t.Helper()
		if err := d.SetPrimaryCredentials(creds); err == nil {
			t.Fatalf("%s: no error", tag)
		}
	}

	ensureErr("no EncSeed", newCreds(false, true, true, true))
	ensureErr("no EncInnerKey", newCreds(true, false, true, true))
	ensureErr("no InnerKeyParams", newCreds(true, true, false, true))
	ensureErr("no OuterKeyParams", newCreds(true, true, true, false))

	// Success
	goodCreds := newCreds(true, true, true, true)
	if err := d.SetPrimaryCredentials(goodCreds); err != nil {
		t.Fatalf("SetPrimaryCredentials error: %v", err)
	}

	compareCreds := func(reCreds, creds *db.PrimaryCredentials) {
		t.Helper()
		if !bytes.Equal(reCreds.EncSeed, creds.EncSeed) {
			t.Fatalf("EncSeed wrong, wanted %x, got %x", creds.EncSeed, reCreds.EncSeed)
		}
		if !bytes.Equal(reCreds.EncInnerKey, creds.EncInnerKey) {
			t.Fatalf("EncInnerKey wrong, wanted %x, got %x", creds.EncInnerKey, reCreds.EncInnerKey)
		}
		if !bytes.Equal(reCreds.InnerKeyParams, creds.InnerKeyParams) {
			t.Fatalf("InnerKeyParams wrong, wanted %x, got %x", creds.InnerKeyParams, reCreds.InnerKeyParams)
		}
		if !bytes.Equal(reCreds.OuterKeyParams, creds.OuterKeyParams) {
			t.Fatalf("OuterKeyParams wrong, wanted %x, got %x", creds.OuterKeyParams, reCreds.OuterKeyParams)
		}
		if !reCreds.Birthday.Equal(creds.Birthday) {
			t.Fatalf("Birthday wrong. wanted %s, got %s", creds.Birthday, reCreds.Birthday)
		}
	}

	// Retrieve the credentials and check for consistency.
	reCreds, err := d.PrimaryCredentials()
	if err != nil {
		t.Fatalf("PrimaryCredentials error: %v", err)
	}
	compareCreds(reCreds, goodCreds)

	// Setting again overwrites. Core's changeAppPass depends on this.
	newerCreds := RandomPrimaryCredentials()
	if err := d.SetPrimaryCredentials(newerCreds); err != nil {
		t.Fatalf("SetPrimaryCredentials overwrite error: %v", err)
	}
	reCreds, err = d.PrimaryCredentials()
	if err != nil {
		t.Fatalf("PrimaryCredentials error after overwrite: %v", err)
	}
	compareCreds(reCreds, newerCreds)
}

func testWallets(t *testing.T, d db.DB) {
	wallets, err := d.Wallets()
	if err != nil {
		t.Fatalf("error listing wallets from empty DB: %v", err)
	}
	if len(wallets) != 0 {
		t.Fatalf("unexpected non-empty wallets in fresh DB")
	}

	// A wallet with no Balance is rejected.
	noBal := RandomWallet()
	noBal.Balance = nil
	if err := d.UpdateWallet(noBal); err == nil {
		t.Fatalf("no error for UpdateWallet with nil Balance")
	}

	// Operations on an unknown asset ID are ErrWalletNotFound; wallet/core
	// distinguishes absence from database corruption.
	const unknownID uint32 = 0xdeadbeef
	if _, err := d.Wallet(unknownID); !errors.Is(err, db.ErrWalletNotFound) {
		t.Fatalf("expected ErrWalletNotFound retrieving unknown wallet, got %v", err)
	}
	if err := d.UpdateBalance(unknownID, &db.Balance{}); !errors.Is(err, db.ErrWalletNotFound) {
		t.Fatalf("expected ErrWalletNotFound for UpdateBalance on unknown wallet, got %v", err)
	}
	if err := d.SetWalletPassword(unknownID, randBytes(32)); !errors.Is(err, db.ErrWalletNotFound) {
		t.Fatalf("expected ErrWalletNotFound for SetWalletPassword on unknown wallet, got %v", err)
	}
	if err := d.SetWalletAddress(unknownID, "addr"); !errors.Is(err, db.ErrWalletNotFound) {
		t.Fatalf("expected ErrWalletNotFound for SetWalletAddress on unknown wallet, got %v", err)
	}

	// Create and insert 1,000 wallets.
	numToDo := 1000
	if testing.Short() {
		numToDo = 50
	}
	wallets = make([]*db.Wallet, 0, numToDo)
	walletMap := make(map[uint32]*db.Wallet)
	tStart := time.Now()
	nTimes(numToDo, func(int) {
		w := RandomWallet()
		wallets = append(wallets, w)
		walletMap[w.AssetID] = w
		if err := d.UpdateWallet(w); err != nil {
			t.Fatalf("UpdateWallet error: %v", err)
		}
	})
	t.Logf("%d milliseconds to insert %d Wallet", time.Since(tStart)/time.Millisecond, numToDo)

	// A token-shaped wallet — no encrypted password, and no address until the
	// first connect — must survive storage and listing. The zero-length field
	// values once tripped a minimum-length check in the desktop engine's
	// datum decoding, aborting Wallets() for every wallet in the table.
	tokenW := RandomWallet()
	tokenW.EncryptedPW = nil
	tokenW.Address = ""
	wallets = append(wallets, tokenW)
	walletMap[tokenW.AssetID] = tokenW
	if err := d.UpdateWallet(tokenW); err != nil {
		t.Fatalf("UpdateWallet error for token-shaped wallet: %v", err)
	}

	tStart = time.Now()
	reWallets, err := d.Wallets()
	if err != nil {
		t.Fatalf("wallets retrieval error: %v", err)
	}
	if len(reWallets) != len(walletMap) {
		t.Fatalf("expected %d wallets, got %d", len(walletMap), len(reWallets))
	}
	for _, reW := range reWallets {
		ogWallet, found := walletMap[reW.AssetID]
		if !found {
			t.Fatalf("wallet %d not found after retrieval", reW.AssetID)
		}
		MustCompareWallets(t, reW, ogWallet)
	}

	// Test changing the balance.
	w := reWallets[0]
	newBal := *w.Balance
	newBal.Available += 1e8
	newBal.Locked += 2e8
	newBal.Immature += 3e8
	newBal.Stamp = newBal.Stamp.Add(time.Second)
	if err := d.UpdateBalance(w.AssetID, &newBal); err != nil {
		t.Fatalf("UpdateBalance error: %v", err)
	}
	reW, err := d.Wallet(w.AssetID)
	if err != nil {
		t.Fatalf("failed to retrieve wallet for balance check")
	}
	MustCompareBalances(t, reW.Balance, &newBal)
	if !reW.Balance.Stamp.After(w.Balance.Stamp) {
		t.Fatalf("update time can't be right: %s > %s", reW.Balance.Stamp, w.Balance.Stamp)
	}

	// Test changing the password.
	newPW := randBytes(32)
	if err := d.SetWalletPassword(w.AssetID, newPW); err != nil {
		t.Fatalf("SetWalletPassword error: %v", err)
	}
	reW, err = d.Wallet(w.AssetID)
	if err != nil {
		t.Fatalf("failed to retrieve wallet for new password check")
	}
	if !bytes.Equal(newPW, reW.EncryptedPW) {
		t.Fatalf("failed to set password. wanted %x, got %x", newPW, reW.EncryptedPW)
	}

	// Test changing the deposit address.
	newAddr := "newDepositAddress"
	if err := d.SetWalletAddress(w.AssetID, newAddr); err != nil {
		t.Fatalf("SetWalletAddress error: %v", err)
	}
	reW, err = d.Wallet(w.AssetID)
	if err != nil {
		t.Fatalf("failed to retrieve wallet for new address check")
	}
	if reW.Address != newAddr {
		t.Fatalf("failed to set address. wanted %s, got %s", newAddr, reW.Address)
	}

	// None of the partial updates disturbed the other fields.
	og := walletMap[w.AssetID]
	reW.Balance = og.Balance
	reW.EncryptedPW = og.EncryptedPW
	reW.Address = og.Address
	MustCompareWallets(t, reW, og)
	t.Logf("%d milliseconds to read and compare %d Wallet", time.Since(tStart)/time.Millisecond, numToDo)
}

func testWalletStatus(t *testing.T, d db.DB) {
	if err := d.UpdateWalletStatus(0xdeadbeef, true); !errors.Is(err, db.ErrWalletNotFound) {
		t.Fatalf("expected ErrWalletNotFound for UpdateWalletStatus on unknown wallet, got %v", err)
	}

	w := RandomWallet()
	if err := d.UpdateWallet(w); err != nil {
		t.Fatalf("UpdateWallet error: %v", err)
	}

	ensureDisabled := func(tag string, want bool) {
		t.Helper()
		reW, err := d.Wallet(w.AssetID)
		if err != nil {
			t.Fatalf("%s: Wallet error: %v", tag, err)
		}
		if reW.Disabled != want {
			t.Fatalf("%s: Disabled = %t, wanted %t", tag, reW.Disabled, want)
		}
	}

	ensureDisabled("fresh wallet", false)

	if err := d.UpdateWalletStatus(w.AssetID, true); err != nil {
		t.Fatalf("UpdateWalletStatus(true) error: %v", err)
	}
	ensureDisabled("after disable", true)

	// The disabled flag survives an UpdateWallet, which writes every wallet
	// field except the flag.
	if err := d.UpdateWallet(w); err != nil {
		t.Fatalf("UpdateWallet error on disabled wallet: %v", err)
	}
	ensureDisabled("after UpdateWallet", true)

	if err := d.UpdateWalletStatus(w.AssetID, false); err != nil {
		t.Fatalf("UpdateWalletStatus(false) error: %v", err)
	}
	ensureDisabled("after re-enable", false)
}

func testNotifications(t *testing.T, d db.DB) {
	// Severity below Success is not storable.
	badNote := RandomNotification(1000)
	badNote.Severeness = db.Success - 1
	if err := d.SaveNotification(badNote); err == nil {
		t.Fatalf("no error for storing severity %s", badNote.Severeness)
	}

	// Acking an unknown notification errors.
	if err := d.AckNotification(randBytes(32)); err == nil {
		t.Fatalf("no error for acking unknown notification")
	}

	numToDo := 1000
	numToFetch := 200
	if testing.Short() {
		numToDo = 10
		numToFetch = 2
	}
	newest := uint64(rand.Int64()) - uint64(numToFetch)

	notes := make([]*db.Notification, 0, numToDo)
	nTimes(numToDo, func(int) {
		notes = append(notes, RandomNotification(newest))
	})

	fetches := make([]*db.Notification, numToFetch)
	for i := numToFetch - 1; i >= 0; i-- {
		newest++
		notes[i].TimeStamp = newest
		fetches[i] = notes[i]
	}

	tStart := time.Now()
	nTimes(numToDo, func(i int) {
		if err := d.SaveNotification(notes[i]); err != nil {
			t.Fatalf("SaveNotification error: %v", err)
		}
	})
	t.Logf("%d milliseconds to insert %d active Notification", time.Since(tStart)/time.Millisecond, numToDo)

	tStart = time.Now()
	fetched, err := d.NotificationsN(numToFetch)
	if err != nil {
		t.Fatalf("fetch error: %v", err)
	}
	t.Logf("%d milliseconds to fetch %d sorted Notification", time.Since(tStart)/time.Millisecond, numToFetch)
	if len(fetched) != numToFetch {
		t.Fatalf("fetched wrong number of notifications. %d != %d", len(fetched), numToFetch)
	}

	for i, note := range fetched {
		MustCompareNotifications(t, note, fetches[i])
		if !bytes.Equal(note.Id, note.ID()) {
			t.Fatalf("fetched notification Id field not populated")
		}
		if err := d.AckNotification(note.ID()); err != nil {
			t.Fatalf("AckNotification error: %v", err)
		}
	}

	fetched, err = d.NotificationsN(numToFetch)
	if err != nil {
		t.Fatalf("fetch error after acks: %v", err)
	}

	for _, note := range fetched {
		if !note.Ack {
			t.Fatalf("notification acknowledgement not recorded")
		}
	}

	// Re-saving an acked notification preserves the acknowledgement.
	// SaveNotification must read the existing record's ack flag before
	// writing; a blind Set would silently un-ack every re-fired notification.
	reSaved := fetches[0] // the newest, so NotificationsN(1) returns it
	if err := d.SaveNotification(reSaved); err != nil {
		t.Fatalf("SaveNotification re-save error: %v", err)
	}
	fetched, err = d.NotificationsN(1)
	if err != nil {
		t.Fatalf("fetch error after re-save: %v", err)
	}
	if len(fetched) != 1 || !bytes.Equal(fetched[0].Id, reSaved.ID()) {
		t.Fatalf("re-saved notification is not the newest")
	}
	if !fetched[0].Ack {
		t.Fatalf("re-save dropped the acknowledgement")
	}

	// Asking for more than are stored returns all of them.
	fetched, err = d.NotificationsN(numToDo + 100)
	if err != nil {
		t.Fatalf("overask fetch error: %v", err)
	}
	if len(fetched) != numToDo {
		t.Fatalf("overask fetched %d notifications, wanted all %d", len(fetched), numToDo)
	}

	// A non-positive count is an empty page, not a panic. The guard has to
	// precede the allocation: a negative n handed to make panics in makeslice
	// and takes the process with it.
	for _, n := range []int{0, -1} {
		fetched, err = d.NotificationsN(n)
		if err != nil {
			t.Fatalf("fetch error for n=%d: %v", n, err)
		}
		if len(fetched) != 0 {
			t.Fatalf("n=%d fetched %d notifications, wanted none", n, len(fetched))
		}
	}
}

func testPokes(t *testing.T, d db.DB) {
	// Loading pokes before any were saved is not an error.
	noPokes, err := d.LoadPokes()
	if err != nil {
		t.Fatalf("LoadPokes error on empty DB: %v", err)
	}
	if len(noPokes) != 0 {
		t.Fatalf("unexpected pokes in fresh DB")
	}

	// A newer slice overwrites an older one.
	if err := d.SavePokes([]*db.Notification{RandomNotification(1)}); err != nil {
		t.Fatalf("first SavePokes error: %v", err)
	}

	pokes := []*db.Notification{
		RandomNotification(1), RandomNotification(2), RandomNotification(3),
		RandomNotification(6), RandomNotification(5), RandomNotification(4),
		RandomNotification(7),
	}

	if err := d.SavePokes(pokes); err != nil {
		t.Fatalf("SavePokes error: %v", err)
	}

	rePokes, err := d.LoadPokes()
	if err != nil {
		t.Fatalf("LoadPokes error: %v", err)
	}

	if len(rePokes) != len(pokes) {
		t.Fatalf("Expected to load %d pokes. Loaded %d instead.", len(pokes), len(rePokes))
	}

	for i, poke := range pokes {
		MustCompareNotifications(t, poke, rePokes[i])
	}

	// Loading deletes, so a second load comes up empty.
	noPokes, err = d.LoadPokes()
	if err != nil {
		t.Fatalf("Second LoadPokes error: %v", err)
	}
	if len(noPokes) != 0 {
		t.Fatal("Result from second LoadPokes wasn't empty")
	}
}

func testDisabledRateSources(t *testing.T, d db.DB) {
	sources, err := d.DisabledRateSources()
	if err != nil {
		t.Fatalf("DisabledRateSources error on empty DB: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("unexpected disabled rate sources in fresh DB")
	}

	// The comma tests that source names are not mangled by the encoding.
	disabled := []string{"Fake News Network", "dcrdata", "Messari, Inc."}
	if err := d.SaveDisabledRateSources(disabled); err != nil {
		t.Fatalf("SaveDisabledRateSources error: %v", err)
	}
	reSources, err := d.DisabledRateSources()
	if err != nil {
		t.Fatalf("DisabledRateSources error: %v", err)
	}
	if len(reSources) != len(disabled) {
		t.Fatalf("expected %d disabled sources, got %d", len(disabled), len(reSources))
	}
	for i, src := range disabled {
		if reSources[i] != src {
			t.Fatalf("disabled source %d wrong, wanted %q, got %q", i, src, reSources[i])
		}
	}

	// Saving an empty slice clears the list.
	if err := d.SaveDisabledRateSources(nil); err != nil {
		t.Fatalf("SaveDisabledRateSources clear error: %v", err)
	}
	reSources, err = d.DisabledRateSources()
	if err != nil {
		t.Fatalf("DisabledRateSources error after clear: %v", err)
	}
	if len(reSources) != 0 {
		t.Fatalf("expected no disabled sources after clear, got %d", len(reSources))
	}
}

func testLanguage(t *testing.T, d db.DB) {
	lang, err := d.Language()
	if err != nil {
		t.Fatalf("Language error on empty DB: %v", err)
	}
	if lang != "" {
		t.Fatalf("unexpected language %q in fresh DB", lang)
	}

	for _, want := range []string{"en-US", "pt-BR"} {
		if err := d.SetLanguage(want); err != nil {
			t.Fatalf("SetLanguage(%q) error: %v", want, err)
		}
		lang, err = d.Language()
		if err != nil {
			t.Fatalf("Language error: %v", err)
		}
		if lang != want {
			t.Fatalf("language wrong, wanted %q, got %q", want, lang)
		}
	}
}

func testSendRequiresPassword(t *testing.T, d db.DB) {
	ensure := func(tag string, want bool) {
		t.Helper()
		require, err := d.SendRequiresPassword()
		if err != nil {
			t.Fatalf("%s: SendRequiresPassword error: %v", tag, err)
		}
		if require != want {
			t.Fatalf("%s: SendRequiresPassword = %t, wanted %t", tag, require, want)
		}
	}

	// Nothing stored reads back as false without an error.
	ensure("fresh DB", false)

	if err := d.SetSendRequiresPassword(true); err != nil {
		t.Fatalf("SetSendRequiresPassword(true) error: %v", err)
	}
	ensure("after set", true)

	if err := d.SetSendRequiresPassword(false); err != nil {
		t.Fatalf("SetSendRequiresPassword(false) error: %v", err)
	}
	ensure("after unset", false)
}

func testTokens(t *testing.T, d db.DB) {
	type tokenStore struct {
		name string
		set  func(string) error
		get  func() (string, error)
	}
	stores := []*tokenStore{
		{"companion", d.SetCompanionToken, d.CompanionToken},
		{"api", d.SetAPIToken, d.APIToken},
	}

	ensure := func(ts *tokenStore, tag, want string) {
		t.Helper()
		token, err := ts.get()
		if err != nil {
			t.Fatalf("%s token: %s: retrieval error: %v", ts.name, tag, err)
		}
		if token != want {
			t.Fatalf("%s token: %s: got %q, wanted %q", ts.name, tag, token, want)
		}
	}

	// Nothing stored reads back as an empty string without an error.
	for _, ts := range stores {
		ensure(ts, "fresh DB", "")
	}

	// The two tokens are stored independently.
	tokens := map[string]string{"companion": "companion-token-hash", "api": "api-token-hash"}
	for _, ts := range stores {
		if err := ts.set(tokens[ts.name]); err != nil {
			t.Fatalf("%s token: set error: %v", ts.name, err)
		}
	}
	for _, ts := range stores {
		ensure(ts, "after set", tokens[ts.name])
	}

	// Setting an empty value clears a token without touching the other.
	if err := stores[0].set(""); err != nil {
		t.Fatalf("%s token: clear error: %v", stores[0].name, err)
	}
	ensure(stores[0], "after clear", "")
	ensure(stores[1], "after clearing the other", tokens[stores[1].name])
}

func testMultisig(t *testing.T, d db.DB) {
	// NextMultisigKeyIndex counts up from zero, tracking each asset
	// separately. A repeated index means address reuse, so this is the one
	// method where a lost update matters.
	assetA, assetB := uint32(42), uint32(60)
	for _, wantIdx := range []uint32{0, 1, 2} {
		gotIdx, err := d.NextMultisigKeyIndex(assetA)
		if err != nil {
			t.Fatalf("NextMultisigKeyIndex error: %v", err)
		}
		if gotIdx != wantIdx {
			t.Fatalf("NextMultisigKeyIndex = %d, wanted %d", gotIdx, wantIdx)
		}
	}
	gotIdx, err := d.NextMultisigKeyIndex(assetB)
	if err != nil {
		t.Fatalf("NextMultisigKeyIndex error for second asset: %v", err)
	}
	if gotIdx != 0 {
		t.Fatalf("NextMultisigKeyIndex for second asset = %d, wanted 0", gotIdx)
	}

	// Concurrent callers must never see a repeated index. NextMultisigKeyIndex
	// is a read-modify-write with no cross-engine transaction to hide behind,
	// so the backend must serialize it; a lost update here is address reuse.
	// Under -race this also polices the data path.
	const numConcurrent = 32
	assetC := uint32(2)
	idxs := make([]uint32, numConcurrent)
	var wg sync.WaitGroup
	for i := range numConcurrent {
		wg.Go(func() {
			idx, err := d.NextMultisigKeyIndex(assetC)
			if err != nil {
				t.Errorf("concurrent NextMultisigKeyIndex error: %v", err)
				return
			}
			idxs[i] = idx
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	seen := make(map[uint32]bool, numConcurrent)
	for _, idx := range idxs {
		if seen[idx] {
			t.Fatalf("concurrent NextMultisigKeyIndex repeated index %d", idx)
		}
		if idx >= numConcurrent {
			t.Fatalf("concurrent NextMultisigKeyIndex returned out-of-range index %d", idx)
		}
		seen[idx] = true
	}

	// An unstored pubkey is an error.
	pubkey := [33]byte{0x02, 0xab}
	if _, err := d.MultisigIndexForPubkey(assetA, pubkey); err == nil {
		t.Fatalf("no error for unstored pubkey")
	}

	ensureIdx := func(tag string, assetID, want uint32, pubkey [33]byte) {
		t.Helper()
		gotIdx, err := d.MultisigIndexForPubkey(assetID, pubkey)
		if err != nil {
			t.Fatalf("%s: MultisigIndexForPubkey error: %v", tag, err)
		}
		if gotIdx != want {
			t.Fatalf("%s: index = %d, wanted %d", tag, gotIdx, want)
		}
	}

	if err := d.StoreMultisigIndexForPubkey(assetA, 5, pubkey); err != nil {
		t.Fatalf("StoreMultisigIndexForPubkey error: %v", err)
	}
	ensureIdx("roundtrip", assetA, 5, pubkey)

	// Same pubkey under another asset is stored separately.
	if _, err := d.MultisigIndexForPubkey(assetB, pubkey); err == nil {
		t.Fatalf("no error for pubkey stored under a different asset")
	}
	if err := d.StoreMultisigIndexForPubkey(assetB, 7, pubkey); err != nil {
		t.Fatalf("StoreMultisigIndexForPubkey error for second asset: %v", err)
	}
	ensureIdx("second asset", assetB, 7, pubkey)
	ensureIdx("first asset unchanged", assetA, 5, pubkey)

	// Storing again overwrites.
	if err := d.StoreMultisigIndexForPubkey(assetA, 9, pubkey); err != nil {
		t.Fatalf("StoreMultisigIndexForPubkey overwrite error: %v", err)
	}
	ensureIdx("overwrite", assetA, 9, pubkey)
}

func nTimes(n int, f func(int)) {
	for i := range n {
		f(i)
	}
}
