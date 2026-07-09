// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"sync"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
)

// stubProber marks a fixed set of addresses used, each with an earliest
// confirmed height.
type stubProber struct {
	mtx  sync.Mutex
	used map[string]int32 // addr -> earliest confirmed height
}

func (s *stubProber) ProbeAddress(addr btcutil.Address) (bool, int32, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	earliest, ok := s.used[addr.String()]
	return ok, earliest, nil
}

// TestRecoverViaScanSeedsSweep verifies that gap-limit discovery on an aged
// wallet seeds each discovered address into the fast sweep tier with a floor
// below its earliest known activity. Discovered addresses have history the
// wallet never ingested, so startup classification misfiles them — no
// last-active entry reads as quiet, and a never-ingested credit reads as
// unfunded — and the catch-up rescan, which starts at SyncedTo, cannot reach
// history below it. The sweep seed is what makes the funds visible. On a
// fresh wallet (empty index) nothing is seeded: the full-range rescan covers
// discoveries by itself.
func TestRecoverViaScanSeedsSweep(t *testing.T) {
	run := func(t *testing.T, aged bool) {
		w, cleanup := testWallet(t)
		defer cleanup()

		if aged {
			// Any last-active entry makes the wallet "aged": the watch
			// set is filtered and the rescan is a catch-up.
			err := walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
				return putLastActive(tx, []byte{0x51}, 1000)
			})
			if err != nil {
				t.Fatalf("putLastActive: %v", err)
			}
		}

		// The used addresses sit past the pool top on both branches: the
		// same-seed-loaded-elsewhere shape discovery exists to find.
		scoped, err := w.Manager.FetchScopedKeyManager(w.ScopeForPurpose(84))
		if err != nil {
			t.Fatalf("FetchScopedKeyManager: %v", err)
		}
		extAddr, err := w.deriveScanAddr(scoped, waddrmgr.ExternalBranch, 3)
		if err != nil {
			t.Fatalf("deriveScanAddr: %v", err)
		}
		intAddr, err := w.deriveScanAddr(scoped, waddrmgr.InternalBranch, 2)
		if err != nil {
			t.Fatalf("deriveScanAddr: %v", err)
		}
		prober := &stubProber{used: map[string]int32{
			extAddr.String(): 700,
			intAddr.String(): 850,
		}}

		if err := w.recoverViaScan(prober, 5); err != nil {
			t.Fatalf("recoverViaScan: %v", err)
		}

		p := &w.watchPol
		p.mtx.Lock()
		extEntry := p.cold[extAddr.String()]
		intEntry := p.cold[intAddr.String()]
		coldN := len(p.cold)
		p.mtx.Unlock()

		if !aged {
			if coldN != 0 {
				t.Fatalf("fresh wallet seeded %d cold entries, want none", coldN)
			}
			return
		}
		if extEntry == nil || intEntry == nil {
			t.Fatalf("discoveries not seeded: ext=%v int=%v", extEntry, intEntry)
		}
		if extEntry.slow || intEntry.slow {
			t.Fatalf("discoveries seeded slow (ext=%v int=%v), want fast tier",
				extEntry.slow, intEntry.slow)
		}
		if extEntry.upTo != 699 || intEntry.upTo != 849 {
			t.Fatalf("floors = %d/%d, want just below earliest activity (699/849)",
				extEntry.upTo, intEntry.upTo)
		}
		if extEntry.internal || !intEntry.internal {
			t.Fatalf("branch flags = ext.internal=%v int.internal=%v",
				extEntry.internal, intEntry.internal)
		}

		// The seeds are durable: discovery only probes above the pool top,
		// so a restart before the first successful sweep must re-apply
		// them from disk. Simulate the restart with an emptied policy and
		// a classification pass.
		extPk, perr := txscript.PayToAddrScript(extAddr)
		if perr != nil {
			t.Fatalf("PayToAddrScript: %v", perr)
		}
		intPk, perr := txscript.PayToAddrScript(intAddr)
		if perr != nil {
			t.Fatalf("PayToAddrScript: %v", perr)
		}
		_ = walletdb.View(w.db, func(tx walletdb.ReadTx) error {
			if f, ok := seedFloor(tx, extPk); !ok || f != 699 {
				t.Fatalf("persisted external seed = %d,%v; want 699,true", f, ok)
			}
			if f, ok := seedFloor(tx, intPk); !ok || f != 849 {
				t.Fatalf("persisted internal seed = %d,%v; want 849,true", f, ok)
			}
			return nil
		})
		p.mtx.Lock()
		p.cold = nil
		p.mtx.Unlock()
		err = walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
			ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
			if err := w.Manager.SetSyncedTo(ns,
				&waddrmgr.BlockStamp{Height: 10_000}); err != nil {
				return err
			}
			_, _, err := w.classifyWatchSet(tx,
				[]btcutil.Address{extAddr, intAddr}, nil)
			return err
		})
		if err != nil {
			t.Fatalf("restart classify: %v", err)
		}
		p.mtx.Lock()
		extEntry = p.cold[extAddr.String()]
		intEntry = p.cold[intAddr.String()]
		p.mtx.Unlock()
		if extEntry == nil || extEntry.upTo != 699 ||
			intEntry == nil || intEntry.upTo != 849 {
			t.Fatalf("restart lost the seeds: ext=%+v int=%+v", extEntry, intEntry)
		}
		if extEntry.slow || intEntry.slow {
			t.Fatal("re-applied seeds not in the fast tier")
		}

		// Ingestion consumes the seed: once a last-active entry exists,
		// the next classification deletes the record.
		err = walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
			if err := putLastActive(tx, extPk, 700); err != nil {
				return err
			}
			_, _, err := w.classifyWatchSet(tx, []btcutil.Address{extAddr}, nil)
			return err
		})
		if err != nil {
			t.Fatalf("post-ingestion classify: %v", err)
		}
		_ = walletdb.View(w.db, func(tx walletdb.ReadTx) error {
			if f, ok := seedFloor(tx, extPk); ok {
				t.Fatalf("consumed seed still present (floor %d)", f)
			}
			return nil
		})
	}
	t.Run("aged", func(t *testing.T) { run(t, true) })
	t.Run("fresh", func(t *testing.T) { run(t, false) })
}
