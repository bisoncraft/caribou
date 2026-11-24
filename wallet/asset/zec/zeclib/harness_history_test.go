// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build zec && cgo && harness

package zeclib

import (
	"bytes"
	"context"
	"testing"
)

// TestHarnessCoinbaseOutputIsNotSpendable mines a block whose coinbase pays
// a wallet's first receive address after the wallet has walked its
// addresses, so that only the unspent-output query reports the output. The
// wallet fetches the funding transaction, knows the output for a coinbase
// output, and never selects it: not while it is immature, and not after 100
// more blocks. A regular output beside it is spent, and the node accepts
// that payment, which it would refuse if the coinbase output were among the
// inputs.
func TestHarnessCoinbaseOutputIsNotSpendable(t *testing.T) {
	sw := newSendWallet(t, "sender")
	bystander := newSendWallet(t, "bystander")
	sw.connect()
	sw.waitSynced(harnessHeight(t))

	harnessCtl(t, "mine-to", sw.transparentAddress(), "1")
	s := sw.waitFor("the coinbase output", func(s *Summary, st SyncStatus) bool {
		return st.State == SyncSynced && sw.balance(s).Coinbase.Total > 0
	})
	coinbase := sw.balance(s).Coinbase.Total
	t.Logf("coinbase output of %d recorded; coinbase balance %+v", coinbase, sw.balance(s).Coinbase)
	if b := sw.balance(s).Transparent; b.Total != 0 {
		t.Errorf("the coinbase output is counted as a regular output: %+v", b)
	}
	if v := sw.spendable(); v.Transparent != 0 {
		t.Fatalf("an immature coinbase output is spendable: %+v", v)
	}

	harnessSend(t, sw.transparentAddress(), 50_000_000)
	harnessMine(t, 1)
	sw.waitSpendable("the regular output", func(v SpendableValue) bool { return v.Transparent == 50_000_000 })
	res := sw.send(bystander.address(), 20_000_000)
	if res.BroadcastErr != nil {
		t.Fatalf("the node refused the payment: %v", res.BroadcastErr)
	}
	harnessMine(t, 1)
	tx := waitNodeTx(t, res.TxIDs[0], true)
	if len(tx.Vin) != 1 {
		t.Errorf("the payment spends %d transparent inputs", len(tx.Vin))
	}

	// Mature now, and still not selected by a payment.
	harnessMine(t, 100)
	sw.waitSynced(harnessHeight(t))
	if v := sw.spendable(); v.Transparent != 0 {
		t.Errorf("a mature coinbase output is spendable: %+v", v)
	}
	s = sw.waitSynced(harnessHeight(t))
	if b := sw.balance(s).Coinbase; b.Total != coinbase {
		t.Errorf("the coinbase output is not held: %+v", b)
	}
}

// TestHarnessHistory reads the history of a shielded send with change and a
// memo, and of a payment to the wallet's own address.
func TestHarnessHistory(t *testing.T) {
	sw := newSendWallet(t, "sender")
	bystander := newSendWallet(t, "bystander")
	sw.connect()
	sw.fund([]uint64{100_000_000, 100_000_000}, nil)
	ctx := context.Background()

	memo := []byte("for the history")
	out, err := sw.w.Send(ctx, sw.account.ID, sw.seed, bystander.address(), 10_000_000, memo, 1_000_000)
	if err != nil || out.BroadcastErr != nil {
		t.Fatalf("send: %+v, %v", out, err)
	}
	self := sw.send(sw.address(), 5_000_000)
	harnessMine(t, 1)
	waitNodeTx(t, out.TxIDs[0], true)
	waitNodeTx(t, self.TxIDs[0], true)
	sw.waitInventory(0)
	sw.waitSynced(harnessHeight(t))

	r, tip, err := sw.w.Transaction(sw.account.ID, out.TxIDs[0])
	if err != nil || r == nil {
		t.Fatalf("Transaction: %+v, %v", r, err)
	}
	t.Logf("send at tip %d: %+v", tip, *r)
	if !r.CreatedByWallet || !r.RawPresent || r.Fee == nil || *r.Fee != 10_000 || r.BalanceDelta != -10_010_000 ||
		r.MinedHeight == nil || r.BlockTime == nil {
		t.Errorf("send record: %+v", r)
	}
	var outside, change *TxOutput
	for i := range r.Outputs {
		o := &r.Outputs[i]
		if o.Pool != OutputIronwood {
			t.Errorf("an output in the %s pool", o.Pool)
		}
		if o.IsChange {
			change = o
		} else if !o.ToWallet {
			outside = o
		}
	}
	if outside == nil || outside.Value != 10_000_000 || outside.Address != bystander.address() ||
		!bytes.HasPrefix(outside.Memo, memo) {
		t.Errorf("outside output: %+v", outside)
	}
	if change == nil || !change.ToWallet || change.Value != 100_000_000-10_010_000 {
		t.Errorf("change: %+v", change)
	}

	r, _, err = sw.w.Transaction(sw.account.ID, self.TxIDs[0])
	if err != nil || r == nil {
		t.Fatalf("Transaction: %+v, %v", r, err)
	}
	t.Logf("payment to itself: %+v", *r)
	if r.BalanceDelta != -10_000 {
		t.Errorf("a payment to itself changes the balance by %d", r.BalanceDelta)
	}
	// The view marks every output to the account of a transaction the
	// account funded as change, the payment to its own address included.
	// The payment carries the address it was made to; change does not.
	var paid *TxOutput
	for i := range r.Outputs {
		if r.Outputs[i].Address != "" {
			paid = &r.Outputs[i]
		}
	}
	if paid == nil || !paid.ToWallet || !paid.IsChange || paid.Value != 5_000_000 || paid.Address != sw.address() {
		t.Errorf("the paid output of a payment to itself: %+v", paid)
	}
}
