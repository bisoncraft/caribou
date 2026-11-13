// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import "time"

// backend is the wallet database: keys, notes, transparent outputs, note
// commitment trees and scan progress. The implementation is the Rust crate,
// reached through the generated bindings (backend_ffi.go). It is an interface
// so that the code that drives it can be tested without the Rust library.
//
// The wallet learns about the chain only through these calls. Every method
// is safe for concurrent use. Calls are serialized by the implementation, so
// a read waits for a scan in progress.
type backend interface {
	// createAccount derives an account from a seed. treeState is the
	// protobuf-encoded lightwalletd TreeState of the block before the
	// account's birthday. recoverUntil is the chain tip height when
	// restoring an account that may already hold funds, and nil for a new
	// account.
	createAccount(name string, seed, treeState []byte, recoverUntil *uint32, keySource string) (*Account, error)
	accounts() ([]Account, error)
	isSeedRelevant(seed []byte) (bool, error)

	currentAddress(accountID string) (string, error)
	nextAvailableAddress(accountID string, receivers ReceiverSet) (string, error)
	// transparentReceivers lists the transparent addresses of the account
	// that can receive funds, including change addresses.
	transparentReceivers(accountID string) ([]string, error)

	// summary returns nil while the wallet lacks the data to compute one:
	// before it has an account, a chain tip and a scanned block.
	// minConfirmations of zero selects the ZIP 315 policy.
	summary(minConfirmations uint32) (*Summary, error)
	// birthday returns the earliest account birthday, and false for a
	// wallet without accounts.
	birthday() (uint32, bool, error)
	// maxScannedHeight returns the greatest scanned height, and false
	// before any block has been scanned.
	maxScannedHeight() (uint32, bool, error)
	// blockHash returns the hash of the scanned block at height, in the
	// byte order of a compact block's hash, or nil if that block has not
	// been scanned.
	blockHash(height uint32) ([]byte, error)

	// putSubtreeRoots stores roots of completed subtrees of the Ironwood
	// note commitment tree.
	putSubtreeRoots(roots *subtreeRoots) error
	updateChainTip(height uint32) error
	// suggestScanRanges returns the ranges the wallet wants scanned, most
	// urgent first. It is empty when the wallet is scanned to its tip.
	suggestScanRanges() ([]scanRange, error)
	// scanBlocks scans consecutive protobuf-encoded compact blocks.
	// fromState is the encoded TreeState of the block before the first
	// one. The batch is stored atomically. A *chainContinuityError means it
	// does not connect to what the wallet has scanned.
	scanBlocks(fromState []byte, blocks [][]byte) (*scanSummary, error)
	// rewindToHeight discards what the wallet learned above height and
	// returns the height it rewound to, which is height or the nearest
	// height below it that the wallet can rewind to. A *rewindError means
	// there is no such height.
	rewindToHeight(height uint32) (uint32, error)

	// utxoQueryHeight is the height from which to ask a server for the
	// account's unspent transparent outputs.
	utxoQueryHeight(accountID string) (uint32, error)
	putUTXO(u *utxo) error

	transactionDataRequests() ([]txRequest, error)
	setTransactionStatus(txid []byte, status txStatus) error
	// decryptAndStoreTransaction stores the details of a full transaction
	// and returns its ID. minedHeight is nil for an unmined transaction.
	decryptAndStoreTransaction(tx []byte, minedHeight *uint32) ([]byte, error)
	// notifyAddressChecked closes a txRequestAddress request. asOfHeight is
	// the last height the query covered.
	notifyAddressChecked(req *txRequest, asOfHeight uint32) error
	// scheduleNextCheck schedules the next check of a transparent address
	// at a randomized time that is offset from now on average. It returns
	// false for an address the wallet does not schedule checks for.
	scheduleNextCheck(address string, offset time.Duration) (time.Time, bool, error)

	// close releases the database. Calls made after it return ErrClosed.
	close() error
}
