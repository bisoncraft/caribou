// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

const testRecipient = "tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm"

// newSendEnv returns an environment with the fake backend, an account, and a
// fee of 10,000 for every send.
func newSendEnv(t *testing.T) (*env, *fakeBackend) {
	t.Helper()
	e := newEnv(t, backendKinds[0], 20, nil)
	e.createAccount(10)
	be := e.be.(*fakeBackend)
	be.set(func(b *fakeBackend) { b.sendFee = 10_000 })
	return e, be
}

func (e *env) send() (*SendResult, error) {
	return e.w.Send(context.Background(), e.account.ID, testSeed, testRecipient, 100_000, nil, 20_000)
}

func TestSendBroadcasts(t *testing.T) {
	e, be := newSendEnv(t)
	res, err := e.send()
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.BroadcastErr != nil {
		t.Fatalf("broadcast: %v", res.BroadcastErr)
	}
	created := be.createdTxs()
	if len(created) != 1 || len(res.TxIDs) != 1 || res.TxIDs[0] != displayTxID(created[0].txid) || res.Fee != 10_000 ||
		res.PaidAddress != testRecipient || res.PaidPool != PaidTransparent {
		t.Fatalf("result %+v for created %+v", res, created)
	}
	if sent := e.srv.sentTxs(); len(sent) != 1 || !bytes.Equal(sent[0], created[0].raw) {
		t.Fatalf("the server received %d transactions", len(sent))
	}
}

// Once the backend has created the transaction, Send reports it whatever the
// broadcast returns.
func TestSendIsPendingWhateverTheBroadcast(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *env, be *fakeBackend)
		check func(t *testing.T, err error)
	}{{
		name: "server unreachable",
		setup: func(e *env, _ *fakeBackend) {
			e.srv.set(func(s *fakeLwd) { s.fail["SendTransaction"] = status.Error(codes.Unavailable, "down") })
		},
		check: func(t *testing.T, err error) {
			if !errors.Is(err, ErrServerUnreachable) || errors.Is(err, ErrBroadcastRejected) {
				t.Errorf("not an unreachable server: %v", err)
			}
		},
	}, {
		name: "rejected in the reply",
		setup: func(e *env, _ *fakeBackend) {
			e.srv.set(func(s *fakeLwd) {
				s.reject = &walletrpc.SendResponse{ErrorCode: -26, ErrorMessage: "bad-txns-inputs-spent"}
			})
		},
		check: func(t *testing.T, err error) {
			var rejected *BroadcastRejectedError
			if !errors.As(err, &rejected) || rejected.Code != -26 || rejected.Message != "bad-txns-inputs-spent" {
				t.Errorf("not the rejection: %v", err)
			}
		},
	}, {
		name: "rejected with a status",
		setup: func(e *env, _ *fakeBackend) {
			e.srv.set(func(s *fakeLwd) {
				s.fail["SendTransaction"] = status.Error(codes.InvalidArgument, "transaction is invalid")
			})
		},
		check: func(t *testing.T, err error) {
			var rejected *BroadcastRejectedError
			if !errors.As(err, &rejected) || rejected.Message != "transaction is invalid" || isConnectionError(err) {
				t.Errorf("not the rejection: %v", err)
			}
		},
	}, {
		name: "server failure",
		setup: func(e *env, _ *fakeBackend) {
			e.srv.set(func(s *fakeLwd) { s.fail["SendTransaction"] = status.Error(codes.Internal, "oops") })
		},
		check: func(t *testing.T, err error) {
			if !errors.Is(err, ErrServerFailure) || errors.Is(err, ErrBroadcastRejected) || errors.Is(err, ErrServerUnreachable) {
				t.Errorf("not a server failure: %v", err)
			}
		},
	}, {
		name: "raw transaction missing",
		setup: func(_ *env, be *fakeBackend) {
			be.set(func(b *fakeBackend) { b.unreadable = true })
		},
		check: func(t *testing.T, err error) {
			if !errors.Is(err, ErrInternal) {
				t.Errorf("not an internal error: %v", err)
			}
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, be := newSendEnv(t)
			tt.setup(e, be)
			res, err := e.send()
			if err != nil {
				t.Fatalf("Send returned an error for a created transaction: %v", err)
			}
			if len(res.TxIDs) != 1 || res.BroadcastErr == nil {
				t.Fatalf("result: %+v", res)
			}
			tt.check(t, res.BroadcastErr)
			inv, _ := be.rebroadcastInventory()
			if len(inv) != 1 || displayTxID(inv[0].txid) != res.TxIDs[0] {
				t.Errorf("the transaction is not pending: %+v", inv)
			}
		})
	}
}

func TestSendCreatesNothingOnError(t *testing.T) {
	e, be := newSendEnv(t)
	_, err := e.w.Send(context.Background(), e.account.ID, testSeed, testRecipient, 100_000, nil, 9_999)
	var tooHigh *FeeTooHighError
	if !errors.As(err, &tooHigh) || !errors.Is(err, ErrFeeTooHigh) || tooHigh.Fee != 10_000 || tooHigh.MaxFee != 9_999 {
		t.Errorf("fee over the limit: %v", err)
	}
	be.set(func(b *fakeBackend) {
		b.failOnce["send"] = &InsufficientFundsError{Available: 1, Required: 2}
	})
	if _, err := e.send(); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("insufficient funds: %v", err)
	}
	if _, err := e.w.Send(context.Background(), "unknown", testSeed, testRecipient, 1, nil, 20_000); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("unknown account: %v", err)
	}
	if n := len(be.createdTxs()); n != 0 {
		t.Errorf("%d transactions created", n)
	}
	if n := e.srv.callCount("SendTransaction"); n != 0 {
		t.Errorf("%d broadcasts", n)
	}
}

func TestEstimateAndSpendable(t *testing.T) {
	e, be := newSendEnv(t)
	be.set(func(b *fakeBackend) { b.spendableValue = SpendableValue{Ironwood: 5, Transparent: 6} })
	est, err := e.w.EstimateTransfer(e.account.ID, testRecipient, 1, nil)
	if err != nil || *est != (TransferEstimate{Fee: 10_000, PaidAddress: testRecipient, PaidPool: PaidTransparent}) {
		t.Errorf("EstimateTransfer: %+v, %v", est, err)
	}
	v, err := e.w.Spendable(e.account.ID)
	if err != nil || v != (SpendableValue{Ironwood: 5, Transparent: 6}) {
		t.Errorf("Spendable: %+v, %v", v, err)
	}
}

func TestErrorTypes(t *testing.T) {
	var err error = &InsufficientFundsError{Available: 3, Required: 4}
	if !errors.Is(err, ErrInsufficientFunds) || errors.Is(err, ErrFeeTooHigh) {
		t.Errorf("InsufficientFundsError matches wrongly")
	}
	err = &FeeTooHighError{Fee: 2, MaxFee: 1}
	if !errors.Is(err, ErrFeeTooHigh) || errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("FeeTooHighError matches wrongly")
	}
	err = &BroadcastRejectedError{Message: "no"}
	if !errors.Is(err, ErrBroadcastRejected) || err.Error() != "zeclib: the transaction was rejected: no" {
		t.Errorf("BroadcastRejectedError: %v", err)
	}
}

func TestSendTransactionReplies(t *testing.T) {
	srv := newFakeLwd(t, 5)
	c, err := dialLightwalletd(&Config{Server: srv.addr, Plaintext: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()
	ctx := context.Background()
	raw := append([]byte(fakeTxPrefix), bytes.Repeat([]byte{1}, 32)...)

	if err := c.sendTransaction(ctx, raw); err != nil {
		t.Fatalf("accepted: %v", err)
	}
	// zebrad refuses a transaction in its mempool. That counts as accepted.
	if err := c.sendTransaction(ctx, raw); err != nil {
		t.Errorf("in the mempool: %v", err)
	}
	// A transaction in its chain is a refusal.
	srv.mineTx(raw[len(fakeTxPrefix):], 3)
	if err := c.sendTransaction(ctx, raw); !errors.Is(err, ErrBroadcastRejected) {
		t.Errorf("mined: %v", err)
	}

	replies := []struct {
		code     int32
		message  string
		accepted bool
	}{
		{-26, "18: txn-already-in-mempool", true},
		{-27, "txn-already-known", false},
		{-22, "io error: failed to fill whole buffer", false},
		{-1, zebradMinedRefusal, false},
	}
	for _, r := range replies {
		srv.set(func(s *fakeLwd) { s.reject = &walletrpc.SendResponse{ErrorCode: r.code, ErrorMessage: r.message} })
		err := c.sendTransaction(ctx, raw)
		var rejected *BroadcastRejectedError
		switch {
		case r.accepted && err != nil:
			t.Errorf("%q: %v, want accepted", r.message, err)
		case !r.accepted && (!errors.As(err, &rejected) || rejected.Code != r.code || rejected.Message != r.message):
			t.Errorf("%q: %v, want a refusal", r.message, err)
		}
	}
	srv.set(func(s *fakeLwd) { s.reject = nil })

	statuses := []struct {
		code codes.Code
		want error
	}{
		{codes.InvalidArgument, ErrBroadcastRejected},
		{codes.Unavailable, ErrServerUnreachable},
		{codes.Internal, ErrServerFailure},
		{codes.Unimplemented, ErrServerFailure},
		{codes.Unknown, ErrServerFailure},
	}
	// A status that says the transaction is in the mempool is accepted too.
	srv.set(func(s *fakeLwd) {
		s.fail["SendTransaction"] = status.Error(codes.InvalidArgument, "transaction already exists in mempool")
	})
	if err := c.sendTransaction(ctx, raw); err != nil {
		t.Errorf("in the mempool, as a status: %v", err)
	}
	for _, st := range statuses {
		srv.set(func(s *fakeLwd) { s.fail["SendTransaction"] = status.Error(st.code, "no") })
		err := c.sendTransaction(ctx, raw)
		if !errors.Is(err, st.want) {
			t.Errorf("status %v: %v, want %v", st.code, err, st.want)
		}
		for _, other := range []error{ErrBroadcastRejected, ErrServerUnreachable, ErrServerFailure} {
			if other != st.want && errors.Is(err, other) {
				t.Errorf("status %v also matches %v", st.code, other)
			}
		}
	}
}

// rebroadcastProcessor returns a processor over the fake backend of e, with
// a clock the test sets, and the chain tip given to the backend.
func rebroadcastProcessor(t *testing.T, e *env, be *fakeBackend, now *time.Time) *processor {
	t.Helper()
	src, err := dialLightwalletd(&Config{Server: e.srv.addr, Plaintext: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { src.close() })
	opts := syncOptionsFor(&e.w.cfg)
	opts.rebroadcastMinDelay = time.Second
	opts.rebroadcastMaxDelay = 8 * time.Second
	opts.now = func() time.Time { return *now }
	if err := be.updateChainTip(e.srv.height()); err != nil {
		t.Fatal(err)
	}
	return newProcessor(newTestLogger(t), testNetwork, be, src, opts, func(func(*SyncStatus)) {})
}

// rebroadcastStep advances the clock, runs a rebroadcast pass, and returns the
// numbers of checks and sends it made.
func rebroadcastStep(t *testing.T, e *env, p *processor, now *time.Time, advance time.Duration) (checks, sends int) {
	t.Helper()
	gets, sent := e.srv.callCount("GetTransaction"), e.srv.callCount("SendTransaction")
	*now = now.Add(advance)
	if err := p.rebroadcast(context.Background()); err != nil {
		t.Fatalf("rebroadcast: %v", err)
	}
	return e.srv.callCount("GetTransaction") - gets, e.srv.callCount("SendTransaction") - sent
}

func TestRebroadcastSchedule(t *testing.T) {
	e, be := newSendEnv(t)
	now := time.Unix(1_000_000, 0)
	p := rebroadcastProcessor(t, e, be, &now)
	step := func(advance time.Duration) (int, int) { return rebroadcastStep(t, e, p, &now, advance) }

	// The first broadcast fails.
	e.srv.set(func(s *fakeLwd) { s.failOnce["SendTransaction"] = status.Error(codes.Unavailable, "down") })
	res, err := e.send()
	if err != nil || res.BroadcastErr == nil {
		t.Fatalf("Send: %+v, %v", res, err)
	}

	// A transaction is first checked after the minimum delay.
	if c, s := step(0); c != 0 || s != 0 {
		t.Fatalf("checked at once: %d checks, %d sends", c, s)
	}
	if p.rebroadcastAt != now.Add(time.Second) {
		t.Errorf("due at %v", p.rebroadcastAt)
	}
	// The server does not know it, so it is sent again.
	if c, s := step(time.Second); c != 1 || s != 1 {
		t.Fatalf("first check: %d checks, %d sends", c, s)
	}
	// The next check comes after twice the delay, and finds it in the
	// mempool.
	if c, _ := step(time.Second); c != 0 {
		t.Fatalf("checked again too early")
	}
	if c, s := step(time.Second); c != 1 || s != 0 {
		t.Fatalf("second check: %d checks, %d sends", c, s)
	}
	// The node drops its mempool. The next check, four seconds on, sends
	// it again.
	e.srv.dropMempool()
	if c, _ := step(3 * time.Second); c != 0 {
		t.Fatalf("checked again too early")
	}
	if c, s := step(time.Second); c != 1 || s != 1 {
		t.Fatalf("third check: %d checks, %d sends", c, s)
	}
	// The delay stops growing at the maximum.
	if c, s := step(8 * time.Second); c != 1 || s != 0 {
		t.Fatalf("fourth check: %d checks, %d sends", c, s)
	}
	if p.rebroadcastAt != now.Add(8*time.Second) {
		t.Errorf("due at %v", p.rebroadcastAt)
	}
}

// A server's report that a transaction is mined is not the wallet's
// knowledge. Whatever the server says, the transaction stays listed, is
// checked again, and is sent again once the server does not know it.
func TestRebroadcastIgnoresServerReports(t *testing.T) {
	reports := []struct {
		name   string
		height uint64
		resent bool
	}{
		{"mined at a historical height", 5, false},
		{"mined above the tip", 1_000, false},
		{"in a block off the main chain", math.MaxUint64, true},
	}
	for _, r := range reports {
		t.Run(r.name, func(t *testing.T) {
			e, be := newSendEnv(t)
			now := time.Unix(1_000_000, 0)
			p := rebroadcastProcessor(t, e, be, &now)
			if _, err := e.send(); err != nil {
				t.Fatal(err)
			}
			created := be.createdTxs()[0]
			e.srv.mineTx(created.txid, r.height)
			rebroadcastStep(t, e, p, &now, 0)
			c, s := rebroadcastStep(t, e, p, &now, time.Second)
			if c != 1 || (s == 1) != r.resent {
				t.Fatalf("%d checks, %d sends", c, s)
			}
			if st, ok := be.statuses[string(created.txid)]; ok {
				t.Errorf("the pass wrote status %+v", st)
			}
			if inv, _ := be.rebroadcastInventory(); len(inv) != 1 {
				t.Fatalf("listed %d", len(inv))
			}
			// The server stops knowing it: the same transaction is sent.
			e.srv.forgetTx(created.txid)
			sentBefore := len(e.srv.sentTxs())
			if c, s := rebroadcastStep(t, e, p, &now, 2*time.Second); c != 1 || s != 1 {
				t.Fatalf("after the server forgot it: %d checks, %d sends", c, s)
			}
			if sent := e.srv.sentTxs(); len(sent) != sentBefore+1 || !bytes.Equal(sent[len(sent)-1], created.raw) {
				t.Errorf("not the same raw transaction")
			}
		})
	}
}

// A transaction that the wallet found mined is listed again after a rewind
// below its height, and sent again when the server no longer knows it.
func TestRebroadcastAfterRewind(t *testing.T) {
	e, be := newSendEnv(t)
	now := time.Unix(1_000_000, 0)
	p := rebroadcastProcessor(t, e, be, &now)
	if _, err := e.send(); err != nil {
		t.Fatal(err)
	}
	created := be.createdTxs()[0]
	rebroadcastStep(t, e, p, &now, 0)

	// The wallet's scan finds it mined at 21.
	e.srv.mineTx(created.txid, 21)
	if err := be.setTransactionStatus(created.txid, txStatus{kind: txMined, height: 21}); err != nil {
		t.Fatal(err)
	}
	if c, _ := rebroadcastStep(t, e, p, &now, time.Hour); c != 0 || len(p.rebroadcasts) != 0 {
		t.Fatalf("a mined transaction is still followed: %d checks", c)
	}

	// A reorganization replaces block 21, and the node does not bring the
	// transaction back.
	if _, err := be.rewindToHeight(20); err != nil {
		t.Fatal(err)
	}
	e.srv.forgetTx(created.txid)
	if inv, _ := be.rebroadcastInventory(); len(inv) != 1 || !bytes.Equal(inv[0].raw, created.raw) {
		t.Fatalf("not listed again after the rewind")
	}
	rebroadcastStep(t, e, p, &now, 0)
	if c, s := rebroadcastStep(t, e, p, &now, time.Second); c != 1 || s != 1 {
		t.Fatalf("%d checks, %d sends", c, s)
	}
	sent := e.srv.sentTxs()
	if !bytes.Equal(sent[len(sent)-1], created.raw) {
		t.Errorf("not the same raw transaction")
	}
}

func TestRebroadcastToleratesTheServer(t *testing.T) {
	e, be := newSendEnv(t)
	now := time.Unix(1_000_000, 0)
	p := rebroadcastProcessor(t, e, be, &now)
	ctx := context.Background()
	e.srv.set(func(s *fakeLwd) { s.fail["SendTransaction"] = status.Error(codes.Unavailable, "down") })
	for range 2 {
		if _, err := e.send(); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.rebroadcast(ctx); err != nil {
		t.Fatal(err)
	}

	// A refusal is tried again at the next check.
	e.srv.set(func(s *fakeLwd) {
		delete(s.fail, "SendTransaction")
		s.reject = &walletrpc.SendResponse{ErrorCode: -26, ErrorMessage: "no"}
	})
	now = now.Add(time.Second)
	if err := p.rebroadcast(ctx); err != nil {
		t.Fatalf("a refusal failed the cycle: %v", err)
	}
	if n := e.srv.callCount("SendTransaction"); n != 4 {
		t.Fatalf("%d sends, want 2 initial and 2 again", n)
	}

	// A lost connection stops the pass without failing the cycle, and the
	// check is made again after the minimum delay.
	e.srv.set(func(s *fakeLwd) {
		s.reject = nil
		s.fail["GetTransaction"] = status.Error(codes.Unavailable, "down")
	})
	now = now.Add(2 * time.Second)
	if err := p.rebroadcast(ctx); err != nil {
		t.Fatalf("a lost connection failed the cycle: %v", err)
	}
	if n := e.srv.callCount("GetTransaction"); n != 3 {
		t.Fatalf("%d checks, want 2 and then 1 that stopped the pass", n)
	}
	e.srv.set(func(s *fakeLwd) { delete(s.fail, "GetTransaction") })
	now = now.Add(time.Second)
	if err := p.rebroadcast(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.srv.sentTxs()) != 2 {
		t.Errorf("the server holds %d transactions", len(e.srv.sentTxs()))
	}

	// A database failure ends the cycle.
	be.set(func(b *fakeBackend) { b.failOnce["rebroadcastInventory"] = wrapf(ErrDatabase, "disk") })
	if err := p.rebroadcast(ctx); !errors.Is(err, ErrDatabase) {
		t.Errorf("database failure: %v", err)
	}
}

// A transaction is not sent again once the chain has passed its expiry.
func TestRebroadcastStopsAtExpiry(t *testing.T) {
	e, be := newSendEnv(t)
	now := time.Unix(1_000_000, 0)
	p := rebroadcastProcessor(t, e, be, &now)
	be.set(func(b *fakeBackend) { b.expiryDelta = 3 })
	e.srv.set(func(s *fakeLwd) { s.fail["SendTransaction"] = status.Error(codes.Unavailable, "down") })
	if _, err := e.send(); err != nil {
		t.Fatal(err)
	}
	// The transaction can be mined up to three blocks above the tip.
	e.srv.mine(2)
	if err := be.updateChainTip(e.srv.height()); err != nil {
		t.Fatal(err)
	}
	if inv, _ := be.rebroadcastInventory(); len(inv) != 1 {
		t.Fatalf("a transaction that can still be mined is not in the inventory")
	}
	e.srv.mine(1)
	if err := be.updateChainTip(e.srv.height()); err != nil {
		t.Fatal(err)
	}
	if inv, _ := be.rebroadcastInventory(); len(inv) != 0 {
		t.Fatalf("an expired transaction is in the inventory")
	}
	now = now.Add(time.Hour)
	if err := p.rebroadcast(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.srv.callCount("GetTransaction") + len(e.srv.sentTxs()); n != 0 {
		t.Errorf("an expired transaction was checked or sent")
	}
}

// The sync loop of a connected wallet resubmits a transaction whose first
// broadcast failed.
func TestSyncRebroadcasts(t *testing.T) {
	e, be := newSendEnv(t)
	tune := e.w.tune
	e.w.tune = func(o *syncOptions) {
		tune(o)
		o.rebroadcastMinDelay = 20 * time.Millisecond
		o.rebroadcastMaxDelay = 40 * time.Millisecond
	}
	e.srv.set(func(s *fakeLwd) { s.failOnce["SendTransaction"] = status.Error(codes.Unavailable, "down") })
	res, err := e.send()
	if err != nil || res.BroadcastErr == nil {
		t.Fatalf("Send: %+v, %v", res, err)
	}
	e.connect()
	deadline := time.Now().Add(waitTimeout)
	for len(e.srv.sentTxs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the transaction was not broadcast again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sent := e.srv.sentTxs(); !bytes.Equal(sent[0], be.createdTxs()[0].raw) {
		t.Errorf("another transaction was sent")
	}
	if n := len(be.createdTxs()); n != 1 {
		t.Errorf("%d transactions created", n)
	}
}

func TestDisplayTxID(t *testing.T) {
	txid := make([]byte, 32)
	for i := range txid {
		txid[i] = byte(i)
	}
	const want = "1f1e1d1c1b1a191817161514131211100f0e0d0c0b0a09080706050403020100"
	if got := displayTxID(txid); got != want {
		t.Errorf("displayTxID = %s, want %s", got, want)
	}
}

// A failed broadcast of the first of two transactions returns both IDs and
// does not send the second, which can depend on the first.
func TestSendStopsAtTheFirstFailedBroadcast(t *testing.T) {
	e, be := newSendEnv(t)
	be.set(func(b *fakeBackend) { b.txsPerSend = 2 })
	e.srv.set(func(s *fakeLwd) { s.failOnce["SendTransaction"] = status.Error(codes.Unavailable, "down") })
	res, err := e.send()
	if err != nil || res.BroadcastErr == nil || len(res.TxIDs) != 2 {
		t.Fatalf("Send: %+v, %v", res, err)
	}
	created := be.createdTxs()
	for i := range created {
		if res.TxIDs[i] != displayTxID(created[i].txid) {
			t.Errorf("transaction %d: %s", i, res.TxIDs[i])
		}
	}
	if n := e.srv.callCount("SendTransaction"); n != 1 {
		t.Errorf("%d broadcasts, want 1", n)
	}
}

// Send broadcasts to the server the list names at the time of the call.
func TestSendBroadcastsToTheCurrentServer(t *testing.T) {
	e, servers := serverEnv(t, 20, 2)
	e.createAccount(10)
	e.be.(*fakeBackend).set(func(b *fakeBackend) { b.sendFee = 10_000 })
	e.w.servers.rotate(e.w.servers.get())
	if _, err := e.send(); err != nil {
		t.Fatal(err)
	}
	if a, b := servers[0].callCount("SendTransaction"), servers[1].callCount("SendTransaction"); a != 0 || b != 1 {
		t.Errorf("broadcasts: %d to the first server, %d to the second", a, b)
	}
}

func TestSendWithADoneContext(t *testing.T) {
	e, be := newSendEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := e.w.Send(ctx, e.account.ID, testSeed, testRecipient, 100_000, nil, 20_000)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !errors.Is(res.BroadcastErr, context.Canceled) || len(res.TxIDs) != 1 || len(be.createdTxs()) != 1 {
		t.Errorf("result %+v", res)
	}
	if n := e.srv.callCount("SendTransaction"); n != 0 {
		t.Errorf("%d broadcasts", n)
	}
}

// Send runs safely against Close, rotation and the rebroadcast of the sync
// loop. Run with -race.
func TestSendConcurrently(t *testing.T) {
	e, _ := serverEnv(t, 20, 2)
	e.createAccount(10)
	be := e.be.(*fakeBackend)
	be.set(func(b *fakeBackend) { b.sendFee = 10_000 })
	tune := e.w.tune
	e.w.tune = func(o *syncOptions) {
		tune(o)
		o.rebroadcastMinDelay = time.Millisecond
		o.rebroadcastMaxDelay = 2 * time.Millisecond
	}
	// The sync loop starts with transactions to check, which its server
	// does not know, so it checks and resends while the sends run.
	be.set(func(b *fakeBackend) {
		for range 3 {
			b.createLocked(testRecipient)
		}
	})
	stop := e.connect()

	var mtx sync.Mutex
	var returned []string
	var wg sync.WaitGroup
	send := func(n int) {
		defer wg.Done()
		for range n {
			res, err := e.send()
			if errors.Is(err, ErrClosed) {
				return
			}
			if err != nil {
				t.Errorf("Send: %v", err)
				return
			}
			mtx.Lock()
			returned = append(returned, res.TxIDs...)
			mtx.Unlock()
		}
	}
	// A first round of sends completes before anything is closed.
	wg.Add(4)
	for range 4 {
		go send(5)
	}
	wg.Wait()
	if len(returned) != 20 {
		t.Fatalf("%d sends completed", len(returned))
	}
	// The sync loop is checking and resending by now.
	deadline := time.Now().Add(waitTimeout)
	for e.srv.callCount("GetTransaction") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the sync loop did not check the pending transactions")
		}
		time.Sleep(time.Millisecond)
	}
	// A second round runs against rotation, rebroadcast, the end of syncing
	// and Close. Each sender signals its first success, and the closing
	// waits for all four, so that sends are under way when it starts.
	started := make(chan struct{}, 4)
	sendSignalling := func(n int) {
		defer wg.Done()
		signalled := false
		for range n {
			res, err := e.send()
			if errors.Is(err, ErrClosed) {
				break
			}
			if err != nil {
				t.Errorf("Send: %v", err)
				break
			}
			mtx.Lock()
			returned = append(returned, res.TxIDs...)
			mtx.Unlock()
			if !signalled {
				started <- struct{}{}
				signalled = true
			}
		}
		if !signalled {
			started <- struct{}{}
		}
	}
	wg.Add(5)
	for range 4 {
		go sendSignalling(50)
	}
	go func() {
		defer wg.Done()
		for range 20 {
			e.w.servers.rotate(e.w.servers.get())
		}
	}()
	for range 4 {
		<-started
	}
	stop()
	if err := e.w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	wg.Wait()

	created := make(map[string]bool)
	for _, tx := range be.createdTxs() {
		created[displayTxID(tx.txid)] = true
	}
	for _, id := range returned {
		if !created[id] {
			t.Errorf("returned transaction %s was not created", id)
		}
	}
}

func TestSendMax(t *testing.T) {
	e, be := newSendEnv(t)
	be.set(func(b *fakeBackend) {
		b.spendableValue = SpendableValue{Ironwood: 50_000, Transparent: 60_000}
		b.leftBehind = LeftBehind{TransparentCount: 1, TransparentValue: 7_000}
	})
	// What one transaction takes, less the fee: 110,000 - 7,000 - 10,000.
	const amount = 93_000
	est, err := e.w.EstimateSendMax(e.account.ID, testRecipient, nil)
	if err != nil || est.Amount != amount || est.Fee != 10_000 || est.LeftBehind.TransparentValue != 7_000 {
		t.Fatalf("EstimateSendMax: %+v, %v", est, err)
	}
	sendMax := func(maxFee, expected uint64) (*SendMaxResult, error) {
		return e.w.SendMax(context.Background(), e.account.ID, testSeed, testRecipient, nil, maxFee, expected)
	}
	if _, err := sendMax(9_999, 0); !errors.Is(err, ErrFeeTooHigh) {
		t.Errorf("fee over the limit: %v", err)
	}
	var changed *AmountChangedError
	if _, err := sendMax(10_000, amount+1); !errors.As(err, &changed) || changed.Expected != amount+1 || changed.Actual != amount {
		t.Errorf("another amount expected: %v", err)
	}
	if n := len(be.createdTxs()); n != 0 {
		t.Fatalf("%d transactions created by failed calls", n)
	}
	e.srv.set(func(s *fakeLwd) { s.failOnce["SendTransaction"] = status.Error(codes.Unavailable, "down") })
	res, err := sendMax(10_000, amount)
	if err != nil || res.Amount != amount || len(res.TxIDs) != 1 || !errors.Is(res.BroadcastErr, ErrServerUnreachable) ||
		res.LeftBehind.TransparentCount != 1 || res.PaidAddress != testRecipient {
		t.Fatalf("SendMax: %+v, %v", res, err)
	}
	if inv, _ := be.rebroadcastInventory(); len(inv) != 1 {
		t.Errorf("the send max is not pending")
	}

	// Too little to cover the fee.
	be.set(func(b *fakeBackend) {
		b.spendableValue = SpendableValue{Transparent: 9_000}
		b.leftBehind = LeftBehind{}
	})
	var funds *InsufficientFundsError
	if _, err := e.w.EstimateSendMax(e.account.ID, testRecipient, nil); !errors.As(err, &funds) || funds.Available != 9_000 || funds.Required != 10_001 {
		t.Errorf("estimate without funds: %v", err)
	}
}

// rebroadcastCycle runs one sync cycle over a chain of 20 blocks scanned one
// block per batch, with a clock that the backend moves on by step at each
// batch, and calls during(batch) at each batch as well.
func rebroadcastCycle(t *testing.T, minDelay, step time.Duration, prepare func(e *env, be *fakeBackend), during func(b *fakeBackend, batch int)) (*env, *fakeBackend) {
	t.Helper()
	return rebroadcastCycleWith(t, minDelay, step, prepare, during, nil)
}

// rebroadcastCycleWith is rebroadcastCycle with the processor's connection
// wrapped by wrap, which is given the clock.
func rebroadcastCycleWith(t *testing.T, minDelay, step time.Duration, prepare func(e *env, be *fakeBackend), during func(b *fakeBackend, batch int), wrap func(src chainSource, now *time.Time) chainSource) (*env, *fakeBackend) {
	t.Helper()
	e := newEnv(t, backendKinds[0], 20, nil)
	e.createAccount(1)
	be := e.be.(*fakeBackend)
	be.set(func(b *fakeBackend) { b.sendFee = 10_000 })
	if prepare != nil {
		prepare(e, be)
	}
	now := time.Unix(1_000_000, 0)
	var batch int
	be.set(func(b *fakeBackend) {
		b.onScan = func(b *fakeBackend) {
			batch++
			now = now.Add(step)
			if during != nil {
				during(b, batch)
			}
		}
	})
	src, err := dialLightwalletd(&Config{Server: e.srv.addr, Plaintext: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.close() })
	opts := syncOptionsFor(&e.w.cfg)
	opts.batchSize = 1
	opts.rebroadcastMinDelay = minDelay
	opts.rebroadcastMaxDelay = 4 * minDelay
	opts.now = func() time.Time { return now }
	var source chainSource = src
	if wrap != nil {
		source = wrap(src, &now)
	}
	p := newProcessor(newTestLogger(t), testNetwork, be, source, opts, func(func(*SyncStatus)) {})
	for again := true; again; {
		again, err = p.cycle(context.Background())
		if err != nil {
			t.Fatalf("cycle: %v", err)
		}
	}
	if batch < 15 {
		t.Fatalf("only %d batches", batch)
	}
	return e, be
}

// A transaction created during a long cycle that began with an empty
// inventory is found and sent again before the cycle ends.
func TestRebroadcastFindsATransactionCreatedDuringACycle(t *testing.T) {
	var created []byte
	e, _ := rebroadcastCycle(t, time.Second, 600*time.Millisecond, nil, func(b *fakeBackend, batch int) {
		if batch == 3 {
			// Created and never broadcast: the server does not know it.
			created = b.createLocked(testRecipient).transactions[0].raw
		}
	})
	sent := e.srv.sentTxs()
	if len(sent) != 1 || !bytes.Equal(sent[0], created) {
		t.Fatalf("the transaction created during the cycle was not sent again before it ended")
	}
}

// With nothing due and the refresh delay not passed, the inventory is not
// read between batches.
func TestRebroadcastDoesNotReadTheInventoryPerBatch(t *testing.T) {
	_, be := rebroadcastCycle(t, time.Hour, time.Second, nil, nil)
	if n := be.callCount("rebroadcastInventory"); n != 1 {
		t.Errorf("the inventory was read %d times, want once at the start of the cycle", n)
	}
	// The same with a pending transaction whose first check, an hour after
	// it is first seen, lies beyond the cycle.
	e, be := rebroadcastCycle(t, time.Hour, time.Second, func(_ *env, be *fakeBackend) {
		be.set(func(b *fakeBackend) { b.createLocked(testRecipient) })
	}, nil)
	if n := be.callCount("rebroadcastInventory"); n != 1 {
		t.Errorf("with a pending transaction, the inventory was read %d times, want once", n)
	}
	if n := e.srv.callCount("GetTransaction"); n != 0 {
		t.Errorf("%d checks of a transaction not due", n)
	}
}

// slowUnreachable is a chainSource whose GetTransaction fails as an
// unreachable server does, after its timeout: the clock moves on by delay
// during each call.
type slowUnreachable struct {
	chainSource
	now   *time.Time
	delay time.Duration
	// calls records the start and end of each call.
	calls [][2]time.Time
}

func (s *slowUnreachable) transaction(context.Context, []byte) (*walletrpc.RawTransaction, error) {
	start := *s.now
	*s.now = s.now.Add(s.delay)
	s.calls = append(s.calls, [2]time.Time{start, *s.now})
	return nil, &serverError{"GetTransaction", status.Error(codes.DeadlineExceeded, "timed out")}
}

// assertHeld checks that the calls to an unreachable server are at least the
// minimum delay apart, counted from the end of the previous call.
func assertHeld(t *testing.T, calls [][2]time.Time, minDelay time.Duration) {
	t.Helper()
	for i := 1; i < len(calls); i++ {
		if gap := calls[i][0].Sub(calls[i-1][1]); gap < minDelay {
			t.Errorf("call %d began %s after call %d returned, want at least %s", i, gap, i-1, minDelay)
		}
	}
}

// After a check that found the server unreachable, no check of any
// transaction is made before the minimum delay after the call returned. The
// call itself takes as long as the delay.
func TestRebroadcastSpacesChecksOnAnUnreachableServer(t *testing.T) {
	const minDelay = 2 * time.Second
	var slow *slowUnreachable
	// 20 batches 400 ms apart, plus the time the calls take.
	rebroadcastCycleWith(t, minDelay, 400*time.Millisecond, func(_ *env, be *fakeBackend) {
		be.set(func(b *fakeBackend) {
			for range 3 {
				b.createLocked(testRecipient)
			}
		})
	}, nil, func(src chainSource, now *time.Time) chainSource {
		slow = &slowUnreachable{chainSource: src, now: now, delay: minDelay}
		return slow
	})
	if len(slow.calls) == 0 {
		t.Fatal("no check was made")
	}
	assertHeld(t, slow.calls, minDelay)
}

// A synced wallet on an unreachable server does not wait out the timeout in
// every cycle: the pass at the start of each cycle makes no check while
// checks are held, and still reads the inventory.
func TestRebroadcastHoldsChecksAcrossCycles(t *testing.T) {
	e, be := newSendEnv(t)
	now := time.Unix(1_000_000, 0)
	p := rebroadcastProcessor(t, e, be, &now)
	slow := &slowUnreachable{chainSource: p.src, now: &now, delay: time.Second}
	p.src = slow
	be.set(func(b *fakeBackend) {
		for range 3 {
			b.createLocked(testRecipient)
		}
	})
	// A cycle every 100 ms, each starting with a pass. The calls move the
	// clock on as well.
	start := now
	reads := be.callCount("rebroadcastInventory")
	for range 100 {
		if err := p.rebroadcast(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(100 * time.Millisecond)
	}
	// Each call takes 1 s and is followed by a hold of 1 s: at most one
	// call per 2 s of clock. Without the hold, every pass would call.
	elapsed := now.Sub(start)
	if n := len(slow.calls); n == 0 || time.Duration(n) > elapsed/(2*time.Second)+1 {
		t.Errorf("%d checks in %s", n, elapsed)
	}
	assertHeld(t, slow.calls, time.Second)
	if n := be.callCount("rebroadcastInventory") - reads; n != 100 {
		t.Errorf("the inventory was read %d times in 100 passes", n)
	}
	// The hold did not count as checks.
	for _, st := range p.rebroadcasts {
		if st.checks != 0 {
			t.Errorf("a held check counted: %+v", st)
		}
	}
}
