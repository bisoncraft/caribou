// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !windows

package keystore

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// syncDir fsyncs a directory so a preceding rename's directory-entry update
// is durable across a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening key file directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("syncing key file directory: %w", err)
	}
	return nil
}
