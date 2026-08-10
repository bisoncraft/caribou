// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
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

// TestTokenWalletTransactionLookupCaseInsensitive covers the token path
// skipping the hash normalization the native path applies. A lowercase hash —
// what an explorer or a user paste supplies — missed the DB entirely and fell
// through to the RPC, losing TokenID and any pending state the stored record
// carried.
func TestTokenWalletTransactionLookupCaseInsensitive(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	txDB, _ := newTestTxDB(t)
	tw.txDB = txDB

	const txID = "DDDD000000000000000000000000000000000000000000000000000000000004"
	assetID := tw.assetID
	recip := tw.parent.address
	if err := txDB.StoreTx(&txdb.WalletTransaction{
		Type: txdb.Receive, ID: txID, Amount: 1_000_000, BlockNumber: 12,
		Confirmed: true, Recipient: &recip, TokenID: &assetID,
	}); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	wt, err := tw.WalletTransaction(context.Background(), strings.ToLower(txID))
	if err != nil {
		t.Fatalf("WalletTransaction (lowercase): %v", err)
	}
	if wt.ID != txID {
		t.Errorf("ID = %q, want %q", wt.ID, txID)
	}
	if wt.TokenID == nil || *wt.TokenID != assetID {
		t.Errorf("TokenID = %v, want %d — the DB record was not found", wt.TokenID, assetID)
	}
}

// TestNativeScanRecordsTokenReceive covers incoming issued-currency transfers
// never reaching the tx DB. IOU payments arrive at the same account the native
// scanner walks, but the native parser returns nil for non-drops amounts and no
// other scan path called the token parser — so receiving USDC moved Balance()
// while TxHistory stayed empty.
func TestNativeScanRecordsTokenReceive(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	w := tw.parent
	txDB, _ := newTestTxDB(t)
	w.txDB = txDB
	tw.txDB = txDB
	w.tokenSpecs = networkTokenSpecs(util.Mainnet)

	const txID = "EEEE000000000000000000000000000000000000000000000000000000000005"
	tr := &txResult{
		Account:         "rOtherSender11111111111111111111",
		Destination:     w.address,
		TransactionType: "Payment",
		Hash:            txID,
		Amount: map[string]interface{}{
			"currency": TokenUSDC.Currency,
			"issuer":   tw.issuerAddr,
			"value":    "100",
		},
	}
	meta := txMeta{
		TransactionResult: "tesSUCCESS",
		DeliveredAmount: map[string]interface{}{
			"currency": TokenUSDC.Currency,
			"issuer":   tw.issuerAddr,
			"value":    "100",
		},
	}

	// The native parser must still decline it — this is what left the
	// transfer unrecorded.
	if wtx := walletTransactionFromTx(tr, w.address, meta, true, 500); wtx != nil {
		t.Fatalf("native parser claimed an IOU payment as %v of %d drops", wtx.Type, wtx.Amount)
	}

	w.processObservedTransaction(tr, meta, 500)

	stored, err := txDB.GetTx(txID)
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if stored == nil {
		t.Fatal("incoming token transfer was not recorded")
	}
	if stored.Type != txdb.Receive {
		t.Errorf("Type = %v, want Receive", stored.Type)
	}
	if stored.TokenID == nil || *stored.TokenID != tw.assetID {
		t.Errorf("TokenID = %v, want %d", stored.TokenID, tw.assetID)
	}
	if stored.Amount != 100_000_000 {
		t.Errorf("Amount = %d, want 100000000 (100 USDC at 6 decimals)", stored.Amount)
	}

	// It must be visible through the token wallet's own history view, which
	// is scoped by asset.
	resp, err := tw.TxHistory(&txdb.TxHistoryRequest{N: 10})
	if err != nil {
		t.Fatalf("TxHistory: %v", err)
	}
	var found bool
	for _, tx := range resp.Txs {
		if tx.ID == txID {
			found = true
		}
	}
	if !found {
		t.Error("token receive is not in the token wallet's TxHistory")
	}
}

// TestTokenSendFeeSurvivesObservation covers the fee being wiped off a token
// send. TokenWallet.Send pre-stores the record with the drops fee it paid;
// processObservedTransaction then copies the parsed record's Fees onto it when
// the validated Payment is seen. The token parser reported no fee, so once the
// native scanner started claiming IOU payments the recorded fee became zero.
func TestTokenSendFeeSurvivesObservation(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	w := tw.parent
	txDB, _ := newTestTxDB(t)
	w.txDB = txDB
	tw.txDB = txDB
	w.tokenSpecs = networkTokenSpecs(util.Mainnet)

	const txID = "FFFF000000000000000000000000000000000000000000000000000000000006"
	const feeDrops = 12
	assetID := tw.assetID
	recip := "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"

	if err := txDB.StoreTx(&txdb.WalletTransaction{
		Type: txdb.Send, ID: txID, Amount: 1_000_000, Fees: feeDrops,
		Recipient: &recip, TokenID: &assetID,
	}); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	tr := &txResult{
		Account:         w.address,
		Destination:     recip,
		TransactionType: "Payment",
		Hash:            txID,
		Fee:             "12",
		Amount: map[string]interface{}{
			"currency": TokenUSDC.Currency,
			"issuer":   tw.issuerAddr,
			"value":    "1",
		},
	}
	w.processObservedTransaction(tr, txMeta{TransactionResult: "tesSUCCESS"}, 700)

	stored, err := txDB.GetTx(txID)
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if stored == nil {
		t.Fatal("record disappeared after observation")
	}
	if stored.Fees != feeDrops {
		t.Errorf("Fees = %d, want %d — the send's fee was wiped", stored.Fees, feeDrops)
	}
	if stored.TokenID == nil || *stored.TokenID != assetID {
		t.Errorf("TokenID = %v, want %d", stored.TokenID, assetID)
	}
	if !stored.Confirmed {
		t.Error("record should be confirmed after a validated observation")
	}
}

// TestRejectedInboundTokenPaymentNotRecorded covers a failed inbound IOU
// payment reaching the tx DB. The token parser used to be reachable only from
// the single-ID WalletTransaction lookup; making it a scanner fallback exposed
// it, and on a non-tesSUCCESS result it falls back to the sender-stated Amount.
// Anyone can send a payment that cannot succeed — tecNO_LINE without a
// trustline, tecPATH_DRY with one — so recording it would let a stranger write
// an arbitrary amount into this wallet's history.
//
// The rule belongs to the scanner, not the parser: a caller that asks about a
// specific hash must still be able to learn that the payment to it failed,
// rather than getting CoinNotFoundError, which is indistinguishable from the
// transaction not existing.
func TestRejectedInboundTokenPaymentNotRecorded(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	w := tw.parent
	txDB, _ := newTestTxDB(t)
	w.txDB = txDB
	tw.txDB = txDB
	w.tokenSpecs = networkTokenSpecs(util.Mainnet)

	const txID = "AAAA000000000000000000000000000000000000000000000000000000000009"
	tr := &txResult{
		Account:         "rAttacker1111111111111111111111",
		Destination:     w.address,
		TransactionType: "Payment",
		Hash:            txID,
		Fee:             "12",
		Amount: map[string]interface{}{
			"currency": TokenUSDC.Currency,
			"issuer":   tw.issuerAddr,
			"value":    "999999", // sender-stated; nothing was delivered
		},
	}
	meta := txMeta{TransactionResult: "tecPATH_DRY"}

	// The parser reports it, marked rejected, so a by-hash lookup can say what
	// happened.
	wtx := tokenTransactionFromTx(tr, w.address, meta, true, 800, tw.spec())
	if wtx == nil {
		t.Fatal("parser dropped a failed inbound payment; a by-hash lookup can no longer report it")
	}
	if !wtx.Rejected {
		t.Error("failed inbound payment not marked Rejected")
	}

	// The scanner is where it must not land.
	if got := w.tokenTransaction(tr, meta, true, 800); got != nil {
		t.Errorf("scanner accepted a failed inbound payment: Amount=%d", got.Amount)
	}

	w.processObservedTransaction(tr, meta, 800)
	stored, err := txDB.GetTx(txID)
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if stored != nil {
		t.Errorf("failed inbound payment stored: Type=%v Amount=%d Rejected=%v",
			stored.Type, stored.Amount, stored.Rejected)
	}
}

// The counterpart: our own failed outgoing token send must still be recorded,
// since Send pre-stores it and checkPendingTxs reconciles the result.
func TestRejectedOutgoingTokenSendStillParsed(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	w := tw.parent

	tr := &txResult{
		Account:         w.address,
		Destination:     "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe",
		TransactionType: "Payment",
		Hash:            "BBBB00000000000000000000000000000000000000000000000000000000000A",
		Fee:             "12",
		Amount: map[string]interface{}{
			"currency": TokenUSDC.Currency,
			"issuer":   tw.issuerAddr,
			"value":    "1",
		},
	}
	meta := txMeta{TransactionResult: "tecNO_LINE"}

	wtx := tokenTransactionFromTx(tr, w.address, meta, true, 800, tw.spec())
	if wtx == nil {
		t.Fatal("our own failed send must still be parsed so it can be reconciled")
	}
	if wtx.Type != txdb.Send {
		t.Errorf("Type = %v, want Send", wtx.Type)
	}
	if !wtx.Rejected {
		t.Error("Rejected should be true for a tec result")
	}
}

// TestTokenRecordEmitsNoNativeNote covers the native scanner announcing token
// records on its own notification channel. WalletEmitter stamps the note with
// the emitting wallet's asset ID, and both Core and the UI route on that rather
// than on the record's TokenID — so a token transaction would be filed against
// the native wallet's pending set, where the UI renders token atoms as drops.
// The record must still be stored; only the note is withheld.
func TestTokenRecordEmitsNoNativeNote(t *testing.T) {
	m := newMockRippled(t)
	// processObservedTransaction re-queries the balance after storing.
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("10000000", 0, 0), nil
	})
	tw := makeTokenTestWallet(t, m)
	w := tw.parent
	txDB, _ := newTestTxDB(t)
	w.txDB = txDB
	tw.txDB = txDB
	w.tokenSpecs = networkTokenSpecs(util.Mainnet)

	noteChan := make(chan asset.WalletNotification, 16)
	w.emit = asset.NewWalletEmitter(noteChan, BipID, util.NewLogger("test", util.LevelError, nil))

	const tokenTxID = "CCCC00000000000000000000000000000000000000000000000000000000000B"
	tokenTx := &txResult{
		Account:         "rOtherSender11111111111111111111",
		Destination:     w.address,
		TransactionType: "Payment",
		Hash:            tokenTxID,
		Fee:             "12",
		Amount: map[string]interface{}{
			"currency": TokenUSDC.Currency,
			"issuer":   tw.issuerAddr,
			"value":    "100",
		},
	}
	tokenMeta := txMeta{
		TransactionResult: "tesSUCCESS",
		DeliveredAmount: map[string]interface{}{
			"currency": TokenUSDC.Currency,
			"issuer":   tw.issuerAddr,
			"value":    "100",
		},
	}
	w.processObservedTransaction(tokenTx, tokenMeta, 900)

	stored, err := txDB.GetTx(tokenTxID)
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if stored == nil || stored.TokenID == nil {
		t.Fatal("token receive should still be recorded")
	}
	for {
		select {
		case n := <-noteChan:
			if tn, ok := n.(*asset.TransactionNote); ok {
				t.Errorf("token record announced on the native channel: assetID=%d tokenID=%v",
					tn.AssetID, tn.Transaction.TokenID)
			}
			continue // balance notes are fine
		default:
		}
		break
	}

	// A native payment in the same wallet must still be announced.
	const nativeTxID = "DDDD00000000000000000000000000000000000000000000000000000000000C"
	nativeTx := &txResult{
		Account:         "rOtherSender11111111111111111111",
		Destination:     w.address,
		TransactionType: "Payment",
		Hash:            nativeTxID,
		Fee:             "12",
		Amount:          "1000000",
	}
	w.processObservedTransaction(nativeTx, txMeta{TransactionResult: "tesSUCCESS", DeliveredAmount: "1000000"}, 901)

	var sawNative bool
	for {
		select {
		case n := <-noteChan:
			if tn, ok := n.(*asset.TransactionNote); ok && tn.Transaction.ID == nativeTxID {
				sawNative = true
			}
			continue
		default:
		}
		break
	}
	if !sawNative {
		t.Error("native payment should still emit a transaction note")
	}
}

// TestScanTokenRecordEmitsNoNativeNote is the scan-path counterpart to
// TestTokenRecordEmitsNoNativeNote: scanAccountTxs must store the token record
// without announcing it on the native asset's channel.
func TestScanTokenRecordEmitsNoNativeNote(t *testing.T) {
	const tokenTxID = "EEEE00000000000000000000000000000000000000000000000000000000000D"
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	walletAddr := tw.parent.address
	issuer := tw.issuerAddr

	m.handle("account_tx", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"transactions": []any{
				map[string]any{
					"tx": map[string]any{
						"hash":            tokenTxID,
						"TransactionType": "Payment",
						"Account":         "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe",
						"Destination":     walletAddr,
						"Fee":             "12",
						"ledger_index":    uint64(51),
						"Amount": map[string]any{
							"currency": TokenUSDC.Currency,
							"issuer":   issuer,
							"value":    "100",
						},
					},
					"meta": map[string]any{
						"TransactionResult": "tesSUCCESS",
						"delivered_amount": map[string]any{
							"currency": TokenUSDC.Currency,
							"issuer":   issuer,
							"value":    "100",
						},
					},
					"validated":    true,
					"ledger_index": uint64(51),
				},
			},
		}, nil
	})

	n, mt := newTestTxDB(t)
	noteChan := make(chan asset.WalletNotification, 16)
	w := tw.parent
	w.txDB = n
	w.metaTable = mt
	w.tokenSpecs = networkTokenSpecs(util.Mainnet)
	w.emit = asset.NewWalletEmitter(noteChan, BipID, util.NewLogger("test", util.LevelError, nil))
	w.lastScannedLedger.Store(50)

	w.scanAccountTxs(context.Background(), 51)

	stored, err := n.GetTx(tokenTxID)
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if stored == nil || stored.TokenID == nil {
		t.Fatal("scan should have stored the token receive")
	}
	for {
		select {
		case note := <-noteChan:
			if tn, ok := note.(*asset.TransactionNote); ok {
				t.Errorf("scan announced a token record on the native channel: assetID=%d tokenID=%v",
					tn.AssetID, tn.Transaction.TokenID)
			}
			continue
		default:
		}
		break
	}
}

// TestCheckPendingTxsEmitsNoTokenNote completes the rule the scanner and the
// WebSocket path already hold to: checkPendingTxs reconciles every asset
// sharing the NetworkTxDB — it must, for sequence accounting — but it must not
// announce a token record on the native asset's channel. Covers both exits:
// the confirmed path and the expiry path.
func TestCheckPendingTxsEmitsNoTokenNote(t *testing.T) {
	const (
		confirmedID = "1111000000000000000000000000000000000000000000000000000000000001"
		expiredID   = "2222000000000000000000000000000000000000000000000000000000000002"
	)

	m := newMockRippled(t)
	m.handle("tx", func(params json.RawMessage) (any, *rpcError) {
		var p struct {
			Transaction string `json:"transaction"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Transaction == expiredID {
			// Never made it into a ledger before its LastLedgerSequence.
			return nil, &rpcError{errType: "txnNotFound", msg: "not found"}
		}
		return map[string]any{
			"status":          "success",
			"hash":            confirmedID,
			"validated":       true,
			"ledger_index":    uint64(300),
			"TransactionType": "Payment",
			"meta":            map[string]any{"TransactionResult": "tesSUCCESS"},
		}, nil
	})

	tw := makeTokenTestWallet(t, m)
	w := tw.parent
	n, mt := newTestTxDB(t)
	w.txDB = n
	w.metaTable = mt
	noteChan := make(chan asset.WalletNotification, 16)
	w.emit = asset.NewWalletEmitter(noteChan, BipID, util.NewLogger("test", util.LevelError, nil))
	w.tipMtx.Lock()
	w.tip = 1000
	w.tipMtx.Unlock()

	assetID := tw.assetID
	recip := "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"
	for _, id := range []string{confirmedID, expiredID} {
		wtx := &txdb.WalletTransaction{
			Type: txdb.Send, ID: id, Amount: 1_000_000, Fees: 12,
			Recipient: &recip, TokenID: &assetID,
		}
		// A LastLedgerSequence below the tip makes the not-found case terminal.
		setLastLedgerSequence(wtx, 100)
		if err := n.StoreTx(wtx); err != nil {
			t.Fatalf("StoreTx %s: %v", id, err)
		}
	}

	w.checkPendingTxs(context.Background())

	for {
		select {
		case note := <-noteChan:
			if tn, ok := note.(*asset.TransactionNote); ok {
				t.Errorf("checkPendingTxs announced a token record on the native channel: id=%s assetID=%d tokenID=%v",
					tn.Transaction.ID, tn.AssetID, tn.Transaction.TokenID)
			}
			continue
		default:
		}
		break
	}

	// The records must still have been reconciled.
	for _, id := range []string{confirmedID, expiredID} {
		got, err := n.GetTx(id)
		if err != nil {
			t.Fatalf("GetTx %s: %v", id, err)
		}
		if got == nil || !got.Confirmed {
			t.Errorf("%s should have been reconciled to confirmed, got %+v", id, got)
		}
	}
}

// TestCheckPendingTxsEmitsTokenNoteToTokenWallet covers a token send never
// being announced as confirmed. TokenWallet.Send stores the record and emits
// the initial note, then depends entirely on the parent's checkPendingTxs to
// resolve it — the parent walks the one account both wallets share. Suppressing
// the note there left the send confirmed in the DB but never announced, so
// Core's pending set and the UI showed it unconfirmed indefinitely. The note
// has to go out on the token's own channel, since Core and the UI route on the
// emitter's asset ID rather than the record's TokenID.
func TestCheckPendingTxsEmitsTokenNoteToTokenWallet(t *testing.T) {
	const txID = "3333000000000000000000000000000000000000000000000000000000000003"

	m := newMockRippled(t)
	m.handle("tx", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":          "success",
			"hash":            txID,
			"validated":       true,
			"ledger_index":    uint64(300),
			"TransactionType": "Payment",
			"meta":            map[string]any{"TransactionResult": "tesSUCCESS"},
		}, nil
	})

	tw := makeTokenTestWallet(t, m)
	w := tw.parent
	n, mt := newTestTxDB(t)
	w.txDB, w.metaTable = n, mt
	tw.txDB = n
	log := util.NewLogger("test", util.LevelError, nil)

	nativeNotes := make(chan asset.WalletNotification, 16)
	w.emit = asset.NewWalletEmitter(nativeNotes, BipID, log)
	tokenNotes := make(chan asset.WalletNotification, 16)
	tw.emit = asset.NewWalletEmitter(tokenNotes, tw.assetID, log)

	w.tipMtx.Lock()
	w.tip = 1000
	w.tipMtx.Unlock()

	// Connect registers the token wallet's emitter with the parent.
	ctx, cancel := context.WithCancel(context.Background())
	wg, err := tw.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	assetID := tw.assetID
	recip := "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"
	wtx := &txdb.WalletTransaction{
		Type: txdb.Send, ID: txID, Amount: 1_000_000, Fees: 12,
		Recipient: &recip, TokenID: &assetID,
	}
	setLastLedgerSequence(wtx, 100)
	if err := n.StoreTx(wtx); err != nil {
		t.Fatalf("StoreTx: %v", err)
	}

	w.checkPendingTxs(context.Background())

	var got *asset.TransactionNote
	for done := false; !done; {
		select {
		case note := <-tokenNotes:
			if tn, ok := note.(*asset.TransactionNote); ok {
				got = tn
				done = true
			}
		default:
			done = true
		}
	}
	if got == nil {
		t.Fatal("token send confirmed in the DB but never announced")
	}
	if got.AssetID != tw.assetID {
		t.Errorf("note AssetID = %d, want the token's %d", got.AssetID, tw.assetID)
	}
	if !got.Transaction.Confirmed {
		t.Error("announced note does not report the send as confirmed")
	}

	// It must not also land on the native channel.
	select {
	case note := <-nativeNotes:
		if tn, ok := note.(*asset.TransactionNote); ok {
			t.Errorf("token record also announced natively: assetID=%d", tn.AssetID)
		}
	default:
	}

	// Disconnecting deregisters, so a closed token wallet stops receiving.
	cancel()
	wg.Wait()
	w.tokenEmitterMtx.RLock()
	_, still := w.tokenEmitters[tw.assetID]
	w.tokenEmitterMtx.RUnlock()
	if still {
		t.Error("token emitter still registered after the wallet's context ended")
	}
}
