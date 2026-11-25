// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"errors"
	"fmt"
)

// Errors that callers are expected to test for with errors.Is.
var (
	// ErrNotBuilt is returned by Open when the package was compiled without
	// the "zec" build tag or without cgo, and so without the Rust backend.
	ErrNotBuilt = errors.New("zeclib: built without the zec build tag")
	// ErrClosed is returned by every method of a Wallet after Close.
	ErrClosed = errors.New("zeclib: the wallet is closed")
	// ErrConnected is returned by Connect when the wallet is already
	// syncing.
	ErrConnected = errors.New("zeclib: the wallet is already connected")
	// ErrInvalidArgument reports an argument that is malformed or out of
	// range.
	ErrInvalidArgument = errors.New("zeclib: invalid argument")
	// ErrDatabase reports a failure of the wallet database.
	ErrDatabase = errors.New("zeclib: wallet database error")
	// ErrSeedRequired is returned by Open when the wallet database has to
	// be migrated to a newer schema and the migration needs the wallet's
	// seed. Opening again with the seed resolves it.
	ErrSeedRequired = errors.New("zeclib: the seed is required to migrate the wallet database")
	// ErrSeedNotRelevant is returned by Open when the seed passed to it does
	// not belong to any account in the wallet.
	ErrSeedNotRelevant = errors.New("zeclib: the seed does not belong to any account in the wallet")
	// ErrAccountNotFound reports an account ID that is not in the wallet.
	ErrAccountNotFound = errors.New("zeclib: account not found")
	// ErrAddressGapLimit is returned when a new address with a transparent
	// receiver cannot be generated because the wallet already holds as many
	// unused transparent addresses as its gap limit allows. Funds sent to
	// an address past the gap would not be found when the wallet is
	// restored from its seed. The limit lifts once one of the unused
	// addresses receives funds.
	ErrAddressGapLimit = errors.New("zeclib: transparent address gap limit reached")
	// ErrInternal reports a bug in the Rust backend that was caught at the
	// FFI boundary.
	ErrInternal = errors.New("zeclib: internal error")
	// ErrSend is returned when a payment cannot be proposed or created for
	// a reason that has no error of its own.
	ErrSend = errors.New("zeclib: the payment cannot be made")
	// ErrInsufficientFunds is returned when the account's spendable funds
	// do not cover a payment and its fee. The error is an
	// *InsufficientFundsError, which carries the amounts.
	ErrInsufficientFunds = errors.New("zeclib: insufficient funds")
	// ErrNotSynced is returned by a send or an estimate when the wallet has
	// not scanned far enough to build a transaction. Syncing resolves it.
	ErrNotSynced = errors.New("zeclib: the wallet is not synced far enough to send")
	// ErrFeeTooHigh is returned when the fee of a payment exceeds the limit
	// the caller set. Nothing is created. The error is a *FeeTooHighError.
	ErrFeeTooHigh = errors.New("zeclib: the fee exceeds the limit")
	// ErrAmountChanged is returned by SendMax when the amount it would pay
	// differs from the expected amount. Nothing is created. The error is an
	// *AmountChangedError.
	ErrAmountChanged = errors.New("zeclib: the amount changed")
	// ErrTooLarge is returned when a payment would make a transaction larger
	// than a block can hold, typically for spending too many inputs.
	// Nothing is created. A smaller payment can succeed.
	ErrTooLarge = errors.New("zeclib: the transaction would be too large")
	// ErrUnsupportedRecipient is returned for a recipient the wallet cannot
	// pay: an address of another network, a Sapling address, a TEX address,
	// or a unified address without an Orchard or a transparent receiver.
	ErrUnsupportedRecipient = errors.New("zeclib: unsupported recipient")
	// ErrAddressExpired is returned for a payment to a unified address that
	// has expired, or that expires before the transaction would (ZIP 316).
	ErrAddressExpired = errors.New("zeclib: the address has expired")
	// ErrAmountBelowMinimum is returned for a payment to a transparent
	// receiver below the smallest output nodes relay. The error is an
	// *AmountBelowMinimumError, which carries the minimum.
	ErrAmountBelowMinimum = errors.New("zeclib: the amount is below the smallest transparent output")
	// ErrServerUnreachable is matched by a failure to reach the server or
	// of the connection to it.
	ErrServerUnreachable = errors.New("zeclib: the server could not be reached")
	// ErrServerFailure is matched by a failure of a server call other than
	// the connection, such as an internal error of the server.
	ErrServerFailure = errors.New("zeclib: the server failed")
	// ErrMemoNotAllowed is returned when a memo is given for a payment to a
	// transparent receiver.
	ErrMemoNotAllowed = errors.New("zeclib: a memo cannot be sent to a transparent recipient")
	// ErrSeedMismatch is returned when the seed given to spend from an
	// account is not the seed the account was derived from.
	ErrSeedMismatch = errors.New("zeclib: the seed does not belong to the account")
	// ErrBroadcastRejected means the server's node refused a transaction,
	// as opposed to a server that could not be reached. The error is a
	// *BroadcastRejectedError.
	ErrBroadcastRejected = errors.New("zeclib: the transaction was rejected")
	// ErrWrongNetwork means the server, or a tree state passed to
	// CreateAccountWithTreeState, is for a different chain than the wallet.
	// During sync it is reported in SyncStatus.
	ErrWrongNetwork = errors.New("zeclib: the server is on a different network")
	// ErrConsensusBranch means the server reports another consensus branch
	// for its chain tip than the wallet has for that height. One of the
	// two has missed a network upgrade: the server, which then follows a
	// chain that the network left at the upgrade, or the wallet, which then
	// finds every server on another branch. During sync it is reported in
	// SyncStatus, and the wallet turns to its next server.
	ErrConsensusBranch = errors.New("zeclib: the server follows other consensus rules")
	// ErrServerStale means the node of the server estimates the chain to be
	// well ahead of its own tip. It is reported in SyncStatus while the
	// wallet turns to its next server.
	ErrServerStale = errors.New("zeclib: the server is behind the chain")
	// ErrReorgTooDeep is reported in SyncStatus when the chain was
	// reorganized below the lowest height the wallet can rewind to. The
	// wallet database has to be rebuilt from the seed.
	ErrReorgTooDeep = errors.New("zeclib: chain reorganization is deeper than the wallet can rewind")
	// ErrOutputUnrecorded is reported in SyncStatus.TransparentErr when an
	// unspent transparent output the server lists could not be recorded:
	// its funding transaction could not be fetched or stored, or the
	// server's output does not match the transaction. The output is in no
	// balance until it is recorded. The wallet asks again in every cycle,
	// except while the requests for the transparent addresses are put off
	// after a failure, which TransparentErr then reports in the same error.
	ErrOutputUnrecorded = errors.New("zeclib: an unspent transparent output could not be recorded")
)

// InsufficientFundsError reports a payment that the account's spendable
// funds do not cover. It matches ErrInsufficientFunds. It is also the error
// for a payment that needs more transparent inputs than the 1,333 one
// transaction takes, with Available the value of the capped selection.
type InsufficientFundsError struct {
	// Available is the value the account can spend, and Required the value
	// the payment needs including its fee, both in zatoshis.
	Available, Required uint64
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("zeclib: insufficient funds: %d zatoshis available, %d required", e.Available, e.Required)
}

// Is makes the error match ErrInsufficientFunds.
func (e *InsufficientFundsError) Is(target error) bool { return target == ErrInsufficientFunds }

// AmountBelowMinimumError reports a payment to a transparent receiver below
// the smallest output nodes relay. It matches ErrAmountBelowMinimum.
type AmountBelowMinimumError struct {
	// Minimum is the smallest amount the receiver can be paid, in zatoshis:
	// 54 for the standard scripts.
	Minimum uint64
}

func (e *AmountBelowMinimumError) Error() string {
	return fmt.Sprintf("zeclib: the amount is below the smallest transparent output of %d zatoshis", e.Minimum)
}

// Is makes the error match ErrAmountBelowMinimum.
func (e *AmountBelowMinimumError) Is(target error) bool { return target == ErrAmountBelowMinimum }

// AmountChangedError reports a send max whose amount differs from the one
// the caller expected, because the spendable inputs changed. It matches
// ErrAmountChanged.
type AmountChangedError struct {
	// Expected is the amount the caller expected and Actual the amount the
	// send would have paid, in zatoshis.
	Expected, Actual uint64
}

func (e *AmountChangedError) Error() string {
	return fmt.Sprintf("zeclib: the amount changed from %d to %d zatoshis", e.Expected, e.Actual)
}

// Is makes the error match ErrAmountChanged.
func (e *AmountChangedError) Is(target error) bool { return target == ErrAmountChanged }

// FeeTooHighError reports a payment whose fee exceeds the caller's limit. It
// matches ErrFeeTooHigh.
type FeeTooHighError struct {
	// Fee is the fee of the payment and MaxFee the limit, in zatoshis.
	Fee, MaxFee uint64
}

func (e *FeeTooHighError) Error() string {
	return fmt.Sprintf("zeclib: the fee of %d zatoshis exceeds the limit of %d", e.Fee, e.MaxFee)
}

// Is makes the error match ErrFeeTooHigh.
func (e *FeeTooHighError) Is(target error) bool { return target == ErrFeeTooHigh }

// BroadcastRejectedError reports a transaction that the server's node
// refused. It matches ErrBroadcastRejected.
type BroadcastRejectedError struct {
	// Code is the error code the server reported, zero if it reported none.
	Code int32
	// Message is the reason the server gave.
	Message string
}

func (e *BroadcastRejectedError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("zeclib: the transaction was rejected: error %d: %s", e.Code, e.Message)
	}
	return "zeclib: the transaction was rejected: " + e.Message
}

// Is makes the error match ErrBroadcastRejected.
func (e *BroadcastRejectedError) Is(target error) bool { return target == ErrBroadcastRejected }

// chainContinuityError reports that scanned blocks do not connect to the
// chain the wallet has already scanned, which is how a chain reorganization
// shows up.
type chainContinuityError struct {
	// height is the height of the first block that does not connect.
	height uint32
	reason string
}

func (e *chainContinuityError) Error() string {
	return fmt.Sprintf("zeclib: chain continuity error at height %d: %s", e.height, e.reason)
}

// rewindError reports that the wallet cannot be rewound to a height.
type rewindError struct {
	requested uint32
	// safe is a height to which the wallet can be rewound, when hasSafe is
	// set.
	safe    uint32
	hasSafe bool
}

func (e *rewindError) Error() string {
	if e.hasSafe {
		return fmt.Sprintf("zeclib: cannot rewind the wallet to height %d, only to %d or above",
			e.requested, e.safe)
	}
	return fmt.Sprintf("zeclib: cannot rewind the wallet to height %d", e.requested)
}

// scanError reports a block that could not be scanned for a reason other
// than chain continuity.
type scanError struct {
	height uint32
	reason string
}

func (e *scanError) Error() string {
	return fmt.Sprintf("zeclib: scan error at height %d: %s", e.height, e.reason)
}

// wrapf returns an error that is base and carries a detail message.
func wrapf(base error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", base, fmt.Sprintf(format, args...))
}
