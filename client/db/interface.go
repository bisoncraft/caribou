// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encrypt"
)

// DB is an interface that must be satisfied by Bison Wallet persistent storage
// manager.
type DB interface {
	dex.Runner
	// SetPrimaryCredentials sets the initial *PrimaryCredentials.
	SetPrimaryCredentials(creds *PrimaryCredentials) error
	// PrimaryCredentials fetches the *PrimaryCredentials.
	PrimaryCredentials() (*PrimaryCredentials, error)
	// Recrypt re-encrypts the wallet passwords and account private keys, and
	// stores the new *PrimaryCredentials.
	Recrypt(creds *PrimaryCredentials, oldCrypter, newCrypter encrypt.Crypter) (
		walletUpdates map[uint32][]byte, acctUpdates map[string][]byte, err error)
	// UpdateWallet adds a wallet to the database, or updates the wallet
	// credentials if the wallet already exists. A wallet is specified by the
	// pair (asset ID, account name).
	UpdateWallet(wallet *Wallet) error
	// SetWalletPassword sets the encrypted password for the wallet.
	SetWalletPassword(wid []byte, newPW []byte) error
	// UpdateBalance updates a wallet's balance.
	UpdateBalance(wid []byte, balance *Balance) error
	// Wallets lists all saved wallets.
	Wallets() ([]*Wallet, error)
	// Wallet fetches the wallet for the specified asset by wallet ID.
	Wallet(wid []byte) (*Wallet, error)
	// UpdateWalletStatus updates a wallet's status.
	UpdateWalletStatus(wid []byte, disable bool) error
	// Backup makes a copy of the database to the default "backups" folder.
	Backup() error
	// BackupTo makes a backup of the database at the specified location,
	// optionally overwriting any existing file and compacting the database.
	BackupTo(dst string, overwrite, compact bool) error
	// SaveNotification saves the notification.
	SaveNotification(*Notification) error
	// NotificationsN reads out the N most recent notifications.
	NotificationsN(int) ([]*Notification, error)
	// AckNotification sets the acknowledgement for a notification.
	AckNotification(id []byte) error
	// SavePokes saves a slice of notifications, overwriting any previously
	// saved slice.
	SavePokes([]*Notification) error
	// LoadPokes loads the slice of notifications last saved with SavePokes.
	// The loaded pokes are deleted from the database.
	LoadPokes() ([]*Notification, error)
	// SetSeedGenerationTime stores the time when the app seed was generated.
	SetSeedGenerationTime(time uint64) error
	// SeedGenerationTime fetches the time when the app seed was generated.
	SeedGenerationTime() (uint64, error)
	// DisabledRateSources retrieves disabled fiat rate sources from the
	// database.
	DisabledRateSources() ([]string, error)
	// SaveDisabledRateSources saves disabled fiat rate sources in the database.
	// A source name must not contain a comma.
	SaveDisabledRateSources(disabledSources []string) error
	// SetLanguage stores the user's chosen language.
	SetLanguage(lang string) error
	// Language gets the language stored with SetLanguage.
	Language() (string, error)
	// SetCompanionToken stores the companion app auth token hash.
	SetCompanionToken(token string) error
	// CompanionToken retrieves the companion app auth token hash stored
	// with SetCompanionToken. If no value has been stored, an empty
	// string is returned without an error.
	CompanionToken() (string, error)
	// NextMultisigKeyIndex returns the next multisig key index and increments the
	// stored value so that subsequent calls will always return a higher index.
	NextMultisigKeyIndex(assetID uint32) (uint32, error)
	// StoreMultisigIndexForPubkey stores the key index for the compressed
	// pubkey bytes of assetID.
	StoreMultisigIndexForPubkey(assetID, idx uint32, pubkey [33]byte) error
	// MultisigIndexForPubkey returns the key index for the compressed
	// pubkey bytes of assetID if stored.
	MultisigIndexForPubkey(assetID uint32, pubkey [33]byte) (uint32, error)
}
