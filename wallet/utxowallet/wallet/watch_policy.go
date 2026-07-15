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
// a spend of an internal address is normally wallet-originated, so once its
// last transaction is this deep the address has nothing left to report to
// the LIVE watch. The exception is a spend authored by the same seed loaded
// in another wallet — for that, an internal that still holds an unspent
// credit joins the metered sweep set when its live watch drops (see
// reapWatches); an empty internal is done for good. The value is a
// reorg-safety margin — deliberately deeper than the asset layer's
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
// watchFinalityDepth — events on them are normally wallet-originated — and
// used external addresses at the deeper externalQuietDepth
// (docs/address-watching-plan.md, increments 1 and 2). State is in-memory
// only: it is rebuilt on every sync from wtxmgr (see hotAddrs), so restarts
// and reconnects need no persistence. Unused externals (the lookahead) are
// never tracked and keep their persistent watch.
type watchPolicy struct {
	mtx     sync.Mutex
	tracked map[string]*addrWatch

	// cold holds demoted (unwatched) addresses awaiting metered sweeps: the
	// used externals, which any third party can pay at any time, and the
	// internals that still hold an unspent credit, which the same seed
	// loaded in another wallet can spend. A sweep hit promotes an address
	// back out.
	cold map[string]*coldEntry
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

// coldEntry is one demoted address and the height up to which its history is
// fully known (the tip at which it was last provably watched or swept). Each
// entry is swept from its OWN floor (chain.SweepEntry): entries join the set
// at different tips, and judging them against a set-wide minimum would sweep
// every entry above the minimum from below its own verified floor, re-hitting
// its own already-ingested history on every sweep.
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
	if p.extQuietDefault > 0 {
		return p.extQuietDefault
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
// least the branch's quiet depth deep. Reaped externals are demoted into the
// cold sweep set here; reaped internals are also returned separately so the
// caller can demote the ones that still hold an unspent credit — a judgement
// that needs the database (see reapWatches).
func (p *watchPolicy) reap(tip int32) (done, internals []btcutil.Address) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
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
		if iw.internal {
			internals = append(internals, iw.addr)
			continue
		}
		if p.cold == nil {
			p.cold = make(map[string]*coldEntry)
		}
		p.cold[key] = &coldEntry{addr: iw.addr, upTo: tip}
	}
	return done, internals
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
// AddressWatcher capability. Reaped externals join the cold sweep set (any
// third party can pay them at any time); a reaped internal joins it only
// while it still holds an unspent credit — the credit is spendable by the
// same seed loaded in another wallet, and with the live watch dropped only
// the sweep can observe such a spend. An empty internal is done for good:
// never handed out, nothing left to spend.
func (w *Wallet) reapWatches(chainClient chain.Backend, tip int32) {
	watcher, ok := chainClient.(chain.AddressWatcher)
	if !ok {
		return
	}
	done, internals := w.watchPol.reap(tip)
	if len(done) == 0 {
		return
	}
	if len(internals) > 0 {
		if err := w.demoteFundedInternals(internals, tip); err != nil {
			// Not demoted means not swept: an external spend of these
			// credits goes unobserved until a recovery scan.
			log.Errorf("Failed to demote %d reaped internal addresses: %v",
				len(internals), err)
		}
	}
	if err := watcher.Unwatch(done); err != nil {
		log.Errorf("Failed to unwatch %d quiet addresses: %v",
			len(done), err)
		return
	}
	log.Debugf("Unwatched %d quiet addresses", len(done))
}

// demoteFundedInternals moves the reaped internal addresses that still hold
// an unspent credit into the cold sweep set.
func (w *Wallet) demoteFundedInternals(addrs []btcutil.Address, tip int32) error {
	return walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
		txmgrNs := dbtx.ReadBucket(wtxmgrNamespaceKey)
		unspent, err := w.TxStore.UnspentOutputs(txmgrNs)
		if err != nil {
			return err
		}
		funded := make(map[string]struct{}, len(unspent))
		for i := range unspent {
			funded[string(unspent[i].PkScript)] = struct{}{}
		}
		for _, addr := range addrs {
			script, err := txscript.PayToAddrScript(addr)
			if err != nil {
				continue
			}
			if _, ok := funded[string(script)]; ok {
				w.watchPol.demote(addr, tip)
			}
		}
		return nil
	})
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

	// Unspent-credit scripts: a cold internal is only sweepable while it is
	// still funded.
	funded := make(map[string]struct{}, len(unspent))
	for i := range unspent {
		funded[string(unspent[i].PkScript)] = struct{}{}
	}

	// The demotion floor. An address classified cold here was already cold
	// at shutdown — had it been hot, its activity would be within its quiet
	// depth of ref and it would classify hot — so it was NOT watched between
	// the last completed sweep and shutdown. The persisted watermark, not
	// ref, is the height through which its history is verified; demoting at
	// ref would put anything that landed in that window below every future
	// sweep floor, unexamined until a recovery scan. A watermark lower than
	// an address's true floor only causes an idempotent re-sweep.
	coldFloor := ref
	if sweptTo, ok := coldSweptTo(dbtx); ok && sweptTo < ref {
		coldFloor = sweptTo
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
		// Cold addresses that remain observable hand off to the sweep at
		// the verified floor: every used external — any third party can
		// pay it at any time — and an internal that still holds an unspent
		// credit, which the same seed loaded in another wallet can spend.
		// An empty internal stays retired: never handed out, nothing left
		// to spend.
		if ma, err := w.Manager.Address(addrmgrNs, addr); err == nil {
			_, isFunded := funded[string(pkScript)]
			if !ma.Internal() || isFunded {
				w.watchPol.demote(addr, coldFloor)
			}
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

// sweepInterval is the BTC-scale fallback sweep cadence, used only when the
// chain params omit a target block time. The effective cadence is the
// chain-derived day (chainQuietDepth: 24h / target block time — 144 on BTC,
// 576 on LTC), so "swept daily" means daily on every chain, not a fixed
// block count that is a day only on BTC. Detection latency on a cold
// address is bounded by this; the post-sync sweep covers the offline gap.
const sweepInterval = 144

// coldSweepReorgMargin is how far below its verified floor each entry is
// swept. Floors are bumped to sweep-launch tips, so a reorg deeper than the
// blocks since the last sweep can seat NEW transactions in replacement
// blocks at or below a floor, where the strict above-FromHeight hit check
// would never revisit them. The margin must stay below every branch's quiet
// depth to be free of false re-promotions: an address is reaped only once
// its last mined activity is at least quietDepth-1 below the reap tip, and
// an entry's floor never falls below its reap tip, so its ingested history
// sits at least quietDepth-1 below its OWN floor — a margin of
// watchFinalityDepth-1 (the minimum branch depth, minus one) can never
// re-hit it. The proof is per-entry, which is why the margin is applied to
// per-entry floors (sweepSet) and never to a set-wide minimum. Reorgs deeper
// than the margin are out of scope, matching the tolerance documented at
// watchFinalityDepth.
const coldSweepReorgMargin = watchFinalityDepth - 1

// demote moves an address into the cold sweep set. upTo is the height
// through which its history is fully known — the tip at which it was last
// watched. Demoting an address that is already cold keeps the LOWER floor:
// a duplicate demotion carries no new verification (only bumpCold does), and
// raising the floor could lift it past unexamined heights.
func (p *watchPolicy) demote(addr btcutil.Address, upTo int32) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if p.cold == nil {
		p.cold = make(map[string]*coldEntry)
	}
	key := addr.String()
	if e, ok := p.cold[key]; ok {
		if upTo < e.upTo {
			e.upTo = upTo
		}
		return
	}
	p.cold[key] = &coldEntry{addr: addr, upTo: upTo}
}

// promote removes addresses from the cold set (a sweep hit re-established
// their watch; ingestion re-tracks them).
func (p *watchPolicy) promote(addrs []btcutil.Address) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	for _, addr := range addrs {
		delete(p.cold, addr.String())
	}
}

// coldSet returns a snapshot of the cold addresses and the minimum
// known-up-to height over the set — the persisted-watermark value. Floors
// are unmargined here: the margin is applied per entry at sweep time
// (sweepSet), exactly once per sweep, and never compounds across restarts.
func (p *watchPolicy) coldSet() ([]btcutil.Address, int32) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	out := make([]btcutil.Address, 0, len(p.cold))
	from := int32(0)
	for _, e := range p.cold {
		out = append(out, e.addr)
		if from == 0 || e.upTo < from {
			from = e.upTo
		}
	}
	return out, from
}

// sweepSet returns the sweep request for the current cold set — each entry's
// FromHeight is its own verified floor less the reorg margin — plus the
// swept-address snapshot for the post-sweep bump. Only addresses in the
// snapshot are bumped: an entry demoted after the sweep launches was not
// examined by it and keeps its own floor.
func (p *watchPolicy) sweepSet() ([]chain.SweepEntry, []btcutil.Address) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	entries := make([]chain.SweepEntry, 0, len(p.cold))
	swept := make([]btcutil.Address, 0, len(p.cold))
	for _, e := range p.cold {
		from := e.upTo - coldSweepReorgMargin
		if from < 0 {
			from = 0
		}
		entries = append(entries, chain.SweepEntry{Addr: e.addr, FromHeight: from})
		swept = append(swept, e.addr)
	}
	return entries, swept
}

// bumpCold records that a completed sweep verified the swept entries through
// tip, advancing each one's known-up-to height. Entries in failed were NOT
// verified — a transient per-address error mid-sweep — and their floors must
// stay put, or the heights the failed check skipped would never be examined
// again (a silently missed payment until seed restore).
func (p *watchPolicy) bumpCold(tip int32, swept, failed []btcutil.Address) {
	skip := make(map[string]struct{}, len(failed))
	for _, addr := range failed {
		skip[addr.String()] = struct{}{}
	}
	p.mtx.Lock()
	defer p.mtx.Unlock()
	for _, addr := range swept {
		key := addr.String()
		if _, ok := skip[key]; ok {
			continue
		}
		e, ok := p.cold[key]
		if !ok {
			continue // promoted out mid-sweep
		}
		if tip > e.upTo {
			e.upTo = tip
		}
	}
}

// maybeSweep launches the cold-set sweep when due: on the first block
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
	if !due || p.sweeping {
		p.mtx.Unlock()
		return
	}
	if len(p.cold) == 0 {
		// Nothing to sweep, but keep the persisted watermark current: with
		// no demoted addresses, the cold set is trivially verified through
		// tip. An address demoted at the next startup then gets a floor
		// near this tip instead of one pinned at the last nonempty sweep,
		// which would trigger a wide (harmless, but wasteful) re-sweep.
		p.lastSweepTip = tip
		p.mtx.Unlock()
		w.persistColdSweptTo(tip)
		return
	}
	p.sweeping = true
	p.lastSweepTip = tip
	p.mtx.Unlock()

	// Each entry is swept from its own floor less the reorg margin: a
	// shallow reorg can seat new transactions in replacement blocks at or
	// below a floor, which the strict above-FromHeight hit check would
	// otherwise never revisit. The margin can't false-hit ingested history
	// (see the const).
	entries, swept := p.sweepSet()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			p.mtx.Lock()
			p.sweeping = false
			p.mtx.Unlock()
		}()
		hits, failed, err := watcher.Sweep(entries)
		if err != nil {
			log.Errorf("Cold-address sweep failed: %v", err)
			// Retry from the same floors next interval. The persisted
			// watermark is untouched: nothing new was verified.
			p.mtx.Lock()
			p.lastSweepTip = tip - p.sweepDue()
			p.mtx.Unlock()
			return
		}
		p.promote(hits)
		p.bumpCold(tip, swept, failed)
		// Persist the verified watermark for the next startup's demotion
		// floors: the set minimum after this sweep's bumps (a failed
		// address pins it), or tip when everything was promoted out.
		if remaining, from := p.coldSet(); len(remaining) == 0 {
			w.persistColdSweptTo(tip)
		} else {
			w.persistColdSweptTo(from)
		}
		if len(failed) > 0 {
			log.Warnf("Cold-address sweep: %d of %d checks failed; their "+
				"floors stay at their last verified heights for the next sweep",
				len(failed), len(entries))
		}
		log.Debugf("Cold-address sweep: %d checked, %d promoted",
			len(entries), len(hits))
	}()
}

// persistColdSweptTo records the cold-set verified watermark (see
// coldSweptToKey in watch_index.go). Failures are logged and tolerated: a
// stale watermark only lowers the next startup's demotion floors, and a
// too-low floor over-sweeps idempotently rather than missing anything.
func (w *Wallet) persistColdSweptTo(height int32) {
	err := walletdb.Update(w.db, func(dbtx walletdb.ReadWriteTx) error {
		return putColdSweptTo(dbtx, height)
	})
	if err != nil {
		log.Errorf("Failed to persist cold-swept watermark %d: %v", height, err)
	}
}
