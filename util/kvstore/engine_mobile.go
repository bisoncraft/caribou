// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build mobile

package kvstore

import "github.com/bisoncraft/caribou/util"

// New opens the mobile store at path, backed by bbolt via util/glossi. The
// path names a file.
func New(path string, log util.Logger) (Store, error) {
	return newGlossiStore(path, log)
}
