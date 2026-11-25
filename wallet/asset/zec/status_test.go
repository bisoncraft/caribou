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
			TransparentErr: zeclib.ErrOutputUnrecorded},
			asset.SyncStatus{Synced: true, TargetHeight: 300, StartingBlocks: 100, Blocks: 300}},
		{"synced after a failed attempt", zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 300,
			Err: errors.New("the database is busy")},
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
// only while synced, once per tip.
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

	// Stopped, as zeclib reports before its sync starts, and connecting
	// report no peer count. An error of a failed attempt and an error of
	// the transparent addresses change neither the peers nor the tips.
	step(zeclib.SyncStatus{State: zeclib.SyncStopped, ChainTip: 0})
	step(zeclib.SyncStatus{State: zeclib.SyncConnecting, ChainTip: 1})
	step(zeclib.SyncStatus{State: zeclib.SyncSyncing, ChainTip: 150})
	step(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 151})
	step(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 152, TransparentErr: zeclib.ErrOutputUnrecorded})
	step(zeclib.SyncStatus{State: zeclib.SyncSyncing, ChainTip: 153, TransparentErr: zeclib.ErrOutputUnrecorded})
	step(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 154, Err: errors.New("the database is busy")})
	step(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 155})
	step(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 156, TransparentErr: zeclib.ErrServerFailure})
	step(zeclib.SyncStatus{State: zeclib.SyncDisconnected, ChainTip: 157, Err: errors.New("unreachable")})
	step(zeclib.SyncStatus{State: zeclib.SyncSyncing, ChainTip: 158})
	step(zeclib.SyncStatus{State: zeclib.SyncSyncing, ChainTip: 159})

	if got, want := e.peerReports(), []uint32{1, 0, 1}; !slices.Equal(got, want) {
		t.Errorf("peer reports %v, want %v", got, want)
	}
	if got, want := tips(), []uint64{151, 152, 154, 155, 156}; !slices.Equal(got, want) {
		t.Errorf("tip changes %v, want %v", got, want)
	}
}

// The error zeclib reports for the transparent addresses is the wallet's
// balance warning. Its code tells outputs that could not be recorded from
// requests that failed, and is that of the unrecorded outputs when both
// hold. Core is sent a note for every change, and the log has one line when
// a warning begins, changes its code, or ends.
func TestBalanceWarning(t *testing.T) {
	e := newEnv(t)
	disconnect := e.connect(e.w)
	be := e.o.latest()
	tip := uint32(200)
	state := zeclib.SyncSynced
	// step emits a status of the state with a transparent error and returns
	// the warning notes it caused. The worker handles statuses in order, so
	// a status has been handled in full, its log lines and notes included,
	// once the tip of the one after it is cached.
	step := func(transparentErr error) (out []*asset.BalanceWarning) {
		t.Helper()
		e.notes.drain()
		for _, err := range []error{transparentErr, transparentErr} {
			tip++
			be.emitStatus(zeclib.SyncStatus{State: state, ChainTip: tip, TransparentErr: err})
			eventually(t, fmt.Sprintf("the status at %d handled", tip), func() bool {
				return e.w.cache.chainTip() == tip
			})
		}
		for _, note := range e.notes.drain() {
			n, ok := note.(*asset.CustomWalletNote)
			if !ok || n.Route != asset.BalanceWarningRoute {
				continue
			}
			out = append(out, n.Payload.(*asset.BalanceWarning))
		}
		return out
	}
	const (
		missing = "Transparent funds are missing from the balance"
		stale   = "The transparent balance can be out of date"
		ended   = "up to date again"
	)
	logged := func() [3]int { return [3]int{e.log.count(missing), e.log.count(stale), e.log.count(ended)} }
	check := func(when string, notes []*asset.BalanceWarning, wantCode string, wantNotes int, wantLogged [3]int) {
		t.Helper()
		if len(notes) != wantNotes {
			t.Fatalf("%s: %d warning notes, want %d", when, len(notes), wantNotes)
		}
		held := e.w.BalanceWarning()
		if (held == nil) != (wantCode == "") || (held != nil && held.Code != wantCode) {
			t.Fatalf("%s: the warning held is %+v, want code %q", when, held, wantCode)
		}
		if wantNotes > 0 {
			last := notes[len(notes)-1]
			if (last == nil) != (held == nil) || (last != nil && *last != *held) {
				t.Fatalf("%s: the last note is %+v, the warning held %+v", when, last, held)
			}
		}
		if got := logged(); got != wantLogged {
			t.Fatalf("%s: log lines (missing, stale, ended) %v, want %v", when, got, wantLogged)
		}
	}

	check("no error", step(nil), "", 0, [3]int{})

	unrecorded := fmt.Errorf("%w: 1 of 1", zeclib.ErrOutputUnrecorded)
	check("an unrecorded output", step(unrecorded), BalanceWarningTransparentUnrecorded, 1, [3]int{1, 0, 0})
	if held := e.w.BalanceWarning(); held.Detail != "an unspent transparent output could not be recorded: 1 of 1" {
		t.Fatalf("the detail is %q", held.Detail)
	}
	// Another count of outputs changes the detail: a note and no log line.
	check("another detail", step(fmt.Errorf("%w: 2 of 2", zeclib.ErrOutputUnrecorded)),
		BalanceWarningTransparentUnrecorded, 1, [3]int{1, 0, 0})

	// Failed requests beside the unrecorded output keep its code.
	failure := fmt.Errorf("%w: the call failed", zeclib.ErrServerFailure)
	check("both", step(fmt.Errorf("%w; %w", failure, unrecorded)),
		BalanceWarningTransparentUnrecorded, 1, [3]int{1, 0, 0})
	check("failed requests alone", step(failure), BalanceWarningTransparentStale, 1, [3]int{1, 1, 0})
	check("no error again", step(nil), "", 1, [3]int{1, 1, 1})
	check("an unrecorded output again", step(unrecorded), BalanceWarningTransparentUnrecorded, 1, [3]int{2, 1, 1})

	// The status of a sync that stopped ends the warning with a note, and
	// without the log line of a warning that the server ended.
	state = zeclib.SyncStopped
	check("a stopped sync", step(nil), "", 1, [3]int{2, 1, 1})
	state = zeclib.SyncSynced
	check("an unrecorded output once more", step(unrecorded), BalanceWarningTransparentUnrecorded, 1, [3]int{3, 1, 1})

	// So does a connection that stops before that status is handled.
	e.notes.drain()
	disconnect()
	var after []*asset.BalanceWarning
	for _, note := range e.notes.drain() {
		if n, ok := note.(*asset.CustomWalletNote); ok && n.Route == asset.BalanceWarningRoute {
			after = append(after, n.Payload.(*asset.BalanceWarning))
		}
	}
	if len(after) != 1 || after[0] != nil || e.w.BalanceWarning() != nil {
		t.Fatalf("after the connection stopped: notes %v, warning %+v", after, e.w.BalanceWarning())
	}
	if got, want := logged(), [3]int{3, 1, 1}; got != want {
		t.Fatalf("after the connection stopped: log lines %v, want %v", got, want)
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
