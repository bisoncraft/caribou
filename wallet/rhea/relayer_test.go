package rhea

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
)

var testLogger = util.StdOutLogger("RHEATEST", util.LevelError)

// lastQuery is the query of the last history or batch request served by
// newRelayerServer. Tests using it do not run in parallel.
var lastQuery url.Values

// newRelayerServer serves the captured relayer responses and records
// submissions.
func newRelayerServer(t *testing.T, submitReply string, submitted *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/get_multichain_lending_config":
			_, _ = w.Write(readTestdata(t, "relayer-config.json"))
		case "/get_multichain_lending_history":
			lastQuery = r.URL.Query()
			_, _ = w.Write(readTestdata(t, "relayer-history.json"))
		case "/get_multichain_lending_data":
			lastQuery = r.URL.Query()
			_, _ = w.Write(readTestdata(t, "relayer-batch.json"))
		case "/multichain_lending_requests":
			if r.Method != http.MethodPost {
				t.Errorf("submit method %s", r.Method)
			}
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, submitted); err != nil {
				t.Errorf("submit body %s: %v", b, err)
			}
			_, _ = w.Write([]byte(submitReply))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRelayerConfig(t *testing.T) {
	srv := newRelayerServer(t, "", nil)
	c := NewRelayerClient(srv.URL, "", testLogger)
	cfg, err := c.Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.CollectFeeAccountID != Mainnet.Relayer || cfg.MCAVersion != "0.1.6" {
		t.Fatalf("config %+v", cfg)
	}
	for tok, want := range map[string]string{
		usdcToken:                "1000",
		"usdt.tether-token.near": "1000",
		"wrap.near":              "1000000000000000000000",
		"zec.omft.near":          "300",
		GasTokenNEAR:             "1000000000000000000000",
	} {
		if got := cfg.GasFee[tok]; got == nil || got.String() != want {
			t.Fatalf("GAS_FEE[%s] = %v, want %s", tok, got, want)
		}
	}
}

func TestRelayerConfigRejects(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate key": `[{"key":"MCA_VERSION","value":"1"},{"key":"MCA_VERSION","value":"2"}]`,
		"bad gas fee":   `[{"key":"GAS_FEE","value":"{\"near\":\"-1\"}"}]`,
		"gas fee json":  `[{"key":"GAS_FEE","value":"nope"}]`,
		"not a list":    `{}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		c := NewRelayerClient(srv.URL, "", testLogger)
		if _, err := c.Config(context.Background()); err == nil {
			t.Errorf("%s: accepted", name)
		}
		srv.Close()
	}
}

func signedTestRequest(t *testing.T) (*Key, *SignedRequest) {
	t.Helper()
	k := testKey(t)
	p := validWithdrawParams()
	b, err := BuildWithdraw(Mainnet, p)
	if err != nil {
		t.Fatal(err)
	}
	req, err := k.SignWithdraw(Mainnet, b, p, testBlockTime.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return k, req
}

func TestRelayerSubmit(t *testing.T) {
	k, req := signedTestRequest(t)
	var sent map[string]any
	srv := newRelayerServer(t, `{"code":0,"data":"1d332eca-5853-4a95-a57b-2a00bb9cdf4e","msg":""}`, &sent)
	c := NewRelayerClient(srv.URL, "", testLogger)
	batch, err := c.Submit(context.Background(), testMCA, req)
	if err != nil || batch != "1d332eca-5853-4a95-a57b-2a00bb9cdf4e" {
		t.Fatalf("Submit = %q, %v", batch, err)
	}
	if sent["mca_id"] != testMCA || sent["wallet"] != `{"EVM":"`+string(k.Identity())+`"}` || sent["page_display_data"] != "" {
		t.Fatalf("submit body %v", sent)
	}
	reqs, _ := sent["request"].([]any)
	want, _ := req.MarshalJSON()
	if len(reqs) != 1 || reqs[0] != string(want) {
		t.Fatalf("submitted request %v", sent["request"])
	}

	// Non-success replies are relayer errors; malformed ones are errors.
	for reply, isRelayerErr := range map[string]bool{
		`{"code":1001,"data":"","msg":"nonce error"}`: true,
		`{"data":"x"}`:             false,
		`{"code":0,"data":""}`:     false,
		`<html>bad gateway</html>`: false,
	} {
		srv := newRelayerServer(t, reply, &sent)
		c := NewRelayerClient(srv.URL, "", testLogger)
		_, err := c.Submit(context.Background(), testMCA, req)
		var re *RelayerError
		if err == nil || errors.As(err, &re) != isRelayerErr {
			t.Fatalf("reply %s: %v", reply, err)
		}
	}
}

func TestRelayerBatchStatus(t *testing.T) {
	srv := newRelayerServer(t, "", nil)
	c := NewRelayerClient(srv.URL, "", testLogger)
	reqs, err := c.BatchStatus(context.Background(), "305f0f9d-232f-4b96-9760-cb4a07fd37a5")
	if err != nil || len(reqs) != 1 {
		t.Fatalf("BatchStatus = %v, %v", reqs, err)
	}
	if lastQuery.Get("batch_id") != "305f0f9d-232f-4b96-9760-cb4a07fd37a5" {
		t.Fatalf("batch query %v", lastQuery)
	}
	r := reqs[0]
	if r.BatchStatus != BatchComplete || r.Result == nil || r.Result.TxHash != "9dFTx2A9LTvpwBksuWrPgTVUAuUzapKDAZ3Lzp5VhDUF" || r.Result.TxErrMsg != "" {
		t.Fatalf("batch request %+v, result %+v", r, r.Result)
	}
	if !strings.Contains(r.Request, `"attach_deposit": "1250000000000000000000"`) || r.MCAID != "rhea001569.multica.near" {
		t.Fatalf("batch request fields %+v", r)
	}
	// A reply for another batch is refused.
	if _, err := c.BatchStatus(context.Background(), "other"); err == nil {
		t.Fatalf("foreign batch accepted")
	}
}

func TestRelayerHistory(t *testing.T) {
	srv := newRelayerServer(t, "", nil)
	c := NewRelayerClient(srv.URL, "", testLogger)
	p, err := c.History(context.Background(), "rhea001569.multica.near", 1, 5)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if lastQuery.Get("mca_id") != "rhea001569.multica.near" || lastQuery.Get("page_number") != "1" || lastQuery.Get("page_size") != "5" {
		t.Fatalf("history query %v", lastQuery)
	}
	if len(p.Records) != 5 || p.TotalSize < 5 || p.PageNumber != 1 {
		t.Fatalf("page %+v", p)
	}
	var relayed int
	for _, r := range p.Records {
		if r.Type == HistoryTypeRelayer {
			relayed++
			if r.BatchID == nil || len(r.RequestHash) < 43 {
				t.Fatalf("relayer record %+v", r)
			}
		}
	}
	if relayed != 3 || p.Records[1].RequestHash != "9dFTx2A9LTvpwBksuWrPgTVUAuUzapKDAZ3Lzp5VhDUF" {
		t.Fatalf("records %+v", p.Records)
	}
	if _, err := c.History(context.Background(), "rhea000001.multica.near", 1, 5); err == nil {
		t.Fatalf("history for another MCA accepted")
	}
}

func TestRelayerResponseLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_multichain_lending_config" {
			_, _ = w.Write([]byte("[" + strings.Repeat(" ", relayerResponseLimit) + "]"))
			return
		}
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	c := NewRelayerClient(srv.URL, "", testLogger)
	if _, err := c.Config(context.Background()); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized response: %v", err)
	}
	if _, err := c.History(context.Background(), "rhea001569.multica.near", 1, 5); err == nil {
		t.Fatalf("HTTP error accepted")
	}
}
