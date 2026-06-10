//go:build harness

// These tests run against the Hyperliquid testnet.
//
// Setup:
//  1. Export the private key of your funded testnet account from MetaMask.
//  2. Set HL_TEST_PRIVATE_KEY=<hex_key> before running.
//
// Run with:
//
//	HL_TEST_PRIVATE_KEY=<hex> go test -v -tags harness -run TestLive ./wallet/asset/hyperliquid/
package hyperliquid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
)

const testnetAPI = "https://api.hyperliquid-testnet.xyz"

// testSinkAddress is a known active account on the Hyperliquid testnet with a
// real HYPE spot balance, used as the recipient for outbound test sends.
// Hyperliquid testnet requires the recipient to be an active clearinghouse
// account — zero-balance or EVM-only accounts are rejected.
const testSinkAddress = "0xeffb9c711ec1d3e551a7391dd848290a614c2e63"

var tLogger = util.StdOutLogger("HLTEST", util.LevelTrace)

// createTestWallet opens a wallet from the private key in HL_TEST_PRIVATE_KEY.
// The key must belong to a funded testnet account. The test is skipped if the
// env var is not set.
func createTestWallet(t *testing.T) (*HyperWallet, []byte) {
	t.Helper()

	privKeyHex := strings.TrimPrefix(os.Getenv("HL_TEST_PRIVATE_KEY"), "0x")
	if privKeyHex == "" {
		t.Skip("set HL_TEST_PRIVATE_KEY=<hex_private_key> of a funded testnet account")
	}
	privBytes, err := hex.DecodeString(privKeyHex)
	if err != nil || len(privBytes) != 32 {
		t.Fatalf("invalid HL_TEST_PRIVATE_KEY: want 32-byte hex, got %d bytes", len(privBytes))
	}
	privKey, err := gethcrypto.ToECDSA(privBytes)
	if err != nil {
		t.Fatalf("parsing private key: %v", err)
	}

	dir := t.TempDir()
	pw := make([]byte, 32)
	if _, err := rand.Read(pw); err != nil {
		t.Fatalf("rand.Read pw: %v", err)
	}

	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := saveKeyFile(filepath.Join(dir, keyFileName), privKey, pw); err != nil {
		t.Fatalf("saveKeyFile: %v", err)
	}

	noteChan := make(chan asset.WalletNotification, 128)
	w, err := (&Driver{}).Open(&asset.WalletConfig{
		Type:        "rpc",
		Settings:    map[string]string{"apiendpoint": testnetAPI},
		Emit:        asset.NewWalletEmitter(noteChan, BipID, tLogger),
		PeersChange: func(n uint32, err error) {},
		DataDir:     dir,
	}, tLogger, util.Testnet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return w.(*HyperWallet), pw
}

func TestLiveConnect(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { cancel(); wg.Wait() }()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	addr, err := w.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress: %v", err)
	}
	t.Logf("Wallet address: %s", addr)

	if !w.ValidateAddress(addr) {
		t.Fatal("wallet's own address fails validation")
	}

	owns, err := w.OwnsDepositAddress(addr)
	if err != nil {
		t.Fatalf("OwnsDepositAddress: %v", err)
	}
	if !owns {
		t.Fatal("wallet doesn't own its own address")
	}

	ss, err := w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus: %v", err)
	}
	if !ss.Synced {
		t.Fatal("expected wallet to be synced after connect")
	}
	t.Logf("SyncStatus: synced=%v", ss.Synced)
}

func TestLiveBalance(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { cancel(); wg.Wait() }()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	t.Logf("Balance: available=%d (%.4f HYPE)", bal.Available, float64(bal.Available)/1e8)

	if bal.Available == 0 {
		t.Fatal("expected non-zero balance for funded testnet account")
	}
}

func TestLiveSend(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { cancel(); wg.Wait() }()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	t.Logf("Balance: %d (%.4f HYPE)", bal.Available, float64(bal.Available)/1e8)

	const sendAtoms = uint64(0.1 * 1e8)
	if bal.Available < sendAtoms {
		t.Skipf("insufficient balance: have %.4f HYPE, need 0.1", float64(bal.Available)/1e8)
	}

	coin, err := w.Send(testSinkAddress, sendAtoms, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	t.Logf("Send tx: %s (value=%d)", coin.TxID(), coin.Value())

	if coin.Value() != sendAtoms {
		t.Errorf("coin value = %d, want %d", coin.Value(), sendAtoms)
	}
}

func TestLiveSendLocked(t *testing.T) {
	w, _ := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { cancel(); wg.Wait() }()

	if !w.Locked() {
		t.Fatal("wallet should be locked initially")
	}

	_, err = w.Send("0x1234567890123456789012345678901234567890", 1e8, 0)
	if err == nil {
		t.Fatal("expected error sending while locked")
	}
	t.Logf("Send while locked: %v (expected)", err)
}

func TestLiveTipChange(t *testing.T) {
	w, _ := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	noteChan := make(chan asset.WalletNotification, 128)
	w.emit = asset.NewWalletEmitter(noteChan, BipID, tLogger)

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { cancel(); wg.Wait() }()

	t.Log("Waiting for tip change notification (≤15s)...")
	timeout := time.After(15 * time.Second)
	for {
		select {
		case note := <-noteChan:
			if _, ok := note.(*asset.TipChangeNote); ok {
				t.Logf("Got tip change: %v", note)
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for tip change")
		}
	}
}

func TestLiveCreateExists(t *testing.T) {
	dir := t.TempDir()
	d := &Driver{}

	exists, err := d.Exists("rpc", dir, nil, util.Testnet)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Fatal("wallet should not exist before Create")
	}

	seed := make([]byte, 32)
	rand.Read(seed)
	pw := make([]byte, 32)
	rand.Read(pw)

	if err := d.Create(&asset.CreateWalletParams{
		Type:    "rpc",
		Seed:    seed,
		Pass:    pw,
		DataDir: dir,
		Net:     util.Testnet,
		Logger:  tLogger,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	exists, err = d.Exists("rpc", dir, nil, util.Testnet)
	if err != nil {
		t.Fatalf("Exists after Create: %v", err)
	}
	if !exists {
		t.Fatal("wallet should exist after Create")
	}

	noteChan := make(chan asset.WalletNotification, 8)
	w1, err := d.Open(&asset.WalletConfig{
		Type:        "rpc",
		Settings:    map[string]string{"apiendpoint": testnetAPI},
		Emit:        asset.NewWalletEmitter(noteChan, BipID, tLogger),
		PeersChange: func(uint32, error) {},
		DataDir:     dir,
	}, tLogger, util.Testnet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	hw1 := w1.(*HyperWallet)
	if err := hw1.Unlock(pw); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	addr1, _ := hw1.DepositAddress()
	t.Logf("Address from seed: %s", addr1)

	// Same seed → same address.
	dir2 := t.TempDir()
	d.Create(&asset.CreateWalletParams{
		Type: "rpc", Seed: seed, Pass: pw, DataDir: dir2,
		Net: util.Testnet, Logger: tLogger,
	})
	w2, _ := d.Open(&asset.WalletConfig{
		Type:        "rpc",
		Settings:    map[string]string{"apiendpoint": testnetAPI},
		Emit:        asset.NewWalletEmitter(noteChan, BipID, tLogger),
		PeersChange: func(uint32, error) {},
		DataDir:     dir2,
	}, tLogger, util.Testnet)
	hw2 := w2.(*HyperWallet)
	hw2.Unlock(pw)
	addr2, _ := hw2.DepositAddress()

	if addr1 != addr2 {
		t.Errorf("same seed produced different addresses: %s vs %s", addr1, addr2)
	}

	// Wrong password should fail.
	dir3 := t.TempDir()
	wrongPW := make([]byte, 32)
	rand.Read(wrongPW)
	d.Create(&asset.CreateWalletParams{
		Type: "rpc", Seed: seed, Pass: pw, DataDir: dir3,
		Net: util.Testnet, Logger: tLogger,
	})
	w3, _ := d.Open(&asset.WalletConfig{
		Type:        "rpc",
		Settings:    map[string]string{"apiendpoint": testnetAPI},
		Emit:        asset.NewWalletEmitter(noteChan, BipID, tLogger),
		PeersChange: func(uint32, error) {},
		DataDir:     dir3,
	}, tLogger, util.Testnet)
	hw3 := w3.(*HyperWallet)
	if err := hw3.Unlock(wrongPW); err == nil {
		t.Fatal("expected error unlocking with wrong password")
	}
}

func TestLiveWalletTransaction(t *testing.T) {
	w, pw := createTestWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { cancel(); wg.Wait() }()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	const sendAtoms = uint64(0.1 * 1e8)
	if bal.Available < sendAtoms {
		t.Skipf("insufficient balance: have %.4f HYPE, need 0.1", float64(bal.Available)/1e8)
	}

	coin, err := w.Send(testSinkAddress, sendAtoms, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	txID := coin.TxID()
	t.Logf("Sent tx: %s", txID)

	pending := w.PendingTransactions(ctx)
	if len(pending) == 0 {
		t.Error("expected pending tx after send")
	}

	wtx, err := w.WalletTransaction(ctx, txID)
	if err != nil {
		t.Fatalf("WalletTransaction: %v", err)
	}
	t.Logf("WalletTransaction: type=%d id=%s confirmed=%v", wtx.Type, wtx.ID, wtx.Confirmed)

	t.Log("Waiting for confirmation...")
	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-time.After(3 * time.Second):
			pending = w.PendingTransactions(ctx)
			if len(pending) == 0 {
				t.Log("No more pending txs — checking history...")
				hist, err := w.TxHistory(&asset.TxHistoryRequest{N: 10})
				if err != nil {
					t.Fatalf("TxHistory: %v", err)
				}
				if len(hist.Txs) > 0 {
					t.Logf("TxHistory[0]: type=%d confirmed=%v", hist.Txs[0].Type, hist.Txs[0].Confirmed)
				}
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for confirmation")
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
		t.Fatalf("Connect: %v", err)
	}
	defer func() { cancel(); wg.Wait() }()

	if err := w.Unlock(pw); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	const sendAtoms = uint64(0.1 * 1e8)
	if bal.Available < sendAtoms {
		t.Skipf("insufficient balance: have %.4f HYPE, need 0.1", float64(bal.Available)/1e8)
	}

	// Drain initial notifications.
	time.Sleep(6 * time.Second)
	for len(noteChan) > 0 {
		<-noteChan
	}

	// Hyperliquid rejects self-transfers, so test outgoing send detection instead.
	// The notification path is the same — ledger scan → processLedgerUpdate → emit.
	coin, err := w.Send(testSinkAddress, sendAtoms, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	t.Logf("Sent %s, waiting for send notification...", coin.TxID())

	t.Log("Waiting for send notification...")
	timeout := time.After(30 * time.Second)
	for {
		select {
		case note := <-noteChan:
			txNote, ok := note.(*asset.TransactionNote)
			if !ok {
				continue
			}
			if txNote.Transaction.Type == asset.Send {
				t.Logf("Detected send: id=%s amount=%d (%.4f HYPE)",
					txNote.Transaction.ID, txNote.Transaction.Amount,
					float64(txNote.Transaction.Amount)/1e8)
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for send notification")
		}
	}
}

func TestLiveStandardSendFee(t *testing.T) {
	w := &HyperWallet{}
	if fee := w.StandardSendFee(0); fee != 0 {
		t.Errorf("StandardSendFee = %d, want 0 (HL transfers are feeless)", fee)
	}
}
