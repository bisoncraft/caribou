// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build xmr && harness

package xmr

// Simnet tests expect the XMR test harness to be running:
//
//	cd util/testing/xmr && ./harness.sh
//
// Then, with the monero binaries on PATH:
//
//	LD_LIBRARY_PATH=$(pwd)/wallet/asset/xmr/lib/linux-amd64 \
//	  go test -tags "xmr harness" -v -timeout 30m ./wallet/asset/xmr/
//
// These exercise the send path, which the unit tests cannot reach: building,
// broadcasting and recording a real transaction.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

const (
	simnetDaemon = "http://127.0.0.1:18081"

	// billAddress is the harness wallet that mining rewards go to, used both
	// to fund the wallet under test and as a send destination.
	billAddress = "42xPx5nWhxegefWEzRNoJZWwK7d5ofKoWLG1Gmf8567nJMVR37P1EvqYxqWtfgtYUn8qgSbeAqoLcLKe3seFXV2k5ZSqvQw"

	// Integer, not 1e12, so amounts derived from it stay untyped integers
	// rather than becoming floats.
	atomsPerXMR = 1_000_000_000_000
)

// harnessCtlDir is where harness.sh writes its control scripts, matching the
// other assets' simnet tests.
func harnessCtlDir(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	return filepath.Join(u.HomeDir, "dextest", "xmr", "harness-ctl")
}

// harnessCmd runs a harness control script and returns its stdout.
func harnessCmd(t *testing.T, script string, args ...string) string {
	t.Helper()
	cmd := exec.Command("./"+script, args...)
	cmd.Dir = harnessCtlDir(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", script, args, err, out)
	}
	return string(out)
}

// mineBlocks mines n blocks to bill, advancing the chain so transactions
// confirm and outputs unlock.
func mineBlocks(t *testing.T, n int) {
	t.Helper()
	harnessCmd(t, "mine-to-bill", fmt.Sprintf("%d", n))
}

// fundAddress sends amt atoms from bill to addr and mines enough blocks for
// the output to unlock. Monero locks a received output for 10 blocks.
func fundAddress(t *testing.T, addr string, atoms uint64) {
	t.Helper()
	t.Logf("funding %s with %d atoms", addr, atoms)
	harnessCmd(t, "bill_transfer_to", fmt.Sprintf("%d", atoms), addr)
	mineBlocks(t, 12)
}

// tWallet creates, opens and connects a wallet against the harness daemon,
// returning it with a cleanup that closes it.
func tWallet(t *testing.T) (*ExchangeWallet, context.Context) {
	t.Helper()

	dataDir := t.TempDir()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pass := []byte("abc")
	logger := util.NewLogger("XMRTEST", util.LevelDebug, os.Stdout)

	drv := &Driver{}
	err := drv.Create(&asset.CreateWalletParams{
		Type:     walletTypeNative,
		Seed:     seed,
		Pass:     pass,
		DataDir:  dataDir,
		Net:      util.Simnet,
		Logger:   logger,
		Birthday: 0,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	w, err := drv.Open(&asset.WalletConfig{
		Type:    walletTypeNative,
		DataDir: dataDir,
		Settings: map[string]string{
			"daemonaddress": simnetDaemon,
		},
	}, logger, util.Simnet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	xw := w.(*ExchangeWallet)

	ctx, cancel := context.WithCancel(context.Background())
	if err := xw.OpenWithPW(ctx, pass); err != nil {
		cancel()
		t.Fatalf("OpenWithPW: %v", err)
	}
	wg, err := xw.Connect(ctx)
	if err != nil {
		cancel()
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		xw.Close()
	})

	waitSynced(t, xw)
	return xw, ctx
}

// waitSynced blocks until the wallet reports a completed sync.
func waitSynced(t *testing.T, w *ExchangeWallet) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		ss, err := w.SyncStatus()
		if err != nil {
			t.Fatalf("SyncStatus: %v", err)
		}
		if ss.Synced {
			t.Logf("synced at block %d", ss.Blocks)
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("timed out waiting for sync")
}

// waitBalance blocks until the available balance reaches at least min.
func waitBalance(t *testing.T, w *ExchangeWallet, min uint64) *asset.Balance {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		bal, err := w.Balance()
		if err != nil {
			t.Fatalf("Balance: %v", err)
		}
		if bal.Available >= min {
			return bal
		}
		mineBlocks(t, 1)
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for balance of %d", min)
	return nil
}

// TestSimnetFeeRates checks the live rate against the harness daemon.
func TestSimnetFeeRates(t *testing.T) {
	w, _ := tWallet(t)

	rate := w.FeeRate()
	if rate == 0 {
		t.Fatal("FeeRate returned 0 against a running daemon")
	}
	t.Logf("FeeRate: %d atoms/weight", rate)

	fee := w.StandardSendFee(rate)
	if fee == 0 {
		t.Fatal("StandardSendFee returned 0")
	}
	// The fee is the rate times the assumed weight, rounded up to the
	// daemon's quantization mask — so assert the bracket rather than
	// equality, which only holds when the product already lands on the mask.
	est := w.feeEstimate()
	if est == nil {
		t.Fatal("no fee estimate after a successful FeeRate")
	}
	raw := rate * standardTxWeight
	if fee < raw || fee >= raw+max(est.QuantizationMask, 1) {
		t.Errorf("StandardSendFee = %d, want %d rounded up to a multiple of %d",
			fee, raw, est.QuantizationMask)
	}
	t.Logf("StandardSendFee: %d atoms (%.6f XMR)", fee, float64(fee)/atomsPerXMR)

	// The estimate must be cached rather than refetched on every call.
	if got := w.FeeRate(); got != rate {
		t.Errorf("second FeeRate = %d, want the cached %d", got, rate)
	}
}

// TestSimnetSend funds the wallet, sends, and verifies the transaction is
// recorded. This covers commitTx and persistFee, which no unit test reaches.
func TestSimnetSend(t *testing.T) {
	w, _ := tWallet(t)

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}
	owns, err := w.OwnsDepositAddress(addr)
	if err != nil {
		t.Fatalf("OwnsDepositAddress: %v", err)
	}
	if !owns {
		t.Fatalf("wallet does not own the address it just handed out: %s", addr)
	}

	const funding = 5 * atomsPerXMR
	fundAddress(t, addr, funding)
	bal := waitBalance(t, w, funding/2)
	t.Logf("balance: available=%d immature=%d", bal.Available, bal.Immature)

	const sendAmt = 1 * atomsPerXMR

	// The fee estimate must come back before the send, and be sane.
	estFee, validAddr, err := w.EstimateSendTxFee(billAddress, sendAmt, 0, false, false)
	if err != nil {
		t.Fatalf("EstimateSendTxFee: %v", err)
	}
	if !validAddr {
		t.Fatal("bill's address reported invalid")
	}
	if estFee == 0 {
		t.Fatal("estimated fee is 0")
	}
	t.Logf("estimated fee: %d atoms", estFee)

	coin, err := w.Send(billAddress, sendAmt, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if coin.Value() != sendAmt {
		t.Errorf("coin value = %d, want %d", coin.Value(), sendAmt)
	}
	// A 32 byte ID confirms the txid survived commit. The known failure mode
	// is a multi-transaction sweep returning a comma-joined string, which
	// fails to hex decode and leaves the ID empty.
	if len(coin.ID()) != 32 {
		t.Errorf("coin ID is %d bytes, want 32: %x", len(coin.ID()), coin.ID())
	}
	t.Logf("sent, coin %s", coin.String())

	// The broadcast fee must have been persisted, with the priority it was
	// built at, so StandardSendFee can report it after a restart.
	assertPersistedFee(t, w, estFee)

	mineBlocks(t, 2)
	assertInHistory(t, w, coin.String())
}

// assertPersistedFee checks the meta file recorded a fee at the wallet's
// current priority, in the neighborhood of wantApprox.
func assertPersistedFee(t *testing.T, w *ExchangeWallet, wantApprox uint64) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(w.dataDir, walletMetaFile))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var meta walletMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.LastFee == 0 {
		t.Error("meta has no persisted fee after a send")
	}
	if meta.LastFeePriority != int(w.feePriority) {
		t.Errorf("meta fee priority = %d, want %d", meta.LastFeePriority, w.feePriority)
	}
	// The persisted fee should be the one the pre-send estimate quoted, give
	// or take the inputs coin selection actually picked. A loose bracket
	// rather than equality: the estimate builds its own transaction, so a
	// different input count legitimately moves the fee. What this catches is a
	// fee recorded from the wrong transaction entirely, which would be off by
	// orders of magnitude rather than by an input.
	if wantApprox > 0 && (meta.LastFee > wantApprox*4 || meta.LastFee*4 < wantApprox) {
		t.Errorf("persisted fee %d is not within a factor of 4 of the estimated %d",
			meta.LastFee, wantApprox)
	}
	t.Logf("persisted fee: %d atoms at priority %d", meta.LastFee, meta.LastFeePriority)

	// The in-memory value must agree with what was written.
	if got := w.observedFee.Load(); got != meta.LastFee {
		t.Errorf("observedFee = %d, meta says %d", got, meta.LastFee)
	}

	// A restart must pick the fee back up, and StandardSendFee must fall back
	// to it when no rate is supplied and the daemon cannot be reached.
	var fresh ExchangeWallet
	fresh.log = w.log
	fresh.feePriority = w.feePriority
	fresh.seedFeeFromMeta(&meta)
	if got := fresh.observedFee.Load(); got != meta.LastFee {
		t.Errorf("seedFeeFromMeta loaded %d, want %d", got, meta.LastFee)
	}

	// And it must refuse a fee built at a different priority.
	other := meta
	other.LastFeePriority = int(w.feePriority) + 1
	var mismatched ExchangeWallet
	mismatched.log = w.log
	mismatched.feePriority = w.feePriority
	mismatched.seedFeeFromMeta(&other)
	if got := mismatched.observedFee.Load(); got != 0 {
		t.Errorf("seedFeeFromMeta accepted a fee from priority %d: %d", other.LastFeePriority, got)
	}
}

// assertInHistory checks a transaction appears in the wallet's history.
func assertInHistory(t *testing.T, w *ExchangeWallet, txID string) {
	t.Helper()

	// A nil request must be handled rather than dereferenced.
	if res, err := w.TxHistory(nil); err != nil || res == nil {
		t.Errorf("TxHistory(nil) = %v, %v; want an empty response and no error", res, err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		res, err := w.TxHistory(&txdb.TxHistoryRequest{N: 100})
		if err == nil && res != nil {
			for _, tx := range res.Txs {
				if tx.ID == txID {
					t.Logf("found in history: type=%v amount=%d fees=%d confirmed=%v",
						tx.Type, tx.Amount, tx.Fees, tx.Confirmed)
					return
				}
			}
		}
		mineBlocks(t, 1)
		time.Sleep(2 * time.Second)
	}
	t.Errorf("transaction %s never appeared in history", txID)
}

// TestSimnetWithdraw exercises the sweep path, where Withdraw iterates to
// converge on a fee and may produce a multi-transaction sweep.
func TestSimnetWithdraw(t *testing.T) {
	w, _ := tWallet(t)

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}

	const funding = 3 * atomsPerXMR
	fundAddress(t, addr, funding)
	bal := waitBalance(t, w, funding/2)

	// A max withdraw sends the whole spendable balance, taking the fee from
	// the amount rather than adding it on top.
	coin, err := w.Withdraw(billAddress, bal.Available, 0)
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if coin.Value() == 0 {
		t.Error("withdraw coin has zero value")
	}
	if coin.Value() > bal.Available {
		t.Errorf("withdrew %d, more than the available %d", coin.Value(), bal.Available)
	}
	t.Logf("withdrew %d of %d available, coin %s", coin.Value(), bal.Available, coin.String())

	mineBlocks(t, 2)

	// The balance must actually have gone down.
	after, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if after.Available >= bal.Available {
		t.Errorf("available balance did not decrease: was %d, now %d", bal.Available, after.Available)
	}
	t.Logf("balance after withdraw: available=%d", after.Available)
}
