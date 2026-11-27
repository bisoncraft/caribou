// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

// pauseAt makes the next call of method on b wait. reached is closed when
// the call is waiting, and closing resume lets it go on.
func pauseAt(b *fakeBackend, method string) (reached, resume chan struct{}) {
	reached, resume = make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.set(func(b *fakeBackend) {
		b.hook = func(m string) {
			if m != method {
				return
			}
			once.Do(func() {
				close(reached)
				<-resume
			})
		}
	})
	return reached, resume
}

// waitClosed waits for ch to close.
func waitClosed(t *testing.T, what string, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// stillRunning fails the test if done is closed within a short wait: a call
// that has to wait for another has not returned.
func stillRunning(t *testing.T, what string, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s did not wait", what)
	case <-time.After(50 * time.Millisecond):
	}
}

// A Close, or a displacement by another instance, that comes between
// Unlock's check of the seed and its publication waits for the
// publication, and then erases the seed: a closed instance is never left
// unlocked.
func TestCloseDuringUnlock(t *testing.T) {
	for _, displace := range []bool{false, true} {
		name := "close"
		if displace {
			name = "displacement"
		}
		t.Run(name, func(t *testing.T) {
			e := openEnv(t)
			reached, resume := pauseAt(e.o.latest(), "IsSeedRelevant")
			unlocked := make(chan error, 1)
			go func() { unlocked <- e.w.Unlock(testPW) }()
			waitClosed(t, "Unlock to check the seed", reached)

			closed := make(chan struct{})
			go func() {
				defer close(closed)
				if displace {
					other := e.newInstance()
					t.Cleanup(func() { other.Close() })
					if err := other.OpenWithPW(context.Background(), testPW); err != nil {
						t.Errorf("OpenWithPW of the other instance: %v", err)
					}
				} else if err := e.w.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()
			stillRunning(t, "the close", closed)
			close(resume)
			if err := <-unlocked; err != nil {
				t.Fatalf("Unlock: %v", err)
			}
			waitClosed(t, "the close", closed)
			if e.w.IsOpen() || !e.w.Locked() {
				t.Errorf("after the close: open %t, locked %t", e.w.IsOpen(), e.w.Locked())
			}
		})
	}
}

// The same for NewAddress: the closed instance holds no address.
func TestCloseDuringNewAddress(t *testing.T) {
	for _, displace := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "displacement"}[displace], func(t *testing.T) {
			e := openEnv(t)
			be := e.o.latest()
			// The current address has received, so NewAddress derives one.
			be.set(func(b *fakeBackend) {
				b.newAddress = "ufake-new"
				b.history = []zeclib.TxRecord{{Seq: 1, Outputs: []zeclib.TxOutput{
					{Pool: zeclib.OutputIronwood, Address: testReceiver, ToWallet: true},
				}}}
			})
			reached, resume := pauseAt(be, "NewAddress")
			made := make(chan error, 1)
			go func() {
				_, err := e.w.NewAddress()
				made <- err
			}()
			waitClosed(t, "NewAddress to run", reached)
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				if displace {
					other := e.newInstance()
					t.Cleanup(func() { other.Close() })
					if err := other.OpenWithPW(context.Background(), testPW); err != nil {
						t.Errorf("OpenWithPW of the other instance: %v", err)
					}
				} else if err := e.w.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()
			stillRunning(t, "the close", closed)
			close(resume)
			if err := <-made; err != nil {
				t.Fatalf("NewAddress: %v", err)
			}
			waitClosed(t, "the close", closed)
			if a, err := e.w.DepositAddress(); err == nil {
				t.Errorf("the closed instance holds the address %q", a)
			}
			if owned, err := e.w.OwnsDepositAddress("ufake-new"); err == nil && owned {
				t.Error("the closed instance answers from its cache")
			}
		})
	}
}

// A refresh in flight when the instance is displaced finishes or fails as
// closed, and the displaced instance's Connect WaitGroup completes.
func TestDisplacementStopsTheOldInstance(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg, err := e.w.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := e.o.latest()
	reached, resume := pauseAt(first, "Summary")
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		e.w.refresh()
	}()
	waitClosed(t, "the refresh to read", reached)

	other := e.newInstance()
	t.Cleanup(func() { other.Close() })
	opened := make(chan struct{})
	go func() {
		defer close(opened)
		if err := other.OpenWithPW(context.Background(), testPW); err != nil {
			t.Errorf("OpenWithPW: %v", err)
		}
	}()
	stillRunning(t, "the displacement", opened)
	close(resume)
	waitClosed(t, "the refresh", refreshed)
	waitClosed(t, "the displacement", opened)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	waitClosed(t, "the displaced instance's WaitGroup", done)
	if !first.isClosed() || e.w.IsOpen() {
		t.Error("the old instance is open")
	}
}

// The registry resolves symbolic links: an instance opened through another
// name of the data directory displaces the first.
func TestRegistryResolvesLinks(t *testing.T) {
	e := openEnv(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(e.dir, alias); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	w, err := newWallet(&asset.WalletConfig{Type: walletTypeNative, DataDir: alias}, nil, e.w.net)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	if w.dbPath != e.w.dbPath {
		t.Fatalf("two keys for one database: %s and %s", w.dbPath, e.w.dbPath)
	}
	if err := w.OpenWithPW(context.Background(), testPW); err != nil {
		t.Fatal(err)
	}
	if e.w.IsOpen() {
		t.Error("the instance of the other name was not displaced")
	}
}

// Balance notes reach core in the order of the refreshes that stored them.
func TestBalanceNotesInRefreshOrder(t *testing.T) {
	var c cache
	c.reset()
	var mtx sync.Mutex
	var reported []uint64
	emit := func(b *asset.Balance) {
		mtx.Lock()
		reported = append(reported, b.Available)
		mtx.Unlock()
	}
	// The later refresh stores first: the earlier one reports nothing.
	early, late := c.next(), c.next()
	c.storeBalance(late, asset.Balance{Available: 50}, emit)
	c.storeBalance(early, asset.Balance{Available: 100}, emit)
	if len(reported) != 1 || reported[0] != 50 {
		t.Fatalf("reported %v", reported)
	}

	c.reset()
	reported = nil
	var wg sync.WaitGroup
	for range 100 {
		seq := c.next()
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.storeBalance(seq, asset.Balance{Available: seq}, emit)
		}()
	}
	wg.Wait()
	for i := 1; i < len(reported); i++ {
		if reported[i] <= reported[i-1] {
			t.Fatalf("a balance was reported after a later one: %v", reported)
		}
	}
	if last := reported[len(reported)-1]; last != c.heldBalance().Available {
		t.Errorf("the last report %d is not the held balance %d", last, c.heldBalance().Available)
	}
}

// Send holds the guard of the backend while it estimates; Close waits for
// it holding the registry; Connect waits for the registry holding connMtx.
// Send reads the connection's context without connMtx, so all three finish.
func TestSendConnectCloseDoNotDeadlock(t *testing.T) {
	e := sendEnv(t)
	inEstimate, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.c.estimate = func(uint64) (*zeclib.TransferEstimate, error) {
		once.Do(func() {
			close(inEstimate)
			<-resume
		})
		return &zeclib.TransferEstimate{Fee: 10_000}, nil
	}
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		e.w.Send("ufake-other", 1_000, feeRate)
	}()
	waitClosed(t, "the send to estimate", inEstimate)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		e.w.Close()
	}()
	// Close is waiting for beMtx once a read lock cannot be taken: a
	// sync.RWMutex refuses new readers while a writer waits.
	eventually(t, "the close to wait for the send", func() bool {
		if e.w.beMtx.TryRLock() {
			e.w.beMtx.RUnlock()
			return false
		}
		return true
	})

	connected := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(connected)
		if wg, err := e.w.Connect(ctx); err == nil {
			t.Cleanup(func() { cancel(); wg.Wait() })
		}
	}()
	// Connect holds connMtx once it cannot be taken; it then waits for the
	// registry, which Close holds.
	eventually(t, "the connect to hold connMtx", func() bool {
		if e.w.connMtx.TryLock() {
			e.w.connMtx.Unlock()
			return false
		}
		return true
	})
	close(resume)

	deadline := time.After(60 * time.Second)
	for _, op := range []struct {
		name string
		done chan struct{}
	}{{"the send", sent}, {"the close", closed}, {"the connect", connected}} {
		select {
		case <-op.done:
		case <-deadline:
			// The deadlocked wallet would block the cleanups' Close.
			e.abandoned.Store(true)
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			fmt.Fprintf(os.Stderr, "%s: %s did not finish: deadlock\n%s\n", t.Name(), op.name, buf)
			panic(fmt.Sprintf("%s did not finish: deadlock", op.name))
		}
	}
}
