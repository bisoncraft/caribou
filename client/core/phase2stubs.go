// phase2stubs.go holds stubs for symbols that remain referenced by out-of-scope
// packages (client/mm, client/webserver) but whose real implementations have
// been removed with the DEX trading layer.

package core

import (
	"errors"
	"time"

	"decred.org/dcrdex/client/orderbook"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
)

// CertStore held TLS certificates for known DEX servers (was in certs.go).
// Empty in this build; retained because client/webserver/http.go references it.
var CertStore = map[dex.Network]map[string][]byte{
	dex.Mainnet: {},
	dex.Testnet: {},
	dex.Simnet:  {},
}

// BookFeed is a stub interface for the removed order-book feed.
// Still required by client/mm which is out of scope.
type BookFeed interface {
	Close()
	Next() <-chan *BookUpdate
	Candles(dur string) error
}

// SyncBook is a stub; order book functionality is removed in this build.
func (c *Core) SyncBook(_ string, _, _ uint32) (*orderbook.OrderBook, BookFeed, error) {
	return nil, nil, errors.New("order book not available in this build")
}

// Cancel and MultiTrade stubs satisfy client/mm's clientCore interface.
// They will be removed when mm is fully decoupled from the trading layer.

func (c *Core) Cancel(_ dex.Bytes) error {
	return errors.New("trading not supported in this build")
}

func (c *Core) MultiTrade(_ []byte, _ *MultiTradeForm) []*MultiTradeResult {
	return nil
}

// MaxFundingFees is a stub; trading not supported in this build.
func (c *Core) MaxFundingFees(_ uint32, _ string, _ uint32, _ map[string]string) (uint64, error) {
	return 0, errors.New("trading not supported in this build")
}

// Order is a stub; trading not supported in this build.
func (c *Core) Order(_ dex.Bytes) (*Order, error) {
	return nil, errors.New("trading not supported in this build")
}

// Orders is a stub; trading not supported in this build.
func (c *Core) Orders(_ *OrderFilter) ([]*Order, error) {
	return nil, errors.New("trading not supported in this build")
}

// SingleLotFees is a stub; trading not supported in this build.
func (c *Core) SingleLotFees(_ *SingleLotFeesForm) (uint64, uint64, uint64, error) {
	return 0, 0, 0, errors.New("trading not supported in this build")
}

// TradingLimits is a stub; trading not supported in this build.
func (c *Core) TradingLimits(_ string) (uint32, uint32, error) {
	return 0, 0, errors.New("trading not supported in this build")
}

// SubscribeMMSnapshots is a stub; trading not supported in this build.
func (c *Core) SubscribeMMSnapshots(_ string, _, _ uint32, _ bool) error {
	return errors.New("trading not supported in this build")
}

// DeleteArchivedRecords is a stub; order archive not available in this build.
func (c *Core) DeleteArchivedRecords(_ *time.Time, _, _ string) (int, error) {
	return 0, nil
}

// DeleteArchivedRecordsWithBackup is a stub; order archive not available in this build.
func (c *Core) DeleteArchivedRecordsWithBackup(_ *time.Time, _, _ bool) (string, int, error) {
	return "", 0, nil
}

// ExportMMSnapshots is a stub; MM snapshots not available in this build.
func (c *Core) ExportMMSnapshots(_ string, _, _ uint32, _, _ uint64) ([]*msgjson.MMEpochSnapshot, error) {
	return nil, errors.New("MM snapshots not available in this build")
}

// PruneMMSnapshots is a stub; MM snapshots not available in this build.
func (c *Core) PruneMMSnapshots(_ string, _, _ uint32, _ uint64) (int, error) {
	return 0, errors.New("MM snapshots not available in this build")
}

// RedeemGeocode is a stub; DEX trading not available in this build.
func (c *Core) RedeemGeocode(_, _ []byte, _ string) (dex.Bytes, uint64, error) {
	return nil, 0, errors.New("DEX trading not available in this build")
}
