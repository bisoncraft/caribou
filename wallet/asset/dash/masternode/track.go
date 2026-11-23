// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/chaincfg/chainhash"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
)

// A shared masternode is found without reading the history of the shared
// collateral script, which every shared masternode on the network adds to and a
// server refuses past its limit:
//
//   - A registration the wallet has the transaction of is added with Track, and
//     proven by a merkle proof once it is mined. The wallet has it, as the
//     registration spends its own funds.
//   - A dissolution of a share of the wallet's pays that share's refund script,
//     which Dash Core enforces for every share but the actor's, so Refresh reads
//     the history of those scripts, which is the wallet's own. The actor's own
//     output is optional, so a dissolution the wallet's actor made may pay
//     nothing to it, and it is added with Track.
//
// Discover is the only thing that reads the shared script, to restore a wallet
// that has no transaction to give.

// Track adds a transaction of the wallet's, a shared registration or a
// dissolution, to be proven by Refresh once it is mined. height is the height
// of its block if it is known, and 0 if not. It is not saved, and a wrong one
// costs a request. It makes no network call.
//
// A registration is kept only if one of its share owner keys is the wallet's,
// which may derive keys, so it can fail on a locked wallet that has not cached
// them; any other is ErrNotOurs. A dissolution is kept until Refresh proves it
// as the spend of a masternode of the wallet's, and stays tracked until then:
// Untrack removes one that is not wanted.
func (m *Manager) Track(raw []byte, height int32) error {
	id, err := trackable(raw)
	if err != nil {
		return err
	}
	tx, err := decodeTx(raw)
	if err != nil {
		return err
	}
	up := Update{Track: [][]byte{raw}}
	if tx.Type == txTypeProReg {
		snap, err := m.store.Load()
		if err != nil {
			return err
		}
		mn, err := RegistrationMN(tx, 1)
		if err != nil {
			return err
		}
		ours, newIDs, minNext, err := m.matchOwners(snap, []*candidate{{tx: tx, mn: mn}})
		if err != nil {
			return err
		}
		if !isOurs(mn, ours) {
			return ErrNotOurs
		}
		up.OwnerKeyIDs, up.MinNextOwner = newIDs, minNext
	}
	if err := m.store.Save(up); err != nil {
		return err
	}
	if height > 0 {
		m.mu.Lock()
		m.hints[id] = height
		m.mu.Unlock()
	}
	return nil
}

// TrackTxID fetches the transaction with id and tracks it, for a registration
// that is known by its id and not by its bytes. The bytes are the server's
// word, but a registration is only taken once Refresh has proven them by a
// merkle proof, so a server cannot make the wallet hold one it did not mine.
func (m *Manager) TrackTxID(ctx context.Context, id chainhash.Hash) error {
	rawHex, err := m.chain.GetRawTransaction(ctx, id.String())
	if err != nil {
		return fmt.Errorf("transaction %s: %w", id, err)
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return fmt.Errorf("transaction %s: %w", id, err)
	}
	if len(raw) == 64 {
		return fmt.Errorf("transaction %s is 64 bytes", id)
	}
	tx, err := decodeTx(raw)
	if err != nil {
		return fmt.Errorf("transaction %s: %w", id, err)
	}
	if tx.TxHash() != id {
		return fmt.Errorf("transaction %s is served as %s", id, tx.TxHash())
	}
	return m.Track(raw, 0)
}

// Untrack stops waiting for a transaction, such as one the wallet has dropped
// because another spent its inputs. It does nothing for a transaction that is
// not tracked, and does not touch one that has been proven.
func (m *Manager) Untrack(id chainhash.Hash) error {
	m.mu.Lock()
	delete(m.hints, id)
	m.mu.Unlock()
	return m.store.Save(Update{Untrack: []chainhash.Hash{id}})
}

// Refresh proves the wallet's tracked transactions that are mined, and finds
// the dissolutions of its masternodes in the history of the refund scripts of
// its shares. It does not read the history of the shared collateral script.
//
// What is stored stays while the block that proved it is still in the chain,
// whatever a server says now: a server that leaves a masternode out, or a spend
// out, cannot make the wallet forget it. A registration whose block is gone is
// tracked again and proven by the block it is in now. A dissolution whose block
// is gone is found again in the history of the refund scripts, or tracked with Track.
//
// A tracked transaction that cannot be proven yet stays tracked. Every error is
// returned, joined, once what could be proven is saved. A transaction below the
// headers cannot be proven ever, and is reported once, as ErrUnprovable, and
// no longer tracked.
func (m *Manager) Refresh(ctx context.Context) error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	snap, err := m.store.Load()
	if err != nil {
		return err
	}
	tip := m.chain.Tip()
	if m.chain.HeaderByHeight(tip) == nil {
		return fmt.Errorf("no header at tip %d", tip)
	}
	stored, err := parseRegistrations(snap)
	if err != nil {
		return err
	}
	up := Update{Dissolutions: make(map[chainhash.Hash]Spend)}
	live, known := m.reconcile(snap, stored, &up)

	// What is waiting to be proven: the stored ones, and those just dropped.
	tracked := make(map[chainhash.Hash][]byte, len(snap.Tracked)+len(up.Track))
	for id, raw := range snap.Tracked {
		tracked[id] = raw
	}
	for _, raw := range up.Track {
		tx, err := decodeTx(raw)
		if err != nil {
			return err
		}
		tracked[tx.TxHash()] = raw
	}
	ids := make([]chainhash.Hash, 0, len(tracked))
	for id := range tracked {
		ids = append(ids, id)
	}
	// A fixed order makes a scan repeatable.
	slices.SortFunc(ids, func(a, b chainhash.Hash) int { return slices.Compare(a[:], b[:]) })

	var errs []error
	unprovable := 0
	var dissolutions []*candidate
	for _, id := range ids {
		// Already proven and stored, so there is nothing to wait for.
		if _, ok := known[id]; ok {
			up.Untrack = append(up.Untrack, id)
			continue
		}
		raw := tracked[id]
		tx, err := decodeTx(raw)
		if err != nil {
			return err
		}
		c := &candidate{txid: id, raw: raw, tx: tx}
		if tx.Type != txTypeProReg {
			dissolutions = append(dissolutions, c)
			continue
		}
		if c.mn, err = RegistrationMN(tx, 1); err != nil {
			return err
		}
		height, block, found, err := m.proveMined(ctx, id, raw, tip)
		switch {
		case errors.Is(err, errBelowHeaders):
			unprovable++
			up.Untrack = append(up.Untrack, id)
		case err != nil:
			errs = append(errs, err)
		case found:
			c.mn, err = RegistrationMN(tx, height)
			if err != nil {
				return err
			}
			up.Registrations = append(up.Registrations, Registration{Height: height, Block: block, Tx: raw})
			live[id] = c.mn
			known[id] = struct{}{}
		}
	}

	// A dissolution is found in the history of the refund scripts of the shares
	// of the wallet's that are held.
	held := func() map[chainhash.Hash]*SharedMN {
		out := make(map[chainhash.Hash]*SharedMN, len(live))
		for h, mn := range live {
			if _, gone := up.Dissolutions[h]; !gone {
				out[h] = mn
			}
		}
		return out
	}
	found, err := m.refundHistory(ctx, held(), snap.OwnerKeyIDs, known, tip)
	if err != nil {
		errs = append(errs, err)
	}
	dissolutions = append(dissolutions, found...)

	for _, c := range dissolutions {
		for h, mn := range held() {
			if !mn.SpendsCollateral(c.tx) {
				continue
			}
			height, block, ok, err := m.proveMined(ctx, c.txid, c.raw, tip)
			switch {
			case errors.Is(err, errBelowHeaders):
				unprovable++
				up.Untrack = append(up.Untrack, c.txid)
				// Reported once: a refund history lists it again.
				m.seen[c.txid] = struct{}{}
			case err != nil:
				errs = append(errs, err)
			case ok:
				up.Dissolutions[h] = Spend{Height: height, Block: block, TxID: c.txid, Tx: c.raw}
			}
		}
	}

	if err := m.store.Save(up); err != nil {
		return err
	}
	after, err := m.store.Load()
	if err != nil {
		return err
	}
	if err := m.install(after); err != nil {
		return err
	}
	// A hint has done its work once the transaction is proven or given up.
	m.mu.Lock()
	for _, id := range up.Untrack {
		delete(m.hints, id)
	}
	for _, r := range up.Registrations {
		if tx, err := decodeTx(r.Tx); err == nil {
			delete(m.hints, tx.TxHash())
		}
	}
	for _, sp := range up.Dissolutions {
		delete(m.hints, sp.TxID)
	}
	m.mu.Unlock()
	if unprovable > 0 {
		errs = append(errs, fmt.Errorf("%w: %d skipped and no longer tracked", ErrUnprovable, unprovable))
	}
	return errors.Join(errs...)
}

// refundHistory reads the history of the refund script of every share of the
// wallet's in mns, and returns the transactions in it that spend a collateral
// of mns. A transaction that is not a dissolution is remembered, as what it
// spends cannot change. Entries that are not mined, or are above the headers, are left for a
// later Refresh. known are the transactions already stored.
func (m *Manager) refundHistory(ctx context.Context, mns map[chainhash.Hash]*SharedMN, owned map[[protx.KeyIDSize]byte]uint32, known map[chainhash.Hash]struct{}, tip int32) ([]*candidate, error) {
	// A dissolution is after every registration it dissolves.
	scripts := make(map[string]struct{})
	minHeight := int32(0)
	for _, mn := range mns {
		for i := range mn.Shares {
			if _, ok := owned[mn.Shares[i].KeyIDOwner]; !ok {
				continue
			}
			scripts[scriptHash(mn.Shares[i].RefundScript)] = struct{}{}
			if minHeight == 0 || mn.RegisteredHeight < minHeight {
				minHeight = mn.RegisteredHeight
			}
		}
	}
	hashes := make([]string, 0, len(scripts))
	for sh := range scripts {
		hashes = append(hashes, sh)
	}
	slices.Sort(hashes)

	var cands []*candidate
	listed := make(map[chainhash.Hash]struct{})
	for _, sh := range hashes {
		history, err := m.chain.GetHistory(ctx, sh)
		if err != nil {
			return cands, fmt.Errorf("refund script history: %w", err)
		}
		for _, e := range history {
			if e.Height <= 0 || e.Height > tip || e.Height < minHeight {
				continue
			}
			id, err := chainhash.NewHashFromStr(e.TxHash)
			if err != nil {
				return cands, fmt.Errorf("history txid %q: %w", e.TxHash, err)
			}
			if _, ok := known[*id]; ok {
				continue
			}
			if _, ok := m.seen[*id]; ok {
				continue
			}
			if _, ok := listed[*id]; ok {
				continue
			}
			listed[*id] = struct{}{}
			rawHex, err := m.chain.GetRawTransaction(ctx, e.TxHash)
			if err != nil {
				return cands, fmt.Errorf("transaction %s: %w", e.TxHash, err)
			}
			raw, err := hex.DecodeString(rawHex)
			if err != nil {
				return cands, fmt.Errorf("transaction %s: %w", e.TxHash, err)
			}
			tx, err := decodeTx(raw)
			if err != nil {
				return cands, fmt.Errorf("transaction %s: %w", e.TxHash, err)
			}
			// The proof is of the id, so the bytes must be the transaction with it.
			if tx.TxHash() != *id {
				return cands, fmt.Errorf("transaction %s is served as %s", e.TxHash, tx.TxHash())
			}
			spends := false
			for _, mn := range mns {
				if mn.SpendsCollateral(tx) {
					spends = true
					break
				}
			}
			if !spends {
				// Only a dissolution can spend a shared collateral, and one may
				// belong to a masternode that is not held yet, so only what is
				// not one is remembered.
				if tx.Type != txTypeProDis {
					m.seen[*id] = struct{}{}
				}
				continue
			}
			cands = append(cands, &candidate{height: e.Height, txid: *id, raw: raw, tx: tx})
		}
	}
	return cands, nil
}

// errBelowHeaders is returned by proveMined for a transaction in a block below
// the headers the chain holds, which cannot be proven.
var errBelowHeaders = errors.New("transaction is below the chain's headers")

// proveMined finds the block that holds the transaction and proves it by a
// merkle proof against the header the chain holds. found is false if it is not
// mined, or its block is not among the headers yet, and then it is looked for
// again. The height Track was given is tried first, unless it is below the
// headers, where nothing can be proven. Otherwise the server says which block,
// which is its word and only says where to ask: the proof decides. A block the
// chain has no header for is below its headers if the server's confirmations
// say so, and is errBelowHeaders.
func (m *Manager) proveMined(ctx context.Context, id chainhash.Hash, raw []byte, tip int32) (int32, chainhash.Hash, bool, error) {
	m.mu.RLock()
	hint := m.hints[id]
	m.mu.RUnlock()
	start := m.chain.StartHeight()
	if hint >= start && hint > 0 && hint <= tip {
		if block, err := m.prove(ctx, id, raw, hint); err == nil {
			return hint, block, true, nil
		}
	}
	v, err := m.chain.GetTransactionVerbose(ctx, id.String())
	if err != nil {
		return 0, chainhash.Hash{}, false, fmt.Errorf("transaction %s: %w", id, err)
	}
	if v.BlockHash == "" {
		return 0, chainhash.Hash{}, false, nil
	}
	bh, err := chainhash.NewHashFromStr(v.BlockHash)
	if err != nil {
		return 0, chainhash.Hash{}, false, fmt.Errorf("transaction %s: block hash %q: %w", id, v.BlockHash, err)
	}
	height, _, ok := m.chain.HeaderByHash(bh)
	if !ok {
		// The chain has no header for the block: it is below the headers it
		// holds, which the confirmations say, or it is not synced to it yet.
		if v.Confirmations > 0 && int64(tip)-int64(v.Confirmations)+1 < int64(start) {
			return 0, chainhash.Hash{}, false, errBelowHeaders
		}
		return 0, chainhash.Hash{}, false, nil
	}
	if height > tip {
		return 0, chainhash.Hash{}, false, nil
	}
	block, err := m.prove(ctx, id, raw, height)
	if err != nil {
		return 0, chainhash.Hash{}, false, err
	}
	return height, block, true, nil
}

// scriptHash is the electrum scripthash of a script: the sha256 of it, reversed,
// in hex.
func scriptHash(script []byte) string {
	sum := sha256.Sum256(script)
	slices.Reverse(sum[:])
	return hex.EncodeToString(sum[:])
}
