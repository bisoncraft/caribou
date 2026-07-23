package bolt

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/db"
	dbtest "github.com/bisoncraft/caribou/wallet/db/test"
)

var tLogger = util.StdOutLogger("db_TEST", util.LevelTrace)

// newTestDB constructs a running BoltDB in a test temp dir, registering
// shutdown with t.Cleanup.
func newTestDB(t *testing.T) db.DB {
	t.Helper()
	dbi, err := NewDB(filepath.Join(t.TempDir(), "db.db"), tLogger)
	if err != nil {
		t.Fatalf("error creating DB: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		dbi.Run(ctx)
	})
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return dbi
}

// TestSuite runs the backend-agnostic db.DB conformance suite against BoltDB.
func TestSuite(t *testing.T) {
	dbtest.RunSuite(t, newTestDB)
}
