// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package polygon

import "testing"

func TestDefaultBorMainnetGenesisBlock(t *testing.T) {
	g := DefaultBorMainnetGenesisBlock()
	if g.Config != BorMainnetChainConfig {
		t.Fatal("genesis Config should be BorMainnetChainConfig")
	}
	if len(g.Alloc) == 0 {
		t.Fatal("genesis Alloc is empty; allocs/bor_mainnet.json failed to load")
	}
}

func TestReadPreallocMissingFilePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected readPrealloc to panic for a missing allocs file")
		}
	}()
	readPrealloc("allocs/does-not-exist.json")
}
