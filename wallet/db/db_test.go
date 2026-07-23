// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// The conformance suite drivers live in package db_test because dbtest
// imports package db. The engine under test is selected at build time: see
// db_lexi_test.go and db_glossi_test.go.

package db_test

import (
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/db"
)

var tLogger = util.StdOutLogger("db_TEST", util.LevelTrace)

// newTestDB constructs a DB in a test temp dir, registering Close with
// t.Cleanup.
func newTestDB(t *testing.T) db.DB {
	t.Helper()
	d, err := db.New(filepath.Join(t.TempDir(), "coredb"), tLogger)
	if err != nil {
		t.Fatalf("error creating DB: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close error: %v", err)
		}
	})
	return d
}
