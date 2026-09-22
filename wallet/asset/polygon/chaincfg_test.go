// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package polygon

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSimnetDataDir(t *testing.T) {
	dir, err := simnetDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dir, filepath.Join("dextest", "polygon")) {
		t.Errorf("simnetDataDir = %q, want a path ending in dextest/polygon", dir)
	}
}
