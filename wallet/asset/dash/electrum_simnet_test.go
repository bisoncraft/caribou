//go:build harness

package dash

// electrum_simnet_test.go contains harness integration tests for the
// electrum-backed Dash wallet (walletTypeElectrum) against a live electrumx
// server. Dash has no SegWit, so the wallet derives legacy BIP-44 P2PKH
// addresses ('y...' on regtest); the electrum protocol is otherwise identical
// to Bitcoin's. Block headers are validated by their X11 hash.
//
// Prerequisites (run by the user, not this file):
//   - Dash regtest harness: cd util/testing/dash && ./harness.sh
//     (tmux session: dash-harness, harness-ctl in window 2)
//   - Electrumx server:     cd util/testing/dash && ./electrumx.sh
//     (delegates to btc/electrumx.sh Dash 20777 55021; installs the x11_hash
//     python package and serves ssl://127.0.0.1:55021 as the DashRegtest coin)
//
// Run the tests:
//
//	go test -tags harness ./wallet/asset/dash/ -run TestDashElectrum

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/config"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/btc"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog"
	"github.com/decred/dcrd/rpcclient/v8"
)

// dashGammaSeed is an arbitrary fixed HD seed for the wallet under test. The
// wallet is funded fresh from the alpha node each run, so the value only needs
// to be stable across a test run.
const dashGammaSeed = "5d7e0707a7bbbb02904084e3bd67de97e92da2c3c2a7b248c5f9596a7da48512"

var (
	dashLogger   util.Logger
	dashCtx      context.Context
	dashPassword = []byte("abc")
)

// TestMain is the harness entry point for the Dash package's harness-tagged
// tests.
func TestMain(m *testing.M) {
	dashLogger = util.StdOutLogger("DASHTEST", util.LevelTrace)
	utxowallet.UseLogger(btclog.NewBackend(os.Stdout))
	var shutdown func()
	dashCtx, shutdown = context.WithCancel(context.Background())
	doIt := func() int {
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

// mineDashAlpha sends the mine-alpha script to the dash-harness tmux session
// (window 2 is harness-ctl).
func mineDashAlpha() error {
	return exec.Command("tmux", "send-keys", "-t", "dash-harness:2", "./mine-alpha 1", "C-m").Run()
}

// dashNetPorts is the fallback RPC-port table for the harness funder config;
// the harness alpha.conf sets rpcport explicitly, so these are rarely used.
var dashNetPorts = btc.NetPorts{
	Mainnet: "9998",
	Testnet: "19998",
	Simnet:  "19898",
}

// dashHarnessNode is a thin funding node backed by the harness dashd's RPC
// interface, providing only the connect/unlock/send surface the simnet tests
// need from the alpha funder. Wallet-under-test coverage lives in the electrum
// and neutrino simnet tests; this only drives the funder.
type dashHarnessNode struct {
	cl *rpcclient.Client
}

// Connect satisfies util.Connector. The RPC client is HTTP-POST based, so there
// is no persistent connection; the client is shut down when ctx is canceled.
func (h *dashHarnessNode) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		h.cl.Shutdown()
	}()
	return &wg, nil
}

// Unlock unlocks the node's wallet via walletpassphrase.
func (h *dashHarnessNode) Unlock(pw []byte) error {
	passJSON, _ := json.Marshal(string(pw))
	_, err := h.cl.RawRequest(dashCtx, "walletpassphrase", []json.RawMessage{passJSON, json.RawMessage("600")})
	return err
}

// Send sends value (in atoms) to addr via sendtoaddress and returns the funding
// output. feeRate is ignored; the harness node uses its own fee policy.
func (h *dashHarnessNode) Send(addr string, val, _ uint64) (asset.Coin, error) {
	addrJSON, _ := json.Marshal(addr)
	amtJSON, _ := json.Marshal(btcutil.Amount(val).ToBTC())
	b, err := h.cl.RawRequest(dashCtx, "sendtoaddress", []json.RawMessage{addrJSON, amtJSON})
	if err != nil {
		return nil, err
	}
	var txID string
	if err := json.Unmarshal(b, &txID); err != nil {
		return nil, fmt.Errorf("error decoding sendtoaddress result: %w", err)
	}
	txHash, err := chainhash.NewHashFromStr(txID)
	if err != nil {
		return nil, fmt.Errorf("error decoding txid %q: %w", txID, err)
	}
	return btc.NewOutput(txHash, 0, val), nil
}

// NewAddress returns a fresh receiving address from the harness alpha wallet,
// used as an external spend destination in recovery tests.
func (h *dashHarnessNode) NewAddress() (string, error) {
	b, err := h.cl.RawRequest(dashCtx, "getnewaddress", nil)
	if err != nil {
		return "", err
	}
	var addr string
	if err := json.Unmarshal(b, &addr); err != nil {
		return "", fmt.Errorf("decode getnewaddress: %w", err)
	}
	return addr, nil
}

// newDashAlphaBackend connects a thin funding node to the harness alpha dashd,
// reading its config at ~/dextest/dash/alpha/alpha.conf.
func newDashAlphaBackend(t *testing.T) (*dashHarnessNode, *util.ConnectionMaster) {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	cfgPath := filepath.Join(u.HomeDir, "dextest", "dash", "alpha", "alpha.conf")
	var rpcCfg btc.RPCConfig
	if err := config.ParseInto(cfgPath, &rpcCfg); err != nil {
		t.Fatalf("config.ParseInto(%s): %v", cfgPath, err)
	}
	if err := btc.CheckRPCConfig(&rpcCfg, "alpha", util.Regtest, dashNetPorts); err != nil {
		t.Fatalf("bad alpha rpc config: %v", err)
	}
	cl, err := rpcclient.New(&rpcclient.ConnConfig{
		HTTPPostMode: true,
		DisableTLS:   true,
		// The harness dashd is multi-wallet, so wallet RPCs must target the
		// /wallet/<name> URI path. An empty walletname selects the default wallet.
		Host: rpcCfg.RPCBind + "/wallet/" + rpcCfg.WalletName,
		User: rpcCfg.RPCUser,
		Pass: rpcCfg.RPCPass,
	}, nil)
	if err != nil {
		t.Fatalf("rpcclient.New(alpha): %v", err)
	}
	node := &dashHarnessNode{cl: cl}
	cm := util.NewConnectionMaster(node)
	if err = cm.Connect(dashCtx); err != nil {
		t.Fatalf("Connect(alpha): %v", err)
	}
	return node, cm
}

// newDashElectrumWallet creates a seeded electrum Dash wallet in a temp dir,
// connects it, and returns it. The electrumx server address is built-in
// (ssl://127.0.0.1:55021, the regtest entry in assets.DashParams).
func newDashElectrumWallet(t *testing.T) (asset.Wallet, *util.ConnectionMaster) {
	t.Helper()
	seed, err := hex.DecodeString(dashGammaSeed)
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	return connectDashElectrumWalletAt(t, t.TempDir(), seed, 0, btc.DefaultWalletBirthday)
}

// connectDashElectrumWalletAt creates and connects an electrum Dash wallet from
// the given seed/birthday. A positive recoveryWindow sets the "recoverywindow"
// wallet setting, overriding the ChainConfig default; 0 leaves it unset, so the
// wallet uses its built-in default (ElectrumRecoveryWindow for Dash electrum).
func connectDashElectrumWalletAt(t *testing.T, dataDir string, seed []byte, recoveryWindow uint32, bday time.Time) (asset.Wallet, *util.ConnectionMaster) {
	t.Helper()
	walletDir := filepath.Join(dataDir, RegressionNetParams.Name)
	if err := createDashElectrumWallet(dashPassword, seed, bday, walletDir, dashLogger, 0, 0, RegressionNetParams); err != nil {
		t.Fatalf("createDashElectrumWallet: %v", err)
	}
	settings := map[string]string{}
	if recoveryWindow > 0 {
		settings["recoverywindow"] = strconv.Itoa(int(recoveryWindow))
	}

	// The master transaction database is supplied by Core in production; the
	// wallet's PendingTransactions and TxHistory require it. Give each test
	// wallet its own.
	tdb, err := txdb.New(filepath.Join(dataDir, "tx.db"), dashLogger)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { tdb.Close() })

	walletCfg := &asset.WalletConfig{
		Type:        walletTypeElectrum,
		DataDir:     dataDir,
		Settings:    settings,
		NetworkTxDB: tdb.NetworkTxDB(BipID),
		Emit:        asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), BipID, dashLogger),
		PeersChange: func(n uint32, err error) {
			dashLogger.Debugf("electrum DASH peers=%d err=%v", n, err)
		},
	}
	w, err := newWallet(walletCfg, dashLogger, util.Regtest)
	if err != nil {
		t.Fatalf("NewWallet(electrum): %v", err)
	}
	cm := util.NewConnectionMaster(w)
	if err = cm.Connect(dashCtx); err != nil {
		t.Fatalf("Connect(electrum): %v", err)
	}
	return w, cm
}

const dashElectrumSyncTimeout = 3 * time.Minute

func waitDashSynced(t *testing.T, w asset.Wallet) {
	t.Helper()
	deadline := time.Now().Add(dashElectrumSyncTimeout)
	for time.Now().Before(deadline) {
		ss, err := w.SyncStatus()
		if err != nil {
			t.Logf("SyncStatus err: %v", err)
		} else if ss != nil && ss.Synced {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("Dash electrum wallet did not sync within %v", dashElectrumSyncTimeout)
}

func waitDashBalanceAtLeast(t *testing.T, w asset.Wallet, wantMin uint64) *asset.Balance {
	t.Helper()
	deadline := time.Now().Add(dashElectrumSyncTimeout)
	for time.Now().Before(deadline) {
		bal, err := w.Balance()
		if err == nil && bal != nil && bal.Available >= wantMin {
			return bal
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("Dash balance did not reach %d within %v", wantMin, dashElectrumSyncTimeout)
	return nil
}

func waitDashPendingContains(t *testing.T, w asset.Wallet, txID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, pt := range w.PendingTransactions(dashCtx) {
			if pt.ID == txID {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("tx %s not in PendingTransactions within 30s", txID)
}

func waitDashPendingNotContains(t *testing.T, w asset.Wallet, txID string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, pt := range w.PendingTransactions(dashCtx) {
			if pt.ID == txID {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("tx %s still in PendingTransactions after 60s", txID)
}

func waitDashTxConfirmed(t *testing.T, w asset.Wallet, txID string) *txdb.WalletTransaction {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		wt, err := w.WalletTransaction(dashCtx, txID)
		if err == nil && wt != nil && wt.Confirmed {
			return wt
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("tx %s not confirmed within 60s", txID)
	return nil
}

func waitDashTxInHistory(t *testing.T, w asset.Wallet, txID string) *txdb.WalletTransaction {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := w.TxHistory(&txdb.TxHistoryRequest{N: 100})
		if err == nil && resp != nil {
			for _, wt := range resp.Txs {
				if wt.ID == txID {
					return wt
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("tx %s not found in TxHistory within 60s", txID)
	return nil
}

// toSatoshiDash converts DASH to its atomic unit (1e8).
func toSatoshiDash(dash float64) uint64 {
	return uint64(dash * 1e8)
}

// unlock unlocks any wallet exposing an Unlock method — the wallet under test
// (via asset.Authenticator) or the thin alpha harness funder. Wallets without
// an unlock surface are skipped.
func unlock(t *testing.T, w any) {
	t.Helper()
	if auth, ok := w.(interface{ Unlock(pw []byte) error }); ok {
		if err := auth.Unlock(dashPassword); err != nil {
			t.Fatalf("unlock: %v", err)
		}
	}
}

// TestDashElectrumConnectSyncStatus verifies the wallet connects to the harness
// electrumx server and reaches Synced.
func TestDashElectrumConnectSyncStatus(t *testing.T) {
	w, cm := newDashElectrumWallet(t)
	defer cm.Disconnect()

	waitDashSynced(t, w)

	ss, err := w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus: %v", err)
	}
	if !ss.Synced {
		t.Fatalf("expected Synced=true, got %+v", ss)
	}
	t.Logf("Dash electrum synced: height=%d target=%d", ss.Blocks, ss.TargetHeight)
}

// TestDashElectrumDepositAddressOwnsValidate verifies DepositAddress,
// OwnsDepositAddress, and ValidateAddress, and that the address is a legacy Dash
// regtest P2PKH ('y...') — confirming the BIP-44 (no-SegWit) address purpose.
func TestDashElectrumDepositAddressOwnsValidate(t *testing.T) {
	w, cm := newDashElectrumWallet(t)
	defer cm.Disconnect()

	waitDashSynced(t, w)

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}
	if !strings.HasPrefix(addr, "y") {
		t.Fatalf("Dash regtest deposit address %q does not start with 'y' (legacy P2PKH)", addr)
	}
	t.Logf("Dash electrum deposit address: %s", addr)

	owns, err := w.OwnsDepositAddress(addr)
	if err != nil {
		t.Fatalf("OwnsDepositAddress(%s): %v", addr, err)
	}
	if !owns {
		t.Fatalf("OwnsDepositAddress(%s) = false", addr)
	}
	if !w.ValidateAddress(addr) {
		t.Fatalf("ValidateAddress(%s) = false for own address", addr)
	}
	if w.ValidateAddress("not-a-valid-dash-address") {
		t.Fatal("ValidateAddress(garbage) returned true")
	}
}

// TestDashElectrumBalanceAfterFunding funds the wallet from alpha and verifies
// the balance increases.
func TestDashElectrumBalanceAfterFunding(t *testing.T) {
	alpha, alphaCM := newDashAlphaBackend(t)
	defer alphaCM.Disconnect()
	unlock(t, alpha)

	w, cm := newDashElectrumWallet(t)
	defer cm.Disconnect()
	waitDashSynced(t, w)

	balBefore, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance before: %v", err)
	}

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const fundAmt = 10 // DASH
	if _, err = alpha.Send(addr, toSatoshiDash(fundAmt), 10); err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}
	if err = mineDashAlpha(); err != nil {
		t.Fatalf("mineDashAlpha: %v", err)
	}

	wantMin := balBefore.Available + toSatoshiDash(fundAmt)
	bal := waitDashBalanceAtLeast(t, w, wantMin)
	t.Logf("Dash balance after funding: available=%d (want>=%d)", bal.Available, wantMin)
}

// TestDashElectrumSendSelfPendingToConfirmed funds the wallet, self-sends, and
// asserts the tx goes pending → confirmed in WalletTransaction and TxHistory.
func TestDashElectrumSendSelfPendingToConfirmed(t *testing.T) {
	alpha, alphaCM := newDashAlphaBackend(t)
	defer alphaCM.Disconnect()
	unlock(t, alpha)

	w, cm := newDashElectrumWallet(t)
	defer cm.Disconnect()
	waitDashSynced(t, w)
	unlock(t, w)

	fundAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress for funding: %v", err)
	}
	if _, err = alpha.Send(fundAddr, toSatoshiDash(20), 10); err != nil {
		t.Fatalf("alpha.Send for funding: %v", err)
	}
	if err = mineDashAlpha(); err != nil {
		t.Fatalf("mineDashAlpha after funding: %v", err)
	}
	waitDashBalanceAtLeast(t, w, toSatoshiDash(20))

	destAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress for self-send dest: %v", err)
	}
	coin, err := w.Send(destAddr, toSatoshiDash(5), 10)
	if err != nil {
		t.Fatalf("Send self: %v", err)
	}
	txID := coin.TxID()
	t.Logf("Dash self-send txid: %s", txID)

	waitDashPendingContains(t, w, txID)

	if err = mineDashAlpha(); err != nil {
		t.Fatalf("mineDashAlpha after self-send: %v", err)
	}
	waitDashPendingNotContains(t, w, txID)

	wt := waitDashTxConfirmed(t, w, txID)
	t.Logf("Dash self-send confirmed at block %d", wt.BlockNumber)

	htx := waitDashTxInHistory(t, w, txID)
	if !htx.Confirmed {
		t.Fatalf("TxHistory entry for %s has Confirmed=false", txID)
	}
}

// TestDashElectrumReceiveDetection sends from alpha to the wallet, mines, and
// asserts the receive appears in TxHistory and the balance reflects it.
func TestDashElectrumReceiveDetection(t *testing.T) {
	alpha, alphaCM := newDashAlphaBackend(t)
	defer alphaCM.Disconnect()
	unlock(t, alpha)

	w, cm := newDashElectrumWallet(t)
	defer cm.Disconnect()
	waitDashSynced(t, w)

	balBefore, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance before receive: %v", err)
	}

	recvAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const recvAmt = 7 // DASH
	coin, err := alpha.Send(recvAddr, toSatoshiDash(recvAmt), 10)
	if err != nil {
		t.Fatalf("alpha.Send: %v", err)
	}
	recvTxID := coin.TxID()

	if err = mineDashAlpha(); err != nil {
		t.Fatalf("mineDashAlpha: %v", err)
	}

	wantMin := balBefore.Available + toSatoshiDash(recvAmt)
	bal := waitDashBalanceAtLeast(t, w, wantMin)
	t.Logf("Dash balance after receive: available=%d (want>=%d)", bal.Available, wantMin)

	htx := waitDashTxInHistory(t, w, recvTxID)
	t.Logf("Dash receive in TxHistory: confirmed=%v type=%v", htx.Confirmed, htx.Type)
}

// TestDashElectrumStandardSendFee checks StandardSendFee is positive and
// monotonic. Dash is legacy (P2PKH) sized.
func TestDashElectrumStandardSendFee(t *testing.T) {
	w, cm := newDashElectrumWallet(t)
	defer cm.Disconnect()

	const feeRate = 10
	fee := w.StandardSendFee(feeRate)
	if fee == 0 {
		t.Fatal("StandardSendFee(10) returned 0")
	}
	if fee2 := w.StandardSendFee(feeRate * 2); fee2 <= fee {
		t.Fatalf("StandardSendFee not monotonic: %d then %d", fee, fee2)
	}
}

// TestDashElectrumEstimateSendTxFee funds the wallet and exercises
// EstimateSendTxFee (the "Network fee" preview behind apiEstimateSendTxFee) for
// a regular send, a max withdraw, and a subtract-fee send. Dash creates only the
// legacy m/44' scope, so this guards the send/coin-select/change paths against
// reintroducing a hardcoded segwit (m/84') or taproot (m/86') scope that Dash
// does not have.
func TestDashElectrumEstimateSendTxFee(t *testing.T) {
	alpha, alphaCM := newDashAlphaBackend(t)
	defer alphaCM.Disconnect()
	unlock(t, alpha)

	w, cm := newDashElectrumWallet(t)
	defer cm.Disconnect()
	waitDashSynced(t, w)
	unlock(t, w)

	estimator, ok := w.(asset.TxFeeEstimator)
	if !ok {
		t.Fatal("Dash wallet does not implement asset.TxFeeEstimator")
	}

	fundAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress for funding: %v", err)
	}
	if _, err = alpha.Send(fundAddr, toSatoshiDash(20), 10); err != nil {
		t.Fatalf("alpha.Send for funding: %v", err)
	}
	if err = mineDashAlpha(); err != nil {
		t.Fatalf("mineDashAlpha after funding: %v", err)
	}
	bal := waitDashBalanceAtLeast(t, w, toSatoshiDash(20))

	destAddr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress for dest: %v", err)
	}

	const feeRate = 10

	// Regular send: builds coin selection and a change output. This is the
	// exact path that failed with "scope m/84'/1' not found".
	fee, valid, err := estimator.EstimateSendTxFee(destAddr, toSatoshiDash(5), feeRate, false, false)
	if err != nil {
		t.Fatalf("EstimateSendTxFee regular: %v", err)
	}
	if !valid {
		t.Fatal("EstimateSendTxFee regular: address reported invalid")
	}
	if fee == 0 {
		t.Fatal("EstimateSendTxFee regular: zero fee")
	}

	// Max withdraw: routes through the send-all estimate, whose change-size
	// helper previously defaulted to the taproot (m/86') scope.
	maxFee, valid, err := estimator.EstimateSendTxFee(destAddr, bal.Available, feeRate, false, true)
	if err != nil {
		t.Fatalf("EstimateSendTxFee max: %v", err)
	}
	if !valid || maxFee == 0 {
		t.Fatalf("EstimateSendTxFee max: valid=%v fee=%d", valid, maxFee)
	}

	// Subtract-fee send: fee comes out of the value.
	subFee, valid, err := estimator.EstimateSendTxFee(destAddr, toSatoshiDash(5), feeRate, true, false)
	if err != nil {
		t.Fatalf("EstimateSendTxFee subtract: %v", err)
	}
	if !valid || subFee == 0 {
		t.Fatalf("EstimateSendTxFee subtract: valid=%v fee=%d", valid, subFee)
	}
}

// TestDashElectrumRecoverySpentChange reproduces the real restore scenario the
// gap-limit test omits: receive funds, then spend twice to an external (harness)
// address so the wallet's remaining balance lives on change (internal-branch)
// addresses. A fresh wallet from the SAME seed with a recovery window must
// rediscover that change balance — exercising internal-branch discovery and
// spent-output ingestion under Dash's legacy scope and X11 block identity.
func TestDashElectrumRecoverySpentChange(t *testing.T) {
	alpha, alphaCM := newDashAlphaBackend(t)
	defer alphaCM.Disconnect()
	unlock(t, alpha)

	seed := randBytes(t, 32)
	bday := time.Now().Add(-time.Hour)

	wA, cmA := connectDashElectrumWalletAt(t, t.TempDir(), seed, 0, bday)
	waitDashSynced(t, wA)
	unlock(t, wA)

	// Receive 20 DASH on an external address.
	fundAddr, err := wA.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}
	if _, err = alpha.Send(fundAddr, toSatoshiDash(20), 10); err != nil {
		t.Fatalf("alpha.Send funding: %v", err)
	}
	if err = mineDashAlpha(); err != nil {
		t.Fatalf("mineDashAlpha funding: %v", err)
	}
	waitDashBalanceAtLeast(t, wA, toSatoshiDash(20))

	// Spend twice to an external harness address, leaving the remaining balance
	// on change (internal-branch) outputs.
	sink, err := alpha.NewAddress()
	if err != nil {
		t.Fatalf("alpha.NewAddress: %v", err)
	}
	for i, amt := range []float64{5, 3} {
		coin, err := wA.Send(sink, toSatoshiDash(amt), 10)
		if err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
		if err = mineDashAlpha(); err != nil {
			t.Fatalf("mineDashAlpha spend %d: %v", i, err)
		}
		waitDashTxConfirmed(t, wA, coin.TxID())
	}

	balA, err := wA.Balance()
	if err != nil {
		t.Fatalf("wA.Balance: %v", err)
	}
	want := balA.Available
	t.Logf("wA available after spends (change): %d", want)
	if want == 0 || want >= toSatoshiDash(20) {
		t.Fatalf("unexpected wA available %d; want change 0 < x < 20 DASH", want)
	}
	cmA.Disconnect()

	// Restore from the same seed WITHOUT an explicit recovery-window setting, so
	// recovery is driven by the wallet's built-in default (ChainConfig.Recovery
	// Window) exactly as the app's restore-from-seed flow does. This must still
	// recover the change balance.
	wB, cmB := connectDashElectrumWalletAt(t, t.TempDir(), seed, 0, bday)
	defer cmB.Disconnect()
	waitDashSynced(t, wB)

	balB := waitDashBalanceAtLeast(t, wB, want)
	t.Logf("wB recovered available: %d (want>=%d)", balB.Available, want)
}

// TestDashElectrumInfo verifies Info returns the Dash WalletInfo.
func TestDashElectrumInfo(t *testing.T) {
	w, cm := newDashElectrumWallet(t)
	defer cm.Disconnect()

	info := w.Info()
	if info == nil || info.Name != "Dash" {
		t.Fatalf("Info().Name = %v, want Dash", info)
	}
}

// randBytes returns n cryptographically-random bytes for a fresh wallet seed.
func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}

// TestDashElectrumRecovery funds a run of external addresses from wallet A, then
// a fresh wallet B from the SAME seed with a recovery window must rediscover the
// funds via the gap-limit scan. This exercises the shared electrum recovery path
// under Dash's coin type (m/44'/5') — the legacy-purpose scope fix.
func TestDashElectrumRecovery(t *testing.T) {
	alpha, alphaCM := newDashAlphaBackend(t)
	defer alphaCM.Disconnect()
	unlock(t, alpha)

	seed := randBytes(t, 32)
	bday := time.Now().Add(-time.Hour)

	wA, cmA := connectDashElectrumWalletAt(t, t.TempDir(), seed, 0, bday)
	waitDashSynced(t, wA)

	const nAddrs = 8
	const perAddr = 1 // DASH each
	seen := make(map[string]bool)
	for i := 0; i < nAddrs; i++ {
		addr, err := wA.DepositAddress()
		if err != nil {
			t.Fatalf("DepositAddress %d: %v", i, err)
		}
		if seen[addr] {
			t.Fatalf("DepositAddress did not advance at i=%d (addr %s reused)", i, addr)
		}
		seen[addr] = true
		if _, err := alpha.Send(addr, toSatoshiDash(perAddr), 10); err != nil {
			t.Fatalf("alpha.Send %d: %v", i, err)
		}
		if err := mineDashAlpha(); err != nil {
			t.Fatalf("mineDashAlpha %d: %v", i, err)
		}
		waitDashBalanceAtLeast(t, wA, toSatoshiDash(float64(i+1)*perAddr))
	}
	wantTotal := toSatoshiDash(nAddrs * perAddr)
	cmA.Disconnect()

	wB, cmB := connectDashElectrumWalletAt(t, t.TempDir(), seed, 20, bday)
	defer cmB.Disconnect()
	waitDashSynced(t, wB)

	bal := waitDashBalanceAtLeast(t, wB, wantTotal)
	t.Logf("Dash recovered balance: available=%d (want>=%d across %d addrs)", bal.Available, wantTotal, nAddrs)
}
