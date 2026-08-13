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

	"github.com/bisoncraft/caribou/util"
)

// evmDriver drives a geth-dev-based harness (eth, polygon). Deposit
// addresses are random 20-byte addresses (funds strand — acceptable on
// simnet). Detection is an eth_getBalance poll; payouts go through
// harness-ctl/sendtoaddress, which takes a conventional-unit amount and an
// address without the 0x prefix. Dev-mode geth mines on its own, so
// Confirm is a no-op.
//
// The driver also reports the test tokens the harness deployed (their
// contract addresses land in test_*_contract_address.txt files in the
// harness data dir), detects token deposits via eth_call balanceOf, and
// pays tokens out through the corresponding harness-ctl send script.
type evmDriver struct {
	slug   string
	rpc    string
	ctlDir string
	tokens []*driverToken
	mtx    sync.Mutex
}

// evmTokenSpec describes a test token an EVM harness may have deployed.
// walletSymbols maps chain slug to the wallet asset symbol whose live rate
// prices the token.
type evmTokenSpec struct {
	symbol           string
	decimals         int
	contractFile     string
	sendScript       string
	fallbackMicroUSD int64
	walletSymbols    map[string]string
}

var evmTokenSpecs = []*evmTokenSpec{{
	symbol:           "USDC",
	decimals:         6, // the harness test contract's scale (see harness-ctl/sendUSDC)
	contractFile:     "test_usdc_contract_address.txt",
	sendScript:       "sendUSDC",
	fallbackMicroUSD: 1_000_000,
	walletSymbols:    map[string]string{"eth": "usdc.eth", "pol": "usdc.polygon"},
}}

func newEVMDriver(slug, harnessDir, rpc, dextestDir string) (*evmDriver, error) {
	ctlDir := filepath.Join(dextestDir, harnessDir, "harness-ctl")
	if _, err := os.Stat(ctlDir); err != nil {
		return nil, fmt.Errorf("harness-ctl dir %s not found (is the %s harness running?): %w", ctlDir, harnessDir, err)
	}
	d := &evmDriver{slug: slug, rpc: rpc, ctlDir: ctlDir}
	for _, spec := range evmTokenSpecs {
		walletSymbol := spec.walletSymbols[slug]
		if walletSymbol == "" {
			continue
		}
		walletBipID, ok := util.BipSymbolID(walletSymbol)
		if !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dextestDir, harnessDir, spec.contractFile))
		if err != nil {
			continue // token not deployed on this harness
		}
		contract := strings.TrimSpace(string(b))
		if !strings.HasPrefix(contract, "0x") || len(contract) != 42 {
			continue
		}
		d.tokens = append(d.tokens, &driverToken{
			symbol:           spec.symbol,
			decimals:         spec.decimals,
			contract:         contract,
			sendScript:       spec.sendScript,
			walletBipID:      walletBipID,
			fallbackMicroUSD: spec.fallbackMicroUSD,
		})
	}
	return d, nil
}

func (d *evmDriver) Tokens() []*driverToken { return d.tokens }

// erc20BalanceOfSelector is the ABI selector for balanceOf(address).
const erc20BalanceOfSelector = "0x70a08231"

func (d *evmDriver) CheckTokenDeposit(contract, addr string) (*big.Int, string, error) {
	data := erc20BalanceOfSelector + strings.Repeat("0", 24) + strings.ToLower(strings.TrimPrefix(addr, "0x"))
	var res string
	err := rpcCall(d.rpc, "eth_call", []any{
		map[string]any{"to": contract, "data": data}, "latest",
	}, &res)
	if err != nil {
		return nil, "", err
	}
	bal, ok := new(big.Int).SetString(strings.TrimPrefix(res, "0x"), 16)
	if !ok {
		return nil, "", fmt.Errorf("bad eth_call balanceOf result %q", res)
	}
	return bal, "", nil
}

func (d *evmDriver) SendToken(sendScript, addr string, amount *big.Int, decimals int) (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	return runScript(d.ctlDir, sendScript, strings.TrimPrefix(addr, "0x"), formatUnits(amount, decimals, decimals))
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
