// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package keystore

// syncDir is a no-op on Windows. FlushFileBuffers on a directory handle
// returns ERROR_ACCESS_DENIED, so directory fsync isn't available there;
// the preceding atomic rename is still durable.
func syncDir(_ string) error {
	return nil
}
