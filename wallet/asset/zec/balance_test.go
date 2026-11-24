// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"sync"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

func summaryOf(iw, tr, cb zeclib.PoolBalance) *zeclib.Summary {
	return &zeclib.Summary{Balances: []zeclib.AccountBalance{{
		AccountID: "acct-0", Ironwood: iw, Transparent: tr, Coinbase: cb,
		Total: iw.Total + tr.Total + cb.Total,
	}}}
}

func TestBalanceOf(t *testing.T) {
	for _, tc := range []struct {
		name      string
		summary   *zeclib.Summary
		spendable zeclib.SpendableValue
		want      asset.Balance
		left      leftOut
	}{{
		name: "no summary yet",
		want: asset.Balance{},
	}, {
		name:      "spendable",
		summary:   summaryOf(zeclib.PoolBalance{Spendable: 100, Total: 100}, zeclib.PoolBalance{Spendable: 50_000, Total: 50_000}, zeclib.PoolBalance{}),
		spendable: zeclib.SpendableValue{Ironwood: 100, Transparent: 50_000},
		want:      asset.Balance{Available: 50_100},
	}, {
		name: "pending change and received value",
		summary: summaryOf(
			zeclib.PoolBalance{Spendable: 100, ChangePending: 40, ValuePending: 60, Total: 200},
			zeclib.PoolBalance{ChangePending: 7_000, ValuePending: 8_000, Total: 15_000},
			zeclib.PoolBalance{}),
		spendable: zeclib.SpendableValue{Ironwood: 100},
		want:      asset.Balance{Available: 100, Immature: 100 + 15_000},
	}, {
		// The summary counts a note as spendable before selection does,
		// for example a change note ahead of the anchor.
		name:      "the summary ahead of selection",
		summary:   summaryOf(zeclib.PoolBalance{Spendable: 300, Total: 300}, zeclib.PoolBalance{}, zeclib.PoolBalance{}),
		spendable: zeclib.SpendableValue{Ironwood: 200},
		want:      asset.Balance{Available: 200, Immature: 100},
	}, {
		name: "locked",
		summary: summaryOf(
			zeclib.PoolBalance{Spendable: 100, Locked: 30, Total: 130},
			zeclib.PoolBalance{Locked: 9_000, Total: 9_000}, zeclib.PoolBalance{}),
		spendable: zeclib.SpendableValue{Ironwood: 100},
		want:      asset.Balance{Available: 100, Locked: 9_030},
	}, {
		name: "coinbase and uneconomic outputs in no balance",
		summary: summaryOf(zeclib.PoolBalance{},
			zeclib.PoolBalance{Spendable: 10_000 + 4_000, Uneconomic: 2_000, Total: 14_000},
			zeclib.PoolBalance{Spendable: 625_000_000, Total: 625_000_000}),
		spendable: zeclib.SpendableValue{Transparent: 10_000},
		want:      asset.Balance{Available: 10_000},
		left:      leftOut{coinbase: 625_000_000, uneconomic: 6_000},
	}, {
		// Spendable above the pool's total, which a stale read could give,
		// is no negative Immature.
		name:      "the clamp",
		summary:   summaryOf(zeclib.PoolBalance{Spendable: 100, Total: 100}, zeclib.PoolBalance{}, zeclib.PoolBalance{}),
		spendable: zeclib.SpendableValue{Ironwood: 150},
		want:      asset.Balance{Available: 150},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, left := balanceOf(tc.summary, "acct-0", tc.spendable)
			if !sameBalance(&got, &tc.want) || left != tc.left {
				t.Errorf("balance %+v left %+v, want %+v left %+v", got, left, tc.want, tc.left)
			}
		})
	}
}

// The balance is cached, refreshed on a change and on a new tip, and
// reported to core only when it changed.
func TestBalanceRefresh(t *testing.T) {
	e := newEnv(t)
	e.connect(e.w)
	be := e.o.latest()
	be.set(func(b *fakeBackend) {
		b.summary = summaryOf(zeclib.PoolBalance{Spendable: 100, Total: 100}, zeclib.PoolBalance{}, zeclib.PoolBalance{})
		b.spendable = zeclib.SpendableValue{Ironwood: 100}
	})
	var balances []uint64
	collect := func() []uint64 {
		for _, note := range e.notes.drain() {
			if n, ok := note.(*asset.BalanceChangeNote); ok {
				balances = append(balances, n.Balance.Available)
			}
		}
		return balances
	}
	be.emitChange()
	// The zero balance read at opening, then 100.
	eventually(t, "the note of the new balance", func() bool { return len(collect()) == 2 })
	if balances[0] != 0 || balances[1] != 100 {
		t.Errorf("balance notes %v", balances)
	}
	if b, _ := e.w.Balance(); b.Available != 100 {
		t.Errorf("Balance %+v", b)
	}
	be.emitChange()
	be.emitStatus(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 151})
	eventually(t, "the tip read", func() bool { return e.w.cache.chainTip() == 151 })
	// A status handled after the tip's makes sure the tip's refresh is done.
	be.emitStatus(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 151, ScanProgress: zeclib.Ratio{Numerator: 1}})
	if n := len(collect()); n != 2 {
		t.Errorf("%d balance notes without a change", n-2)
	}
	// A new tip makes outputs spendable.
	be.set(func(b *fakeBackend) { b.spendable = zeclib.SpendableValue{Ironwood: 100, Transparent: 9_000} })
	be.emitStatus(zeclib.SyncStatus{State: zeclib.SyncSynced, ChainTip: 152})
	eventually(t, "the balance at the new tip", func() bool {
		b := collect()
		return len(b) == 3 && b[2] == 9_100
	})
}

// A refresh that read zeclib before another does not overwrite the later
// one's result, whichever stores first.
func TestBalanceRefreshOrder(t *testing.T) {
	var c cache
	c.reset()
	early, late := c.next(), c.next()
	if !c.storeBalance(late, asset.Balance{Available: 2}, nil) {
		t.Fatal("the later refresh was not stored")
	}
	if c.storeBalance(early, asset.Balance{Available: 1}, nil) || c.heldBalance().Available != 2 {
		t.Fatalf("the earlier refresh overwrote the later one: %+v", c.heldBalance())
	}

	c.reset()
	early, late = c.next(), c.next()
	c.storeBalance(early, asset.Balance{Available: 1}, nil)
	c.storeBalance(late, asset.Balance{Available: 2}, nil)
	if c.heldBalance().Available != 2 {
		t.Fatalf("held %+v", c.heldBalance())
	}

	// Concurrent refreshes keep the highest number's result.
	c.reset()
	var wg sync.WaitGroup
	for range 50 {
		seq := c.next()
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.storeBalance(seq, asset.Balance{Available: seq}, nil)
		}()
	}
	wg.Wait()
	if got, want := c.heldBalance().Available, c.seq.Load(); got != want {
		t.Errorf("held the result of refresh %d, want %d", got, want)
	}
}
