// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package util

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// drainConnector is a Connector whose goroutine keeps running until its context
// is cancelled, then does a little post-cancel work before its WaitGroup
// returns. drained is set true only after that work completes, modeling an
// appserver goroutine that still references shared resources (e.g. a logger)
// right up until it exits.
type drainConnector struct {
	drained atomic.Bool
}

func (c *drainConnector) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		// Simulate post-cancel teardown that touches shared state. If
		// Disconnect/Wait returned before this finished, a caller that then
		// tore down those resources would race this goroutine.
		time.Sleep(20 * time.Millisecond)
		c.drained.Store(true)
	}()
	return &wg, nil
}

// TestConnectionMasterDisconnectDrains proves the invariant the mobile Start()
// error path relies on: Disconnect() does not return until every connector
// goroutine has exited. That is what lets closeLog() run immediately after
// srvCM.Disconnect() without racing a live appserver goroutine.
func TestConnectionMasterDisconnectDrains(t *testing.T) {
	c := &drainConnector{}
	cm := NewConnectionMaster(c)
	if err := cm.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	cm.Disconnect()

	if !c.drained.Load() {
		t.Fatal("Disconnect returned before the connector goroutine finished draining")
	}
	if cm.On() {
		t.Fatal("ConnectionMaster still On after Disconnect")
	}
}

// TestConnectionMasterDisconnectViaParentCancel covers the Start() error path
// shape directly: the parent context is cancelled (as cancel() does on core
// death) and then Disconnect() is called. Disconnect must still block until the
// goroutine has fully drained.
func TestConnectionMasterDisconnectViaParentCancel(t *testing.T) {
	c := &drainConnector{}
	cm := NewConnectionMaster(c)
	ctx, cancel := context.WithCancel(context.Background())
	if err := cm.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	cancel() // mirrors the cancel() before srvCM.Disconnect() in Start()
	cm.Disconnect()

	if !c.drained.Load() {
		t.Fatal("Disconnect returned before the connector goroutine finished draining")
	}
}

// TestConnectionMasterWaitDrains is the same guarantee through Wait(), which is
// what Disconnect() uses internally.
func TestConnectionMasterWaitDrains(t *testing.T) {
	c := &drainConnector{}
	cm := NewConnectionMaster(c)
	ctx, cancel := context.WithCancel(context.Background())
	if err := cm.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	cancel()
	cm.Wait()

	if !c.drained.Load() {
		t.Fatal("Wait returned before the connector goroutine finished draining")
	}
}
