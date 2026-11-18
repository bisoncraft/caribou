package near

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
)

// byteArray encodes s the way nearcore transmits call_function results: a
// JSON array of byte values.
func byteArray(s string) []int {
	a := make([]int, len(s))
	for i := 0; i < len(s); i++ {
		a[i] = int(s[i])
	}
	return a
}

func newTestViewClient(t *testing.T, regular, archival *fakeRPCServer) *ViewClient {
	t.Helper()
	cfg := &ViewClientConfig{Net: util.Mainnet, Endpoints: []string{regular.srv.URL}}
	if archival != nil {
		cfg.ArchivalEndpoints = []string{archival.srv.URL}
	} else {
		cfg.Net = util.Simnet // no default archival endpoints
	}
	c, err := NewViewClient(cfg, testLogger)
	if err != nil {
		t.Fatalf("NewViewClient: %v", err)
	}
	return c
}

func TestViewClientCallFunction(t *testing.T) {
	srv := newFakeRPCServer(t)
	var lastParams map[string]any
	srv.handle("query", func(params json.RawMessage) (any, *rpcError) {
		lastParams = nil
		_ = json.Unmarshal(params, &lastParams)
		args, _ := base64.StdEncoding.DecodeString(lastParams["args_base64"].(string))
		switch lastParams["method_name"] {
		case "get_mca_by_wallet":
			if string(args) != `{"wallet":{"EVM":"ab"}}` {
				t.Errorf("args not passed through: %s", args)
			}
			return map[string]any{"result": byteArray("null"), "logs": []string{}, "block_height": 7, "block_hash": "HASH7"}, nil
		case "get_nonce":
			return map[string]any{"result": byteArray(`"90"`), "logs": []string{"l"}, "block_height": 8, "block_hash": "HASH8"}, nil
		case "nothing":
			return map[string]any{"result": []int{}, "block_height": 8, "block_hash": "HASH8"}, nil
		case "panics":
			return map[string]any{"error": "wasm execution failed", "block_height": 8, "block_hash": "HASH8"}, nil
		case "noref":
			return map[string]any{"result": byteArray("1")}, nil
		case "otherblock":
			return map[string]any{"result": byteArray("1"), "block_height": 9, "block_hash": "HASH9"}, nil
		}
		return nil, &rpcError{Code: -32000, Message: "Server error", Cause: &rpcErrorCause{Name: "UNKNOWN_ACCOUNT"}}
	})
	c := newTestViewClient(t, srv, nil)
	ctx := context.Background()

	// A named block is passed as block_id, and an authoritative null comes
	// back as the bytes "null".
	res, err := c.CallFunction(ctx, "multica.near", "get_mca_by_wallet", []byte(`{"wallet":{"EVM":"ab"}}`), "HASH7")
	if err != nil {
		t.Fatalf("CallFunction: %v", err)
	}
	if string(res.Result) != "null" || res.BlockHeight != 7 || res.BlockHash != "HASH7" {
		t.Fatalf("wrong result %+v", res)
	}
	if lastParams["block_id"] != "HASH7" || lastParams["finality"] != nil {
		t.Fatalf("block selector not passed: %v", lastParams)
	}

	// No block hash reads at finality final.
	res, err = c.CallFunction(ctx, "rhea001569.multica.near", "get_nonce", nil, "")
	if err != nil {
		t.Fatalf("CallFunction: %v", err)
	}
	if string(res.Result) != `"90"` || len(res.Logs) != 1 || res.BlockHash != "HASH8" {
		t.Fatalf("wrong result %+v", res)
	}
	if lastParams["finality"] != "final" || lastParams["block_id"] != nil {
		t.Fatalf("finality not passed: %v", lastParams)
	}
	if args, _ := base64.StdEncoding.DecodeString(lastParams["args_base64"].(string)); string(args) != "{}" {
		t.Fatalf("nil args not sent as {}: %q", args)
	}

	// An empty return is empty, not null.
	if res, err = c.CallFunction(ctx, "x.near", "nothing", nil, ""); err != nil || res.Result == nil || len(res.Result) != 0 {
		t.Fatalf("empty return: %v, %+v", err, res)
	}

	// Failures are errors, never results.
	for _, m := range []string{"panics", "noref", "unknown"} {
		if _, err := c.CallFunction(ctx, "x.near", m, nil, ""); err == nil {
			t.Fatalf("%s: expected error", m)
		}
	}
	// A result for another block than the one named is refused; at
	// finality final any block is the answer.
	if _, err := c.CallFunction(ctx, "x.near", "otherblock", nil, "HASH8"); err == nil {
		t.Fatalf("result from another block accepted")
	}
	if res, err := c.CallFunction(ctx, "x.near", "otherblock", nil, ""); err != nil || res.BlockHash != "HASH9" {
		t.Fatalf("final read: %+v, %v", res, err)
	}
}

func TestViewClientViewAccount(t *testing.T) {
	srv := newFakeRPCServer(t)
	srv.handle("query", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{
			"amount":               "157608019447165699999630",
			"locked":               "0",
			"code_hash":            "11111111111111111111111111111111",
			"global_contract_hash": "FDX6nYTmwVVwkjSXwFZ8xRjkPsyU1BEt1QqB9TbXwD11",
			"storage_usage":        494,
			"block_height":         225016693,
			"block_hash":           "4jRBh7o3ND46sRyPzqLenVKarw2NcCYxnwf72aEfehHH",
		}, nil
	})
	c := newTestViewClient(t, srv, nil)
	v, err := c.ViewAccount(context.Background(), "rhea001569.multica.near", "4jRBh7o3ND46sRyPzqLenVKarw2NcCYxnwf72aEfehHH")
	if err != nil {
		t.Fatalf("ViewAccount: %v", err)
	}
	if v.GlobalContractHash != "FDX6nYTmwVVwkjSXwFZ8xRjkPsyU1BEt1QqB9TbXwD11" || v.StorageUsage != 494 || v.BlockHeight != 225016693 {
		t.Fatalf("wrong account view %+v", v)
	}
	if _, err := c.ViewAccount(context.Background(), "rhea001569.multica.near", "OTHERHASH"); err == nil {
		t.Fatalf("account view from another block accepted")
	}
	if v, err = c.ViewAccount(context.Background(), "rhea001569.multica.near", ""); err != nil || v.BlockHash == "" {
		t.Fatalf("final account view: %+v, %v", v, err)
	}
}

func TestViewClientBlocks(t *testing.T) {
	srv := newFakeRPCServer(t)
	const ts = 1794949549020383055
	srv.handle("block", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		hash := "FINALHASH"
		if id, ok := p["block_id"].(string); ok {
			hash = id
			if id == "MISMATCH" {
				hash = "OTHER"
			}
		}
		return map[string]any{"header": map[string]any{
			"height": 225016693, "hash": hash, "prev_hash": "PREV", "timestamp": uint64(ts),
		}}, nil
	})
	c := newTestViewClient(t, srv, nil)
	ctx := context.Background()

	h, err := c.FinalBlock(ctx)
	if err != nil {
		t.Fatalf("FinalBlock: %v", err)
	}
	if h.Hash != "FINALHASH" || h.Height != 225016693 || h.PrevHash != "PREV" || !h.Timestamp.Equal(time.Unix(0, ts)) {
		t.Fatalf("wrong header %+v", h)
	}
	if h, err = c.BlockByHash(ctx, "4jRB"); err != nil || h.Hash != "4jRB" {
		t.Fatalf("BlockByHash: %v, %+v", err, h)
	}
	if _, err = c.BlockByHash(ctx, "MISMATCH"); err == nil {
		t.Fatalf("hash mismatch accepted")
	}
}

func TestViewClientTxStatusArchivalFallback(t *testing.T) {
	unknown := &rpcError{Code: -32000, Name: "HANDLER_ERROR", Message: "Server error",
		Cause: &rpcErrorCause{Name: "UNKNOWN_TRANSACTION"}}
	result := map[string]any{"final_execution_status": "FINAL", "receipts_outcome": []any{}}

	regular := newFakeRPCServer(t)
	archival := newFakeRPCServer(t)
	var regularKnows bool
	var waitSeen string
	regular.handle("EXPERIMENTAL_tx_status", func(params json.RawMessage) (any, *rpcError) {
		var p map[string]string
		_ = json.Unmarshal(params, &p)
		waitSeen = p["wait_until"]
		if p["sender_account_id"] != "intents.near" {
			t.Errorf("sender not passed: %v", p)
		}
		if regularKnows {
			return result, nil
		}
		return nil, unknown
	})
	archival.handle("EXPERIMENTAL_tx_status", func(json.RawMessage) (any, *rpcError) {
		return result, nil
	})
	c := newTestViewClient(t, regular, archival)
	ctx := context.Background()

	// Unknown on the regular endpoint: answered by the archival endpoint.
	raw, err := c.TxStatus(ctx, "1aqc", "intents.near", TxWaitFinal)
	if err != nil {
		t.Fatalf("TxStatus: %v", err)
	}
	if !json.Valid(raw) || archival.callCount("EXPERIMENTAL_tx_status") != 1 || waitSeen != TxWaitFinal {
		t.Fatalf("archival fallback not used: %s", raw)
	}

	// Known on the regular endpoint: no archival request.
	regularKnows = true
	if _, err = c.TxStatus(ctx, "1aqc", "intents.near", TxWaitNone); err != nil {
		t.Fatalf("TxStatus: %v", err)
	}
	if archival.callCount("EXPERIMENTAL_tx_status") != 1 {
		t.Fatalf("archival queried for a transaction the regular endpoint served")
	}

	// Unknown everywhere: the classification survives.
	regularKnows = false
	archival.handle("EXPERIMENTAL_tx_status", func(json.RawMessage) (any, *rpcError) { return nil, unknown })
	if _, err = c.TxStatus(ctx, "1aqc", "intents.near", TxWaitNone); !IsUnknownTransaction(err) {
		t.Fatalf("expected unknown transaction, got %v", err)
	}

	// Without an archival endpoint the regular answer stands.
	solo := newTestViewClient(t, regular, nil)
	if _, err = solo.TxStatus(ctx, "1aqc", "intents.near", TxWaitNone); !IsUnknownTransaction(err) {
		t.Fatalf("expected unknown transaction, got %v", err)
	}
}

func TestViewClientCallFunctionAll(t *testing.T) {
	answer := func(result string, hash string) func(json.RawMessage) (any, *rpcError) {
		return func(json.RawMessage) (any, *rpcError) {
			return map[string]any{"result": byteArray(result), "block_height": 7, "block_hash": hash}, nil
		}
	}
	a, b := newFakeRPCServer(t), newFakeRPCServer(t)
	a.handle("query", answer("null", "HASH7"))
	b.handle("query", answer("null", "HASH7"))
	c, err := NewViewClient(&ViewClientConfig{Net: util.Simnet, Endpoints: []string{a.srv.URL, b.srv.URL}}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := c.CallFunctionAll(ctx, "multica.near", "get_mca_by_wallet", []byte(`{}`), "HASH7")
	if err != nil || len(res) != 2 || string(res[0].Result) != "null" || string(res[1].Result) != "null" {
		t.Fatalf("CallFunctionAll = %+v, %v", res, err)
	}
	if a.callCount("query") != 1 || b.callCount("query") != 1 {
		t.Fatalf("not every endpoint asked")
	}
	if _, err := c.CallFunctionAll(ctx, "multica.near", "get_mca_by_wallet", nil, ""); err == nil {
		t.Fatalf("finality read accepted")
	}
	// One endpoint failing, erroring, or answering for another block fails
	// the whole call.
	b.setHTTPErr(true)
	if _, err := c.CallFunctionAll(ctx, "multica.near", "get_mca_by_wallet", nil, "HASH7"); err == nil {
		t.Fatalf("unreachable endpoint ignored")
	}
	b.setHTTPErr(false)
	b.handle("query", func(json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{Code: -32000, Message: "Server error", Cause: &rpcErrorCause{Name: "UNKNOWN_BLOCK"}}
	})
	if _, err := c.CallFunctionAll(ctx, "multica.near", "get_mca_by_wallet", nil, "HASH7"); err == nil {
		t.Fatalf("erroring endpoint ignored")
	}
	b.handle("query", answer("null", "HASH6"))
	if _, err := c.CallFunctionAll(ctx, "multica.near", "get_mca_by_wallet", nil, "HASH7"); err == nil {
		t.Fatalf("other-block answer ignored")
	}
}
