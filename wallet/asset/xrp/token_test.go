// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// TestTokenWalletConnectReportsPeers guards against a token wallet's Send
// being permanently rejected by Core's no-connected-peers safety check. The
// token wallet has no independent connectivity probe of its own, so it must
// report peers once on Connect using the parent's already-verified RPC/WS
// connection, or Core's per-asset peer counter for the token never leaves
// its initial "unknown" state.
func TestTokenWalletConnectReportsPeers(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	connectTestHandlers(m)
	var gotNumPeers uint32
	var calls int
	tw.peersChange = func(numPeers uint32, err error) {
		calls++
		gotNumPeers = numPeers
		if err != nil {
			t.Fatalf("peersChange error: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg, err := tw.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	cancel()
	wg.Wait()

	if calls != 1 {
		t.Fatalf("expected peersChange to be called once, got %d calls", calls)
	}
	if gotNumPeers != 1 {
		t.Fatalf("expected peersChange(1, nil), got peersChange(%d, ...)", gotNumPeers)
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
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("100000000", 0, 0), nil
	})
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse(nil), nil
	})
	tw := makeTokenTestWallet(t, m)
	_, err := tw.Send("rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe", 1_000_000, 0)
	if err == nil {
		t.Fatal("expected error when destination has no trustline")
	}
}

// TestTokenWalletSendRequiresDestinationTag guards against burning the
// network fee on a token payment that is guaranteed to fail on-chain:
// TokenWallet has no SendTagged, so a destination with lsfRequireDestTag set
// must be rejected before submit, same as the native wallet.
func TestTokenWalletSendRequiresDestinationTag(t *testing.T) {
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("100000000", 0, lsfRequireDestTag), nil
	})
	tw := makeTokenTestWallet(t, m)
	_, err := tw.Send("rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe", 1_000_000, 0)
	if err == nil {
		t.Fatal("expected an error sending to a tag-required destination")
	}
	if !strings.Contains(err.Error(), "destination tag") {
		t.Errorf("error should mention a destination tag, got: %v", err)
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
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse(nil), nil
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

	if _, err := tw.EstablishTrustline(); err != nil {
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

	if _, err := tw.EstablishTrustline(); err == nil {
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

// TestEstablishTrustlineInsufficientReserve covers a TrustSet preflight that
// checks only the fee. A successful TrustSet creates a RippleState object,
// incrementing OwnerCount and so the owner reserve the account must hold —
// on top of the fee. A check that stops at the fee lets the preflight pass
// for an account that can afford the fee but not the reserve, burning both
// on-chain with tecINSUF_RESERVE_LINE.
func TestEstablishTrustlineInsufficientReserve(t *testing.T) {
	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	tw.parent.ownerReserve = OwnerReserveDrops

	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse(nil), nil
	})
	m.handle("fee", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":               "success",
			"drops":                map[string]any{"open_ledger_fee": "12"},
			"ledger_current_index": uint64(100),
		}, nil
	})
	// Enough to pay the fee, not enough to also cover the owner reserve a
	// successful TrustSet will add.
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("100", 0, 0), nil
	})
	var submits atomic.Int32
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		submits.Add(1)
		return map[string]any{"status": "success", "engine_result": "tesSUCCESS"}, nil
	})

	_, err := tw.EstablishTrustline()
	if !errors.Is(err, asset.ErrInsufficientBalance) {
		t.Fatalf("EstablishTrustline error = %v, want ErrInsufficientBalance", err)
	}
	if submits.Load() != 0 {
		t.Fatalf("submit called %d times, want 0 — an unaffordable reserve must not reach the network", submits.Load())
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
				Limit:    "999999999.999999",
			}}), nil
		}
		return accountLinesResponse([]accountLine{{
			Account:  TokenUSDC.NetIssuers[util.Mainnet],
			Balance:  "100",
			Currency: TokenUSDC.Currency,
			Limit:    "999999999.999999",
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

// TestTokenWalletSendInsufficientNativeFee covers a token send never
// checking whether the parent account holds enough native XRP to pay the
// fee. The trustline balance alone was sufficient, so the send reserved a
// sequence and reached the network with a payment the account could not
// fund.
func TestTokenWalletSendInsufficientNativeFee(t *testing.T) {
	const destAddr = "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"
	m := newMockRippled(t)
	m.handle("account_lines", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		if p["account"] == destAddr {
			return accountLinesResponse([]accountLine{{
				Account:  TokenUSDC.NetIssuers[util.Mainnet],
				Balance:  "0",
				Currency: TokenUSDC.Currency,
				Limit:    "999999999.999999",
			}}), nil
		}
		return accountLinesResponse([]accountLine{{
			Account:  TokenUSDC.NetIssuers[util.Mainnet],
			Balance:  "100",
			Currency: TokenUSDC.Currency,
			Limit:    "999999999.999999",
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
		// The parent account holds far less XRP than the fee.
		return accountInfoResponse("5", 0, 0), nil
	})
	var submits atomic.Int32
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		submits.Add(1)
		return map[string]any{"status": "success", "engine_result": "tesSUCCESS"}, nil
	})

	tw := makeTokenTestWallet(t, m)
	_, err := tw.Send(destAddr, 1_000_000, 0)
	if !errors.Is(err, asset.ErrInsufficientBalance) {
		t.Fatalf("Send error = %v, want ErrInsufficientBalance (parent can't cover the fee)", err)
	}
	if submits.Load() != 0 {
		t.Fatalf("submit called %d times, want 0 — a fee the account can't pay must not reach the network", submits.Load())
	}
}

// TestConcurrentNativeAndTokenSendsSerializeFeeCheck covers the token send
// path sharing the parent's sendMtx with native sends. A token payment is
// still funded in native XRP, so a concurrent native Send draining the
// account must not let a token send authorize its fee against a stale,
// pre-drain balance snapshot. With both paths serialized through sendMtx,
// exactly one of two concurrent sends — one native, one token — must see the
// drained balance and be rejected.
func TestConcurrentNativeAndTokenSendsSerializeFeeCheck(t *testing.T) {
	const nativeDest = "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"
	tokenDestPriv := make([]byte, 32)
	tokenDestPriv[31] = 2
	_, tokenDestPub := btcec.PrivKeyFromBytes(tokenDestPriv)
	tokenDest := pubKeyToAddress(tokenDestPub)

	m := newMockRippled(t)
	tw := makeTokenTestWallet(t, m)
	w := tw.parent
	w.baseReserve = 0
	w.ownerReserve = 0
	w.nextSeq = 7

	var (
		selfCurrentCalls  atomic.Int32
		inCriticalSection atomic.Bool
	)
	m.handle("account_info", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		if p["account"] != w.address {
			// account_info lookup for the native send's destination.
			return accountInfoResponse("100000000", 0, 0), nil
		}
		if p["ledger_index"] != "current" {
			// Validated-ledger query for the wallet's own account: value is
			// irrelevant since strictCurrent always authorizes off the
			// current-ledger figure below.
			return accountInfoResponse("13", 0, 0), nil
		}
		// This is the query Send authorizes against. Detect overlap
		// directly — with sendMtx serializing both paths, no two sends
		// should ever be inside this query at once — rather than only
		// inferring serialization from which balance figure came back.
		if !inCriticalSection.CompareAndSwap(false, true) {
			t.Errorf("two sends queried the current ledger for %s concurrently — sendMtx did not serialize them", w.address)
		}
		time.Sleep(20 * time.Millisecond) // widen the window so a missing lock is reliably caught
		n := selfCurrentCalls.Add(1)
		inCriticalSection.Store(false)
		// Whichever send acquires sendMtx first sees the pre-send balance;
		// the second sees it drained — exactly as a real submit would leave
		// it.
		if n == 1 {
			return accountInfoResponse("13", 0, 0), nil
		}
		return accountInfoResponse("0", 0, 0), nil
	})
	m.handle("account_lines", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		if p["account"] == tokenDest {
			return accountLinesResponse([]accountLine{{
				Account:  tw.issuerAddr,
				Balance:  "0",
				Currency: TokenUSDC.Currency,
				Limit:    "999999999.999999",
			}}), nil
		}
		return accountLinesResponse([]accountLine{{
			Account:  tw.issuerAddr,
			Balance:  "100",
			Currency: TokenUSDC.Currency,
			Limit:    "999999999.999999",
		}}), nil
	})
	m.handle("fee", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":               "success",
			"drops":                map[string]any{"open_ledger_fee": "12"},
			"ledger_current_index": uint64(100),
		}, nil
	})
	var submits atomic.Int32
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		submits.Add(1)
		return map[string]any{"status": "success", "engine_result": "tesSUCCESS"}, nil
	})

	nativeErr := make(chan error, 1)
	tokenErr := make(chan error, 1)
	start := make(chan struct{})
	go func() {
		<-start
		_, err := w.Send(nativeDest, 1, 0)
		nativeErr <- err
	}()
	go func() {
		<-start
		_, err := tw.Send(tokenDest, 1_000_000, 0)
		tokenErr <- err
	}()
	close(start)

	nErr, tErr := <-nativeErr, <-tokenErr
	var success, insufficient int
	for _, err := range []error{nErr, tErr} {
		switch {
		case err == nil:
			success++
		case errors.Is(err, asset.ErrInsufficientBalance):
			insufficient++
		default:
			t.Fatalf("unexpected send error: %v", err)
		}
	}
	if success != 1 || insufficient != 1 {
		t.Fatalf("results: success=%d insufficient=%d (native=%v token=%v), want 1 each", success, insufficient, nErr, tErr)
	}
	if submits.Load() != 1 {
		t.Fatalf("submit called %d times, want 1 — sendMtx must stop the loser from reaching the network", submits.Load())
	}
}

// TestTokenWalletTransactionLookupCaseInsensitive covers the token path
// skipping the hash normalization the native path applies. A lowercase hash —
// what an explorer or a user paste supplies — missed the DB entirely and fell
// through to the RPC, losing the token AssetID and any pending state the stored record
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
		Confirmed: true, Recipient: &recip, NetworkID: BipID, AssetID: assetID,
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
	if wt.AssetID != assetID {
		t.Errorf("AssetID = %v, want %d — the DB record was not found", wt.AssetID, assetID)
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
	if stored.AssetID != tw.assetID {
		t.Errorf("AssetID = %v, want %d", stored.AssetID, tw.assetID)
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
		Recipient: &recip, NetworkID: BipID, AssetID: assetID,
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
	if stored.AssetID != assetID {
		t.Errorf("AssetID = %v, want %d", stored.AssetID, assetID)
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
// than on the record's AssetID — so a token transaction would be filed against
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
	if stored == nil || stored.AssetID == BipID {
		t.Fatal("token receive should still be recorded")
	}
	for {
		select {
		case n := <-noteChan:
			if tn, ok := n.(*asset.TransactionNote); ok {
				t.Errorf("token record announced on the native channel: noteAssetID=%d txAssetID=%d",
					tn.AssetID, tn.Transaction.AssetID)
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
	if stored == nil || stored.AssetID == BipID {
		t.Fatal("scan should have stored the token receive")
	}
	for {
		select {
		case note := <-noteChan:
			if tn, ok := note.(*asset.TransactionNote); ok {
				t.Errorf("scan announced a token record on the native channel: noteAssetID=%d txAssetID=%d",
					tn.AssetID, tn.Transaction.AssetID)
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
			Recipient: &recip, NetworkID: BipID, AssetID: assetID,
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
				t.Errorf("checkPendingTxs announced a token record on the native channel: id=%s noteAssetID=%d txAssetID=%d",
					tn.Transaction.ID, tn.AssetID, tn.Transaction.AssetID)
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
// emitter's asset ID rather than the record's AssetID.
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
	connectTestHandlers(m)
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

	// Connect registers the token wallet with the parent.
	ctx, cancel := context.WithCancel(context.Background())
	wg, err := tw.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	assetID := tw.assetID
	recip := "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"
	wtx := &txdb.WalletTransaction{
		Type: txdb.Send, ID: txID, Amount: 1_000_000, Fees: 12,
		Recipient: &recip, NetworkID: BipID, AssetID: assetID,
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
	w.tokenMtx.RLock()
	_, still := w.tokenWallets[tw.assetID]
	w.tokenMtx.RUnlock()
	if still {
		t.Error("token wallet still registered after its context ended")
	}
}

// connectTestHandlers serves the queries TokenWallet.Connect makes to prime
// the parent's account state and the trust-line cache: an existing account
// with no trust line.
func connectTestHandlers(m *mockRippled) {
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("100000000", 0, 0), nil
	})
	m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
		return accountLinesResponse(nil), nil
	})
}

func TestTokenWalletEstimateSendTxFee(t *testing.T) {
	const destAddr = "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"

	t.Run("sufficient trustline balance and native XRP", func(t *testing.T) {
		m := newMockRippled(t)
		m.handle("fee", feeHandler("15"))
		m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
			return accountInfoResponse("100000000", 0, 0), nil
		})
		m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
			return accountLinesResponse([]accountLine{{
				Account:  TokenUSDC.NetIssuers[util.Mainnet],
				Currency: TokenUSDC.Currency,
				Balance:  "1000",
				Limit:    "999999999.999999",
			}}), nil
		})
		tw := makeTokenTestWallet(t, m)

		fee, valid, err := tw.EstimateSendTxFee(destAddr, 1_000_000, 0, false, false)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if !valid {
			t.Error("expected isValidAddress=true")
		}
		if fee != 15 {
			t.Errorf("fee = %d, want 15", fee)
		}
	})

	t.Run("no native XRP to cover the fee", func(t *testing.T) {
		m := newMockRippled(t)
		m.handle("fee", feeHandler("15"))
		m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
			return accountInfoResponse("0", 0, 0), nil
		})
		m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
			return accountLinesResponse([]accountLine{{
				Account:  TokenUSDC.NetIssuers[util.Mainnet],
				Currency: TokenUSDC.Currency,
				Balance:  "1000",
				Limit:    "999999999.999999",
			}}), nil
		})
		tw := makeTokenTestWallet(t, m)

		_, _, err := tw.EstimateSendTxFee(destAddr, 1_000_000, 0, false, false)
		if err != asset.ErrInsufficientBalance {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
	})

	t.Run("insufficient trustline balance", func(t *testing.T) {
		m := newMockRippled(t)
		m.handle("fee", feeHandler("15"))
		m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
			return accountInfoResponse("100000000", 0, 0), nil
		})
		m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
			return accountLinesResponse([]accountLine{{
				Account:  TokenUSDC.NetIssuers[util.Mainnet],
				Currency: TokenUSDC.Currency,
				Balance:  "0.5", // 500000 atomic (USDC has 6 decimals) < value
				Limit:    "999999999.999999",
			}}), nil
		})
		tw := makeTokenTestWallet(t, m)

		_, _, err := tw.EstimateSendTxFee(destAddr, 1_000_000, 0, false, false)
		if err != asset.ErrInsufficientBalance {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
	})

	t.Run("missing trustline", func(t *testing.T) {
		m := newMockRippled(t)
		m.handle("fee", feeHandler("15"))
		m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
			return accountInfoResponse("100000000", 0, 0), nil
		})
		m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
			return accountLinesResponse(nil), nil
		})
		tw := makeTokenTestWallet(t, m)

		_, _, err := tw.EstimateSendTxFee(destAddr, 1_000_000, 0, false, false)
		if err != asset.ErrInsufficientBalance {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
	})

	t.Run("invalid address reported via isValidAddress, not an error", func(t *testing.T) {
		m := newMockRippled(t)
		m.handle("fee", feeHandler("15"))
		m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
			return accountInfoResponse("100000000", 0, 0), nil
		})
		m.handle("account_lines", func(_ json.RawMessage) (any, *rpcError) {
			return accountLinesResponse([]accountLine{{
				Account:  TokenUSDC.NetIssuers[util.Mainnet],
				Currency: TokenUSDC.Currency,
				Balance:  "1000",
				Limit:    "999999999.999999",
			}}), nil
		})
		tw := makeTokenTestWallet(t, m)

		fee, valid, err := tw.EstimateSendTxFee("not-an-address", 1_000_000, 0, false, false)
		if err != nil {
			t.Fatalf("EstimateSendTxFee: %v", err)
		}
		if valid {
			t.Error("expected isValidAddress=false")
		}
		if fee != 15 {
			t.Errorf("fee = %d, want 15", fee)
		}
	})
}
