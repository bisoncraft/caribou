// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"context"
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/dgraph-io/badger/v4"
)

// ErrKeyNotFound is an alias for badger.ErrKeyNotFound so that the caller
// doesn't have to import badger to use the semantics. Either error will satisfy
// errors.Is the same.
var ErrKeyNotFound = badger.ErrKeyNotFound

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

// New constructs a new Lexi DB at the directory named by cfg.Path.
func New(cfg *Config) (*DB, error) {
	logger := cfg.Log.SubLogger("BADG")
	opts := badger.DefaultOptions(cfg.Path).WithLogger(&badgerLoggerWrapper{logger})
	bdb, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}
	idSeq, err := bdb.GetSequence(PrefixedKey(primarySequencePrefix, []byte{0x00}), 1000)
	if err != nil {
		// Close the handle before bailing, or the Badger directory lock is held
		// for the life of the process and every retry of New on this path fails
		// with a lock error.
		if cerr := bdb.Close(); cerr != nil {
			logger.Errorf("error closing badger after sequence failure: %v", cerr)
		}
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

func (db *DB) nextID() (dbID DBID, _ error) {
	i, err := db.idSeq.Next()
	if err != nil {
		return dbID, err
	}
	binary.BigEndian.PutUint64(dbID[:], i)
	return
}

// KeyID returns the DBID for the key. This is the same DBID that will be used
// internally for the key when datum is inserted into a Table with Set. This
// method is provided as a tool to keep database index entries short.
func (db *DB) KeyID(kB []byte) (dbID DBID, err error) {
	err = db.View(func(txn *badger.Txn) error {
		dbID, err = db.keyID(txn, kB, true)
		return err
	})
	return
}

func (db *DB) keyID(txn *badger.Txn, kB []byte, readOnly bool) (dbID DBID, err error) {
	item, err := txn.Get(PrefixedKey(keyToIDPrefix, kB))
	if err == nil {
		err = item.Value(func(v []byte) error {
			copy(dbID[:], v)
			return nil
		})
		return
	}
	if !readOnly && errors.Is(err, ErrKeyNotFound) {
		if dbID, err = db.nextID(); err != nil {
			return
		}
		if err = txn.Set(PrefixedKey(keyToIDPrefix, kB), dbID[:]); err != nil {
			err = fmt.Errorf("error mapping key to ID: %w", err)
		} else if err = txn.Set(PrefixedKey(IdToKeyPrefix, dbID[:]), kB); err != nil {
			err = fmt.Errorf("error mapping ID to key: %w", err)
		}
	}
	return
}

// deleteDBID deletes the id-to-key mapping and the key-to-id mapping for the
// DBID.
func (db *DB) deleteDBID(txn *badger.Txn, dbID DBID) error {
	idK := PrefixedKey(IdToKeyPrefix, dbID[:])
	item, err := txn.Get(idK)
	if err != nil {
		return convertError(err)
	}
	if err := item.Value(func(kB []byte) error {
		if err := txn.Delete(PrefixedKey(keyToIDPrefix, kB)); err != nil {
			return fmt.Errorf("error deleting key to ID mapping: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := txn.Delete(idK); err != nil {
		return fmt.Errorf("error deleting ID to key mapping: %w", err)
	}
	return nil
}

// KV is any one of a number of common types whose binary encoding is
// straight-forward.
type KV any

func parseKV(i KV) (b []byte, err error) {
	switch it := i.(type) {
	case []byte:
		b = it
	case uint32:
		b = make([]byte, 4)
		binary.BigEndian.PutUint32(b[:], it)
	case time.Time:
		b = make([]byte, 8)
		binary.BigEndian.PutUint64(b[:], uint64(it.UnixMilli()))
	case encoding.BinaryMarshaler:
		b, err = it.MarshalBinary()
	case nil:
	default:
		err = fmt.Errorf("unknown IndexBucket type %T", it)
	}
	return
}
