// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
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

// TestCatchUpFromExactFork: a reorg rewinds exactly to the fork point — only
// the orphaned blocks are disconnected, never a blind window's worth — and
// the orphans stay resolvable by hash so a consumer's rollback can look up
// each one's parent.
func TestCatchUpFromExactFork(t *testing.T) {
	const forkAt, lenA, lenB = 27, 30, 35
	a, b := forkedChain(forkAt, lenA, lenB)

	h, _ := newTestHeaders(t)
	if _, err := h.AppendBatch(0, serializeBatch(t, a), 0); err != nil {
		t.Fatalf("load chain A: %v", err)
	}
	n := &Network{
		cfg:     &Config{Chain: testChainConfig(), HeaderBatchSize: 5},
		headers: h,
	}
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
	// Heights 27..29 were orphaned; nothing below the fork moves.
	wantDisc := []int32{29, 28, 27}
	if len(disc) != len(wantDisc) {
		t.Fatalf("disconnected %d blocks, want exactly the %d orphans", len(disc), len(wantDisc))
	}
	for i, want := range wantDisc {
		if disc[i].Height != want || disc[i].Hash != a[want].BlockHash() {
			t.Fatalf("disc[%d] = height %d hash %x, want A's block %d", i,
				disc[i].Height, disc[i].Hash, want)
		}
	}
	if got := h.byHeight[forkAt-1].BlockHash(); got != a[forkAt-1].BlockHash() {
		t.Fatalf("header below the fork replaced")
	}
	if got := h.byHeight[forkAt].BlockHash(); got != b[forkAt].BlockHash() {
		t.Fatalf("header at fork height %d = %s, want B's %s", forkAt, got, b[forkAt].BlockHash())
	}
	// A rollback consumer resolves each orphan's parent by hash; for all but
	// the deepest orphan that parent is itself an orphan.
	for _, d := range disc[:len(disc)-1] {
		parent := a[d.Height-1].BlockHash()
		if height, _, ok := h.ByHash(&parent); !ok || height != d.Height-1 {
			t.Fatalf("orphan parent at %d not resolvable (ok=%v, height=%d)",
				d.Height-1, ok, height)
		}
	}
}

// TestCatchUpFromDeepReorg is the regression for review #6: a reorg deeper than
// a single reorgRewindDepth window must self-heal rather than stall. We load
// chain A, then drive catchUpFrom against a fork that diverges 9 blocks below
// our tip (deeper than one 8-block search window) and verify the loop searches
// window by window, adopts the new chain, and reports exactly the orphaned
// blocks newest-first.
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
	// The reorg was 10 blocks deep (tip 29 down to the first divergent
	// block at 20). The first 8-block window (22..29) shares nothing with
	// B and rewinds whole; the second (14..21) matches at 19, so the loop
	// rewinds exactly two more. Only the orphans are disconnected.
	if len(disc) != lenA-forkAt {
		t.Fatalf("disconnected count = %d, want %d", len(disc), lenA-forkAt)
	}
	if disc[0].Height != lenA-1 {
		t.Fatalf("first disconnected height = %d, want %d (newest-first)", disc[0].Height, lenA-1)
	}
	if disc[len(disc)-1].Height != forkAt {
		t.Fatalf("last disconnected height = %d, want the fork height %d", disc[len(disc)-1].Height, forkAt)
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

// TestCatchUpFromNoFork: a server that serves the store's own tip header and
// then a next header that does not link to it is faulty, not reorged. The
// catch-up must fail without rewinding a single canonical block; a blind
// one-block step here disconnected canonical blocks until the reorg budget
// was spent, on every retry.
func TestCatchUpFromNoFork(t *testing.T) {
	a := synthChain(20, 1)
	h, _ := newTestHeaders(t)
	if _, err := h.AppendBatch(0, serializeBatch(t, a), 0); err != nil {
		t.Fatalf("load chain A: %v", err)
	}
	n := &Network{
		cfg:     &Config{Chain: testChainConfig(), HeaderBatchSize: 5},
		headers: h,
	}
	// Serves A faithfully below and at the tip, then a header at 20 whose
	// parent is nothing we have.
	orphan := &wire.BlockHeader{PrevBlock: chainhash.Hash{31: 9}, Bits: 0x207fffff}
	mineHeader(orphan)
	served := append(append([]*wire.BlockHeader(nil), a...), orphan)
	fetch := func(start, count int32) ([]byte, int, error) {
		end := min(int(start)+int(count), len(served))
		if int(start) >= end {
			return nil, 0, nil
		}
		return serializeBatch(t, served[start:end]), end - int(start), nil
	}

	disc, err := n.catchUpFrom(20, fetch)
	if !errors.Is(err, errForkNotFound) {
		t.Fatalf("catchUpFrom = %v, want errForkNotFound", err)
	}
	if len(disc) != 0 {
		t.Fatalf("disconnected %d canonical blocks chasing a fork that does not exist", len(disc))
	}
	if h.Tip() != 19 || h.byHeight[19].BlockHash() != a[19].BlockHash() {
		t.Fatalf("store moved: tip %d", h.Tip())
	}
}
