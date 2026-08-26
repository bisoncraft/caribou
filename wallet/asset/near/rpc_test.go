package near

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
)

var testLogger = util.StdOutLogger("NEARTEST", util.LevelError)

// fakeRPCServer is a scriptable NEAR JSON-RPC server for driving the client
// and wallet without a network.
type fakeRPCServer struct {
	t   *testing.T
	srv *httptest.Server

	mtx      sync.Mutex
	handlers map[string]func(params json.RawMessage) (any, *rpcError)
	calls    map[string]int
	reqs     int  // all requests, including those failed with httpErr
	httpErr  bool // respond 500 to everything
}

func newFakeRPCServer(t *testing.T) *fakeRPCServer {
	f := &fakeRPCServer{
		t:        t,
		handlers: make(map[string]func(json.RawMessage) (any, *rpcError)),
		calls:    make(map[string]int),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mtx.Lock()
		f.reqs++
		httpErr := f.httpErr
		f.mtx.Unlock()
		if httpErr {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mtx.Lock()
		f.calls[req.Method]++
		handler := f.handlers[req.Method]
		f.mtx.Unlock()

		resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
		if handler == nil {
			resp.Error = &rpcError{Code: jsonRPCMethodNotFound, Message: "Method not found"}
		} else {
			params, _ := json.Marshal(req.Params)
			result, rpcErr := handler(params)
			if rpcErr != nil {
				resp.Error = rpcErr
			} else {
				b, err := json.Marshal(result)
				if err != nil {
					t.Errorf("error marshaling result: %v", err)
				}
				resp.Result = b
			}
		}
		if err := json.NewEncoder(w).Encode(&resp); err != nil {
			t.Errorf("error encoding response: %v", err)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRPCServer) handle(method string, handler func(json.RawMessage) (any, *rpcError)) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.handlers[method] = handler
}

func (f *fakeRPCServer) callCount(method string) int {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	return f.calls[method]
}

func (f *fakeRPCServer) reqCount() int {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	return f.reqs
}

func (f *fakeRPCServer) setHTTPErr(fail bool) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.httpErr = fail
}

func blockResult(height uint64) any {
	return map[string]any{
		"header": map[string]any{
			"height":    height,
			"hash":      "11111111111111111111111111111111",
			"timestamp": 1_700_000_000_000_000_000,
		},
	}
}

func TestRPCFailover(t *testing.T) {
	bad := newFakeRPCServer(t)
	bad.setHTTPErr(true)
	good := newFakeRPCServer(t)
	good.handle("block", func(json.RawMessage) (any, *rpcError) {
		return blockResult(100), nil
	})

	c, err := newRPCClient([]string{bad.srv.URL, good.srv.URL}, "", testLogger)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}

	block, err := c.getBlock(context.Background(), "final")
	if err != nil {
		t.Fatalf("getBlock with one endpoint down: %v", err)
	}
	if block.Header.Height != 100 {
		t.Fatalf("wrong height %d", block.Header.Height)
	}

	// The healthy endpoint is now active and the bad one is cooling: further
	// calls must not touch the bad endpoint.
	if _, err := c.getBlock(context.Background(), "final"); err != nil {
		t.Fatalf("second getBlock: %v", err)
	}
	if n := good.callCount("block"); n != 2 {
		t.Fatalf("expected 2 calls to healthy endpoint, got %d", n)
	}
}

// TestRPCOversizedResponse covers the response-body cap: an endpoint
// returning an over-limit body is an endpoint failure, so failover
// engages rather than the body ballooning memory.
func TestRPCOversizedResponse(t *testing.T) {
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(make([]byte, rpcResponseSizeLimit+1))
	}))
	t.Cleanup(huge.Close)
	good := newFakeRPCServer(t)
	good.handle("block", func(json.RawMessage) (any, *rpcError) {
		return blockResult(100), nil
	})

	c, err := newRPCClient([]string{huge.URL, good.srv.URL}, "", testLogger)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}
	block, err := c.getBlock(context.Background(), "final")
	if err != nil {
		t.Fatalf("getBlock with oversized endpoint first: %v", err)
	}
	if block.Header.Height != 100 {
		t.Fatalf("wrong height %d", block.Header.Height)
	}

	// With no healthy endpoint the over-limit body is an error, not a
	// giant allocation handed to the JSON decoder.
	solo, err := newRPCClient([]string{huge.URL}, "", testLogger)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}
	if _, err := solo.getBlock(context.Background(), "final"); err == nil {
		t.Fatalf("oversized response accepted")
	}
}

func TestRPCAuthoritativeError(t *testing.T) {
	first := newFakeRPCServer(t)
	first.handle("query", func(json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{
			Name:    "HANDLER_ERROR",
			Cause:   &rpcErrorCause{Name: "UNKNOWN_ACCOUNT"},
			Code:    -32000,
			Message: "Server error",
			Data:    json.RawMessage(`"account does not exist"`),
		}
	})
	second := newFakeRPCServer(t)

	c, err := newRPCClient([]string{first.srv.URL, second.srv.URL}, "", testLogger)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}

	_, err = c.viewAccount(context.Background(), "nobody.near")
	if !isAccountNotFound(err) {
		t.Fatalf("expected account-not-found, got %v", err)
	}
	if n := second.callCount("query"); n != 0 {
		t.Fatalf("server error should not fail over, but second endpoint saw %d calls", n)
	}
}

func TestRPCMethodNotFoundFailover(t *testing.T) {
	partial := newFakeRPCServer(t) // no gas_price handler -> -32601
	full := newFakeRPCServer(t)
	full.handle("gas_price", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"gas_price": "100000000"}, nil
	})

	c, err := newRPCClient([]string{partial.srv.URL, full.srv.URL}, "", testLogger)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}

	price, err := c.gasPrice(context.Background())
	if err != nil {
		t.Fatalf("gasPrice should fail over past method-not-found: %v", err)
	}
	if price.Cmp(big.NewInt(100_000_000)) != 0 {
		t.Fatalf("wrong gas price %s", price)
	}
}

func TestRPCAllEndpointsDown(t *testing.T) {
	a := newFakeRPCServer(t)
	a.setHTTPErr(true)
	b := newFakeRPCServer(t)
	b.setHTTPErr(true)

	c, err := newRPCClient([]string{a.srv.URL, b.srv.URL}, "", testLogger)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}

	if _, err := c.getBlock(context.Background(), "final"); err == nil ||
		!strings.Contains(err.Error(), "all NEAR RPC endpoints failed") {
		t.Fatalf("expected all-endpoints error, got %v", err)
	}

	// Both endpoints are now cooling; the second pass must still attempt
	// them rather than reporting nothing attempted.
	if _, err := c.getBlock(context.Background(), "final"); err == nil ||
		!strings.Contains(err.Error(), "all NEAR RPC endpoints failed") {
		t.Fatalf("expected all-endpoints error on retry, got %v", err)
	}
	if n := a.reqCount() + b.reqCount(); n < 4 {
		t.Fatalf("cooling endpoints were not retried: %d total attempts", n)
	}
}

// newTestWallet builds an unlocked NearWallet wired to the fake server.
func newTestWallet(t *testing.T, f *fakeRPCServer) *NearWallet {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	rpc, err := newRPCClient([]string{f.srv.URL}, "", testLogger)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}
	w := &NearWallet{
		ctx:       context.Background(),
		log:       testLogger,
		net:       util.Simnet,
		privKey:   priv,
		pubKey:    pub,
		accountID: hex.EncodeToString(pub),
	}
	w.rpc.Store(rpc)
	return w
}

// parseSignedTransfer extracts the nonce and transfer deposit from a
// Borsh-serialized signed transfer transaction produced by the wallet. The
// signer is always a 64-character implicit account and the transaction holds
// exactly one Transfer action.
func parseSignedTransfer(t *testing.T, signedTxB64 string) (nonce uint64, deposit *big.Int) {
	raw, err := base64.StdEncoding.DecodeString(signedTxB64)
	if err != nil {
		t.Fatalf("bad signed tx encoding: %v", err)
	}
	txBytes := raw[:len(raw)-65] // strip key type byte + 64-byte signature
	// signerID string (4 + 64), public key (1 + 32), nonce (8).
	nonceOff := 4 + 64 + 33
	nonce = binary.LittleEndian.Uint64(txBytes[nonceOff : nonceOff+8])
	// Transfer deposit is the final u128 of the single action.
	depositB := make([]byte, 16)
	copy(depositB, txBytes[len(txBytes)-16:])
	// Little-endian to big-endian for big.Int.
	for i, j := 0, len(depositB)-1; i < j; i, j = i+1, j-1 {
		depositB[i], depositB[j] = depositB[j], depositB[i]
	}
	return nonce, new(big.Int).SetBytes(depositB)
}

func successOutcome(tokensBurnt string) any {
	return map[string]any{
		"status": map[string]any{"SuccessValue": ""},
		"transaction_outcome": map[string]any{
			"block_hash": "11111111111111111111111111111111",
			"outcome":    map[string]any{"tokens_burnt": tokensBurnt},
		},
		"receipts_outcome": []any{
			map[string]any{
				"block_hash": "11111111111111111111111111111111",
				"outcome":    map[string]any{"tokens_burnt": tokensBurnt},
			},
		},
	}
}

func TestSendNonceWatermarkAndRetry(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newTestWallet(t, f)

	f.handle("gas_price", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"gas_price": "100000000"}, nil
	})
	f.handle("query", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			RequestType string `json:"request_type"`
			Finality    string `json:"finality"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad query params: %v", err)
		}
		if req.RequestType == "view_account" {
			return map[string]any{"amount": "1000000000000000000000000", "locked": "0", "storage_usage": 182}, nil
		}
		if req.RequestType != "view_access_key" {
			t.Fatalf("unexpected query type %s", req.RequestType)
		}
		if req.Finality == "optimistic" {
			return map[string]any{"nonce": 15}, nil
		}
		return map[string]any{"nonce": 10}, nil
	})

	var sentNonces []uint64
	f.handle("send_tx", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			SignedTx string `json:"signed_tx_base64"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad send_tx params: %v", err)
		}
		nonce, _ := parseSignedTransfer(t, req.SignedTx)
		sentNonces = append(sentNonces, nonce)
		if len(sentNonces) == 1 {
			return nil, &rpcError{
				Name:    "HANDLER_ERROR",
				Cause:   &rpcErrorCause{Name: "INVALID_TRANSACTION"},
				Code:    -32000,
				Message: "Server error",
				Data:    json.RawMessage(`{"TxExecutionError":{"InvalidTxError":{"InvalidNonce":{"tx_nonce":11,"ak_nonce":15}}}}`),
			}
		}
		return successOutcome("22770000000000000000"), nil
	})

	coin, err := w.Send("alice.near", 5_000_000, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if coin.Value() != 5_000_000 {
		t.Fatalf("wrong coin value %d", coin.Value())
	}
	if len(sentNonces) != 2 || sentNonces[0] != 11 || sentNonces[1] != 16 {
		t.Fatalf("wrong nonce sequence %v; expected [11 16]", sentNonces)
	}
	if w.nonce != 16 {
		t.Fatalf("nonce watermark %d, expected 16", w.nonce)
	}

	// A follow-up send must use the watermark (17), not final-view nonce 10.
	f.handle("send_tx", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			SignedTx string `json:"signed_tx_base64"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad send_tx params: %v", err)
		}
		nonce, _ := parseSignedTransfer(t, req.SignedTx)
		sentNonces = append(sentNonces, nonce)
		return successOutcome("22770000000000000000"), nil
	})
	if _, err := w.Send("alice.near", 1_000_000, 0); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if last := sentNonces[len(sentNonces)-1]; last != 17 {
		t.Fatalf("watermark not honored: nonce %d, expected 17", last)
	}
}

func TestSendExecutionFailure(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newTestWallet(t, f)

	f.handle("gas_price", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"gas_price": "100000000"}, nil
	})
	f.handle("query", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			RequestType string `json:"request_type"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad query params: %v", err)
		}
		if req.RequestType == "view_account" {
			return map[string]any{"amount": "1000000000000000000000000", "locked": "0", "storage_usage": 182}, nil
		}
		return map[string]any{"nonce": 41}, nil
	})
	f.handle("send_tx", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"status": map[string]any{
				"Failure": map[string]any{"ActionError": map[string]any{"index": 0}},
			},
			"transaction_outcome": map[string]any{
				"block_hash": "11111111111111111111111111111111",
				"outcome":    map[string]any{"tokens_burnt": "0"},
			},
		}, nil
	})

	_, err := w.Send("alice.near", 1_000_000, 0)
	if err == nil || !strings.Contains(err.Error(), "failed on-chain") {
		t.Fatalf("expected on-chain failure error, got %v", err)
	}
	if w.nonce != 42 {
		t.Fatalf("failed execution must still consume the nonce: watermark %d, expected 42", w.nonce)
	}
}

// TestSendAmbiguousBroadcast covers the fund-safety invariant: when a
// broadcast's outcome is unknown — a transport-level failure (here a
// method-not-found response, which is endpoint-attributable) or the
// server's authoritative TIMEOUT_ERROR — the transfer is recorded as
// possibly-live rather than retried, so no second transaction is
// constructed and no error is returned to prompt a caller retry.
func TestSendAmbiguousBroadcast(t *testing.T) {
	timeoutHandler := func(json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{
			Name:    "TIMEOUT_ERROR",
			Cause:   &rpcErrorCause{Name: "TIMEOUT_ERROR"},
			Code:    -32000,
			Message: "Timeout",
			Data:    json.RawMessage(`"timeout"`),
		}
	}
	for _, tc := range []struct {
		name string
		// register controls whether a send_tx handler is registered; when
		// false the fake returns method-not-found (endpoint-attributable).
		register bool
		handler  func(json.RawMessage) (any, *rpcError)
	}{
		{name: "endpoint failure", register: false},
		{name: "server timeout", register: true, handler: timeoutHandler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRPCServer(t)
			w := newTestWallet(t, f)
			f.handle("gas_price", func(json.RawMessage) (any, *rpcError) {
				return map[string]any{"gas_price": "100000000"}, nil
			})
			f.handle("query", func(params json.RawMessage) (any, *rpcError) {
				var req struct {
					RequestType string `json:"request_type"`
				}
				if err := json.Unmarshal(params, &req); err != nil {
					t.Fatalf("bad query params: %v", err)
				}
				if req.RequestType == "view_account" {
					return map[string]any{"amount": "1000000000000000000000000", "locked": "0", "storage_usage": 182}, nil
				}
				return map[string]any{"nonce": 7}, nil
			})
			if tc.register {
				f.handle("send_tx", tc.handler)
			}

			coin, err := w.Send("alice.near", 1_000_000, 0)
			if err != nil {
				t.Fatalf("ambiguous broadcast must not error: %v", err)
			}
			if coin == nil {
				t.Fatalf("expected a coin for the possibly-live transfer")
			}
			if n := f.callCount("send_tx"); n != 1 {
				t.Fatalf("ambiguous broadcast must not retry: %d send_tx calls", n)
			}
			if w.nonce != 8 {
				t.Fatalf("nonce must advance past a possibly-live tx: got %d, want 8", w.nonce)
			}
		})
	}
}

func TestWithdrawCapsAndFees(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newTestWallet(t, f)

	const balanceYocto = "1000000000000000000000000" // 1 NEAR
	const storageUsed = 1000

	f.handle("gas_price", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"gas_price": "100000000"}, nil
	})
	f.handle("query", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			RequestType string `json:"request_type"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad query params: %v", err)
		}
		switch req.RequestType {
		case "view_account":
			return map[string]any{
				"amount":        balanceYocto,
				"locked":        "0",
				"storage_usage": storageUsed,
			}, nil
		case "view_access_key":
			return map[string]any{"nonce": 7}, nil
		}
		return nil, &rpcError{Code: -32000, Message: "unexpected query"}
	})

	var deposits []*big.Int
	f.handle("send_tx", func(params json.RawMessage) (any, *rpcError) {
		var req struct {
			SignedTx string `json:"signed_tx_base64"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("bad send_tx params: %v", err)
		}
		_, deposit := parseSignedTransfer(t, req.SignedTx)
		deposits = append(deposits, deposit)
		return successOutcome("22770000000000000000"), nil
	})

	// fee = gasPrice(1e8) * transferGas(450e9) = 4.5e19 yocto.
	fee := new(big.Int).Mul(big.NewInt(100_000_000), big.NewInt(transferGas))
	balance, _ := new(big.Int).SetString(balanceYocto, 10)
	reserve := new(big.Int).Mul(big.NewInt(storageUsed), storageAmountPerByte)
	spendable := new(big.Int).Set(balance)
	spendable.Sub(spendable, reserve)
	spendable.Sub(spendable, new(big.Int).Mul(fee, big.NewInt(withdrawFeeMargin)))

	// Withdrawing the full balance must cap the deposit at the spendable
	// amount (balance - storage reserve - fee margin).
	if _, err := w.Withdraw(w.accountID, 100_000_000, 0); err != nil {
		t.Fatalf("max Withdraw: %v", err)
	}
	if deposits[0].Cmp(spendable) != 0 {
		t.Fatalf("max withdraw deposit %s, expected %s", deposits[0], spendable)
	}

	// A partial withdraw sends value minus one fee, uncapped.
	partial := DropsToYocto(10_000_000) // 0.1 NEAR
	partial.Sub(partial, fee)
	if _, err := w.Withdraw(w.accountID, 10_000_000, 0); err != nil {
		t.Fatalf("partial Withdraw: %v", err)
	}
	if deposits[1].Cmp(partial) != 0 {
		t.Fatalf("partial withdraw deposit %s, expected %s", deposits[1], partial)
	}
}

func TestEstimateSendTxFee(t *testing.T) {
	f := newFakeRPCServer(t)
	w := newTestWallet(t, f)

	f.handle("gas_price", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"gas_price": "100000000"}, nil
	})
	f.handle("query", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"amount":        "1000000000000000000000000", // 1 NEAR
			"locked":        "0",
			"storage_usage": 1000,
		}, nil
	})

	// 1e8 yocto/gas * 450e9 gas = 4.5e19 yocto = 4500 drops.
	const wantFee = 4500

	fee, valid, err := w.EstimateSendTxFee("alice.near", 50_000_000, 0, false, false)
	if err != nil {
		t.Fatalf("EstimateSendTxFee: %v", err)
	}
	if fee != wantFee {
		t.Fatalf("fee %d, expected %d", fee, wantFee)
	}
	if !valid {
		t.Fatalf("alice.near reported invalid")
	}

	if w.StandardSendFee(0) != wantFee {
		t.Fatalf("StandardSendFee %d, expected %d", w.StandardSendFee(0), wantFee)
	}

	// Sending more than the spendable balance errors.
	if _, _, err := w.EstimateSendTxFee("alice.near", 100_000_000, 0, false, false); err == nil {
		t.Fatalf("expected insufficient balance error")
	}

	// Invalid address is reported without error when the balance covers it.
	_, valid, err = w.EstimateSendTxFee("NOT!VALID", 50_000_000, 0, false, false)
	if err != nil {
		t.Fatalf("EstimateSendTxFee invalid addr: %v", err)
	}
	if valid {
		t.Fatalf("invalid address reported valid")
	}
}

func TestRPCEndpointsSetting(t *testing.T) {
	w := &NearWallet{net: util.Mainnet, settings: map[string]string{}}
	if eps := w.rpcEndpoints(); len(eps) != len(DefaultRPCEndpoints[util.Mainnet]) {
		t.Fatalf("expected default endpoints, got %v", eps)
	}
	w.settings["rpcprovider"] = " https://a.example , https://b.example ,"
	eps := w.rpcEndpoints()
	if len(eps) != 2 || eps[0] != "https://a.example" || eps[1] != "https://b.example" {
		t.Fatalf("bad parsed endpoints %v", eps)
	}
}

func TestYoctoToDropsCeil(t *testing.T) {
	for _, tt := range []struct {
		yocto string
		want  uint64
	}{
		{"0", 0},
		{"1", 1},
		{"10000000000000000", 1}, // exactly 1 drop
		{"10000000000000001", 2}, // 1 drop + 1 yocto
		{"45000000000000000000", 4500},
	} {
		y, _ := new(big.Int).SetString(tt.yocto, 10)
		if got := YoctoToDropsCeil(y); got != tt.want {
			t.Errorf("YoctoToDropsCeil(%s) = %d, want %d", tt.yocto, got, tt.want)
		}
	}
}

// TestRPCContextCancel verifies that a context deadline actually aborts an
// in-flight RPC call rather than being ignored until the client timeout.
func TestRPCContextCancel(t *testing.T) {
	m := newMockNearRPC(t)
	m.handle("block", func(_ json.RawMessage) (any, *rpcError) {
		time.Sleep(200 * time.Millisecond) // slow response
		return map[string]any{"header": map[string]any{
			"height": uint64(1), "hash": "11111111111111111111111111111111111111111", "timestamp": uint64(0),
		}}, nil
	})
	c := m.client()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.getBlock(ctx, "final"); err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestFeeDrops(t *testing.T) {
	res := &txResult{
		TransactionOutcome: txOutcomeBlock{Outcome: txOutcome{TokensBurnt: "22770000000000000000"}},
		ReceiptsOutcome: []txOutcomeBlock{
			{Outcome: txOutcome{TokensBurnt: "22770000000000000000"}},
		},
	}
	// 2 * 2.277e19 yocto = 4.554e19 yocto = 4554 drops.
	if got := res.feeDrops(); got != 4554 {
		t.Fatalf("feeDrops %d, expected 4554", got)
	}
	if fmt.Sprint((&txResult{}).feeDrops()) != "0" {
		t.Fatalf("empty outcome should burn nothing")
	}
}

// TestTxStatusBareStringPending verifies that nearcore's bare-string
// mid-execution status (e.g. "Started") unmarshals as pending rather than
// as a decode error, the same as the absent-outcome shape.
func TestTxStatusBareStringPending(t *testing.T) {
	var s txStatus
	if err := json.Unmarshal([]byte(`"Started"`), &s); err != nil {
		t.Fatalf("unmarshal bare-string status: %v", err)
	}
	if !s.isPending() {
		t.Fatalf("bare-string status should be pending")
	}

	s = txStatus{}
	if err := json.Unmarshal([]byte(`{"SuccessValue":""}`), &s); err != nil {
		t.Fatalf("unmarshal success status: %v", err)
	}
	if !s.isSuccess() {
		t.Fatalf("object status should still unmarshal correctly")
	}
}
