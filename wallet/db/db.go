// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/bisoncraft/caribou/util/kvstore/engine"
)

// Table names. Neither engine supports nested namespaces, so the layout is
// flat, with composite keys where bolt used nested buckets.
const (
	appTable             = "app"
	walletsTable         = "wallets"
	notesTable           = "notes"
	multisigIndexesTable = "multisigIndexes"
	multisigPubkeysTable = "multisigPubkeys"

	// stampIndexName is the notes table's secondary index on the notification
	// timestamp, enabling NotificationsN to read newest-first without a scan.
	stampIndexName = "stamp"
)

// app table keys. Each holds a single value.
var (
	credsKey               = []byte("creds")
	langKey                = []byte("lang")
	disabledRateSourcesKey = []byte("disabledRateSources")
	companionTokenKey      = []byte("companionToken")
	apiTokenKey            = []byte("apiToken")
	sendRequiresPwKey      = []byte("sendRequiresPassword")
	termsVersionKey        = []byte("termsVersion")
	pokesKey               = []byte("pokes")
)

// wallets table field tags. A wallet's records live under composite keys
// assetID(4) || tag(1), one key per field group so that every writer is a
// blind Set (or SetBatch) of exactly the fields it owns: UpdateBalance writes
// only the balance key, SetWalletAddress only the address key,
// SetWalletPassword only the encrypted-password key, and UpdateWalletStatus
// only the disabled key. No wallet write reads before writing.
const (
	walletTag   byte = 0x01 // the configuration encoding: type + settings
	balanceTag  byte = 0x02
	disabledTag byte = 0x03
	addressTag  byte = 0x04
	encPWTag    byte = 0x05
)

// widLen is the length of the key prefix identifying a wallet: the big-endian
// uint32 asset ID.
const widLen = 4

// maxNotePrealloc bounds the head start NotificationsN reserves for a page of
// n notifications. n is a count the caller asks for, not a count the database
// holds, so reserving its full width lets a caller-supplied number size the
// allocation directly. n still bounds the page through the iteration, so this
// only caps how much is reserved before the first record is read.
const maxNotePrealloc = 256

// kvDB implements DB over a kvstore.Store: Badger (util/lexi) on desktop,
// bbolt (util/glossi) on mobile, selected at build time by util/kvstore/engine.
type kvDB struct {
	store kvstore.Store
	log   util.Logger

	// mtx serializes the read-modify-write operations, which have no
	// cross-engine transaction to hide behind: SaveNotification/
	// AckNotification (the ack flag lives in the note record),
	// NextMultisigKeyIndex, and LoadPokes' delete-after-read. A lost update
	// in NextMultisigKeyIndex would repeat a key index, i.e. address reuse.
	// All are cold, user-driven paths, so one coarse mutex suffices. Both
	// engines hold an exclusive file lock, so there is no cross-process case
	// to defend. Wallet writes need no serialization: every wallet field
	// group lives under its own key with a single blind writer.
	mtx sync.Mutex

	app             kvstore.StoreTable
	wallets         kvstore.StoreTable
	notes           kvstore.StoreTable
	noteStampIdx    kvstore.StoreIndex
	multisigIndexes kvstore.StoreTable
	multisigPubkeys kvstore.StoreTable

	intentsSwaps     kvstore.StoreTable
	intentsStampIdx  kvstore.StoreIndex
	intentsActiveIdx kvstore.StoreIndex
}

var _ DB = (*kvDB)(nil)

// New opens the database at path — a directory on desktop, a file on mobile —
// creating it if it does not exist.
func New(path string, log util.Logger) (DB, error) {
	store, err := engine.New(path, log)
	if err != nil {
		return nil, err
	}
	d, err := newDB(store, log)
	if err != nil {
		if cerr := store.Close(); cerr != nil {
			log.Errorf("error closing store after newDB failure: %v", cerr)
		}
		return nil, err
	}
	log.Infof("Started database (path = %s)", path)
	return d, nil
}

// newDB builds a kvDB over an already-constructed Store.
func newDB(store kvstore.Store, log util.Logger) (*kvDB, error) {
	d := &kvDB{store: store, log: log}
	var err error
	if d.app, err = store.Table(appTable); err != nil {
		return nil, err
	}
	if d.wallets, err = store.Table(walletsTable); err != nil {
		return nil, err
	}
	if d.notes, err = store.Table(notesTable); err != nil {
		return nil, err
	}
	if d.noteStampIdx, err = d.notes.AddIndex(stampIndexName, noteStampIndexFunc); err != nil {
		return nil, err
	}
	if d.multisigIndexes, err = store.Table(multisigIndexesTable); err != nil {
		return nil, err
	}
	if d.multisigPubkeys, err = store.Table(multisigPubkeysTable); err != nil {
		return nil, err
	}
	if d.intentsSwaps, err = store.Table(intentsSwapsTable); err != nil {
		return nil, err
	}
	if d.intentsStampIdx, err = d.intentsSwaps.AddIndex(intentsStampIndexName, intentsStampIndexFunc); err != nil {
		return nil, err
	}
	if d.intentsActiveIdx, err = d.intentsSwaps.AddIndex(intentsActiveIndexName, intentsActiveIndexFunc); err != nil {
		return nil, err
	}
	return d, nil
}

// Connect ties the DB's shutdown to ctx: when ctx is canceled, the underlying
// database is closed. The returned WaitGroup completes once shutdown is
// finished.
func (d *kvDB) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	return d.store.Connect(ctx)
}

// Close closes the underlying store. Only safe when Connect has not been
// called; otherwise cancel the context passed to Connect and wait the
// returned WaitGroup.
func (d *kvDB) Close() error {
	return d.store.Close()
}

// deleteIfExists removes k from the table, tolerating its absence.
func deleteIfExists(table kvstore.StoreTable, k []byte) error {
	if err := table.Delete(k); err != nil && !errors.Is(err, kvstore.ErrKeyNotFound) {
		return err
	}
	return nil
}

// validateCreds checks that the PrimaryCredentials fields are properly
// populated.
func validateCreds(creds *PrimaryCredentials) error {
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

// SetPrimaryCredentials validates and stores the PrimaryCredentials,
// replacing any previously stored set.
func (d *kvDB) SetPrimaryCredentials(creds *PrimaryCredentials) error {
	if err := validateCreds(creds); err != nil {
		return err
	}
	return d.app.Set(credsKey, creds.Encode(), kvstore.WithReplace())
}

// PrimaryCredentials retrieves the *PrimaryCredentials. It is
// ErrNoCredentials if none have been stored.
func (d *kvDB) PrimaryCredentials() (*PrimaryCredentials, error) {
	b, err := d.app.Get(credsKey)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return nil, ErrNoCredentials
		}
		return nil, err
	}
	return DecodeCredentials(b)
}

// fieldKey is the composite wallets-table key for one field of one wallet.
func fieldKey(assetID uint32, tag byte) []byte {
	k := make([]byte, widLen+1)
	copy(k, uint32Bytes(assetID))
	k[widLen] = tag
	return k
}

// walletExists is ErrWalletNotFound unless the wallet configuration key is
// present. The single-field setters guard on it so a stray write cannot
// create an orphaned field row, which would poison Wallets(): assemble
// requires the configuration record. Wallets are never deleted, so there is
// no check-then-write race to worry about.
func (d *kvDB) walletExists(assetID uint32) error {
	if _, err := d.wallets.Get(fieldKey(assetID, walletTag)); err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return fmt.Errorf("%w for asset ID %d", ErrWalletNotFound, assetID)
		}
		return err
	}
	return nil
}

// UpdateWallet adds a wallet to the database, or updates an existing wallet's
// configuration, balance, address and encrypted password in one batch. The
// disabled flag, if any, is untouched.
func (d *kvDB) UpdateWallet(wallet *Wallet) error {
	if wallet.Balance == nil {
		return fmt.Errorf("cannot UpdateWallet with nil Balance field")
	}
	assetID := wallet.AssetID
	return d.wallets.SetBatch([]kvstore.BatchEntry{
		{K: fieldKey(assetID, walletTag), V: wallet.Encode()},
		{K: fieldKey(assetID, balanceTag), V: wallet.Balance.Encode()},
		{K: fieldKey(assetID, addressTag), V: []byte(wallet.Address)},
		{K: fieldKey(assetID, encPWTag), V: wallet.EncryptedPW},
	}, kvstore.WithReplace())
}

// SetWalletPassword sets the encrypted password for the wallet. It is
// ErrWalletNotFound if the wallet is not known.
func (d *kvDB) SetWalletPassword(assetID uint32, newEncPW []byte) error {
	if err := d.walletExists(assetID); err != nil {
		return err
	}
	return d.wallets.Set(fieldKey(assetID, encPWTag), newEncPW, kvstore.WithReplace())
}

// SetWalletAddress sets the last known deposit address for the wallet. It is
// ErrWalletNotFound if the wallet is not known.
func (d *kvDB) SetWalletAddress(assetID uint32, addr string) error {
	if err := d.walletExists(assetID); err != nil {
		return err
	}
	return d.wallets.Set(fieldKey(assetID, addressTag), []byte(addr), kvstore.WithReplace())
}

// UpdateBalance updates a wallet's balance. It is ErrWalletNotFound if the
// wallet is not known.
func (d *kvDB) UpdateBalance(assetID uint32, bal *Balance) error {
	if err := d.walletExists(assetID); err != nil {
		return err
	}
	return d.wallets.Set(fieldKey(assetID, balanceTag), bal.Encode(), kvstore.WithReplace())
}

// UpdateWalletStatus updates a wallet's disabled status. It is
// ErrWalletNotFound if the wallet is not known.
func (d *kvDB) UpdateWalletStatus(assetID uint32, disable bool) error {
	if err := d.walletExists(assetID); err != nil {
		return err
	}
	v := encode.ByteFalse
	if disable {
		v = encode.ByteTrue
	}
	return d.wallets.Set(fieldKey(assetID, disabledTag), v, kvstore.WithReplace())
}

// walletRow collects the field records of a single wallet during iteration.
type walletRow struct {
	wallet   []byte
	balance  []byte
	disabled []byte
	address  []byte
	encPW    []byte
}

// assemble builds the *Wallet from the collected records. The configuration
// record is required; the other fields are optional.
func (row *walletRow) assemble(assetID uint32) (*Wallet, error) {
	if row.wallet == nil {
		return nil, fmt.Errorf("no wallet configuration stored for asset ID %d", assetID)
	}
	wallet, err := DecodeWallet(row.wallet)
	if err != nil {
		return nil, fmt.Errorf("DecodeWallet error: %w", err)
	}
	wallet.AssetID = assetID
	if row.balance != nil {
		wallet.Balance, err = DecodeBalance(row.balance)
		if err != nil {
			return nil, fmt.Errorf("DecodeBalance error: %w", err)
		}
	}
	wallet.Disabled = bytes.Equal(row.disabled, encode.ByteTrue)
	wallet.Address = string(row.address)
	wallet.EncryptedPW = row.encPW
	return wallet, nil
}

// Wallets lists all saved wallets. The wallets table is iterated in full and
// grouped by asset ID through a map, so the grouping does not depend on
// iteration order.
func (d *kvDB) Wallets() ([]*Wallet, error) {
	rows := make(map[uint32]*walletRow)
	err := d.wallets.Iterate(nil, func(it kvstore.StoreIter) error {
		k, err := it.K()
		if err != nil {
			return err
		}
		if len(k) != widLen+1 {
			return fmt.Errorf("unexpected wallets table key length %d", len(k))
		}
		assetID := intCoder.Uint32(k[:widLen])
		row := rows[assetID]
		if row == nil {
			row = &walletRow{}
			rows[assetID] = row
		}
		tag := k[len(k)-1]
		return it.V(func(v []byte) error {
			vCopy := make([]byte, len(v))
			copy(vCopy, v)
			switch tag {
			case walletTag:
				row.wallet = vCopy
			case balanceTag:
				row.balance = vCopy
			case disabledTag:
				row.disabled = vCopy
			case addressTag:
				row.address = vCopy
			case encPWTag:
				row.encPW = vCopy
			default:
				return fmt.Errorf("unknown wallets table field tag %d", tag)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	wallets := make([]*Wallet, 0, len(rows))
	for assetID, row := range rows {
		wallet, err := row.assemble(assetID)
		if err != nil {
			return nil, err
		}
		wallets = append(wallets, wallet)
	}
	return wallets, nil
}

// Wallet fetches the wallet for the specified asset. It is ErrWalletNotFound
// if the wallet is not known.
func (d *kvDB) Wallet(assetID uint32) (*Wallet, error) {
	row := &walletRow{}
	var err error
	if row.wallet, err = d.wallets.Get(fieldKey(assetID, walletTag)); err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return nil, fmt.Errorf("%w for asset ID %d", ErrWalletNotFound, assetID)
		}
		return nil, err
	}
	getOptional := func(tag byte) ([]byte, error) {
		b, err := d.wallets.Get(fieldKey(assetID, tag))
		if err != nil && !errors.Is(err, kvstore.ErrKeyNotFound) {
			return nil, err
		}
		return b, nil
	}
	if row.balance, err = getOptional(balanceTag); err != nil {
		return nil, err
	}
	if row.disabled, err = getOptional(disabledTag); err != nil {
		return nil, err
	}
	if row.address, err = getOptional(addressTag); err != nil {
		return nil, err
	}
	if row.encPW, err = getOptional(encPWTag); err != nil {
		return nil, err
	}
	return row.assemble(assetID)
}

// noteRecord is the value inserted into the notes table: the notification and
// its ack flag. The typed record is what gets passed to Set, so the stamp
// index recovers it with a type assertion and reads the timestamp directly
// instead of decoding the stored bytes.
type noteRecord struct {
	note *Notification
	ack  bool
}

// MarshalBinary satisfies encoding.BinaryMarshaler, encoding the notification
// encoding plus the ack flag in a versioned envelope.
func (r *noteRecord) MarshalBinary() ([]byte, error) {
	ackB := encode.ByteFalse
	if r.ack {
		ackB = encode.ByteTrue
	}
	return versionedBytes(0).AddData(r.note.Encode()).AddData(ackB), nil
}

// decodeNoteRecord splits a notes-table value into the raw notification
// encoding and the ack flag.
func decodeNoteRecord(b []byte) (noteB []byte, ack bool, _ error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, false, err
	}
	switch ver {
	case 0:
		if len(pushes) != 2 {
			return nil, false, fmt.Errorf("decodeNoteRecord: expected 2 pushes, got %d", len(pushes))
		}
		return pushes[0], bytes.Equal(pushes[1], encode.ByteTrue), nil
	}
	return nil, false, fmt.Errorf("unknown note record version %d", ver)
}

// noteStampIndexFunc derives the stamp index entry — the notification's
// 8-byte big-endian timestamp — from the *noteRecord under insertion.
func noteStampIndexFunc(_, vI kvstore.KV) ([]byte, error) {
	rec, ok := vI.(*noteRecord)
	if !ok {
		return nil, fmt.Errorf("expected *noteRecord value, got %T", vI)
	}
	return uint64Bytes(rec.note.TimeStamp), nil
}

// SaveNotification saves the notification. Re-saving a notification preserves
// any acknowledgement already recorded for it.
func (d *kvDB) SaveNotification(note *Notification) error {
	if note.Severeness < Success {
		return fmt.Errorf("storage of notification with severity %s is forbidden", note.Severeness)
	}
	d.mtx.Lock()
	defer d.mtx.Unlock()
	id := note.ID()
	var ack bool
	if b, err := d.notes.Get(id); err == nil {
		if _, ack, err = decodeNoteRecord(b); err != nil {
			return err
		}
	} else if !errors.Is(err, kvstore.ErrKeyNotFound) {
		return err
	}
	return d.notes.Set(id, &noteRecord{note: note, ack: ack}, kvstore.WithReplace())
}

// AckNotification sets the acknowledgement flag for the notification.
func (d *kvDB) AckNotification(id []byte) error {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	b, err := d.notes.Get(id)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return fmt.Errorf("notification not found")
		}
		return err
	}
	noteB, _, err := decodeNoteRecord(b)
	if err != nil {
		return err
	}
	note, err := DecodeNotification(noteB)
	if err != nil {
		return err
	}
	return d.notes.Set(id, &noteRecord{note: note, ack: true}, kvstore.WithReplace())
}

// NotificationsN reads out the N most recent notifications, newest first.
func (d *kvDB) NotificationsN(n int) ([]*Notification, error) {
	// The guard precedes the allocation: a negative n reaching make panics in
	// makeslice rather than returning the empty page the contract promises.
	if n <= 0 {
		return []*Notification{}, nil
	}
	prealloc := n
	if prealloc > maxNotePrealloc {
		prealloc = maxNotePrealloc
	}
	notes := make([]*Notification, 0, prealloc)
	err := d.noteStampIdx.Iterate(nil, func(it kvstore.StoreIter) error {
		err := it.V(func(v []byte) error {
			noteB, ack, err := decodeNoteRecord(v)
			if err != nil {
				return err
			}
			note, err := DecodeNotification(noteB)
			if err != nil {
				return err
			}
			note.Ack = ack
			note.Id = note.ID()
			notes = append(notes, note)
			return nil
		})
		if err != nil {
			return err
		}
		if len(notes) >= n {
			return kvstore.ErrEndIteration
		}
		return nil
	}, kvstore.WithReverse())
	if err != nil {
		return nil, err
	}
	return notes, nil
}

// SavePokes saves a slice of notifications, overwriting any previously saved
// slice.
func (d *kvDB) SavePokes(pokes []*Notification) error {
	b, err := json.Marshal(pokes)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %w", err)
	}
	return d.app.Set(pokesKey, b, kvstore.WithReplace())
}

// LoadPokes loads the slice of notifications last saved with SavePokes. The
// loaded pokes are deleted from the database.
func (d *kvDB) LoadPokes() ([]*Notification, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	b, err := d.app.Get(pokesKey)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var pokes []*Notification
	if err := json.Unmarshal(b, &pokes); err != nil {
		return nil, err
	}
	return pokes, deleteIfExists(d.app, pokesKey)
}

// SaveDisabledRateSources saves disabled fiat rate sources as a JSON array.
// An empty slice clears the list.
func (d *kvDB) SaveDisabledRateSources(disabledSources []string) error {
	if len(disabledSources) == 0 {
		return deleteIfExists(d.app, disabledRateSourcesKey)
	}
	b, err := json.Marshal(disabledSources)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %w", err)
	}
	return d.app.Set(disabledRateSourcesKey, b, kvstore.WithReplace())
}

// DisabledRateSources retrieves disabled fiat rate sources.
func (d *kvDB) DisabledRateSources() ([]string, error) {
	b, err := d.app.Get(disabledRateSourcesKey)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var disabledSources []string
	if err := json.Unmarshal(b, &disabledSources); err != nil {
		return nil, err
	}
	return disabledSources, nil
}

// SetLanguage stores the user's chosen language. An empty value deletes the
// key from the database.
func (d *kvDB) SetLanguage(lang string) error {
	if lang == "" {
		return deleteIfExists(d.app, langKey)
	}
	return d.app.Set(langKey, []byte(lang), kvstore.WithReplace())
}

// Language gets the language stored with SetLanguage. If no language has been
// stored, an empty string is returned without an error.
func (d *kvDB) Language() (string, error) {
	return d.getString(langKey)
}

// SetCompanionToken stores the companion app auth token hash. An empty value
// deletes the key from the database.
func (d *kvDB) SetCompanionToken(token string) error {
	if token == "" {
		return deleteIfExists(d.app, companionTokenKey)
	}
	return d.app.Set(companionTokenKey, []byte(token), kvstore.WithReplace())
}

// CompanionToken retrieves the companion app auth token hash stored with
// SetCompanionToken. If no value has been stored, an empty string is returned
// without an error.
func (d *kvDB) CompanionToken() (string, error) {
	return d.getString(companionTokenKey)
}

// SetAPIToken stores the local API token hash. An empty value deletes the key
// from the database.
func (d *kvDB) SetAPIToken(token string) error {
	if token == "" {
		return deleteIfExists(d.app, apiTokenKey)
	}
	return d.app.Set(apiTokenKey, []byte(token), kvstore.WithReplace())
}

// APIToken retrieves the local API token hash stored with SetAPIToken. If no
// value has been stored, an empty string is returned without an error.
func (d *kvDB) APIToken() (string, error) {
	return d.getString(apiTokenKey)
}

// getString reads an app-table value as a string, mapping a missing key to an
// empty string.
func (d *kvDB) getString(k []byte) (string, error) {
	b, err := d.app.Get(k)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

// SetSendRequiresPassword stores whether sending funds requires the app
// password. When false the key is deleted, so the default (no value stored)
// reads back as false.
func (d *kvDB) SetSendRequiresPassword(require bool) error {
	if !require {
		return deleteIfExists(d.app, sendRequiresPwKey)
	}
	return d.app.Set(sendRequiresPwKey, encode.ByteTrue, kvstore.WithReplace())
}

// SendRequiresPassword retrieves whether sending requires the app password.
// If no value has been stored, false is returned without an error.
func (d *kvDB) SendRequiresPassword() (bool, error) {
	b, err := d.app.Get(sendRequiresPwKey)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(b, encode.ByteTrue), nil
}

// SetAcceptedTermsVersion records the version of the Terms of Use and Privacy
// Policy the user has accepted. An empty value deletes the key from the
// database.
func (d *kvDB) SetAcceptedTermsVersion(version string) error {
	if version == "" {
		return deleteIfExists(d.app, termsVersionKey)
	}
	return d.app.Set(termsVersionKey, []byte(version), kvstore.WithReplace())
}

// AcceptedTermsVersion returns the version recorded with
// SetAcceptedTermsVersion. If nothing has been recorded, an empty string is
// returned without an error.
func (d *kvDB) AcceptedTermsVersion() (string, error) {
	return d.getString(termsVersionKey)
}

// NextMultisigKeyIndex returns the next multisig key index and increments the
// stored value so that subsequent calls will always return a higher index.
func (d *kvDB) NextMultisigKeyIndex(assetID uint32) (uint32, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	k := uint32Bytes(assetID)
	var idx uint32
	b, err := d.multisigIndexes.Get(k)
	if err == nil {
		if len(b) != 4 {
			return 0, fmt.Errorf("multisig index length %d, expected 4", len(b))
		}
		idx = intCoder.Uint32(b)
	} else if !errors.Is(err, kvstore.ErrKeyNotFound) {
		return 0, err
	}
	if err := d.multisigIndexes.Set(k, uint32Bytes(idx+1), kvstore.WithReplace()); err != nil {
		return 0, err
	}
	return idx, nil
}

// pubkeyKey is the composite multisigPubkeys key for a pubkey of an asset.
func pubkeyKey(assetID uint32, pubkey [33]byte) []byte {
	k := make([]byte, 4+33)
	copy(k, uint32Bytes(assetID))
	copy(k[4:], pubkey[:])
	return k
}

// StoreMultisigIndexForPubkey stores the key index for the compressed pubkey
// bytes of assetID.
func (d *kvDB) StoreMultisigIndexForPubkey(assetID, idx uint32, pubkey [33]byte) error {
	return d.multisigPubkeys.Set(pubkeyKey(assetID, pubkey), uint32Bytes(idx), kvstore.WithReplace())
}

// MultisigIndexForPubkey returns the key index for the compressed pubkey
// bytes of assetID if stored.
func (d *kvDB) MultisigIndexForPubkey(assetID uint32, pubkey [33]byte) (uint32, error) {
	b, err := d.multisigPubkeys.Get(pubkeyKey(assetID, pubkey))
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return 0, fmt.Errorf("no multisig index stored for asset %d pubkey %x", assetID, pubkey)
		}
		return 0, err
	}
	if len(b) != 4 {
		return 0, fmt.Errorf("multisig pubkey index length %d, expected 4", len(b))
	}
	return intCoder.Uint32(b), nil
}
