// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bisoncraft/caribou/util"
)

func TestFunctionCallActionEncoding(t *testing.T) {
	args := []byte(`{"account_id":"a.near"}`)
	deposit, _ := new(big.Int).SetString(standardStorageDepositYocto, 10)
	a := functionCallAction("storage_deposit", args, 30_000_000_000_000, deposit)
	if a.variant != actionFunctionCall {
		t.Fatalf("variant = %d, want %d", a.variant, actionFunctionCall)
	}
	// method_name string: 4-byte LE length + bytes.
	methodLen := len("storage_deposit")
	if int(a.data[0]) != methodLen {
		t.Fatalf("method name length prefix = %d, want %d", a.data[0], methodLen)
	}
	got := string(a.data[4 : 4+methodLen])
	if got != "storage_deposit" {
		t.Fatalf("method name = %q, want %q", got, "storage_deposit")
	}
}

// mockNearRPC is an httptest server that responds to NEAR JSON-RPC calls.
type mockNearRPC struct {
	t        *testing.T
	handlers map[string]func(params json.RawMessage) (any, *rpcError)
	srv      *httptest.Server
}

func newMockNearRPC(t *testing.T) *mockNearRPC {
	m := &mockNearRPC{t: t, handlers: make(map[string]func(json.RawMessage) (any, *rpcError))}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockNearRPC) handle(method string, h func(params json.RawMessage) (any, *rpcError)) {
	m.handlers[method] = h
}

func (m *mockNearRPC) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		m.t.Fatalf("mockNearRPC: decode request: %v", err)
	}
	h, ok := m.handlers[req.Method]
	if !ok {
		m.t.Fatalf("mockNearRPC: unhandled method %q", req.Method)
	}
	result, rpcErr := h(req.Params)
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: "meshwallet"}
	if rpcErr != nil {
		b, _ := json.Marshal(rpcErr)
		resp.Error = &rpcError{}
		_ = json.Unmarshal(b, resp.Error)
	} else {
		b, _ := json.Marshal(result)
		resp.Result = b
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (m *mockNearRPC) client() *rpcClient {
	c, err := newRPCClient(m.srv.URL, "", util.Disabled)
	if err != nil {
		m.t.Fatalf("newRPCClient: %v", err)
	}
	return c
}

// callFunctionResult encodes a JSON value as the NEP-141-style byte-array
// result that call_function returns.
func callFunctionResult(v any) map[string]any {
	b, _ := json.Marshal(v)
	bytesArr := make([]int, len(b))
	for i, c := range b {
		bytesArr[i] = int(c)
	}
	return map[string]any{"result": bytesArr}
}

func makeTokenTestWallet(t *testing.T, m *mockNearRPC) *TokenWallet {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x09}, 64)))
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	parent := &NearWallet{
		pubKey:    pub,
		privKey:   priv,
		accountID: "sender.near",
		rpc:       m.client(),
		log:       util.Disabled,
	}
	return &TokenWallet{
		parent:     parent,
		assetID:    usdcTokenID,
		token:      TokenUSDC,
		contractID: TokenUSDC.NetContracts[util.Mainnet],
	}
}

func TestTokenWalletDepositAddress(t *testing.T) {
	m := newMockNearRPC(t)
	tw := makeTokenTestWallet(t, m)
	addr, err := tw.DepositAddress()
	if err != nil {
		t.Fatalf("DepositAddress error: %v", err)
	}
	if addr != "sender.near" {
		t.Fatalf("DepositAddress = %q, want %q", addr, "sender.near")
	}
}

func TestTokenWalletBalance(t *testing.T) {
	m := newMockNearRPC(t)
	m.handle("query", func(_ json.RawMessage) (any, *rpcError) {
		return callFunctionResult("42000000"), nil
	})
	tw := makeTokenTestWallet(t, m)
	bal, err := tw.Balance()
	if err != nil {
		t.Fatalf("Balance error: %v", err)
	}
	if bal.Available != 42_000_000 {
		t.Fatalf("Available = %d, want 42000000", bal.Available)
	}
}

func TestTokenWalletSendInsufficientBalance(t *testing.T) {
	m := newMockNearRPC(t)
	m.handle("query", func(_ json.RawMessage) (any, *rpcError) {
		return callFunctionResult("100"), nil
	})
	tw := makeTokenTestWallet(t, m)
	_, err := tw.Send("receiver.near", 1_000_000, 0)
	if err == nil {
		t.Fatal("expected error for insufficient token balance")
	}
}

func TestTokenWalletSendInvalidAddress(t *testing.T) {
	m := newMockNearRPC(t)
	tw := makeTokenTestWallet(t, m)
	if _, err := tw.Send("not an address!", 1_000_000, 0); err == nil {
		t.Fatal("expected error for invalid recipient address")
	}
}

func TestTokenWalletSendHappyPathRegistersStorage(t *testing.T) {
	m := newMockNearRPC(t)
	const dest = "receiver.near"
	tw := makeTokenTestWallet(t, m)

	m.handle("query", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		switch p["request_type"] {
		case "call_function":
			switch p["method_name"] {
			case "ft_balance_of":
				return callFunctionResult("10000000"), nil
			case "storage_balance_of":
				return callFunctionResult(nil), nil // not registered yet
			}
		case "view_access_key":
			return map[string]any{"nonce": uint64(5), "block_height": uint64(100), "block_hash": "11111111111111111111111111111111111111111"}, nil
		}
		return nil, &rpcError{Message: "unexpected query", Code: -1}
	})
	m.handle("broadcast_tx_async", func(_ json.RawMessage) (any, *rpcError) {
		return "somehash", nil
	})

	_, err := tw.Send(dest, 1_000_000, 0)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
}

// TestReserveNonceAdvancesPastPending guards against a nonce collision: NEAR
// only advances an account's on-chain nonce once a transaction is included
// in a block, but broadcastTxAsync returns before that happens. Two sends
// issued back-to-back (native or token, sharing nonceMtx) must not both
// compute the same nonce from a still-stale access-key view.
func TestReserveNonceAdvancesPastPending(t *testing.T) {
	m := newMockNearRPC(t)
	m.handle("query", func(_ json.RawMessage) (any, *rpcError) {
		// The network's view of the account nonce never advances in this
		// test, simulating a transaction still pending inclusion.
		return map[string]any{"nonce": uint64(5)}, nil
	})
	w := &NearWallet{rpc: m.client()}

	n1, err := w.reserveNonce()
	if err != nil {
		t.Fatalf("reserveNonce (1st) error: %v", err)
	}
	if n1 != 6 {
		t.Fatalf("n1 = %d, want 6", n1)
	}

	n2, err := w.reserveNonce()
	if err != nil {
		t.Fatalf("reserveNonce (2nd) error: %v", err)
	}
	if n2 != n1+1 {
		t.Fatalf("n2 = %d, want %d (network nonce still stale, must advance past reserved n1)", n2, n1+1)
	}
}
