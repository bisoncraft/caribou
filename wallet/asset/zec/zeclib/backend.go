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
	// that can receive funds, including change addresses, ordered by chain
	// and index. On each chain they are the addresses up to the last used
	// one and the unused ones after it. An address is used once the wallet
	// has stored an output that pays it, spent or not.
	transparentReceivers(accountID string) ([]watchedAddress, error)

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

	// consensusBranchID is the consensus branch ID of the wallet's network
	// at a height, which a node reports for a chain tip at that height.
	consensusBranchID(height uint32) (uint32, error)

	// utxoQueryHeight is the height from which to ask a server for the
	// account's unspent transparent outputs.
	utxoQueryHeight(accountID string) (uint32, error)
	// putUTXO records an unspent output. When the wallet holds the output's
	// transaction, an output the transaction does not have, by index,
	// script and value, fails with ErrInvalidArgument, and a held
	// transaction that cannot be read fails with ErrDatabase. Neither is
	// recorded.
	putUTXO(u *utxo) error
	// missingTransactions returns the IDs among txids of the transactions
	// the wallet does not hold the full data of.
	missingTransactions(txids [][]byte) ([][]byte, error)

	transactionDataRequests() ([]txRequest, error)
	setTransactionStatus(txid []byte, status txStatus) error
	// decryptAndStoreTransaction stores the details of a full transaction
	// and returns its ID. A set txid is the ID that the transaction must
	// have: one with another ID is not stored, and the error is then
	// ErrInvalidArgument. minedHeight is nil for an unmined transaction. A
	// transaction that does not involve the wallet is not stored, and the
	// call succeeds.
	decryptAndStoreTransaction(tx, txid []byte, minedHeight *uint32) ([]byte, error)
	// notifyAddressChecked closes a txRequestAddress request. asOfHeight is
	// the last height the query covered.
	notifyAddressChecked(req *txRequest, asOfHeight uint32) error
	// scheduleNextCheck schedules the next check of a transparent address
	// at a randomized time that is offset from now on average. It returns
	// false for an address the wallet does not schedule checks for.
	scheduleNextCheck(address string, offset time.Duration) (time.Time, bool, error)

	// estimateTransfer returns the fee of a payment, proposed as send would
	// propose it. Nothing is stored. minConfirmations is as for summary.
	estimateTransfer(accountID, address string, amount uint64, memo []byte, minConfirmations uint32) (*TransferEstimate, error)
	// send creates and stores the transactions that pay amount to address
	// from the account, and returns them for broadcast. seed is the
	// account's ZIP 32 seed. Once it returns without error the transactions
	// exist, whether or not their raw bytes could be read back.
	send(accountID string, seed []byte, address string, amount uint64, memo []byte, maxFee uint64, minConfirmations uint32) (*sendResult, error)
	// estimateSendMax returns what sendMax would send.
	estimateSendMax(accountID, address string, memo []byte, minConfirmations uint32) (*SendMaxEstimate, error)
	// sendMax creates and stores the transaction that sends everything one
	// transaction can take from the account to address. A non-zero
	// expectedAmount must equal the amount, or nothing is created.
	sendMax(accountID string, seed []byte, address string, memo []byte, maxFee, expectedAmount uint64, minConfirmations uint32) (*sendMaxResult, error)
	// spendable returns the value of the inputs a send can select.
	spendable(accountID string, minConfirmations uint32) (SpendableValue, error)
	// rebroadcastInventory lists the transactions the wallet created that
	// are neither mined nor expired as of the last chain tip, oldest first.
	rebroadcastInventory() ([]unminedTx, error)
	// transactions lists the account's history after afterSeq, at most
	// limit records, and transaction reads one record by its ID in wire
	// byte order, with the chain tip it was read against. openTransactions
	// lists the records that can still change.
	transactions(accountID string, afterSeq uint64, limit uint32) (*TxListing, error)
	transaction(accountID string, txid []byte) (*TxRecord, uint32, error)
	openTransactions(accountID string, depth uint32) (*TxListing, error)
	// ownsAddress returns the account an address belongs to, and false if it
	// belongs to none.
	ownsAddress(address string) (accountID string, ok bool, err error)

	// close releases the database. Calls made after it return ErrClosed.
	close() error
}
