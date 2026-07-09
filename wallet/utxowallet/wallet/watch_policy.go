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

// externalQuietDepth is the quiet depth after which a used external address
// with no unmined activity is demoted from its persistent watch: a third
// party can pay a cold external at any time, so demotion trades instant
// detection for sweep-interval latency (docs/address-watching-plan.md,
// increment 2). Unused externals — the gap-limit lookahead — are never
// tracked and stay watched. ~One day of blocks.
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

	// extQuiet and sweepEvery override externalQuietDepth and sweepInterval
	// when positive — test knobs, set via the wallet settings.
	extQuiet   int32
	sweepEvery int32
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

// hotAddrs rebuilds the set of addresses that are still observationally
// open — touched by an unmined tx, or by a tx mined within the branch's
// quiet depth of the tip (watchFinalityDepth for internal,
// externalQuietDepth for external) — and seeds the watch policy with them.
// The RangeTransactions passes cover unmined txs and the deeper of the two
// windows; everything older is quiet by definition, so no persistent tiering
// state is needed.
func (w *Wallet) hotAddrs(dbtx walletdb.ReadTx, tip int32) (map[string]struct{}, error) {
	addrmgrNs := dbtx.ReadBucket(waddrmgrNamespaceKey)
	txmgrNs := dbtx.ReadBucket(wtxmgrNamespaceKey)

	hot := make(map[string]struct{})

	note := func(pkScript []byte, txHash chainhash.Hash, height int32) {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript, w.btcParams)
		if err != nil {
			return
		}
		for _, addr := range addrs {
			ma, err := w.Manager.Address(addrmgrNs, addr)
			if err != nil {
				continue
			}
			quietDepth := w.watchPol.externalQuiet()
			if ma.Internal() {
				quietDepth = watchFinalityDepth
			}
			if height > 0 && tip-height+1 >= quietDepth {
				continue // quiet for this branch
			}
			hot[addr.String()] = struct{}{}
			w.watchPol.note(addr, ma.Internal(), txHash, height)
		}
	}

	rangeF := func(details []wtxmgr.TxDetails) (bool, error) {
		for i := range details {
			det := &details[i]
			for _, cred := range det.Credits {
				if cred.Index < uint32(len(det.Tx.TxOut)) {
					note(det.Tx.TxOut[cred.Index].PkScript,
						det.Hash, det.Block.Height)
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
					note(prev.Tx.TxOut[op.Index].PkScript,
						det.Hash, det.Block.Height)
				}
			}
		}
		return false, nil
	}

	// Unmined txs (height -1), then the still-unfinal mined window.
	if err := w.TxStore.RangeTransactions(txmgrNs, -1, -1, rangeF); err != nil {
		return nil, err
	}
	begin := tip - w.watchPol.externalQuiet() + 1
	if begin < 0 {
		begin = 0
	}
	if err := w.TxStore.RangeTransactions(txmgrNs, begin, tip, rangeF); err != nil {
		return nil, err
	}
	return hot, nil
}

// filterColdAddrs partitions the sync watch set: addresses outside the hot
// set (no unmined activity, nothing mined within their branch's quiet depth)
// are dropped from both the address list and the watched-outpoint credits,
// so a reconnect re-establishes only the live window. Unused external
// addresses — the gap-limit lookahead, whose first payment can arrive at any
// time — always pass through; a used external goes cold like an internal
// does, just at the deeper depth, and the metered sweep is its safety net.
// hot is the set from hotAddrs.
func (w *Wallet) filterColdAddrs(dbtx walletdb.ReadTx, hot map[string]struct{}, tip int32,
	addrs []btcutil.Address, unspent []wtxmgr.Credit) ([]btcutil.Address, []wtxmgr.Credit, error) {

	addrmgrNs := dbtx.ReadBucket(waddrmgrNamespaceKey)

	isCold := func(addr btcutil.Address) bool {
		ma, err := w.Manager.Address(addrmgrNs, addr)
		if err != nil {
			return false
		}
		if !ma.Internal() && !ma.Used(addrmgrNs) {
			return false // lookahead: always watched
		}
		_, isHot := hot[addr.String()]
		return !isHot
	}

	keptAddrs := addrs[:0]
	for _, addr := range addrs {
		if !isCold(addr) {
			keptAddrs = append(keptAddrs, addr)
			continue
		}
		if ma, err := w.Manager.Address(addrmgrNs, addr); err == nil && !ma.Internal() {
			// History is known up to the previous session's synced
			// height: the address was watched through it.
			w.watchPol.demote(addr, tip)
		}
	}
	keptUnspent := unspent[:0]
	for _, credit := range unspent {
		_, cAddrs, _, err := txscript.ExtractPkScriptAddrs(credit.PkScript, w.btcParams)
		if err != nil || len(cAddrs) != 1 || !isCold(cAddrs[0]) {
			keptUnspent = append(keptUnspent, credit)
		}
	}
	if dropped := len(addrs) - len(keptAddrs); dropped > 0 {
		log.Infof("Watch policy: excluding %d quiet addresses "+
			"(%d outpoints) from the watch set", dropped,
			len(unspent)-len(keptUnspent))
	}
	return keptAddrs, keptUnspent, nil
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
