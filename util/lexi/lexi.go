// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/dgraph-io/badger/v4"
)

// ErrKeyNotFound is returned when a requested key is not in the database. It
// is kvstore's sentinel, matching glossi, so consumers don't have to care
// which engine is in use; badger.ErrKeyNotFound is translated to it at this
// package's boundary (see convertError).
const ErrKeyNotFound = kvstore.ErrKeyNotFound

// ErrDBClosed is returned by Update once the context passed to Connect has been
// canceled. It is an alias for badger.ErrDBClosed, which is what a write that
// slipped through to a closed Badger would have returned anyway.
var ErrDBClosed = badger.ErrDBClosed

func convertError(err error) error {
	switch {
	case errors.Is(err, badger.ErrKeyNotFound):
		return ErrKeyNotFound
	}
	return err
}

// The engine chooser (util/kvstore/engine) only compiles this engine's
// conformance on desktop builds, so it is asserted here for every build.
var (
	_ kvstore.Store      = (*DB)(nil)
	_ kvstore.StoreTable = (*Table)(nil)
	_ kvstore.StoreIndex = (*Index)(nil)
	_ kvstore.StoreIter  = (*Iter)(nil)
)

// DB is the Lexi DB. The Lexi DB wraps a badger key-value database and provides
// the ability to add indexed data.
type DB struct {
	*badger.DB
	log   util.Logger
	idSeq *badger.Sequence
	wg    sync.WaitGroup

	// closeMtx guards closed and inFlight, and drained wakes a shutdown once
	// the last write retires.
	//
	// Update registers under the mutex rather than holding it for the duration
	// of the write. Holding a read lock across the write instead would deadlock
	// a nested Update: Go's RWMutex blocks new readers once a writer is
	// waiting, so a shutdown arriving mid-write would wedge the inner
	// acquisition against the outer one that can never release. A waitgroup
	// does not work either, since an Update that observed "not closing" could
	// call Add after Wait had already returned zero and commit into a closing
	// database. Counting under the same mutex that publishes closed keeps the
	// check and the registration atomic without spanning the write.
	closeMtx sync.Mutex
	drained  *sync.Cond
	closed   bool
	inFlight int
}

// beginUpdate registers an in-flight write, or reports that the database is
// closing and no new write may start.
func (db *DB) beginUpdate() error {
	db.closeMtx.Lock()
	defer db.closeMtx.Unlock()
	if db.closed {
		return ErrDBClosed
	}
	db.inFlight++
	return nil
}

// endUpdate retires an in-flight write, waking a shutdown waiting to drain.
func (db *DB) endUpdate() {
	db.closeMtx.Lock()
	defer db.closeMtx.Unlock()
	db.inFlight--
	if db.inFlight == 0 {
		db.drained.Broadcast()
	}
}

// closeGate marks the database closed and waits for in-flight writes to retire.
// A nested Update that starts after this point fails with ErrDBClosed, which
// rolls its outer transaction back rather than leaving it stuck.
func (db *DB) closeGate() {
	db.closeMtx.Lock()
	defer db.closeMtx.Unlock()
	db.closed = true
	for db.inFlight > 0 {
		db.drained.Wait()
	}
}

// Config is the configuration settings for the Lexi DB.
type Config struct {
	Path string
	Log  util.Logger
}

// layoutVersion identifies the on-disk key layout. Version 1 is the row-keyed
// layout: rows stored under tablePrefix||userKey, the row's KeyID carried in
// the datum, and the KeyID→key map under IDToKeyPrefix. Databases written
// before the stamp existed (the dual key<->DBID map layout) have no version
// key.
const layoutVersion = 1

// layoutVersionKey is where the layout version is stamped. The trailing byte
// keeps the raw key longer than a bare prefix, which reverse iteration
// requires of every key.
var layoutVersionKey = PrefixedKey(layoutVersionPrefix, []byte{0x00})

// checkLayoutVersion refuses to open a database whose on-disk layout does not
// match this package's, stamping a fresh database with the current version.
// Without the stamp check, a database written under an older layout opens
// cleanly and misreads - rows surface as phantom empty-value entries and
// lookups miss - which presents as missing data rather than as the layout
// mismatch it is. There is no migration; a database from an older layout must
// be recreated.
func checkLayoutVersion(bdb *badger.DB, path string) error {
	return bdb.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(layoutVersionKey)
		if err == nil {
			return item.Value(func(v []byte) error {
				if len(v) != 1 || v[0] != layoutVersion {
					return fmt.Errorf("database at %q has layout version %v where this build requires %d: delete the database directory to recreate it", path, v, layoutVersion)
				}
				return nil
			})
		}
		if !errors.Is(err, badger.ErrKeyNotFound) {
			return fmt.Errorf("error reading layout version: %w", err)
		}
		// No stamp. Only a fresh (empty) database may be adopted and stamped;
		// a non-empty one predates the stamp and cannot be read correctly.
		iterOpts := badger.DefaultIteratorOptions
		iterOpts.PrefetchValues = false
		it := txn.NewIterator(iterOpts)
		it.Rewind()
		empty := !it.Valid()
		it.Close()
		if !empty {
			return fmt.Errorf("database at %q predates layout version %d and cannot be read: delete the database directory to recreate it", path, layoutVersion)
		}
		return txn.Set(layoutVersionKey, []byte{layoutVersion})
	})
}

// New constructs a new Lexi DB at the directory named by cfg.Path.
func New(cfg *Config) (*DB, error) {
	logger := cfg.Log.SubLogger("BADG")
	opts := badger.DefaultOptions(cfg.Path).WithLogger(&badgerLoggerWrapper{logger})
	bdb, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}
	// Close the handle before bailing on any later failure, or the Badger
	// directory lock is held for the life of the process and every retry of
	// New on this path fails with a lock error.
	closeOnErr := func() {
		if cerr := bdb.Close(); cerr != nil {
			logger.Errorf("error closing badger after failed open: %v", cerr)
		}
	}
	if err := checkLayoutVersion(bdb, cfg.Path); err != nil {
		closeOnErr()
		return nil, err
	}
	idSeq, err := bdb.GetSequence(PrefixedKey(primarySequencePrefix, []byte{0x00}), 1000)
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("error getting constructing primary sequence: %w", err)
	}

	db := &DB{
		DB:    bdb,
		log:   cfg.Log,
		idSeq: idSeq,
	}
	db.drained = sync.NewCond(&db.closeMtx)
	return db, nil
}

// Connect starts the DB, and creates goroutines to perform shutdown when the
// context is canceled.
func (db *DB) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	db.wg.Add(1)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer func() {
			ticker.Stop()
			// Refuse new writes and wait for in-flight ones, so nothing can be
			// mid-commit when Badger closes.
			db.closeGate()
			if err := db.idSeq.Release(); err != nil {
				db.log.Errorf("Error releasing sequence: %v", err)
			}
			// Badger reports its final memtable flush and value-log sync from
			// Close, so an error here is unwritten data, not a cleanup nit.
			if err := db.Close(); err != nil {
				db.log.Errorf("Error closing database: %v", err)
			}
			db.wg.Done()
		}()
		for {
			select {
			case <-ticker.C:
				err := db.RunValueLogGC(0.5)
				if err != nil && !errors.Is(err, badger.ErrNoRewrite) {
					db.log.Errorf("garbage collection error: %v", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return &db.wg, nil
}

// Update: badger can return an ErrConflict if a read and write happen
// concurrently. This bugs the hell out of me, because I though that if a
// database was ACID-compliant, this was impossible, but I guess not. Either
// way, the solution is to try again.
func (db *DB) Update(f func(txn *badger.Txn) error) (err error) {
	if err := db.beginUpdate(); err != nil {
		return err
	}
	defer db.endUpdate()

	const (
		maxRetries = 10
		// The backoff is capped because callers wait on this synchronously,
		// several of them (SaveNotification, NextMultisigKeyIndex, LoadPokes)
		// while holding a shared mutex, so an uncapped doubling stalls every
		// write queued behind the contended one.
		maxSleep = 200 * time.Millisecond
	)
	sleepTime := 5 * time.Millisecond

	for i := 0; i < maxRetries; i++ {
		err = db.DB.Update(f)
		if err == nil || !errors.Is(err, badger.ErrConflict) {
			return err
		}
		if i == maxRetries-1 {
			// Out of attempts. Sleeping now would only delay the error.
			break
		}
		time.Sleep(sleepTime)
		if sleepTime *= 2; sleepTime > maxSleep {
			sleepTime = maxSleep
		}
	}

	return err
}

// prefixForName returns a unique prefix for the provided name and logs the
// relationship in the DB. Repeated calls to prefixForName with the same name
// will return the same prefix, including through restarts.
func (db *DB) prefixForName(name string) (prefix keyPrefix, _ error) {
	nameKey := PrefixedKey(nameToPrefixPrefix, []byte(name))
	return prefix, db.Update(func(txn *badger.Txn) error {
		it, err := txn.Get(nameKey)
		if err == nil {
			return it.Value(func(b []byte) error {
				prefix = bytesToPrefix(b)
				return nil
			})
		}
		if !errors.Is(err, badger.ErrKeyNotFound) {
			return fmt.Errorf("error getting name: %w", err)
		}
		lastPrefix := lastKeyForPrefix(txn, prefixToNamePrefix)
		if len(lastPrefix) == 0 {
			prefix = firstAvailablePrefix
		} else {
			prefix = incrementPrefix(bytesToPrefix(lastPrefix))
		}
		if err := txn.Set(PrefixedKey(nameToPrefixPrefix, []byte(name)), prefix[:]); err != nil {
			return fmt.Errorf("error setting prefix for table name: %w", err)
		}
		if err := txn.Set(PrefixedKey(prefixToNamePrefix, prefix[:]), []byte(name)); err != nil {
			return fmt.Errorf("error setting table name for prefix: %w", err)
		}
		return nil
	})
}

func (db *DB) nextID() (keyID KeyID, _ error) {
	i, err := db.idSeq.Next()
	if err != nil {
		return keyID, err
	}
	binary.BigEndian.PutUint64(keyID[:], i)
	return
}

// KeyID returns the row-unique KeyID stored in the datum for the key. This is
// the same ID embedded in the table's index entries, provided as a tool to
// keep caller-maintained index entries short. KeyID is read-only: IDs are
// assigned by Set, so a key with no row returns ErrKeyNotFound.
func (t *Table) KeyID(kB []byte) (keyID KeyID, err error) {
	err = t.View(func(txn *badger.Txn) error {
		d, err := t.get(txn, kB)
		if err != nil {
			return err
		}
		keyID = d.keyID
		return nil
	})
	return
}

// idMapKey is the key of a row's entry in the KeyID→key map. The entry is
// qualified by the table prefix: a KeyID identifies a row in one table, and
// scoping the map per table keeps every structure a row touches - the row
// itself, its index entries, and this mapping - resolvable without consulting
// any table-spanning state.
func idMapKey(tPrefix keyPrefix, keyID KeyID) []byte {
	k := make([]byte, PrefixSize+KeyIDSize)
	copy(k, tPrefix[:])
	copy(k[PrefixSize:], keyID[:])
	return PrefixedKey(IDToKeyPrefix, k)
}

// KV is any one of a number of common types whose binary encoding is
// straight-forward. It is an alias of kvstore.KV; see kvstore.ParseKV for the
// supported types.
type KV = kvstore.KV
