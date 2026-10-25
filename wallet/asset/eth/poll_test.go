//go:build !harness && !rpclive

package eth

import (
	"context"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// tClock is a settable clock for pollSchedule.now.
type tClock struct {
	mtx sync.Mutex
	t   time.Time
}

func (c *tClock) now() time.Time {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	return c.t
}

func (c *tClock) advance(d time.Duration) {
	c.mtx.Lock()
	c.t = c.t.Add(d)
	c.mtx.Unlock()
}

func pendingPolls(s *pollSchedule) int {
	return len(s.wake)
}

func TestPollScheduleInterval(t *testing.T) {
	clock := &tClock{t: time.Unix(1_700_000_000, 0)}
	s := &pollSchedule{now: clock.now}

	if got := s.interval(); got != idlePollInterval {
		t.Fatalf("new schedule interval = %v, want idle %v", got, idlePollInterval)
	}

	if wasIdle := s.extend(); !wasIdle {
		t.Fatal("extend on a new schedule reported active, want idle")
	}
	if got := s.interval(); got != activePollInterval {
		t.Fatalf("interval after extend = %v, want active %v", got, activePollInterval)
	}

	clock.advance(activeHold - time.Second)
	if wasIdle := s.extend(); wasIdle {
		t.Fatal("extend within the hold reported idle, want active")
	}

	// The second extend restarted the hold, so the first one's deadline
	// passing does not end active mode.
	clock.advance(2 * time.Second)
	if got := s.interval(); got != activePollInterval {
		t.Fatalf("interval past the first hold = %v, want active %v", got, activePollInterval)
	}

	clock.advance(activeHold)
	if got := s.interval(); got != idlePollInterval {
		t.Fatalf("interval past the hold = %v, want idle %v", got, idlePollInterval)
	}
}

func TestPollScheduleOverrides(t *testing.T) {
	clock := &tClock{t: time.Unix(1_700_000_000, 0)}
	s := &pollSchedule{active: time.Second, idle: time.Minute, hold: 10 * time.Second, now: clock.now}
	if got := s.interval(); got != time.Minute {
		t.Fatalf("idle interval = %v, want 1m", got)
	}
	s.extend()
	if got := s.interval(); got != time.Second {
		t.Fatalf("active interval = %v, want 1s", got)
	}
	clock.advance(10 * time.Second)
	if got := s.interval(); got != time.Minute {
		t.Fatalf("interval at the end of the hold = %v, want idle 1m", got)
	}
}

func TestPollIntervals(t *testing.T) {
	for _, tt := range []struct {
		net              util.Network
		wantActive, want time.Duration
	}{
		{util.Mainnet, activePollInterval, idlePollInterval},
		{util.Testnet, activePollInterval, idlePollInterval},
		{util.Simnet, simnetActivePollInterval, simnetIdlePollInterval},
	} {
		active, idle := pollIntervals(tt.net)
		if active != tt.wantActive || idle != tt.want {
			t.Errorf("%s intervals = %v, %v, want %v, %v", tt.net, active, idle, tt.wantActive, tt.want)
		}
	}
}

func TestRequestPollCoalesces(t *testing.T) {
	s := &pollSchedule{wake: make(chan struct{}, 1)}
	s.requestPoll()
	s.requestPoll()
	if n := pendingPolls(s); n != 1 {
		t.Fatalf("pending polls after two requests = %d, want 1", n)
	}

	// The zero value has no wake channel. Requests must not block.
	var zero pollSchedule
	zero.requestPoll()
}

func TestAlertMethods(t *testing.T) {
	newWallet := func() *baseWallet {
		return &baseWallet{poll: pollSchedule{wake: make(chan struct{}, 1)}}
	}

	t.Run("Alert polls when idle and when active", func(t *testing.T) {
		w := newWallet()
		w.Alert()
		if n := pendingPolls(&w.poll); n != 1 {
			t.Fatalf("pending polls after Alert = %d, want 1", n)
		}
		if got := w.poll.interval(); got != activePollInterval {
			t.Fatalf("interval after Alert = %v, want active", got)
		}
		<-w.poll.wake
		w.Alert()
		if n := pendingPolls(&w.poll); n != 1 {
			t.Fatalf("pending polls after a second Alert = %d, want 1", n)
		}
	})

	t.Run("alert polls only from idle", func(t *testing.T) {
		w := newWallet()
		w.alert()
		if n := pendingPolls(&w.poll); n != 1 {
			t.Fatalf("pending polls after alert from idle = %d, want 1", n)
		}
		<-w.poll.wake
		w.alert()
		if n := pendingPolls(&w.poll); n != 0 {
			t.Fatalf("pending polls after alert while active = %d, want 0", n)
		}
		if got := w.poll.interval(); got != activePollInterval {
			t.Fatalf("interval after alert = %v, want active", got)
		}
	})
}

func TestAlertCalls(t *testing.T) {
	// The calls fail early, since the test wallet has no bridges and the
	// addresses are empty, but alert comes first.
	ctx := t.Context()
	calls := []struct {
		name string
		call func(w asset.Wallet)
	}{
		{"FeeRate", func(w asset.Wallet) { w.FeeRate() }},
		{"StandardSendFee", func(w asset.Wallet) { w.StandardSendFee(1) }},
		{"EstimateSendTxFee", func(w asset.Wallet) {
			w.(asset.TxFeeEstimator).EstimateSendTxFee("", 1, 0, false, false)
		}},
		{"Send", func(w asset.Wallet) { w.Send("", 1, 0) }},
		{"TakeAction", func(w asset.Wallet) { w.(asset.ActionTaker).TakeAction("", nil) }},
		{"ApproveBridgeContract", func(w asset.Wallet) {
			w.(asset.Bridger).ApproveBridgeContract(ctx, "", nil)
		}},
		{"UnapproveBridgeContract", func(w asset.Wallet) {
			w.(asset.Bridger).UnapproveBridgeContract(ctx, "", nil)
		}},
		{"InitiateBridge", func(w asset.Wallet) { w.(asset.Bridger).InitiateBridge(ctx, 1, 0, "") }},
		{"RecordIncomingTx", func(w asset.Wallet) {
			w.(asset.IncomingTxRecorder).RecordIncomingTx(ctx, "", 0)
		}},
	}
	for _, assetID := range []uint32{BipID, usdcEthID} {
		for _, tt := range calls {
			t.Run(util.BipIDSymbol(assetID)+"/"+tt.name, func(t *testing.T) {
				w, aw, node, cancel := tassetWallet(assetID)
				defer cancel()
				node.bal = new(big.Int)
				node.tokenContractor.bal = new(big.Int)
				aw.poll.wake = make(chan struct{}, 1)
				tt.call(w)
				if n := pendingPolls(&aw.poll); n != 1 {
					t.Fatalf("pending polls = %d, want 1", n)
				}
				if got := aw.poll.interval(); got != activePollInterval {
					t.Fatalf("interval = %v, want active", got)
				}
			})
		}
	}
}

// TestNotAlertCalls checks methods that core calls on every tip change. As
// alert calls, they would keep the wallet active permanently.
func TestNotAlertCalls(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func(w asset.Wallet) error
	}{
		{"Balance", func(w asset.Wallet) error { _, err := w.Balance(); return err }},
		{"SyncStatus", func(w asset.Wallet) error { _, err := w.SyncStatus(); return err }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, aw, node, cancel := tassetWallet(BipID)
			defer cancel()
			node.bal = big.NewInt(1)
			aw.poll.wake = make(chan struct{}, 1)
			if err := tt.call(w); err != nil {
				t.Fatal(err)
			}
			if n := pendingPolls(&aw.poll); n != 0 {
				t.Fatalf("pending polls = %d, want 0", n)
			}
			if got := aw.poll.interval(); got != idlePollInterval {
				t.Fatalf("interval = %v, want idle", got)
			}
		})
	}
}

func TestHasPendingWork(t *testing.T) {
	newTx := func() *extendedWalletTx {
		return &extendedWalletTx{WalletTransaction: &txdb.WalletTransaction{}}
	}
	for _, tt := range []struct {
		name  string
		setup func(eth *ETHWallet)
		want  bool
	}{
		{"none", func(*ETHWallet) {}, false},
		{"pending tx", func(eth *ETHWallet) {
			eth.pendingTxs = []*extendedWalletTx{newTx()}
		}, true},
		{"tx awaiting user action", func(eth *ETHWallet) {
			tx := newTx()
			tx.actionRequested = true
			eth.pendingTxs = []*extendedWalletTx{tx}
		}, false},
		{"tx assumed lost", func(eth *ETHWallet) {
			tx := newTx()
			tx.AssumedLost = true
			eth.pendingTxs = []*extendedWalletTx{tx}
		}, false},
		{"one stuck and one pending tx", func(eth *ETHWallet) {
			stuck := newTx()
			stuck.actionRequested = true
			eth.pendingTxs = []*extendedWalletTx{stuck, newTx()}
		}, true},
		{"final tx behind a stuck tx", func(eth *ETHWallet) {
			// Pruning stops at the stuck tx, so the final one stays listed.
			stuck, final := newTx(), newTx()
			stuck.actionRequested = true
			final.Confirmed = true
			eth.pendingTxs = []*extendedWalletTx{stuck, final}
		}, false},
		// An approval or bridge holds the wallet active through its txs only,
		// so that one that never resolves cannot hold it indefinitely.
		{"pending approval", func(eth *ETHWallet) {
			eth.connected.Store(true)
			eth.pendingApprovals[common.Address{0x01}] = &pendingApproval{}
		}, false},
		{"pending bridge", func(eth *ETHWallet) {
			eth.bridgeManager = &bridgeManager{pendingBridges: map[string]*pendingBridge{"0x01": {}}}
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, _, _, cancel := tassetWallet(BipID)
			defer cancel()
			eth := w.(*ETHWallet)
			tt.setup(eth)
			if got := eth.hasPendingWork(); got != tt.want {
				t.Fatalf("hasPendingWork = %t, want %t", got, tt.want)
			}
			eth.holdIfPending()
			wantInterval := idlePollInterval
			if tt.want {
				wantInterval = activePollInterval
			}
			if got := eth.poll.interval(); got != wantInterval {
				t.Fatalf("interval after holdIfPending = %v, want %v", got, wantInterval)
			}
		})
	}
}

func TestCurrentNetworkFeesExpiry(t *testing.T) {
	w, _, node, cancel := tassetWallet(BipID)
	defer cancel()
	eth := w.(*ETHWallet)
	eth.currentTip = &types.Header{Number: big.NewInt(5)}
	// An active interval shorter than the default shows that the expiry
	// follows the schedule, as it must on simnet.
	eth.poll.active = time.Second

	node.baseFee = GweiToWei(10)
	base, _, err := eth.currentNetworkFees(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if base.Cmp(GweiToWei(10)) != 0 {
		t.Fatalf("base fee = %v, want 10 gwei", base)
	}

	// Same tip, fresh entry: cached.
	node.baseFee = GweiToWei(20)
	if base, _, _ = eth.currentNetworkFees(t.Context()); base.Cmp(GweiToWei(10)) != 0 {
		t.Fatalf("base fee for a fresh entry = %v, want cached 10 gwei", base)
	}

	// Same tip, entry older than the active poll interval: refetched. An idle
	// wallet's tip can be that old.
	eth.currentFees.stamp = time.Now().Add(-2 * time.Second)
	if base, _, _ = eth.currentNetworkFees(t.Context()); base.Cmp(GweiToWei(20)) != 0 {
		t.Fatalf("base fee for an expired entry = %v, want refetched 20 gwei", base)
	}
}

func TestSendL1FeeExpiry(t *testing.T) {
	w, _, node, cancel := tassetWallet(BipID)
	defer cancel()
	eth := w.(*ETHWallet)
	eth.isOpStack = true
	eth.currentTip = &types.Header{Number: big.NewInt(5)}
	eth.poll.active = time.Second

	node.l1Fee = GweiToWei(10)
	if fee := eth.sendL1Fee(); fee != 10 {
		t.Fatalf("L1 fee = %d, want 10 gwei", fee)
	}

	// Same tip, fresh entry: cached.
	node.l1Fee = GweiToWei(20)
	if fee := eth.sendL1Fee(); fee != 10 {
		t.Fatalf("L1 fee for a fresh entry = %d, want cached 10 gwei", fee)
	}

	// Same tip, entry older than the active poll interval: refetched.
	eth.l1Fees.sendStamp = time.Now().Add(-2 * time.Second)
	if fee := eth.sendL1Fee(); fee != 20 {
		t.Fatalf("L1 fee for an expired entry = %d, want refetched 20 gwei", fee)
	}
}

// tPollNode records bestHeader calls and counts provider probes.
type tPollNode struct {
	*testNode
	polls  chan struct{}
	probes atomic.Int32
	hdr    *types.Header
}

func (n *tPollNode) probeFailedProviders(context.Context) {
	n.probes.Add(1)
}

func (n *tPollNode) bestHeader(context.Context) (*types.Header, error) {
	n.polls <- struct{}{}
	return n.hdr, nil
}

// monitorTest runs monitorBlocks against a tPollNode, with intervals short
// enough to observe both modes and an idle interval long enough never to fire.
type monitorTest struct {
	t   *testing.T
	eth *ETHWallet
	pn  *tPollNode
}

const (
	monitorTestActive = 20 * time.Millisecond
	monitorTestHold   = 100 * time.Millisecond
	// monitorTestQuiet is how long to watch for a poll that must not happen.
	monitorTestQuiet = 150 * time.Millisecond
	// monitorTestSoon bounds the wait for a poll that must happen.
	monitorTestSoon = 5 * time.Second
)

func newMonitorTest(t *testing.T) *monitorTest {
	w, _, node, cancel := tassetWallet(BipID)
	t.Cleanup(cancel)
	eth := w.(*ETHWallet)
	pn := &tPollNode{
		testNode: node.testNode,
		polls:    make(chan struct{}, 1024),
		hdr:      &types.Header{Number: big.NewInt(1)},
	}
	eth.node = pn
	eth.poll = pollSchedule{
		active: monitorTestActive,
		idle:   time.Hour,
		hold:   monitorTestHold,
		wake:   make(chan struct{}, 1),
	}
	return &monitorTest{t: t, eth: eth, pn: pn}
}

// start runs monitorBlocks until the test ends.
func (m *monitorTest) start() {
	ctx, stop := context.WithCancel(m.t.Context())
	done := make(chan struct{})
	go func() {
		m.eth.monitorBlocks(ctx)
		close(done)
	}()
	m.t.Cleanup(func() {
		stop()
		<-done
		awaitPendingTxChecks(m.t, m.eth.baseWallet)
	})
}

func (m *monitorTest) expectPoll(what string) {
	m.t.Helper()
	select {
	case <-m.pn.polls:
	case <-time.After(monitorTestSoon):
		m.t.Fatalf("no poll %s", what)
	}
}

func (m *monitorTest) expectQuiet(what string) {
	m.t.Helper()
	select {
	case <-m.pn.polls:
		m.t.Fatalf("unexpected poll %s", what)
	case <-time.After(monitorTestQuiet):
	}
}

// drain discards the polls recorded so far.
func (m *monitorTest) drain() {
	for {
		select {
		case <-m.pn.polls:
		default:
			return
		}
	}
}

// settle waits out the hold with margin for a slow scheduler and discards
// the polls made meanwhile.
func (m *monitorTest) settle() {
	time.Sleep(3 * monitorTestHold)
	m.drain()
}

func (m *monitorTest) setPendingTx(pending bool) {
	m.eth.nonceMtx.Lock()
	defer m.eth.nonceMtx.Unlock()
	m.eth.pendingTxs = nil
	if pending {
		m.eth.pendingTxs = []*extendedWalletTx{{WalletTransaction: &txdb.WalletTransaction{}}}
	}
}

func TestMonitorBlocks(t *testing.T) {
	m := newMonitorTest(t)
	m.start()

	m.expectQuiet("while idle")

	m.eth.Alert()
	m.expectPoll("after Alert")
	// Each poll gives failed providers a chance to recover first.
	if m.pn.probes.Load() == 0 {
		t.Fatal("poll did not probe failed providers")
	}
	m.expectPoll("at the active interval")
	// The loop is serial, so the second poll's start means the first poll's
	// header has been applied.
	if got := m.eth.tipHeight(); got != 1 {
		t.Fatalf("tip after poll = %d, want 1", got)
	}
	m.expectPoll("at the active interval, again")

	// After the hold runs out the schedule drops to the hour-long idle
	// interval. At most one active poll can land after the deadline.
	m.settle()
	m.expectQuiet("after the hold expired")

	// An alert call from idle polls immediately.
	m.eth.alert()
	m.expectPoll("after alert from idle")
	m.settle()

	// Pending work holds the wallet active after the poll that finds it.
	m.setPendingTx(true)
	m.eth.Alert()
	m.expectPoll("after Alert with a pending tx")
	time.Sleep(2 * monitorTestHold)
	m.drain()
	m.expectPoll("past the hold with a pending tx")

	// Once the work clears, the wallet goes idle after one more hold.
	m.setPendingTx(false)
	m.settle()
	m.expectQuiet("after the pending tx cleared")
}

func TestMonitorBlocksStartsActiveWithPendingWork(t *testing.T) {
	m := newMonitorTest(t)
	// The polled header is already the tip, so polls do not run the pending
	// tx check on the placeholder tx.
	m.eth.currentTip = m.pn.hdr
	m.setPendingTx(true)
	m.start()
	m.expectPoll("at startup with a pending tx")
}
