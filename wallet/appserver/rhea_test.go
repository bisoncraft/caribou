//go:build !live

package appserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/core"
)

func TestAPIRhea(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()
	c.rheaMarkets = &core.RheaMarketsView{Markets: []*core.RheaMarket{{Symbol: "USDC"}}}
	c.rheaAccount = &core.RheaAccountView{Discovery: core.RheaFound, MCA: "rhea000001.multica.near",
		Admission: &core.RheaAdmission{Reasons: []string{"window"}}}
	c.rheaOps = []*core.RheaOp{{ID: "op1", Kind: "withdraw"}}

	get := func(h http.HandlerFunc, path string) map[string]json.RawMessage {
		t.Helper()
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: bad response %q: %v", path, rec.Body.String(), err)
		}
		return resp
	}

	resp := get(s.apiRheaMarkets, "/api/rhea/markets")
	var markets core.RheaMarketsView
	if string(resp["ok"]) != "true" || json.Unmarshal(resp["markets"], &markets) != nil || markets.Markets[0].Symbol != "USDC" {
		t.Fatalf("markets response %v", resp)
	}
	resp = get(s.apiRheaAccount, "/api/rhea/account")
	var account core.RheaAccountView
	if string(resp["ok"]) != "true" || json.Unmarshal(resp["account"], &account) != nil ||
		account.MCA != "rhea000001.multica.near" || account.Admission.Open || len(account.Admission.Reasons) != 1 {
		t.Fatalf("account response %v", resp)
	}
	resp = get(s.apiRheaOps, "/api/rhea/ops")
	var ops []*core.RheaOp
	if string(resp["ok"]) != "true" || json.Unmarshal(resp["ops"], &ops) != nil || len(ops) != 1 || ops[0].ID != "op1" {
		t.Fatalf("ops response %v", resp)
	}

	c.rheaErr = errors.New("RHEA lending is not available")
	for path, h := range map[string]http.HandlerFunc{
		"/api/rhea/markets": s.apiRheaMarkets,
		"/api/rhea/account": s.apiRheaAccount,
		"/api/rhea/ops":     s.apiRheaOps,
	} {
		if resp := get(h, path); string(resp["ok"]) == "true" {
			t.Fatalf("%s: error reported as ok: %v", path, resp)
		}
	}
}

func TestAPIRheaSupply(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()
	post := func(h http.HandlerFunc, body any) bool {
		t.Helper()
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b)))
		var resp standardResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad response %q: %v", rec.Body.String(), err)
		}
		return resp.OK
	}
	src := uint32(0)

	if post(s.apiRheaSupplyPreview, map[string]any{"tokenID": "usdt.tether-token.near", "value": 1}) {
		t.Fatalf("preview without a source asset accepted")
	}
	if !post(s.apiRheaSupplyPreview, map[string]any{"sourceAssetID": src, "tokenID": "usdt.tether-token.near", "value": 1}) || c.rheaPreviewCalls != 1 {
		t.Fatalf("preview not served")
	}

	type execBody struct {
		SourceAssetID  *uint32          `json:"sourceAssetID"`
		TokenID        string           `json:"tokenID"`
		Value          uint64           `json:"value"`
		MinDelivered   string           `json:"minDelivered"`
		Creating       bool             `json:"creating"`
		MaxCreationFee string           `json:"maxCreationFee"`
		Pass           encode.PassBytes `json:"pass"`
	}
	body := &execBody{SourceAssetID: &src, TokenID: "usdt.tether-token.near", Value: 1}
	if post(s.apiRheaSupplyExecute, body) {
		t.Fatalf("execute without a previewed minimum accepted")
	}
	body.MinDelivered, body.Creating = "1", true
	if post(s.apiRheaSupplyExecute, body) {
		t.Fatalf("creation without a previewed fee accepted")
	}
	body.MaxCreationFee = "600000"

	// With sends requiring the password, none supplied is refused before
	// Core is called.
	c.sendRequiresPW = true
	if post(s.apiRheaSupplyExecute, body) || c.rheaExecuteCalls != 0 {
		t.Fatalf("execute without a required password")
	}
	body.Pass = encode.PassBytes("apppass")
	if !post(s.apiRheaSupplyExecute, body) || c.rheaExecuteCalls != 1 || c.rheaExecutePass != "apppass" {
		t.Fatalf("execute with a password: calls %d, pass %q", c.rheaExecuteCalls, c.rheaExecutePass)
	}
	c.verifyPasswordErr = errors.New("wrong password")
	if post(s.apiRheaSupplyExecute, body) || c.rheaExecuteCalls != 1 {
		t.Fatalf("execute with a wrong password")
	}
}

func TestAPIRheaWithdraw(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()
	post := func(h http.HandlerFunc, body any) bool {
		t.Helper()
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b)))
		var resp standardResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad response %q: %v", rec.Body.String(), err)
		}
		return resp.OK
	}
	dest := uint32(0)
	if post(s.apiRheaWithdrawPreview, map[string]any{"tokenID": "usdt.tether-token.near"}) {
		t.Fatalf("preview without a destination accepted")
	}
	if !post(s.apiRheaWithdrawPreview, map[string]any{"tokenID": "usdt.tether-token.near", "destAssetID": dest}) || c.rheaPreviewCalls != 1 {
		t.Fatalf("preview not served")
	}
	type execBody struct {
		TokenID     string           `json:"tokenID"`
		DestAssetID *uint32          `json:"destAssetID"`
		Amount      string           `json:"amount"`
		MinAtomsOut uint64           `json:"minAtomsOut"`
		MaxFee      string           `json:"maxFee"`
		Pass        encode.PassBytes `json:"pass"`
	}
	body := &execBody{TokenID: "usdt.tether-token.near", DestAssetID: &dest, Amount: "1000000"}
	if post(s.apiRheaWithdrawExecute, body) {
		t.Fatalf("execute without previewed bounds accepted")
	}
	body.MinAtomsOut, body.MaxFee = 1, "4750"
	c.sendRequiresPW = true
	if post(s.apiRheaWithdrawExecute, body) || c.rheaExecuteCalls != 0 {
		t.Fatalf("execute without a required password")
	}
	body.Pass = encode.PassBytes("apppass")
	if !post(s.apiRheaWithdrawExecute, body) || c.rheaExecuteCalls != 1 || c.rheaExecutePass != "apppass" {
		t.Fatalf("execute with a password: calls %d, pass %q", c.rheaExecuteCalls, c.rheaExecutePass)
	}
}

func TestAPIRheaSweep(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()
	post := func(h http.HandlerFunc, body any) bool {
		t.Helper()
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b)))
		var resp standardResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad response %q: %v", rec.Body.String(), err)
		}
		return resp.OK
	}
	if post(s.apiRheaSweepPreview, map[string]any{}) {
		t.Fatalf("preview without a market accepted")
	}
	// No destination is a sweep back into supply.
	if !post(s.apiRheaSweepPreview, map[string]any{"tokenID": "usdt.tether-token.near"}) || c.rheaPreviewCalls != 1 {
		t.Fatalf("preview not served")
	}
	type execBody struct {
		TokenID     string           `json:"tokenID"`
		DestAssetID *uint32          `json:"destAssetID"`
		MinAtomsOut uint64           `json:"minAtomsOut"`
		MaxFee      string           `json:"maxFee"`
		Pass        encode.PassBytes `json:"pass"`
	}
	dest := uint32(0)
	body := &execBody{TokenID: "usdt.tether-token.near"}
	if post(s.apiRheaSweepExecute, body) {
		t.Fatalf("execute without a previewed fee accepted")
	}
	body.DestAssetID, body.MaxFee = &dest, "1000000000000000000000"
	if post(s.apiRheaSweepExecute, body) {
		t.Fatalf("sweep out without a previewed minimum accepted")
	}
	body.DestAssetID = nil
	c.sendRequiresPW = true
	if post(s.apiRheaSweepExecute, body) || c.rheaExecuteCalls != 0 {
		t.Fatalf("execute without a required password")
	}
	body.Pass = encode.PassBytes("apppass")
	if !post(s.apiRheaSweepExecute, body) || c.rheaExecuteCalls != 1 || c.rheaExecutePass != "apppass" {
		t.Fatalf("execute with a password: calls %d, pass %q", c.rheaExecuteCalls, c.rheaExecutePass)
	}
}

// TestAPIRheaCreationFeeJSON pins the JSON shape of the one-time creation
// fee line in the supply preview and the account view.
func TestAPIRheaCreationFeeJSON(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()
	line := &core.RheaFeeLine{
		TokenID: "usdt.tether-token.near", Symbol: "USDT", Decimals: 6, Amount: "600000", USD: "0.60", OneTime: true,
		Components: []*core.RheaFeeComponent{
			{Kind: core.RheaFeeFixed, Amount: "300000", USD: "0.30"},
			{Kind: core.RheaFeeAccountFunding, Amount: "300000", USD: "0.30", NEAR: "100000000000000000000000"},
		},
	}
	c.rheaSupplyPreview = &core.RheaSupplyPreview{Creating: true, CreationFee: line, MinDelivered: "5000000",
		CreationMinimum: &core.RheaCreationMinimum{Amount: "5000000", USD: "5.00"}}
	noRate := "no current USD rate for ZEC; supplying is unavailable until rates refresh"
	acctLine := *line
	acctLine.CreationMinimum = &core.RheaCreationMinimum{Amount: "5000000", USD: "5.00"}
	c.rheaAccount = &core.RheaAccountView{Discovery: core.RheaNone, Admission: &core.RheaAdmission{},
		CreationFees: []*core.RheaFeeLine{&acctLine, {TokenID: "zec.omft.near", Symbol: "ZEC", OneTime: true, Unavailable: noRate}}}

	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var v map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("bad response %q: %v", rec.Body.String(), err)
		}
		return v
	}
	wantLine := `{"tokenID":"usdt.tether-token.near","symbol":"USDT","decimals":6,"amount":"600000","usd":"0.60","oneTime":true,` +
		`"components":[{"kind":"fixed","amount":"300000","usd":"0.30"},` +
		`{"kind":"accountFunding","amount":"300000","usd":"0.30","near":"100000000000000000000000"}]}`
	same := func(what string, got any, want string) {
		t.Helper()
		g, _ := json.Marshal(got)
		var gv, wv any
		_ = json.Unmarshal(g, &gv)
		_ = json.Unmarshal([]byte(want), &wv)
		gb, _ := json.Marshal(gv)
		wb, _ := json.Marshal(wv)
		if !bytes.Equal(gb, wb) {
			t.Fatalf("%s:\n got  %s\n want %s", what, gb, wb)
		}
	}

	b, _ := json.Marshal(map[string]any{"sourceAssetID": 0, "tokenID": "usdt.tether-token.near", "value": 1})
	rec := httptest.NewRecorder()
	s.apiRheaSupplyPreview(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b)))
	pv, _ := decode(rec)["preview"].(map[string]any)
	same("preview creation fee", pv["creationFee"], wantLine)
	same("preview creation minimum", pv["creationMinimum"], `{"amount":"5000000","usd":"5.00"}`)

	rec = httptest.NewRecorder()
	s.apiRheaAccount(rec, httptest.NewRequest(http.MethodGet, "/api/rhea/account", nil))
	acct, _ := decode(rec)["account"].(map[string]any)
	fees, _ := acct["creationFees"].([]any)
	if len(fees) != 2 {
		t.Fatalf("account creation fees %v", acct["creationFees"])
	}
	same("account creation fee", fees[0], strings.TrimSuffix(wantLine, "}")+`,"creationMinimum":{"amount":"5000000","usd":"5.00"}}`)
	same("unavailable creation fee", fees[1], `{"tokenID":"zec.omft.near","symbol":"ZEC","decimals":0,"usd":"","oneTime":true,"unavailable":"`+noRate+`"}`)
	if _, ok := acct["creationFeesUnavailable"]; ok {
		t.Fatalf("creationFeesUnavailable present with fees listed")
	}

	// Without a creation, the preview carries no fee line and no minimum.
	c.rheaSupplyPreview = &core.RheaSupplyPreview{MCA: "rhea000001.multica.near", MinDelivered: "1"}
	rec = httptest.NewRecorder()
	s.apiRheaSupplyPreview(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b)))
	pv, _ = decode(rec)["preview"].(map[string]any)
	if _, ok := pv["creationFee"]; ok {
		t.Fatalf("creation fee line without a creation: %v", pv["creationFee"])
	}
	if _, ok := pv["creationMinimum"]; ok {
		t.Fatalf("creation minimum without a creation: %v", pv["creationMinimum"])
	}
}

// TestAPIRheaRateFieldsJSON pins the JSON of the fee cap source in the
// withdrawal and sweep previews and of a market's supply unavailability.
func TestAPIRheaRateFieldsJSON(t *testing.T) {
	s, c, shutdown := newTServer(t, false)
	defer shutdown()
	call := func(h http.HandlerFunc, method string, body any, key string) map[string]json.RawMessage {
		t.Helper()
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(method, "/", bytes.NewReader(b)))
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad response %q: %v", rec.Body.String(), err)
		}
		var v map[string]json.RawMessage
		if err := json.Unmarshal(resp[key], &v); err != nil {
			t.Fatalf("no %s in %s: %v", key, rec.Body.String(), err)
		}
		return v
	}
	c.rheaWithdrawPreview = &core.RheaWithdrawPreview{MinAtomsOut: 1, FeeCap: "4000000", FeeCapSource: core.RheaFeeCapCatalog}
	pv := call(s.apiRheaWithdrawPreview, http.MethodPost, map[string]any{"tokenID": "usdt.tether-token.near", "destAssetID": 0}, "preview")
	if string(pv["feeCapSource"]) != `"catalog"` || string(pv["feeCap"]) != `"4000000"` {
		t.Fatalf("withdrawal preview fee cap %s (%s)", pv["feeCap"], pv["feeCapSource"])
	}
	c.rheaSweepPreview = &core.RheaSweepPreview{FeeCapSource: core.RheaFeeCapFiatRate}
	pv = call(s.apiRheaSweepPreview, http.MethodPost, map[string]any{"tokenID": "usdt.tether-token.near"}, "preview")
	if string(pv["feeCapSource"]) != `"fiatRate"` {
		t.Fatalf("sweep preview fee cap source %s", pv["feeCapSource"])
	}

	noRate := "no current USD rate for ZEC; supplying is unavailable until rates refresh"
	c.rheaMarkets = &core.RheaMarketsView{Markets: []*core.RheaMarket{{Symbol: "USDC"}, {Symbol: "ZEC", SupplyUnavailable: noRate}}}
	mv := call(s.apiRheaMarkets, http.MethodGet, nil, "markets")
	var markets []map[string]json.RawMessage
	if err := json.Unmarshal(mv["markets"], &markets); err != nil || len(markets) != 2 {
		t.Fatalf("markets %s: %v", mv["markets"], err)
	}
	if _, ok := markets[0]["supplyUnavailable"]; ok {
		t.Fatalf("supplyUnavailable on a priced market: %s", markets[0]["supplyUnavailable"])
	}
	if string(markets[1]["supplyUnavailable"]) != `"`+noRate+`"` {
		t.Fatalf("unpriced market %s", markets[1]["supplyUnavailable"])
	}
}
