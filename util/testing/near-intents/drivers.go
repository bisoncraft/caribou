// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Driver moves funds on one chain's simnet harness. Amounts are in the
// chain's catalog base units (sats, yoctoNEAR, lamports, wei). Drivers
// serialize their own harness access; callers may invoke them from
// concurrent swap goroutines.
type Driver interface {
	Slug() string
	// NewDepositAddress returns a fresh address the mock can watch for a
	// deposit. The mock never needs to spend from it on chains where
	// detection is balance-based; funds sent there simply strand, which is
	// acceptable on simnet.
	NewDepositAddress() (string, error)
	// CheckDeposit returns the total received at addr, and a transaction
	// ID when the chain makes one cheap to recover ("" otherwise — the
	// server falls back to the /v0/deposit/submit hint, then a synthetic
	// ID).
	CheckDeposit(addr string) (amount *big.Int, txid string, err error)
	// Send delivers amount to addr and returns the transaction ID.
	Send(addr string, amount *big.Int) (txid string, err error)
	// Confirm advances the chain so wallets see recent transactions
	// confirmed (mining a block on UTXO chains). A no-op on chains that
	// produce blocks on their own.
	Confirm() error
}

// newHarnessDriver builds the harness-backed driver for a chain slug.
func newHarnessDriver(slug, dextestDir string) (Driver, error) {
	switch slug {
	case "btc", "ltc", "bch", "doge", "dash", "zec":
		return newUTXODriver(slug, dextestDir)
	case "near":
		return newNearDriver(dextestDir)
	case "sol":
		return newSolDriver(dextestDir)
	case "eth":
		return newEVMDriver(slug, "eth", "http://127.0.0.1:38556", dextestDir)
	case "pol":
		return newEVMDriver(slug, "polygon", "http://127.0.0.1:48296", dextestDir)
	}
	return nil, fmt.Errorf("no driver for chain %q", slug)
}

// runScript executes a harness helper script and returns its trimmed
// combined output. The script's directory is used as the working directory,
// matching how the harness scripts expect to be invoked.
func runScript(dir, name string, args ...string) (string, error) {
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("harness script %s not found (is the harness running?): %w", path, err)
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// rpcCall performs a JSON-RPC 2.0 request and decodes the result member
// into thing. An RPC-level error is returned as rpcError.
type rpcError struct {
	msg string
}

func (e *rpcError) Error() string { return e.msg }

var rpcHTTPClient = &http.Client{Timeout: 10 * time.Second}

func rpcCall(url, method string, params any, thing any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "nearintentsmock",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	resp, err := rpcHTTPClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("bad RPC response from %s: %w", url, err)
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return &rpcError{msg: string(env.Error)}
	}
	if thing != nil {
		if err := json.Unmarshal(env.Result, thing); err != nil {
			return fmt.Errorf("error decoding %s result: %w", method, err)
		}
	}
	return nil
}
