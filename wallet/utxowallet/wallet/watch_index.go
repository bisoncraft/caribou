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
