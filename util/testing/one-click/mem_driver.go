// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package oneclickmock

import (
	"fmt"
	"math/big"
	"sync"
)

// MemDriver is an in-memory Driver for tests in other packages: deposits
// are credited with Credit, and payouts are recorded instead of sent.
type MemDriver struct {
	slug string

	mtx      sync.Mutex
	addrN    int
	deposits map[string]*big.Int
	sends    map[string]*big.Int
}

// NewMemDriver returns an in-memory driver for a chain slug (a key of the
// mock's chain table, such as "btc").
func NewMemDriver(slug string) *MemDriver {
	return &MemDriver{slug: slug, deposits: make(map[string]*big.Int), sends: make(map[string]*big.Int)}
}

func (d *MemDriver) Slug() string { return d.slug }

func (d *MemDriver) NewDepositAddress() (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	d.addrN++
	return fmt.Sprintf("%s-deposit-%d", d.slug, d.addrN), nil
}

func (d *MemDriver) CheckDeposit(addr string) (*big.Int, string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	if amt := d.deposits[addr]; amt != nil {
		return new(big.Int).Set(amt), "", nil
	}
	return new(big.Int), "", nil
}

func (d *MemDriver) Send(addr string, amount *big.Int) (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	if d.sends[addr] == nil {
		d.sends[addr] = new(big.Int)
	}
	d.sends[addr].Add(d.sends[addr], amount)
	return fmt.Sprintf("%s-payout-%s", d.slug, addr), nil
}

func (d *MemDriver) Confirm() error { return nil }

// Credit records a deposit of amount at addr.
func (d *MemDriver) Credit(addr string, amount *big.Int) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	if d.deposits[addr] == nil {
		d.deposits[addr] = new(big.Int)
	}
	d.deposits[addr].Add(d.deposits[addr], amount)
}

// Sent returns the total paid out to addr.
func (d *MemDriver) Sent(addr string) *big.Int {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	if v := d.sends[addr]; v != nil {
		return new(big.Int).Set(v)
	}
	return new(big.Int)
}
