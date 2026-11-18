// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
)

const (
	// sharedCollateralScripthash is the electrum scripthash of
	// SharedCollateralScript: every shared registration and every spend of a
	// shared collateral is in its history.
	sharedCollateralScripthash = "53736f32ba7e196c3b66cac97b64773e6ab03c42da90e407c23049029385e4bd"

	// ownerGap is how many owner indexes past the last used one discovery
	// derives before it stops looking for the wallet's registrations.
	ownerGap = 20

	// rescanDepth is how many blocks below the cursor an incremental scan
	// reads again, in case the server's history lags its headers. An entry
	// the server lists more than this many blocks late is not seen until the
	// next Discover.
	rescanDepth = 6
)

var (
	// ErrUnknownMN is returned for a masternode the wallet has not found.
	ErrUnknownMN = errors.New("not a shared masternode of this wallet")
	// ErrDissolved is returned for a masternode whose collateral a mined
	// transaction has spent.
	ErrDissolved = errors.New("shared masternode is dissolved")
	// ErrUnprovable is returned by a scan that finished but skipped a
	// transaction of the wallet's that is below the headers the chain holds, so
	// it cannot be proven. What the scan proved is saved.
	ErrUnprovable = errors.New("transaction below the chain's headers cannot be proven")
	// ErrStale is returned when the server's info is not for the chain tip
	// the wallet has. The caller may retry.
	ErrStale = errors.New("server masternode info is not at the tip")
)

// Chain is what the Manager needs of the chain. *electrum.Network satisfies it.
type Chain interface {
	Tip() int32
	// StartHeight is the lowest height the chain holds a header for. A block
	// below it is final but cannot be proven against a header.
	StartHeight() int32
	// HeaderByHeight returns nil if the header is not known.
	HeaderByHeight(height int32) *wire.BlockHeader
	GetHistory(ctx context.Context, scripthash string) ([]electrum.HistoryEntry, error)
	// GetRawTransaction returns the hex of a transaction that hashes to txid.
	GetRawTransaction(ctx context.Context, txid string) (string, error)
	GetMerkle(ctx context.Context, txid string, height int32) (*electrum.MerkleProof, error)
	ProTxInfo(ctx context.Context, proTxHash chainhash.Hash) (*electrum.ProTxInfo, error)
}

var _ Chain = (*electrum.Network)(nil)

// Manager finds the shared masternodes the wallet owns a share of, keeps them
// current and tells their state. A masternode is only ever taken from a mined
// registration proven by a merkle proof against a header the chain holds; what
// the server says about it afterwards is applied to a copy, in SharedMN.
type Manager struct {
	chain    Chain
	store    *Store
	keys     KeyDeriver
	coinType uint32
	net      *chaincfg.Params

	// syncMu serialises Discover and Refresh, which hold it across network
	// calls. Lock order: syncMu before mu.
	syncMu sync.Mutex

	// mu guards the maps and is never held across a network call. The
	// masternodes in mns are not changed once stored.
	mu        sync.RWMutex
	mns       map[chainhash.Hash]*SharedMN
	dissolved map[chainhash.Hash]Spend
	owned     map[[protx.KeyIDSize]byte]uint32 // the wallet's owner key ids by index
}

// NewManager loads the stored state. It makes no network calls.
func NewManager(chain Chain, store *Store, keys KeyDeriver, coinType uint32, net *chaincfg.Params) (*Manager, error) {
	m := &Manager{chain: chain, store: store, keys: keys, coinType: coinType, net: net}
	snap, err := store.Load()
	if err != nil {
		return nil, err
	}
	if err := m.install(snap); err != nil {
		return nil, err
	}
	return m, nil
}

// parseRegistrations rebuilds the masternodes of a snapshot.
func parseRegistrations(snap Snapshot) (map[chainhash.Hash]*SharedMN, error) {
	mns := make(map[chainhash.Hash]*SharedMN, len(snap.Registrations))
	for h, r := range snap.Registrations {
		mn, err := decodeRegistration(r.Tx, r.Height)
		if err != nil {
			return nil, fmt.Errorf("stored registration %s: %w", h, err)
		}
		if mn.ProTxHash != h {
			return nil, fmt.Errorf("stored registration %s is %s", h, mn.ProTxHash)
		}
		mns[h] = mn
	}
	return mns, nil
}

// install makes snap the in-memory state.
func (m *Manager) install(snap Snapshot) error {
	mns, err := parseRegistrations(snap)
	if err != nil {
		return err
	}
	owned := make(map[[protx.KeyIDSize]byte]uint32, len(snap.OwnerKeyIDs))
	for id, idx := range snap.OwnerKeyIDs {
		owned[id] = idx
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mns = mns
	m.dissolved = snap.Dissolutions
	// The cached ids only grow. PrimeOwnerKeys adds to them without waiting for
	// a scan, so a scan that read the store before it must not drop what it added.
	if m.owned == nil {
		m.owned = make(map[[protx.KeyIDSize]byte]uint32, len(owned))
	}
	for id, idx := range owned {
		m.owned[id] = idx
	}
	return nil
}

func (mn *SharedMN) clone() *SharedMN {
	c := *mn
	c.Shares = make([]protx.Share, len(mn.Shares))
	for i, s := range mn.Shares {
		s.RefundScript = append([]byte(nil), s.RefundScript...)
		s.RewardScript = append([]byte(nil), s.RewardScript...)
		c.Shares[i] = s
	}
	return &c
}

// ownerKeyID is the key id of the owner key at index idx.
func ownerKeyID(d KeyDeriver, coinType, idx uint32) ([protx.KeyIDSize]byte, error) {
	var id [protx.KeyIDSize]byte
	key, err := OwnerKey(d, coinType, idx)
	if err != nil {
		return id, fmt.Errorf("derive owner key %d: %w", idx, err)
	}
	defer key.Zero()
	copy(id[:], btcutil.Hash160(key.PubKey().SerializeCompressed()))
	return id, nil
}

// prove checks that the transaction raw, with id txid, is in the block at
// height, by a merkle proof against the header the chain holds, and returns the
// fingerprint of that block.
func (m *Manager) prove(ctx context.Context, txid chainhash.Hash, raw []byte, height int32) (chainhash.Hash, error) {
	// A 64 byte transaction can be an interior node of the merkle tree.
	if len(raw) == 64 {
		return chainhash.Hash{}, fmt.Errorf("transaction %s is 64 bytes", txid)
	}
	hdr := m.chain.HeaderByHeight(height)
	if hdr == nil {
		return chainhash.Hash{}, fmt.Errorf("no header at height %d", height)
	}
	proof, err := m.chain.GetMerkle(ctx, txid.String(), height)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("merkle proof of %s: %w", txid, err)
	}
	if proof.BlockHeight != height {
		return chainhash.Hash{}, fmt.Errorf("merkle proof of %s is for height %d, not %d", txid, proof.BlockHeight, height)
	}
	root, err := electrum.MerkleRoot(txid, proof)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("merkle proof of %s: %w", txid, err)
	}
	if root != hdr.MerkleRoot {
		return chainhash.Hash{}, fmt.Errorf("merkle proof of %s does not match the header at %d", txid, height)
	}
	return hdr.BlockHash(), nil
}

// unchanged reports whether the block at height is still the one with the
// fingerprint block. A block below the chain's headers is final and so
// unchanged: a reorg cannot reach it.
func (m *Manager) unchanged(height int32, block chainhash.Hash) bool {
	if height < m.chain.StartHeight() {
		return true
	}
	hdr := m.chain.HeaderByHeight(height)
	return hdr != nil && hdr.BlockHash() == block
}

// Discover reads the whole history of the shared collateral script. Use it to
// restore a wallet.
func (m *Manager) Discover(ctx context.Context) error {
	return m.scan(ctx, true)
}

// Refresh reads what is new since the last scan. If the chain has reorganised
// since, it reads everything again.
//
// Either way, what is stored stays while the block that proved it is still in
// the chain, whatever the server's history says now: a server that leaves
// a masternode out, or a spend out, cannot make the wallet forget it. It is
// dropped only when its block is gone, and found again if the new chain has it.
func (m *Manager) Refresh(ctx context.Context) error {
	return m.scan(ctx, false)
}

type candidate struct {
	height int32
	txid   chainhash.Hash
	raw    []byte
	tx     *bisonwire.Tx
	mn     *SharedMN // set for a registration
}

func (m *Manager) scan(ctx context.Context, full bool) error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	snap, err := m.store.Load()
	if err != nil {
		return err
	}
	tip := m.chain.Tip()
	tipHdr := m.chain.HeaderByHeight(tip)
	if tipHdr == nil {
		return fmt.Errorf("no header at tip %d", tip)
	}
	if !full {
		cur := snap.Cursor
		if cur.Height <= 0 {
			full = true
		} else if h := m.chain.HeaderByHeight(cur.Height); h == nil || h.BlockHash() != cur.Hash {
			full = true
		}
	}
	from := int32(0)
	if !full {
		from = max(snap.Cursor.Height-rescanDepth, 0)
	}

	stored, err := parseRegistrations(snap)
	if err != nil {
		return err
	}
	up := Update{
		Dissolutions: make(map[chainhash.Hash]Spend),
		Cursor:       &Cursor{Height: tip, Hash: tipHdr.BlockHash()},
	}
	// What is stored stays while its block does. A registration whose block is
	// gone is dropped with its spend, and a spend whose block is gone alone.
	live := make(map[chainhash.Hash]*SharedMN)
	known := make(map[chainhash.Hash]struct{})
	for h, mn := range stored {
		r := snap.Registrations[h]
		if !m.unchanged(r.Height, r.Block) {
			up.Drop = append(up.Drop, h)
			continue
		}
		live[h] = mn
		known[h] = struct{}{}
		if sp, ok := snap.Dissolutions[h]; ok {
			if !m.unchanged(sp.Height, sp.Block) {
				up.DropDissolutions = append(up.DropDissolutions, h)
				continue
			}
			up.Dissolutions[h] = sp
			known[sp.TxID] = struct{}{}
		}
	}

	history, err := m.chain.GetHistory(ctx, sharedCollateralScripthash)
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}
	var cands []*candidate
	for _, e := range history {
		// Mempool entries are not proven, and entries above the headers are
		// read once the headers catch up.
		if e.Height <= from || e.Height > tip {
			continue
		}
		id, err := chainhash.NewHashFromStr(e.TxHash)
		if err != nil {
			return fmt.Errorf("history txid %q: %w", e.TxHash, err)
		}
		if _, ok := known[*id]; ok {
			continue
		}
		rawHex, err := m.chain.GetRawTransaction(ctx, e.TxHash)
		if err != nil {
			return fmt.Errorf("transaction %s: %w", e.TxHash, err)
		}
		raw, err := hex.DecodeString(rawHex)
		if err != nil {
			return fmt.Errorf("transaction %s: %w", e.TxHash, err)
		}
		tx, err := decodeTx(raw)
		if err != nil {
			return fmt.Errorf("transaction %s: %w", e.TxHash, err)
		}
		// The proof is of the id, so the bytes must be the transaction with it.
		if tx.TxHash() != *id {
			return fmt.Errorf("transaction %s is served as %s", e.TxHash, tx.TxHash())
		}
		c := &candidate{height: e.Height, txid: *id, raw: raw, tx: tx}
		if tx.Type == txTypeProReg {
			// A registration that is not a valid shared one is not ours
			// to track.
			if mn, err := RegistrationMN(tx, e.Height); err == nil {
				c.mn = mn
			}
		}
		cands = append(cands, c)
	}

	ours, newIDs, err := m.matchOwners(snap, cands)
	if err != nil {
		return err
	}
	up.OwnerKeyIDs = newIDs

	// A transaction below the chain's headers has no header to prove it
	// against. It is skipped, and the scan says so once it has saved the rest.
	start := m.chain.StartHeight()
	unprovable := 0
	for _, c := range cands {
		if c.mn == nil || !isOurs(c.mn, ours) {
			continue
		}
		if c.height < start {
			unprovable++
			continue
		}
		block, err := m.prove(ctx, c.txid, c.raw, c.height)
		if err != nil {
			return err
		}
		up.Registrations = append(up.Registrations, Registration{Height: c.height, Block: block, Tx: c.raw})
		live[c.txid] = c.mn
	}
	for _, c := range cands {
		if c.mn != nil {
			continue
		}
		for h, mn := range live {
			if !mn.SpendsCollateral(c.tx) {
				continue
			}
			if c.height < start {
				unprovable++
				continue
			}
			block, err := m.prove(ctx, c.txid, c.raw, c.height)
			if err != nil {
				return err
			}
			up.Dissolutions[h] = Spend{Height: c.height, Block: block, TxID: c.txid}
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
	if unprovable > 0 {
		return fmt.Errorf("%w: %d skipped", ErrUnprovable, unprovable)
	}
	return nil
}

func isOurs(mn *SharedMN, ours map[[protx.KeyIDSize]byte]struct{}) bool {
	for i := range mn.Shares {
		if _, ok := ours[mn.Shares[i].KeyIDOwner]; ok {
			return true
		}
	}
	return false
}

// matchOwners finds which owner key ids in the registrations among cands are
// the wallet's: those it derives at an index below the owner counter plus the
// gap, where each hit moves the limit to the gap past it. It returns the set of
// the wallet's ids found and the ids it had to derive, which the caller caches.
func (m *Manager) matchOwners(snap Snapshot, cands []*candidate) (map[[protx.KeyIDSize]byte]struct{}, map[uint32][protx.KeyIDSize]byte, error) {
	want := make(map[[protx.KeyIDSize]byte]struct{})
	for _, c := range cands {
		if c.mn == nil {
			continue
		}
		for i := range c.mn.Shares {
			want[c.mn.Shares[i].KeyIDOwner] = struct{}{}
		}
	}
	ours := make(map[[protx.KeyIDSize]byte]struct{})
	newIDs := make(map[uint32][protx.KeyIDSize]byte)
	if len(want) == 0 {
		return ours, newIDs, nil
	}
	cached := make(map[uint32][protx.KeyIDSize]byte, len(snap.OwnerKeyIDs))
	for id, idx := range snap.OwnerKeyIDs {
		cached[idx] = id
	}
	limit := uint64(snap.NextOwner) + ownerGap
	for idx := uint32(0); uint64(idx) < limit && idx <= maxMasternodeIdx; idx++ {
		id, ok := cached[idx]
		if !ok {
			var err error
			if id, err = ownerKeyID(m.keys, m.coinType, idx); err != nil {
				return nil, nil, err
			}
			newIDs[idx] = id
		}
		if _, hit := want[id]; hit {
			ours[id] = struct{}{}
			limit = max(limit, uint64(idx)+1+ownerGap)
		}
	}
	return ours, newIDs, nil
}

// SharedMN returns a copy of the masternode with hash. With fresh set, the
// copy carries the server's current reward scripts, voting key and operator
// key, which are the server's word and not verified (see ApplyInfo); without
// it, they are as registered. The stored masternode is never changed.
//
// A server that has no info returns an error wrapping electrum.ErrProTxNotFound,
// which does not mean the masternode is dissolved. A reply that is not for the
// wallet's tip is ErrStale. A reply that contradicts the registration, which a
// reorg can cause, is retried once after a Refresh.
func (m *Manager) SharedMN(ctx context.Context, hash chainhash.Hash, fresh bool) (*SharedMN, error) {
	if !fresh {
		return m.copyOf(hash)
	}
	c, _, err := m.Reported(ctx, hash)
	return c, err
}

// Reported is SharedMN with the server's data applied. It also returns the
// reply that was checked against the registration, for what only the server
// knows: banned state and the last paid height. The reply is untrusted beyond
// the fields ApplyInfo matched.
func (m *Manager) Reported(ctx context.Context, hash chainhash.Hash) (*SharedMN, *electrum.ProTxInfo, error) {
	c, err := m.copyOf(hash)
	if err != nil {
		return nil, nil, err
	}
	for attempt := 0; ; attempt++ {
		info, err := m.applyFresh(ctx, c)
		if err == nil {
			return c, info, nil
		}
		if !errors.Is(err, ErrInfoMismatch) || attempt > 0 {
			return nil, nil, err
		}
		if err := m.Refresh(ctx); err != nil {
			return nil, nil, err
		}
		if c, err = m.copyOf(hash); err != nil {
			return nil, nil, err
		}
	}
}

func (m *Manager) copyOf(hash chainhash.Hash) (*SharedMN, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mn, ok := m.mns[hash]
	if !ok {
		return nil, ErrUnknownMN
	}
	if _, gone := m.dissolved[hash]; gone {
		return nil, ErrDissolved
	}
	return mn.clone(), nil
}

func (m *Manager) applyFresh(ctx context.Context, c *SharedMN) (*electrum.ProTxInfo, error) {
	info, err := m.chain.ProTxInfo(ctx, c.ProTxHash)
	if err != nil {
		return nil, fmt.Errorf("protx info: %w", err)
	}
	// The reply's confirmations count from the block that registered the
	// masternode, so they say which tip the server answered for.
	if int64(info.State.RegisteredHeight)+int64(info.Confirmations)-1 != int64(m.chain.Tip()) {
		return nil, ErrStale
	}
	if err := c.ApplyInfo(info, m.net); err != nil {
		return nil, err
	}
	return info, nil
}

// StakedAmount is the total of the wallet's shares in masternodes that are not
// dissolved. A spend that is not mined yet does not count as a dissolution.
func (m *Manager) StakedAmount() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total int64
	for h, mn := range m.mns {
		if _, gone := m.dissolved[h]; gone {
			continue
		}
		for i := range mn.Shares {
			if _, ok := m.owned[mn.Shares[i].KeyIDOwner]; ok {
				total += mn.Shares[i].Amount
			}
		}
	}
	return total
}

// OwnerIndex returns the owner index of one of the wallet's owner key ids. It
// knows only the ids that have been derived and cached.
func (m *Manager) OwnerIndex(id [protx.KeyIDSize]byte) (uint32, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	idx, ok := m.owned[id]
	return idx, ok
}

// NeedsKeys reports whether a scan could need a key that is not cached, so that
// it fails while the wallet is locked: some index below the owner counter plus
// the gap has no cached key id.
func (m *Manager) NeedsKeys() (bool, error) {
	snap, err := m.store.Load()
	if err != nil {
		return false, err
	}
	return uint64(len(snap.OwnerKeyIDs)) < uint64(snap.NextOwner)+ownerGap, nil
}

// PrimeOwnerKeys derives and caches the owner key id of every index below the
// owner counter plus the gap, which is every index a scan looks at, so that a
// scan needs no key while the wallet is locked. It needs the wallet unlocked if
// anything is missing, and does not wait for a scan.
func (m *Manager) PrimeOwnerKeys() error {
	snap, err := m.store.Load()
	if err != nil {
		return err
	}
	cached := make(map[uint32]struct{}, len(snap.OwnerKeyIDs))
	for _, idx := range snap.OwnerKeyIDs {
		cached[idx] = struct{}{}
	}
	newIDs := make(map[uint32][protx.KeyIDSize]byte)
	limit := uint64(snap.NextOwner) + ownerGap
	for idx := uint32(0); uint64(idx) < limit && idx <= maxMasternodeIdx; idx++ {
		if _, ok := cached[idx]; ok {
			continue
		}
		id, err := ownerKeyID(m.keys, m.coinType, idx)
		if err != nil {
			return err
		}
		newIDs[idx] = id
	}
	if len(newIDs) == 0 {
		return nil
	}
	if err := m.store.Save(Update{OwnerKeyIDs: newIDs}); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for idx, id := range newIDs {
		m.owned[id] = idx
	}
	return nil
}

// ReserveOwner reserves the next owner index and returns it with its key id,
// which goes in a share table. The index is recorded as used before the key is
// derived (see NewShareOwner), and the ids a scan will need are cached after, so
// that the next scan does not need the wallet unlocked. If that fails the index
// stays reserved.
func (m *Manager) ReserveOwner() (uint32, [protx.KeyIDSize]byte, error) {
	idx, id, err := NewShareOwner(m.store, m.keys, m.coinType)
	if err != nil {
		return 0, id, err
	}
	if err := m.PrimeOwnerKeys(); err != nil {
		return 0, id, fmt.Errorf("owner index %d is reserved: %w", idx, err)
	}
	return idx, id, nil
}

// Owned is a masternode the wallet has a share of.
type Owned struct {
	MN *SharedMN
	// Shares are the indexes in MN.Shares of the wallet's own shares.
	Shares []int
	// Dissolution is the proven spend of the collateral, or nil.
	Dissolution *Spend
}

// List returns copies of the wallet's masternodes, oldest first, as registered
// (not with the server's info), with the dissolved ones.
func (m *Manager) List() []Owned {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Owned, 0, len(m.mns))
	for h, mn := range m.mns {
		o := Owned{MN: mn.clone()}
		for i := range mn.Shares {
			if _, ok := m.owned[mn.Shares[i].KeyIDOwner]; ok {
				o.Shares = append(o.Shares, i)
			}
		}
		if sp, ok := m.dissolved[h]; ok {
			o.Dissolution = &sp
		}
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b Owned) int {
		if c := cmp.Compare(a.MN.RegisteredHeight, b.MN.RegisteredHeight); c != 0 {
			return c
		}
		return bytes.Compare(a.MN.ProTxHash[:], b.MN.ProTxHash[:])
	})
	return out
}
