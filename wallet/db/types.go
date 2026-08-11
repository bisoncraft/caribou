// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package db

import (
	"fmt"
	"strings"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/config"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"golang.org/x/crypto/blake2s"
)

// Severity indicates the level of required action for a notification. The DEX
// db only stores notifications with Severity >= Success.
type Severity uint8

const (
	Ignorable Severity = iota
	// Data notifications are not meant for display to the user. These
	// notifications are used only for communication of information necessary for
	// UI updates or other high-level state changes.
	Data
	// Poke notifications are not persistent across sessions. These should be
	// displayed if the user has a live notification feed. They are not stored in
	// the database.
	Poke
	// Success and higher are stored and can be recalled using DB.NotificationsN.
	Success
	WarningLevel
	ErrorLevel
)

const (
	ErrNoCredentials  = util.ErrorKind("no credentials have been stored")
	ErrWalletNotFound = util.ErrorKind("wallet not found")
)

// String satisfies fmt.Stringer for Severity.
func (s Severity) String() string {
	switch s {
	case Ignorable:
		return "ignore"
	case Data:
		return "data"
	case Poke:
		return "poke"
	case WarningLevel:
		return "warning"
	case ErrorLevel:
		return "error"
	case Success:
		return "success"
	}
	return "unknown severity"
}

// PrimaryCredentials should be created during app initialization. Both the seed
// and the inner key (and technically the other two fields) should be generated
// with a cryptographically-secure prng.
type PrimaryCredentials struct {
	// EncSeed is the root seed used to create a hierarchical deterministic
	// key chain (see also dcrd/hdkeychain.NewMaster/ExtendedKey).
	EncSeed []byte
	// EncInnerKey is an encrypted encryption key. The inner key will never
	// change. The inner key is encrypted with the outer key, which itself is
	// based on the user's password.
	EncInnerKey []byte
	// InnerKeyParams are the key parameters for the inner key.
	InnerKeyParams []byte
	// OuterKeyParams are the key parameters for the outer key.
	OuterKeyParams []byte
	// Birthday is the time stamp associated with seed creation.
	Birthday time.Time
}

// Encode encodes the PrimaryCredentials to a versioned blob. The Birthday is
// stored at second precision.
func (c *PrimaryCredentials) Encode() []byte {
	return versionedBytes(0).
		AddData(c.EncSeed).
		AddData(c.EncInnerKey).
		AddData(c.InnerKeyParams).
		AddData(c.OuterKeyParams).
		AddData(uint64Bytes(uint64(c.Birthday.Unix())))
}

// DecodeCredentials decodes the versioned blob to *PrimaryCredentials.
func DecodeCredentials(b []byte) (*PrimaryCredentials, error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, err
	}
	switch ver {
	case 0:
		return decodeCredentials_v0(pushes)
	}
	return nil, fmt.Errorf("unknown PrimaryCredentials version %d", ver)
}

func decodeCredentials_v0(pushes [][]byte) (*PrimaryCredentials, error) {
	if len(pushes) != 5 {
		return nil, fmt.Errorf("decodeCredentials_v0: expected 5 pushes, got %d", len(pushes))
	}
	if len(pushes[4]) != 8 {
		return nil, fmt.Errorf("decodeCredentials_v0: birthday push is supposed to be length 8. got %d", len(pushes[4]))
	}
	return &PrimaryCredentials{
		EncSeed:        pushes[0],
		EncInnerKey:    pushes[1],
		InnerKeyParams: pushes[2],
		OuterKeyParams: pushes[3],
		Birthday:       time.Unix(int64(intCoder.Uint64(pushes[4])), 0),
	}, nil
}

// encodeAssetBalance serializes an asset.Balance.
func encodeAssetBalance(bal *asset.Balance) []byte {
	return versionedBytes(0).
		AddData(uint64Bytes(bal.Available)).
		AddData(uint64Bytes(bal.Immature)).
		AddData(uint64Bytes(bal.Locked))
}

// decodeAssetBalance deserializes an asset.Balance.
func decodeAssetBalance(b []byte) (*asset.Balance, error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, err
	}
	switch ver {
	case 0:
		return decodeAssetBalance_v0(pushes)
	}
	return nil, fmt.Errorf("unknown Balance version %d", ver)
}

func decodeAssetBalance_v0(pushes [][]byte) (*asset.Balance, error) {
	if len(pushes) != 3 {
		return nil, fmt.Errorf("decodeAssetBalance_v0: expected 3 push, got %d", len(pushes))
	}
	for i, push := range pushes {
		if len(push) != 8 {
			return nil, fmt.Errorf("decodeAssetBalance_v0: push %d is supposed to be length 8. got %d", i, len(push))
		}
	}
	return &asset.Balance{
		Available: intCoder.Uint64(pushes[0]),
		Immature:  intCoder.Uint64(pushes[1]),
		Locked:    intCoder.Uint64(pushes[2]),
	}, nil
}

// Balance represents a wallet's balance in various contexts.
type Balance struct {
	asset.Balance
	Stamp time.Time `json:"stamp"`
}

// Encode encodes the Balance to a versioned blob.
func (b *Balance) Encode() []byte {
	return versionedBytes(0).
		AddData(encodeAssetBalance(&b.Balance)).
		AddData(uint64Bytes(uint64(b.Stamp.UnixMilli())))
}

// DecodeBalance decodes the versioned blob to a *Balance.
func DecodeBalance(b []byte) (*Balance, error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, err
	}
	switch ver {
	case 0:
		return decodeBalance_v0(pushes)
	}
	return nil, fmt.Errorf("unknown Balance version %d", ver)
}

func decodeBalance_v0(pushes [][]byte) (*Balance, error) {
	if len(pushes) != 2 {
		return nil, fmt.Errorf("decodeBalance_v0: expected 2 pushes, got %d", len(pushes))
	}
	if len(pushes[1]) != 8 {
		return nil, fmt.Errorf("decodeBalance_v0: stamp push is supposed to be length 8. got %d", len(pushes[1]))
	}
	bal, err := decodeAssetBalance(pushes[0])
	if err != nil {
		return nil, fmt.Errorf("decodeBalance_v0: error decoding zero conf balance: %w", err)
	}

	return &Balance{
		Balance: *bal,
		Stamp:   time.UnixMilli(int64(intCoder.Uint64(pushes[1]))),
	}, nil
}

// Wallet is information necessary to create an asset.Wallet.
type Wallet struct {
	AssetID     uint32
	Type        string
	Settings    map[string]string
	Balance     *Balance
	EncryptedPW []byte
	Address     string
	Disabled    bool
}

// Encode encodes the Wallet's configuration — its type and settings — to a
// versioned blob. The other fields are stored under their own keys: they have
// different writers and write cadences, so bundling them here would force
// read-modify-write cycles on the single-field setters.
func (w *Wallet) Encode() []byte {
	return versionedBytes(0).
		AddData(config.Data(w.Settings)).
		AddData([]byte(w.Type))
}

// DecodeWallet decodes the versioned blob to a *Wallet. Only the fields in the
// configuration encoding — Type and Settings — are set; the caller supplies
// AssetID (the record key) and retrieves the separately-stored fields.
func DecodeWallet(b []byte) (*Wallet, error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, err
	}
	switch ver {
	case 0:
		return decodeWallet_v0(pushes)
	}
	return nil, fmt.Errorf("unknown DecodeWallet version %d", ver)
}

func decodeWallet_v0(pushes [][]byte) (*Wallet, error) {
	if len(pushes) != 2 {
		return nil, fmt.Errorf("decodeWallet_v0: expected 2 pushes, got %d", len(pushes))
	}
	settingsB, typeB := pushes[0], pushes[1]
	settings, err := config.Parse(settingsB)
	if err != nil {
		return nil, fmt.Errorf("unable to decode wallet settings")
	}
	return &Wallet{
		Type:     string(typeB),
		Settings: settings,
	}, nil
}

func versionedBytes(v byte) encode.BuildyBytes {
	return encode.BuildyBytes{v}
}

var uint64Bytes = encode.Uint64Bytes
var uint32Bytes = encode.Uint32Bytes
var intCoder = encode.IntCoder

// Topic is a language-independent unique ID for a Notification.
type Topic string

// Notification is information for the user that is typically meant for display,
// and is persisted for recall across sessions.
type Notification struct {
	NoteType    string     `json:"type"`
	TopicID     Topic      `json:"topic"`
	SubjectText string     `json:"subject"`
	DetailText  string     `json:"details"`
	Severeness  Severity   `json:"severity"`
	TimeStamp   uint64     `json:"stamp"`
	Ack         bool       `json:"acked"`
	Id          util.Bytes `json:"id"`
}

// NewNotification is a constructor for a Notification.
func NewNotification(noteType string, topic Topic, subject, details string, severity Severity) Notification {
	note := Notification{
		NoteType:    noteType,
		TopicID:     topic,
		SubjectText: subject,
		DetailText:  details,
		Severeness:  severity,
	}
	note.Stamp()
	return note
}

// ID is a unique ID based on a hash of the notification data.
func (n *Notification) ID() util.Bytes {
	return noteKey(n.Encode())
}

// Type is the notification type.
func (n *Notification) Type() string {
	return n.NoteType
}

// Topic is a language-independent unique ID for the Notification.
func (n *Notification) Topic() Topic {
	return n.TopicID
}

// Subject is a short description of the notification contents.
func (n *Notification) Subject() string {
	return n.SubjectText
}

// Details should contain more detailed information.
func (n *Notification) Details() string {
	return n.DetailText
}

// Severity is the notification severity.
func (n *Notification) Severity() Severity {
	return n.Severeness
}

// Time is the notification timestamp. The timestamp is set in NewNotification.
func (n *Notification) Time() uint64 {
	return n.TimeStamp
}

// Acked is true if the user has seen the notification. Acknowledgement is
// recorded with DB.AckNotification.
func (n *Notification) Acked() bool {
	return n.Ack
}

// Stamp sets the notification timestamp. If NewNotification is used to
// construct the Notification, the timestamp will already be set.
func (n *Notification) Stamp() {
	n.TimeStamp = uint64(time.Now().UnixMilli())
	n.Id = n.ID()
}

// DBNote is a function to return the *Notification itself. It  should really be
// defined on the concrete types in core, but is ubiquitous so defined here for
// convenience.
func (n *Notification) DBNote() *Notification {
	return n
}

// String generates a compact human-readable representation of the Notification
// that is suitable for logging. For example:
//
//	|SUCCESS| (fee payment) Fee paid - Waiting for 2 confirmations before trading at https://superdex.tld:7232
//	|DATA| (boring event) Subject without details
func (n *Notification) String() string {
	// In case type and/or detail or empty strings, adjust the formatting to
	// avoid extra whitespace.
	var format strings.Builder
	format.WriteString("|%s| (%s)") // always nil error
	if len(n.DetailText) > 0 || len(n.SubjectText) > 0 {
		format.WriteString(" ")
	}
	format.WriteString("%s")
	if len(n.DetailText) > 0 && len(n.SubjectText) > 0 {
		format.WriteString(" - ")
	}
	format.WriteString("%s")

	severity := strings.ToUpper(n.Severity().String())
	return fmt.Sprintf(format.String(), severity, n.NoteType, n.SubjectText, n.DetailText)
}

// DecodeNotification decodes the versioned blob to a *Notification.
func DecodeNotification(b []byte) (*Notification, error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, err
	}
	switch ver {
	case 0:
		return decodeNotification_v0(pushes)
	}
	return nil, fmt.Errorf("unknown DecodeNotification version %d", ver)
}

func decodeNotification_v0(pushes [][]byte) (*Notification, error) {
	if len(pushes) != 6 {
		return nil, fmt.Errorf("decodeNotification_v0: expected 6 pushes, got %d", len(pushes))
	}
	if len(pushes[3]) != 1 {
		return nil, fmt.Errorf("decodeNotification_v0: severity push is supposed to be length 1. got %d", len(pushes[3]))
	}
	if len(pushes[4]) != 8 {
		return nil, fmt.Errorf("decodeNotification_v0: timestamp push is supposed to be length 8. got %d", len(pushes[4]))
	}

	return &Notification{
		NoteType:    string(pushes[0]),
		TopicID:     Topic(string(pushes[5])),
		SubjectText: string(pushes[1]),
		DetailText:  string(pushes[2]),
		Severeness:  Severity(pushes[3][0]),
		TimeStamp:   intCoder.Uint64(pushes[4]),
	}, nil
}

// Encode encodes the Notification to a versioned blob.
func (n *Notification) Encode() []byte {
	return versionedBytes(0).
		AddData([]byte(n.NoteType)).
		AddData([]byte(n.SubjectText)).
		AddData([]byte(n.DetailText)).
		AddData([]byte{byte(n.Severeness)}).
		AddData(uint64Bytes(n.TimeStamp)).
		AddData([]byte(n.TopicID))
}

// noteKeySize must be <= 32.
const noteKeySize = 8

// noteKey creates a unique key from the hash of the supplied bytes.
func noteKey(b []byte) []byte {
	h := blake2s.Sum256(b)
	return h[:noteKeySize]
}
