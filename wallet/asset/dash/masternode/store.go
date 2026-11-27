// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package masternode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
)

var (
	storeBucket  = []byte("dashmn")
	nextKey      = []byte("next")
	cursorKey    = []byte("cursor")
	txReadKey    = []byte("txread")
	ownersBucket = []byte("owners")
	regsBucket   = []byte("regs")
	versionKey   = []byte("version")
	// dissolvedBucket holds the proven spends of registered collaterals.
	dissolvedBucket = []byte("dissolved")
	// trackedBucket holds the wallet's own registrations and dissolutions that
	// are not proven yet, by transaction id, as raw transactions.
	trackedBucket = []byte("tracked")
	// walletBucket marks the tracked transactions that came from the wallet's
	// own transactions, by transaction id, as opposed to those a reorg put back
	// or that were added by id, which the wallet has no record of.
	walletBucket = []byte("fromwallet")
)

// storeVersion is the layout of the store. Once a store has shipped, a record
// format or bucket change raises it, so a store written by another layout is
// refused and not read as this one. None has shipped, so the buckets and records
// added while this is under review are part of version 1.
const storeVersion = 1

// legacyVersion is the layout of a store with no version recorded: the only
// layout before it was. It is history and does not follow storeVersion.
const legacyVersion = 1

// Store keeps what a shared masternode wallet cannot recompute: the owner
// index counter, the owner key ids it has derived, how far Discover has
// read, each registration found, as its raw transaction, and the wallet's
// own transactions it is waiting to prove. Everything else is rebuilt from
// these and the chain.
type Store struct {
	db walletdb.DB
}

var _ OwnerIndexStore = (*Store)(nil)

// version returns the layout b was written with, 0 if it has no version.
func version(b walletdb.ReadBucket) (uint32, error) {
	v := b.Get(versionKey)
	switch len(v) {
	case 0:
		return 0, nil
	case 4:
		return binary.BigEndian.Uint32(v), nil
	}
	return 0, fmt.Errorf("store version is %d bytes", len(v))
}

// checkVersion refuses a store of a layout this code does not read.
func checkVersion(v uint32) error {
	if v != storeVersion {
		return fmt.Errorf("masternode store is version %d, this wallet reads version %d", v, storeVersion)
	}
	return nil
}

// NewStore opens the store in db, creating it if needed. It fails on a store
// of another version.
func NewStore(db walletdb.DB) (*Store, error) {
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		b := tx.ReadWriteBucket(storeBucket)
		if b == nil {
			var err error
			if b, err = tx.CreateTopLevelBucket(storeBucket); err != nil {
				return err
			}
		}
		v, err := version(b)
		if err != nil {
			return err
		}
		if v == 0 {
			v = legacyVersion
		}
		if err := checkVersion(v); err != nil {
			return err
		}
		for _, name := range [][]byte{ownersBucket, regsBucket, dissolvedBucket, trackedBucket, walletBucket} {
			if _, err := b.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], v)
		return b.Put(versionKey, buf[:])
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Cursor is how far Discover has read the history of the shared collateral
// script: the tip it read to. It is empty until a Discover has finished. Hash
// is the double sha256 of the header at Height, a fingerprint that is only
// compared with itself and need not be the chain's block id.
type Cursor struct {
	Height int32
	Hash   chainhash.Hash
}

// Registration is a mined registration: its raw serialized transaction, the
// height of the block that holds it and that block's fingerprint (the double
// sha256 of its header, as in Cursor). While the chain's header at Height has
// that fingerprint the block is unchanged, so the registration is still mined.
type Registration struct {
	Height int32
	Block  chainhash.Hash
	Tx     []byte
}

// Spend is the proven spend of a registered collateral: the height of the
// block that holds it, that block's fingerprint and its transaction id. Tx is
// the raw transaction, so a spend whose block a reorg undoes can be proven
// again by the block it is in next: the refund scripts that find a dissolution
// do not show one the wallet's own actor made without paying itself. It may be
// empty, and then there is nothing to prove again.
type Spend struct {
	Height int32
	Block  chainhash.Hash
	TxID   chainhash.Hash
	Tx     []byte
}

// Snapshot is everything in the store.
type Snapshot struct {
	// NextOwner is the next owner index to hand out.
	NextOwner uint32
	// OwnerKeyIDs maps an owner key id to its index.
	OwnerKeyIDs map[[protx.KeyIDSize]byte]uint32
	Cursor      Cursor
	// TxRead is the height the wallet's transactions have been given to Track
	// through, or 0 if none have.
	TxRead        int32
	Registrations map[chainhash.Hash]Registration
	// Dissolutions are by the proTxHash of a stored registration.
	Dissolutions map[chainhash.Hash]Spend
	// Tracked are the raw transactions to prove, by transaction id.
	Tracked map[chainhash.Hash][]byte
	// FromWallet are the tracked ones the wallet had.
	FromWallet map[chainhash.Hash]struct{}
}

// Update is a change to the store, applied together or not at all.
//
// A registration saved with one of its owner key ids already in the owners
// bucket (or in this Update) raises the counter past that key's index, so a
// restored wallet never hands out an owner index already used: Core refuses a
// repeated owner key and reuse links registrations. Caching the key ids in the
// same Update as the registration is what makes that work.
type Update struct {
	// OwnerKeyIDs are owner key ids by index. They are a cache of public
	// data and never move the counter.
	OwnerKeyIDs map[uint32][protx.KeyIDSize]byte
	// MinNextOwner raises the owner index counter to at least this. It never
	// lowers it.
	MinNextOwner uint32
	// Registrations are put by their transaction id. A registration already
	// stored is replaced, as a reorg can move its height.
	Registrations []Registration
	// Dissolutions are put by proTxHash. Each must have a registration in the
	// store once the update is applied.
	Dissolutions map[chainhash.Hash]Spend
	// Drop removes these registrations and their dissolutions, and
	// DropDissolutions removes dissolutions only, before the others are put:
	// a reorg can undo them. Neither touches the owner key ids or the
	// counter, which only grow.
	Drop             []chainhash.Hash
	DropDissolutions []chainhash.Hash
	// Track puts raw transactions to prove: each must be a valid shared
	// registration or a provider dissolution. Untrack removes tracked ones by
	// transaction id. A registration saved or a dissolution saved in the same
	// Update is no longer tracked, and Track is applied after Drop, so a
	// registration a reorg dropped can be put back to be proven again.
	Track   [][]byte
	Untrack []chainhash.Hash
	// Wallet marks tracked transactions as the wallet's own, which it may
	// stop tracking once the wallet no longer has them.
	Wallet []chainhash.Hash
	// Cursor, if not nil, replaces the discovery cursor.
	Cursor *Cursor
	// TxRead, if not nil, replaces the height the wallet's transactions have
	// been read through.
	TxRead *int32
}

func nextOwner(b walletdb.ReadBucket) (uint32, error) {
	v := b.Get(nextKey)
	switch len(v) {
	case 0:
		return 0, nil
	case 4:
		return binary.BigEndian.Uint32(v), nil
	}
	return 0, fmt.Errorf("owner counter is %d bytes", len(v))
}

func putNext(b walletdb.ReadWriteBucket, n uint32) error {
	var v [4]byte
	binary.BigEndian.PutUint32(v[:], n)
	return b.Put(nextKey, v[:])
}

// ReserveOwnerIndex returns the next unused owner index and records it as used
// before returning.
func (s *Store) ReserveOwnerIndex() (uint32, error) {
	var idx uint32
	err := walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		b := tx.ReadWriteBucket(storeBucket)
		if b == nil {
			return errors.New("masternode store is missing")
		}
		n, err := nextOwner(b)
		if err != nil {
			return err
		}
		if n > maxMasternodeIdx {
			return errors.New("owner indexes are exhausted")
		}
		idx = n
		return putNext(b, n+1)
	})
	if err != nil {
		return 0, err
	}
	return idx, nil
}

func regValue(r Registration) []byte {
	v := make([]byte, 4, 36+len(r.Tx))
	binary.BigEndian.PutUint32(v, uint32(r.Height))
	v = append(v, r.Block[:]...)
	return append(v, r.Tx...)
}

// decodeTx deserializes a Dash transaction and refuses trailing bytes.
func decodeTx(raw []byte) (*bisonwire.Tx, error) {
	tx := &bisonwire.Tx{Chain: "dash"}
	r := bytes.NewReader(raw)
	if err := tx.Deserialize(r); err != nil {
		return nil, err
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("%d trailing bytes", r.Len())
	}
	return tx, nil
}

// decodeRegistration checks that raw is a valid shared registration mined at
// height and returns the masternode it registers.
func decodeRegistration(raw []byte, height int32) (*SharedMN, error) {
	tx, err := decodeTx(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRegistration, err)
	}
	return RegistrationMN(tx, height)
}

// trackable checks that raw is a transaction worth proving, a shared
// registration or a provider dissolution, and returns its id. Whether it is the
// wallet's is for the caller to say: the store only holds transactions of a
// kind that a masternode can be built from, so it cannot be filled with others.
func trackable(raw []byte) (chainhash.Hash, error) {
	tx, err := decodeTx(raw)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("tracked transaction: %w", err)
	}
	switch tx.Type {
	case txTypeProReg:
		// The height only has to be positive: it is not known yet.
		if _, err := RegistrationMN(tx, 1); err != nil {
			return chainhash.Hash{}, err
		}
	case txTypeProDis:
		var pd protx.ProDisTx
		if err := protx.Parse(tx.ExtraPayload, &pd); err != nil {
			return chainhash.Hash{}, fmt.Errorf("%w: payload: %w", ErrInvalidDissolution, err)
		}
	default:
		return chainhash.Hash{}, fmt.Errorf("tracked transaction: type %d is not a shared registration or dissolution", tx.Type)
	}
	return tx.TxHash(), nil
}

// Save applies u. A registration that is not a valid shared registration fails
// the whole update, so the store holds only valid ones.
func (s *Store) Save(u Update) error {
	hashes := make([]chainhash.Hash, len(u.Registrations))
	shareOwners := make(map[[protx.KeyIDSize]byte]struct{})
	for i, r := range u.Registrations {
		m, err := decodeRegistration(r.Tx, r.Height)
		if err != nil {
			return err
		}
		hashes[i] = m.ProTxHash
		for j := range m.Shares {
			shareOwners[m.Shares[j].KeyIDOwner] = struct{}{}
		}
	}
	for h, sp := range u.Dissolutions {
		if len(sp.Tx) == 0 {
			continue
		}
		id, err := trackable(sp.Tx)
		if err != nil {
			return fmt.Errorf("dissolution of %s: %w", h, err)
		}
		if id != sp.TxID {
			return fmt.Errorf("dissolution of %s: transaction is %s, not %s", h, id, sp.TxID)
		}
	}
	tracked := make([]chainhash.Hash, len(u.Track))
	for i, raw := range u.Track {
		id, err := trackable(raw)
		if err != nil {
			return err
		}
		tracked[i] = id
	}
	return walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		b := tx.ReadWriteBucket(storeBucket)
		if b == nil {
			return errors.New("masternode store is missing")
		}
		owners := b.NestedReadWriteBucket(ownersBucket)
		trk := b.NestedReadWriteBucket(trackedBucket)
		wl := b.NestedReadWriteBucket(walletBucket)
		if owners == nil || trk == nil || wl == nil || b.NestedReadWriteBucket(regsBucket) == nil || b.NestedReadWriteBucket(dissolvedBucket) == nil {
			return errors.New("masternode store is missing a bucket")
		}
		for idx, id := range u.OwnerKeyIDs {
			var k [4]byte
			binary.BigEndian.PutUint32(k[:], idx)
			if err := owners.Put(k[:], id[:]); err != nil {
				return err
			}
		}
		// An owner key in a saved registration is used, so the counter must
		// be past its index. This is enforced here and not left to the caller:
		// a counter that lags would hand out a used key again.
		floor := u.MinNextOwner
		err := owners.ForEach(func(k, v []byte) error {
			if len(k) != 4 || len(v) != protx.KeyIDSize {
				return errors.New("malformed owner record")
			}
			if _, used := shareOwners[[protx.KeyIDSize]byte(v)]; used {
				floor = max(floor, binary.BigEndian.Uint32(k)+1)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if floor > 0 {
			n, err := nextOwner(b)
			if err != nil {
				return err
			}
			if floor > n {
				if err := putNext(b, floor); err != nil {
					return err
				}
			}
		}
		regs := b.NestedReadWriteBucket(regsBucket)
		dis := b.NestedReadWriteBucket(dissolvedBucket)
		for _, h := range u.Drop {
			if err := regs.Delete(h[:]); err != nil {
				return err
			}
			if err := dis.Delete(h[:]); err != nil {
				return err
			}
		}
		for _, h := range u.DropDissolutions {
			if err := dis.Delete(h[:]); err != nil {
				return err
			}
		}
		untrack := func(h []byte) error {
			if err := wl.Delete(h); err != nil {
				return err
			}
			return trk.Delete(h)
		}
		for _, h := range u.Untrack {
			if err := untrack(h[:]); err != nil {
				return err
			}
		}
		for i, raw := range u.Track {
			if err := trk.Put(tracked[i][:], raw); err != nil {
				return err
			}
			// A transaction put back is no longer the wallet's to drop.
			if err := wl.Delete(tracked[i][:]); err != nil {
				return err
			}
		}
		for _, h := range u.Wallet {
			if trk.Get(h[:]) == nil {
				return errors.New("wallet mark of a transaction that is not tracked")
			}
			if err := wl.Put(h[:], []byte{1}); err != nil {
				return err
			}
		}
		for i, r := range u.Registrations {
			if err := regs.Put(hashes[i][:], regValue(r)); err != nil {
				return err
			}
			if err := untrack(hashes[i][:]); err != nil {
				return err
			}
		}
		for h, sp := range u.Dissolutions {
			if regs.Get(h[:]) == nil {
				return errors.New("dissolution of an unknown registration")
			}
			v := make([]byte, 4, 68+len(sp.Tx))
			binary.BigEndian.PutUint32(v, uint32(sp.Height))
			v = append(v, sp.Block[:]...)
			v = append(v, sp.TxID[:]...)
			if err := dis.Put(h[:], append(v, sp.Tx...)); err != nil {
				return err
			}
			if err := untrack(sp.TxID[:]); err != nil {
				return err
			}
		}
		if u.Cursor != nil {
			v := make([]byte, 4, 36)
			binary.BigEndian.PutUint32(v, uint32(u.Cursor.Height))
			if err := b.Put(cursorKey, append(v, u.Cursor.Hash[:]...)); err != nil {
				return err
			}
		}
		if u.TxRead != nil {
			v := make([]byte, 4)
			binary.BigEndian.PutUint32(v, uint32(*u.TxRead))
			if err := b.Put(txReadKey, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// Load returns the store's contents. A malformed record is an error. The
// registrations are raw: the caller runs RegistrationMN on them.
func (s *Store) Load() (Snapshot, error) {
	snap := Snapshot{
		OwnerKeyIDs:   make(map[[protx.KeyIDSize]byte]uint32),
		Registrations: make(map[chainhash.Hash]Registration),
		Dissolutions:  make(map[chainhash.Hash]Spend),
		Tracked:       make(map[chainhash.Hash][]byte),
		FromWallet:    make(map[chainhash.Hash]struct{}),
	}
	err := walletdb.View(s.db, func(tx walletdb.ReadTx) error {
		b := tx.ReadBucket(storeBucket)
		if b == nil {
			return errors.New("masternode store is missing")
		}
		v, err := version(b)
		if err != nil {
			return err
		}
		if err := checkVersion(v); err != nil {
			return err
		}
		for _, name := range [][]byte{ownersBucket, regsBucket, dissolvedBucket, trackedBucket, walletBucket} {
			if b.NestedReadBucket(name) == nil {
				return errors.New("masternode store is missing a bucket")
			}
		}
		if snap.NextOwner, err = nextOwner(b); err != nil {
			return err
		}
		if v := b.Get(cursorKey); v != nil {
			if len(v) != 36 {
				return fmt.Errorf("cursor is %d bytes", len(v))
			}
			snap.Cursor.Height = int32(binary.BigEndian.Uint32(v))
			copy(snap.Cursor.Hash[:], v[4:])
		}
		if v := b.Get(txReadKey); v != nil {
			if len(v) != 4 {
				return fmt.Errorf("transaction height is %d bytes", len(v))
			}
			snap.TxRead = int32(binary.BigEndian.Uint32(v))
		}
		err = b.NestedReadBucket(ownersBucket).ForEach(func(k, v []byte) error {
			if len(k) != 4 || len(v) != protx.KeyIDSize {
				return errors.New("malformed owner record")
			}
			var id [protx.KeyIDSize]byte
			copy(id[:], v)
			if _, dup := snap.OwnerKeyIDs[id]; dup {
				return errors.New("two owner indexes have one key id")
			}
			snap.OwnerKeyIDs[id] = binary.BigEndian.Uint32(k)
			return nil
		})
		if err != nil {
			return err
		}
		err = b.NestedReadBucket(regsBucket).ForEach(func(k, v []byte) error {
			if len(k) != chainhash.HashSize || len(v) <= 36 {
				return errors.New("malformed registration record")
			}
			var h chainhash.Hash
			copy(h[:], k)
			r := Registration{
				Height: int32(binary.BigEndian.Uint32(v)),
				Tx:     bytes.Clone(v[36:]),
			}
			copy(r.Block[:], v[4:36])
			snap.Registrations[h] = r
			return nil
		})
		if err != nil {
			return err
		}
		err = b.NestedReadBucket(trackedBucket).ForEach(func(k, v []byte) error {
			if len(k) != chainhash.HashSize || len(v) == 0 {
				return errors.New("malformed tracked record")
			}
			var h chainhash.Hash
			copy(h[:], k)
			snap.Tracked[h] = bytes.Clone(v)
			return nil
		})
		if err != nil {
			return err
		}
		err = b.NestedReadBucket(walletBucket).ForEach(func(k, v []byte) error {
			if len(k) != chainhash.HashSize {
				return errors.New("malformed wallet mark")
			}
			var h chainhash.Hash
			copy(h[:], k)
			snap.FromWallet[h] = struct{}{}
			return nil
		})
		if err != nil {
			return err
		}
		return b.NestedReadBucket(dissolvedBucket).ForEach(func(k, v []byte) error {
			if len(k) != chainhash.HashSize || len(v) < 68 {
				return errors.New("malformed dissolution record")
			}
			var h chainhash.Hash
			copy(h[:], k)
			if _, ok := snap.Registrations[h]; !ok {
				return errors.New("dissolution of an unknown registration")
			}
			sp := Spend{Height: int32(binary.BigEndian.Uint32(v))}
			copy(sp.Block[:], v[4:36])
			copy(sp.TxID[:], v[36:68])
			if len(v) > 68 {
				sp.Tx = bytes.Clone(v[68:])
			}
			snap.Dissolutions[h] = sp
			return nil
		})
	})
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}
