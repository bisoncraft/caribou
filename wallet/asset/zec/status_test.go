// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

func TestSyncStatusOf(t *testing.T) {
	half := zeclib.Ratio{Numerator: 1, Denominator: 2}
	for _, tc := range []struct {
		name string
		st   zeclib.SyncStatus
		want asset.SyncStatus
	}{
		{"before the server", zeclib.SyncStatus{State: zeclib.SyncConnecting},
			asset.SyncStatus{StartingBlocks: 100, Blocks: 100}},
		{"half way", zeclib.SyncStatus{State: zeclib.SyncSyncing, ChainTip: 300, ScanProgress: half},
			asset.SyncStatus{TargetHeight: 300, StartingBlocks: 100, Blocks: 200}},
		{"synced", zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 300, ScanProgress: half},
			asset.SyncStatus{Synced: true, TargetHeight: 300, StartingBlocks: 100, Blocks: 300}},
		// An error in the status of a synced wallet does not unsync it.
		{"synced with an unrecorded output", zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 300,
			Err: zeclib.ErrOutputUnrecorded},
			asset.SyncStatus{Synced: true, TargetHeight: 300, StartingBlocks: 100, Blocks: 300}},
		{"disconnected", zeclib.SyncStatus{State: zeclib.SyncDisconnected, ChainTip: 300,
			ScanProgress: zeclib.Ratio{Numerator: 1, Denominator: 1}, Err: errors.New("gone")},
			asset.SyncStatus{TargetHeight: 300, StartingBlocks: 100, Blocks: 300}},
	} {
		if got := syncStatusOf(tc.st, 100); *got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, *got, tc.want)
		}
	}
}

// The peer count follows zeclib's state, never its error, and is reported
// only once the sync has reached the server or lost it. TipChange is sent
// only while synced, once per tip. An unrecorded output is logged at warning
// level once each time it appears.
func TestStatusReports(t *testing.T) {
	e := newEnv(t)
	e.connect(e.w)
	be := e.o.latest()
	step := func(st zeclib.SyncStatus) {
		t.Helper()
		be.emitStatus(st)
		// The worker has handled a status once the cached tip is its tip.
		eventually(t, fmt.Sprintf("status %+v handled", st), func() bool {
			return e.w.cache.chainTip() == st.ChainTip
		})
	}
	tips := func() (out []uint64) {
		for _, note := range e.notes.drain() {
			if n, ok := note.(*asset.TipChangeNote); ok {
				out = append(out, n.Tip)
			}
		}
		return out
	}
	unrecorded := fmt.Errorf("%w: 1 of 1", zeclib.ErrOutputUnrecorded)
	const warning = "Transparent funds are missing from the balance"

	// The worker handles statuses in order, so a status has been handled
	// in full, its log lines and reports included, once the next one's tip
	// is cached. Each check comes a step after what it checks.
	// Stopped, as zeclib reports before its sync starts, and connecting
	// report no peer count.
	step(zeclib.SyncStatus{State: zeclib.SyncStopped, ChainTip: 0})
	step(zeclib.SyncStatus{State: zeclib.SyncConnecting, ChainTip: 1})
	step(zeclib.SyncStatus{State: zeclib.SyncSyncing, ChainTip: 150})
	step(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 151})
	step(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 152, Err: unrecorded})
	step(zeclib.SyncStatus{State: zeclib.SyncSyncing, ChainTip: 153, Err: unrecorded})
	step(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 154, Err: unrecorded})
	step(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 155})
	if n := e.log.count(warning); n != 1 {
		t.Errorf("the unrecorded output was logged %d times while it lasted", n)
	}
	step(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 156, Err: unrecorded})
	step(zeclib.SyncStatus{State: zeclib.SyncDisconnected, ChainTip: 157, Err: errors.New("unreachable")})
	if n := e.log.count(warning); n != 2 {
		t.Errorf("the unrecorded output was logged %d times after it came back", n)
	}
	step(zeclib.SyncStatus{State: zeclib.SyncSyncing, ChainTip: 158})
	step(zeclib.SyncStatus{State: zeclib.SyncSyncing, ChainTip: 159})

	if got, want := e.peerReports(), []uint32{1, 0, 1}; !slices.Equal(got, want) {
		t.Errorf("peer reports %v, want %v", got, want)
	}
	if got, want := tips(), []uint64{151, 152, 154, 155, 156}; !slices.Equal(got, want) {
		t.Errorf("tip changes %v, want %v", got, want)
	}
}

// Nothing is reported before Connect.
func TestNoReportsBeforeConnect(t *testing.T) {
	e := newEnv(t)
	if err := e.w.Unlock(testPW); err != nil {
		t.Fatal(err)
	}
	e.o.latest().emitStatus(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 150})
	if n := len(e.peerReports()); n != 0 {
		t.Errorf("%d peer reports before Connect", n)
	}
	ss, err := e.w.SyncStatus()
	if err != nil || !ss.Synced || ss.TargetHeight != 150 {
		t.Errorf("SyncStatus: %+v, %v", ss, err)
	}
	e.w.Close()
	if _, err := e.w.SyncStatus(); !errors.Is(err, errClosed) {
		t.Errorf("SyncStatus when closed: %v", err)
	}
}

// The peer count is reported under the guard of the backend: a close that
// comes while the report is made waits for it.
func TestPeersChangeUnderTheGuard(t *testing.T) {
	e := newEnv(t)
	inReport, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w, err := newWallet(&asset.WalletConfig{
		Type:    walletTypeNative,
		DataDir: e.dir,
		PeersChange: func(uint32, error) {
			once.Do(func() {
				close(inReport)
				<-resume
			})
		},
	}, util.Disabled, util.Simnet)
	if err != nil {
		t.Fatal(err)
	}
	e.connect(w)
	e.o.latest().emitStatus(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 150})
	waitClosed(t, "the peer report", inReport)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		w.Close()
	}()
	stillRunning(t, "the close", closed)
	close(resume)
	waitClosed(t, "the close", closed)
}
