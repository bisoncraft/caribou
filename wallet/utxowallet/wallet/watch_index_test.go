// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	_ "github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb/bdb"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
)

func tempIndexDB(t *testing.T) walletdb.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := walletdb.Create("bdb", filepath.Join(dir, "idx.db"), true, defaultDBTimeout)
	if err != nil {
		t.Fatalf("walletdb.Create: %v", err)
	}
	t.Cleanup(func() { db.Close(); os.RemoveAll(dir) })
	return db
}

// TestColdSweptTo exercises the per-tier verified watermarks: absent until
// written, monotonic — a lower write can only be stale and must not regress
// the verified height — and independent between tiers, since the slow tier's
// floors lag the fast tier's by design.
func TestColdSweptTo(t *testing.T) {
	db := tempIndexDB(t)

	_ = walletdb.View(db, func(tx walletdb.ReadTx) error {
		for _, slow := range []bool{false, true} {
			if h, ok := coldSweptTo(tx, slow); ok {
				t.Fatalf("watermark(slow=%v) %d present in empty db", slow, h)
			}
		}
		return nil
	})

	_ = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		for _, h := range []int32{500, 700, 600} { // 600 must not lower 700
			if err := putColdSweptTo(tx, h, false); err != nil {
				t.Fatalf("putColdSweptTo(%d): %v", h, err)
			}
		}
		if err := putColdSweptTo(tx, 300, true); err != nil {
			t.Fatalf("putColdSweptTo(slow): %v", err)
		}
		return putColdSweptTo(tx, 0, false) // ignored
	})
	_ = walletdb.View(db, func(tx walletdb.ReadTx) error {
		h, ok := coldSweptTo(tx, false)
		if !ok || h != 700 {
			t.Fatalf("coldSweptTo(fast) = %d,%v; want 700,true", h, ok)
		}
		// The fast tier's writes must not have touched the slow tier's
		// (lower) watermark.
		h, ok = coldSweptTo(tx, true)
		if !ok || h != 300 {
			t.Fatalf("coldSweptTo(slow) = %d,%v; want 300,true", h, ok)
		}
		return nil
	})
}

func TestLastActiveIndex(t *testing.T) {
	db := tempIndexDB(t)
	script := []byte{0x00, 0x14, 0x01, 0x02, 0x03}

	// Absent bucket: no entry, not backfilled, no entries.
	_ = walletdb.View(db, func(tx walletdb.ReadTx) error {
		if _, ok := lastActive(tx, script); ok {
			t.Fatal("entry present in empty db")
		}
		if indexBackfilled(tx) {
			t.Fatal("backfilled with no bucket")
		}
		if indexHasEntries(tx) {
			t.Fatal("entries with no bucket")
		}
		return nil
	})

	// Write, then read back; a lower height must not lower the stored value.
	_ = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		for _, h := range []int32{100, 250, 200} { // 200 must not overwrite 250
			if err := putLastActive(tx, script, h); err != nil {
				t.Fatalf("putLastActive: %v", err)
			}
		}
		return putLastActive(tx, script, -1) // mempool: ignored
	})
	_ = walletdb.View(db, func(tx walletdb.ReadTx) error {
		h, ok := lastActive(tx, script)
		if !ok || h != 250 {
			t.Fatalf("lastActive = %d,%v; want 250,true", h, ok)
		}
		if !indexHasEntries(tx) {
			t.Fatal("indexHasEntries false after a write")
		}
		if indexBackfilled(tx) {
			t.Fatal("backfilled before marking")
		}
		return nil
	})

	// The marker alone is not a real entry.
	db2 := tempIndexDB(t)
	_ = walletdb.Update(db2, func(tx walletdb.ReadWriteTx) error { return markBackfilled(tx) })
	_ = walletdb.View(db2, func(tx walletdb.ReadTx) error {
		if !indexBackfilled(tx) {
			t.Fatal("marker not recorded")
		}
		if indexHasEntries(tx) {
			t.Fatal("marker counted as an entry")
		}
		return nil
	})
}

// TestClassifyWatchSet drives the load-time classification on a real wallet:
// an unused (lookahead) external is always hot; a used external active within
// quietDepth of the reference is hot; one active before it is cold and gets
// demoted to the fast sweep tier; a quiet internal is tiered by whether it
// still holds an unspent credit — fast while funded, slow once empty.
func TestClassifyWatchSet(t *testing.T) {
	w, cleanup := testWallet(t)
	defer cleanup()

	const ref = int32(10_000)
	depth := w.watchPol.externalQuiet()

	newAddr := func() (btcutil.Address, []byte) {
		a, err := w.NewAddress(0, waddrmgr.KeyScopeBIP0084)
		if err != nil {
			t.Fatalf("NewAddress: %v", err)
		}
		pk, err := txscript.PayToAddrScript(a)
		if err != nil {
			t.Fatalf("PayToAddrScript: %v", err)
		}
		return a, pk
	}
	newChange := func() (btcutil.Address, []byte) {
		a, err := w.NewChangeAddress(0, waddrmgr.KeyScopeBIP0084)
		if err != nil {
			t.Fatalf("NewChangeAddress: %v", err)
		}
		pk, err := txscript.PayToAddrScript(a)
		if err != nil {
			t.Fatalf("PayToAddrScript: %v", err)
		}
		return a, pk
	}
	fresh, _ := newAddr()               // unused lookahead
	recent, recentPk := newAddr()       // used, recent
	old, oldPk := newAddr()             // used, quiet
	chFunded, chFundedPk := newChange() // internal, quiet, still funded
	chEmpty, chEmptyPk := newChange()   // internal, quiet, spent out

	err := walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		if err := w.Manager.SetSyncedTo(ns, &waddrmgr.BlockStamp{Height: ref}); err != nil {
			return err
		}
		for _, a := range []btcutil.Address{recent, old} {
			if err := w.Manager.MarkUsed(ns, a); err != nil {
				return err
			}
		}
		if err := putLastActive(tx, recentPk, ref-depth+2); err != nil {
			return err
		}
		for _, pk := range [][]byte{oldPk, chFundedPk, chEmptyPk} {
			if err := putLastActive(tx, pk, ref-depth-100); err != nil {
				return err
			}
		}
		// The last sweeps of the prior run verified the tiers through
		// here; demotion floors must land on them, not on ref — the window
		// (watermark, ref] was neither watched nor swept. The slow tier's
		// watermark lags the fast tier's, as it does in practice.
		if err := putColdSweptTo(tx, ref-500, false); err != nil {
			return err
		}
		if err := putColdSweptTo(tx, ref-900, true); err != nil {
			return err
		}
		return markBackfilled(tx)
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// The unspent set carries a credit on chFunded only; classification
	// judges internal sweepability from it.
	unspent := []wtxmgr.Credit{{PkScript: chFundedPk}}

	err = walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
		kept, keptUnspent, err := w.classifyWatchSet(tx,
			[]btcutil.Address{fresh, recent, old, chFunded, chEmpty}, unspent)
		if err != nil {
			return err
		}
		got := map[string]bool{}
		for _, a := range kept {
			got[a.String()] = true
		}
		if !got[fresh.String()] {
			t.Error("unused lookahead external was dropped")
		}
		if !got[recent.String()] {
			t.Error("recently-active external was dropped")
		}
		if got[old.String()] {
			t.Error("quiet external was kept hot")
		}
		if got[chFunded.String()] || got[chEmpty.String()] {
			t.Error("quiet internal was kept hot")
		}
		// The cold internal's credit leaves the live watch too — the sweep
		// covers it.
		if len(keptUnspent) != 0 {
			t.Errorf("keptUnspent = %v, want none", keptUnspent)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}

	cold, _ := w.watchPol.coldSet()
	coldMap := map[string]bool{}
	for _, a := range cold {
		coldMap[a.String()] = true
	}
	if len(cold) != 3 || !coldMap[old.String()] || !coldMap[chFunded.String()] ||
		!coldMap[chEmpty.String()] {
		t.Fatalf("cold set = %v, want [%s %s %s]", cold, old, chFunded, chEmpty)
	}
	// Tiers: the used external and the funded internal sweep at the fast
	// cadence from the fast watermark; the empty internal is slow-tier —
	// improbable events, but never invisible — from the slow watermark.
	w.watchPol.mtx.Lock()
	oldEntry := w.watchPol.cold[old.String()]
	fundedEntry := w.watchPol.cold[chFunded.String()]
	emptyEntry := w.watchPol.cold[chEmpty.String()]
	w.watchPol.mtx.Unlock()
	if oldEntry.slow || fundedEntry.slow {
		t.Fatalf("fast-tier entries misfiled: external slow=%v, funded internal slow=%v",
			oldEntry.slow, fundedEntry.slow)
	}
	if !emptyEntry.slow {
		t.Fatal("empty internal not in the slow tier")
	}
	if oldEntry.upTo != ref-500 || fundedEntry.upTo != ref-500 {
		t.Fatalf("fast floors = %d/%d, want the fast watermark %d",
			oldEntry.upTo, fundedEntry.upTo, ref-500)
	}
	if emptyEntry.upTo != ref-900 {
		t.Fatalf("slow floor = %d, want the slow watermark %d",
			emptyEntry.upTo, ref-900)
	}
}
