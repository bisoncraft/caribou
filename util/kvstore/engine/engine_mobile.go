// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build mobile

package engine

import (
	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/glossi"
	"github.com/bisoncraft/caribou/util/kvstore"
)

// New opens the mobile store at path, backed by bbolt via util/glossi. The
// path names a file.
func New(path string, log util.Logger) (kvstore.Store, error) {
	db, err := glossi.New(path, log)
	if err != nil {
		return nil, err
	}
	return db, nil
}
