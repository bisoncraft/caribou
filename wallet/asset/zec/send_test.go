// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

// sendEnv is an unlocked, synced wallet whose sends create a record.
func sendEnv(t *testing.T) *txEnv {
	t.Helper()
	e := openTxEnv(t, 300)
	if err := e.w.Unlock(testPW); err != nil {
		t.Fatal(err)
	}
	e.c.sentRecord = func(id string, amount uint64) *zeclib.TxRecord {
		r := send(id, amount, 10_000, 0)
		return &r
	}
	return e
}

// setAvailable sets what a payment can spend and refreshes the balance.
func (e *txEnv) setAvailable(v uint64) {
	e.o.latest().set(func(b *fakeBackend) {
		b.summary = summaryOf(zeclib.PoolBalance{Spendable: v, Total: v}, zeclib.PoolBalance{}, zeclib.PoolBalance{})
		b.spendable = zeclib.SpendableValue{Ironwood: v}
	})
	e.w.refresh()
}

func TestSendGates(t *testing.T) {
	e := openTxEnv(t, 300)
	if _, err := e.w.Send("ufake-other", 1_000, feeRate); !errors.Is(err, errLocked) {
		t.Errorf("locked: %v", err)
	}
	if err := e.w.Unlock(testPW); err != nil {
		t.Fatal(err)
	}
	if _, err := e.w.Send("ufake-other", 1_000, 1); err == nil {
		t.Error("another fee rate")
	}
	e.setStatus(zeclib.SyncSyncing, 300)
	if _, err := e.w.Send("ufake-other", 1_000, feeRate); !errors.Is(err, errNotSynced) {
		t.Errorf("syncing: %v", err)
	}
	// Synced with an error in the status is synced.
	e.o.latest().set(func(b *fakeBackend) {
		b.status = zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 300, Err: zeclib.ErrOutputUnrecorded}
	})
	if _, err := e.w.Send("ufake-other", 1_000, feeRate); err != nil {
		t.Errorf("synced with an unrecorded output: %v", err)
	}
	if len(e.c.sent) != 1 {
		t.Fatalf("%d sends", len(e.c.sent))
	}
}

// The fee limit is the estimate's fee, the send gets a copy of the seed,
// and the record is written before Send returns.
func TestSend(t *testing.T) {
	e := sendEnv(t)
	e.c.estimate = func(uint64) (*zeclib.TransferEstimate, error) { return &zeclib.TransferEstimate{Fee: 15_000}, nil }
	coin, err := e.w.Send("ufake-other", 40_000, feeRate)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	s := e.c.sent[0]
	if s.sendMax || s.amount != 40_000 || s.maxFee != 15_000 || !bytes.Equal(s.seed, testZip32) {
		t.Errorf("send: %+v", s)
	}
	id := fmt.Sprintf("%064x", 0xabc001)
	if coin.TxID() != id || coin.String() != id || coin.Value() != 40_000 || displayTxID(coin.ID()) != id {
		t.Errorf("coin %s %s %d %x", coin.TxID(), coin.String(), coin.Value(), coin.ID())
	}
	if wt := e.stored(id); wt == nil || wt.Type != txdb.Send || wt.Amount != 40_000 {
		t.Fatalf("record: %+v", wt)
	}
	notes := e.txNotes()
	if len(notes) != 1 || !notes[0].New {
		t.Errorf("notes: %+v", notes)
	}
	if _, open := e.w.proj.open[id]; !open {
		t.Error("the sent transaction is not open")
	}
}

// A failed first broadcast, and a record that cannot be read, do not fail a
// send that created its transaction.
func TestSendAfterCreationCannotFail(t *testing.T) {
	e := sendEnv(t)
	e.c.broadcastErr = zeclib.ErrServerUnreachable
	if coin, err := e.w.Send("ufake-other", 1_000, feeRate); err != nil || coin == nil {
		t.Fatalf("a failed broadcast: %v", err)
	}
	if e.log.count("first broadcast") != 1 {
		t.Error("the failed broadcast was not logged")
	}
	e.c.sentRecord = nil
	if coin, err := e.w.Send("ufake-other", 1_000, feeRate); err != nil || coin == nil {
		t.Fatalf("a record that cannot be read: %v", err)
	}
}

func TestWithdrawWhole(t *testing.T) {
	e := sendEnv(t)
	e.setAvailable(100_000)
	e.c.sendMax = &zeclib.SendMaxEstimate{Amount: 85_000, Fee: 15_000}
	coin, err := e.w.Withdraw("ufake-other", 100_000, feeRate)
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if s := e.c.sent[0]; !s.sendMax || s.maxFee != 15_000 || s.expected != 85_000 || coin.Value() != 85_000 {
		t.Errorf("send max: %+v, coin %d", s, coin.Value())
	}
	if fee, valid, err := e.w.EstimateSendTxFee("ufake-other", 100_000, feeRate, true, false); err != nil || !valid || fee != 15_000 {
		t.Errorf("estimate of the whole balance: %d %t %v", fee, valid, err)
	}
	if fee, _, err := e.w.EstimateSendTxFee("ufake-other", 0, feeRate, false, true); err != nil || fee != 15_000 {
		t.Errorf("estimate of the max: %d %v", fee, err)
	}

	// The balance moved since it was read.
	e.c.sendMax = &zeclib.SendMaxEstimate{Amount: 80_000, Fee: 15_000}
	if _, err := e.w.Withdraw("ufake-other", 100_000, feeRate); !errors.Is(err, errBalanceChanged) {
		t.Errorf("a changed balance: %v", err)
	}
	if _, _, err := e.w.EstimateSendTxFee("ufake-other", 100_000, feeRate, true, false); !errors.Is(err, errBalanceChanged) {
		t.Errorf("estimate with a changed balance: %v", err)
	}
	// More than one transaction can take.
	e.c.sendMax = &zeclib.SendMaxEstimate{Amount: 80_000, Fee: 15_000,
		LeftBehind: zeclib.LeftBehind{TransparentCount: 1, TransparentValue: 5_000}}
	if _, err := e.w.Withdraw("ufake-other", 100_000, feeRate); !errors.Is(err, errLeftBehind) {
		t.Errorf("funds left behind: %v", err)
	}
	if _, _, err := e.w.EstimateSendTxFee("ufake-other", 100_000, feeRate, true, false); !errors.Is(err, errLeftBehind) {
		t.Errorf("estimate with funds left behind: %v", err)
	}
	if len(e.c.sent) != 1 {
		t.Errorf("%d sends", len(e.c.sent))
	}
}

// notesBackend models a wallet of equal Ironwood notes: a payment of amount
// spends the fewest notes that cover it and its fee of 5,000 per logical
// action, at least two.
func notesBackend(count int, note uint64) func(uint64) (*zeclib.TransferEstimate, error) {
	fee := func(n int) uint64 { return feeRate * uint64(max(n, standardActions)) }
	return func(amount uint64) (*zeclib.TransferEstimate, error) {
		for n := 1; n <= count; n++ {
			if uint64(n)*note >= amount+fee(n) {
				return &zeclib.TransferEstimate{Fee: fee(n)}, nil
			}
		}
		return nil, &zeclib.InsufficientFundsError{Available: uint64(count) * note, Required: amount + fee(count)}
	}
}

// A withdrawal near the balance: the first estimate is not covered, and its
// required value leads to the fee that is.
func TestWithdrawNearTheBalance(t *testing.T) {
	e := sendEnv(t)
	e.setAvailable(100_000)
	e.c.estimate = notesBackend(4, 25_000)
	if fee, _, err := e.w.EstimateSendTxFee("ufake-other", 95_000, feeRate, true, false); err != nil || fee != 20_000 {
		t.Fatalf("estimate: %d %v", fee, err)
	}
	coin, err := e.w.Withdraw("ufake-other", 95_000, feeRate)
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	s := e.c.sent[0]
	if s.sendMax || s.amount != 75_000 || s.maxFee != 20_000 || coin.Value() != 75_000 {
		t.Errorf("send: %+v", s)
	}
}

// Change below the dust threshold makes the required value of an estimate
// more than its fee: the difference only picks the next amount, and the fee
// sent is that of an estimate that succeeded.
func TestWithdrawWithDustChange(t *testing.T) {
	e := sendEnv(t)
	e.setAvailable(100_000)
	e.c.estimate = func(amount uint64) (*zeclib.TransferEstimate, error) {
		const input, fee, dust = 100_000, 15_000, 5_000
		if amount+fee > input {
			return nil, &zeclib.InsufficientFundsError{Available: input, Required: amount + fee}
		}
		if change := input - amount - fee; change > 0 && change < dust {
			return nil, &zeclib.InsufficientFundsError{Available: input, Required: input + dust - change}
		}
		return &zeclib.TransferEstimate{Fee: fee}, nil
	}
	if fee, _, err := e.w.EstimateSendTxFee("ufake-other", 99_000, feeRate, true, false); err != nil || fee != 15_000 {
		t.Fatalf("estimate: %d %v", fee, err)
	}
	if _, err := e.w.Withdraw("ufake-other", 99_000, feeRate); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	s := e.c.sent[0]
	if s.amount+s.maxFee > 99_000 || s.maxFee != 15_000 {
		t.Errorf("send: %+v", s)
	}
}

// A fee that oscillates between two amounts stops after the round bound and
// sends the value less the highest fee seen.
func TestWithdrawOscillatingFee(t *testing.T) {
	e := sendEnv(t)
	e.setAvailable(1_000_000)
	e.c.estimate = func(amount uint64) (*zeclib.TransferEstimate, error) {
		if amount > 85_000 {
			return &zeclib.TransferEstimate{Fee: 20_000}, nil
		}
		return &zeclib.TransferEstimate{Fee: 10_000}, nil
	}
	// 100,000 less 10,000 needs 20,000; less 20,000 needs 10,000.
	if _, err := e.w.Withdraw("ufake-other", 100_000, feeRate); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if n := len(e.c.estimates); n != feeRounds+1 {
		t.Errorf("%d estimates", n)
	}
	s := e.c.sent[0]
	if s.amount != 80_000 || s.maxFee != 10_000 {
		t.Errorf("send: %+v", s)
	}
}

func TestEstimateSendTxFee(t *testing.T) {
	e := sendEnv(t)
	e.setAvailable(100_000)
	e.c.estimate = func(uint64) (*zeclib.TransferEstimate, error) { return &zeclib.TransferEstimate{Fee: 15_000}, nil }
	if fee, valid, err := e.w.EstimateSendTxFee("ufake-other", 10_000, feeRate, false, false); err != nil || !valid || fee != 15_000 {
		t.Errorf("a send: %d %t %v", fee, valid, err)
	}
	if _, valid, err := e.w.EstimateSendTxFee("nonsense", 10_000, feeRate, false, false); err == nil || valid {
		t.Errorf("an invalid address: %t %v", valid, err)
	}
	if _, _, err := e.w.EstimateSendTxFee("ufake-other", 10_000, 1, false, false); err == nil {
		t.Error("another rate")
	}
	if len(e.c.sent) != 0 {
		t.Error("an estimate sent")
	}
}

// A send in flight when another instance displaces the wallet completes,
// record included, before the displacement closes the database.
func TestSendInFlightDuringDisplacement(t *testing.T) {
	e := sendEnv(t)
	reached, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.c.sendHook = func() {
		once.Do(func() {
			close(reached)
			<-resume
		})
	}
	sent := make(chan error, 1)
	go func() {
		_, err := e.w.Send("ufake-other", 1_000, feeRate)
		sent <- err
	}()
	waitClosed(t, "the send to start", reached)
	other := e.newTxInstance()
	opened := make(chan struct{})
	go func() {
		defer close(opened)
		if err := other.OpenWithPW(context.Background(), testPW); err != nil {
			t.Errorf("OpenWithPW: %v", err)
		}
	}()
	stillRunning(t, "the displacement", opened)
	close(resume)
	if err := <-sent; err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitClosed(t, "the displacement", opened)
	id := fmt.Sprintf("%064x", 0xabc001)
	if wt := e.stored(id); wt == nil {
		t.Fatal("the send's record was not written")
	}
	if _, err := e.w.Send("ufake-other", 1_000, feeRate); !errors.Is(err, errClosed) {
		t.Errorf("a send on the displaced instance: %v", err)
	}
}

// A send and the worker's passes find a new transaction at once: it is
// reported as new once.
func TestSendAndWorkerReportOnce(t *testing.T) {
	e := sendEnv(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				e.project(projectChange)
			}
		}
	}()
	for range 20 {
		if _, err := e.w.Send("ufake-other", 1_000, feeRate); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	news := make(map[string]int)
	for _, n := range e.txNotes() {
		if n.New {
			news[n.Transaction.ID]++
		}
	}
	if len(news) != 20 {
		t.Errorf("%d transactions reported new", len(news))
	}
	for id, n := range news {
		if n != 1 {
			t.Errorf("%s reported new %d times", id, n)
		}
	}
}

// A payment to the wallet's own address is a SelfSend from the moment Send
// returns, before zeclib has scanned the block and marked the output the
// wallet's. An output to an address only partly the wallet's stays a Send.
func TestSelfSendRecordedAtOnce(t *testing.T) {
	e := sendEnv(t)
	parsed["ufake-foreign-o+ours-t"] = &zeclib.ParsedAddress{Kind: zeclib.AddressUnified, Payable: true,
		Receivers: []zeclib.Receiver{{Kind: zeclib.ReceiverOrchard, Address: "ufake-foreign-o"},
			{Kind: zeclib.ReceiverTransparent, Address: testTAddress}}}
	t.Cleanup(func() { delete(parsed, "ufake-foreign-o+ours-t") })
	var to string
	e.c.sentRecord = func(id string, amount uint64) *zeclib.TxRecord {
		return &zeclib.TxRecord{TxID: id, TotalSpent: amount + 10_000 + 1_000, TotalReceived: 1_000,
			BalanceDelta: -int64(amount + 10_000), Fee: u64(10_000), RawPresent: true, CreatedByWallet: true,
			Outputs: []zeclib.TxOutput{
				// Not yet marked the wallet's.
				{Pool: zeclib.OutputIronwood, Value: amount, Address: to},
				{Pool: zeclib.OutputIronwood, Value: 1_000, ToWallet: true, IsChange: true},
			}}
	}
	to = testAddress
	coin, err := e.w.Send(testAddress, 20_000, feeRate)
	if err != nil {
		t.Fatal(err)
	}
	wt := e.stored(coin.TxID())
	if wt.Type != txdb.SelfSend || wt.Amount != 20_000 || wt.Recipient == nil || *wt.Recipient != testAddress {
		t.Errorf("a payment to the own address: %+v", wt)
	}
	// The record does not change once zeclib marks the output.
	e.c.mtx.Lock()
	e.c.records[coin.TxID()].Outputs[0].ToWallet = true
	e.c.mtx.Unlock()
	e.txNotes()
	e.project(projectChange)
	if n := len(e.txNotes()); n != 0 {
		t.Errorf("%d notes when zeclib marked the output", n)
	}

	to = "ufake-foreign-o+ours-t"
	coin, err = e.w.Send(to, 20_000, feeRate)
	if err != nil {
		t.Fatal(err)
	}
	if wt := e.stored(coin.TxID()); wt.Type != txdb.Send {
		t.Errorf("a payment to an address only partly the wallet's: %+v", wt)
	}
}

// An estimate fails while the wallet syncs, as a send would, and does not
// need the wallet unlocked.
func TestEstimateNeedsSync(t *testing.T) {
	e := sendEnv(t)
	e.setStatus(zeclib.SyncSyncing, 300)
	if _, _, err := e.w.EstimateSendTxFee("ufake-other", 1_000, feeRate, false, false); !errors.Is(err, errNotSynced) {
		t.Errorf("an estimate while syncing: %v", err)
	}
	// Locked does not matter for an estimate.
	e.setStatus(zeclib.SyncSynced, 300)
	e.w.Lock()
	if _, _, err := e.w.EstimateSendTxFee("ufake-other", 1_000, feeRate, false, false); err != nil {
		t.Errorf("an estimate while locked: %v", err)
	}
}

// After the transactions exist, an ID zeclib could not give still makes a
// coin.
func TestCoinForNeverFails(t *testing.T) {
	e := openEnv(t)
	for _, ids := range [][]string{nil, {"zz"}, {"abcd"}} {
		c := e.w.coinFor(ids, 5)
		if c == nil || c.Value() != 5 {
			t.Errorf("%q: %+v", ids, c)
		}
	}
	if c := e.w.coinFor([]string{"zz"}, 5); c.TxID() != "zz" || string(c.ID()) != "zz" {
		t.Errorf("a malformed ID: %s %x", c.TxID(), c.ID())
	}
}
