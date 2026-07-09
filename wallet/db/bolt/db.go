// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/encrypt"
	dexdb "github.com/bisoncraft/caribou/wallet/db"
	"go.etcd.io/bbolt"
	bolterr "go.etcd.io/bbolt/errors"
)

// getCopy returns a copy of the value for the given key and provided bucket. If
// the key is not found or if an empty slice was loaded, nil is returned. Thus,
// use bkt.Get(key) == nil directly to test for existence of the key. This
// function should be used instead of bbolt.(*Bucket).Get when the read value
// needs to be kept after the transaction, at which time the buffer from Get is
// no longer safe to use.
func getCopy(bkt *bbolt.Bucket, key []byte) []byte {
	b := bkt.Get(key)
	if len(b) == 0 {
		return nil
	}
	val := make([]byte, len(b))
	copy(val, b)
	return val
}

// Short names for some commonly used imported functions.
var (
	intCoder    = encode.IntCoder
	bEqual      = bytes.Equal
	uint16Bytes = encode.Uint16Bytes
	uint32Bytes = encode.Uint32Bytes
	uint64Bytes = encode.Uint64Bytes
)

// Bolt works on []byte keys and values. These are some commonly used key and
// value encodings.
var (
	// bucket keys
	appBucket             = []byte("appBucket")
	walletsBucket         = []byte("wallets")
	notesBucket           = []byte("notes")
	pokesBucket           = []byte("pokes")
	credentialsBucket     = []byte("credentials")
	multisigIndexesBucket = []byte("multiIndexes")
	multisigPubKeysBucket = []byte("multiPubKeys")
	// value keys
	balanceKey            = []byte("balance")
	walletKey             = []byte("wallet")
	noteKey               = []byte("note")
	pokesKey              = []byte("pokesKey")
	stampKey              = []byte("stamp")
	severityKey           = []byte("severity")
	ackKey                = []byte("ack")
	seedGenTimeKey        = []byte("seedGenTime")
	encSeedKey            = []byte("encSeed")
	encInnerKeyKey        = []byte("encInnerKey")
	innerKeyParamsKey     = []byte("innerKeyParams")
	outerKeyParamsKey     = []byte("outerKeyParams")
	birthdayKey           = []byte("birthday")
	credsVersionKey       = []byte("credsVersion")
	disabledRateSourceKey = []byte("disabledRateSources")
	walletDisabledKey     = []byte("walletDisabled")
	langKey               = []byte("lang")
	companionTokenKey     = []byte("companionToken")
	apiTokenKey           = []byte("apiToken")
	sendRequiresPwKey     = []byte("sendRequiresPassword")

	// values
	byteTrue = encode.ByteTrue

	backupDir = "backup"
)

// Opts is a set of options for the DB.
type Opts struct {
	BackupOnShutdown bool // default is true
}

var defaultOpts = Opts{
	BackupOnShutdown: true,
}

// BoltDB is a bbolt-based database backend for Boss Wallet. BoltDB satisfies
// the db.DB interface defined at github.com/bisoncraft/caribou/wallet/db.
type BoltDB struct {
	*bbolt.DB
	opts Opts
	log  util.Logger
}

// Check that BoltDB satisfies the db.DB interface.
var _ dexdb.DB = (*BoltDB)(nil)

// NewDB is a constructor for a *BoltDB.
func NewDB(dbPath string, logger util.Logger, opts ...Opts) (dexdb.DB, error) {
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		if errors.Is(err, bolterr.ErrTimeout) {
			err = fmt.Errorf("%w, could happen when database is already being used by another process", err)
		}
		return nil, err
	}

	// Release the file lock on exit.
	bdb := &BoltDB{
		DB:   db,
		opts: defaultOpts,
		log:  logger,
	}
	if len(opts) > 0 {
		bdb.opts = opts[0]
	}

	if err = bdb.makeTopLevelBuckets([][]byte{
		appBucket, walletsBucket, notesBucket, credentialsBucket,
		pokesBucket, multisigIndexesBucket, multisigPubKeysBucket,
	}); err != nil {
		return nil, err
	}

	bdb.log.Infof("Started database (file = %s)", dbPath)

	return bdb, nil
}

func (db *BoltDB) fileSize(path string) int64 {
	stat, err := os.Stat(path)
	if err != nil {
		db.log.Errorf("Failed to stat %v: %v", path, err)
		return 0
	}
	return stat.Size()
}

// Run waits for context cancellation and closes the database.
func (db *BoltDB) Run(ctx context.Context) {
	<-ctx.Done() // wait for shutdown to backup and compact

	// Create a backup in the backups folder.
	if db.opts.BackupOnShutdown {
		db.log.Infof("Backing up database...")
		if err := db.Backup(); err != nil {
			db.log.Errorf("Unable to backup database: %v", err)
		}
	}

	// Only compact the current DB file if there is excessive free space, in
	// terms of bytes AND relative to total DB size.
	const byteThresh = 1 << 18 // 256 KiB free
	const pctThresh = 0.05     // 5% free
	var dbSize int64
	_ = db.View(func(tx *bbolt.Tx) error {
		dbSize = tx.Size() // db size including free bytes
		return nil
	})
	dbStats := db.Stats()
	// FreeAlloc is (FreePageN + PendingPageN) * db.Info().PageSize
	// FreelistInuse seems to be page header overhead (much smaller). Add them.
	// https://github.com/etcd-io/bbolt/blob/020684ea1eb7b5574a8007c8d69605b1de8d9ec4/tx.go#L308-L309
	freeBytes := int64(dbStats.FreeAlloc + dbStats.FreelistInuse)
	pctFree := float64(freeBytes) / float64(dbSize)
	db.log.Debugf("Total DB size %d bytes, %d bytes unused (%.2f%%)",
		dbSize, freeBytes, 100*pctFree)
	// Only compact if free space is at least the byte threshold AND that fee
	// space accounts for a significant percent of the file.
	if freeBytes < byteThresh || pctFree < pctThresh {
		_ = db.Close()
		return
	}

	// TODO: If we never see trouble with the compacted DB files when bossw
	// starts up, we can just compact to the backups folder and copy that backup
	// file back on top of the main DB file after it is closed.  For now, the
	// backup above is a direct (not compacted) copy.

	// Compact the database by writing into a temporary file, closing the source
	// DB, and overwriting the original with the compacted temporary file.
	db.log.Infof("Compacting database to reclaim at least %d bytes...", freeBytes)
	srcPath := db.Path()                     // before db.Close
	compFile := srcPath + ".tmp"             // deterministic on *same fs*
	err := db.BackupTo(compFile, true, true) // overwrite and compact
	if err != nil {
		_ = db.Close()
		db.log.Errorf("Unable to compact database: %v", err)
		return
	}

	_ = db.Close() // close db file at srcPath

	initSize, compSize := db.fileSize(srcPath), db.fileSize(compFile)
	db.log.Infof("Compacted database file from %v => %v bytes (%.2f%% reduction)",
		initSize, compSize, 100*float64(initSize-compSize)/float64(initSize))

	err = os.Rename(compFile, srcPath) // compFile => srcPath
	if err != nil {
		db.log.Errorf("Unable to switch to compacted database: %v", err)
		return
	}
}

// Recrypt re-encrypts the wallet passwords. As a convenience, the provided
// *PrimaryCredentials are stored under the same transaction.
func (db *BoltDB) Recrypt(creds *dexdb.PrimaryCredentials, oldCrypter, newCrypter encrypt.Crypter) (walletUpdates map[uint32][]byte, err error) {
	if err := validateCreds(creds); err != nil {
		return nil, err
	}

	walletUpdates = make(map[uint32][]byte)

	return walletUpdates, db.Update(func(tx *bbolt.Tx) error {
		wallets := tx.Bucket(walletsBucket)
		if wallets == nil {
			return fmt.Errorf("no wallets bucket")
		}

		if err := wallets.ForEach(func(wid, _ []byte) error {
			wBkt := wallets.Bucket(wid)
			w, err := makeWallet(wBkt)
			if err != nil {
				return err
			}
			if len(w.EncryptedPW) == 0 {
				return nil
			}

			pw, err := oldCrypter.Decrypt(w.EncryptedPW)
			if err != nil {
				return fmt.Errorf("decrypt error: %w", err)
			}
			w.EncryptedPW, err = newCrypter.Encrypt(pw)
			if err != nil {
				return fmt.Errorf("encrypt error: %w", err)
			}
			err = wBkt.Put(walletKey, w.Encode())
			if err != nil {
				return err
			}
			walletUpdates[w.AssetID] = w.EncryptedPW
			return nil

		}); err != nil {
			return fmt.Errorf("wallets update error: %w", err)
		}

		// Store the new credentials.
		return db.setCreds(tx, creds)
	})
}

// SetPrimaryCredentials validates and stores the PrimaryCredentials.
func (db *BoltDB) SetPrimaryCredentials(creds *dexdb.PrimaryCredentials) error {
	if err := validateCreds(creds); err != nil {
		return err
	}

	return db.Update(func(tx *bbolt.Tx) error {
		return db.setCreds(tx, creds)
	})
}

// validateCreds checks that the PrimaryCredentials fields are properly
// populated.
func validateCreds(creds *dexdb.PrimaryCredentials) error {
	if len(creds.EncSeed) == 0 {
		return errors.New("EncSeed not set")
	}
	if len(creds.EncInnerKey) == 0 {
		return errors.New("EncInnerKey not set")
	}
	if len(creds.InnerKeyParams) == 0 {
		return errors.New("InnerKeyParams not set")
	}
	if len(creds.OuterKeyParams) == 0 {
		return errors.New("OuterKeyParams not set")
	}
	return nil
}

// primaryCreds reconstructs the *PrimaryCredentials.
func (db *BoltDB) primaryCreds() (creds *dexdb.PrimaryCredentials, err error) {
	return creds, db.View(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(credentialsBucket)
		if bkt == nil {
			return errors.New("no credentials bucket")
		}
		if bkt.Stats().KeyN == 0 {
			return dexdb.ErrNoCredentials
		}

		versionB := getCopy(bkt, credsVersionKey)
		if len(versionB) != 2 {
			versionB = []byte{0x00, 0x00}
		}

		bdayB := getCopy(bkt, birthdayKey)
		bday := time.Time{}
		if len(bdayB) > 0 {
			bday = time.Unix(int64(intCoder.Uint64(bdayB)), 0)
		}

		creds = &dexdb.PrimaryCredentials{
			EncSeed:        getCopy(bkt, encSeedKey),
			EncInnerKey:    getCopy(bkt, encInnerKeyKey),
			InnerKeyParams: getCopy(bkt, innerKeyParamsKey),
			OuterKeyParams: getCopy(bkt, outerKeyParamsKey),
			Birthday:       bday,
			Version:        intCoder.Uint16(versionB),
		}
		return nil
	})
}

// setCreds stores the *PrimaryCredentials.
func (db *BoltDB) setCreds(tx *bbolt.Tx, creds *dexdb.PrimaryCredentials) error {
	bkt := tx.Bucket(credentialsBucket)
	if bkt == nil {
		return errors.New("no credentials bucket")
	}
	return newBucketPutter(bkt).
		put(encSeedKey, creds.EncSeed).
		put(encInnerKeyKey, creds.EncInnerKey).
		put(innerKeyParamsKey, creds.InnerKeyParams).
		put(outerKeyParamsKey, creds.OuterKeyParams).
		put(birthdayKey, uint64Bytes(uint64(creds.Birthday.Unix()))).
		put(credsVersionKey, uint16Bytes(creds.Version)).
		err()
}

// PrimaryCredentials retrieves the *PrimaryCredentials, if they are stored. It
// is an error if none have been stored.
func (db *BoltDB) PrimaryCredentials() (creds *dexdb.PrimaryCredentials, err error) {
	return db.primaryCreds()
}

// SetSeedGenerationTime stores the time the app seed was generated.
func (db *BoltDB) SetSeedGenerationTime(time uint64) error {
	return db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(credentialsBucket)
		if bkt == nil {
			return errors.New("no credentials bucket")
		}

		return newBucketPutter(bkt).
			put(seedGenTimeKey, uint64Bytes(time)).
			err()
	})
}

// SeedGenerationTime returns the time the app seed was generated, if it was
// stored. It returns dexdb.ErrNoSeedGenTime if it was not stored.
func (db *BoltDB) SeedGenerationTime() (uint64, error) {
	var seedGenTime uint64
	return seedGenTime, db.View(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(credentialsBucket)
		if bkt == nil {
			return errors.New("no credentials bucket")
		}

		seedGenTimeBytes := bkt.Get(seedGenTimeKey)
		if seedGenTimeBytes == nil {
			return dexdb.ErrNoSeedGenTime
		}
		if len(seedGenTimeBytes) != 8 {
			return fmt.Errorf("seed generation time length %v, expected 8", len(seedGenTimeBytes))
		}

		seedGenTime = intCoder.Uint64(seedGenTimeBytes)
		return nil
	})
}

// NextMultisigKeyIndex returns the next multisig key index and increments the stored
// value so that subsequent calls will always return a higher index.
func (db *BoltDB) NextMultisigKeyIndex(assetID uint32) (uint32, error) {
	var multisigIndex uint32
	return multisigIndex, db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(multisigIndexesBucket)
		if bkt == nil {
			return errors.New("no multisig indexes bucket")
		}

		thisMultisigIdxKey := uint32Bytes(assetID)
		multisigIndexB := bkt.Get(thisMultisigIdxKey)
		if len(multisigIndexB) != 0 {
			multisigIndex = intCoder.Uint32(multisigIndexB)
		}
		return bkt.Put(thisMultisigIdxKey, uint32Bytes(multisigIndex+1))
	})
}

// StoreMultisigIndexForPubkey stores the index for pubkey and asset id.
func (db *BoltDB) StoreMultisigIndexForPubkey(assetID, idx uint32, pubkey [33]byte) error {
	return db.Update(func(tx *bbolt.Tx) error {
		outerBkt := tx.Bucket(multisigPubKeysBucket)
		if outerBkt == nil {
			return errors.New("no multisig indexes bucket")
		}

		bkt, err := outerBkt.CreateBucketIfNotExists(uint32Bytes(assetID))
		if err != nil {
			return err
		}
		return bkt.Put(pubkey[:], uint32Bytes(idx))
	})
}

// MultisigIndexForPubkey returns the index for pubkey and asset id if stored.
func (db *BoltDB) MultisigIndexForPubkey(assetID uint32, pubkey [33]byte) (uint32, error) {
	var multisigIndex uint32
	return multisigIndex, db.View(func(tx *bbolt.Tx) error {
		outerBkt := tx.Bucket(multisigPubKeysBucket)
		if outerBkt == nil {
			return errors.New("no multisig indexes bucket")
		}
		bkt := outerBkt.Bucket(uint32Bytes(assetID))
		if bkt == nil {
			return fmt.Errorf("no multisig bucket for asset id %d", assetID)
		}
		idxB := bkt.Get(pubkey[:])
		if len(idxB) == 0 {
			return errors.New("pubkey index not found")
		}
		multisigIndex = intCoder.Uint32(idxB)
		return nil
	})
}

// UpdateWallet adds a wallet to the database.
func (db *BoltDB) UpdateWallet(wallet *dexdb.Wallet) error {
	if wallet.Balance == nil {
		return fmt.Errorf("cannot UpdateWallet with nil Balance field")
	}
	return db.walletsUpdate(func(master *bbolt.Bucket) error {
		wBkt, err := master.CreateBucketIfNotExists(wallet.ID())
		if err != nil {
			return err
		}
		err = wBkt.Put(walletKey, wallet.Encode())
		if err != nil {
			return err
		}
		return wBkt.Put(balanceKey, wallet.Balance.Encode())
	})
}

// SetWalletPassword set the encrypted password field for the wallet.
func (db *BoltDB) SetWalletPassword(wid []byte, newEncPW []byte) error {
	return db.walletsUpdate(func(master *bbolt.Bucket) error {
		wBkt := master.Bucket(wid)
		if wBkt == nil {
			return fmt.Errorf("wallet with ID is %x not known", wid)
		}
		b := getCopy(wBkt, walletKey)
		if b == nil {
			return fmt.Errorf("no wallet found in bucket")
		}
		wallet, err := dexdb.DecodeWallet(b)
		if err != nil {
			return err
		}
		wallet.EncryptedPW = make([]byte, len(newEncPW))
		copy(wallet.EncryptedPW, newEncPW)
		// No need to populate wallet.Balance since it's not part of the
		// serialization stored in the walletKey sub-bucket.

		return wBkt.Put(walletKey, wallet.Encode())
	})
}

// UpdateBalance updates balance in the wallet bucket.
func (db *BoltDB) UpdateBalance(wid []byte, bal *dexdb.Balance) error {
	return db.walletsUpdate(func(master *bbolt.Bucket) error {
		wBkt := master.Bucket(wid)
		if wBkt == nil {
			return fmt.Errorf("wallet %x bucket is not a bucket", wid)
		}
		return wBkt.Put(balanceKey, bal.Encode())
	})
}

// UpdateWalletStatus updates a wallet's status.
func (db *BoltDB) UpdateWalletStatus(wid []byte, disable bool) error {
	return db.walletsUpdate(func(master *bbolt.Bucket) error {
		wBkt := master.Bucket(wid)
		if wBkt == nil {
			return fmt.Errorf("wallet %x bucket is not a bucket", wid)
		}
		if disable {
			return wBkt.Put(walletDisabledKey, encode.ByteTrue)
		}
		return wBkt.Put(walletDisabledKey, encode.ByteFalse)
	})
}

// Wallets loads all wallets from the database.
func (db *BoltDB) Wallets() ([]*dexdb.Wallet, error) {
	var wallets []*dexdb.Wallet
	return wallets, db.walletsView(func(master *bbolt.Bucket) error {
		c := master.Cursor()
		// key, _ := c.First()
		for wid, _ := c.First(); wid != nil; wid, _ = c.Next() {
			w, err := makeWallet(master.Bucket(wid))
			if err != nil {
				return err
			}
			wallets = append(wallets, w)
		}
		return nil
	})
}

// Wallet loads a single wallet from the database.
func (db *BoltDB) Wallet(wid []byte) (wallet *dexdb.Wallet, err error) {
	return wallet, db.walletsView(func(master *bbolt.Bucket) error {
		wallet, err = makeWallet(master.Bucket(wid))
		return err
	})
}

func makeWallet(wBkt *bbolt.Bucket) (*dexdb.Wallet, error) {
	if wBkt == nil {
		return nil, fmt.Errorf("wallets bucket value not a nested bucket")
	}
	b := getCopy(wBkt, walletKey)
	if b == nil {
		return nil, fmt.Errorf("no wallet found in bucket")
	}

	w, err := dexdb.DecodeWallet(b)
	if err != nil {
		return nil, fmt.Errorf("DecodeWallet error: %w", err)
	}

	balB := getCopy(wBkt, balanceKey)
	if balB != nil {
		bal, err := dexdb.DecodeBalance(balB)
		if err != nil {
			return nil, fmt.Errorf("DecodeBalance error: %w", err)
		}
		w.Balance = bal
	}

	statusB := wBkt.Get(walletDisabledKey)
	if statusB != nil {
		w.Disabled = bytes.Equal(statusB, encode.ByteTrue)
	}
	return w, nil
}

// walletsView is a convenience function for reading from the wallets bucket.
func (db *BoltDB) walletsView(f bucketFunc) error {
	return db.withBucket(walletsBucket, db.View, f)
}

// walletUpdate is a convenience function for updating the wallets bucket.
func (db *BoltDB) walletsUpdate(f bucketFunc) error {
	return db.withBucket(walletsBucket, db.Update, f)
}

// SaveNotification saves the notification.
func (db *BoltDB) SaveNotification(note *dexdb.Notification) error {
	if note.Severeness < dexdb.Success {
		return fmt.Errorf("storage of notification with severity %s is forbidden", note.Severeness)
	}
	return db.notesUpdate(func(master *bbolt.Bucket) error {
		noteB := note.Encode()
		k := note.ID()
		noteBkt, err := master.CreateBucketIfNotExists(k)
		if err != nil {
			return err
		}
		err = noteBkt.Put(stampKey, uint64Bytes(note.TimeStamp))
		if err != nil {
			return err
		}
		err = noteBkt.Put(severityKey, []byte{byte(note.Severeness)})
		if err != nil {
			return err
		}
		return noteBkt.Put(noteKey, noteB)
	})
}

// AckNotification sets the acknowledgement for a notification.
func (db *BoltDB) AckNotification(id []byte) error {
	return db.notesUpdate(func(master *bbolt.Bucket) error {
		noteBkt := master.Bucket(id)
		if noteBkt == nil {
			return fmt.Errorf("notification not found")
		}
		return noteBkt.Put(ackKey, byteTrue)
	})
}

// NotificationsN reads out the N most recent notifications.
func (db *BoltDB) NotificationsN(n int) ([]*dexdb.Notification, error) {
	notes := make([]*dexdb.Notification, 0, n)
	return notes, db.notesUpdate(func(master *bbolt.Bucket) error {
		trios := newestBuckets([]*bbolt.Bucket{master}, n, stampKey, nil)
		for _, trio := range trios {
			note, err := dexdb.DecodeNotification(getCopy(trio.b, noteKey))
			if err != nil {
				return err
			}
			note.Ack = bEqual(trio.b.Get(ackKey), byteTrue)
			note.Id = note.ID()
			if !bytes.Equal(note.Id, trio.k) {
				// This notification was initially stored when the serialization
				// and thus note ID did not include the TopicID. Ignore it, and
				// flag it acknowledged so we don't have to hear about it again.
				if !note.Ack {
					db.log.Tracef("Ignoring stored note with bad key: %x != %x \"%s\"",
						[]byte(note.Id), trio.k, note.String())
					_ = trio.b.Put(ackKey, byteTrue)
				}
				continue
			}
			notes = append(notes, note)
		}
		return nil
	})
}

// notesUpdate is a convenience function for updating the notifications bucket.
func (db *BoltDB) notesUpdate(f bucketFunc) error {
	return db.withBucket(notesBucket, db.Update, f)
}

// SavePokes saves a slice of notifications, overwriting any previously saved
// slice.
func (db *BoltDB) SavePokes(pokes []*dexdb.Notification) error {
	// Just save it as JSON.
	b, err := json.Marshal(pokes)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %w", err)
	}
	return db.withBucket(pokesBucket, db.Update, func(bkt *bbolt.Bucket) error {
		return bkt.Put(pokesKey, b)
	})
}

// LoadPokes loads the slice of notifications last saved with SavePokes. The
// loaded pokes are deleted from the database.
func (db *BoltDB) LoadPokes() (pokes []*dexdb.Notification, _ error) {
	return pokes, db.withBucket(pokesBucket, db.Update, func(bkt *bbolt.Bucket) error {
		b := bkt.Get(pokesKey)
		if len(b) == 0 { // None saved
			return nil
		}
		if err := json.Unmarshal(b, &pokes); err != nil {
			return err
		}
		return bkt.Delete(pokesKey)
	})
}

// newest buckets gets the nested buckets with the highest timestamp from the
// specified master buckets. The nested bucket should have an encoded uint64 at
// the timeKey. An optional filter function can be used to reject buckets.
func newestBuckets(buckets []*bbolt.Bucket, n int, timeKey []byte, filter func([]byte, *bbolt.Bucket) bool) []*keyTimeTrio {
	idx := newTimeIndexNewest(n)
	for _, master := range buckets {
		master.ForEach(func(k, _ []byte) error {
			bkt := master.Bucket(k)
			stamp := intCoder.Uint64(bkt.Get(timeKey))
			if filter == nil || filter(k, bkt) {
				idx.add(stamp, k, bkt)
			}
			return nil
		})
	}
	return idx.trios
}

// makeTopLevelBuckets creates a top-level bucket for each of the provided keys,
// if the bucket doesn't already exist.
func (db *BoltDB) makeTopLevelBuckets(buckets [][]byte) error {
	return db.Update(func(tx *bbolt.Tx) error {
		for _, bucket := range buckets {
			_, err := tx.CreateBucketIfNotExists(bucket)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// withBucket is a creates a view into a (probably nested) bucket. The viewer
// can be read-only (db.View), or read-write (db.Update). The provided
// bucketFunc will be called with the requested bucket as its only argument.
func (db *BoltDB) withBucket(bkt []byte, viewer txFunc, f bucketFunc) error {
	return viewer(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bkt)
		if bucket == nil {
			return fmt.Errorf("failed to open %s bucket", string(bkt))
		}
		return f(bucket)
	})
}

func ovrFlag(overwrite bool) int {
	if overwrite {
		return os.O_TRUNC
	}
	return os.O_EXCL
}

// compact writes a compacted copy of the DB into the specified destination
// file. This function should be called from BackupTo to validate the
// destination file and create any needed parent folders.
func (db *BoltDB) compact(dst string, overwrite bool) error {
	opts := bbolt.Options{
		OpenFile: func(path string, _ int, mode os.FileMode) (*os.File, error) {
			return os.OpenFile(path, os.O_RDWR|os.O_CREATE|ovrFlag(overwrite), mode)
		},
	}
	newDB, err := bbolt.Open(dst, 0600, &opts)
	if err != nil {
		return fmt.Errorf("unable to compact database: %w", err)
	}

	const txMaxSize = 1 << 19 // 512 KiB
	err = bbolt.Compact(newDB, db.DB, txMaxSize)
	if err != nil {
		_ = newDB.Close()
		return fmt.Errorf("unable to compact database: %w", err)
	}
	return newDB.Close()
}

// backup writes a direct copy of the DB into the specified destination file.
// This function should be called from BackupTo to validate the destination file
// and create any needed parent folders.
func (db *BoltDB) backup(dst string, overwrite bool) error {
	// Just copy. This is like tx.CopyFile but with the overwrite flag set
	// as specified.
	f, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|ovrFlag(overwrite), 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	err = db.View(func(tx *bbolt.Tx) error {
		_, err = tx.WriteTo(f)
		return err
	})
	if err != nil {
		return err
	}
	return f.Sync()
}

// BackupTo makes a copy of the database to the specified file, optionally
// overwriting and compacting the DB.
func (db *BoltDB) BackupTo(dst string, overwrite, compact bool) error {
	// If relative path, use current db path.
	var dir string
	if !filepath.IsAbs(dst) {
		dir = filepath.Dir(db.Path())
		dst = filepath.Join(dir, dst)
	}
	dst = filepath.Clean(dst)
	dir = filepath.Dir(dst)
	if dst == filepath.Clean(db.Path()) {
		return errors.New("destination is the active DB")
	}

	// Make the parent folder if it does not exists.
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		err = os.MkdirAll(dir, 0700)
		if err != nil {
			return fmt.Errorf("unable to create backup directory: %w", err)
		}
	}

	if compact {
		return db.compact(dst, overwrite)
	}

	return db.backup(dst, overwrite)
}

// Backup makes a copy of the database in the "backup" folder, overwriting any
// existing backup.
func (db *BoltDB) Backup() error {
	dir, file := filepath.Split(db.Path())
	return db.BackupTo(filepath.Join(dir, backupDir, file), true, false)
}

// bucketPutter enables chained calls to (*bbolt.Bucket).Put with error
// deferment.
type bucketPutter struct {
	bucket *bbolt.Bucket
	putErr error
}

// newBucketPutter is a constructor for a bucketPutter.
func newBucketPutter(bkt *bbolt.Bucket) *bucketPutter {
	return &bucketPutter{bucket: bkt}
}

// put calls Put on the underlying bucket. If an error has been encountered in a
// previous call to push, nothing is done. put enables the *bucketPutter to
// enable chaining.
func (bp *bucketPutter) put(k, v []byte) *bucketPutter {
	if bp.putErr != nil {
		return bp
	}
	bp.putErr = bp.bucket.Put(k, v)
	return bp
}

// Return any push error encountered.
func (bp *bucketPutter) err() error {
	return bp.putErr
}

// keyTimeTrio is used to build an on-the-fly time-sorted index.
type keyTimeTrio struct {
	k []byte
	t uint64
	b *bbolt.Bucket
}

// timeIndexNewest is a struct used to build an index of sorted keyTimeTrios.
// The index can have a maximum capacity. If the capacity is set to zero, the
// index size is unlimited.
type timeIndexNewest struct {
	trios []*keyTimeTrio
	cap   int
}

// Create a new *timeIndexNewest, with the specified capacity.
func newTimeIndexNewest(n int) *timeIndexNewest {
	return &timeIndexNewest{
		trios: make([]*keyTimeTrio, 0, n),
		cap:   n,
	}
}

// Conditionally add a time-key trio to the index. The trio will only be added
// if the timeIndexNewest is under capacity and the time t is larger than the
// oldest trio's time.
func (idx *timeIndexNewest) add(t uint64, k []byte, b *bbolt.Bucket) {
	count := len(idx.trios)
	if idx.cap == 0 || count < idx.cap {
		idx.trios = append(idx.trios, &keyTimeTrio{
			// Need to make a copy, and []byte(k) upsets the linter.
			k: append([]byte(nil), k...),
			t: t,
			b: b,
		})
	} else {
		// non-zero length, at capacity.
		if t <= idx.trios[count-1].t {
			// Too old. Discard.
			return
		}
		idx.trios[count-1] = &keyTimeTrio{
			k: append([]byte(nil), k...),
			t: t,
			b: b,
		}
	}
	sort.Slice(idx.trios, func(i, j int) bool {
		// newest (highest t) first, or by lexicographically by key if the times
		// are equal.
		t1, t2 := idx.trios[i].t, idx.trios[j].t
		return t1 > t2 || (t1 == t2 && bytes.Compare(idx.trios[i].k, idx.trios[j].k) == 1)
	})
}

// SaveDisabledRateSources updates disabled fiat rate sources.
func (db *BoltDB) SaveDisabledRateSources(disabledSources []string) error {
	return db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(appBucket)
		if bkt == nil {
			return fmt.Errorf("failed to open %s bucket", string(appBucket))
		}
		return bkt.Put(disabledRateSourceKey, []byte(strings.Join(disabledSources, ",")))
	})
}

// DisabledRateSources retrieves a map of disabled fiat rate sources.
func (db *BoltDB) DisabledRateSources() (disabledSources []string, err error) {
	return disabledSources, db.View(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(appBucket)
		if bkt == nil {
			return fmt.Errorf("no %s bucket", string(appBucket))
		}

		disabledString := string(bkt.Get(disabledRateSourceKey))
		if disabledString == "" {
			return nil
		}

		disabled := strings.Split(disabledString, ",")
		disabledSources = make([]string, 0, len(disabled))
		for _, token := range disabled {
			if token != "" {
				disabledSources = append(disabledSources, token)
			}
		}
		return nil
	})
}

// SetLanguage stores the language.
func (db *BoltDB) SetLanguage(lang string) error {
	return db.Update(func(dbTx *bbolt.Tx) error {
		bkt := dbTx.Bucket(appBucket)
		if bkt == nil {
			return fmt.Errorf("app bucket not found")
		}
		return bkt.Put(langKey, []byte(lang))
	})
}

// Language retrieves the language stored with SetLanguage. If no language
// has been stored, an empty string is returned without an error.
func (db *BoltDB) Language() (lang string, _ error) {
	return lang, db.View(func(dbTx *bbolt.Tx) error {
		bkt := dbTx.Bucket(appBucket)
		if bkt != nil {
			lang = string(bkt.Get(langKey))
		}
		return nil
	})
}

// SetCompanionToken stores the companion app auth token hash. An empty
// value deletes the key from the database.
func (db *BoltDB) SetCompanionToken(token string) error {
	return db.Update(func(dbTx *bbolt.Tx) error {
		bkt := dbTx.Bucket(appBucket)
		if bkt == nil {
			return fmt.Errorf("app bucket not found")
		}
		if token == "" {
			return bkt.Delete(companionTokenKey)
		}
		return bkt.Put(companionTokenKey, []byte(token))
	})
}

// CompanionToken retrieves the companion app auth token hash stored with
// SetCompanionToken. If no value has been stored, an empty string is
// returned without an error.
func (db *BoltDB) CompanionToken() (token string, _ error) {
	return token, db.View(func(dbTx *bbolt.Tx) error {
		bkt := dbTx.Bucket(appBucket)
		if bkt != nil {
			token = string(bkt.Get(companionTokenKey))
		}
		return nil
	})
}

// SetAPIToken stores the local API token hash. An empty value deletes the
// key from the database.
func (db *BoltDB) SetAPIToken(token string) error {
	return db.Update(func(dbTx *bbolt.Tx) error {
		bkt := dbTx.Bucket(appBucket)
		if bkt == nil {
			return fmt.Errorf("app bucket not found")
		}
		if token == "" {
			return bkt.Delete(apiTokenKey)
		}
		return bkt.Put(apiTokenKey, []byte(token))
	})
}

// APIToken retrieves the local API token hash stored with SetAPIToken. If no
// value has been stored, an empty string is returned without an error.
func (db *BoltDB) APIToken() (token string, _ error) {
	return token, db.View(func(dbTx *bbolt.Tx) error {
		bkt := dbTx.Bucket(appBucket)
		if bkt != nil {
			token = string(bkt.Get(apiTokenKey))
		}
		return nil
	})
}

// SetSendRequiresPassword stores whether the user must supply their app
// password to send funds. When false the key is deleted, so the default
// (no value stored) reads back as false.
func (db *BoltDB) SetSendRequiresPassword(require bool) error {
	return db.Update(func(dbTx *bbolt.Tx) error {
		bkt := dbTx.Bucket(appBucket)
		if bkt == nil {
			return fmt.Errorf("app bucket not found")
		}
		if !require {
			return bkt.Delete(sendRequiresPwKey)
		}
		return bkt.Put(sendRequiresPwKey, byteTrue)
	})
}

// SendRequiresPassword retrieves whether sending requires the app password.
// If no value has been stored, false is returned without an error.
func (db *BoltDB) SendRequiresPassword() (require bool, _ error) {
	return require, db.View(func(dbTx *bbolt.Tx) error {
		bkt := dbTx.Bucket(appBucket)
		if bkt != nil {
			require = bytes.Equal(bkt.Get(sendRequiresPwKey), byteTrue)
		}
		return nil
	})
}

// A couple of common bbolt functions.
type bucketFunc func(*bbolt.Bucket) error
type txFunc func(func(*bbolt.Tx) error) error
