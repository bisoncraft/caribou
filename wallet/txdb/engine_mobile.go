// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build mobile

package txdb

import "github.com/bisoncraft/bosswallet/util"

// newStore opens the mobile transaction store, backed by bbolt via
// util/glossi.
func newStore(path string, log util.Logger) (Store, error) {
	return newGlossiStore(path, log)
}
