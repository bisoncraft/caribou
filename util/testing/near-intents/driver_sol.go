// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/decred/base58"
)

// solHarnessRPC is the Solana test-validator harness RPC endpoint.
const solHarnessRPC = "http://127.0.0.1:18999"

// solDriver drives the Solana harness. Deposit addresses are random
// 32-byte pubkeys (system transfers create the account on first receipt;
// the wallet's own send tolerates unfunded recipients). Detection is a
// getBalance RPC poll; payouts go through harness-ctl/send, which
// transfers from the faucet keypair with --allow-unfunded-recipient. The
// test validator produces blocks on its own, so Confirm is a no-op.
type solDriver struct {
	rpc    string
	ctlDir string
	mtx    sync.Mutex
}

func newSolDriver(dextestDir string) (*solDriver, error) {
	ctlDir := filepath.Join(dextestDir, "sol", "harness-ctl")
	if _, err := os.Stat(ctlDir); err != nil {
		return nil, fmt.Errorf("harness-ctl dir %s not found (is the sol harness running?): %w", ctlDir, err)
	}
	return &solDriver{rpc: solHarnessRPC, ctlDir: ctlDir}, nil
}

func (d *solDriver) Slug() string { return "sol" }

func (d *solDriver) NewDepositAddress() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base58.Encode(b[:]), nil
}

func (d *solDriver) CheckDeposit(addr string) (*big.Int, string, error) {
	var res struct {
		Value uint64 `json:"value"`
	}
	if err := rpcCall(d.rpc, "getBalance", []any{addr}, &res); err != nil {
		return nil, "", err
	}
	return new(big.Int).SetUint64(res.Value), "", nil
}

func (d *solDriver) Send(addr string, amount *big.Int) (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	out, err := runScript(d.ctlDir, "send", addr, formatUnits(amount, 9, 9))
	if err != nil {
		return "", err
	}
	// The solana CLI prints "Signature: <sig>" among other output.
	for line := range strings.SplitSeq(out, "\n") {
		if sig, ok := strings.CutPrefix(strings.TrimSpace(line), "Signature: "); ok {
			return sig, nil
		}
	}
	return "", nil
}

func (d *solDriver) Confirm() error { return nil }
