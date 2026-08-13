// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
)

// utxoDriver drives a Bitcoin-family harness through its harness-ctl
// scripts. Deposit addresses belong to the harness's alpha wallet, which is
// what makes getreceivedbyaddress-based detection work; minconf 0 sees
// mempool transactions, so detection precedes mining.
type utxoDriver struct {
	slug   string
	ctlDir string
	mtx    sync.Mutex
}

func newUTXODriver(slug, dextestDir string) (*utxoDriver, error) {
	ctlDir := filepath.Join(dextestDir, slug, "harness-ctl")
	if _, err := os.Stat(ctlDir); err != nil {
		return nil, fmt.Errorf("harness-ctl dir %s not found (is the %s harness running?): %w", ctlDir, slug, err)
	}
	return &utxoDriver{slug: slug, ctlDir: ctlDir}, nil
}

func (d *utxoDriver) Slug() string { return d.slug }

func (d *utxoDriver) NewDepositAddress() (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	return runScript(d.ctlDir, "alpha", "getnewaddress")
}

func (d *utxoDriver) CheckDeposit(addr string) (*big.Int, string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	out, err := runScript(d.ctlDir, "alpha", "getreceivedbyaddress", addr, "0")
	if err != nil {
		return nil, "", err
	}
	amt, err := parseUnits(out, 8)
	if err != nil {
		return nil, "", fmt.Errorf("bad getreceivedbyaddress output %q: %w", out, err)
	}
	return amt, "", nil
}

func (d *utxoDriver) Send(addr string, amount *big.Int) (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	return runScript(d.ctlDir, "alpha", "sendtoaddress", addr, formatUnits(amount, 8, 8))
}

func (d *utxoDriver) Confirm() error {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	_, err := runScript(d.ctlDir, "mine-alpha", "1")
	return err
}
