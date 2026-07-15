// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
	btcec "github.com/btcsuite/btcd/btcec/v2"
)

func makeTokenTestWallet(t *testing.T, m *mockRippled) *TokenWallet {
	t.Helper()
	privKeyBytes := make([]byte, 32)
	privKeyBytes[31] = 1
	privKey, pubKey := btcec.PrivKeyFromBytes(privKeyBytes)
	addr := pubKeyToAddress(pubKey)

	parent := &xrpWallet{
		address: addr,
		pubKey:  pubKey,
		privKey: privKey,
		rpc:     m.rpcClient(),
		log:     util.NewLogger("test", util.LevelError, nil),
		ctx:     context.Background(),
	}
	parent.running.Store(true)

	currency, err := encodeCurrencyCode(TokenUSDC.Currency)
	if err != nil {
		t.Fatalf("encodeCurrencyCode: %v", err)
	}
	issuerAddr := TokenUSDC.NetIssuers[util.Mainnet]
	issuerAccountID, err := addressToAccountID(issuerAddr)
	if err != nil {
		t.Fatalf("addressToAccountID: %v", err)
	}
	return &TokenWallet{
		parent:     parent,
		assetID:    usdcTokenID,
		token:      TokenUSDC,
		issuerAddr: issuerAddr,
		currency:   currency,
		issuer:     [20]byte(issuerAccountID),
	}
}

func accountLinesResponse(lines []accountLine) map[string]any {
	return map[string]any{
		"status": "success",
		"lines":  lines,
	}
}

func TestTokenWalletDepositAddress(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	addr, err := tw.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress error: %v", err)
	}
	if addr != tw.parent.address {
		t.Fatalf("DepositAddress = %q, want parent address %q", addr, tw.parent.address)
	}
}

func TestTokenWalletBalanceNoTrustline(t *testing.T) {
	m := newMockRippled(t)
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse(nil), nil
	})
	tw := makeTokenTestWallet(t, m)
	bal, err := tw.Balance()
	if err != nil {
		t.Fatalf("Balance error: %v", err)
	}
	if bal.Available != 0 {
		t.Fatalf("Available = %d, want 0 with no trustline", bal.Available)
	}
}

func TestTokenWalletBalanceWithTrustline(t *testing.T) {
	m := newMockRippled(t)
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse([]accountLine{{
			Account:  TokenUSDC.NetIssuers[util.Mainnet],
			Balance:  "42.5",
			Currency: TokenUSDC.Currency,
			Limit:    "1000000000",
		}}), nil
	})
	tw := makeTokenTestWallet(t, m)
	bal, err := tw.Balance()
	if err != nil {
		t.Fatalf("Balance error: %v", err)
	}
	if bal.Available != 42_500_000 {
		t.Fatalf("Available = %d, want 42500000", bal.Available)
	}
}

func TestWalletTransactionFromTxPartialPayment(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	issuerAddr := tw.issuerAddr

	// A tfPartialPayment transaction can validate tesSUCCESS while
	// delivering far less than Amount states; the wallet must credit
	// meta.delivered_amount, not Amount, or it can be tricked into
	// recording funds that were never actually received.
	tr := &txResult{
		Account:         "rOtherSender11111111111111111111",
		Destination:     tw.parent.address,
		TransactionType: "Payment",
		Amount: map[string]interface{}{
			"currency": TokenUSDC.Currency,
			"issuer":   issuerAddr,
			"value":    "10000",
		},
		Hash: "0123abcd",
		Meta: txMeta{
			TransactionResult: "tesSUCCESS",
			DeliveredAmount: map[string]interface{}{
				"currency": TokenUSDC.Currency,
				"issuer":   issuerAddr,
				"value":    "0.0001",
			},
		},
	}
	wt := tw.walletTransactionFromTx(tr)
	if wt == nil {
		t.Fatal("expected wallet transaction")
	}
	if wt.Type != txdb.Receive {
		t.Fatalf("Type = %v, want Receive", wt.Type)
	}
	if wt.Amount != 100 {
		t.Fatalf("Amount = %d, want 100 (delivered), not the stated 10000", wt.Amount)
	}
}

func TestWalletTransactionFromTxIndeterminateDeliveredAmount(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	issuerAddr := tw.issuerAddr

	// A tesSUCCESS payment whose delivered_amount is absent or "unavailable"
	// gives no trustworthy record of what was actually delivered. The
	// sender-stated Amount is exactly what a spoofed tfPartialPayment forges,
	// so the wallet must not fall back to it and credit a possibly-fabricated
	// amount.
	for name, deliveredAmount := range map[string]interface{}{
		"absent":      nil,
		"unavailable": "unavailable",
	} {
		t.Run(name, func(t *testing.T) {
			tr := &txResult{
				Account:         "rOtherSender11111111111111111111",
				Destination:     tw.parent.address,
				TransactionType: "Payment",
				Amount: map[string]interface{}{
					"currency": TokenUSDC.Currency,
					"issuer":   issuerAddr,
					"value":    "10000",
				},
				Hash: "0123abcd",
				Meta: txMeta{
					TransactionResult: "tesSUCCESS",
					DeliveredAmount:   deliveredAmount,
				},
			}
			if wt := tw.walletTransactionFromTx(tr); wt != nil {
				t.Fatalf("expected nil wallet transaction (indeterminate delivered amount), got Amount=%d", wt.Amount)
			}
		})
	}
}

func TestTokenWalletSendNoDestinationTrustline(t *testing.T) {
	m := newMockRippled(t)
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse(nil), nil
	})
	tw := makeTokenTestWallet(t, m)
	_, err := tw.Send("rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe", 1_000_000, 0)
	if err == nil {
		t.Fatal("expected error when destination has no trustline")
	}
}

func TestTokenWalletSendInvalidAddress(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	if _, err := tw.Send("not-an-address", 1_000_000, 0); err == nil {
		t.Fatal("expected error for invalid recipient address")
	}
}

func trustlineTestHandlers(m *mockRippled, engineResult, engineResultMsg string) {
	m.handle("fee", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":               "success",
			"drops":                map[string]any{"open_ledger_fee": "12"},
			"ledger_current_index": uint64(100),
		}, nil
	})
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("100000000", 0, 0), nil
	})
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":                "success",
			"engine_result":         engineResult,
			"engine_result_message": engineResultMsg,
			"tx_json":               map[string]any{},
		}, nil
	})
}

// A tec result means the TrustSet was included in a validated ledger but the
// trustline was NOT actually created (e.g. tecINSUF_RESERVE_LINE for a
// reserve-starved account). Before this fix, EstablishTrustline returned nil
// in this case with nothing recorded anywhere, so the failure was invisible.
func TestEstablishTrustlineTecTrackedAsPending(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	txDB, _ := newTestTxDB(t)
	tw.txDB = txDB
	trustlineTestHandlers(m, "tecINSUF_RESERVE_LINE", "Insufficient reserve to add trust line.")

	if err := tw.EstablishTrustline(); err != nil {
		t.Fatalf("EstablishTrustline error: %v", err)
	}

	pending, err := txDB.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending trustline tx for checkPendingTxs to reconcile, got %d", len(pending))
	}
	if pending[0].Type != txdb.ApproveToken {
		t.Fatalf("Type = %v, want ApproveToken", pending[0].Type)
	}
	if pending[0].Confirmed {
		t.Fatal("tec result should remain pending here; checkPendingTxs resolves confirmation/rejection")
	}
}

func TestEstablishTrustlineDeterministicRejection(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	txDB, _ := newTestTxDB(t)
	tw.txDB = txDB
	trustlineTestHandlers(m, "temBAD_AMOUNT", "bad amount")

	if err := tw.EstablishTrustline(); err == nil {
		t.Fatal("expected error for deterministically rejected trustline tx")
	}

	pending, err := txDB.GetPendingTxs()
	if err != nil {
		t.Fatalf("GetPendingTxs: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending trustline txs after deterministic rejection, got %d", len(pending))
	}
}

func TestTokenWalletSendHappyPath(t *testing.T) {
	m := newMockRippled(t)
	const destAddr = "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"
	tw := makeTokenTestWallet(t, m)

	m.handle("account_lines", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		acct, _ := p["account"].(string)
		if acct == destAddr {
			return accountLinesResponse([]accountLine{{
				Account:  TokenUSDC.NetIssuers[util.Mainnet],
				Balance:  "0",
				Currency: TokenUSDC.Currency,
			}}), nil
		}
		return accountLinesResponse([]accountLine{{
			Account:  TokenUSDC.NetIssuers[util.Mainnet],
			Balance:  "100",
			Currency: TokenUSDC.Currency,
		}}), nil
	})
	m.handle("fee", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":               "success",
			"drops":                map[string]any{"open_ledger_fee": "12"},
			"ledger_current_index": uint64(100),
		}, nil
	})
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("100000000", 0, 0), nil
	})
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":                "success",
			"engine_result":         "tesSUCCESS",
			"engine_result_message": "The transaction was applied.",
			"tx_json":               map[string]any{"hash": "0000000000000000000000000000000000000000000000000000000000000001"},
		}, nil
	})

	coin, err := tw.Send(destAddr, 1_000_000, 0)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if coin.Value() != 1_000_000 {
		t.Fatalf("coin value = %d, want 1000000", coin.Value())
	}
}
