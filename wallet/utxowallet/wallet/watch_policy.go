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

// internalWatch is the tracker state for one watched internal address.
type internalWatch struct {
	addr btcutil.Address
	// lastMined is the highest block height at which a relevant tx touched
	// this address.
	lastMined int32
	// unmined holds the hashes of unmined relevant txs touching this
	// address. A hash moves out when the tx is seen mined.
	unmined map[chainhash.Hash]struct{}
}

// watchPolicy tracks watched internal-branch addresses so their backend
// watches can be dropped at finality (docs/address-watching-plan.md,
// increment 1). State is in-memory only: it is rebuilt on every sync from
// wtxmgr (see hotInternalAddrs), so restarts and reconnects need no
// persistence. External addresses are not tracked — their inbound timing is
// third-party-controlled, so they keep the historical persistent watch.
type watchPolicy struct {
	mtx     sync.Mutex
	tracked map[string]*internalWatch
}

// note records relevant-tx activity on an internal address. Mined activity
// (height > 0) clears the tx from the unmined set and advances lastMined;
// unmined activity parks the hash until it is seen mined.
func (p *watchPolicy) note(addr btcutil.Address, txHash chainhash.Hash, height int32) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if p.tracked == nil {
		p.tracked = make(map[string]*internalWatch)
	}
	key := addr.String()
	iw, ok := p.tracked[key]
	if !ok {
		iw = &internalWatch{addr: addr, unmined: make(map[chainhash.Hash]struct{})}
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
	log.Debugf("Watch policy: noted internal %s tx %s at height %d "+
		"(unmined touches: %d)", addr, txHash, height, len(iw.unmined))
}

// reap returns and forgets the tracked addresses whose observable window has
// closed at the given tip: no unmined activity and the last mined activity at
// least watchFinalityDepth deep.
func (p *watchPolicy) reap(tip int32) []btcutil.Address {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	var done []btcutil.Address
	for key, iw := range p.tracked {
		if len(iw.unmined) > 0 || iw.lastMined <= 0 {
			continue
		}
		if tip-iw.lastMined+1 < watchFinalityDepth {
			continue
		}
		done = append(done, iw.addr)
		delete(p.tracked, key)
	}
	return done
}

// noteInternalActivity feeds the watch policy from a just-ingested relevant
// tx: every internal-branch address the tx credits or debits is noted, with
// debit addresses resolved through the previous transactions' outputs (all of
// which are wallet transactions, or the input would not be a debit). Called
// from addRelevantTx inside its database transaction.
func (w *Wallet) noteInternalActivity(dbtx walletdb.ReadWriteTx, rec *wtxmgr.TxRecord,
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
			if err != nil || !ma.Internal() {
				continue
			}
			w.watchPol.note(addr, rec.Hash, height)
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

// reapInternalWatches drops the backend watches of tracked internal addresses
// whose observable window has closed at tip. A no-op for backends without the
// AddressWatcher capability.
func (w *Wallet) reapInternalWatches(chainClient chain.Backend, tip int32) {
	watcher, ok := chainClient.(chain.AddressWatcher)
	if !ok {
		return
	}
	done := w.watchPol.reap(tip)
	if len(done) == 0 {
		return
	}
	if err := watcher.Unwatch(done); err != nil {
		log.Errorf("Failed to unwatch %d finalized internal addresses: %v",
			len(done), err)
		return
	}
	log.Debugf("Unwatched %d finalized internal addresses", len(done))
}

// hotInternalAddrs rebuilds the set of internal addresses that are still
// observationally open — touched by an unmined tx or by a tx mined within
// watchFinalityDepth of the tip — and seeds the watch policy with them. The
// two RangeTransactions passes cover unmined txs and the recent window;
// everything older is final by definition, so no persistent tiering state is
// needed.
func (w *Wallet) hotInternalAddrs(dbtx walletdb.ReadTx, tip int32) (map[string]struct{}, error) {
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
			if err != nil || !ma.Internal() {
				continue
			}
			hot[addr.String()] = struct{}{}
			w.watchPol.note(addr, txHash, height)
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
	begin := tip - watchFinalityDepth + 1
	if begin < 0 {
		begin = 0
	}
	if err := w.TxStore.RangeTransactions(txmgrNs, begin, tip, rangeF); err != nil {
		return nil, err
	}
	return hot, nil
}

// filterColdInternalAddrs partitions the sync watch set: internal-branch
// addresses outside the hot set (no unmined activity, nothing mined within
// watchFinalityDepth) are dropped from both the address list and the watched-
// outpoint credits, so a reconnect re-establishes only the live window.
// External addresses always pass through. hot is the set from
// hotInternalAddrs.
func (w *Wallet) filterColdInternalAddrs(dbtx walletdb.ReadTx, hot map[string]struct{},
	addrs []btcutil.Address, unspent []wtxmgr.Credit) ([]btcutil.Address, []wtxmgr.Credit, error) {

	addrmgrNs := dbtx.ReadBucket(waddrmgrNamespaceKey)

	isColdInternal := func(addr btcutil.Address) bool {
		ma, err := w.Manager.Address(addrmgrNs, addr)
		if err != nil || !ma.Internal() {
			return false
		}
		_, isHot := hot[addr.String()]
		return !isHot
	}

	keptAddrs := addrs[:0]
	for _, addr := range addrs {
		if !isColdInternal(addr) {
			keptAddrs = append(keptAddrs, addr)
		}
	}
	keptUnspent := unspent[:0]
	for _, credit := range unspent {
		_, cAddrs, _, err := txscript.ExtractPkScriptAddrs(credit.PkScript, w.btcParams)
		if err != nil || len(cAddrs) != 1 || !isColdInternal(cAddrs[0]) {
			keptUnspent = append(keptUnspent, credit)
		}
	}
	if dropped := len(addrs) - len(keptAddrs); dropped > 0 {
		log.Infof("Watch policy: excluding %d finalized internal addresses "+
			"(%d outpoints) from the watch set", dropped,
			len(unspent)-len(keptUnspent))
	}
	return keptAddrs, keptUnspent, nil
}
