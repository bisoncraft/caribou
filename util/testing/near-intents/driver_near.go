// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// nearSandboxRPC is the NEAR sandbox harness RPC endpoint
// (util/testing/near/harness.sh publishes container port 3030 on 23456).
const nearSandboxRPC = "http://localhost:23456"

// nearDriver drives the NEAR sandbox harness. Deposit addresses are
// implicit accounts — the hex of 32 random bytes — which spring into
// existence on their first incoming transfer; no key is retained because
// deposits are never spent. Detection queries the account balance over RPC
// at optimistic finality (final lags ~2 blocks on the sandbox). Payouts go
// through the harness's ~/dextest/near/send helper, which spends from the
// pre-funded test.near account. The sandbox produces blocks on its own, so
// Confirm is a no-op.
type nearDriver struct {
	rpc     string
	nearDir string // ~/dextest/near, where the harness writes its helpers
	mtx     sync.Mutex
}

func newNearDriver(dextestDir string) (*nearDriver, error) {
	nearDir := filepath.Join(dextestDir, "near")
	if _, err := os.Stat(filepath.Join(nearDir, "send")); err != nil {
		return nil, fmt.Errorf("%s/send not found (re-run util/testing/near/harness.sh to regenerate it): %w", nearDir, err)
	}
	return &nearDriver{rpc: nearSandboxRPC, nearDir: nearDir}, nil
}

func (d *nearDriver) Slug() string { return "near" }

func (d *nearDriver) NewDepositAddress() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (d *nearDriver) CheckDeposit(addr string) (*big.Int, string, error) {
	var res struct {
		Amount string `json:"amount"`
	}
	err := rpcCall(d.rpc, "query", map[string]any{
		"request_type": "view_account",
		"finality":     "optimistic",
		"account_id":   addr,
	}, &res)
	if err != nil {
		// An account that has not received anything yet does not exist.
		// The sandbox reports that as UNKNOWN_ACCOUNT (or a "does not
		// exist" data string, depending on version).
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) && (strings.Contains(rpcErr.msg, "UNKNOWN_ACCOUNT") ||
			strings.Contains(rpcErr.msg, "does not exist")) {
			return new(big.Int), "", nil
		}
		return nil, "", err
	}
	amt, ok := new(big.Int).SetString(res.Amount, 10)
	if !ok {
		return nil, "", fmt.Errorf("bad account amount %q", res.Amount)
	}
	return amt, "", nil
}

func (d *nearDriver) Send(addr string, amount *big.Int) (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	// The send helper (wrapping sendnear) takes a conventional NEAR amount
	// and parses it as a float; 12 decimal places is ample precision for
	// simnet payouts.
	out, err := runScript(d.nearDir, "send", addr, formatUnits(amount, 24, 12))
	if err != nil {
		return "", err
	}
	// sendnear prints the transaction hash on success; fall back to the
	// whole output if it is not a single token.
	if fields := strings.Fields(out); len(fields) > 0 {
		return fields[len(fields)-1], nil
	}
	return out, nil
}

func (d *nearDriver) Confirm() error { return nil }
