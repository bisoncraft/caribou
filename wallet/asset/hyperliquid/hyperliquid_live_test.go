//go:build harness

// These tests run against the Hyperliquid testnet, or against the local
// Hyperliquid mock server (see util/testing/hyperliquid_mock) when
// HL_API_ENDPOINT is set.
//
// Setup (real testnet):
//  1. Export the private key of your funded testnet account from MetaMask.
//  2. Set HL_TEST_PRIVATE_KEY=<hex_key> before running.
//
// Run with:
//
//	HL_TEST_PRIVATE_KEY=<hex> go test -v -tags harness -run TestLive ./wallet/asset/hyperliquid/
//
// Setup (mock server):
//  1. Start the mock: ./run_hype_mock_harness.sh
//  2. Set HL_API_ENDPOINT=http://127.0.0.1:18080 and
//     HL_WS_ENDPOINT=ws://127.0.0.1:18080/ws before running. HL_TEST_PRIVATE_KEY
//     is not needed — a fresh key is generated and funded via the mock's
//     /test/fund endpoint.
//
// Run with:
//
//	HL_API_ENDPOINT=http://127.0.0.1:18080 HL_WS_ENDPOINT=ws://127.0.0.1:18080/ws \
//	  go test -v -tags harness -run TestLive ./wallet/asset/hyperliquid/
package hyperliquid

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
)

const defaultTestnetAPI = "https://api.hyperliquid-testnet.xyz"

// testnetAPI returns the REST API base URL to test against: the local mock
// server if HL_API_ENDPOINT is set, otherwise the public testnet.
func testnetAPI() string {
	if ep := os.Getenv("HL_API_ENDPOINT"); ep != "" {
		return ep
	}
	return defaultTestnetAPI
}

// usingMock reports whether the live tests are configured to run against the
// local Hyperliquid mock server rather than the real testnet.
func usingMock() bool {
	return os.Getenv("HL_API_ENDPOINT") != ""
}

// testSinkAddress is the recipient used for outbound test sends.
//
// Against the real testnet this must be a known active account with a real
// HYPE spot balance — Hyperliquid rejects sends to inactive/zero-balance
// accounts. Against the mock server any address works, since the mock
// creates accounts on demand.
const testSinkAddress = "0xeffb9c711ec1d3e551a7391dd848290a614c2e63"

var tLogger = util.StdOutLogger("HLTEST", util.LevelTrace)

// walletSettings returns the wallet config settings to use for the live
// tests, pointing at the mock server's WS endpoint if HL_API_ENDPOINT is set.
func walletSettings() map[string]string {
	settings := map[string]string{"apiendpoint": testnetAPI()}
	if ep := os.Getenv("HL_WS_ENDPOINT"); ep != "" {
		settings["wsendpoint"] = ep
	}
	return settings
}

// fundMockAddress credits address with amountAtoms via the mock server's
// /test/fund endpoint. It is only valid when usingMock() is true.
func fundMockAddress(t *testing.T, address string, amountAtoms uint64) {
	t.Helper()
	body, err := json.Marshal(struct {
		Address     string `json:"address"`
		AmountAtoms uint64 `json:"amountAtoms"`
	}{Address: address, AmountAtoms: amountAtoms})
	if err != nil {
		t.Fatalf("marshal fund request: %v", err)
	}
	resp, err := http.Post(testnetAPI()+"/test/fund", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("fund request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fund request: unexpected status %d", resp.StatusCode)
	}
}

// createTestWallet opens a wallet for the live tests.
//
// Against the real testnet it uses the raw private key in HL_TEST_PRIVATE_KEY,
// which must belong to a funded testnet account; the test is skipped if the
// env var is not set.
//
// Against the mock server (HL_API_ENDPOINT set) it generates a fresh key and
// funds it directly via the mock's /test/fund endpoint, so no pre-funded
// account is needed.
//
// We call saveKeyFile directly rather than Driver.Create because Create expects
// a seed and derives the key via HD derivation, which would produce a different
// address than the MetaMask account the test key belongs to. Create performs no
// other initialization beyond saveKeyFile — DB setup and metadata happen in
// Connect, which the tests call normally.
func createTestWallet(t *testing.T) (*HyperWallet, []byte) {
	t.Helper()

	var privKey *ecdsa.PrivateKey
	switch {
	case usingMock():
		var err error
		privKey, err = gethcrypto.GenerateKey()
		if err != nil {
			t.Fatalf("generating key: %v", err)
		}
	default:
		privKeyHex := strings.TrimPrefix(os.Getenv("HL_TEST_PRIVATE_KEY"), "0x")
		if privKeyHex == "" {
			t.Skip("set HL_TEST_PRIVATE_KEY=<hex_private_key> of a funded testnet account, or HL_API_ENDPOINT to use the mock server")
		}
		privBytes, err := hex.DecodeString(privKeyHex)
		if err != nil || len(privBytes) != 32 {
			t.Fatalf("invalid HL_TEST_PRIVATE_KEY: want 32-byte hex, got %d bytes", len(privBytes))
		}
		privKey, err = gethcrypto.ToECDSA(privBytes)
		if err != nil {
			t.Fatalf("parsing private key: %v", err)
		}
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
		Settings:    walletSettings(),
		Emit:        asset.NewWalletEmitter(noteChan, BipID, tLogger),
		PeersChange: func(n uint32, err error) {},
		DataDir:     dir,
	}, tLogger, util.Testnet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	hw := w.(*HyperWallet)
	if usingMock() {
		addr, err := hw.DepositAddress()
		if err != nil {
			t.Fatalf("DepositAddress: %v", err)
		}
		fundMockAddress(t, addr, 1000*1e8) // 1000 HYPE
	}

	return hw, pw
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
		Settings:    walletSettings(),
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
		Settings:    walletSettings(),
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
		Settings:    walletSettings(),
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
			pending := w.PendingTransactions(ctx)
			if len(pending) == 0 {
				t.Log("No more pending txs — checking history...")
				hist, err := w.TxHistory(&txdb.TxHistoryRequest{N: 10})
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
			if txNote.Transaction.Type == txdb.Send {
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
