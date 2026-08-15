// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !mobile

package engine

import (
	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/bisoncraft/caribou/util/lexi"
)

// New opens the desktop store at path, backed by Badger via util/lexi. The
// path names a directory.
func New(path string, log util.Logger) (kvstore.Store, error) {
	return lexi.NewStore(path, log)
}
