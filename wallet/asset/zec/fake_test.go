// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

// fakeBackend is a scriptable backend. Its fields are read and written
// under mtx.
type fakeBackend struct {
	mtx sync.Mutex
	cfg *zeclib.Config

	closed   bool
	accounts []zeclib.Account
	// relevant is the seed IsSeedRelevant accepts.
	relevant []byte

	status    zeclib.SyncStatus
	summary   *zeclib.Summary
	spendable zeclib.SpendableValue

	address      string
	newAddress   string
	newAddrErr   error
	newAddrCalls int
	// historyErr fails Transactions.
	historyErr error
	owns       map[string]string
	// ownsErr fails the next OwnsAddress.
	ownsErr error

	heightForTime    uint32
	heightForTimeErr error
	birthdayAsked    []time.Time
	marginAsked      []time.Duration
	createParams     []zeclib.AccountParams
	createErr        error
	// heightCtx and createCtx are the contexts of the last HeightForTime
	// and CreateAccount.
	heightCtx, createCtx context.Context
	// createWait, if set, is closed when CreateAccount begins, which then
	// waits for its context to be done and returns the context's error.
	createWait chan struct{}

	history []zeclib.TxRecord
	pages   int

	// hook, if set, runs at the start of IsSeedRelevant, NewAddress and
	// Summary, with the method's name, outside the fake's lock.
	hook func(method string)

	// connected is set while a Connect runs, until its ctx is done or
	// Close.
	connected bool
	connects  int
	stopSync  context.CancelFunc
	syncWG    *sync.WaitGroup
}

var _ backend = (*fakeBackend)(nil)

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		accounts: []zeclib.Account{{ID: "acct-0", Birthday: 100, Derived: true, SeedFingerprint: []byte{0xfe}}},
		address:  testAddress,
		owns:     make(map[string]string),
	}
}

func (b *fakeBackend) set(f func(b *fakeBackend)) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	f(b)
}

// emitStatus sets the status and calls the wallet's status callback, as
// zeclib does from its sync goroutine.
func (b *fakeBackend) emitStatus(st zeclib.SyncStatus) {
	b.mtx.Lock()
	b.status = st
	cb := b.cfg.OnSyncStatus
	b.mtx.Unlock()
	if cb != nil {
		cb(st)
	}
}

// emitChange calls the wallet's change callback.
func (b *fakeBackend) emitChange() {
	b.mtx.Lock()
	cb := b.cfg.OnChange
	b.mtx.Unlock()
	if cb != nil {
		cb()
	}
}

// runHook runs the hook, if one is set.
func (b *fakeBackend) runHook(method string) {
	b.mtx.Lock()
	hook := b.hook
	b.mtx.Unlock()
	if hook != nil {
		hook(method)
	}
}

func (b *fakeBackend) isClosed() bool {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return b.closed
}

func (b *fakeBackend) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if b.closed {
		return nil, zeclib.ErrClosed
	}
	if b.connected {
		return nil, zeclib.ErrConnected
	}
	b.connected = true
	b.connects++
	ctx, cancel := context.WithCancel(ctx)
	b.stopSync = cancel
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		b.mtx.Lock()
		b.connected = false
		b.mtx.Unlock()
	}()
	b.syncWG = &wg
	return &wg, nil
}

func (b *fakeBackend) Close() error {
	b.mtx.Lock()
	b.closed = true
	stop, wg := b.stopSync, b.syncWG
	b.mtx.Unlock()
	if stop != nil {
		stop()
		wg.Wait()
	}
	return nil
}

func (b *fakeBackend) SyncStatus() zeclib.SyncStatus {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return b.status
}

func (b *fakeBackend) CreateAccount(ctx context.Context, p *zeclib.AccountParams) (*zeclib.Account, error) {
	b.mtx.Lock()
	b.createCtx = ctx
	wait := b.createWait
	b.mtx.Unlock()
	if wait != nil {
		close(wait)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	b.mtx.Lock()
	defer b.mtx.Unlock()
	params := *p
	params.Seed = bytes.Clone(p.Seed)
	b.createParams = append(b.createParams, params)
	if b.createErr != nil {
		return nil, b.createErr
	}
	a := b.accounts[0]
	return &a, nil
}

func (b *fakeBackend) Accounts() ([]zeclib.Account, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if b.closed {
		return nil, zeclib.ErrClosed
	}
	return append([]zeclib.Account(nil), b.accounts...), nil
}

func (b *fakeBackend) IsSeedRelevant(seed []byte) (bool, error) {
	b.runHook("IsSeedRelevant")
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return bytes.Equal(seed, b.relevant), nil
}

func (b *fakeBackend) HeightForTime(ctx context.Context, t time.Time, margin time.Duration) (uint32, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	b.heightCtx = ctx
	b.birthdayAsked = append(b.birthdayAsked, t)
	b.marginAsked = append(b.marginAsked, margin)
	return b.heightForTime, b.heightForTimeErr
}

func (b *fakeBackend) Address(string) (string, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return b.address, nil
}

func (b *fakeBackend) NewAddress(string, zeclib.ReceiverSet) (string, error) {
	b.runHook("NewAddress")
	b.mtx.Lock()
	defer b.mtx.Unlock()
	b.newAddrCalls++
	if b.newAddrErr != nil {
		return "", b.newAddrErr
	}
	b.address = b.newAddress
	return b.newAddress, nil
}

func (b *fakeBackend) OwnsAddress(address string) (string, bool, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if err := b.ownsErr; err != nil {
		b.ownsErr = nil
		return "", false, err
	}
	id, ok := b.owns[address]
	return id, ok, nil
}

func (b *fakeBackend) Summary() (*zeclib.Summary, error) {
	b.runHook("Summary")
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if b.closed {
		return nil, zeclib.ErrClosed
	}
	return b.summary, nil
}

func (b *fakeBackend) Spendable(string) (zeclib.SpendableValue, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	return b.spendable, nil
}

func (b *fakeBackend) EstimateTransfer(string, string, uint64, []byte) (*zeclib.TransferEstimate, error) {
	return nil, errors.New("not scripted")
}

func (b *fakeBackend) Send(context.Context, string, []byte, string, uint64, []byte, uint64) (*zeclib.SendResult, error) {
	return nil, errors.New("not scripted")
}

func (b *fakeBackend) EstimateSendMax(string, string, []byte) (*zeclib.SendMaxEstimate, error) {
	return nil, errors.New("not scripted")
}

func (b *fakeBackend) SendMax(context.Context, string, []byte, string, []byte, uint64, uint64) (*zeclib.SendMaxResult, error) {
	return nil, errors.New("not scripted")
}

func (b *fakeBackend) Transactions(_ string, afterSeq uint64, limit uint32) (*zeclib.TxListing, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if b.historyErr != nil {
		return nil, b.historyErr
	}
	b.pages++
	var out []zeclib.TxRecord
	for _, r := range b.history {
		if r.Seq > afterSeq && len(out) < int(limit) {
			out = append(out, r)
		}
	}
	return &zeclib.TxListing{ChainTip: b.status.ChainTip, Records: out}, nil
}

func (b *fakeBackend) Transaction(string, string) (*zeclib.TxRecord, uint32, error) {
	return nil, 0, nil
}

func (b *fakeBackend) OpenTransactions(string, uint32) (*zeclib.TxListing, error) {
	return &zeclib.TxListing{}, nil
}

// testAddress is the fake's deposit address, and testReceiver the address
// of its Ironwood receiver, which fakeParse knows.
const (
	testAddress  = "ufake-deposit"
	testReceiver = "ufake-deposit-orchard"
	testTAddress = "tfake-deposit"
)

// parsed is what fakeParse returns for each address it knows. An address it
// does not know fails with ErrInvalidArgument.
var parsed = map[string]*zeclib.ParsedAddress{
	testAddress: {
		Kind: zeclib.AddressUnified, Payable: true,
		Receivers: []zeclib.Receiver{
			{Kind: zeclib.ReceiverOrchard, Address: testReceiver},
			{Kind: zeclib.ReceiverTransparent, Address: testTAddress},
		},
	},
	testReceiver: {Kind: zeclib.AddressUnified, Payable: true,
		Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: testReceiver}}},
	testTAddress: {Kind: zeclib.AddressTransparent, Payable: true},
	"ufake-new": {Kind: zeclib.AddressUnified, Payable: true,
		Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: "ufake-new"}}},
	"ufake-other": {Kind: zeclib.AddressUnified, Payable: true,
		Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: "ufake-other"}}},
	"zfake-sapling": {Kind: zeclib.AddressSapling, Unpayable: "Sapling"},
}

func fakeParse(_ zeclib.Network, address string) (*zeclib.ParsedAddress, error) {
	if p, ok := parsed[address]; ok {
		c := *p
		return &c, nil
	}
	if strings.HasPrefix(address, "uexp") {
		return &zeclib.ParsedAddress{Kind: zeclib.AddressUnified, Payable: true}, nil
	}
	return nil, zeclib.ErrInvalidArgument
}

// opener replaces openBackend for a test. Each open of a path gets the
// backend registered for it, or a new one, and the seed the open was given
// is recorded. needSeed makes an open without a seed fail with
// ErrSeedRequired.
type opener struct {
	mtx      sync.Mutex
	backends map[string]*fakeBackend
	opens    []*fakeBackend
	seeds    [][]byte
	needSeed bool
	err      error
}

func installOpener(t *testing.T) *opener {
	t.Helper()
	o := &opener{backends: make(map[string]*fakeBackend)}
	prevOpen, prevParse := openBackend, parseAddress
	openBackend = o.open
	parseAddress = fakeParse
	t.Cleanup(func() {
		openBackend, parseAddress = prevOpen, prevParse
	})
	return o
}

func (o *opener) open(cfg *zeclib.Config, seed []byte) (backend, error) {
	o.mtx.Lock()
	defer o.mtx.Unlock()
	o.seeds = append(o.seeds, bytes.Clone(seed))
	if o.err != nil {
		return nil, o.err
	}
	if o.needSeed && seed == nil {
		return nil, zeclib.ErrSeedRequired
	}
	prev := o.backends[cfg.DBPath]
	b := newFakeBackend()
	if prev != nil {
		// The same database: what it holds carries over.
		prev.mtx.Lock()
		b.accounts, b.relevant, b.address = prev.accounts, prev.relevant, prev.address
		b.summary, b.spendable, b.status = prev.summary, prev.spendable, prev.status
		b.history, b.owns = prev.history, prev.owns
		prev.mtx.Unlock()
	}
	b.cfg = cfg
	o.backends[cfg.DBPath] = b
	o.opens = append(o.opens, b)
	return b, nil
}

// latest returns the backend of the last open.
func (o *opener) latest() *fakeBackend {
	o.mtx.Lock()
	defer o.mtx.Unlock()
	return o.opens[len(o.opens)-1]
}

// notes collects the wallet notifications.
type notes struct {
	ch chan asset.WalletNotification
}

func newNotes() *notes {
	return &notes{ch: make(chan asset.WalletNotification, 4096)}
}

// drain returns the notifications sent so far.
func (n *notes) drain() []asset.WalletNotification {
	var out []asset.WalletNotification
	for {
		select {
		case note := <-n.ch:
			out = append(out, note)
		default:
			return out
		}
	}
}

// logRecorder is a logger that keeps every line.
type logRecorder struct {
	mtx   sync.Mutex
	lines []string
}

func (r *logRecorder) Write(p []byte) (int, error) {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	r.lines = append(r.lines, string(p))
	return len(p), nil
}

func (r *logRecorder) count(substr string) int {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// testPW is a password core would give: at least 32 bytes.
var testPW = bytes.Repeat([]byte{7}, 32)

// testEntropy is the seed core gives the wallet, and testZip32 its ZIP 32
// seed: the BIP 39 seed of its phrase with an empty passphrase.
var (
	testEntropy = bytes.Repeat([]byte{0x7f}, 32)
	testZip32   = mustZip32(testEntropy)
)

func mustZip32(entropy []byte) []byte {
	s, err := zip32Seed(entropy)
	if err != nil {
		panic(err)
	}
	return s
}

// env is a wallet on the fake backend, with its data directory, emitter and
// peer reports.
type env struct {
	t     *testing.T
	dir   string
	o     *opener
	w     *zecWallet
	notes *notes
	log   *logRecorder

	peersMtx sync.Mutex
	peers    []uint32

	// abandoned is set by a test that found the wallet deadlocked: the
	// cleanups then leave it as it is instead of blocking on its Close.
	abandoned atomic.Bool
}

// newEnv creates a wallet in a new data directory and builds an instance
// for it, which opens nothing.
func newEnv(t *testing.T) *env {
	t.Helper()
	o := installOpener(t)
	e := &env{t: t, dir: t.TempDir(), o: o, notes: newNotes(), log: &logRecorder{}}
	if err := (&Driver{}).Create(t.Context(), &asset.CreateWalletParams{
		Type: walletTypeNative, Seed: testEntropy, Pass: testPW, DataDir: e.dir, Net: util.Simnet,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The backend of the database Create made knows the seed.
	o.latest().set(func(b *fakeBackend) { b.relevant = testZip32 })
	e.w = e.newInstance()
	t.Cleanup(func() {
		if e.abandoned.Load() {
			return
		}
		if err := e.w.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return e
}

// newInstance builds another instance for the same data directory.
func (e *env) newInstance() *zecWallet {
	e.t.Helper()
	w, err := newWallet(&asset.WalletConfig{
		Type:    walletTypeNative,
		DataDir: e.dir,
		Emit:    asset.NewWalletEmitter(e.notes.ch, BipID, util.Disabled),
		PeersChange: func(n uint32, _ error) {
			e.peersMtx.Lock()
			e.peers = append(e.peers, n)
			e.peersMtx.Unlock()
		},
	}, util.NewLogger("ZEC", util.LevelTrace, e.log), util.Simnet)
	if err != nil {
		e.t.Fatalf("newWallet: %v", err)
	}
	return w
}

// connect connects the wallet and returns a function that disconnects it
// and waits.
func (e *env) connect(w *zecWallet) (disconnect func()) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	wg, err := w.Connect(ctx)
	if err != nil {
		cancel()
		e.t.Fatalf("Connect: %v", err)
	}
	var once sync.Once
	disconnect = func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}
	e.t.Cleanup(disconnect)
	return disconnect
}

func (e *env) peerReports() []uint32 {
	e.peersMtx.Lock()
	defer e.peersMtx.Unlock()
	return append([]uint32(nil), e.peers...)
}

// eventually waits up to two seconds for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// testClock sets the package clock to a fixed time for the test, and returns
// a function that advances it.
func testClock(t *testing.T) (advance func(time.Duration)) {
	t.Helper()
	clock := time.Unix(1_800_000_000, 0)
	var mtx sync.Mutex
	prev := now
	now = func() time.Time {
		mtx.Lock()
		defer mtx.Unlock()
		return clock
	}
	t.Cleanup(func() { now = prev })
	return func(d time.Duration) {
		mtx.Lock()
		clock = clock.Add(d)
		mtx.Unlock()
	}
}

// countSynced drains the notes and counts the history-synced ones.
func (e *txEnv) countSynced() int {
	return syncedNotes(e.notes.drain())
}

// failListing makes the next full listing fail on a write and checks that
// it did, which starts the wait.
func (e *txEnv) failListing(t *testing.T) {
	t.Helper()
	e.faulty.fail(0, 1)
	e.project(projectStatus)
	if !e.w.proj.fullNeeded || e.w.proj.listRetryAt.IsZero() {
		t.Fatal("the listing did not fail")
	}
}
