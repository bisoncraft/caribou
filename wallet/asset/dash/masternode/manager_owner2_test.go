// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import "testing"

func TestNeedsKeys(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	if need, err := m.NeedsKeys(); err != nil || !need {
		t.Fatalf("a new wallet: %v %v", need, err)
	}
	if err := m.PrimeOwnerKeys(); err != nil {
		t.Fatal(err)
	}
	if need, err := m.NeedsKeys(); err != nil || need {
		t.Fatalf("after priming: %v %v", need, err)
	}
	e.own(nil, 3)
	if need, err := m.NeedsKeys(); err != nil || !need {
		t.Fatalf("after the counter moved: %v %v", need, err)
	}
}

// A scan that read the store before PrimeOwnerKeys cached ids must not drop them
// from memory when it installs what it read.
func TestInstallKeepsCachedOwnerIds(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	old := e.snapshot()
	idx, id, err := m.ReserveOwner()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.install(old); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.OwnerIndex(id); !ok || got != idx {
		t.Fatalf("owner index after an install of an older snapshot: %d %v", got, ok)
	}
}
