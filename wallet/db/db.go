// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/kvstore"
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
	seedGenTimeKey         = []byte("seedGenTime")
	langKey                = []byte("lang")
	disabledRateSourcesKey = []byte("disabledRateSources")
	companionTokenKey      = []byte("companionToken")
	apiTokenKey            = []byte("apiToken")
	sendRequiresPwKey      = []byte("sendRequiresPassword")
	pokesKey               = []byte("pokes")
)

// wallets table field tags. A wallet's records live under composite keys
// assetID(4) || tag(1), preserving bolt's partial-update semantics:
// UpdateBalance writes only the balance key, UpdateWalletStatus only the
// disabled key, and neither disturbs the wallet encoding.
const (
	walletTag   byte = 0x01
	balanceTag  byte = 0x02
	disabledTag byte = 0x03
)

// widLen is the length of a wallet ID: Wallet.ID is the big-endian uint32
// asset ID.
const widLen = 4

// kvDB implements DB over a kvstore.Store: Badger (util/lexi) on desktop,
// bbolt (util/glossi) on mobile, selected at build time by util/kvstore.
type kvDB struct {
	store kvstore.Store
	log   util.Logger

	// mtx serializes the read-modify-write operations, which have no
	// cross-engine transaction to hide behind: SetWalletPassword,
	// SaveNotification/AckNotification (the ack flag lives in the note
	// record), NextMultisigKeyIndex, and LoadPokes' delete-after-read. A lost
	// update in NextMultisigKeyIndex would repeat a key index, i.e. address
	// reuse. All are cold, user-driven paths, so one coarse mutex suffices.
	// Both engines hold an exclusive file lock, so there is no cross-process
	// case to defend.
	mtx sync.Mutex

	app             kvstore.StoreTable
	wallets         kvstore.StoreTable
	notes           kvstore.StoreTable
	stampIdx        kvstore.StoreIndex
	multisigIndexes kvstore.StoreTable
	multisigPubkeys kvstore.StoreTable
}

var _ DB = (*kvDB)(nil)

// New opens the database at path — a directory on desktop, a file on mobile —
// creating it if it does not exist.
func New(path string, log util.Logger) (DB, error) {
	store, err := kvstore.New(path, log)
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
	if d.stampIdx, err = d.notes.AddIndex(stampIndexName, noteStampIndexFunc); err != nil {
		return nil, err
	}
	if d.multisigIndexes, err = store.Table(multisigIndexesTable); err != nil {
		return nil, err
	}
	if d.multisigPubkeys, err = store.Table(multisigPubkeysTable); err != nil {
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
	return d.app.Set(credsKey, creds.Encode(), true)
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
func fieldKey(wid []byte, tag byte) []byte {
	k := make([]byte, len(wid)+1)
	copy(k, wid)
	k[len(wid)] = tag
	return k
}

// walletExists errors unless the wallet encoding key is present.
func (d *kvDB) walletExists(wid []byte) error {
	if _, err := d.wallets.Get(fieldKey(wid, walletTag)); err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return fmt.Errorf("wallet with ID %x is not known", wid)
		}
		return err
	}
	return nil
}

// UpdateWallet adds a wallet to the database, or updates an existing wallet's
// encoding and balance. The disabled flag, if any, is untouched.
func (d *kvDB) UpdateWallet(wallet *Wallet) error {
	if wallet.Balance == nil {
		return fmt.Errorf("cannot UpdateWallet with nil Balance field")
	}
	wid := wallet.ID()
	return d.wallets.SetBatch([]kvstore.BatchEntry{
		{K: fieldKey(wid, walletTag), V: wallet.Encode()},
		{K: fieldKey(wid, balanceTag), V: wallet.Balance.Encode()},
	}, true)
}

// SetWalletPassword sets the encrypted password field for the wallet.
func (d *kvDB) SetWalletPassword(wid []byte, newEncPW []byte) error {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	b, err := d.wallets.Get(fieldKey(wid, walletTag))
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return fmt.Errorf("wallet with ID %x is not known", wid)
		}
		return err
	}
	wallet, err := DecodeWallet(b)
	if err != nil {
		return err
	}
	wallet.EncryptedPW = make([]byte, len(newEncPW))
	copy(wallet.EncryptedPW, newEncPW)
	return d.wallets.Set(fieldKey(wid, walletTag), wallet.Encode(), true)
}

// UpdateBalance updates a wallet's balance.
func (d *kvDB) UpdateBalance(wid []byte, bal *Balance) error {
	if err := d.walletExists(wid); err != nil {
		return err
	}
	return d.wallets.Set(fieldKey(wid, balanceTag), bal.Encode(), true)
}

// UpdateWalletStatus updates a wallet's disabled status.
func (d *kvDB) UpdateWalletStatus(wid []byte, disable bool) error {
	if err := d.walletExists(wid); err != nil {
		return err
	}
	v := encode.ByteFalse
	if disable {
		v = encode.ByteTrue
	}
	return d.wallets.Set(fieldKey(wid, disabledTag), v, true)
}

// walletRow collects the field records of a single wallet during iteration.
type walletRow struct {
	wallet   []byte
	balance  []byte
	disabled []byte
}

// assemble builds the *Wallet from the collected records. The wallet encoding
// is required; balance and disabled flag are optional.
func (row *walletRow) assemble(wid []byte) (*Wallet, error) {
	if row.wallet == nil {
		return nil, fmt.Errorf("no wallet encoding stored for wallet %x", wid)
	}
	wallet, err := DecodeWallet(row.wallet)
	if err != nil {
		return nil, fmt.Errorf("DecodeWallet error: %w", err)
	}
	if row.balance != nil {
		wallet.Balance, err = DecodeBalance(row.balance)
		if err != nil {
			return nil, fmt.Errorf("DecodeBalance error: %w", err)
		}
	}
	wallet.Disabled = bytes.Equal(row.disabled, encode.ByteTrue)
	return wallet, nil
}

// Wallets lists all saved wallets. The wallets table is iterated in full and
// grouped by wallet ID: kvstore table iteration is key-ordered only on the
// bbolt engine (Badger iterates insertion order and does not filter by user
// key prefix), so grouping cannot assume adjacency.
func (d *kvDB) Wallets() ([]*Wallet, error) {
	rows := make(map[[widLen]byte]*walletRow)
	err := d.wallets.Iterate(nil, func(it kvstore.StoreIter) error {
		k, err := it.K()
		if err != nil {
			return err
		}
		if len(k) != widLen+1 {
			return fmt.Errorf("unexpected wallets table key length %d", len(k))
		}
		var wid [widLen]byte
		copy(wid[:], k)
		row := rows[wid]
		if row == nil {
			row = &walletRow{}
			rows[wid] = row
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
			default:
				return fmt.Errorf("unknown wallets table field tag %d", tag)
			}
			return nil
		})
	}, false, nil)
	if err != nil {
		return nil, err
	}
	wallets := make([]*Wallet, 0, len(rows))
	for wid, row := range rows {
		wallet, err := row.assemble(wid[:])
		if err != nil {
			return nil, err
		}
		wallets = append(wallets, wallet)
	}
	return wallets, nil
}

// Wallet fetches the wallet for the specified asset by wallet ID.
func (d *kvDB) Wallet(wid []byte) (*Wallet, error) {
	row := &walletRow{}
	var err error
	if row.wallet, err = d.wallets.Get(fieldKey(wid, walletTag)); err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return nil, fmt.Errorf("wallet with ID %x is not known", wid)
		}
		return nil, err
	}
	if row.balance, err = d.wallets.Get(fieldKey(wid, balanceTag)); err != nil &&
		!errors.Is(err, kvstore.ErrKeyNotFound) {

		return nil, err
	}
	if row.disabled, err = d.wallets.Get(fieldKey(wid, disabledTag)); err != nil &&
		!errors.Is(err, kvstore.ErrKeyNotFound) {

		return nil, err
	}
	return row.assemble(wid)
}

// encodeNoteRecord encodes a notes-table value: the notification encoding
// plus its ack flag, in a versioned envelope. noteB must be the encoding from
// (*Notification).Encode.
func encodeNoteRecord(noteB []byte, ack bool) []byte {
	ackB := encode.ByteFalse
	if ack {
		ackB = encode.ByteTrue
	}
	return versionedBytes(0).AddData(noteB).AddData(ackB)
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
// 8-byte big-endian timestamp — from a notes-table value.
func noteStampIndexFunc(_, v []byte) ([]byte, error) {
	noteB, _, err := decodeNoteRecord(v)
	if err != nil {
		return nil, err
	}
	note, err := DecodeNotification(noteB)
	if err != nil {
		return nil, err
	}
	return uint64Bytes(note.TimeStamp), nil
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
	return d.notes.Set(id, encodeNoteRecord(note.Encode(), ack), true)
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
	return d.notes.Set(id, encodeNoteRecord(noteB, true), true)
}

// NotificationsN reads out the N most recent notifications, newest first.
func (d *kvDB) NotificationsN(n int) ([]*Notification, error) {
	notes := make([]*Notification, 0, n)
	if n <= 0 {
		return notes, nil
	}
	err := d.stampIdx.Iterate(nil, func(it kvstore.StoreIter) error {
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
	}, true, nil)
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
	return d.app.Set(pokesKey, b, true)
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

// SetSeedGenerationTime stores the time the app seed was generated.
func (d *kvDB) SetSeedGenerationTime(t uint64) error {
	return d.app.Set(seedGenTimeKey, uint64Bytes(t), true)
}

// SeedGenerationTime returns the time the app seed was generated, if it was
// stored. It returns ErrNoSeedGenTime if it was not stored.
func (d *kvDB) SeedGenerationTime() (uint64, error) {
	b, err := d.app.Get(seedGenTimeKey)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return 0, ErrNoSeedGenTime
		}
		return 0, err
	}
	if len(b) != 8 {
		return 0, fmt.Errorf("seed generation time length %d, expected 8", len(b))
	}
	return intCoder.Uint64(b), nil
}

// SaveDisabledRateSources saves disabled fiat rate sources. A source name
// must not contain a comma. An empty slice clears the list.
func (d *kvDB) SaveDisabledRateSources(disabledSources []string) error {
	joined := strings.Join(disabledSources, ",")
	if joined == "" {
		return deleteIfExists(d.app, disabledRateSourcesKey)
	}
	return d.app.Set(disabledRateSourcesKey, []byte(joined), true)
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
	disabled := strings.Split(string(b), ",")
	disabledSources := make([]string, 0, len(disabled))
	for _, token := range disabled {
		if token != "" {
			disabledSources = append(disabledSources, token)
		}
	}
	return disabledSources, nil
}

// SetLanguage stores the user's chosen language. An empty value deletes the
// key from the database.
func (d *kvDB) SetLanguage(lang string) error {
	if lang == "" {
		return deleteIfExists(d.app, langKey)
	}
	return d.app.Set(langKey, []byte(lang), true)
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
	return d.app.Set(companionTokenKey, []byte(token), true)
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
	return d.app.Set(apiTokenKey, []byte(token), true)
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
	return d.app.Set(sendRequiresPwKey, encode.ByteTrue, true)
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
	if err := d.multisigIndexes.Set(k, uint32Bytes(idx+1), true); err != nil {
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
	return d.multisigPubkeys.Set(pubkeyKey(assetID, pubkey), uint32Bytes(idx), true)
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
