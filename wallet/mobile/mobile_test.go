// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build mobile

package mobile

import "testing"

// TestLockIdempotentWhenNotRunning covers the case where an eviction alarm (or
// any lock trigger) fires after the core has been torn down or before it
// finished starting. Lock must report success rather than a "not running"
// error, since an unbound app is already effectively locked.
func TestLockIdempotentWhenNotRunning(t *testing.T) {
	a := &App{}
	if err := a.Lock(); err != nil {
		t.Fatalf("Lock on an unbound app should be a no-op, got %v", err)
	}
}

// TestUnlockNotRunning confirms Unlock refuses to restore a session when the app
// is not running, rather than pretending to succeed.
func TestUnlockNotRunning(t *testing.T) {
	a := &App{}
	url, err := a.Unlock("pw")
	if err == nil {
		t.Fatal("Unlock on an unbound app should error")
	}
	if url != "" {
		t.Fatalf("Unlock returned a url on failure: %q", url)
	}
}

// TestVerifyPasswordNotRunning confirms VerifyPassword errors when there is no
// core to check against.
func TestVerifyPasswordNotRunning(t *testing.T) {
	a := &App{}
	if err := a.VerifyPassword("pw"); err == nil {
		t.Fatal("VerifyPassword on an unbound app should error")
	}
}
