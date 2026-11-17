// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !zec || !cgo

package zeclib

// openBackend reports that the Rust backend is not compiled in.
func openBackend(string, Network, []byte) (backend, error) {
	return nil, ErrNotBuilt
}

// transparentReceiver reports that the Rust backend is not compiled in.
func transparentReceiver(Network, string) (string, error) {
	return "", ErrNotBuilt
}
