// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// plantFunded makes the server of e report an unspent output of value at
// address, mined at height, and serve its funding transaction, which spends
// an output no wallet knows, distinguished by salt. It returns the
// transaction ID, as the backend kind computes it.
func plantFunded(t *testing.T, e *env, kind backendKind, address string, value int64, height uint64, salt uint32) []byte {
	t.Helper()
	return plantOutput(t, e, kind, address, value, height, testOutPoint{txid: bytes.Repeat([]byte{0x55}, 32), index: salt})
}

// plantCoinbase is plantFunded for a coinbase transaction: its one input
// has the null outpoint of a coinbase.
func plantCoinbase(t *testing.T, e *env, kind backendKind, address string, value int64, height uint64) []byte {
	t.Helper()
	return plantOutput(t, e, kind, address, value, height, testOutPoint{txid: make([]byte, 32), index: 0xffffffff})
}

func plantOutput(t *testing.T, e *env, kind backendKind, address string, value int64, height uint64, spends testOutPoint) []byte {
	t.Helper()
	raw := testTx(
		[]testOutPoint{spends},
		[]testOutput{{script: p2pkhScript(address), value: uint64(value)}},
	)
	scratch := kind.open(t)
	// The Rust backend reads a transaction only once it knows a chain tip.
	if err := scratch.updateChainTip(uint32(height)); err != nil {
		t.Fatalf("updateChainTip: %v", err)
	}
	txid, err := scratch.decryptAndStoreTransaction(raw, nil, nil)
	if err != nil {
		t.Fatalf("computing the transaction ID: %v", err)
	}
	e.srv.set(func(s *fakeLwd) {
		s.txs[string(txid)] = &walletrpc.RawTransaction{Data: raw, Height: height}
		s.utxos[address] = append(s.utxos[address], &walletrpc.GetAddressUtxosReply{
			Address: address, Txid: txid, Script: p2pkhScript(address), ValueZat: value, Height: height,
		})
	})
	return txid
}

// fundEnv returns an environment with an account born at 100 on a chain of
// 150 blocks, and the account's first receive address.
func fundEnv(t *testing.T, kind backendKind) (*env, string) {
	t.Helper()
	e := newEnv(t, kind, 150, func(cfg *Config) { cfg.MinConfirmations = 1 })
	account := e.createAccount(100)
	addresses, err := e.w.TransparentAddresses(account.ID)
	if err != nil || len(addresses) == 0 {
		t.Fatalf("TransparentAddresses: %v, %v", addresses, err)
	}
	e.w.tune = func(o *syncOptions) {
		o.minBackoff = 10 * time.Millisecond
		o.maxBackoff = 40 * time.Millisecond
	}
	return e, addresses[0]
}

// An unspent output is recorded only once its funding transaction is
// stored. While the server cannot give the transaction, the output waits,
// and both the output query and the fetch are made again.
func TestAnOutputWaitsForItsFundingTransaction(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e, address := fundEnv(t, kind)
		txid := plantFunded(t, e, kind, address, 70_000, 120, 1)
		var funding *walletrpc.RawTransaction
		e.srv.set(func(s *fakeLwd) {
			funding = s.txs[string(txid)]
			delete(s.txs, string(txid))
		})

		e.connect()
		e.waitSynced(150)
		gets, queries := e.srv.callCount("GetTransaction"), e.srv.callCount("GetAddressUtxosStream")
		eventually(t, "the fetch and the query repeated", func() bool {
			return e.srv.callCount("GetTransaction") > gets && e.srv.callCount("GetAddressUtxosStream") > queries
		})
		if got := transparentBalance(t, e); got != 0 {
			t.Fatalf("an output was recorded without its funding transaction: %d", got)
		}

		e.srv.set(func(s *fakeLwd) { s.txs[string(txid)] = funding })
		eventually(t, "the output recorded", func() bool { return transparentBalance(t, e) == 70_000 })
		if missing, err := e.be.missingTransactions([][]byte{txid}); err != nil || len(missing) != 0 {
			t.Errorf("the funding transaction is not stored: %v, %v", missing, err)
		}
		// Once stored, the query is not repeated in every cycle. The cycle
		// that recorded the output may still be under way, and the output
		// makes its address a used one, which extends the watched
		// addresses and asks the query once more. Two cycles are let start
		// first, and three more must ask nothing.
		waitCycles(t, e, 2)
		queries = e.srv.callCount("GetAddressUtxosStream")
		gets = e.srv.callCount("GetTransaction")
		waitCycles(t, e, 3)
		if n := e.srv.callCount("GetAddressUtxosStream") - queries; n != 0 {
			t.Errorf("the output query was repeated %d times in 3 cycles with nothing new", n)
		}
		if n := e.srv.callCount("GetTransaction") - gets; n != 0 {
			t.Errorf("the funding transaction was fetched %d more times", n)
		}
	})
}

// A connection that fails while a funding transaction is fetched does not
// end the sync attempt. The wallet is synced, the failure is reported apart
// from that of the attempt, and the output is recorded once the server
// answers again.
func TestAFundingFetchThatLosesTheConnection(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e, address := fundEnv(t, kind)
		plantFunded(t, e, kind, address, 70_000, 120, 1)
		e.srv.set(func(s *fakeLwd) {
			s.fail["GetTransaction"] = status.Error(codes.Unavailable, "the connection was lost")
		})

		e.connect()
		failed := e.statuses.wait(t, "synced with the failed fetch reported", func(s SyncStatus) bool {
			return s.State == SyncSynced && s.TransparentErr != nil
		})
		if !isConnectionError(failed.TransparentErr) || failed.Err != nil {
			t.Fatalf("status while the fetch fails: %+v", failed)
		}
		if got := transparentBalance(t, e); got != 0 {
			t.Fatalf("an output was recorded without its funding transaction: %d", got)
		}
		for _, s := range e.statuses.all() {
			if s.Err != nil || s.State == SyncDisconnected {
				t.Fatalf("the failed fetch failed the sync attempt: %+v", s)
			}
		}

		e.srv.set(func(s *fakeLwd) { delete(s.fail, "GetTransaction") })
		eventually(t, "the output recorded", func() bool { return transparentBalance(t, e) == 70_000 })
		e.statuses.wait(t, "the error to be cleared", func(s SyncStatus) bool {
			return s.State == SyncSynced && s.TransparentErr == nil && s.Err == nil
		})
	})
}

// The state of an output that waits for its funding transaction is kept
// across a failure of the connection. An unknown funding transaction leaves
// the output unrecorded and its fetch deferred. A later fetch that loses the
// connection changes neither, and its error is reported in one error with
// the unrecorded output. While the requests for the transparent addresses are
// put off, blocks are scanned and no address or funding request is made. Once
// the output has left the listing, nothing waits.
func TestAnUnrecordedOutputOutlastsALostConnection(t *testing.T) {
	srv := newFakeLwd(t, 150)
	be := newFakeBackend()
	account := testAccount(t, be, 1)
	now := time.Unix(1_700_000_000, 0)
	p := newTestProcessor(t, srv, be, &now)
	var st SyncStatus
	p.update = func(f func(*SyncStatus)) { f(&st) }
	ctx := context.Background()

	address := fakeAddress(account.ID, false, 0)
	id := bytes.Repeat([]byte{0xd1}, 32)
	srv.set(func(s *fakeLwd) {
		s.utxos[address] = []*walletrpc.GetAddressUtxosReply{{
			Address: address, Txid: id, Script: p2pkhScript(address), ValueZat: 1_000, Height: 120,
		}}
	})
	// cycle runs one sync cycle, which must not fail, and checks the height
	// scanned up to.
	cycle := func(wantScanned uint32) {
		t.Helper()
		if again, err := p.cycle(ctx); again || err != nil {
			t.Fatalf("cycle: %t, %v", again, err)
		}
		if st.FullyScanned != wantScanned {
			t.Fatalf("scanned up to %d, want %d", st.FullyScanned, wantScanned)
		}
	}
	// waiting checks that the output is unrecorded and its fetch deferred.
	waiting := func(when string) {
		t.Helper()
		if err := p.unrecordedErr(); !errors.Is(err, ErrOutputUnrecorded) {
			t.Fatalf("%s: unrecorded: %v", when, err)
		}
		if _, ok := p.fundDeferred[string(id)]; !ok || len(p.fundDeferred) != 1 {
			t.Fatalf("%s: deferred: %v", when, p.fundDeferred)
		}
	}
	requests := func() int {
		return srv.callCount("GetAddressUtxosStream") + srv.callCount("GetTaddressTxids") + srv.callCount("GetTransaction")
	}

	// The server does not know the funding transaction. The unrecorded
	// output is reported alone.
	cycle(150)
	waiting("an unknown funding transaction")
	if !errors.Is(st.TransparentErr, ErrOutputUnrecorded) || isServerError(st.TransparentErr) {
		t.Fatalf("TransparentErr: %v", st.TransparentErr)
	}

	// The fetch is due again, and the connection fails during it. The two
	// errors are reported as one.
	now = now.Add(2 * time.Minute)
	srv.set(func(s *fakeLwd) {
		s.fail["GetTransaction"] = status.Error(codes.Unavailable, "the connection was lost")
	})
	srv.mine(1)
	fetches := srv.callCount("GetTransaction")
	cycle(151)
	if n := srv.callCount("GetTransaction") - fetches; n != 1 {
		t.Fatalf("%d fetches of the funding transaction, want 1", n)
	}
	waiting("a lost connection")
	if !isConnectionError(st.TransparentErr) || !errors.Is(st.TransparentErr, ErrOutputUnrecorded) {
		t.Fatalf("TransparentErr: %v", st.TransparentErr)
	}
	if st.Err != nil {
		t.Fatalf("Err: %v", st.Err)
	}

	// Within the delay of the transparent requests, blocks are scanned and
	// neither the addresses nor the transaction are asked about.
	now = now.Add(30 * time.Second)
	srv.mine(1)
	before := requests()
	cycle(152)
	if n := requests() - before; n != 0 {
		t.Fatalf("%d transparent requests were made within their delay", n)
	}
	waiting("the delay")
	if !isConnectionError(st.TransparentErr) || !errors.Is(st.TransparentErr, ErrOutputUnrecorded) {
		t.Fatalf("TransparentErr within the delay: %v", st.TransparentErr)
	}

	// The delay has passed, the server answers, and the output has left
	// the listing: nothing waits.
	now = now.Add(time.Minute)
	srv.set(func(s *fakeLwd) {
		delete(s.fail, "GetTransaction")
		s.utxos = make(map[string][]*walletrpc.GetAddressUtxosReply)
	})
	before = requests()
	cycle(152)
	if requests() == before {
		t.Fatal("no transparent request was made after the delay")
	}
	if len(p.fundDeferred) != 0 || p.unrecordedErr() != nil || st.TransparentErr != nil {
		t.Fatalf("after the output left: deferred %d, unrecorded %v, TransparentErr %v",
			len(p.fundDeferred), p.unrecordedErr(), st.TransparentErr)
	}
}

// SyncStatus.TransparentErr is the failure of an update, the outputs that
// could not be recorded, or both in one error that matches each and reads as
// one line that begins with the failure.
func TestTransparentErrCombines(t *testing.T) {
	p := new(processor)
	if err := p.transparentErr(nil); err != nil {
		t.Fatalf("nothing to report: %v", err)
	}
	var failed error = &serverError{"GetAddressUtxosStream", status.Error(codes.Unavailable, "the connection was lost")}
	if err := p.transparentErr(failed); err != failed {
		t.Fatalf("a failure alone: %v, want the failure itself", err)
	}
	p.unrecorded = map[string]error{"account": wrapf(ErrOutputUnrecorded, "1 of the 2 unspent outputs")}
	alone := p.transparentErr(nil)
	if !errors.Is(alone, ErrOutputUnrecorded) || isServerError(alone) {
		t.Fatalf("an unrecorded output alone: %v", alone)
	}
	both := p.transparentErr(failed)
	if !errors.Is(both, ErrOutputUnrecorded) || !errors.Is(both, ErrServerUnreachable) || !isConnectionError(both) {
		t.Fatalf("both: %v", both)
	}
	if want := failed.Error() + "; " + alone.Error(); both.Error() != want {
		t.Fatalf("both read %q, want %q", both.Error(), want)
	}
}

// waitCycles waits until n more sync cycles have started. Each cycle asks
// the server for its chain tip once.
func waitCycles(t *testing.T, e *env, n int) {
	t.Helper()
	start := e.srv.callCount("GetLatestBlock")
	eventually(t, fmt.Sprintf("%d more cycles", n), func() bool { return e.srv.callCount("GetLatestBlock") >= start+n })
}

// waitUnrecorded waits for a synced status that reports an unrecorded
// output, or with want false, one that reports none.
func waitUnrecorded(t *testing.T, e *env, want bool) {
	t.Helper()
	e.statuses.wait(t, fmt.Sprintf("synced with an unrecorded output: %t", want), func(s SyncStatus) bool {
		return s.State == SyncSynced && s.Err == nil &&
			errors.Is(s.TransparentErr, ErrOutputUnrecorded) == want && (want || s.TransparentErr == nil)
	})
}

// A server that answers the request for one funding transaction with
// another valid transaction: nothing is stored or recorded for the listed
// output until the server gives the right one.
func TestAFundingFetchAnsweredWithAnotherTransaction(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e, address := fundEnv(t, kind)
		listed := plantFunded(t, e, kind, address, 70_000, 120, 11)
		other := testTx(
			[]testOutPoint{{txid: bytes.Repeat([]byte{0x66}, 32)}},
			[]testOutput{{script: p2pkhScript(address), value: 70_000}},
		)
		var funding *walletrpc.RawTransaction
		e.srv.set(func(s *fakeLwd) {
			funding = s.txs[string(listed)]
			s.txs[string(listed)] = &walletrpc.RawTransaction{Data: other, Height: 120}
		})
		e.connect()
		e.waitSynced(150)
		waitUnrecorded(t, e, true)
		if got := transparentBalance(t, e); got != 0 {
			t.Fatalf("recorded from another transaction: %d", got)
		}
		if missing, err := e.be.missingTransactions([][]byte{listed}); err != nil || len(missing) != 1 {
			t.Errorf("the listed transaction is held: %v, %v", missing, err)
		}

		e.srv.set(func(s *fakeLwd) { s.txs[string(listed)] = funding })
		eventually(t, "the output recorded", func() bool { return transparentBalance(t, e) == 70_000 })
		waitUnrecorded(t, e, false)
	})
}

// An output the server lists that its transaction does not have, by value,
// index or script, is not recorded.
func TestAListedOutputTheTransactionDoesNotHave(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(u *walletrpc.GetAddressUtxosReply, other string)
	}{
		{"value", func(u *walletrpc.GetAddressUtxosReply, _ string) { u.ValueZat = 60_000 }},
		{"index", func(u *walletrpc.GetAddressUtxosReply, _ string) { u.Index = 1 }},
		{"script", func(u *walletrpc.GetAddressUtxosReply, other string) {
			u.Address, u.Script = other, p2pkhScript(other)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, kind backendKind) {
				e, address := fundEnv(t, kind)
				addresses, err := e.w.TransparentAddresses(e.account.ID)
				if err != nil || len(addresses) < 2 {
					t.Fatalf("TransparentAddresses: %v, %v", addresses, err)
				}
				plantFunded(t, e, kind, address, 70_000, 120, 12)
				var right *walletrpc.GetAddressUtxosReply
				e.srv.set(func(s *fakeLwd) {
					right = s.utxos[address][0]
					wrong := proto.Clone(right).(*walletrpc.GetAddressUtxosReply)
					tc.edit(wrong, addresses[1])
					delete(s.utxos, address)
					s.utxos[wrong.Address] = []*walletrpc.GetAddressUtxosReply{wrong}
				})
				e.connect()
				e.waitSynced(150)
				waitUnrecorded(t, e, true)
				// The Rust backend records the true output when it stores
				// the transaction. The listed one is never added to it.
				if got := transparentBalance(t, e); got != 0 && got != 70_000 {
					t.Fatalf("a mismatched output was recorded: %d", got)
				}
				e.srv.set(func(s *fakeLwd) {
					s.utxos = map[string][]*walletrpc.GetAddressUtxosReply{address: {right}}
				})
				e.srv.mine(1)
				waitUnrecorded(t, e, false)
				if got := transparentBalance(t, e); got != 70_000 {
					t.Fatalf("balance after the right listing: %d", got)
				}
			})
		})
	}
}

// The fetch of a funding transaction has to agree with the listing of its
// output. A transaction the server reports in its mempool, on a fork or at
// another height was moved after the listing, and nothing is recorded at
// the listed height.
func TestAFundingFetchThatDisagreesWithTheListing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		height uint64
	}{
		{"mempool", 0},
		{"fork", math.MaxUint64},
		{"other height", 121},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, kind backendKind) {
				e, address := fundEnv(t, kind)
				txid := plantFunded(t, e, kind, address, 70_000, 120, 13)
				e.srv.set(func(s *fakeLwd) { s.txs[string(txid)].Height = tc.height })
				e.connect()
				e.waitSynced(150)
				waitUnrecorded(t, e, true)
				if got := transparentBalance(t, e); got != 0 {
					t.Fatalf("recorded at the listed height: %d", got)
				}
				if missing, err := e.be.missingTransactions([][]byte{txid}); err != nil || len(missing) != 1 {
					t.Errorf("stored although the fetch disagreed: %v, %v", missing, err)
				}
				e.srv.set(func(s *fakeLwd) { s.txs[string(txid)].Height = 120 })
				eventually(t, "the output recorded", func() bool { return transparentBalance(t, e) == 70_000 })
				waitUnrecorded(t, e, false)
			})
		})
	}
}

// A reorganization between the output query and the fetch: the server lists
// the output, then no longer knows its transaction. The output is not
// recorded. When the chain drops the output, nothing is left behind, and
// when it comes back with its transaction, it is recorded.
func TestReorganizationBetweenTheQueryAndTheFetch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e, address := fundEnv(t, kind)
		txid := plantFunded(t, e, kind, address, 70_000, 120, 2)
		var funding *walletrpc.RawTransaction
		var listed []*walletrpc.GetAddressUtxosReply
		// The output is listed when asked, and its transaction is gone by
		// the time it is fetched.
		e.srv.set(func(s *fakeLwd) {
			funding = s.txs[string(txid)]
			delete(s.txs, string(txid))
			listed = s.utxos[address]
		})
		e.connect()
		e.waitSynced(150)
		eventually(t, "a failed fetch", func() bool { return e.srv.callCount("GetTransaction") >= 1 })
		if got := transparentBalance(t, e); got != 0 {
			t.Fatalf("recorded during the reorganization: %d", got)
		}

		// The new chain does not have the output.
		e.srv.set(func(s *fakeLwd) { delete(s.utxos, address) })
		e.srv.mine(1)
		e.waitSynced(151)
		if got := transparentBalance(t, e); got != 0 {
			t.Fatalf("recorded after the reorganization: %d", got)
		}

		// It is mined again, and its transaction is served.
		e.srv.set(func(s *fakeLwd) {
			s.utxos[address] = listed
			s.txs[string(txid)] = funding
		})
		e.srv.mine(1)
		eventually(t, "the output recorded", func() bool { return transparentBalance(t, e) == 70_000 })
	})
}

// A funding transaction that the address walk stored is not fetched again.
func TestAWalkedTransactionIsNotFetchedAgain(t *testing.T) {
	forEachBackend(t, func(t *testing.T, kind backendKind) {
		e, address := fundEnv(t, kind)
		txid := plantFunded(t, e, kind, address, 70_000, 120, 3)
		// The walk of the still unused address returns the transaction.
		e.srv.set(func(s *fakeLwd) { s.addrTxs[address] = []*walletrpc.RawTransaction{s.txs[string(txid)]} })
		e.connect()
		e.waitSynced(150)
		eventually(t, "the output recorded", func() bool { return transparentBalance(t, e) == 70_000 })
		// The wallet can ask for other transactions, such as the one an
		// input of this transaction spends. This one it already holds.
		e.srv.set(func(s *fakeLwd) {
			for _, q := range s.txQueries {
				if bytes.Equal(q, txid) {
					t.Errorf("the walked transaction was fetched")
				}
			}
		})
	})
}
