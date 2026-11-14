// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

// usedAddresses is a run of receive addresses of the account that testSeed
// derives, each paid once in index order. Every output but the last was
// spent again.
type usedAddresses struct {
	addresses []string
	// value is what the last address still holds.
	value uint64
	// scratch is a wallet of the same account, in which the transactions
	// are stored to learn their IDs.
	scratch backend
}

// newUsedAddresses derives the first n receive addresses of the test
// account. A wallet does not list an address until an address at most one
// gap below it has been used, so they are taken from a scratch wallet in
// which the last listed address is recorded as used until there are enough.
func newUsedAddresses(t *testing.T, kind backendKind, n int) *usedAddresses {
	t.Helper()
	scratch := newEnv(t, kind, 200, nil)
	account := scratch.createAccount(100)
	if err := scratch.be.updateChainTip(200); err != nil {
		t.Fatalf("updateChainTip: %v", err)
	}
	for round := 0; ; round++ {
		watched, err := scratch.be.transparentReceivers(account.ID)
		if err != nil {
			t.Fatalf("transparentReceivers: %v", err)
		}
		var receive []string
		for _, a := range watched {
			if !a.change {
				receive = append(receive, a.address)
			}
		}
		if len(receive) >= n {
			return &usedAddresses{addresses: receive[:n], value: 123_000, scratch: scratch.be}
		}
		txid := bytes.Repeat([]byte{0xbb}, 32)
		txid[0], txid[1] = byte(round), byte(round>>8)
		last := receive[len(receive)-1]
		if err := scratch.be.putUTXO(&utxo{txid: txid, script: p2pkhScript(last), value: 1, height: 110}); err != nil {
			t.Fatalf("putUTXO: %v", err)
		}
	}
}

// serve gives the server the history of the addresses: a transaction that
// pays each one, a transaction that spends the output of each one but the
// last, and the unspent output of the last.
func (u *usedAddresses) serve(t *testing.T, srv *fakeLwd) {
	t.Helper()
	type history struct {
		address string
		txs     []*walletrpc.RawTransaction
		txid    []byte
	}
	histories := make([]history, len(u.addresses))
	for i, address := range u.addresses {
		// The input is an output the wallet does not know.
		paying := testTx(
			[]testOutPoint{{txid: bytes.Repeat([]byte{0xaa}, 32), index: uint32(i)}},
			[]testOutput{{script: p2pkhScript(address), value: u.value}},
		)
		txid, err := u.scratch.decryptAndStoreTransaction(paying, nil)
		if err != nil {
			t.Fatalf("decryptAndStoreTransaction: %v", err)
		}
		h := history{address: address, txid: txid, txs: []*walletrpc.RawTransaction{{Data: paying, Height: 110}}}
		if i < len(u.addresses)-1 {
			spending := testTx(
				[]testOutPoint{{txid: txid}},
				[]testOutput{{script: []byte{0x51}, value: u.value - 1000}},
			)
			h.txs = append(h.txs, &walletrpc.RawTransaction{Data: spending, Height: 111})
		}
		histories[i] = h
	}
	srv.set(func(s *fakeLwd) {
		for _, h := range histories {
			s.addrTxs[h.address] = h.txs
		}
		last := histories[len(histories)-1]
		s.utxos[last.address] = []*walletrpc.GetAddressUtxosReply{{
			Address:  last.address,
			Txid:     last.txid,
			Script:   p2pkhScript(last.address),
			ValueZat: int64(u.value),
			Height:   110,
		}}
	})
}

// restore creates the test account in the wallet of e as a restored one.
func restoreTestAccount(t *testing.T, e *env) *Account {
	t.Helper()
	account, err := e.w.CreateAccount(context.Background(), &AccountParams{
		Name: "restored", Seed: testSeed, Birthday: 100, Restore: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	e.account = account
	return account
}

// historyQueries returns the watched addresses of the account of e that the
// server was asked the history of, in the order of the requests. The wallet
// database asks about other addresses of its own accord, its ephemeral
// ones, and those requests are left out.
func historyQueries(t *testing.T, e *env) []string {
	t.Helper()
	watched, err := e.be.transparentReceivers(e.account.ID)
	if err != nil {
		t.Fatalf("transparentReceivers: %v", err)
	}
	var asked []string
	e.srv.set(func(s *fakeLwd) {
		for _, q := range s.addrQueries {
			if slices.ContainsFunc(watched, func(a watchedAddress) bool { return a.address == q.Address }) {
				asked = append(asked, q.Address)
			}
		}
	})
	return asked
}

// transparentBalance returns the transparent balance of the account of e.
func transparentBalance(t *testing.T, e *env) uint64 {
	t.Helper()
	summary, err := e.w.Summary()
	if err != nil || summary == nil {
		t.Fatalf("Summary: %+v, %v", summary, err)
	}
	balance, _ := summary.Balance(e.account.ID)
	return balance.Transparent.Total
}

// TestWalkFindsFundsBehindSpentAddresses restores a seed whose first 29
// receive addresses were used and emptied, with funds at the 30th. Nothing
// in the first window of watched addresses is unspent, so the unspent
// outputs alone do not lead the wallet to the funds.
func TestWalkFindsFundsBehindSpentAddresses(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		const used = 30
		u := newUsedAddresses(t, kind, used)
		e := newEnv(t, kind, 200, func(cfg *Config) { cfg.MinConfirmations = 1 })
		u.serve(t, e.srv)
		account := restoreTestAccount(t, e)

		e.connect()
		e.waitSynced(200)
		eventually(t, "the funds at the last used address", func() bool {
			return transparentBalance(t, e) == u.value
		})

		// Every used address was asked about, and one run of unused ones
		// after the last on each chain: 30 + 10 on the receive chain and 5
		// on the change chain. None was asked about twice.
		asked := historyQueries(t, e)
		if len(asked) != used+10+5 {
			t.Errorf("%d history requests, want %d", len(asked), used+10+5)
		}
		sorted := slices.Clone(asked)
		slices.Sort(sorted)
		if len(slices.Compact(sorted)) != len(asked) {
			t.Errorf("an address was asked about twice: %v", asked)
		}
		for _, address := range u.addresses {
			if !slices.Contains(asked, address) {
				t.Errorf("address %s was not asked about", address)
			}
		}
		watched, err := e.be.transparentReceivers(account.ID)
		if err != nil || len(watched) != used+10+5 {
			t.Errorf("%d watched addresses, %v; want %d", len(watched), err, used+10+5)
		}
	})
}

// TestWalkOfAThousandAddresses counts the requests a restore makes when a
// thousand addresses were used.
func TestWalkOfAThousandAddresses(t *testing.T) {
	const used = 1000
	u := newUsedAddresses(t, fakeKind, used)
	e, _ := newFakeEnv(t, 200, nil)
	u.serve(t, e.srv)
	restoreTestAccount(t, e)

	e.connect()
	e.waitSynced(200)
	eventually(t, "the funds at the last used address", func() bool {
		return transparentBalance(t, e) == u.value
	})
	if asked := historyQueries(t, e); len(asked) != used+10+5 {
		t.Errorf("%d history requests, want %d", len(asked), used+10+5)
	}
}

// TestWalkAsksAboutTheUnusedRunOncePerConnection checks what the walk costs
// a wallet that is up to date: nothing more while it stays connected, and
// one run of unused addresses per chain when it connects again.
func TestWalkAsksAboutTheUnusedRunOncePerConnection(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		const used = 12
		u := newUsedAddresses(t, kind, used)
		e := newEnv(t, kind, 200, nil)
		u.serve(t, e.srv)
		account := restoreTestAccount(t, e)

		stop := e.connect()
		e.waitSynced(200)
		first := len(historyQueries(t, e))
		if first != used+10+5 {
			t.Fatalf("%d history requests on the first connection, want %d", first, used+10+5)
		}

		// New blocks bring no new requests.
		e.srv.mine(2)
		e.waitSynced(202)
		if n := len(historyQueries(t, e)); n != first {
			t.Errorf("%d history requests after two new blocks, want %d", n, first)
		}

		stop()
		e.srv.set(func(s *fakeLwd) { s.addrQueries = nil })
		e.statuses.reset()
		e.connect()
		e.waitSynced(202)

		watched, err := e.be.transparentReceivers(account.ID)
		if err != nil {
			t.Fatalf("transparentReceivers: %v", err)
		}
		var want []string
		for _, a := range watched {
			if a.unused {
				want = append(want, a.address)
			}
		}
		if len(want) != 15 {
			t.Fatalf("%d unused addresses, want 15", len(want))
		}
		asked := historyQueries(t, e)
		slices.Sort(want)
		slices.Sort(asked)
		if !slices.Equal(asked, want) {
			t.Errorf("the second connection asked about\n%v\nwant the unused run of each chain\n%v", asked, want)
		}
	})
}

// TestRefusedHistoryRequestsAreRetried checks that a server which refuses
// the history requests does not keep the wallet from syncing, and that the
// addresses are asked about again.
func TestRefusedHistoryRequestsAreRetried(t *testing.T) {
	const used = 12
	u := newUsedAddresses(t, fakeKind, used)
	e, _ := newFakeEnv(t, 200, nil)
	u.serve(t, e.srv)
	restoreTestAccount(t, e)
	e.srv.set(func(s *fakeLwd) {
		s.fail["GetTaddressTxids"] = status.Error(codes.InvalidArgument, "not today")
	})

	e.connect()
	e.waitSynced(200)
	if got := transparentBalance(t, e); got != 0 {
		t.Fatalf("transparent balance %d while the history requests are refused", got)
	}

	e.srv.set(func(s *fakeLwd) { delete(s.fail, "GetTaddressTxids") })
	eventually(t, "the funds at the last used address", func() bool {
		return transparentBalance(t, e) == u.value
	})
	if status := e.w.SyncStatus(); status.Err != nil {
		t.Errorf("status: %+v", status)
	}
}

// TestWalkAsksAgainAfterAnOutage checks that the unused addresses are asked
// about again when the wallet has lost and regained the server. While it was
// out of touch, the last address it watched was paid and emptied, and the
// last address that this brought into view was paid. No query for unspent
// outputs shows the first of the two.
func TestWalkAsksAgainAfterAnOutage(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		u := newUsedAddresses(t, kind, 20)
		u.addresses = []string{u.addresses[9], u.addresses[19]}
		e := newEnv(t, kind, 200, func(cfg *Config) { cfg.MinConfirmations = 1 })
		restoreTestAccount(t, e)

		e.connect()
		e.waitSynced(200)
		if n := len(historyQueries(t, e)); n != 15 {
			t.Fatalf("%d history requests before the outage, want 15", n)
		}

		e.srv.set(func(s *fakeLwd) { s.fail["GetLatestBlock"] = status.Error(codes.Unavailable, "down") })
		e.statuses.wait(t, "the outage", func(s SyncStatus) bool { return s.State == SyncDisconnected })
		u.serve(t, e.srv)
		e.statuses.reset()
		e.srv.set(func(s *fakeLwd) {
			s.mineLocked(20)
			delete(s.fail, "GetLatestBlock")
		})

		e.waitSynced(220)
		eventually(t, "the funds paid during the outage", func() bool {
			return transparentBalance(t, e) == u.value
		})
	})
}

// TestWalkAsksAboutAnAddressThatIsNoLongerUnused checks that an address
// whose history request failed is asked about again after a payment has
// made it a used one. The address holds two outputs. The server's list of
// unspent outputs has the later one only, as the answer to a query that
// begins above the earlier one does, and that takes the address out of the
// unused ones. The earlier output is only in its history.
func TestWalkAsksAboutAnAddressThatIsNoLongerUnused(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		const value = 70_000
		u := newUsedAddresses(t, kind, 1)
		address := u.addresses[0]
		e := newEnv(t, kind, 200, func(cfg *Config) { cfg.MinConfirmations = 1 })
		restoreTestAccount(t, e)

		var history []*walletrpc.RawTransaction
		var unspent []*walletrpc.GetAddressUtxosReply
		for i, height := range []uint64{120, 150} {
			tx := testTx(
				[]testOutPoint{{txid: bytes.Repeat([]byte{0xab}, 32), index: uint32(i)}},
				[]testOutput{{script: p2pkhScript(address), value: value}},
			)
			txid, err := u.scratch.decryptAndStoreTransaction(tx, nil)
			if err != nil {
				t.Fatalf("decryptAndStoreTransaction: %v", err)
			}
			history = append(history, &walletrpc.RawTransaction{Data: tx, Height: height})
			if i == 0 {
				continue
			}
			unspent = append(unspent, &walletrpc.GetAddressUtxosReply{
				Address: address, Txid: txid, Script: p2pkhScript(address), ValueZat: value, Height: height,
			})
		}
		e.srv.set(func(s *fakeLwd) {
			s.addrTxs[address] = history
			s.utxos[address] = unspent
			s.fail["GetTaddressTxids"] = status.Error(codes.InvalidArgument, "not today")
		})

		e.connect()
		e.waitSynced(200)
		// Only the later output so far, and the address is a used one.
		if got := transparentBalance(t, e); got != value {
			t.Fatalf("transparent balance %d while the history requests are refused, want %d", got, value)
		}
		watched, err := e.be.transparentReceivers(e.account.ID)
		if err != nil || watched[0].address != address || watched[0].unused {
			t.Fatalf("the first watched address: %+v, %v", watched[0], err)
		}

		e.srv.set(func(s *fakeLwd) { delete(s.fail, "GetTaddressTxids") })
		eventually(t, "the earlier output", func() bool {
			return transparentBalance(t, e) == 2*value
		})
	})
}

// TestWalkStartsAtTheBirthday checks that the history of an address is asked
// for from the birthday of its account on. The first address of the restored
// account was paid below the birthday. The wallet does not learn of it: the
// address stays an unused one, and the wallet watches no further.
func TestWalkStartsAtTheBirthday(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		u := newUsedAddresses(t, kind, 1)
		address := u.addresses[0]
		e := newEnv(t, kind, 200, func(cfg *Config) { cfg.MinConfirmations = 1 })
		account := restoreTestAccount(t, e)
		tx := testTx(
			[]testOutPoint{{txid: bytes.Repeat([]byte{0xac}, 32)}},
			[]testOutput{{script: p2pkhScript(address), value: 70_000}},
		)
		e.srv.set(func(s *fakeLwd) {
			s.addrTxs[address] = []*walletrpc.RawTransaction{{Data: tx, Height: uint64(account.Birthday) - 1}}
		})

		e.connect()
		e.waitSynced(200)
		asked := historyQueries(t, e)
		if len(asked) != 15 || !slices.Contains(asked, address) {
			t.Fatalf("history requests: %v", asked)
		}
		e.srv.set(func(s *fakeLwd) {
			for _, q := range s.addrQueries {
				if !slices.Contains(asked, q.Address) {
					continue
				}
				if q.Range.Start.Height != uint64(account.Birthday) || q.Range.End.Height != 200 {
					t.Errorf("the history of %s was asked for from %d to %d, want %d to 200",
						q.Address, q.Range.Start.Height, q.Range.End.Height, account.Birthday)
				}
			}
		})
		watched, err := e.be.transparentReceivers(account.ID)
		if err != nil || len(watched) != 15 || watched[0].address != address || !watched[0].unused {
			t.Errorf("%d watched addresses, the first %+v, %v", len(watched), watched[0], err)
		}
		if got := transparentBalance(t, e); got != 0 {
			t.Errorf("transparent balance %d", got)
		}
	})
}

// TestWalkWaitsForTheBirthday checks that the walk asks nothing about the
// addresses of an account that begins above the server's tip, and asks once
// the tip has reached the birthday.
func TestWalkWaitsForTheBirthday(t *testing.T) {
	e, _ := newFakeEnv(t, 150, nil)
	account := e.createAccount(140)
	srv := newFakeLwd(t, 150)
	now := time.Unix(1_700_000_000, 0)
	p := newTestProcessor(t, srv, e.be.(*fakeBackend), &now)

	walked, err := p.walkAddresses(context.Background(), account.Birthday-1)
	if err != nil || walked != 0 || srv.callCount("GetTaddressTxids") != 0 {
		t.Fatalf("below the birthday: %d addresses walked, %d requests, %v", walked, srv.callCount("GetTaddressTxids"), err)
	}
	walked, err = p.walkAddresses(context.Background(), account.Birthday)
	if err != nil || walked != 15 || srv.callCount("GetTaddressTxids") != 15 {
		t.Fatalf("at the birthday: %d addresses walked, %d requests, %v", walked, srv.callCount("GetTaddressTxids"), err)
	}
	srv.set(func(s *fakeLwd) {
		for _, q := range s.addrQueries {
			if q.Range.Start.Height != uint64(account.Birthday) || q.Range.End.Height != uint64(account.Birthday) {
				t.Errorf("the history of %s was asked for from %d to %d", q.Address, q.Range.Start.Height, q.Range.End.Height)
			}
		}
	})
}

// TestWalkSkipsATransactionItCannotStore checks that a transaction the
// wallet cannot read does not hold up the walk or fail the sync.
func TestWalkSkipsATransactionItCannotStore(t *testing.T) {
	e, be := newFakeEnv(t, 150, nil)
	account := e.createAccount(100)
	address := fakeAddress(account.ID, false, 3)
	e.srv.set(func(s *fakeLwd) {
		s.addrTxs[address] = []*walletrpc.RawTransaction{{Data: []byte{1, 2, 3}, Height: 120}}
	})
	be.set(func(b *fakeBackend) {
		b.fail["decryptAndStoreTransaction"] = wrapf(ErrInvalidArgument, "not a transaction")
	})

	e.connect()
	e.waitSynced(150)
	if status := e.w.SyncStatus(); status.Err != nil {
		t.Errorf("status: %+v", status)
	}
	// Every address of the unused runs was asked about once, the one with
	// the unreadable transaction included.
	if asked := historyQueries(t, e); len(asked) != 15 || !slices.Contains(asked, address) {
		t.Errorf("history requests: %v", asked)
	}
}
