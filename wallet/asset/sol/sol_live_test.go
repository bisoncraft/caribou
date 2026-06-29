//go:build harness

package sol

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
)

const harnessRPC = "http://127.0.0.1:18999"

var harnessLog = util.StdOutLogger("SOLTEST", util.LevelTrace)

func harnessCall(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	resp, err := http.Post(harnessRPC, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: %v — is the harness running? (./util/testing/sol/harness.sh)", method, err)
	}
	defer resp.Body.Close()
	var rr struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("decode %s response: %v", method, err)
	}
	if rr.Error != nil {
		t.Fatalf("%s returned error: %v", method, rr.Error)
	}
	return rr.Result
}

func harnessAirdrop(t *testing.T, address string, lamports uint64) {
	t.Helper()
	raw := harnessCall(t, "requestAirdrop", []any{address, lamports})
	var sig string
	if err := json.Unmarshal(raw, &sig); err != nil {
		t.Fatalf("unmarshal requestAirdrop result: %v", err)
	}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		statusRaw := harnessCall(t, "getSignatureStatuses", []any{[]string{sig}, map[string]any{"searchTransactionHistory": true}})
		var statuses struct {
			Value []struct {
				ConfirmationStatus string `json:"confirmationStatus"`
			} `json:"value"`
		}
		if err := json.Unmarshal(statusRaw, &statuses); err != nil {
			t.Fatalf("unmarshal getSignatureStatuses: %v", err)
		}
		if len(statuses.Value) > 0 && statuses.Value[0].ConfirmationStatus == "finalized" {
			return
		}
	}
	t.Fatalf("airdrop to %s did not finalize", address)
}

func newHarnessTxDB(t *testing.T) txdb.NetworkTxDB {
	t.Helper()
	log := util.NewLogger("txdb", util.LevelError, io.Discard)
	master, err := txdb.New(t.TempDir()+"/tx.db", log)
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	return master.NetworkTxDB(BipID)
}

func createHarnessWallet(t *testing.T) (*Wallet, []byte) {
	t.Helper()
	dir := t.TempDir()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	pw := make([]byte, 32)
	if _, err := rand.Read(pw); err != nil {
		t.Fatal(err)
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
	noteChan := make(chan asset.WalletNotification, 64)
	go func() {
		for range noteChan {
		}
	}()
	raw, err := drv.Open(&asset.WalletConfig{
		Settings:    map[string]string{"rpcprovider": harnessRPC},
		Emit:        asset.NewWalletEmitter(noteChan, BipID, harnessLog),
		PeersChange: func(uint32, error) {},
		DataDir:     dir,
		NetworkTxDB: newHarnessTxDB(t),
	}, harnessLog, util.Simnet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { close(noteChan) })
	return raw.(*Wallet), pw
}

func TestHarnessConnectAndBalance(t *testing.T) {
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
		t.Fatal("wallet address failed validation")
	}
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance before funding: %v", err)
	}
	if bal.Available != 0 {
		t.Fatalf("expected zero available before funding, got %d", bal.Available)
	}
	harnessAirdrop(t, addr, 2_000_000_000)
	time.Sleep(2 * time.Second)
	bal, err = w.Balance()
	if err != nil {
		t.Fatalf("Balance after funding: %v", err)
	}
	if bal.Available == 0 {
		t.Fatal("expected funded wallet to have available balance")
	}
}

func TestHarnessSendAndHistory(t *testing.T) {
	sender, senderPW := createHarnessWallet(t)
	receiver, receiverPW := createHarnessWallet(t)
	ctx, cancel := context.WithCancel(context.Background())
	senderWG, err := sender.Connect(ctx)
	if err != nil {
		cancel()
		t.Fatalf("sender Connect: %v", err)
	}
	receiverWG, err := receiver.Connect(ctx)
	if err != nil {
		cancel()
		senderWG.Wait()
		t.Fatalf("receiver Connect: %v", err)
	}
	defer func() {
		cancel()
		senderWG.Wait()
		receiverWG.Wait()
	}()
	if err := sender.Unlock(senderPW); err != nil {
		t.Fatalf("sender Unlock: %v", err)
	}
	if err := receiver.Unlock(receiverPW); err != nil {
		t.Fatalf("receiver Unlock: %v", err)
	}
	senderAddr, _ := sender.DepositAddress()
	receiverAddr, _ := receiver.DepositAddress()
	harnessAirdrop(t, senderAddr, 3_000_000_000)
	time.Sleep(2 * time.Second)
	c, err := sender.Send(receiverAddr, 250_000_000, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if c.TxID() == "" {
		t.Fatal("empty txid")
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		hist, err := receiver.TxHistory(&txdb.TxHistoryRequest{N: 10})
		if err == nil {
			for _, tx := range hist.Txs {
				if tx.ID == c.TxID() {
					return
				}
			}
		}
	}
	t.Fatalf("receiver history never observed tx %s", c.TxID())
}
