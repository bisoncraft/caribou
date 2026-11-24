// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

// backend is the part of *zeclib.Wallet the asset wallet uses. Tests
// substitute a fake, so the package builds and is tested without the zec
// build tag, under which zeclib reports ErrNotBuilt.
type backend interface {
	Connect(ctx context.Context) (*sync.WaitGroup, error)
	Close() error
	SyncStatus() zeclib.SyncStatus

	CreateAccount(ctx context.Context, p *zeclib.AccountParams) (*zeclib.Account, error)
	Accounts() ([]zeclib.Account, error)
	IsSeedRelevant(seed []byte) (bool, error)
	HeightForTime(ctx context.Context, t time.Time, margin time.Duration) (uint32, error)

	Address(accountID string) (string, error)
	NewAddress(accountID string, receivers zeclib.ReceiverSet) (string, error)
	OwnsAddress(address string) (string, bool, error)

	Summary() (*zeclib.Summary, error)
	Spendable(accountID string) (zeclib.SpendableValue, error)

	EstimateTransfer(accountID, address string, amount uint64, memo []byte) (*zeclib.TransferEstimate, error)
	Send(ctx context.Context, accountID string, seed []byte, address string, amount uint64, memo []byte, maxFee uint64) (*zeclib.SendResult, error)
	EstimateSendMax(accountID, address string, memo []byte) (*zeclib.SendMaxEstimate, error)
	SendMax(ctx context.Context, accountID string, seed []byte, address string, memo []byte, maxFee, expectedAmount uint64) (*zeclib.SendMaxResult, error)

	Transactions(accountID string, afterSeq uint64, limit uint32) (*zeclib.TxListing, error)
	Transaction(accountID, txid string) (*zeclib.TxRecord, uint32, error)
	OpenTransactions(accountID string, depth uint32) (*zeclib.TxListing, error)
}

var _ backend = (*zeclib.Wallet)(nil)

var (
	// openBackend opens the zeclib wallet database. seed is normally nil;
	// see zeclib.Open.
	openBackend = func(cfg *zeclib.Config, seed []byte) (backend, error) {
		w, err := zeclib.Open(cfg, seed)
		if err != nil {
			return nil, err
		}
		return w, nil
	}
	// parseAddress is zeclib.ParseAddress.
	parseAddress = zeclib.ParseAddress
)
