// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

func TestOpenWithPW(t *testing.T) {
	e := newEnv(t)
	if e.w.IsOpen() {
		t.Fatal("open before OpenWithPW")
	}
	if err := e.w.OpenWithPW(context.Background(), testPW); err != nil {
		t.Fatalf("OpenWithPW: %v", err)
	}
	if !e.w.IsOpen() {
		t.Fatal("not open")
	}
	if seed := e.o.seeds[len(e.o.seeds)-1]; seed != nil {
		t.Errorf("opened with a seed it did not need: %x", seed)
	}
	if a, err := e.w.DepositAddress(); err != nil || a != testAddress {
		t.Errorf("DepositAddress: %q, %v", a, err)
	}
	opens := len(e.o.opens)
	if err := e.w.OpenWithPW(context.Background(), testPW); err != nil || len(e.o.opens) != opens {
		t.Errorf("a second OpenWithPW: %v, %d opens", err, len(e.o.opens)-opens)
	}
	if err := e.w.Close(); err != nil || e.w.IsOpen() {
		t.Fatalf("Close: %v, open %t", err, e.w.IsOpen())
	}
	if _, err := e.w.Balance(); !errors.Is(err, errClosed) {
		t.Errorf("Balance when closed: %v", err)
	}
	if _, err := e.w.DepositAddress(); !errors.Is(err, errClosed) {
		t.Errorf("DepositAddress when closed: %v", err)
	}
}

// A database that needs the seed for a migration is opened with the seed
// decrypted with the password.
func TestOpenWithPWForAMigration(t *testing.T) {
	e := newEnv(t)
	e.o.needSeed = true
	if err := e.w.OpenWithPW(context.Background(), bytes.Repeat([]byte{8}, 32)); err == nil {
		t.Fatal("opened with a wrong password")
	}
	if e.w.IsOpen() {
		t.Fatal("open after a wrong password")
	}
	if err := e.w.OpenWithPW(context.Background(), testPW); err != nil {
		t.Fatalf("OpenWithPW: %v", err)
	}
	if seed := e.o.seeds[len(e.o.seeds)-1]; !bytes.Equal(seed, testZip32) {
		t.Errorf("reopened with %x", seed)
	}
	// Connect cannot do it without the password.
	e.w.Close()
	if _, err := e.w.Connect(context.Background()); !errors.Is(err, zeclib.ErrSeedRequired) {
		t.Errorf("Connect of a database that needs the seed: %v", err)
	}
}

// A database whose account is not ZIP 32 account 0 is not used.
func TestOpenChecksTheAccount(t *testing.T) {
	e := newEnv(t)
	e.o.latest().set(func(b *fakeBackend) { b.accounts = append(b.accounts, b.accounts[0]) })
	if err := e.w.OpenWithPW(context.Background(), testPW); err == nil {
		t.Fatal("opened a database with two accounts")
	}
	if !e.o.latest().isClosed() || e.w.IsOpen() {
		t.Error("the refused database was left open")
	}
	e.o.latest().set(func(b *fakeBackend) {
		b.accounts = []zeclib.Account{{ID: "a", Derived: true, HDIndex: 1}}
	})
	if err := e.w.OpenWithPW(context.Background(), testPW); err == nil {
		t.Fatal("opened a database with account 1")
	}
}

func TestUnlockAndLock(t *testing.T) {
	e := newEnv(t)
	if !e.w.Locked() {
		t.Fatal("unlocked before Unlock")
	}
	if err := e.w.Unlock(bytes.Repeat([]byte{8}, 32)); err == nil {
		t.Fatal("unlocked with a wrong password")
	}
	// Unlock opens a closed database.
	if err := e.w.Unlock(testPW); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if e.w.Locked() || !e.w.IsOpen() {
		t.Fatalf("locked %t, open %t", e.w.Locked(), e.w.IsOpen())
	}
	seed, err := e.w.seedCopy()
	if err != nil || !bytes.Equal(seed, testZip32) {
		t.Fatalf("seedCopy: %x, %v", seed, err)
	}
	e.w.seedMtx.Lock()
	held := e.w.seed
	e.w.seedMtx.Unlock()
	if err := e.w.Lock(); err != nil || !e.w.Locked() {
		t.Fatalf("Lock: %v, locked %t", err, e.w.Locked())
	}
	if !bytes.Equal(held, make([]byte, len(held))) {
		t.Error("Lock did not erase the held seed")
	}
	if !bytes.Equal(seed, testZip32) {
		t.Error("Lock erased a send's copy")
	}
	if _, err := e.w.seedCopy(); !errors.Is(err, errLocked) {
		t.Errorf("seedCopy when locked: %v", err)
	}

	// A seed that is not the account's.
	e.o.latest().set(func(b *fakeBackend) { b.relevant = []byte{1} })
	if err := e.w.Unlock(testPW); err == nil || !e.w.Locked() {
		t.Errorf("unlocked with a seed that is not the account's: %v", err)
	}

	// Close erases the seed.
	e.o.latest().set(func(b *fakeBackend) { b.relevant = testZip32 })
	if err := e.w.Unlock(testPW); err != nil {
		t.Fatal(err)
	}
	e.w.Close()
	if !e.w.Locked() {
		t.Error("the seed survived Close")
	}
}

// Connect opens a closed database without the password, and the instance
// connects again after a pause (the context cancelled) and after a Close,
// as core's disable and enable do.
func TestConnectLifecycle(t *testing.T) {
	e := newEnv(t)
	disconnect := e.connect(e.w)
	if !e.w.IsOpen() {
		t.Fatal("Connect did not open the database")
	}
	be := e.o.latest()
	be.emitStatus(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 150})
	eventually(t, "a peer report", func() bool { return len(e.peerReports()) == 1 })
	disconnect()
	if !e.w.IsOpen() {
		t.Fatal("a pause closed the database")
	}
	be.set(func(b *fakeBackend) {
		if b.connected {
			t.Error("still syncing after the pause")
		}
	})

	// Resume.
	disconnect = e.connect(e.w)
	if n := len(e.o.opens); n != 2 { // Create's and this instance's
		t.Errorf("%d opens", n)
	}
	disconnect()

	// Disable, then enable: core closes the wallet while it is connected,
	// and connects it again. The Close stops the connection.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg, err := e.w.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.w.Close(); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()
	waitClosed(t, "the connection to stop on Close", stopped)
	if !be.isClosed() {
		t.Fatal("Close did not close the database")
	}
	e.connect(e.w)
	if !e.w.IsOpen() || e.o.latest() == be {
		t.Fatal("Connect after Close did not open the database again")
	}
}

// The context of the connection is kept for calls core gives no context.
func TestConnContext(t *testing.T) {
	e := newEnv(t)
	if e.w.connContext().Err() == nil {
		t.Error("a live context before Connect")
	}
	disconnect := e.connect(e.w)
	if e.w.connContext().Err() != nil {
		t.Error("the connection's context is done")
	}
	disconnect()
	if e.w.connContext().Err() == nil {
		t.Error("a live context after the pause")
	}
}

// One instance holds a database open. Another instance for the same data
// directory closes it through the first one's own close: the first reports
// closed and fails as closed, and its late Close leaves the second's
// database open. When the first connects again, as core's restartOnFail
// does after a failed reconfiguration, it displaces the second in turn.
func TestOneOpenInstancePerDatabase(t *testing.T) {
	e := newEnv(t)
	e.connect(e.w)
	first := e.o.latest()

	other := e.newInstance()
	t.Cleanup(func() { other.Close() })
	e.connect(other)
	second := e.o.latest()
	if !first.isClosed() || e.w.IsOpen() {
		t.Fatal("the first instance was not closed")
	}
	if !other.IsOpen() || second.isClosed() {
		t.Fatal("the second instance is not open")
	}
	if _, err := e.w.Balance(); !errors.Is(err, errClosed) {
		t.Errorf("Balance of the displaced instance: %v", err)
	}
	// Core disconnects the old instance later.
	if err := e.w.Close(); err != nil || second.isClosed() {
		t.Fatalf("the late Close of the old instance closed the new one's database: %v", err)
	}
	registry.Lock()
	holder := registry.open[e.w.dbPath]
	registry.Unlock()
	if holder != other {
		t.Fatal("the registry lost the open instance")
	}

	// restartOnFail: the old instance connects again.
	e.connect(e.w)
	if !second.isClosed() || other.IsOpen() || !e.w.IsOpen() {
		t.Fatal("reconnecting the old instance did not displace the new one")
	}
	open := 0
	for _, b := range e.o.opens {
		if !b.isClosed() {
			open++
		}
	}
	if open != 1 {
		t.Errorf("%d databases open", open)
	}
	if b, err := e.w.Balance(); err != nil || b == nil {
		t.Errorf("Balance after the reconnect: %v", err)
	}
}
