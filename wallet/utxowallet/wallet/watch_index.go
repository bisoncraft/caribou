// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
)

// The watch-tier index persists, per scripthash, the highest block height at
// which the wallet saw activity on that script — its last-active height. It
// lets the wallet classify each address hot or cold at startup by a cheap
// comparison against the on-disk SyncedTo, loading only hot addresses into
// the subscribe/watch set instead of the whole accumulated set (see
// docs/neutrino-watching-plan.md). The bucket is created on demand, so
// wallets that predate the index need no migration; a one-time backfill from
// transaction history populates it on first use.

var watchTierNamespaceKey = []byte("watchtier")

// backfilledKey marks that the index has been backfilled from transaction
// history once. Its presence gates the one-time scan and distinguishes a
// genuinely empty index (fresh or restored wallet) from one not yet built.
// The 0x00 prefix keeps it out of the 32-byte scripthash keyspace.
var backfilledKey = []byte{0x00}

// coldSweptToKey stores the cold-set verified watermark: the height through
// which every cold address's history has provably been examined (the minimum
// sweep floor, or the tip when the cold set is empty). The in-memory floors
// die with the process, and a cold address is NOT watched between its last
// sweep and shutdown — without this watermark, startup classification would
// claim cold addresses as known through the on-disk SyncedTo and the next
// sweep would skip the pre-shutdown window (up to a full sweep interval) for
// good. The 0x01 prefix keeps it out of the scripthash keyspace.
var coldSweptToKey = []byte{0x01}

// scriptHashKey is the 32-byte index key for a pkScript: sha256 of the
// script. This is the canonical form both backends watch — electrum's
// scripthash without its wire byte-reversal, and the script neutrino adds to
// its cfilter watch list — so one key serves both.
func scriptHashKey(pkScript []byte) [sha256.Size]byte {
	return sha256.Sum256(pkScript)
}

// putLastActive records height as the pkScript's last-active height when it
// exceeds the stored value. Only mined activity (height > 0) is recorded;
// unmined activity keeps an address hot through the in-memory tracker's
// unmined set and is persisted when it confirms. The bucket is created on
// demand for wallets that predate the index.
func putLastActive(tx walletdb.ReadWriteTx, pkScript []byte, height int32) error {
	if height <= 0 {
		return nil
	}
	ns, err := tx.CreateTopLevelBucket(watchTierNamespaceKey)
	if err != nil {
		return err
	}
	key := scriptHashKey(pkScript)
	if cur := ns.Get(key[:]); cur != nil &&
		int32(binary.BigEndian.Uint32(cur)) >= height {
		return nil
	}
	var v [4]byte
	binary.BigEndian.PutUint32(v[:], uint32(height))
	return ns.Put(key[:], v[:])
}

// lastActive returns the stored last-active height for a pkScript and whether
// an entry exists. Absent bucket or entry reports (0, false).
func lastActive(tx walletdb.ReadTx, pkScript []byte) (int32, bool) {
	ns := tx.ReadBucket(watchTierNamespaceKey)
	if ns == nil {
		return 0, false
	}
	key := scriptHashKey(pkScript)
	v := ns.Get(key[:])
	if v == nil {
		return 0, false
	}
	return int32(binary.BigEndian.Uint32(v)), true
}

// putColdSweptTo records the cold-set verified watermark when it exceeds the
// stored value. Sweep floors only advance and a set's minimum only rises as
// entries are verified or promoted out, so a lower write can only be stale.
func putColdSweptTo(tx walletdb.ReadWriteTx, height int32) error {
	if height <= 0 {
		return nil
	}
	ns, err := tx.CreateTopLevelBucket(watchTierNamespaceKey)
	if err != nil {
		return err
	}
	if cur := ns.Get(coldSweptToKey); cur != nil &&
		int32(binary.BigEndian.Uint32(cur)) >= height {
		return nil
	}
	var v [4]byte
	binary.BigEndian.PutUint32(v[:], uint32(height))
	return ns.Put(coldSweptToKey, v[:])
}

// coldSweptTo returns the persisted cold-set verified watermark and whether
// one exists. Absent bucket or entry reports (0, false) — a wallet from
// before the watermark, whose cold history is treated as known through the
// on-disk SyncedTo (the pre-watermark behavior, correct for a wallet that
// watched everything).
func coldSweptTo(tx walletdb.ReadTx) (int32, bool) {
	ns := tx.ReadBucket(watchTierNamespaceKey)
	if ns == nil {
		return 0, false
	}
	v := ns.Get(coldSweptToKey)
	if v == nil {
		return 0, false
	}
	return int32(binary.BigEndian.Uint32(v)), true
}

// indexBackfilled reports whether the one-time history backfill has run.
func indexBackfilled(tx walletdb.ReadTx) bool {
	ns := tx.ReadBucket(watchTierNamespaceKey)
	return ns != nil && ns.Get(backfilledKey) != nil
}

// markBackfilled records that the backfill has run, so subsequent startups
// read the maintained index directly instead of rescanning history.
func markBackfilled(tx walletdb.ReadWriteTx) error {
	ns, err := tx.CreateTopLevelBucket(watchTierNamespaceKey)
	if err != nil {
		return err
	}
	return ns.Put(backfilledKey, []byte{1})
}

// indexHasEntries reports whether the index holds any last-active entry
// (i.e. a scripthash key, ignoring the backfilled marker). No entries means
// no prior history to judge hotness from — a fresh or restored wallet.
func indexHasEntries(tx walletdb.ReadTx) bool {
	ns := tx.ReadBucket(watchTierNamespaceKey)
	if ns == nil {
		return false
	}
	found := false
	_ = ns.ForEach(func(k, _ []byte) error {
		if len(k) == sha256.Size {
			found = true
			return errStopForEach
		}
		return nil
	})
	return found
}

// errStopForEach short-circuits a ForEach once the answer is known.
var errStopForEach = errors.New("stop")
