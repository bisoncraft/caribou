// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"sync"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/chain"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
)

// watchFinalityDepth is the confirmation depth after which an internal
// (change) address with no unmined activity is considered observationally
// closed and its backend watch is dropped. A persistent watch only earns its
// cost when it observes an event whose timing the wallet does not control;
// every spend of an internal address is wallet-originated, so once its last
// transaction is this deep the address has nothing left to report. The value
// is a reorg-safety margin — deliberately deeper than the asset layer's
// display-oriented confTxFinality (1). A reorg deeper than this after an
// address was forgotten is not observed until the address is next involved
// in wallet activity; see docs/address-watching-plan.md.
const watchFinalityDepth = 6

// externalQuietDepth is the BTC-scale fallback external quiet depth, used
// only when the chain params omit a target block time. The effective depth
// is chain-derived (chainQuietDepth: 24h / target block time — 144 on BTC,
// 576 on LTC), because "quiet for a day" must mean a day on every chain, not
// a fixed block count that is a day only on BTC. A used external address
// quieter than this is demoted from its persistent watch; the metered sweep
// is its safety net. Unused externals (the lookahead) are never tracked and
// stay watched.
const externalQuietDepth = 144

// addrWatch is the tracker state for one watched address.
type addrWatch struct {
	addr btcutil.Address
	// internal marks the address's branch; it selects the quiet depth at
	// which the watch is reaped (watchFinalityDepth vs externalQuietDepth).
	internal bool
	// lastMined is the highest block height at which a relevant tx touched
	// this address.
	lastMined int32
	// unmined holds the hashes of unmined relevant txs touching this
	// address. A hash moves out when the tx is seen mined.
	unmined map[chainhash.Hash]struct{}
}

// watchPolicy tracks watched addresses with activity so their backend
// watches can be dropped once quiet: internal (change) addresses at
// watchFinalityDepth — every event on them is wallet-originated — and used
// external addresses at the deeper externalQuietDepth
// (docs/address-watching-plan.md, increments 1 and 2). State is in-memory
// only: it is rebuilt on every sync from wtxmgr (see hotAddrs), so restarts
// and reconnects need no persistence. Unused externals (the lookahead) are
// never tracked and keep their persistent watch.
type watchPolicy struct {
	mtx     sync.Mutex
	tracked map[string]*addrWatch

	// coldExternal holds demoted (unwatched) used external addresses
	// awaiting metered sweeps; a sweep hit promotes an address back out.
	coldExternal map[string]*coldEntry
	// sweeping is the single-flight guard for the sweep goroutine.
	sweeping bool
	// lastSweepTip is the tip at which the last sweep launched; zero means
	// no sweep since (re)sync, so the next block triggers one.
	lastSweepTip int32

	// extQuietDefault is the chain-derived external quiet depth
	// (24h / target block time), set at wallet open. extQuiet and sweepEvery
	// override the external quiet depth and sweep interval when positive —
	// test knobs, set via the wallet settings.
	extQuietDefault int32
	extQuiet        int32
	sweepEvery      int32
}

// coldEntry is one demoted external address and the height up to which its
// history is fully known (the tip at which it was last provably watched or
// swept). The sweep's fromHeight is the minimum over the cold set, so a
// stale entry can never false-hit on history the wallet already ingested.
type coldEntry struct {
	addr btcutil.Address
	upTo int32
}

// externalQuiet and sweepDue return the effective depths (test overrides or
// the defaults).
func (p *watchPolicy) externalQuiet() int32 {
	if p.extQuiet > 0 {
		return p.extQuiet
	}
	if p.extQuietDefault > 0 {
		return p.extQuietDefault
	}
	return externalQuietDepth
}

func (p *watchPolicy) sweepDue() int32 {
	if p.sweepEvery > 0 {
		return p.sweepEvery
	}
	return sweepInterval
}

// note records relevant-tx activity on an address. Mined activity
// (height > 0) clears the tx from the unmined set and advances lastMined;
// unmined activity parks the hash until it is seen mined.
func (p *watchPolicy) note(addr btcutil.Address, internal bool, txHash chainhash.Hash, height int32) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if p.tracked == nil {
		p.tracked = make(map[string]*addrWatch)
	}
	key := addr.String()
	iw, ok := p.tracked[key]
	if !ok {
		iw = &addrWatch{addr: addr, internal: internal,
			unmined: make(map[chainhash.Hash]struct{})}
		p.tracked[key] = iw
	}
	if height > 0 {
		delete(iw.unmined, txHash)
		if height > iw.lastMined {
			iw.lastMined = height
		}
	} else {
		iw.unmined[txHash] = struct{}{}
	}
	log.Debugf("Watch policy: noted %s (internal=%v) tx %s at height %d "+
		"(unmined touches: %d)", addr, internal, txHash, height, len(iw.unmined))
}

// reap returns and forgets the tracked addresses whose observable window has
// closed at the given tip: no unmined activity and the last mined activity at
// least the branch's quiet depth deep.
func (p *watchPolicy) reap(tip int32) []btcutil.Address {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	var done []btcutil.Address
	for key, iw := range p.tracked {
		if len(iw.unmined) > 0 || iw.lastMined <= 0 {
			continue
		}
		quietDepth := int32(watchFinalityDepth)
		if !iw.internal {
			quietDepth = p.externalQuiet()
		}
		if tip-iw.lastMined+1 < quietDepth {
			continue
		}
		done = append(done, iw.addr)
		delete(p.tracked, key)
		if !iw.internal {
			if p.coldExternal == nil {
				p.coldExternal = make(map[string]*coldEntry)
			}
			p.coldExternal[key] = &coldEntry{addr: iw.addr, upTo: tip}
		}
	}
	return done
}

// noteActivity feeds the watch policy from a just-ingested relevant tx:
// every wallet address the tx credits or debits is noted, with debit
// addresses resolved through the previous transactions' outputs (all of
// which are wallet transactions, or the input would not be a debit). Called
// from addRelevantTx inside its database transaction.
func (w *Wallet) noteActivity(dbtx walletdb.ReadWriteTx, rec *wtxmgr.TxRecord,
	block *wtxmgr.BlockMeta) {

	addrmgrNs := dbtx.ReadBucket(waddrmgrNamespaceKey)
	txmgrNs := dbtx.ReadBucket(wtxmgrNamespaceKey)

	height := int32(0)
	if block != nil {
		height = block.Height
	}

	note := func(pkScript []byte) {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript, w.btcParams)
		if err != nil {
			return
		}
		for _, addr := range addrs {
			ma, err := w.Manager.Address(addrmgrNs, addr)
			if err != nil {
				continue
			}
			w.watchPol.note(addr, ma.Internal(), rec.Hash, height)
			// Persist last-active so hot/cold is judgeable from disk at
			// the next startup without a history scan.
			if err := putLastActive(dbtx, pkScript, height); err != nil {
				log.Errorf("Watch policy: persist last-active for %s: %v", addr, err)
			}
		}
	}

	for _, out := range rec.Tx.TxOut {
		note(out.PkScript)
	}
	for _, in := range rec.Tx.TxIn {
		prev, err := w.TxStore.TxDetails(txmgrNs, &in.PreviousOutPoint.Hash)
		if err != nil || prev == nil {
			continue // not a wallet tx: the input is not our debit
		}
		if idx := in.PreviousOutPoint.Index; idx < uint32(len(prev.Tx.TxOut)) {
			note(prev.Tx.TxOut[idx].PkScript)
		}
	}
}

// reapWatches drops the backend watches of tracked addresses whose
// observable window has closed at tip. A no-op for backends without the
// AddressWatcher capability.
func (w *Wallet) reapWatches(chainClient chain.Backend, tip int32) {
	watcher, ok := chainClient.(chain.AddressWatcher)
	if !ok {
		return
	}
	done := w.watchPol.reap(tip)
	if len(done) == 0 {
		return
	}
	if err := watcher.Unwatch(done); err != nil {
		log.Errorf("Failed to unwatch %d quiet addresses: %v",
			len(done), err)
		return
	}
	log.Debugf("Unwatched %d quiet addresses", len(done))
}

// classifyWatchSet partitions the sync watch set by the persisted last-active
// index, keeping only the hot addresses. An address is hot when it is an
// unused external (the gap-limit lookahead, whose first payment can arrive at
// any time), has unmined activity, or its last-active height is within its
// branch's quiet depth of REFERENCE — the on-disk SyncedTo, i.e. the best
// header at last shutdown. Referencing SyncedTo rather than the (possibly
// months-ahead) new chain tip keeps an address the wallet was expecting funds
// on at shutdown in the watch set through the catch-up, where its payment
// lands; genuinely quiet addresses drop to the cold set and are swept.
//
// The index is backfilled from history once (marker-gated); when it has no
// entries — a fresh or restored wallet with no prior basis to judge hotness —
// nothing is filtered and the rescan ingests everything, populating the index
// for next startup. Watched unspent outpoints are tiered the same way.
func (w *Wallet) classifyWatchSet(dbtx walletdb.ReadWriteTx,
	addrs []btcutil.Address, unspent []wtxmgr.Credit) ([]btcutil.Address, []wtxmgr.Credit, error) {

	if !indexBackfilled(dbtx) {
		if err := w.backfillTierIndex(dbtx); err != nil {
			return nil, nil, err
		}
		if err := markBackfilled(dbtx); err != nil {
			return nil, nil, err
		}
	}
	if !indexHasEntries(dbtx) {
		// No prior history: watch everything, let the rescan ingest and
		// build the index for the next startup.
		return addrs, unspent, nil
	}

	addrmgrNs := dbtx.ReadBucket(waddrmgrNamespaceKey)
	ref := w.Manager.SyncedTo().Height
	extDepth := w.watchPol.externalQuiet()

	// Scripthashes with unmined activity are hot regardless of mined
	// history; the pass also seeds the tracker's unmined set for the reaper.
	unminedHot, err := w.unminedHotScripts(dbtx)
	if err != nil {
		return nil, nil, err
	}

	isCold := func(pkScript []byte, addr btcutil.Address) bool {
		ma, err := w.Manager.Address(addrmgrNs, addr)
		if err != nil {
			return false
		}
		if !ma.Internal() && !ma.Used(addrmgrNs) {
			return false // external lookahead: always watched
		}
		key := scriptHashKey(pkScript)
		if _, ok := unminedHot[key]; ok {
			return false
		}
		depth := extDepth
		if ma.Internal() {
			depth = watchFinalityDepth
		}
		if la, ok := lastActive(dbtx, pkScript); ok && la >= ref-depth {
			return false
		}
		return true
	}

	keptAddrs := addrs[:0]
	for _, addr := range addrs {
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			keptAddrs = append(keptAddrs, addr) // can't script it; keep watching
			continue
		}
		if !isCold(pkScript, addr) {
			keptAddrs = append(keptAddrs, addr)
			continue
		}
		// Cold used external: hand to the sweep, known through the
		// reference height (the address was watched up to it).
		if ma, err := w.Manager.Address(addrmgrNs, addr); err == nil && !ma.Internal() {
			w.watchPol.demote(addr, ref)
		}
	}
	keptUnspent := unspent[:0]
	for _, credit := range unspent {
		_, cAddrs, _, err := txscript.ExtractPkScriptAddrs(credit.PkScript, w.btcParams)
		if err != nil || len(cAddrs) != 1 || !isCold(credit.PkScript, cAddrs[0]) {
			keptUnspent = append(keptUnspent, credit)
		}
	}
	if dropped := len(addrs) - len(keptAddrs); dropped > 0 {
		log.Infof("Watch policy: excluding %d quiet addresses (%d outpoints) "+
			"from the watch set, referenced to synced height %d",
			dropped, len(unspent)-len(keptUnspent), ref)
	}
	return keptAddrs, keptUnspent, nil
}

// backfillTierIndex populates the last-active index from transaction history
// for wallets that predate it. It scans the mined window that classification
// consults — [ref - externalQuiet, ref] (the deepest branch window) — writing
// each credited or debited wallet scripthash's highest height. Addresses
// active only before the window get no entry and are correctly cold; new
// activity updates the index incrementally thereafter. One-time, marker-gated.
func (w *Wallet) backfillTierIndex(dbtx walletdb.ReadWriteTx) error {
	txmgrNs := dbtx.ReadBucket(wtxmgrNamespaceKey)
	ref := w.Manager.SyncedTo().Height
	begin := ref - w.watchPol.externalQuiet()
	if begin < 0 {
		begin = 0
	}
	record := func(pkScript []byte, height int32) {
		if err := putLastActive(dbtx, pkScript, height); err != nil {
			log.Errorf("Watch policy: backfill last-active: %v", err)
		}
	}
	err := w.TxStore.RangeTransactions(txmgrNs, begin, ref,
		func(details []wtxmgr.TxDetails) (bool, error) {
			for i := range details {
				det := &details[i]
				for _, cred := range det.Credits {
					if cred.Index < uint32(len(det.Tx.TxOut)) {
						record(det.Tx.TxOut[cred.Index].PkScript, det.Block.Height)
					}
				}
				for _, deb := range det.Debits {
					if deb.Index >= uint32(len(det.Tx.TxIn)) {
						continue
					}
					op := det.Tx.TxIn[deb.Index].PreviousOutPoint
					prev, err := w.TxStore.TxDetails(txmgrNs, &op.Hash)
					if err != nil || prev == nil {
						continue
					}
					if op.Index < uint32(len(prev.Tx.TxOut)) {
						record(prev.Tx.TxOut[op.Index].PkScript, det.Block.Height)
					}
				}
			}
			return false, nil
		})
	if err != nil {
		return err
	}
	log.Infof("Watch policy: backfilled last-active index from history "+
		"[%d, %d]", begin, ref)
	return nil
}

// unminedHotScripts returns the set of wallet scripthashes touched by an
// unmined transaction — hot regardless of mined history — and seeds the
// watch-policy tracker's unmined set so the reaper tracks them after sync.
// The unmined set is small, so this pass is cheap.
func (w *Wallet) unminedHotScripts(dbtx walletdb.ReadTx) (map[[32]byte]struct{}, error) {
	addrmgrNs := dbtx.ReadBucket(waddrmgrNamespaceKey)
	txmgrNs := dbtx.ReadBucket(wtxmgrNamespaceKey)
	hot := make(map[[32]byte]struct{})

	note := func(pkScript []byte, txHash chainhash.Hash) {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript, w.btcParams)
		if err != nil {
			return
		}
		for _, addr := range addrs {
			ma, err := w.Manager.Address(addrmgrNs, addr)
			if err != nil {
				continue
			}
			hot[scriptHashKey(pkScript)] = struct{}{}
			w.watchPol.note(addr, ma.Internal(), txHash, 0)
		}
	}
	err := w.TxStore.RangeTransactions(txmgrNs, -1, -1,
		func(details []wtxmgr.TxDetails) (bool, error) {
			for i := range details {
				det := &details[i]
				for _, cred := range det.Credits {
					if cred.Index < uint32(len(det.Tx.TxOut)) {
						note(det.Tx.TxOut[cred.Index].PkScript, det.Hash)
					}
				}
				for _, deb := range det.Debits {
					if deb.Index >= uint32(len(det.Tx.TxIn)) {
						continue
					}
					op := det.Tx.TxIn[deb.Index].PreviousOutPoint
					prev, err := w.TxStore.TxDetails(txmgrNs, &op.Hash)
					if err != nil || prev == nil {
						continue
					}
					if op.Index < uint32(len(prev.Tx.TxOut)) {
						note(prev.Tx.TxOut[op.Index].PkScript, det.Hash)
					}
				}
			}
			return false, nil
		})
	return hot, err
}

// sweepInterval is how many blocks pass between metered sweeps of the cold
// external set. Detection latency on a cold address is bounded by this; the
// post-sync sweep covers the offline gap.
const sweepInterval = 144

// demote moves an address into the cold-external set awaiting sweeps. upTo
// is the height through which its history is fully known — the tip at which
// it was last watched.
func (p *watchPolicy) demote(addr btcutil.Address, upTo int32) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if p.coldExternal == nil {
		p.coldExternal = make(map[string]*coldEntry)
	}
	p.coldExternal[addr.String()] = &coldEntry{addr: addr, upTo: upTo}
}

// promote removes addresses from the cold set (a sweep hit re-established
// their watch; ingestion re-tracks them).
func (p *watchPolicy) promote(addrs []btcutil.Address) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	for _, addr := range addrs {
		delete(p.coldExternal, addr.String())
	}
}

// coldSet returns a snapshot of the cold-external addresses and the height
// above which a sweep must look — the minimum known-up-to over the set. No
// reorg margin is subtracted: dipping below the known-up-to point makes
// already-ingested history register as hits (instant false re-promotion),
// while a transaction reorged back in confirms at a new post-reorg height
// above upTo and is caught without one.
func (p *watchPolicy) coldSet() ([]btcutil.Address, int32) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	out := make([]btcutil.Address, 0, len(p.coldExternal))
	from := int32(0)
	for _, e := range p.coldExternal {
		out = append(out, e.addr)
		if from == 0 || e.upTo < from {
			from = e.upTo
		}
	}
	return out, from
}

// bumpCold records that a completed sweep verified the remaining cold set
// through tip, advancing each verified entry's known-up-to height. Entries
// in failed were NOT verified — a transient per-address error mid-sweep —
// and their floors must stay put, or the heights the failed check skipped
// would never be examined again (a silently missed payment until seed
// restore).
func (p *watchPolicy) bumpCold(tip int32, failed []btcutil.Address) {
	skip := make(map[string]struct{}, len(failed))
	for _, addr := range failed {
		skip[addr.String()] = struct{}{}
	}
	p.mtx.Lock()
	defer p.mtx.Unlock()
	for key, e := range p.coldExternal {
		if _, ok := skip[key]; ok {
			continue
		}
		if tip > e.upTo {
			e.upTo = tip
		}
	}
}

// maybeSweep launches the cold-external sweep when due: on the first block
// after a sync (lastSweepTip zero) and every sweepInterval blocks after. The
// sweep runs on its own goroutine — it is a batch of network calls and must
// not block the notification loop — with single-flight protection.
func (w *Wallet) maybeSweep(chainClient chain.Backend, tip int32) {
	watcher, ok := chainClient.(chain.AddressWatcher)
	if !ok {
		return
	}
	p := &w.watchPol
	p.mtx.Lock()
	due := p.lastSweepTip == 0 || tip-p.lastSweepTip >= p.sweepDue()
	if !due || p.sweeping || len(p.coldExternal) == 0 {
		p.mtx.Unlock()
		return
	}
	p.sweeping = true
	p.lastSweepTip = tip
	p.mtx.Unlock()

	cold, fromHeight := p.coldSet()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			p.mtx.Lock()
			p.sweeping = false
			p.mtx.Unlock()
		}()
		hits, failed, err := watcher.Sweep(cold, fromHeight)
		if err != nil {
			log.Errorf("Cold-address sweep failed: %v", err)
			// Retry from the same floor next interval.
			p.mtx.Lock()
			p.lastSweepTip = tip - p.sweepDue()
			p.mtx.Unlock()
			return
		}
		p.promote(hits)
		p.bumpCold(tip, failed)
		if len(failed) > 0 {
			log.Warnf("Cold-address sweep: %d of %d checks failed; their "+
				"floors stay at their last verified heights for the next sweep",
				len(failed), len(cold))
		}
		log.Debugf("Cold-address sweep: %d checked from height %d, %d promoted",
			len(cold), fromHeight, len(hits))
	}()
}
