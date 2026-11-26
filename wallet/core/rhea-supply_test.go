package core

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util/encrypt"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/intents"
	"github.com/bisoncraft/caribou/wallet/rhea"
	"github.com/decred/base58"
)

const (
	usdtToken         = "usdt.tether-token.near"
	createTxHash      = "1aqcDa8YCP1RDHMd3KSxKkfkayfzDPaaZZBi3Zfgfr7"
	createdMCA        = "rhea001594.multica.near"
	supplyDepositAddr = "0000000000000000000000000000000000000000000000000000000000000abc"
)

func readRheaTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "rhea", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// createTreeMsg returns the recipient message the captured creation
// delivered.
func createTreeMsg(t *testing.T, raw []byte) string {
	t.Helper()
	tree, err := rhea.DecodeTxTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := tree.VerifierWithdrawals(rheaVerifier)
	if err != nil || len(ws) != 1 {
		t.Fatalf("withdrawals %v, %v", ws, err)
	}
	return ws[0].Msg
}

// editTree decodes a captured tree, applies f to its JSON, and decodes the
// result.
func editTree(t *testing.T, raw []byte, f func(m map[string]any)) *rhea.TxTree {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	b, _ := json.Marshal(m)
	tree, err := rhea.DecodeTxTree(b)
	if err != nil {
		t.Fatalf("DecodeTxTree: %v", err)
	}
	return tree
}

// resolveReceipt finds the ft_resolve_transfer receipt for sender →
// receiver and returns its ID.
func resolveReceipt(t *testing.T, m map[string]any, sender, receiver string) string {
	t.Helper()
	for _, r := range m["receipts"].([]any) {
		rm := r.(map[string]any)
		action, _ := rm["receipt"].(map[string]any)["Action"].(map[string]any)
		if action == nil {
			continue
		}
		for _, a := range action["actions"].([]any) {
			am, _ := a.(map[string]any)
			fc, _ := am["FunctionCall"].(map[string]any)
			if fc == nil || fc["method_name"] != "ft_resolve_transfer" {
				continue
			}
			args, _ := base64.StdEncoding.DecodeString(fc["args"].(string))
			var v struct {
				SenderID   string `json:"sender_id"`
				ReceiverID string `json:"receiver_id"`
			}
			_ = json.Unmarshal(args, &v)
			if v.SenderID == sender && v.ReceiverID == receiver {
				return rm["receipt_id"].(string)
			}
		}
	}
	t.Fatalf("no resolution %s → %s", sender, receiver)
	return ""
}

func outcomeByID(m map[string]any, id string) map[string]any {
	for _, o := range m["receipts_outcome"].([]any) {
		if o.(map[string]any)["id"] == id {
			return o.(map[string]any)["outcome"].(map[string]any)
		}
	}
	return nil
}

func setResolved(t *testing.T, m map[string]any, sender, receiver, used string) {
	t.Helper()
	out := outcomeByID(m, resolveReceipt(t, m, sender, receiver))
	out["status"] = map[string]any{"SuccessValue": base64.StdEncoding.EncodeToString([]byte(`"` + used + `"`))}
}

// editDepositEvent rewrites (or, with amount "", drops) Burrow's deposit
// event log.
func editDepositEvent(m map[string]any, amount string) {
	for _, o := range m["receipts_outcome"].([]any) {
		out := o.(map[string]any)["outcome"].(map[string]any)
		logs := out["logs"].([]any)
		kept := logs[:0]
		for _, l := range logs {
			s := l.(string)
			if strings.Contains(s, `"event":"deposit"`) && strings.Contains(s, `"standard":"burrow"`) {
				if amount == "" {
					continue
				}
				s = strings.Replace(s, `"amount":"29935000000000000"`, `"amount":"`+amount+`"`, 1)
			}
			kept = append(kept, s)
		}
		out["logs"] = kept
	}
}

func createParams(msg string) *supplyAttributionParams {
	return &supplyAttributionParams{
		dep: rhea.Mainnet, verifier: rheaVerifier, recipient: rhea.Mainnet.AccountManager,
		tokenID: usdtToken, msg: msg, extraDecimals: 12,
	}
}

// TestAttributeCreationSupply attributes the mainnet creation 1aqcDa8Y…
// and variations of it that refund, strand, or lose evidence.
func TestAttributeCreationSupply(t *testing.T) {
	raw := readRheaTestdata(t, "tx-create-supply-1aqcDa8Y.json")
	msg := createTreeMsg(t, raw)
	tree, err := rhea.DecodeTxTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	res := attributeSupply(tree, createParams(msg))
	if res.State != db.RheaStateSupplied || res.MCA != createdMCA || res.Delivered.Int64() != 629935 ||
		res.Fees.Int64() != 600000 || res.Supplied.Int64() != 29935 || res.Stranded.Sign() != 0 || len(res.Refunds) != 0 {
		t.Fatalf("attribution %+v (%s)", res, res.Reason)
	}

	// The supply to Burrow refunded in full: stranded in the new MCA.
	res = attributeSupply(editTree(t, raw, func(m map[string]any) {
		setResolved(t, m, createdMCA, rhea.Mainnet.Burrow, "0")
		editDepositEvent(m, "")
	}), createParams(msg))
	if res.State != db.RheaStateStranded || res.Stranded.Int64() != 29935 || res.Refunds[0].Account != createdMCA {
		t.Fatalf("stranded attribution %+v (%s)", res, res.Reason)
	}

	// Part supplied, part refunded into the MCA.
	res = attributeSupply(editTree(t, raw, func(m map[string]any) {
		setResolved(t, m, createdMCA, rhea.Mainnet.Burrow, "20000")
		editDepositEvent(m, "20000000000000000")
	}), createParams(msg))
	if res.State != db.RheaStatePartial || res.Supplied.Int64() != 20000 || res.Stranded.Int64() != 9935 {
		t.Fatalf("partial attribution %+v (%s)", res, res.Reason)
	}

	unresolvedCases := map[string]func(m map[string]any){
		"refund to the account manager": func(m map[string]any) {
			setResolved(t, m, rhea.Mainnet.AccountManager, createdMCA, "29000")
		},
		"refund to the verifier": func(m map[string]any) {
			setResolved(t, m, rheaVerifier, rhea.Mainnet.AccountManager, "600000")
		},
		"deposit event mismatch": func(m map[string]any) { editDepositEvent(m, "1") },
		"deposit event missing":  func(m map[string]any) { editDepositEvent(m, "") },
		"supply resolution missing": func(m map[string]any) {
			// Re-address the resolution so the MCA → Burrow hop has none.
			id := resolveReceipt(t, m, createdMCA, rhea.Mainnet.Burrow)
			for _, r := range m["receipts"].([]any) {
				rm := r.(map[string]any)
				if rm["receipt_id"] != id {
					continue
				}
				fc := rm["receipt"].(map[string]any)["Action"].(map[string]any)["actions"].([]any)[0].(map[string]any)["FunctionCall"].(map[string]any)
				fc["args"] = base64.StdEncoding.EncodeToString([]byte(`{"sender_id":"x.near","receiver_id":"y.near","amount":"29935"}`))
			}
		},
		"not final":        func(m map[string]any) { m["final_execution_status"] = "EXECUTED_OPTIMISTIC" },
		"receipts missing": func(m map[string]any) { delete(m, "receipts") },
	}
	for name, f := range unresolvedCases {
		res := attributeSupply(editTree(t, raw, f), createParams(msg))
		if res.State != db.RheaStateDeliveryUnresolved || res.Reason == "" {
			t.Errorf("%s: %s (%s)", name, res.State, res.Reason)
		}
		if name == "refund to the account manager" && (len(res.Refunds) != 1 || res.Refunds[0].Account != rhea.Mainnet.AccountManager || res.Refunds[0].Amount != "935") {
			t.Errorf("%s: refunds %+v", name, res.Refunds)
		}
	}

	// Another operation's message does not match the delivery.
	other := createParams(rhea.SupplyMsg("4500538e8c3060947e71621e9278cbde8ef387d3"))
	if res := attributeSupply(tree, other); res.State != db.RheaStateDeliveryUnresolved {
		t.Fatalf("foreign message attributed: %+v", res)
	}
}

// TestAttributeCollateralDelivery reads the mainnet delivery Ey4MNC5d…,
// which used the collateral template: the tokens reached Burrow but no
// supply deposit was made, so it is not a supply.
func TestAttributeCollateralDelivery(t *testing.T) {
	raw := readRheaTestdata(t, "tx-supply-existing-Ey4MNC5d.json")
	tree, err := rhea.DecodeTxTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := tree.VerifierWithdrawals(rheaVerifier)
	p := &supplyAttributionParams{
		dep: rhea.Mainnet, verifier: rheaVerifier, recipient: "rhea001624.multica.near", mca: "rhea001624.multica.near",
		tokenID: usdtToken, msg: ws[0].Msg, extraDecimals: 12,
	}
	res := attributeSupply(tree, p)
	if res.State != db.RheaStateDeliveryUnresolved || !strings.Contains(res.Reason, "deposit") {
		t.Fatalf("collateral delivery: %s (%s)", res.State, res.Reason)
	}
}

// oneClickMock is a 1Click service for supply tests: a catalog with BTC
// and NEAR-side USDT, signed quotes that echo the request, and scripted
// statuses. ZEC's NEAR-side entry is listed only when zecNear is set.
// Catalog prices are deliberately unlike the rig's fiat rates, which are
// the only USD source for fee lines and the creation minimum.
type oneClickMock struct {
	t          *testing.T
	srv        *httptest.Server
	pub        ed25519.PublicKey
	priv       ed25519.PrivateKey
	mtx        sync.Mutex
	out        string
	minOut     string
	lastReq    map[string]json.RawMessage
	quotes     int
	statuses   map[string]string
	submitted  int
	usdcPrice  float64
	wnearPrice float64
	zecNear    bool
}

func newOneClickMock(t *testing.T) *oneClickMock {
	pub, priv, _ := ed25519.GenerateKey(nil)
	m := &oneClickMock{t: t, pub: pub, priv: priv, out: "5000000", minOut: "4950000", statuses: make(map[string]string), usdcPrice: 1, wnearPrice: 3}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *oneClickMock) handle(w http.ResponseWriter, r *http.Request) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	switch r.URL.Path {
	case "/v0/tokens":
		_, _ = fmt.Fprintf(w, `[
			{"assetId":"nep141:btc.omft.near","decimals":8,"blockchain":"btc","symbol":"BTC","price":60000,"contractAddress":""},
			{"assetId":"nep141:usdt.tether-token.near","decimals":6,"blockchain":"near","symbol":"USDT","price":1,"contractAddress":"usdt.tether-token.near"},
			{"assetId":"nep141:%[1]s","decimals":6,"blockchain":"near","symbol":"USDC","price":%[2]f,"contractAddress":"%[1]s"},
			{"assetId":"nep141:zec.omft.near","decimals":8,"blockchain":"zec","symbol":"ZEC","price":900,"contractAddress":""},
			{"assetId":"nep141:wrap.near","decimals":24,"blockchain":"near","symbol":"wNEAR","price":%[4]f,"contractAddress":"wrap.near"}%[3]s
		]`, usdcTokenForTest, m.usdcPrice, m.zecNearEntry(), m.wnearPrice)
	case "/v0/quote":
		body, _ := io.ReadAll(r.Body)
		var req map[string]json.RawMessage
		if err := json.Unmarshal(body, &req); err != nil {
			m.t.Errorf("bad quote request: %v", err)
		}
		m.lastReq = req
		m.quotes++
		var dry bool
		_ = json.Unmarshal(req["dry"], &dry)
		deadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		quote := map[string]any{
			"amountIn": req["amount"], "amountInFormatted": "0.0001", "amountInUsd": "6",
			"minAmountIn": req["amount"], "amountOut": m.out, "amountOutFormatted": "5",
			"amountOutUsd": "5", "minAmountOut": m.minOut, "timeEstimate": 60,
			"deadline": deadline, "timeWhenInactive": deadline,
		}
		if !dry {
			quote["depositAddress"] = supplyDepositAddr
		}
		rawQuote, _ := json.Marshal(quote)
		const ts = "2026-11-17T23:40:38.373Z"
		sig, err := intents.SignQuote(m.priv, body, rawQuote, ts)
		if err != nil {
			m.t.Errorf("SignQuote: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"timestamp": ts, "signature": sig, "correlationId": "c",
			"quoteRequest": json.RawMessage(body), "quote": json.RawMessage(rawQuote),
		})
	case "/v0/status":
		st, ok := m.statuses[r.URL.Query().Get("depositAddress")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found","error":"Not Found","statusCode":404}`))
			return
		}
		_, _ = w.Write([]byte(st))
	case "/v0/deposit/submit":
		m.submitted++
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func (m *oneClickMock) zecNearEntry() string {
	if !m.zecNear {
		return ""
	}
	return `,
			{"assetId":"1cs_v1:near:nep141:zec.omft.near","decimals":8,"blockchain":"near","symbol":"ZEC","price":1,"contractAddress":"zec.omft.near"}`
}

// setQuote sets the quoted delivery and minimum delivery.
func (m *oneClickMock) setQuote(out, minOut string) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.out, m.minOut = out, minOut
}

func (m *oneClickMock) setStatus(addr, status string, nearTxs ...string) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	hashes, _ := json.Marshal(nearTxs)
	m.statuses[addr] = fmt.Sprintf(`{"status":%q,"updatedAt":"x","swapDetails":{"nearTxHashes":%s}}`, status, hashes)
}

func (m *oneClickMock) requestField(field string) string {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	var s string
	_ = json.Unmarshal(m.lastReq[field], &s)
	return s
}

// copyCrypter is the test crypter with real crypters' buffer semantics:
// Decrypt returns a fresh slice, so callers that zero the plaintext do not
// clobber the stored ciphertext.
type copyCrypter struct{ tCrypter }

func (c *copyCrypter) Decrypt(b []byte) ([]byte, error) {
	return append([]byte(nil), b...), c.decryptErr
}

type supplyRig struct {
	rig    *testRig
	eng    *rheaEngine
	chain  *fakeRheaChain
	mock   *oneClickMock
	wallet *TXCWallet
	notes  chan Notification

	ratesMtx sync.Mutex
	rates    map[string]float64 // USD rates by ticker
}

// setRate sets the USD rate of a ticker, or removes it when rate is zero,
// as for a ticker no rate source has a fresh value for.
func (r *supplyRig) setRate(ticker string, rate float64) {
	r.ratesMtx.Lock()
	defer r.ratesMtx.Unlock()
	if rate == 0 {
		delete(r.rates, ticker)
		return
	}
	r.rates[ticker] = rate
}

// tripRates returns a ticker rate source that fails the test when read.
// Reconciliation, admission walks, and receipt recovery never depend on a
// rate.
func tripRates(t *testing.T) func() map[string]float64 {
	return func() map[string]float64 {
		t.Errorf("fiat rates read outside a new lending operation")
		return nil
	}
}

// newSupplyRig builds a Core with a BTC wallet, the captured mainnet chain
// views, a mock 1Click, and USD ticker rates for every market (USDC and
// USDT at 1, NEAR at 3, ZEC at 900). creating selects an identity with no MCA (the
// rig's own seed); otherwise the identity bound to rhea001569.
func newSupplyRig(t *testing.T, creating bool) *supplyRig {
	r := &supplyRig{rig: newTestRig(), chain: newFakeRheaChain(t), mock: newOneClickMock(t),
		rates: map[string]float64{"USDC": 1, "USDT": 1, "NEAR": 3, "ZEC": 900}}
	t.Cleanup(r.rig.shutdown)
	c := r.rig.core
	cc := &copyCrypter{}
	c.reCrypter = func([]byte, []byte) (encrypt.Crypter, error) { return cc, nil }
	r.notes = subscribeNotes(r.rig)
	if creating {
		c.setRheaIdentities(c.creds().EncSeed) // the test crypter stores the seed in the clear
		id := c.rheaIdentities()[0].Identity
		r.chain.setView("multica.near", "get_mca_by_wallet", `{"wallet":{"EVM":"`+string(id)+`"}}`, "null")
	} else {
		ids := []rheaIdentity{{Version: rhea.DerivationV1, Identity: rheaTestIdentity}}
		c.rheaIDs.Store(&ids)
	}
	ieng := newTestIntentsEngine(r.rig)
	client, err := intents.NewClient(&intents.Config{
		BaseURL: r.mock.srv.URL, SigningKey: base58.Encode(r.mock.pub), AppFees: intentsAppFees(),
	}, tLogger)
	if err != nil {
		t.Fatal(err)
	}
	ieng.client = client
	r.eng = newRheaEngine(tLogger, rhea.Mainnet, r.chain, &fakeRheaHistory{}, r.rig.db, c.rheaIdentities)
	r.eng.c, r.eng.notify = c, c.notify
	r.eng.oneClick = func() (*intentsEngine, error) { return ieng, nil }
	r.eng.tickerRates = func() map[string]float64 {
		r.ratesMtx.Lock()
		defer r.ratesMtx.Unlock()
		return maps.Clone(r.rates)
	}
	c.rheaEng.Store(r.eng)

	xcw, tw := newTWallet(0)
	tw.info = &asset.WalletInfo{BlockchainClass: asset.BlockchainClassAccount, UnitInfo: tWalletInfo.UnitInfo}
	tw.bal = &asset.Balance{Available: 1e10}
	tw.sendCoin = &tCoin{}
	xcw.address = "bc1qrefund"
	c.wallets[0] = xcw
	if err := xcw.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	r.wallet = tw
	return r
}

func TestRheaSupplyExistingAccount(t *testing.T) {
	r := newSupplyRig(t, false)
	c := r.rig.core

	pv, err := c.RheaSupplyPreview(tCtx, 0, usdtToken, 100_000)
	if err != nil {
		t.Fatalf("RheaSupplyPreview: %v", err)
	}
	if pv.Creating || pv.MCA != rheaTestMCA || pv.Delivered != "5000000" || pv.MinDelivered != "4950000" || pv.MinSupplied != "4950000" || pv.Decimals != 6 ||
		pv.CreationFee != nil || pv.CreationMinimum != nil {
		t.Fatalf("preview %+v", pv)
	}
	if got := r.mock.requestField("recipient"); got != rheaTestMCA {
		t.Fatalf("quote recipient %s", got)
	}
	if got := r.mock.requestField("customRecipientMsg"); got != rhea.SupplyMsg(rheaTestIdentity) {
		t.Fatalf("quote message %s", got)
	}
	if got := r.mock.requestField("refundTo"); got != "bc1qrefund" {
		t.Fatalf("quote refund address %s", got)
	}
	// Supply quotes carry the default application fee.
	var fees []intents.AppFee
	_ = json.Unmarshal(r.mock.lastReq["appFees"], &fees)
	if len(fees) != 1 || fees[0].Recipient != intentsFeeRecipient {
		t.Fatalf("supply quote fees %+v", fees)
	}

	// A committed quote below the previewed floor is refused before
	// anything is persisted or sent.
	if _, err := c.RheaSupplyExecute(nil, 0, usdtToken, 100_000, "4950001", false, ""); err == nil || !strings.Contains(err.Error(), "rate moved") {
		t.Fatalf("floor violation: %v", err)
	}
	// A preview that claimed creation no longer matches.
	if _, err := c.RheaSupplyExecute(tPW, 0, usdtToken, 100_000, pv.MinDelivered, true, "600000"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("account state mismatch: %v", err)
	}
	if ops, _ := r.rig.db.RheaOps(); len(ops) != 0 || r.wallet.sendCoin.val != 0 {
		t.Fatalf("refused supplies left %d ops, sent %d", len(ops), r.wallet.sendCoin.val)
	}

	op, err := c.RheaSupplyExecute(nil, 0, usdtToken, 100_000, pv.MinDelivered, false, "")
	if err != nil {
		t.Fatalf("RheaSupplyExecute: %v", err)
	}
	if op.State != IntentsStateDepositSent || op.MCA != rheaTestMCA || op.Leg.Recipient != rheaTestMCA ||
		op.Leg.DepositAddress != supplyDepositAddr || op.Creation || r.wallet.sendCoin.val != 100_000 || r.mock.submitted != 1 {
		t.Fatalf("supply op %+v, sent %d", op, r.wallet.sendCoin.val)
	}
	stored, err := r.rig.db.RheaOp(op.ID)
	if err != nil || stored.State != IntentsStateDepositSent || stored.Leg.CustomRecipientMsg != rhea.SupplyMsg(rheaTestIdentity) {
		t.Fatalf("stored op %+v, %v", stored, err)
	}

	// The leg advances under the shared rules; delivery moves to
	// attribution, which cannot find this operation's message in a
	// foreign transaction and stays unresolved.
	r.mock.setStatus(supplyDepositAddr, intents.StatusProcessing)
	r.eng.pollOps(tCtx)
	if got, _ := r.rig.db.RheaOp(op.ID); got.State != intents.StatusProcessing {
		t.Fatalf("processing: %s", got.State)
	}
	r.chain.txs[createTxHash] = readRheaTestdata(t, "tx-create-supply-1aqcDa8Y.json")
	r.mock.setStatus(supplyDepositAddr, intents.StatusSuccess, createTxHash)
	r.eng.pollOps(tCtx)
	got, _ := r.rig.db.RheaOp(op.ID)
	if got.State != db.RheaStateDeliveryUnresolved || got.Leg.Status != intents.StatusSuccess || !got.Active() {
		t.Fatalf("delivered: %s (%s)", got.State, got.Notes)
	}
}

func TestRheaSupplyCreation(t *testing.T) {
	r := newSupplyRig(t, true)
	c := r.rig.core
	id := c.rheaIdentities()[0].Identity
	r.mock.setQuote("5500000", "5445000")

	pv, err := c.RheaSupplyPreview(tCtx, 0, usdtToken, 100_000)
	if err != nil {
		t.Fatalf("RheaSupplyPreview: %v", err)
	}
	// 0.3 USDT creation fee plus 0.1 NEAR at 3 USDT per NEAR, shown as
	// its own one-time line with both parts, priced at the rig's 1 USD
	// rate.
	if !pv.Creating || pv.MinSupplied != "4845000" || pv.MCA != "" || pv.CreationFee == nil {
		t.Fatalf("creation preview %+v", pv)
	}
	if m := pv.CreationMinimum; m == nil || m.Amount != "5000000" || m.USD != "5.00" {
		t.Fatalf("creation minimum %+v", m)
	}
	fl := pv.CreationFee
	if fl.Amount != "600000" || fl.USD != "0.60" || !fl.OneTime || fl.Symbol != "USDT" || fl.Decimals != 6 || fl.TokenID != usdtToken ||
		len(fl.Components) != 2 || fl.Unavailable != "" {
		t.Fatalf("creation fee line %+v", fl)
	}
	if c := fl.Components[0]; c.Kind != RheaFeeFixed || c.Amount != "300000" || c.USD != "0.30" || c.NEAR != "" {
		t.Fatalf("fixed component %+v", c)
	}
	if c := fl.Components[1]; c.Kind != RheaFeeAccountFunding || c.Amount != "300000" || c.USD != "0.30" || c.NEAR != "100000000000000000000000" {
		t.Fatalf("account funding component %+v", c)
	}
	if got := r.mock.requestField("recipient"); got != rhea.Mainnet.AccountManager {
		t.Fatalf("creation quote recipient %s", got)
	}
	if msg, err := rhea.ParseRecipientMsg(r.mock.requestField("customRecipientMsg")); err != nil || msg.Template != rhea.TemplateBurrowRegisterAndSupply {
		t.Fatalf("creation preview message %+v, %v", msg, err)
	}

	// Creating signs with the identity, so it needs the password.
	if _, err := c.RheaSupplyExecute(nil, 0, usdtToken, 100_000, pv.MinDelivered, true, pv.CreationFee.Amount); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("creation without a password: %v", err)
	}
	// A fee above the previewed one is refused.
	if _, err := c.RheaSupplyExecute(tPW, 0, usdtToken, 100_000, pv.MinDelivered, true, "599999"); err == nil || !strings.Contains(err.Error(), "fee rose") {
		t.Fatalf("fee increase: %v", err)
	}
	op, err := c.RheaSupplyExecute(tPW, 0, usdtToken, 100_000, pv.MinDelivered, true, pv.CreationFee.Amount)
	if err != nil {
		t.Fatalf("RheaSupplyExecute: %v", err)
	}
	msg, err := rhea.ParseRecipientMsg(op.Leg.CustomRecipientMsg)
	if err != nil || len(msg.Signatures) != 1 {
		t.Fatalf("creation message %+v, %v", msg, err)
	}
	if signer, err := rhea.RecoverPersonal(rhea.CreationMessage(id), msg.Signatures[0]); err != nil || signer != id {
		t.Fatalf("creation signature by %s, %v", signer, err)
	}
	if !op.Creation || op.CreationFee != "600000" || op.MCA != "" || op.Leg.Recipient != rhea.Mainnet.AccountManager {
		t.Fatalf("creation op %+v", op)
	}

	// A second creation waits for the first.
	if _, err := c.RheaSupplyPreview(tCtx, 0, usdtToken, 100_000); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("second creation: %v", err)
	}
}

func TestRheaSupplyRefusals(t *testing.T) {
	r := newSupplyRig(t, true)
	c := r.rig.core
	r.mock.setQuote("5500000", "5445000")
	preview := func() error {
		_, err := c.RheaSupplyPreview(tCtx, 0, usdtToken, 100_000)
		return err
	}
	if err := preview(); err != nil {
		t.Fatalf("baseline preview: %v", err)
	}

	// Absence must be corroborated by every endpoint.
	r.chain.allAnswers = []string{`"rhea000001.multica.near"`}
	if err := preview(); err == nil || !strings.Contains(err.Error(), "reports an account") {
		t.Fatalf("contradicted absence: %v", err)
	}
	r.chain.allAnswers = nil
	r.chain.allErr = fmt.Errorf("endpoint down")
	if err := preview(); err == nil || !strings.Contains(err.Error(), "not corroborated") {
		t.Fatalf("uncorroborated absence: %v", err)
	}
	r.chain.allErr = nil

	// Altered templates fail closed.
	tmpl := string(r.chain.views["multica.near get_reliable_business_template_paged "+rheaCanonArgs(t, []byte(`{"from_index":0,"limit":100}`))])
	r.chain.setView("multica.near", "get_reliable_business_template_paged", `{"from_index":0,"limit":100}`,
		strings.Replace(tmpl, `\\\"msg\\\":\\\"\\\"`, `\\\"msg\\\":\\\"\\\\\\\"OnlyIncreaseCollateral\\\\\\\"\\\"`, 1))
	if err := preview(); err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("altered template: %v", err)
	}
	r.chain.setView("multica.near", "get_reliable_business_template_paged", `{"from_index":0,"limit":100}`, tmpl)

	// A market that refuses deposits, or has no room, is refused.
	assetKey := `{"token_id":"usdt.tether-token.near"}`
	orig := string(r.chain.views["contract.main.burrow.near get_asset "+rheaCanonArgs(t, []byte(assetKey))])
	var a map[string]any
	_ = json.Unmarshal([]byte(orig), &a)
	a["config"].(map[string]any)["can_deposit"] = false
	b, _ := json.Marshal(a)
	r.chain.setView("contract.main.burrow.near", "get_asset", assetKey, string(b))
	if err := preview(); err == nil || !strings.Contains(err.Error(), "not accepting") {
		t.Fatalf("closed market: %v", err)
	}
	a["config"].(map[string]any)["can_deposit"] = true
	a["config"].(map[string]any)["supplied_limit"] = a["supplied"].(map[string]any)["balance"]
	b, _ = json.Marshal(a)
	r.chain.setView("contract.main.burrow.near", "get_asset", assetKey, string(b))
	if err := preview(); err == nil || !strings.Contains(err.Error(), "supply limit") {
		t.Fatalf("full market: %v", err)
	}
	r.chain.setView("contract.main.burrow.near", "get_asset", assetKey, orig)

	// Unsupported markets and a failed discovery are refused.
	if _, err := c.RheaSupplyPreview(tCtx, 0, "eth.bridge.near", 100_000); err == nil {
		t.Fatalf("unlisted market accepted")
	}
	r.chain.viewErr["multica.near get_mca_by_wallet"] = fmt.Errorf("endpoint down")
	if err := preview(); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("failed discovery: %v", err)
	}
}

// TestRheaSupplyCreationMinimum holds an account-creating supply's
// guaranteed delivery to the larger of twice the creation fee and 5 USD at
// Caribou's fiat rate. The 1Click catalog's prices play no part, and
// without a fresh rate no supply is planned.
func TestRheaSupplyCreationMinimum(t *testing.T) {
	r := newSupplyRig(t, true)
	c := r.rig.core
	preview := func(token string) *RheaSupplyPreview {
		t.Helper()
		pv, err := c.RheaSupplyPreview(tCtx, 0, token, 100_000)
		if err != nil {
			t.Fatalf("RheaSupplyPreview(%s): %v", token, err)
		}
		return pv
	}
	wantMin := func(pv *RheaSupplyPreview, amount, usd string) {
		t.Helper()
		if m := pv.CreationMinimum; m == nil || m.Amount != amount || m.USD != usd {
			t.Fatalf("creation minimum %+v, want %s, %q", m, amount, usd)
		}
	}
	refused := func(token, want string) {
		t.Helper()
		if _, err := c.RheaSupplyPreview(tCtx, 0, token, 100_000); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("below the minimum: %v, want %q", err, want)
		}
	}

	// USDC at 1 USD (the catalog says 0.5): the 0.60 fee doubles to 1.20,
	// so the 5 USD term wins. A guaranteed delivery equal to the minimum
	// passes; one unit less is refused with the minimum named.
	r.mock.mtx.Lock()
	r.mock.usdcPrice = 0.5
	r.mock.mtx.Unlock()
	r.mock.setQuote("5500000", "5000000")
	pv := preview(usdcTokenForTest)
	wantMin(pv, "5000000", "5.00")
	if pv.CreationFee.Amount != "600000" || pv.CreationFee.USD != "0.60" || pv.CreationFee.Components[1].USD != "0.30" {
		t.Fatalf("creation fee %+v", pv.CreationFee)
	}
	r.mock.setQuote("5500000", "4999999")
	refused(usdcTokenForTest, "must guarantee a delivery of at least 5 USDC ($5.00), the larger of twice the 0.6 USDC account creation fee and $5; this quote guarantees 4.999999 USDC")

	// A 2.70 USDC fixed fee makes the creation fee 3.00 USDC, and twice
	// the fee wins.
	feeArgs := `{"token_id":"` + usdcTokenForTest + `"}`
	r.chain.setView("multica.near", "get_create_mca_fee", feeArgs, `"2700000"`)
	r.mock.setQuote("6500000", "6000000")
	wantMin(preview(usdcTokenForTest), "6000000", "6.00")
	r.mock.setQuote("6500000", "5999999")
	refused(usdcTokenForTest, "at least 6 USDC ($6.00), the larger of twice the 3 USDC account creation fee")
	r.chain.setView("multica.near", "get_create_mca_fee", feeArgs, `"300000"`)

	// ZEC at 900 USD (the catalog says 1): 5 USD is 0.00555556 ZEC, above
	// twice the 0.0006 ZEC fee.
	r.mock.mtx.Lock()
	r.mock.zecNear = true
	r.mock.mtx.Unlock()
	ieng, _ := r.eng.oneClick()
	ieng.catMtx.Lock()
	ieng.catalog = nil // list the NEAR-side ZEC entry
	ieng.catMtx.Unlock()
	r.mock.setQuote("600000", "555556")
	pv = preview("zec.omft.near")
	wantMin(pv, "555556", "5.00")
	if pv.CreationFee.Amount != "60000" || pv.CreationFee.USD != "0.54" {
		t.Fatalf("ZEC creation fee %+v", pv.CreationFee)
	}
	r.mock.setQuote("600000", "555555")
	refused("zec.omft.near", "at least 0.00555556 ZEC ($5.00)")

	// No fresh fiat rate for USDC: refused, whatever the quote.
	r.setRate("USDC", 0)
	r.mock.setQuote("5500000", "5000000")
	refused(usdcTokenForTest, "no current USD rate for USDC; supplying is unavailable until rates refresh")
}

// TestRheaSupplyCreationMinimumExecute re-checks the creation minimum
// against the committed quote and the current fiat rate, before anything is
// persisted or sent.
func TestRheaSupplyCreationMinimumExecute(t *testing.T) {
	r := newSupplyRig(t, true)
	c := r.rig.core
	r.mock.setQuote("5500000", "5000000")
	pv, err := c.RheaSupplyPreview(tCtx, 0, usdcTokenForTest, 100_000)
	if err != nil {
		t.Fatalf("RheaSupplyPreview: %v", err)
	}
	execute := func() (*RheaOp, error) {
		return c.RheaSupplyExecute(tPW, 0, usdcTokenForTest, 100_000, pv.MinDelivered, true, pv.CreationFee.Amount)
	}

	// The committed quote guarantees less than the minimum.
	r.mock.setQuote("5500000", "4999999")
	if _, err := execute(); err == nil || !strings.Contains(err.Error(), "at least 5 USDC ($5.00)") {
		t.Fatalf("committed quote below the minimum: %v", err)
	}
	// The previewed quote at a lower rate: 5 USD is now 5.555556 USDC,
	// above the guaranteed 5 USDC the preview floor still accepts.
	r.mock.setQuote("5500000", "5000000")
	r.setRate("USDC", 0.9)
	if _, err := execute(); err == nil || !strings.Contains(err.Error(), "at least 5.555556 USDC ($5.00)") {
		t.Fatalf("rate drop below the minimum: %v", err)
	}
	// The rate lost since the preview: refused.
	r.setRate("USDC", 0)
	if _, err := execute(); err == nil || !strings.Contains(err.Error(), "no current USD rate for USDC") {
		t.Fatalf("lost rate: %v", err)
	}
	if ops, _ := r.rig.db.RheaOps(); len(ops) != 0 || r.wallet.sendCoin.val != 0 || r.mock.submitted != 0 {
		t.Fatalf("refused supplies left %d ops, sent %d, submitted %d", len(ops), r.wallet.sendCoin.val, r.mock.submitted)
	}

	// At the previewed terms it proceeds.
	r.setRate("USDC", 1)
	r.mock.setQuote("5500000", "5000000")
	op, err := execute()
	if err != nil {
		t.Fatalf("RheaSupplyExecute: %v", err)
	}
	if !op.Creation || op.MinDelivered != "5000000" || r.wallet.sendCoin.val != 100_000 {
		t.Fatalf("creation op %+v, sent %d", op, r.wallet.sendCoin.val)
	}
}

// TestRheaSupplyNoRate refuses a supply to an existing account without a
// current USD rate for the market token, at preview and again at execute
// when the rate is lost after the preview.
func TestRheaSupplyNoRate(t *testing.T) {
	r := newSupplyRig(t, false)
	c := r.rig.core
	const want = "no current USD rate for USDT; supplying is unavailable until rates refresh"
	r.setRate("USDT", 0)
	if _, err := c.RheaSupplyPreview(tCtx, 0, usdtToken, 100_000); err == nil || err.Error() != want {
		t.Fatalf("preview without a rate: %v", err)
	}
	r.setRate("USDT", 1)
	pv, err := c.RheaSupplyPreview(tCtx, 0, usdtToken, 100_000)
	if err != nil {
		t.Fatalf("RheaSupplyPreview: %v", err)
	}
	r.setRate("USDT", 0)
	if _, err := c.RheaSupplyExecute(nil, 0, usdtToken, 100_000, pv.MinDelivered, false, ""); err == nil || err.Error() != want {
		t.Fatalf("execute after the rate was lost: %v", err)
	}
	if ops, _ := r.rig.db.RheaOps(); len(ops) != 0 || r.wallet.sendCoin.val != 0 || r.mock.submitted != 0 {
		t.Fatalf("refused supply left %d ops, sent %d, submitted %d", len(ops), r.wallet.sendCoin.val, r.mock.submitted)
	}
	r.setRate("USDT", 1)
	if _, err := c.RheaSupplyExecute(nil, 0, usdtToken, 100_000, pv.MinDelivered, false, ""); err != nil {
		t.Fatalf("RheaSupplyExecute with the rate back: %v", err)
	}
}

func TestCreationMinimum(t *testing.T) {
	fee := big.NewInt(600000)
	for _, tt := range []struct {
		name     string
		fee      *big.Int
		decimals int
		price    float64
		want     string
	}{
		{"usd floor wins", fee, 6, 1, "5000000"},
		{"fee multiple wins", fee, 6, 10, "1200000"},
		{"rounded up", fee, 6, 0.9, "5555556"},
		{"wrap.near", new(big.Int).Mul(big.NewInt(2), pow10Big(23)), 24, 3, "1666666666666666666666667"},
		// Rates whose micro-USD rounding would undervalue the floor.
		{"just over a micro-USD fraction", big.NewInt(1), 6, 1.0000006, "4999998"},
		{"sub-micro fraction", big.NewInt(1), 6, 1.6e-6, "3125000000001"},
	} {
		if got, err := creationMinimum(tt.fee, tt.decimals, tt.price); err != nil || got.String() != tt.want {
			t.Errorf("%s: %v, %v, want %s", tt.name, got, err, tt.want)
		}
	}
	for _, price := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if got, err := creationMinimum(fee, 6, price); err == nil {
			t.Errorf("price %v: minimum %s", price, got)
		}
	}

	// Wherever the USD term sets the minimum, the minimum is worth at least
	// 5 USD at the exact rate and one base unit less is worth less.
	five := big.NewRat(rheaCreationMinMicroUSD, 1_000_000)
	worth := func(units *big.Int, decimals int, price float64) *big.Rat {
		v := new(big.Rat).SetFrac(units, pow10Big(decimals))
		return v.Mul(v, new(big.Rat).SetFloat64(price))
	}
	for _, price := range []float64{1e-6, 1.4e-6, 1.6e-6, 0.0012345678, 0.3333333, 0.9, 0.9996, 1.0000006, 2.9999997, 3.25, 899.99, 64123.456789} {
		for _, decimals := range []int{6, 8, 18, 24} {
			got, err := creationMinimum(big.NewInt(1), decimals, price)
			if err != nil {
				t.Fatalf("price %v, decimals %d: %v", price, decimals, err)
			}
			if worth(got, decimals, price).Cmp(five) < 0 {
				t.Errorf("price %v, decimals %d: minimum %s is worth %s USD", price, decimals, got,
					worth(got, decimals, price).FloatString(12))
			}
			if less := new(big.Int).Sub(got, big.NewInt(1)); worth(less, decimals, price).Cmp(five) >= 0 {
				t.Errorf("price %v, decimals %d: minimum %s is not the least amount worth 5 USD", price, decimals, got)
			}
		}
	}
	for _, tt := range []struct {
		v        int64
		decimals int
		want     string
	}{
		{5000000, 6, "5"},
		{600000, 6, "0.6"},
		{4999999, 6, "4.999999"},
		{120000, 8, "0.0012"},
		{0, 6, "0"},
		{7, 0, "7"},
	} {
		if got := formatBaseUnits(big.NewInt(tt.v), tt.decimals); got != tt.want {
			t.Errorf("formatBaseUnits(%d, %d) = %q, want %q", tt.v, tt.decimals, got, tt.want)
		}
	}
}

// TestRheaSupplyLegRules runs a supply leg through the shared timeout,
// late settlement, and retirement rules.
func TestRheaSupplyLegRules(t *testing.T) {
	r := newSupplyRig(t, false)
	c := r.rig.core
	pv, err := c.RheaSupplyPreview(tCtx, 0, usdtToken, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	op, err := c.RheaSupplyExecute(nil, 0, usdtToken, 100_000, pv.MinDelivered, false, "")
	if err != nil {
		t.Fatal(err)
	}
	rates := r.eng.tickerRates
	r.eng.tickerRates = tripRates(t)
	reload := func() *RheaOp {
		got, err := r.rig.db.RheaOp(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// Past the deadline with no deposit observed: FAILED, inferred, still
	// active.
	r.mock.setStatus(supplyDepositAddr, intents.StatusPendingDeposit)
	r.eng.now = func() time.Time { return op.Leg.Deadline.Add(intentsExpiryGrace + time.Second) }
	r.eng.pollOps(tCtx)
	got := reload()
	if got.State != intents.StatusFailed || !got.Leg.Inferred || !got.Active() {
		t.Fatalf("timed out: %+v", got)
	}
	// A restated PENDING_DEPOSIT does not supersede the inference.
	r.eng.pollOps(tCtx)
	if got = reload(); got.State != intents.StatusFailed || !got.Leg.Inferred {
		t.Fatalf("restatement superseded the inference: %+v", got)
	}
	// A late settlement does.
	r.mock.setStatus(supplyDepositAddr, intents.StatusSuccess)
	r.eng.pollOps(tCtx)
	if got = reload(); got.State != db.RheaStateDeliveryUnresolved || got.Leg.Inferred || got.Leg.Status != intents.StatusSuccess {
		t.Fatalf("late settlement: %+v", got)
	}

	// A second supply times out and retires past the horizon.
	r.eng.tickerRates = rates
	op2, err := c.RheaSupplyExecute(nil, 0, usdtToken, 100_000, pv.MinDelivered, false, "")
	if err != nil {
		t.Fatal(err)
	}
	r.eng.tickerRates = tripRates(t)
	op = op2
	r.mock.mtx.Lock()
	delete(r.mock.statuses, supplyDepositAddr)
	r.mock.mtx.Unlock()
	r.rig.db.rheaMtx.Lock()
	for id, o := range r.rig.db.rheaOps {
		if id != op2.ID {
			o.State = db.RheaStateSupplied // settle the first
		}
	}
	r.rig.db.rheaMtx.Unlock()
	r.eng.pollOps(tCtx) // unknown to the service, past the deadline
	if got = reload(); got.State != intents.StatusFailed || !got.Leg.Inferred {
		t.Fatalf("unknown deposit timed out: %+v", got)
	}
	r.eng.now = func() time.Time { return op.Leg.Deadline.Add(intentsInferredHorizon + time.Second) }
	r.eng.pollOps(tCtx)
	if got = reload(); got.Leg.Inferred || got.Active() || got.State != intents.StatusFailed {
		t.Fatalf("retired: %+v", got)
	}
}

// TestRheaAttributeOp attributes a delivered creation supply recorded for
// the captured mainnet transaction.
func TestRheaAttributeOp(t *testing.T) {
	r := newSupplyRig(t, false)
	raw := readRheaTestdata(t, "tx-create-supply-1aqcDa8Y.json")
	r.chain.txs[createTxHash] = raw
	op := &RheaOp{
		ID: "op", Kind: db.RheaOpSupply, State: db.RheaStateDelivered, TokenID: usdtToken, Creation: true,
		AccountManager: rhea.Mainnet.AccountManager, CreatedAt: time.Now(),
		Leg: &db.RheaLeg{
			DepositAddress: supplyDepositAddr, Recipient: rhea.Mainnet.AccountManager, Status: intents.StatusSuccess,
			CustomRecipientMsg: createTreeMsg(t, raw), NearTxHashes: []string{"deposittx", createTxHash},
			// The operation's own deposit is reported too; its role is
			// known, so it is not read.
			DepositTxID: "deposittx",
		},
	}
	if err := r.rig.db.StoreRheaOp(op); err != nil {
		t.Fatal(err)
	}
	r.eng.pollOps(tCtx)
	got, err := r.rig.db.RheaOp("op")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != db.RheaStateSupplied || got.MCA != createdMCA || got.Delivered != "629935" || got.Fees != "600000" || got.Supplied != "29935" || got.Active() {
		t.Fatalf("attributed op %+v (%s)", got, got.Notes)
	}

	// A reported transaction of unknown role keeps the attribution
	// unresolved even beside the readable delivery, whose evidence that the
	// recipient kept tokens is still recorded.
	// Each case starts from a clean leg.
	freshLeg := func(hashes ...string) {
		leg := *op.Leg
		leg.NearTxHashes, leg.Received, leg.VerifierRefunded = hashes, false, false
		op.Leg = &leg
	}
	op.ID, op.State = "op2", db.RheaStateDelivered
	freshLeg("unknownhash", createTxHash)
	if err := r.rig.db.StoreRheaOp(op); err != nil {
		t.Fatal(err)
	}
	r.eng.pollOps(tCtx)
	if got, _ = r.rig.db.RheaOp("op2"); got.State != db.RheaStateDeliveryUnresolved || !got.Active() ||
		!strings.Contains(got.Notes, "could not be established") || !got.Leg.Received {
		t.Fatalf("unknown extra transaction %+v (%s)", got, got.Notes)
	}

	// With every reported transaction unreadable, the delivery stays
	// unresolved.
	op.ID, op.State = "op3", db.RheaStateDelivered
	freshLeg("unknownhash")
	if err := r.rig.db.StoreRheaOp(op); err != nil {
		t.Fatal(err)
	}
	r.eng.pollOps(tCtx)
	if got, _ = r.rig.db.RheaOp("op3"); got.State != db.RheaStateDeliveryUnresolved || !got.Active() || got.Leg.Received {
		t.Fatalf("unreadable delivery %+v (%s)", got, got.Notes)
	}
}

// TestRheaSupplyConcurrentCreation runs two creation supplies for one
// identity past the planning guard at once: only one may be funded.
func TestRheaSupplyConcurrentCreation(t *testing.T) {
	r := newSupplyRig(t, true)
	r.mock.setQuote("5500000", "5445000")
	entered, release := make(chan struct{}, 2), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/v0/quote" {
			entered <- struct{}{}
			<-release
		}
		r.mock.handle(w, q)
	}))
	defer srv.Close()
	client, err := intents.NewClient(&intents.Config{BaseURL: srv.URL, SigningKey: base58.Encode(r.mock.pub), AppFees: intentsAppFees()}, tLogger)
	if err != nil {
		t.Fatal(err)
	}
	ieng, _ := r.eng.oneClick()
	ieng.client = client

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := r.rig.core.RheaSupplyExecute(tPW, 0, usdtToken, 100_000, "5445000", true, "600000")
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("the two supplies did not both reach the quote")
		}
	}
	close(release)
	var failed int
	for range 2 {
		if err := <-results; err != nil {
			if !strings.Contains(err.Error(), "in progress") {
				t.Fatalf("unexpected error: %v", err)
			}
			failed++
		}
	}
	ops, err := r.rig.db.RheaOps()
	if failed != 1 || err != nil || len(ops) != 1 || r.mock.submitted != 1 {
		t.Fatalf("%d refused, %d ops, %d deposits", failed, len(ops), r.mock.submitted)
	}
}

// TestRheaSupplyUnresolvedReconciles keeps an unresolved delivery in step
// with 1Click. A refund report ends it only on positive evidence that the
// delivery went back to the verifier, after every reported transaction is
// examined; a corrected transaction list restarts attribution, which
// otherwise backs off.
func TestRheaSupplyUnresolvedReconciles(t *testing.T) {
	raw := readRheaTestdata(t, "tx-create-supply-1aqcDa8Y.json")
	msg := createTreeMsg(t, raw)
	// rig returns a supply rig whose chain serves the given transactions,
	// with an unresolved creation supply reporting unknownhash.
	rig := func(txs map[string][]byte) (*supplyRig, *RheaOp) {
		r := newSupplyRig(t, false)
		for h, b := range txs {
			r.chain.txs[h] = b
		}
		op := &RheaOp{
			ID: "op", Kind: db.RheaOpSupply, State: db.RheaStateDeliveryUnresolved, TokenID: usdtToken, Creation: true,
			AccountManager: rhea.Mainnet.AccountManager, CreatedAt: time.Now(),
			Leg: &db.RheaLeg{
				DepositAddress: supplyDepositAddr, Recipient: rhea.Mainnet.AccountManager, Status: intents.StatusSuccess,
				CustomRecipientMsg: msg, NearTxHashes: []string{"unknownhash"},
			},
		}
		if err := r.rig.db.StoreRheaOp(op); err != nil {
			t.Fatal(err)
		}
		return r, op
	}
	state := func(r *supplyRig) *RheaOp {
		got, err := r.rig.db.RheaOp("op")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	edit := func(f func(m map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		f(m)
		b, _ := json.Marshal(m)
		return b
	}

	// No evidence either way: a refund report is not adopted.
	r, op := rig(nil)
	r.mock.setStatus(supplyDepositAddr, intents.StatusRefunded, "unknownhash")
	if err := r.eng.pollSupply(tCtx, op); err != nil {
		t.Fatal(err)
	}
	if got := state(r); got.State != db.RheaStateDeliveryUnresolved || !got.Active() {
		t.Fatalf("refund without evidence adopted: %+v", got)
	}

	// The delivery returned to the verifier in full, then refunded: the
	// operation ends as REFUNDED.
	verifierRefund := edit(func(m map[string]any) {
		setResolved(t, m, rheaVerifier, rhea.Mainnet.AccountManager, "0")
	})
	r, op = rig(map[string][]byte{createTxHash: verifierRefund})
	r.mock.setStatus(supplyDepositAddr, intents.StatusRefunded, createTxHash)
	if err := r.eng.pollSupply(tCtx, op); err != nil {
		t.Fatal(err)
	}
	if got := state(r); got.State != intents.StatusRefunded || got.Active() || !got.Leg.VerifierRefunded || got.Leg.Received {
		t.Fatalf("verifier refund: %+v (%s)", got, got.Notes)
	}

	// A refund proof does not survive a changed transaction list: a later
	// report adding a transaction of unknown role leaves the operation
	// unresolved even though a refund is reported.
	r, op = rig(map[string][]byte{createTxHash: verifierRefund})
	r.mock.setStatus(supplyDepositAddr, intents.StatusSuccess, createTxHash)
	if err := r.eng.pollSupply(tCtx, op); err != nil {
		t.Fatal(err)
	}
	if got := state(r); got.State != db.RheaStateDeliveryUnresolved || !got.Leg.VerifierRefunded {
		t.Fatalf("verifier refund before the report: %+v (%s)", got, got.Notes)
	}
	r.mock.setStatus(supplyDepositAddr, intents.StatusRefunded, createTxHash, "unknownhash")
	if err := r.eng.pollSupply(tCtx, state(r)); err != nil {
		t.Fatal(err)
	}
	if got := state(r); got.State != db.RheaStateDeliveryUnresolved || !got.Active() || got.Leg.VerifierRefunded {
		t.Fatalf("stale refund proof adopted: %+v (%s)", got, got.Notes)
	}

	// A consumed delivery arriving in the same status response as the
	// refund report is attributed, not retired.
	r, op = rig(map[string][]byte{createTxHash: raw})
	r.mock.setStatus(supplyDepositAddr, intents.StatusRefunded, createTxHash)
	if err := r.eng.pollSupply(tCtx, op); err != nil {
		t.Fatal(err)
	}
	if got := state(r); got.State != db.RheaStateSupplied {
		t.Fatalf("consumed delivery with a refund report: %+v (%s)", got, got.Notes)
	}

	// Two consumed deliveries are ambiguous, and their evidence keeps a
	// later refund report from retiring the operation.
	copied := edit(func(m map[string]any) { m["transaction"].(map[string]any)["hash"] = "copy" })
	r, op = rig(map[string][]byte{createTxHash: raw, "copy": copied})
	r.mock.setStatus(supplyDepositAddr, intents.StatusSuccess, createTxHash, "copy")
	if err := r.eng.pollSupply(tCtx, op); err != nil {
		t.Fatal(err)
	}
	if got := state(r); got.State != db.RheaStateDeliveryUnresolved || !got.Leg.Received {
		t.Fatalf("ambiguous deliveries: %+v (%s)", got, got.Notes)
	}
	r.mock.setStatus(supplyDepositAddr, intents.StatusRefunded, createTxHash, "copy")
	r.eng.clearAttributionBackoff("op")
	if err := r.eng.pollSupply(tCtx, state(r)); err != nil {
		t.Fatal(err)
	}
	if got := state(r); got.State != db.RheaStateDeliveryUnresolved || !got.Active() {
		t.Fatalf("ambiguous deliveries retired by a refund report: %+v", got)
	}

	// Attribution backs off while the transaction list is unchanged.
	r, op = rig(map[string][]byte{createTxHash: raw})
	r.mock.setStatus(supplyDepositAddr, intents.StatusSuccess, "unknownhash")
	if err := r.eng.pollSupply(tCtx, op); err != nil {
		t.Fatal(err)
	}
	r.chain.txCalls = 0
	if err := r.eng.pollSupply(tCtx, state(r)); err != nil {
		t.Fatal(err)
	}
	if r.chain.txCalls != 0 {
		t.Fatalf("attribution retried during backoff: %d lookups", r.chain.txCalls)
	}
	// A corrected list restarts it at once and the delivery resolves.
	r.mock.setStatus(supplyDepositAddr, intents.StatusSuccess, createTxHash)
	if err := r.eng.pollSupply(tCtx, state(r)); err != nil {
		t.Fatal(err)
	}
	if got := state(r); got.State != db.RheaStateSupplied || got.MCA != createdMCA || !got.Leg.Received {
		t.Fatalf("corrected hash list: %+v (%s)", got, got.Notes)
	}

	// The backoff doubles and is capped.
	for range 10 {
		r.eng.deferAttribution("x")
	}
	if d := r.eng.attrDelay["x"]; d != rheaAttrMaxDelay {
		t.Fatalf("backoff %s", d)
	}
}

// TestRheaAttributeOpSelection checks which reported transaction a
// delivery is attributed to.
func TestRheaAttributeOpSelection(t *testing.T) {
	raw := readRheaTestdata(t, "tx-create-supply-1aqcDa8Y.json")
	msg := createTreeMsg(t, raw)
	rehash := func(hash string, f func(m map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["transaction"].(map[string]any)["hash"] = hash
		if f != nil {
			f(m)
		}
		b, _ := json.Marshal(m)
		return b
	}
	run := func(hashes ...string) (*RheaOp, *supplyRig) {
		r := newSupplyRig(t, false)
		op := &RheaOp{
			ID: "op", Kind: db.RheaOpSupply, State: db.RheaStateDelivered, TokenID: usdtToken, Creation: true,
			AccountManager: rhea.Mainnet.AccountManager, CreatedAt: time.Now(),
			Leg: &db.RheaLeg{
				DepositAddress: supplyDepositAddr, Recipient: rhea.Mainnet.AccountManager, Status: intents.StatusSuccess,
				CustomRecipientMsg: msg, NearTxHashes: hashes,
			},
		}
		if err := r.rig.db.StoreRheaOp(op); err != nil {
			t.Fatal(err)
		}
		return op, r
	}

	// A status for another transaction than the one asked for is refused.
	op, r := run("different-hash")
	r.chain.txs["different-hash"] = raw
	if err := r.eng.attributeOp(tCtx, op); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.rig.db.RheaOp("op"); got.State != db.RheaStateDeliveryUnresolved || !strings.Contains(got.Notes, "describes") {
		t.Fatalf("mismatched transaction: %s (%s)", got.State, got.Notes)
	}

	// A transaction delivering another token to the same recipient with
	// the same message does not shadow the real delivery.
	op, r = run("othertoken", createTxHash)
	r.chain.txs[createTxHash] = raw
	r.chain.txs["othertoken"] = rehash("othertoken", func(m map[string]any) {
		for _, o := range m["receipts_outcome"].([]any) {
			out := o.(map[string]any)["outcome"].(map[string]any)
			for i, l := range out["logs"].([]any) {
				if s := l.(string); strings.Contains(s, `"ft_withdraw"`) {
					out["logs"].([]any)[i] = strings.Replace(s, `"token":"usdt.tether-token.near"`, `"token":"zec.omft.near"`, 1)
				}
			}
		}
	})
	if err := r.eng.attributeOp(tCtx, op); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.rig.db.RheaOp("op"); got.State != db.RheaStateSupplied {
		t.Fatalf("shadowed delivery: %s (%s)", got.State, got.Notes)
	}

	// Two reported transactions carrying the delivery are ambiguous.
	op, r = run(createTxHash, "copy")
	r.chain.txs[createTxHash] = raw
	r.chain.txs["copy"] = rehash("copy", nil)
	if err := r.eng.attributeOp(tCtx, op); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.rig.db.RheaOp("op"); got.State != db.RheaStateDeliveryUnresolved || !strings.Contains(got.Notes, "2 reported") {
		t.Fatalf("ambiguous delivery: %s (%s)", got.State, got.Notes)
	}

	// A pending reported transaction is not yet established as anything
	// else: it may still log the delivery.
	op, r = run(createTxHash, "pending")
	r.chain.txs[createTxHash] = raw
	r.chain.txs["pending"] = rehash("pending", func(m map[string]any) {
		m["final_execution_status"] = "EXECUTED_OPTIMISTIC"
		for _, o := range m["receipts_outcome"].([]any) {
			out := o.(map[string]any)["outcome"].(map[string]any)
			kept := []any{}
			for _, l := range out["logs"].([]any) {
				if !strings.Contains(l.(string), `"ft_withdraw"`) {
					kept = append(kept, l)
				}
			}
			out["logs"] = kept
		}
	})
	if err := r.eng.attributeOp(tCtx, op); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.rig.db.RheaOp("op"); got.State != db.RheaStateDeliveryUnresolved || !strings.Contains(got.Notes, "not final") {
		t.Fatalf("pending extra transaction: %s (%s)", got.State, got.Notes)
	}

	// A repeated hash is the same transaction, not a second delivery.
	op, r = run(createTxHash, createTxHash)
	r.chain.txs[createTxHash] = raw
	if err := r.eng.attributeOp(tCtx, op); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.rig.db.RheaOp("op"); got.State != db.RheaStateSupplied {
		t.Fatalf("repeated hash: %s (%s)", got.State, got.Notes)
	}
}
