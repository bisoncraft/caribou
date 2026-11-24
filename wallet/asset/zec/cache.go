// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"sync"
	"sync/atomic"

	"github.com/bisoncraft/caribou/wallet/asset"
)

// cache holds what the wallet read from zeclib last, so that the calls core
// makes often read memory, and what it last reported to core, so that a note
// is sent only on a change.
type cache struct {
	// seq numbers the refreshes of the balance. A refresh takes its number
	// before it reads zeclib, and stores its result only if no refresh with
	// a later number has stored: a refresh that read before a send must not
	// overwrite the balance read after it.
	seq atomic.Uint64

	mtx sync.Mutex
	// stored is the number of the refresh whose balance is held.
	stored  uint64
	balance asset.Balance
	// emitted is the balance last sent in a BalanceChange, nil before the
	// first.
	emitted *asset.Balance

	// address is the deposit address, and owned the address and the
	// addresses of its receivers.
	address string
	owned   map[string]struct{}

	// tip is the chain tip of the last sync status read.
	tip uint32
	// peers is the peer count last reported, -1 before the first report of
	// a connection.
	peers int
	// tipEmitted is the tip of the last TipChange.
	tipEmitted uint32
	// errText is the text of the last sync error logged, and unrecorded is
	// set while the status reports an unrecorded output.
	errText    string
	unrecorded bool
}

// reset forgets everything, for a database that was closed or opened.
func (c *cache) reset() {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	c.stored = c.seq.Load()
	c.balance = asset.Balance{}
	c.emitted = nil
	c.address = ""
	c.owned = nil
	c.tip = 0
	c.peers = -1
	c.tipEmitted = 0
	c.errText = ""
	c.unrecorded = false
}

// connected starts the peer reports of a new connection.
func (c *cache) connected() {
	c.mtx.Lock()
	c.peers = -1
	c.mtx.Unlock()
}

// next numbers a refresh of the balance.
func (c *cache) next() uint64 {
	return c.seq.Add(1)
}

// storeBalance stores the balance of refresh seq, unless a later refresh has
// stored, and reports it with emit if it differs from the balance last
// reported. The store and the report are one step under the cache's lock,
// so the reports reach core in the order of the refreshes: a refresh that
// stored before a later one cannot report after it. emit must not block.
// It returns whether it reported.
func (c *cache) storeBalance(seq uint64, bal asset.Balance, emit func(*asset.Balance)) bool {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if seq <= c.stored {
		return false
	}
	c.stored = seq
	c.balance = bal
	if c.emitted != nil && sameBalance(c.emitted, &bal) {
		return false
	}
	emitted := bal
	c.emitted = &emitted
	if emit != nil {
		report := bal
		emit(&report)
	}
	return true
}

func sameBalance(a, b *asset.Balance) bool {
	return a.Available == b.Available && a.Immature == b.Immature && a.Locked == b.Locked
}

// heldBalance returns the stored balance.
func (c *cache) heldBalance() asset.Balance {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	return c.balance
}

// chainTip returns the chain tip of the last status read, zero if none was.
func (c *cache) chainTip() uint32 {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	return c.tip
}
