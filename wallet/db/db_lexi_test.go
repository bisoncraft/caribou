// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !mobile

package db_test

import (
	"testing"

	dbtest "github.com/bisoncraft/caribou/wallet/db/test"
)

// TestLexiSuite runs the db.DB conformance suite against the desktop engine,
// Badger via util/lexi.
func TestLexiSuite(t *testing.T) {
	dbtest.RunSuite(t, newTestDB)
}
