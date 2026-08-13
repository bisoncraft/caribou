// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// evmDriver drives a geth-dev-based harness (eth, polygon). Deposit
// addresses are random 20-byte addresses (funds strand — acceptable on
// simnet). Detection is an eth_getBalance poll; payouts go through
// harness-ctl/sendtoaddress, which takes a conventional-unit amount and an
// address without the 0x prefix. Dev-mode geth mines on its own, so
// Confirm is a no-op.
type evmDriver struct {
	slug   string
	rpc    string
	ctlDir string
	mtx    sync.Mutex
}

func newEVMDriver(slug, harnessDir, rpc, dextestDir string) (*evmDriver, error) {
	ctlDir := filepath.Join(dextestDir, harnessDir, "harness-ctl")
	if _, err := os.Stat(ctlDir); err != nil {
		return nil, fmt.Errorf("harness-ctl dir %s not found (is the %s harness running?): %w", ctlDir, harnessDir, err)
	}
	return &evmDriver{slug: slug, rpc: rpc, ctlDir: ctlDir}, nil
}

func (d *evmDriver) Slug() string { return d.slug }

func (d *evmDriver) NewDepositAddress() (string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(b[:]), nil
}

func (d *evmDriver) CheckDeposit(addr string) (*big.Int, string, error) {
	var res string
	if err := rpcCall(d.rpc, "eth_getBalance", []any{addr, "latest"}, &res); err != nil {
		return nil, "", err
	}
	wei, ok := new(big.Int).SetString(strings.TrimPrefix(res, "0x"), 16)
	if !ok {
		return nil, "", fmt.Errorf("bad eth_getBalance result %q", res)
	}
	return wei, "", nil
}

func (d *evmDriver) Send(addr string, amount *big.Int) (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	// sendtoaddress parses the amount as a float and multiplies by 1e18;
	// six decimal places keeps it well within float64 precision.
	return runScript(d.ctlDir, "sendtoaddress", strings.TrimPrefix(addr, "0x"), formatUnits(amount, 18, 6))
}

func (d *evmDriver) Confirm() error { return nil }
