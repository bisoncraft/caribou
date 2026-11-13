// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package nearintentsmock

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/decred/base58"
)

// zecHarnessRPC is the JSON-RPC endpoint of the Zcash harness alpha node.
const zecHarnessRPC = "http://127.0.0.1:33768"

// zecRegtestP2PKHPrefix is the version prefix of a transparent
// pay-to-pubkey-hash address on Zcash testnet and regtest ("tm...").
var zecRegtestP2PKHPrefix = [2]byte{0x1D, 0x25}

// zecDriver drives the Zcash harness. Its zebrad nodes have no wallet, so
// the wallet RPCs utxoDriver relies on do not exist there.
//
// Deposit addresses are transparent addresses for random key hashes, so
// deposits strand. Detection sums what the node's address index reports as
// mined with what the mempool holds, which keeps detection ahead of mining
// as on the wallet-backed UTXO harnesses. Payouts go through
// harness-ctl/send-alpha, which spends the alpha funder's shielded funds
// and pays transparent, Sapling and unified addresses.
type zecDriver struct {
	rpc    string
	ctlDir string
	// mtx serializes use of the funder wallet, which send-alpha syncs and
	// spends from.
	mtx sync.Mutex
}

func newZecDriver(dextestDir string) (*zecDriver, error) {
	ctlDir := filepath.Join(dextestDir, "zec", "harness-ctl")
	if _, err := os.Stat(filepath.Join(ctlDir, "send-alpha")); err != nil {
		return nil, fmt.Errorf("harness script %s not found (is the zec harness running?): %w",
			filepath.Join(ctlDir, "send-alpha"), err)
	}
	return &zecDriver{rpc: zecHarnessRPC, ctlDir: ctlDir}, nil
}

func (d *zecDriver) Slug() string { return "zec" }

func (d *zecDriver) Ping() error {
	return rpcCall(d.rpc, "getblockcount", []any{}, nil)
}

// encodeZecP2PKH encodes a 20-byte public key hash as a regtest transparent
// address. Zcash uses a two-byte version and a double SHA-256 checksum.
func encodeZecP2PKH(pkHash [20]byte) string {
	b := make([]byte, 0, 26)
	b = append(b, zecRegtestP2PKHPrefix[:]...)
	b = append(b, pkHash[:]...)
	first := sha256.Sum256(b)
	second := sha256.Sum256(first[:])
	return base58.Encode(append(b, second[:4]...))
}

func (d *zecDriver) NewDepositAddress() (string, error) {
	var pkHash [20]byte
	if _, err := rand.Read(pkHash[:]); err != nil {
		return "", err
	}
	return encodeZecP2PKH(pkHash), nil
}

// CheckDeposit returns the mined plus unmined total paid to addr. The mined
// total is read before the mempool, so a transaction mined between the two
// calls is missed for one poll and never counted twice.
func (d *zecDriver) CheckDeposit(addr string) (*big.Int, string, error) {
	addrs := map[string]any{"addresses": []string{addr}}
	var bal struct {
		Received int64 `json:"received"`
	}
	if err := rpcCall(d.rpc, "getaddressbalance", []any{addrs}, &bal); err != nil {
		return nil, "", err
	}
	total := big.NewInt(bal.Received)

	var mempool []string
	if err := rpcCall(d.rpc, "getrawmempool", []any{}, &mempool); err != nil {
		return nil, "", err
	}
	var txid string
	for _, id := range mempool {
		var tx struct {
			Vout []struct {
				ValueZat     int64 `json:"valueZat"`
				ScriptPubKey struct {
					Addresses []string `json:"addresses"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		}
		if err := rpcCall(d.rpc, "getrawtransaction", []any{id, 1}, &tx); err != nil {
			continue // mined or evicted since the mempool was listed
		}
		for _, out := range tx.Vout {
			if slices.Contains(out.ScriptPubKey.Addresses, addr) {
				total.Add(total, big.NewInt(out.ValueZat))
				txid = id
			}
		}
	}

	if txid == "" && bal.Received > 0 {
		var ids []string
		if err := rpcCall(d.rpc, "getaddresstxids", []any{addrs}, &ids); err == nil && len(ids) > 0 {
			txid = ids[len(ids)-1]
		}
	}
	return total, txid, nil
}

func (d *zecDriver) Send(addr string, amount *big.Int) (string, error) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	out, err := runScript(d.ctlDir, "send-alpha", addr, formatUnits(amount, 8, 8))
	if err != nil {
		return "", err
	}
	// send-alpha prints the proposal and then the transaction ID on the
	// last line.
	lines := strings.Split(out, "\n")
	txid := strings.TrimSpace(lines[len(lines)-1])
	if len(txid) != 64 {
		return "", fmt.Errorf("no transaction ID in send-alpha output: %s", out)
	}
	return txid, nil
}

func (d *zecDriver) Confirm() error {
	_, err := runScript(d.ctlDir, "mine-alpha", "1")
	return err
}
