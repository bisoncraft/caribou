// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
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
