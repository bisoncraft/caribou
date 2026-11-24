// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build zec && cgo && harness

// These tests run the asset wallet against the Zcash simnet harness,
// util/testing/zec, which has to be running. They create wallets through the
// Driver, keep their history in a real NetworkTxDB, use the alpha node's
// light-client gRPC server and the control scripts in
// ~/dextest/zec/harness-ctl, mine blocks, spend from the alpha funder, and
// reorganize the chain by a few blocks.

package zec

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
	"github.com/bisoncraft/caribou/wallet/txdb"
)

const (
	// harnessTimeout bounds each wait for a wallet to reach a state.
	harnessTimeout = 2 * time.Minute
	zatsPerZEC     = 100_000_000
)

var harnessNetwork = zeclib.Regtest(zeclib.AllUpgradesAt(1))

// harnessCtlMtx serializes use of the control scripts. The funder wallet
// they spend from cannot be used by two processes at once.
var harnessCtlMtx sync.Mutex

func harnessDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home directory: %v", err)
	}
	return filepath.Join(home, "dextest", "zec")
}

// harnessCtl runs a harness control script and returns its trimmed output.
func harnessCtl(t *testing.T, script string, args ...string) string {
	t.Helper()
	dir := filepath.Join(harnessDir(t), "harness-ctl")
	if _, err := os.Stat(filepath.Join(dir, script)); err != nil {
		t.Fatalf("harness script not found (is the zec harness running?): %v", err)
	}
	harnessCtlMtx.Lock()
	defer harnessCtlMtx.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "./"+script, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", script, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func harnessHeight(t *testing.T) uint32 {
	t.Helper()
	out := harnessCtl(t, "alpha", "getblockcount")
	height, err := strconv.ParseUint(out, 10, 32)
	if err != nil {
		t.Fatalf("getblockcount returned %q", out)
	}
	return uint32(height)
}

func harnessMine(t *testing.T, n int) {
	t.Helper()
	harnessCtl(t, "mine-alpha", strconv.Itoa(n))
}

// harnessSend pays an address from the alpha funder, with a memo if one is
// given, and returns the transaction ID.
func harnessSend(t *testing.T, address string, zats uint64, memo ...string) string {
	t.Helper()
	args := []string{address, fmt.Sprintf("%d.%08d", zats/zatsPerZEC, zats%zatsPerZEC)}
	args = append(args, memo...)
	out := harnessCtl(t, "send-alpha", args...)
	lines := strings.Split(out, "\n")
	txid := strings.TrimSpace(lines[len(lines)-1])
	if len(txid) != 64 {
		t.Fatalf("send-alpha did not end with a transaction ID:\n%s", out)
	}
	return txid
}

// fastPolling makes the zeclib wallets of the test poll the harness often.
func fastPolling(t *testing.T) {
	t.Helper()
	prev := openBackend
	openBackend = func(cfg *zeclib.Config, seed []byte) (backend, error) {
		cfg.PollInterval = 250 * time.Millisecond
		return prev(cfg, seed)
	}
	t.Cleanup(func() { openBackend = prev })
}

// testLogWriter writes log lines to the test log until the test ends.
type testLogWriter struct {
	t    *testing.T
	mtx  sync.Mutex
	done bool
}

func (w *testLogWriter) Write(line []byte) (int, error) {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	if !w.done {
		w.t.Log(strings.TrimSuffix(string(line), "\n"))
	}
	return len(line), nil
}

func newTestLogger(t *testing.T, name string) util.Logger {
	w := &testLogWriter{t: t}
	t.Cleanup(func() {
		w.mtx.Lock()
		w.done = true
		w.mtx.Unlock()
	})
	return util.NewLogger(name, util.LevelDebug, w)
}

func randomSeed(t *testing.T) []byte {
	t.Helper()
	seed := make([]byte, entropyLen)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	return seed
}

// harnessWallet is an asset wallet created through the Driver on simnet,
// with its own NetworkTxDB.
type harnessWallet struct {
	t     *testing.T
	name  string
	dir   string
	seed  []byte
	ntx   txdb.NetworkTxDB
	notes chan asset.WalletNotification
	w     *zecWallet
	stop  func()
}

// createHarnessWallet creates a wallet of seed in a new data directory and
// opens, unlocks and connects it.
func createHarnessWallet(t *testing.T, name string, seed []byte) *harnessWallet {
	t.Helper()
	fastPolling(t)
	hw := &harnessWallet{t: t, name: name, dir: t.TempDir(), seed: seed,
		notes: make(chan asset.WalletNotification, 4096)}
	err := (&Driver{}).Create(&asset.CreateWalletParams{
		Type: walletTypeNative, Seed: seed, Pass: testPW, DataDir: hw.dir, Net: util.Simnet,
		Birthday: uint64(time.Now().Unix()), Logger: newTestLogger(t, name+"-create"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	db, err := txdb.New(filepath.Join(t.TempDir(), "txdb"), util.Disabled)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hw.ntx = db.NetworkTxDB(BipID)
	hw.start()
	return hw
}

// start builds an instance through the Driver, opens it with the password,
// unlocks and connects it.
func (hw *harnessWallet) start() {
	t := hw.t
	t.Helper()
	aw, err := (&Driver{}).Open(&asset.WalletConfig{
		Type:        walletTypeNative,
		DataDir:     hw.dir,
		Emit:        asset.NewWalletEmitter(hw.notes, BipID, util.Disabled),
		PeersChange: func(uint32, error) {},
		NetworkTxDB: hw.ntx,
	}, newTestLogger(t, hw.name), util.Simnet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	hw.w = aw.(*zecWallet)
	if err := hw.w.OpenWithPW(context.Background(), testPW); err != nil {
		t.Fatalf("OpenWithPW: %v", err)
	}
	if err := hw.w.Unlock(testPW); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wg, err := hw.w.Connect(ctx)
	if err != nil {
		cancel()
		t.Fatalf("Connect: %v", err)
	}
	w := hw.w
	var once sync.Once
	hw.stop = func() {
		once.Do(func() {
			cancel()
			wg.Wait()
			if err := w.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
	t.Cleanup(hw.stop)
}

// waitFor waits for ok.
func (hw *harnessWallet) waitFor(what string, ok func() bool) {
	hw.t.Helper()
	deadline := time.Now().Add(harnessTimeout)
	for !ok() {
		if time.Now().After(deadline) {
			ss, _ := hw.w.SyncStatus()
			bal, _ := hw.w.Balance()
			hw.t.Fatalf("%s: timed out waiting for %s; status %+v, balance %+v", hw.name, what, ss, bal)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitSynced waits until the wallet is synced to the harness tip.
func (hw *harnessWallet) waitSynced() {
	hw.t.Helper()
	tip := uint64(harnessHeight(hw.t))
	hw.waitFor(fmt.Sprintf("sync to %d", tip), func() bool {
		ss, err := hw.w.SyncStatus()
		return err == nil && ss.Synced && ss.TargetHeight >= tip
	})
}

func (hw *harnessWallet) available() uint64 {
	b, err := hw.w.Balance()
	if err != nil {
		return 0
	}
	return b.Available
}

// waitAvailable waits for the available balance to be v.
func (hw *harnessWallet) waitAvailable(v uint64) {
	hw.t.Helper()
	hw.waitFor(fmt.Sprintf("an available balance of %d", v), func() bool { return hw.available() == v })
}

func (hw *harnessWallet) address() string {
	hw.t.Helper()
	a, err := hw.w.DepositAddress()
	if err != nil {
		hw.t.Fatal(err)
	}
	return a
}

// transparentAddress returns the transparent receiver of the deposit
// address.
func (hw *harnessWallet) transparentAddress() string {
	hw.t.Helper()
	return transparentReceiver(hw.t, hw.address())
}

func transparentReceiver(t *testing.T, unified string) string {
	t.Helper()
	p, err := zeclib.ParseAddress(harnessNetwork, unified)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Receivers {
		if r.Kind == zeclib.ReceiverTransparent {
			return r.Address
		}
	}
	t.Fatalf("%s has no transparent receiver", unified)
	return ""
}

// record returns the wallet's record of a transaction, nil if none.
func (hw *harnessWallet) record(txid string) *txdb.WalletTransaction {
	hw.t.Helper()
	wt, err := hw.ntx.GetTx(txid)
	if err != nil {
		hw.t.Fatal(err)
	}
	return wt
}

// waitRecord waits for a record of the transaction for which ok holds.
func (hw *harnessWallet) waitRecord(txid string, what string, ok func(*txdb.WalletTransaction) bool) *txdb.WalletTransaction {
	hw.t.Helper()
	var wt *txdb.WalletTransaction
	hw.waitFor(fmt.Sprintf("the record of %s %s", txid, what), func() bool {
		wt = hw.record(txid)
		return wt != nil && ok(wt)
	})
	return wt
}

// history returns every record of the wallet.
func (hw *harnessWallet) history() []*txdb.WalletTransaction {
	hw.t.Helper()
	res, err := hw.w.TxHistory(&txdb.TxHistoryRequest{N: 1000})
	if err != nil {
		hw.t.Fatal(err)
	}
	return res.Txs
}

// newNotes returns how many transaction notes with New set each transaction
// got since the last call.
func (hw *harnessWallet) newNotes() map[string]int {
	out := make(map[string]int)
	for {
		select {
		case n := <-hw.notes:
			if tn, ok := n.(*asset.TransactionNote); ok && tn.New {
				out[tn.Transaction.ID]++
			}
		default:
			return out
		}
	}
}

// orchardReceiver returns the Orchard-only form of a unified address, the
// receiver a shielded payment to it lands in, as the history records it.
func orchardReceiver(t *testing.T, unified string) string {
	t.Helper()
	p, err := zeclib.ParseAddress(harnessNetwork, unified)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Receivers {
		if r.Kind == zeclib.ReceiverOrchard {
			return r.Address
		}
	}
	t.Fatalf("%s has no Orchard receiver", unified)
	return ""
}

// checkReceiveRecipient checks that the recipient of a Receive is the
// receiver the test paid: the Orchard-only form for a shielded payment, the
// t-address for a transparent one. It is the wallet's, by the rule of
// OwnsDepositAddress.
func (hw *harnessWallet) checkReceiveRecipient(wt *txdb.WalletTransaction, want string) {
	hw.t.Helper()
	if wt.Recipient == nil || *wt.Recipient != want {
		hw.t.Errorf("the recipient of %s is %v, want %s", wt.ID, wt.Recipient, want)
		return
	}
	if owned, err := hw.w.OwnsDepositAddress(want); err != nil || !owned {
		hw.t.Errorf("the recipient of %s, %s, is not the wallet's: %v", wt.ID, want, err)
	}
}

// checkNotes checks that each of the wanted transactions was reported new
// exactly once since the last call, and no other.
func (hw *harnessWallet) checkNotes(want ...string) {
	hw.t.Helper()
	got := hw.newNotes()
	for _, id := range want {
		if got[id] != 1 {
			hw.t.Errorf("%s was reported new %d times, want once", id, got[id])
		}
		delete(got, id)
	}
	for id, n := range got {
		hw.t.Errorf("%s was reported new %d times, unexpectedly", id, n)
	}
}

// checkRecord checks a record's type, amount, fee and recipient.
func checkRecord(t *testing.T, what string, wt *txdb.WalletTransaction, typ txdb.TransactionType, amount, fee uint64, recipient string) {
	t.Helper()
	if wt == nil {
		t.Errorf("%s: no record", what)
		return
	}
	var got string
	if wt.Recipient != nil {
		got = *wt.Recipient
	}
	if wt.Type != typ || wt.Amount != amount || wt.Fees != fee || got != recipient {
		t.Errorf("%s: type %d amount %d fee %d recipient %q; want type %d amount %d fee %d recipient %q",
			what, wt.Type, wt.Amount, wt.Fees, got, typ, amount, fee, recipient)
	}
}

// balance returns the wallet's balance.
func (hw *harnessWallet) balance() asset.Balance {
	hw.t.Helper()
	b, err := hw.w.Balance()
	if err != nil {
		hw.t.Fatal(err)
	}
	return *b
}

// bystander returns a unified address that no wallet under test follows.
func bystander(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := (&Driver{}).Create(&asset.CreateWalletParams{
		Type: walletTypeNative, Seed: randomSeed(t), Pass: testPW, DataDir: dir, Net: util.Simnet,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	aw, err := (&Driver{}).Open(&asset.WalletConfig{Type: walletTypeNative, DataDir: dir}, nil, util.Simnet)
	if err != nil {
		t.Fatal(err)
	}
	w := aw.(*zecWallet)
	if err := w.OpenWithPW(context.Background(), testPW); err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	a, err := w.DepositAddress()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// fund pays the wallet shielded and transparent amounts from the funder and
// mines until they are spendable.
func (hw *harnessWallet) fund(shielded, transparent uint64, memo ...string) (shieldedTx, transparentTx string) {
	hw.t.Helper()
	want := hw.available()
	if shielded > 0 {
		shieldedTx = harnessSend(hw.t, hw.address(), shielded, memo...)
		want += shielded
	}
	if transparent > 0 {
		transparentTx = harnessSend(hw.t, hw.transparentAddress(), transparent)
		want += transparent
	}
	// ZIP 315: received outputs are spendable at 10 confirmations.
	harnessMine(hw.t, finalConfirmations)
	hw.waitAvailable(want)
	return shieldedTx, transparentTx
}

// TestHarnessReceiveSendAndHistory creates a wallet through the Driver,
// receives shielded and transparent payments, sends to a unified address and
// a transparent address of another wallet and to three addresses of its own
// (its deposit address, its transparent receiver and its Orchard receiver),
// and reads the records in the NetworkTxDB, also through WalletTransaction:
// types, amounts, fees and recipients when Send returns, after the scan and
// at finality, a memo, and one new-transaction note for each.
func TestHarnessReceiveSendAndHistory(t *testing.T) {
	hw := createHarnessWallet(t, "sender", randomSeed(t))
	hw.waitSynced()
	other := bystander(t)
	ctx := context.Background()

	// The receive view asks for an address each time it opens: until the
	// address has received, it is the same one.
	first := hw.address()
	for i := range 12 {
		if a, err := hw.w.NewAddress(); err != nil || a != first {
			t.Fatalf("NewAddress call %d before any payment: %q, %v", i, a, err)
		}
	}

	const memo = "harness memo"
	shieldedTx, transparentTx := hw.fund(100_000_000, 50_000_000, memo)
	r := hw.waitRecord(shieldedTx, "final", func(wt *txdb.WalletTransaction) bool { return wt.Confirmed })
	checkRecord(t, "the shielded receive", r, txdb.Receive, 100_000_000, 0, orchardReceiver(t, hw.address()))
	if r.AdditionalData["memo"] != memo || r.Confirms.Current != finalConfirmations {
		t.Errorf("the shielded receive: %+v, data %v", r, r.AdditionalData)
	}
	hw.checkReceiveRecipient(r, orchardReceiver(t, hw.address()))
	r = hw.waitRecord(transparentTx, "final", func(wt *txdb.WalletTransaction) bool { return wt.Confirmed })
	checkRecord(t, "the transparent receive", r, txdb.Receive, 50_000_000, 0, hw.transparentAddress())
	hw.checkReceiveRecipient(r, hw.transparentAddress())

	type sent struct {
		name   string
		to     string
		value  uint64
		typ    txdb.TransactionType
		fee    uint64
		coinID string
	}
	sends := []*sent{
		{name: "to another wallet's unified address", to: other, value: 10_000_000, typ: txdb.Send},
		{name: "to another wallet's transparent address", to: transparentReceiver(t, other), value: 5_000_000, typ: txdb.Send},
		{name: "to the own deposit address", to: hw.address(), value: 20_000_000, typ: txdb.SelfSend},
		{name: "to the own transparent receiver", to: hw.transparentAddress(), value: 3_000_000, typ: txdb.SelfSend},
		{name: "to the own Orchard receiver", to: orchardReceiver(t, hw.address()), value: 4_000_000, typ: txdb.SelfSend},
	}
	for _, s := range sends {
		before := hw.available()
		fee, valid, err := hw.w.EstimateSendTxFee(s.to, s.value, feeRate, false, false)
		if err != nil || !valid {
			t.Fatalf("EstimateSendTxFee %s: %d %t %v", s.name, fee, valid, err)
		}
		s.fee = fee
		coin, err := hw.w.Send(s.to, s.value, feeRate)
		if err != nil {
			t.Fatalf("Send %s: %v", s.name, err)
		}
		s.coinID = coin.TxID()
		// The record is written before Send returns, and read as the
		// Intents engine reads it.
		checkRecord(t, s.name+" when Send returned", hw.record(s.coinID), s.typ, s.value, fee, s.to)
		wt, err := hw.w.WalletTransaction(ctx, s.coinID)
		if err != nil {
			t.Fatalf("WalletTransaction %s: %v", s.name, err)
		}
		checkRecord(t, s.name+" through WalletTransaction", wt, s.typ, s.value, fee, s.to)
		// Change of the wallet's own transactions is spendable at 3
		// confirmations; a payment to its own address, like any received
		// output, at 10.
		mine, want := 3, before-s.value-fee
		if s.typ == txdb.SelfSend {
			mine, want = finalConfirmations, before-fee
		}
		harnessMine(t, mine)
		hw.waitAvailable(want)
		wt = hw.waitRecord(s.coinID, "mined", func(wt *txdb.WalletTransaction) bool { return wt.BlockNumber > 0 })
		checkRecord(t, s.name+" after the scan", wt, s.typ, s.value, fee, s.to)
	}
	harnessMine(t, finalConfirmations)
	for _, s := range sends {
		wt := hw.waitRecord(s.coinID, "final", func(wt *txdb.WalletTransaction) bool {
			return wt.Confirmed && wt.Confirms.Current == finalConfirmations
		})
		checkRecord(t, s.name+" at finality", wt, s.typ, s.value, s.fee, s.to)
	}
	want := []string{shieldedTx, transparentTx}
	for _, s := range sends {
		want = append(want, s.coinID)
	}
	hw.checkNotes(want...)
	if pending := hw.w.PendingTransactions(ctx); len(pending) != 0 {
		t.Errorf("%d pending after finality", len(pending))
	}
	if n := len(hw.history()); n != len(want) {
		t.Errorf("%d records, want %d", n, len(want))
	}

	// The address has received: the next call gives a new one, with both
	// receivers new.
	next, err := hw.w.NewAddress()
	if err != nil || next == first {
		t.Fatalf("NewAddress after the receives: %q, %v", next, err)
	}
	if orchardReceiver(t, next) == orchardReceiver(t, first) || transparentReceiver(t, next) == transparentReceiver(t, first) {
		t.Errorf("the new address %s shares a receiver with %s", next, first)
	}
}

// TestHarnessWithdraw withdraws a part of the balance, with the fee taken
// from it, and then the whole balance.
func TestHarnessWithdraw(t *testing.T) {
	hw := createHarnessWallet(t, "withdrawer", randomSeed(t))
	hw.waitSynced()
	other := bystander(t)
	hw.fund(60_000_000, 40_000_000)

	const part = 30_000_000
	fee, _, err := hw.w.EstimateSendTxFee(other, part, feeRate, true, false)
	if err != nil {
		t.Fatalf("EstimateSendTxFee of a part: %v", err)
	}
	coin, err := hw.w.Withdraw(other, part, feeRate)
	if err != nil {
		t.Fatalf("Withdraw of a part: %v", err)
	}
	if coin.Value() != part-fee {
		t.Errorf("the recipient gets %d, want %d", coin.Value(), part-fee)
	}
	checkRecord(t, "the partial withdrawal", hw.record(coin.TxID()), txdb.Send, part-fee, fee, other)
	harnessMine(t, 3)
	hw.waitAvailable(100_000_000 - part)

	all := hw.available()
	fee, _, err = hw.w.EstimateSendTxFee(other, all, feeRate, true, false)
	if err != nil {
		t.Fatalf("EstimateSendTxFee of everything: %v", err)
	}
	coin, err = hw.w.Withdraw(other, all, feeRate)
	if err != nil {
		t.Fatalf("Withdraw of everything: %v", err)
	}
	if coin.Value() != all-fee {
		t.Errorf("the recipient gets %d of %d, fee %d", coin.Value(), all, fee)
	}
	checkRecord(t, "the withdrawal of everything", hw.record(coin.TxID()), txdb.Send, all-fee, fee, other)
	harnessMine(t, 3)
	hw.waitFor("an empty wallet", func() bool {
		b, err := hw.w.Balance()
		return err == nil && b.Available == 0 && b.Immature == 0 && b.Locked == 0
	})
}

// TestHarnessLockAndRestart sends only while unlocked, and keeps its balance
// and history across a restart: Close, a new instance through the Driver,
// OpenWithPW and Connect.
func TestHarnessLockAndRestart(t *testing.T) {
	hw := createHarnessWallet(t, "restarter", randomSeed(t))
	hw.waitSynced()
	other := bystander(t)
	hw.fund(50_000_000, 0)

	if err := hw.w.Lock(); err != nil {
		t.Fatal(err)
	}
	if _, err := hw.w.Send(other, 1_000_000, feeRate); !errors.Is(err, errLocked) {
		t.Fatalf("a send while locked: %v", err)
	}
	if err := hw.w.Unlock(testPW); err != nil {
		t.Fatal(err)
	}
	fee, _, err := hw.w.EstimateSendTxFee(other, 1_000_000, feeRate, false, false)
	if err != nil {
		t.Fatal(err)
	}
	coin, err := hw.w.Send(other, 1_000_000, feeRate)
	if err != nil {
		t.Fatalf("a send after Unlock: %v", err)
	}
	harnessMine(t, 3)
	hw.waitRecord(coin.TxID(), "mined", func(wt *txdb.WalletTransaction) bool { return wt.BlockNumber > 0 })
	balance := 50_000_000 - 1_000_000 - fee
	hw.waitAvailable(balance)
	records := len(hw.history())
	hw.newNotes()

	hw.stop()
	if hw.w.IsOpen() {
		t.Fatal("open after the stop")
	}
	harnessMine(t, 2)
	hw.start()
	hw.waitSynced()
	hw.waitAvailable(balance)
	if n := len(hw.history()); n != records {
		t.Errorf("%d records after the restart, want %d", n, records)
	}
	hw.checkNotes()
	if _, err := hw.w.Send(other, 1_000_000, feeRate); err != nil {
		t.Errorf("a send after the restart: %v", err)
	}
}

// TestHarnessRestore creates a second wallet from the seed of one that
// received, sent to another wallet and paid itself: it finds the same
// balance in every component, and the Send and the SelfSend.
func TestHarnessRestore(t *testing.T) {
	seed := randomSeed(t)
	first := createHarnessWallet(t, "original", seed)
	first.waitSynced()
	other := bystander(t)
	shieldedTx, transparentTx := first.fund(30_000_000, 20_000_000)

	sendFee, _, err := first.w.EstimateSendTxFee(other, 7_000_000, feeRate, false, false)
	if err != nil {
		t.Fatal(err)
	}
	sendCoin, err := first.w.Send(other, 7_000_000, feeRate)
	if err != nil {
		t.Fatalf("Send to another wallet: %v", err)
	}
	harnessMine(t, 3)
	first.waitRecord(sendCoin.TxID(), "mined", func(wt *txdb.WalletTransaction) bool { return wt.BlockNumber > 0 })
	selfFee, _, err := first.w.EstimateSendTxFee(first.address(), 6_000_000, feeRate, false, false)
	if err != nil {
		t.Fatal(err)
	}
	selfCoin, err := first.w.Send(first.address(), 6_000_000, feeRate)
	if err != nil {
		t.Fatalf("Send to itself: %v", err)
	}
	harnessMine(t, finalConfirmations)
	first.waitRecord(selfCoin.TxID(), "final", func(wt *txdb.WalletTransaction) bool { return wt.Confirmed })
	first.waitAvailable(50_000_000 - 7_000_000 - sendFee - selfFee)
	want := first.balance()
	firstAddress := first.address()
	first.stop()

	restored := createHarnessWallet(t, "restored", seed)
	restored.waitSynced()
	restored.waitFor("the original's balance", func() bool {
		b, err := restored.w.Balance()
		return err == nil && b.Available == want.Available && b.Immature == want.Immature && b.Locked == want.Locked
	})
	r := restored.waitRecord(shieldedTx, "final", func(wt *txdb.WalletTransaction) bool { return wt.Confirmed })
	restored.checkReceiveRecipient(r, orchardReceiver(t, restored.address()))
	r = restored.waitRecord(transparentTx, "final", func(wt *txdb.WalletTransaction) bool { return wt.Confirmed })
	restored.checkReceiveRecipient(r, restored.transparentAddress())
	// The restored wallet did not create these: it knows the recipient of
	// the send as the receiver it paid, and its own receiver of the
	// self-payment as a received output.
	wt := restored.waitRecord(sendCoin.TxID(), "final", func(wt *txdb.WalletTransaction) bool { return wt.Confirmed })
	checkRecord(t, "the restored send", wt, txdb.Send, 7_000_000, sendFee, orchardReceiver(t, other))
	wt = restored.waitRecord(selfCoin.TxID(), "final", func(wt *txdb.WalletTransaction) bool { return wt.Confirmed })
	checkRecord(t, "the restored self-payment", wt, txdb.SelfSend, 6_000_000, selfFee, orchardReceiver(t, firstAddress))
}

// TestHarnessReorgRewritesARecord: a payment in a block that a
// reorganization replaces, and that cannot be mined again, is rewritten
// unmined, and rejected once it has expired.
func TestHarnessReorgRewritesARecord(t *testing.T) {
	hw := createHarnessWallet(t, "reorged", randomSeed(t))
	hw.waitSynced()

	// The payment is built on a block whose coinbase goes into the
	// Ironwood pool, and that block is replaced as well: the payment's
	// anchor never exists on the new chain.
	harnessCtl(t, "mine-to", bystander(t), "1")
	const dropped = 7_000_000
	txid := harnessSend(t, hw.address(), dropped)
	harnessMine(t, 1)
	before := hw.waitRecord(txid, "mined", func(wt *txdb.WalletTransaction) bool { return wt.BlockNumber > 0 })
	if before.Type != txdb.Receive || before.Amount != dropped {
		t.Fatalf("the record before the reorganization: %+v", before)
	}
	hw.newNotes()

	harnessCtl(t, "reorg", "2")
	after := hw.waitRecord(txid, "unmined", func(wt *txdb.WalletTransaction) bool { return wt.BlockNumber == 0 })
	if after.Confirmed || after.Rejected {
		t.Errorf("the record after the reorganization: %+v", after)
	}
	if pending := hw.w.PendingTransactions(context.Background()); len(pending) != 1 || pending[0].ID != txid {
		t.Errorf("pending after the reorganization: %d", len(pending))
	}
	if n := hw.newNotes()[txid]; n != 0 {
		t.Errorf("the rewritten record was reported new %d times", n)
	}

	// Past its expiry height, 40 blocks after it was made.
	harnessMine(t, 45)
	hw.waitRecord(txid, "rejected", func(wt *txdb.WalletTransaction) bool { return wt.Rejected && wt.Confirmed })
}

// TestHarnessDevtoolRestore restores the RestorationInfo phrase in
// zcash-devtool, which finds the wallet's funds.
func TestHarnessDevtoolRestore(t *testing.T) {
	seed := randomSeed(t)
	hw := createHarnessWallet(t, "phrase", seed)
	hw.waitSynced()
	hw.fund(40_000_000, 10_000_000)

	rs, err := hw.w.RestorationInfo(seed)
	if err != nil {
		t.Fatal(err)
	}
	phrase := rs[0].Seed
	birthday := strconv.FormatUint(uint64(hw.w.account.Birthday), 10)
	if !strings.Contains(rs[0].Instructions, "--birthday "+birthday) {
		t.Errorf("the instructions do not give the birthday %s: %s", birthday, rs[0].Instructions)
	}

	dir := t.TempDir()
	devtool := func(stdin string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "zcash-devtool", append([]string{"wallet", "-w", dir}, args...)...)
		cmd.Env = append(os.Environ(), "RUST_LOG=warn")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("zcash-devtool %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	server := []string{"-s", simnetServer, "--connection", "direct"}
	devtool(phrase+"\n", append([]string{"init", "--name", "restored", "-i", filepath.Join(dir, "age-identity.txt"),
		"--birthday", birthday, "-n", "regtest", "--activation-heights",
		filepath.Join(harnessDir(t), "activation-heights.toml")}, server...)...)
	devtool("", append([]string{"sync"}, server...)...)
	// balance --json prints one line, a flat object whose "total" is the
	// account's total in zatoshis (zcash-devtool 0b3436b,
	// src/commands/wallet/balance.rs).
	out := devtool("", "balance", "--json")
	line := strings.TrimSpace(out[strings.LastIndex(strings.TrimSpace(out), "\n")+1:])
	var balance struct {
		Total *uint64 `json:"total"`
	}
	if err := json.Unmarshal([]byte(line), &balance); err != nil || balance.Total == nil {
		t.Fatalf("balance --json: %v\n%s", err, out)
	}
	if *balance.Total != 50_000_000 {
		t.Errorf("zcash-devtool finds %d zatoshis, want 50,000,000:\n%s", *balance.Total, out)
	}
}
