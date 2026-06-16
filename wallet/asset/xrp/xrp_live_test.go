//go:build harness

// These tests drive the local rippled simnet harness.
//
// Prerequisites: start the harness with
//
//	./util/testing/xrp/harness.sh
//
// Run with:
//
//	go test -v -tags harness -run TestHarness ./wallet/asset/xrp/
package xrp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	btcec "github.com/btcsuite/btcd/btcec/v2"
)

const (
	harnessEndpoint = "http://127.0.0.1:15005"

	// genesisKeyHex is the well-known master key for rippled --standalone --start.
	// This is public knowledge embedded in rippled's own test suite; not a secret.
	genesisKeyHex = "1ACAAEDECE405B2A958212629E16F2EB46B153EEE94CDD350FDEFF52795525B7"
	genesisAddr   = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"

	// tipWait is long enough for one XRPL ledger close (~3-4 s) plus margin.
	// The harness uses a live websocket subscription, so tips arrive quickly.
	tipWait = 4 * time.Second
)

var harnessLog = util.StdOutLogger("XRPTEST", util.LevelTrace)

// harnessCall makes a raw JSON-RPC call to the local simnet and returns the
// result field. Fatals the test if the node is unreachable.
func harnessCall(t *testing.T, method string, params map[string]any) json.RawMessage {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"method": method,
		"params": []any{params},
	})
	resp, err := http.Post(harnessEndpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("harnessCall %s: %v — is the harness running? (./util/testing/xrp/harness.sh)", method, err)
	}
	defer resp.Body.Close()
	var rr struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("decode %s response: %v", method, err)
	}
	return rr.Result
}

func harnessAdvanceLedger(t *testing.T) {
	t.Helper()
	harnessCall(t, "ledger_accept", map[string]any{})
}

func harnessGenesisSeq(t *testing.T) uint32 {
	t.Helper()
	raw := harnessCall(t, "account_info", map[string]any{
		"account":      genesisAddr,
		"ledger_index": "current",
	})
	var r struct {
		AccountData struct {
			Sequence uint32 `json:"Sequence"`
		} `json:"account_data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode account_info: %v", err)
	}
	return r.AccountData.Sequence
}

func harnessFeeInfo(t *testing.T) (fee uint64, ledgerIdx uint64) {
	t.Helper()
	raw := harnessCall(t, "fee", map[string]any{})
	var r struct {
		Drops struct {
			OpenLedgerFee uint64 `json:"open_ledger_fee,string"`
		} `json:"drops"`
		LedgerCurrentIndex uint64 `json:"ledger_current_index"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode fee: %v", err)
	}
	if r.Drops.OpenLedgerFee == 0 {
		r.Drops.OpenLedgerFee = DefaultFeeDrops
	}
	return r.Drops.OpenLedgerFee, r.LedgerCurrentIndex
}

func harnessSubmit(t *testing.T, txBlob string) string {
	t.Helper()
	raw := harnessCall(t, "submit", map[string]any{"tx_blob": txBlob})
	var r struct {
		EngineResult        string `json:"engine_result"`
		EngineResultMessage string `json:"engine_result_message"`
		TxJSON              struct {
			Hash string `json:"hash"`
		} `json:"tx_json"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode submit: %v", err)
	}
	if len(r.EngineResult) >= 3 && r.EngineResult[:3] != "tes" {
		t.Fatalf("submit rejected: %s — %s", r.EngineResult, r.EngineResultMessage)
	}
	return r.TxJSON.Hash
}

// harnessFund sends drops from the genesis account to address and advances
// the ledger so the payment is confirmed.
func harnessFund(t *testing.T, address string, drops uint64) {
	t.Helper()
	keyBytes, err := hex.DecodeString(genesisKeyHex)
	if err != nil {
		t.Fatalf("genesis key decode: %v", err)
	}
	privKey, pubKey := btcec.PrivKeyFromBytes(keyBytes)
	seq := harnessGenesisSeq(t)
	fee, ledgerIdx := harnessFeeInfo(t)
	lastLedger := uint32(ledgerIdx + lastLedgerWindow)
	txBlob, err := BuildSignedPaymentBlob(privKey, pubKey, genesisAddr, address, drops, fee, seq, lastLedger)
	if err != nil {
		t.Fatalf("BuildSignedPaymentBlob: %v", err)
	}
	hash := harnessSubmit(t, txBlob)
	harnessAdvanceLedger(t)
	t.Logf("funded %s with %d drops (tx %s)", address, drops, hash)
}

// createHarnessWallet creates a wallet pointed at the local simnet.
func createHarnessWalletWithNotes(t *testing.T) (*xrpWallet, []byte, chan asset.WalletNotification) {
	t.Helper()
	dir := t.TempDir()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("rand: %v", err)
	}
	pw := make([]byte, 32)
	if _, err := rand.Read(pw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	drv := &Driver{}
	if err := drv.Create(&asset.CreateWalletParams{
		Type:    walletTypeRPC,
		Seed:    seed,
		Pass:    pw,
		DataDir: dir,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	noteChan := make(chan asset.WalletNotification, 128)
	raw, err := drv.Open(&asset.WalletConfig{
		Settings:    map[string]string{"rpcprovider": harnessEndpoint},
		Emit:        asset.NewWalletEmitter(noteChan, BipID, harnessLog),
		PeersChange: func(uint32, error) {},
		DataDir:     dir,
	}, harnessLog, util.Simnet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return raw.(*xrpWallet), pw, noteChan
}

func createHarnessWallet(t *testing.T) (*xrpWallet, []byte) {
	t.Helper()
	w, pw, _ := createHarnessWalletWithNotes(t)
	return w, pw
}

func TestHarnessConnect(t *testing.T) {
	w, pw := createHarnessWallet(t)
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
	if !w.ValidateAddress(addr) {
		t.Fatal("own address failed validation")
	}

	owns, err := w.OwnsDepositAddress(addr)
	if err != nil || !owns {
		t.Fatalf("OwnsDepositAddress: owns=%v err=%v", owns, err)
	}

	ss, err := w.SyncStatus()
	if err != nil {
		t.Fatalf("SyncStatus: %v", err)
	}
	t.Logf("tip=%d synced=%v", ss.Blocks, ss.Synced)
	if ss.Blocks == 0 {
		t.Fatal("expected non-zero ledger index from simnet")
	}

	if err := w.Lock(); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if !w.Locked() {
		t.Error("expected wallet locked after Lock")
	}
}

func TestHarnessBalance(t *testing.T) {
	w, pw := createHarnessWallet(t)
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

	addr, _ := w.DepositAddress()

	// Unfunded account: balance must be zero.
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance (unfunded): %v", err)
	}
	if bal.Available != 0 || bal.Locked != 0 {
		t.Fatalf("expected zero balance, got avail=%d locked=%d", bal.Available, bal.Locked)
	}

	harnessFund(t, addr, 2_000_000) // 1 XRP reserve + 1 XRP available
	time.Sleep(tipWait)

	bal, err = w.Balance()
	if err != nil {
		t.Fatalf("Balance (funded): %v", err)
	}
	t.Logf("Balance: avail=%d locked=%d", bal.Available, bal.Locked)
	if bal.Available == 0 && bal.Locked == 0 {
		t.Fatal("expected non-zero balance after funding")
	}
}

func TestHarnessBalanceNotification(t *testing.T) {
	w, pw, noteChan := createHarnessWalletWithNotes(t)
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

	addr, _ := w.DepositAddress()
	harnessFund(t, addr, 2_000_000)

	timeout := time.After(5 * time.Second)
	for {
		select {
		case note := <-noteChan:
			balNote, ok := note.(*asset.BalanceChangeNote)
			if !ok {
				continue
			}
			if balNote.Balance == nil {
				t.Fatal("received nil balance note")
			}
			if balNote.Balance.Available == 0 && balNote.Balance.Locked == 0 {
				continue
			}
			return
		case <-timeout:
			t.Fatal("timed out waiting for funded balance notification")
		}
	}
}

// TestHarnessConfirmation submits two payments back-to-back, forces
// ledger_accept, and asserts both are confirmed in the txdb. This exercises
// the H1/H2 sequence handling and the live update confirmation path.
func TestHarnessConfirmation(t *testing.T) {
	sender, pw := createHarnessWallet(t)
	receiver, _ := createHarnessWallet(t)

	senderAddr, _ := sender.DepositAddress()
	receiverAddr, _ := receiver.DepositAddress()

	// Fund both accounts. The base reserve is 1 XRP, so the receiver must be
	// pre-activated or Send() rejects the payment. Sender needs enough for two
	// sends (1 XRP each) plus its own reserve (1 XRP) plus fees.
	harnessFund(t, senderAddr, 5_000_000)
	harnessFund(t, receiverAddr, 1_000_000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := sender.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { cancel(); wg.Wait() }()

	if err := sender.Unlock(pw); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	// Wait for the live update loop to observe the funding ledger.
	time.Sleep(tipWait)

	bal, err := sender.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available == 0 {
		t.Fatal("sender has zero available after funding")
	}

	const sendDrops = uint64(1_000_000) // 1 XRP each
	c1, err := sender.Send(receiverAddr, sendDrops, 0)
	if err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	c2, err := sender.Send(receiverAddr, sendDrops, 0)
	if err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	t.Logf("tx1=%s tx2=%s", c1.TxID(), c2.TxID())

	// Both should appear as pending immediately.
	pending := sender.PendingTransactions(ctx)
	if len(pending) < 2 {
		t.Errorf("expected ≥2 pending txs, got %d", len(pending))
	}

	// Force ledger close and let the live update loop process the tip.
	harnessAdvanceLedger(t)
	harnessAdvanceLedger(t)
	time.Sleep(tipWait * 2)

	// Both txs must be confirmed in the txdb.
	for _, txID := range []string{c1.TxID(), c2.TxID()} {
		dbEntry, err := sender.txDB.GetTx(txID)
		if err != nil {
			t.Fatalf("getTx %s: %v", txID, err)
		}
		if dbEntry == nil {
			t.Fatalf("tx %s not found in txdb after confirmation", txID)
		}
		if !dbEntry.Confirmed {
			t.Errorf("tx %s not confirmed (BlockNumber=%d)", txID, dbEntry.BlockNumber)
		}
		if dbEntry.Rejected {
			t.Errorf("tx %s unexpectedly rejected", txID)
		}
	}
}

func TestHarnessStandardSendFee(t *testing.T) {
	w, _ := createHarnessWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, err := w.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { cancel(); wg.Wait() }()

	fee := w.StandardSendFee(0)
	t.Logf("StandardSendFee: %d drops", fee)
	if fee == 0 {
		t.Error("expected non-zero fee")
	}
}
