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
	ownersBucket = []byte("owners")
	regsBucket   = []byte("regs")
	versionKey   = []byte("version")
)

// storeVersion is the layout of the store. A record format or bucket change
// raises it, so a store written by another layout is refused and not read as
// this one.
const storeVersion = 1

// legacyVersion is the layout of a store with no version recorded: the only
// layout before it was. It is history and does not follow storeVersion.
const legacyVersion = 1

// Store keeps what a shared masternode wallet cannot recompute: the owner
// index counter, the owner key ids it has derived, how far discovery has
// scanned, and each registration found, as its raw transaction. Everything else
// is rebuilt from these and the chain.
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
		for _, name := range [][]byte{ownersBucket, regsBucket} {
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

// Cursor is how far discovery has scanned: the tip it scanned to.
type Cursor struct {
	Height int32
	Hash   chainhash.Hash
}

// Registration is a mined registration: its raw serialized transaction and
// the height of the block that holds it.
type Registration struct {
	Height int32
	Tx     []byte
}

// Snapshot is everything in the store.
type Snapshot struct {
	// NextOwner is the next owner index to hand out.
	NextOwner uint32
	// OwnerKeyIDs maps an owner key id to its index.
	OwnerKeyIDs   map[[protx.KeyIDSize]byte]uint32
	Cursor        Cursor
	Registrations map[chainhash.Hash]Registration
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
	// Cursor, if not nil, replaces the scan cursor.
	Cursor *Cursor
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
	v := make([]byte, 4, 4+len(r.Tx))
	binary.BigEndian.PutUint32(v, uint32(r.Height))
	return append(v, r.Tx...)
}

// decodeRegistration checks that raw is a valid shared registration mined at
// height and returns its transaction id.
// It also returns the owner key ids of its shares.
func decodeRegistration(raw []byte, height int32) (chainhash.Hash, [][protx.KeyIDSize]byte, error) {
	var tx bisonwire.Tx
	tx.Chain = "dash"
	r := bytes.NewReader(raw)
	if err := tx.Deserialize(r); err != nil {
		return chainhash.Hash{}, nil, fmt.Errorf("%w: %w", ErrInvalidRegistration, err)
	}
	if r.Len() != 0 {
		return chainhash.Hash{}, nil, badReg("%d trailing bytes", r.Len())
	}
	m, err := RegistrationMN(&tx, height)
	if err != nil {
		return chainhash.Hash{}, nil, err
	}
	ids := make([][protx.KeyIDSize]byte, len(m.Shares))
	for i := range m.Shares {
		ids[i] = m.Shares[i].KeyIDOwner
	}
	return m.ProTxHash, ids, nil
}

// Save applies u. A registration that is not a valid shared registration fails
// the whole update, so the store holds only valid ones.
func (s *Store) Save(u Update) error {
	hashes := make([]chainhash.Hash, len(u.Registrations))
	shareOwners := make(map[[protx.KeyIDSize]byte]struct{})
	for i, r := range u.Registrations {
		h, ids, err := decodeRegistration(r.Tx, r.Height)
		if err != nil {
			return err
		}
		hashes[i] = h
		for _, id := range ids {
			shareOwners[id] = struct{}{}
		}
	}
	return walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		b := tx.ReadWriteBucket(storeBucket)
		if b == nil {
			return errors.New("masternode store is missing")
		}
		owners := b.NestedReadWriteBucket(ownersBucket)
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
		for i, r := range u.Registrations {
			if err := regs.Put(hashes[i][:], regValue(r)); err != nil {
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
		return nil
	})
}

// Load returns the store's contents. A malformed record is an error. The
// registrations are raw: the caller runs RegistrationMN on them.
func (s *Store) Load() (Snapshot, error) {
	snap := Snapshot{
		OwnerKeyIDs:   make(map[[protx.KeyIDSize]byte]uint32),
		Registrations: make(map[chainhash.Hash]Registration),
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
		return b.NestedReadBucket(regsBucket).ForEach(func(k, v []byte) error {
			if len(k) != chainhash.HashSize || len(v) <= 4 {
				return errors.New("malformed registration record")
			}
			var h chainhash.Hash
			copy(h[:], k)
			snap.Registrations[h] = Registration{
				Height: int32(binary.BigEndian.Uint32(v)),
				Tx:     bytes.Clone(v[4:]),
			}
			return nil
		})
	})
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}
