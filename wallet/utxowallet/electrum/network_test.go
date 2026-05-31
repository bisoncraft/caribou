// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"context"
	"testing"
	"time"
)

// TestNetworkWaitForShutdownAfterFailedStart is the regression for H2:
// calling WaitForShutdown after Start fails must not deadlock. Previously
// the done channel was only closed by the watchNode goroutine, which never
// ran when Start returned early.
//
// We trigger a Start failure by pointing at a TCP port that's almost
// certainly not listening (port 1 is privileged and not in use locally).
func TestNetworkWaitForShutdownAfterFailedStart(t *testing.T) {
	net, err := NewNetwork(&Config{
		Chain:         testChainConfig(),
		TrustedServer: &ServerAddr{Proto: "tcp", Addr: "127.0.0.1:1"},
		DataDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := net.Start(ctx); err == nil {
		t.Fatal("expected Start to fail when no server is listening")
	}

	// WaitForShutdown must return promptly even though the start failed
	// and no goroutines were spawned.
	done := make(chan struct{})
	go func() {
		net.WaitForShutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForShutdown deadlocked after failed Start")
	}
}

// TestNetworkStopWithoutStart is a tighter version of the H2 regression: a
// caller may Stop and WaitForShutdown a Network that was never started at
// all. Should be a no-op rather than a deadlock.
func TestNetworkStopWithoutStart(t *testing.T) {
	net, err := NewNetwork(&Config{
		Chain:         testChainConfig(),
		TrustedServer: &ServerAddr{Proto: "tcp", Addr: "127.0.0.1:1"},
		DataDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	net.Stop()
	done := make(chan struct{})
	go func() {
		net.WaitForShutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForShutdown deadlocked after Stop-without-Start")
	}
}
