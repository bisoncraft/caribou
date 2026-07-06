// Copyright (c) 2024 The Bison Wallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"fmt"
	"sync"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/chain"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/btcutil"
)

// proberConcurrency bounds how many address probes run at once. Probing the gap
// window concurrently keeps a fresh wallet's empty scan from serializing, but
// unbounded concurrency would risk rate-limiting or disconnection on public
// electrum servers.
const proberConcurrency = 16

// recoverViaScan performs gap-limit address discovery using a per-address
// history probe (chain.AddressProber) instead of a block walk. For the BIP84
// scope's external and internal branches it probes derived addresses in
// concurrent windows and extends the wallet's address pool to the highest index
// that has history. A branch is abandoned after gapLimit consecutive unused
// addresses.
//
// Each branch is scanned from its current pool top (the next-to-derive index),
// not from zero. Addresses below the pool top are already active — the
// rescanWithTarget that follows in syncWithChain watches and ingests their
// history regardless — so discovery's only job is extending the pool. On a
// fresh-seed restore the pool is empty and the scan covers the branch from
// index 0; on an ordinary reconnect it probes just gapLimit addresses beyond
// the pool top, which bounds the steady-state cost at O(gapLimit + newly
// found) probes per branch per connect and doubles as detection of another
// wallet instance (same seed elsewhere) having extended the branch.
//
// Gap-limit semantics survive the pool-top start: a gap-limit peer never uses
// an address more than gapLimit past its own last-used index, which is at most
// the pool top here, so probing [top, top+gapLimit) covers every index a peer
// could have reached beyond what this wallet already derived.
//
// Extending the pool makes the discovered addresses "active" (see
// ForEachRelevantActiveAddress), so the rescan that follows ingests them — no
// separate re-trigger. The scan is idempotent: ExtendExternal/InternalAddresses
// only ever extends the pool, so re-running (or running on a rebuild already
// extended via RecoveryCfg) cannot lose state.
func (w *Wallet) recoverViaScan(prober chain.AddressProber, gapLimit uint32) error {
	if gapLimit == 0 {
		return nil
	}

	// These wallets generate only their primary scope's addresses (BIP84 native
	// segwit for BTC/LTC; BIP44 legacy P2PKH for non-segwit chains like Dash),
	// so recovery scans just that scope — both the external (receive) and
	// internal (change) branches. Probing the other default scopes would only
	// ever find empty branches while multiplying the probe count (and the
	// per-probe get_history round-trips dominate recovery latency).
	//
	// PrimaryScope is coin-type-aware (BTC coin 0, LTC coin 2, Dash coin 5) and
	// honors netparams.AddrPurpose, so it matches what DepositAddress derives.
	scoped, err := w.Manager.FetchScopedKeyManager(w.PrimaryScope())
	if err != nil {
		return fmt.Errorf("recover: fetch primary scope manager: %w", err)
	}
	scope := scoped.Scope()

	// The branch pool tops: ExternalKeyCount/InternalKeyCount are the
	// next-to-derive indices for the default account.
	var branchStart map[uint32]uint32
	err = walletdb.View(w.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		props, err := scoped.AccountProperties(ns, waddrmgr.DefaultAccountNum)
		if err != nil {
			return err
		}
		branchStart = map[uint32]uint32{
			waddrmgr.ExternalBranch: props.ExternalKeyCount,
			waddrmgr.InternalBranch: props.InternalKeyCount,
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("recover: scope %v account properties: %w", scope, err)
	}

	log.Infof("Recovery: gap-limit scan starting on scope %v "+
		"(gap limit %d, external start %d, internal start %d)",
		scope, gapLimit, branchStart[waddrmgr.ExternalBranch],
		branchStart[waddrmgr.InternalBranch])

	for _, branch := range []uint32{waddrmgr.ExternalBranch, waddrmgr.InternalBranch} {
		lastUsed, err := w.scanBranch(prober, scoped, branch, branchStart[branch], gapLimit)
		if err != nil {
			return fmt.Errorf("recover: scope %v branch %d: %w", scope, branch, err)
		}
		log.Infof("Recovery: scope %v branch %d scan complete: highest used index = %d",
			scope, branch, lastUsed)
		if lastUsed < 0 {
			continue
		}
		if err := w.extendScanBranch(scoped, branch, uint32(lastUsed)); err != nil {
			return fmt.Errorf("recover: extend scope %v branch %d to %d: %w",
				scope, branch, lastUsed, err)
		}
		log.Infof("Recovery: scope %v branch %d used through index %d",
			scope, branch, lastUsed)
	}
	return nil
}

// scanBranch probes (scope, branch) in concurrent windows of gapLimit
// addresses beginning at index start, returning the highest index with any
// history, or -1 if none. It stops after gapLimit consecutive unused
// addresses.
func (w *Wallet) scanBranch(prober chain.AddressProber,
	scoped *waddrmgr.ScopedKeyManager, branch, start, gapLimit uint32) (int64, error) {

	lastUsed := int64(-1)
	var gap uint32
	idx := start
	for gap < gapLimit {
		used, err := w.probeWindow(prober, scoped, branch, idx, gapLimit)
		if err != nil {
			return -1, err
		}
		// Process in order so the consecutive-gap count is exact.
		for i := uint32(0); i < gapLimit && gap < gapLimit; i++ {
			if used[i] {
				lastUsed = int64(idx + i)
				gap = 0
			} else {
				gap++
			}
		}
		idx += gapLimit
	}
	return lastUsed, nil
}

// probeWindow derives [start, start+n) on (scope, branch) and probes each
// address concurrently, bounded by proberConcurrency. Derivation is local and
// runs on the caller's goroutine; only the history probes are concurrent.
func (w *Wallet) probeWindow(prober chain.AddressProber,
	scoped *waddrmgr.ScopedKeyManager, branch, start, n uint32) ([]bool, error) {

	used := make([]bool, n)
	errs := make([]error, n)
	sem := make(chan struct{}, proberConcurrency)
	var wg sync.WaitGroup
	for i := uint32(0); i < n; i++ {
		addr, err := w.deriveScanAddr(scoped, branch, start+i)
		if err != nil {
			wg.Wait()
			return nil, err
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i uint32, addr btcutil.Address) {
			defer wg.Done()
			defer func() { <-sem }()
			used[i], errs[i] = prober.ProbeAddress(addr)
		}(i, addr)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return used, nil
}

// deriveScanAddr derives the address at (branch, index) for the default account
// of the given scope without advancing any pool pointer. Public derivation, so
// it works while the wallet is locked.
func (w *Wallet) deriveScanAddr(scoped *waddrmgr.ScopedKeyManager,
	branch, index uint32) (btcutil.Address, error) {

	var addr btcutil.Address
	err := walletdb.View(w.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		ma, err := scoped.DeriveFromKeyPath(ns, waddrmgr.DerivationPath{
			InternalAccount: waddrmgr.DefaultAccountNum,
			Account:         waddrmgr.DefaultAccountNum,
			Branch:          branch,
			Index:           index,
		})
		if err != nil {
			return err
		}
		addr = ma.Address()
		return nil
	})
	return addr, err
}

// extendScanBranch derives and stores all keys on the branch through lastIndex,
// catching the wallet's persistent state up to the highest used index found.
func (w *Wallet) extendScanBranch(scoped *waddrmgr.ScopedKeyManager,
	branch, lastIndex uint32) error {

	return walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		if branch == waddrmgr.ExternalBranch {
			return scoped.ExtendExternalAddresses(ns, waddrmgr.DefaultAccountNum, lastIndex)
		}
		return scoped.ExtendInternalAddresses(ns, waddrmgr.DefaultAccountNum, lastIndex)
	})
}
