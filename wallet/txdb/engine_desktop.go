// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !mobile

package txdb

import "github.com/bisoncraft/bosswallet/util"

// newStore opens the desktop transaction store, backed by Badger via util/lexi.
func newStore(path string, log util.Logger) (Store, error) {
	return newLexiStore(path, log)
}
