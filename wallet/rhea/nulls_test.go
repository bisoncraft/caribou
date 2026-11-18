package rhea

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The decoders must reject null members rather than read them as absent
// funds, debt, records, or calls, and must never panic on them.

func TestDecodeTxTreeRejectsNulls(t *testing.T) {
	for name, raw := range map[string]string{
		"null outcome":            `{"transaction":{"hash":"tx"},"receipts_outcome":[null]}`,
		"outcome without id":      `{"transaction":{"hash":"tx"},"receipts_outcome":[{"outcome":{}}]}`,
		"null receipt":            `{"transaction":{"hash":"tx"},"receipts":[null]}`,
		"null transaction action": `{"transaction":{"hash":"tx","actions":[null]}}`,
		"null receipt action":     `{"transaction":{"hash":"tx"},"receipts":[{"receipt_id":"r","receipt":{"Action":{"actions":[null]}}}]}`,
		"null action list":        `{"transaction":{"hash":"tx"},"receipts":[{"receipt_id":"r","receipt":{"Action":{"actions":null}}}]}`,
		"null action body":        `{"transaction":{"hash":"tx"},"receipts":[{"receipt_id":"r","receipt":{"Action":null}}]}`,
		"two receipt variants":    `{"transaction":{"hash":"tx"},"receipts":[{"receipt_id":"r","receipt":{"Action":{"actions":[]},"Data":{}}}]}`,
		"duplicate receipt":       `{"transaction":{"hash":"tx"},"receipts":[{"receipt_id":"r","receipt":{"Data":{}}},{"receipt_id":"r","receipt":{"Data":{}}}]}`,
	} {
		if _, err := DecodeTxTree([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEventItemsRejectNulls(t *testing.T) {
	for name, data := range map[string]string{
		"null item":  `[null]`,
		"null data":  `null`,
		"no data":    ``,
		"mixed item": `[{"account_id":"a","token_id":"t","amount":"1"},null]`,
	} {
		log := `EVENT_JSON:{"standard":"burrow","version":"1.0.0","event":"deposit"`
		if data != "" {
			log += `,"data":` + data
		}
		log += `}`
		logJSON, _ := json.Marshal(log)
		raw := `{"transaction":{"hash":"tx"},"receipts":[{"receipt_id":"r","receipt":{"Data":{}}}],` +
			`"receipts_outcome":[{"id":"r","outcome":{"executor_id":"contract.main.burrow.near","logs":[` + string(logJSON) + `],"status":{"SuccessValue":""}}}]}`
		tree, err := DecodeTxTree([]byte(raw))
		if err != nil {
			t.Fatalf("%s: DecodeTxTree: %v", name, err)
		}
		if evs, err := tree.BurrowEvents(Mainnet.Burrow); err == nil {
			t.Errorf("%s: accepted as %d events", name, len(evs))
		}
	}
}

func TestPositionsRejectNulls(t *testing.T) {
	const ok = `{"account_id":"a","supplied":[],"positions":{"REGULAR":{"collateral":[],"borrowed":[]}}}`
	var p Positions
	if err := json.Unmarshal([]byte(ok), &p); err != nil || p.HasDebt() {
		t.Fatalf("valid positions: %v", err)
	}
	for name, raw := range map[string]string{
		"null position":    `{"supplied":[],"positions":{"REGULAR":null}}`,
		"null supplied":    `{"supplied":null,"positions":{}}`,
		"missing supplied": `{"positions":{}}`,
		"null supply item": `{"supplied":[null],"positions":{}}`,
		"null positions":   `{"supplied":[],"positions":null}`,
		"null collateral":  `{"supplied":[],"positions":{"REGULAR":{"collateral":null,"borrowed":[]}}}`,
		"missing borrowed": `{"supplied":[],"positions":{"REGULAR":{"collateral":[]}}}`,
		"null debt item":   `{"supplied":[],"positions":{"REGULAR":{"collateral":[],"borrowed":[null]}}}`,
		"null balance":     `{"supplied":[{"token_id":"t","balance":null,"shares":"1"}],"positions":{}}`,
	} {
		var p Positions
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAssetAndMetadataRequireFields(t *testing.T) {
	raw := viewResult(t, "contract.main.burrow.near", "get_asset")
	var a Asset
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("captured asset: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(m map[string]any){
		"null config":            func(m map[string]any) { m["config"] = nil },
		"missing extra decimals": func(m map[string]any) { delete(m["config"].(map[string]any), "extra_decimals") },
		"missing can_withdraw":   func(m map[string]any) { delete(m["config"].(map[string]any), "can_withdraw") },
		"null can_deposit":       func(m map[string]any) { m["config"].(map[string]any)["can_deposit"] = nil },
		"null supplied pool":     func(m map[string]any) { m["supplied"] = nil },
	} {
		var c map[string]any
		_ = json.Unmarshal(raw, &c)
		mutate(c)
		b, _ := json.Marshal(c)
		var a Asset
		if err := json.Unmarshal(b, &a); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var md FTMetadata
	if err := json.Unmarshal([]byte(`{"spec":"ft-1.0.0","symbol":"X"}`), &md); err == nil {
		t.Errorf("metadata without decimals accepted")
	}
	if err := json.Unmarshal([]byte(`{"spec":"ft-1.0.0","symbol":"X","decimals":null}`), &md); err == nil {
		t.Errorf("metadata with null decimals accepted")
	}
}

func TestReaderRejectsNullWallets(t *testing.T) {
	f := newFakeViewer(t)
	f.override[viewKey("multica.near", "list_wallets_by_mca", `{"mca_id":"rhea001569.multica.near"}`)] = []byte(`[{"EVM":"242c9fc3162321d326892cfe044fc0d9ffc3e9b0"},null]`)
	if _, _, err := NewReader(f, Mainnet).WalletsByMCA(context.Background(), "rhea001569.multica.near", f.hash); err == nil {
		t.Fatalf("null wallet accepted")
	}
}

func TestBusinessRejectsNulls(t *testing.T) {
	for name, raw := range map[string]string{
		"null request":       `{"nonce":"1","deadline":"2","tx_requests":[null]}`,
		"null requests":      `{"nonce":"1","deadline":"2","tx_requests":null}`,
		"missing requests":   `{"nonce":"1","deadline":"2"}`,
		"null function call": `{"nonce":"1","deadline":"2","tx_requests":[{"FunctionCall":{"receiver_id":"x.near","function_calls":[null]}}]}`,
	} {
		var b Business
		if err := json.Unmarshal([]byte(raw), &b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseExecArgs([]byte(`{"business":null,"signature":"ab","signer_wallet":{"EVM":"00"}}`)); err == nil {
		t.Errorf("null business accepted")
	}
	if _, err := ParseExecArgs([]byte(`{"business":{"nonce":"1","deadline":"2","tx_requests":[]},"signature":"ab","signer_wallet":null}`)); err == nil {
		t.Errorf("null signer wallet accepted")
	}
	if _, err := ParseRecipientMsg(`{"w":[{"EVM":"00"}],"b":{"r":"BurrowRegisterAndSupply"},"s":[null]}`); err == nil {
		t.Errorf("null creation signature accepted")
	}
}

func TestRelayerRejectsNulls(t *testing.T) {
	for name, tc := range map[string]struct {
		path, body string
		call       func(c *RelayerClient) error
	}{
		"null history record": {"/get_multichain_lending_history", `{"record_list":[null],"page_number":1}`,
			func(c *RelayerClient) error {
				_, err := c.History(context.Background(), "rhea001569.multica.near", 1, 5)
				return err
			}},
		"null record list": {"/get_multichain_lending_history", `{"record_list":null,"page_number":1}`,
			func(c *RelayerClient) error {
				_, err := c.History(context.Background(), "rhea001569.multica.near", 1, 5)
				return err
			}},
		"null config entry": {"/get_multichain_lending_config", `[null]`,
			func(c *RelayerClient) error { _, err := c.Config(context.Background()); return err }},
		"null batch request": {"/get_multichain_lending_data", `[null]`,
			func(c *RelayerClient) error { _, err := c.BatchStatus(context.Background(), "b"); return err }},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != tc.path {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(tc.body))
		}))
		if err := tc.call(NewRelayerClient(srv.URL, "", testLogger)); err == nil {
			t.Errorf("%s: accepted", name)
		}
		srv.Close()
	}
}

// TestEvidenceAvailability separates a complete execution from available
// evidence. With the receipt list withheld the outcomes are still complete,
// but the call-based extractors must fail rather than report that no
// transfer was resolved and no exec ran.
func TestEvidenceAvailability(t *testing.T) {
	for _, name := range []string{
		"tx-create-supply-1aqcDa8Y.json",
		"tx-supply-existing-Ey4MNC5d.json",
		"tx-exec-4SDDqzRj.json",
		"tx-exec-gaspayment-4175G6iY.json",
	} {
		if err := loadTree(t, name).EvidenceAvailable(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// The 1Click delivery's first receipt runs locally on intents.near and
	// is not in the receipt list; it is reconstructed from the transaction.
	tree := loadTree(t, "tx-create-supply-1aqcDa8Y.json")
	local := tree.Receipt(tree.Outcome.ReceiptIDs[0])
	if local == nil || local.Kind != ReceiptKindAction || local.PredecessorID != "intents.near" || len(local.Actions) != len(tree.Actions) {
		t.Fatalf("local receipt not reconstructed: %+v", local)
	}

	stripped := mutateTree(t, "tx-create-supply-1aqcDa8Y.json", func(m map[string]any) { delete(m, "receipts") })
	if err := stripped.Complete(); err != nil {
		t.Fatalf("outcomes should still be complete: %v", err)
	}
	if err := stripped.EvidenceAvailable(); err == nil {
		t.Fatalf("evidence reported available without receipts")
	}
	if res, err := stripped.ResolvedTransfers(); err == nil {
		t.Fatalf("resolutions reported without receipts: %d", len(res))
	}
	if calls, err := stripped.ExecCalls(Mainnet); err == nil {
		t.Fatalf("exec calls reported without receipts: %d", len(calls))
	}
	// Event evidence comes from outcomes and survives.
	if evs, err := stripped.BurrowEvents(Mainnet.Burrow); err != nil || len(evs) != 1 {
		t.Fatalf("burrow events %v, %v", evs, err)
	}

	// Withholding a single receipt is enough to fail.
	partial := mutateTree(t, "tx-exec-4SDDqzRj.json", func(m map[string]any) {
		rs := m["receipts"].([]any)
		m["receipts"] = rs[1:]
	})
	if err := partial.EvidenceAvailable(); err == nil || !strings.Contains(err.Error(), "not reported") {
		t.Fatalf("partial receipts: %v", err)
	}
	if _, err := partial.ExecCalls(Mainnet); err == nil {
		t.Fatalf("exec calls reported with the exec receipt withheld")
	}
}
