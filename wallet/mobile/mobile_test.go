// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build mobile

package mobile

import (
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/core"
)

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

// TestPauseResumeNotRunning covers a pause grace or a foreground callback that
// lands after teardown or before startup finished. Pause reports success (an
// unbound app has nothing connected, matching Lock's idempotence) and Resume
// errors so the shell routes through a full Unlock.
func TestPauseResumeNotRunning(t *testing.T) {
	a := &App{}
	if err := a.Pause(); err != nil {
		t.Fatalf("Pause on an unbound app should be a no-op, got %v", err)
	}
	if err := a.Resume(); err == nil {
		t.Fatal("Resume on an unbound app should error")
	}
	// Repeat: a release mispaired with its acquire would drive lifecycleWG
	// negative and panic here rather than during a shutdown on a device.
	if err := a.Pause(); err != nil {
		t.Fatalf("second Pause: %v", err)
	}
	if err := a.Resume(); err == nil {
		t.Fatal("second Resume on an unbound app should error")
	}
}

// TestAcquireCoreDrains covers the registration Stop relies on to know a Pause
// or Resume is still in flight. Those calls deliberately do not hold a.mtx, so
// without this Stop could cancel the core while a reconnect was still running
// and start a sync monitor on the WaitGroup Core.Run is waiting on.
func TestAcquireCoreDrains(t *testing.T) {
	a := &App{}

	// Unbound: nothing registered, and the release is still safe to call.
	if c, release := a.acquireCore(); c != nil {
		t.Fatal("acquireCore returned a core for an unbound app")
	} else {
		release()
	}

	// A bare Core is enough: this exercises the bookkeeping around the call, not
	// the call itself, so no core method is ever invoked on it.
	a.core = new(core.Core)
	c, release := a.acquireCore()
	if c == nil {
		t.Fatal("acquireCore returned no core for a bound app")
	}

	drained := make(chan struct{})
	go func() {
		a.lifecycleWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("lifecycleWG drained while a call was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycleWG did not drain after release")
	}
}
