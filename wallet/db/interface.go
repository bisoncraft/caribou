// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"context"
	"sync"
)

// DB is an interface that must be satisfied by Caribou persistent storage
// manager.
type DB interface {
	// Connect ties the DB's shutdown to ctx: when ctx is canceled, the
	// underlying database is closed. The returned WaitGroup completes once
	// shutdown is finished.
	Connect(ctx context.Context) (*sync.WaitGroup, error)
	// Close closes the database. Only safe to call when Connect has not been
	// called; for the managed lifecycle, cancel the context passed to Connect
	// and wait the returned WaitGroup.
	Close() error
	// SetPrimaryCredentials sets the initial *PrimaryCredentials.
	SetPrimaryCredentials(creds *PrimaryCredentials) error
	// PrimaryCredentials fetches the *PrimaryCredentials.
	PrimaryCredentials() (*PrimaryCredentials, error)
	// UpdateWallet adds a wallet to the database, or updates an existing
	// wallet's configuration, balance, address and encrypted password. The
	// disabled flag is only written by UpdateWalletStatus.
	UpdateWallet(wallet *Wallet) error
	// SetWalletPassword sets the encrypted password for the wallet. It is
	// ErrWalletNotFound if the wallet is not known.
	SetWalletPassword(assetID uint32, newEncPW []byte) error
	// SetWalletAddress sets the last known deposit address for the wallet.
	// It is ErrWalletNotFound if the wallet is not known.
	SetWalletAddress(assetID uint32, addr string) error
	// UpdateBalance updates a wallet's balance. It is ErrWalletNotFound if
	// the wallet is not known.
	UpdateBalance(assetID uint32, balance *Balance) error
	// Wallets lists all saved wallets.
	Wallets() ([]*Wallet, error)
	// Wallet fetches the wallet for the specified asset. It is
	// ErrWalletNotFound if the wallet is not known.
	Wallet(assetID uint32) (*Wallet, error)
	// UpdateWalletStatus updates a wallet's disabled status. It is
	// ErrWalletNotFound if the wallet is not known.
	UpdateWalletStatus(assetID uint32, disable bool) error
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
	// DisabledRateSources retrieves disabled fiat rate sources from the
	// database.
	DisabledRateSources() ([]string, error)
	// SaveDisabledRateSources saves disabled fiat rate sources in the
	// database.
	SaveDisabledRateSources(disabledSources []string) error
	// SetLanguage stores the user's chosen language.
	SetLanguage(lang string) error
	// Language gets the language stored with SetLanguage.
	Language() (string, error)
	// SetSendRequiresPassword stores whether sending funds requires the app
	// password.
	SetSendRequiresPassword(require bool) error
	// SendRequiresPassword gets whether sending requires the app password.
	// If no value has been stored, false is returned without an error.
	SendRequiresPassword() (bool, error)
	// SetAcceptedTermsVersion records the version of the Terms of Use and
	// Privacy Policy the user has accepted.
	SetAcceptedTermsVersion(version string) error
	// AcceptedTermsVersion gets the version stored with
	// SetAcceptedTermsVersion. If nothing has been stored, an empty string is
	// returned without an error.
	AcceptedTermsVersion() (string, error)
	// SetCompanionToken stores the companion app auth token hash.
	SetCompanionToken(token string) error
	// CompanionToken retrieves the companion app auth token hash stored
	// with SetCompanionToken. If no value has been stored, an empty
	// string is returned without an error.
	CompanionToken() (string, error)
	// SetAPIToken stores the local API token hash.
	SetAPIToken(token string) error
	// APIToken retrieves the local API token hash stored with SetAPIToken.
	// If no value has been stored, an empty string is returned without an
	// error.
	APIToken() (string, error)
	// NextMultisigKeyIndex returns the next multisig key index and increments the
	// stored value so that subsequent calls will always return a higher index.
	NextMultisigKeyIndex(assetID uint32) (uint32, error)
	// StoreMultisigIndexForPubkey stores the key index for the compressed
	// pubkey bytes of assetID.
	StoreMultisigIndexForPubkey(assetID, idx uint32, pubkey [33]byte) error
	// MultisigIndexForPubkey returns the key index for the compressed
	// pubkey bytes of assetID if stored.
	MultisigIndexForPubkey(assetID uint32, pubkey [33]byte) (uint32, error)
	// StoreIntentsSwap stores the NEAR Intents swap record under its
	// deposit address, stamping UpdatedAt and replacing any existing
	// record. CreatedAt orders the listing and must not change across
	// re-stores of the same swap. A record stored in a terminal state
	// leaves the active set.
	StoreIntentsSwap(swap *IntentsSwap) error
	// IntentsSwap fetches the swap record stored under the deposit
	// address. It is ErrIntentsSwapNotFound if no record exists.
	IntentsSwap(depositAddress string) (*IntentsSwap, error)
	// IntentsSwaps returns every stored swap record, newest first.
	IntentsSwaps() ([]*IntentsSwap, error)
	// ActiveIntentsSwaps returns the swap records in a non-terminal
	// state, oldest first.
	ActiveIntentsSwaps() ([]*IntentsSwap, error)
}
