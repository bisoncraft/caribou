// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package xrp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/txdb"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	btcec "github.com/btcsuite/btcd/btcec/v2"
)

// mockRippled is an httptest server that responds to rippled JSON-RPC calls.
// Register a handler per method; unregistered methods fatal the test.
type mockRippled struct {
	t        *testing.T
	handlers map[string]func(params json.RawMessage) (any, *rpcError)
	srv      *httptest.Server
}

func newMockRippled(t *testing.T) *mockRippled {
	m := &mockRippled{
		t:        t,
		handlers: make(map[string]func(json.RawMessage) (any, *rpcError)),
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockRippled) handle(method string, h func(params json.RawMessage) (any, *rpcError)) {
	m.handlers[method] = h
}

func (m *mockRippled) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		m.t.Errorf("mockRippled: decode request: %v", err)
		http.Error(w, "bad request", 400)
		return
	}
	h, ok := m.handlers[req.Method]
	if !ok {
		m.t.Errorf("mockRippled: unhandled method %q", req.Method)
		http.Error(w, "not handled", 500)
		return
	}
	var p json.RawMessage
	if len(req.Params) > 0 {
		p = req.Params[0]
	}
	result, rpcErr := h(p)

	type envelope struct {
		Result any `json:"result"`
	}
	var resp any
	if rpcErr != nil {
		resp = envelope{Result: map[string]any{
			"status":        "error",
			"error":         rpcErr.errType,
			"error_code":    rpcErr.code,
			"error_message": rpcErr.msg,
		}}
	} else {
		type resultWithStatus struct {
			Status string `json:"status"`
		}
		if result == nil {
			result = resultWithStatus{Status: "success"}
		}
		resp = envelope{Result: result}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (m *mockRippled) rpcClient() *rpcClient {
	return newRPCClient(m.srv.URL, util.NewLogger("mock", util.LevelError, nil))
}

// ---- account_info tests ----

func TestRPCAccountInfo(t *testing.T) {
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"account_data": map[string]any{
				"Account":    "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
				"Balance":    "10000000",
				"Sequence":   uint32(42),
				"OwnerCount": uint32(0),
				"Flags":      uint32(0),
			},
			"ledger_index": uint64(100),
		}, nil
	})
	c := m.rpcClient()
	info, err := c.accountInfo(context.Background(), "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh")
	if err != nil {
		t.Fatalf("accountInfo: %v", err)
	}
	if info.AccountData.Balance != "10000000" {
		t.Errorf("Balance = %q, want 10000000", info.AccountData.Balance)
	}
	if info.AccountData.Sequence != 42 {
		t.Errorf("Sequence = %d, want 42", info.AccountData.Sequence)
	}
}

func TestRPCAccountInfoNotFound(t *testing.T) {
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{errType: "actNotFound", msg: "account not found"}
	})
	c := m.rpcClient()
	_, err := c.accountInfo(context.Background(), "rNonExistent")
	if !isAccountNotFound(err) {
		t.Fatalf("expected actNotFound, got %v", err)
	}
}

// ---- account_tx tests ----

func TestRPCAccountTxEmpty(t *testing.T) {
	m := newMockRippled(t)
	m.handle("account_tx", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":       "success",
			"transactions": []any{},
		}, nil
	})
	c := m.rpcClient()
	atr, err := c.accountTx(context.Background(), "rAddr", 1, 100, nil)
	if err != nil {
		t.Fatalf("accountTx: %v", err)
	}
	if len(atr.Transactions) != 0 {
		t.Errorf("expected 0 txs, got %d", len(atr.Transactions))
	}
	if atr.Marker != nil {
		t.Error("expected nil marker")
	}
}

func TestRPCAccountTxMultiPage(t *testing.T) {
	callCount := 0
	m := newMockRippled(t)
	m.handle("account_tx", func(params json.RawMessage) (any, *rpcError) {
		callCount++
		if callCount == 1 {
			return map[string]any{
				"status":       "success",
				"transactions": []any{},
				"marker":       map[string]any{"ledger": 50, "seq": 1},
			}, nil
		}
		return map[string]any{
			"status":       "success",
			"transactions": []any{},
		}, nil
	})
	c := m.rpcClient()
	_, err := c.accountTx(context.Background(), "rAddr", 1, 100, nil)
	if err != nil {
		t.Fatalf("accountTx page 1: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call, got %d", callCount)
	}
	// The second call to accountTx exercises the marker passing.
	atr1, _ := c.accountTx(context.Background(), "rAddr", 1, 100, nil)
	atr2, err := c.accountTx(context.Background(), "rAddr", 1, 100, atr1.Marker)
	if err != nil {
		t.Fatalf("accountTx page 2: %v", err)
	}
	if atr2.Marker != nil {
		t.Error("expected nil marker on final page")
	}
}

// ---- submit tests ----

func TestRPCSubmitSuccess(t *testing.T) {
	m := newMockRippled(t)
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":                "success",
			"engine_result":         "tesSUCCESS",
			"engine_result_message": "The transaction was applied.",
			"tx_json":               map[string]any{"hash": "AABBCCDD" + "00000000" + "00000000" + "00000000" + "00000000" + "00000000" + "00000000" + "00000000"},
		}, nil
	})
	c := m.rpcClient()
	sr, err := c.submit(context.Background(), "DEADBEEF")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if sr.EngineResult != "tesSUCCESS" {
		t.Errorf("EngineResult = %q, want tesSUCCESS", sr.EngineResult)
	}
}

func TestRPCSubmitEngineClasses(t *testing.T) {
	for _, engineResult := range []string{
		"tesSUCCESS",
		"tecNO_DST",
		"terQUEUED",
		"tefBAD_AUTH",
		"temDISABLED",
		"telINSUF_FEE_P",
	} {
		er := engineResult
		m := newMockRippled(t)
		m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
			return map[string]any{
				"status":        "success",
				"engine_result": er,
				"tx_json":       map[string]any{"hash": "0000000000000000000000000000000000000000000000000000000000000000"},
			}, nil
		})
		c := m.rpcClient()
		sr, err := c.submit(context.Background(), "BLOB")
		if err != nil {
			t.Fatalf("%s: submit error: %v", er, err)
		}
		if sr.EngineResult != er {
			t.Errorf("%s: got EngineResult %q", er, sr.EngineResult)
		}
	}
}

func TestRPCSubmitNetworkError(t *testing.T) {
	m := newMockRippled(t)
	// Register handler but immediately close server to simulate network error.
	m.handle("submit", func(_ json.RawMessage) (any, *rpcError) { return nil, nil })
	m.srv.Close()

	c := m.rpcClient()
	_, err := c.submit(context.Background(), "BLOB")
	if err == nil {
		t.Fatal("expected error from closed server")
	}
}

// ---- tx tests ----

func TestRPCTxValidated(t *testing.T) {
	const txHash = "AABB000000000000000000000000000000000000000000000000000000001234"
	m := newMockRippled(t)
	m.handle("tx", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":          "success",
			"hash":            txHash,
			"validated":       true,
			"ledger_index":    uint64(200),
			"TransactionType": "Payment",
			"Account":         "rSender",
			"Destination":     "rReceiver",
			"Amount":          "1000000",
			"Fee":             "12",
			"meta": map[string]any{
				"TransactionResult": "tesSUCCESS",
			},
		}, nil
	})
	c := m.rpcClient()
	tr, err := c.tx(context.Background(), txHash)
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	if !tr.Validated {
		t.Error("expected Validated=true")
	}
	if tr.LedgerIndex != 200 {
		t.Errorf("LedgerIndex = %d, want 200", tr.LedgerIndex)
	}
}

func TestRPCTxNotFound(t *testing.T) {
	m := newMockRippled(t)
	m.handle("tx", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{errType: "txnNotFound", msg: "Transaction not found."}
	})
	c := m.rpcClient()
	_, err := c.tx(context.Background(), "NOTFOUND")
	if !isTxNotFound(err) {
		t.Fatalf("expected txnNotFound, got %v", err)
	}
}

// ---- server_info tests ----

func TestRPCServerInfo(t *testing.T) {
	m := newMockRippled(t)
	m.handle("server_info", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": "success",
			"info": map[string]any{
				"server_state": "full",
				"validated_ledger": map[string]any{
					"seq": uint64(500),
					"age": float64(2),
				},
			},
		}, nil
	})
	c := m.rpcClient()
	si, err := c.serverInfo(context.Background())
	if err != nil {
		t.Fatalf("serverInfo: %v", err)
	}
	if si.Info.ValidatedLedger.Seq != 500 {
		t.Errorf("Seq = %d, want 500", si.Info.ValidatedLedger.Seq)
	}
}

// ---- context cancellation ----

func TestRPCContextCancel(t *testing.T) {
	m := newMockRippled(t)
	m.handle("fee", func(_ json.RawMessage) (any, *rpcError) {
		time.Sleep(200 * time.Millisecond) // slow response
		return map[string]any{"status": "success", "drops": map[string]any{"open_ledger_fee": "12"}, "ledger_current_index": uint64(1)}, nil
	})
	c := m.rpcClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.fee(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// ---- Send to unfunded destination ----

// TestSendToUnfundedDestination verifies that Send does not fail when the
// destination account has never been funded (actNotFound from accountInfo).
// Sending to a new address is the canonical XRP account-activation flow and
// must be allowed; a non-existent account cannot have lsfRequireDestTag set.
func TestSendToUnfundedDestination(t *testing.T) {
	// mockInfoHandler returns actNotFound for validated-ledger account_info
	// (destination does not exist) and sender info for current-ledger lookups
	// (sequence initialization).
	mockInfoHandler := func(m *mockRippled) {
		m.handlers["account_info"] = func(params json.RawMessage) (any, *rpcError) {
			var p map[string]any
			_ = json.Unmarshal(params, &p)
			if li, _ := p["ledger_index"].(string); li == "current" {
				return map[string]any{
					"status": "success",
					"account_data": map[string]any{
						"Account":    "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
						"Balance":    "100000000",
						"Sequence":   uint32(5),
						"OwnerCount": uint32(0),
						"Flags":      uint32(0),
					},
					"ledger_index": uint64(100),
				}, nil
			}
			return nil, &rpcError{errType: "actNotFound", msg: "account not found"}
		}
	}

	log := util.NewLogger("test", util.LevelError, nil)
	privKeyBytes := make([]byte, 32)
	privKeyBytes[31] = 1
	privKey, pubKey := btcec.PrivKeyFromBytes(privKeyBytes)
	const destAddr = "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"

	t.Run("at base reserve succeeds", func(t *testing.T) {
		m := newMockRippled(t)
		mockInfoHandler(m)
		m.handle("fee", func(_ json.RawMessage) (any, *rpcError) {
			return map[string]any{
				"status":               "success",
				"drops":                map[string]any{"open_ledger_fee": "12"},
				"ledger_current_index": uint64(100),
			}, nil
		})
		m.handle("submit", func(_ json.RawMessage) (any, *rpcError) {
			return map[string]any{
				"status":                "success",
				"engine_result":         "tesSUCCESS",
				"engine_result_message": "The transaction was applied.",
				"tx_json":               map[string]any{"hash": "0000000000000000000000000000000000000000000000000000000000000001"},
			}, nil
		})
		w := &xrpWallet{
			address:     pubKeyToAddress(pubKey),
			pubKey:      pubKey,
			privKey:     privKey,
			rpc:         m.rpcClient(),
			log:         log,
			baseReserve: BaseReserveDrops,
			ctx:         context.Background(),
		}
		w.running.Store(true)
		if _, err := w.Send(destAddr, BaseReserveDrops, 0); err != nil {
			t.Fatalf("Send at base reserve: %v", err)
		}
	})

	t.Run("below base reserve rejected", func(t *testing.T) {
		m := newMockRippled(t)
		mockInfoHandler(m)
		w := &xrpWallet{
			address:     pubKeyToAddress(pubKey),
			pubKey:      pubKey,
			privKey:     privKey,
			rpc:         m.rpcClient(),
			log:         log,
			baseReserve: BaseReserveDrops,
			ctx:         context.Background(),
		}
		w.running.Store(true)
		_, err := w.Send(destAddr, BaseReserveDrops-1, 0)
		if err == nil {
			t.Fatal("expected error sending below base reserve to unfunded address")
		}
		if !strings.Contains(err.Error(), "base reserve") {
			t.Errorf("error should mention base reserve, got: %v", err)
		}
	})
}

// ---- Balance ----

func makeBalanceWallet(t *testing.T, m *mockRippled) *xrpWallet {
	t.Helper()
	w := &xrpWallet{
		address:      "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
		rpc:          m.rpcClient(),
		log:          util.NewLogger("test", util.LevelError, nil),
		baseReserve:  BaseReserveDrops,
		ownerReserve: OwnerReserveDrops,
		ctx:          context.Background(),
	}
	w.running.Store(true)
	return w
}

func accountInfoResponse(balance string, ownerCount uint32, flags uint32) map[string]any {
	return map[string]any{
		"status": "success",
		"account_data": map[string]any{
			"Account":    "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
			"Balance":    balance,
			"Sequence":   uint32(1),
			"OwnerCount": ownerCount,
			"Flags":      flags,
		},
		"ledger_index": uint64(100),
	}
}

func TestBalanceNotConnected(t *testing.T) {
	w := &xrpWallet{}
	w.running.Store(true)
	if _, err := w.Balance(); err == nil {
		t.Fatal("expected error when rpc is nil")
	}
}

func TestBalanceNotRunning(t *testing.T) {
	m := newMockRippled(t)
	w := makeBalanceWallet(t, m)
	w.running.Store(false)
	if _, err := w.Balance(); err == nil {
		t.Fatal("expected error when wallet not running")
	}
}

func TestBalanceUnfundedAccount(t *testing.T) {
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{errType: "actNotFound", msg: "account not found"}
	})
	w := makeBalanceWallet(t, m)
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 0 || bal.Locked != 0 {
		t.Errorf("unfunded: want {0 0}, got {%d %d}", bal.Available, bal.Locked)
	}
}

func TestBalanceFunded(t *testing.T) {
	// 10 XRP validated, 10 XRP current, 0 owned objects.
	// Reserve = BaseReserveDrops (1_000_000). Available = 10_000_000 - 1_000_000 = 9_000_000.
	m := newMockRippled(t)
	var callCount atomic.Int32
	m.handle("account_info", func(params json.RawMessage) (any, *rpcError) {
		callCount.Add(1)
		return accountInfoResponse("10000000", 0, 0), nil
	})
	w := makeBalanceWallet(t, m)
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 9_000_000 {
		t.Errorf("available = %d, want 9_000_000", bal.Available)
	}
	if bal.Locked != BaseReserveDrops {
		t.Errorf("locked = %d, want %d", bal.Locked, BaseReserveDrops)
	}
	if n := callCount.Load(); n != 2 {
		t.Errorf("expected 2 account_info calls (validated + current), got %d", n)
	}
}

func TestBalancePendingSendsReduceAvailable(t *testing.T) {
	// validated=10 XRP, current=8 XRP (two 1 XRP sends are queued).
	// Reserve=1_000_000. Available = 8_000_000 - 1_000_000 = 7_000_000.
	m := newMockRippled(t)
	m.handle("account_info", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		if p["ledger_index"] == "current" {
			return accountInfoResponse("8000000", 0, 0), nil
		}
		return accountInfoResponse("10000000", 0, 0), nil
	})
	w := makeBalanceWallet(t, m)
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 7_000_000 {
		t.Errorf("available = %d, want 7_000_000", bal.Available)
	}
}

func TestBalanceCurrentLedgerFallback(t *testing.T) {
	// accountInfoCurrent fails; should fall back to validated balance.
	m := newMockRippled(t)
	m.handle("account_info", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		if p["ledger_index"] == "current" {
			return nil, &rpcError{errType: "serverTimeout", msg: "timeout"}
		}
		return accountInfoResponse("5000000", 0, 0), nil
	})
	w := makeBalanceWallet(t, m)
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	// Fallback: both balances use validated (5_000_000). Available = 5M - 1M = 4M.
	if bal.Available != 4_000_000 {
		t.Errorf("available = %d, want 4_000_000 (fallback to validated)", bal.Available)
	}
}

func TestBalanceBelowReserve(t *testing.T) {
	// Balance equals exactly the reserve; available must be 0.
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("1000000", 0, 0), nil
	})
	w := makeBalanceWallet(t, m)
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 0 {
		t.Errorf("available = %d, want 0 when balance == reserve", bal.Available)
	}
	if bal.Locked != BaseReserveDrops {
		t.Errorf("locked = %d, want %d", bal.Locked, BaseReserveDrops)
	}
}

// TestBalanceBelowReserveClampsLocked covers Locked being reported unclamped.
// An account funded below its reserve — which happens as soon as it acquires an
// owned object it can no longer cover — reported more locked than it holds, so
// Available+Locked exceeded the account balance.
func TestBalanceBelowReserveClampsLocked(t *testing.T) {
	// 1.1 XRP with one owned object: reserve is 1_200_000 drops, above the
	// 1_100_000 balance.
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("1100000", 1, 0), nil
	})
	w := makeBalanceWallet(t, m)
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 0 {
		t.Errorf("available = %d, want 0 when balance < reserve", bal.Available)
	}
	if bal.Locked != 1_100_000 {
		t.Errorf("locked = %d, want the whole 1100000 drop balance", bal.Locked)
	}
	if bal.Available+bal.Locked > 1_100_000 {
		t.Errorf("available+locked = %d, more than the account holds", bal.Available+bal.Locked)
	}
}

func TestBalanceOwnerReserve(t *testing.T) {
	// 10 XRP, 2 owned objects. Reserve = 1_000_000 + 2*200_000 = 1_400_000.
	m := newMockRippled(t)
	m.handle("account_info", func(_ json.RawMessage) (any, *rpcError) {
		return accountInfoResponse("10000000", 2, 0), nil
	})
	w := makeBalanceWallet(t, m)
	bal, err := w.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	wantReserve := uint64(BaseReserveDrops + 2*OwnerReserveDrops)
	if bal.Locked != wantReserve {
		t.Errorf("locked = %d, want %d", bal.Locked, wantReserve)
	}
	if bal.Available != 10_000_000-wantReserve {
		t.Errorf("available = %d, want %d", bal.Available, 10_000_000-wantReserve)
	}
}

// ---- WalletTransaction ----

func makeWalletTransactionWallet(t *testing.T, m *mockRippled) *xrpWallet {
	t.Helper()
	log := util.NewLogger("test", util.LevelError, nil)
	n, mt := newTestTxDB(t)
	w := &xrpWallet{
		address:   "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
		rpc:       m.rpcClient(),
		txDB:      n,
		metaTable: mt,
		log:       log,
	}
	w.running.Store(true)
	return w
}

func TestWalletTransactionNotConnected(t *testing.T) {
	w := &xrpWallet{}
	w.running.Store(true)
	if _, err := w.WalletTransaction(context.Background(), "AA"); err == nil {
		t.Fatal("expected error when rpc is nil")
	}
}

func TestWalletTransactionNotRunning(t *testing.T) {
	m := newMockRippled(t)
	w := makeWalletTransactionWallet(t, m)
	w.running.Store(false)
	if _, err := w.WalletTransaction(context.Background(), "AA"); err == nil {
		t.Fatal("expected error when wallet not running")
	}
}

func TestWalletTransactionFromDB(t *testing.T) {
	m := newMockRippled(t)
	w := makeWalletTransactionWallet(t, m)

	const txID = "AAAA000000000000000000000000000000000000000000000000000000000001"
	recip := "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"
	stored := &txdb.WalletTransaction{
		Type:        txdb.Send,
		ID:          txID,
		Amount:      1_000_000,
		Fees:        12,
		BlockNumber: 500,
		Confirmed:   true,
		Recipient:   &recip,
	}
	if err := w.txDB.StoreTx(stored); err != nil {
		t.Fatalf("storeTx: %v", err)
	}

	// DB hit — RPC must not be called.
	m.handle("tx", func(_ json.RawMessage) (any, *rpcError) {
		t.Error("tx RPC should not be called when tx is in DB")
		return nil, nil
	})

	wt, err := w.WalletTransaction(context.Background(), txID)
	if err != nil {
		t.Fatalf("WalletTransaction: %v", err)
	}
	if wt.ID != txID || wt.Amount != 1_000_000 || !wt.Confirmed {
		t.Errorf("unexpected result: %+v", wt)
	}
}

func TestWalletTransactionDBLookupCaseInsensitive(t *testing.T) {
	// txIDs stored uppercase must be found when queried lowercase.
	m := newMockRippled(t)
	w := makeWalletTransactionWallet(t, m)

	const txID = "BBBB000000000000000000000000000000000000000000000000000000000002"
	recip := "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"
	if err := w.txDB.StoreTx(&txdb.WalletTransaction{
		Type: txdb.Send, ID: txID, Amount: 500, BlockNumber: 10, Confirmed: true, Recipient: &recip,
	}); err != nil {
		t.Fatalf("storeTx: %v", err)
	}

	wt, err := w.WalletTransaction(context.Background(), strings.ToLower(txID))
	if err != nil {
		t.Fatalf("WalletTransaction (lowercase): %v", err)
	}
	if wt.ID != txID {
		t.Errorf("ID = %q, want %q", wt.ID, txID)
	}
}

func TestWalletTransactionFromRPC(t *testing.T) {
	const txID = "CCCC000000000000000000000000000000000000000000000000000000000003"
	const walletAddr = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	const otherAddr = "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe"

	m := newMockRippled(t)
	m.handle("tx", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":          "success",
			"hash":            txID,
			"validated":       true,
			"ledger_index":    uint64(300),
			"TransactionType": "Payment",
			"Account":         walletAddr,
			"Destination":     otherAddr,
			"Amount":          "2000000",
			"Fee":             "12",
			"meta":            map[string]any{"TransactionResult": "tesSUCCESS"},
		}, nil
	})

	w := makeWalletTransactionWallet(t, m)
	wt, err := w.WalletTransaction(context.Background(), txID)
	if err != nil {
		t.Fatalf("WalletTransaction: %v", err)
	}
	if wt.Type != txdb.Send || wt.Amount != 2_000_000 || wt.BlockNumber != 300 || !wt.Confirmed {
		t.Errorf("unexpected result: %+v", wt)
	}
}

func TestWalletTransactionNotFound(t *testing.T) {
	m := newMockRippled(t)
	m.handle("tx", func(_ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{errType: "txnNotFound", msg: "Transaction not found."}
	})
	w := makeWalletTransactionWallet(t, m)
	_, err := w.WalletTransaction(context.Background(), "DDDD")
	if !errors.Is(err, asset.CoinNotFoundError) {
		t.Errorf("expected CoinNotFoundError, got %v", err)
	}
}

func TestWalletTransactionNonPayment(t *testing.T) {
	// A validated non-Payment tx in the RPC response must yield CoinNotFoundError.
	m := newMockRippled(t)
	m.handle("tx", func(_ json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status":          "success",
			"hash":            "EEEE",
			"validated":       true,
			"ledger_index":    uint64(400),
			"TransactionType": "AccountSet",
			"Account":         "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
			"Fee":             "12",
			"meta":            map[string]any{"TransactionResult": "tesSUCCESS"},
		}, nil
	})
	w := makeWalletTransactionWallet(t, m)
	_, err := w.WalletTransaction(context.Background(), "EEEE")
	if !errors.Is(err, asset.CoinNotFoundError) {
		t.Errorf("expected CoinNotFoundError for non-payment tx, got %v", err)
	}
}

// ---- feeDrops sentinel ----

func TestFeeDropsSentinel(t *testing.T) {
	// feeDrops returns 0 when unparseable; callers must substitute DefaultFeeDrops.
	fr := &feeResult{}
	fr.Drops.OpenLedgerFee = "notanumber"
	if got := fr.feeDrops(); got != 0 {
		t.Errorf("feeDrops(invalid) = %d, want 0", got)
	}
	fr.Drops.OpenLedgerFee = "0"
	if got := fr.feeDrops(); got != 0 {
		t.Errorf("feeDrops(0) = %d, want 0", got)
	}
	fr.Drops.OpenLedgerFee = "250"
	if got := fr.feeDrops(); got != 250 {
		t.Errorf("feeDrops(250) = %d, want 250", got)
	}
}
