//go:build harness

// These tests require the NEAR sandbox harness to be running:
//
//   cd dex/testing/near && ./harness.sh
//
// Run with:
//
//   go test -v -tags harness -run TestLive ./client/asset/near/

package near

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/meshwallet/wallet/asset"
	"github.com/bisoncraft/meshwallet/util"
	dexnear "github.com/bisoncraft/meshwallet/util/networks/near"
)

const (
	sandboxRPC = "http://localhost:23456"
)

var (
	tLogger = util.StdOutLogger("NEARTEST", util.LevelTrace)
	tCtx    context.Context
)

// sendFromSandbox sends NEAR from the sandbox's test.near account to a
// recipient using the sendnear tool.
func sendFromSandbox(t *testing.T, recipient string, amountNEAR string) {
	t.Helper()
	homeDir := os.Getenv("HOME")
	sendnear := filepath.Join(homeDir, "dextest", "near", "sendnear")
	cmd := exec.Command(sendnear, sandboxRPC, recipient, amountNEAR)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sendnear failed: %v\n%s", err, out)
	}
	t.Logf("sendnear: %s", out)
}

// createTestWallet creates a new NEAR wallet in a temp directory, returning
// the wallet and its password.
func createTestWallet(t *testing.T) (*NearWallet, []byte) {
	t.Helper()
	dir := t.TempDir()

	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("rand.Read error: %v", err)
	}
	pw := make([]byte, 32)
	if _, err := rand.Read(pw); err != nil {
		t.Fatalf("rand.Read error: %v", err)
	}

	drv := &Driver{}
	err := drv.Create(&asset.CreateWalletParams{
		Type:     walletTypeRPC,
		Seed:     seed,
		Pass:     pw,
		Settings: map[string]string{"rpcprovider": sandboxRPC},
		DataDir:  dir,
		Net:      util.Simnet,
		Logger:   tLogger,
	})
	if err != nil {
		t.Fatalf("Create error: %v", err)
	}

	noteChan := make(chan asset.WalletNotification, 128)
	w, err := drv.Open(&asset.WalletConfig{
		Type:     walletTypeRPC,
		Settings: map[string]string{"rpcprovider": sandboxRPC},
		Emit:     asset.NewWalletEmitter(noteChan, BipID, tLogger),
		PeersChange: func(n uint32, err error) {
			if err != nil {
				tLogger.Errorf("PeersChange error: %v", err)
			}
		},
		DataDir: dir,
	}, tLogger, util.Simnet)
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}

	nw := w.(*NearWallet)
	return nw, pw
}

func TestLiveConnect(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer func() {
		cancel()
		wg.Wait()
	}()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress error: %v", err)
	}
	t.Logf("Wallet address: %s", addr)

	if !w.ValidateAddress(addr) {
		t.Fatalf("wallet's own address failed validation")
	}

	owns, err := w.OwnsDepositAddress(addr)
	if err != nil {
		t.Fatalf("OwnsDepositAddress error: %v", err)
	}
	if !owns {
		t.Fatal("wallet doesn't own its own address")
	}

	ss, err := w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus error: %v", err)
	}
	t.Logf("Sync status: synced=%v blocks=%d", ss.Synced, ss.Blocks)
	if !ss.Synced {
		t.Fatal("expected sandbox to be synced")
	}
	if ss.Blocks == 0 {
		t.Fatal("expected non-zero block height")
	}
}

func TestLiveBalance(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer func() {
		cancel()
		wg.Wait()
	}()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}

	// Before funding: balance should be zero (account doesn't exist yet).
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance error: %v", err)
	}
	t.Logf("Balance before funding: available=%d locked=%d", bal.Available, bal.Locked)
	if bal.Available != 0 {
		t.Fatalf("expected zero balance before funding, got %d", bal.Available)
	}

	// Fund the wallet from the sandbox.
	addr, _ := w.DepositAddress()
	sendFromSandbox(t, addr, "50")

	// Wait a moment for the transaction to be processed.
	time.Sleep(2 * time.Second)

	// After funding: balance should be ~50 NEAR.
	bal, err = w.Balance()
	if err != nil {
		t.Fatalf("Balance error after funding: %v", err)
	}
	t.Logf("Balance after funding: available=%d drops (%.4f NEAR)",
		bal.Available, float64(bal.Available)/1e8)

	// 50 NEAR = 5e9 drops. Allow some margin for storage staking.
	expectedDrops := uint64(50 * 1e8)
	if bal.Available < expectedDrops/2 || bal.Available > expectedDrops*2 {
		t.Fatalf("balance %d drops out of expected range for 50 NEAR", bal.Available)
	}
}

func TestLiveSend(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer func() {
		cancel()
		wg.Wait()
	}()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}

	// Fund the wallet.
	addr, _ := w.DepositAddress()
	sendFromSandbox(t, addr, "100")
	time.Sleep(2 * time.Second)

	// Verify we have funds.
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance error: %v", err)
	}
	t.Logf("Balance before send: %d drops (%.4f NEAR)", bal.Available, float64(bal.Available)/1e8)
	if bal.Available == 0 {
		t.Fatal("wallet has zero balance after funding")
	}

	// Generate a random recipient (implicit account).
	recipientKey := make([]byte, 32)
	if _, err := rand.Read(recipientKey); err != nil {
		t.Fatal(err)
	}
	recipient := hex.EncodeToString(recipientKey)

	// Send 5 NEAR.
	sendDrops := uint64(5 * 1e8) // 5 NEAR in drops
	coin, err := w.Send(recipient, sendDrops, 0)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	t.Logf("Send tx: %s (value=%d drops)", coin.TxID(), coin.Value())

	if coin.Value() != sendDrops {
		t.Errorf("coin value = %d, want %d", coin.Value(), sendDrops)
	}
	if len(coin.ID()) != 32 {
		t.Errorf("coin ID length = %d, want 32", len(coin.ID()))
	}

	// Wait for the async broadcast to be processed on-chain.
	t.Log("Waiting for send to confirm...")
	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-time.After(time.Second):
			recipInfo, err := w.rpc.viewAccount(recipient)
			if err != nil {
				continue // Account might not exist yet.
			}
			recipYocto, ok := parseYoctoNEAR(recipInfo.Amount)
			if !ok {
				continue
			}
			recipDrops := dexnear.YoctoToDrops(recipYocto)
			if recipDrops > 0 {
				t.Logf("Recipient balance: %d drops (%.4f NEAR)", recipDrops, float64(recipDrops)/1e8)
				if recipDrops < sendDrops/2 {
					t.Errorf("recipient balance %d too low, expected ~%d", recipDrops, sendDrops)
				}
				goto sendConfirmed
			}
		case <-timeout:
			t.Fatal("timed out waiting for recipient balance")
		}
	}
sendConfirmed:

	// Verify sender balance decreased.
	balAfter, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance error after send: %v", err)
	}
	t.Logf("Balance after send: %d drops (%.4f NEAR)", balAfter.Available, float64(balAfter.Available)/1e8)
	if balAfter.Available >= bal.Available {
		t.Errorf("balance didn't decrease: before=%d after=%d", bal.Available, balAfter.Available)
	}
}

func TestLiveSendLocked(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer func() {
		cancel()
		wg.Wait()
	}()

	// Don't unlock. Send should fail.
	if !w.Locked() {
		t.Fatal("expected wallet to be locked")
	}

	_, err = w.Send("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 100, 0)
	if err == nil {
		t.Fatal("expected error sending while locked")
	}
	t.Logf("Send while locked: %v (expected)", err)

	// Unlock, lock, then try again.
	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}
	if err := w.Lock(); err != nil {
		t.Fatalf("Lock error: %v", err)
	}

	_, err = w.Send("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 100, 0)
	if err == nil {
		t.Fatal("expected error sending after re-locking")
	}
}

func TestLiveTipChange(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	noteChan := make(chan asset.WalletNotification, 128)
	w.emit = asset.NewWalletEmitter(noteChan, BipID, tLogger)

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer func() {
		cancel()
		wg.Wait()
	}()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}

	// The sandbox produces blocks ~every second. Wait for a tip change.
	t.Log("Waiting for tip change notification...")
	timeout := time.After(15 * time.Second)
	for {
		select {
		case note := <-noteChan:
			if _, ok := note.(*asset.TipChangeNote); ok {
				t.Logf("Got tip change: %v", note)
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for tip change notification")
		}
	}
}

func TestLiveCreateExists(t *testing.T) {
	dir := t.TempDir()
	drv := &Driver{}

	// Should not exist yet.
	exists, err := drv.Exists(walletTypeRPC, dir, nil, util.Simnet)
	if err != nil {
		t.Fatalf("Exists error: %v", err)
	}
	if exists {
		t.Fatal("wallet should not exist yet")
	}

	// Create.
	seed := make([]byte, 32)
	rand.Read(seed)
	pw := make([]byte, 32)
	rand.Read(pw)

	err = drv.Create(&asset.CreateWalletParams{
		Type:    walletTypeRPC,
		Seed:    seed,
		Pass:    pw,
		DataDir: dir,
		Net:     util.Simnet,
		Logger:  tLogger,
	})
	if err != nil {
		t.Fatalf("Create error: %v", err)
	}

	// Should exist now.
	exists, err = drv.Exists(walletTypeRPC, dir, nil, util.Simnet)
	if err != nil {
		t.Fatalf("Exists error: %v", err)
	}
	if !exists {
		t.Fatal("wallet should exist after Create")
	}

	// Open, unlock, verify address is deterministic.
	noteChan := make(chan asset.WalletNotification, 8)
	w, err := drv.Open(&asset.WalletConfig{
		Type:     walletTypeRPC,
		Settings: map[string]string{"rpcprovider": sandboxRPC},
		Emit:     asset.NewWalletEmitter(noteChan, BipID, tLogger),
		PeersChange: func(uint32, error) {},
		DataDir:  dir,
	}, tLogger, util.Simnet)
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}

	nw := w.(*NearWallet)
	if err := nw.Unlock(pw); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}

	addr, _ := nw.DepositAddress()
	t.Logf("Address: %s", addr)

	// Create again with same seed — should get same address.
	dir2 := t.TempDir()
	err = drv.Create(&asset.CreateWalletParams{
		Type:    walletTypeRPC,
		Seed:    seed,
		Pass:    pw,
		DataDir: dir2,
		Net:     util.Simnet,
		Logger:  tLogger,
	})
	if err != nil {
		t.Fatalf("second Create error: %v", err)
	}

	w2, err := drv.Open(&asset.WalletConfig{
		Type:     walletTypeRPC,
		Settings: map[string]string{"rpcprovider": sandboxRPC},
		Emit:     asset.NewWalletEmitter(noteChan, BipID, tLogger),
		PeersChange: func(uint32, error) {},
		DataDir:  dir2,
	}, tLogger, util.Simnet)
	if err != nil {
		t.Fatalf("second Open error: %v", err)
	}

	nw2 := w2.(*NearWallet)
	if err := nw2.Unlock(pw); err != nil {
		t.Fatalf("second Unlock error: %v", err)
	}

	addr2, _ := nw2.DepositAddress()
	if addr != addr2 {
		t.Errorf("same seed produced different addresses: %s vs %s", addr, addr2)
	}

	// Wrong password should fail to unlock.
	dir3 := t.TempDir()
	drv.Create(&asset.CreateWalletParams{
		Type: walletTypeRPC, Seed: seed, Pass: pw, DataDir: dir3, Net: util.Simnet, Logger: tLogger,
	})
	w3, _ := drv.Open(&asset.WalletConfig{
		Type: walletTypeRPC, Settings: map[string]string{"rpcprovider": sandboxRPC},
		Emit: asset.NewWalletEmitter(noteChan, BipID, tLogger), PeersChange: func(uint32, error) {},
		DataDir: dir3,
	}, tLogger, util.Simnet)
	nw3 := w3.(*NearWallet)
	wrongPW := make([]byte, 32)
	rand.Read(wrongPW)
	if err := nw3.Unlock(wrongPW); err == nil {
		t.Fatal("expected error unlocking with wrong password")
	}
}

func TestLiveWalletTransaction(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer func() {
		cancel()
		wg.Wait()
	}()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}

	// Fund.
	addr, _ := w.DepositAddress()
	sendFromSandbox(t, addr, "50")
	time.Sleep(2 * time.Second)

	// Send to get a tx ID.
	recipientKey := make([]byte, 32)
	rand.Read(recipientKey)
	recipient := hex.EncodeToString(recipientKey)

	coin, err := w.Send(recipient, 1e8, 0) // 1 NEAR
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}

	txID := coin.TxID()
	t.Logf("Sent tx: %s", txID)

	// The tx is broadcast asynchronously. It should appear as pending
	// immediately after send.
	pending := w.PendingTransactions(ctx)
	t.Logf("Pending transactions immediately after send: %d", len(pending))
	if len(pending) == 0 {
		t.Error("expected at least 1 pending tx after async send")
	}

	// The tx should be in the local DB.
	wtx, err := w.WalletTransaction(ctx, txID)
	if err != nil {
		t.Fatalf("WalletTransaction error: %v", err)
	}
	t.Logf("WalletTransaction right after send: type=%d id=%s confirmed=%v", wtx.Type, wtx.ID, wtx.Confirmed)
	if wtx.Type != asset.Send {
		t.Errorf("expected Send type, got %d", wtx.Type)
	}

	// Wait for the tip poller to confirm the transaction.
	t.Log("Waiting for tx confirmation via tip poller...")
	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-time.After(tipPollInterval):
			wtx, err = w.WalletTransaction(ctx, txID)
			if err != nil {
				t.Fatalf("WalletTransaction poll error: %v", err)
			}
			if wtx.Confirmed {
				t.Logf("Transaction confirmed at block %d", wtx.BlockNumber)
				goto confirmed
			}
		case <-timeout:
			t.Fatal("timed out waiting for tx confirmation")
		}
	}
confirmed:

	// After confirmation, pending list should be empty.
	pending = w.PendingTransactions(ctx)
	t.Logf("Pending transactions after confirmation: %d", len(pending))
	if len(pending) != 0 {
		t.Errorf("expected 0 pending txs after confirmation, got %d", len(pending))
	}

	// TxHistory should contain the confirmed tx.
	hist, err := w.TxHistory(&asset.TxHistoryRequest{N: 10})
	if err != nil {
		t.Fatalf("TxHistory error: %v", err)
	}
	t.Logf("TxHistory: %d txs", len(hist.Txs))
	if len(hist.Txs) == 0 {
		t.Error("expected at least 1 tx in history after confirmation")
	} else {
		tx0 := hist.Txs[0]
		t.Logf("TxHistory[0]: id=%s type=%d amount=%d confirmed=%v block=%d",
			tx0.ID, tx0.Type, tx0.Amount, tx0.Confirmed, tx0.BlockNumber)
		if tx0.Amount != 1e8 {
			t.Errorf("expected amount 1e8, got %d", tx0.Amount)
		}
		if !tx0.Confirmed {
			t.Error("expected tx to be confirmed in history")
		}
	}
}

func TestLiveReceiveDetection(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	noteChan := make(chan asset.WalletNotification, 128)
	w.emit = asset.NewWalletEmitter(noteChan, BipID, tLogger)

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	defer func() {
		cancel()
		wg.Wait()
	}()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}

	addr, _ := w.DepositAddress()

	// Fund the account so it exists on-chain (required for balance comparison).
	sendFromSandbox(t, addr, "10")
	time.Sleep(3 * time.Second)

	// Wait for the tip poller to pick up the initial funding and advance
	// lastScannedHeight past it.
	time.Sleep(tipPollInterval * 2)

	// Drain any existing notifications.
	for len(noteChan) > 0 {
		<-noteChan
	}

	// Send another deposit. The scanner should detect this as a receive.
	sendFromSandbox(t, addr, "5")

	t.Log("Waiting for receive detection...")
	timeout := time.After(30 * time.Second)
	for {
		select {
		case note := <-noteChan:
			if txNote, ok := note.(*asset.TransactionNote); ok {
				if txNote.Transaction.Type == asset.Receive {
					t.Logf("Detected receive: id=%s amount=%d drops",
						txNote.Transaction.ID, txNote.Transaction.Amount)
					// Verify it's in tx history.
					hist, err := w.TxHistory(&asset.TxHistoryRequest{N: 10})
					if err != nil {
						t.Fatalf("TxHistory error: %v", err)
					}
					found := false
					for _, tx := range hist.Txs {
						if tx.ID == txNote.Transaction.ID {
							found = true
							if tx.Type != asset.Receive {
								t.Errorf("expected Receive type, got %d", tx.Type)
							}
							break
						}
					}
					if !found {
						t.Error("receive tx not found in TxHistory")
					}
					return
				}
			}
		case <-timeout:
			t.Fatal("timed out waiting for receive detection")
		}
	}
}

func TestLiveStandardSendFee(t *testing.T) {
	w := &NearWallet{}
	fee := w.StandardSendFee(0)
	t.Logf("StandardSendFee: %d drops (%.6f NEAR)", fee, float64(fee)/1e8)
	if fee == 0 {
		t.Error("expected non-zero fee")
	}
}

func TestLiveInfo(t *testing.T) {
	drv := &Driver{}
	info := drv.Info()
	if info.Name != "NEAR Protocol" {
		t.Errorf("Name = %q, want %q", info.Name, "NEAR Protocol")
	}
	if info.UnitInfo.Conventional.ConversionFactor != 1e8 {
		t.Errorf("ConversionFactor = %v, want 1e8", info.UnitInfo.Conventional.ConversionFactor)
	}
	if len(info.AvailableWallets) == 0 {
		t.Error("no available wallets")
	}
	t.Logf("Info: %s, unit=%s, conventional=%s (factor=%v)",
		info.Name, info.UnitInfo.AtomicUnit,
		info.UnitInfo.Conventional.Unit, info.UnitInfo.Conventional.ConversionFactor)

	// Decode a coin ID.
	coinID := make([]byte, 32)
	for i := range coinID {
		coinID[i] = byte(i)
	}
	decoded, err := drv.DecodeCoinID(coinID)
	if err != nil {
		t.Fatalf("DecodeCoinID error: %v", err)
	}
	if decoded == "" {
		t.Error("DecodeCoinID returned empty string")
	}
	t.Logf("DecodeCoinID: %s", decoded)

	_, err = drv.DecodeCoinID([]byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for short coin ID")
	}
}

// Ensure unused imports don't cause issues.
var _ = fmt.Sprint
