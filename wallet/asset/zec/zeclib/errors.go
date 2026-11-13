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
	// ErrWrongNetwork means the server, or a tree state passed to
	// CreateAccountWithTreeState, is for a different chain than the wallet.
	// During sync it is reported in SyncStatus.
	ErrWrongNetwork = errors.New("zeclib: the server is on a different network")
	// ErrReorgTooDeep is reported in SyncStatus when the chain was
	// reorganized below the lowest height the wallet can rewind to. The
	// wallet database has to be rebuilt from the seed.
	ErrReorgTooDeep = errors.New("zeclib: chain reorganization is deeper than the wallet can rewind")
)

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
