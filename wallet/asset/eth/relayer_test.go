// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/evmrelay"
	"github.com/ethereum/go-ethereum/common"
)

// capturedReq records what the relay server received so tests can assert on
// request construction (path, method, body).
type capturedReq struct {
	method   string
	path     string
	rawQuery string
	body     []byte
}

// newRelayTestServer stands up an httptest server returning the given status
// and body, recording the inbound request into *got when non-nil. It returns a
// relayer wired to the server, constructed via newHTTPRelayer so the
// constructor's chain-ID logic is exercised too.
func newRelayTestServer(t *testing.T, status int, respBody string, got *capturedReq) *httpRelayer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			b, _ := io.ReadAll(r.Body)
			*got = capturedReq{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, body: b}
		}
		w.WriteHeader(status)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return newHTTPRelayer(srv.URL, util.Mainnet, 60, 1)
}

func TestNewHTTPRelayer(t *testing.T) {
	tests := []struct {
		name        string
		net         util.Network
		baseChainID uint32
		chainID     int64
		want        int64
	}{
		{"mainnet uses chainID arg", util.Mainnet, 60, 1, 1},
		{"non-simnet ignores map", util.Testnet, 60, 11155111, 11155111},
		{"simnet eth", util.Simnet, 60, 1337, 1},
		{"simnet polygon", util.Simnet, 966, 1337, 137},
		{"simnet base", util.Simnet, 8453, 1337, 8453},
		{"simnet unknown chain falls back", util.Simnet, 9999, 1337, 1337},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newHTTPRelayer("http://localhost", tc.net, tc.baseChainID, tc.chainID)
			if r.relayChainID != tc.want {
				t.Errorf("relayChainID = %d, want %d", r.relayChainID, tc.want)
			}
		})
	}
}

func TestHTTPRelayerEstimateFee(t *testing.T) {
	target := common.HexToAddress("0x1bbd020DDD6dc01f974Aa74D2D727B2A6782F32D")

	t.Run("success", func(t *testing.T) {
		var got capturedReq
		r := newRelayTestServer(t, http.StatusOK,
			`{"relayAddr":"0xab5801a7d398351b8be11c439e05c5b3259aec9b","fee":"123456"}`, &got)

		est, err := r.estimateFee(context.Background(), 3, target)
		if err != nil {
			t.Fatal(err)
		}
		if est.Fee.Int64() != 123456 {
			t.Errorf("fee = %s, want 123456", est.Fee)
		}
		if est.RelayAddr != common.HexToAddress("0xab5801a7d398351b8be11c439e05c5b3259aec9b") {
			t.Errorf("relayAddr = %s", est.RelayAddr)
		}

		// Request construction.
		if got.method != http.MethodPost || got.path != "/api/estimatefee" {
			t.Errorf("request = %s %s, want POST /api/estimatefee", got.method, got.path)
		}
		var reqBody evmrelay.EstimateFeeRequest
		if err := json.Unmarshal(got.body, &reqBody); err != nil {
			t.Fatalf("server received unparseable body: %v", err)
		}
		if reqBody.ChainID != 1 || reqBody.NumRedemptions != 3 || reqBody.Target != target.Hex() {
			t.Errorf("request body = %+v", reqBody)
		}
	})

	t.Run("invalid fee value", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusOK, `{"relayAddr":"0xab5801a7d398351b8be11c439e05c5b3259aec9b","fee":"notanumber"}`, nil)
		if _, err := r.estimateFee(context.Background(), 1, target); err == nil {
			t.Error("expected error for non-numeric fee")
		}
	})

	t.Run("zero relay address", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusOK, `{"relayAddr":"0x0000000000000000000000000000000000000000","fee":"1"}`, nil)
		if _, err := r.estimateFee(context.Background(), 1, target); err == nil {
			t.Error("expected error for zero relay address")
		}
	})

	t.Run("http error", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusInternalServerError, `boom`, nil)
		if _, err := r.estimateFee(context.Background(), 1, target); err == nil {
			t.Error("expected error for 500 response")
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusOK, `not json`, nil)
		if _, err := r.estimateFee(context.Background(), 1, target); err == nil {
			t.Error("expected error for malformed json")
		}
	})
}

func TestHTTPRelayerCheckHealth(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		var got capturedReq
		r := newRelayTestServer(t, http.StatusOK, `{"ok":true}`, &got)
		if err := r.checkHealth(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got.method != http.MethodGet || got.path != "/api/health" {
			t.Errorf("request = %s %s, want GET /api/health", got.method, got.path)
		}
		if got.rawQuery != "chain=1" {
			t.Errorf("query = %q, want chain=1", got.rawQuery)
		}
	})

	t.Run("relay reports error", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusOK, `{"ok":false,"error":"unsupported chain"}`, nil)
		if err := r.checkHealth(context.Background()); err == nil {
			t.Error("expected error when relay reports an error")
		}
	})

	t.Run("http error", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusServiceUnavailable, `down`, nil)
		if err := r.checkHealth(context.Background()); err == nil {
			t.Error("expected error for 503 response")
		}
	})
}

func TestHTTPRelayerSubmitSignedRedeem(t *testing.T) {
	req := &relayRequest{
		Target:   common.HexToAddress("0x1bbd020DDD6dc01f974Aa74D2D727B2A6782F32D"),
		Calldata: []byte{0xde, 0xad, 0xbe, 0xef},
	}

	t.Run("success", func(t *testing.T) {
		var got capturedReq
		r := newRelayTestServer(t, http.StatusOK, `{"taskID":"task-42"}`, &got)
		taskID, err := r.submitSignedRedeem(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if taskID != "task-42" {
			t.Errorf("taskID = %q, want task-42", taskID)
		}
		if got.method != http.MethodPost || got.path != "/api/relay" {
			t.Errorf("request = %s %s, want POST /api/relay", got.method, got.path)
		}
		var reqBody evmrelay.RelayRequest
		if err := json.Unmarshal(got.body, &reqBody); err != nil {
			t.Fatalf("unparseable body: %v", err)
		}
		if reqBody.Calldata != "0xdeadbeef" {
			t.Errorf("calldata = %q, want 0xdeadbeef", reqBody.Calldata)
		}
		if reqBody.Target != req.Target.Hex() || reqBody.ChainID != 1 {
			t.Errorf("request body = %+v", reqBody)
		}
	})

	t.Run("empty task id", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusOK, `{"taskID":""}`, nil)
		if _, err := r.submitSignedRedeem(context.Background(), req); err == nil {
			t.Error("expected error for empty task ID")
		}
	})

	t.Run("http error", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusBadRequest, `bad`, nil)
		if _, err := r.submitSignedRedeem(context.Background(), req); err == nil {
			t.Error("expected error for 400 response")
		}
	})
}

func TestHTTPRelayerGetRelayStatus(t *testing.T) {
	txHash := "0x67b27503aa20dd35bc2ae18fabfec477cf4d089b32173a590dc8d7319cbd0184"

	t.Run("success with tx hash", func(t *testing.T) {
		var got capturedReq
		r := newRelayTestServer(t, http.StatusOK, `{"state":"success","txHash":"`+txHash+`"}`, &got)
		st, err := r.getRelayStatus(context.Background(), "task-42")
		if err != nil {
			t.Fatal(err)
		}
		if st.State != evmrelay.RelayStateSuccess {
			t.Errorf("state = %q, want success", st.State)
		}
		if st.TxHash != common.HexToHash(txHash) {
			t.Errorf("txHash = %s, want %s", st.TxHash, txHash)
		}
		if got.path != "/api/relay/task-42" {
			t.Errorf("path = %q, want /api/relay/task-42", got.path)
		}
	})

	t.Run("pending has zero tx hash", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusOK, `{"state":"pending","txHash":""}`, nil)
		st, err := r.getRelayStatus(context.Background(), "task-1")
		if err != nil {
			t.Fatal(err)
		}
		if st.State != evmrelay.RelayStatePending {
			t.Errorf("state = %q, want pending", st.State)
		}
		if (st.TxHash != common.Hash{}) {
			t.Errorf("txHash = %s, want zero", st.TxHash)
		}
	})

	t.Run("failed carries reason", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusOK, `{"state":"failed","failureReason":"reverted"}`, nil)
		st, err := r.getRelayStatus(context.Background(), "task-2")
		if err != nil {
			t.Fatal(err)
		}
		if st.State != evmrelay.RelayStateFailed || st.FailureReason != "reverted" {
			t.Errorf("status = %+v, want failed/reverted", st)
		}
	})

	t.Run("http error", func(t *testing.T) {
		r := newRelayTestServer(t, http.StatusNotFound, `missing`, nil)
		if _, err := r.getRelayStatus(context.Background(), "task-3"); err == nil {
			t.Error("expected error for 404 response")
		}
	})
}
