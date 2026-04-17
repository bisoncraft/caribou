// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"fmt"
	"sync/atomic"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/db"
	"decred.org/dcrdex/dex"
)

// Notifications should use the following note type strings.
const (
	NoteTypeSend       = "send"
	NoteTypeBalance    = "balance"
	NoteTypeWalletConfig = "walletconfig"
	NoteTypeWalletState  = "walletstate"
	NoteTypeWalletSync   = "walletsync"
	NoteTypeSecurity     = "security"
	NoteTypeFiatRates    = "fiatrateupdate"
	NoteTypeLogin        = "login"
	NoteTypeWalletNote   = "walletnote"
	NoteTypeBridge       = "bridge"
)

var noteChanCounter uint64

func (c *Core) logNote(n Notification) {
	if n.Subject() == "" && n.Details() == "" {
		return
	}

	logFun := c.log.Warnf // default in case the Severity level is unknown to notify
	switch n.Severity() {
	case db.Data:
		logFun = c.log.Tracef
	case db.Poke:
		logFun = c.log.Debugf
	case db.Success:
		logFun = c.log.Infof
	case db.WarningLevel:
		logFun = c.log.Warnf
	case db.ErrorLevel:
		logFun = c.log.Errorf
	}

	logFun("notify: %v", n)
}

func (c *Core) Broadcast(n Notification) {
	c.notify(n)
}

// notify sends a notification to all subscribers. If the notification is of
// sufficient severity, it is stored in the database.
func (c *Core) notify(n Notification) {
	if n.Severity() >= db.Success {
		c.db.SaveNotification(n.DBNote())
	}

	c.logNote(n)

	c.noteMtx.RLock()
	for _, ch := range c.noteChans {
		select {
		case ch <- n:
		default:
			c.log.Errorf("blocking notification channel")
		}
	}
	c.noteMtx.RUnlock()
}

// NoteFeed contains a receiving channel for notifications.
type NoteFeed struct {
	C      <-chan Notification
	closer func()
}

// ReturnFeed should be called when the channel is no longer needed.
func (c *NoteFeed) ReturnFeed() {
	if c.closer != nil {
		c.closer()
	}
}

// NotificationFeed returns a new receiving channel for notifications. The
// channel has capacity 1024, and should be monitored for the lifetime of the
// Core. Blocking channels are silently ignored.
func (c *Core) NotificationFeed() *NoteFeed {
	id, ch := c.notificationFeed()
	return &NoteFeed{
		C:      ch,
		closer: func() { c.returnFeed(id) },
	}
}

func (c *Core) notificationFeed() (uint64, <-chan Notification) {
	ch := make(chan Notification, 1024)
	cid := atomic.AddUint64(&noteChanCounter, 1)
	c.noteMtx.Lock()
	c.noteChans[cid] = ch
	c.noteMtx.Unlock()
	return cid, ch
}

func (c *Core) returnFeed(channelID uint64) {
	c.noteMtx.Lock()
	delete(c.noteChans, channelID)
	c.noteMtx.Unlock()
}

// AckNotes sets the acknowledgement field for the notifications.
func (c *Core) AckNotes(ids []dex.Bytes) {
	for _, id := range ids {
		err := c.db.AckNotification(id)
		if err != nil {
			c.log.Errorf("error saving notification acknowledgement for %s: %v", id, err)
		}
	}
}

func (c *Core) formatDetails(topic Topic, args ...any) (translatedSubject, details string) {
	locale := c.locale()
	trans, found := locale.m[topic]
	if !found {
		c.log.Errorf("No translation found for topic %q", topic)
		originTrans, found := originLocale[topic]
		if !found {
			return string(topic), "translation error"
		}
		return originTrans.subject.T, fmt.Sprintf(originTrans.template.T, args...)
	}
	return trans.subject.T, locale.printer.Sprintf(string(topic), args...)
}

// Notification is an interface for a user notification. Notification is
// satisfied by db.Notification, so concrete types can embed the db type.
type Notification interface {
	// Type is a string ID unique to the concrete type.
	Type() string
	// Topic is a string ID unique to the message subject. Since subjects must
	// be translated, we cannot rely on the subject to programmatically identify
	// the message.
	Topic() Topic
	// Subject is a short description of the notification contents. When displayed
	// to the user, the Subject will typically be given visual prominence. For
	// notifications with Severity < Poke (not meant for display), the Subject
	// field may be repurposed as a second-level category ID.
	Subject() string
	// Details should contain more detailed information.
	Details() string
	// Severity is the notification severity.
	Severity() db.Severity
	// Time is the notification timestamp. The timestamp is set in
	// db.NewNotification. Time is a UNIX timestamp, in milliseconds.
	Time() uint64
	// Acked is true if the user has seen the notification. Acknowledgement is
	// recorded with (*Core).AckNotes.
	Acked() bool
	// ID should be unique, except in the case of identical copies of
	// db.Notification where the IDs should be the same.
	ID() dex.Bytes
	// Stamp sets the notification timestamp. If db.NewNotification is used to
	// construct the db.Notification, the timestamp will already be set.
	Stamp()
	// DBNote returns the underlying *db.Notification.
	DBNote() *db.Notification
	// String generates a compact human-readable representation of the
	// Notification that is suitable for logging.
	String() string
}

// Topic is a language-independent unique ID for a Notification.
type Topic = db.Topic

// SecurityNote is a note regarding application security, credentials, or
// authentication.
type SecurityNote struct {
	db.Notification
}

const (
	TopicSeedNeedsSaving Topic = "SeedNeedsSaving"
	TopicUpgradedToSeed  Topic = "UpgradedToSeed"
)

func newSecurityNote(topic Topic, subject, details string, severity db.Severity) *SecurityNote {
	return &SecurityNote{
		Notification: db.NewNotification(NoteTypeSecurity, topic, subject, details, severity),
	}
}

const (
	TopicWalletConnectionWarning Topic = "WalletConnectionWarning"
	TopicWalletUnlockError       Topic = "WalletUnlockError"
	TopicWalletCommsWarning      Topic = "WalletCommsWarning"
	TopicWalletPeersRestored     Topic = "WalletPeersRestored"
)

// SendNote is a notification regarding a requested send or withdraw.
type SendNote struct {
	db.Notification
}

const (
	TopicSendError   Topic = "SendError"
	TopicSendSuccess Topic = "SendSuccess"
)

func newSendNote(topic Topic, subject, details string, severity db.Severity) *SendNote {
	return &SendNote{
		Notification: db.NewNotification(NoteTypeSend, topic, subject, details, severity),
	}
}

// FiatRatesNote is an update of fiat rate data for assets.
type FiatRatesNote struct {
	db.Notification
	FiatRates map[uint32]float64 `json:"fiatRates"`
}

const TopicFiatRatesUpdate Topic = "fiatrateupdate"

func newFiatRatesUpdate(rates map[uint32]float64) *FiatRatesNote {
	return &FiatRatesNote{
		Notification: db.NewNotification(NoteTypeFiatRates, TopicFiatRatesUpdate, "", "", db.Data),
		FiatRates:    rates,
	}
}

// BalanceNote is an update to a wallet's balance.
type BalanceNote struct {
	db.Notification
	AssetID uint32         `json:"assetID"`
	Balance *WalletBalance `json:"balance"`
}

const TopicBalanceUpdated Topic = "BalanceUpdated"

func newBalanceNote(assetID uint32, bal *WalletBalance) *BalanceNote {
	return &BalanceNote{
		Notification: db.NewNotification(NoteTypeBalance, TopicBalanceUpdated, "", "", db.Data),
		AssetID:      assetID,
		Balance:      bal, // Once created, balance is never modified by Core.
	}
}

// WalletConfigNote is a notification regarding a change in wallet
// configuration.
type WalletConfigNote struct {
	db.Notification
	Wallet *WalletState `json:"wallet"`
}

const (
	TopicWalletConfigurationUpdated Topic = "WalletConfigurationUpdated"
	TopicWalletPasswordUpdated      Topic = "WalletPasswordUpdated"
	TopicWalletPeersWarning         Topic = "WalletPeersWarning"
	TopicWalletTypeDeprecated       Topic = "WalletTypeDeprecated"
	TopicWalletPeersUpdate          Topic = "WalletPeersUpdate"
)

func newWalletConfigNote(topic Topic, subject, details string, severity db.Severity, walletState *WalletState) *WalletConfigNote {
	return &WalletConfigNote{
		Notification: db.NewNotification(NoteTypeWalletConfig, topic, subject, details, severity),
		Wallet:       walletState,
	}
}

// WalletStateNote is a notification regarding a change in wallet state,
// including: creation, locking, unlocking, connect, disabling and enabling. This
// is intended to be a Data Severity notification.
type WalletStateNote WalletConfigNote

const TopicWalletState Topic = "WalletState"
const TopicTokenApproval Topic = "TokenApproval"
const TopicBridgeApproval Topic = "BridgeApproval"

func newTokenApprovalNote(walletState *WalletState) *WalletStateNote {
	return &WalletStateNote{
		Notification: db.NewNotification(NoteTypeWalletState, TopicTokenApproval, "", "", db.Data),
		Wallet:       walletState,
	}
}

func newBridgeApprovalNote(walletState *WalletState) *WalletStateNote {
	return &WalletStateNote{
		Notification: db.NewNotification(NoteTypeWalletState, TopicBridgeApproval, "", "", db.Data),
		Wallet:       walletState,
	}
}

// BridgeNote is a notification for bridge status updates.
// It is sent whenever the destination chain submits a transaction,
// and when the bridge is fully complete.
type BridgeNote struct {
	db.Notification
	SourceAssetID   uint32   `json:"sourceAssetID"`
	DestAssetID     uint32   `json:"destAssetID"`
	TxID            string   `json:"txID"`
	CompletionTxIDs []string `json:"completionTxIDs"`
	Amount          uint64   `json:"amount"`
	Complete        bool     `json:"complete"`
}

const TopicBridgeUpdate Topic = "BridgeUpdate"

func newBridgeNote(n *asset.BridgeCompletedNote) *BridgeNote {
	bn := &BridgeNote{
		Notification:    db.NewNotification(NoteTypeBridge, TopicBridgeUpdate, "", "", db.Data),
		SourceAssetID:   n.SourceAssetID,
		DestAssetID:     n.AssetID,
		TxID:            n.InitiationTxID,
		CompletionTxIDs: n.CompletionTxIDs,
		Amount:          n.AmtReceived,
		Complete:        n.Complete,
	}
	return bn
}

func newWalletStateNote(walletState *WalletState) *WalletStateNote {
	return &WalletStateNote{
		Notification: db.NewNotification(NoteTypeWalletState, TopicWalletState, "", "", db.Data),
		Wallet:       walletState,
	}
}

// WalletSyncNote is a notification of the wallet sync status.
type WalletSyncNote struct {
	db.Notification
	AssetID      uint32            `json:"assetID"`
	SyncStatus   *asset.SyncStatus `json:"syncStatus"`
	SyncProgress float32           `json:"syncProgress"`
}

const TopicWalletSync = "WalletSync"

func newWalletSyncNote(assetID uint32, ss *asset.SyncStatus) *WalletSyncNote {
	return &WalletSyncNote{
		Notification: db.NewNotification(NoteTypeWalletSync, TopicWalletState, "", "", db.Data),
		AssetID:      assetID,
		SyncStatus:   ss,
		SyncProgress: ss.BlockProgress(),
	}
}

// LoginNote is a notification with the recent login status.
type LoginNote struct {
	db.Notification
}

const TopicLoginStatus Topic = "LoginStatus"

func newLoginNote(message string) *LoginNote {
	return &LoginNote{
		Notification: db.NewNotification(NoteTypeLogin, TopicLoginStatus, "", message, db.Data),
	}
}

// WalletNote is a notification originating from a wallet.
type WalletNote struct {
	db.Notification
	Payload asset.WalletNotification `json:"payload"`
}

const TopicWalletNotification Topic = "WalletNotification"

func newWalletNote(n asset.WalletNotification) *WalletNote {
	return &WalletNote{
		Notification: db.NewNotification(NoteTypeWalletNote, TopicWalletNotification, "", "", db.Data),
		Payload:      n,
	}
}

