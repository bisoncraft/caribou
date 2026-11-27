// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"encoding/hex"
	"slices"
)

// OutputPool is the pool of a transaction output.
type OutputPool uint8

const (
	OutputTransparent OutputPool = iota + 1
	OutputSapling
	OutputOrchard
	OutputIronwood
)

func (p OutputPool) String() string {
	switch p {
	case OutputTransparent:
		return "transparent"
	case OutputSapling:
		return "Sapling"
	case OutputOrchard:
		return "Orchard"
	case OutputIronwood:
		return "Ironwood"
	default:
		return "unknown"
	}
}

// TxOutput is an output of a transaction in an account's history: one the
// account sent or received.
type TxOutput struct {
	Pool        OutputPool
	OutputIndex uint32
	// Value is in zatoshis.
	Value uint64
	// Address is the address the output pays, when the wallet knows it. For
	// an output of a transaction this wallet created, it is the address the
	// payment named. For an output to the wallet in a transaction this
	// wallet did not create (a payment received, or one another instance of
	// the same seed made, as a restored wallet sees it), it is the receiver
	// that was paid, as an address a sender could pay on its own: for an
	// Ironwood output the wallet's unified address reduced to its Orchard
	// receiver (as ParseAddress names it), for a transparent output the
	// bare transparent address. Such an address is not necessarily the
	// string the wallet handed out, and it never has a Sapling receiver. A
	// payment this wallet created to its own address keeps the address it
	// named, which can have a Sapling receiver when the user paid that form.
	// It is empty for change and for a received output whose address the
	// wallet did not generate.
	//
	// What a payment named is known only to the database that created it.
	// For an Ironwood output, the creating wallet records a payment to
	// itself under the address the payment named, and a restored wallet
	// records the same payment under the Orchard-only receiver: the same
	// receiver, a different string. A transparent output is the t-address
	// on both. A payment to someone else likewise: the creating wallet has
	// the address named, a restored one the receiver its viewing key
	// recovers.
	Address string
	// ToWallet is set for an output that pays one of the wallet's accounts.
	// IsChange is set for a shielded output to the account of a transaction
	// the account funded: its change, and also a payment to one of its own
	// addresses. Such a payment carries the address it was made to, and
	// change carries none. A transparent output is never marked, even in a
	// transaction the account funded. IsChange does not tell whether value
	// left the wallet: an output that is not ToWallet does.
	ToWallet, IsChange bool
	// Memo is the memo of a shielded output, nil for a transparent one. A
	// single 0xF6 byte is the empty memo.
	Memo []byte
}

// TxRecord is a transaction in an account's history.
type TxRecord struct {
	// Seq is the position of the transaction in the order the wallet
	// discovered transactions. It grows with discovery, not with height: a
	// restore can find an old transaction late.
	Seq uint64
	// TxID is the transaction ID in display byte order.
	TxID string
	// MinedHeight and BlockTime (Unix seconds) are those of the block that
	// mined the transaction, nil while it is unmined. BlockTime is also nil
	// for a mined transaction whose block the wallet has not scanned yet.
	MinedHeight *uint32
	BlockTime   *uint64
	// ExpiryHeight is the last height at which the transaction can be
	// mined, zero if it does not expire, and nil when the wallet does not
	// know it.
	ExpiryHeight *uint32
	// ExpiredUnmined is set for an unmined transaction whose expiry height
	// is at or below the wallet's greatest scanned height. It is reliable
	// only once the wallet is synced to the tip: before, a transaction
	// mined in a block not scanned yet can show as expired.
	ExpiredUnmined bool
	// CreatedByWallet is set for a transaction this wallet created.
	CreatedByWallet bool
	// RawPresent is set when the wallet holds the full transaction. Without
	// it, a transaction found by scanning shows only what compact blocks
	// tell: one that another instance of the same seed created looks like
	// a payment to itself until its full transaction is stored.
	RawPresent bool
	// BalanceDelta is the change of the account's balance, and TotalSpent
	// and TotalReceived the value of the account's inputs the transaction
	// spent and of its outputs that pay the account, in zatoshis.
	BalanceDelta              int64
	TotalSpent, TotalReceived uint64
	// Fee is the fee of the transaction in zatoshis, whoever paid it: on a
	// receive it is the sender's. It is nil while unknown, which it can
	// stay when the transaction spends transparent inputs whose values the
	// wallet does not know.
	Fee *uint64
	// IsShielding is set for a transaction that only moved the account's
	// transparent funds into a shielded pool.
	IsShielding bool
	// Outputs are the outputs the account sent or received.
	Outputs []TxOutput
}

// TxListing is a part of an account's history and the chain tip the wallet
// knew when it read it, against which confirmations are counted. ChainTip
// is zero before the wallet knows a tip.
type TxListing struct {
	ChainTip uint32
	Records  []TxRecord
}

// Transactions lists the account's transactions with a Seq above afterSeq,
// at most limit of them, in Seq order. A listing from zero, continued from
// the last Seq returned, reads the whole history. A listing from a
// watermark reads the transactions discovered since, whatever their height.
// A transaction can be linked to the account after its row was stored, and
// then appears with an old, lower Seq, below a watermark; a full listing
// finds it.
func (w *Wallet) Transactions(accountID string, afterSeq uint64, limit uint32) (*TxListing, error) {
	return w.be.transactions(accountID, afterSeq, limit)
}

// Transaction returns one transaction of the account's history by its ID in
// display byte order, nil if the account has none with that ID, and the
// chain tip the wallet knew when it read it.
func (w *Wallet) Transaction(accountID, txid string) (*TxRecord, uint32, error) {
	wire, err := hex.DecodeString(txid)
	if err != nil || len(wire) != 32 {
		return nil, 0, wrapf(ErrInvalidArgument, "a transaction ID is 64 hexadecimal digits: %q", txid)
	}
	slices.Reverse(wire)
	return w.be.transaction(accountID, wire)
}

// OpenTransactions lists the account's transactions whose record can still
// change: the unmined ones, those with at most depth confirmations, those
// mined in a block the wallet has not scanned yet, those whose full
// transaction the wallet lacks, and those funded by the account whose fee is
// unknown. A transaction with exactly depth confirmations is still listed,
// so a caller that counts confirmations against the listing's chain tip sees
// it reach depth before it leaves.
func (w *Wallet) OpenTransactions(accountID string, depth uint32) (*TxListing, error) {
	return w.be.openTransactions(accountID, depth)
}

// AddressKind is the kind of an address.
type AddressKind uint8

const (
	AddressTransparent AddressKind = iota + 1
	AddressUnified
	AddressSapling
	// AddressTex is a ZIP 320 transparent-source-only address.
	AddressTex
)

// ReceiverKind is the kind of a receiver of a unified address.
type ReceiverKind uint8

const (
	// ReceiverOrchard receives into the Ironwood pool from NU6.3 on.
	ReceiverOrchard ReceiverKind = iota + 1
	ReceiverSapling
	ReceiverTransparent
)

// Receiver is a receiver of a unified address.
type Receiver struct {
	Kind ReceiverKind
	// Address is an address a sender can pay this receiver at on its own,
	// as the wallet names it when paying that receiver alone: the unified
	// address reduced to the receiver and the address's expiry, or for a
	// transparent receiver of an address without an expiry, the bare
	// transparent address. It is empty for a Sapling receiver, which the
	// wallet does not pay.
	Address string
}

// ParsedAddress is what ParseAddress finds in an address.
type ParsedAddress struct {
	Kind AddressKind
	// Receivers are those of a unified address, Orchard first, then
	// Sapling, then transparent. Empty for any other kind.
	Receivers []Receiver
	// ExpiryHeight and ExpiryTime (Unix seconds) are those of a unified
	// address, nil when it has none. Zero is a value an address can carry.
	// An address is expired once the chain tip is above ExpiryHeight, or
	// the time is past ExpiryTime.
	ExpiryHeight *uint32
	ExpiryTime   *uint64
	// Payable reports whether the wallet can pay the address, and if so
	// PaidPool and PaidAddress are what a payment to it would land in and
	// name. Unpayable says why not. The expiry is not considered: a caller
	// compares ExpiryHeight with its chain tip and ExpiryTime with the
	// clock.
	Payable     bool
	PaidPool    PaidPool
	PaidAddress string
	Unpayable   string
}

// ParseAddress parses an address of the network: its kind, the receivers of
// a unified address, its expiry, and whether and how the wallet pays it. It
// takes no lock and makes no network request, so it suits validating input
// as it is typed. Whether the address is the wallet's own is the caller's
// business (OwnsAddress). An address that is not valid for the network fails
// with ErrInvalidArgument.
func ParseAddress(network Network, address string) (*ParsedAddress, error) {
	return parseAddress(network, address)
}
