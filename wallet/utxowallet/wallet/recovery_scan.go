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
// It is O(used + gapLimit) probes per branch, independent of chain length.
// Extending the pool makes the discovered addresses "active" (see
// ForEachRelevantActiveAddress), so the rescanWithTarget that follows in
// syncWithChain watches and ingests them — no separate re-trigger.
//
// The scan is idempotent: ExtendExternal/InternalAddresses only ever extends
// the pool, so re-running on an already-synced wallet (or one a rebuild already
// extended via RecoveryCfg) is a no-op. A run-once gate is a future
// optimization, not a correctness requirement.
func (w *Wallet) recoverViaScan(prober chain.AddressProber, gapLimit uint32) error {
	if gapLimit == 0 {
		return nil
	}

	// These wallets generate only BIP84 (native segwit) addresses, so recovery
	// scans just that scope — both the external (receive) and internal (change)
	// branches. Probing the other default scopes (BIP44/49/86) would only ever
	// find empty branches while quadrupling the probe count (and the per-probe
	// get_history round-trips dominate recovery latency).
	//
	// Use ScopeForPurpose so the coin type matches what DepositAddress derives
	// (BTC coin 0, LTC coin 2); waddrmgr.KeyScopeBIP0084 hardcodes coin 0 and
	// would miss LTC's addresses.
	scoped, err := w.Manager.FetchScopedKeyManager(w.ScopeForPurpose(84))
	if err != nil {
		return fmt.Errorf("recover: fetch BIP84 scope manager: %w", err)
	}
	scope := scoped.Scope()
	log.Infof("Recovery: BIP84 gap-limit scan starting on scope %v (gap limit %d)",
		scope, gapLimit)

	for _, branch := range []uint32{waddrmgr.ExternalBranch, waddrmgr.InternalBranch} {
		lastUsed, err := w.scanBranch(prober, scoped, branch, gapLimit)
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

// scanBranch probes (scope, branch) in concurrent windows of gapLimit addresses,
// returning the highest index with any history, or -1 if none. It stops after
// gapLimit consecutive unused addresses.
func (w *Wallet) scanBranch(prober chain.AddressProber,
	scoped *waddrmgr.ScopedKeyManager, branch, gapLimit uint32) (int64, error) {

	lastUsed := int64(-1)
	var gap, idx uint32
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
