// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
)

func TestDeriveWSEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		rpcURL  string
		want    string
		wantErr bool
	}{
		{
			name:   "http scheme, no special port",
			rpcURL: "http://127.0.0.1:15005",
			want:   "ws://127.0.0.1:15005/",
		},
		{
			name:   "xrpl rpc port",
			rpcURL: "https://s.altnet.rippletest.net:51234",
			want:   "wss://s.altnet.rippletest.net:51233/",
		},
		{
			name:    "bad scheme",
			rpcURL:  "ftp://example.com",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := deriveWSEndpoint(test.rpcURL)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("deriveWSEndpoint: %v", err)
			}
			if got != test.want {
				t.Fatalf("derived websocket endpoint = %q, want %q", got, test.want)
			}
		})
	}
}

func TestHandleWSMessageTransactionEmitsBalanceChange(t *testing.T) {
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("10000000", 0, 0), nil
	})

	log := util.NewLogger("test", util.LevelError, nil)
	db, err := newTxDB(t.TempDir(), log)
	if err != nil {
		t.Fatalf("newTxDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const walletAddr = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	noteChan := make(chan asset.WalletNotification, 8)
	w := &xrpWallet{
		address: walletAddr,
		rpc:     m.rpcClient(),
		txDB:    db,
		log:     log,
		emit:    asset.NewWalletEmitter(noteChan, BipID, log),
	}
	w.running.Store(true)

	// Use API v2 format: hash is at the top level, not inside tx_json.
	msg := []byte(`{
		"type":"transaction",
		"hash":"ABCDEF0000000000000000000000000000000000000000000000000000000001",
		"validated":true,
		"ledger_index":321,
		"close_time_iso":"2026-06-03T12:00:00Z",
		"meta":{"TransactionResult":"tesSUCCESS","delivered_amount":"10000000"},
		"tx_json":{
			"TransactionType":"Payment",
			"Account":"rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe",
			"Destination":"rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
			"Amount":"10000000",
			"Fee":"12"
		}
	}`)

	if err := w.handleWSMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleWSMessage: %v", err)
	}

	var (
		txNote  *asset.TransactionNote
		balNote *asset.BalanceChangeNote
	)
	timeout := time.After(2 * time.Second)
	for txNote == nil || balNote == nil {
		select {
		case note := <-noteChan:
			switch n := note.(type) {
			case *asset.TransactionNote:
				txNote = n
			case *asset.BalanceChangeNote:
				balNote = n
			}
		case <-timeout:
			t.Fatalf("timed out waiting for websocket notifications (tx=%v balance=%v)", txNote != nil, balNote != nil)
		}
	}

	if !txNote.New {
		t.Fatal("expected websocket receive to be reported as a new transaction")
	}
	if txNote.Transaction.Type != asset.Receive {
		t.Fatalf("transaction type = %v, want receive", txNote.Transaction.Type)
	}
	if txNote.Transaction.BlockNumber != 321 || !txNote.Transaction.Confirmed {
		t.Fatalf("unexpected tx confirmation state: %+v", txNote.Transaction)
	}
	if balNote.Balance == nil {
		t.Fatal("expected non-nil balance note")
	}
	if balNote.Balance.Available == 0 && balNote.Balance.Locked == 0 {
		t.Fatalf("expected funded balance, got %+v", balNote.Balance)
	}

	stored, err := db.getTx(txNote.Transaction.ID)
	if err != nil {
		t.Fatalf("getTx: %v", err)
	}
	if stored == nil || !stored.Confirmed || stored.BlockNumber != 321 {
		t.Fatalf("expected stored confirmed tx, got %+v", stored)
	}
}
