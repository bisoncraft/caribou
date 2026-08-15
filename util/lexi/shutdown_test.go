// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/dgraph-io/badger/v4"
)

// newConnectedTestDB builds a DB whose lifetime is bound to a context, and
// returns the cancel func and the WaitGroup Connect hands back. It does not use
// newTestDB, whose shutdown func is a no-op because those tests never Connect.
func newConnectedTestDB(t *testing.T) (*DB, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	db, err := New(&Config{
		Path: filepath.Join(t.TempDir(), "test.db"),
		Log:  util.StdOutLogger("T", util.LevelInfo),
	})
	if err != nil {
		t.Fatalf("error constructing db: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wg, err := db.Connect(ctx)
	if err != nil {
		t.Fatalf("error connecting db: %v", err)
	}
	return db, cancel, wg
}

// TestUpdateAfterShutdown asserts that a write arriving after shutdown is
// rejected with ErrDBClosed rather than reaching a closed Badger.
//
// The write must fail loudly. A caller that gets a nil error from Update has
// been told its data is committed, so silently dropping a write raced against
// shutdown would surface later as missing data with nothing pointing at the
// shutdown that caused it.
func TestUpdateAfterShutdown(t *testing.T) {
	db, cancel, wg := newConnectedTestDB(t)

	tbl, err := db.table("t")
	if err != nil {
		t.Fatalf("error creating table: %v", err)
	}
	if err := tbl.Set([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("error setting before shutdown: %v", err)
	}

	cancel()
	wg.Wait()

	if err := tbl.Set([]byte("k2"), []byte("v2")); !errors.Is(err, ErrDBClosed) {
		t.Fatalf("Set after shutdown returned %v, want ErrDBClosed", err)
	}
	if err := db.Update(func(*badger.Txn) error { return nil }); !errors.Is(err, ErrDBClosed) {
		t.Fatalf("Update after shutdown returned %v, want ErrDBClosed", err)
	}
}

// TestConcurrentUpdatesDrainOnShutdown runs writers alongside a shutdown and
// asserts every write either committed or was refused with ErrDBClosed. A
// write that gets partway into Badger as it closes would fail with something
// else, which is the outcome the close-time drain exists to prevent.
//
// Run with -race, this also covers the ordering: the shutdown path must not
// observe the closed flag independently of the in-flight writes it is waiting
// on.
func TestConcurrentUpdatesDrainOnShutdown(t *testing.T) {
	db, cancel, wg := newConnectedTestDB(t)

	tbl, err := db.table("t")
	if err != nil {
		t.Fatalf("error creating table: %v", err)
	}

	const writers = 8
	errs := make(chan error, writers)
	var writersWG sync.WaitGroup
	for i := 0; i < writers; i++ {
		writersWG.Add(1)
		go func(i int) {
			defer writersWG.Done()
			for j := 0; j < 20; j++ {
				k := []byte{byte(i), byte(j)}
				if err := tbl.Set(k, []byte("v"), kvstore.WithReplace()); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}

	cancel()
	writersWG.Wait()
	wg.Wait()
	close(errs)

	for err := range errs {
		if !errors.Is(err, ErrDBClosed) {
			t.Fatalf("write during shutdown failed with %v, want nil or ErrDBClosed", err)
		}
	}
}

// TestNestedUpdateDuringShutdown asserts that a write nested inside another
// write does not wedge when a shutdown arrives between them.
//
// Update registers an in-flight write rather than holding a lock across it. If
// it held a read lock instead, this would deadlock: Go's RWMutex blocks new
// readers once a writer is waiting, so the shutdown's acquisition would park
// the inner Update behind the outer one, which cannot release until the inner
// returns. Nothing would ever complete, and the shutdown WaitGroup that
// callers block on would never fire - the process would hang on exit rather
// than fail.
//
// Nesting is a real pattern here: an index function runs inside the
// transaction and may itself write (see TestSetDatumIsPerAttempt).
func TestNestedUpdateDuringShutdown(t *testing.T) {
	db, cancel, wg := newConnectedTestDB(t)

	tbl, err := db.table("t")
	if err != nil {
		t.Fatalf("error creating table: %v", err)
	}

	inOuter := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- db.Update(func(*badger.Txn) error {
			close(inOuter)
			<-release
			return tbl.Set([]byte("k"), []byte("v"), kvstore.WithReplace())
		})
	}()

	<-inOuter
	cancel()
	// Give the shutdown goroutine time to reach the drain before the nested
	// write starts, which is the ordering that used to wedge.
	time.Sleep(200 * time.Millisecond)
	close(release)

	select {
	case err := <-done:
		// Either outcome is fine: the nested write may commit, or it may be
		// refused because shutdown got there first. It must not hang, and a
		// refusal must roll the outer transaction back rather than half-apply.
		if err != nil && !errors.Is(err, ErrDBClosed) {
			t.Fatalf("outer Update returned %v, want nil or ErrDBClosed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nested Update deadlocked against shutdown")
	}

	wg.Wait() // must not hang either
}
