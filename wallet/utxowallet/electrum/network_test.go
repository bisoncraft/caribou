// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
)

// forkedChain returns two header chains that share the first `forkAt` headers
// and diverge after: `a` has `lenA` total, `b` has `lenB` total. Both are
// mined against the regtest PowLimit so they pass the per-header PoW check.
func forkedChain(forkAt, lenA, lenB int) (a, b []*wire.BlockHeader) {
	a = synthChain(lenA, 1000)
	b = make([]*wire.BlockHeader, lenB)
	copy(b, a[:forkAt]) // shared prefix
	prev := a[forkAt-1].BlockHash()
	ts := a[forkAt-1].Timestamp
	for i := forkAt; i < lenB; i++ {
		h := &wire.BlockHeader{
			Version:   1,
			PrevBlock: prev,
			Timestamp: ts.Add(time.Duration(i-forkAt+1) * time.Minute),
			Bits:      0x207fffff,
			Nonce:     9_000_000 + uint32(i), // distinct seed → distinct hashes
		}
		mineHeader(h)
		b[i] = h
		prev = h.BlockHash()
	}
	return a, b
}

// TestCatchUpFromDeepReorg is the regression for review #6: a reorg deeper than
// a single reorgRewindDepth step must self-heal rather than stall. We load
// chain A, then drive catchUpFrom against a fork that diverges 9 blocks below
// our tip (deeper than one 8-block rewind) and verify the loop rewinds in
// steps, adopts the new chain, and reports the disconnected blocks newest-first.
func TestCatchUpFromDeepReorg(t *testing.T) {
	const forkAt, lenA, lenB = 20, 30, 35
	a, b := forkedChain(forkAt, lenA, lenB)

	h, _ := newTestHeaders(t)
	if _, err := h.AppendBatch(0, serializeBatch(t, a), 0); err != nil {
		t.Fatalf("load chain A: %v", err)
	}
	if h.Tip() != lenA-1 {
		t.Fatalf("tip after load = %d, want %d", h.Tip(), lenA-1)
	}

	n := &Network{
		cfg:     &Config{Chain: testChainConfig(), HeaderBatchSize: 5},
		headers: h,
	}
	// fetch serves the forked chain B for any requested range.
	fetch := func(start, count int32) ([]byte, int, error) {
		end := min(int(start)+int(count), len(b))
		if int(start) >= end {
			return nil, 0, nil
		}
		return serializeBatch(t, b[start:end]), end - int(start), nil
	}

	target := int32(lenB - 1)
	disc, err := n.catchUpFrom(target, fetch)
	if err != nil {
		t.Fatalf("catchUpFrom: %v", err)
	}
	if h.Tip() != target {
		t.Fatalf("tip after reorg = %d, want %d", h.Tip(), target)
	}
	// The adopted chain must be B past the fork.
	if got := h.byHeight[forkAt].BlockHash(); got != b[forkAt].BlockHash() {
		t.Fatalf("header at fork height %d = %s, want B's %s", forkAt, got, b[forkAt].BlockHash())
	}
	// Reorg was 9 blocks deep (tip 29 down to fork 20), so two 8-block
	// rewind steps (16 blocks) were taken: heights 29..14 disconnected.
	if len(disc) != 16 {
		t.Fatalf("disconnected count = %d, want 16", len(disc))
	}
	if disc[0].Height != lenA-1 {
		t.Fatalf("first disconnected height = %d, want %d (newest-first)", disc[0].Height, lenA-1)
	}
	if disc[len(disc)-1].Height != lenA-1-15 {
		t.Fatalf("last disconnected height = %d, want %d", disc[len(disc)-1].Height, lenA-1-15)
	}
	// The disconnected blocks must be the original chain A's blocks.
	if disc[0].Hash != a[lenA-1].BlockHash() {
		t.Fatalf("first disconnected hash is not A's tip")
	}
}

// TestCatchUpFromReorgExceedsBound verifies a reorg deeper than maxReorgDepth
// is surfaced as an error rather than rewinding without bound.
func TestCatchUpFromReorgExceedsBound(t *testing.T) {
	// Fork point far below tip: lenA-1 - forkAt > maxReorgDepth.
	forkAt := 5
	lenA := maxReorgDepth + 50
	lenB := lenA + 5
	a, b := forkedChain(forkAt, lenA, lenB)

	h, _ := newTestHeaders(t)
	if _, err := h.AppendBatch(0, serializeBatch(t, a), 0); err != nil {
		t.Fatalf("load chain A: %v", err)
	}

	n := &Network{
		cfg:     &Config{Chain: testChainConfig(), HeaderBatchSize: 16},
		headers: h,
	}
	fetch := func(start, count int32) ([]byte, int, error) {
		end := min(int(start)+int(count), len(b))
		if int(start) >= end {
			return nil, 0, nil
		}
		return serializeBatch(t, b[start:end]), end - int(start), nil
	}

	disc, err := n.catchUpFrom(int32(lenB-1), fetch)
	if err == nil {
		t.Fatal("expected error for reorg deeper than maxReorgDepth")
	}
	// Even on the unrecoverable error, the blocks already truncated must be
	// returned so the caller (applyTipNotification) can publish the rollback
	// rather than leaving the store and the wallet view diverged (review #1).
	if len(disc) == 0 {
		t.Fatal("expected non-empty disconnects returned alongside the error")
	}
	// The reported disconnects must match the blocks actually removed: the
	// tip dropped from lenA-1 by len(disc).
	if h.Tip() != int32(lenA-1)-int32(len(disc)) {
		t.Fatalf("tip %d inconsistent with %d disconnects from %d", h.Tip(), len(disc), lenA-1)
	}
}

// TestApplyTipNotificationBlocksUntilDelivered is the regression for reviews
// #2/#5: tip changes are never dropped. When the tip-change channel is full,
// applyTipNotification applies backpressure (blocks) until the consumer drains
// it, then advances lastDelivered. Nothing is stranded.
func TestApplyTipNotificationBlocksUntilDelivered(t *testing.T) {
	chain := synthChain(6, 2000) // heights 0..5
	h, _ := newTestHeaders(t)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain[:4]), 0); err != nil { // load 0..3
		t.Fatalf("load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := &Network{
		cfg:           &Config{Chain: testChainConfig(), HeaderBatchSize: 16},
		headers:       h,
		tipChangeChan: make(chan *TipChange, 1),
		ctx:           ctx,
	}
	hdrHex := func(i int) string { return hex.EncodeToString(serializeBatch(t, chain[i:i+1])) }

	// Pre-fill the channel so the Connected[4] delivery must block.
	n.tipChangeChan <- &TipChange{}

	lastDelivered := int32(3)
	done := make(chan struct{})
	go func() {
		n.applyTipNotification(&HeadersTip{Height: 4, Hex: hdrHex(4)}, &lastDelivered)
		close(done)
	}()

	// It must block on the full channel rather than dropping and returning.
	select {
	case <-done:
		t.Fatal("applyTipNotification returned without delivering (event dropped?)")
	case <-time.After(50 * time.Millisecond):
	}

	// The header is appended before delivery, so the tip already advanced.
	if h.Tip() != 4 {
		t.Fatalf("tip = %d, want 4", h.Tip())
	}

	// Drain the placeholder; the blocked Connected[4] then lands.
	<-n.tipChangeChan
	select {
	case tc := <-n.tipChangeChan:
		if len(tc.Connected) != 1 || tc.Connected[0] != 4 {
			t.Fatalf("Connected = %v, want [4]", tc.Connected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connected[4] never delivered")
	}
	<-done
	if lastDelivered != 4 {
		t.Fatalf("lastDelivered = %d, want 4 after delivery", lastDelivered)
	}
}

// TestApplyTipNotificationShutdownEscape verifies that a delivery blocked on a
// full channel unblocks on context cancellation and does NOT advance
// lastDelivered (so a restart re-emits from the cursor).
func TestApplyTipNotificationShutdownEscape(t *testing.T) {
	chain := synthChain(6, 2000)
	h, _ := newTestHeaders(t)
	if _, err := h.AppendBatch(0, serializeBatch(t, chain[:4]), 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Network{
		cfg:           &Config{Chain: testChainConfig(), HeaderBatchSize: 16},
		headers:       h,
		tipChangeChan: make(chan *TipChange, 1),
		ctx:           ctx,
	}
	hdrHex := func(i int) string { return hex.EncodeToString(serializeBatch(t, chain[i:i+1])) }
	n.tipChangeChan <- &TipChange{} // fill

	lastDelivered := int32(3)
	done := make(chan struct{})
	go func() {
		n.applyTipNotification(&HeadersTip{Height: 4, Hex: hdrHex(4)}, &lastDelivered)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // let it block on the send
	cancel()                          // shutdown
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("applyTipNotification did not unblock on ctx cancel")
	}
	if lastDelivered != 3 {
		t.Fatalf("lastDelivered = %d, want 3 (no advance on shutdown)", lastDelivered)
	}
}

// TestNetworkStopNotStartedKeepsSenderChannelsOpenWhenLaunched is the
// white-box regression for F1: once the goroutines are launched, the !started
// Stop path (reached when watchNode calls n.Stop() after an external Stop
// flipped started=false) must NOT close the sender channels. watchNode's
// wg.Wait()-gated close is the sole closer; closing here could panic a parked
// headersLoop/scripthashLoop send.
func TestNetworkStopNotStartedKeepsSenderChannelsOpenWhenLaunched(t *testing.T) {
	n := &Network{
		tipChangeChan: make(chan *TipChange, 1),
		scripthashCh:  make(chan *ScripthashStatus, 1),
		done:          make(chan struct{}),
	}
	n.launched = true // goroutines were launched
	n.started = false // ...then transitioned to stopped

	n.Stop() // !started path

	assertOpen := func(name string, closed bool) {
		if closed {
			t.Fatalf("%s was closed by the !started Stop path while launched", name)
		}
	}
	select {
	case _, ok := <-n.tipChangeChan:
		assertOpen("tipChangeChan", !ok)
		t.Fatal("unexpected value on tipChangeChan")
	default:
	}
	select {
	case _, ok := <-n.scripthashCh:
		assertOpen("scripthashCh", !ok)
		t.Fatal("unexpected value on scripthashCh")
	default:
	}
	// A send must still succeed — the channel is open.
	select {
	case n.tipChangeChan <- &TipChange{}:
	default:
		t.Fatal("could not send on an open tipChangeChan")
	}
}

// TestNetworkStopRaceParkedSenders is the F1 stress regression: with senders
// parked on the blocking channel sends, a concurrent Stop (cancel) plus the
// wg.Wait()-gated close must tear down without a send-on-closed panic. Run
// under -race.
func TestNetworkStopRaceParkedSenders(t *testing.T) {
	for range 300 {
		n := &Network{
			cfg:           &Config{Chain: testChainConfig()},
			tipChangeChan: make(chan *TipChange, 1),
			scripthashCh:  make(chan *ScripthashStatus, 1),
			done:          make(chan struct{}),
		}
		n.ctx, n.cancel = context.WithCancel(context.Background())
		n.started, n.launched = true, true

		// Two senders that park on the blocking sends (nothing drains them).
		n.wg.Add(2)
		go func() {
			defer n.wg.Done()
			for n.deliverTipChange(&TipChange{Connected: []int32{1}}) {
			}
		}()
		go func() {
			defer n.wg.Done()
			for {
				select {
				case n.scripthashCh <- &ScripthashStatus{}:
				case <-n.ctx.Done():
					return
				}
			}
		}()

		// Stand-in for watchNode's child goroutine: the sole, wg.Wait()-gated
		// closer the fix relies on.
		go func() {
			n.wg.Wait()
			n.doneClosed.Do(func() {
				close(n.tipChangeChan)
				close(n.scripthashCh)
				close(n.done)
			})
		}()

		n.Stop() // started path: cancel() (nil node)
		// A second Stop now hits the !started branch; with launched=true it
		// must be a no-op, never racing the close against the senders.
		go n.Stop()

		<-n.done // no panic ⇒ pass
	}
}

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
