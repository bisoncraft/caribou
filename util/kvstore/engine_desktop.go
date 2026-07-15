// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !mobile

package kvstore

import "github.com/bisoncraft/caribou/util"

// New opens the desktop store at path, backed by Badger via util/lexi. The
// path names a directory.
func New(path string, log util.Logger) (Store, error) {
	return newLexiStore(path, log)
}
